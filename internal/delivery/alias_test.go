package delivery

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/address"
	"github.com/robinmail/robinmail/internal/smtp"
	"github.com/robinmail/robinmail/internal/srs"
	"github.com/robinmail/robinmail/internal/store"
)

// fakeAliases is an alias table with the lookup rules of the store.
type fakeAliases struct {
	list []store.Alias
	fail error
}

func (f *fakeAliases) LookupAlias(_ context.Context, a address.Address) (store.Alias, bool, error) {
	if f.fail != nil {
		return store.Alias{}, false, f.fail
	}
	base := a.Local
	if b, _, ok := strings.Cut(a.Local, "+"); ok {
		base = b
	}
	for _, want := range []string{a.String(), base + "@" + a.Domain} {
		for _, al := range f.list {
			if al.Address == want {
				return al, true, nil
			}
		}
	}
	var best store.Alias
	weight := -1
	for _, al := range f.list {
		local, domain, _ := strings.Cut(al.Address, "@")
		if !al.Wildcard || domain != a.Domain {
			continue
		}
		if ok, w := store.MatchAlias(local, base); ok && w > weight {
			best, weight = al, w
		}
	}
	return best, weight >= 0, nil
}

// queued is one message a fakeForwarder was given.
type queued struct{ domain, key, from, to, raw string }

type fakeForwarder struct {
	sent []queued
	keys map[string]bool
	fail error
}

func (f *fakeForwarder) Forward(_ context.Context, domain, key, from, to string, raw []byte) error {
	if f.fail != nil {
		return f.fail
	}
	if f.keys == nil {
		f.keys = map[string]bool{}
	}
	if f.keys[key] {
		return store.ErrForwarded
	}
	f.keys[key] = true
	f.sent = append(f.sent, queued{domain, key, from, to, string(raw)})
	return nil
}

func aliasSetup(t *testing.T, reply string) (*Deliverer, *fakeStore, *fakeAliases, *fakeForwarder) {
	d, st := setup(t, reply)
	st.accounts["anna@example.test"] = "acct-anna"
	al := &fakeAliases{list: []store.Alias{
		{Address: "info@example.test", Targets: []string{"mike@example.test", "anna@example.test"}},
		{Address: "away@example.test", Targets: []string{"mike@elsewhere.example"}},
		{Address: "both@example.test", Targets: []string{"anna@example.test", "Friend@elsewhere.example"}},
		{Address: "chain@example.test", Targets: []string{"info@example.test"}},
		{Address: "loop-a@example.test", Targets: []string{"loop-b@example.test"}},
		{Address: "loop-b@example.test", Targets: []string{"loop-a@example.test"}},
		// plain.test has no catch-all, so a target there can be missing.
		{Address: "dangling@plain.test", Targets: []string{"nobody@plain.test"}},
		{Address: "half@plain.test", Targets: []string{"nobody@plain.test", "anna@example.test"}},
		{Address: "sales-*@example.test", Targets: []string{"anna@example.test"}, Wildcard: true},
		{Address: "*@example.test", Targets: []string{"mike@example.test"}, Wildcard: true},
		{Address: "*@other.test", Targets: []string{"catch@elsewhere.example"}, Wildcard: true},
	}}
	fw := &fakeForwarder{}
	st.domains = []string{"example.test", "other.test", "plain.test"}
	d.Aliases, d.Forwards = al, fw
	d.SRS = &srs.Rewriter{Key: []byte("a key for the tests, 32 bytes!!!"), Now: d.Now}
	return d, st, al, fw
}

func routeOf(t *testing.T, d *Deliverer, from, to string) (accounts, forwards []string, err error) {
	t.Helper()
	var r routes
	err = d.route(context.Background(), from, to, &r)
	for _, a := range r.accounts {
		accounts = append(accounts, a.Address)
	}
	for _, f := range r.forwards {
		forwards = append(forwards, f.to)
	}
	return accounts, forwards, err
}

// Where mail for an address goes: an account before an alias, the exact
// alias before a pattern, the longest pattern before the catch-all, and
// plus-addressing kept for accounts and aliases alike.
func TestRouteOrder(t *testing.T) {
	d, _, _, _ := aliasSetup(t, "")
	for _, c := range []struct {
		to       string
		accounts string
		forwards string
	}{
		{"mike@example.test", "mike@example.test", ""},
		{"mike+lists@example.test", "mike@example.test", ""},
		{"info@example.test", "mike@example.test anna@example.test", ""},
		{"INFO+tag@Example.Test", "mike@example.test anna@example.test", ""},
		{"away@example.test", "", "mike@elsewhere.example"},
		{"both@example.test", "anna@example.test", "Friend@elsewhere.example"},
		{"chain@example.test", "mike@example.test anna@example.test", ""},
		{"sales-nl@example.test", "anna@example.test", ""},
		{"whoever@example.test", "mike@example.test", ""}, // the catch-all
		{"half@plain.test", "anna@example.test", ""},      // a target that is gone does not stop the others
		{"anyone@other.test", "", "catch@elsewhere.example"},
	} {
		accounts, forwards, err := routeOf(t, d, "a@sender.test", c.to)
		if err != nil || strings.Join(accounts, " ") != c.accounts || strings.Join(forwards, " ") != c.forwards {
			t.Errorf("route(%s) = %v %v, %v", c.to, accounts, forwards, err)
		}
	}
	for to, want := range map[string]int{
		"loop-a@example.test": 550, // comes back to itself: leads nowhere
		"dangling@plain.test": 550,
		"x@elsewhere.example": 550, // not ours: no relay
	} {
		if _, _, err := routeOf(t, d, "a@sender.test", to); code(err) != want {
			t.Errorf("route(%s): %v, want %d", to, err, want)
		}
	}
	if _, _, err := routeOf(t, d, "a@sender.test", "x@elsewhere.example"); err != errNoRelay {
		t.Errorf("an address elsewhere: %v", err)
	}
}

// A chain of aliases ends after maxAliasDepth steps.
func TestAliasDepth(t *testing.T) {
	d, _, al, _ := aliasSetup(t, "")
	al.list = nil
	names := []string{"a0", "a1", "a2", "a3", "a4", "a5", "a6"}
	for i := 0; i < len(names)-1; i++ {
		al.list = append(al.list, store.Alias{Address: names[i] + "@example.test", Targets: []string{names[i+1] + "@example.test"}})
	}
	al.list = append(al.list, store.Alias{Address: "a6@example.test", Targets: []string{"mike@example.test"}})
	if _, _, err := routeOf(t, d, "", "a2@example.test"); err != nil {
		t.Errorf("five aliases deep: %v", err)
	}
	if _, _, err := routeOf(t, d, "", "a0@example.test"); code(err) != 550 {
		t.Errorf("seven aliases deep: %v", err)
	}
}

// A lookup that fails defers the mail; it does not bounce it.
func TestAliasLookupFails(t *testing.T) {
	d, _, al, fw := aliasSetup(t, "")
	al.fail = errors.New("the database is down")
	if _, _, err := routeOf(t, d, "", "info@example.test"); code(err) != 451 {
		t.Errorf("alias lookup down: %v", err)
	}
	al.fail = nil
	fw.fail = errors.New("the database is down")
	msg := []byte("Subject: x\r\n\r\nbody\r\n")
	if err := d.Deliver(context.Background(), env("both@example.test"), msg); code(err) != 451 {
		t.Errorf("forward queue down: %v", err)
	}
	// The retry stores nothing twice and forwards once.
	fw.fail = nil
	for i := 0; i < 2; i++ {
		if err := d.Deliver(context.Background(), env("both@example.test"), msg); err != nil {
			t.Fatal(err)
		}
	}
	if len(fw.sent) != 1 {
		t.Errorf("%d forwards queued, want 1", len(fw.sent))
	}
}

// What a forwarded message looks like when it leaves.
func TestForwardedMessage(t *testing.T) {
	d, st, _, fw := aliasSetup(t, "")
	msg := "From: Bob <bob@sender.test>\r\nReturn-Path: <forged@x.test>\r\nSubject: hello\r\n\r\nbody\r\n"
	e := env("both@example.test", "info@example.test")
	e.From = "bob@sender.test"
	if err := d.Deliver(context.Background(), e, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	// Local targets of both aliases get one copy each.
	if n := len(st.mail["acct-anna/inbox"]); n != 1 {
		t.Errorf("anna has %d copies", n)
	}
	if n := len(st.mail["acct-mike/inbox"]); n != 1 {
		t.Errorf("mike has %d copies", n)
	}
	if len(fw.sent) != 1 {
		t.Fatalf("forwards: %+v", fw.sent)
	}
	got := fw.sent[0]
	// The envelope sender is the original one rewritten under the alias's
	// domain (SRS0), and reverses to the original.
	if !regexp.MustCompile(`^SRS0=[a-z2-7]{8}=[a-z2-7]{2}=sender\.test=bob@example\.test$`).MatchString(got.from) {
		t.Errorf("envelope sender %q", got.from)
	}
	if orig, err := d.SRS.Reverse(got.from); err != nil || orig != "bob@sender.test" {
		t.Errorf("Reverse: %q, %v", orig, err)
	}
	if got.to != "Friend@elsewhere.example" || got.domain != "example.test" {
		t.Errorf("recipient %q", got.to)
	}
	// Delivered-To names the alias; our Received line is there; no
	// Return-Path (RFC 5321 4.4: that is for final delivery); the rest of
	// the message is as it came, so a DKIM signature still verifies.
	if !strings.HasPrefix(got.raw, "Delivered-To: both@example.test\r\nReceived: from sender.test") {
		t.Errorf("top of the forwarded message:\n%s", got.raw)
	}
	if strings.Contains(got.raw, "anna@") || strings.Contains(got.raw, "mike@") {
		t.Errorf("the forwarded copy names the alias's other targets:\n%s", got.raw)
	}
	if strings.Contains(got.raw, "Return-Path") || !strings.HasSuffix(got.raw, "From: Bob <bob@sender.test>\r\nSubject: hello\r\n\r\nbody\r\n") {
		t.Errorf("forwarded message:\n%s", got.raw)
	}
	// The local copy has the Return-Path of final delivery.
	if !strings.HasPrefix(st.mail["acct-anna/inbox"][0], "Return-Path: <bob@sender.test>\r\nReceived:") {
		t.Errorf("local copy:\n%s", st.mail["acct-anna/inbox"][0])
	}

	// A notification (null reverse-path) is forwarded as one, and a sender
	// too long to rewrite goes out with the null sender.
	for from, want := range map[string]string{"": "", strings.Repeat("x", 60) + "@a-very-long-domain-name.example": ""} {
		fw.sent = nil
		e := env("away@example.test")
		e.From = from
		if err := d.Deliver(context.Background(), e, []byte("Subject: "+from+"\r\n\r\nx\r\n")); err != nil {
			t.Fatal(err)
		}
		if len(fw.sent) != 1 || fw.sent[0].from != want {
			t.Errorf("from %q: %+v", from, fw.sent)
		}
	}
}

// Loops. A message that comes back to the alias that forwarded it is
// refused (Delivered-To), as is one with too many Received fields (RFC
// 5321 6.3).
func TestForwardLoops(t *testing.T) {
	d, _, _, fw := aliasSetup(t, "")
	back := "Delivered-To: away@example.test\r\nReceived: by elsewhere\r\nSubject: again\r\n\r\nx\r\n"
	err := d.Deliver(context.Background(), env("away@example.test"), []byte(back))
	var se *smtp.Error
	if !errors.As(err, &se) || se.Code != 554 || se.Enhanced != "5.4.6" || len(fw.sent) != 0 {
		t.Errorf("a message that was here before: %v, %d forwarded", err, len(fw.sent))
	}
	// Another alias's mark does not stop it, nor does it stop local delivery.
	other := strings.Replace(back, "away@", "both@", 1)
	if err := d.Deliver(context.Background(), env("away@example.test"), []byte(other)); err != nil || len(fw.sent) != 1 {
		t.Errorf("another alias's Delivered-To: %v", err)
	}
	if err := d.Deliver(context.Background(), env("mike@example.test"), []byte(back)); err != nil {
		t.Errorf("local delivery of a message with Delivered-To: %v", err)
	}
	hops := strings.Repeat("Received: by somewhere\r\n", maxHops) + "Subject: far\r\n\r\nx\r\n"
	if err := d.Deliver(context.Background(), env("mike@example.test"), []byte(hops)); !errors.As(err, &se) || se.Enhanced != "5.4.6" {
		t.Errorf("%d hops: %v", maxHops, err)
	}
	fewer := strings.Repeat("Received: by somewhere\r\n", maxHops-1) + "Subject: far\r\n\r\nx\r\n"
	if err := d.Deliver(context.Background(), env("mike@example.test"), []byte(fewer)); err != nil {
		t.Errorf("%d hops: %v", maxHops-1, err)
	}
}

// Spam is not forwarded. With a local target the message is filed under
// Junk there; with only forwards the sender is told.
func TestSpamIsNotForwarded(t *testing.T) {
	const spam = `{"action":"add header","score":9,"milter":{}}`
	d, st, _, fw := aliasSetup(t, spam)
	if err := d.Deliver(context.Background(), env("both@example.test"), []byte("Subject: buy\r\n\r\nx\r\n")); err != nil {
		t.Fatal(err)
	}
	if len(st.mail["acct-anna/junk"]) != 1 || len(fw.sent) != 0 {
		t.Errorf("junk %d, forwarded %d", len(st.mail["acct-anna/junk"]), len(fw.sent))
	}
	err := d.Deliver(context.Background(), env("away@example.test"), []byte("Subject: buy more\r\n\r\nx\r\n"))
	if code(err) != 550 || len(fw.sent) != 0 {
		t.Errorf("spam for a forward-only alias: %v", err)
	}
}

// The way back: a bounce for a rewritten sender address goes to the
// original sender, and only a bounce may use such an address.
func TestBounceToRewrittenSender(t *testing.T) {
	d, _, _, fw := aliasSetup(t, "")
	rewritten, _ := d.SRS.Forward("bob@sender.test", "example.test")
	ctx := context.Background()

	// With a sender: refused, or the address would be an open relay.
	e := env()
	e.From = "spammer@x.test"
	if err := d.Rcpt(ctx, e, rewritten); code(err) != 550 {
		t.Errorf("mail with a sender to a rewritten address: %v", err)
	}
	e.From = ""
	if err := d.Rcpt(ctx, e, rewritten); err != nil {
		t.Fatalf("a bounce to a rewritten address: %v", err)
	}
	// Not ours, tampered with, or expired: no such user.
	other := &srs.Rewriter{Key: []byte("another server's key............"), Now: d.Now}
	forged, _ := other.Forward("victim@x.test", "example.test")
	for _, to := range []string{forged, strings.Replace(rewritten, "bob", "eve", 1), "SRS0=junk@example.test"} {
		if err := d.Rcpt(ctx, e, to); code(err) != 550 {
			t.Errorf("Rcpt(%s): %v", to, err)
		}
	}
	now := d.SRS
	d.SRS = &srs.Rewriter{Key: now.Key, Now: func() time.Time { return d.Now().Add(srs.MaxAge + 48*time.Hour) }}
	if err := d.Rcpt(ctx, e, rewritten); code(err) != 550 {
		t.Errorf("an expired rewritten address: %v", err)
	}
	d.SRS = now

	e.Rcpts = []string{rewritten}
	bounce := "From: MAILER-DAEMON@elsewhere.example\r\nSubject: Undelivered\r\n\r\nYour message could not be delivered.\r\n"
	if err := d.Deliver(ctx, e, []byte(bounce)); err != nil {
		t.Fatal(err)
	}
	if len(fw.sent) != 1 || fw.sent[0].to != "bob@sender.test" || fw.sent[0].from != "" || fw.sent[0].domain != "example.test" || strings.Contains(fw.sent[0].raw, "Delivered-To") {
		t.Fatalf("the bounce on its way back: %+v", fw.sent)
	}

	// The queue's own bounce of forwarded mail takes the same way (Local).
	fw.sent = nil
	if err := d.Local(ctx, "", rewritten, []byte("Subject: Undelivered Mail\r\n\r\nx\r\n")); err != nil || len(fw.sent) != 1 || fw.sent[0].to != "bob@sender.test" {
		t.Errorf("Local to a rewritten address: %v %+v", err, fw.sent)
	}
	// Mail between users here reaches an alias's targets too.
	fw.sent = nil
	if err := d.Local(ctx, "mike@example.test", "both@example.test", []byte("Subject: internal\r\n\r\nx\r\n")); err != nil || len(fw.sent) != 1 {
		t.Errorf("Local to an alias: %v %+v", err, fw.sent)
	}
}

// Without the alias machinery everything is as before: no alias, no relay.
func TestNoAliases(t *testing.T) {
	d, _ := setup(t, "")
	if err := d.Rcpt(context.Background(), env(), "info@example.test"); code(err) != 550 {
		t.Errorf("got %v", err)
	}
	if err := d.Rcpt(context.Background(), &smtp.Envelope{}, "SRS0=a=b=c=d@example.test"); code(err) != 550 {
		t.Errorf("got %v", err)
	}
}
