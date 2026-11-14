package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/mikepage/robinmail/internal/queue"
)

// TestQuotaEndToEnd is phase 8's quotas on a real server: a default limit
// from the config, an account's own limit from the command line, and the
// refusal at every way in: SMTP (RFC 5321 4.2.2: 452 for a full mailbox,
// 552 for a message that can never fit), IMAP APPEND (RFC 9208 5.2:
// OVERQUOTA), JMAP Email/import (overQuota) and upload. The use is shown
// by IMAP GETQUOTAROOT, JMAP Quota/get and "user quota".
//
//	ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 go test -v -run TestQuotaEndToEnd ./cmd/robinmail
func TestQuotaEndToEnd(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	var imapAddr string
	onIMAPListen = func(_, implicit net.Addr) { imapAddr = implicit.String() }
	t.Cleanup(func() { onIMAPListen = nil })
	in := startInstanceWith(t, base, "q", `"quota":{"default_messages":3},"imap":{"listen_tls":"127.0.0.1:0"},`, func(*queue.Worker, *instance) {})
	anna, bob := in.user("anna"), in.user("bob")

	mail := func(n int) string {
		return message(anna, fmt.Sprintf("quota-%d-%s", n, randomHex()), fmt.Sprintf("Message %d", n), "x")
	}
	smtpc := dialSMTP(t, in.smtp)
	smtpc.cmd("EHLO client.example", 250)
	for n := 1; n <= 3; n++ {
		smtpc.send("a@sender.example", anna, mail(n), 250)
	}
	// The default of the config: three messages. The fourth is refused at
	// RCPT, for this recipient only: Bob, in the same transaction, is not.
	smtpc.cmd("MAIL FROM:<a@sender.example>", 250)
	if reply := smtpc.cmd("RCPT TO:<"+anna+">", 452); !strings.HasPrefix(reply, "452 4.2.2 ") {
		t.Errorf("RCPT for a full mailbox: %q", reply)
	}
	smtpc.cmd("RCPT TO:<"+bob+">", 250)
	smtpc.cmd("DATA", 354)
	smtpc.conn.Write([]byte(mail(4) + ".\r\n"))
	smtpc.read(250)
	in.expect(in.inbox(bob), `Message 4`)
	if strings.Contains(in.inbox(anna), "Message 4") {
		t.Error("the full mailbox got the fourth message")
	}

	// IMAP: the quota root and its use; APPEND is refused.
	c := dialIMAP(t, "q", imapAddr)
	login := c.cmd(fmt.Sprintf(`LOGIN %s "correct horse"`, anna))
	if caps := login[len(login)-1]; !strings.Contains(caps, "QUOTA QUOTA=RES-STORAGE QUOTA=RES-MESSAGE") {
		t.Errorf("capabilities: %s", caps)
	}
	if got := c.ok("GETQUOTAROOT INBOX"); !hasLine(got, `* QUOTAROOT INBOX ""`) || !hasLine(got, `* QUOTA "" (MESSAGE 3 3)`) {
		t.Errorf("GETQUOTAROOT: %v", got)
	}
	appended := "Subject: appended\r\n\r\nx\r\n"
	c.write(fmt.Sprintf("ap1 APPEND INBOX {%d+}\r\n%s\r\n", len(appended), appended))
	if got := c.line(10e9); !strings.HasPrefix(got, "ap1 NO [OVERQUOTA]") {
		t.Errorf("APPEND to a full mailbox: %q", got)
	}

	// JMAP: Quota/get, an import that is refused, an upload that is not
	// (a message limit does not stop an upload; an octet limit does, below).
	j, acct := jmapSession(t, in, anna)
	quota := func() []any {
		t.Helper()
		body := `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:quota"],"methodCalls":[["Quota/get",{"accountId":"` + acct + `","ids":null},"0"]]}`
		resp, data := j.do("POST", "/jmap/api", body, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("Quota/get: %d %s", resp.StatusCode, data)
		}
		return dig(t, decodeJSON(t, data), "methodResponses", 0, 1, "list").([]any)
	}
	if list := quota(); len(list) != 1 || dig(t, list, 0, "id") != "count" || dig(t, list, 0, "used") != float64(3) || dig(t, list, 0, "hardLimit") != float64(3) {
		t.Errorf("Quota/get: %v", list)
	}
	blob := j.upload(acct, "message/rfc822", appended)
	res := j.api("/jmap/api", `["Email/import", {"accountId": "`+acct+`", "emails": {"x": {"blobId": "`+blob+`", "mailboxIds": {"m1": true}}}}, "0"]`)
	if dig(t, res, 0, 1, "notCreated", "x", "type") != "overQuota" {
		t.Errorf("Email/import into a full account: %v", res)
	}

	// The administrator gives the account its own limits: 2000 octets, no
	// limit on the number.
	out := in.ok("", "user", "quota", anna, "2000", "none")
	in.expect(out, `storage   .* of 2\.0 KiB \(its own\)`, `messages  3 no limit \(its own\)`)
	in.expect(in.ok("", "user", "quota", bob), `messages  1 of 3 \(the default\)`, `storage   .* no limit \(the default\)`)
	used := int64(dig(t, quota(), 0, "used").(float64))
	if list := quota(); len(list) != 1 || dig(t, list, 0, "id") != "octets" || dig(t, list, 0, "hardLimit") != float64(2000) {
		t.Errorf("Quota/get after the change: %v", list)
	}
	if got := c.ok("GETQUOTAROOT INBOX"); !hasLine(got, fmt.Sprintf(`* QUOTA "" (STORAGE %d 1)`, (used+1023)/1024)) {
		t.Errorf("GETQUOTAROOT with a storage limit: %v", got)
	}
	// A message larger than the whole quota never fits: 552. One that
	// would fit an empty mailbox but not this one: 452.
	smtpc.cmd("MAIL FROM:<a@sender.example>", 250)
	smtpc.cmd("RCPT TO:<"+anna+">", 250)
	smtpc.cmd("DATA", 354)
	smtpc.conn.Write([]byte(message(anna, "big-"+randomHex(), "Too big", strings.Repeat("0123456789\r\n", 300)) + ".\r\n"))
	if reply := smtpc.read(552); !strings.HasPrefix(reply, "552 5.2.3 ") {
		t.Errorf("a message larger than the quota: %q", reply)
	}
	smtpc.cmd("MAIL FROM:<a@sender.example>", 250)
	smtpc.cmd("RCPT TO:<"+anna+">", 250)
	smtpc.cmd("DATA", 354)
	smtpc.conn.Write([]byte(message(anna, "mid-"+randomHex(), "No room now", strings.Repeat("0123456789\r\n", 100)) + ".\r\n"))
	if reply := smtpc.read(452); !strings.HasPrefix(reply, "452 4.2.2 ") {
		t.Errorf("a message there is no room for: %q", reply)
	}
	// An upload that would not fit either.
	if resp, data := j.do("POST", "/jmap/upload/"+acct+"/", strings.Repeat("x", 3000), map[string]string{"Content-Type": "text/plain"}); resp.StatusCode != 507 {
		t.Errorf("an upload past the quota: %d %s", resp.StatusCode, data)
	}

	// Room is made by deleting, and mail flows again.
	c.ok("SELECT INBOX")
	c.ok(`STORE 1:2 +FLAGS (\Deleted)`)
	c.ok("EXPUNGE")
	smtpc.send("a@sender.example", anna, mail(5), 250)
	in.expect(in.inbox(anna), `Message 5`)
	// "default" gives the account the limits of the config back, and the
	// counters are what the mail adds up to.
	in.expect(in.ok("", "user", "quota", anna, "default", "default"), `messages  2 of 3 \(the default\)`)
	in.expect(in.ok("", "quota", "recount", anna), `1 accounts counted, 0 corrected`)
	for _, bad := range [][]string{{"user", "quota", anna, "lots", "none"}, {"user", "quota", anna, "-5", "none"}, {"user", "quota", "nobody@" + in.domain}} {
		if out, err := in.do("", bad...); err == nil {
			t.Errorf("%v worked: %s", bad, out)
		}
	}
}

// decodeJSON reads a JSON document into the shapes dig walks.
func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return v
}
