package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/store"
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

// The limit per IP counts an IPv6 /64 as one client: whoever has one
// address in it has them all.
func TestLoginRateLimitCountsAnIPv6Prefix(t *testing.T) {
	st := newSite(t)
	refused := 0
	for i := 0; i < maxFailsPerIP+10; i++ {
		// A new address of the same /64 and a new name with every guess.
		rec := st.login(tlsBase, fmt.Sprintf("user%d@example.test", i), "a guess", from(fmt.Sprintf("[2001:db8:1:2::%x]:4000", i+1)))
		if rec.Code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused != 10 {
		t.Errorf("%d of the guesses past the limit were refused, want 10", refused)
	}
	// Another /64 is another client.
	if rec := st.login(tlsBase, "anna@example.test", "a guess", from("[2001:db8:1:3::1]:4000")); rec.Code != http.StatusUnauthorized {
		t.Errorf("a guess from another /64: %d", rec.Code)
	}
}

// Wrong secrets for an address, from anywhere, must not close the door
// for the mail clients of its owner: an app password is 160 random bits,
// which nobody guesses, so there is nothing for a limit per address to
// protect, and a limit that also stops the right app password lets
// anybody lock a user's phone out for as long as they like.
func TestAppPasswordLoginIsNotLockedOutByOthers(t *testing.T) {
	st := newSite(t)
	const anna = "anna@example.test"
	_, secret := st.appPassword(st.in(anna, "correct horse"), "correct horse", "Phone")
	guess := func(ip, secret string) int {
		_, status, _ := st.s.MailLogin(context.Background(), ip, store.ScopeIMAP, anna, secret)
		return status
	}
	// Somebody else, from several addresses, with wrong secrets of both kinds.
	for i := 0; i < 3*maxFailsPerAddr; i++ {
		wrong := fmt.Sprint("a guess ", i)
		if i%2 == 1 {
			wrong = "abcd-efgh-ijkl-mnop-qrst-uvwx-yzab-" + fmt.Sprintf("%04d", i)[:4]
			wrong = strings.NewReplacer("0", "a", "1", "b", "8", "c", "9", "d").Replace(wrong)
		}
		if status := guess(fmt.Sprintf("198.51.100.%d", i%7+1), wrong); status != http.StatusUnauthorized && status != http.StatusTooManyRequests {
			t.Fatalf("guess %d: %d", i, status)
		}
	}
	if status := guess("192.0.2.50", secret); status != 0 {
		t.Errorf("anna's phone with its app password after the guesses of others: %d", status)
	}
	// The account password stays limited per address: it can be guessed.
	if status := guess("192.0.2.51", "another guess"); status != http.StatusTooManyRequests {
		t.Errorf("a password guess after %d failures: %d", 3*maxFailsPerAddr, status)
	}
	// And app-password-shaped guesses are limited per client address.
	st2 := newSite(t)
	n := 0
	for i := 0; i < maxFailsPerIP+5; i++ {
		w := strings.NewReplacer("0", "a", "1", "b", "8", "c", "9", "d").Replace(fmt.Sprintf("abcd-efgh-ijkl-mnop-qrst-uvwx-yzab-%04d", i))
		if _, status, _ := st2.s.MailLogin(context.Background(), "203.0.113.9", store.ScopeIMAP, anna, w); status == http.StatusTooManyRequests {
			n++
		}
	}
	if n != 5 {
		t.Errorf("%d of the app password guesses past the limit per address were refused, want 5", n)
	}
}
