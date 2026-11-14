package labtest

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// seedEmail is a message to put into an account before a client sees it.
type seedEmail struct {
	raw       []byte
	messageID string
	mailboxes []string // paths
	keywords  []string
}

// seed imports the messages over JMAP, fifty to a request, with rising
// arrival times a minute apart.
func (j *jmap) seed(t testing.TB, boxes map[string]string, mails []seedEmail) {
	t.Helper()
	start := time.Now().Add(-time.Duration(len(mails)) * time.Minute).UTC().Truncate(time.Second)
	for at := 0; at < len(mails); at += 50 {
		batch := map[string]any{}
		for i := at; i < len(mails) && i < at+50; i++ {
			m := mails[i]
			ids := map[string]bool{}
			for _, p := range m.mailboxes {
				if boxes[p] == "" {
					t.Fatalf("no mailbox %s", p)
				}
				ids[boxes[p]] = true
			}
			kw := map[string]bool{}
			for _, k := range m.keywords {
				kw[k] = true
			}
			batch[fmt.Sprintf("m%d", i)] = map[string]any{"blobId": j.upload(t, m.raw, "message/rfc822"), "mailboxIds": ids, "keywords": kw,
				"receivedAt": start.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)}
		}
		res := j.call(t, []any{"Email/import", map[string]any{"accountId": j.account, "emails": batch}, "0"})
		if created, _ := res[0].args["created"].(map[string]any); len(created) != len(batch) {
			t.Fatalf("Email/import: %s", clip(fmt.Sprint(res[0].args), 1500))
		}
	}
}

// tbFlags is what a message list row says about a message, in the words
// of JMAP keywords.
func tbFlags(m tbMessage) []string {
	var out []string
	for _, f := range []struct {
		on bool
		kw string
	}{{m.Read, "$seen"}, {m.Flagged, "$flagged"}, {m.Answered, "$answered"}, {m.Forwarded, "$forwarded"}, {m.Deleted, "$deleted"}} {
		if f.on {
			out = append(out, f.kw)
		}
	}
	for _, k := range m.Keywords {
		out = append(out, strings.ToLower(k))
	}
	sort.Strings(out)
	return out
}

func jmapFlags(e jmapEmail) []string {
	out := []string{}
	for k := range e.keywords {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// shownFlags is jmapFlags without what Thunderbird's message list has no
// column for: it keeps no \Draft flag on a message.
func shownFlags(e jmapEmail) []string {
	out := []string{}
	for k := range e.keywords {
		if k != "$draft" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// compareFolders checks that Thunderbird's folders hold the messages
// robinmail has in the mailboxes of the same path, with the same flags.
// Every folder is brought up to date first, as clicking through them does.
// It returns the number of rows compared.
func compareFolders(t *testing.T, x *tb, j *jmap) int {
	t.Helper()
	x.run(`for (const f of root.descendants) { await url(l => MailServices.imap.selectFolder(f, l, msgWindow)); }`)
	boxes := j.mailboxes(t)
	emails := j.emails(t)
	folders := x.folders()
	var want, got []string
	for p := range boxes {
		want = append(want, p)
	}
	for p := range folders {
		got = append(got, p)
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("folders: robinmail has %q, Thunderbird shows %q", want, got)
	}
	rows := 0
	for _, p := range want {
		list := x.messages(p)
		ids := in(emails, boxes[p])
		if len(list) != len(ids) {
			t.Errorf("%s: robinmail has %d messages, Thunderbird lists %d", p, len(ids), len(list))
		}
		unread := 0
		for _, id := range ids {
			m, ok := list[id]
			if !ok {
				t.Errorf("%s: Thunderbird does not list %s", p, id)
				continue
			}
			rows++
			if a, b := shownFlags(emails[id]), tbFlags(m); strings.Join(a, " ") != strings.Join(b, " ") {
				t.Errorf("%s: %s has the keywords %v in robinmail and %v in Thunderbird", p, id, a, b)
			}
			if !emails[id].keywords["$seen"] {
				unread++
			}
		}
		if f := folders[p]; f.Total != len(ids) || f.Unread != unread {
			t.Errorf("%s: Thunderbird counts %d messages, %d unread; robinmail has %d, %d unread", p, f.Total, f.Unread, len(ids), unread)
		}
	}
	return rows
}

// TestThunderbirdFirstSync is a new account in Thunderbird on a mailbox
// that is already full: 300 messages in five mailboxes, one of them
// nested and two with names that are not ASCII or need quoting, with
// flags and tags, ten of the Emails in two mailboxes at once.
func TestThunderbirdFirstSync(t *testing.T) {
	lab(t)
	defer cell(t, "Thunderbird: first sync, 300 messages, 9 folders; two labels show in both", tbCol)("")
	user := labUser(t, "tb-sync")
	j := newJMAP(t, user)
	projects := j.createMailbox(t, "Projects", "")
	j.createMailbox(t, "Küche", projects)
	j.createMailbox(t, "Notes & Things", "")
	j.createMailbox(t, "日本語", "")
	boxes := j.mailboxes(t)

	var mails []seedEmail
	add := func(n int, kind string, mailboxes []string, keywords ...string) {
		tok := fmt.Sprintf("s%03d", len(mails))
		subject := fmt.Sprintf("Message %d of the first sync", len(mails))
		if n%5 == 0 {
			subject = fmt.Sprintf("=?utf-8?Q?Gr=C3=BC=C3=9Fe_Nr._%d?= =?utf-8?B?5pel5pys6Kqe?=", len(mails))
		}
		mails = append(mails, seedEmail{raw: syncMessage(tok, subject, kind), messageID: tok + "@postfix.test", mailboxes: mailboxes, keywords: keywords})
	}
	kinds := []string{"plain", "utf8", "multipart", "plain", "long", "plain"}
	for i := 0; i < 200; i++ {
		var kw []string
		if i%3 != 0 {
			kw = append(kw, "$seen")
		}
		if i%10 == 0 {
			kw = append(kw, "$flagged")
		}
		if i%25 == 0 {
			kw = append(kw, "$answered")
		}
		if i%40 == 0 {
			kw = append(kw, "$label1")
		}
		if i%50 == 1 {
			kw = append(kw, "$forwarded")
		}
		in := []string{"INBOX"}
		if i%20 == 7 {
			in = append(in, "Projects") // ten Emails with two labels
		}
		add(i, kinds[i%len(kinds)], in, kw...)
	}
	for i := 0; i < 50; i++ {
		add(i, kinds[i%len(kinds)], []string{"Projects"}, "$seen")
	}
	for i := 0; i < 25; i++ {
		add(i, "utf8", []string{"Projects/Küche"})
	}
	for i := 0; i < 15; i++ {
		add(i, "plain", []string{"Notes & Things"}, "$seen", "$flagged")
	}
	for i := 0; i < 10; i++ {
		add(i, "multipart", []string{"日本語"}, "$seen")
	}
	j.seed(t, boxes, mails)

	since := time.Now().Add(-time.Second)
	x := startThunderbird(t, user, password())
	start := time.Now()
	x.sync()
	took := time.Since(start)

	// Special-use: the five folders with a role are what Thunderbird
	// takes them for, and nothing else is.
	folders := x.folders()
	for path, role := range map[string]string{"INBOX": "inbox", "Drafts": "drafts", "Sent": "sent", "Junk": "junk", "Trash": "trash"} {
		if got := folders[path].Special; !reflect.DeepEqual(got, []string{role}) {
			t.Errorf("Thunderbird takes %s for %v, not for %s", path, got, role)
		}
	}
	for path, f := range folders {
		if len(f.Special) > 0 && !map[string]bool{"INBOX": true, "Drafts": true, "Sent": true, "Junk": true, "Trash": true}[path] {
			t.Errorf("Thunderbird takes %s for %v", path, f.Special)
		}
	}

	rows := compareFolders(t, x, j)
	if rows != 310 {
		t.Errorf("%d rows compared, want 310: 300 Emails, ten of them in two folders", rows)
	}
	// The ten with two labels are in both folders, each with a UID of its
	// own there.
	inbox, proj := x.messages("INBOX"), x.messages("Projects")
	for i := 7; i < 200; i += 20 {
		id := fmt.Sprintf("s%03d@postfix.test", i)
		if _, ok := inbox[id]; !ok {
			t.Errorf("%s is not in Thunderbird's INBOX", id)
		}
		if _, ok := proj[id]; !ok {
			t.Errorf("%s is not in Thunderbird's Projects", id)
		}
	}
	// Two encoded words with a space between them: the space is not part
	// of the subject (RFC 2047 6.2).
	if got := inbox["s005@postfix.test"].Subject; got != "Grüße Nr. 5日本語" {
		t.Errorf("the subject with encoded words shows as %q", got)
	}
	if !inbox["s002@postfix.test"].Attachment {
		t.Logf("Thunderbird does not mark s002 as having an attachment before it opened it")
	}

	// Opening a message gives the bytes robinmail has: one in UTF-8, one
	// with an attachment, a long one, one from a nested folder.
	emails := j.emails(t)
	for _, c := range []struct{ folder, id string }{{"INBOX", "s001@postfix.test"}, {"INBOX", "s002@postfix.test"},
		{"INBOX", "s004@postfix.test"}, {"Projects/Küche", "s250@postfix.test"}, {"日本語", "s290@postfix.test"}} {
		if got, want := hashOf(x.source(c.folder, c.id)), hashOf(j.download(t, emails[c.id].blob)); got != want {
			t.Errorf("%s in %s: Thunderbird got other bytes than robinmail has", c.id, c.folder)
		}
	}

	// What it did on the wire.
	lines, logins := imapTrace(t, since, user)
	t.Logf("first sync: %s for 300 messages in %d folders, %d IMAP connections, %d commands", took.Round(time.Millisecond), len(folders), logins, len(lines)/2)
	t.Logf("commands used: %s", strings.Join(verbs(lines), ", "))
	for _, want := range []string{"ID (\"name\" \"Thunderbird\"", "ENABLE UTF8=ACCEPT", "LIST (subscribed) \"\" \"*\" return (special-use)", "SELECT \"INBOX\"", "UID FETCH 1:* (FLAGS)"} {
		if len(sent(lines, want)) == 0 {
			t.Errorf("Thunderbird did not send %s", want)
		}
	}
	if s := sent(lines, "SELECT"); len(s) > 0 {
		t.Logf("it selects like this: %s", strings.Join(s, " | "))
	}
	if f := sent(lines, "UID FETCH"); len(f) > 1 {
		t.Logf("it fetches headers like this: %s", clip(f[1], 400))
	}
	condstore := false
	for _, l := range lines {
		if !l.reply && (strings.Contains(strings.ToUpper(l.text), "CONDSTORE") || strings.Contains(strings.ToUpper(l.text), "CHANGEDSINCE")) {
			condstore = true
		}
	}
	observe("Thunderbird: uses CONDSTORE", tbCol, yesNo(condstore))
	observe("Thunderbird: IMAP connections for a first sync", tbCol, fmt.Sprint(logins))
	observe("Thunderbird: time for the first sync", tbCol, took.Round(100*time.Millisecond).String())
}
