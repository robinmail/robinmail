package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/message"
	"github.com/mikepage/robinmail/internal/rspamd"
	"github.com/mikepage/robinmail/internal/smtp"
	"github.com/mikepage/robinmail/internal/store"
)

// queueStore records what a Submitter queues.
type queueStore struct {
	fakeStore
	queued  []store.NewSubmission
	refuse  error
	dsnFor  []string // account + " " + message id of every ApplyDSN
	reports [][]store.Recipient
}

func (q *queueStore) Submit(_ context.Context, account string, n store.NewSubmission) (store.Submission, error) {
	if q.refuse != nil {
		return store.Submission{}, q.refuse
	}
	q.queued = append(q.queued, n)
	return store.Submission{Account: account, ID: int64(len(q.queued)), SendAt: n.SendAt}, nil
}

func (q *queueStore) ApplyDSN(_ context.Context, account, messageID string, reports []store.Recipient) (bool, error) {
	q.dsnFor = append(q.dsnFor, account+" "+messageID)
	q.reports = append(q.reports, reports)
	return true, nil
}

// scanner is an Rspamd that answers with a fixed reply and keeps the
// request it got.
type scanner struct {
	mu     sync.Mutex
	reply  string
	header http.Header
	body   string
}

func (s *scanner) start(t *testing.T) *rspamd.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.header, s.body = r.Header.Clone(), string(body)
		reply := s.reply
		s.mu.Unlock()
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return &rspamd.Client{URL: srv.URL}
}

var mike = store.Account{ID: "acct-mike", Address: "mike@example.test"}

func submitter(t *testing.T) (*Submitter, *queueStore) {
	st := &queueStore{fakeStore: fakeStore{
		accounts: map[string]string{"mike@example.test": "acct-mike", "anna@example.test": "acct-anna"},
		keys:     map[string]bool{}, mail: map[string][]string{}, domains: []string{"example.test"}}}
	return &Submitter{Hostname: "mail.example.test", Store: st,
		Now: func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.FixedZone("", 3600)) }}, st
}

func draft(extra string) []byte {
	return []byte("From: Mike <mike@example.test>\r\nTo: Anna <anna@elsewhere.test>\r\n" + extra + "Subject: Lunch\r\n\r\nTuesday?\r\n")
}

func kind(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Kind
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return ""
}

// A user sends as their own address, with or without a "+tag", and as
// nobody else.
func TestOwns(t *testing.T) {
	for addr, want := range map[string]bool{
		"mike@example.test":          true,
		"Mike@Example.TEST":          true,
		"mike+lists@example.test":    true,
		"mike+a+b@example.test":      true,
		"anna@example.test":          false,
		"mike@other.test":            false,
		"mike+x@other.test":          false,
		"+x@example.test":            false,
		"mikey@example.test":         false,
		"mike@example.test.evil.org": false,
		"":                           false,
		"not an address":             false,
	} {
		if got := Owns("mike@example.test", addr); got != want {
			t.Errorf("Owns(%q) = %v", addr, got)
		}
	}
}

// An account may send as the aliases that deliver to it, with or without a
// tag, in the envelope and in From and Sender.
func TestSendAsAlias(t *testing.T) {
	s, st := submitter(t)
	asked := 0
	s.SendAs = func(_ context.Context, account string) ([]string, error) {
		asked++
		if account != "mike@example.test" {
			t.Errorf("SendAs asked for %q", account)
		}
		return []string{"info@example.test"}, nil
	}
	msg := func(from string) []byte {
		return []byte("From: " + from + "\r\nSubject: x\r\n\r\nbody\r\n")
	}
	for _, c := range []struct{ envelope, from, want string }{
		{"info@example.test", "Info <info@example.test>", ""},
		{"mike@example.test", "info+shop@example.test", ""},
		{"INFO@example.test", "mike@example.test", ""},
		{"", "info@example.test", ""},
		{"other@example.test", "mike@example.test", RefuseMailFrom},
		{"mike@example.test", "other@example.test", RefuseFrom},
		{"info@example.test", "info@example.test, other@example.test", RefuseFrom},
		{"info@other.test", "info@example.test", RefuseMailFrom},
	} {
		_, err := s.Submit(context.Background(), Outgoing{Account: mike, From: c.envelope, Rcpts: []string{"bob@elsewhere.example"}, Raw: msg(c.from)})
		if kind(err) != c.want {
			t.Errorf("envelope %q, From %q: %v, want %q", c.envelope, c.from, err, c.want)
		}
	}
	if len(st.queued) != 4 || asked == 0 {
		t.Errorf("%d queued, aliases asked %d times", len(st.queued), asked)
	}
	// A lookup that fails gives no address away.
	s.SendAs = func(context.Context, string) ([]string, error) { return nil, errors.New("rqlite is down") }
	_, err := s.Submit(context.Background(), Outgoing{Account: mike, From: "info@example.test", Rcpts: []string{"bob@elsewhere.example"}, Raw: msg("info@example.test")})
	if kind(err) != RefuseMailFrom {
		t.Errorf("with the alias lookup down: %v", err)
	}
	// The SMTP handler checks MAIL FROM the same way.
	h := &Handler{Submitter: s}
	if err := h.Mail(context.Background(), &smtp.Envelope{AuthUser: "mike@example.test", From: "info@example.test"}); code(err) != 550 {
		t.Errorf("Mail with the lookup down: %v", err)
	}
	s.SendAs = func(context.Context, string) ([]string, error) { return []string{"info@example.test"}, nil }
	if err := h.Mail(context.Background(), &smtp.Envelope{AuthUser: "mike@example.test", From: "info@example.test"}); err != nil {
		t.Errorf("Mail as the alias: %v", err)
	}
}

// RFC 6409 6.1 and 8.1: a submission server may check the sender, and may
// add the fields a message needs. RFC 5322 3.6.3: Bcc is not for the
// recipients to see.
func TestSubmitCompletesTheMessage(t *testing.T) {
	s, st := submitter(t)
	st1 := tls.ConnectionState{Version: tls.VersionTLS13, CipherSuite: tls.TLS_AES_128_GCM_SHA256}
	sub, err := s.Submit(context.Background(), Outgoing{Account: mike, From: "mike@example.test",
		Rcpts: []string{"anna@elsewhere.test", "secret@hidden.test", "ANNA@elsewhere.test"},
		Raw:   draft("Bcc: secret@hidden.test\r\n"), Protocol: "ESMTPSA", IP: net.ParseIP("203.0.113.77"), Helo: "mikes-laptop.home.arpa", TLS: &st1})
	if err != nil || sub.ID != 1 {
		t.Fatalf("%v %+v", err, sub)
	}
	n := st.queued[0]
	// A recipient named twice is one recipient.
	if n.MailFrom != "mike@example.test" || strings.Join(n.Rcpts, " ") != "anna@elsewhere.test secret@hidden.test" || !n.SendAt.IsZero() {
		t.Errorf("queued %+v", n)
	}
	out := string(n.Raw)
	t.Logf("queued:\n%s", strings.ReplaceAll(out, "\r\n", "\n"))
	// The trace line (RFC 5321 4.4, RFC 3848) without the FROM clause: a
	// recipient learns neither the address nor the name of the sender's
	// computer.
	wantTop := "Received: by mail.example.test \\(robinmail\\) with ESMTPSA id [0-9A-F]{16}\r\n\t\\(TLS 1.3 TLS_AES_128_GCM_SHA256\\);\r\n\tSat, 14 Nov 2026 12:00:00 \\+0100\r\n"
	if !regexp.MustCompile("^" + wantTop + "From: ").MatchString(out) {
		t.Errorf("the message does not start with our Received line:\n%q", out)
	}
	for _, leak := range []string{"203.0.113.77", "mikes-laptop", "secret@hidden.test", "Bcc"} {
		if strings.Contains(out, leak) {
			t.Errorf("the message as sent has %q", leak)
		}
	}
	fields := message.ParseHeader(n.Raw)
	if date, _ := message.Get(fields, "Date"); strings.TrimSpace(date) != "Sat, 14 Nov 2026 12:00:00 +0100" {
		t.Errorf("Date %q", date)
	}
	if id, _ := message.Get(fields, "Message-ID"); !regexp.MustCompile(`^ <[0-9a-f]{32}@mail\.example\.test>$`).MatchString(id) {
		t.Errorf("Message-ID %q", id)
	}
	if !strings.HasSuffix(out, "\r\n\r\nTuesday?\r\n") {
		t.Errorf("the body changed: %q", out)
	}

	// What a message has is kept.
	s.Submit(context.Background(), Outgoing{Account: mike, From: "mike+work@example.test", Rcpts: []string{"anna@elsewhere.test"},
		Raw: draft("Date: Fri, 13 Nov 2026 08:00:00 +0000\r\nMessage-ID: <mine@client.test>\r\n"), Protocol: "JMAP"})
	out = string(st.queued[1].Raw)
	if strings.Count(out, "Date:") != 1 || strings.Count(out, "Message-ID:") != 1 || !strings.Contains(out, "Message-ID: <mine@client.test>\r\n") ||
		!strings.Contains(out, "Date: Fri, 13 Nov 2026 08:00:00 +0000\r\n") {
		t.Errorf("Date or Message-ID of the client replaced:\n%s", out)
	}
	if !strings.HasPrefix(out, "Received: by mail.example.test (robinmail) (JMAP) id ") {
		t.Errorf("Received for JMAP: %q", out[:80])
	}
	// A message with LF line ends (imported over JMAP) goes out with CRLF.
	s.Submit(context.Background(), Outgoing{Account: mike, From: "mike@example.test", Rcpts: []string{"anna@elsewhere.test"},
		Raw: []byte("From: mike@example.test\nSubject: unix\n\nline one\nline two\n")})
	if out := string(st.queued[2].Raw); strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") || !strings.HasSuffix(out, "\r\n\r\nline one\r\nline two\r\n") {
		t.Errorf("bare LF on the way out: %q", out)
	}
}

func TestSubmitRefusals(t *testing.T) {
	s, st := submitter(t)
	s.MaxRecipients, s.MaxMessageBytes = 3, 2000
	ok := []string{"anna@elsewhere.test"}
	for _, tc := range []struct {
		name string
		o    Outgoing
		want string
	}{
		{"fine", Outgoing{From: "mike@example.test", Rcpts: ok, Raw: draft("")}, ""},
		{"the null reverse-path", Outgoing{From: "", Rcpts: ok, Raw: draft("")}, ""},
		{"a +tag", Outgoing{From: "mike+tag@example.test", Rcpts: ok, Raw: draft("")}, ""},
		{"envelope sender of someone else", Outgoing{From: "anna@example.test", Rcpts: ok, Raw: draft("")}, RefuseMailFrom},
		{"From of someone else", Outgoing{From: "mike@example.test", Rcpts: ok, Raw: []byte("From: The Boss <boss@example.test>\r\n\r\nx\r\n")}, RefuseFrom},
		{"two authors, one of them someone else", Outgoing{From: "mike@example.test", Rcpts: ok,
			Raw: []byte("From: mike@example.test, boss@example.test\r\n\r\nx\r\n")}, RefuseFrom},
		{"Sender of someone else", Outgoing{From: "mike@example.test", Rcpts: ok,
			Raw: []byte("From: mike@example.test\r\nSender: boss@example.test\r\n\r\nx\r\n")}, RefuseFrom},
		// The check must see the From a reader sees: with two, programs
		// disagree about which that is.
		{"two From fields, the first someone else's", Outgoing{From: "mike@example.test", Rcpts: ok,
			Raw: []byte("From: The Boss <boss@example.test>\r\nFrom: mike@example.test\r\n\r\nx\r\n")}, RefuseInvalid},
		{"two From fields, the last someone else's", Outgoing{From: "mike@example.test", Rcpts: ok,
			Raw: []byte("From: mike@example.test\r\nfrom: boss@example.test\r\n\r\nx\r\n")}, RefuseInvalid},
		{"two Sender fields", Outgoing{From: "mike@example.test", Rcpts: ok,
			Raw: []byte("From: mike@example.test\r\nSender: boss@example.test\r\nSender: mike@example.test\r\n\r\nx\r\n")}, RefuseInvalid},
		// A line that is no header field ends the header for us, and not
		// for a program that takes the obsolete "From :" (RFC 5322 4.5).
		{"a From field behind a line that ends our header", Outgoing{From: "mike@example.test", Rcpts: ok,
			Raw: []byte("From: mike@example.test\r\nFrom : boss@example.test\r\nSubject: x\r\n\r\nx\r\n")}, RefuseInvalid},
		{"a header without a body", Outgoing{From: "mike@example.test", Rcpts: ok, Raw: []byte("From: mike@example.test\r\nSubject: x\r\n")}, ""},
		{"no From", Outgoing{From: "mike@example.test", Rcpts: ok, Raw: []byte("Subject: x\r\n\r\nx\r\n")}, RefuseInvalid},
		{"From without an address", Outgoing{From: "mike@example.test", Rcpts: ok, Raw: []byte("From: undisclosed:;\r\n\r\nx\r\n")}, RefuseInvalid},
		{"no recipients", Outgoing{From: "mike@example.test", Raw: draft("")}, RefuseNoRecipients},
		{"a recipient that is no address", Outgoing{From: "mike@example.test", Rcpts: []string{"anna@elsewhere.test", "anna at home", "a b@c.test"}, Raw: draft("")}, RefuseRecipients},
		{"too many recipients", Outgoing{From: "mike@example.test", Rcpts: []string{"a@x.test", "b@x.test", "c@x.test", "d@x.test"}, Raw: draft("")}, RefuseTooMany},
		{"too large", Outgoing{From: "mike@example.test", Rcpts: ok, Raw: draft("X-Pad: " + strings.Repeat("x", 2000) + "\r\n")}, RefuseTooLarge},
	} {
		before := len(st.queued)
		tc.o.Account = mike
		_, err := s.Submit(context.Background(), tc.o)
		if got := kind(err); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		if queued := len(st.queued) - before; queued != map[bool]int{true: 1}[tc.want == ""] {
			t.Errorf("%s: %d messages queued", tc.name, queued)
		}
	}
	var r *Refusal
	_, err := s.Submit(context.Background(), Outgoing{Account: mike, From: "mike@example.test", Rcpts: []string{"anna at home"}, Raw: draft("")})
	if !errors.As(err, &r) || len(r.Recipients) != 1 || r.Recipients[0] != "anna at home" {
		t.Errorf("invalid recipients are not named: %+v", r)
	}
	st.refuse = store.ErrRateLimited
	if _, err := s.Submit(context.Background(), Outgoing{Account: mike, From: "mike@example.test", Rcpts: ok, Raw: draft("")}); kind(err) != RefuseRateLimit {
		t.Errorf("rate limit: %v", err)
	}
	st.refuse = store.ErrStateMismatch
	if _, err := s.Submit(context.Background(), Outgoing{Account: mike, From: "mike@example.test", Rcpts: ok, Raw: draft("")}); !errors.Is(err, store.ErrStateMismatch) {
		t.Errorf("a store error is not passed on: %v", err)
	}
}

// Outgoing mail is scanned as mail of an authenticated user: that is what
// makes Rspamd sign it. The signature comes back as a header to add.
func TestSubmitThroughRspamd(t *testing.T) {
	s, st := submitter(t)
	sc := &scanner{}
	s.Rspamd = sc.start(t)
	s.Hold, s.MaxPerHour = 10*time.Second, 300
	const sig = "v=1; a=rsa-sha256; c=relaxed/relaxed; d=example.test; s=robinmail;\r\n\th=from:to:subject:date:message-id; bh=abc=; b=def="
	// The shape of a real reply of Rspamd 4.3 over HTTP: the signature in
	// "dkim-signature", folded with CRLF and a tab.
	sc.reply = `{"action":"no action","score":-0.1,"required_score":15,
		"dkim-signature":"` + strings.NewReplacer("\r", `\r`, "\n", `\n`, "\t", `\t`).Replace(sig) + `",
		"milter":{"remove_headers":{"X-Mailer":0,"X-Spam":0}}}`
	o := Outgoing{Account: mike, From: "mike@example.test", Rcpts: []string{"anna@elsewhere.test"}, EmailID: 7, IdentityID: 1,
		Raw: draft("X-Mailer: secret client 1.0\r\nBcc: boss@example.test\r\n"), Protocol: "JMAP", IP: net.ParseIP("203.0.113.77"), Helo: "laptop"}
	if _, err := s.Submit(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	// What Rspamd was told (its protocol's headers).
	if sc.header.Get("User") != "mike@example.test" || sc.header.Get("From") != "mike@example.test" || sc.header.Get("Rcpt") != "anna@elsewhere.test" ||
		sc.header.Get("Ip") != "203.0.113.77" || sc.header.Get("Queue-Id") == "" {
		t.Errorf("request to Rspamd: %v", sc.header)
	}
	// What it scanned and signed is what goes out, apart from our
	// Received line: complete, and without Bcc.
	if strings.Contains(sc.body, "Bcc") || !strings.Contains(sc.body, "\r\nDate: ") || !strings.Contains(sc.body, "\r\nMessage-ID: ") {
		t.Errorf("Rspamd scanned:\n%s", sc.body)
	}
	n := st.queued[0]
	out := string(n.Raw)
	if !regexp.MustCompile(`^Received: [^\n]+\n\t[^\n]+\nDKIM-Signature: v=1; a=rsa-sha256; c=relaxed/relaxed; d=example\.test; s=robinmail;\r\n\th=from:to:subject:date:message-id; bh=abc=; b=def=\r\nFrom: `).MatchString(out) {
		t.Errorf("no DKIM-Signature on top of the message:\n%s", out)
	}
	if strings.Contains(out, "X-Mailer") {
		t.Error("a header Rspamd wanted removed is still there")
	}
	signed := out[strings.Index(out, "From: "):]
	if signed != strings.Replace(sc.body, "X-Mailer: secret client 1.0\r\n", "", 1) {
		t.Errorf("the message changed after it was signed:\nscanned %q\nqueued  %q", sc.body, signed)
	}
	// A JMAP submission waits for the undo time and carries the limit.
	if n.EmailID != 7 || n.IdentityID != 1 || n.MaxPerHour != 300 || !n.SendAt.Equal(s.now().Add(10*time.Second)) {
		t.Errorf("queued %+v", n)
	}

	// What Rspamd rejects is not sent.
	sc.reply = `{"action":"reject","score":20,"messages":{"smtp_message":"Gtube pattern"}}`
	var r *Refusal
	if _, err := s.Submit(context.Background(), o); !errors.As(err, &r) || r.Kind != RefuseToSend || r.Msg != "Gtube pattern" || len(st.queued) != 1 {
		t.Errorf("reject: %v, %d queued", err, len(st.queued))
	}
	sc.reply = `{"action":"soft reject","messages":{"smtp_message":"Rate limit exceeded"}}`
	if _, err := s.Submit(context.Background(), o); kind(err) != RefuseLater || len(st.queued) != 1 {
		t.Errorf("soft reject: %v", err)
	}
	// "add header" is Rspamd's word for probable spam. It is the user's
	// own mail: it goes out, and without a spam mark of ours.
	sc.reply = `{"action":"add header","score":8,"milter":{"add_headers":{"X-Spamd-Result":{"value":"default: True [8.00 / 15.00]"}}}}`
	if _, err := s.Submit(context.Background(), o); err != nil || strings.Contains(string(st.queued[1].Raw), "X-Spam: Yes") {
		t.Errorf("add header: %v", err)
	}

	// Rspamd away: try later, or with fail_open send unsigned.
	s.Rspamd = &rspamd.Client{URL: "http://127.0.0.1:1", Timeout: time.Second}
	if _, err := s.Submit(context.Background(), o); kind(err) != RefuseLater || len(st.queued) != 2 {
		t.Errorf("Rspamd down: %v", err)
	}
	s.FailOpen = true
	if _, err := s.Submit(context.Background(), o); err != nil || strings.Contains(string(st.queued[2].Raw), "DKIM-Signature") {
		t.Errorf("Rspamd down with fail_open: %v", err)
	}
	// No Rspamd configured: unsigned.
	s.Rspamd = nil
	if _, err := s.Submit(context.Background(), o); err != nil || strings.Contains(string(st.queued[3].Raw), "DKIM-Signature") {
		t.Errorf("without Rspamd: %v", err)
	}
}

// The SMTP side of submission: who may log in, what they may send as, and
// which reply each refusal gets.
func TestSubmissionHandler(t *testing.T) {
	s, st := submitter(t)
	s.MaxRecipients = 2
	status := 0
	h := &Handler{Submitter: s, Login: func(_ context.Context, ip, username, password string) (store.Account, int, time.Duration) {
		if ip != "203.0.113.77" {
			t.Errorf("login from %q", ip)
		}
		if status != 0 {
			return store.Account{}, status, time.Minute
		}
		return mike, 0, 0
	}}
	ctx := context.Background()
	ip := net.ParseIP("203.0.113.77")
	if user, err := h.Authenticate(ctx, ip, "Mike@Example.test", "pw"); err != nil || user != "mike@example.test" {
		t.Errorf("login: %q %v", user, err)
	}
	status = http.StatusUnauthorized
	if _, err := h.Authenticate(ctx, ip, "mike@example.test", "wrong"); !errors.Is(err, smtp.ErrAuthFailed) {
		t.Errorf("wrong password: %v", err)
	}
	for _, status = range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		var e *smtp.Error
		if _, err := h.Authenticate(ctx, ip, "mike@example.test", "pw"); !errors.As(err, &e) || e.Code != 454 || e.Enhanced != "4.7.0" {
			t.Errorf("status %d: %v", status, err)
		}
	}

	env := &smtp.Envelope{AuthUser: "mike@example.test", RemoteIP: ip, Helo: "laptop", From: "mike@example.test"}
	if err := h.Mail(ctx, env); err != nil {
		t.Errorf("own address: %v", err)
	}
	if err := h.Mail(ctx, &smtp.Envelope{AuthUser: "mike@example.test", From: ""}); err != nil {
		t.Errorf("null reverse-path: %v", err)
	}
	var e *smtp.Error
	if err := h.Mail(ctx, &smtp.Envelope{AuthUser: "mike@example.test", From: "anna@example.test"}); !errors.As(err, &e) || e.Code != 550 || e.Enhanced != "5.7.1" {
		t.Errorf("another sender: %v", err)
	}
	// Any address may be a recipient: this is the port that relays.
	if err := h.Rcpt(ctx, env, "someone@anywhere.example"); err != nil {
		t.Errorf("a remote recipient: %v", err)
	}

	env.Rcpts = []string{"anna@elsewhere.test"}
	env.UTF8 = true
	if err := h.Deliver(ctx, env, draft("")); err != nil {
		t.Fatal(err)
	}
	if out := string(st.queued[0].Raw); !strings.Contains(out, " with UTF8SMTPSA id ") {
		t.Errorf("Received: %q", out[:90])
	}
	for name, tc := range map[string]struct {
		rcpts []string
		msg   []byte
		store error
		code  int
		enh   string
	}{
		"From of someone else": {env.Rcpts, []byte("From: boss@example.test\r\n\r\nx\r\n"), nil, 550, "5.7.1"},
		"no From":              {env.Rcpts, []byte("Subject: x\r\n\r\nx\r\n"), nil, 550, "5.6.0"},
		"too many recipients":  {[]string{"a@x.test", "b@x.test", "c@x.test"}, draft(""), nil, 452, "4.5.3"},
		"rate limit":           {env.Rcpts, draft(""), store.ErrRateLimited, 451, "4.7.1"},
		"store down":           {env.Rcpts, draft(""), errors.New("rqlite is away"), 451, "4.3.0"},
	} {
		st.refuse = tc.store
		err := h.Deliver(ctx, &smtp.Envelope{AuthUser: "mike@example.test", From: "mike@example.test", Rcpts: tc.rcpts}, tc.msg)
		if !errors.As(err, &e) || e.Code != tc.code || e.Enhanced != tc.enh {
			t.Errorf("%s: %v, want %d %s", name, err, tc.code, tc.enh)
		}
	}
}

// Mail from the queue for a recipient here is stored without SMTP.
func TestLocalDelivery(t *testing.T) {
	d, fs := setup(t, "")
	ctx := context.Background()
	msg := []byte("Received: by mail.example.test (robinmail) with ESMTPSA id 1;\r\n\tFri, 13 Nov 2026 12:00:00 +0000\r\nReturn-Path: <forged@x.test>\r\n" +
		"From: anna@example.test\r\nTo: mike@example.test\r\nSubject: hi\r\n\r\nhello\r\n")
	if local, err := d.LocalDomain(ctx, "Example.TEST"); !local || err != nil {
		t.Errorf("our own domain: %v %v", local, err)
	}
	if local, _ := d.LocalDomain(ctx, "elsewhere.test"); local {
		t.Error("someone else's domain is local")
	}
	if err := d.Local(ctx, "anna@example.test", "mike+tag@example.test", msg); err != nil {
		t.Fatal(err)
	}
	got := fs.mail["acct-mike/inbox"]
	if len(got) != 1 || !strings.HasPrefix(got[0], "Return-Path: <anna@example.test>\r\nReceived: by mail.example.test") ||
		strings.Count(got[0], "Return-Path:") != 1 || !strings.HasSuffix(got[0], "\r\n\r\nhello\r\n") {
		t.Errorf("stored %q", got)
	}
	// A second attempt of the queue stores nothing new.
	if err := d.Local(ctx, "anna@example.test", "mike@example.test", msg); err != nil || len(fs.mail["acct-mike/inbox"]) != 1 {
		t.Errorf("second delivery: %v, %d copies", err, len(fs.mail["acct-mike/inbox"]))
	}
	var e *smtp.Error
	if err := d.Local(ctx, "anna@example.test", "nobody@example.test", msg); !errors.As(err, &e) || e.Code != 550 || e.Enhanced != "5.1.1" {
		t.Errorf("unknown user: %v", err)
	}
	fs.failFor = "acct-mike"
	if err := d.Local(ctx, "other@example.test", "mike@example.test", msg); !errors.As(err, &e) || e.Code != 451 {
		t.Errorf("store down: %v", err)
	}
}

// A delivery status notification that arrives for mail an account sent
// stays normal mail in the Inbox, and what it says is recorded with the
// submission.
func TestIncomingDSNIsRecorded(t *testing.T) {
	d, fs := setup(t, "")
	qs := &queueStore{}
	d.Submissions = qs
	name := "Mail Delivery Subsystem"
	dsn := message.BuildDSN(message.DSNMessage{From: message.Address{Name: &name, Email: "MAILER-DAEMON@mx.far.test"}, To: "mike@example.test",
		MessageID: "bounce-1@mx.far.test", Date: d.now(),
		Original: []byte("From: mike@example.test\r\nTo: gone@far.test\r\nMessage-ID: <sent-42@mail.example.test>\r\n\r\nhi\r\n")},
		message.DSN{ReportingMTA: "mx.far.test", Recipients: []message.DSNRecipient{
			{Address: "gone@far.test", Action: "failed", Status: "5.2.1", Diagnostic: "550 5.2.1 Mailbox disabled", RemoteMTA: "store.far.test"},
			{Address: "slow@far.test", Action: "delayed", Status: "4.4.1"},
			{Address: "ok@far.test", Action: "delivered", Status: "2.0.0"},
		}})
	env := &smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "mx.far.test", From: "", Rcpts: []string{"mike@example.test"}}
	if err := d.Deliver(context.Background(), env, dsn); err != nil {
		t.Fatal(err)
	}
	if got := fs.mail["acct-mike/inbox"]; len(got) != 1 || !strings.Contains(got[0], "multipart/report") {
		t.Fatalf("the bounce is not in the Inbox: %d messages", len(got))
	}
	if len(qs.dsnFor) != 1 || qs.dsnFor[0] != "acct-mike sent-42@mail.example.test" {
		t.Fatalf("recorded for %v", qs.dsnFor)
	}
	// "delayed" says nothing final and is left out.
	reps := qs.reports[0]
	if len(reps) != 2 || reps[0].Address != "gone@far.test" || reps[0].State != store.RcptFailed || reps[0].Status != "5.2.1" ||
		reps[0].Reply != "550 5.2.1 Mailbox disabled" || reps[0].MTA != "store.far.test" || reps[1].State != store.RcptDelivered || reps[1].MTA != "mx.far.test" {
		t.Errorf("reports %+v", reps)
	}

	// RFC 3464 2: a DSN has the null reverse-path. The same message from
	// an ordinary sender is mail, and nothing more.
	env2 := &smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "mx.far.test", From: "someone@far.test", Rcpts: []string{"mike@example.test"}}
	d.Deliver(context.Background(), env2, dsn)
	// And a message that is not a DSN is not looked at.
	env.Rcpts = []string{"postmaster@example.test"}
	d.Deliver(context.Background(), env, []byte("From: MAILER-DAEMON@mx.far.test\r\nSubject: Undelivered\r\n\r\nYour mail to gone@far.test bounced.\r\n"))
	if len(qs.dsnFor) != 1 {
		t.Errorf("recorded %d times: %v", len(qs.dsnFor), qs.dsnFor)
	}
}

// The notifications of Postfix, Exim and OpenSMTPD as they arrived in the
// interoperability lab (contrib/lab; the files are those of
// internal/message). Each must reach the submission, also OpenSMTPD's,
// which is a multipart/mixed, and each must leave a reason that reads as a
// reply (RFC 8621 7, smtpReply), also where the report has no
// Diagnostic-Code.
func TestPeerNotificationsAreRecorded(t *testing.T) {
	for peer, want := range map[string]store.Recipient{
		"postfix":   {Status: "5.1.1", Reply: `550 5.1.1 unknown user: "ghost"`},
		"exim":      {Status: "5.0.0", Reply: "550 5.0.0 reported by mx.exim.test without a reason"},
		"opensmtpd": {Status: "5.3.0", Reply: "550 5.3.0 reported by mx.opensmtpd.test without a reason"},
	} {
		t.Run(peer, func(t *testing.T) {
			raw, err := os.ReadFile("../message/testdata/dsn-" + peer + ".eml")
			if err != nil {
				t.Fatal(err)
			}
			// As it came over SMTP: with CRLF (the files have LF) and
			// without the Return-Path the delivery adds.
			raw = bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))
			if _, rest, ok := bytes.Cut(raw, []byte("Return-Path: <>\r\n")); ok {
				raw = rest
			}
			d, _ := setup(t, "")
			qs := &queueStore{}
			d.Submissions = qs
			env := &smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "mx." + peer + ".test", From: "", Rcpts: []string{"mike@example.test"}}
			if err := d.Deliver(context.Background(), env, raw); err != nil {
				t.Fatal(err)
			}
			if len(qs.dsnFor) != 1 || !strings.HasSuffix(qs.dsnFor[0], "@mx.robin.test") {
				t.Fatalf("recorded for %v", qs.dsnFor)
			}
			reps := qs.reports[0]
			if len(reps) != 1 || reps[0].Address != "ghost@late."+peer+".test" || reps[0].State != store.RcptFailed ||
				reps[0].Status != want.Status || reps[0].Reply != want.Reply || reps[0].MTA != "mx."+peer+".test" {
				t.Errorf("reports %+v", reps)
			}
		})
	}
}
