package jscal

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
)

// PropError names the properties of an Event that are invalid, for the
// invalidProperties SetError of JMAP.
type PropError struct {
	Props []string
	Msg   string
}

func (e *PropError) Error() string {
	return "invalid properties " + strings.Join(e.Props, ", ") + ": " + e.Msg
}

func bad(prop, format string, args ...any) error {
	return &PropError{Props: []string{prop}, Msg: fmt.Sprintf(format, args...)}
}

// handled are the iCalendar properties ToICal writes from JSCalendar;
// a previous component keeps every other one.
var handled = []string{"UID", "DTSTART", "DTEND", "DURATION", "DUE", "RECURRENCE-ID", "SUMMARY", "DESCRIPTION",
	"STYLED-DESCRIPTION", "COLOR", "CLASS", "TRANSP", "STATUS", "PRIORITY", "SEQUENCE", "CREATED", "LAST-MODIFIED",
	"CATEGORIES", "LOCATION", "GEO", "CONFERENCE", "URL", "RELATED-TO", "RRULE", "RDATE", "EXDATE", "ORGANIZER",
	"ATTENDEE", "DTSTAMP"}

// ToICal converts a JSCalendar Event into a VCALENDAR with the master
// VEVENT and one VEVENT per override that patches more than exclusion.
// prev, the stored object, gives the properties JSCalendar has no word
// for, which stay as they were. now is DTSTAMP.
func ToICal(ev Event, prev *ical.Component, prodID string, now time.Time) (*ical.Component, error) {
	cal := &ical.Component{Name: "VCALENDAR"}
	cal.Add(ical.Prop{Name: "VERSION", Value: "2.0"})
	cal.SetText("PRODID", prodID)
	var prevMaster *ical.Component
	prevOverrides := map[string]*ical.Component{}
	if prev != nil {
		for _, p := range prev.Props {
			if p.Name != "VERSION" && p.Name != "PRODID" && p.Name != "METHOD" {
				cal.Add(p)
			}
		}
		for _, c := range Components(prev) {
			if rid := c.Value("RECURRENCE-ID"); rid != "" {
				prevOverrides[rid] = c
			} else if prevMaster == nil {
				prevMaster = c
			}
		}
	}
	master, err := componentICal(ev, prevMaster, now)
	if err != nil {
		return nil, err
	}
	zones := map[string]bool{}
	note := func(c *ical.Component) {
		for _, p := range c.Props {
			if tz := p.Param("TZID"); tz != "" {
				zones[tz] = true
			}
		}
	}
	note(master)
	var overrides []*ical.Component
	if ro, ok := ev["recurrenceOverrides"]; ok && ro != nil {
		m, ok := ro.(map[string]any)
		if !ok {
			return nil, bad("recurrenceOverrides", "not an object")
		}
		tz, _ := ev["timeZone"].(string)
		loc := time.UTC
		if tz != "" {
			loc = ical.LoadZone(tz)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var exdates, rdates []string
		date := ev["showWithoutTime"] == true
		for _, k := range keys {
			t, ok := ParseLocal(k, loc)
			if !ok {
				return nil, bad("recurrenceOverrides/"+k, "not a LocalDateTime")
			}
			patch, ok := m[k].(map[string]any)
			if !ok {
				return nil, bad("recurrenceOverrides/"+k, "not a PatchObject")
			}
			value := ridValue(t, tz, date)
			if patch["excluded"] == true {
				exdates = append(exdates, value)
				continue
			}
			if len(patch) == 0 {
				rdates = append(rdates, value)
				continue
			}
			inst, err := ApplyPatch(ev, patch)
			if err != nil {
				return nil, bad("recurrenceOverrides/"+k, "%v", err)
			}
			delete(inst, "recurrenceRule")
			delete(inst, "recurrenceOverrides")
			if _, has := inst["start"]; !has || patch["start"] == nil {
				inst["start"] = k
			}
			rid := ridProp(t, tz, date)
			c, err := componentICal(inst, prevOverrides[rid.Value], now)
			if err != nil {
				var pe *PropError
				if e, ok := err.(*PropError); ok {
					pe = e
					for i := range pe.Props {
						pe.Props[i] = "recurrenceOverrides/" + k + "/" + pe.Props[i]
					}
				}
				return nil, err
			}
			c.Remove("RECURRENCE-ID", "RRULE", "RDATE", "EXDATE")
			c.Props = append([]ical.Prop{c.Props[0], rid}, c.Props[1:]...)
			if !inRule(ev, master, t) {
				rdates = append(rdates, value)
			}
			note(c)
			overrides = append(overrides, c)
		}
		addList := func(name string, values []string) {
			if len(values) == 0 {
				return
			}
			p := ical.Prop{Name: name, Value: strings.Join(values, ",")}
			if date {
				p.SetParam("VALUE", "DATE")
			} else if tz != "" && tz != "Etc/UTC" && tz != "UTC" {
				p.SetParam("TZID", tz)
			}
			master.Add(p)
		}
		addList("RDATE", rdates)
		addList("EXDATE", exdates)
	}
	// VTIMEZONEs for the zones used, for clients that want them (RFC 5545
	// 3.6.5 says a TZID MUST have one; CalDAV servers may drop them, RFC
	// 7809, but not all clients ask for that).
	var zoneNames []string
	for z := range zones {
		zoneNames = append(zoneNames, z)
	}
	sort.Strings(zoneNames)
	have := map[string]bool{}
	if prev != nil {
		for _, vt := range prev.Sub("VTIMEZONE") {
			if zones[vt.Text("TZID")] {
				cal.Children = append(cal.Children, vt)
				have[vt.Text("TZID")] = true
			}
		}
	}
	year := now.Year()
	if s, ok := ev["start"].(string); ok && len(s) >= 4 {
		if y, err := strconv.Atoi(s[:4]); err == nil {
			year = y
		}
	}
	for _, z := range zoneNames {
		if have[z] {
			continue
		}
		if loc := ical.LoadZone(z); loc != nil {
			cal.Children = append(cal.Children, ical.VTimezone(loc, year, year+1))
		}
	}
	cal.Children = append(cal.Children, master)
	cal.Children = append(cal.Children, overrides...)
	return cal, nil
}

func ridValue(t time.Time, tz string, date bool) string {
	switch {
	case date:
		return ical.FormatDate(t)
	case tz == "Etc/UTC" || tz == "UTC":
		return ical.FormatUTC(t)
	}
	return ical.FormatLocal(t)
}

func ridProp(t time.Time, tz string, date bool) ical.Prop {
	p := ical.Prop{Name: "RECURRENCE-ID", Value: ridValue(t, tz, date)}
	if date {
		p.SetParam("VALUE", "DATE")
	} else if tz != "" && tz != "Etc/UTC" && tz != "UTC" {
		p.SetParam("TZID", tz)
	}
	return p
}

// inRule reports whether the master's rule (without RDATEs) makes t.
func inRule(ev Event, master *ical.Component, t time.Time) bool {
	cal := &ical.Component{Name: "VCALENDAR", Children: []*ical.Component{master}}
	o := Read(cal, t.Location())
	set, ok := o.Set()
	if !ok || len(set.Rules) == 0 {
		return false
	}
	set.RDates, set.ExDates = nil, nil
	return inSet(set, t)
}

func str(ev Event, key string) (string, bool, error) {
	v, ok := ev[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, bad(key, "not a string")
	}
	return s, true, nil
}

func num(ev Event, key string) (int, bool, error) {
	v, ok := ev[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	switch n := v.(type) {
	case float64:
		if n != float64(int(n)) {
			return 0, false, bad(key, "not an integer")
		}
		return int(n), true, nil
	case int:
		return n, true, nil
	case int64:
		return int(n), true, nil
	}
	return 0, false, bad(key, "not a number")
}

func obj(ev Event, key string) (map[string]any, error) {
	v, ok := ev[key]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, bad(key, "not an object")
	}
	return m, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// componentICal writes one VEVENT (or VTODO) from a JSCalendar object.
func componentICal(ev Event, prev *ical.Component, now time.Time) (*ical.Component, error) {
	name := "VEVENT"
	if t, _ := ev["@type"].(string); t == "Task" {
		name = "VTODO"
	} else if t != "" && t != "Event" {
		return nil, bad("@type", "must be Event")
	}
	c := &ical.Component{Name: name}
	if prev != nil {
		c = prev.Clone()
		c.Name = name
		c.Remove(handled...)
		// Alarms are rewritten from alerts below.
		var keep []*ical.Component
		for _, x := range c.Children {
			if x.Name != "VALARM" {
				keep = append(keep, x)
			}
		}
		c.Children = keep
	}
	var props []ical.Prop
	add := func(p ical.Prop) { props = append(props, p) }
	text := func(name, value string) { add(ical.Prop{Name: name, Value: ical.Escape(value)}) }

	uid, ok, err := str(ev, "uid")
	if err != nil {
		return nil, err
	}
	if !ok || uid == "" {
		return nil, bad("uid", "required")
	}
	text("UID", uid)
	add(ical.Prop{Name: "DTSTAMP", Value: ical.FormatUTC(now)})

	tz, _, err := str(ev, "timeZone")
	if err != nil {
		return nil, err
	}
	loc := time.UTC
	if tz != "" {
		if loc = ical.LoadZone(tz); loc == nil {
			return nil, bad("timeZone", "unknown time zone %q", tz)
		}
	}
	date := ev["showWithoutTime"] == true
	startS, hasStart, err := str(ev, "start")
	if err != nil {
		return nil, err
	}
	var start time.Time
	if hasStart {
		var ok bool
		if start, ok = ParseLocal(startS, loc); !ok {
			return nil, bad("start", "not a LocalDateTime")
		}
		p := ical.Prop{Name: "DTSTART"}
		switch {
		case date:
			p.SetParam("VALUE", "DATE")
			p.Value = ical.FormatDate(start)
		case tz == "":
			p.Value = ical.FormatLocal(start)
		case tz == "Etc/UTC" || tz == "UTC":
			p.Value = ical.FormatUTC(start)
		default:
			p.SetParam("TZID", tz)
			p.Value = ical.FormatLocal(start)
		}
		add(p)
	} else if name == "VEVENT" {
		return nil, bad("start", "required")
	}
	if durS, ok, err := str(ev, "duration"); err != nil {
		return nil, err
	} else if ok && name == "VEVENT" {
		d, err := ical.ParseDuration(durS)
		if err != nil || d.Negative {
			return nil, bad("duration", "not a Duration")
		}
		if date {
			// A whole-day event ends on a date (RFC 5545 3.6.1: DTEND is
			// exclusive).
			days := d.Days + 7*d.Weeks
			if d.Time > 0 {
				days++
			}
			if days == 0 {
				days = 1
			}
			add(ical.Prop{Name: "DTEND", Params: []ical.Param{{Name: "VALUE", Values: []string{"DATE"}}}, Value: ical.FormatDate(start.AddDate(0, 0, days))})
		} else {
			add(ical.Prop{Name: "DURATION", Value: d.String()})
		}
	}
	if due, ok, err := str(ev, "due"); err != nil {
		return nil, err
	} else if ok {
		t, ok := ParseLocal(due, loc)
		if !ok {
			return nil, bad("due", "not a LocalDateTime")
		}
		p := ical.Prop{Name: "DUE", Value: ical.FormatLocal(t)}
		if tz != "" && tz != "Etc/UTC" {
			p.SetParam("TZID", tz)
		} else if tz != "" {
			p.Value = ical.FormatUTC(t)
		}
		add(p)
	}
	if rid, ok, err := str(ev, "recurrenceId"); err != nil {
		return nil, err
	} else if ok {
		rtz, _, _ := str(ev, "recurrenceIdTimeZone")
		rloc := loc
		if rtz != "" {
			if rloc = ical.LoadZone(rtz); rloc == nil {
				return nil, bad("recurrenceIdTimeZone", "unknown time zone")
			}
		} else {
			rtz = tz
		}
		t, ok := ParseLocal(rid, rloc)
		if !ok {
			return nil, bad("recurrenceId", "not a LocalDateTime")
		}
		add(ridProp(t, rtz, date))
	}
	for _, f := range []struct{ key, prop string }{{"title", "SUMMARY"}, {"color", "COLOR"}} {
		if s, ok, err := str(ev, f.key); err != nil {
			return nil, err
		} else if ok && s != "" {
			text(f.prop, s)
		}
	}
	if s, ok, err := str(ev, "description"); err != nil {
		return nil, err
	} else if ok && s != "" {
		ct, _, _ := str(ev, "descriptionContentType")
		if strings.HasPrefix(strings.ToLower(ct), "text/html") {
			p := ical.Prop{Name: "STYLED-DESCRIPTION", Value: ical.Escape(s)}
			p.SetParam("FMTTYPE", "text/html")
			add(p)
			text("DESCRIPTION", htmlText(s))
		} else {
			text("DESCRIPTION", s)
		}
	}
	if s, ok, err := str(ev, "privacy"); err != nil {
		return nil, err
	} else if ok {
		switch s {
		case "public":
		case "private":
			add(ical.Prop{Name: "CLASS", Value: "PRIVATE"})
		case "secret":
			add(ical.Prop{Name: "CLASS", Value: "CONFIDENTIAL"})
		default:
			return nil, bad("privacy", "unknown value")
		}
	}
	if s, ok, err := str(ev, "freeBusyStatus"); err != nil {
		return nil, err
	} else if ok {
		switch s {
		case "free":
			add(ical.Prop{Name: "TRANSP", Value: "TRANSPARENT"})
		case "busy":
			add(ical.Prop{Name: "TRANSP", Value: "OPAQUE"})
		default:
			return nil, bad("freeBusyStatus", "unknown value")
		}
	}
	if s, ok, err := str(ev, "status"); err != nil {
		return nil, err
	} else if ok {
		switch s {
		case "confirmed", "cancelled", "tentative":
			add(ical.Prop{Name: "STATUS", Value: strings.ToUpper(s)})
		default:
			return nil, bad("status", "unknown value")
		}
	} else if s, ok, _ := str(ev, "progress"); ok && name == "VTODO" {
		add(ical.Prop{Name: "STATUS", Value: strings.ToUpper(s)})
	}
	if n, ok, err := num(ev, "priority"); err != nil {
		return nil, err
	} else if ok && n > 0 {
		if n > 9 {
			return nil, bad("priority", "0 to 9")
		}
		add(ical.Prop{Name: "PRIORITY", Value: strconv.Itoa(n)})
	}
	if n, ok, err := num(ev, "sequence"); err != nil {
		return nil, err
	} else if ok && n > 0 {
		add(ical.Prop{Name: "SEQUENCE", Value: strconv.Itoa(n)})
	}
	for _, f := range []struct{ key, prop string }{{"created", "CREATED"}, {"updated", "LAST-MODIFIED"}} {
		if s, ok, err := str(ev, f.key); err != nil {
			return nil, err
		} else if ok {
			t, ok := ParseUTC(s)
			if !ok {
				return nil, bad(f.key, "not a UTCDateTime")
			}
			add(ical.Prop{Name: f.prop, Value: ical.FormatUTC(t)})
		}
	}
	if kw, err := obj(ev, "keywords"); err != nil {
		return nil, err
	} else if len(kw) > 0 {
		add(ical.Prop{Name: "CATEGORIES", Value: ical.JoinText(sortedKeys(kw), ",")})
	}
	if locs, err := obj(ev, "locations"); err != nil {
		return nil, err
	} else {
		for _, k := range sortedKeys(locs) {
			l, ok := locs[k].(map[string]any)
			if !ok {
				return nil, bad("locations/"+k, "not a Location")
			}
			if n, _ := l["name"].(string); n != "" {
				text("LOCATION", n)
				if g, _ := l["coordinates"].(string); strings.HasPrefix(g, "geo:") {
					lat, lon, ok := strings.Cut(strings.TrimPrefix(g, "geo:"), ",")
					if ok {
						lon, _, _ = strings.Cut(lon, ";")
						add(ical.Prop{Name: "GEO", Value: lat + ";" + lon})
					}
				}
				break // iCalendar has one LOCATION
			}
		}
	}
	if vl, err := obj(ev, "virtualLocations"); err != nil {
		return nil, err
	} else {
		for _, k := range sortedKeys(vl) {
			l, ok := vl[k].(map[string]any)
			if !ok {
				return nil, bad("virtualLocations/"+k, "not a VirtualLocation")
			}
			uri, _ := l["uri"].(string)
			if uri == "" {
				return nil, bad("virtualLocations/"+k+"/uri", "required")
			}
			p := ical.Prop{Name: "CONFERENCE", Value: uri}
			p.SetParam("VALUE", "URI")
			if n, _ := l["name"].(string); n != "" {
				p.SetParam("LABEL", n)
			}
			add(p)
		}
	}
	if links, err := obj(ev, "links"); err != nil {
		return nil, err
	} else {
		for _, k := range sortedKeys(links) {
			l, ok := links[k].(map[string]any)
			if !ok {
				return nil, bad("links/"+k, "not a Link")
			}
			href, _ := l["href"].(string)
			if href == "" {
				continue // a blobId only: the JMAP layer makes the href
			}
			if rel, _ := l["rel"].(string); rel == "enclosure" {
				p := ical.Prop{Name: "ATTACH", Value: href}
				if t, _ := l["contentType"].(string); t != "" {
					p.SetParam("FMTTYPE", t)
				}
				if t, _ := l["title"].(string); t != "" {
					p.SetParam("FILENAME", t)
				}
				add(p)
			} else {
				add(ical.Prop{Name: "URL", Value: href})
			}
		}
	}
	if rel, err := obj(ev, "relatedTo"); err != nil {
		return nil, err
	} else {
		for _, k := range sortedKeys(rel) {
			p := ical.Prop{Name: "RELATED-TO", Value: ical.Escape(k)}
			if r, ok := rel[k].(map[string]any); ok {
				if kinds, ok := r["relation"].(map[string]any); ok {
					for _, kind := range sortedKeys(kinds) {
						if kind != "parent" {
							p.SetParam("RELTYPE", strings.ToUpper(kind))
						}
						break
					}
				}
			}
			add(p)
		}
	}
	if rr, err := obj(ev, "recurrenceRule"); err != nil {
		return nil, err
	} else if rr != nil {
		r, err := ruleICal(rr, tz, date)
		if err != nil {
			return nil, err
		}
		add(ical.Prop{Name: "RRULE", Value: r.String()})
	}
	parts, err := participantsICal(ev)
	if err != nil {
		return nil, err
	}
	props = append(props, parts...)
	c.Props = append(props, c.Props...)
	alarms, err := alertsICal(ev)
	if err != nil {
		return nil, err
	}
	c.Children = append(c.Children, alarms...)
	return c, nil
}

// htmlText is a plain text version of HTML for DESCRIPTION.
func htmlText(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func ruleICal(m map[string]any, tz string, date bool) (ical.RRule, error) {
	r := ical.RRule{Interval: 1, WKST: time.Monday, Freq: -1}
	f, _ := m["frequency"].(string)
	for i, n := range freqJS {
		if n == f {
			r.Freq = ical.Freq(i)
		}
	}
	if r.Freq < 0 {
		return r, bad("recurrenceRule/frequency", "unknown")
	}
	ev := Event(m)
	if n, ok, err := num(ev, "interval"); err != nil {
		return r, bad("recurrenceRule/interval", "not a number")
	} else if ok {
		if n < 1 {
			return r, bad("recurrenceRule/interval", "at least 1")
		}
		r.Interval = n
	}
	if n, ok, err := num(ev, "count"); err != nil {
		return r, bad("recurrenceRule/count", "not a number")
	} else if ok {
		if n < 1 {
			return r, bad("recurrenceRule/count", "at least 1")
		}
		r.Count = n
	}
	if s, _ := m["firstDayOfWeek"].(string); s != "" {
		found := false
		for d := time.Sunday; d <= time.Saturday; d++ {
			if strings.ToLower(ical.DayName(d)) == s {
				r.WKST, found = d, true
			}
		}
		if !found {
			return r, bad("recurrenceRule/firstDayOfWeek", "unknown day")
		}
	}
	if days, ok := m["byDay"].([]any); ok {
		for _, x := range days {
			nd, _ := x.(map[string]any)
			day, _ := nd["day"].(string)
			found := false
			for d := time.Sunday; d <= time.Saturday; d++ {
				if strings.ToLower(ical.DayName(d)) == day {
					w := ical.WeekdayNum{Day: d}
					if n, ok, _ := num(Event(nd), "nthOfPeriod"); ok {
						w.N = n
					}
					r.ByDay = append(r.ByDay, w)
					found = true
				}
			}
			if !found {
				return r, bad("recurrenceRule/byDay", "unknown day")
			}
		}
	}
	ints := func(key string) ([]int, error) {
		l, ok := m[key].([]any)
		if !ok {
			return nil, nil
		}
		var out []int
		for _, x := range l {
			n, ok := x.(float64)
			if !ok {
				if i, isInt := x.(int); isInt {
					n, ok = float64(i), true
				}
			}
			if !ok || n != float64(int(n)) {
				return nil, bad("recurrenceRule/"+key, "not a list of integers")
			}
			out = append(out, int(n))
		}
		return out, nil
	}
	var err error
	if r.ByMonthDay, err = ints("byMonthDay"); err != nil {
		return r, err
	}
	if r.ByYearDay, err = ints("byYearDay"); err != nil {
		return r, err
	}
	if r.ByWeekNo, err = ints("byWeekNo"); err != nil {
		return r, err
	}
	if r.ByHour, err = ints("byHour"); err != nil {
		return r, err
	}
	if r.ByMinute, err = ints("byMinute"); err != nil {
		return r, err
	}
	if r.BySecond, err = ints("bySecond"); err != nil {
		return r, err
	}
	if r.BySetPos, err = ints("bySetPosition"); err != nil {
		return r, err
	}
	if l, ok := m["byMonth"].([]any); ok {
		for _, x := range l {
			s, _ := x.(string)
			n, err := strconv.Atoi(strings.TrimSuffix(s, "L"))
			if err != nil || n < 1 || n > 12 {
				return r, bad("recurrenceRule/byMonth", "not a month")
			}
			r.ByMonth = append(r.ByMonth, n)
		}
	}
	if s, _ := m["rscale"].(string); s != "" && s != "gregorian" {
		r.Extra = append(r.Extra, "RSCALE="+strings.ToUpper(s))
		if sk, _ := m["skip"].(string); sk != "" {
			r.Extra = append(r.Extra, "SKIP="+strings.ToUpper(sk))
		}
	}
	if u, _ := m["until"].(string); u != "" {
		loc := time.UTC
		if tz != "" {
			loc = ical.LoadZone(tz)
		}
		t, ok := ParseLocal(u, loc)
		if !ok {
			return r, bad("recurrenceRule/until", "not a LocalDateTime")
		}
		// RFC 5545 3.3.10: with a zoned DTSTART, UNTIL is in UTC.
		switch {
		case date:
			r.Until = &ical.DateTime{Time: t, Date: true}
		case tz == "":
			r.Until = &ical.DateTime{Time: t, Floating: true}
		default:
			r.Until = &ical.DateTime{Time: t.UTC()}
		}
	}
	if r.Count > 0 && r.Until != nil {
		return r, bad("recurrenceRule", "count and until together")
	}
	// Check what came out by reading it back.
	if _, err := ical.ParseRRule(r.String()); err != nil {
		return r, bad("recurrenceRule", "%v", err)
	}
	return r, nil
}

// participantsICal writes ORGANIZER and ATTENDEEs.
func participantsICal(ev Event) ([]ical.Prop, error) {
	parts, err := obj(ev, "participants")
	if err != nil {
		return nil, err
	}
	org, _, err := str(ev, "organizerCalendarAddress")
	if err != nil {
		return nil, err
	}
	var out []ical.Prop
	orgDone := false
	for _, id := range sortedKeys(parts) {
		p, ok := parts[id].(map[string]any)
		if !ok {
			return nil, bad("participants/"+id, "not a Participant")
		}
		addr, _ := p["calendarAddress"].(string)
		if addr == "" {
			if s, ok := p["sendTo"].(map[string]any); ok { // RFC 8984
				addr, _ = s["imip"].(string)
			}
		}
		email, _ := p["email"].(string)
		if addr == "" && email != "" {
			addr = "mailto:" + email
		}
		if addr == "" {
			return nil, bad("participants/"+id+"/calendarAddress", "required")
		}
		roles, _ := p["roles"].(map[string]any)
		name, _ := p["name"].(string)
		common := func(x *ical.Prop) {
			if name != "" {
				x.SetParam("CN", name)
			}
			if email != "" && !strings.EqualFold(mailto(addr), email) {
				x.SetParam("EMAIL", email)
			}
			if s, _ := p["sentBy"].(string); s != "" {
				x.SetParam("SENT-BY", s)
			}
			if s, _ := p["scheduleAgent"].(string); s != "" && s != "server" {
				x.SetParam("SCHEDULE-AGENT", strings.ToUpper(s))
			}
		}
		owner := roles["owner"] == true
		if owner && (org == "" || strings.EqualFold(org, addr)) && !orgDone {
			o := ical.Prop{Name: "ORGANIZER", Value: addr}
			common(&o)
			out = append(out, o)
			orgDone = true
			org = addr
		}
		attends := roles["attendee"] == true || roles["chair"] == true || roles["optional"] == true || roles["informational"] == true
		if !attends && (owner || len(roles) > 0) {
			continue
		}
		a := ical.Prop{Name: "ATTENDEE", Value: addr}
		common(&a)
		switch {
		case roles["chair"] == true:
			a.SetParam("ROLE", "CHAIR")
		case roles["optional"] == true:
			a.SetParam("ROLE", "OPT-PARTICIPANT")
		case roles["informational"] == true && roles["attendee"] != true:
			a.SetParam("ROLE", "NON-PARTICIPANT")
		default:
			a.SetParam("ROLE", "REQ-PARTICIPANT")
		}
		if s, _ := p["participationStatus"].(string); s != "" {
			switch s {
			case "needs-action", "accepted", "declined", "tentative", "delegated":
				a.SetParam("PARTSTAT", strings.ToUpper(s))
			default:
				return nil, bad("participants/"+id+"/participationStatus", "unknown value")
			}
		}
		if p["expectReply"] == true {
			a.SetParam("RSVP", "TRUE")
		}
		switch k, _ := p["kind"].(string); k {
		case "", "individual":
		case "group", "resource":
			a.SetParam("CUTYPE", strings.ToUpper(k))
		case "location":
			a.SetParam("CUTYPE", "ROOM")
		default:
			a.SetParam("CUTYPE", "UNKNOWN")
		}
		if n, ok, _ := num(Event(p), "scheduleSequence"); ok && n > 0 {
			a.SetParam("X-SCHEDULE-SEQUENCE", strconv.Itoa(n))
		}
		out = append(out, a)
	}
	if org != "" && !orgDone {
		out = append([]ical.Prop{{Name: "ORGANIZER", Value: org}}, out...)
	}
	return out, nil
}

func alertsICal(ev Event) ([]*ical.Component, error) {
	alerts, err := obj(ev, "alerts")
	if err != nil {
		return nil, err
	}
	var out []*ical.Component
	for _, id := range sortedKeys(alerts) {
		a, ok := alerts[id].(map[string]any)
		if !ok {
			return nil, bad("alerts/"+id, "not an Alert")
		}
		tr, ok := a["trigger"].(map[string]any)
		if !ok {
			return nil, bad("alerts/"+id+"/trigger", "required")
		}
		c := &ical.Component{Name: "VALARM"}
		c.SetText("UID", id)
		action := "DISPLAY"
		if s, _ := a["action"].(string); s == "email" {
			action = "EMAIL"
		}
		c.Add(ical.Prop{Name: "ACTION", Value: action})
		switch t, _ := tr["@type"].(string); t {
		case "AbsoluteTrigger":
			w, _ := tr["when"].(string)
			at, ok := ParseUTC(w)
			if !ok {
				return nil, bad("alerts/"+id+"/trigger/when", "not a UTCDateTime")
			}
			p := ical.Prop{Name: "TRIGGER", Value: ical.FormatUTC(at)}
			p.SetParam("VALUE", "DATE-TIME")
			c.Add(p)
		default:
			off, _ := tr["offset"].(string)
			d, err := ical.ParseDuration(off)
			if err != nil {
				return nil, bad("alerts/"+id+"/trigger/offset", "not a SignedDuration")
			}
			p := ical.Prop{Name: "TRIGGER", Value: d.String()}
			if rel, _ := tr["relativeTo"].(string); rel == "end" {
				p.SetParam("RELATED", "END")
			}
			c.Add(p)
		}
		if action == "DISPLAY" {
			text, _ := ev["title"].(string)
			if text == "" {
				text = "Reminder"
			}
			c.SetText("DESCRIPTION", text)
		} else {
			c.SetText("SUMMARY", "Reminder")
			c.SetText("DESCRIPTION", "Reminder")
		}
		if s, _ := a["acknowledged"].(string); s != "" {
			if at, ok := ParseUTC(s); ok {
				c.Add(ical.Prop{Name: "ACKNOWLEDGED", Value: ical.FormatUTC(at)})
			}
		}
		out = append(out, c)
	}
	return out, nil
}
