// Package raftdbtest gives tests a database: one node in a temporary
// directory, or a cluster of several in this process, connected by a
// transport in memory.
package raftdbtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/raftdb"
)

// Open returns a database of one node, closed when the test ends.
func Open(t testing.TB) *raftdb.DB {
	t.Helper()
	d, err := raftdb.Open(raftdb.Config{Dir: t.TempDir(), Self: raftdb.Member{Cert: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// Net connects the nodes of a cluster in this process.
type Net struct {
	mu       sync.Mutex
	handlers map[string]http.Handler
}

type transport struct{ net *Net }

func (t transport) Do(ctx context.Context, to raftdb.Member, method, path string, body io.Reader) (io.ReadCloser, error) {
	t.net.mu.Lock()
	h := t.net.handlers[to.Address]
	t.net.mu.Unlock()
	if h == nil {
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

// waiting answers 503 until the node is open.
type waiting struct {
	mu sync.Mutex
	h  http.Handler
}

func (w *waiting) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	h := w.h
	w.mu.Unlock()
	if h == nil {
		http.Error(rw, "starting", http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(rw, r)
}

// Cluster returns n nodes of one cluster: the first starts it, the others
// are added and join the way "robinmail node add" and "node join" do.
// They are closed when the test ends.
func Cluster(t testing.TB, n int) []*raftdb.DB {
	t.Helper()
	nw := &Net{handlers: map[string]http.Handler{}}
	var out []*raftdb.DB
	t.Cleanup(func() {
		for _, d := range out {
			d.Close()
		}
	})
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("n%d", i)
		self := raftdb.Member{Node: name, Address: name, Cert: "cert-" + name}
		dir := t.TempDir()
		if i > 1 {
			self.ID = raftdb.RaftID(self.Cert)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := out[0].AddMember(ctx, self)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if err := raftdb.Join(dir, self.ID, out[0].Members()); err != nil {
				t.Fatal(err)
			}
		}
		w := &waiting{}
		nw.mu.Lock()
		nw.handlers[name] = w
		nw.mu.Unlock()
		d, err := raftdb.Open(raftdb.Config{Dir: dir, Self: self, Transport: transport{nw},
			Tick: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		w.mu.Lock()
		w.h = d.Handler()
		w.mu.Unlock()
		out = append(out, d)
	}
	return out
}
