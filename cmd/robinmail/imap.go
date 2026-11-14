package main

import (
	"context"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/mikepage/robinmail/internal/imap"
	"github.com/mikepage/robinmail/internal/notify"
	"github.com/mikepage/robinmail/internal/store"
	"github.com/mikepage/robinmail/internal/web"
)

// imapBackend puts the store behind the interfaces of internal/imap.
//
// How the store's model shows in IMAP. A mailbox is a label and an Email
// can have several: it is then a message in each of those folders, with a
// UID of its own in each, and its flags are the same in all of them,
// because keywords belong to the Email. COPY adds a label, MOVE swaps one,
// EXPUNGE takes one away and destroys the Email when it was the last. A
// login is an app password, or the account password while the account may
// use that here, checked by the same code as SMTP AUTH and JMAP's Basic
// auth (web.Server.MailLogin), so they share the failure counters.
type imapBackend struct {
	st  *store.Store
	hub *notify.Hub
	// auth follows the accounts' auth_seq: it wakes a session when a way
	// into its account was withdrawn, on any node.
	auth  *notify.Hub
	login func(ctx context.Context, ip, scope, username, password string) (web.Credential, int, time.Duration)
	valid func(ctx context.Context, c web.Credential, scope string) (bool, error)
	// maxMessage is the largest message APPEND stores.
	maxMessage int64
}

func (b *imapBackend) Login(ctx context.Context, ip, username, password string) (imap.User, error) {
	cred, status, _ := b.login(ctx, ip, store.ScopeIMAP, username, password)
	switch status {
	case 0:
		return &imapUser{b: b, acct: cred.Account, cred: cred}, nil
	case http.StatusUnauthorized:
		return nil, imap.ErrAuth
	case http.StatusTooManyRequests:
		return nil, imap.ErrLimited
	}
	return nil, errors.New("the login cannot be checked right now")
}

type imapUser struct {
	b    *imapBackend
	acct store.Account
	cred web.Credential
}

// Revoked implements imap.Revocable: the channel is closed once the app
// password the session logged in with is gone, or, for a login with the
// account password, once that changed or stopped being accepted for IMAP.
func (u *imapUser) Revoked() (<-chan struct{}, func()) {
	return watchLogin(u.b.auth, u.b.valid, u.cred, store.ScopeIMAP)
}

// watchLogin returns a channel that is closed once a login of a mail
// client no longer stands for the scope, and a function that ends the
// watching. The login is looked at when the account's auth_seq moved,
// which this node hears at once for its own writes and within a second
// for another node's. While the store cannot say, the login stays.
func watchLogin(auth *notify.Hub, valid func(ctx context.Context, c web.Credential, scope string) (bool, error),
	cred web.Credential, scope string) (<-chan struct{}, func()) {
	revoked := make(chan struct{})
	changes, unsubscribe := auth.Subscribe(cred.Account.ID)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case <-changes:
			}
			cctx, done := context.WithTimeout(ctx, 10*time.Second)
			ok, err := valid(cctx, cred, scope)
			done()
			if err == nil && !ok {
				close(revoked)
				return
			}
		}
	}()
	return revoked, cancel
}

// imapErr gives a store error the meaning internal/imap knows it by.
func imapErr(err error) error {
	for _, m := range []struct{ from, to error }{
		{store.ErrNoMailbox, imap.ErrNoMailbox}, {store.ErrMailboxExists, imap.ErrExists}, {store.ErrMailboxName, imap.ErrName},
		{store.ErrMailboxHasChild, imap.ErrHasChildren}, {store.ErrMailboxRole, imap.ErrSpecial}, {store.ErrMailboxLoop, imap.ErrLoop},
		{store.ErrKeyword, imap.ErrFlag}, {store.ErrNoEmail, imap.ErrNoMessage}, {store.ErrNoBlob, imap.ErrNoMessage},
	} {
		if errors.Is(err, m.from) {
			return m.to
		}
	}
	return err
}

func (u *imapUser) Username() string { return u.acct.Address }

func (u *imapUser) Mailboxes(ctx context.Context) ([]imap.Mailbox, error) {
	list, err := u.b.st.MailboxStatuses(ctx, u.acct.ID)
	if err != nil {
		return nil, imapErr(err)
	}
	out := make([]imap.Mailbox, len(list))
	for i, m := range list {
		out[i] = imap.Mailbox{ID: m.ID, ParentID: m.ParentID, Name: m.Name, Role: m.Role, Subscribed: m.Subscribed,
			UIDValidity: m.UIDValidity, UIDNext: m.UIDNext, Messages: m.Total, Unseen: m.Unread, Deleted: m.Deleted,
			Size: m.Size, HighestModseq: m.HighestModseq}
	}
	return out, nil
}

func (u *imapUser) Create(ctx context.Context, parent int64, name string) (int64, error) {
	// Not subscribed: in IMAP a client subscribes to what it wants in its
	// list (RFC 3501 6.3.6); JMAP's default for a new mailbox is the other.
	m, err := u.b.st.NewMailbox(ctx, u.acct.ID, store.NewMailbox{ParentID: parent, Name: name, SortOrder: 10})
	return m.ID, imapErr(err)
}

func (u *imapUser) Rename(ctx context.Context, id, parent int64, name string) error {
	_, err := u.b.st.UpdateMailbox(ctx, u.acct.ID, id, store.MailboxUpdate{Name: &name, ParentID: &parent})
	return imapErr(err)
}

// Delete removes the mailbox. An email that is in another mailbox too
// only loses this label; one that is not is destroyed (as JMAP's
// onDestroyRemoveEmails).
func (u *imapUser) Delete(ctx context.Context, id int64) error {
	_, err := u.b.st.DestroyMailbox(ctx, u.acct.ID, id, true, 0)
	return imapErr(err)
}

func (u *imapUser) Subscribe(ctx context.Context, id int64, subscribed bool) error {
	_, err := u.b.st.UpdateMailbox(ctx, u.acct.ID, id, store.MailboxUpdate{Subscribed: &subscribed})
	return imapErr(err)
}

func (u *imapUser) View(ctx context.Context, mailbox, since int64) (imap.View, error) {
	v, err := u.b.st.ViewMailbox(ctx, u.acct.ID, mailbox, since)
	if err != nil {
		return imap.View{}, imapErr(err)
	}
	out := imap.View{UIDValidity: v.UIDValidity, UIDNext: v.UIDNext, Modseq: v.Modseq, UIDs: v.UIDs,
		Changed: make([]imap.Message, len(v.Changed))}
	for i, m := range v.Changed {
		out.Changed[i] = imap.Message{UID: m.UID, ID: m.EmailID, Modseq: m.Modseq, Size: m.Size,
			InternalDate: m.ReceivedAt, Flags: m.Keywords}
	}
	return out, nil
}

func (u *imapUser) Open(ctx context.Context, id int64) ([]byte, error) {
	raw, _, err := u.b.st.Raw(ctx, u.acct.ID, id)
	return raw, imapErr(err)
}

// headerCap is how many characters of a header field the store keeps
// (store.summarize). A field of that length may have been cut.
const headerCap = 250

func (u *imapUser) Summaries(ctx context.Context, ids []int64) (map[int64]imap.Summary, error) {
	emails, err := u.b.st.EmailsByID(ctx, u.acct.ID, ids)
	if err != nil {
		return nil, imapErr(err)
	}
	out := make(map[int64]imap.Summary, len(emails))
	for _, e := range emails {
		s := imap.Summary{From: e.Sender, To: e.Recipients, Cc: e.Cc, Bcc: e.Bcc, Subject: e.Subject, SentAt: e.SentAt}
		for _, f := range []string{s.From, s.To, s.Cc, s.Bcc, s.Subject} {
			if utf8.RuneCountInString(f) >= headerCap {
				s.Partial = true
			}
		}
		out[e.ID] = s
	}
	return out, nil
}

// SearchText makes imapUser an imap.TextSearcher: SEARCH BODY and TEXT are
// answered by the full-text index.
func (u *imapUser) SearchText(ctx context.Context, mailbox int64, text string, bodyOnly bool) ([]int64, bool, error) {
	ids, indexed, err := u.b.st.SearchMailbox(ctx, u.acct.ID, mailbox, text, bodyOnly)
	return ids, indexed, imapErr(err)
}

func (u *imapUser) Append(ctx context.Context, mailbox int64, raw []byte, flags []string, date time.Time) (uint32, uint32, error) {
	if u.b.maxMessage > 0 && int64(len(raw)) > u.b.maxMessage {
		return 0, 0, imap.ErrTooLarge
	}
	e, err := u.b.st.Deliver(ctx, u.acct.ID, store.Message{Raw: raw, MailboxID: mailbox, Keywords: flags, ReceivedAt: date})
	if err != nil {
		return 0, 0, imapErr(err)
	}
	validity, err := u.b.st.MailboxUIDValidity(ctx, u.acct.ID, mailbox)
	return e.Mailboxes[mailbox], validity, imapErr(err)
}

func storeRanges(uids []imap.UIDRange) []store.UIDRange {
	if uids == nil {
		return nil
	}
	out := make([]store.UIDRange, len(uids))
	for i, r := range uids {
		out[i] = store.UIDRange{Lo: r.Lo, Hi: r.Hi}
	}
	return out
}

func (u *imapUser) Store(ctx context.Context, mailbox int64, uids []imap.UIDRange, mode int, flags []string, unchangedSince int64) ([]uint32, error) {
	storeMode := map[int]int{imap.FlagsAdd: store.FlagsAdd, imap.FlagsRemove: store.FlagsRemove, imap.FlagsReplace: store.FlagsReplace}[mode]
	_, modified, err := u.b.st.SetFlags(ctx, u.acct.ID, store.FlagUpdate{Mailbox: mailbox, UIDs: storeRanges(uids),
		Mode: storeMode, Keywords: flags, UnchangedSince: unchangedSince})
	return modified, imapErr(err)
}

func (u *imapUser) Copy(ctx context.Context, from int64, uids []imap.UIDRange, to int64, move bool) (uint32, []imap.UIDPair, error) {
	r, err := u.b.st.CopyMessages(ctx, u.acct.ID, from, storeRanges(uids), to, move)
	if err != nil {
		return 0, nil, imapErr(err)
	}
	pairs := make([]imap.UIDPair, len(r.Pairs))
	for i, p := range r.Pairs {
		pairs[i] = imap.UIDPair{From: p.From, To: p.To}
	}
	return r.UIDValidity, pairs, nil
}

func (u *imapUser) Expunge(ctx context.Context, mailbox int64, uids []imap.UIDRange) error {
	_, err := u.b.st.Expunge(ctx, u.acct.ID, mailbox, storeRanges(uids))
	return imapErr(err)
}

func (u *imapUser) Changes() (<-chan struct{}, func()) { return u.b.hub.Subscribe(u.acct.ID) }
