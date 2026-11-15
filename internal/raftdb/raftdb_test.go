package raftdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/db"
)

var ctx = context.Background()

func testLogger(t testing.TB) *slog.Logger {
	if os.Getenv("RAFTDB_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return nil
}

func openOne(t *testing.T, dir string) *DB {
	t.Helper()
	d, err := Open(Config{Dir: dir, Self: Member{Cert: "solo"}, Logger: testLogger(t), Wait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func exec(t *testing.T, d db.DB, sql string, args ...any) []db.Result {
	t.Helper()
	res, err := d.Execute(ctx, db.Stmt(sql, args...))
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return res
}

func query(t *testing.T, d db.DB, level db.Level, sql string, args ...any) [][]any {
	t.Helper()
	res, err := d.Query(ctx, level, db.Stmt(sql, args...))
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return res[0].Values
}

func TestSingleNode(t *testing.T) {
	dir := t.TempDir()
	d := openOne(t, dir)
	exec(t, d, `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT UNIQUE, n REAL, b TEXT)`)
	res := exec(t, d, `INSERT INTO t (name, n, b) VALUES (?, ?, ?)`, "a", 1.5, []byte("xy"))
	if res[0].RowsAffected != 1 || res[0].LastInsertID != 1 {
		t.Errorf("insert: %+v", res[0])
	}
	// A batch mixes writes and reads; named parameters.
	res, err := d.Execute(ctx,
		db.Statement{SQL: `INSERT INTO t (name, n) VALUES (:name, :n)`, Named: map[string]any{"name": "b", "n": int64(7)}},
		db.Stmt(`SELECT id, name, n, b FROM t ORDER BY id`))
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{{int64(1), "a", 1.5, "eHk="}, {int64(2), "b", float64(7), nil}}
	if fmt.Sprint(res[1].Values) != fmt.Sprint(want) {
		t.Errorf("select in a write batch: %v, want %v", res[1].Values, want)
	}
	// A failed statement rolls the batch back and names itself.
	_, err = d.Execute(ctx, db.Stmt(`INSERT INTO t (name) VALUES ('c')`), db.Stmt(`INSERT INTO t (name) VALUES ('a')`))
	var e *db.Error
	if !errors.As(err, &e) || e.Index != 1 || !e.Unique() || !strings.HasPrefix(e.Message, "UNIQUE constraint failed") {
		t.Fatalf("unique violation: %#v", err)
	}
	if n := query(t, d, db.Weak, `SELECT count(*) FROM t WHERE name = 'c'`)[0][0]; n != int64(0) {
		t.Errorf("the failed batch left a row: %v", n)
	}
	exec(t, d, `CREATE TABLE g (ok INTEGER CHECK (ok = 0))`)
	_, err = d.Execute(ctx, db.Stmt(`INSERT INTO g VALUES (1)`))
	if !errors.As(err, &e) || !e.Check() {
		t.Errorf("check violation: %v", err)
	}
	// A read error is a *db.Error too.
	if _, err := d.Query(ctx, db.Weak, db.Stmt(`SELECT nope FROM t`)); !errors.As(err, &e) {
		t.Errorf("bad read: %v", err)
	}
	for _, level := range []db.Level{db.None, db.Weak, db.Strong} {
		if n := query(t, d, level, `SELECT count(*) FROM t`)[0][0]; n != int64(2) {
			t.Errorf("%s: %v rows", level, n)
		}
	}

	// A second process cannot open it.
	if _, err := Open(Config{Dir: dir}); !errors.Is(err, ErrLocked) || !Locked(dir) {
		t.Errorf("second open: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if Locked(dir) {
		t.Error("still locked after Close")
	}
	// After a restart everything is there and the node leads again.
	d = openOne(t, dir)
	defer d.Close()
	if n := query(t, d, db.Weak, `SELECT count(*) FROM t`)[0][0]; n != int64(2) {
		t.Errorf("after restart: %v rows", n)
	}
	exec(t, d, `INSERT INTO t (name) VALUES ('d')`)
}

// TestDeduplication: a proposal applied twice (proposed again after a lost
// answer) runs once, and the second gets the first one's outcome.
func TestDeduplication(t *testing.T) {
	d := openOne(t, t.TempDir())
	defer d.Close()
	exec(t, d, `CREATE TABLE c (n INTEGER)`)
	exec(t, d, `INSERT INTO c VALUES (0)`)
	data := `{"id":"same","stmts":[["UPDATE c SET n = n + 1"],["SELECT n FROM c"]]}`
	for range 3 {
		if err := d.node.Propose(ctx, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, d, `SELECT 1`) // after the three
	if n := query(t, d, db.Weak, `SELECT n FROM c`)[0][0]; n != int64(1) {
		t.Errorf("applied %v times", n)
	}
	var result string
	d.wdb.QueryRow(`SELECT result FROM raft_request WHERE id = 'same'`).Scan(&result)
	if !strings.Contains(result, `"values":[[1]]`) {
		t.Errorf("recorded outcome %s", result)
	}
}

// TestDeterminism: what is applied is the same on every node, and nothing
// in a batch may depend on the node: values are normalised before they are
// proposed, and SQLite functions that read the clock or chance are not
// used by the store (TestStoreSQLIsDeterministic in internal/store).
func TestNormalize(t *testing.T) {
	in := []db.Statement{
		db.Stmt(`x`, 1, int32(2), uint8(3), 4.0, 4.5, "s", []byte{1, 2}, nil, true),
		{SQL: `y`, Named: map[string]any{"a": int64(1) << 60, "b": float32(0.5)}},
	}
	out, err := db.Normalize(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{int64(1), int64(2), int64(3), int64(4), 4.5, "s", "AQI=", nil, true}
	if fmt.Sprintf("%#v", out[0].Args) != fmt.Sprintf("%#v", want) {
		t.Errorf("args %#v", out[0].Args)
	}
	if out[1].Named["a"] != int64(1)<<60 || out[1].Named["b"] != 0.5 {
		t.Errorf("named %#v", out[1].Named)
	}
}

// ---- in-process clusters ----

// memNet connects nodes of one process. cut marks links that drop
// everything, in both directions.
type memNet struct {
	mu       sync.Mutex
	handlers map[string]http.Handler
	cut      map[string]bool
}

func (n *memNet) link(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

type memTransport struct {
	net  *memNet
	from string
}

func (t memTransport) Do(ctx context.Context, to Member, method, path string, body io.Reader) (io.ReadCloser, error) {
	t.net.mu.Lock()
	h := t.net.handlers[to.Address]
	cut := t.net.cut[t.net.link(t.from, to.Address)]
	t.net.mu.Unlock()
	if h == nil || cut {
		return nil, errors.New("connection refused")
	}
	req := httptest.NewRequestWithContext(ctx, method, path, body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	return io.NopCloser(rec.Body), nil
}

type cluster struct {
	t     *testing.T
	net   *memNet
	nodes map[string]*DB
	dirs  map[string]string
	cfg   func(*Config)
}

func (c *cluster) config(name string) Config {
	cfg := Config{Dir: c.dirs[name], Self: Member{Node: name, Address: name, Cert: "cert-" + name},
		Transport: memTransport{c.net, name}, Logger: testLogger(c.t), Tick: 20 * time.Millisecond,
		Wait: 10 * time.Second}
	if c.cfg != nil {
		c.cfg(&cfg)
	}
	return cfg
}

// start opens a node; it registers its handler first, so that the others
// reach it as soon as it runs.
func (c *cluster) start(name string) *DB {
	c.t.Helper()
	if c.dirs[name] == "" {
		c.dirs[name] = c.t.TempDir()
	}
	h := &lateHandler{}
	c.net.mu.Lock()
	c.net.handlers[name] = h
	c.net.mu.Unlock()
	d, err := Open(c.config(name))
	if err != nil {
		c.t.Fatalf("open %s: %v", name, err)
	}
	h.set(d.Handler())
	c.nodes[name] = d
	return d
}

func (c *cluster) kill(name string) {
	c.net.mu.Lock()
	delete(c.net.handlers, name)
	c.net.mu.Unlock()
	c.nodes[name].Close()
	delete(c.nodes, name)
}

// lateHandler answers 503 until the node is open.
type lateHandler struct{ h atomic.Pointer[http.Handler] }

func (l *lateHandler) set(h http.Handler) { l.h.Store(&h) }
func (l *lateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := l.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	http.Error(w, "starting", http.StatusServiceUnavailable)
}

// newCluster starts n1 alone, then adds and starts the others the way
// "robinmail node add" and "node join" do.
func newCluster(t *testing.T, n int, cfg func(*Config)) *cluster {
	c := &cluster{t: t, net: &memNet{handlers: map[string]http.Handler{}, cut: map[string]bool{}},
		nodes: map[string]*DB{}, dirs: map[string]string{}, cfg: cfg}
	t.Cleanup(func() {
		for name := range c.nodes {
			c.nodes[name].Close()
		}
	})
	c.start("n1")
	for i := 2; i <= n; i++ {
		c.add(fmt.Sprintf("n%d", i))
	}
	return c
}

// add admits a node through a member and starts it.
func (c *cluster) add(name string) *DB {
	c.t.Helper()
	m := Member{Node: name, Address: name, Cert: "cert-" + name}
	m.ID = RaftID(m.Cert)
	via := c.anyNode()
	if err := via.AddMember(ctx, m); err != nil {
		c.t.Fatalf("add %s: %v", name, err)
	}
	c.dirs[name] = c.t.TempDir()
	if err := Join(c.dirs[name], m.ID, via.Members()); err != nil {
		c.t.Fatal(err)
	}
	return c.start(name)
}

func (c *cluster) anyNode() *DB {
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range c.nodes {
			if !yield(n) {
				return
			}
		}
	})
	return c.nodes[names[0]]
}

func (c *cluster) leader() (string, *DB) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for name, d := range c.nodes {
			if d.Status().State == "StateLeader" {
				return name, d
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatal("no leader")
	return "", nil
}

// dump is every row of every table of a node, in a fixed order, after it
// has applied everything committed.
func dump(t *testing.T, d *DB) string {
	t.Helper()
	if err := d.Barrier(ctx); err != nil {
		t.Fatal(err)
	}
	tables := query(t, d, db.None, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	var b strings.Builder
	for _, row := range tables {
		name := row[0].(string)
		rows := query(t, d, db.None, `SELECT rowid, * FROM "`+name+`" ORDER BY 1`)
		fmt.Fprintf(&b, "%s %v\n", name, rows)
	}
	return b.String()
}

func waitSame(t *testing.T, c *cluster) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var first, firstName string
		same := true
		for name, d := range c.nodes {
			s := dump(t, d)
			if first == "" {
				first, firstName = s, name
			} else if s != first {
				same = false
				if time.Now().After(deadline) {
					t.Fatalf("%s and %s differ:\n%s\n----\n%s", firstName, name, first, s)
				}
			}
		}
		if same {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestClusterReplicates(t *testing.T) {
	c := newCluster(t, 3, nil)
	exec(t, c.nodes["n2"], `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	for i := range 30 {
		name := fmt.Sprintf("n%d", i%3+1)
		exec(t, c.nodes[name], `INSERT INTO t (v) VALUES (?)`, name)
		// Read your write on any node: reads are linearizable.
		other := c.nodes[fmt.Sprintf("n%d", (i+1)%3+1)]
		if n := query(t, other, db.Weak, `SELECT count(*) FROM t`)[0][0]; n != int64(i+1) {
			t.Fatalf("after write %d a read on another node sees %v rows", i+1, n)
		}
	}
	waitSame(t, c)
	if got := len(c.nodes["n3"].Members()); got != 3 {
		t.Errorf("%d members", got)
	}
}

// TestLeaderKilledDuringWrites: writers on every node keep writing while
// the leader is killed and comes back. Every write that returned success
// is there exactly once, nothing else is, and the counters the batches
// compute in SQL (as UIDs and modseq are) have no gaps or doubles.
func TestLeaderKilledDuringWrites(t *testing.T) {
	c := newCluster(t, 3, nil)
	exec(t, c.nodes["n1"], `CREATE TABLE seq (one INTEGER PRIMARY KEY, n INTEGER NOT NULL)`)
	exec(t, c.nodes["n1"], `INSERT INTO seq VALUES (1, 0)`)
	exec(t, c.nodes["n1"], `CREATE TABLE mail (key TEXT PRIMARY KEY, uid INTEGER NOT NULL UNIQUE)`)

	var mu sync.Mutex
	acked := map[string]bool{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var attempts atomic.Int64
	writer := func(w int) {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("w%d-%d", w, i)
			// Like a delivery: a counter taken in SQL, a row keyed by the
			// delivery key, sent again after a failure until it is in.
			for {
				mu.Lock()
				var d *DB
				for _, name := range []string{fmt.Sprintf("n%d", w%3+1), "n1", "n2", "n3"} {
					if c.nodes[name] != nil {
						d = c.nodes[name]
						break
					}
				}
				mu.Unlock()
				attempts.Add(1)
				_, err := d.Execute(ctx,
					db.Stmt(`UPDATE seq SET n = n + 1`),
					db.Stmt(`INSERT INTO mail (key, uid) SELECT ?, n FROM seq`, key))
				if err == nil || db.IsUnique(err) {
					mu.Lock()
					acked[key] = true
					mu.Unlock()
					break
				}
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
	}
	for w := range 6 {
		wg.Add(1)
		go writer(w)
	}
	time.Sleep(300 * time.Millisecond)
	name, _ := c.leader()
	mu.Lock()
	c.kill(name)
	mu.Unlock()
	t.Logf("killed the leader %s", name)
	time.Sleep(time.Second)
	newName, _ := c.leader()
	if newName == name {
		t.Fatal("the dead node leads")
	}
	mu.Lock()
	c.start(name)
	mu.Unlock()
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()

	waitSame(t, c)
	d := c.nodes["n1"]
	rows := query(t, d, db.Weak, `SELECT key, uid FROM mail ORDER BY uid`)
	n := query(t, d, db.Weak, `SELECT n FROM seq`)[0][0].(int64)
	if int64(len(rows)) != n {
		t.Errorf("%d rows, counter at %d", len(rows), n)
	}
	for i, r := range rows {
		if r[1] != int64(i+1) {
			t.Fatalf("uid %v at position %d: a gap or a double", r[1], i)
		}
		if !acked[r[0].(string)] {
			t.Errorf("%s is stored but was never acknowledged", r[0])
		}
	}
	if len(rows) != len(acked) {
		t.Errorf("%d acknowledged, %d stored", len(acked), len(rows))
	}
	t.Logf("%d writes acknowledged in %d attempts", len(acked), attempts.Load())
}

// TestPartition: a node cut off from the others refuses writes and
// linearizable reads; the majority goes on; healed, the node catches up.
func TestPartition(t *testing.T) {
	c := newCluster(t, 3, func(cfg *Config) { cfg.Wait = 2 * time.Second })
	exec(t, c.nodes["n1"], `CREATE TABLE t (v TEXT)`)
	lead, _ := c.leader()
	// Cut the leader off: the minority side.
	c.net.mu.Lock()
	for _, other := range []string{"n1", "n2", "n3"} {
		if other != lead {
			c.net.cut[c.net.link(lead, other)] = true
		}
	}
	c.net.mu.Unlock()
	minority := c.nodes[lead]
	if _, err := minority.Execute(ctx, db.Stmt(`INSERT INTO t VALUES ('minority')`)); err == nil {
		t.Error("the cut-off node took a write")
	}
	if _, err := minority.Query(ctx, db.Weak, db.Stmt(`SELECT 1`)); err == nil {
		t.Error("the cut-off node answered a linearizable read")
	}
	var majority *DB
	for name, d := range c.nodes {
		if name != lead {
			majority = d
		}
	}
	exec(t, majority, `INSERT INTO t VALUES ('majority')`)
	// After 5 seconds without a leader a local read refuses too.
	time.Sleep(4 * time.Second)
	if _, err := minority.Query(ctx, db.None, db.Stmt(`SELECT 1`)); !errors.Is(err, db.ErrStale) {
		t.Errorf("stale local read: %v", err)
	}
	c.net.mu.Lock()
	clear(c.net.cut)
	c.net.mu.Unlock()
	waitSame(t, c)
	rows := query(t, minority, db.Weak, `SELECT v FROM t`)
	if fmt.Sprint(rows) != "[[majority]]" {
		t.Errorf("after healing: %v", rows)
	}
}

// TestSnapshotAndRestart: the log is compacted; a node that was away
// longer than the log reaches gets the leader's database as a snapshot;
// a restarted node comes back from its file and the rest of the log.
func TestSnapshotAndRestart(t *testing.T) {
	c := newCluster(t, 3, func(cfg *Config) { cfg.SnapshotEvery = 50; cfg.KeepEntries = 10 })
	exec(t, c.nodes["n1"], `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	c.kill("n3")
	for i := range 200 {
		exec(t, c.nodes["n1"], `INSERT INTO t (v) VALUES (?)`, fmt.Sprint(i))
	}
	if s := c.nodes["n1"].Status(); s.Snapshot < 100 {
		t.Fatalf("no compaction: %+v", s)
	}
	n3 := c.start("n3")
	waitSame(t, c)
	if n := query(t, n3, db.Weak, `SELECT count(*) FROM t`)[0][0]; n != int64(200) {
		t.Errorf("n3 has %v rows", n)
	}
	// Restart all three from disk.
	for _, name := range []string{"n1", "n2", "n3"} {
		c.kill(name)
	}
	for _, name := range []string{"n1", "n2", "n3"} {
		c.start(name)
	}
	exec(t, c.nodes["n2"], `INSERT INTO t (v) VALUES ('after')`)
	waitSame(t, c)
	if n := query(t, c.nodes["n3"], db.Weak, `SELECT count(*) FROM t`)[0][0]; n != int64(201) {
		t.Errorf("after restart: %v rows", n)
	}
}

// TestMembership: a node is added to a cluster with data and catches up;
// a node is removed and the others go on without it; the leader removes
// itself.
func TestMembership(t *testing.T) {
	c := newCluster(t, 2, func(cfg *Config) { cfg.SnapshotEvery = 20; cfg.KeepEntries = 5 })
	exec(t, c.nodes["n1"], `CREATE TABLE t (v TEXT)`)
	for i := range 50 {
		exec(t, c.nodes["n2"], `INSERT INTO t VALUES (?)`, fmt.Sprint(i))
	}
	c.add("n3")
	waitSame(t, c)
	if got := len(c.nodes["n3"].Members()); got != 3 {
		t.Fatalf("n3 sees %d members", got)
	}
	// Remove n2 (stopped first), then the others still have a majority.
	id2 := c.nodes["n2"].Self().ID
	c.kill("n2")
	if err := c.nodes["n1"].RemoveMember(ctx, id2); err != nil {
		t.Fatal(err)
	}
	exec(t, c.nodes["n3"], `INSERT INTO t VALUES ('two left')`)
	if got := len(c.nodes["n1"].Members()); got != 2 {
		t.Errorf("%d members after remove", got)
	}
	// The leader removes itself; the remaining node leads alone.
	name, lead := c.leader()
	if err := lead.RemoveMember(ctx, lead.Self().ID); err != nil {
		t.Fatal(err)
	}
	c.kill(name)
	var last *DB
	for _, d := range c.nodes {
		last = d
	}
	exec(t, last, `INSERT INTO t VALUES ('alone')`)
	if got := len(last.Members()); got != 1 {
		t.Errorf("%d members at the end", got)
	}
}

func TestBackupAndLoad(t *testing.T) {
	d := openOne(t, t.TempDir())
	defer d.Close()
	exec(t, d, `CREATE TABLE t (v TEXT)`)
	exec(t, d, `INSERT INTO t VALUES ('one')`)
	f, err := os.Create(filepath.Join(t.TempDir(), "backup.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Backup(ctx, f); err != nil {
		t.Fatal(err)
	}
	exec(t, d, `INSERT INTO t VALUES ('two')`)

	// Load it into another node, which keeps its own membership.
	e := openOne(t, t.TempDir())
	defer e.Close()
	exec(t, e, `CREATE TABLE other (x)`)
	if err := e.Load(ctx, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if rows := query(t, e, db.Weak, `SELECT v FROM t`); fmt.Sprint(rows) != "[[one]]" {
		t.Errorf("loaded: %v", rows)
	}
	if rows := query(t, e, db.Weak, `SELECT count(*) FROM sqlite_master WHERE name = 'other'`); rows[0][0] != int64(0) {
		t.Error("the table made before the load is still there")
	}
	exec(t, e, `INSERT INTO t VALUES ('after')`)
	if m := e.Members(); len(m) != 1 || m[0].ID != e.Self().ID {
		t.Errorf("members after load: %+v", m)
	}
	// And it survives a restart.
	dir := e.cfg.Dir
	e.Close()
	e = openOne(t, dir)
	if rows := query(t, e, db.Weak, `SELECT v FROM t ORDER BY v`); fmt.Sprint(rows) != "[[after] [one]]" {
		t.Errorf("after restart: %v", rows)
	}
}
