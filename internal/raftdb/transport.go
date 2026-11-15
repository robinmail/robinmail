package raftdb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
)

// The paths of Handler, under the node-to-node API of internal/cluster.
const (
	pathMessage  = "/v1/raft/message"
	pathSnapshot = "/v1/raft/snapshot"
	pathMembers  = "/v1/raft/members"
)

// maxMessages bounds the body of one POST of Raft messages.
const maxMessages = 512 << 20

// peer sends Raft messages to one member, in order, a batch per request.
type peer struct {
	m    Member
	q    chan *pb.Message
	stop chan struct{}
	once sync.Once
}

func (p *peer) close() { p.once.Do(func() { close(p.stop) }) }

// send hands Raft's messages to the peers. A full queue or an unknown
// member drops the message and tells Raft the peer is unreachable: Raft
// sends again what matters.
func (d *DB) send(msgs []*pb.Message) {
	for _, m := range msgs {
		to := m.GetTo()
		if to == d.id {
			continue
		}
		if m.GetType() == pb.MsgSnap {
			go d.sendSnapshot(m)
			continue
		}
		p := d.peer(to)
		if p == nil {
			d.node.ReportUnreachable(to)
			continue
		}
		select {
		case p.q <- m:
		default:
			d.node.ReportUnreachable(to)
		}
	}
}

// peer returns the sender for a member, made anew when its address or
// certificate changed.
func (d *DB) peer(id uint64) *peer {
	if d.cfg.Transport == nil {
		return nil
	}
	m, ok := d.member(id)
	if !ok {
		return nil
	}
	d.peersMu.Lock()
	defer d.peersMu.Unlock()
	if p, ok := d.peers[id]; ok {
		if p.m == m {
			return p
		}
		p.close()
	}
	select {
	case <-d.stop:
		return nil
	default:
	}
	p := &peer{m: m, q: make(chan *pb.Message, 4096), stop: make(chan struct{})}
	d.peers[id] = p
	go d.runPeer(p)
	return p
}

// dropPeersNotIn stops the senders of nodes that are no longer members.
func (d *DB) dropPeersNotIn(members map[uint64]Member) {
	d.peersMu.Lock()
	defer d.peersMu.Unlock()
	for id, p := range d.peers {
		if _, ok := members[id]; ok {
			continue
		}
		seed := false
		for _, s := range d.seeds {
			seed = seed || s.ID == id
		}
		if !seed {
			p.close()
			delete(d.peers, id)
		}
	}
}

func (d *DB) runPeer(p *peer) {
	var failing time.Time
	for {
		var batch []*pb.Message
		select {
		case m := <-p.q:
			batch = append(batch, m)
		case <-p.stop:
			return
		}
	more:
		for len(batch) < 1024 {
			select {
			case m := <-p.q:
				batch = append(batch, m)
			default:
				break more
			}
		}
		body, err := encodeMessages(batch)
		if err != nil {
			d.log.Error("encoding raft messages", "err", err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		rc, err := d.cfg.Transport.Do(ctx, p.m, http.MethodPost, pathMessage, bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, rc)
			rc.Close()
		}
		cancel()
		if err == nil {
			if !failing.IsZero() {
				d.log.Info("raft peer reachable again", "node", p.m.Node, "after", time.Since(failing).Round(time.Millisecond))
				failing = time.Time{}
			}
			continue
		}
		d.node.ReportUnreachable(p.m.ID)
		if failing.IsZero() {
			failing = time.Now()
			d.log.Warn("raft peer unreachable", "node", p.m.Node, "address", p.m.Address, "err", err)
		}
		// Do not spin on a dead peer; Raft sends again.
		select {
		case <-time.After(100 * time.Millisecond):
		case <-p.stop:
			return
		}
	}
}

// sendSnapshot sends a snapshot message on its own. The member answers
// only after it fetched and installed this node's database, so the
// answer is what Raft is told.
func (d *DB) sendSnapshot(m *pb.Message) {
	status := raft.SnapshotFailure
	defer func() { d.node.ReportSnapshot(m.GetTo(), status) }()
	to, ok := d.member(m.GetTo())
	if !ok || d.cfg.Transport == nil {
		return
	}
	body, err := encodeMessages([]*pb.Message{m})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	go func() {
		select {
		case <-d.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	d.log.Info("sending a snapshot", "to", to.Node, "index", m.GetSnapshot().GetMetadata().GetIndex())
	rc, err := d.cfg.Transport.Do(ctx, to, http.MethodPost, pathMessage, bytes.NewReader(body))
	if err != nil {
		d.log.Warn("sending a snapshot failed", "to", to.Node, "err", err)
		return
	}
	io.Copy(io.Discard, rc)
	rc.Close()
	status = raft.SnapshotFinish
}

// encodeMessages writes each message as its length (uvarint) and its
// protobuf encoding.
func encodeMessages(msgs []*pb.Message) ([]byte, error) {
	var buf bytes.Buffer
	var n [binary.MaxVarintLen64]byte
	for _, m := range msgs {
		data, err := proto.Marshal(m)
		if err != nil {
			return nil, err
		}
		buf.Write(n[:binary.PutUvarint(n[:], uint64(len(data)))])
		buf.Write(data)
	}
	return buf.Bytes(), nil
}

func decodeMessages(r io.Reader) ([]*pb.Message, error) {
	br := bufio.NewReader(r)
	var out []*pb.Message
	for {
		size, err := binary.ReadUvarint(br)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if size > maxMessages {
			return nil, errors.New("raft message too large")
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(br, data); err != nil {
			return nil, err
		}
		m := &pb.Message{}
		if err := proto.Unmarshal(data, m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
}

// Handler serves the Raft paths for the other members. internal/cluster
// mounts it behind its mutual TLS, which lets only members in:
//
//	POST /v1/raft/message    Raft messages (see encodeMessages); 204
//	GET  /v1/raft/snapshot   this node's data.db, a consistent copy
//	GET  /v1/raft/members    the members as this node knows them, JSON
func (d *DB) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+pathMessage, func(w http.ResponseWriter, r *http.Request) {
		msgs, err := decodeMessages(http.MaxBytesReader(w, r.Body, maxMessages))
		if err != nil {
			http.Error(w, "bad raft messages", http.StatusBadRequest)
			return
		}
		for _, m := range msgs {
			if m.GetTo() != d.id {
				continue
			}
			if m.GetType() == pb.MsgSnap {
				if err := d.installSnapshot(r.Context(), m.GetFrom(), m.GetSnapshot().GetMetadata().GetIndex()); err != nil {
					d.log.Warn("snapshot not installed", "err", err)
					http.Error(w, "snapshot not installed", http.StatusInternalServerError)
					return
				}
			}
			if err := d.node.Step(r.Context(), m); err != nil && !errors.Is(err, raft.ErrStepLocalMsg) {
				if errors.Is(err, raft.ErrStopped) {
					http.Error(w, "stopped", http.StatusServiceUnavailable)
					return
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET "+pathSnapshot, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.sqlite3")
		if err := d.writeCopy(r.Context(), w); err != nil {
			d.log.Warn("serving a snapshot", "err", err)
		}
	})
	mux.HandleFunc("GET "+pathMembers, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(d.Members())
	})
	return mux
}

// FetchMembers asks a member for the members it knows, for Join.
func FetchMembers(ctx context.Context, t Transport, from Member) ([]Member, error) {
	rc, err := t.Do(ctx, from, http.MethodGet, pathMembers, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var out []Member
	if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("the members from %s: %w", from.Address, err)
	}
	return out, nil
}
