package jmap

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/mikepage/robinmail/internal/store"
)

// mailboxProperties are the properties of a Mailbox (RFC 8621 2).
var mailboxProperties = []string{"id", "name", "parentId", "role", "sortOrder", "totalEmails", "unreadEmails",
	"totalThreads", "unreadThreads", "myRights", "isSubscribed"}

func mailboxObject(m store.Mailbox) map[string]any {
	var parent, role any
	if m.ParentID != 0 {
		parent = store.MailboxID(m.ParentID)
	}
	if m.Role != "" {
		role = m.Role
	}
	return map[string]any{
		"id":            store.MailboxID(m.ID),
		"name":          m.Name,
		"parentId":      parent,
		"role":          role,
		"sortOrder":     m.SortOrder,
		"totalEmails":   m.Total,
		"unreadEmails":  m.Unread,
		"totalThreads":  m.TotalThreads,
		"unreadThreads": m.UnreadThreads,
		"isSubscribed":  m.Subscribed,
		"myRights": map[string]bool{
			"mayReadItems": true, "mayAddItems": true, "mayRemoveItems": true, "maySetSeen": true,
			"maySetKeywords": true, "mayCreateChild": true, "mayRename": true, "maySubmit": true,
			// Delivery needs the mailboxes with a role.
			"mayDelete": m.Role == "",
		},
	}
}

// pick keeps the asked properties of an object. The id always stays.
func pick(obj map[string]any, props []string) map[string]any {
	if props == nil {
		return obj
	}
	out := map[string]any{"id": obj["id"]}
	for _, p := range props {
		if v, ok := obj[p]; ok {
			out[p] = v
		}
	}
	return out
}

func checkProperties(props *[]string, known []string) ([]string, *methodError) {
	if props == nil {
		return nil, nil
	}
	for _, p := range *props {
		found := false
		for _, k := range known {
			found = found || k == p
		}
		if !found {
			return nil, invalidArgs("unknown property %q", p)
		}
	}
	return append([]string{}, *props...), nil
}

// mailboxGet is Mailbox/get (RFC 8621 2.1, RFC 8620 5.1).
func mailboxGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(args.Properties, mailboxProperties)
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
	all, err := c.h.Store.Mailboxes(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	list, notFound := []map[string]any{}, []string{}
	if args.IDs == nil {
		for _, m := range all {
			list = append(list, pick(mailboxObject(m), props))
		}
	} else {
		byID := map[string]store.Mailbox{}
		for _, m := range all {
			byID[store.MailboxID(m.ID)] = m
		}
		seen := map[string]bool{}
		for _, id := range *args.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			if m, ok := byID[id]; ok {
				list = append(list, pick(mailboxObject(m), props))
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

// changesArgs are the arguments of every /changes (RFC 8620 5.2).
type changesArgs struct {
	AccountID  string  `json:"accountId"`
	SinceState *string `json:"sinceState"`
	MaxChanges *int64  `json:"maxChanges"`
}

// changes answers a /changes for one type.
func (c *call) changes(raw json.RawMessage, typ string, format func(int64) string) (map[string]any, *methodError) {
	var args changesArgs
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if args.SinceState == nil {
		return nil, invalidArgs("sinceState is required")
	}
	max := 0
	if args.MaxChanges != nil {
		if *args.MaxChanges <= 0 {
			return nil, invalidArgs("maxChanges must be greater than 0")
		}
		max = int(min(*args.MaxChanges, maxChangesLimit))
	}
	since, ok := parseState(*args.SinceState)
	if !ok {
		// Not a state this server ever gave out.
		return nil, mErr("cannotCalculateChanges", "the server cannot calculate the changes from that state")
	}
	set, newState, more, err := c.h.Store.ChangesOf(c.ctx, args.AccountID, typ, since, max)
	if err != nil {
		return nil, storeErr(err)
	}
	ids := func(list []int64) []string {
		out := make([]string, len(list))
		for i, id := range list {
			out[i] = format(id)
		}
		return out
	}
	return map[string]any{
		"accountId":      args.AccountID,
		"oldState":       *args.SinceState,
		"newState":       stateString(newState),
		"hasMoreChanges": more,
		"created":        ids(set.Created),
		"updated":        ids(set.Updated),
		"destroyed":      ids(set.Destroyed),
	}, nil
}

// mailboxChanges is Mailbox/changes (RFC 8621 2.2). updatedProperties is
// always null: the change log does not record whether only counts changed.
func mailboxChanges(c *call, raw json.RawMessage) (any, *methodError) {
	out, e := c.changes(raw, "Mailbox", store.MailboxID)
	if e != nil {
		return nil, e
	}
	out["updatedProperties"] = nil
	return out, nil
}

// mailboxFilter is the filter of Mailbox/query (RFC 8621 2.3).
type mailboxFilter struct {
	operator   string
	conditions []mailboxFilter

	parentSet    bool
	parent       int64 // 0: the top level
	name         *string
	roleSet      bool
	role         string // "": no role
	hasAnyRole   *bool
	isSubscribed *bool
}

func parseMailboxFilter(raw json.RawMessage, depth int, nodes *int) (*mailboxFilter, *methodError) {
	if *nodes++; *nodes > maxFilterNodes || depth > maxFilterDepth {
		return nil, invalidArgs("the filter is too large")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, invalidArgs("a filter must be an object")
	}
	f := &mailboxFilter{}
	if op, ok := obj["operator"]; ok {
		var conds []json.RawMessage
		if json.Unmarshal(op, &f.operator) != nil || (f.operator != "AND" && f.operator != "OR" && f.operator != "NOT") {
			return nil, invalidArgs("a filter operator must be AND, OR or NOT")
		}
		if len(obj) != 2 || json.Unmarshal(obj["conditions"], &conds) != nil || conds == nil {
			return nil, invalidArgs("a filter operator needs conditions and nothing else")
		}
		for _, cr := range conds {
			sub, e := parseMailboxFilter(cr, depth+1, nodes)
			if e != nil {
				return nil, e
			}
			f.conditions = append(f.conditions, *sub)
		}
		return f, nil
	}
	for key, v := range obj {
		bad := invalidArgs("the filter property %q has the wrong type", key)
		switch key {
		case "parentId":
			var id *string
			if json.Unmarshal(v, &id) != nil {
				return nil, bad
			}
			f.parentSet = true
			if id != nil {
				// An id that cannot exist matches nothing.
				if f.parent, _ = store.ParseMailboxID(*id); f.parent == 0 {
					f.parent = -1
				}
			}
		case "name":
			if json.Unmarshal(v, &f.name) != nil || f.name == nil {
				return nil, bad
			}
		case "role":
			var role *string
			if json.Unmarshal(v, &role) != nil {
				return nil, bad
			}
			f.roleSet = true
			if role != nil {
				if f.role = *role; f.role == "" {
					f.role = "\x00" // matches nothing
				}
			}
		case "hasAnyRole":
			if json.Unmarshal(v, &f.hasAnyRole) != nil || f.hasAnyRole == nil {
				return nil, bad
			}
		case "isSubscribed":
			if json.Unmarshal(v, &f.isSubscribed) != nil || f.isSubscribed == nil {
				return nil, bad
			}
		default:
			return nil, mErr("unsupportedFilter", "the server cannot filter mailboxes on %q", key)
		}
	}
	return f, nil
}

func (f *mailboxFilter) match(m store.Mailbox) bool {
	if f == nil {
		return true
	}
	switch f.operator {
	case "AND":
		for i := range f.conditions {
			if !f.conditions[i].match(m) {
				return false
			}
		}
		return true
	case "OR", "NOT":
		any := false
		for i := range f.conditions {
			any = any || f.conditions[i].match(m)
		}
		return any == (f.operator == "OR")
	}
	return (!f.parentSet || m.ParentID == f.parent) &&
		(f.name == nil || strings.Contains(strings.ToLower(m.Name), strings.ToLower(*f.name))) &&
		(!f.roleSet || m.Role == f.role) &&
		(f.hasAnyRole == nil || (m.Role != "") == *f.hasAnyRole) &&
		(f.isSubscribed == nil || m.Subscribed == *f.isSubscribed)
}

type mailboxQueryArgs struct {
	AccountID string          `json:"accountId"`
	Filter    json.RawMessage `json:"filter"`
	Sort      *[]struct {
		Property    string  `json:"property"`
		IsAscending *bool   `json:"isAscending"`
		Collation   *string `json:"collation"`
	} `json:"sort"`
	queryArgs
	SortAsTree   bool `json:"sortAsTree"`
	FilterAsTree bool `json:"filterAsTree"`
}

// runMailboxQuery returns the ids of the whole result, in order.
func (c *call) runMailboxQuery(args *mailboxQueryArgs) ([]string, *methodError) {
	var filter *mailboxFilter
	if len(args.Filter) > 0 && string(args.Filter) != "null" {
		nodes := 0
		var e *methodError
		if filter, e = parseMailboxFilter(args.Filter, 0, &nodes); e != nil {
			return nil, e
		}
	}
	type key struct {
		prop string
		asc  bool
	}
	var keys []key
	if args.Sort != nil {
		for _, s := range *args.Sort {
			if s.Property != "sortOrder" && s.Property != "name" {
				return nil, mErr("unsupportedSort", "the server cannot sort mailboxes by %q", s.Property)
			}
			if s.Collation != nil && *s.Collation != "i;ascii-casemap" {
				return nil, mErr("unsupportedSort", "the server does not have the collation %q", *s.Collation)
			}
			keys = append(keys, key{s.Property, s.IsAscending == nil || *s.IsAscending})
		}
	}
	all, err := c.h.Store.Mailboxes(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	less := func(a, b store.Mailbox) bool {
		for _, k := range keys {
			var cmp int
			switch k.prop {
			case "sortOrder":
				cmp = int(a.SortOrder - b.SortOrder)
			case "name":
				cmp = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
			}
			if cmp != 0 {
				return (cmp < 0) == k.asc
			}
		}
		return a.ID < b.ID
	}
	sort.SliceStable(all, func(i, j int) bool { return less(all[i], all[j]) })

	if args.SortAsTree {
		// Parents before their children, each level in the sort order
		// (RFC 8621 2.3).
		children := map[int64][]store.Mailbox{}
		ids := map[int64]bool{}
		for _, m := range all {
			ids[m.ID] = true
		}
		for _, m := range all {
			parent := m.ParentID
			if !ids[parent] {
				parent = 0
			}
			children[parent] = append(children[parent], m)
		}
		ordered := all[:0:0]
		var walk func(parent int64, depth int)
		walk = func(parent int64, depth int) {
			if depth > MaxMailboxDepth+len(all) {
				return
			}
			for _, m := range children[parent] {
				ordered = append(ordered, m)
				walk(m.ID, depth+1)
			}
		}
		walk(0, 0)
		all = ordered
	}

	byID := map[int64]store.Mailbox{}
	for _, m := range all {
		byID[m.ID] = m
	}
	keep := func(m store.Mailbox) bool {
		if !filter.match(m) {
			return false
		}
		if args.FilterAsTree {
			// A mailbox is only in the results if all its ancestors are.
			for p, depth := m.ParentID, 0; p != 0 && depth <= len(all); depth++ {
				parent, ok := byID[p]
				if !ok || !filter.match(parent) {
					return false
				}
				p = parent.ParentID
			}
		}
		return true
	}
	ids := []string{}
	for _, m := range all {
		if keep(m) {
			ids = append(ids, store.MailboxID(m.ID))
		}
	}
	return ids, nil
}

// mailboxQuery is Mailbox/query (RFC 8621 2.3, RFC 8620 5.5).
func mailboxQuery(c *call, raw json.RawMessage) (any, *methodError) {
	var args mailboxQueryArgs
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
	all, e := c.runMailboxQuery(&args)
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

// queryChangesArgs are what every /queryChanges has besides its filter
// and sort (RFC 8620 5.6).
type queryChangesArgs struct {
	SinceQueryState *string `json:"sinceQueryState"`
	MaxChanges      *int64  `json:"maxChanges"`
	UpToID          *string `json:"upToId"`
	CalculateTotal  bool    `json:"calculateTotal"`
}

// queryChanges builds the answer of a /queryChanges from the ids that may
// have entered, left or moved in the results (candidates) and the results
// as they are now.
//
// It reports every candidate as removed and, if it is in the results now,
// as added at its index. RFC 8620 5.6 allows exactly this: removed may hold
// ids that were not in the old results, and an id whose place may have
// changed "MUST" be in both lists. upToId is not used to shorten the
// answer, which the RFC leaves to the server.
func queryChanges(account string, args queryChangesArgs, since, now int64, candidates []string, results []string) (map[string]any, *methodError) {
	if args.MaxChanges != nil {
		if *args.MaxChanges <= 0 {
			return nil, invalidArgs("maxChanges must be greater than 0")
		}
		if int64(len(candidates)) > *args.MaxChanges {
			return nil, mErr("tooManyChanges", "more changes than maxChanges")
		}
	}
	if len(candidates) > maxChangesLimit {
		return nil, mErr("cannotCalculateChanges", "too much has changed; run the query again")
	}
	isCandidate := map[string]bool{}
	removed := []string{}
	for _, id := range candidates {
		if !isCandidate[id] {
			isCandidate[id] = true
			removed = append(removed, id)
		}
	}
	added := []map[string]any{}
	for i, id := range results {
		if isCandidate[id] {
			added = append(added, map[string]any{"id": id, "index": i})
		}
	}
	out := map[string]any{"accountId": account, "oldQueryState": stateString(since), "newQueryState": stateString(now),
		"removed": removed, "added": added}
	if args.CalculateTotal {
		out["total"] = len(results)
	}
	return out, nil
}

// mailboxQueryChanges is Mailbox/queryChanges (RFC 8621 2.4).
func mailboxQueryChanges(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		mailboxQueryArgs
		queryChangesArgs
	}
	// The embedded query arguments that /queryChanges does not have stay
	// zero; decodeStrict would accept them, so they are refused here.
	var probe map[string]json.RawMessage
	json.Unmarshal(raw, &probe)
	for _, k := range []string{"position", "anchor", "anchorOffset", "limit", "sortAsTree", "filterAsTree"} {
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
	set, now, _, err := c.h.Store.ChangesOf(c.ctx, args.AccountID, "Mailbox", since, 0)
	if err != nil {
		return nil, storeErr(err)
	}
	results, e := c.runMailboxQuery(&args.mailboxQueryArgs)
	if e != nil {
		return nil, e
	}
	var candidates []string
	for _, list := range [][]int64{set.Created, set.Updated, set.Destroyed} {
		for _, id := range list {
			candidates = append(candidates, store.MailboxID(id))
		}
	}
	return queryChanges(args.AccountID, args.queryChangesArgs, since, now, candidates, results)
}

// setError is a SetError (RFC 8620 5.3).
type setError struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties,omitempty"`
	// What some types add (RFC 8621 4.6, 7.5).
	NotFound          []string `json:"notFound,omitempty"`
	InvalidRecipients []string `json:"invalidRecipients,omitempty"`
	MaxRecipients     int      `json:"maxRecipients,omitempty"`
	MaxSize           int64    `json:"maxSize,omitempty"`
}

func invalidProps(description string, props ...string) *setError {
	return &setError{Type: "invalidProperties", Description: description, Properties: props}
}

// setArgs are the arguments every /set shares (RFC 8620 5.3).
type setArgs struct {
	AccountID string                                `json:"accountId"`
	IfInState *string                               `json:"ifInState"`
	Create    map[string]json.RawMessage            `json:"create"`
	Update    map[string]map[string]json.RawMessage `json:"update"`
	Destroy   *[]string                             `json:"destroy"`
}

// setRun carries a /set from its first write to its last.
type setRun struct {
	c        *call
	account  string
	oldState int64
	// ifState is the state the next write must find, until a write has
	// succeeded: from then on the account is in a state this call made.
	ifState  int64
	newState int64
	wrote    bool

	created      map[string]any
	updated      map[string]any
	destroyed    []string
	notCreated   map[string]*setError
	notUpdated   map[string]*setError
	notDestroyed map[string]*setError
}

// begin checks ifInState and the size of a /set.
func (c *call) begin(args *setArgs) (*setRun, *methodError) {
	n := len(args.Create) + len(args.Update)
	if args.Destroy != nil {
		n += len(*args.Destroy)
	}
	if n > c.h.limits.MaxObjectsInSet {
		return nil, mErr("requestTooLarge", "more objects than maxObjectsInSet")
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	run := &setRun{c: c, account: args.AccountID, oldState: state, newState: state,
		notCreated: map[string]*setError{}, notUpdated: map[string]*setError{}, notDestroyed: map[string]*setError{}}
	if args.IfInState != nil {
		want, ok := parseState(*args.IfInState)
		if !ok || want != state {
			return nil, mErr("stateMismatch", "the state is not the ifInState given")
		}
		// The check above is only a first look. The guard travels with the
		// first write, inside its transaction (see store.batch.ifState).
		run.ifState = want
	}
	return run, nil
}

// wroteState records a successful write.
func (r *setRun) wroteState(state int64) {
	r.newState, r.wrote, r.ifState = state, true, 0
}

// result is the response of the /set.
func (r *setRun) result() map[string]any {
	nilIfEmpty := func(m map[string]*setError) any {
		if len(m) == 0 {
			return nil
		}
		return m
	}
	out := map[string]any{"accountId": r.account, "oldState": stateString(r.oldState), "newState": stateString(r.newState),
		"created": nil, "updated": nil, "destroyed": nil,
		"notCreated": nilIfEmpty(r.notCreated), "notUpdated": nilIfEmpty(r.notUpdated), "notDestroyed": nilIfEmpty(r.notDestroyed)}
	if len(r.created) > 0 {
		out["created"] = r.created
	}
	if len(r.updated) > 0 {
		out["updated"] = r.updated
	}
	if len(r.destroyed) > 0 {
		out["destroyed"] = r.destroyed
	}
	return out
}

// mailboxRef reads a mailbox id that may be a "#creationId" reference.
func (c *call) mailboxRef(s string) (int64, bool) {
	if strings.HasPrefix(s, "#") {
		id, ok := c.created[s[1:]]
		if !ok {
			return 0, false
		}
		s = id
	}
	return store.ParseMailboxID(s)
}

// mailboxSetError maps what the store refuses to a SetError.
func mailboxSetError(err error) *setError {
	switch {
	case errors.Is(err, store.ErrNoMailbox):
		return &setError{Type: "notFound"}
	case errors.Is(err, store.ErrMailboxName):
		return invalidProps("the name is empty, too long, or has control characters or space at an end", "name")
	case errors.Is(err, store.ErrMailboxExists):
		return invalidProps("the parent already has a mailbox with that name", "name")
	case errors.Is(err, store.ErrMailboxLoop):
		return invalidProps("a mailbox cannot be moved under itself", "parentId")
	case errors.Is(err, store.ErrMailboxHasChild):
		return &setError{Type: "mailboxHasChild"}
	case errors.Is(err, store.ErrMailboxHasEmail):
		return &setError{Type: "mailboxHasEmail"}
	case errors.Is(err, store.ErrMailboxRole):
		return &setError{Type: "forbidden", Description: "a mailbox with a role cannot be destroyed"}
	}
	return &setError{Type: "serverFail"}
}

// mailboxPatch is what a create or an update may set.
type mailboxPatch struct {
	name       *string
	parentSet  bool
	parent     int64
	sortOrder  *int64
	subscribed *bool
}

// parseMailboxPatch reads the settable properties of a Mailbox.
func (c *call) parseMailboxPatch(obj map[string]json.RawMessage) (mailboxPatch, *setError) {
	var p mailboxPatch
	var bad []string
	for key, v := range obj {
		switch key {
		case "name":
			if json.Unmarshal(v, &p.name) != nil || p.name == nil {
				bad = append(bad, key)
			}
		case "parentId":
			var id *string
			if json.Unmarshal(v, &id) != nil {
				bad = append(bad, key)
				break
			}
			p.parentSet = true
			if id != nil {
				var ok bool
				if p.parent, ok = c.mailboxRef(*id); !ok {
					return p, invalidProps("there is no such parent mailbox", "parentId")
				}
			}
		case "sortOrder":
			if json.Unmarshal(v, &p.sortOrder) != nil || p.sortOrder == nil || *p.sortOrder < 0 || *p.sortOrder > 1<<31 {
				bad = append(bad, key)
			}
		case "isSubscribed":
			if json.Unmarshal(v, &p.subscribed) != nil || p.subscribed == nil {
				bad = append(bad, key)
			}
		case "role":
			// Only null: the roles belong to the mailboxes every account
			// is created with.
			if string(v) != "null" {
				return p, invalidProps("the server does not let a client set a role", "role")
			}
		default:
			bad = append(bad, key)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return p, invalidProps("these properties cannot be set, or have the wrong type", bad...)
	}
	return p, nil
}

// depthUnder returns how deep a mailbox under parent would be, 1 for the
// top level, and how many levels the subtree of id has below it.
func depthUnder(all []store.Mailbox, parent, id int64) int {
	byID := map[int64]store.Mailbox{}
	for _, m := range all {
		byID[m.ID] = m
	}
	depth := 1
	for p, n := parent, 0; p != 0 && n <= len(all); n++ {
		depth++
		p = byID[p].ParentID
	}
	if id == 0 {
		return depth
	}
	var below func(id int64, n int) int
	below = func(id int64, n int) int {
		deepest := 0
		if n > len(all) {
			return 0
		}
		for _, m := range all {
			if m.ParentID == id {
				deepest = max(deepest, 1+below(m.ID, n+1))
			}
		}
		return deepest
	}
	return depth + below(id, 0)
}

// mailboxSet is Mailbox/set (RFC 8621 2.5). Each create, update and
// destroy is one transaction of its own; ifInState guards the first.
func mailboxSet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		setArgs
		OnDestroyRemoveEmails bool `json:"onDestroyRemoveEmails"`
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
	tooDeep := invalidProps("mailboxes cannot nest deeper than maxMailboxDepth", "parentId")

	// Creates. A parent may be created in the same call, so a create whose
	// parent reference does not resolve yet waits for a later round.
	pending := make([]string, 0, len(args.Create))
	for id := range args.Create {
		pending = append(pending, id)
	}
	sort.Strings(pending)
	for len(pending) > 0 {
		var waiting []string
		progress := false
		for _, cid := range pending {
			var obj map[string]json.RawMessage
			if json.Unmarshal(args.Create[cid], &obj) != nil || obj == nil {
				run.notCreated[cid] = invalidProps("a Mailbox must be an object")
				continue
			}
			var parentRef *string
			if json.Unmarshal(obj["parentId"], &parentRef) == nil && parentRef != nil && strings.HasPrefix(*parentRef, "#") {
				if _, resolved := c.created[(*parentRef)[1:]]; !resolved {
					if _, coming := args.Create[(*parentRef)[1:]]; coming && run.notCreated[(*parentRef)[1:]] == nil {
						waiting = append(waiting, cid)
						continue
					}
				}
			}
			progress = true
			p, se := c.parseMailboxPatch(obj)
			if se != nil {
				run.notCreated[cid] = se
				continue
			}
			if p.name == nil {
				run.notCreated[cid] = invalidProps("a mailbox needs a name", "name")
				continue
			}
			all, err := st.Mailboxes(c.ctx, args.AccountID)
			if err != nil {
				return nil, storeErr(err)
			}
			if depthUnder(all, p.parent, 0) > MaxMailboxDepth {
				run.notCreated[cid] = tooDeep
				continue
			}
			n := store.NewMailbox{ParentID: p.parent, Name: *p.name, Subscribed: true, IfInState: run.ifState}
			if p.sortOrder != nil {
				n.SortOrder = *p.sortOrder
			}
			if p.subscribed != nil {
				n.Subscribed = *p.subscribed
			}
			m, err := st.NewMailbox(c.ctx, args.AccountID, n)
			if errors.Is(err, store.ErrStateMismatch) {
				return nil, mismatch
			}
			if errors.Is(err, store.ErrNoMailbox) {
				run.notCreated[cid] = invalidProps("there is no such parent mailbox", "parentId")
				continue
			}
			if err != nil {
				run.notCreated[cid] = mailboxSetError(err)
				continue
			}
			run.wroteState(m.Modseq)
			id := store.MailboxID(m.ID)
			c.created[cid] = id
			if run.created == nil {
				run.created = map[string]any{}
			}
			// The id and what the server chose (RFC 8620 5.3).
			obj2 := mailboxObject(m)
			run.created[cid] = pick(obj2, []string{"totalEmails", "unreadEmails", "totalThreads", "unreadThreads",
				"myRights", "sortOrder", "isSubscribed", "role", "parentId"})
		}
		if !progress {
			for _, cid := range waiting {
				run.notCreated[cid] = invalidProps("there is no such parent mailbox", "parentId")
			}
			break
		}
		pending = waiting
	}

	// Updates.
	updates := make([]string, 0, len(args.Update))
	for id := range args.Update {
		updates = append(updates, id)
	}
	sort.Strings(updates)
	for _, sid := range updates {
		id, ok := store.ParseMailboxID(sid)
		if !ok {
			run.notUpdated[sid] = &setError{Type: "notFound"}
			continue
		}
		p, se := c.parseMailboxPatch(args.Update[sid])
		if se != nil {
			run.notUpdated[sid] = se
			continue
		}
		u := store.MailboxUpdate{Name: p.name, SortOrder: p.sortOrder, Subscribed: p.subscribed, IfInState: run.ifState}
		if p.parentSet {
			u.ParentID = &p.parent
			all, err := st.Mailboxes(c.ctx, args.AccountID)
			if err != nil {
				return nil, storeErr(err)
			}
			if p.parent != id && depthUnder(all, p.parent, id) > MaxMailboxDepth {
				run.notUpdated[sid] = tooDeep
				continue
			}
		}
		state, err := st.UpdateMailbox(c.ctx, args.AccountID, id, u)
		if errors.Is(err, store.ErrStateMismatch) {
			return nil, mismatch
		}
		if err != nil {
			run.notUpdated[sid] = mailboxSetError(err)
			continue
		}
		run.wroteState(state)
		if run.updated == nil {
			run.updated = map[string]any{}
		}
		run.updated[sid] = nil
	}

	// Destroys. A child may be listed after its parent; trying the ones
	// that failed with mailboxHasChild again once handles any order.
	if args.Destroy != nil {
		todo := append([]string{}, *args.Destroy...)
		for round := 0; len(todo) > 0 && round <= len(*args.Destroy); round++ {
			var again []string
			for _, sid := range todo {
				id, ok := store.ParseMailboxID(sid)
				if !ok {
					run.notDestroyed[sid] = &setError{Type: "notFound"}
					continue
				}
				state, err := st.DestroyMailbox(c.ctx, args.AccountID, id, args.OnDestroyRemoveEmails, run.ifState)
				if errors.Is(err, store.ErrStateMismatch) {
					return nil, mismatch
				}
				if errors.Is(err, store.ErrMailboxHasChild) {
					again = append(again, sid)
					run.notDestroyed[sid] = mailboxSetError(err)
					continue
				}
				if err != nil {
					run.notDestroyed[sid] = mailboxSetError(err)
					continue
				}
				delete(run.notDestroyed, sid)
				run.wroteState(state)
				run.destroyed = append(run.destroyed, sid)
			}
			if len(again) == len(todo) {
				break
			}
			todo = again
		}
	}
	return run.result(), nil
}
