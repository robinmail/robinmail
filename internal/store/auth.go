package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/mikepage/robinmail/internal/metrics"
	"strings"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/mikepage/robinmail/internal/rqlite"
)

// This file holds what phase 7 added to the ways into an account: app
// passwords, TOTP with recovery codes, the password itself, the link to an
// OIDC subject and the log of auth events. Sessions are in session.go.
//
// Secrets and what is stored of them:
//
//	account password   Argon2id (internal/users): people choose it
//	session token      SHA-256: 256 random bits
//	app password       SHA-256: 160 random bits, made by the server
//	recovery code      SHA-256: 80 random bits, made by the server, and
//	                   every guess is counted against the login limits
//	TOTP secret        itself, sealed with the account's key: the server
//	                   needs it to compute codes
//
// A fast hash is right for a secret nobody could guess: what a slow one
// buys is time against a dictionary, and there is none for random bits.

// MaxAppPasswords is how many app passwords one account can have.
const MaxAppPasswords = 50

// Scopes of an app password: the protocols it opens.
const (
	ScopeIMAP = "imap"
	ScopeSMTP = "smtp"
	ScopeJMAP = "jmap"
)

// AppPassword is a password for one device or program.
type AppPassword struct {
	ID   string
	Name string
	// Scopes lists the protocols it opens; none is all.
	Scopes    []string
	CreatedAt time.Time
	// UsedAt and UsedIP are the last login the server noted (zero: never).
	UsedAt time.Time
	UsedIP string
}

// Allows reports whether the app password opens the protocol.
func (p AppPassword) Allows(scope string) bool {
	if len(p.Scopes) == 0 {
		return true
	}
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

const appPasswordColumns = `id, name, scope, created_at, used_at, used_ip`

func scanAppPassword(row []any) AppPassword {
	p := AppPassword{ID: asString(row[0]), Name: asString(row[1]), Scopes: strings.Fields(asString(row[2])),
		CreatedAt: time.Unix(asInt(row[3]), 0), UsedIP: asString(row[5])}
	if at := asInt(row[4]); at > 0 {
		p.UsedAt = time.Unix(at, 0)
	}
	return p
}

// CreateAppPassword stores an app password under a random id the caller
// made. hash is the SHA-256 of the secret. The account's auth_seq moves:
// from its first app password on, the account password no longer opens
// IMAP, and sessions that logged in with it are to end.
func (s *Store) CreateAppPassword(ctx context.Context, account, id, name, hash string, scopes []string) error {
	b := s.newBatch(account)
	b.guardAccount()
	b.guard(ErrAppPasswords, `(SELECT count(*) FROM app_password WHERE account_id = :a AND id <> :id) < :max`,
		"id", id, "max", MaxAppPasswords)
	// Id and hash are random: a row with them can only be this one.
	b.idempotent()
	b.add(`INSERT OR IGNORE INTO app_password (account_id, id, name, hash, scope, created_at) VALUES (:a, :id, :name, :hash, :scope, :now)`,
		"id", id, "name", name, "hash", hash, "scope", strings.Join(scopes, " "))
	b.add(`UPDATE account SET auth_seq = auth_seq + 1 WHERE id = :a`)
	if _, err := s.run(ctx, b); err != nil {
		return err
	}
	s.authChanged(account)
	return nil
}

// AppPasswords lists the app passwords of an account, the newest first.
func (s *Store) AppPasswords(ctx context.Context, account string) ([]AppPassword, error) {
	res, err := s.query(ctx, account, q(`SELECT `+appPasswordColumns+` FROM app_password WHERE account_id = :a ORDER BY created_at DESC, id`))
	if err != nil {
		return nil, err
	}
	out := make([]AppPassword, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, scanAppPassword(row))
	}
	return out, nil
}

// AppPasswordByHash finds the app password of an account with the given
// hash, or ErrNoAppPassword.
func (s *Store) AppPasswordByHash(ctx context.Context, account, hash string) (AppPassword, error) {
	res, err := s.query(ctx, account, q(`SELECT `+appPasswordColumns+` FROM app_password WHERE account_id = :a AND hash = :hash`, "hash", hash))
	if err != nil {
		return AppPassword{}, err
	}
	if len(res[0].Values) == 0 {
		return AppPassword{}, ErrNoAppPassword
	}
	return scanAppPassword(res[0].Values[0]), nil
}

// AppPassword returns one app password by id, or ErrNoAppPassword.
func (s *Store) AppPassword(ctx context.Context, account, id string) (AppPassword, error) {
	res, err := s.query(ctx, account, q(`SELECT `+appPasswordColumns+` FROM app_password WHERE account_id = :a AND id = :id`, "id", id))
	if err != nil {
		return AppPassword{}, err
	}
	if len(res[0].Values) == 0 {
		return AppPassword{}, ErrNoAppPassword
	}
	return scanAppPassword(res[0].Values[0]), nil
}

// UseAppPassword notes a login with an app password. Callers do this every
// few minutes per app password at most: it is a write.
func (s *Store) UseAppPassword(ctx context.Context, account, id, ip string) error {
	_, err := s.db.ExecuteIdempotent(ctx, rqlite.Stmt(
		`UPDATE app_password SET used_at = max(used_at, ?3), used_ip = ?4 WHERE account_id = ?1 AND id = ?2`,
		account, id, s.Now().Unix(), ip))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// RevokeAppPassword deletes an app password and reports whether there was
// one. The account's auth_seq moves, which ends the IMAP sessions that
// logged in with it, on every node.
func (s *Store) RevokeAppPassword(ctx context.Context, account, id string) (bool, error) {
	res, err := s.db.ExecuteIdempotent(ctx,
		rqlite.Stmt(`DELETE FROM app_password WHERE account_id = ?1 AND id = ?2`, account, id),
		rqlite.Stmt(sqlBumpAuthSeq, account))
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	s.authChanged(account)
	return res[0].RowsAffected > 0, nil
}

// SetPassword replaces the account's password hash and ends every session
// except the one with keepTokenHash (empty: all of them). With oldHash not
// empty the write only happens if that is still the stored hash
// (ErrPasswordChanged otherwise): the caller verified the current password
// against it.
func (s *Store) SetPassword(ctx context.Context, account, oldHash, newHash, keepTokenHash string) error {
	b := s.newBatch(account)
	b.guardAccount()
	if oldHash != "" {
		b.guard(ErrPasswordChanged, `(SELECT password_hash FROM account WHERE id = :a) = :old`, "old", oldHash)
	}
	b.add(`UPDATE account SET password_hash = :hash, password_changed_at = :now, auth_seq = auth_seq + 1 WHERE id = :a`, "hash", newHash)
	b.add(`DELETE FROM session WHERE account_id = :a AND token_hash <> :keep`, "keep", keepTokenHash)
	if _, err := s.run(ctx, b); err != nil {
		return err
	}
	s.authChanged(account)
	return nil
}

// TOTP is the state of an account's second factor.
type TOTP struct {
	Secret    []byte
	Confirmed bool
	CreatedAt time.Time
	// LastStep is the newest time step a code was accepted for.
	LastStep int64
	// RecoveryCodes is how many unused recovery codes are left.
	RecoveryCodes int
}

func totpAD(account string) []byte { return []byte("robinmail totp secret\x00" + account) }

const sealVersion = "s1:"

// seal encrypts a small secret of an account with the account's key, so
// that a copy of the database without the master key does not give it up.
func (s *Store) seal(ctx context.Context, account string, plain, ad []byte) (string, error) {
	key, err := s.blobKey(ctx, account)
	if err != nil {
		return "", err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", fmt.Errorf("store: %w", err)
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("store: %w", err)
	}
	return sealVersion + base64.RawStdEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, ad)), nil
}

func (s *Store) unseal(ctx context.Context, account, sealed string, ad []byte) ([]byte, error) {
	enc, ok := strings.CutPrefix(sealed, sealVersion)
	if !ok {
		return nil, errors.New("store: unknown format of a sealed secret")
	}
	data, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("store: sealed secret: %w", err)
	}
	key, err := s.blobKey(ctx, account)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if len(data) < aead.NonceSize() {
		return nil, errors.New("store: sealed secret is too short")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], ad)
	if err != nil {
		return nil, fmt.Errorf("store: sealed secret of account %s does not open: %w", account, err)
	}
	return plain, nil
}

// SetPendingTOTP stores a new secret that the user still has to confirm
// with a code. It replaces one that was never confirmed and fails with
// ErrTOTPEnabled when TOTP is on: that has to be turned off first.
func (s *Store) SetPendingTOTP(ctx context.Context, account string, secret []byte) error {
	sealed, err := s.seal(ctx, account, secret, totpAD(account))
	if err != nil {
		return err
	}
	b := s.newBatch(account)
	b.guardAccount()
	b.guard(ErrTOTPEnabled, `NOT EXISTS (SELECT 1 FROM totp WHERE account_id = :a AND confirmed_at > 0)`)
	b.add(`INSERT OR REPLACE INTO totp (account_id, secret, created_at, confirmed_at, last_step) VALUES (:a, :secret, :now, 0, 0)`, "secret", sealed)
	_, err = s.run(ctx, b)
	return err
}

// TOTPState returns the account's secret, confirmed or not, or ErrNoTOTP.
func (s *Store) TOTPState(ctx context.Context, account string) (TOTP, error) {
	res, err := s.query(ctx, account,
		q(`SELECT secret, created_at, confirmed_at, last_step FROM totp WHERE account_id = :a`),
		q(`SELECT count(*) FROM recovery_code WHERE account_id = :a`))
	if err != nil {
		return TOTP{}, err
	}
	if len(res[0].Values) == 0 {
		return TOTP{}, ErrNoTOTP
	}
	row := res[0].Values[0]
	secret, err := s.unseal(ctx, account, asString(row[0]), totpAD(account))
	if err != nil {
		return TOTP{}, err
	}
	return TOTP{Secret: secret, CreatedAt: time.Unix(asInt(row[1]), 0), Confirmed: asInt(row[2]) > 0, LastStep: asInt(row[3]),
		RecoveryCodes: int(asInt(res[1].Values[0][0]))}, nil
}

// ConfirmTOTP turns TOTP on: the user showed the code of time step step
// from the pending secret. The recovery codes, as hashes, are stored with
// it. ErrTOTPNotPending when there is no secret waiting.
func (s *Store) ConfirmTOTP(ctx context.Context, account string, step int64, recoveryHashes []string) error {
	b := s.newBatch(account)
	b.guard(ErrTOTPNotPending, `EXISTS (SELECT 1 FROM totp WHERE account_id = :a AND confirmed_at = 0)`)
	b.add(`UPDATE totp SET confirmed_at = :now, last_step = :step WHERE account_id = :a`, "step", step)
	b.replaceRecoveryCodes(recoveryHashes)
	_, err := s.run(ctx, b)
	return err
}

func (b *batch) replaceRecoveryCodes(hashes []string) {
	b.add(`DELETE FROM recovery_code WHERE account_id = :a`)
	for _, h := range hashes {
		b.add(`INSERT INTO recovery_code (account_id, hash) VALUES (:a, :h)`, "h", h)
	}
}

// ReplaceRecoveryCodes gives an account with TOTP a new set of recovery
// codes; the old ones stop working.
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, account string, hashes []string) error {
	b := s.newBatch(account)
	b.guard(ErrNoTOTP, `EXISTS (SELECT 1 FROM totp WHERE account_id = :a AND confirmed_at > 0)`)
	b.replaceRecoveryCodes(hashes)
	_, err := s.run(ctx, b)
	return err
}

// UseTOTPStep claims a time step for the account and reports whether this
// call got it. A step can be claimed once, and none older than the newest
// one claimed: that is what keeps a code from working twice, on whatever
// node it is tried (RFC 6238 5.2).
func (s *Store) UseTOTPStep(ctx context.Context, account string, step int64) (bool, error) {
	res, err := s.db.Execute(ctx, rqlite.Stmt(
		`UPDATE totp SET last_step = ?2 WHERE account_id = ?1 AND confirmed_at > 0 AND last_step < ?2`, account, step))
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected > 0, nil
}

// UseRecoveryCode spends a recovery code and reports whether this call got
// it: the row is deleted, so a code works once.
func (s *Store) UseRecoveryCode(ctx context.Context, account, hash string) (bool, error) {
	res, err := s.db.Execute(ctx, rqlite.Stmt(`DELETE FROM recovery_code WHERE account_id = ?1 AND hash = ?2`, account, hash))
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected > 0, nil
}

// DisableTOTP removes the account's secret, pending or confirmed, and its
// recovery codes, and reports whether TOTP was on.
func (s *Store) DisableTOTP(ctx context.Context, account string) (bool, error) {
	res, err := s.db.ExecuteIdempotent(ctx,
		rqlite.Stmt(`DELETE FROM totp WHERE account_id = ?1 AND confirmed_at > 0`, account),
		rqlite.Stmt(`DELETE FROM totp WHERE account_id = ?1`, account),
		rqlite.Stmt(`DELETE FROM recovery_code WHERE account_id = ?1`, account),
		// Logins that wait for a code that no longer exists.
		rqlite.Stmt(`DELETE FROM session WHERE account_id = ?1 AND stage = 'totp'`, account))
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected > 0, nil
}

// LinkOIDC records the subject under which a provider knows the account,
// at the first login through that provider. A later login with another
// subject fails with ErrOIDCSubject.
func (s *Store) LinkOIDC(ctx context.Context, account, provider, subject string) error {
	b := s.newBatch(account)
	b.guardAccount()
	// The insert does nothing the second time and the guard reads.
	b.idempotent()
	b.add(`INSERT OR IGNORE INTO oidc_link (account_id, provider, subject, linked_at) VALUES (:a, :provider, :subject, :now)`,
		"provider", provider, "subject", subject)
	b.guard(ErrOIDCSubject, `(SELECT subject FROM oidc_link WHERE account_id = :a AND provider = :provider) = :subject`,
		"provider", provider, "subject", subject)
	_, err := s.run(ctx, b)
	return err
}

// UnlinkOIDC forgets the subjects of an account, for all providers, and
// returns how many there were. The next login through a provider links
// again.
func (s *Store) UnlinkOIDC(ctx context.Context, account string) (int64, error) {
	res, err := s.db.ExecuteIdempotent(ctx, rqlite.Stmt(`DELETE FROM oidc_link WHERE account_id = ?`, account))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected, nil
}

// AuthEvent is one line of the log of auth events.
type AuthEvent struct {
	ID int64
	At time.Time
	// AccountID is empty when the address is nobody's.
	AccountID string
	Address   string
	// Event names what happened, such as "login", "login-failed",
	// "app-password-created". The list is at internal/web's audit.go.
	Event  string
	IP     string
	Detail string
}

// Limits on what one event stores.
const (
	maxEventAddress = 320
	maxEventDetail  = 300
)

func cut(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	return strings.ToValidUTF8(s, "")
}

// AddAuthEvent appends to the log of auth events. At is set to now.
func (s *Store) AddAuthEvent(ctx context.Context, e AuthEvent) error {
	metrics.AuthEvents.Inc(e.Event)
	_, err := s.db.Execute(ctx, rqlite.Stmt(
		`INSERT INTO auth_event (at, account_id, address, event, ip, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		s.Now().Unix(), e.AccountID, cut(e.Address, maxEventAddress), e.Event, cut(e.IP, 64), cut(e.Detail, maxEventDetail)))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// AuthEvents returns the newest events, at most limit, newest first: of
// one account, or of all with account empty.
func (s *Store) AuthEvents(ctx context.Context, account string, limit int) ([]AuthEvent, error) {
	where := ``
	if account != "" {
		where = `WHERE account_id = :a`
	}
	res, err := s.query(ctx, account, q(`SELECT id, at, account_id, address, event, ip, detail FROM auth_event `+where+`
		ORDER BY id DESC LIMIT :limit`, "limit", limit))
	if err != nil {
		return nil, err
	}
	out := make([]AuthEvent, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, AuthEvent{ID: asInt(row[0]), At: time.Unix(asInt(row[1]), 0), AccountID: asString(row[2]),
			Address: asString(row[3]), Event: asString(row[4]), IP: asString(row[5]), Detail: asString(row[6])})
	}
	return out, nil
}

// TrimAuthEvents removes the events from before the given time and returns
// how many.
func (s *Store) TrimAuthEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecuteIdempotent(ctx, rqlite.Stmt(`DELETE FROM auth_event WHERE at < ?`, before.Unix()))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected, nil
}
