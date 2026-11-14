package jmap

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/mikepage/robinmail/internal/store"
)

// JMAP Quota (RFC 9425): what the account may hold and holds, as up to two
// Quota objects: "octets" for the size of its mail and "count" for the
// number of messages. A limit that is not set has no object.

// CapQuota is the capability of RFC 9425 3.
const CapQuota = "urn:ietf:params:jmap:quota"

// The ids of the two quotas.
const (
	quotaOctets = "octets"
	quotaCount  = "count"
)

var quotaProperties = []string{"id", "resourceType", "used", "hardLimit", "scope", "name", "types", "warnLimit", "softLimit", "description"}

// quotaObjects are the Quota objects of an account (RFC 9425 4).
func quotaObjects(q store.Quota) []map[string]any {
	var out []map[string]any
	add := func(id, name, description string, used, limit int64) {
		if limit <= 0 {
			return
		}
		out = append(out, map[string]any{
			"id": id, "resourceType": id, "used": used, "hardLimit": limit, "scope": "account", "name": name,
			// The quota is about mail: Emails are what counts.
			"types": []string{"Email"}, "warnLimit": nil, "softLimit": nil, "description": description,
		})
	}
	add(quotaOctets, "Storage", "The size of all messages in the account, in octets. Each message counts once, whatever number of mailboxes it is in.", q.UsedBytes, q.Bytes)
	add(quotaCount, "Messages", "The number of messages in the account.", q.UsedMessages, q.Messages)
	return out
}

// quotaGet is Quota/get (RFC 9425 4.1).
func quotaGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(args.Properties, quotaProperties)
	if e != nil {
		return nil, e
	}
	if args.IDs != nil && len(*args.IDs) > c.h.limits.MaxObjectsInGet {
		return nil, mErr("requestTooLarge", "more ids than maxObjectsInGet")
	}
	// The state first: what is read after it is at least that new.
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	q, err := c.h.Store.Quota(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	all := quotaObjects(q)
	list, notFound := []map[string]any{}, []string{}
	if args.IDs == nil {
		for _, o := range all {
			list = append(list, pick(o, props))
		}
	} else {
		seen := map[string]bool{}
		for _, id := range *args.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			found := false
			for _, o := range all {
				if o["id"] == id {
					list = append(list, pick(o, props))
					found = true
				}
			}
			if !found {
				notFound = append(notFound, id)
			}
		}
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

// quotaChanged says what happened to the quotas of an account since a
// state. Their use moves with every Email that comes or goes, which the
// change log has; a limit that was set is logged as Quota. usedOnly is
// true when nothing but "used" can have changed.
func (c *call) quotaChanged(account string, since int64) (changed, usedOnly bool, now int64, e *methodError) {
	ch, err := c.h.Store.Changes(c.ctx, account, since)
	if err != nil {
		return false, false, 0, storeErr(err)
	}
	limits := ch.Other["Quota"] != nil
	use := len(ch.Email.Created)+len(ch.Email.Destroyed) > 0
	return limits || use, !limits, ch.NewState, nil
}

// quotaChanges is Quota/changes (RFC 9425 4.2), with updatedProperties:
// "used" alone when only the use changed, null otherwise.
func quotaChanges(c *call, raw json.RawMessage) (any, *methodError) {
	var args changesArgs
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if args.SinceState == nil {
		return nil, invalidArgs("sinceState is required")
	}
	if args.MaxChanges != nil && *args.MaxChanges <= 0 {
		return nil, invalidArgs("maxChanges must be greater than 0")
	}
	since, ok := parseState(*args.SinceState)
	if !ok {
		return nil, mErr("cannotCalculateChanges", "the server cannot calculate the changes from that state")
	}
	changed, usedOnly, now, e := c.quotaChanged(args.AccountID, since)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"accountId": args.AccountID, "oldState": *args.SinceState, "newState": stateString(now),
		"hasMoreChanges": false, "created": []string{}, "updated": []string{}, "destroyed": []string{}, "updatedProperties": nil}
	if !changed {
		return out, nil
	}
	q, err := c.h.Store.Quota(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	have := map[string]bool{}
	updated := []string{}
	for _, o := range quotaObjects(q) {
		have[o["id"].(string)] = true
		updated = append(updated, o["id"].(string))
	}
	out["updated"] = updated
	if usedOnly {
		out["updatedProperties"] = []string{"used"}
		return out, nil
	}
	// A limit was set or taken away: a quota that is not there now may
	// have been before. Telling a client that never had it is harmless.
	destroyed := []string{}
	for _, id := range []string{quotaOctets, quotaCount} {
		if !have[id] {
			destroyed = append(destroyed, id)
		}
	}
	out["destroyed"] = destroyed
	return out, nil
}

type quotaQueryArgs struct {
	AccountID string          `json:"accountId"`
	Filter    json.RawMessage `json:"filter"`
	Sort      *[]struct {
		Property    string  `json:"property"`
		IsAscending *bool   `json:"isAscending"`
		Collation   *string `json:"collation"`
	} `json:"sort"`
	queryArgs
}

// quotaFilter is the filter of Quota/query (RFC 9425 4.3).
type quotaFilter struct {
	operator                            string
	conditions                          []quotaFilter
	name, scope, resourceType, typeName *string
}

func parseQuotaFilter(raw json.RawMessage, depth int) (*quotaFilter, *methodError) {
	if depth > maxFilterDepth {
		return nil, invalidArgs("the filter is nested too deep")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, invalidArgs("a filter must be an object")
	}
	f := &quotaFilter{}
	if op, ok := obj["operator"]; ok {
		var conds []json.RawMessage
		if json.Unmarshal(op, &f.operator) != nil || (f.operator != "AND" && f.operator != "OR" && f.operator != "NOT") {
			return nil, invalidArgs("a filter operator must be AND, OR or NOT")
		}
		if len(obj) != 2 || json.Unmarshal(obj["conditions"], &conds) != nil || conds == nil || len(conds) > maxFilterNodes {
			return nil, invalidArgs("a filter operator needs conditions and nothing else")
		}
		for _, cr := range conds {
			sub, e := parseQuotaFilter(cr, depth+1)
			if e != nil {
				return nil, e
			}
			f.conditions = append(f.conditions, *sub)
		}
		return f, nil
	}
	for key, v := range obj {
		var dst **string
		switch key {
		case "name":
			dst = &f.name
		case "scope":
			dst = &f.scope
		case "resourceType":
			dst = &f.resourceType
		case "type":
			dst = &f.typeName
		default:
			return nil, mErr("unsupportedFilter", "the server cannot filter quotas on %q", key)
		}
		if json.Unmarshal(v, dst) != nil || *dst == nil {
			return nil, invalidArgs("the filter property %q must be a string", key)
		}
	}
	return f, nil
}

func (f *quotaFilter) match(o map[string]any) bool {
	switch f.operator {
	case "AND":
		for i := range f.conditions {
			if !f.conditions[i].match(o) {
				return false
			}
		}
		return true
	case "OR", "NOT":
		any := false
		for i := range f.conditions {
			any = any || f.conditions[i].match(o)
		}
		return any != (f.operator == "NOT")
	}
	if f.name != nil && !strings.Contains(strings.ToLower(o["name"].(string)), strings.ToLower(*f.name)) {
		return false
	}
	if (f.scope != nil && *f.scope != o["scope"]) || (f.resourceType != nil && *f.resourceType != o["resourceType"]) {
		return false
	}
	return f.typeName == nil || contains(o["types"].([]string), *f.typeName)
}

// runQuotaQuery returns the ids of the quotas that match, in order.
func (c *call) runQuotaQuery(args *quotaQueryArgs) ([]string, *methodError) {
	var filter *quotaFilter
	if len(args.Filter) > 0 && string(args.Filter) != "null" {
		var e *methodError
		if filter, e = parseQuotaFilter(args.Filter, 0); e != nil {
			return nil, e
		}
	}
	if args.Sort != nil {
		for _, s := range *args.Sort {
			// RFC 9425 4.3: "name" and "used".
			if s.Property != "name" && s.Property != "used" {
				return nil, mErr("unsupportedSort", "the server cannot sort quotas by %q", s.Property)
			}
			if s.Collation != nil && *s.Collation != "i;ascii-casemap" {
				return nil, mErr("unsupportedSort", "the server does not have the collation %q", *s.Collation)
			}
		}
	}
	q, err := c.h.Store.Quota(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	var found []map[string]any
	for _, o := range quotaObjects(q) {
		if filter == nil || filter.match(o) {
			found = append(found, o)
		}
	}
	if args.Sort != nil {
		sort.SliceStable(found, func(i, j int) bool {
			for _, s := range *args.Sort {
				asc := s.IsAscending == nil || *s.IsAscending
				cmp := 0
				if s.Property == "name" {
					cmp = strings.Compare(strings.ToLower(found[i]["name"].(string)), strings.ToLower(found[j]["name"].(string)))
				} else if a, b := found[i]["used"].(int64), found[j]["used"].(int64); a != b {
					cmp = 1
					if a < b {
						cmp = -1
					}
				}
				if cmp != 0 {
					return (cmp < 0) == asc
				}
			}
			return false
		})
	}
	ids := make([]string, len(found))
	for i, o := range found {
		ids[i] = o["id"].(string)
	}
	return ids, nil
}

// quotaQuery is Quota/query (RFC 9425 4.3).
func quotaQuery(c *call, raw json.RawMessage) (any, *methodError) {
	var args quotaQueryArgs
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	limit, clamped, e := args.limit()
	if e != nil {
		return nil, e
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	all, e := c.runQuotaQuery(&args)
	if e != nil {
		return nil, e
	}
	position, ids, e := window(all, args.Position, args.Anchor, args.AnchorOffset, limit)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"accountId": args.AccountID, "queryState": stateString(state), "canCalculateChanges": true,
		"position": position, "ids": ids}
	if args.CalculateTotal {
		out["total"] = len(all)
	}
	if clamped {
		out["limit"] = limit
	}
	return out, nil
}

// quotaQueryChanges is Quota/queryChanges (RFC 9425 4.4). A sort by "used"
// can change with any change of use, so every quota is a candidate then.
func quotaQueryChanges(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		quotaQueryArgs
		queryChangesArgs
	}
	var probe map[string]json.RawMessage
	json.Unmarshal(raw, &probe)
	for _, k := range []string{"position", "anchor", "anchorOffset", "limit"} {
		if _, ok := probe[k]; ok {
			return nil, invalidArgs("unknown argument %q", k)
		}
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if args.SinceQueryState == nil {
		return nil, invalidArgs("sinceQueryState is required")
	}
	since, ok := parseState(*args.SinceQueryState)
	if !ok {
		return nil, mErr("cannotCalculateChanges", "the server cannot calculate the changes from that state")
	}
	changed, _, now, e := c.quotaChanged(args.AccountID, since)
	if e != nil {
		return nil, e
	}
	results, e := c.runQuotaQuery(&args.quotaQueryArgs)
	if e != nil {
		return nil, e
	}
	var candidates []string
	if changed {
		candidates = []string{quotaOctets, quotaCount}
	}
	return queryChanges(args.AccountID, args.queryChangesArgs, since, now, candidates, results)
}
