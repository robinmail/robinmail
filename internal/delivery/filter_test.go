package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/message"
	"github.com/mikepage/robinmail/internal/smtp"
	"github.com/mikepage/robinmail/internal/store"
)

// stored is one message a recStore was given.
type stored struct {
	account   string
	mailboxes []int64
	role      string
	keywords  []string
	raw       string
}

// recStore is fakeStore that also remembers how each message was stored.
type recStore struct {
	*fakeStore
	got []stored
}

func (r *recStore) Deliver(ctx context.Context, account string, m store.Message) (store.Email, error) {
	e, err := r.fakeStore.Deliver(ctx, account, m)
	if err == nil {
		r.got = append(r.got, stored{account, m.MailboxIDs, m.Role, m.Keywords, string(m.Raw)})
	}
	return e, err
}

// fakeFilters holds a script and mailboxes per account.
type fakeFilters struct {
	scripts map[string]string // account id -> script
	boxes   map[string][]store.Mailbox
	sent    map[string]time.Time // vacation keys
	away    map[string]store.VacationResponse
	now     func() time.Time
	fail    error
	reads   int
}

func (f *fakeFilters) ActiveSieveScript(_ context.Context, account string) (string, func() ([]byte, error), bool, error) {
	if f.fail != nil {
		return "", nil, false, f.fail
	}
	text, ok := f.scripts[account]
	if !ok {
		return "", nil, false, nil
	}
	return fmt.Sprintf("%s-%x", account, len(text)) + text[:min(len(text), 20)], func() ([]byte, error) { f.reads++; return []byte(text), nil }, true, nil
}

func (f *fakeFilters) Mailboxes(_ context.Context, account string) ([]store.Mailbox, error) {
	return f.boxes[account], nil
}

func (f *fakeFilters) CreateMailbox(_ context.Context, account string, parent int64, name string) (store.Mailbox, error) {
	id := int64(0)
	for _, have := range f.boxes[account] {
		id = max(id, have.ID)
	}
	m := store.Mailbox{ID: id + 1, ParentID: parent, Name: name}
	f.boxes[account] = append(f.boxes[account], m)
	return m, nil
}

func (f *fakeFilters) VacationResponse(_ context.Context, account string) (store.VacationResponse, error) {
	return f.away[account], f.fail
}

func (f *fakeFilters) VacationOnce(_ context.Context, account, key string, period time.Duration) (bool, error) {
	if at, ok := f.sent[account+key]; ok && f.now().Sub(at) < period {
		return false, nil
	}
	f.sent[account+key] = f.now()
	return true, nil
}

// reply is one automatic reply that was queued.
type reply struct{ account, to, raw string }

type filterEnv struct {
	d       *Deliverer
	st      *recStore
	filters *fakeFilters
	fw      *fakeForwarder
	replies []reply
	clock   time.Time
}

func filterSetup(t *testing.T, reply string) *filterEnv {
	d, base, _, fw := aliasSetup(t, reply)
	e := &filterEnv{d: d, st: &recStore{fakeStore: base}, fw: fw, clock: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	d.Store = e.st
	d.Now = func() time.Time { return e.clock }
	boxes := func() []store.Mailbox {
		return []store.Mailbox{{ID: 1, Name: "Inbox", Role: store.RoleInbox}, {ID: 4, Name: "Junk", Role: store.RoleJunk},
			{ID: 6, Name: "Work"}, {ID: 7, ParentID: 6, Name: "Invoices"}}
	}
	e.filters = &fakeFilters{scripts: map[string]string{}, sent: map[string]time.Time{}, now: d.Now,
		boxes: map[string][]store.Mailbox{"acct-mike": boxes(), "acct-anna": boxes()}}
	d.Filters = e.filters
	d.AutoReply = func(_ context.Context, acct store.Account, to string, raw []byte) error {
		e.replies = append(e.replies, struct{ account, to, raw string }{acct.ID, to, string(raw)})
		return nil
	}
	d.SendAs = func(_ context.Context, addr string) ([]string, error) {
		if addr == "mike@example.test" {
			return []string{"info@example.test"}, nil
		}
		return nil, nil
	}
	return e
}

func (e *filterEnv) deliver(t *testing.T, rcpt, msg string) error {
	t.Helper()
	return e.d.Deliver(context.Background(), env(rcpt), []byte(msg))
}

const plainMsg = "From: Bob <a@sender.test>\r\nTo: mike@example.test\r\nSubject: Invoice 42\r\nMessage-ID: <m1@sender.test>\r\n\r\nPlease pay.\r\n"

// What each action of a script does to a delivery.
func TestFilterActions(t *testing.T) {
	for _, c := range []struct {
		name, script string
		mailboxes    string // ids, or "role:<role>" for the default mailbox
		keywords     string
		forwards     string
	}{
		{"no script", "", "role:inbox", "", ""},
		{"keep", `keep;`, "role:inbox", "", ""},
		{"fileinto", `require "fileinto"; fileinto "Work/Invoices";`, "[7]", "", ""},
		{"fileinto, any case", `require "fileinto"; fileinto "work/invoices";`, "[7]", "", ""},
		{"INBOX by its IMAP name", `require "fileinto"; fileinto "INBOX";`, "[1]", "", ""},
		// Filed into several mailboxes: one Email with all of them as labels.
		{"two mailboxes", `require "fileinto"; fileinto "Work"; fileinto "Work/Invoices";`, "[6 7]", "", ""},
		{"fileinto and keep", `require ["fileinto", "copy"]; fileinto :copy "Work";`, "[6 1]", "", ""},
		// RFC 5228 4.1: a mailbox that is not there; the message is kept.
		{"a mailbox that is not there", `require "fileinto"; fileinto "Nope";`, "[1]", "", ""},
		{":create", `require ["fileinto", "mailbox"]; fileinto :create "Lists/Dev";`, "[9]", "", ""},
		{"flags become keywords", `require ["imap4flags", "fileinto"]; addflag ["\\Seen", "$Label1", "\\Recent", "bad(flag"]; fileinto "Work";`, "[6]", "$seen $label1", ""},
		{"mark read and keep", `require "imap4flags"; setflag "\\Seen"; keep;`, "role:inbox", "$seen", ""},
		{"discard", `discard;`, "", "", ""},
		{"redirect", `redirect "elsewhere@other.example";`, "", "", "elsewhere@other.example"},
		{"redirect :copy", `require "copy"; redirect :copy "elsewhere@other.example";`, "role:inbox", "", "elsewhere@other.example"},
		// A script that fails at run time keeps the message (RFC 5228 2.10.6).
		{"a failing script", `require ["reject", "fileinto"]; fileinto "Work"; reject "no";`, "role:inbox", "", ""},
	} {
		e := filterSetup(t, "")
		if c.script != "" {
			e.filters.scripts["acct-mike"] = c.script
		}
		if err := e.deliver(t, "mike@example.test", plainMsg); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		got := ""
		if len(e.st.got) == 1 {
			got = fmt.Sprint(e.st.got[0].mailboxes)
			if len(e.st.got[0].mailboxes) == 0 {
				got = "role:" + e.st.got[0].role
			}
			if kw := strings.Join(e.st.got[0].keywords, " "); kw != c.keywords {
				t.Errorf("%s: keywords %q, want %q", c.name, kw, c.keywords)
			}
		}
		if got != c.mailboxes {
			t.Errorf("%s: stored in %s, want %s", c.name, got, c.mailboxes)
		}
		var fwd []string
		for _, f := range e.fw.sent {
			fwd = append(fwd, f.to)
			// A redirect leaves like a forward: the sender rewritten
			// under the account's domain, Delivered-To the account.
			if !strings.HasPrefix(f.from, "SRS0=") || !strings.HasPrefix(f.raw, "Delivered-To: mike@example.test\r\n") {
				t.Errorf("%s: redirect left as %q:\n%.80s", c.name, f.from, f.raw)
			}
		}
		if strings.Join(fwd, " ") != c.forwards {
			t.Errorf("%s: forwards %v, want %s", c.name, fwd, c.forwards)
		}
	}
}

// The script sees the envelope and the message as it is stored.
func TestFilterSeesEnvelopeAndTrace(t *testing.T) {
	e := filterSetup(t, "")
	e.filters.scripts["acct-mike"] = `require ["envelope", "subaddress", "fileinto"];
if allof (envelope :detail "to" "bills", envelope :is "from" "a@sender.test", header :contains "received" "mx.example.test") {
  fileinto "Work/Invoices";
}`
	if err := e.deliver(t, "mike+bills@example.test", plainMsg); err != nil || len(e.st.got) != 1 || fmt.Sprint(e.st.got[0].mailboxes) != "[7]" {
		t.Fatalf("with the tag: %v %+v", err, e.st.got)
	}
	e.st.got = nil
	if err := e.deliver(t, "mike@example.test", strings.Replace(plainMsg, "42", "43", 1)); err != nil || e.st.got[0].role != "inbox" || len(e.st.got[0].mailboxes) != 0 {
		t.Fatalf("without the tag: %v %+v", err, e.st.got)
	}
	// A parsed script is kept: two messages, one read of the script.
	if e.filters.reads != 1 {
		t.Errorf("the script was read %d times", e.filters.reads)
	}
	// Through an alias the envelope recipient is the alias.
	e.filters.scripts["acct-anna"] = `require ["envelope", "fileinto"]; if envelope :localpart "to" "sales-nl" { fileinto "Work"; }`
	e.st.got = nil
	if err := e.deliver(t, "sales-nl@example.test", plainMsg); err != nil || len(e.st.got) != 1 || fmt.Sprint(e.st.got[0].mailboxes) != "[6]" {
		t.Fatalf("through an alias: %v %+v", err, e.st.got)
	}
}

// Spam: the default mailbox is Junk, also for a script that keeps; a
// script decides otherwise by filing; nothing is redirected or answered.
func TestFilterAndSpam(t *testing.T) {
	const spam = `{"action":"add header","score":9,"milter":{}}`
	e := filterSetup(t, spam)
	e.filters.scripts["acct-mike"] = `require ["fileinto", "vacation"];
vacation "I am away";
if header :contains "subject" "invoice" { fileinto "Work"; }
elsif header :contains "subject" "forward" { redirect "elsewhere@other.example"; }`
	if err := e.deliver(t, "mike@example.test", plainMsg); err != nil || fmt.Sprint(e.st.got[0].mailboxes) != "[6]" {
		t.Fatalf("spam the script files: %v %+v", err, e.st.got)
	}
	e.st.got = nil
	if err := e.deliver(t, "mike@example.test", "From: a@sender.test\r\nTo: mike@example.test\r\nSubject: other\r\n\r\nx\r\n"); err != nil || e.st.got[0].role != "junk" {
		t.Fatalf("spam the script keeps: %v %+v", err, e.st.got)
	}
	e.st.got = nil
	if err := e.deliver(t, "mike@example.test", "From: a@sender.test\r\nTo: mike@example.test\r\nSubject: forward me\r\n\r\nx\r\n"); err != nil ||
		len(e.fw.sent) != 0 || len(e.st.got) != 1 || e.st.got[0].role != "junk" {
		t.Fatalf("spam the script redirects: %v, %d forwards, %+v", err, len(e.fw.sent), e.st.got)
	}
	if len(e.replies) != 0 {
		t.Errorf("%d automatic replies to spam", len(e.replies))
	}
}

// reject and ereject (RFC 5429 2.1): the refusal is the SMTP reply when
// the message was for this account alone, and a notification otherwise.
func TestFilterReject(t *testing.T) {
	e := filterSetup(t, "")
	e.filters.scripts["acct-mike"] = "require \"ereject\";\r\nereject text:\r\nI no longer accept\r\nmail from this address\r\n.\r\n;"
	err := e.deliver(t, "mike@example.test", plainMsg)
	var se *smtp.Error
	if !errors.As(err, &se) || se.Code != 550 || se.Enhanced != "5.7.1" || se.Msg != "I no longer accept mail from this address" {
		t.Fatalf("alone: %v", err)
	}
	if len(e.st.got) != 0 || len(e.fw.sent) != 0 {
		t.Fatalf("a refused message was stored or answered: %+v %+v", e.st.got, e.fw.sent)
	}
	// Two recipients: Anna has it, Mike does not, the sender is told.
	if err := e.d.Deliver(context.Background(), env("mike@example.test", "anna@example.test"), []byte(plainMsg)); err != nil {
		t.Fatal(err)
	}
	if len(e.st.got) != 1 || e.st.got[0].account != "acct-anna" {
		t.Fatalf("stored: %+v", e.st.got)
	}
	if len(e.fw.sent) != 1 || e.fw.sent[0].to != "a@sender.test" || e.fw.sent[0].from != "" {
		t.Fatalf("the notification: %+v", e.fw.sent)
	}
	dsn, ok := message.ParseDSN(message.Parse([]byte(e.fw.sent[0].raw)))
	if !ok || len(dsn.Recipients) != 1 || dsn.Recipients[0].Address != "mike@example.test" || dsn.Recipients[0].Status != "5.7.1" ||
		!strings.Contains(dsn.Recipients[0].Diagnostic, "I no longer accept mail from this address") || dsn.OriginalMessageID != "m1@sender.test" {
		t.Errorf("the notification is not the DSN it should be: %+v %v", dsn, ok)
	}
	// A retry of the sender tells nobody twice.
	if err := e.d.Deliver(context.Background(), env("mike@example.test", "anna@example.test"), []byte(plainMsg)); err != nil || len(e.fw.sent) != 1 {
		t.Errorf("after a retry: %v, %d notifications", err, len(e.fw.sent))
	}
	// A notification that is refused gets none in return.
	e.fw.sent = nil
	bounce := env("mike@example.test", "anna@example.test")
	bounce.From = ""
	if err := e.d.Deliver(context.Background(), bounce, []byte("Subject: Undelivered\r\n\r\nx\r\n")); err != nil || len(e.fw.sent) != 0 {
		t.Errorf("a refused notification: %v, %+v", err, e.fw.sent)
	}
	// Mail between users here (the queue's Local): the refusal goes back
	// to the queue, which bounces to the sender.
	if err := e.d.Local(context.Background(), "anna@example.test", "mike@example.test", []byte("Subject: local\r\n\r\nx\r\n")); code(err) != 550 {
		t.Errorf("Local: %v", err)
	}
}

// A redirect that comes back is kept instead of sent round again.
func TestFilterRedirectLoop(t *testing.T) {
	e := filterSetup(t, "")
	e.filters.scripts["acct-mike"] = `redirect "elsewhere@other.example";`
	back := "Delivered-To: mike@example.test\r\n" + plainMsg
	if err := e.deliver(t, "mike@example.test", back); err != nil || len(e.fw.sent) != 0 || len(e.st.got) != 1 {
		t.Fatalf("a message that was redirected before: %v, %d forwards, %+v", err, len(e.fw.sent), e.st.got)
	}
}

// A script that cannot be read defers the mail; one that no longer parses
// is ignored.
func TestFilterFailures(t *testing.T) {
	e := filterSetup(t, "")
	e.filters.fail = errors.New("rqlite is down")
	if err := e.deliver(t, "mike@example.test", plainMsg); code(err) != 451 || len(e.st.got) != 0 {
		t.Errorf("store down: %v", err)
	}
	e.filters.fail = nil
	e.filters.scripts["acct-mike"] = `this is not sieve`
	if err := e.deliver(t, "mike@example.test", plainMsg); err != nil || len(e.st.got) != 1 || e.st.got[0].role != "inbox" {
		t.Errorf("a script that does not parse: %v %+v", err, e.st.got)
	}
}

// RFC 5230 and RFC 3834: when an automatic reply goes out, and what it is.
func TestVacationRules(t *testing.T) {
	const script = `require "vacation"; vacation :days 3 :addresses "mike.page@old.example" "I am away until Monday.";`
	base := func(extra string) string {
		return "From: Bob <a@sender.test>\r\nTo: Mike <mike@example.test>\r\nSubject: Lunch?\r\nMessage-ID: <q1@sender.test>\r\n" +
			"References: <q0@sender.test>\r\n" + extra + "\r\nAre you free?\r\n"
	}
	for _, c := range []struct {
		name, from, rcpt, msg string
		want                  bool
	}{
		{"an ordinary message", "a@sender.test", "mike@example.test", base(""), true},
		// RFC 5230 4.5: the recipient must be named in To, Cc, Bcc or Resent-*.
		{"named in Cc", "a@sender.test", "mike@example.test", strings.Replace(base("Cc: MIKE@example.test\r\n"), "To: Mike <mike@example.test>", "To: other@x.test", 1), true},
		{"named by a tagged address", "a@sender.test", "mike@example.test", strings.Replace(base(""), "mike@example.test", "mike+shop@example.test", 1), true},
		{"named by an alias of the account", "a@sender.test", "mike@example.test", strings.Replace(base(""), "mike@example.test", "info@example.test", 1), true},
		{"named by :addresses", "a@sender.test", "mike@example.test", strings.Replace(base(""), "mike@example.test", "mike.page@old.example", 1), true},
		{"named by Resent-To", "a@sender.test", "mike@example.test", strings.Replace(base("Resent-To: mike@example.test\r\n"), "To: Mike <mike@example.test>", "To: other@x.test", 1), true},
		{"not named (a Bcc, or a list)", "a@sender.test", "mike@example.test", strings.Replace(base(""), "mike@example.test", "undisclosed@x.test", 1), false},
		// RFC 5230 4.6, RFC 3834 2.
		{"the null reverse-path", "", "mike@example.test", base(""), false},
		{"MAILER-DAEMON", "MAILER-DAEMON@sender.test", "mike@example.test", base(""), false},
		{"owner-", "owner-list@sender.test", "mike@example.test", base(""), false},
		{"-request", "list-request@sender.test", "mike@example.test", base(""), false},
		{"LISTSERV", "listserv@sender.test", "mike@example.test", base(""), false},
		{"majordomo", "Majordomo@sender.test", "mike@example.test", base(""), false},
		{"no-reply", "no-reply@sender.test", "mike@example.test", base(""), false},
		{"Auto-Submitted: auto-replied", "a@sender.test", "mike@example.test", base("Auto-Submitted: auto-replied\r\n"), false},
		{"Auto-Submitted: auto-generated", "a@sender.test", "mike@example.test", base("Auto-Submitted: auto-generated; x=y\r\n"), false},
		{"Auto-Submitted: no", "a@sender.test", "mike@example.test", base("Auto-Submitted: no\r\n"), true},
		{"List-Id", "a@sender.test", "mike@example.test", base("List-Id: <dev.lists.example>\r\n"), false},
		{"List-Unsubscribe", "a@sender.test", "mike@example.test", base("List-Unsubscribe: <mailto:x@y.example>\r\n"), false},
		{"Precedence: bulk", "a@sender.test", "mike@example.test", base("Precedence: bulk\r\n"), false},
		{"Precedence: list", "a@sender.test", "mike@example.test", base("Precedence: List\r\n"), false},
		{"Precedence: first-class", "a@sender.test", "mike@example.test", base("Precedence: first-class\r\n"), true},
		{"X-Auto-Response-Suppress", "a@sender.test", "mike@example.test", base("X-Auto-Response-Suppress: OOF\r\n"), false},
		{"from the user's own address", "mike@example.test", "mike@example.test", base(""), false},
	} {
		e := filterSetup(t, "")
		e.filters.scripts["acct-mike"] = script
		en := env(c.rcpt)
		en.From = c.from
		if err := e.d.Deliver(context.Background(), en, []byte(c.msg)); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := len(e.replies) == 1; got != c.want {
			t.Errorf("%s: replied %v, want %v", c.name, got, c.want)
		}
		if len(e.st.got) != 1 {
			t.Errorf("%s: the message itself was not stored", c.name)
		}
	}
}

func TestVacationReplyAndPeriod(t *testing.T) {
	e := filterSetup(t, "")
	e.filters.scripts["acct-mike"] = "require \"vacation\";\r\nvacation :days 3 text:\r\nI am away until Monday.\r\nMike\r\n.\r\n;"
	msg := func(n int) string {
		return fmt.Sprintf("From: Bob <a@sender.test>\r\nTo: mike@example.test\r\nSubject: =?utf-8?q?Caf=C3=A9_%d?=\r\nMessage-ID: <q%d@sender.test>\r\nReferences: <q0@sender.test>\r\n\r\nx\r\n", n, n)
	}
	if err := e.deliver(t, "mike@example.test", msg(1)); err != nil || len(e.replies) != 1 {
		t.Fatalf("first message: %v, %d replies", err, len(e.replies))
	}
	r := e.replies[0]
	if r.account != "acct-mike" || r.to != "a@sender.test" {
		t.Errorf("the reply is for %s to %s", r.account, r.to)
	}
	m := message.Parse([]byte(r.raw))
	get := func(name string) string {
		v, _ := message.Get(m.Headers, name)
		return strings.TrimSpace(message.Text(v))
	}
	for name, want := range map[string]string{
		"From":           "mike@example.test", // RFC 5230 5.3
		"To":             "a@sender.test",     // 5.4: the envelope sender
		"Subject":        "Auto: Café 1",      // 5.2
		"In-Reply-To":    "<q1@sender.test>",  // 5.8
		"References":     "<q0@sender.test> <q1@sender.test>",
		"Auto-Submitted": "auto-replied", // RFC 3834 5
	} {
		if got := get(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if get("Date") == "" || get("Message-ID") == "" || get("MIME-Version") != "1.0" {
		t.Errorf("Date, Message-ID or MIME-Version missing:\n%s", r.raw)
	}
	if body := m.BodyText(1000); body != "I am away until Monday.\nMike\n" {
		t.Errorf("body %q", body)
	}

	// RFC 5230 4.2: one reply per sender in :days days, whatever node asks.
	e.clock = e.clock.Add(3*24*time.Hour - time.Minute)
	if err := e.deliver(t, "mike@example.test", msg(2)); err != nil || len(e.replies) != 1 {
		t.Fatalf("within the period: %v, %d replies", err, len(e.replies))
	}
	// Another sender has not had one.
	other := env("mike@example.test")
	other.From = "carl@elsewhere.test"
	if err := e.d.Deliver(context.Background(), other, []byte(msg(3))); err != nil || len(e.replies) != 2 {
		t.Fatalf("another sender: %v, %d replies", err, len(e.replies))
	}
	e.clock = e.clock.Add(2 * time.Minute)
	if err := e.deliver(t, "mike@example.test", msg(4)); err != nil || len(e.replies) != 3 {
		t.Fatalf("after the period: %v, %d replies", err, len(e.replies))
	}

	// 4.3 and 5.3: :from when it is the user's (an alias here), else the
	// account's address; 4.2: :subject; 4.5: :mime.
	e2 := filterSetup(t, "")
	e2.filters.scripts["acct-mike"] = "require \"vacation\";\r\nvacation :from \"The Office <info@example.test>\" :subject \"Away\" :mime text:\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n<p>Away</p>\r\n.\r\n;"
	if err := e2.deliver(t, "mike@example.test", msg(1)); err != nil || len(e2.replies) != 1 {
		t.Fatalf("mime: %v", err)
	}
	m = message.Parse([]byte(e2.replies[0].raw))
	from, _ := message.Get(m.Headers, "From")
	if list := message.Addresses(from); len(list) != 1 || list[0].Email != "info@example.test" || *list[0].Name != "The Office" {
		t.Errorf("From with :from: %q", from)
	}
	if subj, _ := message.Get(m.Headers, "Subject"); strings.TrimSpace(subj) != "Away" {
		t.Errorf("Subject with :subject: %q", subj)
	}
	if m.Root.Type != "text/html" || !strings.Contains(string(m.Root.Content()), "<p>Away</p>") || strings.Count(e2.replies[0].raw, "Content-Type") != 1 {
		t.Errorf("the :mime reply:\n%s", e2.replies[0].raw)
	}
	e3 := filterSetup(t, "")
	e3.filters.scripts["acct-mike"] = `require "vacation"; vacation :from "boss@example.test" "x";`
	e3.deliver(t, "mike@example.test", msg(1))
	if from, _ := message.Get(message.Parse([]byte(e3.replies[0].raw)).Headers, "From"); strings.TrimSpace(from) != "mike@example.test" {
		t.Errorf("From with somebody else's :from: %q", from)
	}

	// The period is cut to the limits: :days 400 is 30 days.
	e4 := filterSetup(t, "")
	e4.filters.scripts["acct-mike"] = `require "vacation"; vacation :days 400 "x";`
	e4.deliver(t, "mike@example.test", msg(1))
	e4.clock = e4.clock.Add(30*24*time.Hour + time.Minute)
	e4.deliver(t, "mike@example.test", msg(2))
	if len(e4.replies) != 2 {
		t.Errorf("after 30 days with :days 400: %d replies", len(e4.replies))
	}
}

// RFC 5230 4.2: the handle names the reply; the same handle counts as the
// same reply, a different reason without one as another.
func TestVacationKey(t *testing.T) {
	e := filterSetup(t, "")
	e.filters.scripts["acct-mike"] = `require "vacation";
if header :contains "subject" "lunch" {
    vacation :handle "ran-away" "I'm out and can't meet for lunch";
} else {
    vacation :handle "ran-away" "I'm out";
}`
	lunch := "From: a@sender.test\r\nTo: mike@example.test\r\nSubject: lunch\r\n\r\nx\r\n"
	e.deliver(t, "mike@example.test", lunch)
	e.deliver(t, "mike@example.test", strings.Replace(lunch, "lunch", "dinner", 1))
	if len(e.replies) != 1 {
		t.Errorf("same handle: %d replies", len(e.replies))
	}
	e = filterSetup(t, "")
	e.filters.scripts["acct-mike"] = `require "vacation";
if header :contains "subject" "lunch" { vacation "I'm out and can't meet for lunch"; } else { vacation "I'm out"; }`
	e.deliver(t, "mike@example.test", lunch)
	e.deliver(t, "mike@example.test", strings.Replace(lunch, "lunch", "dinner", 1))
	e.deliver(t, "mike@example.test", strings.Replace(lunch, "lunch", "supper", 1))
	if len(e.replies) != 2 {
		t.Errorf("two reasons without a handle: %d replies", len(e.replies))
	}
}

// RFC 8621 8: the vacation response set over JMAP is sent by the rules of
// the vacation action, while it is enabled and between its dates.
func TestVacationResponseAtDelivery(t *testing.T) {
	str := func(s string) *string { return &s }
	msg := func(n int) string {
		return fmt.Sprintf("From: Bob <a@sender.test>\r\nTo: mike@example.test\r\nSubject: Question %d\r\nMessage-ID: <v%d@sender.test>\r\n\r\nx\r\n", n, n)
	}
	e := filterSetup(t, "")
	from, to := e.clock.Add(-time.Hour), e.clock.Add(time.Hour)
	e.filters.away = map[string]store.VacationResponse{"acct-mike": {Enabled: true, From: &from, To: &to,
		Subject: str("Out of office"), TextBody: str("Back on Monday.")}}
	if err := e.deliver(t, "mike@example.test", msg(1)); err != nil || len(e.replies) != 1 || len(e.st.got) != 1 {
		t.Fatalf("enabled: %v, %d replies, %d stored", err, len(e.replies), len(e.st.got))
	}
	m := message.Parse([]byte(e.replies[0].raw))
	subject, _ := message.Get(m.Headers, "Subject")
	auto, _ := message.Get(m.Headers, "Auto-Submitted")
	if strings.TrimSpace(subject) != "Out of office" || strings.TrimSpace(auto) != "auto-replied" || m.BodyText(100) != "Back on Monday." {
		t.Errorf("the reply:\n%s", e.replies[0].raw)
	}
	// Once per sender, also after the text changed.
	e.filters.away["acct-mike"] = store.VacationResponse{Enabled: true, TextBody: str("Back on Tuesday.")}
	if e.deliver(t, "mike@example.test", msg(2)); len(e.replies) != 1 {
		t.Errorf("a second reply to the same sender: %d", len(e.replies))
	}
	// The rules of RFC 3834 hold for it as for a script's vacation.
	list := strings.Replace(msg(3), "Subject:", "List-Id: <x.example>\r\nSubject:", 1)
	other := env("mike@example.test")
	other.From = "carl@elsewhere.test"
	if e.d.Deliver(context.Background(), other, []byte(list)); len(e.replies) != 1 {
		t.Errorf("a reply to list mail: %d", len(e.replies))
	}

	// Not enabled, before fromDate, at toDate: no reply.
	for name, v := range map[string]store.VacationResponse{
		"not enabled":     {TextBody: str("x")},
		"before fromDate": {Enabled: true, From: &to},
		"at toDate":       {Enabled: true, To: &e.clock},
	} {
		e := filterSetup(t, "")
		e.filters.away = map[string]store.VacationResponse{"acct-mike": v}
		if e.deliver(t, "mike@example.test", msg(1)); len(e.replies) != 0 {
			t.Errorf("%s: %d replies", name, len(e.replies))
		}
	}

	// Without a subject and a body the server fills them in; with HTML
	// the reply has both forms, the text made from the HTML.
	e = filterSetup(t, "")
	e.filters.away = map[string]store.VacationResponse{"acct-mike": {Enabled: true}}
	e.deliver(t, "mike@example.test", msg(1))
	m = message.Parse([]byte(e.replies[0].raw))
	if subject, _ := message.Get(m.Headers, "Subject"); strings.TrimSpace(subject) != "Auto: Question 1" || !strings.Contains(m.BodyText(200), "automatic reply") {
		t.Errorf("defaults:\n%s", e.replies[0].raw)
	}
	e = filterSetup(t, "")
	e.filters.away = map[string]store.VacationResponse{"acct-mike": {Enabled: true, HTMLBody: str("<p>Back on <b>Monday</b>.</p><script>x()</script>")}}
	e.deliver(t, "mike@example.test", msg(1))
	m = message.Parse([]byte(e.replies[0].raw))
	if m.Root.Type != "multipart/alternative" || len(m.Leaves) != 2 || m.Leaves[1].Type != "text/html" ||
		strings.TrimSpace(m.BodyText(100)) != "Back on Monday." || strings.Count(e.replies[0].raw, "MIME-Version") != 1 {
		t.Errorf("an HTML response:\n%s", e.replies[0].raw)
	}

	// A script's own vacation comes first; one that refuses the message
	// gets no reply sent either.
	e = filterSetup(t, "")
	e.filters.away = map[string]store.VacationResponse{"acct-mike": {Enabled: true, TextBody: str("from JMAP")}}
	e.filters.scripts["acct-mike"] = `require "vacation"; vacation "from the script";`
	e.deliver(t, "mike@example.test", msg(1))
	if len(e.replies) != 1 || !strings.Contains(e.replies[0].raw, "from the script") {
		t.Errorf("script and response: %+v", e.replies)
	}
	// A script without vacation leaves the response in place.
	e = filterSetup(t, "")
	e.filters.away = map[string]store.VacationResponse{"acct-mike": {Enabled: true, TextBody: str("from JMAP")}}
	e.filters.scripts["acct-mike"] = `require "fileinto"; fileinto "Work";`
	e.deliver(t, "mike@example.test", msg(1))
	if len(e.replies) != 1 || !strings.Contains(e.replies[0].raw, "from JMAP") || fmt.Sprint(e.st.got[0].mailboxes) != "[6]" {
		t.Errorf("a filing script and the response: %+v %+v", e.replies, e.st.got)
	}
}
