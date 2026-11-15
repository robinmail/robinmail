// Package raftdb is robinmail's database: SQLite, replicated with Raft,
// inside the robinmail process. It implements internal/db.
//
// # Writes
//
// A write is a batch of statements. Execute proposes it to Raft, which
// orders it in the log; once a majority holds it, every node applies it to
// its own SQLite file, in log order, in one transaction: a statement that
// fails rolls the batch back on every node alike. The node that proposed
// waits until it has applied the entry itself and returns the results. A
// follower's proposal reaches the leader as a Raft message (MsgProp) over
// the node-to-node mutual TLS; there is no other way to send SQL to a
// node.
//
// Every proposal carries a random id. Applying it records the id and its
// outcome (raft_request), and an entry whose id was applied before is not
// applied again: its first outcome is the answer. So a proposal that got
// no answer in time (a leader change, a lost message) is simply proposed
// again, Execute and ExecuteIdempotent alike, until Wait has passed. Only
// then does a write end with db.ErrUnknownOutcome.
//
// Applying must give the same result on every node, so a statement may
// not read the clock or chance (now(), random()): the store passes such
// values as parameters. Values are normalised to what JSON makes of them
// (db.Normalize) before they are proposed. An error that is not an answer
// about the statement (I/O, a full disk, a damaged file) is not recorded
// as the outcome: the node stops applying and says why, rather than
// differ from the others.
//
// # Reads
//
// A read at Weak, Linearizable or Strong is linearizable through Raft's
// ReadIndex: the node asks the leader for its commit index, the leader
// confirms with a quorum that it still leads, and the node answers from
// its own copy once it has applied that far. Reads that wait together
// share one round. A lease would save the round to the quorum but depends
// on the clocks of the nodes running at the same pace; ReadIndex depends
// on nothing but messages. None reads the local copy as it is, unless the
// node has been without a leader for 5 seconds (db.ErrStale).
//
// # Files
//
// In <dir>: data.db, the database itself (WAL, synchronous NORMAL: what a
// crash loses, the log redoes); raft.db, the Raft log, HardState and
// snapshot metadata (synchronous FULL); lock, held while a process has the
// node open. data.db holds three tables of this package besides the
// store's: raft_applied (the index applied last, written in the same
// transaction as the entry), raft_member (the members, as applied from
// the log) and raft_request (the outcomes of proposals, for the
// deduplication above).
//
// # Snapshots
//
// data.db is the state machine and is durable on its own, so a snapshot
// needs no copy: every SnapshotEvery entries the node checkpoints data.db
// and drops the log up to KeepEntries behind. A member that is further
// behind gets a Raft snapshot message without data, and fetches the
// leader's data.db (an online backup, page by page, so rowids are as they
// were) over the mutual TLS before Raft learns of it. The copy may be of a
// later index than the snapshot named: raft_applied in it says which, and
// entries up to that are skipped.
package raftdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/robinmail/robinmail/internal/db"
	"github.com/robinmail/robinmail/internal/metrics"
	"go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
	sqlite "modernc.org/sqlite"
)

// Member is a node of the cluster as Raft knows it.
type Member struct {
	// ID is the Raft id, RaftID of the certificate it joined with.
	ID uint64 `json:"id,string"`
	// Node is the robinmail node id; empty for an instance without a node
	// section.
	Node string `json:"node"`
	// Address is where the others reach it (host:port of the node API).
	Address string `json:"address"`
	// Cert is the SHA-256 of its certificate, in hex.
	Cert string `json:"cert"`
}

// RaftID derives a node's Raft id from its certificate's fingerprint.
func RaftID(cert string) uint64 {
	sum := sha256.Sum256([]byte("robinmail raft id\x00" + cert))
	if id := binary.BigEndian.Uint64(sum[:8]); id != 0 {
		return id
	}
	return 1
}

// Transport reaches another member over the node-to-node mutual TLS. The
// path is one of Handler's.
type Transport interface {
	Do(ctx context.Context, to Member, method, path string, body io.Reader) (io.ReadCloser, error)
}

// Config describes a node.
type Config struct {
	// Dir holds the files, <data_dir>/db.
	Dir string
	// Self is this node. Its ID is RaftID(Cert) when the node is new, and
	// what the log says after that.
	Self Member
	// Transport reaches the other members. Without one the node can only
	// be a cluster of one.
	Transport Transport
	Logger    *slog.Logger
	// Tick is Raft's clock. [100ms] An election starts after 10 to 20
	// ticks without a leader; the leader sends a heartbeat every tick.
	Tick time.Duration
	// SnapshotEvery is how many applied entries lead to a compaction of
	// the log. [10000]
	SnapshotEvery uint64
	// KeepEntries is how many entries stay in the log behind a snapshot,
	// for members that are a little behind. [5000]
	KeepEntries uint64
	// Wait is how long a call keeps trying. [15s]
	Wait time.Duration
}

// ErrLocked is returned by Open when another process has the node open.
var ErrLocked = errors.New("raftdb: another process has this database open")

// ErrStopped is returned after Close, or after the node stopped applying.
var ErrStopped = errors.New("raftdb: the node is stopped")

// DB is one node. It implements db.DB.
type DB struct {
	cfg  Config
	log  *slog.Logger
	id   uint64
	lock *os.File
	logs *logStore
	ms   *raft.MemoryStorage
	node raft.Node

	// mu guards the handles of data.db against a swap (a snapshot from
	// the leader, Load): readers and the applier hold it shared.
	mu   sync.RWMutex
	wdb  *sql.DB // one connection: the applier's
	rdb  *sql.DB // readers
	path string

	applied   atomic.Uint64 // the index data.db holds
	appliedMu sync.Mutex
	appliedCh chan struct{} // closed and replaced when applied moves

	snapIndex atomic.Uint64
	confState *pb.ConfState // the applier's, as it applies
	// stateMu guards the pair applied index and configuration at that
	// index, for snapshots Raft asks for.
	stateMu      sync.Mutex
	appliedConf  *pb.ConfState
	appliedIndex uint64
	sinceSnap    uint64

	membersMu sync.RWMutex
	members   map[uint64]Member
	seeds     []Member

	lead       atomic.Uint64
	leaderLost atomic.Int64 // unix nanoseconds since when no leader is known; 0 when one is

	waitMu  sync.Mutex
	waiters map[string]chan outcome

	readMu      sync.Mutex
	readNext    *notifier
	readTrigger chan struct{}
	readStates  chan raft.ReadState

	peersMu sync.Mutex
	peers   map[uint64]*peer

	applyc  chan applyBatch
	stop    chan struct{}
	stopped sync.Once
	wg      sync.WaitGroup
	failure atomic.Pointer[error]
}

type applyBatch struct {
	entries []*pb.Entry
	done    chan struct{}
}

// outcome is what applying a proposal gave, as raft_request keeps it.
type outcome struct {
	Results []db.Result `json:"results,omitempty"`
	Err     *db.Error   `json:"error,omitempty"`
}

// proposal is the data of a normal log entry.
type proposal struct {
	ID    string         `json:"id"`
	Stmts []db.Statement `json:"stmts"`
}

const dataSchema = `
CREATE TABLE IF NOT EXISTS raft_applied (one INTEGER PRIMARY KEY CHECK (one = 1), idx INTEGER NOT NULL);
INSERT OR IGNORE INTO raft_applied (one, idx) VALUES (1, 0);
CREATE TABLE IF NOT EXISTS raft_member (id INTEGER PRIMARY KEY, node TEXT NOT NULL, address TEXT NOT NULL, cert TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS raft_request (id TEXT PRIMARY KEY, idx INTEGER NOT NULL, result TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS raft_request_idx ON raft_request (idx);`

// requestWindow is how many entries back an outcome is kept for a
// repeated proposal. A proposer gives up after Wait, so this only has to
// outlast the entries of that long.
const requestWindow = 200000

// Open opens the node in cfg.Dir and starts it. A new directory starts a
// cluster of one, unless "robinmail node join" prepared it to join one.
func Open(cfg Config) (*DB, error) {
	if cfg.Tick <= 0 {
		cfg.Tick = 100 * time.Millisecond
	}
	if cfg.SnapshotEvery == 0 {
		cfg.SnapshotEvery = 10000
	}
	if cfg.KeepEntries == 0 {
		cfg.KeepEntries = 5000
	}
	if cfg.Wait <= 0 {
		cfg.Wait = 15 * time.Second
	}
	d := &DB{
		cfg:         cfg,
		log:         cfg.Logger,
		appliedCh:   make(chan struct{}),
		members:     map[uint64]Member{},
		waiters:     map[string]chan outcome{},
		readNext:    newNotifier(),
		readTrigger: make(chan struct{}, 1),
		readStates:  make(chan raft.ReadState, 64),
		peers:       map[uint64]*peer{},
		applyc:      make(chan applyBatch, 64),
		stop:        make(chan struct{}),
		path:        filepath.Join(cfg.Dir, "data.db"),
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("raftdb: %w", err)
	}
	lock, err := lockDir(cfg.Dir)
	if err != nil {
		return nil, err
	}
	d.lock = lock
	ok := false
	defer func() {
		if !ok {
			d.closeFiles()
		}
	}()
	if d.logs, err = openLog(filepath.Join(cfg.Dir, "raft.db")); err != nil {
		return nil, err
	}
	if err := d.openData(); err != nil {
		return nil, err
	}

	// Who this node is: fixed by the first start.
	var stored struct {
		ID uint64 `json:"id,string"`
	}
	found, err := d.logs.getJSON("id", &stored)
	if err != nil {
		return nil, fmt.Errorf("raftdb: %w", err)
	}
	switch {
	case found:
		d.id = stored.ID
	case cfg.Self.ID != 0:
		d.id = cfg.Self.ID
	default:
		d.id = RaftID(cfg.Self.Cert)
	}
	if !found {
		stored.ID = d.id
		if err := d.logs.setJSON("id", stored); err != nil {
			return nil, fmt.Errorf("raftdb: %w", err)
		}
	}
	d.cfg.Self.ID = d.id
	if _, err := d.logs.getJSON("seeds", &d.seeds); err != nil {
		return nil, fmt.Errorf("raftdb: %w", err)
	}

	empty, err := d.logs.empty()
	if err != nil {
		return nil, fmt.Errorf("raftdb: %w", err)
	}
	ms, meta, err := d.logs.load()
	if err != nil {
		return nil, err
	}
	d.ms = ms
	d.snapIndex.Store(meta.GetIndex())
	d.confState = pb.EnsureConfState(proto.Clone(meta.GetConfState()).(*pb.ConfState))
	d.appliedConf, d.appliedIndex = d.confState, meta.GetIndex()
	if d.applied.Load() < meta.GetIndex() {
		return nil, fmt.Errorf("raftdb: data.db is at entry %d, older than the snapshot at %d: the database file does not belong to this log", d.applied.Load(), meta.GetIndex())
	}
	hs, _, _ := ms.InitialState()

	rc := &raft.Config{
		ID:                        d.id,
		ElectionTick:              10,
		HeartbeatTick:             1,
		Storage:                   storage{ms, d},
		Applied:                   meta.GetIndex(),
		MaxSizePerMsg:             1 << 20,
		MaxCommittedSizePerReady:  64 << 20,
		MaxUncommittedEntriesSize: 1 << 30,
		MaxInflightMsgs:           256,
		CheckQuorum:               true,
		PreVote:                   true,
		ReadOnlyOption:            raft.ReadOnlySafe,
		StepDownOnRemoval:         true,
		Logger:                    raftLogger{d.log},
	}
	bootstrap := empty && len(d.seeds) == 0
	if bootstrap {
		self, _ := json.Marshal(d.cfg.Self)
		d.node = raft.StartNode(rc, []raft.Peer{{ID: d.id, Context: self}})
		d.log.Info("started a new cluster", "raft_id", fmt.Sprintf("%x", d.id))
	} else {
		d.node = raft.RestartNode(rc)
	}
	d.wg.Add(3)
	go d.loop()
	go d.applier()
	go d.readLoop()
	ok = true

	// Replay what is committed, then a cluster of one elects itself at
	// once instead of after an election timeout.
	commit := hs.GetCommit()
	if bootstrap {
		commit = 1
	}
	wctx, cancel := context.WithTimeout(context.Background(), cfg.Wait)
	defer cancel()
	if err := d.waitReplayed(wctx, commit); err != nil {
		d.Close()
		return nil, fmt.Errorf("raftdb: replaying the log: %w", err)
	}
	if v := d.voters(); len(v) == 1 && v[0] == d.id {
		// Raft refuses to campaign while the configuration it just
		// applied is not yet acknowledged by Advance: ask again.
		for i := 0; i < 100 && d.lead.Load() == raft.None; i++ {
			d.node.Campaign(wctx)
			time.Sleep(10 * time.Millisecond)
		}
	}
	return d, nil
}

func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("raftdb: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("raftdb: lock: %w", err)
	}
	return f, nil
}

// Locked reports whether a process has the node in dir open.
func Locked(dir string) bool {
	f, err := lockDir(dir)
	if err != nil {
		return errors.Is(err, ErrLocked)
	}
	f.Close()
	return false
}

// openData opens data.db, adds this package's tables and reads what was
// applied.
func (d *DB) openData() error {
	w, err := openSQLite(d.path, "NORMAL", false, 1)
	if err != nil {
		return fmt.Errorf("raftdb: %s: %w", d.path, err)
	}
	if _, err := w.Exec(dataSchema); err != nil {
		w.Close()
		return fmt.Errorf("raftdb: %s: %w", d.path, err)
	}
	r, err := openSQLite(d.path, "NORMAL", true, 16)
	if err != nil {
		w.Close()
		return fmt.Errorf("raftdb: %s: %w", d.path, err)
	}
	var applied int64
	if err := w.QueryRow(`SELECT idx FROM raft_applied`).Scan(&applied); err != nil {
		w.Close()
		r.Close()
		return fmt.Errorf("raftdb: %s: %w", d.path, err)
	}
	d.wdb, d.rdb = w, r
	d.applied.Store(uint64(applied))
	return d.loadMembers()
}

func (d *DB) loadMembers() error {
	rows, err := d.wdb.Query(`SELECT id, node, address, cert FROM raft_member`)
	if err != nil {
		return fmt.Errorf("raftdb: members: %w", err)
	}
	defer rows.Close()
	m := map[uint64]Member{}
	for rows.Next() {
		var x Member
		var id int64
		if err := rows.Scan(&id, &x.Node, &x.Address, &x.Cert); err != nil {
			return fmt.Errorf("raftdb: members: %w", err)
		}
		x.ID = uint64(id)
		m[x.ID] = x
	}
	if err := rows.Err(); err != nil {
		return err
	}
	d.membersMu.Lock()
	d.members = m
	d.membersMu.Unlock()
	d.dropPeersNotIn(m)
	return nil
}

// Close stops the node and closes its files.
func (d *DB) Close() error {
	d.stopped.Do(func() {
		close(d.stop)
		d.node.Stop()
	})
	d.wg.Wait()
	d.peersMu.Lock()
	for _, p := range d.peers {
		p.close()
	}
	d.peers = map[uint64]*peer{}
	d.peersMu.Unlock()
	return d.closeFiles()
}

func (d *DB) closeFiles() error {
	var errs []error
	if d.wdb != nil {
		errs = append(errs, d.wdb.Close())
	}
	if d.rdb != nil {
		errs = append(errs, d.rdb.Close())
	}
	if d.logs != nil {
		errs = append(errs, d.logs.close())
	}
	if d.lock != nil {
		errs = append(errs, d.lock.Close())
	}
	return errors.Join(errs...)
}

// fail stops the node for good: applying gave an error that another node
// would not have had. It says why on every call from now on.
func (d *DB) fail(err error) {
	err = fmt.Errorf("raftdb: this node stopped applying the log: %w", err)
	if d.failure.CompareAndSwap(nil, &err) {
		d.log.Error("the database of this node stopped; fix the cause and restart: the log is applied again from where it stopped", "err", err)
		go d.stopped.Do(func() {
			close(d.stop)
			d.node.Stop()
		})
	}
}

func (d *DB) err() error {
	if p := d.failure.Load(); p != nil {
		return *p
	}
	select {
	case <-d.stop:
		return ErrStopped
	default:
	}
	return nil
}

// ---- the Raft loop ----

func (d *DB) loop() {
	defer d.wg.Done()
	t := time.NewTicker(d.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
			d.node.Tick()
		case rd := <-d.node.Ready():
			if rd.SoftState != nil {
				d.setLead(rd.SoftState.Lead)
			}
			if err := d.logs.save(rd.HardState, rd.Entries, rd.Snapshot); err != nil {
				d.fail(fmt.Errorf("writing the raft log: %w", err))
				return
			}
			if rd.Snapshot != nil && !raft.IsEmptySnap(rd.Snapshot) {
				if err := d.ms.ApplySnapshot(rd.Snapshot); err != nil && !errors.Is(err, raft.ErrSnapOutOfDate) {
					d.fail(err)
					return
				}
				d.snapIndex.Store(rd.Snapshot.GetMetadata().GetIndex())
			}
			if rd.HardState != nil && !raft.IsEmptyHardState(rd.HardState) {
				d.ms.SetHardState(rd.HardState)
			}
			if len(rd.Entries) > 0 {
				if err := d.ms.Append(rd.Entries); err != nil {
					d.fail(err)
					return
				}
			}
			d.send(rd.Messages)
			for _, rs := range rd.ReadStates {
				select {
				case d.readStates <- rs:
				default:
				}
			}
			if len(rd.CommittedEntries) > 0 || (rd.Snapshot != nil && !raft.IsEmptySnap(rd.Snapshot)) {
				b := applyBatch{entries: rd.CommittedEntries, done: make(chan struct{})}
				select {
				case d.applyc <- b:
				case <-d.stop:
					return
				}
				// A configuration change must be in force before Raft goes
				// on: the next Ready may already depend on it.
				if hasConfChange(rd.CommittedEntries) {
					select {
					case <-b.done:
					case <-d.stop:
						return
					}
				}
			}
			d.node.Advance()
		}
	}
}

func hasConfChange(es []*pb.Entry) bool {
	for _, e := range es {
		if e.GetType() == pb.EntryConfChange || e.GetType() == pb.EntryConfChangeV2 {
			return true
		}
	}
	return false
}

func (d *DB) setLead(lead uint64) {
	old := d.lead.Swap(lead)
	if lead == raft.None {
		if old != raft.None || d.leaderLost.Load() == 0 {
			d.leaderLost.Store(time.Now().UnixNano())
		}
	} else {
		d.leaderLost.Store(0)
	}
	if old != lead {
		d.log.Info("raft leader", "leader", fmt.Sprintf("%x", lead), "self", fmt.Sprintf("%x", d.id))
	}
}

// ---- applying ----

func (d *DB) applier() {
	defer d.wg.Done()
	for {
		select {
		case <-d.stop:
			return
		case b := <-d.applyc:
			if d.err() == nil {
				if err := d.apply(b.entries); err != nil {
					d.fail(err)
				}
			}
			close(b.done)
		}
	}
}

// apply applies committed entries in one transaction.
func (d *DB) apply(entries []*pb.Entry) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	ctx := context.Background()
	tx, err := d.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var last uint64
	answers := map[string]outcome{}
	membersChanged := false
	for _, e := range entries {
		idx := e.GetIndex()
		skip := idx <= d.applied.Load()
		switch e.GetType() {
		case pb.EntryConfChange:
			var cc pb.ConfChange
			if err := proto.Unmarshal(e.GetData(), &cc); err != nil {
				return fmt.Errorf("entry %d: %w", idx, err)
			}
			d.confState = d.node.ApplyConfChange(&cc)
			if !skip {
				if err := applyMember(ctx, tx, &cc); err != nil {
					return fmt.Errorf("entry %d: %w", idx, err)
				}
				membersChanged = true
			}
		case pb.EntryConfChangeV2:
			var cc pb.ConfChangeV2
			if err := proto.Unmarshal(e.GetData(), &cc); err != nil {
				return fmt.Errorf("entry %d: %w", idx, err)
			}
			d.confState = d.node.ApplyConfChange(&cc)
		case pb.EntryNormal:
			if skip || len(e.GetData()) == 0 {
				break
			}
			var p proposal
			if err := json.Unmarshal(e.GetData(), &p); err != nil {
				// Every node fails the same way on the same entry.
				d.log.Error("an entry of the log does not decode; skipped", "index", idx, "err", err)
				break
			}
			out, err := applyProposal(ctx, tx, idx, &p)
			if err != nil {
				return fmt.Errorf("entry %d: %w", idx, err)
			}
			answers[p.ID] = out
		}
		if skip {
			continue
		}
		if idx%1024 == 0 && idx > requestWindow {
			if _, err := tx.ExecContext(ctx, `DELETE FROM raft_request WHERE idx < ?`, int64(idx-requestWindow)); err != nil {
				return fmt.Errorf("entry %d: %w", idx, err)
			}
		}
		last = idx
	}
	if last > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE raft_applied SET idx = ?`, int64(last)); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		d.setApplied(last)
		d.sinceSnap += uint64(len(entries))
	}
	if n := len(entries); n > 0 {
		// Raft's applied index: entries skipped because data.db held
		// them count too.
		d.stateMu.Lock()
		d.appliedConf, d.appliedIndex = proto.Clone(d.confState).(*pb.ConfState), entries[n-1].GetIndex()
		d.stateMu.Unlock()
		d.setApplied(0)
	}
	if membersChanged {
		if err := d.loadMembers(); err != nil {
			return err
		}
	}
	if len(answers) > 0 {
		d.waitMu.Lock()
		for id, out := range answers {
			if ch, ok := d.waiters[id]; ok {
				select {
				case ch <- out:
				default:
				}
			}
		}
		d.waitMu.Unlock()
	}
	if d.sinceSnap >= d.cfg.SnapshotEvery {
		d.compact()
	}
	return nil
}

// applyMember writes a configuration change into raft_member. The
// context of an added or updated member is the Member, as JSON.
func applyMember(ctx context.Context, tx *sql.Tx, cc *pb.ConfChange) error {
	switch cc.GetType() {
	case pb.ConfChangeAddNode, pb.ConfChangeAddLearnerNode, pb.ConfChangeUpdateNode:
		var m Member
		if len(cc.GetContext()) > 0 {
			if err := json.Unmarshal(cc.GetContext(), &m); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO raft_member (id, node, address, cert) VALUES (?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET node = excluded.node, address = excluded.address, cert = excluded.cert`,
			int64(cc.GetNodeId()), m.Node, m.Address, m.Cert)
		return err
	case pb.ConfChangeRemoveNode:
		_, err := tx.ExecContext(ctx, `DELETE FROM raft_member WHERE id = ?`, int64(cc.GetNodeId()))
		return err
	}
	return nil
}

// applyProposal runs one proposal inside the applier's transaction, under
// a savepoint, and records its outcome. An error returned is a fault of
// this node; a refused statement is an outcome.
func applyProposal(ctx context.Context, tx *sql.Tx, idx uint64, p *proposal) (outcome, error) {
	var out outcome
	var prior string
	err := tx.QueryRowContext(ctx, `SELECT result FROM raft_request WHERE id = ?`, p.ID).Scan(&prior)
	switch {
	case err == nil:
		// Proposed again after an answer that did not arrive: the first
		// run's outcome stands.
		if err := json.Unmarshal([]byte(prior), &out); err != nil {
			return out, err
		}
		return out, nil
	case !errors.Is(err, sql.ErrNoRows):
		return out, err
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT p`); err != nil {
		return out, err
	}
	total, err := totalChanges(ctx, tx)
	if err != nil {
		return out, err
	}
	for i, st := range p.Stmts {
		r, err := run(ctx, tx, st, &total)
		if err != nil {
			e, answer := statementError(i, err)
			if !answer {
				return out, err
			}
			if _, err := tx.ExecContext(ctx, `ROLLBACK TO p`); err != nil {
				return out, err
			}
			out = outcome{Err: e}
			break
		}
		out.Results = append(out.Results, r)
	}
	if _, err := tx.ExecContext(ctx, `RELEASE p`); err != nil {
		return out, err
	}
	data, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO raft_request (id, idx, result) VALUES (?, ?, ?)`, p.ID, int64(idx), string(data)); err != nil {
		return out, err
	}
	return out, nil
}

func (d *DB) setApplied(i uint64) {
	d.appliedMu.Lock()
	if i > d.applied.Load() {
		d.applied.Store(i)
	}
	close(d.appliedCh)
	d.appliedCh = make(chan struct{})
	d.appliedMu.Unlock()
}

// waitApplied waits until data.db holds entry i.
func (d *DB) waitApplied(ctx context.Context, i uint64) error {
	for {
		d.appliedMu.Lock()
		ch := d.appliedCh
		d.appliedMu.Unlock()
		if d.applied.Load() >= i {
			return nil
		}
		if err := d.err(); err != nil {
			return err
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-d.stop:
			if err := d.err(); err != nil {
				return err
			}
			return ErrStopped
		}
	}
}

// waitReplayed waits until Raft has handed every entry up to i to the
// applier, also those data.db held already.
func (d *DB) waitReplayed(ctx context.Context, i uint64) error {
	for {
		d.appliedMu.Lock()
		ch := d.appliedCh
		d.appliedMu.Unlock()
		d.stateMu.Lock()
		done := d.appliedIndex >= i
		d.stateMu.Unlock()
		if done {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-d.stop:
			if err := d.err(); err != nil {
				return err
			}
			return ErrStopped
		}
	}
}

// compact takes a snapshot at the applied index: data.db is checkpointed,
// so it holds that index durably, and the log up to KeepEntries behind it
// goes. Runs in the applier.
func (d *DB) compact() {
	var busy, logFrames, done int
	if err := d.wdb.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err != nil || busy != 0 {
		d.log.Debug("checkpoint before a snapshot did not complete; trying later", "err", err)
		return
	}
	meta, err := d.snapshotMeta()
	if err != nil {
		d.log.Warn("snapshot", "err", err)
		d.sinceSnap = 0
		return
	}
	applied := meta.GetIndex()
	keepFrom := uint64(1)
	if applied > d.cfg.KeepEntries {
		keepFrom = applied - d.cfg.KeepEntries
	}
	if err := d.logs.compact(meta, keepFrom); err != nil {
		d.log.Warn("compacting the raft log", "err", err)
		return
	}
	if first, _ := d.ms.FirstIndex(); keepFrom > first {
		if err := d.ms.Compact(keepFrom); err != nil && !errors.Is(err, raft.ErrCompacted) {
			d.log.Warn("compacting the raft log in memory", "err", err)
		}
	}
	d.snapIndex.Store(applied)
	d.sinceSnap = 0
	d.log.Debug("raft snapshot", "index", applied, "kept_from", keepFrom)
}

// storage is the MemoryStorage Raft reads, except that the snapshot it
// asks for, to send a member that is behind, is made at that moment at
// the applied index, with the configuration of that index. The data comes
// from data.db when the member fetches it, so any applied index will do,
// and a stored snapshot from before a member was added would not name it.
type storage struct {
	*raft.MemoryStorage
	d *DB
}

func (s storage) Snapshot() (*pb.Snapshot, error) {
	meta, err := s.d.snapshotMeta()
	if err != nil {
		return s.MemoryStorage.Snapshot()
	}
	snap, err := s.MemoryStorage.CreateSnapshot(meta.GetIndex(), meta.GetConfState(), nil)
	if errors.Is(err, raft.ErrSnapOutOfDate) {
		return s.MemoryStorage.Snapshot()
	}
	return snap, err
}

// snapshotMeta describes a snapshot at the applied index.
func (d *DB) snapshotMeta() (*pb.SnapshotMetadata, error) {
	d.stateMu.Lock()
	idx, cs := d.appliedIndex, proto.Clone(d.appliedConf).(*pb.ConfState)
	d.stateMu.Unlock()
	term, err := d.ms.Term(idx)
	if err != nil {
		return nil, err
	}
	return pb.EnsureSnapshotMetadata(&pb.SnapshotMetadata{Index: &idx, Term: &term, ConfState: cs}), nil
}

// ---- db.DB ----

// Execute implements db.DB. See the package comment on retries.
func (d *DB) Execute(ctx context.Context, stmts ...db.Statement) ([]db.Result, error) {
	res, err := d.execute(ctx, stmts)
	count(err)
	return res, err
}

// ExecuteIdempotent implements db.DB. It is Execute: every proposal is
// deduplicated.
func (d *DB) ExecuteIdempotent(ctx context.Context, stmts ...db.Statement) ([]db.Result, error) {
	return d.Execute(ctx, stmts...)
}

// count records the outcome of a call in /metrics.
func count(err error) {
	var e *db.Error
	switch {
	case err == nil:
		metrics.DBRequests.Inc("ok")
	case errors.As(err, &e):
		metrics.DBRequests.Inc("error")
	default:
		metrics.DBRequests.Inc("failed")
	}
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (d *DB) execute(ctx context.Context, stmts []db.Statement) ([]db.Result, error) {
	if len(stmts) == 0 {
		return nil, nil
	}
	if err := d.err(); err != nil {
		return nil, err
	}
	norm, err := db.Normalize(stmts)
	if err != nil {
		return nil, err
	}
	p := proposal{ID: newID(), Stmts: norm}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("raftdb: %w", err)
	}
	ch := make(chan outcome, 1)
	d.waitMu.Lock()
	d.waiters[p.ID] = ch
	d.waitMu.Unlock()
	defer func() {
		d.waitMu.Lock()
		delete(d.waiters, p.ID)
		d.waitMu.Unlock()
	}()

	deadline := time.Now().Add(d.cfg.Wait)
	backoff := 50 * time.Millisecond
	proposed := false
	var lastErr error
	for {
		var err error
		if d.lead.Load() == raft.None {
			// Raft would hold the proposal until a leader is known; it
			// is not sent until then.
			err = raft.ErrProposalDropped
		} else {
			pctx, cancel := context.WithTimeout(ctx, time.Second)
			err = d.node.Propose(pctx, data)
			cancel()
		}
		var wait time.Duration
		if err == nil || !errors.Is(err, raft.ErrProposalDropped) && !errors.Is(err, raft.ErrStopped) {
			// Taken, or given up on while it may have been taken.
			proposed = true
		}
		if err == nil {
			// Long enough for an answer in any healthy cluster; a
			// proposal lost with a leader change is sent again.
			wait = 3 * time.Second
		} else {
			lastErr = err
			wait = backoff
			if backoff *= 2; backoff > time.Second {
				backoff = time.Second
			}
		}
		if left := time.Until(deadline); left < wait {
			wait = max(left, 0)
		}
		t := time.NewTimer(wait)
		select {
		case out := <-ch:
			t.Stop()
			if out.Err != nil {
				return nil, out.Err
			}
			return out.Results, nil
		case <-ctx.Done():
			t.Stop()
			if proposed {
				return nil, fmt.Errorf("raftdb: %w: %w", db.ErrUnknownOutcome, ctx.Err())
			}
			return nil, fmt.Errorf("raftdb: %w", ctx.Err())
		case <-d.stop:
			t.Stop()
			if err := d.err(); err != nil && !errors.Is(err, ErrStopped) {
				return nil, err
			}
			if proposed {
				return nil, fmt.Errorf("raftdb: %w: %w", db.ErrUnknownOutcome, ErrStopped)
			}
			return nil, ErrStopped
		case <-t.C:
		}
		if !time.Now().Before(deadline) {
			if proposed {
				return nil, fmt.Errorf("raftdb: %w: no answer within %s", db.ErrUnknownOutcome, d.cfg.Wait)
			}
			return nil, fmt.Errorf("raftdb: no leader within %s: %w", d.cfg.Wait, lastErr)
		}
	}
}

// Query implements db.DB.
func (d *DB) Query(ctx context.Context, level db.Level, stmts ...db.Statement) ([]db.Result, error) {
	res, err := d.query(ctx, level, stmts)
	count(err)
	return res, err
}

func (d *DB) query(ctx context.Context, level db.Level, stmts []db.Statement) ([]db.Result, error) {
	if len(stmts) == 0 {
		return nil, nil
	}
	if err := d.err(); err != nil {
		return nil, err
	}
	if level == db.None {
		if lost := d.leaderLost.Load(); lost != 0 && time.Since(time.Unix(0, lost)) > 5*time.Second {
			return nil, db.ErrStale
		}
	} else if err := d.Barrier(ctx); err != nil {
		return nil, err
	}
	norm, err := db.Normalize(stmts)
	if err != nil {
		return nil, err
	}
	return d.readLocal(ctx, norm)
}

// readLocal runs read-only statements on this node's copy, in one read
// transaction.
func (d *DB) readLocal(ctx context.Context, stmts []db.Statement) ([]db.Result, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	tx, err := d.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("raftdb: %w", err)
	}
	defer tx.Rollback()
	out := make([]db.Result, 0, len(stmts))
	for i, st := range stmts {
		r, err := run(ctx, tx, st, nil)
		if err != nil {
			if e, ok := statementError(i, err); ok {
				return nil, e
			}
			return nil, fmt.Errorf("raftdb: %w", err)
		}
		out = append(out, r)
	}
	return out, nil
}

// ---- linearizable reads ----

type notifier struct {
	done chan struct{}
	err  error
}

func newNotifier() *notifier { return &notifier{done: make(chan struct{})} }

func (n *notifier) notify(err error) {
	n.err = err
	close(n.done)
}

// Barrier returns once this node has applied everything that was
// committed when it was called: what follows reads at least that.
func (d *DB) Barrier(ctx context.Context) error {
	d.readMu.Lock()
	n := d.readNext
	d.readMu.Unlock()
	select {
	case d.readTrigger <- struct{}{}:
	default:
	}
	t := time.NewTimer(d.cfg.Wait)
	defer t.Stop()
	select {
	case <-n.done:
		return n.err
	case <-ctx.Done():
		return fmt.Errorf("raftdb: %w", ctx.Err())
	case <-t.C:
		return fmt.Errorf("raftdb: no leader confirmed a read within %s", d.cfg.Wait)
	case <-d.stop:
		if err := d.err(); err != nil {
			return err
		}
		return ErrStopped
	}
}

// readLoop serves Barrier: one ReadIndex round for all who wait.
func (d *DB) readLoop() {
	defer d.wg.Done()
	for {
		select {
		case <-d.stop:
			return
		case <-d.readTrigger:
		}
		d.readMu.Lock()
		n := d.readNext
		d.readNext = newNotifier()
		d.readMu.Unlock()
		idx, err := d.readIndex()
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Wait)
			err = d.waitApplied(ctx, idx)
			cancel()
		}
		n.notify(err)
	}
}

func (d *DB) readIndex() (uint64, error) {
	deadline := time.Now().Add(d.cfg.Wait)
	for time.Now().Before(deadline) {
		var rctx [8]byte
		rand.Read(rctx[:])
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := d.node.ReadIndex(ctx, rctx[:])
		cancel()
		if err != nil {
			select {
			case <-d.stop:
				return 0, ErrStopped
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		// No leader drops the request silently: ask again after a while.
		t := time.NewTimer(time.Second)
	wait:
		for {
			select {
			case rs := <-d.readStates:
				if string(rs.RequestCtx) == string(rctx[:]) {
					t.Stop()
					return rs.Index, nil
				}
			case <-t.C:
				break wait
			case <-d.stop:
				t.Stop()
				return 0, ErrStopped
			}
		}
	}
	return 0, fmt.Errorf("raftdb: no leader confirmed a read within %s", d.cfg.Wait)
}

// ---- members ----

// Members returns the members as this node has applied them, by Raft id.
// Before a joining node has applied its own admission, the members it was
// given by "robinmail node join".
func (d *DB) Members() []Member {
	d.membersMu.RLock()
	out := make([]Member, 0, len(d.members)+len(d.seeds))
	for _, m := range d.members {
		out = append(out, m)
	}
	_, self := d.members[d.id]
	if !self {
		for _, s := range d.seeds {
			if _, ok := d.members[s.ID]; !ok {
				out = append(out, s)
			}
		}
	}
	d.membersMu.RUnlock()
	slices.SortFunc(out, func(a, b Member) int {
		if a.Node != b.Node {
			if a.Node < b.Node {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}

func (d *DB) member(id uint64) (Member, bool) {
	d.membersMu.RLock()
	defer d.membersMu.RUnlock()
	if m, ok := d.members[id]; ok {
		return m, true
	}
	for _, s := range d.seeds {
		if s.ID == id {
			return s, true
		}
	}
	return Member{}, false
}

func (d *DB) voters() []uint64 {
	st := d.node.Status()
	var out []uint64
	for id := range st.Config.Voters.IDs() {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Self is this node as a member.
func (d *DB) Self() Member { return d.cfg.Self }

// AddMember admits a node, or updates the address of a member. It
// returns once this node has applied the change.
func (d *DB) AddMember(ctx context.Context, m Member) error {
	if m.Cert == "" || m.Address == "" {
		return errors.New("raftdb: a member needs an address and a certificate")
	}
	if m.ID == 0 {
		m.ID = RaftID(m.Cert)
	}
	if d.cfg.Transport == nil && m.ID != d.id {
		return errors.New("raftdb: this node has no node section, so no other node can reach it")
	}
	typ := pb.ConfChangeAddNode
	if _, ok := d.member(m.ID); ok {
		typ = pb.ConfChangeUpdateNode
	}
	data, _ := json.Marshal(m)
	return d.confChange(ctx, &pb.ConfChange{Type: &typ, NodeId: &m.ID, Context: data}, func() bool {
		d.membersMu.RLock()
		defer d.membersMu.RUnlock()
		return d.members[m.ID] == m
	})
}

// RemoveMember takes a node out of the cluster.
func (d *DB) RemoveMember(ctx context.Context, id uint64) error {
	if _, ok := d.member(id); !ok {
		return fmt.Errorf("raftdb: %x is not a member", id)
	}
	if len(d.voters()) == 1 && d.voters()[0] == id {
		return errors.New("raftdb: the last member cannot be removed")
	}
	typ := pb.ConfChangeRemoveNode
	return d.confChange(ctx, &pb.ConfChange{Type: &typ, NodeId: &id}, func() bool {
		d.membersMu.RLock()
		defer d.membersMu.RUnlock()
		_, ok := d.members[id]
		return !ok
	})
}

// confChange proposes a configuration change until done says it is in
// force here. Raft drops a change while another is pending; it is
// proposed again.
func (d *DB) confChange(ctx context.Context, cc *pb.ConfChange, done func() bool) error {
	deadline := time.Now().Add(d.cfg.Wait)
	for {
		if done() {
			return nil
		}
		if err := d.err(); err != nil {
			return err
		}
		pctx, cancel := context.WithTimeout(ctx, time.Second)
		err := d.node.ProposeConfChange(pctx, cc)
		cancel()
		until := time.Now().Add(3 * time.Second)
		if err != nil {
			until = time.Now().Add(200 * time.Millisecond)
		}
		for time.Now().Before(until) && !done() {
			d.appliedMu.Lock()
			ch := d.appliedCh
			d.appliedMu.Unlock()
			t := time.NewTimer(time.Until(until))
			select {
			case <-ch:
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-d.stop:
				t.Stop()
				return ErrStopped
			}
			t.Stop()
		}
		if done() {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = errors.New("not applied")
			}
			return fmt.Errorf("raftdb: the membership change did not take effect within %s: %w", d.cfg.Wait, err)
		}
	}
}

// Join prepares a new, empty node to join a cluster: it records the
// members the cluster has (from a member, over the mutual TLS), so that
// the node accepts their messages before it has applied its own
// admission. Call it before Open, after "robinmail node add" on a member.
func Join(dir string, self uint64, members []Member) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	logs, err := openLog(filepath.Join(dir, "raft.db"))
	if err != nil {
		return err
	}
	defer logs.close()
	empty, err := logs.empty()
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("raftdb: %s already holds a cluster's log; a node joins with an empty one (move the directory away first)", dir)
	}
	found := false
	for _, m := range members {
		if m.ID == self {
			found = true
		}
	}
	if !found {
		return errors.New("raftdb: the cluster does not list this node: run \"robinmail node add\" on a member first")
	}
	if err := logs.setJSON("id", struct {
		ID uint64 `json:"id,string"`
	}{self}); err != nil {
		return err
	}
	return logs.setJSON("seeds", members)
}

// ---- status ----

// Status is what "robinmail status" shows of the database.
type Status struct {
	ID       uint64
	Leader   uint64
	Term     uint64
	Commit   uint64
	Applied  uint64
	Snapshot uint64
	State    string
	Members  []Member
	// Match is, on the leader, the index each member is known to hold.
	Match map[uint64]uint64
}

// Status returns this node's view.
func (d *DB) Status() Status {
	st := d.node.Status()
	s := Status{ID: d.id, Leader: st.Lead, Term: st.GetTerm(), Commit: st.GetCommit(), Applied: d.applied.Load(),
		Snapshot: d.snapIndex.Load(), State: st.RaftState.String(), Members: d.Members()}
	if st.Progress != nil {
		s.Match = map[uint64]uint64{}
		for id, pr := range st.Progress {
			s.Match[id] = pr.Match
		}
	}
	return s
}

// TransferLeadership asks the leader to hand over to another member and
// waits up to Wait for it.
func (d *DB) TransferLeadership(ctx context.Context, to uint64) error {
	st := d.node.Status()
	d.node.TransferLeadership(ctx, st.Lead, to)
	deadline := time.Now().Add(d.cfg.Wait)
	for time.Now().Before(deadline) {
		if d.lead.Load() == to {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("raftdb: leadership did not move")
}

// ---- backup, snapshot, load ----

type backuper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

// copyTo writes a consistent copy of data.db to path with SQLite's online
// backup: the pages as they are, so rowids stay what they were.
func (d *DB) copyTo(ctx context.Context, path string) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	conn, err := d.rdb.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		b, ok := dc.(backuper)
		if !ok {
			return errors.New("the SQLite driver has no online backup")
		}
		bk, err := b.NewBackup(path)
		if err != nil {
			return err
		}
		if _, err := bk.Step(-1); err != nil {
			bk.Finish()
			return err
		}
		return bk.Finish()
	})
}

// writeCopy streams a copy of data.db to w.
func (d *DB) writeCopy(ctx context.Context, w io.Writer) error {
	f, err := os.CreateTemp(d.cfg.Dir, "copy-*.db")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	defer os.Remove(name)
	if err := d.copyTo(ctx, name); err != nil {
		return fmt.Errorf("raftdb: copy of the database: %w", err)
	}
	src, err := os.Open(name)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(w, src)
	return err
}

// Backup implements db.DB: a copy of this node's data.db once it holds
// everything committed when Backup was called.
func (d *DB) Backup(ctx context.Context, w io.Writer) error {
	if err := d.Barrier(ctx); err != nil {
		return err
	}
	return d.writeCopy(ctx, w)
}

// installFile makes the SQLite file at name this node's data.db, if it is
// of a later index than data.db now. It returns that index.
func (d *DB) installFile(name string, prepare func(*sql.DB) error) (uint64, error) {
	f, err := openSQLite(name, "NORMAL", false, 1)
	if err != nil {
		return 0, err
	}
	var check string
	if err := f.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil || check != "ok" {
		f.Close()
		return 0, fmt.Errorf("the database file is damaged: %v %s", err, check)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// Under the lock the applier stands still: what prepare reads of this
	// node stays true until the swap.
	if prepare != nil {
		if err := prepare(f); err != nil {
			f.Close()
			return 0, err
		}
	}
	var idx int64
	if err := f.QueryRow(`SELECT idx FROM raft_applied`).Scan(&idx); err != nil {
		f.Close()
		return 0, fmt.Errorf("the database file has no raft_applied: %w", err)
	}
	if _, err := f.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if uint64(idx) <= d.applied.Load() && prepare == nil {
		return uint64(idx), nil
	}
	d.wdb.Close()
	d.rdb.Close()
	d.wdb, d.rdb = nil, nil
	for _, ext := range []string{"-wal", "-shm"} {
		os.Remove(d.path + ext)
		os.Remove(name + ext)
	}
	if err := os.Rename(name, d.path); err != nil {
		return 0, err
	}
	if err := syncDir(d.cfg.Dir); err != nil {
		return 0, err
	}
	if err := d.openData(); err != nil {
		return 0, err
	}
	d.setApplied(uint64(idx))
	return uint64(idx), nil
}

// installSnapshot fetches the data.db of a member, for a Raft snapshot at
// index, and installs it. Called by the transport before the snapshot
// message reaches Raft.
func (d *DB) installSnapshot(ctx context.Context, from uint64, index uint64) error {
	if d.applied.Load() >= index {
		return nil
	}
	m, ok := d.member(from)
	if !ok {
		return fmt.Errorf("raftdb: snapshot from %x, which is not a member", from)
	}
	if d.cfg.Transport == nil {
		return errors.New("raftdb: no transport")
	}
	body, err := d.cfg.Transport.Do(ctx, m, "GET", pathSnapshot, nil)
	if err != nil {
		return fmt.Errorf("raftdb: fetching a snapshot from %s: %w", m.Node, err)
	}
	defer body.Close()
	f, err := os.CreateTemp(d.cfg.Dir, "incoming-*.db")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	n, err := io.Copy(f, body)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("raftdb: fetching a snapshot from %s: %w", m.Node, err)
	}
	idx, err := d.installFile(name, nil)
	if err != nil {
		return fmt.Errorf("raftdb: installing a snapshot from %s: %w", m.Node, err)
	}
	if idx < index {
		return fmt.Errorf("raftdb: the snapshot from %s is at %d, Raft asked for %d", m.Node, idx, index)
	}
	d.log.Info("installed a snapshot", "from", m.Node, "index", idx, "bytes", n)
	return nil
}

// Load implements db.DB: it replaces the database by the SQLite file in
// r. Only a cluster of one member can load: the log up to now is dropped,
// so a member added later gets the loaded database as a snapshot.
func (d *DB) Load(ctx context.Context, r io.ReadSeeker) error {
	if v := d.voters(); len(v) != 1 || v[0] != d.id || len(d.Members()) != 1 {
		return errors.New("raftdb: a database is loaded into a cluster of one node; add the other nodes afterwards")
	}
	if err := d.Barrier(ctx); err != nil {
		return err
	}
	f, err := os.CreateTemp(d.cfg.Dir, "load-*.db")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("raftdb: %w", err)
	}
	self := d.cfg.Self
	if m, ok := d.member(d.id); ok {
		self = m
	}
	// The applier must not move between reading the index and the swap.
	// Proposals are stopped by the fact that this node is alone and
	// waits here; an entry applied meanwhile is in the index read below.
	var at uint64
	_, err = d.installFile(name, func(f *sql.DB) error {
		if _, err := f.Exec(`DROP TABLE IF EXISTS raft_applied; DROP TABLE IF EXISTS raft_member;
			DROP TABLE IF EXISTS raft_request;` + dataSchema); err != nil {
			return err
		}
		if _, err := f.Exec(`INSERT INTO raft_member (id, node, address, cert) VALUES (?, ?, ?, ?)`,
			int64(self.ID), self.Node, self.Address, self.Cert); err != nil {
			return err
		}
		at = d.applied.Load()
		_, err := f.Exec(`UPDATE raft_applied SET idx = ?`, int64(at))
		return err
	})
	if err != nil {
		return fmt.Errorf("raftdb: load: %w", err)
	}
	// Drop the log up to here: replaying it on another node would not
	// give the loaded database.
	snap, err := d.ms.CreateSnapshot(at, d.confState, nil)
	if err != nil && !errors.Is(err, raft.ErrSnapOutOfDate) {
		return fmt.Errorf("raftdb: load: %w", err)
	}
	if err == nil {
		if err := d.logs.compact(snap.GetMetadata(), at+1); err != nil {
			return fmt.Errorf("raftdb: load: %w", err)
		}
		if err := d.ms.Compact(at); err != nil && !errors.Is(err, raft.ErrCompacted) {
			return fmt.Errorf("raftdb: load: %w", err)
		}
		d.snapIndex.Store(at)
	}
	return nil
}

// ---- logging ----

// raftLogger hands Raft's log lines to slog: debug and info as debug,
// since Raft is talkative.
type raftLogger struct{ l *slog.Logger }

func (r raftLogger) Debug(v ...any)              { r.l.Debug(fmt.Sprint(v...)) }
func (r raftLogger) Debugf(f string, v ...any)   { r.l.Debug(fmt.Sprintf(f, v...)) }
func (r raftLogger) Info(v ...any)               { r.l.Debug(fmt.Sprint(v...)) }
func (r raftLogger) Infof(f string, v ...any)    { r.l.Debug(fmt.Sprintf(f, v...)) }
func (r raftLogger) Warning(v ...any)            { r.l.Warn(fmt.Sprint(v...)) }
func (r raftLogger) Warningf(f string, v ...any) { r.l.Warn(fmt.Sprintf(f, v...)) }
func (r raftLogger) Error(v ...any)              { r.l.Error(fmt.Sprint(v...)) }
func (r raftLogger) Errorf(f string, v ...any)   { r.l.Error(fmt.Sprintf(f, v...)) }
func (r raftLogger) Fatal(v ...any)              { r.l.Error(fmt.Sprint(v...)); panic(fmt.Sprint(v...)) }
func (r raftLogger) Fatalf(f string, v ...any)   { r.Fatal(fmt.Sprintf(f, v...)) }
func (r raftLogger) Panic(v ...any)              { panic(fmt.Sprint(v...)) }
func (r raftLogger) Panicf(f string, v ...any)   { panic(fmt.Sprintf(f, v...)) }

var _ db.DB = (*DB)(nil)
