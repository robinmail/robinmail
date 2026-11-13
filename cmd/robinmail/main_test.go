package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCommands walks through phase 1's exit: deliver from the command line,
// move and label a message, and see the ids hold and the change log fill.
// It needs a real rqlite, see internal/store.
func TestCommands(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "robinmail.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"hostname":"mail.example.test","data_dir":"data",
		"domains":["example.test"],"rqlite":{"url":%q}}`, base)), 0o600)
	var r [6]byte
	rand.Read(r[:])
	user := "cli-" + hex.EncodeToString(r[:]) + "@example.test"

	do := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := run(append([]string{"-config", cfg}, args...), strings.NewReader(stdin), &out)
		return out.String(), err
	}
	ok := func(stdin string, args ...string) string {
		t.Helper()
		out, err := do(stdin, args...)
		if err != nil {
			t.Fatalf("robinmail %s: %v", strings.Join(args, " "), err)
		}
		return out
	}
	expect := func(out string, patterns ...string) {
		t.Helper()
		for _, p := range patterns {
			if !regexp.MustCompile(p).MatchString(out) {
				t.Errorf("output does not match %q:\n%s", p, out)
			}
		}
	}

	if _, err := do("short\n", "user", "add", user); err == nil {
		t.Error("accepted a weak password")
	}
	expect(ok("correct horse\n", "user", "add", user), `^created `+user)
	if _, err := do("correct horse\n", "user", "add", user); err == nil {
		t.Error("created the same user twice")
	}
	if _, err := do("correct horse\n", "user", "add", "x@elsewhere.test"); err == nil {
		t.Error("created a user in a foreign domain")
	}
	expect(ok("", "user", "list"), regexp.QuoteMeta(user)+` +account [a-z2-7]{16}  state 1\n`)

	first := "From: Anna <anna@sender.test>\r\nSubject: first\r\nMessage-ID: <1@sender.test>\r\n\r\nhello\r\n"
	reply := "From: Bob <bob@sender.test>\r\nSubject: Re: first\r\nMessage-ID: <2@sender.test>\r\nIn-Reply-To: <1@sender.test>\r\n\r\nhi\r\n"
	expect(ok(first, "deliver", user), `Inbox: email e1, thread t1, uid 1, \d+ bytes, state 2\n`)
	expect(ok(reply, "deliver", user, "inbox"), `email e2, thread t1, uid 2, .* state 3\n`)
	if out := ok("", "cat", user, "e1"); out != first {
		t.Errorf("cat e1 = %q", out)
	}

	expect(ok("", "mailbox", "create", user, "Work"), `created mailbox m6 Work, state 4\n`)
	expect(ok("", "mailbox", "create", user, "Work/Invoices"), `created mailbox m7 Work/Invoices, state 5\n`)
	// Move e1 to Work, label e2 with Trash, mark e1 read.
	expect(ok("", "move", user, "e1", "inbox", "Work"), `moved e1 from Inbox to Work: now in m6 uid 1; keywords \[\]; state 6\n`)
	expect(ok("", "label", user, "e2", "trash"), `now in m1 uid 2, m5 uid 1; .* state 7\n`)
	expect(ok("", "flag", user, "e1", "$seen", "todo"), `keywords \[\$seen todo\]; state 8\n`)
	expect(ok("", "mailbox", "rename", user, "m6", "Werk"), `renamed mailbox m6 to Werk, state 9\n`)

	// The ids held: e1 is still e1 in thread t1, now in the renamed mailbox.
	expect(ok("", "list", user, "Werk"), `Werk \(m6\): 1 emails`, `(?m)^e1 +uid 1 +t1 +m6 .* first +\$seen todo$`)
	expect(ok("", "list", user), `Inbox \(m1\): 1 emails, uidvalidity \d+, uidnext 3`, `(?m)^e2 +uid 2 +t1 +m1,m5 .*Bob.*Re: first`)
	expect(ok("", "mailbox", "list", user),
		`(?m)^m1 +inbox +1 +1 +3 `, `(?m)^m5 +trash +1 +1 +2 `, `(?m)^m6 +1 +0 +2 .* Werk\nm7 .* Werk/Invoices$`)
	if out := ok("", "cat", user, "e1"); out != first {
		t.Errorf("cat e1 after the move = %q", out)
	}

	// The change log filled.
	expect(ok("", "changes", user),
		`state 0 -> 9\n`, `Email +created +e1 e2\n`, `Mailbox +created +m1 m2 m3 m4 m5 m6 m7\n`, `Thread +created +t1\n`)
	expect(ok("", "changes", user, "5"),
		`state 5 -> 9\n`, `Email +updated +e1 e2\n`, `Mailbox +updated +m1 m5 m6\n`)
	if out := ok("", "changes", user, "5"); strings.Contains(out, "Thread") || strings.Contains(out, "created") {
		t.Errorf("changes since 5:\n%s", out)
	}

	expect(ok("", "unlabel", user, "e2", "inbox"), `removed e2 from Inbox: now in m5 uid 1;`)
	if _, err := do("", "unlabel", user, "e2", "trash"); err == nil {
		t.Error("removed an email from its last mailbox")
	}
	expect(ok("", "rm", user, "e2"), `destroyed e2, state 11\n`)
	expect(ok("", "changes", user, "9"), `Email +destroyed +e2\n`, `Thread +updated +t1\n`)
	expect(ok("", "trim"), `removed \d+ change rows and \d+ delivery keys\n`)

	for _, bad := range [][]string{
		{"cat", user, "e2"}, {"cat", user, "2"}, {"list", user, "Nope"}, {"list", "nobody@example.test"},
		{"move", user, "e1", "inbox", "trash"}, {"changes", user, "99"}, {"changes", user, "x"}, {"nonsense"}, {"user"},
	} {
		if _, err := do("", bad...); err == nil {
			t.Errorf("robinmail %s: no error", strings.Join(bad, " "))
		}
	}
}
