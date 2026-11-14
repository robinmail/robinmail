package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func woken(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// A local write wakes the listeners of its account, and only those.
func TestChangedWakesTheAccount(t *testing.T) {
	h := &Hub{}
	a1, stop1 := h.Subscribe("a")
	a2, stop2 := h.Subscribe("a")
	b, stopB := h.Subscribe("b")
	defer stop1()
	defer stop2()
	defer stopB()

	h.Changed("a")
	if !woken(a1) || !woken(a2) {
		t.Fatal("a listener of the account was not woken")
	}
	if woken(b) {
		t.Fatal("a listener of another account was woken")
	}
	h.Changed("nobody") // no listeners: nothing happens
}

// Changes while a listener is busy collapse into one wake-up, and Changed
// never blocks on a listener that does not read.
func TestWakeUpsCollapse(t *testing.T) {
	h := &Hub{}
	c, stop := h.Subscribe("a")
	defer stop()
	for i := 0; i < 100; i++ {
		h.Changed("a")
	}
	if !woken(c) {
		t.Fatal("not woken")
	}
	if woken(c) {
		t.Fatal("woken twice for changes that came before the first read")
	}
}

func TestUnsubscribe(t *testing.T) {
	h := &Hub{}
	c, stop := h.Subscribe("a")
	if h.Listeners() != 1 {
		t.Fatalf("listeners = %d", h.Listeners())
	}
	stop()
	stop() // twice is fine
	h.Changed("a")
	if woken(c) || h.Listeners() != 0 {
		t.Fatal("a closed subscription was woken or is still counted")
	}
}

// The poller wakes an account's listeners when its modseq is not the one it
// saw last, once when it sees the account for the first time, and asks only
// for accounts that have listeners.
func TestPoll(t *testing.T) {
	var mu sync.Mutex
	states := map[string]int64{"a": 5, "b": 9}
	var asked [][]string
	h := &Hub{States: func(_ context.Context, accounts []string) (map[string]int64, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, accounts)
		out := map[string]int64{}
		for _, a := range accounts {
			if s, ok := states[a]; ok {
				out[a] = s
			}
		}
		return out, nil
	}}
	ctx := context.Background()
	if err := h.Poll(ctx); err != nil || len(asked) != 0 {
		t.Fatalf("a poll without listeners asked the database: %v %v", asked, err)
	}

	a, stopA := h.Subscribe("a")
	defer stopA()
	gone, stopGone := h.Subscribe("gone") // an account the database does not have
	defer stopGone()

	h.Poll(ctx)
	if !woken(a) {
		t.Fatal("the first poll of an account must wake it: a change between the listener's read and the poll would be lost")
	}
	h.Poll(ctx)
	if woken(a) {
		t.Fatal("woken although the modseq did not move")
	}
	mu.Lock()
	states["a"] = 6
	states["b"] = 10
	mu.Unlock()
	h.Poll(ctx)
	if !woken(a) {
		t.Fatal("not woken although the modseq moved")
	}
	if woken(gone) {
		t.Fatal("an account that does not exist was woken")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, accounts := range asked {
		for _, acct := range accounts {
			if acct == "b" {
				t.Fatal("asked for an account without listeners")
			}
		}
	}
}

// A listener that comes back after all listeners of its account left is
// woken on its first poll again: what the poller knew is forgotten.
func TestPollForgetsAccountsWithoutListeners(t *testing.T) {
	h := &Hub{States: func(context.Context, []string) (map[string]int64, error) {
		return map[string]int64{"a": 1}, nil
	}}
	c, stop := h.Subscribe("a")
	h.Poll(context.Background())
	woken(c)
	stop()
	c, stop = h.Subscribe("a")
	defer stop()
	h.Poll(context.Background())
	if !woken(c) {
		t.Fatal("not woken on the first poll after subscribing again")
	}
}

// Run polls on its interval and survives a database that does not answer.
func TestRun(t *testing.T) {
	var mu sync.Mutex
	state, fail := int64(1), true
	h := &Hub{Interval: 5 * time.Millisecond, States: func(context.Context, []string) (map[string]int64, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return nil, errors.New("no leader")
		}
		return map[string]int64{"a": state}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	c, stop := h.Subscribe("a")
	defer stop()

	time.Sleep(30 * time.Millisecond)
	if woken(c) {
		t.Fatal("woken while the database did not answer")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatal("not woken after the database came back")
	}
	mu.Lock()
	state = 2
	mu.Unlock()
	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatal("not woken by a change")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}
