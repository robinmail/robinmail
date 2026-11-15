package main

import (
	"crypto/x509"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/robinmail/robinmail/internal/queue"
)

// TestAliasesAndForwarding is phase 8's aliases between two servers, each
// with its own domain and database, with DNS and SMTP as in
// TestSendBetweenTwoServers.
//
//   - Server A has the alias info@A for Anna (an account on A) and Bob (on
//     B). Dave on B writes to it: Anna has the message, and Bob gets it
//     from A with A's rewritten sender (SRS) and Delivered-To.
//
//   - The alias gone@A leads to an address B does not have. B refuses it,
//     A's queue bounces, and the bounce travels back through the rewritten
//     address to Dave on B.
//
//   - Anna sends as info@A over SMTP and has an identity for it over JMAP;
//     she may not send as an alias that is not hers.
//
//     ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 ROBINMAIL_TEST_RQLITE_B=http://127.0.0.1:4003 \
//     go test -v -run TestAliasesAndForwarding ./cmd/robinmail
func TestAliasesAndForwarding(t *testing.T) {
	baseA, baseB := os.Getenv("ROBINMAIL_TEST_RQLITE"), os.Getenv("ROBINMAIL_TEST_RQLITE_B")
	if baseA == "" || baseB == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE and ROBINMAIL_TEST_RQLITE_B are not both set")
	}
	world := newSendNet()
	setup := func(w *queue.Worker, in *instance) {
		w.Resolver = world
		w.Dial = world.dial
		w.RootCAs = x509.NewCertPool()
	}
	b := startInstance(t, baseB, "b", setup)
	a := startInstance(t, baseA, "a", setup)
	world.host(a.domain, a.host, "192.0.2.1", a.smtp)
	world.host(b.domain, b.host, "192.0.2.2", b.smtp)
	anna, bob, dave := a.user("anna"), b.user("bob"), b.user("dave")
	info, gone := "info@"+a.domain, "gone@"+a.domain
	// What the system accounts of the two domains still have queued goes
	// with the test, like the users' own.
	t.Cleanup(func() { a.purgeSystem(); b.purgeSystem() })

	// The commands.
	a.expect(a.ok("", "alias", "add", "Info@"+a.domain, anna, bob), regexp.QuoteMeta(info+" -> "+anna+", "+bob))
	a.ok("", "alias", "add", gone, "nobody@"+b.domain)
	a.ok("", "alias", "add", "sales-*@"+a.domain, anna)
	a.ok("", "alias", "add", "@"+a.domain, anna)
	a.expect(a.ok("", "alias", "list", a.domain), `(?m)^`+regexp.QuoteMeta(info)+` -> `, `(?m)^\*@`+regexp.QuoteMeta(a.domain)+` -> `, `(?m)^sales-\*@`)
	for _, bad := range [][]string{
		{"alias", "add", anna, bob},                   // a user's own address
		{"alias", "add", "x@elsewhere.example", anna}, // not a domain of ours
		{"alias", "add", "a*b*c@" + a.domain, anna},   // two stars
		{"alias", "add", "loop@" + a.domain, "loop@" + a.domain},
		{"alias", "remove", "nothing@" + a.domain},
	} {
		if out, err := a.do("", bad...); err == nil {
			t.Errorf("%v worked: %s", bad, out)
		}
	}
	if out, err := a.do("", "domain", "remove", a.domain); err == nil {
		t.Errorf("a domain with aliases was removed: %s", out)
	}

	// --- 1. Dave on B writes to the alias on A. ---
	send := func(from, password, to, subject string, in *instance, want int) {
		t.Helper()
		s := dialSMTP(t, in.submission)
		s.cmd("EHLO laptop.home.arpa", 250)
		s.cmd("STARTTLS", 220)
		s.startTLS()
		s.cmd("EHLO laptop.home.arpa", 250)
		s.cmd(plainAuth(from, password), 235)
		s.cmd("MAIL FROM:<"+from+">", 250)
		s.cmd("RCPT TO:<"+to+">", 250)
		s.cmd("DATA", 354)
		s.conn.Write([]byte("From: <" + from + ">\r\nTo: <" + to + ">\r\nSubject: " + subject + "\r\n\r\nA question for the office.\r\n.\r\n"))
		s.read(want)
		s.cmd("QUIT", 221)
	}
	send(dave, "correct horse", info, "To the alias", b, 250)
	waitFor(t, "the message in Anna's Inbox", func() bool { return strings.Contains(a.inbox(anna), "To the alias") })
	waitFor(t, "the forwarded message in Bob's Inbox", func() bool { return strings.Contains(b.inbox(bob), "To the alias") })
	forwarded := b.ok("", "cat", bob, "e1")
	t.Logf("in Bob's Inbox on server B, forwarded by A:\n%s", strings.ReplaceAll(forwarded, "\r\n", "\n"))
	b.expect(forwarded,
		// The envelope sender is Dave's, rewritten under A's domain.
		`^Return-Path: <SRS0=[a-z2-7]{8}=[a-z2-7]{2}=`+regexp.QuoteMeta(b.domain)+`=dave@`+regexp.QuoteMeta(a.domain)+`>\r\n`,
		// B got it from A, and A's own trace of receiving it is below.
		`\r\nReceived: from `+regexp.QuoteMeta(a.host)+` `,
		`\r\nDelivered-To: `+regexp.QuoteMeta(info)+`\r\nReceived: from `+regexp.QuoteMeta(b.host)+` `,
		`\r\nFrom: <`+regexp.QuoteMeta(dave)+`>\r\n`,
		`\r\n\r\nA question for the office\.\r\n$`)
	// Anna's copy is a final delivery: Dave's own address in Return-Path.
	a.expect(a.ok("", "cat", anna, "e1"), `^Return-Path: <`+regexp.QuoteMeta(dave)+`>\r\n`)
	// Plus-addressing, a pattern and the catch-all reach her too.
	for _, to := range []string{"info+office@" + a.domain, "sales-north@" + a.domain, "whoever@" + a.domain} {
		send(dave, "correct horse", to, "Also for "+to, b, 250)
		waitFor(t, "mail for "+to, func() bool { return strings.Contains(a.inbox(anna), "Also for "+to) })
	}

	// --- 2. An alias that leads nowhere on B: the bounce finds Dave. ---
	send(dave, "correct horse", gone, "To nobody", b, 250)
	waitFor(t, "the bounce in Dave's Inbox", func() bool { return strings.Contains(b.inbox(dave), "Undelivered") })
	bounce := b.ok("", "cat", dave, "e1")
	t.Logf("in Dave's Inbox on server B, the bounce A passed back:\n%s", strings.ReplaceAll(bounce, "\r\n", "\n"))
	b.expect(bounce,
		`^Return-Path: <>\r\n`,
		`\r\nFrom: .*MAILER-DAEMON@`+regexp.QuoteMeta(a.host),
		// It is addressed to the rewritten sender it went back through.
		`\r\nTo: <?SRS0=[^@]+=dave@`+regexp.QuoteMeta(a.domain),
		`Final-Recipient: rfc822; ?nobody@`+regexp.QuoteMeta(b.domain),
		`Status: 5\.1\.1`,
		`Subject: To nobody`)

	// A rewritten address takes notifications only: with a sender it is
	// refused, so it cannot be used to have mail passed on.
	rewritten := regexp.MustCompile(`Return-Path: <(SRS0=[^>]+)>`).FindStringSubmatch(forwarded)[1]
	s := dialSMTP(t, a.smtp)
	s.cmd("EHLO relay.example", 250)
	s.cmd("MAIL FROM:<someone@elsewhere.example>", 250)
	if reply := s.cmd("RCPT TO:<"+rewritten+">", 550); !strings.HasPrefix(reply, "550 5.7.1") {
		t.Errorf("mail with a sender for a rewritten address: %q", reply)
	}
	s.cmd("RSET", 250)
	s.cmd("MAIL FROM:<>", 250)
	s.cmd("RCPT TO:<"+rewritten+">", 250)
	s.cmd("RCPT TO:<"+strings.Replace(rewritten, "=dave@", "=eve@", 1)+">", 550)
	s.cmd("QUIT", 221)

	// --- 3. Anna sends as the alias. ---
	sa := dialSMTP(t, a.submission)
	sa.cmd("EHLO annas-laptop.home.arpa", 250)
	sa.cmd("STARTTLS", 220)
	sa.startTLS()
	sa.cmd("EHLO annas-laptop.home.arpa", 250)
	sa.cmd(plainAuth(anna, "correct horse"), 235)
	sa.cmd("MAIL FROM:<"+gone+">", 550)                 // an alias, but not hers
	sa.cmd("MAIL FROM:<whoever@"+a.domain+">", 550)     // the catch-all gives no right to send
	sa.cmd("MAIL FROM:<sales-north@"+a.domain+">", 550) // nor does a pattern
	sa.cmd("MAIL FROM:<"+info+">", 250)
	sa.cmd("RCPT TO:<"+dave+">", 250)
	sa.cmd("DATA", 354)
	sa.conn.Write([]byte("From: The Office <" + info + ">\r\nTo: <" + dave + ">\r\nSubject: From the office\r\n\r\nAn answer.\r\n.\r\n"))
	sa.read(250)
	// Her own envelope sender with the alias in From is fine too; an alias
	// that is not hers in From is not.
	sa.cmd("MAIL FROM:<"+anna+">", 250)
	sa.cmd("RCPT TO:<"+dave+">", 250)
	sa.cmd("DATA", 354)
	sa.conn.Write([]byte("From: <" + gone + ">\r\nSubject: Not mine\r\n\r\nx\r\n.\r\n"))
	sa.read(550)
	sa.cmd("QUIT", 221)
	waitFor(t, "Anna's message as the alias in Dave's Inbox", func() bool { return strings.Contains(b.inbox(dave), "From the office") })

	ja, annaAcct := jmapSession(t, a, anna)
	res := ja.call(`["Identity/set", {"accountId": "` + annaAcct + `", "create": {
		"office": {"email": "` + info + `", "name": "The Office"},
		"tagged": {"email": "info+shop@` + a.domain + `"},
		"notmine": {"email": "` + gone + `"},
		"pattern": {"email": "sales-north@` + a.domain + `"}}}, "0"]`)
	if dig(t, res, 0, 1, "created", "office", "id") == nil || dig(t, res, 0, 1, "created", "tagged", "id") == nil {
		t.Errorf("identities for the alias: %v", res)
	}
	for _, id := range []string{"notmine", "pattern"} {
		if got := dig(t, res, 0, 1, "notCreated", id, "type"); got != "forbiddenFrom" {
			t.Errorf("identity %s: %v", id, got)
		}
	}

	// Without the alias the address is gone at the next RCPT.
	a.ok("", "alias", "remove", info, bob)
	a.expect(a.ok("", "alias", "list", a.domain), `(?m)^`+regexp.QuoteMeta(info+" -> "+anna)+`$`)
	a.ok("", "alias", "remove", gone)
	a.ok("", "alias", "remove", "@"+a.domain)
	s = dialSMTP(t, a.smtp)
	s.cmd("EHLO relay.example", 250)
	s.cmd("MAIL FROM:<someone@elsewhere.example>", 250)
	s.cmd("RCPT TO:<"+gone+">", 550)
	s.cmd("RCPT TO:<"+info+">", 250)
	s.cmd("QUIT", 221)
}
