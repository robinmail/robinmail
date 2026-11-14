package jmap

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/mikepage/robinmail/internal/address"
	"github.com/mikepage/robinmail/internal/message"
	"github.com/mikepage/robinmail/internal/store"
)

var identityProperties = []string{"id", "name", "email", "replyTo", "bcc", "textSignature", "htmlSignature", "mayDelete"}

// addressList turns the stored JSON of replyTo or bcc into what JMAP
// shows: a list of addresses, or null.
func addressList(stored string) any {
	var list []message.Address
	if stored == "" || json.Unmarshal([]byte(stored), &list) != nil || len(list) == 0 {
		return nil
	}
	return list
}

// identityObject is an Identity (RFC 8621 6).
func identityObject(i store.Identity) map[string]any {
	return map[string]any{
		"id":            store.IdentityID(i.ID),
		"name":          i.Name,
		"email":         i.Email,
		"replyTo":       addressList(i.ReplyTo),
		"bcc":           addressList(i.Bcc),
		"textSignature": i.TextSignature,
		"htmlSignature": i.HTMLSignature,
		"mayDelete":     i.MayDelete(),
	}
}

// identityGet is Identity/get (RFC 8621 6.1).
func identityGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(args.Properties, identityProperties)
	if e != nil {
		return nil, e
	}
	if args.IDs != nil && len(*args.IDs) > c.h.limits.MaxObjectsInGet {
		return nil, mErr("requestTooLarge", "more ids than maxObjectsInGet")
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	all, err := c.h.Store.Identities(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	list, notFound := []map[string]any{}, []string{}
	if args.IDs == nil {
		for _, i := range all {
			list = append(list, pick(identityObject(i), props))
		}
	} else {
		byID := map[string]store.Identity{}
		for _, i := range all {
			byID[store.IdentityID(i.ID)] = i
		}
		seen := map[string]bool{}
		for _, id := range *args.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			if i, ok := byID[id]; ok {
				list = append(list, pick(identityObject(i), props))
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

// identityChanges is Identity/changes (RFC 8621 6.2).
func identityChanges(c *call, raw json.RawMessage) (any, *methodError) {
	return c.changes(raw, "Identity", store.IdentityID)
}

// identityPatch reads the properties of an Identity a client may set.
// email is only read for a creation: it never changes afterwards.
func identityPatch(obj map[string]json.RawMessage, create bool) (store.Identity, store.IdentityUpdate, *setError) {
	var i store.Identity
	var u store.IdentityUpdate
	var bad []string
	text := func(key string, dst *string, max int) *string {
		var s string
		if json.Unmarshal(obj[key], &s) != nil || len(s) > max || strings.ContainsRune(s, 0) {
			bad = append(bad, key)
			return nil
		}
		*dst = s
		return dst
	}
	addrs := func(key string, dst *string) *string {
		if string(obj[key]) == "null" {
			*dst = ""
			return dst
		}
		var list []message.Address
		if json.Unmarshal(obj[key], &list) != nil || len(list) > 50 {
			bad = append(bad, key)
			return nil
		}
		for _, a := range list {
			if !validAddr(a) {
				bad = append(bad, key)
				return nil
			}
		}
		b, _ := json.Marshal(list)
		*dst = string(b)
		return dst
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch key {
		case "name":
			u.Name = text(key, &i.Name, 255)
			if strings.ContainsAny(i.Name, "\r\n") {
				bad = append(bad, key)
			}
		case "email":
			if !create {
				bad = append(bad, key) // immutable
				continue
			}
			text(key, &i.Email, 320)
		case "replyTo":
			u.ReplyTo = addrs(key, &i.ReplyTo)
		case "bcc":
			u.Bcc = addrs(key, &i.Bcc)
		case "textSignature":
			u.TextSignature = text(key, &i.TextSignature, 64<<10)
		case "htmlSignature":
			u.HTMLSignature = text(key, &i.HTMLSignature, 64<<10)
		default:
			bad = append(bad, key) // id and mayDelete are the server's
		}
	}
	if len(bad) > 0 {
		return i, u, invalidProps("these properties of an identity cannot be set as given", bad...)
	}
	return i, u, nil
}

// identitySet is Identity/set (RFC 8621 6.3). A user may only have
// identities for addresses that are theirs: the address of the account,
// with or without a "+tag". Anything else is forbiddenFrom.
func identitySet(c *call, raw json.RawMessage) (any, *methodError) {
	var args setArgs
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	run, e := c.begin(&args)
	if e != nil {
		return nil, e
	}
	st := c.h.Store
	mismatch := mErr("stateMismatch", "the state is not the ifInState given")

	cids := make([]string, 0, len(args.Create))
	for cid := range args.Create {
		cids = append(cids, cid)
	}
	sort.Strings(cids)
	for _, cid := range cids {
		var obj map[string]json.RawMessage
		if json.Unmarshal(args.Create[cid], &obj) != nil || obj == nil {
			run.notCreated[cid] = invalidProps("an identity to create must be an object")
			continue
		}
		i, _, se := identityPatch(obj, true)
		if se != nil {
			run.notCreated[cid] = se
			continue
		}
		if i.Email == "" {
			run.notCreated[cid] = invalidProps("an identity needs an email", "email")
			continue
		}
		if !address.Owns(c.user.Username, i.Email) {
			run.notCreated[cid] = &setError{Type: "forbiddenFrom", Description: "you may not send from that address"}
			continue
		}
		i.Email = strings.ToLower(i.Email)
		made, state, err := st.CreateIdentity(c.ctx, args.AccountID, i, run.ifState)
		switch {
		case errors.Is(err, store.ErrStateMismatch):
			return nil, mismatch
		case err != nil:
			run.notCreated[cid] = &setError{Type: "serverFail"}
		default:
			run.wroteState(state)
			if run.created == nil {
				run.created = map[string]any{}
			}
			sid := store.IdentityID(made.ID)
			run.created[cid] = map[string]any{"id": sid, "mayDelete": made.MayDelete()}
			c.created[cid] = sid
		}
	}

	sids := make([]string, 0, len(args.Update))
	for sid := range args.Update {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	for _, sid := range sids {
		id, ok := store.ParseIdentityID(sid)
		if !ok {
			run.notUpdated[sid] = &setError{Type: "notFound"}
			continue
		}
		_, u, se := identityPatch(args.Update[sid], false)
		if se != nil {
			run.notUpdated[sid] = se
			continue
		}
		state, err := st.UpdateIdentity(c.ctx, args.AccountID, id, u, run.ifState)
		switch {
		case errors.Is(err, store.ErrStateMismatch):
			return nil, mismatch
		case errors.Is(err, store.ErrNoIdentity):
			run.notUpdated[sid] = &setError{Type: "notFound"}
		case err != nil:
			run.notUpdated[sid] = &setError{Type: "serverFail"}
		default:
			run.wroteState(state)
			if run.updated == nil {
				run.updated = map[string]any{}
			}
			run.updated[sid] = nil
		}
	}

	if args.Destroy != nil {
		for _, sid := range *args.Destroy {
			id, ok := store.ParseIdentityID(sid)
			if !ok {
				run.notDestroyed[sid] = &setError{Type: "notFound"}
				continue
			}
			state, err := st.DestroyIdentity(c.ctx, args.AccountID, id, run.ifState)
			switch {
			case errors.Is(err, store.ErrStateMismatch):
				return nil, mismatch
			case errors.Is(err, store.ErrNoIdentity):
				run.notDestroyed[sid] = &setError{Type: "notFound"}
			case errors.Is(err, store.ErrIdentityDefault):
				run.notDestroyed[sid] = &setError{Type: "forbidden", Description: "the identity of the account's own address stays"}
			case err != nil:
				run.notDestroyed[sid] = &setError{Type: "serverFail"}
			default:
				run.wroteState(state)
				run.destroyed = append(run.destroyed, sid)
			}
		}
	}
	return run.result(), nil
}
