package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/config"
	"github.com/robinmail/robinmail/internal/store"
)

// appOf opens the store of a cli's config, as a command does.
func appOf(t *testing.T, c *cli) *app {
	t.Helper()
	cfg, err := config.Load(c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err := open(context.Background(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	a.ctx, a.stdout = context.Background(), io.Discard
	// The commands the test runs meanwhile reach the database through
	// it, as they would through a running server.
	if a.raft != nil {
		if a.control, err = serveControl(cfg, a.identity, a.raft, a.log); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(a.close)
	return a
}

// sibling is a second node on the same disk as c: the same database and
// blob directory, a master key file of its own, which starts as a copy of
// the given one.
func sibling(t *testing.T, c *cli, name, keyFrom string) (*cli, string) {
	t.Helper()
	keyFile := filepath.Join(c.dir, name+".key")
	copyFile(t, keyFrom, keyFile)
	cfg := filepath.Join(c.dir, name+".json")
	body := fmt.Sprintf(`{"hostname":"mail.example.test","data_dir":"data","master_key_file":%q}`, name+".key")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &cli{t: t, dir: c.dir, cfg: cfg}, keyFile
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestKeyRotation replaces the master key of a server that keeps running:
// a new key joins the file, becomes the current one, every account's key
// is wrapped again, and the old key leaves. A second node with a file one
// step behind reads everything at every step; stopping in the middle and
// two nodes rotating at once leave nothing unreadable.
func TestKeyRotation(t *testing.T) {
	c := newCLI(t, "")
	domain := "rotate.test"
	anna := "anna@" + domain
	c.ok("", "key", "generate")
	c.ok("", "domain", "add", domain)
	c.ok("correct horse\n", "user", "add", anna)
	c.ok(message(anna, "1", "Before the rotation", "Still readable afterwards."), "deliver", anna)
	readable := func(who *cli, what string) {
		t.Helper()
		who.expect(who.ok("", "cat", anna, "e1"), `Still readable afterwards\.`)
		who.expect(who.ok("", "verify", anna), `1 emails checked, 0 failed`)
		if t.Failed() {
			t.Fatalf("not readable: %s", what)
		}
	}
	original := filepath.Join(t.TempDir(), "original.key")
	copyFile(t, filepath.Join(c.dir, "master.key"), original)

	status := c.ok("", "key", "status")
	c.expect(status, `^[0-9a-f]{16}  current   1 accounts\n$`)
	oldFP := status[:16]
	c.expect(c.ok("", "key", "prune"), `nothing to prune`)
	if out, err := c.do("", "key", "rotate", "0123456789abcdef"); err == nil {
		t.Errorf("rotated to a key that is not there: %s", out)
	}

	// Step 1: a new key, accepted and unused.
	added := c.ok("", "key", "add")
	m := regexp.MustCompile(`added key ([0-9a-f]{16}) to `).FindStringSubmatch(added)
	if m == nil {
		t.Fatalf("key add: %s", added)
	}
	newFP := m[1]
	c.expect(added, `robinmail key rotate `+newFP)
	c.expect(c.ok("", "key", "status"), `(?m)^`+oldFP+`  current   1 accounts$`, `(?m)^`+newFP+`  accepted  0 accounts$`)
	if fi, _ := os.Stat(filepath.Join(c.dir, "master.key")); fi.Mode().Perm() != 0o600 {
		t.Errorf("the key file has mode %v", fi.Mode().Perm())
	}
	readable(c, "after key add")
	// "Copy the file to every node": a second node, whose file stays one
	// step behind from here on.
	node2, node2Key := sibling(t, c, "node2", filepath.Join(c.dir, "master.key"))
	readable(node2, "on the second node after key add")

	// Step 2: the new key becomes current here and wraps every account.
	c.expect(c.ok("", "key", "rotate", newFP), newFP+` is now the current key`, `wrapped the keys of 1 accounts under `+newFP+`; 0 already were`)
	c.expect(c.ok("", "key", "status"), `(?m)^`+newFP+`  current   1 accounts$`, `(?m)^`+oldFP+`  accepted  0 accounts$`)
	readable(c, "after key rotate")
	readable(node2, "on a node with the old order after key rotate")
	// Again changes nothing.
	c.expect(c.ok("", "key", "rotate", newFP), `wrapped the keys of 0 accounts under `+newFP+`; 1 already were`)

	// The second node still wraps under the old key: an account it makes
	// keeps the old key in use, and prune refuses.
	bob := "bob@" + domain
	node2.ok("correct horse\n", "user", "add", bob)
	c.expect(c.ok("", "key", "status"), `(?m)^`+oldFP+`  accepted  1 accounts$`)
	if out, err := c.do("", "key", "prune"); err == nil || !strings.Contains(out, "still in use: "+oldFP+" (1 accounts)") {
		t.Errorf("prune with an account under the old key: %v\n%s", err, out)
	}
	if n := strings.Count(string(mustRead(t, filepath.Join(c.dir, "master.key"))), "\n"); n != 2 {
		t.Errorf("the key file has %d keys after a refused prune", n)
	}
	// The health check says so too.
	c.expect(c.ok("", "health"), `(?m)^ok    master key +2 keys in the file, current `+newFP+`, 1 accounts still under an older key$`)

	// Step 2 again picks it up; then the second node gets the new file.
	c.expect(c.ok("", "key", "rotate", newFP), `wrapped the keys of 1 accounts under `+newFP+`; 1 already were`)
	copyFile(t, filepath.Join(c.dir, "master.key"), node2Key)

	// Step 3: the old key leaves the file.
	c.expect(c.ok("", "key", "prune"), `removed key `+oldFP+` from `, `A backup made before the rotation needs the old key`)
	c.expect(c.ok("", "key", "status"), `^`+newFP+`  current   2 accounts\n$`)
	readable(c, "after key prune")
	readable(node2, "on a node that still has both keys after key prune")
	// The old key alone opens nothing any more.
	stale, _ := sibling(t, c, "stale", original)
	if out, err := stale.do("", "cat", anna, "e1"); err == nil || !strings.Contains(err.Error(), "does not unwrap") {
		t.Errorf("the old key reads mail after the rotation: %v\n%s", err, out)
	}
	if out, err := stale.do("", "key", "status"); err == nil || !strings.Contains(out, "2 accounts have a key that no master key") {
		t.Errorf("key status with the old key: %v\n%s", err, out)
	}
	c.expect(c.ok("", "audit"), `key-added +- +`+newFP, `key-rotated +- +`+newFP+`, 1 accounts`, `key-pruned +- +`+oldFP)

	// Interrupted and concurrent: 60 more accounts, a third key, a first
	// run that is cut off, then two nodes at once.
	a := appOf(t, c)
	for i := 0; i < 60; i++ {
		if _, err := a.store.CreateAccount(a.ctx, fmt.Sprintf("u%02d@%s", i, domain), "!"); err != nil {
			t.Fatal(err)
		}
	}
	third := regexp.MustCompile(`added key ([0-9a-f]{16}) to `).FindStringSubmatch(c.ok("", "key", "add"))[1]
	copyFile(t, filepath.Join(c.dir, "master.key"), node2Key)
	a = appOf(t, c)
	promoted, err := a.master.Promote(third)
	if err != nil {
		t.Fatal(err)
	}
	a.store.UseMasterKey(promoted)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	changed, already, err := a.store.RewrapAccountKeys(ctx)
	if err == nil || changed+already >= 62 {
		t.Errorf("a cancelled run: %d changed, %d already, %v", changed, already, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	changed, _, err = a.store.RewrapAccountKeys(ctx)
	cancel()
	t.Logf("a run cut off after 30 ms wrapped %d of 62 accounts: %v", changed, err)
	// Whatever it got done, every account is readable with the file that
	// has both keys, and under one of the two.
	// The write that was cut off may have been applied all the same: its
	// answer is what did not arrive. So one more than was counted is fine.
	use, err := a.store.AccountKeyUse(context.Background())
	if got := use.ByKey[third]; err != nil || use.Unreadable != 0 || use.ByKey[newFP]+got != 62 || (got != changed && got != changed+1) {
		t.Fatalf("after the interrupted run: %+v %v (changed %d)", use, err, changed)
	}
	readable(node2, "in the middle of an interrupted rotation")
	// Two nodes finish it at the same time.
	var wg sync.WaitGroup
	outs := make([]string, 2)
	errs := make([]error, 2)
	for i, who := range []*cli{c, node2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = who.do("", "key", "rotate", third)
		}()
	}
	wg.Wait()
	for i := range outs {
		if errs[i] != nil {
			t.Errorf("concurrent rotate %d: %v\n%s", i, errs[i], outs[i])
		}
	}
	c.expect(c.ok("", "key", "status"), `(?m)^`+third+`  current   62 accounts$`, `(?m)^`+newFP+`  accepted  0 accounts$`)
	c.ok("", "key", "prune")
	readable(c, "after the second rotation")
	node2.ok("", "key", "prune")
	readable(node2, "on the second node after the second rotation")
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// jmapView logs a user in and returns what JMAP shows of the account:
// mailboxes, every email with its body values, identities, quota, the
// Sieve scripts and the vacation response, and the bytes of every message.
func jmapView(t *testing.T, web, user string) map[string]any {
	t.Helper()
	c := newJMAPClient(t, "http://"+web)
	resp, data := c.do("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":"correct horse"}`, user), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login of %s: %d %s", user, resp.StatusCode, data)
	}
	var who struct{ AccountID string }
	json.Unmarshal(data, &who)
	id := who.AccountID
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission",
		"urn:ietf:params:jmap:quota","urn:ietf:params:jmap:sieve","urn:ietf:params:jmap:vacationresponse"],"methodCalls":[
		["Mailbox/get",{"accountId":%[1]q},"m"],
		["Email/query",{"accountId":%[1]q,"sort":[{"property":"receivedAt"}]},"q"],
		["Email/get",{"accountId":%[1]q,"#ids":{"resultOf":"q","name":"Email/query","path":"/ids"},
			"properties":["id","blobId","threadId","mailboxIds","keywords","size","receivedAt","messageId","inReplyTo","references","from","to","cc",
				"subject","sentAt","hasAttachment","preview","bodyStructure","bodyValues","textBody","htmlBody","attachments"],
			"fetchAllBodyValues":true},"e"],
		["Thread/get",{"accountId":%[1]q,"#ids":{"resultOf":"e","name":"Email/get","path":"/list/*/threadId"}},"t"],
		["Identity/get",{"accountId":%[1]q},"i"],
		["Quota/get",{"accountId":%[1]q},"quota"],
		["SieveScript/get",{"accountId":%[1]q},"sieve"],
		["VacationResponse/get",{"accountId":%[1]q},"v"]]}`, id)
	resp, data = c.do("POST", "/jmap/api", body, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("JMAP for %s: %d %s", user, resp.StatusCode, data)
	}
	var out struct {
		MethodResponses [][]any `json:"methodResponses"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	view := map[string]any{}
	for _, r := range out.MethodResponses {
		if r[0] == "error" {
			t.Fatalf("JMAP for %s: %s", user, data)
		}
		view[r[2].(string)] = r[1]
	}
	// The messages themselves, byte for byte.
	blobs := map[string]any{}
	for _, e := range view["e"].(map[string]any)["list"].([]any) {
		blob := e.(map[string]any)["blobId"].(string)
		resp, raw := c.do("GET", "/jmap/download/"+id+"/"+blob+"/m.eml?type=message/rfc822", "", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("download of %s: %d", blob, resp.StatusCode)
		}
		blobs[blob] = string(raw)
	}
	view["blobs"] = blobs
	return view
}

// TestBackupRestore makes a backup of a server with mail and restores it
// into an empty one: a second data directory, with its own database. What JMAP
// shows of every account is the same afterwards, to the byte and to the
// state string.
func TestBackupRestore(t *testing.T) {
	const web = `"smtp":{"listen":"127.0.0.1:0"},"web":{"http_listen":"127.0.0.1:0"},"scrub":{"disabled":true},`
	c := newCLI(t, web)
	domain := "backup.test"
	anna, bob := "anna@"+domain, "bob@"+domain
	c.ok("", "key", "generate")
	c.ok("", "domain", "add", domain)
	c.ok("correct horse\n", "user", "add", anna)
	c.ok("correct horse\n", "user", "add", bob)
	c.ok("", "alias", "add", "team@"+domain, anna, bob)
	c.ok("", "alias", "add", "press@"+domain, "someone@elsewhere.example")
	c.ok("", "mailbox", "create", anna, "Work")
	c.ok("", "mailbox", "create", anna, "Work/Invoices")
	c.ok("", "user", "quota", anna, "50M", "1000")
	c.ok(message(anna, "1", "First", "The first message."), "deliver", anna)
	c.ok(strings.Replace(message(anna, "2", "Re: First", "A reply."), "MIME-Version", "References: <1@sender.example>\r\nMIME-Version", 1), "deliver", anna)
	c.ok(reportMessage(anna), "deliver", anna, "Work/Invoices")
	c.ok("", "flag", anna, "e1", "$seen", "$flagged")
	c.ok("", "label", anna, "e2", "Work/Invoices")
	c.ok(message(bob, "3", "For Bob", "Bob's only message."), "deliver", bob)
	// A message that is destroyed keeps its blob file for a day; the
	// database no longer refers to it.
	c.ok(message(bob, "4", "Gone", "Destroyed before the backup."), "deliver", bob)
	c.ok("", "rm", bob, "e2")

	srv := startServe(t, c)
	before := map[string]map[string]any{anna: jmapView(t, srv.web, anna), bob: jmapView(t, srv.web, bob)}
	if n := len(before[anna]["e"].(map[string]any)["list"].([]any)); n != 3 {
		t.Fatalf("anna has %d emails before the backup", n)
	}

	// The backup, taken while the server runs.
	dir := filepath.Join(t.TempDir(), "backup")
	out := c.ok("", "backup", dir)
	t.Logf("robinmail backup:\n%s", out)
	c.expect(out, `database: .*, schema \d+, 2 accounts, 4 emails`, `blobs: 5 files`, `The master key is not in the backup`)
	var m manifest
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, "manifest.json")), &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != 1 || m.Accounts != 2 || m.Emails != 4 || m.Blobs != 5 || len(m.Keys) != 1 || len(m.Missing) != 0 || m.Schema != store.SchemaVersion() {
		t.Errorf("the manifest: %+v", m)
	}
	// What is in it: a SQLite file and the encrypted blob files. No
	// message is readable in the blob files, and the master key is
	// nowhere. The database file is the database: it has the previews
	// and subjects in the clear, as the node's own database file does.
	db := mustRead(t, filepath.Join(dir, "db.sqlite"))
	if !bytes.HasPrefix(db, []byte("SQLite format 3\x00")) {
		t.Error("db.sqlite is not a SQLite file")
	}
	key := strings.TrimSpace(string(mustRead(t, filepath.Join(c.dir, "master.key"))))
	files := 0
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		data := mustRead(t, path)
		secrets := []string{key}
		if strings.Contains(path, string(filepath.Separator)+"blobs"+string(filepath.Separator)) {
			secrets = append(secrets, "The first message.", "Bob's only message.", "sender.example")
		}
		for _, secret := range secrets {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s holds %q in the clear", path, secret)
			}
		}
		if fi, _ := d.Info(); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", path, fi.Mode().Perm())
		}
		return nil
	})
	if files != 7 {
		t.Errorf("the backup has %d files, want manifest, database and 5 blobs", files)
	}
	if out, err := c.do("", "backup", dir); err == nil {
		t.Errorf("a backup into a directory that has one: %s", out)
	}
	srv.stop()

	// The empty server: another directory, another database.
	r := newCLI(t, web)
	// Without a key file nothing runs; with another key the restore
	// refuses before it touches anything.
	if _, err := r.do("", "restore", dir); err == nil || !strings.Contains(err.Error(), "master key") {
		t.Errorf("restore without a key file: %v", err)
	}
	r.ok("", "key", "generate")
	if out, err := r.do("", "restore", dir); err == nil || !strings.Contains(err.Error(), "does not hold the master key the backup was made under") {
		t.Errorf("restore with another master key: %v\n%s", err, out)
	}
	if out := r.ok("", "domain", "list"); strings.TrimSpace(out) != "" {
		t.Errorf("a refused restore left domains: %s", out)
	}
	copyFile(t, filepath.Join(c.dir, "master.key"), filepath.Join(r.dir, "master.key"))
	// A backup that was not finished, or whose database file is not the
	// one it wrote, is refused too.
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.CopyFS(broken, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(broken, "manifest.json"))
	if _, err := r.do("", "restore", broken); err == nil || !strings.Contains(err.Error(), "not a backup, or one that was not finished") {
		t.Errorf("restore without a manifest: %v", err)
	}
	copyFile(t, filepath.Join(dir, "manifest.json"), filepath.Join(broken, "manifest.json"))
	os.WriteFile(filepath.Join(broken, "db.sqlite"), append(db[:len(db):len(db)], 0), 0o600)
	if _, err := r.do("", "restore", broken); err == nil || !strings.Contains(err.Error(), "is not the file the backup wrote") {
		t.Errorf("restore of a changed database file: %v", err)
	}

	out = r.ok("", "restore", dir)
	t.Logf("robinmail restore:\n%s", out)
	r.expect(out, `database loaded: schema \d+`, `blobs: 5 restored`, `restored the backup of mail\.example\.test made .*: 2 accounts, 4 emails`)
	if out, err := r.do("", "restore", dir); err == nil || !strings.Contains(err.Error(), "restored into an empty one") {
		t.Errorf("a second restore: %v\n%s", err, out)
	}

	// The same server: by the commands, and by JMAP to the byte.
	for _, args := range [][]string{{"domain", "list"}, {"alias", "list"}, {"mailbox", "list", anna}, {"list", anna}, {"list", anna, "Work/Invoices"},
		{"list", bob}, {"user", "quota", anna}, {"cat", anna, "e1"}, {"cat", anna, "e3"}, {"changes", anna}} {
		if was, is := c.ok("", args...), r.ok("", args...); was != is {
			t.Errorf("robinmail %s differs.\nbefore:\n%s\nafter:\n%s", strings.Join(args, " "), was, is)
		}
	}
	r.expect(r.ok("", "verify", anna), `3 emails checked, 0 failed`)
	r.expect(r.ok("", "verify", bob), `1 emails checked, 0 failed`)
	r.expect(r.ok("", "scrub"), `4 blob files checked, 0 corrupt`)
	r.expect(r.ok("", "audit"), `restore +- +backup of mail\.example\.test from .*: 2 accounts, 5 blobs, 0 missing`)
	srv2 := startServe(t, r)
	for _, user := range []string{anna, bob} {
		after := jmapView(t, srv2.web, user)
		for part, was := range before[user] {
			a, _ := json.MarshalIndent(was, "", " ")
			b, _ := json.MarshalIndent(after[part], "", " ")
			if !bytes.Equal(a, b) {
				t.Errorf("%s: %q differs after the restore.\nbefore: %s\nafter:  %s", user, part, clipLog(string(a)), clipLog(string(b)))
			}
		}
		t.Logf("%s over JMAP after the restore: %d mailboxes, %d emails, %d message files, state %v: the same as before", user,
			len(after["m"].(map[string]any)["list"].([]any)), len(after["e"].(map[string]any)["list"].([]any)),
			len(after["blobs"].(map[string]any)), after["e"].(map[string]any)["state"])
	}
	// Mail arrives on the restored server as on any.
	s := dialSMTP(t, srv2.addr)
	s.cmd("EHLO client.sender.example", 250)
	s.send("anna@sender.example", "team@"+domain, message("team@"+domain, "9", "After the restore", "To the alias."), 250)
	s.cmd("QUIT", 221)
	r.expect(r.ok("", "list", bob), `After the restore`)
	srv2.stop()

	// A backup that lacks a blob the database refers to: the restore says
	// which, the rest is there, and restore-blobs fills it in later.
	r3 := newCLI(t, web)
	copyFile(t, filepath.Join(c.dir, "master.key"), filepath.Join(r3.dir, "master.key"))
	partial := filepath.Join(t.TempDir(), "partial")
	if err := os.CopyFS(partial, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	a := appOf(t, c)
	bobAcct, err := a.store.Account(a.ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := a.store.BlobCatalog(a.ctx)
	if err != nil || len(refs[bobAcct.ID]) != 1 {
		t.Fatalf("bob's blobs: %v %v", refs[bobAcct.ID], err)
	}
	for sum := range refs[bobAcct.ID] {
		if err := os.Remove(blobPath(partial, bobAcct.ID, sum)); err != nil {
			t.Fatal(err)
		}
	}
	out, err = r3.do("", "restore", partial)
	if err == nil || !strings.Contains(err.Error(), "1 blobs the database refers to are not in the backup") || !strings.Contains(out, "missing: "+bobAcct.ID+"/") {
		t.Errorf("restore of a backup without one blob: %v\n%s", err, out)
	}
	r3.expect(r3.ok("", "verify", anna), `3 emails checked, 0 failed`)
	if out, err := r3.do("", "verify", bob); err == nil {
		t.Errorf("bob's message without its blob verifies: %s", out)
	}
	r3.expect(r3.ok("", "restore-blobs", dir), `blobs: 1 restored, 0 failed`)
	r3.expect(r3.ok("", "verify", bob), `1 emails checked, 0 failed`)
}

// TestOperations is the rest of the administrator's side on one server:
// status and health, /healthz, the metrics listener, the queue commands,
// showing and removing an account, and a scrub that finds a damaged file.
func TestOperations(t *testing.T) {
	c := newCLI(t, `"smtp":{"listen":"127.0.0.1:0"},"web":{"http_listen":"127.0.0.1:0"},"metrics":{"listen":"127.0.0.1:0"},"scrub":{"disabled":true},`)
	domain := "ops.test"
	anna, bob := "anna@"+domain, "bob@"+domain
	c.ok("", "key", "generate")
	c.ok("", "domain", "add", domain)
	c.ok("correct horse\n", "user", "add", anna)
	c.ok("correct horse\n", "user", "add", bob)
	c.ok("", "alias", "add", "info@"+domain, anna)
	c.ok("", "alias", "add", "both@"+domain, anna, bob)
	c.ok(message(anna, "1", "Hello", "A message."), "deliver", anna)

	// No server yet: the node is fine, nothing answers. health fails,
	// which is what a monitor wants to hear.
	out, err := c.do("", "health")
	if err == nil || !regexp.MustCompile(`(?m)^FAIL  server +http://127\.0\.0\.1:0/healthz: `).MatchString(out) {
		t.Errorf("health without a server: %v\n%s", err, out)
	}
	c.expect(out, `(?m)^ok    database +answers, schema \d+$`, `(?m)^ok    master key +1 keys in the file, current [0-9a-f]{16}$`, `(?m)^ok    blob directory +\S+blobs$`)

	// The queue: two messages that wait, put there as a submission is.
	a := appOf(t, c)
	acct, err := a.store.Account(a.ctx, anna)
	if err != nil {
		t.Fatal(err)
	}
	for i, rcpts := range [][]string{{"one@elsewhere.example", "two@elsewhere.example"}, {"three@elsewhere.example"}} {
		if _, err := a.store.Submit(a.ctx, acct.ID, store.NewSubmission{Raw: []byte(message(rcpts[0], fmt.Sprint(10+i), "Outbound", "x")),
			MailFrom: anna, Rcpts: rcpts, SendAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	list := c.ok("", "queue", "list")
	t.Logf("robinmail queue list:\n%s", list)
	c.expect(list, `(?m)^anna@ops\.test +s1 +\S+ +tries 0 +starts in \S+ +anna@ops\.test -> 2 of 2 waiting$`,
		`(?m)^anna@ops\.test +s2 .* -> 1 of 1 waiting$`, `(?m)^2 in the queue: 0 due, 0 being sent, 2 waiting$`)
	c.expect(c.ok("", "queue", "show", anna, "s1"), `(?m)^message +s1 of anna@ops\.test$`, `(?m)^queue +active$`, `(?m)^from +anna@ops\.test$`,
		`(?m)^  queued +one@elsewhere\.example$`, `(?m)^  queued +two@elsewhere\.example$`)
	c.expect(c.ok("", "queue", "retry", anna, "s1"), `s1 is due now`)
	c.expect(c.ok("", "queue", "retry", anna, "s1"), `s1 was due already`)
	c.expect(c.ok("", "queue", "list"), `(?m)^anna@ops\.test +s1 .* due +anna@ops\.test`, `(?m)^2 in the queue: 1 due, 0 being sent, 1 waiting$`)
	c.expect(c.ok("", "queue", "retry", "all"), `1 messages are due now`)
	c.expect(c.ok("", "queue", "delete", anna, "s1"), `removed s1 from the queue; not sent to one@elsewhere\.example, two@elsewhere\.example`, `No bounce is sent`)
	c.expect(c.ok("", "queue", "list"), `(?m)^1 in the queue: 1 due`)
	c.expect(c.ok("", "queue", "show", anna, "s1"), `(?m)^queue +done$`, `(?m)^  failed +one@elsewhere\.example  5\.0\.0 Removed from the queue by the administrator$`)
	c.expect(c.ok("", "audit", anna), `queue-deleted .* s1 for one@elsewhere\.example, two@elsewhere\.example, by an administrator`)
	// A message a node is working on is left alone.
	if _, err := a.store.ClaimSubmission(a.ctx, store.SubmissionRef{Account: acct.ID, ID: 2}, "a-node", time.Now(), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"queue", "delete", anna, "s2"}, {"queue", "retry", anna, "s2"}} {
		if out, err := c.do("", args...); err == nil || !strings.Contains(err.Error(), "right now") {
			t.Errorf("robinmail %s on a claimed message: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	c.expect(c.ok("", "queue", "list"), `being sent now`, `(?m)^1 in the queue: 0 due, 1 being sent, 0 waiting$`)
	for _, bad := range [][]string{{"queue", "show", anna, "s9"}, {"queue", "delete", anna, "s1"}, {"queue", "delete", anna, "e1"}, {"queue", "retry", "nobody@" + domain, "s1"},
		{"queue", "list", "0"}, {"queue", "retry"}} {
		if out, err := c.do("", bad...); err == nil {
			t.Errorf("robinmail %s: no error: %s", strings.Join(bad, " "), out)
		}
	}

	// The server, with its metrics listener. It opens the database, so
	// the test's own hold on it goes first.
	a.close()
	metricsAddr := make(chan net.Addr, 1)
	onMetricsListen = func(a net.Addr) { metricsAddr <- a }
	t.Cleanup(func() { onMetricsListen = nil })
	srv := startServe(t, c)
	get := func(url string, header ...string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", url, nil)
		for i := 0; i+1 < len(header); i += 2 {
			if header[i] == "Host" {
				req.Host = header[i+1]
			} else {
				req.Header.Set(header[i], header[i+1])
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	// /healthz: for whoever asks, under whatever name, and nothing but
	// "ok".
	for _, host := range []string{"", "10.0.0.7", "lb.internal.example"} {
		if status, body := get("http://"+srv.web+"/healthz", "Host", host); status != 200 || body != "ok\n" {
			t.Errorf("/healthz as %q: %d %q", host, status, body)
		}
	}
	if resp, err := http.Post("http://"+srv.web+"/healthz", "text/plain", nil); err != nil || resp.StatusCode != 405 {
		t.Errorf("POST /healthz: %v %v", resp, err)
	}
	// health and status ask it. The config says port 0, so the commands
	// are pointed at the port the server got.
	cfg := string(mustRead(t, c.cfg))
	os.WriteFile(c.cfg, []byte(strings.Replace(cfg, `"web":{"http_listen":"127.0.0.1:0"}`, `"web":{"http_listen":"`+srv.web+`"}`, 1)), 0o600)
	c.expect(c.ok("", "health"), `(?m)^ok    server +answers on http://`+regexp.QuoteMeta(srv.web)+`/healthz$`)
	status := c.ok("", "status")
	t.Logf("robinmail status:\n%s", status)
	c.expect(status, `(?m)^mail\.example\.test: 1 domains, 2 accounts, 2 aliases, 1 emails \(\d+ octets\)$`,
		`(?m)^queue: 1 messages: 0 due, 1 being sent, 0 waiting; the oldest is \S+ old$`, `(?m)^ok    database `, `(?m)^ok    server `)

	// Mail in, so the counters move.
	s := dialSMTP(t, srv.addr)
	s.cmd("EHLO client.sender.example", 250)
	s.send("carl@sender.example", "both@"+domain, message("both@"+domain, "2", "To both", "Two mailboxes."), 250)
	s.cmd("QUIT", 221)
	maddr := (<-metricsAddr).String()
	code, text := get("http://" + maddr + "/metrics")
	t.Logf("GET /metrics:\n%s", text)
	if code != 200 {
		t.Fatalf("/metrics: %d", code)
	}
	for _, want := range []string{
		"# TYPE robinmail_smtp_connections_total counter\n", "# TYPE robinmail_queue_messages gauge\n",
		`(?m)^robinmail_smtp_connections_total [1-9]\d*$`, `(?m)^robinmail_delivered_total ([2-9]|[1-9]\d+)$`,
		`(?m)^robinmail_http_requests_total\{code="2xx"\} [1-9]\d*$`, `(?m)^robinmail_db_requests_total\{result="ok"\} [1-9]\d*$`,
		`(?m)^robinmail_healthy 1$`, `(?m)^robinmail_accounts 2$`, `(?m)^robinmail_domains 1$`, `(?m)^robinmail_emails 3$`,
		`(?m)^robinmail_queue_messages\{state="sending"\} 1$`, `(?m)^robinmail_queue_messages\{state="due"\} 0$`,
		`(?m)^robinmail_queue_oldest_seconds \d`, `(?m)^robinmail_start_time_seconds 1\.\d+e\+09$`,
	} {
		if !regexp.MustCompile(want).MatchString(text) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	// Only /metrics, only GET, and not on the web listener.
	if code, _ := get("http://" + maddr + "/"); code != 404 {
		t.Errorf("the metrics listener serves /: %d", code)
	}
	if code, body := get("http://" + srv.web + "/metrics"); code == 200 && strings.Contains(body, "robinmail_") {
		t.Error("the web listener serves the metrics")
	}
	if host, _, _ := net.SplitHostPort(maddr); !net.ParseIP(host).IsLoopback() {
		t.Errorf("the metrics listen on %s", maddr)
	}
	srv.stop()

	// An account as the administrator sees it, and its removal.
	show := c.ok("", "user", "show", anna)
	t.Logf("robinmail user show:\n%s", show)
	c.expect(show, `(?m)^address +anna@ops\.test$`, `(?m)^mail +2 messages \(no limit\), \d+ octets \(no limit\)$`, `(?m)^mailboxes +5$`,
		`(?m)^password +set$`, `(?m)^two-step +off$`, `(?m)^app passwords 0$`, `(?m)^aliases +both@ops\.test, info@ops\.test$`)
	if out, err := c.do("", "user", "remove", anna); err == nil || !strings.Contains(err.Error(), "--yes") || !strings.Contains(err.Error(), "2 messages") {
		t.Errorf("user remove without --yes: %v\n%s", err, out)
	}
	c.expect(c.ok("", "list", anna), `Hello`)
	c.expect(c.ok("", "user", "remove", anna, "--yes"), `removed anna@ops\.test with 2 messages`)
	if out, err := c.do("", "list", anna); err == nil {
		t.Errorf("a removed user has a mailbox: %s", out)
	}
	users := c.ok("", "user", "list")
	if strings.Contains(users, anna) || !strings.Contains(users, bob) {
		t.Errorf("user list after the removal:\n%s", users)
	}
	// Aliases lose the target; the one that had only this target is gone.
	aliases := c.ok("", "alias", "list")
	if strings.Contains(aliases, anna) || strings.Contains(aliases, "info@") || !regexp.MustCompile(`both@ops\.test +-> +bob@ops\.test`).MatchString(aliases) {
		t.Errorf("alias list after the removal:\n%s", aliases)
	}
	c.expect(c.ok("", "queue", "list"), `(?m)^0 in the queue`)
	// (The server is stopped, which status reports as a failed check.)
	if out, err := c.do("", "status"); err == nil || !strings.Contains(out, "1 accounts, 1 aliases, 1 emails") || !strings.Contains(out, "no server is listening") {
		t.Errorf("status after the removal, without a server: %v\n%s", err, out)
	}
	c.expect(c.ok("", "audit"), `account-removed +anna@ops\.test +- +account \S+, 2 messages, by an administrator`)
	// Nothing of the account is left in any table but the audit log, and
	// the other account is whole.
	a = appOf(t, c)
	left, err := a.store.BlobCatalog(a.ctx)
	if _, has := left[acct.ID]; err != nil || has || len(left) != 1 {
		t.Errorf("blob references after the removal: %v %v", left, err)
	}
	c.expect(c.ok("", "verify", bob), `1 emails checked, 0 failed`)
	// The address is free again, for a new account.
	c.ok("correct horse\n", "user", "add", anna)
	c.expect(c.ok("", "list", anna), `0 emails`)

	// A scrub on a single node: a file with one flipped bit is found and
	// named; there is nobody to repair it from.
	c.expect(c.ok("", "scrub"), `1 blob files checked, 0 corrupt, 0 repaired`)
	bobAcct, _ := a.store.Account(a.ctx, bob)
	var damaged string
	for sum := range left[bobAcct.ID] {
		damaged = filepath.Join(c.dir, "data", "blobs", bobAcct.ID, sum[:2], sum)
	}
	data := mustRead(t, damaged)
	data[len(data)-5] ^= 0x10
	os.WriteFile(damaged, data, 0o600)
	out, err = c.do("", "scrub")
	if err == nil || !strings.Contains(out, "1 blob files checked, 1 corrupt, 0 repaired") || !strings.Contains(out, "corrupt: "+bobAcct.ID+"/") {
		t.Errorf("scrub with a damaged file: %v\n%s", err, out)
	}
	c.expect(c.ok("", "audit"), `scrub +- +1 checked, 1 corrupt, 0 repaired`)
	if _, err := os.Stat(damaged); err != nil {
		t.Errorf("the scrub removed the damaged file, the only copy: %v", err)
	}

	// The metrics listener is for this machine only.
	bad := newCLI(t, `"metrics":{"listen":"0.0.0.0:9150"},`)
	if _, err := bad.do("", "status"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("metrics on every interface: %v", err)
	}
}
