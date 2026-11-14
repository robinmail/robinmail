package store

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sha(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func session(a Account, id string) Session {
	return Session{ID: id, AccountID: a.ID, Stage: StageFull, Method: "password",
		ExpiresAt: time.Now().Add(time.Hour), Idle: time.Hour, IP: "192.0.2.1", Agent: "test"}
}

func TestSessions(t *testing.T) {
	s := open(t)
	a := account(t, s)
	tok := "token-" + a.ID
	if err := s.CreateSession(ctx, sha(tok), session(a, "s1-"+a.ID), ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionAccount(ctx, sha(tok))
	if err != nil || got.ID != a.ID || got.Address != a.Address {
		t.Fatalf("session: %+v %v", got, err)
	}
	// The token itself is not what is stored.
	_, err = s.SessionAccount(ctx, tok)
	wantErr(t, "token instead of hash", err, ErrNoSession)
	_, err = s.SessionAccount(ctx, sha("other"))
	wantErr(t, "unknown", err, ErrNoSession)
	wantErr(t, "unknown account", s.CreateSession(ctx, sha("x"), Session{ID: "x", AccountID: "nobody", Stage: StageFull}, ""), ErrNoAccount)

	// Both lifetimes end a session: the absolute one, and the idle one.
	for name, sess := range map[string]Session{
		"expired": {ExpiresAt: time.Now().Add(-time.Minute), Idle: time.Hour},
		"idle":    {ExpiresAt: time.Now().Add(time.Hour), Idle: 0},
	} {
		sess.ID, sess.AccountID, sess.Stage = name+"-"+a.ID, a.ID, StageFull
		s.CreateSession(ctx, sha(name+a.ID), sess, "")
		_, err = s.SessionAccount(ctx, sha(name+a.ID))
		wantErr(t, name, err, ErrNoSession)
	}
	if n, err := s.TrimSessions(ctx, time.Now()); err != nil || n < 2 {
		t.Errorf("trim: %d %v", n, err)
	}
	if _, err := s.SessionAccount(ctx, sha(tok)); err != nil {
		t.Errorf("a live session was trimmed: %v", err)
	}

	// A login that still owes its second step is no session yet.
	pending := session(a, "p-"+a.ID)
	pending.Stage = StageTOTP
	wantErr(t, "pending", s.CreateSession(ctx, sha("pending"+a.ID), pending, ""), nil)
	_, err = s.SessionAccount(ctx, sha("pending"+a.ID))
	wantErr(t, "a pending login as a session", err, ErrNoSession)
	sess, _, err := s.SessionByToken(ctx, sha("pending"+a.ID))
	want(t, "the pending login", []any{sess.Stage, err}, []any{StageTOTP, nil})

	// Rotation: the new session replaces the one it came from, in one batch.
	wantErr(t, "rotate", s.CreateSession(ctx, sha("full"+a.ID), session(a, "s2-"+a.ID), sha("pending"+a.ID)), nil)
	_, _, err = s.SessionByToken(ctx, sha("pending"+a.ID))
	wantErr(t, "the replaced session", err, ErrNoSession)

	// The list has the complete logins; a touch moves "last seen".
	s.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	wantErr(t, "touch", s.TouchSession(ctx, sha(tok), "192.0.2.9"), nil)
	s.Now = time.Now
	list, err := s.Sessions(ctx, a.ID)
	if err != nil || len(list) != 2 || list[0].ID != "s1-"+a.ID || list[0].SeenIP != "192.0.2.9" || list[0].IP != "192.0.2.1" ||
		!list[0].SeenAt.After(list[0].CreatedAt) || list[0].Agent != "test" {
		t.Fatalf("sessions: %+v %v", list, err)
	}

	// Revoking by id moves auth_seq and tells this node's listeners.
	var told atomic.Int32
	s.OnAuthChange = func(id string) {
		if id == a.ID {
			told.Add(1)
		}
	}
	before, _ := s.AuthSeqs(ctx, []string{a.ID})
	ok, err := s.RevokeSession(ctx, a.ID, "s2-"+a.ID)
	want(t, "revoke", []any{ok, err}, []any{true, nil})
	ok, err = s.RevokeSession(ctx, a.ID, "s2-"+a.ID)
	want(t, "revoke again", []any{ok, err}, []any{false, nil})
	// Another account cannot revoke it.
	other := account(t, s)
	ok, _ = s.RevokeSession(ctx, other.ID, "s1-"+a.ID)
	want(t, "revoke with another account", ok, false)
	after, _ := s.AuthSeqs(ctx, []string{a.ID})
	if after[a.ID] <= before[a.ID] || told.Load() < 2 {
		t.Errorf("auth_seq %d -> %d, told %d times", before[a.ID], after[a.ID], told.Load())
	}

	// All but one.
	s.CreateSession(ctx, sha("third"+a.ID), session(a, "s3-"+a.ID), "")
	n, err := s.RevokeSessions(ctx, a.ID, sha(tok))
	want(t, "revoke the others", []any{n, err}, []any{int64(1), nil})
	if _, err := s.SessionAccount(ctx, sha(tok)); err != nil {
		t.Errorf("the session that was to stay: %v", err)
	}

	if err := s.DeleteSession(ctx, sha(tok)); err != nil {
		t.Fatal(err)
	}
	_, err = s.SessionAccount(ctx, sha(tok))
	wantErr(t, "deleted", err, ErrNoSession)
	if err := s.DeleteSession(ctx, sha(tok)); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
	byID, err := s.AccountByID(ctx, a.ID)
	if err != nil || byID.Address != a.Address {
		t.Errorf("AccountByID: %+v %v", byID, err)
	}
	_, err = s.AccountByID(ctx, "nobody")
	wantErr(t, "AccountByID unknown", err, ErrNoAccount)
}

func TestAppPasswords(t *testing.T) {
	s := open(t)
	a := account(t, s)
	other := account(t, s)
	wantErr(t, "create", s.CreateAppPassword(ctx, a.ID, "p1", "Phone", sha("secret-1"+a.ID), nil), nil)
	wantErr(t, "create", s.CreateAppPassword(ctx, a.ID, "p2", "Laptop", sha("secret-2"+a.ID), []string{ScopeIMAP, ScopeSMTP}), nil)
	// The same batch again (a retry after an unknown outcome) adds nothing.
	wantErr(t, "create again", s.CreateAppPassword(ctx, a.ID, "p2", "Laptop", sha("secret-2"+a.ID), []string{ScopeIMAP, ScopeSMTP}), nil)
	wantErr(t, "unknown account", s.CreateAppPassword(ctx, "nobody", "p", "x", sha("zz"+a.ID), nil), ErrNoAccount)

	got, err := s.Account(ctx, a.Address)
	want(t, "the account counts them", []any{got.AppPasswords, err}, []any{2, nil})
	if got.AuthSeq < 2 {
		t.Errorf("auth_seq after two app passwords: %d", got.AuthSeq)
	}

	p, err := s.AppPasswordByHash(ctx, a.ID, sha("secret-2"+a.ID))
	if err != nil || p.ID != "p2" || p.Name != "Laptop" || !p.Allows(ScopeIMAP) || p.Allows(ScopeJMAP) || !p.UsedAt.IsZero() {
		t.Fatalf("by hash: %+v %v", p, err)
	}
	// The secret is not what is stored, and a hash opens its own account only.
	_, err = s.AppPasswordByHash(ctx, a.ID, "secret-2"+a.ID)
	wantErr(t, "the secret instead of its hash", err, ErrNoAppPassword)
	_, err = s.AppPasswordByHash(ctx, other.ID, sha("secret-2"+a.ID))
	wantErr(t, "another account", err, ErrNoAppPassword)
	all, _ := s.AppPasswordByHash(ctx, a.ID, sha("secret-1"+a.ID))
	want(t, "no scope is every protocol", []bool{all.Allows(ScopeIMAP), all.Allows(ScopeSMTP), all.Allows(ScopeJMAP)}, []bool{true, true, true})

	wantErr(t, "use", s.UseAppPassword(ctx, a.ID, "p2", "198.51.100.7"), nil)
	list, err := s.AppPasswords(ctx, a.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	for _, p := range list {
		if p.ID == "p2" && (p.UsedIP != "198.51.100.7" || p.UsedAt.IsZero()) {
			t.Errorf("last use: %+v", p)
		}
	}

	ok, err := s.RevokeAppPassword(ctx, other.ID, "p2")
	want(t, "revoke with another account", []any{ok, err}, []any{false, nil})
	ok, err = s.RevokeAppPassword(ctx, a.ID, "p2")
	want(t, "revoke", []any{ok, err}, []any{true, nil})
	_, err = s.AppPasswordByHash(ctx, a.ID, sha("secret-2"+a.ID))
	wantErr(t, "revoked", err, ErrNoAppPassword)
	_, err = s.AppPassword(ctx, a.ID, "p2")
	wantErr(t, "revoked, by id", err, ErrNoAppPassword)

	// The limit.
	for i := 0; i < MaxAppPasswords-1; i++ {
		id := "x" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		wantErr(t, "fill", s.CreateAppPassword(ctx, a.ID, id, "n", sha(id+a.ID), nil), nil)
	}
	wantErr(t, "one too many", s.CreateAppPassword(ctx, a.ID, "last", "n", sha("last"+a.ID), nil), ErrAppPasswords)
}

func TestSetPassword(t *testing.T) {
	s := open(t)
	a := account(t, s)
	s.CreateSession(ctx, sha("keep"+a.ID), session(a, "k-"+a.ID), "")
	s.CreateSession(ctx, sha("lose"+a.ID), session(a, "l-"+a.ID), "")
	wantErr(t, "a stale current hash", s.SetPassword(ctx, a.ID, "not the hash", "$argon2id$new", sha("keep"+a.ID)), ErrPasswordChanged)
	wantErr(t, "set", s.SetPassword(ctx, a.ID, a.PasswordHash, "$argon2id$new", sha("keep"+a.ID)), nil)
	got, _ := s.Account(ctx, a.Address)
	if got.PasswordHash != "$argon2id$new" || got.AuthSeq != a.AuthSeq+1 || got.PasswordChangedAt.IsZero() {
		t.Errorf("after the change: %+v", got)
	}
	if _, err := s.SessionAccount(ctx, sha("keep"+a.ID)); err != nil {
		t.Errorf("the session that changed the password: %v", err)
	}
	_, err := s.SessionAccount(ctx, sha("lose"+a.ID))
	wantErr(t, "the other session", err, ErrNoSession)
	// Without a current hash (an administrator) and without a session to keep.
	wantErr(t, "reset", s.SetPassword(ctx, a.ID, "", "$argon2id$reset", ""), nil)
	_, err = s.SessionAccount(ctx, sha("keep"+a.ID))
	wantErr(t, "every session after a reset", err, ErrNoSession)
	wantErr(t, "unknown account", s.SetPassword(ctx, "nobody", "", "x", ""), ErrNoAccount)
}

func TestTOTPState(t *testing.T) {
	s := open(t)
	a := account(t, s)
	_, err := s.TOTPState(ctx, a.ID)
	wantErr(t, "none yet", err, ErrNoTOTP)
	wantErr(t, "confirm without a secret", s.ConfirmTOTP(ctx, a.ID, 1, nil), ErrTOTPNotPending)

	secret := []byte("12345678901234567890")
	wantErr(t, "pending", s.SetPendingTOTP(ctx, a.ID, secret), nil)
	st, err := s.TOTPState(ctx, a.ID)
	if err != nil || string(st.Secret) != string(secret) || st.Confirmed {
		t.Fatalf("pending: %+v %v", st, err)
	}
	// Not asked for at login until it is confirmed.
	got, _ := s.Account(ctx, a.Address)
	want(t, "account.TOTP while pending", got.TOTP, false)
	ok, err := s.UseTOTPStep(ctx, a.ID, 100)
	want(t, "a step of a pending secret", []any{ok, err}, []any{false, nil})

	// The secret is sealed: the row does not hold it, and it does not open
	// as another account's.
	res, _ := s.query(ctx, a.ID, q(`SELECT secret FROM totp WHERE account_id = :a`))
	sealed := asString(res[0].Values[0][0])
	if len(sealed) < 40 || sealed[:3] != sealVersion {
		t.Fatalf("stored secret: %q", sealed)
	}
	other := account(t, s)
	if _, err := s.unseal(ctx, other.ID, sealed, totpAD(other.ID)); err == nil {
		t.Error("the sealed secret opened under another account")
	}
	if _, err := s.unseal(ctx, a.ID, sealed, totpAD(other.ID)); err == nil {
		t.Error("the sealed secret opened with another account's associated data")
	}

	// A new pending secret replaces the old one.
	secret2 := []byte("abcdefghijklmnopqrst")
	wantErr(t, "pending again", s.SetPendingTOTP(ctx, a.ID, secret2), nil)
	wantErr(t, "confirm", s.ConfirmTOTP(ctx, a.ID, 100, []string{sha("r1" + a.ID), sha("r2" + a.ID)}), nil)
	st, _ = s.TOTPState(ctx, a.ID)
	if string(st.Secret) != string(secret2) || !st.Confirmed || st.LastStep != 100 || st.RecoveryCodes != 2 {
		t.Fatalf("confirmed: %+v", st)
	}
	got, _ = s.Account(ctx, a.Address)
	want(t, "account.TOTP", got.TOTP, true)
	wantErr(t, "confirm twice", s.ConfirmTOTP(ctx, a.ID, 101, nil), ErrTOTPNotPending)
	wantErr(t, "a new secret while on", s.SetPendingTOTP(ctx, a.ID, secret), ErrTOTPEnabled)

	// RFC 6238 5.2: "the verifier MUST NOT accept the second attempt of
	// the OTP after the successful validation has been issued for the
	// first OTP". The step used to confirm is spent, an older one too.
	for _, c := range []struct {
		step int64
		ok   bool
	}{{100, false}, {99, false}, {101, true}, {101, false}, {103, true}, {102, false}} {
		ok, err := s.UseTOTPStep(ctx, a.ID, c.step)
		want(t, "step", []any{c.step, ok, err}, []any{c.step, c.ok, nil})
	}
	// Twenty nodes get the same code at once: one of them accepts it.
	var wg sync.WaitGroup
	var won atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := s.UseTOTPStep(ctx, a.ID, 200); ok {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	want(t, "winners of one step", won.Load(), int32(1))

	// A recovery code works once.
	ok, err = s.UseRecoveryCode(ctx, a.ID, sha("r1"+a.ID))
	want(t, "recovery code", []any{ok, err}, []any{true, nil})
	ok, err = s.UseRecoveryCode(ctx, a.ID, sha("r1"+a.ID))
	want(t, "recovery code again", []any{ok, err}, []any{false, nil})
	ok, _ = s.UseRecoveryCode(ctx, other.ID, sha("r2"+a.ID))
	want(t, "another account's recovery code", ok, false)
	wantErr(t, "new codes", s.ReplaceRecoveryCodes(ctx, a.ID, []string{sha("n1" + a.ID)}), nil)
	ok, _ = s.UseRecoveryCode(ctx, a.ID, sha("r2"+a.ID))
	want(t, "an old code after new ones were made", ok, false)
	wantErr(t, "codes without TOTP", s.ReplaceRecoveryCodes(ctx, other.ID, []string{"x"}), ErrNoTOTP)

	was, err := s.DisableTOTP(ctx, a.ID)
	want(t, "disable", []any{was, err}, []any{true, nil})
	was, err = s.DisableTOTP(ctx, a.ID)
	want(t, "disable again", []any{was, err}, []any{false, nil})
	_, err = s.TOTPState(ctx, a.ID)
	wantErr(t, "after disabling", err, ErrNoTOTP)
	ok, _ = s.UseRecoveryCode(ctx, a.ID, sha("n1"+a.ID))
	want(t, "a recovery code after disabling", ok, false)
}

func TestOIDCLink(t *testing.T) {
	s := open(t)
	a := account(t, s)
	wantErr(t, "first login", s.LinkOIDC(ctx, a.ID, "kc", "subject-1"), nil)
	wantErr(t, "same subject", s.LinkOIDC(ctx, a.ID, "kc", "subject-1"), nil)
	wantErr(t, "another subject", s.LinkOIDC(ctx, a.ID, "kc", "subject-2"), ErrOIDCSubject)
	wantErr(t, "another provider", s.LinkOIDC(ctx, a.ID, "other", "subject-2"), nil)
	n, err := s.UnlinkOIDC(ctx, a.ID)
	want(t, "unlink", []any{n, err}, []any{int64(2), nil})
	wantErr(t, "after unlinking", s.LinkOIDC(ctx, a.ID, "kc", "subject-2"), nil)
	wantErr(t, "unknown account", s.LinkOIDC(ctx, "nobody", "kc", "s"), ErrNoAccount)
}

func TestAuthEvents(t *testing.T) {
	s := open(t)
	a := account(t, s)
	// The log is shared with every other test on this database; this one
	// lives in 2001, where nothing else writes and no trim of others reaches.
	old := time.Date(2001, 5, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return old }
	long := make([]byte, 1000)
	for i := range long {
		long[i] = 'x'
	}
	wantErr(t, "add", s.AddAuthEvent(ctx, AuthEvent{AccountID: a.ID, Address: a.Address, Event: "login", IP: "192.0.2.1", Detail: "web"}), nil)
	s.Now = func() time.Time { return old.Add(time.Hour) }
	wantErr(t, "add", s.AddAuthEvent(ctx, AuthEvent{AccountID: a.ID, Address: a.Address, Event: "login-failed", IP: "192.0.2.2", Detail: string(long)}), nil)
	list, err := s.AuthEvents(ctx, a.ID, 10)
	if err != nil || len(list) != 2 || list[0].Event != "login-failed" || list[1].Event != "login" ||
		len(list[0].Detail) != maxEventDetail || !list[1].At.Equal(old) || list[1].IP != "192.0.2.1" {
		t.Fatalf("events: %+v %v", list, err)
	}
	if one, _ := s.AuthEvents(ctx, a.ID, 1); len(one) != 1 || one[0].ID != list[0].ID {
		t.Errorf("limit: %+v", one)
	}
	if all, err := s.AuthEvents(ctx, "", 5); err != nil || len(all) == 0 {
		t.Errorf("all accounts: %+v %v", all, err)
	}
	n, err := s.TrimAuthEvents(ctx, old.Add(time.Minute))
	if err != nil || n < 1 {
		t.Errorf("trim: %d %v", n, err)
	}
	list, _ = s.AuthEvents(ctx, a.ID, 10)
	if len(list) != 1 || list[0].Event != "login-failed" {
		t.Errorf("after the trim: %+v", list)
	}
	s.TrimAuthEvents(ctx, old.Add(2*time.Hour))
}
