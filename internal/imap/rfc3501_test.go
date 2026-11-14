package imap

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Tests written from RFC 3501, section by section, with its examples. Where
// the server says more than the example (it has more capabilities and
// response codes than the RFC's 2003 server) the test says what and why.

// The message of the sample session in RFC 3501 section 8. Its header is
// the 342 octets the RFC shows; the body is made to be the 3028 octets in
// 92 lines that the example's BODY says.
var (
	sampleHeader = "Date: Wed, 17 Jul 1996 02:23:25 -0700 (PDT)\r\n" +
		"From: Terry Gray <gray@cac.washington.edu>\r\n" +
		"Subject: IMAP4rev1 WG mtg summary and minutes\r\n" +
		"To: imap@cac.washington.edu\r\n" +
		"cc: minutes@CNRI.Reston.VA.US, John Klensin <KLENSIN@MIT.EDU>\r\n" +
		"Message-Id: <B27397-0100000@cac.washington.edu>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: TEXT/PLAIN; CHARSET=US-ASCII\r\n" +
		"\r\n"
	sampleBody    = strings.Repeat(strings.Repeat("x", 31)+"\r\n", 91) + strings.Repeat("y", 23) + "\r\n"
	sampleMessage = sampleHeader + sampleBody
)

// plain is a small message for filling mailboxes.
func plain(n int, subject string) string {
	return fmt.Sprintf("Date: Mon, 7 Feb 1994 21:52:25 -0800\r\nFrom: Fred Foobar <foobar@Blurdybloop.COM>\r\n"+
		"Subject: %s\r\nTo: mooch@owatagu.siam.edu\r\nMessage-Id: <B27397-%07d@Blurdybloop.COM>\r\n\r\nMessage %d.\r\n", subject, n, n)
}

// fill puts n small messages in a mailbox.
func (e *env) fill(box string, n int) {
	for i := 1; i <= n; i++ {
		e.st.add(box, plain(i, fmt.Sprintf("message %d", i)))
	}
}

// RFC 3501 section 8, "Sample IMAP4rev1 connection", line for line. The
// differences: our greeting and LOGIN answer carry a capability list; the
// mailbox has no \Recent message (the flag is not kept), so RECENT is 0;
// SELECT also says PERMANENTFLAGS, UIDNEXT and HIGHESTMODSEQ; RFC822.SIZE
// is the real size, 3370, where the RFC's example says 4286 next to a
// header of 342 and a body of 3028; and flags come in a fixed order.
func TestRFC3501SampleSession(t *testing.T) {
	if len(sampleHeader) != 342 || len(sampleBody) != 3028 {
		t.Fatalf("the sample message has a header of %d and a body of %d octets", len(sampleHeader), len(sampleBody))
	}
	e := newEnv(t)
	for i := 1; i <= 18; i++ {
		switch {
		case i == 12:
			e.st.add("INBOX", sampleMessage, "$seen")
		case i < 17:
			e.st.add("INBOX", plain(i, "old"), "$seen")
		default:
			e.st.add("INBOX", plain(i, "new"))
		}
	}
	c := e.dial()
	c.expect("login fred@example.test secret",
		"OK [CAPABILITY IMAP4rev1 IMAP4rev2 LITERAL+ SASL-IR ID ENABLE IDLE NAMESPACE UNSELECT UIDPLUS MOVE SPECIAL-USE CHILDREN "+
			"LIST-EXTENDED LIST-STATUS CONDSTORE ESEARCH SEARCHRES BINARY STATUS=SIZE UTF8=ACCEPT] logged in")
	c.expect("select inbox",
		`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)`,
		"* 18 EXISTS",
		"* 0 RECENT",
		"* OK [UNSEEN 17] the first message that is not seen",
		`* OK [PERMANENTFLAGS (\Answered \Flagged \Deleted \Seen \Draft \*)] flags and keywords can be changed`,
		"* OK [UIDVALIDITY 3857529046] UIDs are valid",
		"* OK [UIDNEXT 19] the next UID",
		"* OK [HIGHESTMODSEQ 19] the highest modseq",
		"OK [READ-WRITE] SELECT completed")
	c.expect("fetch 12 full",
		`* 12 FETCH (FLAGS (\Seen) INTERNALDATE "17-Jul-1996 02:44:25 -0700" RFC822.SIZE 3370 `+
			`ENVELOPE ("Wed, 17 Jul 1996 02:23:25 -0700 (PDT)" "IMAP4rev1 WG mtg summary and minutes" `+
			`(("Terry Gray" NIL "gray" "cac.washington.edu")) `+
			`(("Terry Gray" NIL "gray" "cac.washington.edu")) `+
			`(("Terry Gray" NIL "gray" "cac.washington.edu")) `+
			`((NIL NIL "imap" "cac.washington.edu")) `+
			`((NIL NIL "minutes" "CNRI.Reston.VA.US")("John Klensin" NIL "KLENSIN" "MIT.EDU")) NIL NIL `+
			`"<B27397-0100000@cac.washington.edu>") `+
			`BODY ("TEXT" "PLAIN" ("CHARSET" "US-ASCII") NIL NIL "7BIT" 3028 92))`,
		"OK FETCH completed")
	// BODY[HEADER] is not .PEEK, but the message is seen already: no
	// FLAGS follow.
	c.expect("fetch 12 body[header]",
		"* 12 FETCH (BODY[HEADER] {342}\r\n"+sampleHeader+")",
		"OK FETCH completed")
	c.expect(`store 12 +flags \deleted`,
		`* 12 FETCH (FLAGS (\Deleted \Seen))`,
		"OK STORE completed")
	c.expect("logout",
		"* BYE imap.example.test robinmail IMAP closing the connection",
		"OK LOGOUT completed")
	if !c.eof() {
		t.Error("the connection stayed open after LOGOUT")
	}
}

// RFC 3501 7.1.1 and 6.1.1: the greeting is an untagged OK, and may carry
// the capabilities; CAPABILITY lists them and "IMAP4rev1" is among them.
// 6.2.1 and 6.2.3: without TLS, STARTTLS and LOGINDISABLED are announced
// and no AUTH= mechanism; after STARTTLS the list is another one, and the
// client asks again (the example of 6.2.1).
func TestRFC3501CapabilityAndStartTLS(t *testing.T) {
	e := newEnv(t)
	c, greeting := e.dialPlain()
	if greeting != "* OK [CAPABILITY IMAP4rev1 IMAP4rev2 LITERAL+ SASL-IR ID ENABLE STARTTLS LOGINDISABLED] imap.example.test robinmail IMAP ready" {
		t.Errorf("greeting without TLS: %q", greeting)
	}
	c.expect("CAPABILITY",
		"* CAPABILITY IMAP4rev1 IMAP4rev2 LITERAL+ SASL-IR ID ENABLE STARTTLS LOGINDISABLED",
		"OK CAPABILITY completed")
	// 6.2.3: "a client MUST NOT issue a LOGIN command if the LOGINDISABLED
	// capability is advertised". One that does is refused, with the right
	// password too, and so is AUTHENTICATE.
	c.expect("LOGIN fred@example.test secret",
		"NO [PRIVACYREQUIRED] LOGIN is disabled without TLS; use STARTTLS first")
	c.expect("AUTHENTICATE PLAIN AGZyZWRAZXhhbXBsZS50ZXN0AHNlY3JldA==",
		"NO [PRIVACYREQUIRED] AUTHENTICATE is disabled without TLS; use STARTTLS first")
	if len(e.st.logins) != 0 {
		t.Errorf("a login without TLS reached the backend: %v", e.st.logins)
	}
	c.expect("SELECT INBOX", "BAD log in first")

	c.expect("STARTTLS", "OK begin TLS negotiation now")
	tc := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	c.conn, c.br = tc, bufio.NewReader(tc)
	// "the client MUST discard cached information about server
	// capabilities and SHOULD re-issue the CAPABILITY command".
	c.expect("CAPABILITY",
		"* CAPABILITY IMAP4rev1 IMAP4rev2 LITERAL+ SASL-IR ID ENABLE AUTH=PLAIN",
		"OK CAPABILITY completed")
	c.expect("STARTTLS", "BAD TLS is already on")
	c.ok("LOGIN fred@example.test secret")
	c.expect("STARTTLS", "BAD already logged in")

	// Under implicit TLS the greeting has AUTH=PLAIN at once.
	s := e.dial()
	s.expect("CAPABILITY",
		"* CAPABILITY IMAP4rev1 IMAP4rev2 LITERAL+ SASL-IR ID ENABLE AUTH=PLAIN",
		"OK CAPABILITY completed")
	s.expect("STARTTLS", "BAD TLS is already on")
}

// RFC 3501 6.2.1: "the server ... MUST discard any knowledge obtained from
// the client" before TLS. A command sent in the same packet as STARTTLS
// was not protected and must not run after the handshake.
func TestStartTLSDoesNotRunPipelinedCommands(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dialPlain()
	c.write("a1 STARTTLS\r\na2 NOOP\r\n")
	if got := c.line(); got != "a1 BAD nothing may follow STARTTLS before the handshake" {
		t.Errorf("got %q", got)
	}
	if !c.eof() {
		t.Error("the connection stayed open")
	}
}

// RFC 3501 6.1.3, the example:
//
//	C: A023 LOGOUT
//	S: * BYE IMAP4rev1 Server logging out
//	S: A023 OK LOGOUT completed
//
// and 3.4: LOGOUT is valid in every state.
func TestRFC3501Logout(t *testing.T) {
	e := newEnv(t)
	for _, c := range []*client{e.dial(), e.login(), e.selected("INBOX")} {
		c.expect("LOGOUT", "* BYE imap.example.test robinmail IMAP closing the connection", "OK LOGOUT completed")
		if !c.eof() {
			t.Error("the connection stayed open")
		}
	}
}

// RFC 3501 6.2.3, the example "a001 LOGIN SMITH SESAME", and 6.2.2 with the
// PLAIN mechanism of RFC 4616. The password and name may be any astring:
// atom, quoted, literal.
func TestRFC3501Login(t *testing.T) {
	e := newEnv(t)
	c := e.dial()
	c.expect("LOGIN fred@example.test sesame", "NO [AUTHENTICATIONFAILED] wrong name or password")
	c.expect(`LOGIN "fred@example.test" "secret"`,
		"OK [CAPABILITY IMAP4rev1 IMAP4rev2 LITERAL+ SASL-IR ID ENABLE IDLE NAMESPACE UNSELECT UIDPLUS MOVE SPECIAL-USE CHILDREN "+
			"LIST-EXTENDED LIST-STATUS CONDSTORE ESEARCH SEARCHRES BINARY STATUS=SIZE UTF8=ACCEPT] logged in")
	c.expect("LOGIN fred@example.test secret", "BAD already logged in")

	// A synchronizing literal: the server asks for it (4.3).
	c = e.dial()
	c.write("a1 LOGIN {17}\r\n")
	if got := c.line(); got != "+ OK" {
		t.Fatalf("continuation: %q", got)
	}
	c.write("fred@example.test {6}\r\n")
	if got := c.line(); got != "+ OK" {
		t.Fatalf("continuation: %q", got)
	}
	c.write("secret\r\n")
	if got := c.line(); !strings.HasPrefix(got, "a1 OK ") {
		t.Fatalf("login with literals: %q", got)
	}

	// The backend is told the client's address, for its rate limits.
	if len(e.st.logins) == 0 || !strings.HasPrefix(e.st.logins[0], "127.0.0.1 ") {
		t.Errorf("logins seen by the backend: %v", e.st.logins)
	}

	// Syntax.
	c = e.dial()
	c.expect("LOGIN fred@example.test", "BAD a space is missing")
	c.expect("LOGIN", "BAD a space is missing")
	c.expect("LOGIN a b c", `BAD unexpected text after the command: " c"`)
}

// Three failed logins end the connection; the backend's "too many" and
// "cannot tell" are not answered as a wrong password, so a mail client
// does not ask its user for a new one.
func TestLoginFailures(t *testing.T) {
	e := newEnv(t)
	c := e.dial()
	c.expect("LOGIN fred@example.test one", "NO [AUTHENTICATIONFAILED] wrong name or password")
	c.expect("LOGIN fred@example.test two", "NO [AUTHENTICATIONFAILED] wrong name or password")
	c.expect("LOGIN fred@example.test three", "* BYE too many failed logins", "NO [AUTHENTICATIONFAILED] wrong name or password")
	if !c.eof() {
		t.Error("the connection stayed open after three failed logins")
	}

	e.st.limited = true
	c = e.dial()
	c.expect("LOGIN fred@example.test secret", "NO [UNAVAILABLE] too many failed logins; try again later")
	e.st.limited, e.st.down = false, true
	c.expect("LOGIN fred@example.test secret", "NO [UNAVAILABLE] the server cannot do that right now, try again later")
	e.st.down = false
	c.ok("LOGIN fred@example.test secret")
}

// RFC 3501 6.1.2, the example: NOOP is how a client polls. Here another
// session expunges message 22, three messages arrive and message 14 is
// marked:
//
//	C: a047 NOOP
//	S: * 22 EXPUNGE
//	S: * 23 EXISTS
//	S: * 3 RECENT
//	S: * 14 FETCH (FLAGS (\Seen \Deleted))
//	S: a047 OK NOOP completed
//
// The count after the EXPUNGE is 22 + 3 = 25 here: the RFC's "23 EXISTS"
// belongs to a mailbox of other size. FETCH comes first with us, while
// the numbers are still the ones before the EXPUNGE; RECENT is always 0.
func TestRFC3501Noop(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 23)
	c := e.selected("INBOX")
	c.expect("NOOP", "OK NOOP completed")

	e.st.remove("INBOX", 22)
	e.fill("INBOX", 3)
	e.st.setFlag("INBOX", 14, "$seen", true)
	e.st.setFlag("INBOX", 14, "$deleted", true)
	c.expect("NOOP",
		`* 14 FETCH (FLAGS (\Deleted \Seen))`,
		"* 22 EXPUNGE",
		"* 25 EXISTS",
		"* 0 RECENT",
		"OK NOOP completed")
	c.expect("NOOP", "OK NOOP completed")
}

// RFC 3501 6.3.1, the example, and 7.1 for the response codes:
//
//	C: A142 SELECT INBOX
//	S: * 172 EXISTS
//	S: * 1 RECENT
//	S: * OK [UNSEEN 12] Message 12 is first unseen
//	S: * OK [UIDVALIDITY 3857529045] UIDs valid
//	S: * OK [UIDNEXT 4392] Predicted next UID
//	S: * FLAGS (\Answered \Flagged \Deleted \Seen \Draft)
//	S: * OK [PERMANENTFLAGS (\Deleted \Seen \*)] Limited
//	S: A142 OK [READ-WRITE] SELECT completed
//
// and 6.3.2, EXAMINE: the same, read-only, "[PERMANENTFLAGS ()] No
// permanent flags permitted".
func TestRFC3501SelectExamine(t *testing.T) {
	e := newEnv(t)
	for i := 1; i <= 172; i++ {
		if i < 12 {
			e.st.add("INBOX", plain(i, "seen"), "$seen")
		} else {
			e.st.add("INBOX", plain(i, "unseen"))
		}
	}
	c := e.login()
	c.expect("SELECT INBOX",
		`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)`,
		"* 172 EXISTS",
		"* 0 RECENT",
		"* OK [UNSEEN 12] the first message that is not seen",
		`* OK [PERMANENTFLAGS (\Answered \Flagged \Deleted \Seen \Draft \*)] flags and keywords can be changed`,
		"* OK [UIDVALIDITY 3857529046] UIDs are valid",
		"* OK [UIDNEXT 173] the next UID",
		"* OK [HIGHESTMODSEQ 173] the highest modseq",
		"OK [READ-WRITE] SELECT completed")
	c.expect("EXAMINE inbox",
		`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)`,
		"* 172 EXISTS",
		"* 0 RECENT",
		"* OK [UNSEEN 12] the first message that is not seen",
		"* OK [PERMANENTFLAGS ()] no flag can be changed",
		"* OK [UIDVALIDITY 3857529046] UIDs are valid",
		"* OK [UIDNEXT 173] the next UID",
		"* OK [HIGHESTMODSEQ 173] the highest modseq",
		"OK [READ-ONLY] EXAMINE completed")
	// 6.3.2: nothing can be changed in a mailbox opened with EXAMINE, and
	// a fetch that would set \Seen does not.
	c.expect(`STORE 12 +FLAGS (\Seen)`, "NO [READ-ONLY] the mailbox is open read-only")
	c.expect("EXPUNGE", "NO [READ-ONLY] the mailbox is open read-only")
	c.ok("FETCH 12 BODY[TEXT]")
	c.expect("FETCH 12 FLAGS", "* 12 FETCH (FLAGS ())", "OK FETCH completed")

	// 6.3.1: "if the client attempts to select a mailbox that does not
	// exist ... the server is in the authenticated state": the mailbox
	// that was selected is not selected any more.
	c.expect("SELECT nothere", "NO [NONEXISTENT] no such mailbox")
	c.expect("FETCH 1 FLAGS", "BAD select a mailbox first")

	// A mailbox with keywords in use names them in FLAGS (7.2.6), and one
	// in which everything is seen has no UNSEEN.
	e.st.add("Sent", plain(1, "sent"), "$seen", "$forwarded", "work")
	c.expect("SELECT Sent",
		`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft $Forwarded work)`,
		"* 1 EXISTS",
		"* 0 RECENT",
		`* OK [PERMANENTFLAGS (\Answered \Flagged \Deleted \Seen \Draft \*)] flags and keywords can be changed`,
		"* OK [UIDVALIDITY 3857529048] UIDs are valid",
		"* OK [UIDNEXT 2] the next UID",
		"* OK [HIGHESTMODSEQ 174] the highest modseq",
		"OK [READ-WRITE] SELECT completed")
}

// RFC 3501 6.3.3, the example:
//
//	C: A003 CREATE owatagusiam/
//	S: A003 OK CREATE completed
//	C: A004 CREATE owatagusiam/blurdybloop
//	S: A004 OK CREATE completed
//
// "It is an error to attempt to create INBOX or a mailbox with a name that
// refers to an extant mailbox." "If the server's hierarchy separator
// character appears elsewhere in the name, the server SHOULD create any
// superior hierarchical names that are needed".
func TestRFC3501Create(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	c.expect("CREATE owatagusiam/", "OK CREATE completed")
	c.expect("CREATE owatagusiam/blurdybloop", "OK CREATE completed")
	c.expect("CREATE owatagusiam/blurdybloop", "NO [ALREADYEXISTS] a mailbox with that name exists")
	c.expect("CREATE INBOX", "NO [ALREADYEXISTS] INBOX exists")
	c.expect("CREATE inbox", "NO [ALREADYEXISTS] INBOX exists")
	c.expect("CREATE a/b/c", "OK CREATE completed")
	c.expect("CREATE Inbox/sub", "OK CREATE completed")
	c.expect(`LIST "" *`,
		`* LIST (\HasNoChildren \Drafts) "/" Drafts`,
		`* LIST (\HasChildren) "/" INBOX`,
		`* LIST (\HasNoChildren) "/" INBOX/sub`,
		`* LIST (\HasNoChildren \Junk) "/" Junk`,
		`* LIST (\HasNoChildren \Sent) "/" Sent`,
		`* LIST (\HasNoChildren \Trash) "/" Trash`,
		`* LIST (\HasChildren) "/" a`,
		`* LIST (\HasChildren) "/" a/b`,
		`* LIST (\HasNoChildren) "/" a/b/c`,
		`* LIST (\HasChildren) "/" owatagusiam`,
		`* LIST (\HasNoChildren) "/" owatagusiam/blurdybloop`,
		"OK LIST completed")
	c.expect("CREATE a//b", "NO [CANNOT] not a name a mailbox can have")
	c.expect("CREATE "+strings.Repeat("x/", 12)+"y", "NO [CANNOT] not a name a mailbox can have")
	c.expect("CREATE", "BAD a space is missing")
}

// RFC 3501 6.3.4, the first example:
//
//	C: A682 LIST "" *
//	S: * LIST () "/" blurdybloop
//	S: * LIST (\Noselect) "/" foo
//	S: * LIST () "/" foo/bar
//	S: A682 OK LIST completed
//	C: A683 DELETE blurdybloop
//	S: A683 OK DELETE completed
//	C: A684 DELETE foo
//	S: A684 NO Name "foo" has inferior hierarchical names
//	C: A685 DELETE foo/bar
//	S: A685 OK DELETE Completed
//	C: A686 LIST "" *
//	S: * LIST (\Noselect) "/" foo
//	S: A686 OK LIST completed
//	C: A687 DELETE foo
//	S: A687 OK DELETE Completed
//
// Our "foo" is an ordinary mailbox, not a \Noselect name: there are none
// here. Deleting it while it has children is refused as in the example. A
// deviation, kept on purpose: for a mailbox that is selectable and has
// children the RFC would remove its messages and leave the name behind as
// \Noselect; we refuse instead. "It is an error to attempt to delete
// INBOX"; the mailboxes with a role are refused too, the server needs them.
func TestRFC3501Delete(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	c.ok("CREATE blurdybloop")
	c.ok("CREATE foo/bar")
	c.expect(`LIST "" *`,
		`* LIST (\HasNoChildren \Drafts) "/" Drafts`,
		`* LIST (\HasNoChildren) "/" INBOX`,
		`* LIST (\HasNoChildren \Junk) "/" Junk`,
		`* LIST (\HasNoChildren \Sent) "/" Sent`,
		`* LIST (\HasNoChildren \Trash) "/" Trash`,
		`* LIST (\HasNoChildren) "/" blurdybloop`,
		`* LIST (\HasChildren) "/" foo`,
		`* LIST (\HasNoChildren) "/" foo/bar`,
		"OK LIST completed")
	c.expect("DELETE blurdybloop", "OK DELETE completed")
	c.expect("DELETE foo", "NO the mailbox has child mailboxes; delete those first")
	c.expect("DELETE foo/bar", "OK DELETE completed")
	c.expect(`LIST "" foo*`, `* LIST (\HasNoChildren) "/" foo`, "OK LIST completed")
	c.expect("DELETE foo", "OK DELETE completed")
	c.expect("DELETE foo", "NO [NONEXISTENT] no such mailbox")
	c.expect("DELETE INBOX", "NO [NOPERM] INBOX cannot be deleted")
	c.expect("DELETE Trash", "NO [NOPERM] this mailbox cannot be deleted")
}

// RFC 3501 6.3.5, the examples:
//
//	C: A683 RENAME blurdybloop sarasoop
//	S: A683 OK RENAME completed
//	C: A684 RENAME foo zowie
//	S: A684 OK RENAME Completed
//	   (foo/bar is now zowie/bar)
//
// and for INBOX: "Renaming INBOX is permitted, and has special behavior.
// It moves all messages in INBOX to a new mailbox with the given name,
// leaving INBOX empty. If the server implementation supports inferior
// hierarchical names of INBOX, these are unaffected":
//
//	C: Z433 RENAME INBOX old-mail
//	S: Z433 OK RENAME completed
//	C: Z434 LIST "" *
//	S: * LIST () "." INBOX
//	S: * LIST () "." INBOX.bar
//	S: * LIST () "." old-mail
func TestRFC3501Rename(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 3)
	c := e.login()
	c.ok("CREATE blurdybloop")
	c.ok("CREATE foo/bar")
	c.ok("CREATE INBOX/bar")
	e.st.add("blurdybloop", plain(1, "kept"))
	before := c.ok("STATUS blurdybloop (UIDVALIDITY UIDNEXT MESSAGES)")
	c.expect("RENAME blurdybloop sarasoop", "OK RENAME completed")
	// "the UIDs ... MUST NOT change": the mailbox is the same one.
	after := c.ok("STATUS sarasoop (UIDVALIDITY UIDNEXT MESSAGES)")
	if strings.Replace(before[0], "blurdybloop", "sarasoop", 1) != after[0] {
		t.Errorf("after a rename: %q, before: %q", after[0], before[0])
	}
	c.expect("RENAME foo zowie", "OK RENAME completed")
	c.expect(`LIST "" zowie*`, `* LIST (\HasChildren) "/" zowie`, `* LIST (\HasNoChildren) "/" zowie/bar`, "OK LIST completed")
	// Into another parent, which is made if it is not there.
	c.expect("RENAME zowie/bar deep/down/bar", "OK RENAME completed")
	c.expect(`LIST "" deep*`, `* LIST (\HasChildren) "/" deep`, `* LIST (\HasChildren) "/" deep/down`,
		`* LIST (\HasNoChildren) "/" deep/down/bar`, "OK RENAME completed"[:0]+"OK LIST completed")

	c.expect("RENAME sarasoop zowie", "NO [ALREADYEXISTS] a mailbox with that name exists")
	c.expect("RENAME nothere other", "NO [NONEXISTENT] no such mailbox")
	c.expect("RENAME deep deep/down/deeper", "NO [CANNOT] a mailbox cannot be moved under itself")

	c.expect("RENAME INBOX old-mail", "OK RENAME completed: the messages of INBOX moved")
	c.expect("STATUS INBOX (MESSAGES)", "* STATUS INBOX (MESSAGES 0)", "OK STATUS completed")
	c.expect("STATUS old-mail (MESSAGES)", "* STATUS old-mail (MESSAGES 3)", "OK STATUS completed")
	c.expect(`LIST "" INBOX*`, `* LIST (\HasChildren) "/" INBOX`, `* LIST (\HasNoChildren) "/" INBOX/bar`, "OK LIST completed")
}

// RFC 3501 6.3.6, 6.3.7 and 6.3.9: SUBSCRIBE, UNSUBSCRIBE and LSUB. The
// example of 6.3.9:
//
//	C: A002 LSUB "#news." "comp.mail.*"
//	S: * LSUB () "." #news.comp.mail.mime
//	S: * LSUB () "." #news.comp.mail.misc
//	C: A003 LSUB "#news." "comp.%"
//	S: * LSUB (\NoSelect) "." #news.comp.mail
//
// "the server MUST NOT unilaterally remove an existing mailbox name from
// the subscription list" has nothing to guard here: a subscription is a
// property of a mailbox and goes with it.
func TestRFC3501Subscribe(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	c.ok("CREATE news/comp/mail/mime")
	c.ok("CREATE news/comp/mail/misc")
	// What CREATE makes is not subscribed; the mailboxes an account
	// comes with are.
	for _, name := range []string{"INBOX", "Drafts", "Sent", "Junk", "Trash"} {
		c.expect("UNSUBSCRIBE "+name, "OK UNSUBSCRIBE completed")
	}
	c.expect(`LSUB "" *`, "OK LSUB completed")
	c.expect("SUBSCRIBE news/comp/mail/mime", "OK SUBSCRIBE completed")
	c.expect("SUBSCRIBE news/comp/mail/misc", "OK SUBSCRIBE completed")
	c.expect(`LSUB "news/" "comp/mail/*"`,
		`* LSUB () "/" news/comp/mail/mime`,
		`* LSUB () "/" news/comp/mail/misc`,
		"OK LSUB completed")
	c.expect(`LSUB "news/" "comp/%"`, `* LSUB (\Noselect) "/" news/comp/mail`, "OK LSUB completed")
	c.expect("UNSUBSCRIBE news/comp/mail/misc", "OK UNSUBSCRIBE completed")
	c.expect("SUBSCRIBE INBOX", "OK SUBSCRIBE completed")
	c.expect(`LSUB "" *`, `* LSUB () "/" INBOX`, `* LSUB () "/" news/comp/mail/mime`, "OK LSUB completed")
	c.expect("SUBSCRIBE nothere", "NO [NONEXISTENT] no such mailbox")
	c.expect("UNSUBSCRIBE nothere", "OK UNSUBSCRIBE completed")
}

// RFC 3501 6.3.8, the examples:
//
//	C: A101 LIST "" ""
//	S: * LIST (\Noselect) "/" ""
//	C: A202 LIST ~/Mail/ %
//	S: * LIST (\Noselect) "/" ~/Mail/foo
//	S: * LIST () "/" ~/Mail/meetings
//
// "*" matches zero or more characters, "%" the same but not the hierarchy
// delimiter; "INBOX" is matched case-insensitively.
func TestRFC3501List(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	c.expect(`LIST "" ""`, `* LIST (\Noselect) "/" ""`, "OK LIST completed")
	c.ok("CREATE Mail/foo/bar")
	c.ok("CREATE Mail/meetings")
	c.expect("LIST Mail/ %",
		`* LIST (\HasChildren) "/" Mail/foo`,
		`* LIST (\HasNoChildren) "/" Mail/meetings`,
		"OK LIST completed")
	c.expect(`LIST "" %`,
		`* LIST (\HasNoChildren \Drafts) "/" Drafts`,
		`* LIST (\HasNoChildren) "/" INBOX`,
		`* LIST (\HasNoChildren \Junk) "/" Junk`,
		`* LIST (\HasChildren) "/" Mail`,
		`* LIST (\HasNoChildren \Sent) "/" Sent`,
		`* LIST (\HasNoChildren \Trash) "/" Trash`,
		"OK LIST completed")
	c.expect(`LIST "" Mail/%/%`, `* LIST (\HasNoChildren) "/" Mail/foo/bar`, "OK LIST completed")
	c.expect(`LIST "" *bar`, `* LIST (\HasNoChildren) "/" Mail/foo/bar`, "OK LIST completed")
	c.expect(`LIST "" M%l/m*s`, `* LIST (\HasNoChildren) "/" Mail/meetings`, "OK LIST completed")
	c.expect(`LIST "" inbox`, `* LIST (\HasNoChildren) "/" INBOX`, "OK LIST completed")
	c.expect(`LIST "" nothing*`, "OK LIST completed")
	c.expect(`LIST ""`, "BAD a space is missing")
}

// matchPattern, on its own: the wildcards of RFC 3501 6.3.8.
func TestListPattern(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*", "a/b/c", true}, {"%", "a", true}, {"%", "a/b", false}, {"a/%", "a/b", true}, {"a/%", "a/b/c", false},
		{"a/*", "a/b/c", true}, {"a/*", "a", false}, {"a*", "a", true}, {"%/%", "a/b", true}, {"%/%", "a", false},
		{"*c", "a/b/c", true}, {"*/c", "a/b/c", true}, {"%c", "a/b/c", false}, {"a%c", "abc", true}, {"a%c", "a/c", false},
		{"", "", true}, {"", "a", false}, {"a", "", false}, {"**", "x", true}, {"%*%", "a/b", true},
		{"*%*%*%*%*%*%*%*%*%*%*%*%*%*%*%*%b", strings.Repeat("a", 200), false},
	} {
		if got := matchPattern(tc.pattern, tc.name); got != tc.want {
			t.Errorf("match(%q, %q) = %v", tc.pattern, tc.name, got)
		}
	}
}

// RFC 3501 6.3.10, the example:
//
//	C: A042 STATUS blurdybloop (UIDNEXT MESSAGES)
//	S: * STATUS blurdybloop (MESSAGES 231 UIDNEXT 44292)
//	S: A042 OK STATUS completed
//
// The items come back in the order asked, which the RFC allows either way.
func TestRFC3501Status(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	c.ok("CREATE blurdybloop")
	for i := 1; i <= 231; i++ {
		if i%3 == 0 {
			e.st.add("blurdybloop", plain(i, "s"), "$seen")
		} else {
			e.st.add("blurdybloop", plain(i, "s"))
		}
	}
	c.expect("STATUS blurdybloop (UIDNEXT MESSAGES)", "* STATUS blurdybloop (UIDNEXT 232 MESSAGES 231)", "OK STATUS completed")
	c.expect("STATUS blurdybloop (MESSAGES RECENT UIDNEXT UIDVALIDITY UNSEEN)",
		"* STATUS blurdybloop (MESSAGES 231 RECENT 0 UIDNEXT 232 UIDVALIDITY 3857529051 UNSEEN 154)", "OK STATUS completed")
	c.expect("STATUS nothere (MESSAGES)", "NO [NONEXISTENT] no such mailbox")
	c.expect("STATUS blurdybloop (COLOUR)", "BAD unknown STATUS item COLOUR")
	c.expect("STATUS blurdybloop ()", "BAD STATUS needs at least one item")
	// STATUS of the selected mailbox is allowed and shows what is there.
	c.ok("SELECT blurdybloop")
	c.expect("STATUS blurdybloop (MESSAGES)", "* STATUS blurdybloop (MESSAGES 231)", "OK STATUS completed")
}

// RFC 3501 6.3.11, the example:
//
//	C: A003 APPEND saved-messages (\Seen) {310}
//	S: + Ready for literal data
//	C: Date: Mon, 7 Feb 1994 21:52:25 -0800 (PST)
//	C: From: Fred Foobar <foobar@Blurdybloop.COM>
//	C: Subject: afternoon meeting
//	C: To: mooch@owatagu.siam.edu
//	C: Message-Id: <B27397-0100000@Blurdybloop.COM>
//	C: MIME-Version: 1.0
//	C: Content-Type: TEXT/PLAIN; CHARSET=US-ASCII
//	C:
//	C: Hello Joe, do you think we can meet at 3:30 tomorrow?
//	C:
//	S: A003 OK APPEND completed
//
// The message of the example is 310 octets, as it says.
func TestRFC3501Append(t *testing.T) {
	msg := "Date: Mon, 7 Feb 1994 21:52:25 -0800 (PST)\r\n" +
		"From: Fred Foobar <foobar@Blurdybloop.COM>\r\n" +
		"Subject: afternoon meeting\r\n" +
		"To: mooch@owatagu.siam.edu\r\n" +
		"Message-Id: <B27397-0100000@Blurdybloop.COM>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: TEXT/PLAIN; CHARSET=US-ASCII\r\n" +
		"\r\n" +
		"Hello Joe, do you think we can meet at 3:30 tomorrow?\r\n"
	// The empty line that ends the example is the end of the command.
	if len(msg) != 310 {
		t.Fatalf("the example message is %d octets", len(msg))
	}
	e := newEnv(t)
	c := e.login()
	c.ok("CREATE saved-messages")
	c.write(`A003 APPEND saved-messages (\Seen) {310}` + "\r\n")
	if got := c.line(); got != "+ OK" {
		t.Fatalf("continuation: %q", got)
	}
	c.write(msg + "\r\n")
	// With UIDPLUS the answer names the UID (RFC 4315 3).
	if got := c.line(); got != "A003 OK [APPENDUID 3857529051 1] APPEND completed" {
		t.Fatalf("APPEND: %q", got)
	}
	c.ok("SELECT saved-messages")
	c.expect("FETCH 1 (FLAGS RFC822.SIZE BODY.PEEK[])",
		"* 1 FETCH (FLAGS (\\Seen) RFC822.SIZE 310 BODY[] {310}\r\n"+msg+")", "OK FETCH completed")

	// With a date-time: it becomes the internal date. And the session
	// that has the mailbox selected hears of its own APPEND.
	c.write(`a1 APPEND saved-messages (\Flagged $Forwarded) " 7-Feb-1994 22:43:04 -0800" {310+}` + "\r\n" + msg + "\r\n")
	wantLines(t, "APPEND to the selected mailbox", c.until("a1"),
		`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft $Forwarded)`,
		"* 2 EXISTS", "* 0 RECENT", "OK [APPENDUID 3857529051 2] APPEND completed")
	c.expect("FETCH 2 (FLAGS INTERNALDATE)",
		`* 2 FETCH (FLAGS (\Flagged $Forwarded) INTERNALDATE "07-Feb-1994 22:43:04 -0800")`, "OK FETCH completed")

	// "If the destination mailbox does not exist, a server MUST return an
	// error, and MUST NOT automatically create the mailbox. Unless it is
	// certain that the destination mailbox can not be created, the server
	// MUST send the response code [TRYCREATE]".
	c.write("a2 APPEND nothere {310+}\r\n" + msg + "\r\n")
	wantLines(t, "APPEND to a mailbox that is not there", c.until("a2"), "NO [TRYCREATE] no such mailbox")

	c.expect("APPEND saved-messages", "BAD a space is missing")
	c.expect("APPEND saved-messages notaliteral", "BAD the message is missing: APPEND takes it as a literal")
	c.write("a3 APPEND saved-messages (\\Seen) \"yesterday\" {3+}\r\nabc\r\n")
	wantLines(t, "a bad date", c.until("a3"), `BAD not a date-time: "yesterday"`)
	c.write("a4 APPEND saved-messages {0+}\r\n\r\n")
	wantLines(t, "an empty message", c.until("a4"), "NO an empty message is not stored")
}

// RFC 3501 6.4.1 and 6.4.2: CHECK, and CLOSE, which "permanently removes
// all messages that have the \Deleted flag set" and sends no EXPUNGE
// responses; "no messages are removed ... if the mailbox is selected by an
// EXAMINE command".
func TestRFC3501CheckClose(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 4)
	c := e.selected("INBOX")
	c.expect("CHECK", "OK CHECK completed")
	c.ok(`STORE 2:3 +FLAGS.SILENT (\Deleted)`)
	c.expect("CLOSE", "OK CLOSE completed")
	c.expect("FETCH 1 FLAGS", "BAD select a mailbox first")
	c.expect("STATUS INBOX (MESSAGES)", "* STATUS INBOX (MESSAGES 2)", "OK STATUS completed")

	c.ok("SELECT INBOX")
	c.ok(`STORE 1 +FLAGS.SILENT (\Deleted)`)
	c.ok("EXAMINE INBOX")
	c.expect("CLOSE", "OK CLOSE completed")
	c.expect("STATUS INBOX (MESSAGES)", "* STATUS INBOX (MESSAGES 2)", "OK STATUS completed")
}

// RFC 3501 6.4.3, the example: messages 3, 4, 7 and 11 are deleted.
//
//	C: A202 EXPUNGE
//	S: * 3 EXPUNGE
//	S: * 3 EXPUNGE
//	S: * 5 EXPUNGE
//	S: * 8 EXPUNGE
//	S: A202 OK EXPUNGE completed
func TestRFC3501Expunge(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 12)
	c := e.selected("INBOX")
	c.ok(`STORE 3:4,7,11 +FLAGS.SILENT (\Deleted)`)
	c.expect("EXPUNGE", "* 3 EXPUNGE", "* 3 EXPUNGE", "* 5 EXPUNGE", "* 8 EXPUNGE", "OK EXPUNGE completed")
	c.expect("EXPUNGE", "OK EXPUNGE completed")
	c.expect("FETCH 1:* UID",
		"* 1 FETCH (UID 1)", "* 2 FETCH (UID 2)", "* 3 FETCH (UID 5)", "* 4 FETCH (UID 6)", "* 5 FETCH (UID 8)",
		"* 6 FETCH (UID 9)", "* 7 FETCH (UID 10)", "* 8 FETCH (UID 12)", "OK FETCH completed")
}

// RFC 3501 7.4.1: "An EXPUNGE response MUST NOT be sent when no command is
// in progress, nor while responding to a FETCH, STORE, or SEARCH command".
// A message another session removed keeps its number until a command that
// may say so; a FETCH of it meanwhile returns nothing for it (RFC 2180
// 4.1.2), and new mail is announced all the same (7.3.1 allows EXISTS
// anywhere).
func TestRFC3501ExpungeOnlyWhenAllowed(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 5)
	c := e.selected("INBOX")
	e.st.remove("INBOX", 2)
	e.st.add("INBOX", plain(6, "new"))
	c.quiet(100 * time.Millisecond) // nothing is said outside a command

	c.expect("FETCH 1:* (UID BODY.PEEK[HEADER.FIELDS (SUBJECT)])",
		"* 1 FETCH (UID 1 BODY[HEADER.FIELDS (SUBJECT)] {22}\r\nSubject: message 1\r\n\r\n)",
		"* 3 FETCH (UID 3 BODY[HEADER.FIELDS (SUBJECT)] {22}\r\nSubject: message 3\r\n\r\n)",
		"* 4 FETCH (UID 4 BODY[HEADER.FIELDS (SUBJECT)] {22}\r\nSubject: message 4\r\n\r\n)",
		"* 5 FETCH (UID 5 BODY[HEADER.FIELDS (SUBJECT)] {22}\r\nSubject: message 5\r\n\r\n)",
		"* 6 EXISTS", "* 0 RECENT", "OK FETCH completed")
	// Once the session knows that the message is gone it has nothing to
	// say about it, but its number stays taken.
	c.expect("FETCH 2 UID", "OK FETCH completed")
	// For the client message 2 is still there, and a search by what the
	// session knows of it finds it; its text is gone with the message.
	c.expect("SEARCH ALL", "* SEARCH 1 2 3 4 5 6", "OK SEARCH completed")
	c.expect("SEARCH 2:3 UNSEEN SMALLER 5000", "* SEARCH 2 3", "OK SEARCH completed")
	c.expect("SEARCH SUBJECT message", "* SEARCH 1 3 4 5", "OK SEARCH completed")
	c.expect("SEARCH BODY message", "* SEARCH 1 3 4 5 6", "OK SEARCH completed")
	c.expect(`STORE 2:3 +FLAGS (\Seen)`, `* 3 FETCH (FLAGS (\Seen))`, "OK STORE completed")
	c.expect("FETCH 2 BODY.PEEK[]", "OK FETCH completed")
	// NOOP may: the numbers move now.
	c.expect("NOOP", "* 2 EXPUNGE", "OK NOOP completed")
	c.expect("FETCH 1:* UID",
		"* 1 FETCH (UID 1)", "* 2 FETCH (UID 3)", "* 3 FETCH (UID 4)", "* 4 FETCH (UID 5)", "* 5 FETCH (UID 6)", "OK FETCH completed")

	// The UID forms are other commands (RFC 9051 7.5.1 says so in words):
	// EXPUNGE may be sent while answering them.
	e.st.remove("INBOX", 4)
	c.expect("UID FETCH 1:* BODY.PEEK[HEADER.FIELDS (SUBJECT)]",
		"* 1 FETCH (UID 1 BODY[HEADER.FIELDS (SUBJECT)] {22}\r\nSubject: message 1\r\n\r\n)",
		"* 2 FETCH (UID 3 BODY[HEADER.FIELDS (SUBJECT)] {22}\r\nSubject: message 3\r\n\r\n)",
		"* 4 FETCH (UID 5 BODY[HEADER.FIELDS (SUBJECT)] {22}\r\nSubject: message 5\r\n\r\n)",
		"* 5 FETCH (UID 6 BODY[HEADER.FIELDS (SUBJECT)] {16}\r\nSubject: new\r\n\r\n)",
		"* 3 EXPUNGE", "OK UID FETCH completed")
}
