package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/store"
	"github.com/mikepage/robinmail/internal/users"
	"github.com/mikepage/robinmail/internal/web"
)

// The administrator's side of authentication: what somebody with a shell
// on a node and its config can do for a user who is locked out, or to
// one who should be. Each command works on the database, so it holds for
// every node; a running server ends the sessions and IMAP connections
// concerned within a second, because it follows the accounts' auth_seq.
// Everything done here is written to the log of auth events, with
// "by an administrator" in the detail and no IP.

const byAdmin = "by an administrator"

func (a *app) adminEvent(acct store.Account, event, detail string) {
	if detail != "" {
		detail += ", "
	}
	if err := a.store.AddAuthEvent(a.ctx, store.AuthEvent{AccountID: acct.ID, Address: acct.Address, Event: event, Detail: detail + byAdmin}); err != nil {
		fmt.Fprintf(a.stdout, "warning: the auth event was not written: %v\n", err)
	}
}

// userPassword sets a new password for a user. Every session of the user
// ends. App passwords and TOTP stay as they are.
func (a *app) userPassword(s string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	password, err := a.readPassword()
	if err != nil {
		return err
	}
	if _, isApp := users.NormalizeAppPassword(password); isApp {
		return errors.New("that has the form of an app password; choose another")
	}
	hash, err := users.Hash(password)
	if err != nil {
		return err
	}
	if err := a.store.SetPassword(a.ctx, acct.ID, "", hash, ""); err != nil {
		return err
	}
	a.adminEvent(acct, web.EventPasswordChanged, "reset, all sessions ended")
	fmt.Fprintf(a.stdout, "set a new password for %s and ended its sessions\n", acct.Address)
	if acct.TOTP {
		fmt.Fprintf(a.stdout, "the login still asks for a TOTP code; \"robinmail user totp-disable %s\" turns that off\n", acct.Address)
	}
	return nil
}

// userTOTPDisable turns TOTP off for a user who lost the authenticator
// and the recovery codes.
func (a *app) userTOTPDisable(s string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	was, err := a.store.DisableTOTP(a.ctx, acct.ID)
	if err != nil {
		return err
	}
	if !was {
		fmt.Fprintf(a.stdout, "%s has no TOTP\n", acct.Address)
		return nil
	}
	a.adminEvent(acct, web.EventTOTPDisabled, "")
	fmt.Fprintf(a.stdout, "turned TOTP off for %s; the login is the password alone until the user sets it up again\n", acct.Address)
	return nil
}

func when(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func (a *app) userAppPasswords(s string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	list, err := a.store.AppPasswords(a.ctx, acct.ID)
	if err != nil {
		return err
	}
	for _, p := range list {
		scope := "all"
		if len(p.Scopes) > 0 {
			scope = strings.Join(p.Scopes, ",")
		}
		used := "never used"
		if !p.UsedAt.IsZero() {
			used = "last used " + when(p.UsedAt) + " from " + p.UsedIP
		}
		fmt.Fprintf(a.stdout, "%-14s  %-24s  %-14s  created %s  %s\n", p.ID, clip(p.Name, 24), scope, when(p.CreatedAt), used)
	}
	if len(list) == 0 {
		fmt.Fprintf(a.stdout, "%s has no app passwords\n", acct.Address)
	}
	return nil
}

func (a *app) userAppPasswordRevoke(s, id string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	p, err := a.store.AppPassword(a.ctx, acct.ID, id)
	if errors.Is(err, store.ErrNoAppPassword) {
		return fmt.Errorf("%s has no app password %q", acct.Address, id)
	}
	if err != nil {
		return err
	}
	if _, err := a.store.RevokeAppPassword(a.ctx, acct.ID, id); err != nil {
		return err
	}
	a.adminEvent(acct, web.EventAppPasswordRevoked, p.Name)
	fmt.Fprintf(a.stdout, "revoked app password %s (%s) of %s\n", id, p.Name, acct.Address)
	return nil
}

func (a *app) userSessions(s string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	list, err := a.store.Sessions(a.ctx, acct.ID)
	if err != nil {
		return err
	}
	for _, x := range list {
		fmt.Fprintf(a.stdout, "%-14s  %-20s  created %s from %-15s  last seen %s from %-15s  %s\n",
			x.ID, x.Method, when(x.CreatedAt), x.IP, when(x.SeenAt), x.SeenIP, clip(x.Agent, 60))
	}
	if len(list) == 0 {
		fmt.Fprintf(a.stdout, "%s has no sessions\n", acct.Address)
	}
	return nil
}

func (a *app) userSessionRevoke(s, id string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	if id == "all" {
		n, err := a.store.RevokeSessions(a.ctx, acct.ID, "")
		if err != nil {
			return err
		}
		a.adminEvent(acct, web.EventSessionRevoked, "all sessions")
		fmt.Fprintf(a.stdout, "ended %d sessions of %s\n", n, acct.Address)
		return nil
	}
	found, err := a.store.RevokeSession(a.ctx, acct.ID, id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s has no session %q", acct.Address, id)
	}
	a.adminEvent(acct, web.EventSessionRevoked, "session "+id)
	fmt.Fprintf(a.stdout, "ended session %s of %s\n", id, acct.Address)
	return nil
}

// userOIDCUnlink forgets under which subject the providers know a user,
// so that the next login through a provider records it anew. For a user
// whose identity at the provider was made again.
func (a *app) userOIDCUnlink(s string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	n, err := a.store.UnlinkOIDC(a.ctx, acct.ID)
	if err != nil {
		return err
	}
	a.adminEvent(acct, "oidc-unlinked", "")
	fmt.Fprintf(a.stdout, "forgot %d provider subjects of %s\n", n, acct.Address)
	return nil
}

// audit prints the newest auth events, oldest first: of all users, or of
// one. who is an address, or "" or "all".
func (a *app) audit(who, countArg string) error {
	count, err := strconv.Atoi(countArg)
	if err != nil || count < 1 || count > 10000 {
		return fmt.Errorf("%q is not a number of events from 1 to 10000", countArg)
	}
	account := ""
	if who != "" && who != "all" {
		acct, err := a.account(who)
		if err != nil {
			return err
		}
		account = acct.ID
	}
	list, err := a.store.AuthEvents(a.ctx, account, count)
	if err != nil {
		return err
	}
	for i := len(list) - 1; i >= 0; i-- {
		e := list[i]
		ip := e.IP
		if ip == "" {
			ip = "-"
		}
		fmt.Fprintf(a.stdout, "%s  %-24s  %-36s  %-15s  %s\n", e.At.Local().Format("2006-01-02 15:04:05"), e.Event, clip(e.Address, 36), ip, e.Detail)
	}
	return nil
}
