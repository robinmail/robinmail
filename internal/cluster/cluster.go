// Package cluster is how the robinmail nodes of one cluster find and talk
// to each other.
//
// # Membership
//
// The members are those of the database's Raft cluster (internal/raftdb):
// each with its node id, address and the fingerprint of its certificate,
// admitted with "robinmail node add" on a member and taken out with "node
// remove". A node also writes a row into the node table when it starts and
// renews seen_at every few seconds while it runs, for people to see; who
// may connect is decided by the membership alone.
//
// # Authentication: mutual TLS, pinned through the membership
//
// Each node makes its own key and self-signed certificate once and keeps
// them in its data directory. There is no CA and no shared secret. Calls
// between nodes use TLS 1.3 with a certificate on both sides, and each
// side accepts exactly the certificates of members: the server refuses a
// client that shows none or an unknown one, the client refuses a server
// that does not show the certificate the member it meant to call joined
// with. So who may join is decided by whoever runs "robinmail node add" on
// a member, which needs a shell there. An unknown certificate makes a node
// read the membership again (at most once a second) before it refuses, so
// a node that was just admitted is let in.
//
// # Protocol
//
// HTTP over that TLS, on the node's internal address:
//
//	PUT /v1/blob/<account-id>/<sha256>   the body is the encrypted blob file
//	    204  verified and durably stored (an existing file is replaced)
//	    400  not a blob name
//	    413  larger than the receiver takes
//	    422  the file is not that blob: it does not decrypt, or its
//	         plaintext has another hash
//	GET /v1/blob/<account-id>/<sha256>
//	    200  the encrypted file, application/octet-stream
//	    404  this node does not have it
//	GET /v1/ping
//	    204
//	/v1/raft/...
//	    the database's Raft messages, snapshots and member list, served
//	    by internal/raftdb (Node.Raft)
//
// Everything else is 404 or 405. The files are the ones internal/blob
// writes; nothing is re-encrypted for the wire.
package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/robinmail/robinmail/internal/blob"
	"github.com/robinmail/robinmail/internal/raftdb"
	"github.com/robinmail/robinmail/internal/store"
)

// Directory is the node table. *store.Store has it.
type Directory interface {
	Nodes(ctx context.Context) ([]store.Node, error)
	RegisterNode(ctx context.Context, n store.Node, running bool) error
	Heartbeat(ctx context.Context, id, cert string) (bool, error)
}

// validID is what a node id looks like: it ends up in logs and tables.
var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidID reports whether s can be a node id.
func ValidID(s string) bool { return validID.MatchString(s) }

// MaxBlobBytes is the largest file a node takes from a peer by default.
const MaxBlobBytes = 1 << 30

// Node is this node's view of the cluster. Fill the exported fields, then
// call Register or Run.
type Node struct {
	// ID and Address are this node's row in the node table.
	ID       string
	Address  string
	Identity *Identity
	Dir      Directory
	Logger   *slog.Logger
	// Heartbeat is how often Run renews the row. [5s]
	Heartbeat time.Duration
	// MaxBlobBytes bounds a pushed file. [MaxBlobBytes]
	MaxBlobBytes int64
	// Raft, when set, serves /v1/raft/ for the other members: the
	// database's Raft messages and snapshots (internal/raftdb).
	Raft http.Handler

	mu      sync.Mutex
	nodes   []store.Node
	readAt  time.Time
	clients map[string]*http.Client // by peer certificate
	long    map[string]*http.Client // the same, without a limit on the wait for an answer
}

// How old the cached node table may be: for ordinary use, and when a
// certificate nobody knows shows up.
const (
	listTTL    = 5 * time.Second
	listMinAge = time.Second
)

func (n *Node) log() *slog.Logger {
	if n.Logger != nil {
		return n.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// list returns the node table, read again when the copy is older than
// maxAge. When the membership cannot be read, an older copy is used: the nodes
// that were members a moment ago still are.
func (n *Node) list(ctx context.Context, maxAge time.Duration) ([]store.Node, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.nodes != nil && time.Since(n.readAt) < maxAge {
		return n.nodes, nil
	}
	nodes, err := n.Dir.Nodes(ctx)
	if err != nil {
		if n.nodes != nil {
			n.log().Warn("reading the node table failed, using the last known one", "err", err)
			n.readAt = time.Now() // and not asking again for every call
			return n.nodes, nil
		}
		return nil, err
	}
	if nodes == nil {
		nodes = []store.Node{}
	}
	n.nodes, n.readAt = nodes, time.Now()
	return nodes, nil
}

// knows reports whether the fingerprint is that of a registered node, and
// of which.
func (n *Node) knows(ctx context.Context, fingerprint string) (string, bool) {
	for _, age := range []time.Duration{listTTL, listMinAge} {
		nodes, err := n.list(ctx, age)
		if err != nil {
			return "", false
		}
		for _, x := range nodes {
			if x.Cert == fingerprint {
				return x.ID, true
			}
		}
	}
	return "", false
}

// Register writes this node's row. running is true for the server, false
// for a command that only needs the others to accept its calls; such a
// command writes only if the row is not right already.
func (n *Node) Register(ctx context.Context, running bool) error {
	if !ValidID(n.ID) {
		return fmt.Errorf("cluster: invalid node id %q", n.ID)
	}
	me := store.Node{ID: n.ID, Address: n.Address, Cert: n.Identity.Fingerprint}
	if !running {
		nodes, err := n.list(ctx, 0)
		if err != nil {
			return err
		}
		for _, x := range nodes {
			if x.ID == me.ID && x.Address == me.Address && x.Cert == me.Cert {
				return nil
			}
		}
	}
	if err := n.Dir.RegisterNode(ctx, me, running); err != nil {
		return err
	}
	n.mu.Lock()
	n.nodes = nil
	n.mu.Unlock()
	return nil
}

// Run renews this node's row until ctx is done. If the row is gone or
// someone else's, it says so and registers again.
func (n *Node) Run(ctx context.Context) {
	every := n.Heartbeat
	if every == 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		bctx, cancel := context.WithTimeout(ctx, every)
		ok, err := n.Dir.Heartbeat(bctx, n.ID, n.Identity.Fingerprint)
		switch {
		case err != nil:
			n.log().Warn("heartbeat failed", "err", err)
		case !ok:
			n.log().Error("this node's row is gone or has another certificate: was it removed, or does another node use the same id? registering again", "node", n.ID)
			if err := n.Register(bctx, true); err != nil {
				n.log().Warn("registering failed", "err", err)
			}
		}
		cancel()
	}
}

// Members returns how many nodes are registered, this one included.
func (n *Node) Members(ctx context.Context) (int, error) {
	nodes, err := n.list(ctx, listTTL)
	return len(nodes), err
}

// Peers returns the other registered nodes. It implements blob.Peers.
func (n *Node) Peers(ctx context.Context) ([]blob.Peer, error) {
	nodes, err := n.list(ctx, listTTL)
	if err != nil {
		return nil, err
	}
	out := make([]blob.Peer, 0, len(nodes))
	for _, x := range nodes {
		if x.ID != n.ID {
			out = append(out, &peer{node: n, id: x.ID, address: x.Address, cert: x.Cert})
		}
	}
	return out, nil
}

// ServerTLS is the TLS config of the listener for the other nodes: TLS 1.3,
// this node's certificate, and a client certificate required and looked up
// in the node table.
func (n *Node) ServerTLS() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{n.Identity.Certificate},
		// Any certificate gets through the handshake's own checks: there
		// is no CA to check against. VerifyConnection decides.
		ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("cluster: no client certificate")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, ok := n.knows(ctx, Fingerprint(cs.PeerCertificates[0].Raw)); !ok {
				return errors.New("cluster: client certificate is not in the node table")
			}
			return nil
		},
	}
}

// clientTLS accepts one server certificate: the one with this fingerprint.
func (n *Node) clientTLS(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{n.Identity.Certificate},
		// No CA and no name to verify: the certificate itself is pinned.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || Fingerprint(cs.PeerCertificates[0].Raw) != fingerprint {
				return errors.New("cluster: the server is not the node in the node table")
			}
			return nil
		},
	}
}

// client returns the HTTP client for the peer with that certificate. A
// peer that registers a new certificate gets a new client.
func (n *Node) client(fingerprint string) *http.Client {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.clients[fingerprint]; ok {
		return c
	}
	if n.clients == nil || len(n.clients) > 64 {
		n.clients = map[string]*http.Client{}
	}
	c := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
		TLSClientConfig:       n.clientTLS(fingerprint),
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}}
	n.clients[fingerprint] = c
	return c
}

// longClient is client without the 60 seconds for an answer: a Raft
// snapshot is answered once it is copied or installed, which takes as
// long as the database is large. The caller's context bounds it.
func (n *Node) longClient(fingerprint string) *http.Client {
	base := n.client(fingerprint)
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.long[fingerprint]; ok {
		return c
	}
	if n.long == nil || len(n.long) > 64 {
		n.long = map[string]*http.Client{}
	}
	t := base.Transport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 0
	c := &http.Client{Transport: t}
	n.long[fingerprint] = c
	return c
}

// Do implements raftdb.Transport: a request to a member over the mutual
// TLS, pinned to the certificate the member joined with.
func (n *Node) Do(ctx context.Context, to raftdb.Member, method, path string, body io.Reader) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, method, "https://"+to.Address+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	c := n.client(to.Cert)
	if strings.HasSuffix(path, "/snapshot") || method == http.MethodPost {
		c = n.longClient(to.Cert)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		resp.Body.Close()
		return nil, fmt.Errorf("node %s answered %d: %s", to.Node, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp.Body, nil
}

// peer is one other node. It implements blob.Peer.
type peer struct {
	node              *Node
	id, address, cert string
}

func (p *peer) ID() string { return p.id }

func (p *peer) url(key string) string { return "https://" + p.address + "/v1/blob/" + key }

func (p *peer) Push(ctx context.Context, key string, size int64, file io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.url(key), io.NopCloser(file))
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := p.node.client(p.cert).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("node %s answered %d: %s", p.id, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (p *peer) Fetch(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url(key), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.node.client(p.cert).Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, blob.ErrNotFound
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	resp.Body.Close()
	return nil, fmt.Errorf("node %s answered %d: %s", p.id, resp.StatusCode, strings.TrimSpace(string(msg)))
}

// Handler serves the protocol in the package comment from the local blob
// directory. verify decides about every pushed file.
func (n *Node) Handler(local *blob.Local, verify blob.Verifier) http.Handler {
	limit := n.MaxBlobBytes
	if limit <= 0 {
		limit = MaxBlobBytes
	}
	mux := http.NewServeMux()
	if n.Raft != nil {
		mux.Handle("/v1/raft/", n.Raft)
	}
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/blob/{account}/{sum}", func(w http.ResponseWriter, r *http.Request) {
		f, err := local.Get(r.Context(), blob.Key(r.PathValue("account"), r.PathValue("sum")))
		switch {
		case errors.Is(err, blob.ErrNotFound):
			http.Error(w, "no such blob here", http.StatusNotFound)
			return
		case err != nil:
			// An invalid key, most likely.
			http.Error(w, "not a blob name", http.StatusBadRequest)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		io.Copy(w, f)
	})
	mux.HandleFunc("PUT /v1/blob/{account}/{sum}", func(w http.ResponseWriter, r *http.Request) {
		key := blob.Key(r.PathValue("account"), r.PathValue("sum"))
		if _, _, err := blob.SplitKey(key); err != nil {
			http.Error(w, "not a blob name", http.StatusBadRequest)
			return
		}
		if r.ContentLength > limit {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		err := local.PutVerified(r.Context(), key, http.MaxBytesReader(w, r.Body, limit), verify)
		var tooLarge *http.MaxBytesError
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.As(err, &tooLarge):
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		case errors.Is(err, blob.ErrCorrupt), errors.Is(err, store.ErrNoAccount):
			n.log().Warn("refused a pushed blob", "blob", key, "peer", peerOf(r), "err", err)
			http.Error(w, "the file is not that blob", http.StatusUnprocessableEntity)
		default:
			n.log().Error("storing a pushed blob failed", "blob", key, "err", err)
			http.Error(w, "could not store the file", http.StatusInternalServerError)
		}
	})
	// The TLS layer has let only registered nodes in. Checking again here
	// costs a map lookup and keeps the handler safe if someone ever serves
	// it another way.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "nodes only", http.StatusForbidden)
			return
		}
		if _, ok := n.knows(r.Context(), Fingerprint(r.TLS.PeerCertificates[0].Raw)); !ok {
			http.Error(w, "nodes only", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func peerOf(r *http.Request) string {
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		return Fingerprint(r.TLS.PeerCertificates[0].Raw)[:12]
	}
	return r.RemoteAddr
}

// HTTPServer returns the server for the other nodes. Serve it with
// ServeTLS(listener, "", "").
func (n *Node) HTTPServer(local *blob.Local, verify blob.Verifier) *http.Server {
	return &http.Server{
		Handler:           n.Handler(local, verify),
		TLSConfig:         n.ServerTLS(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(n.log().Handler(), slog.LevelDebug),
	}
}
