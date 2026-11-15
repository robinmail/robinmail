package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/robinmail/robinmail/internal/pim"
	"github.com/robinmail/robinmail/internal/store"
)

// Calendars, contacts and principals (phase 9): JMAP for Calendars
// (draft-ietf-jmap-calendars-33), JMAP for Contacts (RFC 9610), and
// Principals and sharing (RFC 9670). Unlike mail, these can live in another
// user's account: a user whose calendar or address book is shared with
// someone gives that someone a second account in their session, holding
// what is shared and nothing else.

// Capability URIs of phase 9.
const (
	CapCalendars        = "urn:ietf:params:jmap:calendars"
	CapCalendarsParse   = "urn:ietf:params:jmap:calendars:parse"
	CapContacts         = "urn:ietf:params:jmap:contacts"
	CapPrincipals       = "urn:ietf:params:jmap:principals"
	CapPrincipalsOwner  = "urn:ietf:params:jmap:principals:owner"
	CapPrincipalsAvail  = "urn:ietf:params:jmap:principals:availability"
	maxExpandedDuration = "P400D"
	maxAvailability     = "P366D"
)

func init() {
	for _, c := range []string{CapCalendars, CapCalendarsParse, CapContacts, CapPrincipals, CapPrincipalsOwner, CapPrincipalsAvail} {
		capabilities[c] = true
	}
}

// account is the user as the pim package wants them.
func (c *call) account() store.Account {
	return store.Account{ID: c.user.AccountID, Address: c.user.Username}
}

// pimCtx is an account of calendars or address books as one request sees
// it: the user's own, or another's that shares with them.
type pimCtx struct {
	owner store.Account
	own   bool
	// colls are the collections of the kind the user sees in the account.
	colls []pim.Access
}

func (p *pimCtx) byID(id int64) (pim.Access, bool) {
	for _, a := range p.colls {
		if a.Collection.ID == id {
			return a, true
		}
	}
	return pim.Access{}, false
}

func (p *pimCtx) ids() []int64 {
	out := make([]int64, 0, len(p.colls))
	for _, a := range p.colls {
		out = append(out, a.Collection.ID)
	}
	return out
}

// open decodes the arguments of a calendar or contacts method and finds
// the account: the user's own, or one that shares a collection of the kind
// with them.
func (c *call) open(raw json.RawMessage, v any, account *string, kind string) (*pimCtx, *methodError) {
	if c.h.PIM == nil {
		return nil, mErr("unknownMethod", "calendars and contacts are off")
	}
	if err := decodeStrict(raw, v); err != nil {
		return nil, invalidArgs("%s", argError(err))
	}
	if *account == "" {
		return nil, invalidArgs("accountId is required")
	}
	return c.pimAccount(*account, kind)
}

func (c *call) pimAccount(id, kind string) (*pimCtx, *methodError) {
	user := c.account()
	if id == user.ID {
		all, err := c.h.PIM.Collections(c.ctx, user, kind)
		if err != nil {
			return nil, storeErr(err)
		}
		p := &pimCtx{owner: user, own: true}
		for _, a := range all {
			if a.Owner == user.ID {
				p.colls = append(p.colls, a)
			}
		}
		return p, nil
	}
	shared, err := c.h.PIM.SharedCollections(c.ctx, user, id, kind)
	if err != nil {
		return nil, storeErr(err)
	}
	if len(shared) == 0 {
		return nil, mErr("accountNotFound", "no such account")
	}
	return &pimCtx{owner: store.Account{ID: id, Address: shared[0].OwnerAddress}, colls: shared}, nil
}

// sharedAccounts lists the accounts that share something with the user:
// owner id -> address and the kinds shared, and whether any is writable.
type sharedAccount struct {
	address  string
	kinds    map[string]bool
	readOnly bool
}

func (h *Handler) sharedAccounts(ctx context.Context, u User) map[string]*sharedAccount {
	if h.PIM == nil {
		return nil
	}
	list, err := h.PIM.Store.SharedWith(ctx, u.AccountID)
	if err != nil {
		return nil
	}
	out := map[string]*sharedAccount{}
	for _, s := range list {
		a := out[s.Owner]
		if a == nil {
			a = &sharedAccount{address: s.OwnerAddress, kinds: map[string]bool{}, readOnly: true}
			out[s.Owner] = a
		}
		a.kinds[s.Kind] = true
		if s.Rights == store.RightWrite {
			a.readOnly = false
		}
	}
	return out
}

// pimCapabilities adds what phase 9 puts in the session object.
func (h *Handler) pimCapabilities(ctx context.Context, u User, caps map[string]any, accounts map[string]any, primary map[string]any) {
	if h.PIM == nil {
		return
	}
	for _, c := range []string{CapCalendars, CapCalendarsParse, CapContacts, CapPrincipals, CapPrincipalsAvail} {
		caps[c] = map[string]any{}
	}
	calendarCap := map[string]any{"maxCalendarsPerEvent": 1, "minDateTime": "1900-01-01T00:00:00Z", "maxDateTime": "2200-01-01T00:00:00Z",
		"maxExpandedQueryDuration": maxExpandedDuration, "maxParticipantsPerEvent": nil, "mayCreateCalendar": true}
	own := accounts[u.AccountID].(map[string]any)["accountCapabilities"].(map[string]any)
	own[CapCalendars] = calendarCap
	own[CapCalendarsParse] = map[string]any{}
	own[CapContacts] = map[string]any{"maxAddressBooksPerCard": 1, "mayCreateAddressBook": true}
	own[CapPrincipals] = map[string]any{"currentUserPrincipalId": u.AccountID}
	own[CapPrincipalsOwner] = map[string]any{"accountIdForPrincipal": u.AccountID, "principalId": u.AccountID}
	own[CapPrincipalsAvail] = map[string]any{"maxAvailabilityDuration": maxAvailability}
	for _, c := range []string{CapCalendars, CapContacts, CapPrincipals} {
		primary[c] = u.AccountID
	}
	for id, s := range h.sharedAccounts(ctx, u) {
		ac := map[string]any{CapPrincipalsOwner: map[string]any{"accountIdForPrincipal": id, "principalId": id}}
		if s.kinds[store.KindCalendar] {
			cc := map[string]any{}
			for k, v := range calendarCap {
				cc[k] = v
			}
			cc["mayCreateCalendar"] = false
			ac[CapCalendars] = cc
		}
		if s.kinds[store.KindAddressBook] {
			ac[CapContacts] = map[string]any{"maxAddressBooksPerCard": 1, "mayCreateAddressBook": false}
		}
		accounts[id] = map[string]any{"name": s.address, "isPersonal": false, "isReadOnly": s.readOnly, "accountCapabilities": ac}
	}
}

// sharedState is part of the session state: it changes when what is
// shared with the user does.
func (h *Handler) sharedState(ctx context.Context, u User) []string {
	var out []string
	for id, s := range h.sharedAccounts(ctx, u) {
		kinds := make([]string, 0, len(s.kinds))
		for k := range s.kinds {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		out = append(out, id+":"+strings.Join(kinds, ",")+":"+map[bool]string{true: "r", false: "w"}[s.readOnly])
	}
	sort.Strings(out)
	return out
}

// pimState is the state of a type in an account.
func (c *call) pimState(p *pimCtx) (int64, *methodError) {
	st, err := c.h.Store.State(c.ctx, p.owner.ID)
	if err != nil {
		return 0, storeErr(err)
	}
	return st, nil
}

// pimChanges answers a /changes of CalendarEvent or ContactCard: from the
// change log in the user's own account; from the collections shared with
// the user in another's, where an object counts as created when it was
// made after the state, and as destroyed when it left those collections.
func (c *call) pimChanges(raw json.RawMessage, typ, kind string, format func(int64) string) (any, *methodError) {
	var args changesArgs
	p, e := c.open(raw, &args, &args.AccountID, kind)
	if e != nil {
		return nil, e
	}
	if p.own {
		return c.changes(raw, typ, format)
	}
	if args.SinceState == nil {
		return nil, invalidArgs("sinceState is required")
	}
	since, ok := parseState(*args.SinceState)
	if !ok {
		return nil, mErr("cannotCalculateChanges", "the server cannot calculate the changes from that state")
	}
	if epoch, err := c.h.PIM.Store.ShareEpoch(c.ctx, p.owner.ID, c.user.AccountID); err != nil {
		return nil, storeErr(err)
	} else if epoch > since {
		return nil, mErr("cannotCalculateChanges", "what is shared with you changed since that state")
	}
	ch, err := c.h.PIM.Store.CollectionChanges(c.ctx, p.owner.ID, p.ids(), since)
	if errors.Is(err, store.ErrCannotCalculateChanges) {
		return nil, mErr("cannotCalculateChanges", "the server cannot calculate the changes from that state")
	}
	if err != nil {
		return nil, storeErr(err)
	}
	created, updated, destroyed := []string{}, []string{}, []string{}
	for _, o := range ch.Changed {
		if o.Created > since {
			created = append(created, format(o.ID))
		} else {
			updated = append(updated, format(o.ID))
		}
	}
	for _, id := range ch.RemovedIDs {
		destroyed = append(destroyed, format(id))
	}
	return map[string]any{"accountId": args.AccountID, "oldState": *args.SinceState, "newState": stateString(ch.State),
		"hasMoreChanges": false, "created": created, "updated": updated, "destroyed": destroyed}, nil
}

// collectionChanges answers Calendar/changes and AddressBook/changes.
func (c *call) collectionChanges(raw json.RawMessage, typ, kind string, format func(int64) string) (any, *methodError) {
	var args changesArgs
	p, e := c.open(raw, &args, &args.AccountID, kind)
	if e != nil {
		return nil, e
	}
	if p.own {
		out, e := c.changes(raw, typ, format)
		if e == nil {
			out["updatedProperties"] = nil
		}
		return out, e
	}
	if args.SinceState == nil {
		return nil, invalidArgs("sinceState is required")
	}
	since, ok := parseState(*args.SinceState)
	if !ok {
		return nil, mErr("cannotCalculateChanges", "the server cannot calculate the changes from that state")
	}
	epoch, err := c.h.PIM.Store.ShareEpoch(c.ctx, p.owner.ID, c.user.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	if epoch > since {
		return nil, mErr("cannotCalculateChanges", "what is shared with you changed since that state")
	}
	state, me := c.pimState(p)
	if me != nil {
		return nil, me
	}
	updated := []string{}
	for _, a := range p.colls {
		if a.Collection.Modseq > since {
			updated = append(updated, format(a.Collection.ID))
		}
	}
	return map[string]any{"accountId": args.AccountID, "oldState": *args.SinceState, "newState": stateString(state),
		"hasMoreChanges": false, "created": []string{}, "updated": updated, "destroyed": []string{}, "updatedProperties": nil}, nil
}

// shareRights reads a shareWith value (JMAP Calendars 4, RFC 9610 2):
// principal id -> rights object. Read and write are what robinmail keeps;
// any right to write gives write, any right to read gives read.
func shareRights(raw json.RawMessage) (map[string]string, bool) {
	if string(raw) == "null" {
		return map[string]string{}, true
	}
	var m map[string]map[string]bool
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	out := map[string]string{}
	for id, r := range m {
		switch {
		case r["mayWriteAll"] || r["mayWrite"] || r["mayWriteOwn"]:
			out[id] = store.RightWrite
		case r["mayReadItems"] || r["mayRead"] || r["mayReadFreeBusy"]:
			out[id] = store.RightRead
		}
	}
	return out, true
}

// applyShares makes the shares of a collection what the client asked.
func (c *call) applyShares(owner string, collection int64, want map[string]string) error {
	have, err := c.h.PIM.Store.SharesOf(c.ctx, owner, collection)
	if err != nil {
		return err
	}
	cur := map[string]string{}
	for _, s := range have {
		cur[s.Sharee] = s.Rights
	}
	for id, r := range want {
		if cur[id] != r {
			if err := c.h.PIM.Store.SetShare(c.ctx, owner, collection, id, r); err != nil {
				return err
			}
		}
	}
	for id := range cur {
		if _, ok := want[id]; !ok {
			if err := c.h.PIM.Store.SetShare(c.ctx, owner, collection, id, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// sharesOf returns the shareWith of a collection, for its owner.
func (c *call) sharesOf(owner string, collection int64, rights func(string) map[string]bool) any {
	list, err := c.h.PIM.Store.SharesOf(c.ctx, owner, collection)
	if err != nil || len(list) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, s := range list {
		out[s.Sharee] = rights(s.Rights)
	}
	return out
}

// pimSetError maps what the store and pim refuse to a SetError.
func pimSetError(err error) *setError {
	var ie *pim.InvalidError
	switch {
	case errors.As(err, &ie):
		return invalidProps(ie.Msg)
	case errors.Is(err, store.ErrNoObject), errors.Is(err, store.ErrNoCollection), errors.Is(err, pim.ErrNotFound):
		return &setError{Type: "notFound"}
	case errors.Is(err, pim.ErrForbidden):
		return &setError{Type: "forbidden", Description: "you may not change this"}
	case errors.Is(err, store.ErrUIDConflict):
		return &setError{Type: "alreadyExists", Description: "an object with this uid is there already"}
	case errors.Is(err, store.ErrOverQuota):
		return &setError{Type: "overQuota"}
	case errors.Is(err, store.ErrObjectTooLarge):
		return &setError{Type: "tooLarge"}
	case errors.Is(err, store.ErrCollectionInUse):
		return &setError{Type: "calendarHasEvent", Description: "it is not empty"}
	case errors.Is(err, store.ErrDefaultCollection):
		return &setError{Type: "forbidden", Description: "the default one cannot be destroyed; make another the default first"}
	case errors.Is(err, store.ErrCollectionName):
		return invalidProps("not a valid name", "name")
	case errors.Is(err, store.ErrCollectionLimit):
		return &setError{Type: "overQuota", Description: "too many"}
	case errors.Is(err, store.ErrNoShare):
		return invalidProps("no such principal to share with", "shareWith")
	}
	return &setError{Type: "serverFail"}
}

// sortedKeys returns the keys of a map in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// idRef reads an id that may be a "#creationId" reference.
func (c *call) idRef(s string) string {
	if strings.HasPrefix(s, "#") {
		if id, ok := c.created[s[1:]]; ok {
			return id
		}
	}
	return s
}

// sortArg is a Comparator (RFC 8620 5.5).
type sortArg struct {
	Property    string `json:"property"`
	IsAscending *bool  `json:"isAscending"`
	Collation   string `json:"collation"`
}

func (s sortArg) ascending() bool { return s.IsAscending == nil || *s.IsAscending }

// shortHash is a short stable id made from a string.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(s)))
	return hex.EncodeToString(sum[:5])
}
