package imap

// What Thunderbird 153 sends, replayed from its own protocol log
// (MOZ_LOG=IMAP:4) of the sessions in the interoperability lab
// (contrib/lab, internal/labtest), so that what it relies on is kept
// without Docker. The lab found nothing to fix in this package; the test
// is here so that it stays that way.
//
// Thunderbird writes its commands partly in lower case ("uid store",
// "select", "list (subscribed) ... return (special-use)") and the item
// names of STORE in three spellings; it logs in with "authenticate PLAIN"
// and waits for the challenge although SASL-IR is offered; it enables
// UTF8=ACCEPT and from then on sends mailbox names as UTF-8 in quoted
// strings and messages as "UTF8 (~{n+}" literals; it does not use
// CONDSTORE, and learns of changes from the untagged FETCH, EXISTS and
// EXPUNGE of IDLE and NOOP, for which it wants the UID in every FETCH.

import (
	"fmt"
	"strings"
	"testing"
)

const thunderbirdHeaders = "(UID RFC822.SIZE FLAGS BODY.PEEK[HEADER.FIELDS (From To Cc Bcc Subject Date Message-ID Priority X-Priority " +
	"References Newsgroups In-Reply-To Content-Type Reply-To Received)])"

// thunderbird logs in the way Thunderbird does.
func (e *env) thunderbird() *client {
	e.t.Helper()
	c := e.dial()
	c.write("1 authenticate PLAIN\r\n")
	wantLines(e.t, "the challenge", []string{c.line()}, "+ ")
	c.write("AGZyZWRAZXhhbXBsZS50ZXN0AHNlY3JldA==\r\n") // NUL fred@example.test NUL secret
	if got := c.until("1"); !strings.HasPrefix(got[len(got)-1], "OK [CAPABILITY ") {
		e.t.Fatalf("authenticate PLAIN: %v", got)
	}
	c.expect(`ID ("name" "Thunderbird" "version" "153.7.0")`, `* ID ("name" "robinmail")`, "OK ID completed")
	c.expect("ENABLE UTF8=ACCEPT", "* ENABLED UTF8=ACCEPT", "OK ENABLE completed")
	return c
}

func TestThunderbirdSession(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 6)
	e.st.setFlag("INBOX", 2, "$seen", true)

	// Start: the folder list, with the roles, then the Inbox.
	c := e.thunderbird()
	c.expect("namespace", `* NAMESPACE (("" "/")) NIL NIL`, "OK NAMESPACE completed")
	c.expect(`list (subscribed) "" "*" return (special-use)`,
		`* LIST (\Subscribed \HasNoChildren \Drafts) "/" Drafts`,
		`* LIST (\Subscribed \HasNoChildren) "/" INBOX`,
		`* LIST (\Subscribed \HasNoChildren \Junk) "/" Junk`,
		`* LIST (\Subscribed \HasNoChildren \Sent) "/" Sent`,
		`* LIST (\Subscribed \HasNoChildren \Trash) "/" Trash`,
		"OK LIST completed")
	c.expect(`list "" "INBOX"`, `* LIST (\HasNoChildren) "/" INBOX`, "OK LIST completed")
	if got := c.status(`select "INBOX"`); got != "OK [READ-WRITE] SELECT completed" {
		t.Fatalf("select: %s", got)
	}
	c.expect("UID fetch 1:* (FLAGS)",
		"* 1 FETCH (UID 1 FLAGS ())", `* 2 FETCH (UID 2 FLAGS (\Seen))`, "* 3 FETCH (UID 3 FLAGS ())",
		"* 4 FETCH (UID 4 FLAGS ())", "* 5 FETCH (UID 5 FLAGS ())", "* 6 FETCH (UID 6 FLAGS ())", "OK UID FETCH completed")
	if got := c.ok("UID fetch 1:6 " + thunderbirdHeaders); len(got) != 6 || !strings.Contains(got[0], "BODY[HEADER.FIELDS (From To Cc Bcc Subject Date Message-ID ") {
		t.Errorf("the header fetch: %q", got)
	}
	// Opening a message.
	if got := c.ok("UID fetch 3 (UID RFC822.SIZE BODY[])"); len(got) != 2 || !strings.HasPrefix(got[0], "* 3 FETCH (UID 3 RFC822.SIZE ") ||
		!strings.Contains(got[0], " BODY[] {") || got[1] != `* 3 FETCH (UID 3 FLAGS (\Seen))` {
		t.Errorf("the body fetch: %q", got)
	}

	// Read, flag, tag, each in the spelling Thunderbird uses for it.
	c.expect(`uid store 1 +Flags (\Seen)`, `* 1 FETCH (UID 1 FLAGS (\Seen))`, "OK UID STORE completed")
	c.expect(`uid store 2 +Flags (\Flagged)`, `* 2 FETCH (UID 2 FLAGS (\Flagged \Seen))`, "OK UID STORE completed")
	c.expect(`uid store 5 +FLAGS ($label1)`,
		`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft $label1)`, `* 5 FETCH (UID 5 FLAGS ($label1))`, "OK UID STORE completed")
	c.expect(`uid store 5 -FLAGS ($label1)`, `* 5 FETCH (UID 5 FLAGS ())`, "OK UID STORE completed")
	c.expect(`uid store 1:2,5 -Flags (\Seen)`,
		"* 1 FETCH (UID 1 FLAGS ())", `* 2 FETCH (UID 2 FLAGS (\Flagged))`, "* 5 FETCH (UID 5 FLAGS ())", "OK UID STORE completed")
	c.expect(`uid store 4 +Flags (\Answered)`, `* 4 FETCH (UID 4 FLAGS (\Answered))`, "OK UID STORE completed")

	// Idle: a flag set elsewhere and new mail come with their UID, which
	// is all Thunderbird has to go by (it sends no CHANGEDSINCE).
	c.write("76 IDLE\r\n")
	wantLines(t, "IDLE", []string{c.line()}, "+ idling")
	e.st.setFlag("INBOX", 6, "$flagged", true)
	wantLines(t, "a flag during IDLE", []string{c.line()}, `* 6 FETCH (UID 6 FLAGS (\Flagged))`)
	e.st.add("INBOX", plain(7, "new mail"))
	wantLines(t, "new mail during IDLE", []string{c.line(), c.line()}, "* 7 EXISTS", "* 0 RECENT")
	c.write("DONE\r\n")
	wantLines(t, "DONE", []string{c.line()}, "76 OK IDLE completed")
	c.expect("noop", "OK NOOP completed")
	c.expect("UID fetch 7:* (FLAGS)", "* 7 FETCH (UID 7 FLAGS ())", "OK UID FETCH completed")
	// Nothing new: "n:*" still names the last message (RFC 3501 6.4.8).
	c.expect("UID fetch 8:* (FLAGS)", "* 7 FETCH (UID 7 FLAGS ())", "OK UID FETCH completed")

	// A folder with a name that is not ASCII, made, filled, renamed.
	c.expect(`create "Entwürfe für Köln"`, "OK CREATE completed")
	c.expect(`subscribe "Entwürfe für Köln"`, "OK SUBSCRIBE completed")
	c.expect(`list "" "Entwürfe für Köln"`, `* LIST (\HasNoChildren) "/" "Entwürfe für Köln"`, "OK LIST completed")
	c.expect(`create "Entwürfe für Köln/日本"`, "OK CREATE completed")
	c.expect(`subscribe "Entwürfe für Köln/日本"`, "OK SUBSCRIBE completed")
	if got := c.cmd(`uid copy 3 "Entwürfe für Köln/日本"`); len(got) != 1 || !strings.HasPrefix(got[0], "OK [COPYUID ") || !strings.HasSuffix(got[0], " 3 1] UID COPY completed") {
		t.Errorf("uid copy: %q", got)
	}
	// A second copy into a mailbox the message is in doubles nothing and
	// names the UID it has there.
	if got := c.cmd(`uid copy 3 "Entwürfe für Köln/日本"`); len(got) != 1 || !strings.HasSuffix(got[0], " 3 1] UID COPY completed") {
		t.Errorf("the second uid copy: %q", got)
	}
	if got := c.cmd(`uid move 1:2 "Entwürfe für Köln"`); len(got) != 4 || !strings.HasPrefix(got[0], "* OK [COPYUID ") || !strings.HasSuffix(got[0], " 1:2 1:2] moved") ||
		got[1] != "* 1 EXPUNGE" || got[2] != "* 1 EXPUNGE" || got[3] != "OK UID MOVE completed" {
		t.Errorf("uid move: %q", got)
	}
	// Rename: Thunderbird asks for the children, closes its mailbox,
	// renames, subscribes to the new names and unsubscribes from the old
	// ones, which are no longer there. RFC 3501 6.3.7 has NO only for a
	// subscription that cannot be removed; one that is not there is gone,
	// so that is OK here, and Thunderbird shows no error.
	c.expect(`list (subscribed) "" "Entwürfe für Köln/*" return (special-use)`,
		`* LIST (\Subscribed \HasNoChildren) "/" "Entwürfe für Köln/日本"`, "OK LIST completed")
	c.expect("close", "OK CLOSE completed")
	c.expect(`rename "Entwürfe für Köln" "Übersicht & Pläne"`, "OK RENAME completed")
	c.expect(`subscribe "Übersicht & Pläne"`, "OK SUBSCRIBE completed")
	c.expect(`unsubscribe "Entwürfe für Köln"`, "OK UNSUBSCRIBE completed")
	c.expect(`subscribe "Übersicht & Pläne/日本"`, "OK SUBSCRIBE completed")
	c.expect(`unsubscribe "Entwürfe für Köln/日本"`, "OK UNSUBSCRIBE completed")
	c.expect(`list "" "Übersicht & Pläne"`, `* LIST (\HasChildren) "/" "Übersicht & Pläne"`, "OK LIST completed")
	if got := c.ok(`select "Übersicht & Pläne"`); len(got) == 0 || got[1] != "* 2 EXISTS" {
		t.Errorf("the renamed mailbox: %q", got)
	}

	// Its copy of a sent message, and a draft saved twice: the second
	// version is appended, then the first is deleted and expunged by UID.
	msg := "Message-ID: <d8fc1f28@robin.test>\r\nDate: Sat, 14 Nov 2026 17:31:16 +0000\r\nMIME-Version: 1.0\r\nUser-Agent: Mozilla Thunderbird\r\n" +
		"To: Other <other@robin.test>\r\nFrom: Thunderbird User <fred@example.test>\r\nSubject: Grüße from Thunderbird\r\n" +
		"Content-Type: text/plain; charset=UTF-8; format=flowed\r\nContent-Transfer-Encoding: 8bit\r\n\r\nZweite Zeile: äöü €\r\n"
	appendTo := func(box, flags string) {
		t.Helper()
		c.tag++
		tag := fmt.Sprintf("a%03d", c.tag)
		c.write(fmt.Sprintf("%s append %q (%s) UTF8 (~{%d+}\r\n%s)\r\n", tag, box, flags, len(msg), msg))
		if got := c.until(tag); !strings.HasPrefix(got[len(got)-1], "OK [APPENDUID ") {
			t.Fatalf("append to %s: %q", box, got)
		}
	}
	appendTo("Sent", `\Seen`)
	c.ok(`select "Sent"`)
	c.expect("UID fetch 1:* (FLAGS)", `* 1 FETCH (UID 1 FLAGS (\Seen))`, "OK UID FETCH completed")
	appendTo("Drafts", `\Draft`)
	c.ok(`select "Drafts"`)
	appendTo("Drafts", `\Draft`)
	c.expect(`uid store 1 +Flags (\Seen \Deleted)`, `* 1 FETCH (UID 1 FLAGS (\Deleted \Seen \Draft))`, "OK UID STORE completed")
	c.expect("uid expunge 1", "* 1 EXPUNGE", "OK UID EXPUNGE completed")
	c.expect("UID fetch 1:* (FLAGS)", `* 1 FETCH (UID 2 FLAGS (\Draft))`, "OK UID FETCH completed")

	// Delete: a message is moved to Trash, a folder is renamed into it.
	c.ok(`select "INBOX"`)
	if got := c.cmd(`uid move 4 "Trash"`); len(got) != 3 || got[2] != "OK UID MOVE completed" {
		t.Errorf("delete: %q", got)
	}
	c.expect(`rename "Übersicht & Pläne/日本" "Trash/日本"`, "OK RENAME completed")
	c.expect(`subscribe "Trash/日本"`, "OK SUBSCRIBE completed")
	c.expect(`unsubscribe "Übersicht & Pläne/日本"`, "OK UNSUBSCRIBE completed")
	// Empty Trash: everything in it flagged and expunged, then the
	// folders under it deleted, and unsubscribed after they are gone.
	if got := c.ok(`select "Trash"`); len(got) == 0 || got[1] != "* 1 EXISTS" {
		t.Errorf("Trash: %q", got)
	}
	c.expect(`store 1:* +FLAGS.SILENT (\Deleted)`, "OK STORE completed")
	c.expect("expunge", "* 1 EXPUNGE", "OK EXPUNGE completed")
	c.expect(`list "" "Trash/*"`, `* LIST (\HasNoChildren) "/" "Trash/日本"`, "OK LIST completed")
	c.expect(`delete "Trash/日本"`, "OK DELETE completed")
	c.expect(`unsubscribe "Trash/日本"`, "OK UNSUBSCRIBE completed")
	c.expect(`list (subscribed) "" "*" return (special-use)`,
		`* LIST (\Subscribed \HasNoChildren \Drafts) "/" Drafts`,
		`* LIST (\Subscribed \HasNoChildren) "/" INBOX`,
		`* LIST (\Subscribed \HasNoChildren \Junk) "/" Junk`,
		`* LIST (\Subscribed \HasNoChildren \Sent) "/" Sent`,
		`* LIST (\Subscribed \HasNoChildren \Trash) "/" Trash`,
		`* LIST (\Subscribed \HasNoChildren) "/" "Übersicht & Pläne"`,
		"OK LIST completed")
}
