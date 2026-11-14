package clustertest

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCluster runs the phase 4 exit tests, in order, against the cluster
// of contrib/compose/cluster.yaml. Each part leaves the cluster whole for
// the next.
func TestCluster(t *testing.T) {
	if os.Getenv("ROBINMAIL_TEST_CLUSTER") == "" {
		t.Skip("ROBINMAIL_TEST_CLUSTER is not set; see doc.go")
	}
	for n := range robin {
		waitFor(t, robin[n].service, 60*time.Second, func() bool { return nodeUp(n) == nil })
	}
	if got := rqliteReachable(); got != 3 {
		t.Fatalf("%d of 3 rqlite nodes reachable", got)
	}
	sessions := logins(t, 0)

	t.Run("a: rqlite leader killed and restarted during deliveries", func(t *testing.T) { leaderKilled(t, sessions) })
	t.Run("b: robinmail node killed during deliveries", func(t *testing.T) { nodeKilled(t, sessions) })
	t.Run("c: node with a wiped blob directory catches up", func(t *testing.T) { wipedNode(t, sessions) })
	t.Run("d: read through a node that lacks the blob", func(t *testing.T) { readWithoutBlob(t, sessions) })
	t.Run("e: throughput", func(t *testing.T) { throughput(t, sessions) })
}

func report(t *testing.T, st *stream, d time.Duration) {
	t.Helper()
	acked, unacked, temp, conn := st.counts()
	st.mu.Lock()
	gap, other := st.gap, st.other
	perNode := make([]int, len(robin))
	for _, m := range st.acked {
		perNode[m.node]++
	}
	st.mu.Unlock()
	t.Logf("stream %s: %d messages got 250 in %s (%.0f/s; per node %v), %d never did; %d answers 4xx, %d attempts without an answer; longest time without a 250: %s",
		st.run, acked, d.Round(time.Second), float64(acked)/d.Seconds(), perNode, unacked, temp, conn, gap.Round(time.Millisecond))
	for _, o := range other {
		t.Errorf("unexpected answer: %s", o)
	}
	if unacked != 0 {
		t.Errorf("%d messages never got 250 although the cluster was whole again", unacked)
	}
}

// (a) A steady stream of deliveries to four accounts through all three
// robinmail nodes. The rqlite leader is killed in the middle and started
// again later. Afterwards every message that got 250 exists exactly once
// and reads back through every robinmail node.
func leaderKilled(t *testing.T, sessions []session) {
	st := newStream("a")
	stop := make(chan struct{})
	start := time.Now()
	wg := st.start(6, stop, 90*time.Second)

	time.Sleep(5 * time.Second)
	leader := rqliteLeader(t)
	before, _, _, _ := st.counts()
	t.Logf("after 5s: %d delivered; killing the rqlite leader, rqlite-%d", before, leader+1)
	compose(t, "kill", fmt.Sprintf("rqlite-%d", leader+1))
	killed := time.Now()

	time.Sleep(15 * time.Second)
	during, _, _, _ := st.counts()
	next := rqliteLeader(t)
	if next == leader {
		t.Errorf("rqlite-%d still leads after it was killed", leader+1)
	}
	t.Logf("15s later: %d more delivered with two rqlite nodes, rqlite-%d leads; starting rqlite-%d again", during-before, next+1, leader+1)
	if during == before {
		t.Error("no delivery succeeded while the old leader was down")
	}
	compose(t, "start", fmt.Sprintf("rqlite-%d", leader+1))
	rejoin := waitFor(t, "rqlite to have three reachable nodes again", 60*time.Second, func() bool { return rqliteReachable() == 3 })
	t.Logf("rqlite-%d was down for %s and rejoined %s after its start", leader+1, time.Since(killed).Round(time.Second), rejoin.Round(time.Millisecond))

	time.Sleep(5 * time.Second)
	close(stop)
	wg.Wait()
	report(t, st, time.Since(start))
	verify(t, st, sessions, 0, 1, 2)
}

// (b) The same stream while a robinmail node is killed. Mail it was given
// and acknowledged is not lost and not doubled, the senders go on through
// the other two nodes, and a web session made on the killed node keeps
// working on the others with the same cookie. When the node is back it
// has, or fetches, what it missed.
func nodeKilled(t *testing.T, sessions []session) {
	const victim = 1
	// A browser logs in through the node that is about to die.
	cookie, account, err := newJMAP(victim).login(users[0])
	if err != nil {
		t.Fatal(err)
	}

	st := newStream("b")
	stop := make(chan struct{})
	start := time.Now()
	wg := st.start(6, stop, 90*time.Second)
	time.Sleep(5 * time.Second)
	before, _, _, _ := st.counts()
	t.Logf("after 5s: %d delivered; killing %s", before, robin[victim].service)
	compose(t, "kill", robin[victim].service)

	// The session of the dead node, on the two that are left.
	for _, n := range []int{0, 2} {
		j := newJMAP(n)
		code, body, _, err := j.do("GET", "/auth/session", cookie, nil)
		if err != nil || code != 200 || !bytes.Contains(body, []byte(account)) {
			t.Errorf("session from %s on %s: %d %s %v", robin[victim].service, robin[n].service, code, body, err)
		}
		res, err := j.call(cookie, "Mailbox/get", map[string]any{"accountId": account, "ids": nil})
		if list, _ := res["list"].([]any); err != nil || len(list) < 5 {
			t.Errorf("Mailbox/get on %s with the cookie from %s: %v %v", robin[n].service, robin[victim].service, res, err)
		}
		// What the webmail does all day: ask for changes and act on mail.
		list, err := j.emails(cookie, account)
		if err != nil || len(list) == 0 {
			t.Fatalf("emails on %s: %d %v", robin[n].service, len(list), err)
		}
		res, err = j.call(cookie, "Email/set", map[string]any{"accountId": account,
			"update": map[string]any{list[0].id: map[string]any{"keywords/$seen": true}}})
		if _, ok := res["updated"].(map[string]any)[list[0].id]; err != nil || !ok {
			t.Errorf("Email/set on %s: %v %v", robin[n].service, res, err)
		}
	}
	t.Logf("the session cookie from %s works on %s and %s: session, Mailbox/get, Email/query, Email/get, Email/set",
		robin[victim].service, robin[0].service, robin[2].service)

	time.Sleep(10 * time.Second)
	during, _, _, _ := st.counts()
	t.Logf("10s later: %d more delivered through two nodes", during-before)
	if during == before {
		t.Error("no delivery succeeded while the node was down")
	}
	// Checked while the node is still dead: only the survivors can answer.
	close(stop)
	wg.Wait()
	report(t, st, time.Since(start))
	verify(t, st, sessions, 0, 2)

	// With one node dead, "on 2 nodes before 250" means on both that are
	// left: each survivor holds a blob for every email there is.
	emails := totalEmails(t, sessions, 0)
	for _, n := range []int{0, 2} {
		have := blobCount(t, n)
		t.Logf("%s holds %d blob files for %d emails", robin[n].service, have, emails)
		if have < emails {
			t.Errorf("%s lacks %d blobs of acknowledged mail", robin[n].service, emails-have)
		}
	}

	compose(t, "start", robin[victim].service)
	waitFor(t, robin[victim].service+" to answer again", 60*time.Second, func() bool { return nodeUp(victim) == nil })
	// It catches up by itself, before anyone reads through it: the repair
	// pass at start fetches the blobs of the mail it missed.
	total := totalEmails(t, sessions, 0)
	had := blobCount(t, victim)
	took := waitFor(t, "the returned node to have every referenced blob", 2*time.Minute, func() bool {
		return blobCount(t, victim) >= total
	})
	t.Logf("%s is back: %d blob files when it answered again, %d (for %d emails) %s later; reading everything through it",
		robin[victim].service, had, blobCount(t, victim), total, took.Round(100*time.Millisecond))
	verify(t, st, sessions, victim)
}

// (c) A node loses its blob directory. Its repair pass fetches every blob
// the database refers to from the other nodes. Proof that it has them
// all: the other two robinmail nodes are killed and every message is read
// through the wiped node alone.
func wipedNode(t *testing.T, sessions []session) {
	const victim = 2
	// Everything delivered so far, by all parts of the test.
	total := totalEmails(t, sessions, 0)
	had := blobCount(t, victim)
	sh(t, victim, "rm -rf "+blobDir)
	if left := blobCount(t, victim); left > had/2 {
		t.Fatalf("after the wipe %s still has %d blob files", robin[victim].service, left)
	}
	t.Logf("%s had %d blob files for %d emails; wiped its blob directory", robin[victim].service, had, total)

	took := waitFor(t, "the wiped node to have every referenced blob", 3*time.Minute, func() bool {
		return blobCount(t, victim) >= total
	})
	now := blobCount(t, victim)
	t.Logf("%s after the wipe: %d blob files, all %d referenced ones, %s later (repair runs every 15s)",
		robin[victim].service, now, total, took.Round(time.Second))
	if now != total {
		// Unreferenced files (a retry's second blob) are not fetched.
		t.Errorf("%s has %d blob files, want exactly the %d that emails refer to", robin[victim].service, now, total)
	}
	out := compose(t, "exec", "-T", robin[victim].service, "robinmail", "-config", robin[victim].config, "repair")
	t.Logf("robinmail repair on %s: %s", robin[victim].service, strings.TrimSpace(out))
	if !strings.Contains(out, "0 blobs were missing") {
		t.Errorf("repair still found something missing")
	}

	// Alone: nobody to fetch from.
	compose(t, "kill", robin[0].service, robin[1].service)
	j := newJMAP(victim)
	read, bad := 0, 0
	for _, s := range sessions {
		list, err := j.emails(s.cookie, s.account)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list {
			raw, err := j.download(s.cookie, s.account, e.blob)
			if err != nil || !bytes.Contains(raw, []byte("token "+e.subject+"\r\n")) {
				bad++
				if bad <= 5 {
					t.Errorf("%s alone cannot read %s: %v", robin[victim].service, e.subject, err)
				}
			}
			read++
		}
	}
	t.Logf("with %s and %s killed, %s alone read %d of %d messages from its own disk",
		robin[0].service, robin[1].service, robin[victim].service, read-bad, read)
	if read != total {
		t.Errorf("read %d messages, want %d", read, total)
	}
	compose(t, "start", robin[0].service, robin[1].service)
	for _, n := range []int{0, 1} {
		waitFor(t, robin[n].service, 60*time.Second, func() bool { return nodeUp(n) == nil })
	}
}

// (d) A message is read through a node that does not have its blob: the
// file is removed from that node's disk and the message is downloaded
// through it at once. The node fetches the blob from another node, serves
// it and keeps it.
func readWithoutBlob(t *testing.T, sessions []session) {
	const via = 1
	s := sessions[0]
	// A new message, delivered through another node.
	token := newStream("d").run
	msg := message(token, s.user)
	c, err := dialSMTP(robin[0].smtp)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if code, err := c.send("sender@elsewhere.example", s.user, msg); code != 250 {
		t.Fatalf("delivery: %d %v", code, err)
	}
	j := newJMAP(via)
	var e email
	waitFor(t, "the message", 10*time.Second, func() bool {
		list, _ := j.emails(s.cookie, s.account)
		for _, x := range list {
			if x.subject == token {
				e = x
				return true
			}
		}
		return false
	})
	path := fmt.Sprintf("%s/%s/%s/%s", blobDir, s.account, e.blob[:2], e.blob)
	// The push to this node may still be on its way: wait for it, then
	// take the file away.
	waitFor(t, "the blob on "+robin[via].service, 10*time.Second, func() bool {
		_, err := composeErr("exec", "-T", robin[via].service, "test", "-f", path)
		return err == nil
	})
	sh(t, via, "rm "+path)
	if _, err := composeErr("exec", "-T", robin[via].service, "test", "-e", path); err == nil {
		t.Fatal("the blob file is still there")
	}
	t.Logf("removed %s on %s", path, robin[via].service)

	start := time.Now()
	raw, err := j.download(s.cookie, s.account, e.blob)
	took := time.Since(start)
	if err != nil || !bytes.HasSuffix(raw, msg) {
		t.Fatalf("download through the node without the blob: %v", err)
	}
	if _, err := composeErr("exec", "-T", robin[via].service, "test", "-f", path); err != nil {
		t.Error("the node did not keep the blob it fetched")
	}
	logs := compose(t, "logs", "--since", "1m", robin[via].service)
	fetched := ""
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "blob fetched") && strings.Contains(line, e.blob) {
			fetched = line[strings.Index(line, "msg="):]
		}
	}
	if fetched == "" {
		t.Error("the node's log does not show the fetch")
	}
	t.Logf("read it through %s in %s, %d bytes as sent; the file is back; its log: %s",
		robin[via].service, took.Round(time.Millisecond), len(raw), fetched)
	// Also over JMAP proper, not only the download.
	res, err := j.call(s.cookie, "Email/get", map[string]any{"accountId": s.account, "ids": []string{e.id},
		"properties": []string{"textBody", "bodyValues"}, "fetchTextBodyValues": true})
	if err != nil || !strings.Contains(fmt.Sprint(res["list"]), "token "+token) {
		t.Errorf("Email/get with body values: %v %v", res, err)
	}
}

// (e) Deliveries per second through the three-node cluster, at several
// numbers of concurrent senders spread over the three nodes. Each
// delivery is a full SMTP transaction that ends, before its 250, with the
// blob on two nodes and one write through rqlite's Raft log.
func throughput(t *testing.T, sessions []session) {
	for _, workers := range []int{3, 12, 48} {
		st := newStream("e")
		stop := make(chan struct{})
		start := time.Now()
		wg := st.start(workers, stop, 60*time.Second)
		time.Sleep(10 * time.Second)
		close(stop)
		wg.Wait()
		d := time.Since(start)
		acked, unacked, temp, conn := st.counts()
		t.Logf("%2d senders: %5d deliveries in %s = %.0f per second (%d 4xx, %d without an answer, %d not delivered)",
			workers, acked, d.Round(100*time.Millisecond), float64(acked)/d.Seconds(), temp, conn, unacked)
		if unacked != 0 || temp != 0 || conn != 0 {
			t.Errorf("a healthy cluster refused mail")
		}
		if workers == 48 {
			verify(t, st, sessions, 0)
		}
	}
}
