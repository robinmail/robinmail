package store

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// TestLatency measures, one after the other, deliveries (Deliver: the
// write an SMTP delivery makes) and keyword changes (UpdateEmail: what a
// JMAP Email/set makes), and prints the median and the 99th percentile.
// It runs only with ROBINMAIL_LATENCY set; PLAN phase 10 has the numbers.
func TestLatency(t *testing.T) {
	if os.Getenv("ROBINMAIL_LATENCY") == "" {
		t.Skip("ROBINMAIL_LATENCY is not set")
	}
	s := open(t)
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
