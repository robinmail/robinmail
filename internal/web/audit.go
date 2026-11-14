package web

import (
	"context"

	"github.com/mikepage/robinmail/internal/store"
)

// Events in the log of auth events (the auth_event table). The detail of
// a login names the door ("web", "imap", "smtp", "jmap") and how.
const (
	EventLogin              = "login"
	EventLoginFailed        = "login-failed"
	EventCodeFailed         = "code-failed"
	EventRecoveryUsed       = "recovery-code-used"
	EventPasswordChanged    = "password-changed"
	EventAppPasswordCreated = "app-password-created"
	EventAppPasswordRevoked = "app-password-revoked"
	EventTOTPEnabled        = "totp-enabled"
	EventTOTPDisabled       = "totp-disabled"
	EventRecoveryReplaced   = "recovery-codes-replaced"
	EventSessionRevoked     = "session-revoked"
	EventOIDCRefused        = "oidc-refused"
	EventAccountCreated     = "account-created"
)

// event writes to the log of auth events. A failure to write is logged
// and does not fail what was being done: the log is a record, not a gate.
//
// What is written, and how often: every login at the web, every failed
// login at any door (which the login limits bound: ten per address and
// fifty per IP in fifteen minutes, after which nothing is checked and
// nothing written), and every change to passwords, app passwords, TOTP
// and sessions. A login of a mail client with an app password is written
// when its "last used" is, every few minutes at most.
func (s *Server) event(ctx context.Context, event string, acct store.Account, address, ip, detail string) {
	if s.Audit == nil {
		return
	}
	if acct.Address != "" {
		address = acct.Address
	}
	err := s.Audit.AddAuthEvent(ctx, store.AuthEvent{AccountID: acct.ID, Address: address, Event: event, IP: ip, Detail: detail})
	if err != nil {
		s.Logger.Warn("writing the auth event failed", "event", event, "address", address, "err", err)
	}
}
