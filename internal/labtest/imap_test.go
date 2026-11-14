package labtest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// robinmail's IMAP server against clients that are not ours, in the
// container "imapclients" of the lab: Dovecot's ImapTest (its scripted
// tests and its stress test), mbsync and OfflineIMAP. The clients reach
// mx.robin.test:993 inside the lab and check its certificate against the
// lab CA; the harness looks at the same accounts over JMAP.

const clients = "imapclients"

func imapTest(args ...string) string {
	cmd := "cd /tmp && rm -rf imaptest-run && mkdir imaptest-run && cd imaptest-run && imaptest host=" + robinHost +
		" port=993 ssl user=imaptest@" + robinDomain + " pass='" + password() + "' " + strings.Join(args, " ") + " 2>&1"
	out, _ := execIn(clients, "", nil, "sh", "-c", cmd)
	return out
}

// imapTestKnown are the groups of ImapTest's scripted tests in which
// commands fail, how many, and why that is left as it is.
var imapTestKnown = map[string]struct {
	commands int
	why      string
}{
	"select":       {3, `expects \Recent and a RECENT count: robinmail keeps no \Recent flag (IMAP4rev2 dropped it), RECENT is always 0`},
	"copy":         {4, `the same: expects \Recent on copied messages`},
	"search-flags": {3, `the same: SEARCH NEW, OLD and RECENT`},
	"search-body": {6, `searches for parts of words ("sdfghjk" in "asdfghjkl"): since phase 8 SEARCH BODY and TEXT ask the ` +
		`full-text index, which knows whole words, not substrings (RFC 3501 6.4.4 has substrings; Dovecot with an index ` +
		`answers these the same way)`},
	"listext": {1, `expects a deleted mailbox that was subscribed to stay in the list as \NonExistent: ` +
		`a subscription here is a property of the mailbox and goes with it`},
	"fetch-envelope": {1, `expects the source route of "<@route:user@domain>" in the ENVELOPE: ` +
		`routes are obsolete (RFC 5322 4.4) and internal/message drops them`},
	"append-binary": {1, `a message appended with "Content-Transfer-Encoding: binary" and NUL octets is stored and ` +
		`returned as it is; ImapTest wants BODY[1] of it re-encoded. Mail from SMTP cannot have NULs`},
}

var (
	imapTestFail    = regexp.MustCompile(`(?m)^\*\*\* Test (\S+) command (\d+)/(\d+)`)
	imapTestSummary = regexp.MustCompile(`(\d+) test groups: (\d+) failed, (\d+) skipped due to missing capabilities`)
	imapTestCounts  = regexp.MustCompile(`(base protocol|extensions): (\d+)/(\d+) individual commands failed`)
)

// TestImapTestScripts runs ImapTest's scripted tests (its src/tests): a
// few hundred commands with the answers Dovecot's authors expect. Every
// failure is either fixed in robinmail or listed above with its reason;
// a failure that is not listed fails this test.
//
// What the first runs found and what was fixed is in PLAN.md (phase 6).
func TestImapTestScripts(t *testing.T) {
	lab(t)
	out := imapTest("test=/lab/imaptest/tests")
	sum := imapTestSummary.FindStringSubmatch(out)
	if sum == nil {
		t.Fatalf("ImapTest did not finish:\n%s", clip(out, 4000))
	}
	failed := map[string]int{}
	for _, m := range imapTestFail.FindAllStringSubmatch(out, -1) {
		failed[m[1]]++
	}
	for _, block := range strings.Split(out, "\n\n") {
		if strings.HasPrefix(block, "*** Test") {
			t.Logf("%s", block)
		}
	}
	names := make([]string, 0, len(failed))
	for g := range failed {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		known, ok := imapTestKnown[g]
		switch {
		case !ok:
			t.Errorf("group %s: %d commands failed and it is not a known failure", g, failed[g])
		case failed[g] > known.commands:
			t.Errorf("group %s: %d commands failed, %d are known (%s)", g, failed[g], known.commands, known.why)
		default:
			t.Logf("known: %-15s %d command(s): %s", g, failed[g], known.why)
		}
	}
	for g := range imapTestKnown {
		if failed[g] == 0 {
			t.Errorf("group %s passes now: take it off the list of known failures", g)
		}
	}
	groups, _ := strconv.Atoi(sum[1])
	nfailed, _ := strconv.Atoi(sum[2])
	skipped, _ := strconv.Atoi(sum[3])
	t.Logf("ImapTest scripts: %d groups, %d pass, %d fail (all known), %d skipped for capabilities robinmail does not offer",
		groups, groups-nfailed-skipped, nfailed, skipped)
	for _, c := range imapTestCounts.FindAllStringSubmatch(out, -1) {
		t.Logf("ImapTest scripts: %s: %s of %s commands failed", c[1], c[2], c[3])
	}
	if groups-nfailed-skipped < 30 {
		t.Errorf("only %d groups passed", groups-nfailed-skipped)
	}
	observe("ImapTest scripts: groups passed / failed (known) / skipped", "robinmail",
		fmt.Sprintf("%d / %d / %d", groups-nfailed-skipped, nfailed, skipped))
}

var (
	stressError = regexp.MustCompile(`(?m)^Error: \S+\[(\d+)\]: (.*?): (\d+\.\d+) `)
	stressTotal = regexp.MustCompile(`(?m)^Totals:\n(.*)\n.*\n.*\n(.*)$`)
)

// TestImapTestStress is ImapTest's stress test: 20 clients on one account
// for 30 seconds, logging in, appending, fetching, searching, storing
// flags, copying, deleting and expunging at random, each checking what
// the server says against the state it has seen so far (flags that change
// without a FETCH, sequence numbers that do not add up, messages that
// come back).
//
// One kind of complaint is expected and checked for what it is: "SEARCH
// result missing" for a search by header or body text. A message that
// another client expunged a moment ago is still in this client's view
// until an EXPUNGE may be sent, which is not during SEARCH (RFC 3501
// 7.4.1). robinmail still finds it by number, UID, flags, size and date,
// which the session holds; its text went with the message. Dovecot keeps
// the file until the last session lets go of it.
func TestImapTestStress(t *testing.T) {
	lab(t)
	// The mailbox the clients copy into is made first. Left to them, two
	// clients that get TRYCREATE at the same moment both CREATE it, and
	// ImapTest counts the ALREADYEXISTS of the second as a failed COPY.
	if j := newJMAP(t, "imaptest@"+robinDomain); j.mailboxes(t)["imaptest-copy"] == "" {
		j.createMailbox(t, "imaptest-copy", "")
	}
	out := imapTest("mbox=/lab/imaptest/messages.mbox", "secs=30", "clients=20", "seed=7", "copybox=imaptest-copy",
		"copy=30", "uidfetch=30", "search=40", "noop=20", "check=10", "rawlog")
	tot := stressTotal.FindStringSubmatch(out)
	if tot == nil {
		t.Fatalf("ImapTest did not finish:\n%s", clip(out, 4000))
	}
	names, counts := strings.Fields(tot[1]), strings.Fields(tot[2])
	var total int
	var table []string
	for i := range names {
		if i < len(counts) {
			n, _ := strconv.Atoi(counts[i])
			total += n
			table = append(table, names[i]+" "+counts[i])
		}
	}
	t.Logf("ImapTest stress: %d commands in 30 seconds from 20 clients: %s", total, strings.Join(table, ", "))
	if total < 2000 {
		t.Errorf("only %d commands ran", total)
	}
	errs := stressError.FindAllStringSubmatch(out, -1)
	other := 0
	for _, line := range strings.Split(out, "\n") {
		if (strings.HasPrefix(line, "Error:") || strings.HasPrefix(line, "Fatal:") || strings.HasPrefix(line, "Panic:")) &&
			!strings.Contains(line, "SEARCH result missing") {
			other++
			t.Errorf("ImapTest: %s", line)
		}
	}
	// Every missing search result must be a search by text.
	textKey := regexp.MustCompile(`(?i) (TEXT|BODY|SUBJECT|FROM|TO|CC|BCC|HEADER|SENTON|SENTBEFORE|SENTSINCE) `)
	byText := 0
	seen := map[string]bool{}
	for _, e := range errs {
		if !strings.HasPrefix(e[2], "SEARCH result missing") || seen[e[3]] {
			continue
		}
		seen[e[3]] = true
		cmd, _ := execIn(clients, "", nil, "sh", "-c", "cd /tmp/imaptest-run && grep -h -m1 ' O: "+e[3]+" ' rawlog.* | head -1")
		if !textKey.MatchString(cmd + " ") {
			t.Errorf("a search without a text key missed a message: %s", strings.TrimSpace(cmd))
			continue
		}
		byText++
	}
	t.Logf("ImapTest stress: %d complaints other than missing search results; %d searches by text missed a message that was expunged under them",
		other, byText)
	observe("ImapTest stress: commands / errors / text searches of expunged messages", "robinmail",
		fmt.Sprintf("%d / %d / %d", total, other, byText))
	// The server is still well: no panic, no error in its log.
	logs := compose(t, "logs", "--since", "2m", "robinmail")
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "panic") || (strings.Contains(line, "level=ERROR") && strings.Contains(line, "imap")) {
			t.Errorf("robinmail log: %s", line)
		}
	}
}

// ---- sync clients ----

// syncClient is a program that keeps a Maildir in step with an IMAP
// account.
type syncClient struct {
	name string
	user string
	root string // the Maildir tree in the container
	// config is written to file before the first run.
	file, config string
	run          []string
	// folder turns a directory under root into an IMAP mailbox name.
	folder func(dir string) string
	// dir is the reverse.
	dir func(folder string) string
}

var syncClients = []syncClient{
	{
		name: "mbsync", user: "mbsync@" + robinDomain, root: "/root/mbsync", file: "/root/.mbsyncrc",
		config: `IMAPAccount robin
Host mx.robin.test
Port 993
User mbsync@robin.test
Pass PASSWORD
TLSType IMAPS
CertificateFile /etc/ssl/certs/ca-certificates.crt

IMAPStore remote
Account robin

MaildirStore local
Path /root/mbsync/
Inbox /root/mbsync/INBOX
SubFolders Verbatim

Channel robin
Far :remote:
Near :local:
Patterns *
Create Both
Remove Both
Expunge Both
SyncState *
`,
		run:    []string{"mbsync", "-a"},
		folder: func(dir string) string { return dir },
		dir:    func(folder string) string { return folder },
	},
	{
		name: "offlineimap", user: "offlineimap@" + robinDomain, root: "/root/offlineimap", file: "/root/.offlineimaprc",
		config: `[general]
accounts = robin
metadata = /root/.offlineimap

[Account robin]
localrepository = local
remoterepository = remote

[Repository local]
type = Maildir
localfolders = /root/offlineimap

[Repository remote]
type = IMAP
remotehost = mx.robin.test
remoteport = 993
ssl = yes
sslcacertfile = /etc/ssl/certs/ca-certificates.crt
remoteuser = offlineimap@robin.test
remotepass = PASSWORD
expunge = yes
`,
		run:    []string{"offlineimap", "-c", "/root/.offlineimaprc", "-o", "-u", "basic"},
		folder: func(dir string) string { return strings.ReplaceAll(dir, ".", "/") },
		dir:    func(folder string) string { return strings.ReplaceAll(folder, "/", ".") },
	},
}

// copyOf is one message in one folder: what it is and how it is flagged.
type copyOf struct {
	hash  string // SHA-256 of the message with LF line ends
	flags string // Maildir flags, sorted: D draft, F flagged, R answered, S seen, T deleted
}

// normal is a message as the two sides can be compared: LF line ends, and
// without the header mbsync puts on a message it uploads to find it again.
func normal(raw []byte) []byte {
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	var out []byte
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("X-TUID: ")) {
			out = append(out, line...)
		}
	}
	return out
}

func hashOf(raw []byte) string {
	sum := sha256.Sum256(normal(raw))
	return hex.EncodeToString(sum[:])
}

func sh(t testing.TB, stdin []byte, script string) string {
	t.Helper()
	out, err := execIn(clients, "", stdin, "sh", "-c", script)
	if err != nil {
		t.Fatalf("in %s: %s: %v\n%s", clients, clip(script, 200), err, out)
	}
	return out
}

// local reads the client's Maildir tree: per folder, its messages, and
// the file of each.
func (c syncClient) local(t testing.TB) (map[string][]copyOf, map[string]string) {
	t.Helper()
	out := sh(t, nil, `cd `+c.root+` 2>/dev/null || exit 0
find . -type f \( -path '*/cur/*' -o -path '*/new/*' \) | while read -r f; do
	printf '%s\t%s\n' "$f" "$(sed 's/\r$//' "$f" | grep -v '^X-TUID: ' | sha256sum | cut -d' ' -f1)"
done`)
	folders := map[string][]copyOf{}
	files := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		path, hash, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		path = strings.TrimPrefix(path, "./")
		i := strings.LastIndex(path, "/cur/")
		if j := strings.LastIndex(path, "/new/"); j > i {
			i = j
		}
		folder := c.folder(path[:i])
		flags := ""
		if k := strings.LastIndex(path, ":2,"); k >= 0 {
			flags = path[k+3:]
		}
		folders[folder] = append(folders[folder], copyOf{hash, flags})
		files[folder+" "+hash] = c.root + "/" + path
	}
	return sorted(folders), files
}

func sorted(m map[string][]copyOf) map[string][]copyOf {
	for _, list := range m {
		sort.Slice(list, func(i, j int) bool {
			if list[i].hash != list[j].hash {
				return list[i].hash < list[j].hash
			}
			return list[i].flags < list[j].flags
		})
	}
	return m
}

// mailboxes returns the account's mailboxes as IMAP names them.
func (j *jmap) mailboxes(t testing.TB) map[string]string {
	t.Helper()
	res := j.call(t, []any{"Mailbox/get", map[string]any{"accountId": j.account, "properties": []string{"name", "parentId", "role"}}, "0"})
	type mb struct{ name, parent, role string }
	all := map[string]mb{}
	for _, m := range res[0].args["list"].([]any) {
		x := m.(map[string]any)
		parent, _ := x["parentId"].(string)
		role, _ := x["role"].(string)
		all[x["id"].(string)] = mb{x["name"].(string), parent, role}
	}
	var path func(id string) string
	path = func(id string) string {
		m := all[id]
		name := m.name
		if m.role == "inbox" {
			name = "INBOX"
		}
		if m.parent == "" {
			return name
		}
		return path(m.parent) + "/" + name
	}
	out := map[string]string{}
	for id := range all {
		out[path(id)] = id
	}
	return out
}

// remote reads the account over JMAP: per mailbox, its messages, and the
// id of each.
func (j *jmap) remote(t testing.TB) (map[string][]copyOf, map[string]string) {
	t.Helper()
	folders := map[string][]copyOf{}
	ids := map[string]string{}
	for name, box := range j.mailboxes(t) {
		res := j.call(t,
			[]any{"Email/query", map[string]any{"accountId": j.account, "filter": map[string]any{"inMailbox": box}, "limit": 500}, "0"},
			[]any{"Email/get", map[string]any{"accountId": j.account, "properties": []string{"blobId", "keywords"},
				"#ids": map[string]any{"resultOf": "0", "name": "Email/query", "path": "/ids"}}, "1"})
		for _, e := range res[1].args["list"].([]any) {
			em := e.(map[string]any)
			flags := ""
			kw, _ := em["keywords"].(map[string]any)
			for _, f := range []struct{ keyword, flag string }{{"$draft", "D"}, {"$flagged", "F"}, {"$answered", "R"}, {"$seen", "S"}, {"$deleted", "T"}} {
				if kw[f.keyword] == true {
					flags += f.flag
				}
			}
			hash := hashOf(j.download(t, em["blobId"].(string)))
			folders[name] = append(folders[name], copyOf{hash, flags})
			ids[name+" "+hash] = em["id"].(string)
		}
	}
	return sorted(folders), ids
}

// empty takes everything out of the account: a lab that is kept between
// runs starts each run from nothing.
func (j *jmap) empty(t testing.TB) {
	t.Helper()
	for {
		res := j.call(t, []any{"Email/query", map[string]any{"accountId": j.account, "limit": 400}, "0"})
		ids := res[0].args["ids"].([]any)
		if len(ids) == 0 {
			break
		}
		j.call(t, []any{"Email/set", map[string]any{"accountId": j.account, "destroy": ids}, "0"})
	}
	boxes := j.mailboxes(t)
	var names []string
	for name := range boxes {
		names = append(names, name)
	}
	sort.Slice(names, func(a, b int) bool { return len(names[a]) > len(names[b]) }) // children first
	roles := map[string]bool{}
	for _, id := range j.box {
		roles[id] = true
	}
	for _, name := range names {
		if id := boxes[name]; !roles[id] {
			j.call(t, []any{"Mailbox/set", map[string]any{"accountId": j.account, "destroy": []string{id}, "onDestroyRemoveEmails": true}, "0"})
		}
	}
}

// put stores a message in a mailbox, with keywords, as an import.
func (j *jmap) put(t testing.TB, mailbox string, raw []byte, keywords ...string) string {
	t.Helper()
	kw := map[string]bool{}
	for _, k := range keywords {
		kw[k] = true
	}
	blob := j.upload(t, raw, "message/rfc822")
	res := j.call(t, []any{"Email/import", map[string]any{"accountId": j.account, "emails": map[string]any{
		"m": map[string]any{"blobId": blob, "mailboxIds": map[string]bool{mailbox: true}, "keywords": kw}}}, "0"})
	created, _ := res[0].args["created"].(map[string]any)
	m, _ := created["m"].(map[string]any)
	if m == nil {
		t.Fatalf("Email/import: %v", res[0].args)
	}
	return m["id"].(string)
}

func (j *jmap) createMailbox(t testing.TB, name, parent string) string {
	t.Helper()
	args := map[string]any{"name": name}
	if parent != "" {
		args["parentId"] = parent
	}
	res := j.call(t, []any{"Mailbox/set", map[string]any{"accountId": j.account, "create": map[string]any{"b": args}}, "0"})
	created, _ := res[0].args["created"].(map[string]any)
	b, _ := created["b"].(map[string]any)
	if b == nil {
		t.Fatalf("Mailbox/set: %v", res[0].args)
	}
	return b["id"].(string)
}

func (j *jmap) state(t testing.TB) string {
	t.Helper()
	res := j.call(t, []any{"Email/get", map[string]any{"accountId": j.account, "ids": []string{}}, "0"})
	return res[0].args["state"].(string)
}

// syncMessage makes a message with a body of its own kind.
func syncMessage(token, subject, kind string) []byte {
	head := "From: Sender <sender@postfix.test>\r\nTo: user@robin.test\r\nSubject: " + subject + "\r\n" +
		"Date: Fri, 13 Nov 2026 10:00:00 +0100\r\nMessage-ID: <" + token + "@postfix.test>\r\nX-Lab: " + token + "\r\nMIME-Version: 1.0\r\n"
	switch kind {
	case "utf8":
		return []byte(head + "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\nGrüße aus Köln, " + token + "\r\n日本語のテキスト\r\n")
	case "multipart":
		return []byte(head + "Content-Type: multipart/mixed; boundary=\"b-" + token + "\"\r\n\r\n" +
			"--b-" + token + "\r\nContent-Type: text/plain\r\n\r\nSee the attachment.\r\n" +
			"--b-" + token + "\r\nContent-Type: application/octet-stream; name=\"data.bin\"\r\nContent-Disposition: attachment; filename=\"data.bin\"\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\nAAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\r\n--b-" + token + "--\r\n")
	case "long":
		return []byte(head + "Content-Type: text/plain\r\n\r\n" + strings.Repeat("a line of ordinary text that is repeated many times\r\n", 400) +
			".\r\n..a line that starts with dots\r\nFrom the start of a line\r\n")
	}
	return []byte(head + "Content-Type: text/plain\r\n\r\nPlain text, " + token + ".\r\n")
}

// TestSyncClients has mbsync and OfflineIMAP each keep a Maildir in step
// with an account, and compares the two sides over JMAP after every step:
// which messages are in which mailbox, byte for byte, and with which
// flags.
//
//  1. The account is filled over JMAP: mail in the Inbox, in a mailbox and
//     in a mailbox inside it, with flags. The first sync brings all of it.
//  2. Both sides change. On the server, over JMAP: a flag is set, a
//     message destroyed, a new one arrives, one moves to another mailbox,
//     a mailbox is made. In the Maildir: a message is marked seen, one is
//     deleted, a new one is put there.
//  3. The second sync carries the changes both ways.
//  4. A third sync finds nothing to do and changes nothing.
func TestSyncClients(t *testing.T) {
	lab(t)
	for _, c := range syncClients {
		t.Run(c.name, func(t *testing.T) {
			j := newJMAP(t, c.user)
			j.empty(t)
			sh(t, []byte(strings.ReplaceAll(c.config, "PASSWORD", password())),
				"rm -rf "+c.root+" /root/.mbsync /root/.offlineimap /root/.local/share/isync /root/.local/state/isync; mkdir -p "+c.root+
					"; cat > "+c.file+"; chmod 600 "+c.file)
			sync := func(step string) {
				t.Helper()
				start := time.Now()
				out, err := execIn(clients, "", nil, c.run...)
				if err != nil {
					t.Fatalf("%s, %s: %v\n%s", c.name, step, err, clip(out, 3000))
				}
				t.Logf("%s, %s: %v", c.name, step, time.Since(start).Round(time.Millisecond))
			}
			check := func(step string) (map[string][]copyOf, map[string]string, map[string]string) {
				t.Helper()
				remote, ids := j.remote(t)
				local, files := c.local(t)
				n := 0
				for folder, want := range remote {
					n += len(want)
					if !reflect.DeepEqual(local[folder], want) {
						t.Errorf("%s: mailbox %s differs\n  over JMAP: %v\n  in the Maildir: %v", step, folder, want, local[folder])
					}
				}
				for folder, have := range local {
					if _, ok := remote[folder]; !ok {
						t.Errorf("%s: the Maildir has %d messages in %s, the server has no such mailbox with mail", step, len(have), folder)
					}
				}
				t.Logf("%s: %d messages in %d mailboxes, the same on both sides", step, n, len(remote))
				return remote, ids, files
			}

			// 1. Fill and sync.
			tok := newToken()
			inbox := j.box["inbox"]
			projects := j.createMailbox(t, "Projects", "")
			alpha := j.createMailbox(t, "Alpha", projects)
			msgs := map[string][]byte{}
			for _, m := range []struct {
				key, box, kind string
				keywords       []string
			}{
				{"plain", inbox, "plain", nil},
				{"seen", inbox, "utf8", []string{"$seen"}},
				{"flagged", inbox, "multipart", []string{"$flagged", "$answered", "$seen"}},
				{"toFlag", inbox, "plain", nil},
				{"toDestroy", inbox, "plain", []string{"$seen"}},
				{"toMove", inbox, "long", nil},
				{"toSee", inbox, "plain", nil},
				{"toDelete", inbox, "utf8", nil},
				{"project", projects, "plain", []string{"$seen"}},
				{"alpha1", alpha, "multipart", nil},
				{"alpha2", alpha, "plain", []string{"$draft"}},
				{"sent", j.box["sent"], "plain", []string{"$seen"}},
			} {
				msgs[m.key] = syncMessage(tok+"-"+m.key, "sync "+m.key, m.kind)
				j.put(t, m.box, msgs[m.key], m.keywords...)
			}
			sync("first sync")
			remote, ids, files := check("after the first sync")
			if len(remote["INBOX"]) != 8 || len(remote["Projects/Alpha"]) != 2 || len(remote["Projects"]) != 1 || len(remote["Sent"]) != 1 {
				t.Fatalf("the account is not as it was filled: %v", remote)
			}

			// 2. Changes on the server.
			key := func(folder, name string) string { return folder + " " + hashOf(msgs[name]) }
			j.call(t, []any{"Email/set", map[string]any{"accountId": j.account,
				"update": map[string]any{
					ids[key("INBOX", "toFlag")]: map[string]any{"keywords/$flagged": true},
					ids[key("INBOX", "toMove")]: map[string]any{"mailboxIds": map[string]bool{alpha: true}},
				},
				"destroy": []string{ids[key("INBOX", "toDestroy")]}}, "0"})
			msgs["arrived"] = syncMessage(tok+"-arrived", "sync arrived", "multipart")
			j.put(t, inbox, msgs["arrived"])
			later := j.createMailbox(t, "Later", "")
			msgs["later"] = syncMessage(tok+"-later", "sync later", "plain")
			j.put(t, later, msgs["later"], "$flagged")

			// Changes in the Maildir.
			see := files[key("INBOX", "toSee")]
			del := files[key("INBOX", "toDelete")]
			if see == "" || del == "" {
				t.Fatalf("the Maildir lacks the messages to change: %q %q", see, del)
			}
			base := see[strings.LastIndex(see, "/")+1:]
			if i := strings.Index(base, ":2,"); i >= 0 {
				base = base[:i]
			}
			msgs["local"] = syncMessage(tok+"-local", "sync local", "utf8")
			sh(t, normal(msgs["local"]), fmt.Sprintf(`set -e
mv '%s' '%s/%s/cur/%s:2,S'
rm '%s'
cat > '%s/%s/new/1794700000.lab%s.clients'`, see, c.root, c.dir("INBOX"), base, del, c.root, c.dir("INBOX"), tok))

			// 3. Sync, and look at each change by name before the full
			// comparison.
			sync("second sync")
			remote, ids, _ = check("after the second sync")
			local, _ := c.local(t)
			has := func(side map[string][]copyOf, folder, name, flags string) bool {
				for _, m := range side[folder] {
					if m.hash == hashOf(msgs[name]) && m.flags == flags {
						return true
					}
				}
				return false
			}
			for _, w := range []struct {
				what, folder, name, flags string
				want                      bool
			}{
				{"flag set over JMAP", "INBOX", "toFlag", "F", true},
				{"destroyed over JMAP", "INBOX", "toDestroy", "S", false},
				{"arrived on the server", "INBOX", "arrived", "", true},
				{"moved over JMAP: gone from the Inbox", "INBOX", "toMove", "", false},
				{"moved over JMAP: in Projects/Alpha", "Projects/Alpha", "toMove", "", true},
				{"new mailbox on the server", "Later", "later", "F", true},
				{"marked seen in the Maildir", "INBOX", "toSee", "S", true},
				{"deleted in the Maildir", "INBOX", "toDelete", "", false},
				{"put into the Maildir", "INBOX", "local", "", true},
			} {
				for side, have := range map[string]map[string][]copyOf{"over JMAP": remote, "in the Maildir": local} {
					if got := has(have, w.folder, w.name, w.flags); got != w.want {
						t.Errorf("%s: %s the message is there: %v, want %v", w.what, side, got, w.want)
					}
				}
			}
			if _, gone := ids[key("INBOX", "toDelete")]; gone {
				t.Errorf("the message deleted in the Maildir is still an Email in the Inbox")
			}
			if len(remote["INBOX"]) != 7 || len(remote["Projects/Alpha"]) != 3 || len(remote["Later"]) != 1 {
				t.Errorf("after the second sync: %d in INBOX, %d in Projects/Alpha, %d in Later; want 7, 3, 1",
					len(remote["INBOX"]), len(remote["Projects/Alpha"]), len(remote["Later"]))
			}

			// 4. Nothing to do: nothing changes.
			before := j.state(t)
			sync("third sync")
			check("after the third sync")
			if after := j.state(t); after != before {
				t.Errorf("a sync with nothing to do changed the account: state %s, then %s", before, after)
			}
			observe("sync: full, a change on each side, again; compared over JMAP", c.name, "pass")
		})
	}
}
