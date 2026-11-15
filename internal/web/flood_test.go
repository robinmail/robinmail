package web

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/store"
)

// Review finding 15: every failed login wrote to the database (the shared
// counters and the auth event log), bounded per client address only. A
// wrong secret in the form of an app password costs no Argon2id, so a
// client with many IPv6 /64s could make thousands of writes a second. Now
// a node writes at most a burst and then failureWriteRate a second; the
// limits still hold on the node, from its memory.
func TestFailureWritesAreBounded(t *testing.T) {
	st := newSite(t)
	limits := &fakeLimits{now: func() time.Time { return st.clock }, m: map[string]store.LoginFailure{}}
	st.s.Limits = limits
	secret := strings.Repeat("a", 32) // the form of an app password
	const n = 2000
	for i := range n {
		ip := fmt.Sprintf("2001:db8:%x:%x::1", i>>16, i&0xffff) // n /64s
		_, status, _ := st.s.MailLogin(context.Background(), ip, store.ScopeIMAP, fmt.Sprintf("user%d@example.test", i), secret)
		if status != 401 {
			t.Fatalf("attempt %d: %d", i, status)
		}
	}
	writes := func() int {
		limits.mu.Lock()
		defer limits.mu.Unlock()
		return limits.writes + len(st.accounts.eventsOf(EventLoginFailed))
	}
	if w := writes(); w > failureWriteBurst {
		t.Fatalf("%d failures made %d writes, want at most %d", n, w, failureWriteBurst)
	}

	// The limits hold all the same: an address is closed after ten
	// failures from as many client addresses, counted in memory.
	for i := range maxFailsPerAddr {
		_, status, _ := st.s.MailLogin(context.Background(), fmt.Sprintf("2001:db8:ffff:%x::1", i), store.ScopeIMAP, "anna@example.test", "guess")
		if status != 401 {
			t.Fatalf("guess %d: %d", i, status)
		}
	}
	if _, status, _ := st.s.MailLogin(context.Background(), "2001:db8:fffe::1", store.ScopeIMAP, "anna@example.test", "correct horse"); status != 429 {
		t.Fatalf("after ten failures with the writes left out: %d", status)
	}

	// The budget comes back with time.
	before := writes()
	st.clock = st.clock.Add(time.Second)
	st.s.MailLogin(context.Background(), "2001:db8:fffd::1", store.ScopeIMAP, "bob@example.test", secret)
	if writes() != before+2 {
		t.Fatalf("after a second: %d writes, want %d", writes(), before+2)
	}
}
