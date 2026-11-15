package smtp

import "sync"

// Budget bounds the bytes of messages that SMTP sessions hold in memory at
// once: what readData has read and the handler has not finished with. A
// message is read whole into memory before it is delivered, so without a
// bound the sum is MaxConnections times MaxMessageBytes, several GiB with
// the defaults. Past the budget a message is answered 452 4.3.1 and the
// sender tries again later; nothing is lost.
//
// Each client address (an IPv6 /64 counts as one, as for the connection
// limits) may hold at most PerClient of it, so one client cannot use the
// whole budget and defer everybody else's mail.
//
// One Budget may be shared by several servers (inbound and submission) so
// that the bound holds for the process.
type Budget struct {
	Total     int64 // bytes all sessions may hold together
	PerClient int64 // bytes one client address may hold

	mu   sync.Mutex
	used int64
	per  map[string]int64
}

// take reserves n bytes for the client key. It reports false, and
// reserves nothing, when that would go past either bound.
func (b *Budget) take(key string, n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.Total || b.per[key]+n > b.PerClient {
		return false
	}
	if b.per == nil {
		b.per = map[string]int64{}
	}
	b.used += n
	b.per[key] += n
	return true
}

// give returns n bytes taken for key.
func (b *Budget) give(key string, n int64) {
	if n == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	if b.per[key] -= n; b.per[key] <= 0 {
		delete(b.per, key)
	}
}

// Used is the number of bytes held now, for tests and metrics.
func (b *Budget) Used() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
