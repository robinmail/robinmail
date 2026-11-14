package clustertest

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// robinCLI runs a robinmail command in a node's container.
func robinCLI(t testing.TB, n int, stdin string, args ...string) string {
	t.Helper()
	out, err := robinCLIErr(n, stdin, args...)
	if err != nil {
		t.Fatalf("robinmail %s on %s: %v\n%s", strings.Join(args, " "), robin[n].service, err, out)
	}
	return out
}

func robinCLIErr(n int, stdin string, args ...string) (string, error) {
	argv := append([]string{"compose", "-f", "../../contrib/compose/cluster.yaml", "exec", "-T", robin[n].service,
		"robinmail", "-config", robin[n].config}, args...)
	cmd := exec.Command("docker", argv...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// newUser makes an account of its own for a test.
func newUser(t testing.TB, prefix string) string {
	t.Helper()
	user := fmt.Sprintf("%s-%d@%s", prefix, time.Now().UnixNano()%1_000_000_000, domain)
	robinCLI(t, 0, password()+"\n", "user", "add", user)
	return user
}

// deliverAll sends one message per entry, each on a connection of its own,
// spread over the three nodes and all at once, and returns the reply code
// each got. subject makes the subject of message i.
func deliverAll(t testing.TB, from, to string, n int, subject func(i int) string) []int {
	t.Helper()
	codes := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := dialSMTP(robin[i%len(robin)].smtp)
			if err != nil {
				codes[i] = -1
				return
			}
			defer c.close()
			<-start
			codes[i], _ = c.send(from, to, message(subject(i), to))
		}()
	}
	time.Sleep(500 * time.Millisecond) // every connection is open and greeted
	close(start)
	wg.Wait()
	return codes
}

func count(codes []int) map[int]int {
	out := map[int]int{}
	for _, c := range codes {
		out[c]++
	}
	return out
}

// (h) A quota is a guard in the batch that stores a message, and rqlite
// applies batches one after another: so an account with room for 20
// messages that is sent 90 at once, through three nodes, ends with 20,
// whatever node took which. The counters are kept by triggers in the
// same batches and say 20 too.
func quotaAcrossNodes(t *testing.T) {
	user := newUser(t, "quota")
	robinCLI(t, 0, "", "user", "quota", user, "none", "20")
	token := newStream("h").run
	codes := deliverAll(t, "sender@elsewhere.example", user, 90, func(i int) string { return fmt.Sprintf("%s-%02d", token, i) })
	got := count(codes)
	t.Logf("90 deliveries at once through three nodes into room for 20: %d got 250, %d got 452, others %v", got[250], got[452], got)
	if got[250] != 20 || got[452] != 70 {
		t.Errorf("replies: %v, want 20 times 250 and 70 times 452", got)
	}
	s := newJMAP(1).loginAs(t, user)
	check := func(what string, want int) {
		t.Helper()
		for n := range robin {
			j := newJMAP(n)
			list, err := j.emails(s.cookie, s.account)
			if err != nil || len(list) != want {
				t.Errorf("%s: %s shows %d emails (%v), want %d", what, robin[n].service, len(list), err, want)
			}
			res, err := j.call(s.cookie, "Quota/get", map[string]any{"accountId": s.account, "ids": []string{"count"}})
			if err != nil {
				t.Fatal(err)
			}
			q := res["list"].([]any)[0].(map[string]any)
			if q["used"] != float64(want) || q["hardLimit"] != float64(20) {
				t.Errorf("%s: Quota/get on %s: used %v of %v, want %d of 20", what, robin[n].service, q["used"], q["hardLimit"], want)
			}
		}
		// The counters against what is there.
		rows := rqliteRows(t, fmt.Sprintf(`SELECT a.used_count, a.used_bytes, (SELECT count(*) FROM email e WHERE e.account_id = a.id),
			(SELECT coalesce(sum(size), 0) FROM email e WHERE e.account_id = a.id) FROM account a WHERE a.id = '%s'`, s.account))
		if len(rows) != 1 || rows[0][0] != float64(want) || rows[0][0] != rows[0][2] || rows[0][1] != rows[0][3] {
			t.Errorf("%s: the counters and the rows: %v, want %d emails", what, rows, want)
		}
		t.Logf("%s: %d emails on every node; counters %v messages, %v octets; rows %v messages, %v octets", what, want,
			rows[0][0], rows[0][1], rows[0][2], rows[0][3])
	}
	check("after the 90", 20)

	// Room is made while more mail comes: five messages are destroyed
	// through one node while 30 arrive through all. Five get in, or
	// fewer when a delivery came before its room; never more.
	list, err := newJMAP(2).emails(s.cookie, s.account)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ids := []string{}
		for _, e := range list[:5] {
			ids = append(ids, e.id)
		}
		res, err := newJMAP(2).call(s.cookie, "Email/set", map[string]any{"accountId": s.account, "destroy": ids})
		if err != nil || len(res["destroyed"].([]any)) != 5 {
			t.Errorf("Email/set destroy: %v %v", res, err)
		}
	}()
	codes = deliverAll(t, "sender@elsewhere.example", user, 30, func(i int) string { return fmt.Sprintf("%s-more-%02d", token, i) })
	wg.Wait()
	got = count(codes)
	t.Logf("5 destroyed while 30 more arrive: %d got 250, %d got 452", got[250], got[452])
	if got[250] > 5 || got[250]+got[452] != 30 {
		t.Errorf("replies: %v, want at most 5 times 250 and 452 for the rest", got)
	}
	check("after making room", 15+got[250])
	// And the rest of the room is still there to use.
	codes = deliverAll(t, "sender@elsewhere.example", user, 12, func(i int) string { return fmt.Sprintf("%s-last-%02d", token, i) })
	if got2 := count(codes); got2[250] != 5-got[250] {
		t.Errorf("filling up: %v, want %d times 250", got2, 5-got[250])
	}
	check("full again", 20)
}

// (i) A Sieve script runs on whichever node takes the message. 60
// messages arrive at once through three nodes for an account whose script
// files into mailboxes that do not exist yet (fileinto :create), sets a
// flag, and answers the sender (vacation). Each mailbox is made once,
// every message is where the script puts it, and the sender gets one
// reply, not one per node.
func sieveAcrossNodes(t *testing.T) {
	sink := newSinkJMAP()
	waitFor(t, sinkService, 60*time.Second, func() bool {
		code, _, _, err := sink.do("GET", "/auth/session", "", nil)
		return err == nil && code == 401
	})
	const sinkUser = "sink@sink.test"
	receiver := sink.loginAs(t, sinkUser)

	user := newUser(t, "sieve")
	s := newJMAP(0).loginAs(t, user)
	token := newStream("i").run
	script := "require [\"fileinto\", \"mailbox\", \"imap4flags\", \"vacation\"];\r\n" +
		"vacation :days 1 :subject \"Away " + token + "\" \"I am away.\";\r\n" +
		"if header :contains \"subject\" \"kind-a\" { fileinto :create \"Alpha\"; }\r\n" +
		"elsif header :contains \"subject\" \"kind-b\" { addflag \"$beta\"; fileinto :create \"Beta/Deep\"; }\r\n"
	j := newJMAP(0)
	code, data, _, err := j.do("POST", "/jmap/upload/"+s.account+"/", s.cookie, []byte(script))
	if err != nil || code != 201 {
		t.Fatalf("upload: %d %s %v", code, data, err)
	}
	blob := regexp.MustCompile(`"blobId":"([^"]+)"`).FindSubmatch(data)
	if blob == nil {
		t.Fatalf("upload: %s", data)
	}
	res, err := j.call(s.cookie, "SieveScript/set", map[string]any{"accountId": s.account,
		"create": map[string]any{"main": map[string]any{"name": "main", "blobId": string(blob[1])}}, "onSuccessActivateScript": "#main"})
	if created, _ := res["created"].(map[string]any); err != nil || created["main"] == nil {
		t.Fatalf("SieveScript/set: %v %v", res, err)
	}

	kinds := []string{"kind-a", "kind-b", "kind-c"}
	codes := deliverAll(t, sinkUser, user, 60, func(i int) string { return fmt.Sprintf("%s %s %02d", token, kinds[i%3], i) })
	if got := count(codes); got[250] != 60 {
		t.Fatalf("replies: %v, want 60 times 250", got)
	}

	// What the account looks like, through every node.
	for n := range robin {
		j := newJMAP(n)
		res, err := j.batch(s.cookie, []any{
			[]any{"Mailbox/get", map[string]any{"accountId": s.account, "properties": []string{"name", "parentId", "totalEmails", "role"}}, "m"},
			[]any{"Email/query", map[string]any{"accountId": s.account, "limit": 500}, "q"},
			[]any{"Email/get", map[string]any{"accountId": s.account, "properties": []string{"subject", "mailboxIds", "keywords"},
				"#ids": map[string]any{"resultOf": "q", "name": "Email/query", "path": "/ids"}}, "e"},
		})
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string][]map[string]any{}
		for _, m := range res[0]["list"].([]any) {
			mb := m.(map[string]any)
			byName[mb["name"].(string)] = append(byName[mb["name"].(string)], mb)
		}
		for name, want := range map[string]float64{"Alpha": 20, "Beta": 0, "Deep": 20, "Inbox": 20} {
			if len(byName[name]) != 1 {
				t.Errorf("%s: %d mailboxes named %s, want 1", robin[n].service, len(byName[name]), name)
				continue
			}
			if got := byName[name][0]["totalEmails"]; got != want {
				t.Errorf("%s: %s has %v emails, want %v", robin[n].service, name, got, want)
			}
		}
		if len(byName["Deep"]) == 1 && len(byName["Beta"]) == 1 && byName["Deep"][0]["parentId"] != byName["Beta"][0]["id"] {
			t.Errorf("%s: Deep is not inside Beta", robin[n].service)
		}
		emails := res[2]["list"].([]any)
		if len(emails) != 60 {
			t.Errorf("%s: %d emails, want 60", robin[n].service, len(emails))
		}
		for _, e := range emails {
			em := e.(map[string]any)
			subject := em["subject"].(string)
			boxes := em["mailboxIds"].(map[string]any)
			keywords, _ := em["keywords"].(map[string]any)
			want := "Inbox"
			switch {
			case strings.Contains(subject, "kind-a"):
				want = "Alpha"
			case strings.Contains(subject, "kind-b"):
				want = "Deep"
			}
			if len(byName[want]) != 1 || len(boxes) != 1 || boxes[byName[want][0]["id"].(string)] != true {
				t.Errorf("%s: %q is in %v, want only %s", robin[n].service, subject, boxes, want)
			}
			if flagged := keywords["$beta"] == true; flagged != strings.Contains(subject, "kind-b") {
				t.Errorf("%s: %q has keywords %v", robin[n].service, subject, keywords)
			}
		}
	}
	t.Logf("60 messages at once through three nodes: Alpha and Beta/Deep were made once each; 20 in Alpha, 20 flagged in Beta/Deep, 20 in the Inbox, on every node")

	// One reply reaches the sender, although three nodes each saw the
	// first message of that sender within the same moment.
	var replies []string
	waitFor(t, "the automatic reply at the sink", 60*time.Second, func() bool {
		replies, err = sink.subjects(receiver.cookie, receiver.account, "Away "+token)
		return err == nil && len(replies) > 0
	})
	time.Sleep(5 * time.Second) // a second reply would be on its way by now
	replies, err = sink.subjects(receiver.cookie, receiver.account, "Away "+token)
	if err != nil || len(replies) != 1 {
		t.Errorf("the sender got %d automatic replies (%v), want 1", len(replies), err)
	}
	rows := rqliteRows(t, fmt.Sprintf(`SELECT count(*) FROM vacation_sent WHERE account_id = '%s'`, s.account))
	t.Logf("the sender has %d automatic reply; vacation_sent has %v row for the account", len(replies), rows[0][0])
	if rows[0][0] != float64(1) {
		t.Errorf("vacation_sent rows: %v", rows)
	}
}

// (j) A blob file on one node is damaged, as a disk does it: one byte.
// "robinmail scrub" on that node finds it, fetches the file again from a
// node that has a good one, and the message reads through that node.
func scrubRepairs(t *testing.T) {
	const victim = 1
	s := newJMAP(0).loginAs(t, users[0])
	token := newStream("j").run
	msg := message(token, s.user)
	c, err := dialSMTP(robin[0].smtp)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if code, err := c.send("sender@elsewhere.example", s.user, msg); code != 250 {
		t.Fatalf("delivery: %d %v", code, err)
	}
	j := newJMAP(victim)
	var e email
	waitFor(t, "the message", 10*time.Second, func() bool {
		list, _ := j.subjectsOf(s, token)
		if len(list) == 1 {
			e = list[0]
		}
		return len(list) == 1
	})
	path := fmt.Sprintf("%s/%s/%s/%s", blobDir, s.account, e.blob[:2], e.blob)
	for n := range robin {
		waitFor(t, "the blob on "+robin[n].service, 20*time.Second, func() bool {
			_, err := composeErr("exec", "-T", robin[n].service, "test", "-f", path)
			return err == nil
		})
	}
	good := sh(t, 0, "sha256sum < "+path)
	// One byte in the middle of the file becomes another.
	sh(t, victim, fmt.Sprintf(`size=$(wc -c < %[1]s); printf '\377\000' | dd of=%[1]s bs=1 seek=$((size / 2)) conv=notrunc 2>/dev/null`, path))
	if bad := sh(t, victim, "sha256sum < "+path); bad == good {
		t.Fatal("the file was not changed")
	}
	// The node cannot read the message from its own disk now.
	if raw, err := j.download(s.cookie, s.account, e.blob); err == nil && bytes.HasSuffix(raw, msg) {
		t.Log("the node served the message although its file is damaged: it fetched a good copy on reading")
	}
	sh(t, victim, fmt.Sprintf(`size=$(wc -c < %[1]s); printf '\377\000' | dd of=%[1]s bs=1 seek=$((size / 2)) conv=notrunc 2>/dev/null`, path))

	out, err := robinCLIErr(victim, "", "scrub")
	t.Logf("robinmail scrub on %s: %s", robin[victim].service, strings.TrimSpace(out))
	m := regexp.MustCompile(`(\d+) blob files checked, (\d+) corrupt, (\d+) repaired from another node`).FindStringSubmatch(out)
	if err != nil || m == nil || m[2] != "1" || m[3] != "1" {
		t.Fatalf("scrub: %v\n%s", err, out)
	}
	if after := sh(t, victim, "sha256sum < "+path); after != good {
		t.Errorf("the file after the scrub is not the file the other nodes have")
	}
	raw, err := j.download(s.cookie, s.account, e.blob)
	if err != nil || !bytes.HasSuffix(raw, msg) {
		t.Errorf("reading the message through %s after the scrub: %v", robin[victim].service, err)
	}
	// A second scrub finds nothing.
	out = robinCLI(t, victim, "", "scrub")
	if !strings.Contains(out, " 0 corrupt, 0 repaired") {
		t.Errorf("a second scrub: %s", out)
	}
	if !strings.Contains(robinCLI(t, victim, "", "audit"), "scrub") {
		t.Error("the audit log has no line for the scrub that found something")
	}
}

// subjectsOf returns the account's emails whose subject contains the text.
func (j *jmap) subjectsOf(s session, text string) ([]email, error) {
	list, err := j.emails(s.cookie, s.account)
	if err != nil {
		return nil, err
	}
	var out []email
	for _, e := range list {
		if strings.Contains(e.subject, text) {
			out = append(out, e)
		}
	}
	return out, nil
}
