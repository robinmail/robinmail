package web

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The limit on wrong passwords for one address holds when the guesses
// arrive at the same time: a burst of requests must not all be let in
// because none of them has failed yet.
func TestLoginRateLimitHoldsForABurst(t *testing.T) {
	st := newSite(t)
	var checks atomic.Int64
	st.s.Verify = func(hash, password string) bool {
		checks.Add(1)
		time.Sleep(2 * time.Millisecond) // a password check takes a while
		return hash != "" && hash == "hash:"+password
	}
	const burst = 200
	var wg sync.WaitGroup
	var unauthorized, tooMany atomic.Int64
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch rec := st.login(tlsBase, "anna@example.test", "a guess"); rec.Code {
			case http.StatusUnauthorized:
				unauthorized.Add(1)
			case http.StatusTooManyRequests:
				tooMany.Add(1)
			default:
				t.Errorf("status %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	if n := checks.Load(); n > maxFailsPerAddr {
		t.Errorf("%d passwords were checked in one burst; the limit is %d per window", n, maxFailsPerAddr)
	}
	if unauthorized.Load() > maxFailsPerAddr || tooMany.Load() < burst-maxFailsPerAddr {
		t.Errorf("%d answers 401 and %d answers 429", unauthorized.Load(), tooMany.Load())
	}
}

// The same for the second step: five wrong codes per window, however they
// arrive.
func TestCodeRateLimitHoldsForABurst(t *testing.T) {
	st := newSite(t)
	acct, _ := st.accounts.Account(t.Context(), "anna@example.test")
	acct.TOTP = true
	const burst = 200
	var wg sync.WaitGroup
	var refused atomic.Int64
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, status, _ := st.s.secondFactor(t.Context(), acct, "192.0.2.1", "000000", ""); status == http.StatusUnauthorized {
				refused.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := refused.Load(); n > maxFailsPerCode {
		t.Errorf("%d codes were tried in one burst; the limit is %d per window", n, maxFailsPerCode)
	}
}
