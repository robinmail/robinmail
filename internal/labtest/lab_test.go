package labtest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/message"
)

const (
	utf8Subject = "Grüße aus dem Labor: 日本語 ✓"
	utf8Name    = "Alíce Müller"
	utf8Body    = "Grüße aus dem Labor.\nΚαλημέρα κόσμε, 日本語, €.\n"
	mime8bit    = "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit"
)

// dotBody has every line a careless dot-stuffer gets wrong (RFC 5321
// 4.5.2): a line that is one dot, lines that start with one, two and three.
const dotBody = "first\n.\n..\n...\n.leading dot\n. \n .\nmiddle\n.\nlast line is a dot\n.\n"

func mimeHeaders(lines ...string) []string {
	return append(strings.Split(mime8bit, "\r\n"), lines...)
}

// parseDSN reads a notification with robinmail's own parser.
func parseDSN(t testing.TB, raw []byte) *message.DSN {
	t.Helper()
	d, ok := message.ParseDSN(message.Parse(crlf(raw)))
	if !ok {
		t.Fatalf("not a delivery status notification (RFC 3464): Content-Type %q", header(raw, "Content-Type"))
	}
	return d
}

// peerDSN parses a notification a peer wrote and notes in the matrix
// whether it is the multipart/report RFC 3464 asks for.
func peerDSN(t testing.TB, p peer, raw []byte) *message.DSN {
	t.Helper()
	ct := strings.Join(header(raw, "Content-Type"), " ")
	observe("in 95 peer's bounce is multipart/report (RFC 3464 2)", p.name, yesNo(strings.Contains(strings.ToLower(ct), "multipart/report")))
	return parseDSN(t, raw)
}

// failedRecipient checks that the DSN reports one address as failed with a
// status of the given class and returns that recipient.
func failedRecipient(t testing.TB, d *message.DSN, addr, class string) message.DSNRecipient {
	t.Helper()
	for _, r := range d.Recipients {
		if strings.EqualFold(r.Address, addr) {
			if r.Action != "failed" || !strings.HasPrefix(r.Status, class) {
				t.Errorf("DSN for %s: action %q status %q, want failed and %s", addr, r.Action, r.Status, class)
			}
			return r
		}
	}
	t.Errorf("the DSN does not report %s: %+v", addr, d.Recipients)
	return message.DSNRecipient{}
}

// received returns the Received lines, newest first.
func received(raw []byte) []string { return header(raw, "Received") }

// ---- the peers send to robinmail ----

func TestInbound(t *testing.T) {
	lab(t)
	alice := newJMAP(t, "alice@robin.test")
	bob := newJMAP(t, "bob@robin.test")
	to := "alice@" + robinDomain
	for _, p := range peers {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			from := "alice@" + p.domain
			// mailLine is the MAIL command the peer sent for a message.
			mailLine := func(t *testing.T, since time.Time, sender string) traceLine {
				t.Helper()
				cmds := commands(trace(t, since.Add(-2*time.Second)), "server", p.ip, "MAIL FROM:<"+sender+">")
				if len(cmds) == 0 {
					t.Fatalf("robinmail's trace has no MAIL FROM:<%s> from %s", sender, p.ip)
				}
				return cmds[len(cmds)-1]
			}

			t.Run("ascii", func(t *testing.T) {
				cell(t, "in 01 plain ASCII", p.name)
				tok, start := newToken(), time.Now()
				text := "Hello from " + p.name + ".\nSecond line.\n"
				p.send(t, from, []string{to}, rawMessage(tok, "Alice <"+from+">", to, nil, text))
				id, raw := alice.find(t, tok)
				if body(raw) != text {
					t.Errorf("body %q, want %q", body(raw), text)
				}
				// RFC 5321 4.4: the final server adds one Return-Path, at
				// the top, and its Received line.
				if rp := header(raw, "Return-Path"); len(rp) != 1 || rp[0] != "<"+from+">" || !bytes.HasPrefix(raw, []byte("Return-Path:")) {
					t.Errorf("Return-Path %q, want one <%s> at the top", rp, from)
				}
				rec := received(raw)
				if len(rec) < 2 || !strings.Contains(rec[0], "by "+robinHost) || !strings.Contains(rec[0], p.host) {
					t.Errorf("Received lines: %q", rec)
				}
				t.Logf("robinmail's Received: %s", rec[0])
				em := alice.get(t, id, "from", "subject")
				if em["subject"] != "lab "+tok {
					t.Errorf("subject %q", em["subject"])
				}
				m := mailLine(t, start, from)
				t.Logf("MAIL as %s sends it: %q (pipelined: %v)", p.name, m.line, m.more)
				observe("in 90 peer declares SIZE", p.name, yesNo(strings.Contains(m.line, " SIZE=")))
				observe("in 91 peer pipelines MAIL, RCPT, DATA", p.name, yesNo(m.more))
			})

			t.Run("tls", func(t *testing.T) {
				// The peer is set to send to robin.test only over TLS
				// with a certificate the lab CA signed for mx.robin.test:
				// the message arriving is the proof. robinmail's Received
				// line says what was negotiated.
				cell(t, "in 02 STARTTLS, certificate checked against the lab CA", p.name)
				tok := newToken()
				p.send(t, from, []string{to}, rawMessage(tok, from, to, nil, "tls\n"))
				_, raw := alice.find(t, tok)
				if rec := received(raw); len(rec) == 0 || !strings.Contains(rec[0], "TLS") || !strings.Contains(rec[0], "ESMTPS") {
					t.Errorf("robinmail's Received line does not show TLS: %q", rec)
				}
			})

			t.Run("8bit", func(t *testing.T) {
				cell(t, "in 03 8-bit body, UTF-8", p.name)
				tok, start := newToken(), time.Now()
				p.send(t, from, []string{to}, rawMessage(tok, from, to, mimeHeaders(), utf8Body))
				id, raw := alice.find(t, tok)
				if body(raw) != utf8Body {
					t.Errorf("body %q, want %q", body(raw), utf8Body)
				}
				res := alice.call(t, []any{"Email/get", map[string]any{"accountId": alice.account, "ids": []string{id},
					"properties": []string{"textBody", "bodyValues"}, "fetchTextBodyValues": true}, "0"})
				vals, _ := res[0].args["list"].([]any)[0].(map[string]any)["bodyValues"].(map[string]any)
				for _, v := range vals {
					if got := v.(map[string]any)["value"]; got != utf8Body {
						t.Errorf("JMAP body value %q, want %q", got, utf8Body)
					}
				}
				m := mailLine(t, start, from)
				t.Logf("MAIL: %q", m.line)
				observe("in 92 peer declares BODY=8BITMIME", p.name, yesNo(strings.Contains(strings.ToUpper(m.line), "BODY=8BITMIME")))
			})

			t.Run("latin1", func(t *testing.T) {
				cell(t, "in 04 8-bit body, ISO-8859-1", p.name)
				tok := newToken()
				text := "Gr\xfc\xdfe, \xe9t\xe9.\n"
				h := []string{"MIME-Version: 1.0", "Content-Type: text/plain; charset=iso-8859-1", "Content-Transfer-Encoding: 8bit"}
				p.send(t, from, []string{to}, rawMessage(tok, from, to, h, text))
				id, raw := alice.find(t, tok)
				if body(raw) != text {
					t.Errorf("body %q, want %q", body(raw), text)
				}
				res := alice.call(t, []any{"Email/get", map[string]any{"accountId": alice.account, "ids": []string{id},
					"properties": []string{"bodyValues"}, "fetchTextBodyValues": true}, "0"})
				vals, _ := res[0].args["list"].([]any)[0].(map[string]any)["bodyValues"].(map[string]any)
				for _, v := range vals {
					if got := v.(map[string]any)["value"]; got != "Grüße, été.\n" {
						t.Errorf("JMAP body value %q", got)
					}
				}
			})

			t.Run("encoded headers", func(t *testing.T) {
				cell(t, "in 05 non-ASCII headers as encoded words", p.name)
				tok := newToken()
				h := []string{"Subject: " + mime.BEncoding.Encode("utf-8", utf8Subject)}
				p.send(t, from, []string{to}, rawMessage(tok, mime.QEncoding.Encode("utf-8", utf8Name)+" <"+from+">", to, h, "x\n"))
				id, _ := alice.find(t, tok)
				em := alice.get(t, id, "from", "subject")
				if em["subject"] != utf8Subject {
					t.Errorf("subject %q, want %q", em["subject"], utf8Subject)
				}
				if f := em["from"].([]any)[0].(map[string]any); f["name"] != utf8Name || f["email"] != from {
					t.Errorf("from %v", f)
				}
			})

			t.Run("utf8 headers", func(t *testing.T) {
				// A message with UTF-8 in its header is an RFC 6532
				// message and needs SMTPUTF8 on the way.
				cell(t, "in 06 UTF-8 headers as they are (RFC 6532)", p.name)
				tok, start := newToken(), time.Now()
				p.send(t, from, []string{to}, rawMessage(tok, utf8Name+" <"+from+">", to, mimeHeaders("Subject: "+utf8Subject), utf8Body))
				id, raw := alice.find(t, tok)
				em := alice.get(t, id, "from", "subject")
				if em["subject"] != utf8Subject {
					t.Errorf("subject %q, want %q (raw %q)", em["subject"], utf8Subject, header(raw, "Subject"))
				}
				if f := em["from"].([]any)[0].(map[string]any); f["name"] != utf8Name {
					t.Errorf("from %v (raw %q)", f, header(raw, "From"))
				}
				m := mailLine(t, start, from)
				t.Logf("MAIL: %q", m.line)
				observe("in 93 peer asks SMTPUTF8 for UTF-8 headers", p.name, yesNo(strings.Contains(strings.ToUpper(m.line), "SMTPUTF8")))
			})

			t.Run("utf8 sender", func(t *testing.T) {
				cell(t, "in 07 SMTPUTF8 sender address", p.name)
				if !p.smtputf8 {
					t.Skip(p.name + " has no SMTPUTF8")
				}
				tok, start := newToken(), time.Now()
				sender := "jörg@" + p.domain
				msg := rawMessage(tok, sender, to, mimeHeaders(), utf8Body)
				if p.name == "exim" {
					// Exim's command line takes no such sender; its
					// SMTP on standard input does.
					script := "EHLO localhost\nMAIL FROM:<" + sender + "> SMTPUTF8\nRCPT TO:<" + to + ">\nDATA\n" + string(lf(msg)) + ".\nQUIT\n"
					out, err := execIn(p.name, "alice", []byte(script), "exim4", "-bs")
					if err != nil || !strings.Contains(out, "250 OK id=") {
						t.Fatalf("exim -bs: %v\n%s", err, out)
					}
				} else {
					p.send(t, sender, []string{to}, msg)
				}
				id, raw := alice.find(t, tok)
				if rp := header(raw, "Return-Path"); len(rp) != 1 || rp[0] != "<"+sender+">" {
					t.Errorf("Return-Path %q, want <%s>", rp, sender)
				}
				em := alice.get(t, id, "from")
				if f := em["from"].([]any)[0].(map[string]any); f["email"] != sender {
					t.Errorf("from %v", f)
				}
				m := mailLine(t, start, sender)
				if !strings.Contains(strings.ToUpper(m.line), "SMTPUTF8") {
					t.Errorf("MAIL without SMTPUTF8: %q", m.line)
				}
			})

			t.Run("near limit", func(t *testing.T) {
				cell(t, "in 08 a message just under robinmail's size limit", p.name)
				tok := newToken()
				text := filler(robinMax - 8000)
				p.send(t, from, []string{to}, rawMessage(tok, from, to, nil, text))
				_, raw := alice.find(t, tok)
				if body(raw) != text {
					t.Errorf("the body changed: %d bytes, want %d", len(body(raw)), len(text))
				}
				t.Logf("stored %d bytes; the limit is %d", len(raw), robinMax)
			})

			t.Run("over limit", func(t *testing.T) {
				cell(t, "in 09 a message over robinmail's size limit: refused, peer bounces", p.name)
				tok, start := newToken(), time.Now()
				p.send(t, from, []string{to}, rawMessage(tok, from, to, nil, filler(robinMax+200000)))
				// The sender on the peer gets the notification.
				bounce := p.find(t, "alice", tok)
				r := failedRecipient(t, peerDSN(t, p, bounce), to, "5.")
				t.Logf("%s reports: status %s, %q", p.name, r.Status, r.Diagnostic)
				alice.absent(t, tok, time.Second)
				var how []string
				for _, l := range trace(t, start.Add(-2*time.Second)) {
					if l.side == "server" && l.remote == p.ip && (strings.HasPrefix(l.line, "552") || strings.HasPrefix(l.line, "MAIL")) {
						how = append(how, l.line)
					}
				}
				t.Logf("on the wire: %q", how)
				offered := false
				for _, h := range how {
					offered = offered || strings.HasPrefix(h, "552")
				}
				observe("in 94 peer offers a message over our SIZE limit", p.name, yesNo(offered))
			})

			t.Run("recipients", func(t *testing.T) {
				cell(t, "in 10 three recipients, one unknown: two delivered, RFC 3464 bounce for the third", p.name)
				tok := newToken()
				unknown := "nosuch@" + robinDomain
				p.send(t, from, []string{to, "bob@" + robinDomain, unknown}, rawMessage(tok, from, to, nil, "three\n"))
				alice.find(t, tok)
				bob.find(t, tok)
				bounce := p.find(t, "alice", tok)
				d := peerDSN(t, p, bounce)
				// Exim and OpenSMTPD say 5.0.0 whatever the reply was.
				r := failedRecipient(t, d, unknown, "5.")
				if !bytes.Contains(bounce, []byte("550 5.1.1 No such user")) {
					t.Errorf("the notification does not carry robinmail's reply:\n%s", clip(string(bounce), 2000))
				}
				if len(d.Recipients) != 1 {
					t.Errorf("recipients in the notification: %+v", d.Recipients)
				}
				t.Logf("%s: Reporting-MTA %q, status %s, diagnostic %q, original Message-ID %q", p.name, d.ReportingMTA, r.Status, r.Diagnostic, d.OriginalMessageID)
			})

			t.Run("long line", func(t *testing.T) {
				cell(t, "in 11 a line of 998 characters", p.name)
				tok := newToken()
				text := "short\n" + strings.Repeat("x", 998) + "\nshort\n"
				p.send(t, from, []string{to}, rawMessage(tok, from, to, nil, text))
				_, raw := alice.find(t, tok)
				if body(raw) != text {
					// OpenSMTPD's sendmail turns such a message into
					// quoted-printable before it is ever sent.
					if _, _, dec := decoded(t, raw); dec != text {
						t.Errorf("the body changed: %q", clip(body(raw), 200))
					}
				}
			})

			t.Run("too long line", func(t *testing.T) {
				// Over RFC 5322's 998: what the peer does with it is its
				// own business; robinmail must take what comes.
				note := cell(t, "in 12 a line of 3000 characters (over RFC 5322's limit)", p.name)
				tok := newToken()
				text := "short\n" + strings.Repeat("y", 3000) + "\nshort\n"
				if out, err := p.sendErr(from, []string{to}, rawMessage(tok, from, to, nil, text)); err != nil {
					note("peer refuses")
					t.Logf("%s refuses it locally: %s", p.name, clip(out, 300))
					return
				}
				var raw []byte
				where := ""
				waitFor(t, "the message or its bounce", 60*time.Second, func() bool {
					var ok bool
					if _, raw, ok = alice.lookup(t, "inbox", tok); ok {
						where = "robinmail"
						return true
					}
					if raw, _, ok = maildirLookup(p.name, "alice", tok); ok {
						where = "bounce"
					}
					return ok
				})
				switch {
				case where == "bounce":
					note("peer bounces")
					t.Logf("%s bounced it: %s", p.name, clip(body(raw), 600))
				case body(raw) == text:
					note("kept")
				default:
					longest := 0
					for _, l := range strings.Split(body(raw), "\n") {
						longest = max(longest, len(l))
					}
					note("peer folds")
					t.Logf("%s changed the line: the longest is now %d", p.name, longest)
					if strings.Count(body(raw), "y") != 3000 {
						t.Errorf("content was lost: %d y", strings.Count(body(raw), "y"))
					}
				}
			})

			t.Run("dots", func(t *testing.T) {
				cell(t, "in 13 lines that start with a dot", p.name)
				tok := newToken()
				p.send(t, from, []string{to}, rawMessage(tok, from, to, nil, dotBody))
				_, raw := alice.find(t, tok)
				if body(raw) != dotBody {
					t.Errorf("body %q, want %q", body(raw), dotBody)
				}
			})
		})
	}
}

// ---- robinmail sends to the peers ----

// textCreate is the Email/set create object of a plain message.
func textCreate(tok, from, to, subject, text string) map[string]any {
	return map[string]any{
		"from":                []any{map[string]any{"email": from}},
		"to":                  []any{map[string]any{"email": to}},
		"subject":             subject,
		"header:X-Lab:asText": tok,
		"bodyValues":          map[string]any{"t": map[string]any{"value": text}},
		"textBody":            []any{map[string]any{"partId": "t", "type": "text/plain"}},
	}
}

// decoded reads a message with the standard library: its subject, the
// display name of From and its first text/plain body, all decoded.
func decoded(t testing.TB, raw []byte) (subject, fromName, text string) {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("net/mail: %v", err)
	}
	dec := new(mime.WordDecoder)
	subject, err = dec.DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		t.Errorf("Subject: %v", err)
	}
	if a, err := m.Header.AddressList("From"); err == nil && len(a) > 0 {
		fromName = a[0].Name
	} else {
		t.Errorf("From %q: %v", m.Header.Get("From"), err)
	}
	var walk func(ctype, cte string, r io.Reader) (string, bool)
	walk = func(ctype, cte string, r io.Reader) (string, bool) {
		mt, params, _ := mime.ParseMediaType(ctype)
		switch strings.ToLower(cte) {
		case "quoted-printable":
			r = quotedprintable.NewReader(r)
		case "base64":
			r = base64.NewDecoder(base64.StdEncoding, r)
		}
		if strings.HasPrefix(mt, "multipart/") {
			mr := multipart.NewReader(r, params["boundary"])
			for {
				part, err := mr.NextRawPart()
				if err != nil {
					return "", false
				}
				if s, ok := walk(part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"), part); ok {
					return s, true
				}
			}
		}
		if mt == "text/plain" || mt == "" {
			b, _ := io.ReadAll(r)
			return string(b), true
		}
		return "", false
	}
	text, _ = walk(m.Header.Get("Content-Type"), m.Header.Get("Content-Transfer-Encoding"), m.Body)
	return subject, fromName, strings.ReplaceAll(text, "\r\n", "\n")
}

func TestOutbound(t *testing.T) {
	lab(t)
	alice := newJMAP(t, "alice@robin.test")
	from := alice.user
	for _, p := range peers {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			to := "alice@" + p.domain
			// sent is what robinmail's client said to the peer for a
			// message: its commands since a moment.
			sent := func(t *testing.T, since time.Time, verb string) []traceLine {
				t.Helper()
				return commands(trace(t, since.Add(-2*time.Second)), "client", p.ip, verb)
			}
			// bounced waits for robinmail's own notification about a
			// message in the sender's Inbox.
			bounced := func(t *testing.T, tok, rcpt, class string) message.DSNRecipient {
				t.Helper()
				_, raw := alice.find(t, tok)
				if rp := header(raw, "Return-Path"); len(rp) != 1 || rp[0] != "<>" {
					t.Errorf("Return-Path of the notification: %q", rp)
				}
				return failedRecipient(t, parseDSN(t, raw), rcpt, class)
			}

			t.Run("ascii", func(t *testing.T) {
				cell(t, "out 01 plain ASCII", p.name)
				tok := newToken()
				text := "Hello " + p.name + ".\nSecond line.\n"
				sub := alice.sendCreate(t, textCreate(tok, from, to, "lab "+tok, text), to)
				raw := p.find(t, "alice", tok)
				if body(raw) != text {
					t.Errorf("body %q, want %q", body(raw), text)
				}
				st := alice.waitStatus(t, sub, to, "unknown", "yes", "no")
				if st.delivered != "unknown" || !strings.HasPrefix(st.reply, "250") {
					t.Errorf("deliveryStatus %+v, want unknown with the peer's 250", st)
				}
				// How the peer treats our mail (RFC 5321 4.4): the
				// envelope sender in one Return-Path, its own Received
				// line above ours.
				if rp := header(raw, "Return-Path"); len(rp) != 1 || rp[0] != "<"+from+">" {
					t.Errorf("Return-Path at %s: %q", p.name, rp)
				}
				rec := received(raw)
				if len(rec) != 2 || !strings.Contains(rec[0], robinHost) || !strings.Contains(rec[0], p.host) ||
					!strings.Contains(rec[1], "by "+robinHost+" (robinmail)") {
					t.Errorf("Received lines at %s: %q", p.name, rec)
				}
				for _, r := range rec {
					t.Logf("Received: %s", r)
				}
				if h := header(raw, "Bcc"); len(h) != 0 {
					t.Errorf("Bcc reached the peer: %q", h)
				}
			})

			t.Run("tls", func(t *testing.T) {
				// postfix.test and exim.test publish an MTA-STS policy in
				// mode enforce: robinmail delivers only after checking
				// the certificate against the lab CA. opensmtpd.test is
				// in mode testing: the same check, logged.
				cell(t, "out 02 STARTTLS, certificate checked against the lab CA (MTA-STS)", p.name)
				tok, start := newToken(), time.Now()
				alice.sendCreate(t, textCreate(tok, from, to, "lab "+tok, "tls\n"), to)
				raw := p.find(t, "alice", tok)
				if rec := received(raw); len(rec) == 0 || !strings.Contains(rec[0], "TLS") {
					t.Errorf("%s's Received line does not show TLS: %q", p.name, rec)
				}
				logs := compose(t, "logs", "--no-log-prefix", "--since", start.Add(-2*time.Second).UTC().Format(time.RFC3339), "robinmail")
				if strings.Contains(logs, "would not pass") || strings.Contains(logs, "is not in the policy") {
					t.Errorf("robinmail logged an MTA-STS complaint:\n%s", logs)
				}
			})

			t.Run("8bit", func(t *testing.T) {
				cell(t, "out 03 8-bit body, sent as BODY=8BITMIME", p.name)
				tok, start := newToken(), time.Now()
				alice.sendRaw(t, rawMessage(tok, from, to, mimeHeaders(), utf8Body), to)
				raw := p.find(t, "alice", tok)
				if body(raw) != utf8Body {
					t.Errorf("body %q, want %q", body(raw), utf8Body)
				}
				m := sent(t, start, "MAIL")
				if len(m) == 0 || !strings.Contains(m[len(m)-1].line, "BODY=8BITMIME") || !strings.Contains(m[len(m)-1].line, "SIZE=") {
					t.Errorf("MAIL as robinmail sent it: %v", m)
				}
			})

			t.Run("webmail utf8", func(t *testing.T) {
				cell(t, "out 04 UTF-8 body and non-ASCII headers built by Email/set", p.name)
				tok := newToken()
				create := textCreate(tok, from, to, utf8Subject, utf8Body)
				create["from"] = []any{map[string]any{"name": utf8Name, "email": from}}
				alice.sendCreate(t, create, to)
				raw := p.find(t, "alice", tok)
				for _, c := range raw {
					if c >= 0x80 {
						t.Fatalf("the message is not 7-bit:\n%s", clip(string(raw), 1500))
					}
				}
				subject, name, text := decoded(t, raw)
				if subject != utf8Subject || name != utf8Name || text != utf8Body {
					t.Errorf("decoded with the standard library: subject %q, name %q, body %q", subject, name, text)
				}
			})

			t.Run("utf8 headers", func(t *testing.T) {
				cell(t, "out 05 UTF-8 headers as they are (needs SMTPUTF8)", p.name)
				tok, start := newToken(), time.Now()
				raw := rawMessage(tok, utf8Name+" <"+from+">", to, mimeHeaders("Subject: "+utf8Subject), utf8Body)
				sub := alice.sendRaw(t, raw, to)
				if !p.smtputf8 {
					// RFC 6531 3.2: without the extension such a
					// message cannot go; the sender is told.
					st := alice.waitStatus(t, sub, to, "no", "unknown", "yes")
					if st.delivered != "no" || !strings.Contains(st.reply, "5.6.7") {
						t.Errorf("deliveryStatus %+v, want no with 5.6.7", st)
					}
					bounced(t, tok, to, "5.6.7")
					maildirAbsent(t, p.name, "alice", tok, 0)
					return
				}
				got := p.find(t, "alice", tok)
				if h := header(got, "Subject"); len(h) != 1 || h[0] != utf8Subject {
					t.Errorf("Subject at %s: %q", p.name, h)
				}
				m := sent(t, start, "MAIL")
				if len(m) == 0 || !strings.Contains(m[len(m)-1].line, "SMTPUTF8") {
					t.Errorf("MAIL as robinmail sent it: %v", m)
				}
			})

			t.Run("utf8 recipient", func(t *testing.T) {
				cell(t, "out 06 SMTPUTF8 recipient address", p.name)
				tok, start := newToken(), time.Now()
				rcpt := "jörg@" + p.domain
				sub := alice.sendCreate(t, textCreate(tok, from, rcpt, "lab "+tok, "to jörg\n"), rcpt)
				if !p.smtputf8 {
					st := alice.waitStatus(t, sub, rcpt, "no", "unknown", "yes")
					if st.delivered != "no" || !strings.Contains(st.reply, "5.6.7") {
						t.Errorf("deliveryStatus %+v, want no with 5.6.7", st)
					}
					bounced(t, tok, rcpt, "5.6.7")
					return
				}
				// jörg is an alias of alice on the peer.
				p.find(t, "alice", tok)
				if r := sent(t, start, "RCPT TO:<"+rcpt+">"); len(r) == 0 {
					t.Errorf("no RCPT TO:<%s> in the trace", rcpt)
				}
				if m := sent(t, start, "MAIL"); len(m) == 0 || !strings.Contains(m[len(m)-1].line, "SMTPUTF8") {
					t.Errorf("MAIL as robinmail sent it: %v", m)
				}
				if st := alice.waitStatus(t, sub, rcpt, "unknown", "no"); st.delivered != "unknown" {
					t.Errorf("deliveryStatus %+v", st)
				}
			})

			t.Run("near limit", func(t *testing.T) {
				cell(t, "out 07 a message just under the peer's size limit", p.name)
				tok := newToken()
				limit := robinMax
				if p.sizeOut {
					limit = peerMax
				}
				text := filler(limit - 8000)
				alice.sendRaw(t, rawMessage(tok, from, to, nil, text), to)
				raw := p.find(t, "alice", tok)
				if body(raw) != text {
					t.Errorf("the body changed: %d bytes, want %d", len(body(raw)), len(text))
				}
			})

			t.Run("over limit", func(t *testing.T) {
				cell(t, "out 08 a message over the peer's SIZE: not offered, bounced with 5.3.4", p.name)
				if !p.sizeOut {
					t.Skip(p.name + " has one limit for its listener and its local users, which cannot be both under and over robinmail's")
				}
				tok, start := newToken(), time.Now()
				sub := alice.sendRaw(t, rawMessage(tok, from, to, nil, filler(peerMax+100000)), to)
				st := alice.waitStatus(t, sub, to, "no", "unknown", "yes")
				if st.delivered != "no" || !strings.Contains(st.reply, "5.3.4") {
					t.Errorf("deliveryStatus %+v, want no with 5.3.4", st)
				}
				bounced(t, tok, to, "5.3.4")
				// RFC 1870 6.1: a client that knows the limit does not
				// start the transaction.
				for _, m := range sent(t, start, "MAIL") {
					if _, size, ok := strings.Cut(m.line, " SIZE="); ok {
						size, _, _ = strings.Cut(size, " ")
						if n, _ := strconv.Atoi(size); n > peerMax {
							t.Errorf("robinmail offered the message anyway: %q", m.line)
						}
					}
				}
				maildirAbsent(t, p.name, "alice", tok, 0)
			})

			t.Run("recipients", func(t *testing.T) {
				cell(t, "out 09 three recipients, one unknown: 5xx at RCPT, bounce, deliveryStatus", p.name)
				tok := newToken()
				bobAt, unknown := "bob@"+p.domain, "nosuch@"+p.domain
				sub := alice.sendCreate(t, textCreate(tok, from, to, "lab "+tok, "three\n"), to, bobAt, unknown)
				p.find(t, "alice", tok)
				p.find(t, "bob", tok)
				st := alice.waitStatus(t, sub, unknown, "no", "unknown", "yes")
				if st.delivered != "no" || !strings.HasPrefix(st.reply, "550") {
					t.Errorf("deliveryStatus of the unknown user %+v, want no with the peer's 550", st)
				}
				for _, a := range []string{to, bobAt} {
					if s := alice.statuses(t, sub)[a]; s.delivered != "unknown" || !strings.HasPrefix(s.reply, "250") {
						t.Errorf("deliveryStatus of %s: %+v", a, s)
					}
				}
				r := bounced(t, tok, unknown, "5.")
				if !strings.Contains(r.Diagnostic, "550") || r.RemoteMTA != p.host {
					t.Errorf("the notification: %+v", r)
				}
				t.Logf("%s said: %s", p.name, st.reply)
			})

			t.Run("long line", func(t *testing.T) {
				cell(t, "out 10 a line of 998 characters", p.name)
				tok := newToken()
				text := "short\n" + strings.Repeat("x", 998) + "\nshort\n"
				alice.sendRaw(t, rawMessage(tok, from, to, nil, text), to)
				raw := p.find(t, "alice", tok)
				if body(raw) != text {
					t.Errorf("the body changed: %q", clip(body(raw), 200))
				}
			})

			t.Run("too long line", func(t *testing.T) {
				note := cell(t, "out 11 a line of 3000 characters (over RFC 5322's limit)", p.name)
				tok := newToken()
				text := "short\n" + strings.Repeat("y", 3000) + "\nshort\n"
				res := alice.sendRawRes(t, rawMessage(tok, from, to, nil, text), to)
				sub := ""
				for _, r := range res {
					if c, ok := r.args["created"].(map[string]any); ok && r.name == "EmailSubmission/set" {
						if s, ok := c["send"].(map[string]any); ok {
							sub = s["id"].(string)
						}
					}
				}
				if sub == "" {
					note("we refuse")
					t.Logf("robinmail refuses to submit it: %v", res)
					return
				}
				var st status
				waitFor(t, "a result", 60*time.Second, func() bool {
					st = alice.statuses(t, sub)[to]
					return st.delivered == "no" || st.delivered == "unknown"
				})
				if st.delivered == "no" {
					note("peer refuses")
					t.Logf("%s refuses it: %s", p.name, st.reply)
					bounced(t, tok, to, "5.")
					return
				}
				raw := p.find(t, "alice", tok)
				if body(raw) == text {
					note("kept")
				} else {
					note("peer folds")
				}
			})

			t.Run("dots", func(t *testing.T) {
				cell(t, "out 12 lines that start with a dot", p.name)
				tok := newToken()
				alice.sendRaw(t, rawMessage(tok, from, to, nil, dotBody), to)
				raw := p.find(t, "alice", tok)
				if body(raw) != dotBody {
					t.Errorf("body %q, want %q", body(raw), dotBody)
				}
			})

			t.Run("attachments", func(t *testing.T) {
				// Built the way the webmail does it: uploads, then
				// Email/set with textBody and attachments. Taken apart on
				// the peer by ripmime.
				cell(t, "out 13 attachments built by Email/set, unpacked on the peer by ripmime", p.name)
				tok := newToken()
				bin := make([]byte, 70000)
				for i := range bin {
					bin[i] = byte(i*7 + i/251)
				}
				files := []struct {
					name, ctype string
					data        []byte
				}{
					{"data.bin", "application/octet-stream", bin},
					{"notes.txt", "text/plain", []byte("line one\nline two, LF only\n\ttab\n")},
					{"résumé final.pdf", "application/pdf", append([]byte("%PDF-1.7\n"), bin[:5000]...)},
				}
				create := textCreate(tok, from, to, "lab "+tok, "Three files attached.\n")
				var atts []any
				want := map[string]string{}
				for _, f := range files {
					blob := alice.upload(t, f.data, f.ctype)
					atts = append(atts, map[string]any{"blobId": blob, "type": f.ctype, "name": f.name, "disposition": "attachment"})
					sum := sha256.Sum256(f.data)
					want[hex.EncodeToString(sum[:])] = f.name
				}
				create["attachments"] = atts
				alice.sendCreate(t, create, to)
				_, file := maildirFind(t, p.name, "alice", tok)
				out, err := execIn(p.name, "", nil, "sh", "-c",
					`d=$(mktemp -d) && ripmime -i "$1" -d "$d" && cd "$d" && sha256sum -- *`, "sh", file)
				if err != nil {
					t.Fatalf("ripmime: %v\n%s", err, out)
				}
				t.Logf("ripmime found:\n%s", out)
				for sum, name := range want {
					if !strings.Contains(out, sum) {
						t.Errorf("%s did not come out as it went in", name)
					}
				}
			})
		})
	}
}
