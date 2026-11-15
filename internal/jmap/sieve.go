package jmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/robinmail/robinmail/internal/sieve"
	"github.com/robinmail/robinmail/internal/store"
)

// JMAP for Sieve scripts (RFC 9661): SieveScript/get, /set, /query and
// /validate. A script is a blob; the account has at most one active
// script, which runs when mail is delivered.

// CapSieve is the capability of RFC 9661 1.3.
const CapSieve = "urn:ietf:params:jmap:sieve"

var sieveProperties = []string{"id", "name", "blobId", "isActive"}

// sieveCapability is the account capability object of RFC 9661 1.3.1.
func sieveCapability() map[string]any {
	return map[string]any{
		"maxSizeScriptName":   store.MaxSieveNameSize,
		"maxSizeScript":       sieve.MaxScriptSize,
		"maxNumberScripts":    store.MaxSieveScripts,
		"maxNumberRedirects":  sieve.MaxRedirects,
		"sieveExtensions":     sieve.Extensions,
		"notificationMethods": nil,
		"externalLists":       nil,
	}
}

func sieveObject(s store.SieveScript) map[string]any {
	return map[string]any{"id": store.SieveScriptID(s.ID), "name": s.Name, "blobId": s.Blob, "isActive": s.Active}
}

// sieveScriptGet is SieveScript/get (RFC 9661 2.1).
func sieveScriptGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(args.Properties, sieveProperties)
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
	all, err := c.h.Store.SieveScripts(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	list, notFound := []map[string]any{}, []string{}
	if args.IDs == nil {
		for _, s := range all {
			list = append(list, pick(sieveObject(s), props))
		}
	} else {
		byID := map[string]store.SieveScript{}
		for _, s := range all {
			byID[store.SieveScriptID(s.ID)] = s
		}
		seen := map[string]bool{}
		for _, id := range *args.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			if s, ok := byID[id]; ok {
				list = append(list, pick(sieveObject(s), props))
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

// sieveContent reads the blob a client named as a script and checks it:
// that it is there, not too large, and a script this server can run.
func (c *call) sieveContent(account string, rawID json.RawMessage) ([]byte, *setError) {
	var blobID string
	if json.Unmarshal(rawID, &blobID) != nil || blobID == "" {
		return nil, invalidProps("blobId must be the id of a blob", "blobId")
	}
	data, err := c.h.blobData(c.ctx, account, blobID)
	if errors.Is(err, store.ErrNoBlob) {
		return nil, &setError{Type: "blobNotFound", Description: "no such blob", NotFound: []string{blobID}}
	}
	if err != nil {
		return nil, &setError{Type: "serverFail"}
	}
	if len(data) > sieve.MaxScriptSize {
		return nil, &setError{Type: "tooLarge", Description: fmt.Sprintf("a script may have %d octets", sieve.MaxScriptSize), MaxSize: sieve.MaxScriptSize}
	}
	if _, err := sieve.Parse(data); err != nil {
		// RFC 9661 2.2: "invalidSieve: The SieveScript content violates
		// the Sieve grammar ... and/or one or more extensions mentioned
		// in the script's "require" statement(s) are not supported".
		return nil, &setError{Type: "invalidSieve", Description: err.Error()}
	}
	return data, nil
}

// sieveName reads a name property: a string, or null for a name the
// server chooses.
func sieveName(raw json.RawMessage) (name string, given bool, se *setError) {
	if raw == nil || string(raw) == "null" {
		return "", false, nil
	}
	if json.Unmarshal(raw, &name) != nil {
		return "", false, invalidProps("name must be a string or null", "name")
	}
	return name, true, nil
}

// sieveScriptSet is SieveScript/set (RFC 9661 2.2), with
// onSuccessActivateScript and onSuccessDeactivateScript.
func sieveScriptSet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		setArgs
		OnSuccessActivateScript   *string `json:"onSuccessActivateScript"`
		OnSuccessDeactivateScript *bool   `json:"onSuccessDeactivateScript"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	run, e := c.begin(&args.setArgs)
	if e != nil {
		return nil, e
	}
	st := c.h.Store
	mismatch := mErr("stateMismatch", "the state is not the ifInState given")
	storeFail := func(err error) *setError {
		switch {
		case errors.Is(err, store.ErrNoSieveScript):
			return &setError{Type: "notFound"}
		case errors.Is(err, store.ErrSieveName):
			return &setError{Type: "alreadyExists", Description: "another script has that name"}
		case errors.Is(err, store.ErrSieveBadName):
			return invalidProps("not a name for a script", "name")
		case errors.Is(err, store.ErrSieveLimit):
			return &setError{Type: "overQuota", Description: fmt.Sprintf("an account may have %d scripts", store.MaxSieveScripts)}
		case errors.Is(err, store.ErrSieveActive):
			// RFC 9661 2.2: "sieveIsActive: The SieveScript is active".
			return &setError{Type: "sieveIsActive", Description: "the active script cannot be destroyed; deactivate it first"}
		}
		return &setError{Type: "serverFail"}
	}

	cids := make([]string, 0, len(args.Create))
	for cid := range args.Create {
		cids = append(cids, cid)
	}
	sort.Strings(cids)
	made := map[string]int64{} // creation id -> script id
	for _, cid := range cids {
		var obj map[string]json.RawMessage
		if json.Unmarshal(args.Create[cid], &obj) != nil || obj == nil {
			run.notCreated[cid] = invalidProps("a script to create must be an object")
			continue
		}
		var bad []string
		for key := range obj {
			if key != "name" && key != "blobId" {
				bad = append(bad, key) // id and isActive are the server's
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			run.notCreated[cid] = invalidProps("these properties of a script cannot be set", bad...)
			continue
		}
		name, named, se := sieveName(obj["name"])
		if se != nil {
			run.notCreated[cid] = se
			continue
		}
		content, se := c.sieveContent(args.AccountID, obj["blobId"])
		if se != nil {
			run.notCreated[cid] = se
			continue
		}
		var script store.SieveScript
		var state int64
		var err error
		if named {
			script, state, err = st.CreateSieveScript(c.ctx, args.AccountID, name, content, run.ifState)
		} else {
			// A name of the server's: the first of "Script 1", "Script 2",
			// ... that is free.
			for n := 1; n <= store.MaxSieveScripts+1; n++ {
				name = fmt.Sprintf("Script %d", n)
				script, state, err = st.CreateSieveScript(c.ctx, args.AccountID, name, content, run.ifState)
				if !errors.Is(err, store.ErrSieveName) {
					break
				}
			}
		}
		switch {
		case errors.Is(err, store.ErrStateMismatch):
			return nil, mismatch
		case err != nil:
			run.notCreated[cid] = storeFail(err)
		default:
			run.wroteState(state)
			if run.created == nil {
				run.created = map[string]any{}
			}
			sid := store.SieveScriptID(script.ID)
			created := map[string]any{"id": sid, "isActive": false}
			if !named {
				created["name"] = name
			}
			run.created[cid] = created
			c.created[cid] = sid
			made[cid] = script.ID
		}
	}

	sids := make([]string, 0, len(args.Update))
	for sid := range args.Update {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	for _, sid := range sids {
		id, ok := store.ParseSieveScriptID(sid)
		if !ok {
			run.notUpdated[sid] = &setError{Type: "notFound"}
			continue
		}
		var name *string
		var content []byte
		var se *setError
		for key, v := range args.Update[sid] {
			switch key {
			case "name":
				n, given, nerr := sieveName(v)
				if nerr != nil || !given {
					se = invalidProps("name must be a string", "name")
				}
				name = &n
			case "blobId":
				if content, se = c.sieveContent(args.AccountID, v); se == nil && content == nil {
					content = []byte{}
				}
			default:
				se = invalidProps("this property of a script cannot be set", key)
			}
			if se != nil {
				break
			}
		}
		if se != nil {
			run.notUpdated[sid] = se
			continue
		}
		if name == nil && content == nil {
			// Nothing to change; the script has to exist all the same.
			list, err := st.SieveScripts(c.ctx, args.AccountID)
			found := false
			for _, s := range list {
				found = found || s.ID == id
			}
			if err != nil || !found {
				run.notUpdated[sid] = &setError{Type: "notFound"}
				continue
			}
			if run.updated == nil {
				run.updated = map[string]any{}
			}
			run.updated[sid] = nil
			continue
		}
		state, err := st.UpdateSieveScript(c.ctx, args.AccountID, id, name, content, run.ifState)
		switch {
		case errors.Is(err, store.ErrStateMismatch):
			return nil, mismatch
		case err != nil:
			run.notUpdated[sid] = storeFail(err)
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
			id, ok := store.ParseSieveScriptID(sid)
			if !ok {
				run.notDestroyed[sid] = &setError{Type: "notFound"}
				continue
			}
			state, err := st.DestroySieveScript(c.ctx, args.AccountID, id, run.ifState)
			switch {
			case errors.Is(err, store.ErrStateMismatch):
				return nil, mismatch
			case err != nil:
				run.notDestroyed[sid] = storeFail(err)
			default:
				run.wroteState(state)
				run.destroyed = append(run.destroyed, sid)
			}
		}
	}

	// RFC 9661 2.2: activation and deactivation happen "if and only if
	// all of the creations, modifications, and destructions (if any)
	// succeed", and activating wins when both are asked for.
	wantOn := args.OnSuccessActivateScript != nil
	wantOff := !wantOn && args.OnSuccessDeactivateScript != nil && *args.OnSuccessDeactivateScript
	if (wantOn || wantOff) && len(run.notCreated)+len(run.notUpdated)+len(run.notDestroyed) == 0 {
		target := int64(0)
		if wantOn {
			ref := *args.OnSuccessActivateScript
			if cid, isRef := strings.CutPrefix(ref, "#"); isRef {
				id, ok := made[cid]
				if !ok {
					return nil, invalidArgs("onSuccessActivateScript names a creation %q that this call did not make", cid)
				}
				target = id
			} else if id, ok := store.ParseSieveScriptID(ref); ok {
				target = id
			} else {
				return nil, invalidArgs("onSuccessActivateScript is not the id of a script")
			}
		}
		before, err := st.SieveScripts(c.ctx, args.AccountID)
		if err != nil {
			return nil, storeErr(err)
		}
		state, err := st.ActivateSieveScript(c.ctx, args.AccountID, target, run.ifState)
		switch {
		case errors.Is(err, store.ErrStateMismatch):
			return nil, mismatch
		case errors.Is(err, store.ErrNoSieveScript):
			return nil, invalidArgs("onSuccessActivateScript names a script that does not exist")
		case err != nil:
			return nil, storeErr(err)
		}
		run.wroteState(state)
		// The scripts whose isActive this changed are reported: one made
		// by this call in created, the others in updated.
		for _, s := range before {
			now := s.ID == target
			if s.Active == now {
				continue
			}
			sid := store.SieveScriptID(s.ID)
			reported := false
			for cid, id := range made {
				if id == s.ID {
					run.created[cid].(map[string]any)["isActive"] = now
					reported = true
				}
			}
			if !reported {
				if run.updated == nil {
					run.updated = map[string]any{}
				}
				run.updated[sid] = map[string]any{"isActive": now}
			}
		}
	}
	return run.result(), nil
}

// sieveFilter is the filter of SieveScript/query (RFC 9661 2.3): name
// (contains) and isActive, under AND, OR and NOT.
type sieveFilter struct {
	operator   string
	conditions []sieveFilter
	name       *string
	isActive   *bool
}

func parseSieveFilter(raw json.RawMessage, depth int) (*sieveFilter, *methodError) {
	if depth > maxFilterDepth {
		return nil, invalidArgs("the filter is nested too deep")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, invalidArgs("a filter must be an object")
	}
	f := &sieveFilter{}
	if op, ok := obj["operator"]; ok {
		var conds []json.RawMessage
		if json.Unmarshal(op, &f.operator) != nil || (f.operator != "AND" && f.operator != "OR" && f.operator != "NOT") {
			return nil, invalidArgs("a filter operator must be AND, OR or NOT")
		}
		if len(obj) != 2 || json.Unmarshal(obj["conditions"], &conds) != nil || conds == nil || len(conds) > maxFilterNodes {
			return nil, invalidArgs("a filter operator needs conditions and nothing else")
		}
		for _, cr := range conds {
			sub, e := parseSieveFilter(cr, depth+1)
			if e != nil {
				return nil, e
			}
			f.conditions = append(f.conditions, *sub)
		}
		return f, nil
	}
	for key, v := range obj {
		switch key {
		case "name":
			if json.Unmarshal(v, &f.name) != nil || f.name == nil {
				return nil, invalidArgs("the filter property name must be a string")
			}
		case "isActive":
			if json.Unmarshal(v, &f.isActive) != nil || f.isActive == nil {
				return nil, invalidArgs("the filter property isActive must be a boolean")
			}
		default:
			return nil, mErr("unsupportedFilter", "the server cannot filter scripts on %q", key)
		}
	}
	return f, nil
}

func (f *sieveFilter) match(s store.SieveScript) bool {
	switch f.operator {
	case "AND":
		for i := range f.conditions {
			if !f.conditions[i].match(s) {
				return false
			}
		}
		return true
	case "OR", "NOT":
		any := false
		for i := range f.conditions {
			any = any || f.conditions[i].match(s)
		}
		return any != (f.operator == "NOT")
	}
	if f.name != nil && !strings.Contains(strings.ToLower(s.Name), strings.ToLower(*f.name)) {
		return false
	}
	return f.isActive == nil || *f.isActive == s.Active
}

// sieveScriptQuery is SieveScript/query (RFC 9661 2.3).
func sieveScriptQuery(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID string          `json:"accountId"`
		Filter    json.RawMessage `json:"filter"`
		Sort      *[]struct {
			Property    string  `json:"property"`
			IsAscending *bool   `json:"isAscending"`
			Collation   *string `json:"collation"`
		} `json:"sort"`
		queryArgs
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	limit, clamped, e := args.limit()
	if e != nil {
		return nil, e
	}
	var filter *sieveFilter
	if len(args.Filter) > 0 && string(args.Filter) != "null" {
		if filter, e = parseSieveFilter(args.Filter, 0); e != nil {
			return nil, e
		}
	}
	if args.Sort != nil {
		for _, s := range *args.Sort {
			if s.Property != "name" && s.Property != "isActive" {
				return nil, mErr("unsupportedSort", "the server cannot sort scripts by %q", s.Property)
			}
			if s.Collation != nil && *s.Collation != "i;ascii-casemap" {
				return nil, mErr("unsupportedSort", "the server does not have the collation %q", *s.Collation)
			}
		}
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	all, err := c.h.Store.SieveScripts(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	var found []store.SieveScript
	for _, s := range all {
		if filter == nil || filter.match(s) {
			found = append(found, s)
		}
	}
	if args.Sort != nil {
		sort.SliceStable(found, func(i, j int) bool {
			for _, s := range *args.Sort {
				asc := s.IsAscending == nil || *s.IsAscending
				var cmp int
				if s.Property == "name" {
					cmp = strings.Compare(strings.ToLower(found[i].Name), strings.ToLower(found[j].Name))
				} else if found[i].Active != found[j].Active {
					cmp = 1
					if !found[i].Active {
						cmp = -1
					}
				}
				if cmp != 0 {
					return (cmp < 0) == asc
				}
			}
			return found[i].ID < found[j].ID
		})
	}
	ids := make([]string, len(found))
	for i, s := range found {
		ids[i] = store.SieveScriptID(s.ID)
	}
	position, window, e := window(ids, args.Position, args.Anchor, args.AnchorOffset, limit)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"accountId": args.AccountID, "queryState": stateString(state), "canCalculateChanges": false,
		"position": position, "ids": window}
	if args.CalculateTotal {
		out["total"] = len(ids)
	}
	if clamped {
		out["limit"] = limit
	}
	return out, nil
}

// sieveScriptValidate is SieveScript/validate (RFC 9661 2.4): is the blob
// a script this server would take? Nothing is stored.
func sieveScriptValidate(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID string          `json:"accountId"`
		BlobID    json.RawMessage `json:"blobId"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if len(args.BlobID) == 0 {
		return nil, invalidArgs("blobId is required")
	}
	out := map[string]any{"accountId": args.AccountID, "error": nil}
	if _, se := c.sieveContent(args.AccountID, args.BlobID); se != nil {
		out["error"] = se
	}
	return out, nil
}
