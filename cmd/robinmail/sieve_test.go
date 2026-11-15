package main

import (
	"crypto/x509"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/robinmail/robinmail/internal/queue"
)

// sieveCall posts method calls with the Sieve capability.
func (c *jmapClient) sieveCall(calls string) []any {
	c.t.Helper()
	body := `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:sieve"],"methodCalls":[` + calls + `]}`
	resp, data := c.do("POST", "/jmap/api", body, nil)
	if resp.StatusCode != 200 {
		c.t.Fatalf("API status %d: %s", resp.StatusCode, data)
	}
	var out struct {
		MethodResponses []any `json:"methodResponses"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		c.t.Fatal(err)
	}
	return out.MethodResponses
}

// upload posts a blob and returns its id (RFC 8620 6.1).
func (c *jmapClient) upload(account, typ, content string) string {
	c.t.Helper()
	resp, data := c.do("POST", "/jmap/upload/"+account+"/", content, map[string]string{"Content-Type": typ})
	if resp.StatusCode != 201 {
		c.t.Fatalf("upload: %d %s", resp.StatusCode, data)
	}
	var out struct {
		BlobID string `json:"blobId"`
	}
	json.Unmarshal(data, &out)
	return out.BlobID
}

// TestSieveEndToEnd is phase 8's filters on real servers: two robinmail
// instances with a database each, DNS and SMTP between them as in
// TestSendBetweenTwoServers.
//
// Anna on A gives herself a script over JMAP (an upload, SieveScript/set,
// activation) and Bob on B writes to her:
//
//   - an invoice is filed into a mailbox the script makes, flagged and not
//     in the Inbox, and into a second mailbox as a label;
//
//   - a question gets a vacation reply, once, with the null reverse-path;
//
//   - a message to pass on reaches Carol on B with A's rewritten sender;
//
//   - a message she refuses comes back to Bob with her reason.
//
//     ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 ROBINMAIL_TEST_RQLITE_B=http://127.0.0.1:4003 \
//     go test -v -run TestSieveEndToEnd ./cmd/robinmail
func TestSieveEndToEnd(t *testing.T) {
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
	anna, bob, carol := a.user("anna"), b.user("bob"), b.user("carol")
	t.Cleanup(func() { a.purgeSystem(); b.purgeSystem() })

	script := `require ["fileinto", "mailbox", "imap4flags", "copy", "vacation", "ereject", "envelope"];

if header :contains "subject" "invoice" {
    addflag "\\Flagged";
    fileinto :create "Work/Invoices";
    fileinto :create "Archive";
    stop;
}
if header :contains "subject" "pass this on" {
    redirect "` + carol + `";
    stop;
}
if header :contains "subject" "unwanted" {
    ereject "Anna does not take this kind of mail";
    stop;
}
vacation :days 2 :subject "Away" "I am away this week. Anna";
`
	ja, annaAcct := jmapSession(t, a, anna)
	// A script that does not parse is refused with the reason, and
	// SieveScript/validate says the same without storing anything.
	broken := ja.upload(annaAcct, "application/sieve", "if header :contains \"subject\" { keep; }\r\n")
	res := ja.sieveCall(`["SieveScript/validate", {"accountId": "` + annaAcct + `", "blobId": "` + broken + `"}, "0"],
		["SieveScript/set", {"accountId": "` + annaAcct + `", "create": {"x": {"name": "broken", "blobId": "` + broken + `"}}}, "1"]`)
	if dig(t, res, 0, 1, "error", "type") != "invalidSieve" || dig(t, res, 1, 1, "notCreated", "x", "type") != "invalidSieve" {
		t.Fatalf("a broken script: %v", res)
	}
	blob := ja.upload(annaAcct, "application/sieve", strings.ReplaceAll(script, "\n", "\r\n"))
	res = ja.sieveCall(`["SieveScript/set", {"accountId": "` + annaAcct + `", "create": {"main": {"name": "main", "blobId": "` + blob + `"}},
		"onSuccessActivateScript": "#main"}, "0"],
		["SieveScript/get", {"accountId": "` + annaAcct + `", "ids": null}, "1"]`)
	if dig(t, res, 0, 1, "created", "main", "isActive") != true || dig(t, res, 1, 1, "list", 0, "isActive") != true {
		t.Fatalf("the script was not made active: %v", res)
	}
	// The session says what the server has.
	_, data := ja.do("GET", "/jmap/session", "", nil)
	if !strings.Contains(string(data), `"urn:ietf:params:jmap:sieve"`) || !strings.Contains(string(data), `"maxNumberRedirects":4`) {
		t.Errorf("the session has no Sieve capability: %.300s", data)
	}

	send := func(to, subject string) {
		t.Helper()
		s := dialSMTP(t, b.submission)
		s.cmd("EHLO laptop.home.arpa", 250)
		s.cmd("STARTTLS", 220)
		s.startTLS()
		s.cmd("EHLO laptop.home.arpa", 250)
		s.cmd(plainAuth(bob, "correct horse"), 235)
		s.cmd("MAIL FROM:<"+bob+">", 250)
		s.cmd("RCPT TO:<"+to+">", 250)
		s.cmd("DATA", 354)
		s.conn.Write([]byte("From: Bob <" + bob + ">\r\nTo: Anna <" + to + ">\r\nSubject: " + subject + "\r\n\r\nBody of " + subject + ".\r\n.\r\n"))
		s.read(250)
		s.cmd("QUIT", 221)
	}

	// --- 1. Filed: two mailboxes the script made, one Email, flagged. ---
	send(anna, "Invoice 7")
	waitFor(t, "the invoice in Work/Invoices", func() bool {
		out, err := a.do("", "list", anna, "Work/Invoices")
		return err == nil && strings.Contains(out, "Invoice 7")
	})
	filed := a.ok("", "list", anna, "Work/Invoices")
	a.expect(filed, `\$flagged`)
	id := regexp.MustCompile(`(?m)^(e\w+)\s`).FindStringSubmatch(filed)[1]
	if archive := a.ok("", "list", anna, "Archive"); !strings.Contains(archive, id+" ") {
		t.Errorf("the same Email is not in Archive too:\n%s", archive)
	}
	if inbox := a.inbox(anna); strings.Contains(inbox, "Invoice 7") {
		t.Errorf("the invoice is in the Inbox as well:\n%s", inbox)
	}
	a.expect(a.ok("", "mailbox", "list", anna), `Work/Invoices`, `Archive`)
	// No vacation reply for it: the script stopped before.

	// --- 2. A vacation reply, once. ---
	send(anna, "Are you there?")
	waitFor(t, "the question in Anna's Inbox", func() bool { return strings.Contains(a.inbox(anna), "Are you there?") })
	waitFor(t, "the vacation reply in Bob's Inbox", func() bool { return strings.Contains(b.inbox(bob), "Away") })
	reply := b.ok("", "cat", bob, "e1")
	t.Logf("in Bob's Inbox on server B, Anna's automatic reply:\n%s", strings.ReplaceAll(reply, "\r\n", "\n"))
	b.expect(reply,
		`^Return-Path: <>\r\n`, // RFC 5230 5.1: the null reverse-path
		`\r\nReceived: by `+regexp.QuoteMeta(a.host)+` \(robinmail\) \(automatic reply\) id `,
		`\r\nFrom: `+regexp.QuoteMeta(anna)+`\r\n`,
		`\r\nTo: `+regexp.QuoteMeta(bob)+`\r\n`,
		`\r\nSubject: Away\r\n`,
		`\r\nIn-Reply-To: <[^>]+@`+regexp.QuoteMeta(b.host)+`>\r\n`,
		`\r\nAuto-Submitted: auto-replied\r\n`,
		`\r\n\r\nI am away this week\. Anna`)
	send(anna, "Still there?")
	waitFor(t, "the second question in Anna's Inbox", func() bool { return strings.Contains(a.inbox(anna), "Still there?") })

	// --- 3. Redirected: Carol on B has it, Anna does not. ---
	send(anna, "Please pass this on")
	waitFor(t, "the redirected message in Carol's Inbox", func() bool { return strings.Contains(b.inbox(carol), "pass this on") })
	passed := b.ok("", "cat", carol, "e1")
	b.expect(passed,
		`^Return-Path: <SRS0=[a-z2-7]{8}=[a-z2-7]{2}=`+regexp.QuoteMeta(b.domain)+`=bob@`+regexp.QuoteMeta(a.domain)+`>\r\n`,
		`\r\nDelivered-To: `+regexp.QuoteMeta(anna)+`\r\n`,
		`\r\nFrom: Bob <`+regexp.QuoteMeta(bob)+`>\r\n`)
	if strings.Contains(a.inbox(anna), "pass this on") {
		t.Error("the redirected message was kept as well")
	}

	// --- 4. Refused: B's queue gets Anna's reason and tells Bob. ---
	send(anna, "Unwanted offer")
	waitFor(t, "the bounce in Bob's Inbox", func() bool { return strings.Contains(b.inbox(bob), "Undelivered") })
	var bounce string
	for _, line := range strings.Split(b.inbox(bob), "\n") {
		if strings.Contains(line, "Undelivered") {
			bounce = b.ok("", "cat", bob, strings.Fields(line)[0])
		}
	}
	b.expect(bounce, `Status: 5\.7\.1`, `Anna does not take this kind of mail`, `Final-Recipient: rfc822; ?`+regexp.QuoteMeta(anna))
	if strings.Contains(a.inbox(anna), "Unwanted") {
		t.Error("the refused message was stored")
	}

	// One vacation reply in all: none for the second question (once per
	// sender in two days), none for the filed, redirected or refused mail.
	if n := strings.Count(b.inbox(bob), "Away"); n != 1 {
		t.Errorf("%d vacation replies in Bob's Inbox:\n%s", n, b.inbox(bob))
	}

	// Without an active script mail is kept as before.
	res = ja.sieveCall(`["SieveScript/set", {"accountId": "` + annaAcct + `", "onSuccessDeactivateScript": true}, "0"]`)
	if dig(t, res, 0, 1, "updated", "s1", "isActive") != false {
		t.Fatalf("deactivating: %v", res)
	}
	send(anna, "Invoice 8")
	waitFor(t, "an invoice in the Inbox once the script is off", func() bool { return strings.Contains(a.inbox(anna), "Invoice 8") })
}
