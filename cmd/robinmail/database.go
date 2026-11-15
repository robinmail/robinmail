package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/cluster"
	"github.com/robinmail/robinmail/internal/config"
	"github.com/robinmail/robinmail/internal/db"
	"github.com/robinmail/robinmail/internal/raftdb"
	"github.com/robinmail/robinmail/internal/store"
)

// The database is internal/raftdb in <data_dir>/db. The server opens it
// and holds it. A command opens it itself when no server runs on this
// node, and otherwise asks the server, over the control listener: TLS on
// loopback, accepting only this node's own certificate, at the address
// the server wrote to <data_dir>/db/control.

// controlFile names the file with the control listener's address.
func controlFile(cfg *config.Config) string { return filepath.Join(cfg.DBDir(), "control") }

// openDatabase opens this node's database, or reaches it through the
// server that has it open. node is nil without a node section.
func openDatabase(ctx context.Context, cfg *config.Config, identity *cluster.Identity, node *cluster.Node, serving bool, log *slog.Logger) (raftdb.Admin, *raftdb.DB, error) {
	dir := cfg.DBDir()
	rc := raftdb.Config{
		Dir:    dir,
		Self:   raftdb.Member{Node: cfg.Node.ID, Address: cfg.Node.Address, Cert: identity.Fingerprint},
		Logger: log,
	}
	if node != nil {
		rc.Transport = node
	}
	for {
		if !serving && raftdb.Locked(dir) {
			if _, err := os.Stat(controlFile(cfg)); err == nil {
				remote, err := controlClient(cfg, identity)
				if err != nil {
					return nil, nil, err
				}
				return remote, nil, nil
			}
			// Another command has it, without a control listener: wait
			// for it to finish.
			select {
			case <-ctx.Done():
				return nil, nil, errors.New("another robinmail command has this node's database open")
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		d, err := raftdb.Open(rc)
		if errors.Is(err, raftdb.ErrLocked) {
			if serving {
				return nil, nil, errors.New("another robinmail process has this node's database open")
			}
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if !serving && len(d.Members()) > 1 {
			d.Close()
			return nil, nil, fmt.Errorf("this node is one of %d in a cluster, and its server is not running: start \"robinmail serve\" and run the command again", len(d.Members()))
		}
		return d, d, nil
	}
}

// controlClient reaches the server of this node.
func controlClient(cfg *config.Config, identity *cluster.Identity) (*raftdb.Remote, error) {
	data, err := os.ReadFile(controlFile(cfg))
	if err != nil {
		return nil, fmt.Errorf("the server of this node has the database open, but its control address is missing (%w): is it still starting?", err)
	}
	addr := strings.TrimSpace(string(data))
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{identity.Certificate},
		InsecureSkipVerify: true, // pinned below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || cluster.Fingerprint(cs.PeerCertificates[0].Raw) != identity.Fingerprint {
				return errors.New("the control listener does not show this node's certificate")
			}
			return nil
		},
	}
	return &raftdb.Remote{URL: "https://" + addr, Client: &http.Client{Transport: &http.Transport{
		TLSClientConfig:     tc,
		DialContext:         (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		ForceAttemptHTTP2:   true,
	}}}, nil
}

// serveControl runs the control listener for the commands of this node
// and writes its address. It is closed when the database is.
func serveControl(cfg *config.Config, identity *cluster.Identity, d *raftdb.DB, log *slog.Logger) (*http.Server, error) {
	tc := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{identity.Certificate},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || cluster.Fingerprint(cs.PeerCertificates[0].Raw) != identity.Fingerprint {
				return errors.New("control: only this node's own certificate")
			}
			return nil
		},
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		return nil, fmt.Errorf("control listener: %w", err)
	}
	tmp := controlFile(cfg) + ".tmp"
	if err := os.WriteFile(tmp, []byte(l.Addr().String()+"\n"), 0o600); err != nil {
		l.Close()
		return nil, err
	}
	if err := os.Rename(tmp, controlFile(cfg)); err != nil {
		l.Close()
		return nil, err
	}
	srv := &http.Server{Handler: d.ControlHandler(), ReadHeaderTimeout: 10 * time.Second,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug)}
	go srv.Serve(l)
	return srv, nil
}

// memberDirectory is the node table internal/cluster works with: the
// members of the database's Raft cluster, who alone may connect, with the
// heartbeats of the node table.
type memberDirectory struct {
	db    raftdb.Admin
	local *raftdb.DB
	store *store.Store
}

func (m memberDirectory) members(ctx context.Context) ([]raftdb.Member, error) {
	if m.local != nil {
		return m.local.Members(), nil
	}
	st, err := m.db.ClusterStatus(ctx)
	return st.Members, err
}

func (m memberDirectory) Nodes(ctx context.Context) ([]store.Node, error) {
	members, err := m.members(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.Node, 0, len(members))
	for _, x := range members {
		if x.Node == "" {
			continue
		}
		out = append(out, store.Node{ID: x.Node, Address: x.Address, Cert: x.Cert})
	}
	return out, nil
}

func (m memberDirectory) RegisterNode(ctx context.Context, n store.Node, running bool) error {
	return m.store.RegisterNode(ctx, n, running)
}

func (m memberDirectory) Heartbeat(ctx context.Context, id, cert string) (bool, error) {
	return m.store.Heartbeat(ctx, id, cert)
}

// ---- robinmail node ----

// nodeList prints the members: Raft id, node id, address, certificate,
// leader and how far each is, and the last heartbeat.
func (a *app) nodeList() error {
	st, err := a.db.ClusterStatus(a.ctx)
	if err != nil {
		return err
	}
	seen := map[string]time.Time{}
	if nodes, err := a.store.Nodes(a.ctx); err == nil {
		for _, n := range nodes {
			seen[n.ID] = n.SeenAt
		}
	}
	for _, m := range st.Members {
		role := "follower"
		if m.ID == st.Leader {
			role = "leader"
		}
		if m.ID == st.ID {
			role += ", this node"
		}
		at := "never ran"
		if t := seen[m.Node]; t.Unix() > 0 {
			at = "seen " + time.Since(t).Round(time.Second).String() + " ago"
		}
		if match, ok := st.Match[m.ID]; ok {
			role += fmt.Sprintf(", at %d of %d", match, st.Commit)
		}
		name := m.Node
		if name == "" {
			name = "(alone)"
		}
		fmt.Fprintf(a.stdout, "%-12s  %016x  %-28s  %s  %s; %s\n", name, m.ID, m.Address, m.Cert[:min(16, len(m.Cert))], role, at)
	}
	return nil
}

// nodeID prints what "robinmail node add" on a member needs to admit
// this node. It makes the node's key and certificate if there are none,
// and does not open the database.
func nodeID(cfg *config.Config, stdout io.Writer) error {
	if cfg.Node.ID == "" {
		return errors.New("this config has no node section: give it one (id, listen, address) first")
	}
	identity, err := cluster.LoadOrCreateIdentity(cfg.DataDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s %s %s\n", cfg.Node.ID, cfg.Node.Address, identity.Fingerprint)
	return nil
}

// nodeAdd admits a node to the cluster. Its certificate is what the
// members will accept from it; it joins with "robinmail node join".
func (a *app) nodeAdd(id, address, cert string) error {
	if !cluster.ValidID(id) {
		return fmt.Errorf("%q is not a node id", id)
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return fmt.Errorf("%q is not a host:port", address)
	}
	if len(cert) != 64 || strings.Trim(cert, "0123456789abcdef") != "" {
		return errors.New("the certificate is its SHA-256 in hex, as \"robinmail node id\" prints it on the new node")
	}
	if a.cfg.Node.ID == "" {
		return errors.New("this node has no node section, so no other node can reach it: give it one and restart it first")
	}
	m := raftdb.Member{Node: id, Address: address, Cert: cert}
	st, err := a.db.ClusterStatus(a.ctx)
	if err != nil {
		return err
	}
	for _, x := range st.Members {
		if x.Node == id && x.Cert != cert {
			return fmt.Errorf("%s is a member with another certificate: remove it first", id)
		}
	}
	if err := a.db.AddMember(a.ctx, m); err != nil {
		return err
	}
	self := st.Members[0]
	for _, x := range st.Members {
		if x.ID == st.ID {
			self = x
		}
	}
	fmt.Fprintf(a.stdout, "added %s (%016x)\non %s: robinmail node join %s %s\n", id, raftdb.RaftID(cert), id, self.Address, self.Cert)
	a.note("node add", "", id+" "+address)
	return nil
}

// nodeJoin prepares this node, which must have an empty database, to join
// the cluster of the member at address, whose certificate is given: it
// asks that member for the members and keeps them, so that the server,
// started next, accepts their messages. It runs without opening the
// database: opening an empty one starts a cluster of its own.
func nodeJoin(ctx context.Context, cfg *config.Config, stdout io.Writer, address, cert string) error {
	if cfg.Node.ID == "" {
		return errors.New("this config has no node section: give it one (id, listen, address) first")
	}
	if cert == "" {
		return errors.New("usage: robinmail node join <address> <certificate>")
	}
	identity, err := cluster.LoadOrCreateIdentity(cfg.DataDir)
	if err != nil {
		return err
	}
	node := &cluster.Node{ID: cfg.Node.ID, Address: cfg.Node.Address, Identity: identity}
	from := raftdb.Member{Node: "member", Address: address, Cert: cert}
	members, err := raftdb.FetchMembers(ctx, node, from)
	if err != nil {
		return fmt.Errorf("asking %s for the members: %w (was this node added there with \"robinmail node add\"?)", address, err)
	}
	if err := raftdb.Join(cfg.DBDir(), raftdb.RaftID(identity.Fingerprint), members); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ready to join %d members; start robinmail serve\n", len(members))
	return nil
}

// nodeRemove takes a node out of the cluster: out of Raft, so it no longer
// counts for a majority and its certificate is refused, and out of the
// node table. Stop the node first.
func (a *app) nodeRemove(arg string) error {
	st, err := a.db.ClusterStatus(a.ctx)
	if err != nil {
		return err
	}
	var target *raftdb.Member
	for i, m := range st.Members {
		if m.Node == arg || strconv.FormatUint(m.ID, 16) == strings.TrimLeft(arg, "0") {
			target = &st.Members[i]
		}
	}
	if target == nil {
		// A row of the node table without a member: from before phase 10.
		ok, err := a.store.RemoveNode(a.ctx, arg)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s: no such node", arg)
		}
		fmt.Fprintf(a.stdout, "removed %s\n", arg)
		return nil
	}
	if err := a.db.RemoveMember(a.ctx, target.ID); err != nil {
		return err
	}
	if target.Node != "" {
		if _, err := a.store.RemoveNode(a.ctx, target.Node); err != nil && target.ID != st.ID {
			return err
		}
	}
	fmt.Fprintf(a.stdout, "removed %s\n", arg)
	a.note("node remove", "", arg)
	return nil
}

// ---- robinmail db ----

// dbQuery runs one read-only statement, linearizable, and prints each row
// as a JSON array on a line of its own. A console for the operator on
// this node, not an API: it needs what opening the database needs.
func (a *app) dbQuery(sql string) error {
	res, err := a.db.Query(a.ctx, db.Weak, db.Stmt(sql))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(a.stdout)
	for _, row := range res[0].Values {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

// dbExec runs one statement as a write through the log, on every node,
// and prints how many rows it changed. For repairs by hand.
func (a *app) dbExec(sql string) error {
	res, err := a.db.Execute(a.ctx, db.Stmt(sql))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%d rows changed\n", res[0].RowsAffected)
	a.note("db exec", "", sql)
	return nil
}
