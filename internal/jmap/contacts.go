package jmap

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/jscal"
	"github.com/robinmail/robinmail/internal/jscontact"
	"github.com/robinmail/robinmail/internal/pim"
	"github.com/robinmail/robinmail/internal/store"
)

var addressBookProperties = []string{"id", "name", "description", "sortOrder", "isDefault", "isSubscribed", "shareWith", "myRights"}

// addressBookRights is the AddressBookRights object (RFC 9610 2).
func addressBookRights(r string) map[string]bool {
	all := r == pim.RightOwner
	return map[string]bool{"mayRead": true, "mayWrite": all || r == store.RightWrite, "mayShare": all, "mayDelete": all}
}

func (c *call) addressBookObject(p *pimCtx, a pim.Access) map[string]any {
	col := a.Collection
	var descr any
	if col.Description != "" {
		descr = col.Description
	}
	obj := map[string]any{"id": store.AddressBookID(col.ID), "name": col.Name, "description": descr, "sortOrder": col.SortOrder,
		"isDefault": col.IsDefault && p.own, "isSubscribed": col.IsSubscribed || !p.own, "shareWith": nil, "myRights": addressBookRights(a.Rights)}
	if a.Rights == pim.RightOwner {
		obj["shareWith"] = c.sharesOf(p.owner.ID, col.ID, addressBookRights)
	}
	return obj
}

func addressBookGet(c *call, raw json.RawMessage) (any, *methodError) {
	return c.collectionGet(raw, store.KindAddressBook, addressBookProperties, store.AddressBookID, c.addressBookObject)
}

func addressBookChanges(c *call, raw json.RawMessage) (any, *methodError) {
	return c.collectionChanges(raw, "AddressBook", store.KindAddressBook, store.AddressBookID)
}

func addressBookSet(c *call, raw json.RawMessage) (any, *methodError) {
	return c.collectionSet(raw, store.KindAddressBook, store.AddressBookID, store.ParseAddressBookID)
}

// cardData is a stored card, parsed.
type cardData struct {
	obj  store.Object
	acc  pim.Access
	card *ical.Component
	js   map[string]any
}

func (c *call) parseCard(p *pimCtx, o store.Object) *cardData {
	a, ok := p.byID(o.CollectionID)
	if !ok || o.Kind != store.ObjectCard {
		return nil
	}
	v, err := ical.ParseOne([]byte(o.Data), "VCARD")
	if err != nil {
		return nil
	}
	return &cardData{obj: o, acc: a, card: v, js: jscal.Normalize(jscontact.ToJS(v)).(map[string]any)}
}

func cardObject(d *cardData) map[string]any {
	obj := jscal.DeepCopy(d.js).(map[string]any)
	obj["id"] = store.ContactCardID(d.obj.ID)
	obj["addressBookIds"] = map[string]any{store.AddressBookID(d.obj.CollectionID): true}
	return obj
}

// contactCardGet is ContactCard/get (RFC 9610 3.1).
func contactCardGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	p, e := c.open(raw, &args, &args.AccountID, store.KindAddressBook)
	if e != nil {
		return nil, e
	}
	state, e := c.pimState(p)
	if e != nil {
		return nil, e
	}
	var objs []store.Object
	var err error
	if args.IDs == nil {
		objs, err = c.h.PIM.Store.Objects(c.ctx, p.owner.ID, store.ObjectFilter{Kind: store.ObjectCard, Collections: p.ids(), WithData: true})
		if err == nil && len(objs) > c.h.limits.MaxObjectsInGet*20 {
			return nil, mErr("requestTooLarge", "too many cards; ask for ids")
		}
	} else {
		if len(*args.IDs) > c.h.limits.MaxObjectsInGet {
			return nil, mErr("requestTooLarge", "more ids than maxObjectsInGet")
		}
		var ids []int64
		for _, s := range *args.IDs {
			if id, ok := store.ParseContactCardID(c.idRef(s)); ok {
				ids = append(ids, id)
			}
		}
		objs, err = c.h.PIM.Store.ObjectsByID(c.ctx, p.owner.ID, ids)
	}
	if err != nil {
		return nil, storeErr(err)
	}
	byID := map[string]*cardData{}
	var order []string
	for _, o := range objs {
		if d := c.parseCard(p, o); d != nil {
			id := store.ContactCardID(o.ID)
			byID[id] = d
			order = append(order, id)
		}
	}
	pickCard := func(obj map[string]any) map[string]any {
		if args.Properties == nil {
			return obj
		}
		out := map[string]any{"id": obj["id"]}
		for _, k := range *args.Properties {
			if v, ok := obj[k]; ok {
				out[k] = v
			} else {
				out[k] = nil
			}
		}
		return out
	}
	list, notFound := []map[string]any{}, []string{}
	if args.IDs == nil {
		for _, id := range order {
			list = append(list, pickCard(cardObject(byID[id])))
		}
	} else {
		seen := map[string]bool{}
		for _, s := range *args.IDs {
			if seen[s] {
				continue
			}
			seen[s] = true
			if d, ok := byID[c.idRef(s)]; ok {
				list = append(list, pickCard(cardObject(d)))
			} else {
				notFound = append(notFound, s)
			}
		}
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

func contactCardChanges(c *call, raw json.RawMessage) (any, *methodError) {
	return c.pimChanges(raw, "ContactCard", store.KindAddressBook, store.ContactCardID)
}

// cardFromJMAP takes addressBookIds and id off a ContactCard.
func (c *call) cardFromJMAP(p *pimCtx, obj map[string]any, create bool) (map[string]any, int64, *setError) {
	var book int64
	if v, ok := obj["addressBookIds"]; ok {
		m, ok := v.(map[string]any)
		if !ok || len(m) != 1 {
			return nil, 0, invalidProps("addressBookIds must name one address book", "addressBookIds")
		}
		for k, t := range m {
			id, ok := store.ParseAddressBookID(c.idRef(k))
			if _, found := p.byID(id); !ok || t != true || !found {
				return nil, 0, invalidProps("no such address book", "addressBookIds")
			}
			book = id
		}
	} else if create {
		for _, a := range p.colls {
			if a.Collection.IsDefault || book == 0 {
				book = a.Collection.ID
			}
		}
		if book == 0 {
			return nil, 0, invalidProps("addressBookIds is required", "addressBookIds")
		}
	}
	out := map[string]any{}
	for k, v := range obj {
		if k != "id" && k != "addressBookIds" && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["@type"]; !ok {
		out["@type"] = "Card"
	}
	if _, ok := out["version"]; !ok {
		out["version"] = "1.0"
	}
	return out, book, nil
}

// contactCardSet is ContactCard/set (RFC 9610 3.5).
func contactCardSet(c *call, raw json.RawMessage) (any, *methodError) {
	var args setArgs
	p, e := c.open(raw, &args, &args.AccountID, store.KindAddressBook)
	if e != nil {
		return nil, e
	}
	run, e := c.begin(&args)
	if e != nil {
		return nil, e
	}
	now := c.h.now().UTC().Truncate(time.Second)
	mismatch := mErr("stateMismatch", "the state is not the ifInState given")
	put := func(acc pim.Access, id int64, name string, card map[string]any, prev *ical.Component) (store.Object, *setError, bool) {
		v, err := jscontact.ToVCard(card, prev, now)
		if err != nil {
			if pe, ok := err.(*jscontact.PropError); ok {
				return store.Object{}, invalidProps(pe.Msg, pe.Props...), false
			}
			return store.Object{}, invalidProps(err.Error()), false
		}
		o, err := c.h.PIM.PutCard(c.ctx, acc, name, v, pim.Cond{ID: id, IfNoneMatch: id == 0, IfState: run.ifState})
		if err == store.ErrStateMismatch {
			return o, nil, true
		}
		if err != nil {
			return o, pimSetError(err), false
		}
		return o, nil, false
	}
	for _, cid := range sortedKeys(args.Create) {
		var obj map[string]any
		if json.Unmarshal(args.Create[cid], &obj) != nil || obj == nil {
			run.notCreated[cid] = invalidProps("not an object")
			continue
		}
		card, book, se := c.cardFromJMAP(p, obj, true)
		if se != nil {
			run.notCreated[cid] = se
			continue
		}
		uid, _ := card["uid"].(string)
		if uid == "" {
			uid = "urn:uuid:" + pim.NewUID()
			card["uid"] = uid
		}
		if _, ok := card["created"]; !ok {
			card["created"] = jscal.UTCDateTime(now)
		}
		acc, _ := p.byID(book)
		o, se, stale := put(acc, 0, pim.NewName(strings.TrimPrefix(uid, "urn:uuid:"), ".vcf"), card, nil)
		if stale {
			return nil, mismatch
		}
		if se != nil {
			run.notCreated[cid] = se
			continue
		}
		st, _ := c.h.Store.State(c.ctx, p.owner.ID)
		run.wroteState(st)
		if run.created == nil {
			run.created = map[string]any{}
		}
		id := store.ContactCardID(o.ID)
		run.created[cid] = map[string]any{"id": id, "uid": uid}
		c.created[cid] = id
	}
	for _, sid := range sortedKeys(args.Update) {
		id, ok := store.ParseContactCardID(c.idRef(sid))
		if !ok {
			run.notUpdated[sid] = &setError{Type: "notFound"}
			continue
		}
		objs, err := c.h.PIM.Store.ObjectsByID(c.ctx, p.owner.ID, []int64{id})
		if err != nil {
			return nil, storeErr(err)
		}
		var cur *cardData
		if len(objs) == 1 {
			cur = c.parseCard(p, objs[0])
		}
		if cur == nil {
			run.notUpdated[sid] = &setError{Type: "notFound"}
			continue
		}
		patch := map[string]any{}
		for k, v := range args.Update[sid] {
			var x any
			json.Unmarshal(v, &x)
			patch[k] = x
		}
		next, err := jscal.ApplyPatch(cardObject(cur), patch)
		if err != nil {
			run.notUpdated[sid] = invalidProps(err.Error())
			continue
		}
		if next["uid"] != cur.js["uid"] {
			run.notUpdated[sid] = invalidProps("uid cannot change", "uid")
			continue
		}
		card, book, se := c.cardFromJMAP(p, next, false)
		if se != nil {
			run.notUpdated[sid] = se
			continue
		}
		o, se, stale := put(cur.acc, cur.obj.ID, cur.obj.Name, card, cur.card)
		if stale {
			return nil, mismatch
		}
		if se != nil {
			run.notUpdated[sid] = se
			continue
		}
		if book != 0 && book != cur.obj.CollectionID {
			if to, _ := p.byID(book); !to.MayWrite() {
				run.notUpdated[sid] = &setError{Type: "forbidden"}
				continue
			}
			if _, err := c.h.PIM.Store.MoveObject(c.ctx, p.owner.ID, o.ID, book, "", "", 0); err != nil {
				run.notUpdated[sid] = pimSetError(err)
				continue
			}
		}
		st, _ := c.h.Store.State(c.ctx, p.owner.ID)
		run.wroteState(st)
		if run.updated == nil {
			run.updated = map[string]any{}
		}
		run.updated[sid] = nil
	}
	if args.Destroy != nil {
		for _, sid := range *args.Destroy {
			id, ok := store.ParseContactCardID(c.idRef(sid))
			objs, err := c.h.PIM.Store.ObjectsByID(c.ctx, p.owner.ID, []int64{id})
			if !ok || err != nil || len(objs) == 0 {
				run.notDestroyed[sid] = &setError{Type: "notFound"}
				continue
			}
			a, found := p.byID(objs[0].CollectionID)
			if !found || objs[0].Kind != store.ObjectCard {
				run.notDestroyed[sid] = &setError{Type: "notFound"}
				continue
			}
			if !a.MayWrite() {
				run.notDestroyed[sid] = &setError{Type: "forbidden"}
				continue
			}
			st, err := c.h.PIM.Store.DeleteObject(c.ctx, p.owner.ID, id, "", run.ifState)
			if err == store.ErrStateMismatch {
				return nil, mismatch
			}
			if err != nil {
				run.notDestroyed[sid] = pimSetError(err)
				continue
			}
			run.wroteState(st)
			run.destroyed = append(run.destroyed, sid)
		}
	}
	return run.result(), nil
}

// cardFilter is a FilterCondition of ContactCard/query (RFC 9610 3.3.1).
type cardFilter struct {
	Operator      string       `json:"operator"`
	Conditions    []cardFilter `json:"conditions"`
	InAddressBook *string      `json:"inAddressBook"`
	UID           *string      `json:"uid"`
	HasMember     *string      `json:"hasMember"`
	Kind          *string      `json:"kind"`
	CreatedBefore *string      `json:"createdBefore"`
	CreatedAfter  *string      `json:"createdAfter"`
	UpdatedBefore *string      `json:"updatedBefore"`
	UpdatedAfter  *string      `json:"updatedAfter"`
	Text          *string      `json:"text"`
	Name          *string      `json:"name"`
	NameGiven     *string      `json:"name/given"`
	NameSurname   *string      `json:"name/surname"`
	NameSurname2  *string      `json:"name/surname2"`
	Nickname      *string      `json:"nickname"`
	Organization  *string      `json:"organization"`
	Email         *string      `json:"email"`
	Phone         *string      `json:"phone"`
	OnlineService *string      `json:"onlineService"`
	Address       *string      `json:"address"`
	Note          *string      `json:"note"`
}

// texts collects the strings at a key of every entry of a map property.
func texts(card map[string]any, prop string, keys ...string) string {
	var b strings.Builder
	m, _ := card[prop].(map[string]any)
	for _, x := range m {
		e, _ := x.(map[string]any)
		for _, k := range keys {
			if s, ok := e[k].(string); ok {
				b.WriteString(" " + s)
			}
		}
		if comps, ok := e["components"].([]any); ok {
			for _, c := range comps {
				cm, _ := c.(map[string]any)
				s, _ := cm["value"].(string)
				b.WriteString(" " + s)
			}
		}
		if units, ok := e["units"].([]any); ok {
			for _, u := range units {
				um, _ := u.(map[string]any)
				s, _ := um["name"].(string)
				b.WriteString(" " + s)
			}
		}
	}
	return b.String()
}

func nameText(card map[string]any, kind string) string {
	name, _ := card["name"].(map[string]any)
	var b strings.Builder
	if kind == "" {
		if s, ok := name["full"].(string); ok {
			b.WriteString(s)
		}
	}
	comps, _ := name["components"].([]any)
	for _, c := range comps {
		m, _ := c.(map[string]any)
		if kind == "" || m["kind"] == kind {
			s, _ := m["value"].(string)
			b.WriteString(" " + s)
		}
	}
	return b.String()
}

func (c *call) cardMatch(f *cardFilter, card map[string]any) bool {
	switch f.Operator {
	case "AND", "OR", "NOT":
		for i := range f.Conditions {
			m := c.cardMatch(&f.Conditions[i], card)
			switch {
			case f.Operator == "AND" && !m:
				return false
			case f.Operator == "OR" && m:
				return true
			case f.Operator == "NOT" && m:
				return false
			}
		}
		return f.Operator != "OR"
	}
	str := func(k string) string { s, _ := card[k].(string); return s }
	if f.InAddressBook != nil {
		ids, _ := card["addressBookIds"].(map[string]any)
		if ids[c.idRef(*f.InAddressBook)] != true {
			return false
		}
	}
	if f.UID != nil && str("uid") != *f.UID {
		return false
	}
	if f.Kind != nil {
		k := str("kind")
		if k == "" {
			k = "individual"
		}
		if k != *f.Kind {
			return false
		}
	}
	if f.HasMember != nil {
		m, _ := card["members"].(map[string]any)
		if m[*f.HasMember] != true {
			return false
		}
	}
	for _, x := range []struct {
		v      *string
		key    string
		before bool
	}{{f.CreatedBefore, "created", true}, {f.CreatedAfter, "created", false}, {f.UpdatedBefore, "updated", true}, {f.UpdatedAfter, "updated", false}} {
		if x.v == nil {
			continue
		}
		t, ok := jscal.ParseUTC(*x.v)
		have, ok2 := jscal.ParseUTC(str(x.key))
		if !ok || !ok2 || (x.before && !have.Before(t)) || (!x.before && have.Before(t)) {
			return false
		}
	}
	checks := []struct {
		v    *string
		have string
	}{
		{f.Name, nameText(card, "")}, {f.NameGiven, nameText(card, "given")}, {f.NameSurname, nameText(card, "surname")},
		{f.NameSurname2, nameText(card, "surname2")}, {f.Nickname, texts(card, "nicknames", "name")},
		{f.Organization, texts(card, "organizations", "name")}, {f.Email, texts(card, "emails", "address", "label")},
		{f.Phone, texts(card, "phones", "number", "label")}, {f.OnlineService, texts(card, "onlineServices", "uri", "user", "service")},
		{f.Address, texts(card, "addresses", "full")}, {f.Note, texts(card, "notes", "note")},
	}
	for _, ch := range checks {
		if ch.v != nil && !containsFold(ch.have, *ch.v) {
			return false
		}
	}
	if f.Text != nil {
		all := nameText(card, "") + texts(card, "nicknames", "name") + texts(card, "organizations", "name") +
			texts(card, "emails", "address") + texts(card, "phones", "number") + texts(card, "addresses", "full") +
			texts(card, "notes", "note") + texts(card, "titles", "name")
		if !containsFold(all, *f.Text) {
			return false
		}
	}
	return true
}

// contactCardQuery is ContactCard/query (RFC 9610 3.3).
func contactCardQuery(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID string `json:"accountId"`
		queryArgs
		Filter *cardFilter `json:"filter"`
		Sort   []sortArg   `json:"sort"`
	}
	p, e := c.open(raw, &args, &args.AccountID, store.KindAddressBook)
	if e != nil {
		return nil, e
	}
	limit, clamped, e := args.limit()
	if e != nil {
		return nil, e
	}
	for _, s := range args.Sort {
		switch s.Property {
		case "created", "updated", "name/given", "name/surname", "name/surname2":
		default:
			return nil, mErr("unsupportedSort", "cannot sort by %q", s.Property)
		}
	}
	state, e := c.pimState(p)
	if e != nil {
		return nil, e
	}
	objs, err := c.h.PIM.Store.Objects(c.ctx, p.owner.ID, store.ObjectFilter{Kind: store.ObjectCard, Collections: p.ids(), WithData: true})
	if err != nil {
		return nil, storeErr(err)
	}
	f := args.Filter
	if f == nil {
		f = &cardFilter{}
	}
	type hit struct {
		id   string
		card map[string]any
	}
	var hits []hit
	for _, o := range objs {
		d := c.parseCard(p, o)
		if d == nil {
			continue
		}
		card := cardObject(d)
		if c.cardMatch(f, card) {
			hits = append(hits, hit{store.ContactCardID(o.ID), card})
		}
	}
	key := func(card map[string]any, prop string) string {
		switch prop {
		case "name/given":
			return strings.ToLower(strings.TrimSpace(nameText(card, "given")))
		case "name/surname":
			return strings.ToLower(strings.TrimSpace(nameText(card, "surname")))
		case "name/surname2":
			return strings.ToLower(strings.TrimSpace(nameText(card, "surname2")))
		}
		s, _ := card[prop].(string)
		return s
	}
	sorts := args.Sort
	if len(sorts) == 0 {
		// Without a sort: by name, which is what a list of contacts wants.
		sort.SliceStable(hits, func(i, j int) bool {
			ni, nj := strings.ToLower(strings.TrimSpace(nameText(hits[i].card, ""))), strings.ToLower(strings.TrimSpace(nameText(hits[j].card, "")))
			if ni != nj {
				return ni < nj
			}
			return hits[i].id < hits[j].id
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		for _, s := range sorts {
			a, b := key(hits[i].card, s.Property), key(hits[j].card, s.Property)
			if a == b {
				continue
			}
			return (a < b) == s.ascending()
		}
		return false
	})
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	pos, page, e := window(ids, args.Position, args.Anchor, args.AnchorOffset, limit)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"accountId": args.AccountID, "queryState": stateString(state), "canCalculateChanges": false, "position": pos, "ids": page}
	if args.CalculateTotal {
		out["total"] = len(ids)
	}
	if clamped {
		out["limit"] = limit
	}
	return out, nil
}
