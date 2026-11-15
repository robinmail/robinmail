// Package jscal converts calendar data between iCalendar (RFC 5545) and
// JSCalendar (draft-ietf-calext-jscalendarbis-22, "JSCalendar 2.0", which
// JMAP for Calendars builds on), following
// draft-ietf-calext-jscalendar-icalendar-28 where it says how, and
// computes the instances of an event.
//
// A stored calendar object is a VCALENDAR with the components of one UID:
// the master VEVENT and its overrides (RECURRENCE-ID), as CalDAV keeps
// them (RFC 4791 4.1). In JSCalendar that is one Event, whose overrides are
// recurrenceOverrides.
package jscal

import (
	"sort"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
)

// Instance is one occurrence of an event.
type Instance struct {
	Start, End time.Time
	// RecurrenceID is the original start of the instance; zero for an
	// event that does not recur.
	RecurrenceID time.Time
	// Component is the VEVENT that describes the instance: the master, or
	// the override with its RECURRENCE-ID.
	Component *ical.Component
}

// Object is a parsed calendar object: its master component (nil when the
// object holds only overrides, as an invitation to single instances does)
// and its overrides by the instant of their RECURRENCE-ID.
type Object struct {
	Cal       *ical.Component
	Kind      string // VEVENT, VTODO or VJOURNAL
	UID       string
	Master    *ical.Component
	Overrides map[int64]*ical.Component
	Zones     ical.Zones
	// Floating is the zone floating times and dates are read in.
	Floating *time.Location
}

// Components lists the components of a calendar object that carry data.
func Components(cal *ical.Component) []*ical.Component {
	var out []*ical.Component
	for _, c := range cal.Children {
		switch c.Name {
		case "VEVENT", "VTODO", "VJOURNAL", "VFREEBUSY":
			out = append(out, c)
		}
	}
	return out
}

// Read takes a calendar object apart. floating is the zone of the
// calendar, for floating times; nil is UTC.
func Read(cal *ical.Component, floating *time.Location) *Object {
	if floating == nil {
		floating = time.UTC
	}
	o := &Object{Cal: cal, Overrides: map[int64]*ical.Component{}, Zones: ical.ZonesOf(cal), Floating: floating}
	for _, c := range Components(cal) {
		if o.Kind == "" {
			o.Kind, o.UID = c.Name, c.Text("UID")
		}
		if p := c.Get("RECURRENCE-ID"); p != nil {
			if d, err := o.Zones.ParseDateTime(p); err == nil {
				o.Overrides[d.In(floating).Unix()] = c
				continue
			}
		}
		if o.Master == nil {
			o.Master = c
		}
	}
	return o
}

// times returns the start and end of a component: DTSTART and DTEND, or
// DTSTART plus DURATION, or for a date one day; for a VTODO DUE.
func (o *Object) times(c *ical.Component) (start, end time.Time, date, ok bool) {
	p := c.Get("DTSTART")
	if p == nil {
		p = c.Get("DUE")
		if p == nil {
			return
		}
	}
	d, err := o.Zones.ParseDateTime(p)
	if err != nil {
		return
	}
	start, date = d.In(o.Floating), d.Date
	end = start
	if e := c.Get("DTEND"); e != nil {
		if ed, err := o.Zones.ParseDateTime(e); err == nil && !ed.In(o.Floating).Before(start) {
			end = ed.In(o.Floating)
		}
	} else if du := c.Get("DURATION"); du != nil {
		if dur, err := ical.ParseDuration(du.Value); err == nil && !dur.Negative {
			end = dur.AddTo(start)
		}
	} else if date {
		end = start.AddDate(0, 0, 1)
	}
	return start, end, date, true
}

// Set returns the recurrence set of the master component.
func (o *Object) Set() (ical.Set, bool) {
	if o.Master == nil {
		return ical.Set{}, false
	}
	start, _, _, ok := o.times(o.Master)
	if !ok {
		return ical.Set{}, false
	}
	set := ical.Set{Start: start}
	for _, p := range o.Master.All("RRULE") {
		r, err := ical.ParseRRule(p.Value)
		if err != nil {
			continue
		}
		if r.Until != nil && (r.Until.Floating || r.Until.Date) {
			u := ical.DateTime{Time: r.Until.In(start.Location())}
			if r.Until.Date {
				u.Time = time.Date(r.Until.Time.Year(), r.Until.Time.Month(), r.Until.Time.Day(), 23, 59, 59, 0, start.Location())
			}
			r.Until = &u
		}
		set.Rules = append(set.Rules, r)
	}
	for _, name := range []string{"RDATE", "EXDATE"} {
		for _, p := range o.Master.All(name) {
			ds, err := o.Zones.ParseDateTimeList(p)
			if err != nil {
				continue
			}
			for _, d := range ds {
				t := d.In(o.Floating)
				if d.Date && !start.IsZero() {
					// A date in a list for a timed event means that day at the
					// event's time.
					t = time.Date(t.Year(), t.Month(), t.Day(), start.Hour(), start.Minute(), start.Second(), 0, start.Location())
				}
				if name == "RDATE" {
					set.RDates = append(set.RDates, t)
				} else {
					set.ExDates = append(set.ExDates, t)
				}
			}
		}
	}
	return set, true
}

// MaxInstances bounds what one expansion returns.
const MaxInstances = 10_000

// maxSteps bounds the instances one expansion looks at, those before the
// range included. Rules without COUNT start near the range
// (ical.Set.IterFrom); this bounds the ones with a large COUNT.
const maxSteps = 250_000

// Instances returns the instances that overlap [from, to): those whose end
// is after from and whose start is before to (RFC 4791 9.9). An instance of
// zero length overlaps when it starts within the range. complete is false
// when the expansion stopped at its bounds.
func (o *Object) Instances(from, to time.Time, max int) (out []Instance, complete bool) {
	if max <= 0 || max > MaxInstances {
		max = MaxInstances
	}
	complete = true
	overlaps := func(s, e time.Time) bool {
		if e.Equal(s) {
			return !s.Before(from) && s.Before(to)
		}
		return e.After(from) && s.Before(to)
	}
	used := map[int64]bool{}
	add := func(inst Instance) bool {
		if len(out) >= max {
			complete = false
			return false
		}
		out = append(out, inst)
		return true
	}
	if set, ok := o.Set(); ok {
		mstart, mend, _, _ := o.times(o.Master)
		length := mend.Sub(mstart)
		dur := ical.Duration{}
		if p := o.Master.Get("DURATION"); p != nil {
			dur, _ = ical.ParseDuration(p.Value)
		}
		// An instance that started before from can still overlap it.
		lookFrom := from.Add(-length).Add(-48 * time.Hour)
		it := set.IterFrom(lookFrom)
		for steps := 0; ; steps++ {
			if steps == maxSteps {
				// Instances before from count too: the expansion stopped
				// without reaching to.
				complete = false
				break
			}
			t, more := it.Next()
			if !more {
				break
			}
			if !t.Before(to) {
				break
			}
			if t.Before(lookFrom) {
				continue
			}
			used[t.Unix()] = true
			if ov, ok := o.Overrides[t.Unix()]; ok {
				s, e, _, ok := o.times(ov)
				if ok && overlaps(s, e) && !cancelled(ov) {
					if !add(Instance{Start: s, End: e, RecurrenceID: t, Component: ov}) {
						break
					}
				}
				continue
			}
			end := t.Add(length)
			if dur != (ical.Duration{}) {
				end = dur.AddTo(t)
			}
			if overlaps(t, end) {
				rid := t
				if len(set.Rules) == 0 && len(set.RDates) == 0 {
					rid = time.Time{}
				}
				if !add(Instance{Start: t, End: end, RecurrenceID: rid, Component: o.Master}) {
					break
				}
			}
		}
	}
	// Overrides moved into the range from an instance outside it, and
	// overrides without a master.
	for rid, ov := range o.Overrides {
		if used[rid] {
			continue
		}
		s, e, _, ok := o.times(ov)
		if ok && overlaps(s, e) && !cancelled(ov) && (o.Master == nil || o.isInstance(time.Unix(rid, 0))) {
			add(Instance{Start: s, End: e, RecurrenceID: time.Unix(rid, 0).In(s.Location()), Component: ov})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, complete
}

// isInstance reports whether the master's set has an instance at t.
func (o *Object) isInstance(t time.Time) bool {
	set, ok := o.Set()
	if !ok {
		return false
	}
	it := set.Iter()
	for i := 0; i < 1_000_000; i++ {
		x, more := it.Next()
		if !more || x.After(t) {
			return false
		}
		if x.Equal(t) {
			return true
		}
	}
	return false
}

func cancelled(c *ical.Component) bool { return c.Value("STATUS") == "CANCELLED" }

// Span returns the earliest start and the latest end over all instances,
// for an index: an event outside [start, end) has no instance there. An
// event without end has the zero end with forever = true.
func (o *Object) Span() (start, end time.Time, forever bool) {
	first := true
	consider := func(s, e time.Time) {
		if first || s.Before(start) {
			start = s
		}
		if first || e.After(end) {
			end = e
		}
		first = false
	}
	if set, ok := o.Set(); ok {
		mstart, mend, _, _ := o.times(o.Master)
		length := mend.Sub(mstart)
		consider(mstart, mend)
		for _, r := range set.RDates {
			consider(r, r.Add(length))
		}
		if len(set.Rules) > 0 {
			if set.Infinite() {
				forever = true
			} else if last, ok := set.Last(MaxInstances * 10); ok {
				consider(last, last.Add(length))
			} else {
				forever = true
			}
		}
	}
	for _, ov := range o.Overrides {
		if s, e, _, ok := o.times(ov); ok {
			consider(s, e)
		}
	}
	return start, end, forever
}
