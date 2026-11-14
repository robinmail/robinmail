// Package notify tells the connections of a node that an account changed:
// JMAP event streams and IMAP sessions wait here instead of asking the
// database on a timer each.
//
// A change reaches a listener in one of two ways.
//
//   - A write made through this node's own store calls Changed, which wakes
//     the account's listeners at once.
//   - A write made on another node is found by polling: while an account
//     has listeners here, Run reads its modseq once per Interval, for all
//     such accounts in one query, and wakes the listeners of every account
//     whose modseq moved. The read is at rqlite's level "none", so the
//     nearest rqlite node answers from its own copy and the leader is not
//     asked; a copy that is a moment behind shows the change on the next
//     round.
//
// Nodes do not tell each other about writes. A nudge over the node channel
// would cut the delay from a second to milliseconds, but the writer does not
// know which nodes have a listener for the account, so every write would
// call every node, and polling would still be needed for the nudges that get
// lost. One small read per second per node, and none at all without
// listeners, is cheaper and has one way to fail less.
//
// A wake-up carries no data and may be spurious (a local write is seen by
// Changed and again by the next poll). A listener reads the state itself
// and compares it with what it last showed its client.
package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultInterval is how often accounts with listeners are polled.
const DefaultInterval = time.Second

// Hub is one node's listeners. Fill States, then call Run. The zero value
// with only Changed in use (no polling) works too.
type Hub struct {
	// States reads the modseq of the given accounts; an account that does
	// not exist is left out. *store.Store has it as ModSeqs.
	States func(ctx context.Context, accounts []string) (map[string]int64, error)
	// Interval between polls (DefaultInterval when 0).
	Interval time.Duration
	Logger   *slog.Logger

	mu    sync.Mutex
	subs  map[string]map[*sub]struct{}
	known map[string]int64 // the modseq the poller last saw, per account with listeners
}

type sub struct{ c chan struct{} }

// Subscribe returns a channel that gets a value after the account changed,
// and a function that ends the subscription. The channel holds one pending
// wake-up: changes that come while the listener is busy collapse into one.
func (h *Hub) Subscribe(account string) (<-chan struct{}, func()) {
	s := &sub{c: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[string]map[*sub]struct{}{}
		h.known = map[string]int64{}
	}
	if h.subs[account] == nil {
		h.subs[account] = map[*sub]struct{}{}
	}
	h.subs[account][s] = struct{}{}
	var once sync.Once
	return s.c, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.subs[account], s)
			if len(h.subs[account]) == 0 {
				delete(h.subs, account)
				delete(h.known, account)
			}
		})
	}
}

// Changed wakes the listeners of an account. The store calls it after
// every write that moved the account's modseq.
func (h *Hub) Changed(account string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.wake(account)
}

func (h *Hub) wake(account string) {
	for s := range h.subs[account] {
		select {
		case s.c <- struct{}{}:
		default:
		}
	}
}

// Listeners returns how many subscriptions there are, for tests and logs.
func (h *Hub) Listeners() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, set := range h.subs {
		n += len(set)
	}
	return n
}

// Run polls until ctx is done.
func (h *Hub) Run(ctx context.Context) {
	interval := h.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := h.Poll(ctx)
		if err != nil && ctx.Err() == nil && !failing && h.Logger != nil {
			h.Logger.Warn("notify: reading account states failed; listeners wait", "err", err)
		}
		failing = err != nil
	}
}

// Poll is one round of Run: read the modseq of every account with
// listeners and wake those whose modseq is not the one seen last. The first
// time an account is seen its listeners are woken too: a change between
// their own read and this one would otherwise be missed.
func (h *Hub) Poll(ctx context.Context) error {
	if h.States == nil {
		return nil
	}
	h.mu.Lock()
	accounts := make([]string, 0, len(h.subs))
	for a := range h.subs {
		accounts = append(accounts, a)
	}
	h.mu.Unlock()
	if len(accounts) == 0 {
		return nil
	}
	states, err := h.States(ctx, accounts)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range accounts {
		if _, listening := h.subs[a]; !listening {
			continue
		}
		state, exists := states[a]
		if !exists {
			continue
		}
		if last, seen := h.known[a]; !seen || last != state {
			h.known[a] = state
			h.wake(a)
		}
	}
	return nil
}
