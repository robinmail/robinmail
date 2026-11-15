package main

import (
	"bytes"
	"context"
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
func TestCommands(t *testing.T) {
	c := newCLI(t, "")
	do, ok, expect, dir := c.do, c.ok, c.expect, c.dir
	// A domain of its own, so the test can add and remove it.
	domain := "cli-" + randomHex() + ".test"
	user := "anna@" + domain

	// Nothing works without a master key, and the key is never overwritten.
	if _, err := do("", "user", "list"); err == nil || !strings.Contains(err.Error(), "key generate") {
		t.Errorf("ran without a master key: %v", err)
	}
	expect(ok("", "key", "generate"), `^wrote .*/master\.key\n`)
	if _, err := do("", "key", "generate"); err == nil {
		t.Error("overwrote the master key")
	}

	// Users only exist in accepted domains, which live in the domain table.
	if _, err := do("correct horse\n", "user", "add", user); err == nil || !strings.Contains(err.Error(), "domain add") {
		t.Errorf("created a user in a domain that was not added: %v", err)
	}
	expect(ok("", "domain", "add", strings.ToUpper(domain)), `^added `+regexp.QuoteMeta(domain))
	if _, err := do("", "domain", "add", domain); err == nil {
		t.Error("added the same domain twice")
	}
	if _, err := do("", "domain", "add", "not a domain"); err == nil {
		t.Error("added an invalid domain")
	}
	expect(ok("", "domain", "list"), `(?m)^`+regexp.QuoteMeta(domain)+`$`)
	expect(ok("", "domain", "add", "empty."+domain), `^added `)
	expect(ok("", "domain", "remove", "empty."+domain), `^removed empty\.`)
	if _, err := do("", "domain", "remove", "empty."+domain); err == nil {
		t.Error("removed a domain twice")
	}

	if _, err := do("short\n", "user", "add", user); err == nil {
		t.Error("accepted a weak password")
	}
	expect(ok("correct horse\n", "user", "add", user), `^created `+user)
	if _, err := do("correct horse\n", "user", "add", user); err == nil {
		t.Error("created the same user twice")
	}
	if _, err := do("correct horse\n", "user", "add", "x@empty."+domain); err == nil {
		t.Error("created a user in a removed domain")
	}
	if _, err := do("", "domain", "remove", domain); err == nil || !strings.Contains(err.Error(), "still has users") {
		t.Errorf("removed a domain with a user: %v", err)
	}
	expect(ok("", "user", "list"), regexp.QuoteMeta(user)+` +account [a-z2-7]{16}  state 1\n`)

	first := "From: Anna <anna@sender.test>\r\nSubject: first\r\nMessage-ID: <1@sender.test>\r\n\r\nhello\r\n"
	reply := "From: Bob <bob@sender.test>\r\nSubject: Re: first\r\nMessage-ID: <2@sender.test>\r\nIn-Reply-To: <1@sender.test>\r\n\r\nhi\r\n"
	expect(ok(first, "deliver", user), `Inbox: email e1, thread t1, uid 1, \d+ bytes, state 2\n`)
	expect(ok(reply, "deliver", user, "inbox"), `email e2, thread t1, uid 2, .* state 3\n`)
	if out := ok("", "cat", user, "e1"); out != first {
		t.Errorf("cat e1 = %q", out)
	}

	// The message on disk is encrypted; verify finds damage.
	var files []string
	filepath.WalkDir(filepath.Join(dir, "data", "blobs"), func(p string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 2 {
		t.Fatalf("%d blob files, want 2", len(files))
	}
	for _, p := range files {
		data, _ := os.ReadFile(p)
		if bytes.Contains(data, []byte("sender.test")) || !bytes.HasPrefix(data, []byte("RMB1")) {
			t.Errorf("%s is not encrypted", p)
		}
		if !regexp.MustCompile(`/blobs/[a-z2-7]{16}/([0-9a-f]{2})/([0-9a-f]{2})[0-9a-f]{62}$`).MatchString(p) ||
			filepath.Base(filepath.Dir(p)) != filepath.Base(p)[:2] {
			t.Errorf("blob path %s", p)
		}
	}
	expect(ok("", "verify", user), `2 emails checked, 0 failed\n`)
	expect(ok("", "verify", user, "e1"), `e1 +ok\n`)
	saved, _ := os.ReadFile(files[0])
	os.WriteFile(files[0], append(bytes.Clone(saved[:len(saved)-1]), saved[len(saved)-1]^1), 0o600)
	if out, err := do("", "verify", user); err == nil || !strings.Contains(out, "FAILED") || !strings.Contains(out, "2 emails checked, 1 failed") {
		t.Errorf("verify with a damaged blob: %v\n%s", err, out)
	}
	os.WriteFile(files[0], saved, 0o600)
	expect(ok("", "verify", user), `0 failed\n`)

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
	expect(ok("", "trim"), `removed \d+ change rows, \d+ delivery keys, \d+ sessions and \d+ auth events\n`)
	// The blob of the destroyed e2 is too young to sweep; reindex reads
	// what is left again.
	expect(ok("", "sweep"), `removed 0 blob files and 0 temporary files, put 0 back\n`)
	expect(ok("", "reindex", user), regexp.QuoteMeta(user)+`: 1 emails\n`)
	expect(ok("", "list", user, "Werk"), `(?m)^e1 +uid 1 +t1 +m6 .* first +\$seen todo$`)

	for _, bad := range [][]string{
		{"cat", user, "e2"}, {"cat", user, "2"}, {"list", user, "Nope"}, {"list", "nobody@" + domain},
		{"move", user, "e1", "inbox", "trash"}, {"changes", user, "99"}, {"changes", user, "x"}, {"nonsense"}, {"user"}, {"domain"}, {"domain", "remove"},
	} {
		if _, err := do("", bad...); err == nil {
			t.Errorf("robinmail %s: no error", strings.Join(bad, " "))
		}
	}
}

func randomHex() string {
	var r [6]byte
	rand.Read(r[:])
	return hex.EncodeToString(r[:])
}

// cli runs robinmail commands against a config in a directory of its own.
type cli struct {
	t   *testing.T
	dir string
	cfg string
}

// newCLI writes a config with a database of its own. extra is more JSON
// members for the config, each followed by a comma.
func newCLI(t *testing.T, extra string) *cli {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "robinmail.json")
	body := fmt.Sprintf(`{%s"hostname":"mail.example.test","data_dir":"data","master_key_file":"master.key"}`, extra)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &cli{t: t, dir: dir, cfg: cfg}
}

func (c *cli) run(ctx context.Context, stdin string, args ...string) (string, error) {
	var out bytes.Buffer
	err := run(ctx, append([]string{"-config", c.cfg}, args...), strings.NewReader(stdin), &out)
	return out.String(), err
}

func (c *cli) do(stdin string, args ...string) (string, error) {
	return c.run(context.Background(), stdin, args...)
}

func (c *cli) ok(stdin string, args ...string) string {
	c.t.Helper()
	out, err := c.do(stdin, args...)
	if err != nil {
		c.t.Fatalf("robinmail %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func (c *cli) expect(out string, patterns ...string) {
	c.t.Helper()
	for _, p := range patterns {
		if !regexp.MustCompile(p).MatchString(out) {
			c.t.Errorf("output does not match %q:\n%s", p, out)
		}
	}
}
