package labtest

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestThunderbirdActions is a person at work in Thunderbird, and the
// webmail open next to it: every change made in the one has to show in
// the other. One Thunderbird and one account for all cases, in the order
// below; the account starts with twelve messages in the Inbox (a01 to
// a12) and an empty mailbox "Work".
func TestThunderbirdActions(t *testing.T) {
	lab(t)
	user := labUser(t, "tb-act")
	other := labUser(t, "tb-other")
	j := newJMAP(t, user)
	j.createMailbox(t, "Work", "")
	boxes := j.mailboxes(t)
	var mails []seedEmail
	kinds := []string{"plain", "utf8", "multipart", "long"}
	for i := 1; i <= 12; i++ {
		tok := fmt.Sprintf("a%02d", i)
		raw := syncMessage(tok, "Action message "+tok, kinds[i%len(kinds)])
		// Mail that can be answered: from a user Postfix has.
		raw = bytes.Replace(raw, []byte("From: Sender <sender@postfix.test>"), []byte("From: Alice <alice@postfix.test>"), 1)
		raw = bytes.Replace(raw, []byte("To: user@robin.test"), []byte("To: "+user), 1)
		mails = append(mails, seedEmail{raw: raw, messageID: tok + "@postfix.test", mailboxes: []string{"INBOX"}})
	}
	j.seed(t, boxes, mails)
	id := func(tok string) string { return tok + "@postfix.test" }

	x := startThunderbird(t, user, password())
	x.sync()
	if n := compareFolders(t, x, j); n != 12 {
		t.Fatalf("%d messages after the first sync", n)
	}
	// since marks where a case's part of the trace begins.
	var since time.Time
	mark := func() { since = time.Now().Add(-time.Second) }
	wire := func(t *testing.T, want ...string) []imapLine {
		t.Helper()
		var lines []imapLine
		waitFor(t, fmt.Sprintf("Thunderbird to send %q", want), 20*time.Second, func() bool {
			lines, _ = imapTrace(t, since, user)
			for _, w := range want {
				if len(sent(lines, w)) == 0 {
					return false
				}
			}
			return true
		})
		return lines
	}
	keywords := func(t *testing.T, what, mid string, want ...string) {
		t.Helper()
		j.waitEmails(t, what, func(e map[string]jmapEmail) bool {
			return strings.Join(jmapFlags(e[mid]), " ") == strings.Join(want, " ")
		})
	}

	t.Run("flags to robinmail", func(t *testing.T) {
		defer cell(t, "Thunderbird: mark read, flag, tag; seen over JMAP", tbCol)("")
		mark()
		x.run(`markRead("INBOX", [arguments[0]], true); markFlagged("INBOX", [arguments[1]], true); tag("INBOX", [arguments[2]], "$label1", true);`,
			id("a01"), id("a02"), id("a03"))
		keywords(t, "a01 read", id("a01"), "$seen")
		keywords(t, "a02 flagged", id("a02"), "$flagged")
		keywords(t, "a03 tagged", id("a03"), "$label1")
		lines := wire(t, `UID STORE`)
		t.Logf("on the wire: %s", strings.Join(sent(lines, "UID STORE"), " | "))
		// And off again.
		x.run(`markRead("INBOX", [arguments[0]], false); markFlagged("INBOX", [arguments[1]], false); tag("INBOX", [arguments[2]], "$label1", false);`,
			id("a01"), id("a02"), id("a03"))
		keywords(t, "a01 unread", id("a01"))
		keywords(t, "a02 unflagged", id("a02"))
		keywords(t, "a03 untagged", id("a03"))
		// Several at once, and left that way for the cases below.
		x.run(`markRead("INBOX", arguments[0], true);`, []string{id("a01"), id("a02"), id("a03")})
		for _, tok := range []string{"a01", "a02", "a03"} {
			keywords(t, tok+" read", id(tok), "$seen")
		}
	})

	t.Run("flags from robinmail", func(t *testing.T) {
		note := cell(t, "Thunderbird: a flag, a read mark and a tag set over JMAP show", tbCol)
		emails := j.emails(t)
		j.set(t, emails[id("a04")].id, map[string]any{"keywords/$flagged": true})
		j.set(t, emails[id("a05")].id, map[string]any{"keywords/$seen": true})
		j.set(t, emails[id("a06")].id, map[string]any{"keywords/$label2": true})
		ok := func(m map[string]tbMessage) bool {
			return m[id("a04")].Flagged && m[id("a05")].Read && reflect.DeepEqual(m[id("a06")].Keywords, []string{"$label2"})
		}
		// The Inbox is the folder Thunderbird idles in: robinmail tells it
		// there, and nobody has to click.
		start := time.Now()
		pushed := false
		for time.Since(start) < 15*time.Second && !pushed {
			pushed = ok(x.messages("INBOX"))
			time.Sleep(200 * time.Millisecond)
		}
		if pushed {
			note("pass: without a refresh, " + time.Since(start).Round(100*time.Millisecond).String())
		} else {
			x.waitMessages("the flags set over JMAP", "INBOX", true, 20*time.Second, ok)
			note("pass: after a refresh only")
		}
		compareFolders(t, x, j)
	})

	t.Run("move and copy to robinmail", func(t *testing.T) {
		defer cell(t, "Thunderbird: move and copy, one and several; the Email keeps its id", tbCol)("")
		before := j.emails(t)
		mark()
		x.run(`await transfer("INBOX", [arguments[0]], "Work", true);`, id("a07"))
		x.run(`await transfer("INBOX", [arguments[0]], "Work", false);`, id("a08"))
		x.run(`await transfer("INBOX", arguments[0], "Work", true);`, []string{id("a09"), id("a10")})
		after := j.waitEmails(t, "a07, a09 and a10 moved and a08 copied to Work", func(e map[string]jmapEmail) bool {
			return reflect.DeepEqual(in(e, boxes["Work"]), []string{id("a07"), id("a08"), id("a09"), id("a10")}) && len(in(e, boxes["INBOX"])) == 9
		})
		for _, tok := range []string{"a07", "a08", "a09", "a10"} {
			if before[id(tok)].id != after[id(tok)].id {
				t.Errorf("%s was Email %s and is now %s", tok, before[id(tok)].id, after[id(tok)].id)
			}
		}
		if !after[id("a08")].mailboxes[boxes["INBOX"]] || len(after[id("a08")].mailboxes) != 2 {
			t.Errorf("the copied a08 is in %v", after[id("a08")].mailboxes)
		}
		lines := wire(t, "UID MOVE", "UID COPY")
		t.Logf("on the wire: %s | %s", strings.Join(sent(lines, "UID MOVE"), " | "), strings.Join(sent(lines, "UID COPY"), " | "))
		observe("Thunderbird: moves with", tbCol, "UID MOVE")
		// A copy into a folder the message is in already doubles nothing.
		x.run(`await transfer("INBOX", [arguments[0]], "Work", false);`, id("a08"))
		x.update("Work")
		x.update("INBOX")
		compareFolders(t, x, j)
	})

	t.Run("move and label from robinmail", func(t *testing.T) {
		defer cell(t, "Thunderbird: a move and a second label made over JMAP show", tbCol)("")
		emails := j.emails(t)
		j.set(t, emails[id("a11")].id, map[string]any{"mailboxIds": map[string]bool{boxes["Work"]: true}})
		j.set(t, emails[id("a12")].id, map[string]any{"mailboxIds/" + boxes["Work"]: true})
		// Out of the Inbox without a refresh; Work is not selected
		// anywhere, so it takes a click.
		x.waitMessages("a11 to leave", "INBOX", false, 20*time.Second, func(m map[string]tbMessage) bool { _, ok := m[id("a11")]; return !ok })
		x.waitMessages("a11 and a12", "Work", true, 20*time.Second, func(m map[string]tbMessage) bool {
			_, a := m[id("a11")]
			_, b := m[id("a12")]
			return a && b
		})
		compareFolders(t, x, j)
	})

	t.Run("delete and empty Trash", func(t *testing.T) {
		defer cell(t, "Thunderbird: delete to Trash, empty Trash; a destroy over JMAP shows", tbCol)("")
		before := j.emails(t)
		mark()
		x.run(`await remove("INBOX", [arguments[0]]);`, id("a01"))
		after := j.waitEmails(t, "a01 in Trash only", func(e map[string]jmapEmail) bool {
			return reflect.DeepEqual(in(e, boxes["Trash"]), []string{id("a01")}) && len(e[id("a01")].mailboxes) == 1
		})
		if before[id("a01")].id != after[id("a01")].id {
			t.Errorf("the deleted a01 was Email %s and is %s in Trash", before[id("a01")].id, after[id("a01")].id)
		}
		lines := wire(t, `UID MOVE`)
		t.Logf("delete on the wire: %s", strings.Join(sent(lines, "UID MOVE"), " | "))
		x.update("Trash")
		compareFolders(t, x, j)

		mark()
		x.run(`await emptyTrash();`)
		j.waitEmails(t, "a01 destroyed", func(e map[string]jmapEmail) bool { _, ok := e[id("a01")]; return !ok })
		lines = wire(t, "EXPUNGE")
		var cmds []string
		for _, l := range lines {
			if !l.reply && !strings.HasPrefix(l.text, "IDLE") && !strings.HasPrefix(l.text, "NOOP") {
				cmds = append(cmds, l.text)
			}
		}
		t.Logf("empty Trash on the wire: %s", clip(strings.Join(cmds, " | "), 600))
		x.update("Trash")
		if m := x.messages("Trash"); len(m) != 0 {
			t.Errorf("Thunderbird's Trash still has %d messages", len(m))
		}

		// The other way: destroyed in the webmail, gone in Thunderbird.
		j.destroy(t, before[id("a02")].id)
		x.waitMessages("a02 to go", "INBOX", false, 20*time.Second, func(m map[string]tbMessage) bool { _, ok := m[id("a02")]; return !ok })
		compareFolders(t, x, j)
	})

	t.Run("folders", func(t *testing.T) {
		defer cell(t, "Thunderbird: create, nest, rename, delete a folder with a non-ASCII name", tbCol)("")
		mark()
		x.run(`await createFolder("", "Entwürfe für Köln");`)
		x.run(`await createFolder("Entwürfe für Köln", "日本");`)
		has := func(want ...string) func() bool {
			return func() bool {
				b := j.mailboxes(t)
				for _, w := range want {
					not := strings.HasPrefix(w, "!")
					if (b[strings.TrimPrefix(w, "!")] != "") == not {
						return false
					}
				}
				return true
			}
		}
		waitFor(t, "the two mailboxes in robinmail", 20*time.Second, has("Entwürfe für Köln", "Entwürfe für Köln/日本"))
		// A message into the nested one, so that the rename has something
		// to keep.
		x.run(`await transfer("INBOX", [arguments[0]], "Entwürfe für Köln/日本", false);`, id("a03"))
		x.run(`await renameFolder("Entwürfe für Köln", "Übersicht & Pläne");`)
		waitFor(t, "the renamed mailbox in robinmail", 20*time.Second,
			has("Übersicht & Pläne", "Übersicht & Pläne/日本", "!Entwürfe für Köln", "!Entwürfe für Köln/日本"))
		x.run(`await discover();`)
		x.update("Übersicht & Pläne/日本")
		compareFolders(t, x, j)
		lines := wire(t, "CREATE", "RENAME", "SUBSCRIBE")
		var cmds []string
		for _, v := range []string{"CREATE", "SUBSCRIBE", "RENAME", "UNSUBSCRIBE"} {
			cmds = append(cmds, sent(lines, v)...)
		}
		t.Logf("on the wire: %s", strings.Join(cmds, " | "))

		// Delete in Thunderbird moves the folder into Trash; emptying the
		// Trash removes it.
		mark()
		x.run(`await deleteFolder("Übersicht & Pläne/日本");`)
		waitFor(t, "the deleted folder under Trash in robinmail", 20*time.Second, has("Trash/日本", "!Übersicht & Pläne/日本"))
		x.run(`await emptyTrash();`)
		waitFor(t, "the folder to go with the Trash", 20*time.Second, has("!Trash/日本"))
		lines = wire(t, "RENAME", "DELETE")
		t.Logf("delete on the wire: %s | %s", strings.Join(sent(lines, "RENAME"), " | "), strings.Join(sent(lines, "DELETE"), " | "))
		if e := j.emails(t); !e[id("a03")].mailboxes[boxes["INBOX"]] || len(e[id("a03")].mailboxes) != 1 {
			t.Errorf("a03 had a copy in the deleted folder and is now in %v", e[id("a03")].mailboxes)
		}
	})

	t.Run("send", func(t *testing.T) {
		defer cell(t, "Thunderbird: send with an attachment, local and to Postfix; in Sent once", tbCol)("")
		tok := newToken()
		attachment := "name,value\r\nKöln,1\r\n" + strings.Repeat("0123456789abcdef,2\r\n", 200)
		tbExec(t, []byte(attachment), "mkdir -p /home/tb/attach && cat > /home/tb/attach/"+tok+".csv")
		smtpSince := time.Now().Add(-time.Second)
		mark()
		x.run(`await send(arguments[0]);`, map[string]any{
			"to": "Other <" + other + ">", "cc": "alice@postfix.test", "subject": "Grüße from Thunderbird " + tok,
			"body":        "A message written in Thunderbird.\nZweite Zeile: äöü €\n",
			"attachments": []any{map[string]any{"path": "/home/tb/attach/" + tok + ".csv", "name": "Tabelle " + tok + ".csv"}}})

		// The local user and Postfix have it, attachment and all.
		jo := newJMAP(t, other)
		_, got := jo.find(t, tok)
		subject, _, text := decoded(t, got)
		if subject != "Grüße from Thunderbird "+tok || !strings.Contains(text, "Zweite Zeile: äöü €") {
			t.Errorf("the local user got the subject %q and the text %q", subject, text)
		}
		at := peers[0].find(t, "alice", tok)
		for name, raw := range map[string][]byte{"the local user": got, "Postfix": at} {
			if !bytes.Contains(raw, []byte("multipart/mixed")) || !bytes.Contains(lf(raw), []byte("Tabelle")) {
				t.Errorf("%s got no attachment:\n%s", name, clip(string(raw), 1500))
			}
		}
		// In Sent exactly once: Thunderbird files its copy with APPEND and
		// robinmail keeps none of its own for mail that comes in over SMTP
		// submission.
		var mid string
		sentBox := j.waitEmails(t, "the message in Sent", func(e map[string]jmapEmail) bool {
			return len(in(e, boxes["Sent"])) == 1
		})
		mid = in(sentBox, boxes["Sent"])[0]
		raw := j.download(t, sentBox[mid].blob)
		if !bytes.Contains(raw, []byte(tok)) {
			t.Fatalf("Sent has another message:\n%s", clip(string(raw), 800))
		}
		if !sentBox[mid].keywords["$seen"] || len(sentBox[mid].mailboxes) != 1 {
			t.Errorf("the copy in Sent has the keywords %v and is in %v", jmapFlags(sentBox[mid]), sentBox[mid].mailboxes)
		}
		time.Sleep(2 * time.Second) // a second copy would come late
		if _, counts := j.emailsCounted(t); counts[mid] != 1 {
			t.Errorf("robinmail has %d Emails of the sent message", counts[mid])
		}
		x.update("Sent")
		if m := x.messages("Sent"); len(m) != 1 || m[mid].ID != mid {
			t.Errorf("Thunderbird's Sent: %v", m)
		}
		lines := wire(t, `APPEND "Sent"`)
		t.Logf("copy to Sent on the wire: %s", strings.Join(sent(lines, "APPEND"), " | "))
		// How it submitted.
		var smtp []string
		for _, l := range trace(t, smtpSince) {
			if l.side == "server" && l.dir == "C" && strings.HasPrefix(l.remote, tbIP) && !strings.HasPrefix(l.line, "RCPT") {
				smtp = append(smtp, l.line)
			}
		}
		t.Logf("submission on the wire: %s", strings.Join(smtp, " | "))
		joined := strings.Join(smtp, "\n")
		if !strings.Contains(joined, "AUTH PLAIN") || !strings.Contains(joined, "MAIL FROM:<"+user+">") {
			t.Errorf("the submission: %s", joined)
		}
		observe("Thunderbird: files its copy in Sent with", tbCol, "APPEND")
		observe("Thunderbird: submits with", tbCol, "AUTH PLAIN on 465")
	})

	t.Run("reply", func(t *testing.T) {
		defer cell(t, "Thunderbird: reply; the original is marked answered", tbCol)("")
		tok := newToken()
		mark()
		x.run(`await reply("INBOX", arguments[0], arguments[1]);`, id("a04"), "My answer "+tok+".\n")
		at := peers[0].find(t, "alice", tok)
		if got := header(at, "In-Reply-To"); len(got) != 1 || got[0] != "<"+id("a04")+">" {
			t.Errorf("In-Reply-To of the reply: %q", got)
		}
		if got := header(at, "References"); len(got) != 1 || !strings.Contains(got[0], "<"+id("a04")+">") {
			t.Errorf("References of the reply: %q", got)
		}
		if got := header(at, "Subject"); len(got) != 1 || got[0] != "Re: Action message a04" {
			t.Errorf("Subject of the reply: %q", got)
		}
		if !strings.Contains(body(at), "> ") {
			t.Errorf("the reply does not quote the original:\n%s", clip(body(at), 600))
		}
		keywords(t, "a04 answered", id("a04"), "$answered", "$flagged", "$seen")
		lines := wire(t, "UID STORE")
		t.Logf("on the wire: %s", strings.Join(sent(lines, "UID STORE"), " | "))
		e := j.waitEmails(t, "the reply in Sent", func(e map[string]jmapEmail) bool { return len(in(e, boxes["Sent"])) == 2 })
		_ = e
	})

	t.Run("draft", func(t *testing.T) {
		defer cell(t, "Thunderbird: save a draft, save it again: one draft", tbCol)("")
		tok := newToken()
		mark()
		x.run(`
			const w = await compose({to: arguments[0], subject: "Draft one " + arguments[1], body: "First version.\n"});
			await saveDraft(w);`, other, tok)
		first := j.waitEmails(t, "the draft", func(e map[string]jmapEmail) bool { return len(in(e, boxes["Drafts"])) == 1 })
		d := first[in(first, boxes["Drafts"])[0]]
		if raw := j.download(t, d.blob); !bytes.Contains(raw, []byte("Draft one "+tok)) || !d.keywords["$draft"] {
			t.Errorf("the draft has the keywords %v:\n%s", jmapFlags(d), clip(string(raw), 600))
		}
		x.run(`
			const w = Services.wm.getMostRecentWindow("msgcompose");
			setSubject(w, "Draft two " + arguments[0]);
			w.GetCurrentEditor().insertText("Second version. ");
			await saveDraft(w);
			w.close();`, tok)
		second := j.waitEmails(t, "the second version to replace the first", func(e map[string]jmapEmail) bool {
			ids := in(e, boxes["Drafts"])
			return len(ids) == 1 && e[ids[0]].id != d.id
		})
		d2 := second[in(second, boxes["Drafts"])[0]]
		if raw := j.download(t, d2.blob); !bytes.Contains(raw, []byte("Draft two "+tok)) || !bytes.Contains(raw, []byte("Second version.")) || !d2.keywords["$draft"] {
			t.Errorf("the second draft has the keywords %v:\n%s", jmapFlags(d2), clip(string(raw), 600))
		}
		lines := wire(t, `APPEND "Drafts"`)
		var cmds []string
		for _, l := range lines {
			if !l.reply && !strings.HasPrefix(l.text, "IDLE") && !strings.HasPrefix(l.text, "NOOP") && !strings.Contains(l.text, "FETCH") && !strings.HasPrefix(l.text, "LIST") {
				cmds = append(cmds, l.text)
			}
		}
		t.Logf("on the wire: %s", clip(strings.Join(cmds, " | "), 700))
		x.update("Drafts")
		compareFolders(t, x, j)
		// Nobody got anything.
		if _, _, ok := newJMAP(t, other).lookup(t, "inbox", tok); ok {
			t.Errorf("the draft was delivered")
		}
	})

	t.Run("new mail while idle", func(t *testing.T) {
		note := cell(t, "Thunderbird: mail delivered over SMTP shows without a refresh (IDLE)", tbCol)
		tok := newToken()
		mark()
		// Give it a moment to be idle in the Inbox again after the cases
		// above.
		wireSince := time.Now().Add(-2 * time.Minute)
		waitFor(t, "Thunderbird to idle", 30*time.Second, func() bool {
			lines, _ := imapTrace(t, wireSince, user)
			return len(sent(lines, "IDLE")) > 0
		})
		start := time.Now()
		peers[0].send(t, "alice@postfix.test", []string{user}, rawMessage(tok, "alice@postfix.test", user, nil, "Mail while Thunderbird idles.\n"))
		j.find(t, tok)
		delivered := time.Since(start)
		x.run(`await arrived("INBOX", arguments[0], 30000);`, tok+"@harness.lab.test")
		t.Logf("in robinmail after %s, in Thunderbird's list %s later", delivered.Round(100*time.Millisecond), (time.Since(start) - delivered).Round(100*time.Millisecond))
		note("pass: " + (time.Since(start) - delivered).Round(100*time.Millisecond).String() + " after delivery")
		lines := wire(t, "IDLE", "UID FETCH")
		t.Logf("on the wire since the delivery: %s", clip(strings.Join(verbs(lines), ", "), 300))
		compareFolders(t, x, j)
		observe("Thunderbird: learns of new mail by", tbCol, "IDLE")
		observe("Thunderbird: logs in with", tbCol, "AUTHENTICATE PLAIN")
	})

	t.Run("folders from robinmail", func(t *testing.T) {
		note := cell(t, "Thunderbird: a mailbox made, and one destroyed, over JMAP", tbCol)
		// A mailbox made in the webmail shows once the folder list is read
		// again.
		made := j.createMailbox(t, "Aus dem Webmail ü", "")
		x.run(`await discover();`)
		if _, ok := x.folders()["Aus dem Webmail ü"]; !ok {
			t.Errorf("a mailbox made over JMAP does not show in Thunderbird: %v", x.folders())
		}
		// One destroyed there is no longer in the list robinmail gives.
		// Thunderbird keeps showing it while it runs.
		j.call(t, []any{"Mailbox/set", map[string]any{"accountId": j.account, "destroy": []string{made}}, "0"})
		x.run(`await discover(); await sleep(1000);`)
		_, stale := x.folders()["Aus dem Webmail ü"]
		x.restart()
		x.sync()
		if _, ok := x.folders()["Aus dem Webmail ü"]; ok {
			t.Errorf("Thunderbird shows a mailbox that was destroyed over JMAP, also after a restart")
		}
		if stale {
			note("pass: the destroyed one goes at the next start")
		}
		// The restart found its passwords again and the account as it is.
		compareFolders(t, x, j)
	})
}

// tbIP is the address of Thunderbird's container in the lab.
const tbIP = "10.77.25.7"
