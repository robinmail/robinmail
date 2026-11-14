package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/queue"
)

// imapClient speaks IMAP by hand and logs the dialogue.
type imapClient struct {
	t    *testing.T
	name string
	conn net.Conn
	br   *bufio.Reader
	tag  int
}

func dialIMAP(t *testing.T, name, addr string) *imapClient {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := &imapClient{t: t, name: name, conn: conn, br: bufio.NewReader(conn)}
	if g := c.line(10 * time.Second); !strings.HasPrefix(g, "* OK ") {
		t.Fatalf("%s: greeting %q", name, g)
	}
	return c
}

var imapLiteral = regexp.MustCompile(`\{(\d+)\}$`)

// line reads one response line; a literal stays in it.
func (c *imapClient) line(within time.Duration) string {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(within))
	var out strings.Builder
	for {
		l, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatalf("%s: read: %v (after %q)", c.name, err, out.String()+l)
		}
		l = strings.TrimSuffix(l, "\r\n")
		out.WriteString(l)
		m := imapLiteral.FindStringSubmatch(l)
		if m == nil {
			c.t.Logf("%s S: %s", c.name, clipLog(out.String()))
			return out.String()
		}
		n, _ := strconv.Atoi(m[1])
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.br, buf); err != nil {
			c.t.Fatalf("%s: read literal: %v", c.name, err)
		}
		out.WriteString("\r\n")
		out.Write(buf)
	}
}

func (c *imapClient) write(s string) {
	c.t.Helper()
	c.t.Logf("%s C: %s", c.name, strings.TrimRight(s, "\r\n"))
	if _, err := io.WriteString(c.conn, s); err != nil {
		c.t.Fatalf("%s: write: %v", c.name, err)
	}
}

// cmd sends a command and returns the responses; the tagged one is last,
// without its tag.
func (c *imapClient) cmd(command string) []string {
	c.t.Helper()
	c.tag++
	tag := fmt.Sprintf("%s%d", c.name, c.tag)
	c.write(tag + " " + command + "\r\n")
	var lines []string
	for {
		l := c.line(20 * time.Second)
		if rest, ok := strings.CutPrefix(l, tag+" "); ok {
			return append(lines, rest)
		}
		lines = append(lines, l)
	}
}

// ok is cmd for a command that must succeed; it returns the untagged lines.
func (c *imapClient) ok(command string) []string {
	c.t.Helper()
	lines := c.cmd(command)
	if !strings.HasPrefix(lines[len(lines)-1], "OK") {
		c.t.Fatalf("%s: %s: %v", c.name, command, lines)
	}
	return lines[:len(lines)-1]
}

// want reads lines until one equals the wanted one, and fails when it
// does not come in time. What a server says unasked comes in any order
// with other news; this picks the line a step waits for.
func (c *imapClient) want(line string, within time.Duration) time.Duration {
	c.t.Helper()
	start := time.Now()
	deadline := start.Add(within)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			c.t.Fatalf("%s: %q did not come within %v", c.name, line, within)
		}
		if got := c.line(left); got == line {
			return time.Since(start)
		}
	}
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// TestIMAPAndJMAPOnTwoNodes is the exit of phase 6 as far as a test can
// show it: two robinmail nodes on one rqlite, two IMAP sessions, a JMAP
// client and an event source, and every change made through one of them
// reaching the others.
//
//   - Mail delivered over SMTP on node B shows as EXISTS in an IMAP session
//     that idles on node A, and as a StateChange on node A's event source.
//   - A flag set over JMAP on node B arrives in the idling session as an
//     untagged FETCH.
//   - A MOVE in IMAP on node B is an EXPUNGE for the session on node A and
//     an updated Email, with the same id, in JMAP's Email/changes.
//   - A flag set over IMAP shows in JMAP; an EXPUNGE in IMAP destroys the
//     Email in JMAP.
//   - Failed IMAP logins count against the web login and the reverse.
//
// The nodes are two "serve" in this process with a store and a hub each.
// They share the blob directory, which stands in for blob replication:
// that is phase 4's and has its own tests. What node A hears of node B's
// writes it hears through its poll of the account's modseq.
//
//	ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 go test -v -run TestIMAPAndJMAPOnTwoNodes ./cmd/robinmail
func TestIMAPAndJMAPOnTwoNodes(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	type node struct {
		*instance
		imapTLS, imapPlain string
	}
	var a, b node
	listen := func(n *node) {
		onIMAPListen = func(plain, implicit net.Addr) { n.imapPlain, n.imapTLS = plain.String(), implicit.String() }
	}
	t.Cleanup(func() { onIMAPListen = nil })
	const imapConfig = `"imap":{"listen_tls":"127.0.0.1:0","listen":"127.0.0.1:0"},`

	// Node A makes the master key and the domain.
	listen(&a)
	a.instance = startInstanceWith(t, base, "imap", imapConfig, func(*queue.Worker, *instance) {})
	anna := a.user("anna")

	// Node B: the same database, master key and blobs, its own listeners.
	dirB := t.TempDir()
	cert, key, _ := selfSigned(t, dirB, a.host)
	cfgB := filepath.Join(dirB, "robinmail.json")
	body := fmt.Sprintf(`{"hostname":%q,"data_dir":%q,"master_key_file":%q,"rqlite":{"url":%q},`+imapConfig+`
		"tls":{"cert_file":%q,"key_file":%q},"smtp":{"listen":"127.0.0.1:0"},"web":{"http_listen":"127.0.0.1:0"}}`,
		a.host, filepath.Join(a.dir, "data"), filepath.Join(a.dir, "master.key"), base, cert, key)
	if err := os.WriteFile(cfgB, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	listen(&b)
	b.instance = &instance{t: t, cli: &cli{t: t, dir: dirB, cfg: cfgB}, domain: a.domain, host: a.host}
	srvB := startServe(t, b.cli)
	b.smtp, b.web = srvB.addr, srvB.web
	if a.imapTLS == "" || b.imapTLS == "" || a.imapTLS == b.imapTLS {
		t.Fatalf("imap listeners: %q %q", a.imapTLS, b.imapTLS)
	}

	// Port 143: no login before STARTTLS.
	plain, err := net.Dial("tcp", a.imapPlain)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	pr := bufio.NewReader(plain)
	greeting, _ := pr.ReadString('\n')
	if !strings.Contains(greeting, "STARTTLS LOGINDISABLED") || strings.Contains(greeting, "AUTH=") {
		t.Fatalf("greeting on the plain listener: %q", greeting)
	}
	fmt.Fprintf(plain, "p1 LOGIN %s \"correct horse\"\r\n", anna)
	if l, _ := pr.ReadString('\n'); !strings.HasPrefix(l, "p1 NO [PRIVACYREQUIRED]") {
		t.Fatalf("LOGIN without TLS: %q", l)
	}

	// Session 1 idles on node A. Session 2 is on node B. JMAP is on node B,
	// the event source on node A.
	one := dialIMAP(t, "one", a.imapTLS)
	one.ok(fmt.Sprintf(`LOGIN %s "correct horse"`, anna))
	if sel := one.ok("SELECT INBOX"); !hasLine(sel, "* 0 EXISTS") {
		t.Fatalf("SELECT of an empty INBOX: %v", sel)
	}
	one.write("idle1 IDLE\r\n")
	one.want("+ idling", 10*time.Second)

	two := dialIMAP(t, "two", b.imapTLS)
	two.ok(fmt.Sprintf(`LOGIN %s "correct horse"`, anna))
	two.ok("SELECT INBOX")

	jb, account := jmapSession(t, b.instance, anna)
	ja, _ := jmapSession(t, a.instance, anna)
	events := make(chan string, 100)
	ectx, stopEvents := context.WithCancel(context.Background())
	defer stopEvents()
	req, _ := http.NewRequestWithContext(ectx, "GET", "http://"+a.web+"/jmap/eventsource/?types=Email,EmailDelivery&closeafter=no&ping=0", nil)
	stream := *ja.hc
	stream.Timeout = 0
	resp, err := stream.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("event source: %v %v", resp, err)
	}
	defer resp.Body.Close()
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				events <- data
			}
		}
	}()
	event := func(what string, has ...string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case ev := <-events:
				t.Logf("event source on node A: %s", ev)
				match := true
				for _, h := range has {
					match = match && strings.Contains(ev, h)
				}
				if match {
					return
				}
			case <-deadline:
				t.Fatalf("no StateChange for %s within 10 seconds", what)
			}
		}
	}

	// 1. New mail over SMTP on node B.
	msg := message(anna, "phase6-"+randomHex(), "Push me", "Hello over three protocols.")
	smtpB := dialSMTP(t, b.smtp)
	smtpB.cmd("EHLO client.example", 250)
	smtpB.send("anna@sender.example", anna, msg, 250)
	took := one.want("* 1 EXISTS", 10*time.Second)
	t.Logf("SMTP on node B -> EXISTS in IDLE on node A after %v", took.Round(time.Millisecond))
	event("the delivery", `"EmailDelivery"`, `"Email"`)
	if got := two.ok("NOOP"); !hasLine(got, "* 1 EXISTS") {
		t.Fatalf("session two after NOOP: %v", got)
	}
	// The message as it was sent, read through the other node. Delivery
	// puts a Received and a Return-Path line on top.
	got := two.ok("FETCH 1 (FLAGS RFC822.SIZE BODY.PEEK[])")
	if len(got) != 1 || !strings.HasSuffix(got[0], msg+")") || !strings.Contains(got[0], "FLAGS ()") {
		t.Fatalf("FETCH on node B: %q", got)
	}

	// 2. A flag over JMAP on node B.
	r := jb.call(fmt.Sprintf(`["Email/query",{"accountId":%q},"q"]`, account))
	emailID := dig(t, r, 0, 1, "ids", 0).(string)
	state := dig(t, r, 0, 1, "queryState").(string)
	jb.call(fmt.Sprintf(`["Email/set",{"accountId":%q,"update":{%q:{"keywords/$flagged":true}}},"s"]`, account, emailID))
	took = one.want(`* 1 FETCH (FLAGS (\Flagged))`, 10*time.Second)
	t.Logf("flag over JMAP on node B -> FETCH in IDLE on node A after %v", took.Round(time.Millisecond))
	event("the flag", `"Email"`)

	// 3. A move over IMAP on node B.
	two.ok("CREATE Archive")
	moved := two.cmd("MOVE 1 Archive")
	if !regexp.MustCompile(`^\* OK \[COPYUID \d+ 1 1\] moved$`).MatchString(moved[0]) || !hasLine(moved, "* 1 EXPUNGE") || moved[len(moved)-1] != "OK MOVE completed" {
		t.Fatalf("MOVE: %v", moved)
	}
	took = one.want("* 1 EXPUNGE", 10*time.Second)
	t.Logf("MOVE on node B -> EXPUNGE in IDLE on node A after %v", took.Round(time.Millisecond))
	r = jb.call(fmt.Sprintf(`["Email/changes",{"accountId":%q,"sinceState":%q},"c"],
		["Email/get",{"accountId":%q,"ids":[%q],"properties":["mailboxIds","keywords"]},"g"],
		["Mailbox/get",{"accountId":%q,"properties":["name","totalEmails"]},"m"]`, account, state, account, emailID, account))
	sameJSON(t, "Email/changes after the move: the same Email, updated", map[string]any{
		"created": dig(t, r, 0, 1, "created"), "updated": dig(t, r, 0, 1, "updated"), "destroyed": dig(t, r, 0, 1, "destroyed")},
		fmt.Sprintf(`{"created":[],"updated":[%q],"destroyed":[]}`, emailID))
	archive := ""
	for _, m := range dig(t, r, 2, 1, "list").([]any) {
		if dig(t, m, "name") == "Archive" {
			archive = dig(t, m, "id").(string)
			if n := dig(t, m, "totalEmails"); n != float64(1) {
				t.Errorf("Archive has %v emails", n)
			}
		}
	}
	sameJSON(t, "the Email is in Archive, still flagged", dig(t, r, 1, 1, "list", 0),
		fmt.Sprintf(`{"id":%q,"mailboxIds":{%q:true},"keywords":{"$flagged":true}}`, emailID, archive))

	// 4. Session 1 leaves IDLE and works in Archive on node A: a flag set
	// in IMAP shows in JMAP on node B.
	one.write("DONE\r\n")
	one.want("idle1 OK IDLE completed", 10*time.Second)
	if sel := one.ok("SELECT Archive"); !hasLine(sel, "* 1 EXISTS") {
		t.Fatalf("SELECT Archive on node A: %v", sel)
	}
	if got := one.ok(`STORE 1 +FLAGS (\Seen $Forwarded)`); !hasLine(got, `* 1 FETCH (FLAGS (\Flagged \Seen $Forwarded))`) {
		t.Fatalf("STORE: %v", got)
	}
	r = jb.call(fmt.Sprintf(`["Email/get",{"accountId":%q,"ids":[%q],"properties":["keywords"]},"g"]`, account, emailID))
	sameJSON(t, "keywords in JMAP after STORE in IMAP", dig(t, r, 0, 1, "list", 0, "keywords"), `{"$flagged":true,"$seen":true,"$forwarded":true}`)

	// A copy is a second label on the same Email: JMAP shows one Email in
	// two mailboxes, IMAP a message in each folder.
	if got := one.cmd("COPY 1 INBOX"); !regexp.MustCompile(`^OK \[COPYUID \d+ 1 2\] COPY completed$`).MatchString(got[len(got)-1]) {
		t.Fatalf("COPY: %v", got)
	}
	r = jb.call(fmt.Sprintf(`["Email/query",{"accountId":%q},"q"],["Email/get",{"accountId":%q,"ids":[%q],"properties":["mailboxIds"]},"g"]`, account, account, emailID))
	if ids := dig(t, r, 0, 1, "ids").([]any); len(ids) != 1 {
		t.Fatalf("after COPY there are %d emails, want the one with two labels", len(ids))
	}
	if boxes := dig(t, r, 1, 1, "list", 0, "mailboxIds").(map[string]any); len(boxes) != 2 {
		t.Fatalf("mailboxIds after COPY: %v", boxes)
	}
	if got := two.ok("NOOP"); !hasLine(got, "* 1 EXISTS") {
		t.Fatalf("session two, INBOX after the copy: %v", got)
	}

	// 5. \Deleted and EXPUNGE in Archive: the label goes, the Email stays
	// in INBOX without the mark. Then the same in INBOX destroys it.
	state = dig(t, r, 0, 1, "queryState").(string)
	one.ok(`STORE 1 +FLAGS.SILENT (\Deleted)`)
	if got := one.ok("EXPUNGE"); !hasLine(got, "* 1 EXPUNGE") {
		t.Fatalf("EXPUNGE: %v", got)
	}
	r = jb.call(fmt.Sprintf(`["Email/get",{"accountId":%q,"ids":[%q],"properties":["mailboxIds","keywords"]},"g"]`, account, emailID))
	if boxes := dig(t, r, 0, 1, "list", 0, "mailboxIds").(map[string]any); len(boxes) != 1 || boxes[archive] != nil {
		t.Fatalf("mailboxIds after EXPUNGE in Archive: %v", boxes)
	}
	if kw := dig(t, r, 0, 1, "list", 0, "keywords").(map[string]any); kw["$deleted"] != nil {
		t.Fatalf("the Email that stays is still marked deleted: %v", kw)
	}
	two.ok("NOOP")
	two.ok(`STORE 1 +FLAGS.SILENT (\Deleted)`)
	if got := two.ok("EXPUNGE"); !hasLine(got, "* 1 EXPUNGE") {
		t.Fatalf("EXPUNGE in INBOX: %v", got)
	}
	r = jb.call(fmt.Sprintf(`["Email/changes",{"accountId":%q,"sinceState":%q},"c"],["Email/query",{"accountId":%q},"q"]`, account, state, account))
	sameJSON(t, "Email/changes after the last EXPUNGE", dig(t, r, 0, 1, "destroyed"), fmt.Sprintf(`[%q]`, emailID))
	if ids := dig(t, r, 1, 1, "ids").([]any); len(ids) != 0 {
		t.Fatalf("emails left: %v", ids)
	}
	event("the expunge", `"Email"`)

	// 6. Failed logins of mail clients are counted for all nodes together:
	// ten wrong passwords over IMAP on node A close IMAP for the address,
	// on node A at once and on node B through the shared counters, and
	// SMTP submission with them. The web login has a counter of its own
	// and stays open: a phone with a stale password must not lock its
	// owner out of the webmail.
	for i := 0; i < 10; i++ {
		c := dialIMAP(t, "bad", a.imapTLS)
		if got := c.cmd(fmt.Sprintf(`LOGIN %s wrong-%d`, anna, i)); !strings.HasPrefix(got[len(got)-1], "NO [AUTHENTICATIONFAILED]") {
			t.Fatalf("wrong password %d: %v", i, got)
		}
		c.conn.Close()
	}
	c := dialIMAP(t, "limited", a.imapTLS)
	if got := c.cmd(fmt.Sprintf(`LOGIN %s "correct horse"`, anna)); !strings.HasPrefix(got[len(got)-1], "NO [UNAVAILABLE] too many failed logins") {
		t.Fatalf("the right password after ten wrong ones: %v", got)
	}
	// Node B saw none of the failures. The tenth was written before node
	// A answered it, so node B knows without a wait: no retry here.
	cb := dialIMAP(t, "limitedB", b.imapTLS)
	if got := cb.cmd(fmt.Sprintf(`LOGIN %s "correct horse"`, anna)); !strings.HasPrefix(got[len(got)-1], "NO [UNAVAILABLE]") {
		t.Fatalf("node B after ten failed logins on node A: %v", got)
	}
	web := newJMAPClient(t, "http://"+b.web)
	if resp, data := web.do("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":"correct horse"}`, anna), nil); resp.StatusCode != 200 {
		t.Fatalf("web login on node B after ten failed IMAP logins: %d %s", resp.StatusCode, data)
	}
	// The sessions that were logged in are not thrown out.
	one.ok("NOOP")
}
