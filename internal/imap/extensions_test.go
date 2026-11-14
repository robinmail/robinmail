package imap

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Tests of the extensions, each from its RFC and with its examples.

// RFC 2177, the example:
//
//	C: A002 IDLE
//	S: + idling
//	...time passes; new mail arrives...
//	S: * 4 EXISTS
//	C: DONE
//	S: A002 OK IDLE terminated
//
// "The IDLE command is sent from the client to the server when the client
// is ready to accept unsolicited mailbox update messages." All three kinds
// come: EXISTS, FETCH and EXPUNGE (which may be sent here: a command is in
// progress and it is not FETCH, STORE or SEARCH).
func TestRFC2177Idle(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 3)
	c := e.selected("INBOX")
	c.write("A002 IDLE\r\n")
	wantLines(t, "IDLE", []string{c.line()}, "+ idling")
	c.quiet(100 * time.Millisecond)

	e.st.add("INBOX", plain(4, "new mail"))
	wantLines(t, "new mail during IDLE", []string{c.line(), c.line()}, "* 4 EXISTS", "* 0 RECENT")
	e.st.setFlag("INBOX", 2, "$seen", true)
	wantLines(t, "a flag during IDLE", []string{c.line()}, `* 2 FETCH (FLAGS (\Seen))`)
	e.st.remove("INBOX", 1)
	wantLines(t, "an expunge during IDLE", []string{c.line()}, "* 1 EXPUNGE")
	// A change in another mailbox wakes the session and says nothing.
	e.st.add("Sent", plain(1, "elsewhere"))
	c.quiet(100 * time.Millisecond)

	c.write("DONE\r\n")
	wantLines(t, "DONE", []string{c.line()}, "A002 OK IDLE completed")
	c.expect("FETCH 1:* UID", "* 1 FETCH (UID 2)", "* 2 FETCH (UID 3)", "* 3 FETCH (UID 4)", "OK FETCH completed")

	// "the client MUST NOT send a command while the server is waiting for
	// the DONE": anything but DONE ends the IDLE as a BAD command.
	c.write("A003 IDLE\r\n")
	c.line()
	c.write("A004 NOOP\r\n")
	wantLines(t, "a command instead of DONE", []string{c.line()}, "A003 BAD IDLE ends with DONE")

	// IDLE is valid in the authenticated state too; there is nothing to
	// report then. "done" in any case.
	a := e.login()
	a.write("A1 IDLE\r\n")
	a.line()
	e.st.add("INBOX", plain(5, "x"))
	a.quiet(100 * time.Millisecond)
	a.write("done\r\n")
	wantLines(t, "IDLE without a mailbox", []string{a.line()}, "A1 OK IDLE completed")
	e.dial().expect("IDLE", "BAD log in first")
}

// RFC 4315 (UIDPLUS). The examples:
//
//	S: A003 OK [APPENDUID 38505 3955] APPEND completed
//	S: A004 OK [COPYUID 38505 304,319:320 3956:3958] Done
//
// and for UID EXPUNGE (2.1):
//
//	C: A003 UID EXPUNGE 3000:3002
//	S: * 3 EXPUNGE
//	S: * 3 EXPUNGE
//	S: * 3 EXPUNGE
//	S: A003 OK UID EXPUNGE completed
//
// "permanently removes all messages that both have the \Deleted flag set
// and have a UID that is included in the specified sequence set".
func TestRFC4315UIDPlus(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 6)
	e.st.remove("INBOX", 2)
	c := e.selected("INBOX")
	c.ok("CREATE target")
	// UIDs 1,3:4 copied: not one range in the source, one in the target.
	c.expect("UID COPY 1,3:4 target", "OK [COPYUID 3857529051 1,3:4 1:3] UID COPY completed")
	c.ok(`STORE 1:5 +FLAGS.SILENT (\Deleted)`)
	// Messages with UIDs 3 to 5 go; UIDs 1 and 6 are marked too and stay.
	c.expect("UID EXPUNGE 3:5", "* 2 EXPUNGE", "* 2 EXPUNGE", "* 2 EXPUNGE", "OK UID EXPUNGE completed")
	c.expect("UID FETCH 1:* FLAGS", `* 1 FETCH (UID 1 FLAGS (\Deleted))`, `* 2 FETCH (UID 6 FLAGS (\Deleted))`, "OK UID FETCH completed")
	c.expect("UID EXPUNGE 100:200", "OK UID EXPUNGE completed")
	c.expect("UID EXPUNGE", "BAD a space is missing")
	// APPENDUID is in TestRFC3501Append.
}

// RFC 6851 (MOVE), the example of 3.3:
//
//	C: a UID MOVE 42:69 foo
//	S: * OK [COPYUID 432432 42:69 1202:1229]
//	S: * 22 EXPUNGE
//	S: (more expunges)
//	S: a OK Done
//
// 4.3: "Servers ... SHOULD send COPYUID in response to a UID MOVE (or MOVE)
// command ... in an untagged OK response before sending EXPUNGE".
func TestRFC6851Move(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 5)
	c := e.selected("INBOX")
	c.expect("MOVE 2:3 foo", "NO [TRYCREATE] no such mailbox")
	c.ok("CREATE foo")
	c.expect("UID MOVE 2:3 foo",
		"* OK [COPYUID 3857529051 2:3 1:2] moved", "* 2 EXPUNGE", "* 2 EXPUNGE", "OK UID MOVE completed")
	c.expect("MOVE 1 foo", "* OK [COPYUID 3857529051 1 3] moved", "* 1 EXPUNGE", "OK MOVE completed")
	c.expect("STATUS foo (MESSAGES)", "* STATUS foo (MESSAGES 3)", "OK STATUS completed")
	c.expect("STATUS INBOX (MESSAGES)", "* STATUS INBOX (MESSAGES 2)", "OK STATUS completed")
	// Another session on the source hears EXPUNGE, one on the target EXISTS.
	src, dst := e.selected("INBOX"), e.selected("foo")
	c.ok("MOVE 1 foo")
	src.expect("NOOP", "* 1 EXPUNGE", "OK NOOP completed")
	dst.expect("NOOP", "* 4 EXISTS", "* 0 RECENT", "OK NOOP completed")
	c.ok("EXAMINE INBOX")
	c.expect("MOVE 1 foo", "NO [READ-ONLY] the mailbox is open read-only")
}

// A mailbox is a label (the backend's rule, see cmd/robinmail/imap.go): a
// copy is the same message with a second UID, its flags are the same in
// both mailboxes, and a copy to where the message already is changes
// nothing and names the UID it has there. Expunging one of the two leaves
// the other, without the \Deleted mark.
func TestCopyIsALabel(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 2)
	c := e.selected("INBOX")
	c.ok("CREATE label")
	c.expect("COPY 1 label", "OK [COPYUID 3857529051 1 1] COPY completed")
	c.expect("COPY 1:2 label", "OK [COPYUID 3857529051 1:2 1:2] COPY completed")
	c.expect("COPY 1 INBOX", "OK [COPYUID 3857529046 1 1] COPY completed")
	// A move to where the message is gives it a new UID: for a client
	// the old message is gone and a new one came.
	c.expect("MOVE 2 INBOX", "* OK [COPYUID 3857529046 2 3] moved", "* 2 EXPUNGE", "* 2 EXISTS", "* 0 RECENT", "OK MOVE completed")
	c.expect("STATUS label (MESSAGES)", "* STATUS label (MESSAGES 2)", "OK STATUS completed")

	o := e.selected("label")
	c.ok(`STORE 1 +FLAGS.SILENT (\Flagged)`)
	o.expect("NOOP", `* 1 FETCH (FLAGS (\Flagged))`, "OK NOOP completed")
	c.ok(`STORE 1 +FLAGS.SILENT (\Deleted)`)
	o.expect("NOOP", `* 1 FETCH (FLAGS (\Flagged \Deleted))`, "OK NOOP completed")
	c.expect("EXPUNGE", "* 1 EXPUNGE", "OK EXPUNGE completed")
	o.expect("NOOP", `* 1 FETCH (FLAGS (\Flagged))`, "OK NOOP completed")
	o.expect("FETCH 1 BODY.PEEK[TEXT]", "* 1 FETCH (BODY[TEXT] {12}\r\nMessage 1.\r\n)", "OK FETCH completed")
}

// RFC 6154 (SPECIAL-USE). 2: the attributes \Drafts, \Junk, \Sent, \Trash
// (and \All, \Archive, \Flagged, which no mailbox here has). 5.1, the
// example of a plain LIST that shows them, which a server "MAY" do:
//
//	C: t1 LIST "" "%"
//	S: * LIST (\Marked \HasNoChildren) "/" Inbox
//	S: * LIST (\HasNoChildren) "/" ToDo
//	S: * LIST (\HasChildren) "/" Projects
//	S: * LIST (\Sent \HasNoChildren) "/" SentMail
//	S: * LIST (\Marked \Drafts \HasNoChildren) "/" MyDrafts
//	S: * LIST (\Trash \HasNoChildren) "/" Trash
//
// and 5.2, the selection option: only mailboxes with a special use.
func TestRFC6154SpecialUse(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	c.ok("CREATE ToDo")
	c.expect(`LIST "" "%"`,
		`* LIST (\HasNoChildren \Drafts) "/" Drafts`,
		`* LIST (\HasNoChildren) "/" INBOX`,
		`* LIST (\HasNoChildren \Junk) "/" Junk`,
		`* LIST (\HasNoChildren \Sent) "/" Sent`,
		`* LIST (\HasNoChildren) "/" ToDo`,
		`* LIST (\HasNoChildren \Trash) "/" Trash`,
		"OK LIST completed")
	c.expect(`LIST (SPECIAL-USE) "" "*"`,
		`* LIST (\HasNoChildren \Drafts) "/" Drafts`,
		`* LIST (\HasNoChildren \Junk) "/" Junk`,
		`* LIST (\HasNoChildren \Sent) "/" Sent`,
		`* LIST (\HasNoChildren \Trash) "/" Trash`,
		"OK LIST completed")
	c.expect(`LIST "" "T*" RETURN (SPECIAL-USE)`,
		`* LIST (\HasNoChildren) "/" ToDo`, `* LIST (\HasNoChildren \Trash) "/" Trash`, "OK LIST completed")
}

// RFC 2342 (NAMESPACE), example 5.1, "a server that supports a single
// personal namespace. No leading prefix is used on personal mailboxes and
// '/' is the hierarchy delimiter":
//
//	C: A001 NAMESPACE
//	S: * NAMESPACE (("" "/")) NIL NIL
//	S: A001 OK NAMESPACE command completed
func TestRFC2342Namespace(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	c.expect("NAMESPACE", `* NAMESPACE (("" "/")) NIL NIL`, "OK NAMESPACE completed")
	c.expect("NAMESPACE x", `BAD unexpected text after the command: " x"`)
	e.dial().expect("NAMESPACE", "BAD log in first")
}

// RFC 7888 (LITERAL+), the example of section 3:
//
//	C: A001 LOGIN {11+}
//	C: FRED FOOBAR {7+}
//	C: fat man
//	S: A001 OK LOGIN completed
//
// The client does not wait and the server sends no continuation.
func TestRFC7888LiteralPlus(t *testing.T) {
	e := newEnv(t)
	e.st.name, e.st.password = "FRED FOOBAR", "fat man"
	c := e.dial()
	c.write("A001 LOGIN {11+}\r\nFRED FOOBAR {7+}\r\nfat man\r\n")
	if got := c.line(); !strings.HasPrefix(got, "A001 OK ") {
		t.Fatalf("LOGIN with non-synchronizing literals: %q", got)
	}
	// A literal as mailbox name, and an empty one.
	c.write("A002 CREATE {7+}\r\nfoo bar\r\n")
	wantLines(t, "CREATE with a literal", c.until("A002"), "OK CREATE completed")
	c.expect(`LIST "" foo*`, `* LIST (\HasNoChildren) "/" "foo bar"`, "OK LIST completed")
	c.write("A003 LIST {0+}\r\n {3+}\r\nfoo\r\n")
	wantLines(t, "LIST with literals", c.until("A003"), "OK LIST completed")

	// RFC 7888 4: a literal that is too large. A synchronizing one is
	// refused before it is sent; a non-synchronizing one is on its way
	// already, and "the server ... MUST ... close the connection".
	c.write(fmt.Sprintf("A004 CREATE {%d}\r\n", maxLiteral+1))
	wantLines(t, "a name of 64 KiB", []string{c.line()},
		fmt.Sprintf("A004 NO [TOOBIG] too large: a message may have %d octets, any other literal %d", 25<<20, maxLiteral))
	c.ok("NOOP")
	c.write(fmt.Sprintf("A005 APPEND INBOX {%d}\r\n", 25<<20+1))
	if got := c.line(); !strings.HasPrefix(got, "A005 NO [TOOBIG]") {
		t.Fatalf("a message over the limit: %q", got)
	}
	c.write(fmt.Sprintf("A006 APPEND INBOX {%d+}\r\n", 25<<20+1))
	wantLines(t, "a non-synchronizing literal over the limit", []string{c.line()}, "* BYE [TOOBIG] literal too large")
	if !c.eof() {
		t.Error("the connection stayed open")
	}
}

// RFC 2971 (ID), the example of 3.1:
//
//	C: a023 ID ("name" "sodr" "version" "19.34" "vendor" "Pink Floyd Music Limited")
//	S: * ID NIL
//	S: a023 OK ID completed
//
// and "ID NIL". We say our name and nothing else (3.3 warns against
// giving out more). It is valid in every state.
func TestRFC2971ID(t *testing.T) {
	e := newEnv(t)
	c := e.dial()
	c.expect(`ID ("name" "sodr" "version" "19.34" "vendor" "Pink Floyd Music Limited")`, `* ID ("name" "robinmail")`, "OK ID completed")
	c.expect(`ID NIL`, `* ID ("name" "robinmail")`, "OK ID completed")
	c.expect(`ID ("name" NIL)`, `* ID ("name" "robinmail")`, "OK ID completed")
	c.expect(`ID ("name")`, "BAD ID takes pairs of field and value")
	c.expect(`ID name`, "BAD ID takes a list or NIL")
	c.ok("LOGIN fred@example.test secret")
	c.expect(`ID ("name" "iPhone Mail" "version" "20A362")`, `* ID ("name" "robinmail")`, "OK ID completed")
}

// RFC 5161 (ENABLE), the example of 3.1 in our terms: capabilities that
// can be enabled are, the others are ignored, and the response lists what
// was enabled:
//
//	C: t2 ENABLE CONDSTORE X-GOOD-IDEA
//	S: * ENABLED X-GOOD-IDEA
//	S: t2 OK foo
//
// "The ENABLE command is only valid in the authenticated state, before
// any mailbox is selected."
func TestRFC5161Enable(t *testing.T) {
	e := newEnv(t)
	e.dial().expect("ENABLE CONDSTORE", "BAD log in first")
	c := e.login()
	c.expect("ENABLE CONDSTORE X-GOOD-IDEA", "* ENABLED CONDSTORE", "OK ENABLE completed")
	c.expect("ENABLE X-GOOD-IDEA", "* ENABLED", "OK ENABLE completed")
	c.expect("ENABLE UTF8=ACCEPT imap4rev2", "* ENABLED UTF8=ACCEPT IMAP4rev2", "OK ENABLE completed")
	c.ok("SELECT INBOX")
	c.expect("ENABLE CONDSTORE", "BAD ENABLE before a mailbox is selected")
}

// RFC 7162 (CONDSTORE). 3.1.8, SELECT with the CONDSTORE parameter and the
// HIGHESTMODSEQ response code (3.1.2.1):
//
//	C: A142 SELECT INBOX (CONDSTORE)
//	S: * 172 EXISTS
//	...
//	S: * OK [HIGHESTMODSEQ 715194045007]
//
// 3.1.4.1, FETCH with CHANGEDSINCE:
//
//	C: s100 UID FETCH 1:* (FLAGS) (CHANGEDSINCE 12345)
//	S: * 1 FETCH (UID 4 MODSEQ (65402) FLAGS (\Seen))
//
// 3.1.3, STORE with UNCHANGEDSINCE and the MODIFIED response code:
//
//	C: d105 STORE 7,5,9 (UNCHANGEDSINCE 320162338) +FLAGS.SILENT (\Deleted)
//	S: * 5 FETCH (MODSEQ (320162350))
//	S: d105 OK [MODIFIED 7,9] Conditional STORE failed
//
// 3.1.5, SEARCH MODSEQ:
//
//	C: a SEARCH MODSEQ "/flags/\\draft" all 620162338
//	S: * SEARCH 2 5 6 7 11 12 18 19 20 23 (MODSEQ 917162500)
func TestRFC7162Condstore(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 9) // modseq 2..10
	c := e.login()
	sel := c.ok("SELECT INBOX (CONDSTORE)")
	if !equalLines(sel[len(sel)-1:], []string{"* OK [HIGHESTMODSEQ 10] the highest modseq"}) {
		t.Fatalf("SELECT (CONDSTORE): %v", sel)
	}
	c.expect("FETCH 5 (UID MODSEQ)", "* 5 FETCH (UID 5 MODSEQ (6))", "OK FETCH completed")

	// Another client changes 4 and 7; this one asks what changed.
	e.st.setFlag("INBOX", 4, "$seen", true)    // modseq 11
	e.st.setFlag("INBOX", 7, "$flagged", true) // modseq 12
	c.expect("UID FETCH 1:* (FLAGS) (CHANGEDSINCE 10)",
		`* 4 FETCH (UID 4 FLAGS (\Seen) MODSEQ (11))`, `* 7 FETCH (UID 7 FLAGS (\Flagged) MODSEQ (12))`, "OK UID FETCH completed")
	c.expect("UID FETCH 1:* (FLAGS) (CHANGEDSINCE 12)", "OK UID FETCH completed")

	// The client last saw the mailbox at modseq 10: messages 7 has moved
	// on since and is left alone; 5 and 9 are changed, and with .SILENT
	// only their new MODSEQ is sent.
	c.expect(`STORE 7,5,9 (UNCHANGEDSINCE 10) +FLAGS.SILENT (\Deleted)`,
		"* 5 FETCH (UID 5 MODSEQ (13))", "* 9 FETCH (UID 9 MODSEQ (13))",
		"OK [MODIFIED 7] conditional STORE failed for some messages")
	c.expect("FETCH 7 FLAGS", `* 7 FETCH (FLAGS (\Flagged))`, "OK FETCH completed")
	c.expect(`UID STORE 7 (UNCHANGEDSINCE 12) +FLAGS (\Deleted)`,
		`* 7 FETCH (UID 7 FLAGS (\Flagged \Deleted) MODSEQ (14))`, "OK UID STORE completed")
	// "UNCHANGEDSINCE 0": fails for every message that has a modseq.
	c.expect(`STORE 1:2 (UNCHANGEDSINCE 0) +FLAGS (\Seen)`, "OK [MODIFIED 1:2] conditional STORE failed for some messages")

	c.expect(`SEARCH MODSEQ "/flags/\\draft" all 12`, "* SEARCH 5 7 9 (MODSEQ 14)", "OK SEARCH completed")
	c.expect("SEARCH MODSEQ 13", "* SEARCH 5 7 9 (MODSEQ 14)", "OK SEARCH completed")
	c.expect("SEARCH MODSEQ 99", "* SEARCH", "OK SEARCH completed")
	c.expect("UID SEARCH UNSEEN MODSEQ 14", "* SEARCH 7 (MODSEQ 14)", "OK UID SEARCH completed")

	// 3.1.7: STATUS with HIGHESTMODSEQ.
	c.expect("STATUS INBOX (HIGHESTMODSEQ MESSAGES)", "* STATUS INBOX (HIGHESTMODSEQ 14 MESSAGES 9)", "OK STATUS completed")

	// 3.1.4.2 and 3.2: once CONDSTORE is on, a FETCH that reports a flag
	// change carries the MODSEQ; it is turned on by any of the commands
	// above ("CONDSTORE enabling command") or by ENABLE.
	e.st.setFlag("INBOX", 1, "$answered", true)
	c.expect("NOOP", `* 1 FETCH (UID 1 FLAGS (\Answered) MODSEQ (15))`, "OK NOOP completed")
	plainClient := e.selected("INBOX")
	e.st.setFlag("INBOX", 2, "$answered", true)
	plainClient.expect("NOOP", `* 2 FETCH (FLAGS (\Answered))`, "OK NOOP completed")
	// 3.1.2.1: the command that turns CONDSTORE on says HIGHESTMODSEQ.
	plainClient.expect("FETCH 2 MODSEQ", "* OK [HIGHESTMODSEQ 16] the highest modseq", "* 2 FETCH (MODSEQ (16))", "OK FETCH completed")
	e.st.setFlag("INBOX", 3, "$answered", true)
	plainClient.expect("NOOP", `* 3 FETCH (FLAGS (\Answered) MODSEQ (17))`, "OK NOOP completed")

	c.ok("NOOP")
	c.expect("SELECT INBOX (QRESYNC (1 2))", "BAD unknown SELECT parameter QRESYNC")
	c.expect("FETCH 1 FLAGS (VANISHED)", "BAD unknown FETCH modifier")
}

// RFC 3691 (UNSELECT): "frees server's resources associated with the
// selected mailbox and returns the server to the authenticated state. This
// command performs the same actions as CLOSE, except that no messages are
// permanently removed from the currently selected mailbox." The example:
//
//	C: A342 UNSELECT
//	S: A342 OK Unselect completed
func TestRFC3691Unselect(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 2)
	c := e.selected("INBOX")
	c.ok(`STORE 1 +FLAGS.SILENT (\Deleted)`)
	c.expect("UNSELECT", "OK UNSELECT completed")
	c.expect("FETCH 1 FLAGS", "BAD select a mailbox first")
	c.expect("UNSELECT", "BAD select a mailbox first")
	c.expect("STATUS INBOX (MESSAGES)", "* STATUS INBOX (MESSAGES 2)", "OK STATUS completed")
}

// RFC 5258 (LIST-EXTENDED). The examples of section 5, in our hierarchy:
// selection options SUBSCRIBED and RECURSIVEMATCH, return options
// SUBSCRIBED and CHILDREN, several patterns. 5, example 8 (RECURSIVEMATCH):
//
//	C: C04 LIST (SUBSCRIBED RECURSIVEMATCH) "" "%"
//	S: * LIST (\Subscribed) "/" "INBOX"
//	S: * LIST () "/" "Foo" ("CHILDINFO" ("SUBSCRIBED"))
//
// and RFC 3348 (CHILDREN): \HasChildren and \HasNoChildren.
func TestRFC5258ListExtended(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	c.ok("CREATE Foo/Bar")
	c.ok("CREATE Foo/Baz")
	// A mailbox made with CREATE is not subscribed until SUBSCRIBE.
	c.ok("SUBSCRIBE Foo/Bar")
	for _, name := range []string{"Drafts", "Sent", "Junk", "Trash"} {
		c.ok("UNSUBSCRIBE " + name)
	}
	c.expect(`LIST (SUBSCRIBED) "" "*"`,
		`* LIST (\Subscribed \HasChildren) "/" Foo/Bar`[:0]+`* LIST (\Subscribed \HasNoChildren) "/" Foo/Bar`,
		`* LIST (\Subscribed \HasNoChildren) "/" INBOX`,
		"OK LIST completed")
	c.expect(`LIST (SUBSCRIBED RECURSIVEMATCH) "" "%"`,
		`* LIST (\HasChildren) "/" Foo ("CHILDINFO" ("SUBSCRIBED"))`,
		`* LIST (\Subscribed \HasNoChildren) "/" INBOX`,
		"OK LIST completed")
	c.expect(`LIST "" "Foo*" RETURN (SUBSCRIBED CHILDREN)`,
		`* LIST (\HasChildren) "/" Foo`,
		`* LIST (\Subscribed \HasNoChildren) "/" Foo/Bar`,
		`* LIST (\HasNoChildren) "/" Foo/Baz`,
		"OK LIST completed")
	// A child that the patterns list itself is no CHILDINFO of its parent.
	c.expect(`LIST (SUBSCRIBED RECURSIVEMATCH) "" (Foo Foo/Bar)`, `* LIST (\Subscribed \HasNoChildren) "/" Foo/Bar`, "OK LIST completed")
	c.expect(`LIST (SUBSCRIBED RECURSIVEMATCH) "" Foo`, `* LIST (\HasChildren) "/" Foo ("CHILDINFO" ("SUBSCRIBED"))`, "OK LIST completed")
	// Several patterns (5, example 4).
	c.expect(`LIST "" ("INBOX" "Drafts" "Foo/%")`,
		`* LIST (\HasNoChildren \Drafts) "/" Drafts`,
		`* LIST (\HasNoChildren) "/" Foo/Bar`,
		`* LIST (\HasNoChildren) "/" Foo/Baz`,
		`* LIST (\HasNoChildren) "/" INBOX`,
		"OK LIST completed")
	// REMOTE is accepted: there are no remote mailboxes to add.
	c.expect(`LIST (REMOTE) "" "INBOX"`, `* LIST (\HasNoChildren) "/" INBOX`, "OK LIST completed")
	// 3.1: "RECURSIVEMATCH ... MUST NOT be used alone".
	c.expect(`LIST (RECURSIVEMATCH) "" "%"`, "BAD RECURSIVEMATCH needs SUBSCRIBED")
	c.expect(`LIST (COLOUR) "" "%"`, "BAD unknown LIST selection option COLOUR")
	c.expect(`LIST "" "%" RETURN (COLOUR)`, "BAD unknown LIST return option COLOUR")
	// An extended LIST of the empty name is not the delimiter question.
	c.expect(`LIST () "" ""`, "OK LIST completed")
}

// RFC 5819 (LIST-STATUS), the example of section 2:
//
//	C: A01 LIST "" % RETURN (STATUS (MESSAGES UNSEEN))
//	S: * LIST () "."  "INBOX"
//	S: * STATUS "INBOX" (MESSAGES 17 UNSEEN 16)
//	S: * LIST () "." "foo"
//	S: * STATUS "foo" (MESSAGES 30 UNSEEN 29)
//	S: * LIST (\NoSelect) "." "bar"
//	S: A01 OK List completed.
func TestRFC5819ListStatus(t *testing.T) {
	e := newEnv(t)
	for i := 1; i <= 17; i++ {
		if i == 1 {
			e.st.add("INBOX", plain(i, "s"), "$seen")
		} else {
			e.st.add("INBOX", plain(i, "s"))
		}
	}
	c := e.login()
	c.ok("CREATE foo")
	e.st.add("foo", plain(1, "x"))
	c.expect(`LIST "" % RETURN (STATUS (MESSAGES UNSEEN))`,
		`* LIST (\HasNoChildren \Drafts) "/" Drafts`, "* STATUS Drafts (MESSAGES 0 UNSEEN 0)",
		`* LIST (\HasNoChildren) "/" INBOX`, "* STATUS INBOX (MESSAGES 17 UNSEEN 16)",
		`* LIST (\HasNoChildren \Junk) "/" Junk`, "* STATUS Junk (MESSAGES 0 UNSEEN 0)",
		`* LIST (\HasNoChildren \Sent) "/" Sent`, "* STATUS Sent (MESSAGES 0 UNSEEN 0)",
		`* LIST (\HasNoChildren \Trash) "/" Trash`, "* STATUS Trash (MESSAGES 0 UNSEEN 0)",
		`* LIST (\HasNoChildren) "/" foo`, "* STATUS foo (MESSAGES 1 UNSEEN 1)",
		"OK LIST completed")
	c.expect(`LIST "" foo RETURN (STATUS (COLOUR))`, "BAD unknown STATUS item COLOUR")
}

// RFC 4959 (SASL-IR) with PLAIN (RFC 4616). The example of RFC 4959 4:
//
//	C: A01 AUTHENTICATE PLAIN dGVzdAB0ZXN0AHRlc3Q=
//	S: A01 OK Success (Privacy protection)
//
// and without an initial response the server sends an empty challenge
// (RFC 3501 6.2.2). RFC 4616 2: "authzid NUL authcid NUL passwd"; an
// authorization identity other than the user's own is refused.
func TestRFC4959AuthenticatePlain(t *testing.T) {
	e := newEnv(t)
	e.st.name, e.st.password = "test", "test"
	c := e.dial()
	if got := c.status("AUTHENTICATE PLAIN dGVzdAB0ZXN0AHRlc3Q="); !strings.HasPrefix(got, "OK [CAPABILITY ") {
		t.Fatalf("with an initial response: %q", got)
	}

	c = e.dial()
	c.write("a1 AUTHENTICATE PLAIN\r\n")
	wantLines(t, "the challenge", []string{c.line()}, "+ ")
	c.write("AHRlc3QAdGVzdA==\r\n") // NUL test NUL test
	if got := c.line(); !strings.HasPrefix(got, "a1 OK ") {
		t.Fatalf("without an initial response: %q", got)
	}

	c = e.dial()
	// RFC 3501 6.2.2: "*" cancels.
	c.write("a1 AUTHENTICATE PLAIN\r\n")
	c.line()
	c.write("*\r\n")
	wantLines(t, "cancelled", []string{c.line()}, "a1 BAD authentication cancelled")
	c.expect("AUTHENTICATE PLAIN AHRlc3QAd3Jvbmc=", "NO [AUTHENTICATIONFAILED] wrong name or password")
	c.expect("AUTHENTICATE PLAIN !!!", "BAD the response is not base64")
	c.expect("AUTHENTICATE PLAIN dGVzdA==", "BAD not a PLAIN response")
	// authzid "admin", authcid "test".
	c.expect("AUTHENTICATE PLAIN YWRtaW4AdGVzdAB0ZXN0", "NO [AUTHORIZATIONFAILED] you can only log in as yourself")
	c.expect("AUTHENTICATE GSSAPI", "NO unsupported authentication mechanism")
	c.expect("AUTHENTICATE", "BAD a space is missing")
}

// RFC 4731 (ESEARCH), the examples of 3.1:
//
//	C: A282 SEARCH RETURN (MIN COUNT) FLAGGED SINCE 1-Feb-1994 NOT FROM "Smith"
//	S: * ESEARCH (TAG "A282") MIN 2 COUNT 3
//
//	C: A283 SEARCH RETURN () FLAGGED SINCE 1-Feb-1994 NOT FROM "Smith"
//	S: * ESEARCH (TAG "A283") ALL 2,10:11
//
//	C: A284 SEARCH RETURN (MIN MAX ALL) ...   (no match)
//	S: * ESEARCH (TAG "A284")
//
//	C: A285 UID SEARCH RETURN (MIN MAX) 1:5000
//	S: * ESEARCH (TAG "A285") UID MIN 7 MAX 3800
func TestRFC4731Esearch(t *testing.T) {
	e := newEnv(t)
	searchBox(e)
	e.st.remove("INBOX", 1)
	c := e.selected("INBOX") // UIDs 2 3 4 5 as messages 1 2 3 4
	run := func(command, want string) {
		t.Helper()
		c.tag++
		tag := fmt.Sprintf("A%d", c.tag)
		c.write(tag + " " + command + "\r\n")
		lines := c.until(tag)
		wantLines(t, command, lines, strings.ReplaceAll(want, "TAG", `(TAG "`+tag+`")`), "OK "+strings.SplitN(command, " RETURN", 2)[0]+" completed")
	}
	run(`SEARCH RETURN (MIN COUNT) FLAGGED SINCE 1-Feb-1994 NOT FROM "Smith"`, "* ESEARCH TAG MIN 1 COUNT 1")
	run(`SEARCH RETURN () SINCE 1-Feb-1994`, "* ESEARCH TAG ALL 1:4")
	run(`SEARCH RETURN (ALL) OR FLAGGED DRAFT`, "* ESEARCH TAG ALL 1:2,4")
	run(`SEARCH RETURN (MIN MAX ALL) TEXT "string not in mailbox"`, "* ESEARCH TAG")
	run(`SEARCH RETURN (COUNT) TEXT "string not in mailbox"`, "* ESEARCH TAG COUNT 0")
	run(`UID SEARCH RETURN (MIN MAX) 1:5000`, "* ESEARCH TAG UID MIN 2 MAX 5")
	run(`UID SEARCH RETURN (MAX MIN COUNT ALL) UNSEEN`, "* ESEARCH TAG UID MIN 2 MAX 5 ALL 2:3,5 COUNT 3")
	c.expect("SEARCH RETURN (BEST) ALL", "BAD unknown SEARCH return option BEST")
	// With CONDSTORE (RFC 7162 3.1.5): MODSEQ in the ESEARCH.
	c.ok("FETCH 1 MODSEQ")
	run(`SEARCH RETURN (ALL) MODSEQ 1`, "* ESEARCH TAG ALL 1:4 MODSEQ 6")
	// RFC 7162 3.1.5: with MIN or MAX alone, the MODSEQ of that message.
	run(`SEARCH RETURN (MIN) MODSEQ 1`, "* ESEARCH TAG MIN 1 MODSEQ 3")
	run(`SEARCH RETURN (MAX) MODSEQ 1`, "* ESEARCH TAG MAX 4 MODSEQ 6")
	run(`SEARCH RETURN (MIN MAX) 1:2 MODSEQ 1`, "* ESEARCH TAG MIN 1 MAX 2 MODSEQ 4")
}

// RFC 5182 (SEARCHRES): SAVE and "$". The examples of 2.1:
//
//	C: A282 SEARCH RETURN (SAVE) FLAGGED SINCE 1-Feb-1994 NOT FROM "Smith"
//	S: A282 OK SEARCH completed, result saved
//	C: A283 FETCH $ (UID INTERNALDATE FLAGS RFC822.HEADER)
//	S: * 2 FETCH (UID 14 ...
//
//	C: A300 MOVE $ "Other Messages"
//
// 2.4 says what "$" holds when SAVE comes with MIN or MAX; 2.1 that a
// message that is expunged leaves the result, and that a failed search
// empties it.
func TestRFC5182SearchRes(t *testing.T) {
	e := newEnv(t)
	searchBox(e)
	c := e.selected("INBOX")
	c.expect("FETCH $ UID", "BAD there is no saved search result")
	c.expect("SEARCH RETURN (SAVE) FLAGGED SINCE 1-Feb-1994", "OK SEARCH completed")
	c.expect("FETCH $ UID", "* 2 FETCH (UID 2)", "* 3 FETCH (UID 3)", "OK FETCH completed")
	c.expect("UID FETCH $ FLAGS", `* 2 FETCH (UID 2 FLAGS (\Flagged))`, `* 3 FETCH (UID 3 FLAGS (\Answered \Flagged))`, "OK UID FETCH completed")
	// "$" in a search: the saved result as a key.
	c.expect("SEARCH $ ANSWERED", "* SEARCH 3", "OK SEARCH completed")
	c.expect("SEARCH NOT $", "* SEARCH 1 4 5", "OK SEARCH completed")
	c.expect(`STORE $ +FLAGS.SILENT (\Seen)`, "OK STORE completed")
	c.expect("SEARCH RETURN (SAVE MIN) SEEN", "* ESEARCH (TAG \"a010\") MIN 1", "OK SEARCH completed")
	c.expect("FETCH $ UID", "* 1 FETCH (UID 1)", "OK FETCH completed")
	c.expect("SEARCH RETURN (SAVE MIN MAX) SEEN", "* ESEARCH (TAG \"a012\") MIN 1 MAX 4", "OK SEARCH completed")
	c.expect("FETCH $ UID", "* 1 FETCH (UID 1)", "* 4 FETCH (UID 4)", "OK FETCH completed")
	// A message of the result that is expunged is no longer in it.
	c.ok("SEARCH RETURN (SAVE) 1:3")
	e.st.remove("INBOX", 2)
	c.ok("NOOP")
	c.expect("FETCH $ UID", "* 1 FETCH (UID 1)", "* 2 FETCH (UID 3)", "OK FETCH completed")
	c.ok("CREATE Other")
	c.expect(`MOVE $ Other`, "* OK [COPYUID 3857529051 1,3 1:2] moved", "* 1 EXPUNGE", "* 1 EXPUNGE", "OK MOVE completed")
	c.expect("FETCH $ UID", "OK FETCH completed")
	// A search that finds nothing saves the empty result.
	c.ok("SEARCH RETURN (SAVE) ALL")
	c.expect("SEARCH RETURN (SAVE) FROM nobody", "OK SEARCH completed")
	c.expect("FETCH $ UID", "OK FETCH completed")
}

// RFC 3516 (BINARY), FETCH only: BINARY[section] is the part with its
// content transfer encoding undone; BINARY.PEEK does not set \Seen;
// BINARY.SIZE is the decoded size. The example of 4.1:
//
//	C: t1 FETCH 2 (BINARY[1.2])   -> * 2 FETCH (BINARY[1.2] {n} ...)
//
// Content with a NUL goes out as a literal8, "~{n}" (4.2).
func TestRFC3516Binary(t *testing.T) {
	e := newEnv(t)
	e.st.add("INBOX", "Content-Type: multipart/mixed; boundary=b\r\n\r\n"+
		"--b\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nGr=C3=BC=C3=9Fe=\r\n aus K=C3=B6ln\r\n"+
		"--b\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nAAEC\r\n/w==\r\n"+
		"--b\r\nContent-Type: text/plain\r\n\r\nas it is\r\n"+
		"--b\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: x-rot13\r\n\r\nuryyb\r\n"+
		"--b--\r\n")
	c := e.selected("INBOX")
	c.expect("FETCH 1 (BINARY.PEEK[1] BINARY.SIZE[1])",
		"* 1 FETCH (BINARY[1] {17}\r\nGrüße aus Köln BINARY.SIZE[1] 17)", "OK FETCH completed")
	c.expect("FETCH 1 BINARY.PEEK[2]", "* 1 FETCH (BINARY[2] ~{4}\r\n\x00\x01\x02\xff)", "OK FETCH completed")
	c.expect("FETCH 1 BINARY.PEEK[3]<3.2>", "* 1 FETCH (BINARY[3]<3> {2}\r\nit)", "OK FETCH completed")
	c.expect("FETCH 1 FLAGS", "* 1 FETCH (FLAGS ())", "OK FETCH completed")
	c.expect("FETCH 1 BINARY[3]", "* 1 FETCH (BINARY[3] {8}\r\nas it is)", `* 1 FETCH (FLAGS (\Seen))`, "OK FETCH completed")
	c.expect("FETCH 1 BINARY.SIZE[9]", "* 1 FETCH (BINARY.SIZE[9] 0)", "OK FETCH completed")
	// 4.3: "If the server does not know how to decode the section's CTE,
	// it MUST fail the request and issue a 'NO' response that contains
	// the 'UNKNOWN-CTE' extended response code."
	c.expect("FETCH 1 BINARY.PEEK[4]", "NO [UNKNOWN-CTE] a part has a content transfer encoding that cannot be decoded")
	c.expect("FETCH 1 BINARY[HEADER]", "BAD a BINARY section is a part number")
	c.expect("FETCH 1 BINARY", "BAD BINARY needs a section")
}

// RFC 3501 5.1.3, modified UTF-7, with the example of the RFC:
// "~peter/mail/&U,BTFw-/&ZeVnLIqe-" is "~peter/mail/台北/日本語". And "&-"
// is "&".
func TestRFC3501ModifiedUTF7(t *testing.T) {
	for _, tc := range []struct{ utf8, utf7 string }{
		{"~peter/mail/台北/日本語", "~peter/mail/&U,BTFw-/&ZeVnLIqe-"},
		{"Entwürfe", "Entw&APw-rfe"},
		{"R&D", "R&-D"},
		{"&", "&-"},
		{"plain", "plain"},
		{"", ""},
		{"😀", "&2D3eAA-"},
		{"a&b😀c", "a&-b&2D3eAA-c"},
	} {
		if got := encodeUTF7(tc.utf8); got != tc.utf7 {
			t.Errorf("encode(%q) = %q, want %q", tc.utf8, got, tc.utf7)
		}
		if got, err := decodeUTF7(tc.utf7); err != nil || got != tc.utf8 {
			t.Errorf("decode(%q) = %q, %v; want %q", tc.utf7, got, err, tc.utf8)
		}
	}
	// 5.1.3: "'&' is used to shift to modified BASE64 and '-' to shift
	// back"; base64 "MUST NOT be used to represent any printing US-ASCII
	// character which can represent itself"; a name does not end inside a
	// shift, has no 8-bit octets, and null shifts ("&AAA-&AAA-" for one
	// run) are not permitted.
	for _, bad := range []string{"&", "&U,BTFw", "&AGE-", "caf\xc3\xa9", "&U,BT-&Fw-", "&*-", "&U,B-", "a\x01b", "&2D0-"} {
		if got, err := decodeUTF7(bad); err == nil {
			t.Errorf("decode(%q) = %q, want an error", bad, got)
		}
	}

	// On the wire: names go out encoded and are understood encoded.
	e := newEnv(t)
	c := e.login()
	c.expect("CREATE Entw&APw-rfe/&U,BTFw-", "OK CREATE completed")
	c.expect(`LIST "" Entw*`,
		`* LIST (\HasChildren) "/" Entw&APw-rfe`, `* LIST (\HasNoChildren) "/" Entw&APw-rfe/&U,BTFw-`, "OK LIST completed")
	c.ok("SELECT Entw&APw-rfe/&U,BTFw-")
	c.expect("CREATE caf&AOk", "NO [CANNOT] the mailbox name is not modified UTF-7")
	c.expect("SELECT caf&AOk", "NO [CANNOT] the mailbox name is not modified UTF-7")
	c.expect(`LIST "" caf&AOk*`, "OK LIST completed")
	c.expect(`CREATE "café"`, "BAD 8-bit characters in a quoted string")
	if b := e.st.box("台北"); b == nil {
		t.Error("the store does not have the mailbox under its UTF-8 name")
	}
}

// RFC 6855 (UTF8=ACCEPT). 3: after ENABLE UTF8=ACCEPT "mailbox names ...
// MUST ... be UTF-8" and the server may send UTF-8 in quoted strings.
// 4: APPEND takes a message with UTF-8 headers as "UTF8 (~{n} ...)".
func TestRFC6855UTF8Accept(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	c.ok("CREATE Entw&APw-rfe")
	c.write("a1 APPEND INBOX UTF8 (~{3+}\r\nabc)\r\n")
	wantLines(t, "APPEND UTF8 before ENABLE", c.until("a1"), "BAD APPEND with UTF8 needs ENABLE UTF8=ACCEPT")
	c.expect("ENABLE UTF8=ACCEPT", "* ENABLED UTF8=ACCEPT", "OK ENABLE completed")
	c.expect(`LIST "" Entw*`, `* LIST (\HasNoChildren) "/" "Entwürfe"`, "OK LIST completed")
	c.expect(`CREATE "Entwürfe/台北"`, "OK CREATE completed")
	c.expect(`STATUS "Entwürfe/台北" (MESSAGES)`, `* STATUS "Entwürfe/台北" (MESSAGES 0)`, "OK STATUS completed")
	// "&" is a character like any other now.
	c.expect(`CREATE "R&D"`, "OK CREATE completed")
	c.expect(`LIST "" R*`, `* LIST (\HasNoChildren) "/" R&D`, "OK LIST completed")

	msg := "From: Jürgen <j@example.org>\r\nSubject: Grüße\r\n\r\nHallo\r\n"
	c.write(fmt.Sprintf("a2 APPEND INBOX (\\Seen) UTF8 (~{%d+}\r\n%s)\r\n", len(msg), msg))
	if got := c.until("a2"); !strings.HasPrefix(got[len(got)-1], "OK [APPENDUID ") {
		t.Fatalf("APPEND UTF8: %v", got)
	}
	c.ok("SELECT INBOX")
	// The envelope: the subject as it is, in a quoted string; the name
	// in the address as an encoded word.
	c.expect("FETCH 1 ENVELOPE",
		`* 1 FETCH (ENVELOPE (NIL "Grüße" (("=?utf-8?q?J=C3=BCrgen?=" NIL "j" "example.org")) (("=?utf-8?q?J=C3=BCrgen?=" NIL "j" "example.org")) `+
			`(("=?utf-8?q?J=C3=BCrgen?=" NIL "j" "example.org")) NIL NIL NIL NIL NIL))`, "OK FETCH completed")
	c.expect(`SEARCH SUBJECT "grüße"`, "* SEARCH 1", "OK SEARCH completed")
	// Without the extension the same subject goes out as a literal.
	o := e.selected("INBOX")
	if got := o.ok("FETCH 1 ENVELOPE"); !strings.Contains(got[0], "{7}\r\nGrüße") {
		t.Errorf("envelope without UTF8=ACCEPT: %q", got)
	}
}

// RFC 9051 (IMAP4rev2), after "ENABLE IMAP4rev2" (6.3.1): the changes of
// appendix E that a client sees.
//
//   - SELECT (6.3.2): no RECENT and no UNSEEN, a LIST response with the
//     mailbox name; selecting while a mailbox is selected sends
//     "* OK [CLOSED]" first. The example:
//
//     C: A142 SELECT INBOX
//     S: * 172 EXISTS
//     S: * OK [UIDVALIDITY 3857529045] UIDs valid
//     S: * OK [UIDNEXT 4392] Predicted next UID
//     S: * FLAGS (\Answered \Flagged \Deleted \Seen \Draft)
//     S: * OK [PERMANENTFLAGS (\Deleted \Seen \*)] Limited
//     S: * LIST () "/" INBOX
//     S: A142 OK [READ-WRITE] SELECT completed
//
//   - SEARCH (6.4.4, 7.3.4) answers with ESEARCH:
//
//     C: A282 SEARCH FLAGGED SINCE 1-Feb-1994 NOT FROM "Smith"
//     S: * ESEARCH (TAG "A282") ALL 2,84,882
//
//   - Mailbox names are UTF-8 (5.1); EXISTS comes without RECENT (7.4.1);
//     STATUS has SIZE and DELETED (6.3.11).
func TestRFC9051Rev2(t *testing.T) {
	e := newEnv(t)
	searchBox(e)
	c := e.login()
	c.ok("CREATE Entw&APw-rfe")
	c.expect("ENABLE IMAP4rev2", "* ENABLED IMAP4rev2", "OK ENABLE completed")
	c.expect("SELECT INBOX",
		`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft work)`,
		"* 5 EXISTS",
		`* OK [PERMANENTFLAGS (\Answered \Flagged \Deleted \Seen \Draft \*)] flags and keywords can be changed`,
		"* OK [UIDVALIDITY 3857529046] UIDs are valid",
		"* OK [UIDNEXT 6] the next UID",
		"* OK [HIGHESTMODSEQ 7] the highest modseq",
		`* LIST () "/" INBOX`,
		"OK [READ-WRITE] SELECT completed")
	c.expect(`SEARCH FLAGGED SINCE 1-Feb-1994 NOT FROM "Smith"`, `* ESEARCH (TAG "a005") ALL 2`, "OK SEARCH completed")
	c.expect(`UID SEARCH UNSEEN`, `* ESEARCH (TAG "a006") UID ALL 2:3,5`, "OK UID SEARCH completed")
	c.expect(`SEARCH TEXT "string not in mailbox"`, `* ESEARCH (TAG "a007")`, "OK SEARCH completed")
	e.st.add("INBOX", plain(6, "new"))
	c.expect("NOOP", "* 6 EXISTS", "OK NOOP completed")
	c.expect(`LIST "" Entw*`, `* LIST (\HasNoChildren) "/" "Entwürfe"`, "OK LIST completed")
	c.expect(`SELECT "Entwürfe"`,
		"* OK [CLOSED] the previous mailbox is closed",
		`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)`,
		"* 0 EXISTS",
		`* OK [PERMANENTFLAGS (\Answered \Flagged \Deleted \Seen \Draft \*)] flags and keywords can be changed`,
		"* OK [UIDVALIDITY 3857529051] UIDs are valid",
		"* OK [UIDNEXT 1] the next UID",
		"* OK [HIGHESTMODSEQ 8] the highest modseq",
		`* LIST () "/" "Entwürfe"`,
		"OK [READ-WRITE] SELECT completed")
	size := 0
	for _, m := range e.st.msgs {
		size += len(m.raw)
	}
	c.expect("STATUS INBOX (MESSAGES UNSEEN DELETED SIZE)",
		fmt.Sprintf("* STATUS INBOX (MESSAGES 6 UNSEEN 4 DELETED 1 SIZE %d)", size), "OK STATUS completed")
	// The commands IMAP4rev1 has and IMAP4rev2 dropped still work: a
	// client that enabled rev2 does not send them.
	c.expect(`LSUB "" INBOX`, `* LSUB () "/" INBOX`, "OK LSUB completed")
}

// RFC 3501 5.4: "If a server has an inactivity autologout timer, the
// duration of that timer MUST be at least 30 minutes." The default is 30
// minutes after login, also during IDLE; before login a client gets a
// minute, which RFC 9051 5.4 allows in words ("... before authentication
// ... MAY be shorter"). The server says BYE before it closes (7.1.5).
func TestAutologout(t *testing.T) {
	var s Server
	s.init()
	if s.IdleTimeout != 30*time.Minute || s.LoginTimeout != time.Minute {
		t.Fatalf("default timeouts: %v after login, %v before", s.IdleTimeout, s.LoginTimeout)
	}
	e := newEnv(t, func(s *Server) { s.LoginTimeout, s.IdleTimeout = 150*time.Millisecond, 400*time.Millisecond })
	c := e.dial()
	start := time.Now()
	wantLines(t, "before login", []string{c.line()}, "* BYE autologout: idle for too long")
	if d := time.Since(start); d < 100*time.Millisecond || !c.eof() {
		t.Errorf("logged out after %v, connection closed: %v", d, c.eof())
	}
	c = e.login()
	time.Sleep(250 * time.Millisecond) // longer than the login timeout
	c.ok("NOOP")
	start = time.Now()
	wantLines(t, "after login", []string{c.line()}, "* BYE autologout: idle for too long")
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("logged out after %v", d)
	}
	// During IDLE the same timer runs: a client re-issues IDLE in time
	// (RFC 2177: "at least every 29 minutes").
	c = e.selected("INBOX")
	c.write("a IDLE\r\n")
	c.line()
	wantLines(t, "in IDLE", []string{c.line()}, "* BYE autologout: idle for too long")
}

// Limits on connections: per address before login, per user after, and in
// all. A connection over the limit is told why.
func TestConnectionLimits(t *testing.T) {
	e := newEnv(t, func(s *Server) { s.MaxConnsPerIP, s.MaxConnsPerUser = 4, 2 })
	one, two := e.login(), e.login()
	three := e.dial()
	three.expect("LOGIN fred@example.test secret", "NO [UNAVAILABLE] too many connections for this user")
	one.expect("LOGOUT", "* BYE imap.example.test robinmail IMAP closing the connection", "OK LOGOUT completed")
	one.eof()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := three.status("LOGIN fred@example.test secret"); strings.HasPrefix(got, "OK") {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("login after another session left: %q", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = two
	// Two logged in, two more make four from this address; the fifth is
	// refused, in the clear so that it can be read without a handshake.
	e.dial()
	e.dial()
	c, greeting := e.dialPlain()
	if greeting != "* BYE too many connections from your address" || !c.eof() {
		t.Errorf("fifth connection: %q", greeting)
	}

	all := newEnv(t, func(s *Server) { s.MaxConnections = 1 })
	all.dial()
	if _, greeting := all.dialPlain(); greeting != "* BYE too many connections" {
		t.Errorf("second connection of one allowed: %q", greeting)
	}
}

// A client that keeps sending what is not IMAP is dropped; so is a line
// that does not end. RFC 3501 7.1.3: BAD "indicates a protocol-level error
// for which the associated command can not be determined" when untagged.
func TestBadInput(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	c.expect("FROBNICATE", "BAD unknown command")
	c.write("\r\n")
	wantLines(t, "an empty line", []string{c.line()}, "* BAD a command starts with a tag")
	c.write("+ NOOP\r\n")
	wantLines(t, "a tag with +", []string{c.line()}, "* BAD a command starts with a tag")
	c.write("tagonly\r\n")
	wantLines(t, "a tag alone", []string{c.line()}, "tagonly BAD unknown command")
	c.expect(`CREATE "unterminated`, "BAD a quoted string does not end")
	c.expect(`CREATE "bad \x escape"`, `BAD a backslash in a quoted string quotes only " and \`)
	c.ok("NOOP") // a good command resets the count
	for i := 0; i < maxBadCommands-1; i++ {
		c.expect("NONSENSE", "BAD unknown command")
	}
	c.expect("NONSENSE", "* BYE too many commands that are not IMAP", "BAD unknown command")
	if !c.eof() {
		t.Error("the connection stayed open")
	}

	c = e.dial()
	c.write("a1 NOOP " + strings.Repeat("x", maxLine+20000))
	wantLines(t, "a line of over 64 KiB", []string{c.line()}, "* BYE command line too long")
	if !c.eof() {
		t.Error("the connection stayed open")
	}
}

// Shutdown: every client is told BYE (RFC 3501 7.1.5), a session in IDLE
// too, and new connections are not taken.
func TestShutdown(t *testing.T) {
	e := newEnv(t)
	waiting := e.login()
	idle := e.selected("INBOX")
	idle.write("a IDLE\r\n")
	idle.line()
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- e.srv.Shutdown(ctx)
	}()
	wantLines(t, "a session waiting for a command", []string{waiting.line()}, "* BYE the server is shutting down")
	wantLines(t, "a session in IDLE", []string{idle.line()}, "* BYE the server is shutting down")
	if !waiting.eof() || !idle.eof() {
		t.Error("a connection stayed open")
	}
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := e.srv.Connections(); n != 0 {
		t.Errorf("%d connections left", n)
	}
}

// The mailbox a session has selected is deleted by another session: the
// first is told and its connection ends, since IMAP has no way to
// deselect a client.
func TestSelectedMailboxDeleted(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	c.ok("CREATE doomed")
	o := e.selected("doomed")
	c.expect("DELETE doomed", "OK DELETE completed")
	o.expect("NOOP", "* BYE the selected mailbox was deleted", "OK NOOP completed")
	if !o.eof() {
		t.Error("the connection stayed open")
	}
	// The session that deletes its own selected mailbox is deselected.
	c.ok("CREATE mine")
	c.ok("SELECT mine")
	c.expect("DELETE mine", "* OK [CLOSED] the selected mailbox was deleted", "OK DELETE completed")
	c.expect("FETCH 1 FLAGS", "BAD select a mailbox first")
}

// Mailboxes that cannot be named in IMAP are not shown: one with the
// delimiter in its own name, and one called INBOX that is not the inbox.
func TestUnnameableMailboxes(t *testing.T) {
	e := newEnv(t)
	e.st.mu.Lock()
	e.st.create(0, "a/b")
	e.st.create(0, "inbox")
	sub := e.st.create(e.st.create(0, "ok"), "child")
	e.st.create(sub, "with/slash")
	e.st.mu.Unlock()
	c := e.login()
	c.expect(`LIST "" *`,
		`* LIST (\HasNoChildren \Drafts) "/" Drafts`,
		`* LIST (\HasNoChildren) "/" INBOX`,
		`* LIST (\HasNoChildren \Junk) "/" Junk`,
		`* LIST (\HasNoChildren \Sent) "/" Sent`,
		`* LIST (\HasNoChildren \Trash) "/" Trash`,
		`* LIST (\HasChildren) "/" ok`,
		`* LIST (\HasNoChildren) "/" ok/child`,
		"OK LIST completed")
}
