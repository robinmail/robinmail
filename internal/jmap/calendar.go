package jmap

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/jscal"
	"github.com/robinmail/robinmail/internal/pim"
	"github.com/robinmail/robinmail/internal/store"
)

var calendarProperties = []string{"id", "name", "description", "color", "sortOrder", "isSubscribed", "isVisible", "isDefault",
	"includeInAvailability", "defaultAlertsWithTime", "defaultAlertsWithoutTime", "timeZone", "shareWith", "myRights"}

// calendarRights is the CalendarRights object (JMAP Calendars 4) of a right.
func calendarRights(r string) map[string]bool {
	all := r == pim.RightOwner
	write := all || r == store.RightWrite
	return map[string]bool{"mayReadFreeBusy": true, "mayReadItems": true, "mayWriteAll": write, "mayWriteOwn": write,
		"mayUpdatePrivate": write, "mayRSVP": write, "mayShare": all, "mayDelete": all}
}

func (c *call) calendarObject(p *pimCtx, a pim.Access) map[string]any {
	col := a.Collection
	var tz any
	if col.TimeZone != "" {
		tz = col.TimeZone
	}
	var color, descr any
	if col.Color != "" {
		color = col.Color
	}
	if col.Description != "" {
		descr = col.Description
	}
	obj := map[string]any{"id": store.CalendarID(col.ID), "name": col.Name, "description": descr, "color": color,
		"sortOrder": col.SortOrder, "isSubscribed": col.IsSubscribed || !p.own, "isVisible": col.IsVisible,
		"isDefault": col.IsDefault && p.own, "includeInAvailability": col.IncludeInAvailability,
		"defaultAlertsWithTime": nil, "defaultAlertsWithoutTime": nil, "timeZone": tz, "shareWith": nil,
		"myRights": calendarRights(a.Rights)}
	if a.Rights == pim.RightOwner {
		obj["shareWith"] = c.sharesOf(p.owner.ID, col.ID, calendarRights)
	}
	if !p.own {
		obj["includeInAvailability"] = "none"
	}
	return obj
}

// collectionGet answers Calendar/get and AddressBook/get.
func (c *call) collectionGet(raw json.RawMessage, kind string, props []string, format func(int64) string,
	object func(*pimCtx, pim.Access) map[string]any) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	p, e := c.open(raw, &args, &args.AccountID, kind)
	if e != nil {
		return nil, e
	}
	want, e := checkProperties(args.Properties, props)
	if e != nil {
		return nil, e
	}
	state, e := c.pimState(p)
	if e != nil {
		return nil, e
	}
	list, notFound := []map[string]any{}, []string{}
	byID := map[string]pim.Access{}
	for _, a := range p.colls {
		byID[format(a.Collection.ID)] = a
	}
	if args.IDs == nil {
		for _, a := range p.colls {
			list = append(list, pick(object(p, a), want))
		}
	} else {
		for _, id := range *args.IDs {
			if a, ok := byID[c.idRef(id)]; ok {
				list = append(list, pick(object(p, a), want))
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

func calendarGet(c *call, raw json.RawMessage) (any, *methodError) {
	return c.collectionGet(raw, store.KindCalendar, calendarProperties, store.CalendarID, c.calendarObject)
}

func calendarChanges(c *call, raw json.RawMessage) (any, *methodError) {
	return c.collectionChanges(raw, "Calendar", store.KindCalendar, store.CalendarID)
}

// collectionPatch reads what a client may set of a Calendar or an
// AddressBook.
func collectionPatch(obj map[string]json.RawMessage, calendar bool) (store.CollectionUpdate, map[string]string, *setError) {
	var u store.CollectionUpdate
	var shares map[string]string
	var bad []string
	str := func(k string, nullable bool) *string {
		raw := obj[k]
		if nullable && string(raw) == "null" {
			e := ""
			return &e
		}
		var s string
		if json.Unmarshal(raw, &s) != nil || len(s) > 4096 {
			bad = append(bad, k)
			return nil
		}
		return &s
	}
	boolean := func(k string) *bool {
		var b bool
		if json.Unmarshal(obj[k], &b) != nil {
			bad = append(bad, k)
			return nil
		}
		return &b
	}
	for _, k := range sortedKeys(obj) {
		switch k {
		case "name":
			u.Name = str(k, false)
		case "description":
			u.Description = str(k, true)
		case "color":
			if calendar {
				u.Color = str(k, true)
			} else {
				bad = append(bad, k)
			}
		case "sortOrder":
			var n int64
			if json.Unmarshal(obj[k], &n) != nil || n < 0 || n >= 1<<31 {
				bad = append(bad, k)
				continue
			}
			u.SortOrder = &n
		case "isSubscribed":
			u.IsSubscribed = boolean(k)
		case "isVisible":
			if calendar {
				u.IsVisible = boolean(k)
			} else {
				bad = append(bad, k)
			}
		case "includeInAvailability":
			s := str(k, false)
			if s != nil && *s != "all" && *s != "attending" && *s != "none" {
				bad = append(bad, k)
				continue
			}
			u.IncludeInAvailability = s
		case "timeZone":
			s := str(k, true)
			if s != nil && *s != "" && ical.LoadZone(*s) == nil {
				bad = append(bad, k)
				continue
			}
			u.TimeZone = s
		case "defaultAlertsWithTime", "defaultAlertsWithoutTime":
			// Kept by clients themselves; robinmail does not deliver alerts.
			if string(obj[k]) != "null" {
				bad = append(bad, k)
			}
		case "shareWith":
			s, ok := shareRights(obj[k])
			if !ok {
				bad = append(bad, k)
				continue
			}
			shares = s
		case "id", "isDefault", "myRights":
			bad = append(bad, k)
		default:
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		return u, nil, invalidProps("these properties cannot be set as given", bad...)
	}
	return u, shares, nil
}

// collectionSet answers Calendar/set and AddressBook/set.
func (c *call) collectionSet(raw json.RawMessage, kind string, format func(int64) string, parse func(string) (int64, bool)) (any, *methodError) {
	var args struct {
		setArgs
		OnDestroyRemoveEvents   bool    `json:"onDestroyRemoveEvents"`
		OnDestroyRemoveContents bool    `json:"onDestroyRemoveContents"`
		OnSuccessSetIsDefault   *string `json:"onSuccessSetIsDefault"`
	}
	p, e := c.open(raw, &args, &args.AccountID, kind)
	if e != nil {
		return nil, e
	}
	run, e := c.begin(&args.setArgs)
	if e != nil {
		return nil, e
	}
	calendar := kind == store.KindCalendar
	mismatch := mErr("stateMismatch", "the state is not the ifInState given")
	for _, cid := range sortedKeys(args.Create) {
		if !p.own {
			run.notCreated[cid] = &setError{Type: "forbidden", Description: "you cannot make one in another's account"}
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(args.Create[cid], &obj) != nil || obj == nil {
			run.notCreated[cid] = invalidProps("not an object")
			continue
		}
		u, shares, se := collectionPatch(obj, calendar)
		if se != nil {
			run.notCreated[cid] = se
			continue
		}
		if u.Name == nil {
			run.notCreated[cid] = invalidProps("name is required", "name")
			continue
		}
		col, err := c.h.PIM.Store.CreateCollection(c.ctx, p.owner.ID, kind, u, run.ifState)
		if err == store.ErrStateMismatch {
			return nil, mismatch
		}
		if err != nil {
			run.notCreated[cid] = pimSetError(err)
			continue
		}
		if shares != nil {
			if err := c.applyShares(p.owner.ID, col.ID, shares); err != nil {
				run.notCreated[cid] = pimSetError(err)
				continue
			}
		}
		st, _ := c.h.Store.State(c.ctx, p.owner.ID)
		run.wroteState(st)
		if run.created == nil {
			run.created = map[string]any{}
		}
		id := format(col.ID)
		run.created[cid] = map[string]any{"id": id, "isDefault": col.IsDefault, "myRights": calendarRightsFor(kind, pim.RightOwner)}
		c.created[cid] = id
	}
	for _, sid := range sortedKeys(args.Update) {
		id, ok := parse(c.idRef(sid))
		a, found := p.byID(id)
		if !ok || !found {
			run.notUpdated[sid] = &setError{Type: "notFound"}
			continue
		}
		u, shares, se := collectionPatch(args.Update[sid], calendar)
		if se != nil {
			run.notUpdated[sid] = se
			continue
		}
		if a.Rights != pim.RightOwner {
			// A sharee may only subscribe or not: kept as their own choice
			// is not possible here, so it is accepted and ignored.
			onlyMine := len(args.Update[sid]) > 0
			for k := range args.Update[sid] {
				if k != "isSubscribed" && k != "isVisible" && k != "color" && k != "sortOrder" {
					onlyMine = false
				}
			}
			if !onlyMine {
				run.notUpdated[sid] = &setError{Type: "forbidden", Description: "only the owner changes this"}
				continue
			}
			if run.updated == nil {
				run.updated = map[string]any{}
			}
			run.updated[sid] = nil
			continue
		}
		if shares != nil {
			if err := c.applyShares(p.owner.ID, id, shares); err != nil {
				run.notUpdated[sid] = pimSetError(err)
				continue
			}
		}
		st, err := c.h.PIM.Store.UpdateCollection(c.ctx, p.owner.ID, id, u, run.ifState)
		if err == store.ErrStateMismatch {
			return nil, mismatch
		}
		if err != nil {
			run.notUpdated[sid] = pimSetError(err)
			continue
		}
		run.wroteState(st)
		if run.updated == nil {
			run.updated = map[string]any{}
		}
		run.updated[sid] = nil
	}
	if args.Destroy != nil {
		for _, sid := range *args.Destroy {
			id, ok := parse(c.idRef(sid))
			a, found := p.byID(id)
			if !ok || !found {
				run.notDestroyed[sid] = &setError{Type: "notFound"}
				continue
			}
			if a.Rights != pim.RightOwner {
				run.notDestroyed[sid] = &setError{Type: "forbidden", Description: "only the owner destroys it"}
				continue
			}
			// JMAP Calendars 4.3: destroying removes no events unless asked,
			// and sends no scheduling messages either way.
			st, err := c.h.PIM.Store.DestroyCollection(c.ctx, p.owner.ID, id, args.OnDestroyRemoveEvents || args.OnDestroyRemoveContents, run.ifState)
			if err == store.ErrStateMismatch {
				return nil, mismatch
			}
			if err != nil {
				se := pimSetError(err)
				if !calendar && se.Type == "calendarHasEvent" {
					se.Type = "addressBookHasContents"
				}
				run.notDestroyed[sid] = se
				continue
			}
			run.wroteState(st)
			run.destroyed = append(run.destroyed, sid)
		}
	}
	if args.OnSuccessSetIsDefault != nil && p.own && len(run.notCreated)+len(run.notUpdated)+len(run.notDestroyed) == 0 {
		if id, ok := parse(c.idRef(*args.OnSuccessSetIsDefault)); ok {
			if st, err := c.h.PIM.Store.UpdateCollection(c.ctx, p.owner.ID, id, store.CollectionUpdate{MakeDefault: true}, 0); err == nil {
				run.wroteState(st)
				if run.updated == nil {
					run.updated = map[string]any{}
				}
				run.updated[format(id)] = map[string]any{"isDefault": true}
			}
		}
	}
	return run.result(), nil
}

func calendarRightsFor(kind, r string) map[string]bool {
	if kind == store.KindAddressBook {
		return addressBookRights(r)
	}
	return calendarRights(r)
}

func calendarSet(c *call, raw json.RawMessage) (any, *methodError) {
	return c.collectionSet(raw, store.KindCalendar, store.CalendarID, store.ParseCalendarID)
}

// eventIDs: a CalendarEvent's id is its object's; an instance of a
// recurring event made by expandRecurrences is "<id>_<recurrence id>",
// the recurrence id written as yyyymmddThhmmss (JMAP Calendars 5.1 leaves
// the form to the server).
func instanceID(base int64, rid string) string {
	return store.CalendarEventID(base) + "_" + strings.NewReplacer("-", "", ":", "").Replace(rid)
}

func parseEventID(s string) (base int64, rid string, ok bool) {
	b, r, inst := strings.Cut(s, "_")
	base, ok = store.ParseCalendarEventID(b)
	if !ok || !inst {
		return base, "", ok
	}
	t, err := time.Parse("20060102T150405", r)
	if err != nil {
		return 0, "", false
	}
	return base, t.Format(jscal.LocalFormat), true
}

// eventData is a stored event, parsed.
type eventData struct {
	obj  store.Object
	acc  pim.Access
	cal  *ical.Component
	o    *jscal.Object
	js   jscal.Event
	zone *time.Location
}

func (c *call) loadEvents(p *pimCtx, ids []int64) (map[int64]*eventData, *methodError) {
	objs, err := c.h.PIM.Store.ObjectsByID(c.ctx, p.owner.ID, ids)
	if err != nil {
		return nil, storeErr(err)
	}
	out := map[int64]*eventData{}
	for _, o := range objs {
		if e := c.parseEvent(p, o); e != nil {
			out[o.ID] = e
		}
	}
	return out, nil
}

func (c *call) parseEvent(p *pimCtx, o store.Object) *eventData {
	a, ok := p.byID(o.CollectionID)
	if !ok || o.Kind != store.ObjectEvent {
		return nil
	}
	cal, err := ical.ParseOne([]byte(o.Data), "VCALENDAR")
	if err != nil {
		return nil
	}
	zone := time.UTC
	if a.Collection.TimeZone != "" {
		if l := ical.LoadZone(a.Collection.TimeZone); l != nil {
			zone = l
		}
	}
	ob := jscal.Read(cal, zone)
	return &eventData{obj: o, acc: a, cal: cal, o: ob, js: jscal.Normalize(jscal.ToJS(ob)).(map[string]any), zone: zone}
}

// isOrigin: the event's organizer is the account, or it has none (JMAP
// Calendars 5.1).
func (c *call) isOrigin(p *pimCtx, ev jscal.Event) bool {
	org, _ := ev["organizerCalendarAddress"].(string)
	if org == "" {
		return true
	}
	for _, a := range c.h.PIM.Addresses(c.ctx, p.owner.Address) {
		if pim.SameAddress(a, org) {
			return true
		}
	}
	return false
}

// eventObject is the CalendarEvent of a stored event, or of one instance
// of it (rid not "").
func (c *call) eventObject(p *pimCtx, e *eventData, rid string, tz *time.Location) map[string]any {
	ev := jscal.DeepCopy(e.js).(map[string]any)
	id := store.CalendarEventID(e.obj.ID)
	if rid != "" {
		overrides, _ := ev["recurrenceOverrides"].(map[string]any)
		inst := ev
		pp, _ := overrides[rid].(map[string]any)
		if pp != nil {
			if x, err := jscal.ApplyPatch(ev, pp); err == nil {
				inst = x
			}
		}
		if _, moved := pp["start"]; !moved {
			inst["start"] = rid
		}
		inst["recurrenceId"] = rid
		inst["recurrenceRule"] = nil
		inst["recurrenceOverrides"] = nil
		delete(inst, "excluded")
		ev = inst
		ev["baseEventId"] = id
		id = instanceID(e.obj.ID, rid)
	} else {
		ev["baseEventId"] = nil
	}
	ev["id"] = id
	ev["calendarIds"] = map[string]any{store.CalendarID(e.obj.CollectionID): true}
	ev["isDraft"] = false
	ev["isOrigin"] = c.isOrigin(p, e.js)
	ev["useDefaultAlerts"] = false
	for _, k := range []string{"mayInviteSelf", "mayInviteOthers", "hideAttendees"} {
		ev[k] = false
	}
	delete(ev, "prodId")
	// utcStart and utcEnd, computed (JMAP Calendars 5.1).
	loc := e.zone
	if tzName, _ := ev["timeZone"].(string); tzName != "" {
		if l := ical.LoadZone(tzName); l != nil {
			loc = l
		}
	} else if tz != nil {
		loc = tz
	}
	if s, ok := ev["start"].(string); ok {
		if t, ok := jscal.ParseLocal(s, loc); ok {
			ev["utcStart"] = jscal.UTCDateTime(t)
			end := t
			if d, ok := ev["duration"].(string); ok {
				if du, err := ical.ParseDuration(d); err == nil {
					end = du.AddTo(t)
				}
			}
			ev["utcEnd"] = jscal.UTCDateTime(end)
		}
	}
	return ev
}

// defaultEventProps are what /get returns without properties: all but
// utcStart and utcEnd (JMAP Calendars 5.7).
func pickEvent(ev map[string]any, props []string) map[string]any {
	if props == nil {
		out := map[string]any{}
		for k, v := range ev {
			if k != "utcStart" && k != "utcEnd" {
				out[k] = v
			}
		}
		return out
	}
	out := map[string]any{"id": ev["id"]}
	for _, k := range []string{"calendarIds", "isOrigin", "isDraft", "useDefaultAlerts"} {
		out[k] = ev[k]
	}
	for _, p := range props {
		if v, ok := ev[p]; ok {
			out[p] = v
		} else {
			out[p] = nil
		}
	}
	return out
}

// calendarEventGet is CalendarEvent/get (JMAP Calendars 5.7).
func calendarEventGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID                 string    `json:"accountId"`
		IDs                       *[]string `json:"ids"`
		Properties                *[]string `json:"properties"`
		RecurrenceOverridesBefore *string   `json:"recurrenceOverridesBefore"`
		RecurrenceOverridesAfter  *string   `json:"recurrenceOverridesAfter"`
		ReduceParticipants        bool      `json:"reduceParticipants"`
		TimeZone                  *string   `json:"timeZone"`
	}
	p, e := c.open(raw, &args, &args.AccountID, store.KindCalendar)
	if e != nil {
		return nil, e
	}
	var tz *time.Location
	if args.TimeZone != nil {
		if tz = ical.LoadZone(*args.TimeZone); tz == nil {
			return nil, invalidArgs("unknown timeZone")
		}
	}
	var props []string
	if args.Properties != nil {
		props = *args.Properties
		for _, x := range props {
			if x == "recurrenceOverrides" {
				for _, y := range props {
					if y == "utcStart" || y == "utcEnd" {
						return nil, invalidArgs("utcStart and utcEnd cannot be asked for with recurrenceOverrides")
					}
				}
			}
		}
	}
	state, e := c.pimState(p)
	if e != nil {
		return nil, e
	}
	var ids []int64
	type want struct {
		raw  string
		base int64
		rid  string
	}
	var wants []want
	if args.IDs == nil {
		objs, err := c.h.PIM.Store.Objects(c.ctx, p.owner.ID, store.ObjectFilter{Kind: store.ObjectEvent, Collections: p.ids()})
		if err != nil {
			return nil, storeErr(err)
		}
		if len(objs) > c.h.limits.MaxObjectsInGet {
			return nil, mErr("requestTooLarge", "more events than maxObjectsInGet; ask for ids")
		}
		for _, o := range objs {
			ids = append(ids, o.ID)
			wants = append(wants, want{store.CalendarEventID(o.ID), o.ID, ""})
		}
	} else {
		if len(*args.IDs) > c.h.limits.MaxObjectsInGet {
			return nil, mErr("requestTooLarge", "more ids than maxObjectsInGet")
		}
		for _, sid := range *args.IDs {
			base, rid, ok := parseEventID(c.idRef(sid))
			wants = append(wants, want{sid, base, rid})
			if ok {
				ids = append(ids, base)
			}
		}
	}
	events, e := c.loadEvents(p, ids)
	if e != nil {
		return nil, e
	}
	before, after := parseUTCArg(args.RecurrenceOverridesBefore), parseUTCArg(args.RecurrenceOverridesAfter)
	list, notFound := []map[string]any{}, []string{}
	seen := map[string]bool{}
	for _, w := range wants {
		if seen[w.raw] {
			continue
		}
		seen[w.raw] = true
		ev := events[w.base]
		if ev == nil {
			notFound = append(notFound, w.raw)
			continue
		}
		obj := c.eventObject(p, ev, w.rid, tz)
		if ro, ok := obj["recurrenceOverrides"].(map[string]any); ok && (before != nil || after != nil) {
			for k := range ro {
				t, ok := jscal.ParseLocal(k, ev.zone)
				if !ok || (before != nil && !t.Before(*before)) || (after != nil && t.Before(*after)) {
					delete(ro, k)
				}
			}
		}
		if args.ReduceParticipants {
			reduceParticipants(c, p, obj)
		}
		list = append(list, pickEvent(obj, props))
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

func parseUTCArg(s *string) *time.Time {
	if s == nil {
		return nil
	}
	if t, ok := jscal.ParseUTC(*s); ok {
		return &t
	}
	return nil
}

// reduceParticipants keeps the owners and the user's own participants
// (JMAP Calendars 5.7).
func reduceParticipants(c *call, p *pimCtx, ev map[string]any) {
	parts, ok := ev["participants"].(map[string]any)
	if !ok {
		return
	}
	mine := c.h.PIM.Addresses(c.ctx, c.user.Username)
	for id, x := range parts {
		pm, _ := x.(map[string]any)
		roles, _ := pm["roles"].(map[string]any)
		addr, _ := pm["calendarAddress"].(string)
		own := false
		for _, m := range mine {
			if pim.SameAddress(m, addr) {
				own = true
			}
		}
		if roles["owner"] != true && !own {
			delete(parts, id)
		}
	}
}

func calendarEventChanges(c *call, raw json.RawMessage) (any, *methodError) {
	return c.pimChanges(raw, "CalendarEvent", store.KindCalendar, store.CalendarEventID)
}

// jmapOnly are the properties of a CalendarEvent that are not JSCalendar.
var jmapOnly = []string{"id", "baseEventId", "calendarIds", "isDraft", "isOrigin", "utcStart", "utcEnd", "useDefaultAlerts",
	"mayInviteSelf", "mayInviteOthers", "hideAttendees", "blobId"}

// essential reports whether a change is more than per-user properties
// (JMAP Calendars 5.4): then the origin increments sequence.
func essential(before, after jscal.Event) bool {
	perUser := map[string]bool{"keywords": true, "color": true, "freeBusyStatus": true, "useDefaultAlerts": true, "alerts": true,
		"updated": true, "sequence": true, "participants": true}
	for k, v := range after {
		if !perUser[k] && !jsonEqual(before[k], v) {
			return true
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok && !perUser[k] {
			return true
		}
	}
	// A participant added or removed, not an answer, counts too.
	bp, _ := before["participants"].(map[string]any)
	ap, _ := after["participants"].(map[string]any)
	if len(bp) != len(ap) {
		return true
	}
	for id := range ap {
		if _, ok := bp[id]; !ok {
			return true
		}
	}
	return false
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// eventFromJMAP takes the JMAP-only properties off a CalendarEvent and
// checks them. It returns the JSCalendar part and the calendar.
func (c *call) eventFromJMAP(p *pimCtx, ev map[string]any, create bool) (jscal.Event, int64, *setError) {
	var cal int64
	if v, ok := ev["calendarIds"]; ok {
		m, ok := v.(map[string]any)
		if !ok || len(m) != 1 {
			return nil, 0, invalidProps("calendarIds must name one calendar", "calendarIds")
		}
		for k, t := range m {
			id, ok := store.ParseCalendarID(c.idRef(k))
			if !ok || t != true {
				return nil, 0, invalidProps("calendarIds must name one calendar", "calendarIds")
			}
			if _, found := p.byID(id); !found {
				return nil, 0, invalidProps("no such calendar", "calendarIds")
			}
			cal = id
		}
	} else if create {
		for _, a := range p.colls {
			if a.Collection.IsDefault || cal == 0 {
				cal = a.Collection.ID
			}
		}
		if cal == 0 {
			return nil, 0, invalidProps("calendarIds is required", "calendarIds")
		}
	}
	if v, ok := ev["isDraft"]; ok && v == true {
		return nil, 0, invalidProps("drafts are not kept", "isDraft")
	}
	if _, ok := ev["method"]; ok {
		return nil, 0, invalidProps("a stored event has no method", "method")
	}
	if _, ok := ev["utcStart"]; ok {
		if _, both := ev["start"]; both && create {
			return nil, 0, invalidProps("utcStart and start together", "utcStart")
		}
	}
	out := jscal.Event{}
	for k, v := range ev {
		skip := false
		for _, j := range jmapOnly {
			if k == j {
				skip = true
			}
		}
		if !skip && v != nil {
			out[k] = v
		}
	}
	// utcStart and utcEnd may be given instead of start and duration.
	if s, ok := ev["utcStart"].(string); ok {
		t, ok := jscal.ParseUTC(s)
		if !ok {
			return nil, 0, invalidProps("not a UTCDateTime", "utcStart")
		}
		tz, _ := out["timeZone"].(string)
		loc := time.UTC
		if tz != "" {
			if loc = ical.LoadZone(tz); loc == nil {
				loc = time.UTC
			}
		} else {
			out["timeZone"] = "Etc/UTC"
		}
		out["start"] = jscal.LocalDateTime(t.In(loc))
		if e, ok := ev["utcEnd"].(string); ok {
			te, ok := jscal.ParseUTC(e)
			if !ok || te.Before(t) {
				return nil, 0, invalidProps("not a UTCDateTime after utcStart", "utcEnd")
			}
			out["duration"] = ical.DurationOf(te.Sub(t)).String()
		}
	}
	if _, ok := out["@type"]; !ok {
		out["@type"] = "Event"
	}
	return out, cal, nil
}

// calendarEventSet is CalendarEvent/set (JMAP Calendars 5.9).
func calendarEventSet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		setArgs
		SendSchedulingMessages bool `json:"sendSchedulingMessages"`
	}
	p, e := c.open(raw, &args, &args.AccountID, store.KindCalendar)
	if e != nil {
		return nil, e
	}
	run, e := c.begin(&args.setArgs)
	if e != nil {
		return nil, e
	}
	user := c.account()
	now := c.h.now().UTC().Truncate(time.Second)
	prod := "-//robinmail//robinmail//EN"
	if c.h.PIM.ProdID != "" {
		prod = c.h.PIM.ProdID
	}
	mismatch := mErr("stateMismatch", "the state is not the ifInState given")
	put := func(acc pim.Access, id int64, name string, ev jscal.Event, prev *ical.Component) (store.Object, *setError, bool) {
		cal, err := jscal.ToICal(ev, prev, prod, now)
		if err != nil {
			if pe, ok := err.(*jscal.PropError); ok {
				return store.Object{}, invalidProps(pe.Msg, pe.Props...), false
			}
			return store.Object{}, invalidProps(err.Error()), false
		}
		o, err := c.h.PIM.PutEvent(c.ctx, user, acc, name, cal, pim.Cond{ID: id, IfNoneMatch: id == 0, IfState: run.ifState}, args.SendSchedulingMessages)
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
		ev, calID, se := c.eventFromJMAP(p, obj, true)
		if se != nil {
			run.notCreated[cid] = se
			continue
		}
		acc, _ := p.byID(calID)
		uid, _ := ev["uid"].(string)
		if uid == "" {
			uid = pim.NewUID()
			ev["uid"] = uid
		}
		if c.isOrigin(p, ev) {
			ev["updated"] = jscal.UTCDateTime(now)
			if _, ok := ev["created"]; !ok {
				ev["created"] = jscal.UTCDateTime(now)
			}
		}
		if parts, ok := ev["participants"].(map[string]any); ok && len(parts) > 0 {
			if _, ok := ev["organizerCalendarAddress"]; !ok {
				// JMAP Calendars 5.4: the server picks the organizer address.
				ev["organizerCalendarAddress"] = "mailto:" + p.owner.Address
			}
		}
		o, se, stale := put(acc, 0, pim.NewName(uid, ".ics"), ev, nil)
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
		id := store.CalendarEventID(o.ID)
		run.created[cid] = map[string]any{"id": id, "uid": uid, "isOrigin": c.isOrigin(p, ev), "isDraft": false, "baseEventId": nil}
		c.created[cid] = id
	}
	for _, sid := range sortedKeys(args.Update) {
		base, rid, ok := parseEventID(c.idRef(sid))
		if !ok {
			run.notUpdated[sid] = &setError{Type: "notFound"}
			continue
		}
		events, me := c.loadEvents(p, []int64{base})
		if me != nil {
			return nil, me
		}
		cur := events[base]
		if cur == nil {
			run.notUpdated[sid] = &setError{Type: "notFound"}
			continue
		}
		patch := map[string]any{}
		for k, v := range args.Update[sid] {
			var x any
			if json.Unmarshal(v, &x) != nil {
				run.notUpdated[sid] = invalidProps("not JSON", k)
				continue
			}
			patch[k] = x
		}
		full := c.eventObject(p, cur, rid, nil)
		// utcStart and utcEnd are computed; only a patch that sets them
		// means them.
		delete(full, "utcStart")
		delete(full, "utcEnd")
		next, err := jscal.ApplyPatch(full, patch)
		if err != nil {
			run.notUpdated[sid] = invalidProps(err.Error())
			continue
		}
		ev, calID, se := c.eventFromJMAP(p, next, false)
		if se != nil {
			run.notUpdated[sid] = se
			continue
		}
		if rid != "" {
			// An instance: its difference from the master becomes the
			// override (JMAP Calendars 5.1, synthetic ids).
			master := jscal.DeepCopy(cur.js).(map[string]any)
			delete(ev, "recurrenceId")
			delete(ev, "recurrenceRule")
			delete(ev, "recurrenceOverrides")
			mb := jscal.DeepCopy(master).(map[string]any)
			delete(mb, "recurrenceOverrides")
			delete(mb, "recurrenceRule")
			delete(mb, "prodId")
			ov := jscal.OverridePatch(mb, ev, rid)
			ro, _ := master["recurrenceOverrides"].(map[string]any)
			if ro == nil {
				ro = map[string]any{}
			}
			ro[rid] = ov
			master["recurrenceOverrides"] = ro
			delete(master, "prodId")
			ev = master
			calID = cur.obj.CollectionID
		}
		if calID == 0 {
			calID = cur.obj.CollectionID
		}
		if c.isOrigin(p, cur.js) && essential(cur.js, ev) {
			seq := 0
			if n, ok := cur.js["sequence"].(float64); ok {
				seq = int(n)
			}
			if n, ok := ev["sequence"].(float64); !ok || int(n) <= seq {
				ev["sequence"] = seq + 1
			}
			ev["updated"] = jscal.UTCDateTime(now)
		}
		o, se, stale := put(cur.acc, cur.obj.ID, cur.obj.Name, ev, cur.cal)
		if stale {
			return nil, mismatch
		}
		if se != nil {
			run.notUpdated[sid] = se
			continue
		}
		if calID != cur.obj.CollectionID {
			to, _ := p.byID(calID)
			if !to.MayWrite() || !cur.acc.MayWrite() {
				run.notUpdated[sid] = &setError{Type: "forbidden", Description: "you may not move it there"}
				continue
			}
			if _, err := c.h.PIM.Store.MoveObject(c.ctx, p.owner.ID, o.ID, calID, "", "", 0); err != nil {
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
			base, rid, ok := parseEventID(c.idRef(sid))
			if !ok {
				run.notDestroyed[sid] = &setError{Type: "notFound"}
				continue
			}
			events, me := c.loadEvents(p, []int64{base})
			if me != nil {
				return nil, me
			}
			cur := events[base]
			if cur == nil {
				run.notDestroyed[sid] = &setError{Type: "notFound"}
				continue
			}
			if rid != "" {
				// One instance: excluded.
				master := jscal.DeepCopy(cur.js).(map[string]any)
				delete(master, "prodId")
				ro, _ := master["recurrenceOverrides"].(map[string]any)
				if ro == nil {
					ro = map[string]any{}
				}
				ro[rid] = map[string]any{"excluded": true}
				master["recurrenceOverrides"] = ro
				_, se, stale := put(cur.acc, cur.obj.ID, cur.obj.Name, master, cur.cal)
				if stale {
					return nil, mismatch
				}
				if se != nil {
					run.notDestroyed[sid] = se
					continue
				}
			} else {
				_, err := c.h.PIM.DeleteEvent(c.ctx, user, cur.acc, cur.obj, "", run.ifState, args.SendSchedulingMessages)
				if err == store.ErrStateMismatch {
					return nil, mismatch
				}
				if err != nil {
					run.notDestroyed[sid] = pimSetError(err)
					continue
				}
			}
			st, _ := c.h.Store.State(c.ctx, p.owner.ID)
			run.wroteState(st)
			run.destroyed = append(run.destroyed, sid)
		}
	}
	return run.result(), nil
}

// eventFilter is a FilterCondition of CalendarEvent/query (JMAP Calendars
// 5.11), or an operator.
type eventFilter struct {
	Operator    string        `json:"operator"`
	Conditions  []eventFilter `json:"conditions"`
	InCalendar  *string       `json:"inCalendar"`
	After       *string       `json:"after"`
	Before      *string       `json:"before"`
	Text        *string       `json:"text"`
	Title       *string       `json:"title"`
	Description *string       `json:"description"`
	Location    *string       `json:"location"`
	Owner       *string       `json:"owner"`
	Participant *string       `json:"participant"`
	UID         *string       `json:"uid"`
}

func containsFold(hay, needle string) bool {
	return strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

// eventMatch tests one event (or instance) against a filter.
func (c *call) eventMatch(f *eventFilter, ev map[string]any, start, end time.Time, tz *time.Location) bool {
	switch f.Operator {
	case "AND":
		for i := range f.Conditions {
			if !c.eventMatch(&f.Conditions[i], ev, start, end, tz) {
				return false
			}
		}
		return true
	case "OR":
		for i := range f.Conditions {
			if c.eventMatch(&f.Conditions[i], ev, start, end, tz) {
				return true
			}
		}
		return false
	case "NOT":
		for i := range f.Conditions {
			if c.eventMatch(&f.Conditions[i], ev, start, end, tz) {
				return false
			}
		}
		return true
	}
	str := func(k string) string { s, _ := ev[k].(string); return s }
	if f.InCalendar != nil {
		ids, _ := ev["calendarIds"].(map[string]any)
		if ids[c.idRef(*f.InCalendar)] != true {
			return false
		}
	}
	if f.After != nil {
		t, ok := jscal.ParseLocal(*f.After, tz)
		if !ok || !end.After(t) && !(end.Equal(start) && !start.Before(t)) {
			return false
		}
	}
	if f.Before != nil {
		t, ok := jscal.ParseLocal(*f.Before, tz)
		if !ok || !start.Before(t) {
			return false
		}
	}
	if f.UID != nil && str("uid") != *f.UID {
		return false
	}
	if f.Title != nil && !containsFold(str("title"), *f.Title) {
		return false
	}
	if f.Description != nil && !containsFold(str("description"), *f.Description) {
		return false
	}
	locText := ""
	if locs, ok := ev["locations"].(map[string]any); ok {
		for _, l := range locs {
			if m, ok := l.(map[string]any); ok {
				n, _ := m["name"].(string)
				locText += " " + n
			}
		}
	}
	if f.Location != nil && !containsFold(locText, *f.Location) {
		return false
	}
	partText, ownerText := "", ""
	if parts, ok := ev["participants"].(map[string]any); ok {
		for _, x := range parts {
			m, _ := x.(map[string]any)
			n, _ := m["name"].(string)
			e, _ := m["email"].(string)
			a, _ := m["calendarAddress"].(string)
			partText += " " + n + " " + e + " " + a
			if r, _ := m["roles"].(map[string]any); r["owner"] == true {
				ownerText += " " + n + " " + e + " " + a
			}
		}
	}
	if f.Participant != nil && !containsFold(partText, *f.Participant) {
		return false
	}
	if f.Owner != nil && !containsFold(ownerText, *f.Owner) {
		return false
	}
	if f.Text != nil && !containsFold(str("title")+" "+str("description")+" "+locText+" "+partText, *f.Text) {
		return false
	}
	return true
}

// calendarEventQuery is CalendarEvent/query (JMAP Calendars 5.11).
func calendarEventQuery(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID string `json:"accountId"`
		queryArgs
		Filter            *eventFilter `json:"filter"`
		Sort              []sortArg    `json:"sort"`
		ExpandRecurrences bool         `json:"expandRecurrences"`
		TimeZone          *string      `json:"timeZone"`
	}
	p, e := c.open(raw, &args, &args.AccountID, store.KindCalendar)
	if e != nil {
		return nil, e
	}
	tz := time.UTC
	if args.TimeZone != nil {
		if tz = ical.LoadZone(*args.TimeZone); tz == nil {
			return nil, invalidArgs("unknown timeZone")
		}
	}
	limit, clamped, e := args.limit()
	if e != nil {
		return nil, e
	}
	f := args.Filter
	if f == nil {
		f = &eventFilter{}
	}
	var from, to *time.Time
	if f.Operator == "" {
		if f.After != nil {
			if t, ok := jscal.ParseLocal(*f.After, tz); ok {
				from = &t
			} else {
				return nil, mErr("unsupportedFilter", "after is not a LocalDateTime")
			}
		}
		if f.Before != nil {
			if t, ok := jscal.ParseLocal(*f.Before, tz); ok {
				to = &t
			} else {
				return nil, mErr("unsupportedFilter", "before is not a LocalDateTime")
			}
		}
	}
	if args.ExpandRecurrences {
		if f.Operator != "" || from == nil || to == nil {
			return nil, invalidArgs("expandRecurrences needs one filter condition with after and before")
		}
		if to.Sub(*from) > 400*24*time.Hour {
			return nil, mErr("expandDurationTooLarge", "the range is longer than maxExpandedQueryDuration")
		}
	}
	for _, s := range args.Sort {
		switch s.Property {
		case "start", "uid", "id", "created", "updated":
		default:
			return nil, mErr("unsupportedSort", "cannot sort by %q", s.Property)
		}
	}
	state, e := c.pimState(p)
	if e != nil {
		return nil, e
	}
	objs, err := c.h.PIM.Store.Objects(c.ctx, p.owner.ID, store.ObjectFilter{Kind: store.ObjectEvent, Collections: p.ids(), From: from, To: to, WithData: true})
	if err != nil {
		return nil, storeErr(err)
	}
	type hit struct {
		id      string
		start   time.Time
		uid     string
		created string
		updated string
	}
	var hits []hit
	for _, o := range objs {
		ed := c.parseEvent(p, o)
		if ed == nil {
			continue
		}
		if args.ExpandRecurrences {
			inst, complete := ed.o.Instances(*from, *to, 2000)
			if !complete {
				return nil, mErr("cannotCalculateOccurrences", "too many occurrences")
			}
			for _, i := range inst {
				rid := ""
				if !i.RecurrenceID.IsZero() {
					rid = jscal.LocalDateTime(i.RecurrenceID.In(eventZone(ed)))
				}
				obj := c.eventObject(p, ed, rid, tz)
				if !c.eventMatch(f, obj, i.Start, i.End, tz) {
					continue
				}
				id := store.CalendarEventID(o.ID)
				if rid != "" {
					id = instanceID(o.ID, rid)
				}
				hits = append(hits, hit{id, i.Start, ed.o.UID, str(obj["created"]), str(obj["updated"])})
			}
			continue
		}
		obj := c.eventObject(p, ed, "", tz)
		start, end, ok := ed.o.MasterTimes()
		if !ok {
			continue
		}
		if from != nil || to != nil {
			// Any instance in the range counts.
			lo, hi := time.Unix(0, 0), time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
			if from != nil {
				lo = *from
			}
			if to != nil {
				hi = *to
			}
			inst, _ := ed.o.Instances(lo, hi, 1)
			if len(inst) == 0 {
				continue
			}
			start, end = inst[0].Start, inst[0].End
		}
		if !c.eventMatch(f, obj, start, end, tz) {
			continue
		}
		s, _, _ := ed.o.MasterTimes()
		hits = append(hits, hit{store.CalendarEventID(o.ID), s, ed.o.UID, str(obj["created"]), str(obj["updated"])})
	}
	sorts := args.Sort
	if len(sorts) == 0 {
		sorts = []sortArg{{Property: "start"}}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		for _, s := range sorts {
			var less, more bool
			switch s.Property {
			case "start":
				less, more = hits[i].start.Before(hits[j].start), hits[j].start.Before(hits[i].start)
			case "uid":
				less, more = hits[i].uid < hits[j].uid, hits[j].uid < hits[i].uid
			case "created":
				less, more = hits[i].created < hits[j].created, hits[j].created < hits[i].created
			case "updated":
				less, more = hits[i].updated < hits[j].updated, hits[j].updated < hits[i].updated
			default:
				less, more = hits[i].id < hits[j].id, hits[j].id < hits[i].id
			}
			if !s.ascending() {
				less, more = more, less
			}
			if less || more {
				return less
			}
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
	out := map[string]any{"accountId": args.AccountID, "queryState": stateString(state), "canCalculateChanges": false,
		"position": pos, "ids": page}
	if args.CalculateTotal {
		out["total"] = len(ids)
	}
	if clamped {
		out["limit"] = limit
	}
	return out, nil
}

func str(v any) string { s, _ := v.(string); return s }

func eventZone(e *eventData) *time.Location {
	if tz, _ := e.js["timeZone"].(string); tz != "" {
		if l := ical.LoadZone(tz); l != nil {
			return l
		}
	}
	return e.zone
}

// queryChangesUnsupported answers a /queryChanges the server does not
// calculate (RFC 8620 5.6 allows it).
func queryChangesUnsupported(c *call, raw json.RawMessage) (any, *methodError) {
	return nil, mErr("cannotCalculateChanges", "this server does not calculate query changes for this type; query again")
}

// calendarEventParse is CalendarEvent/parse (JMAP Calendars 5.13).
func calendarEventParse(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		BlobIDs    []string  `json:"blobIds"`
		Properties *[]string `json:"properties"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	parsed := map[string]any{}
	var notFound, notParsable []string
	for _, b := range args.BlobIDs {
		r, size, err := c.h.Store.OpenBlob(c.ctx, args.AccountID, b)
		if err != nil {
			notFound = append(notFound, b)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(r, store.MaxObjectSize*4))
		r.Close()
		if err != nil || size > store.MaxObjectSize*4 {
			notParsable = append(notParsable, b)
			continue
		}
		cal, err := ical.ParseOne(data, "VCALENDAR")
		if err != nil {
			notParsable = append(notParsable, b)
			continue
		}
		// One event per UID, the overrides with their master.
		byUID := map[string]*ical.Component{}
		var order []string
		for _, comp := range jscal.Components(cal) {
			uid := comp.Text("UID")
			x := byUID[uid]
			if x == nil {
				x = &ical.Component{Name: "VCALENDAR", Props: cal.Props, Children: cal.Sub("VTIMEZONE")}
				byUID[uid] = x
				order = append(order, uid)
			}
			x.Children = append(x.Children, comp)
		}
		var list []any
		for _, uid := range order {
			ev := jscal.Normalize(jscal.ToJS(jscal.Read(byUID[uid], time.UTC))).(map[string]any)
			if m := cal.Value("METHOD"); m != "" {
				ev["method"] = strings.ToLower(m)
			}
			for _, k := range []string{"id", "baseEventId", "calendarIds", "isDraft", "isOrigin"} {
				ev[k] = nil
			}
			delete(ev, "prodId")
			if args.Properties != nil {
				ev = pickEvent(ev, *args.Properties)
			}
			list = append(list, ev)
		}
		parsed[b] = list
	}
	out := map[string]any{"accountId": args.AccountID, "parsed": nil, "notFound": nil, "notParsable": nil}
	if len(parsed) > 0 {
		out["parsed"] = parsed
	}
	if len(notFound) > 0 {
		out["notFound"] = notFound
	}
	if len(notParsable) > 0 {
		out["notParsable"] = notParsable
	}
	return out, nil
}

// participantIdentities are the user's calendar addresses (JMAP Calendars
// 3): the account's address and its aliases, made by the server.
func (c *call) participantIdentities() []map[string]any {
	var out []map[string]any
	for i, a := range c.h.PIM.Addresses(c.ctx, c.user.Username) {
		out = append(out, map[string]any{"id": "pi" + shortHash(a), "name": "", "calendarAddress": a, "isDefault": i == 0})
	}
	return out
}

func participantIdentityGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(args.Properties, []string{"id", "name", "calendarAddress", "isDefault"})
	if e != nil {
		return nil, e
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	all := c.participantIdentities()
	list, notFound := []map[string]any{}, []string{}
	if args.IDs == nil {
		for _, x := range all {
			list = append(list, pick(x, props))
		}
	} else {
		for _, id := range *args.IDs {
			found := false
			for _, x := range all {
				if x["id"] == id {
					list = append(list, pick(x, props))
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

func participantIdentityChanges(c *call, raw json.RawMessage) (any, *methodError) {
	return c.changes(raw, "ParticipantIdentity", func(n int64) string { return "" })
}

// participantIdentitySet refuses everything: the identities are the
// account's addresses, which the administrator manages (JMAP Calendars
// 3.3 allows out-of-band identities).
func participantIdentitySet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		setArgs
		OnSuccessSetIsDefault *string `json:"onSuccessSetIsDefault"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	run, e := c.begin(&args.setArgs)
	if e != nil {
		return nil, e
	}
	no := &setError{Type: "forbidden", Description: "participant identities are the addresses of the account"}
	for cid := range args.Create {
		run.notCreated[cid] = no
	}
	for sid := range args.Update {
		run.notUpdated[sid] = no
	}
	if args.Destroy != nil {
		for _, sid := range *args.Destroy {
			run.notDestroyed[sid] = no
		}
	}
	return run.result(), nil
}
