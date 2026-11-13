package delivery

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/rspamd"
	"github.com/mikepage/robinmail/internal/smtp"
	"github.com/mikepage/robinmail/internal/store"
)

// fakeStore keeps what was delivered, per account and role, and honours
// delivery keys like the real store.
type fakeStore struct {
	accounts map[string]string // address -> account id
	noJunk   bool              // accounts have no Junk mailbox
	failFor  string            // account id whose deliveries fail
	lookups  error             // fails every Account call
	keys     map[string]bool
	mail     map[string][]string // account id + "/" + role -> messages
}

func (f *fakeStore) Account(_ context.Context, addr string) (store.Account, error) {
	if f.lookups != nil {
		return store.Account{}, f.lookups
	}
	id, ok := f.accounts[addr]
	if !ok {
		return store.Account{}, store.ErrNoAccount
	}
	return store.Account{ID: id, Address: addr}, nil
}

func (f *fakeStore) Deliver(_ context.Context, account string, m store.Message) (store.Email, error) {
	if account == f.failFor {
		return store.Email{}, errors.New("rqlite is down")
	}
	if m.Key == "" || m.ReceivedAt.IsZero() {
		return store.Email{}, errors.New("delivery without key or time")
	}
	if f.keys[account+m.Key] {
		return store.Email{}, store.ErrDuplicate
	}
	f.keys[account+m.Key] = true
	role := m.Role
	if role == store.RoleJunk && f.noJunk {
		role = store.RoleInbox
	}
	f.mail[account+"/"+role] = append(f.mail[account+"/"+role], string(m.Raw))
	return store.Email{ID: 1}, nil
}

func setup(t *testing.T, reply string) (*Deliverer, *fakeStore) {
	t.Helper()
	st := &fakeStore{
		accounts: map[string]string{"mike@example.test": "acct-mike", "postmaster@example.test": "acct-pm"},
		keys:     map[string]bool{}, mail: map[string][]string{},
	}
	d := &Deliverer{
		Hostname: "mx.example.test", Domains: []string{"example.test"}, Store: st,
		Now:      func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) },
		Resolver: fakeResolver{},
	}
	if reply != "" {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, reply)
		}))
		t.Cleanup(srv.Close)
		d.Rspamd = &rspamd.Client{URL: srv.URL}
	}
	return d, st
}

// fakeResolver knows a confirmed PTR for 192.0.2.7 and a forged one for
// 192.0.2.8, whose name points elsewhere.
type fakeResolver struct{}

func (fakeResolver) LookupAddr(_ context.Context, ip string) ([]string, error) {
	switch ip {
	case "192.0.2.7":
		return []string{"Mail.Sender.Test."}, nil
	case "192.0.2.8":
		return []string{"mail.bank.test."}, nil
	}
	return nil, errors.New("no PTR")
}

func (fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	switch host {
	case "mail.sender.test":
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.7")}}, nil
	case "mail.bank.test":
		return []net.IPAddr{{IP: net.ParseIP("198.51.100.1")}}, nil
	}
	return nil, errors.New("no address")
}

func TestReverseDNS(t *testing.T) {
	d, _ := setup(t, "")
	for ip, want := range map[string]string{"192.0.2.7": "mail.sender.test", "192.0.2.8": "", "192.0.2.9": ""} {
		if got := d.reverseDNS(context.Background(), net.ParseIP(ip)); got != want {
			t.Errorf("reverseDNS(%s) = %q, want %q", ip, got, want)
		}
	}
}

func env(rcpts ...string) *smtp.Envelope {
	return &smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "sender.test", From: "a@sender.test", Rcpts: rcpts}
}

func code(err error) int {
	var e *smtp.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// read returns the messages of mike@example.test in the mailbox with role.
func read(_ *testing.T, st *fakeStore, role string) []string {
	return st.mail["acct-mike/"+role]
}

func TestRcpt(t *testing.T) {
	d, _ := setup(t, "")
	for to, want := range map[string]int{
		"mike@example.test":       0,
		"MIKE@Example.Test":       0,
		"mike+lists@example.test": 0,
		"Postmaster":              0,
		"nobody@example.test":     550,
		"mike@elsewhere.test":     550,
		"\"odd\"@elsewhere.test":  550,
	} {
		if got := code(d.Rcpt(context.Background(), env(), to)); got != want {
			t.Errorf("Rcpt(%q) = %d, want %d", to, got, want)
		}
	}
}

func TestDeliverUnscanned(t *testing.T) {
	d, st := setup(t, "")
	msg := "Subject: hi\r\n\r\nbody\r\n"
	if err := d.Deliver(context.Background(), env("mike@example.test", "mike+x@example.test"), []byte(msg)); err != nil {
		t.Fatal(err)
	}
	got := read(t, st, store.RoleInbox)
	if len(got) != 1 {
		t.Fatalf("%d copies, want 1", len(got))
	}
	if !strings.HasPrefix(got[0], "Return-Path: <a@sender.test>\r\nReceived: from sender.test (mail.sender.test [192.0.2.7])\r\n\tby mx.example.test (robinmail) with ESMTP id ") {
		t.Errorf("trace headers:\n%s", got[0])
	}
	if !strings.HasSuffix(got[0], "\r\n\tfor <mike@example.test>;\r\n\tFri, 13 Nov 2026 12:00:00 +0000\r\n"+msg) {
		t.Errorf("message:\n%s", got[0])
	}
}

func TestRcptStoreDown(t *testing.T) {
	d, st := setup(t, "")
	st.lookups = errors.New("rqlite is down")
	if got := code(d.Rcpt(context.Background(), env(), "mike@example.test")); got != 451 {
		t.Errorf("Rcpt with the store down = %d, want 451", got)
	}
}

// A delivery that fails for one recipient is retried by the sender as a
// whole. The recipient that already has the message must not get a second.
func TestRetryAfterPartialFailure(t *testing.T) {
	d, st := setup(t, "")
	msg := []byte("Subject: hi\r\n\r\nbody\r\n")
	e := env("mike@example.test", "postmaster@example.test")
	st.failFor = "acct-pm"
	if got := code(d.Deliver(context.Background(), e, msg)); got != 451 {
		t.Fatalf("first attempt = %d, want 451", got)
	}
	st.failFor = ""
	if err := d.Deliver(context.Background(), e, msg); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n, m := len(st.mail["acct-mike/inbox"]), len(st.mail["acct-pm/inbox"]); n != 1 || m != 1 {
		t.Errorf("mike has %d copies, postmaster %d; want 1 and 1", n, m)
	}
	// Another message, or the same from another sender, is not a retry.
	if err := d.Deliver(context.Background(), e, []byte("Subject: hi\r\n\r\nother\r\n")); err != nil {
		t.Fatal(err)
	}
	e.From = "b@sender.test"
	if err := d.Deliver(context.Background(), e, msg); err != nil {
		t.Fatal(err)
	}
	if n := len(st.mail["acct-mike/inbox"]); n != 3 {
		t.Errorf("mike has %d messages, want 3", n)
	}
}

func TestSpamWithoutJunkMailbox(t *testing.T) {
	d, st := setup(t, `{"action":"add header","score":9}`)
	st.noJunk = true
	if err := d.Deliver(context.Background(), env("mike@example.test"), []byte("Subject: x\r\n\r\ny\r\n")); err != nil {
		t.Fatal(err)
	}
	if n := len(read(t, st, store.RoleInbox)); n != 1 {
		t.Errorf("%d messages in the inbox, want 1", n)
	}
}

func TestDeliverSpam(t *testing.T) {
	d, st := setup(t, `{"action":"rewrite subject","score":9,"subject":"[SPAM] hi",
		"milter":{"add_headers":{"X-Spamd-Result":{"value":"default: True [9.00 / 15.00];\n\tGTUBE(0.00)","order":0}},
		"remove_headers":{"X-Spam":0}}}`)
	msg := "X-Spam: forged\r\nSubject: hi\r\n\r\nbody\r\n"
	if err := d.Deliver(context.Background(), env("mike@example.test"), []byte(msg)); err != nil {
		t.Fatal(err)
	}
	if n := len(read(t, st, store.RoleInbox)); n != 0 {
		t.Errorf("%d messages in the inbox", n)
	}
	junk := read(t, st, store.RoleJunk)
	if len(junk) != 1 {
		t.Fatalf("%d messages in Junk", len(junk))
	}
	m := junk[0]
	for _, want := range []string{
		"X-Spamd-Result: default: True [9.00 / 15.00];\r\n\tGTUBE(0.00)\r\nX-Spam: Yes\r\nSubject: [SPAM] hi\r\n\r\nbody\r\n",
	} {
		if !strings.HasSuffix(m, want) {
			t.Errorf("message:\n%s\nwant suffix:\n%s", m, want)
		}
	}
	if strings.Contains(m, "forged") {
		t.Error("forged header kept")
	}
}

func TestDeliverVerdicts(t *testing.T) {
	for reply, want := range map[string]int{
		`{"action":"reject","score":1000,"symbols":{"GTUBE":{}}}`: 550,
		`{"action":"soft reject"}`:                                451,
		`{"action":"greylist","symbols":{"GREYLIST":{}}}`:         451,
		`{"action":"greylist"}`:                                   0,
		`{"action":"no action"}`:                                  0,
	} {
		d, st := setup(t, reply)
		err := d.Deliver(context.Background(), env("mike@example.test"), []byte("Subject: x\r\n\r\ny\r\n"))
		if code(err) != want {
			t.Errorf("%s: got %v, want %d", reply, err, want)
		}
		if n := len(read(t, st, store.RoleInbox)); (n == 1) != (want == 0) {
			t.Errorf("%s: %d messages delivered", reply, n)
		}
	}
}

func TestRspamdDown(t *testing.T) {
	d, st := setup(t, "")
	d.Rspamd = &rspamd.Client{URL: "http://127.0.0.1:1"}
	msg := []byte("Subject: x\r\n\r\ny\r\n")
	if got := code(d.Deliver(context.Background(), env("mike@example.test"), msg)); got != 451 {
		t.Errorf("fail closed: got %d", got)
	}
	d.FailOpen = true
	if err := d.Deliver(context.Background(), env("mike@example.test"), msg); err != nil {
		t.Errorf("fail open: %v", err)
	}
	if n := len(read(t, st, store.RoleInbox)); n != 1 {
		t.Errorf("%d messages delivered", n)
	}
}

func TestHeaderEdit(t *testing.T) {
	in := "A: 1\r\nB: 2\r\n continued\r\nA: 3\r\nnot a header\r\n\r\nbody"
	fields, rest := splitHeader([]byte(in))
	if len(fields) != 3 || string(rest) != "not a header\r\n\r\nbody" {
		t.Fatalf("fields %q rest %q", fields, rest)
	}
	if got := string(joinHeader(fields, rest)); got != in {
		t.Errorf("round trip %q", got)
	}
	if got := string(joinHeader(removeHeader(append([]field(nil), fields...), "a", -1), nil)); got != "A: 1\r\nB: 2\r\n continued\r\n" {
		t.Errorf("remove last %q", got)
	}
	if got := string(joinHeader(removeHeader(append([]field(nil), fields...), "A", 0), nil)); got != "B: 2\r\n continued\r\n" {
		t.Errorf("remove all %q", got)
	}
	if f, ok := newField("X", "a\r\nInjected: 1\x00"); !ok || string(f.raw) != "X: a\r\n\tInjected: 1\r\n" {
		t.Errorf("newField %q", f.raw)
	}
	if f, _ := newField("Subject", "héllo"); string(f.raw) != "Subject: =?utf-8?q?h=C3=A9llo?=\r\n" {
		t.Errorf("newField %q", f.raw)
	}
	if _, ok := newField("Bad Name", "x"); ok {
		t.Error("accepted a bad name")
	}
}

func FuzzSplitHeader(f *testing.F) {
	f.Add([]byte("A: 1\r\n b\r\n\r\nbody"))
	f.Fuzz(func(t *testing.T, in []byte) {
		fields, rest := splitHeader(in)
		if string(joinHeader(fields, rest)) != string(in) {
			t.Fatalf("round trip changed %q", in)
		}
		for _, n := range []int{0, 1, -1, 5} {
			removeHeader(append([]field(nil), fields...), "a", n)
		}
	})
}

func FuzzNewField(f *testing.F) {
	f.Add("X-Test", "a\nb\r\nc")
	f.Fuzz(func(t *testing.T, name, value string) {
		fl, ok := newField(name, value)
		if !ok {
			return
		}
		raw := string(fl.raw)
		lines := strings.Split(strings.TrimSuffix(raw, "\r\n"), "\r\n")
		for i, l := range lines {
			if strings.ContainsAny(l, "\r\n") || i > 0 && !strings.HasPrefix(l, "\t") {
				t.Fatalf("bad field %q", raw)
			}
		}
	})
}
