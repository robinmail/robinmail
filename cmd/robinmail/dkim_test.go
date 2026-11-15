package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/queue"
	"github.com/robinmail/robinmail/internal/smtp"
)

// catcher is a mail server that keeps what it is given.
type catcher struct {
	mu   sync.Mutex
	msgs [][]byte
}

func (c *catcher) Rcpt(context.Context, *smtp.Envelope, string) error { return nil }
func (c *catcher) Deliver(_ context.Context, _ *smtp.Envelope, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, bytes.Clone(msg))
	return nil
}

func (c *catcher) got() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.msgs...)
}

func startCatcher(t *testing.T) (*catcher, string) {
	c := &catcher{}
	srv := &smtp.Server{Hostname: "mx.elsewhere.test", Handler: c}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return c, l.Addr().String()
}

var wsp = regexp.MustCompile(`[ \t]+`)

// relaxedHeader is the "relaxed" header canonicalization of RFC 6376
// 3.4.2: the name in lowercase, the value unfolded, runs of white space
// one space, none at the ends or around the colon.
func relaxedHeader(field string) string {
	name, value, _ := strings.Cut(field, ":")
	value = strings.NewReplacer("\r\n", "").Replace(value)
	return strings.ToLower(strings.TrimSpace(name)) + ":" + strings.TrimSpace(wsp.ReplaceAllString(value, " ")) + "\r\n"
}

// relaxedBody is the "relaxed" body canonicalization of RFC 6376 3.4.4:
// no white space at the end of a line, runs of it one space, no empty
// lines at the end.
func relaxedBody(body string) string {
	lines := strings.Split(body, "\r\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(wsp.ReplaceAllString(l, " "), " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

// verifyDKIM checks the first DKIM-Signature of a message against a public
// key, by RFC 6376 3.7 and 6.1.3, for rsa-sha256 and relaxed/relaxed. It
// returns the tags of the signature.
func verifyDKIM(t *testing.T, msg []byte, pub *rsa.PublicKey) map[string]string {
	t.Helper()
	head, body, _ := strings.Cut(string(msg), "\r\n\r\n")
	var fields []string
	for _, line := range strings.SplitAfter(head+"\r\n", "\r\n") {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(fields) > 0 {
			fields[len(fields)-1] += line
		} else {
			fields = append(fields, line)
		}
	}
	sigField := ""
	for _, f := range fields {
		if strings.HasPrefix(strings.ToLower(f), "dkim-signature:") {
			sigField = strings.TrimSuffix(f, "\r\n")
			break
		}
	}
	if sigField == "" {
		t.Fatalf("no DKIM-Signature in:\n%s", head)
	}
	tags := map[string]string{}
	_, value, _ := strings.Cut(sigField, ":")
	for _, tag := range strings.Split(value, ";") {
		if k, v, ok := strings.Cut(tag, "="); ok {
			tags[strings.TrimSpace(k)] = strings.Join(strings.Fields(v), "")
		}
	}
	if tags["a"] != "rsa-sha256" || tags["c"] != "relaxed/relaxed" {
		t.Fatalf("signature with a=%s c=%s; this check knows rsa-sha256 and relaxed/relaxed", tags["a"], tags["c"])
	}
	// The body hash.
	bh := sha256.Sum256([]byte(relaxedBody(body)))
	if got := base64.StdEncoding.EncodeToString(bh[:]); got != tags["bh"] {
		t.Errorf("DKIM body hash: the body as received hashes to %s, the signature says %s", got, tags["bh"])
	}
	// The header hash: the fields of h=, each taken from the bottom up,
	// then the signature field itself with an empty b= and no line end.
	h := sha256.New()
	used := map[int]bool{}
	for _, name := range strings.Split(tags["h"], ":") {
		for i := len(fields) - 1; i >= 0; i-- {
			if n, _, _ := strings.Cut(fields[i], ":"); !used[i] && strings.EqualFold(strings.TrimSpace(n), name) {
				used[i] = true
				h.Write([]byte(relaxedHeader(fields[i])))
				break
			}
		}
	}
	unsigned := regexp.MustCompile(`(?s)([; \t]b=)[^;]*`).ReplaceAllString(sigField, "$1")
	h.Write([]byte(strings.TrimSuffix(relaxedHeader(unsigned), "\r\n")))
	sig, err := base64.StdEncoding.DecodeString(tags["b"])
	if err != nil {
		t.Fatalf("b= is not base64: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, h.Sum(nil), sig); err != nil {
		t.Errorf("the DKIM signature does not verify: %v\nheader:\n%s", err, head)
	}
	return tags
}

// TestOutgoingMailIsSignedByRspamd sends a message through a real Rspamd
// with contrib/rspamd/local.d/dkim_signing.conf and a key made by
// "robinmail dns", catches it at the receiving end, and verifies the
// DKIM-Signature against the public key that command printed for DNS. So
// it also shows that nothing robinmail does after the scan (its Received
// line, the queue, SMTP) breaks the signature. It needs rqlite, and the
// Rspamd of contrib/compose with its DKIM key directory:
//
//	docker compose -f contrib/compose/compose.yaml up -d
//	ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 ROBINMAIL_TEST_RSPAMD=http://127.0.0.1:11333 \
//	ROBINMAIL_TEST_DKIM_DIR=$PWD/contrib/compose/rspamd/dkim go test -v -run TestOutgoingMailIsSignedByRspamd ./cmd/robinmail
func TestOutgoingMailIsSignedByRspamd(t *testing.T) {
	base, scanner, keyDir := os.Getenv("ROBINMAIL_TEST_RQLITE"), os.Getenv("ROBINMAIL_TEST_RSPAMD"), os.Getenv("ROBINMAIL_TEST_DKIM_DIR")
	if base == "" || scanner == "" || keyDir == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE, ROBINMAIL_TEST_RSPAMD and ROBINMAIL_TEST_DKIM_DIR are not all set")
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	if resp, err := hc.Get(scanner + "/ping"); err != nil {
		t.Skipf("Rspamd is not reachable: %v", err)
	} else {
		resp.Body.Close()
	}
	world := newSendNet()
	a := startInstanceWith(t, base, "dkim", fmt.Sprintf(`"rspamd":{"url":%q,"timeout_seconds":60},`, scanner), func(w *queue.Worker, _ *instance) {
		w.Resolver, w.Dial = world, world.dial
	})
	caught, addr := startCatcher(t)
	world.host("elsewhere.test", "mx.elsewhere.test", "192.0.2.20", addr)

	// The documented step: one command makes the key and prints the
	// records. The key goes where the Rspamd of contrib/compose reads it.
	keyFile := filepath.Join(keyDir, a.domain+".robinmail.key")
	t.Cleanup(func() { os.Remove(keyFile) })
	records := a.ok("", "dns", a.domain, keyFile)
	t.Logf("robinmail dns %s %s\n%s", a.domain, keyFile, records)
	a.expect(records,
		`(?m)^`+regexp.QuoteMeta(a.domain)+`\.\tMX\t10 `+regexp.QuoteMeta(a.host)+`\.$`,
		`(?m)^`+regexp.QuoteMeta(a.domain)+`\.\tTXT\t"v=spf1 a:`+regexp.QuoteMeta(a.host)+` -all"$`,
		`(?m)^robinmail\._domainkey\.`+regexp.QuoteMeta(a.domain)+`\.\tTXT\t"v=DKIM1; k=rsa; p=[A-Za-z0-9+/=" ]+"$`,
		`(?m)^_dmarc\.`+regexp.QuoteMeta(a.domain)+`\.\tTXT\t"v=DMARC1; p=quarantine; rua=mailto:postmaster@`,
		`(?m)^_mta-sts\.`+regexp.QuoteMeta(a.domain)+`\.\tTXT\t"v=STSv1; id=\d{14}"$`,
		`wrote a new key to `)
	// Run again: the same key, not a new one.
	again := a.ok("", "dns", a.domain, keyFile)
	p := regexp.MustCompile(`v=DKIM1; k=rsa; p=([A-Za-z0-9+/=" ]+)"`)
	if strings.Contains(again, "wrote a new key") || p.FindStringSubmatch(again)[1] != p.FindStringSubmatch(records)[1] {
		t.Error("a second run replaced the key")
	}
	der, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(p.FindStringSubmatch(records)[1], `" "`, ""))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatal(err)
	}
	pub := parsed.(*rsa.PublicKey)
	if pub.N.BitLen() != 2048 {
		t.Errorf("a key of %d bits", pub.N.BitLen())
	}
	// Rspamd runs as another user and reads the key through the mount.
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}

	anna := a.user("anna")
	ja, acct := jmapSession(t, a, anna)
	submit := func(subject, body string) []any {
		return ja.call(`["Email/set", {"accountId": "` + acct + `", "create": {"d": {"mailboxIds": {"m3": true},
				"from": [{"name": "Anna de Vries", "email": "` + anna + `"}], "to": [{"name": "Zoë", "email": "zoe@elsewhere.test"}],
				"subject": "` + subject + `", "textBody": [{"partId": "t", "type": "text/plain"}],
				"bodyValues": {"t": {"value": "` + body + `"}}}}}, "0"],
			["EmailSubmission/set", {"accountId": "` + acct + `", "create": {"s": {"identityId": "i1", "emailId": "#d"}}}, "1"]`)
	}
	res := submit("Getekend: één bericht met een lange onderwerpregel die gevouwen wordt", `Hallo Zoë,\n\ndit bericht is ondertekend.   \n\n\n`)
	if dig(t, res, 1, 1, "created") == nil {
		t.Fatalf("not submitted: %v", res)
	}
	waitFor(t, "the signed message at the receiving server", func() bool { return len(caught.got()) == 1 })
	msg := caught.got()[0]
	t.Logf("as received:\n%s", strings.ReplaceAll(string(msg), "\r\n", "\n"))
	tags := verifyDKIM(t, msg, pub)
	if tags["d"] != a.domain || tags["s"] != "robinmail" {
		t.Errorf("signed as d=%s s=%s, want d=%s s=robinmail", tags["d"], tags["s"], a.domain)
	}
	// RFC 6376 5.4: From must be signed; the fields a reader sees should be.
	for _, name := range []string{"from", "to", "subject", "date", "message-id"} {
		if !strings.Contains(":"+strings.ToLower(tags["h"])+":", ":"+name+":") {
			t.Errorf("%s is not among the signed fields: %s", name, tags["h"])
		}
	}

	// What Rspamd rejects is not sent: its test pattern for "reject".
	res = submit("Buy now", gtube)
	if got := dig(t, res, 1, 1, "notCreated", "s", "type"); got != "forbiddenToSend" {
		t.Errorf("GTUBE in outgoing mail: %v", dig(t, res, 1, 1))
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(caught.got()); n != 1 {
		t.Errorf("%d messages arrived, want only the first", n)
	}
}
