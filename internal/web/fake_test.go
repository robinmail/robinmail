package web

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robinmail/robinmail/internal/store"
)

// fakeAccounts is the store in memory: accounts, sessions, app passwords,
// TOTP and the log of auth events, with the rules of internal/store that
// the handlers lean on (a session's two lifetimes, a time step claimed
// once, a recovery code spent once). Two Servers given the same
// fakeAccounts are two nodes on one database. Passwords are "hashed" as
// "hash:" plus the password, which the Verify of newSite undoes.
type fakeAccounts struct {
	mu       sync.Mutex
	accounts map[string]*store.Account // by id
	sessions map[string]*store.Session // by token hash
	apps     map[string][]fakeApp      // by account id
	totp     map[string]*store.TOTP    // by account id
	recovery map[string]map[string]bool
	events   []store.AuthEvent
	domains  map[string]bool   // accepted domains
	links    map[string]string // account/provider -> subject
	now      func() time.Time
	writes   int // sessions touched and app passwords noted as used
}

type fakeApp struct {
	store.AppPassword
	hash string
}

func newFakeAccounts(now func() time.Time, accounts ...store.Account) *fakeAccounts {
	f := &fakeAccounts{now: now, accounts: map[string]*store.Account{}, sessions: map[string]*store.Session{},
		apps: map[string][]fakeApp{}, totp: map[string]*store.TOTP{}, recovery: map[string]map[string]bool{},
		domains: map[string]bool{"example.test": true}}
	for _, a := range accounts {
		f.accounts[a.ID] = &a
	}
	return f
}

// view is the account as a read returns it, with what is computed.
func (f *fakeAccounts) view(a *store.Account) store.Account {
	out := *a
	out.AppPasswords = len(f.apps[a.ID])
	out.TOTP = f.totp[a.ID] != nil && f.totp[a.ID].Confirmed
	return out
}

func (f *fakeAccounts) Account(_ context.Context, address string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.accounts {
		if a.Address == address {
			return f.view(a), nil
		}
	}
	return store.Account{}, store.ErrNoAccount
}

func (f *fakeAccounts) AccountByID(_ context.Context, id string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.accounts[id]; ok {
		return f.view(a), nil
	}
	return store.Account{}, store.ErrNoAccount
}

func (f *fakeAccounts) SetPassword(_ context.Context, account, oldHash, newHash, keep string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[account]
	if !ok {
		return store.ErrNoAccount
	}
	if oldHash != "" && a.PasswordHash != oldHash {
		return store.ErrPasswordChanged
	}
	a.PasswordHash, a.PasswordChangedAt = newHash, f.now()
	a.AuthSeq++
	for h, s := range f.sessions {
		if s.AccountID == account && h != keep {
			delete(f.sessions, h)
		}
	}
	return nil
}

func (f *fakeAccounts) CreateSession(_ context.Context, hash string, sess store.Session, replaces string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.accounts[sess.AccountID]; !ok {
		return store.ErrNoAccount
	}
	delete(f.sessions, replaces)
	sess.CreatedAt, sess.SeenAt, sess.SeenIP = f.now(), f.now(), sess.IP
	f.sessions[hash] = &sess
	return nil
}

func (f *fakeAccounts) live(s *store.Session) bool {
	now := f.now()
	return now.Before(s.ExpiresAt) && now.Before(s.SeenAt.Add(s.Idle))
}

func (f *fakeAccounts) SessionByToken(_ context.Context, hash string) (store.Session, store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[hash]
	if !ok || !f.live(s) || f.accounts[s.AccountID] == nil {
		return store.Session{}, store.Account{}, store.ErrNoSession
	}
	return *s, f.view(f.accounts[s.AccountID]), nil
}

func (f *fakeAccounts) TouchSession(_ context.Context, hash, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[hash]; ok {
		s.SeenAt, s.SeenIP = f.now(), ip
		f.writes++
	}
	return nil
}

func (f *fakeAccounts) DeleteSession(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, hash)
	return nil
}

func (f *fakeAccounts) Sessions(_ context.Context, account string) ([]store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Session
	for _, s := range f.sessions {
		if s.AccountID == account && s.Stage == store.StageFull && f.live(s) {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SeenAt.After(out[j].SeenAt) })
	return out, nil
}

func (f *fakeAccounts) RevokeSession(_ context.Context, account, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for h, s := range f.sessions {
		if s.AccountID == account && s.ID == id {
			delete(f.sessions, h)
			f.accounts[account].AuthSeq++
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeAccounts) RevokeSessions(_ context.Context, account, except string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for h, s := range f.sessions {
		if s.AccountID == account && h != except {
			delete(f.sessions, h)
			n++
		}
	}
	f.accounts[account].AuthSeq++
	return n, nil
}

func (f *fakeAccounts) CreateAppPassword(_ context.Context, account, id, name, hash string, scopes []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.apps[account]) >= store.MaxAppPasswords {
		return store.ErrAppPasswords
	}
	f.apps[account] = append(f.apps[account], fakeApp{store.AppPassword{ID: id, Name: name, Scopes: scopes, CreatedAt: f.now()}, hash})
	f.accounts[account].AuthSeq++
	return nil
}

func (f *fakeAccounts) AppPasswords(_ context.Context, account string) ([]store.AppPassword, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AppPassword
	for _, p := range f.apps[account] {
		out = append(out, p.AppPassword)
	}
	return out, nil
}

func (f *fakeAccounts) AppPasswordByHash(_ context.Context, account, hash string) (store.AppPassword, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.apps[account] {
		if p.hash == hash {
			return p.AppPassword, nil
		}
	}
	return store.AppPassword{}, store.ErrNoAppPassword
}

func (f *fakeAccounts) AppPassword(_ context.Context, account, id string) (store.AppPassword, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.apps[account] {
		if p.ID == id {
			return p.AppPassword, nil
		}
	}
	return store.AppPassword{}, store.ErrNoAppPassword
}

func (f *fakeAccounts) UseAppPassword(_ context.Context, account, id, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.apps[account] {
		if p.ID == id {
			f.apps[account][i].UsedAt, f.apps[account][i].UsedIP = f.now(), ip
			f.writes++
		}
	}
	return nil
}

func (f *fakeAccounts) RevokeAppPassword(_ context.Context, account, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.apps[account] {
		if p.ID == id {
			f.apps[account] = append(f.apps[account][:i:i], f.apps[account][i+1:]...)
			f.accounts[account].AuthSeq++
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeAccounts) SetPendingTOTP(_ context.Context, account string, secret []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.totp[account]; t != nil && t.Confirmed {
		return store.ErrTOTPEnabled
	}
	f.totp[account] = &store.TOTP{Secret: secret, CreatedAt: f.now()}
	return nil
}

func (f *fakeAccounts) TOTPState(_ context.Context, account string) (store.TOTP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.totp[account]
	if t == nil {
		return store.TOTP{}, store.ErrNoTOTP
	}
	out := *t
	out.RecoveryCodes = len(f.recovery[account])
	return out, nil
}

func (f *fakeAccounts) setCodes(account string, hashes []string) {
	f.recovery[account] = map[string]bool{}
	for _, h := range hashes {
		f.recovery[account][h] = true
	}
}

func (f *fakeAccounts) ConfirmTOTP(_ context.Context, account string, step int64, hashes []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.totp[account]
	if t == nil || t.Confirmed {
		return store.ErrTOTPNotPending
	}
	t.Confirmed, t.LastStep = true, step
	f.setCodes(account, hashes)
	return nil
}

func (f *fakeAccounts) ReplaceRecoveryCodes(_ context.Context, account string, hashes []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.totp[account]; t == nil || !t.Confirmed {
		return store.ErrNoTOTP
	}
	f.setCodes(account, hashes)
	return nil
}

func (f *fakeAccounts) UseTOTPStep(_ context.Context, account string, step int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.totp[account]
	if t == nil || !t.Confirmed || t.LastStep >= step {
		return false, nil
	}
	t.LastStep = step
	return true, nil
}

func (f *fakeAccounts) UseRecoveryCode(_ context.Context, account, hash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.recovery[account][hash] {
		return false, nil
	}
	delete(f.recovery[account], hash)
	return true, nil
}

func (f *fakeAccounts) DisableTOTP(_ context.Context, account string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	was := f.totp[account] != nil && f.totp[account].Confirmed
	delete(f.totp, account)
	delete(f.recovery, account)
	for h, s := range f.sessions {
		if s.AccountID == account && s.Stage == store.StageTOTP {
			delete(f.sessions, h)
		}
	}
	return was, nil
}

func (f *fakeAccounts) AddAuthEvent(_ context.Context, e store.AuthEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.At = f.now()
	f.events = append(f.events, e)
	return nil
}

// eventsOf returns the events of that name, oldest first.
func (f *fakeAccounts) eventsOf(event string) []store.AuthEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AuthEvent
	for _, e := range f.events {
		if e.Event == event {
			out = append(out, e)
		}
	}
	return out
}

// CreateAccount makes an account if the address is in an accepted domain.
func (f *fakeAccounts) CreateAccount(_ context.Context, address, hash string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	domain := address[strings.LastIndexByte(address, '@')+1:]
	if !f.domains[domain] {
		return store.Account{}, store.ErrNoDomain
	}
	for _, a := range f.accounts {
		if a.Address == address {
			return store.Account{}, store.ErrAccountExists
		}
	}
	a := &store.Account{ID: "acct-" + address, Address: address, PasswordHash: hash, CreatedAt: f.now()}
	f.accounts[a.ID] = a
	return *a, nil
}

func (f *fakeAccounts) LinkOIDC(_ context.Context, account, provider, subject string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.links == nil {
		f.links = map[string]string{}
	}
	key := account + "/" + provider
	if had, ok := f.links[key]; ok && had != subject {
		return store.ErrOIDCSubject
	}
	f.links[key] = subject
	return nil
}
