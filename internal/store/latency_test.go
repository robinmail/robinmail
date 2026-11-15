package store

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/blob"
	"github.com/robinmail/robinmail/internal/raftdb/raftdbtest"
)

// TestLatency measures, one after the other, deliveries (Deliver: the
// write an SMTP delivery makes) and keyword changes (UpdateEmail: what a
// JMAP Email/set makes), and prints the median and the 99th percentile.
// It runs only with ROBINMAIL_LATENCY set; PLAN phase 10 has the numbers.
// ROBINMAIL_LATENCY=follower and =leader run it on a store of a follower
// or the leader of a three-node cluster in this process (a transport in
// memory: no network, no TLS); anything else on a node of its own.
func TestLatency(t *testing.T) {
	mode := os.Getenv("ROBINMAIL_LATENCY")
	if mode == "" {
		t.Skip("ROBINMAIL_LATENCY is not set")
	}
	var s *Store
	if mode == "follower" || mode == "leader" {
		dbs := raftdbtest.Cluster(t, 3)
		master, _ := blob.NewMasterKey(testMasterKey)
		var lead, follow *Store
		for _, d := range dbs {
			st := New(d, blob.NewLocal(t.TempDir()), master)
			if d.Status().State == "StateLeader" {
				lead = st
			} else {
				follow = st
			}
		}
		if lead == nil {
			t.Fatal("no leader")
		}
		if err := lead.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if err := lead.AddDomain(ctx, "example.test"); err != nil {
			t.Fatal(err)
		}
		s = follow
		if mode == "leader" {
			s = lead
		}
		locals[s] = s.blobs.(*blob.Local)
	} else {
		s = open(t)
	}
	a := account(t, s)
	const n = 500
	measure := func(name string, op func(i int)) {
		d := make([]time.Duration, n)
		for i := range n {
			start := time.Now()
			op(i)
			d[i] = time.Since(start)
		}
		slices.Sort(d)
		fmt.Printf("latency %-8s n=%d p50=%v p90=%v p99=%v\n", name, n, d[n/2].Round(10*time.Microsecond),
			d[n*9/10].Round(10*time.Microsecond), d[n*99/100].Round(10*time.Microsecond))
	}
	ids := make([]int64, n)
	measure("deliver", func(i int) {
		e, err := s.Deliver(ctx, a.ID, Message{Raw: msg(fmt.Sprintf("lat-%d@x", i), "latency"), Key: fmt.Sprintf("lat-%d-%d", time.Now().UnixNano(), i)})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = e.ID
	})
	measure("keyword", func(i int) {
		if _, err := s.UpdateEmail(ctx, a.ID, ids[i], Update{AddKeywords: []string{"$seen"}}); err != nil {
			t.Fatal(err)
		}
	})
}
