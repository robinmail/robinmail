package pim

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/jscal"
	"github.com/robinmail/robinmail/internal/store"
)

// Scheduling (RFC 6638 3.2, RFC 5546). After an account's calendar object
// changed, robinmail works out what the others need to hear:
//
//   - The account organizes it (its ORGANIZER is one of the account's
//     addresses): a REQUEST to every attendee when something other than an
//     attendee's answer changed, a CANCEL to attendees taken off, a CANCEL
//     to all when the event is deleted.
//   - The account attends it: a REPLY to the organizer when its own answer
//     (PARTSTAT) changed, and DECLINED when it deleted the event.
//
// A message to an account of this server is applied to that account's
// calendar at once and put in its scheduling inbox; to anyone else it goes
// as an email (iMIP). An attendee with SCHEDULE-AGENT other than SERVER is
// left to the client (RFC 6638 7.1).

// role says how an account stands to an event.
func organizerOf(cal *ical.Component) string {
	for _, c := range jscal.Components(cal) {
		if v := c.Value("ORGANIZER"); v != "" {
			return v
		}
	}
	return ""
}

// attendees lists the scheduled attendees of all components, once each.
func attendees(cal *ical.Component) []*ical.Prop {
	seen := map[string]bool{}
	var out []*ical.Prop
	for _, c := range jscal.Components(cal) {
		for _, a := range c.All("ATTENDEE") {
			if agent := a.Param("SCHEDULE-AGENT"); agent != "" && !strings.EqualFold(agent, "SERVER") {
				continue
			}
			k := strings.ToLower(a.Value)
			if !seen[k] {
				seen[k] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// partstats maps "recurrence id|address" to the PARTSTAT of the address.
func partstats(cal *ical.Component, addrs []string) map[string]string {
	out := map[string]string{}
	if cal == nil {
		return out
	}
	for _, c := range jscal.Components(cal) {
		for _, a := range c.All("ATTENDEE") {
			if isOwn(addrs, a.Value) {
				ps := strings.ToUpper(a.Param("PARTSTAT"))
				if ps == "" {
					ps = "NEEDS-ACTION"
				}
				out[c.Value("RECURRENCE-ID")] = ps
			}
		}
	}
	return out
}

// essence is an object without what does not call for a new REQUEST:
// stamps, the answers of attendees, alarms and X- properties.
func essence(cal *ical.Component) string {
	if cal == nil {
		return ""
	}
	c := cal.Clone()
	c.Remove("PRODID", "METHOD")
	var comps []*ical.Component
	for _, x := range c.Children {
		if x.Name == "VTIMEZONE" {
			continue
		}
		x.Remove("DTSTAMP", "LAST-MODIFIED", "SEQUENCE", "CREATED")
		var keep []ical.Prop
		for _, p := range x.Props {
			if strings.HasPrefix(p.Name, "X-") {
				continue
			}
			if p.Name == "ATTENDEE" || p.Name == "ORGANIZER" {
				p.SetParam("PARTSTAT")
				p.SetParam("RSVP")
				p.SetParam("SCHEDULE-STATUS")
				p.SetParam("SCHEDULE-FORCE-SEND")
				p.SetParam("X-SCHEDULE-SEQUENCE")
				p.Value = strings.ToLower(p.Value)
			}
			keep = append(keep, p)
		}
		sort.SliceStable(keep, func(i, j int) bool { return keep[i].Name < keep[j].Name })
		x.Props = keep
		var kids []*ical.Component
		for _, k := range x.Children {
			if k.Name != "VALARM" {
				kids = append(kids, k)
			}
		}
		x.Children = kids
		comps = append(comps, x)
	}
	c.Children = comps
	return string(ical.Encode(c))
}

// afterChange sends what a change of an account's object calls for. old
// is nil for a new object, cur nil for a deleted one.
func (s *Service) afterChange(ctx context.Context, ownerID string, old, cur *ical.Component) {
	acct, err := s.Store.AccountByID(ctx, ownerID)
	if err != nil {
		return
	}
	mine := s.Addresses(ctx, acct.Address)
	ref := cur
	if ref == nil {
		ref = old
	}
	org := organizerOf(ref)
	if org == "" {
		return
	}
	if isOwn(mine, org) {
		var oldList, curList []*ical.Prop
		if old != nil {
			oldList = attendees(old)
		}
		if cur != nil {
			curList = attendees(cur)
		}
		in := func(list []*ical.Prop, a string) bool {
			for _, x := range list {
				if SameAddress(x.Value, a) {
					return true
				}
			}
			return false
		}
		var removed, everyone []string
		for _, a := range oldList {
			if !isOwn(mine, a.Value) && (cur == nil || !in(curList, a.Value)) {
				removed = append(removed, a.Value)
			}
		}
		for _, a := range curList {
			if !isOwn(mine, a.Value) {
				everyone = append(everyone, a.Value)
			}
		}
		if len(removed) > 0 {
			s.deliver(ctx, acct, org, removed, buildCancel(ref, removed, s.prodID(), s.now()))
		}
		if cur != nil && len(everyone) > 0 && (old == nil || essence(old) != essence(cur)) {
			s.deliver(ctx, acct, org, everyone, buildRequest(cur, s.prodID()))
		}
		return
	}
	// An attendee: a REPLY when the account's own answer changed.
	before, after := partstats(old, mine), partstats(cur, mine)
	if cur == nil {
		for rid := range before {
			after[rid] = "DECLINED"
		}
	}
	var changed []string
	for rid, ps := range after {
		if ps != "NEEDS-ACTION" && before[rid] != ps {
			changed = append(changed, rid)
		}
	}
	if len(changed) == 0 {
		return
	}
	me := ""
	for _, c := range jscal.Components(ref) {
		for _, a := range c.All("ATTENDEE") {
			if isOwn(mine, a.Value) {
				me = a.Value
			}
		}
	}
	reply := buildReply(ref, me, changed, after, s.prodID(), s.now())
	if reply != nil {
		s.deliver(ctx, acct, me, []string{org}, reply)
	}
}

func baseMessage(method, prodID string) *ical.Component {
	m := &ical.Component{Name: "VCALENDAR"}
	m.Add(ical.Prop{Name: "VERSION", Value: "2.0"})
	m.SetText("PRODID", prodID)
	m.Add(ical.Prop{Name: "METHOD", Value: method})
	return m
}

func zonesOf(cal *ical.Component) []*ical.Component { return cal.Sub("VTIMEZONE") }

// buildRequest is the whole object as a REQUEST (RFC 5546 3.2.2), without
// the organizer's alarms.
func buildRequest(cal *ical.Component, prodID string) *ical.Component {
	m := baseMessage("REQUEST", prodID)
	m.Children = append(m.Children, zonesOf(cal)...)
	for _, c := range jscal.Components(cal) {
		x := c.Clone()
		var kids []*ical.Component
		for _, k := range x.Children {
			if k.Name != "VALARM" {
				kids = append(kids, k)
			}
		}
		x.Children = kids
		for i := range x.Props {
			if x.Props[i].Name == "ATTENDEE" || x.Props[i].Name == "ORGANIZER" {
				x.Props[i].SetParam("SCHEDULE-STATUS")
				x.Props[i].SetParam("SCHEDULE-AGENT")
			}
		}
		m.Children = append(m.Children, x)
	}
	return m
}

// buildCancel is a CANCEL of the whole event for some attendees (RFC 5546
// 3.2.5).
func buildCancel(cal *ical.Component, to []string, prodID string, now time.Time) *ical.Component {
	m := baseMessage("CANCEL", prodID)
	m.Children = append(m.Children, zonesOf(cal)...)
	comps := jscal.Components(cal)
	if len(comps) == 0 {
		return m
	}
	src := comps[0]
	for _, c := range comps {
		if c.Get("RECURRENCE-ID") == nil {
			src = c
		}
	}
	x := &ical.Component{Name: src.Name}
	for _, name := range []string{"UID", "ORGANIZER", "DTSTART", "SUMMARY", "RRULE"} {
		for _, p := range src.All(name) {
			x.Add(*p)
		}
	}
	x.Add(ical.Prop{Name: "DTSTAMP", Value: ical.FormatUTC(now)})
	seq := 0
	if n, err := strconv.Atoi(src.Value("SEQUENCE")); err == nil {
		seq = n
	}
	x.Add(ical.Prop{Name: "SEQUENCE", Value: strconv.Itoa(seq + 1)})
	x.Add(ical.Prop{Name: "STATUS", Value: "CANCELLED"})
	for _, a := range src.All("ATTENDEE") {
		for _, t := range to {
			if SameAddress(a.Value, t) {
				x.Add(*a)
			}
		}
	}
	m.Children = append(m.Children, x)
	return m
}

// buildReply is a REPLY of one attendee for the components whose answer
// changed (RFC 5546 3.2.3).
func buildReply(cal *ical.Component, me string, rids []string, ps map[string]string, prodID string, now time.Time) *ical.Component {
	m := baseMessage("REPLY", prodID)
	want := map[string]bool{}
	for _, r := range rids {
		want[r] = true
	}
	for _, c := range jscal.Components(cal) {
		rid := c.Value("RECURRENCE-ID")
		if !want[rid] {
			continue
		}
		x := &ical.Component{Name: c.Name}
		for _, name := range []string{"UID", "RECURRENCE-ID", "ORGANIZER", "DTSTART", "DTEND", "DURATION", "SUMMARY", "SEQUENCE"} {
			if p := c.Get(name); p != nil {
				x.Add(*p)
			}
		}
		x.Add(ical.Prop{Name: "DTSTAMP", Value: ical.FormatUTC(now)})
		var att *ical.Prop
		for _, a := range c.All("ATTENDEE") {
			if SameAddress(a.Value, me) {
				cp := *a
				att = &cp
			}
		}
		if att == nil {
			att = &ical.Prop{Name: "ATTENDEE", Value: me}
		}
		att.SetParam("PARTSTAT", ps[rid])
		att.SetParam("RSVP")
		att.SetParam("SCHEDULE-STATUS")
		x.Add(*att)
		m.Children = append(m.Children, x)
	}
	if len(m.Children) == 0 {
		return nil
	}
	if len(zonesOf(cal)) > 0 {
		m.Children = append(zonesOf(cal), m.Children...)
	}
	return m
}

// deliver sends one iTIP message from an account to calendar users: those
// of this server get it applied at once, the others by email.
func (s *Service) deliver(ctx context.Context, from store.Account, fromAddr string, to []string, msg *ical.Component) {
	method := msg.Value("METHOD")
	var remote []string
	for _, addr := range to {
		email := strings.TrimSpace(addr)
		if len(email) > 7 && strings.EqualFold(email[:7], "mailto:") {
			email = email[7:]
		} else {
			s.log().Info("scheduling: not a mailto address", "to", addr)
			continue
		}
		acct, err := s.Store.Account(ctx, email)
		if err == nil {
			if acct.ID == from.ID {
				continue
			}
			if err := s.Apply(ctx, acct, msg, mailtoOf(fromAddr)); err != nil {
				s.log().Warn("scheduling: applying to a local account failed", "method", method, "to", acct.Address, "err", err)
			} else {
				s.log().Info("scheduling: delivered", "method", method, "from", from.Address, "to", acct.Address)
			}
			continue
		}
		remote = append(remote, email)
	}
	if len(remote) == 0 || s.Send == nil {
		return
	}
	sender := mailtoOf(fromAddr)
	if sender == "" {
		sender = from.Address
	}
	for _, rcpt := range remote {
		raw := BuildIMIP(sender, rcpt, msg, s.now())
		if err := s.Send(ctx, from, sender, []string{rcpt}, raw); err != nil {
			s.log().Warn("scheduling: sending by email failed", "method", method, "to", rcpt, "err", err)
		} else {
			s.log().Info("scheduling: sent by email", "method", method, "from", sender, "to", rcpt)
		}
	}
}

func mailtoOf(uri string) string {
	if len(uri) > 7 && strings.EqualFold(uri[:7], "mailto:") {
		return uri[7:]
	}
	return ""
}

// ErrNotForUs is an iTIP message that does not concern the account or does
// not come from whom it should.
var ErrNotForUs = errors.New("pim: the scheduling message is not for this account or not from the right sender")

// Apply processes an iTIP message for an account (RFC 5546 3.2): one that
// arrived by email, or one of another account of this server. sender is
// the address it came from; a REQUEST or CANCEL must come from the
// organizer, a REPLY from the attendee it answers for. The message is also
// put in the account's scheduling inbox, where CalDAV clients look.
func (s *Service) Apply(ctx context.Context, acct store.Account, msg *ical.Component, sender string) error {
	method := strings.ToUpper(msg.Value("METHOD"))
	comps := jscal.Components(msg)
	if len(comps) == 0 {
		return ErrNotForUs
	}
	uid := comps[0].Text("UID")
	org := organizerOf(msg)
	mine := s.Addresses(ctx, acct.Address)
	switch method {
	case "REQUEST", "ADD", "CANCEL":
		if org == "" || !SameAddress(org, "mailto:"+sender) {
			return ErrNotForUs
		}
		invited := false
		for _, a := range attendeesAll(msg) {
			if isOwn(mine, a) {
				invited = true
			}
		}
		if !invited {
			return ErrNotForUs
		}
	case "REPLY":
		if org == "" || !isOwn(mine, org) {
			return ErrNotForUs
		}
		ok := false
		for _, a := range attendeesAll(msg) {
			if SameAddress(a, "mailto:"+sender) {
				ok = true
			}
		}
		if !ok {
			return ErrNotForUs
		}
	default:
		return ErrNotForUs
	}
	if err := s.Store.EnsureCollections(ctx, acct.ID); err != nil {
		return err
	}
	existing, err := s.Store.Objects(ctx, acct.ID, store.ObjectFilter{Kind: store.ObjectEvent, UID: uid, WithData: true})
	if err != nil {
		return err
	}
	var cur *store.Object
	var curCal *ical.Component
	if len(existing) > 0 {
		cur = &existing[0]
		curCal, _ = ical.ParseOne([]byte(cur.Data), "VCALENDAR")
	}
	var next *ical.Component
	switch method {
	case "REQUEST", "ADD":
		next = mergeRequest(curCal, msg, mine)
	case "CANCEL":
		if curCal == nil {
			return s.toInbox(ctx, acct, msg)
		}
		next = applyCancel(curCal, msg)
	case "REPLY":
		if curCal == nil {
			return ErrNotForUs
		}
		next = applyReply(curCal, msg, sender)
	}
	if next == nil {
		return s.toInbox(ctx, acct, msg)
	}
	next.Remove("METHOD")
	if cur != nil {
		acc, err := s.Collection(ctx, acct, acct.ID, cur.CollectionID)
		if err != nil {
			return err
		}
		start, end, summary := index(next, acc.Collection)
		_, err = s.Store.Put(ctx, acct.ID, store.PutObject{CollectionID: cur.CollectionID, ID: cur.ID, Name: cur.Name,
			Kind: store.ObjectEvent, UID: uid, Data: string(ical.Encode(next)), Component: cur.Component,
			Start: start, End: end, Summary: summary, IfMatch: cur.ETag})
		if err != nil {
			return err
		}
	} else {
		cals, err := s.Store.Collections(ctx, acct.ID, store.KindCalendar)
		if err != nil {
			return err
		}
		var def store.Collection
		for _, c := range cals {
			if c.IsDefault {
				def = c
			}
		}
		if def.ID == 0 {
			return store.ErrNoCollection
		}
		start, end, summary := index(next, def)
		_, err = s.Store.Put(ctx, acct.ID, store.PutObject{CollectionID: def.ID, Name: NewName(uid, ".ics"), Kind: store.ObjectEvent,
			UID: uid, Data: string(ical.Encode(next)), Component: comps[0].Name, Start: start, End: end, Summary: summary, IfNoneMatch: true})
		if err != nil {
			return err
		}
	}
	return s.toInbox(ctx, acct, msg)
}

func attendeesAll(cal *ical.Component) []string {
	var out []string
	for _, c := range jscal.Components(cal) {
		for _, a := range c.All("ATTENDEE") {
			out = append(out, a.Value)
		}
	}
	return out
}

// toInbox keeps an iTIP message in the account's scheduling inbox (RFC
// 6638 4.1). Clients read and remove them.
func (s *Service) toInbox(ctx context.Context, acct store.Account, msg *ical.Component) error {
	inbox, err := s.Store.Collections(ctx, acct.ID, store.KindInbox)
	if err != nil || len(inbox) == 0 {
		return err
	}
	comps := jscal.Components(msg)
	_, err = s.Store.Put(ctx, acct.ID, store.PutObject{CollectionID: inbox[0].ID, Name: RandomID() + ".ics", Kind: store.ObjectITIP,
		UID: comps[0].Text("UID"), Data: string(ical.Encode(msg)), Component: comps[0].Name, Summary: comps[0].Text("SUMMARY"), IfNoneMatch: true})
	return err
}

func seqOf(c *ical.Component) int {
	n, _ := strconv.Atoi(strings.TrimSpace(c.Value("SEQUENCE")))
	return n
}

// mergeRequest takes an invitation into what the account has: the
// organizer's version of each component, with the account's own answer
// and alarms kept while the sequence is the same. An older sequence is
// ignored (RFC 5546 2.1.5).
func mergeRequest(cur, msg *ical.Component, mine []string) *ical.Component {
	next := msg.Clone()
	next.Remove("METHOD")
	if cur == nil {
		return next
	}
	byRID := map[string]*ical.Component{}
	for _, c := range jscal.Components(cur) {
		byRID[c.Value("RECURRENCE-ID")] = c
	}
	for _, c := range jscal.Components(next) {
		old := byRID[c.Value("RECURRENCE-ID")]
		if old == nil {
			continue
		}
		if seqOf(c) < seqOf(old) {
			return nil
		}
		if seqOf(c) == seqOf(old) {
			own := partstats(&ical.Component{Name: "VCALENDAR", Children: []*ical.Component{old}}, mine)[c.Value("RECURRENCE-ID")]
			for i := range c.Props {
				if c.Props[i].Name == "ATTENDEE" && isOwn(mine, c.Props[i].Value) && own != "" {
					c.Props[i].SetParam("PARTSTAT", own)
				}
			}
		}
		for _, k := range old.Children {
			if k.Name == "VALARM" {
				c.Children = append(c.Children, k)
			}
		}
	}
	// ADD and an invitation to one instance leave the other components.
	if msg.Value("METHOD") == "ADD" || len(jscal.Components(next)) < len(jscal.Components(cur)) {
		have := map[string]bool{}
		for _, c := range jscal.Components(next) {
			have[c.Value("RECURRENCE-ID")] = true
		}
		for _, c := range jscal.Components(cur) {
			if !have[c.Value("RECURRENCE-ID")] {
				next.Children = append(next.Children, c)
			}
		}
	}
	return next
}

// applyCancel marks the cancelled components (RFC 5546 3.2.5): the whole
// event, or the instances named by RECURRENCE-ID, which become EXDATEs.
func applyCancel(cur, msg *ical.Component) *ical.Component {
	next := cur.Clone()
	for _, m := range jscal.Components(msg) {
		rid := m.Get("RECURRENCE-ID")
		if rid == nil {
			for _, c := range jscal.Components(next) {
				c.Set(ical.Prop{Name: "STATUS", Value: "CANCELLED"})
				if seqOf(m) > seqOf(c) {
					c.Set(ical.Prop{Name: "SEQUENCE", Value: strconv.Itoa(seqOf(m))})
				}
			}
			continue
		}
		var keep []*ical.Component
		for _, c := range next.Children {
			if c.Value("RECURRENCE-ID") == rid.Value {
				continue
			}
			keep = append(keep, c)
		}
		next.Children = keep
		for _, c := range jscal.Components(next) {
			if c.Get("RECURRENCE-ID") == nil {
				ex := ical.Prop{Name: "EXDATE", Params: rid.Params, Value: rid.Value}
				c.Add(ex)
			}
		}
	}
	return next
}

// applyReply records an attendee's answer in the organizer's copy (RFC 5546
// 3.2.3). An answer for an instance the copy has no override for is
// recorded on the master.
func applyReply(cur, msg *ical.Component, sender string) *ical.Component {
	next := cur.Clone()
	changed := false
	for _, m := range jscal.Components(msg) {
		var ps, who string
		for _, a := range m.All("ATTENDEE") {
			if SameAddress(a.Value, "mailto:"+sender) {
				ps, who = a.Param("PARTSTAT"), a.Value
			}
		}
		if ps == "" {
			continue
		}
		rid := m.Value("RECURRENCE-ID")
		var target *ical.Component
		for _, c := range jscal.Components(next) {
			if c.Value("RECURRENCE-ID") == rid {
				target = c
			}
		}
		if target == nil {
			for _, c := range jscal.Components(next) {
				if c.Get("RECURRENCE-ID") == nil {
					target = c
				}
			}
		}
		if target == nil {
			continue
		}
		for i := range target.Props {
			p := &target.Props[i]
			if p.Name == "ATTENDEE" && SameAddress(p.Value, who) {
				p.SetParam("PARTSTAT", strings.ToUpper(ps))
				p.SetParam("RSVP")
				p.SetParam("SCHEDULE-STATUS", "2.0")
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return next
}
