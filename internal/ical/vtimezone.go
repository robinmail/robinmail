package ical

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ZonesOf resolves the VTIMEZONEs of a VCALENDAR. Most TZIDs are IANA
// names (or Windows names, or names that end in one) and need nothing.
// For one that is not, the zone's own rules are compared with the IANA
// zones Go has: the zone whose offsets agree at every change of the
// VTIMEZONE over a span of years is taken. When none agrees, the zone
// becomes its standard offset, fixed: correct half the year at worst, and
// the TZID and the VTIMEZONE are kept for clients that read them.
func ZonesOf(cal *Component) Zones {
	z := Zones{Defined: map[string]*time.Location{}}
	for _, vt := range cal.Sub("VTIMEZONE") {
		id := vt.Text("TZID")
		if id == "" || LoadZone(id) != nil {
			continue
		}
		if loc := LoadZone(vt.Text("X-LIC-LOCATION")); loc != nil {
			z.Defined[id] = loc
			continue
		}
		z.Defined[id] = matchZone(vt)
	}
	return z
}

// ParseOffset reads a UTC-OFFSET value: "+0100", "-053000".
func ParseOffset(s string) (int, error) {
	s = strings.TrimSpace(s)
	if len(s) != 5 && len(s) != 7 || (s[0] != '+' && s[0] != '-') {
		return 0, fmt.Errorf("%w: bad UTC-OFFSET %q", ErrSyntax, s)
	}
	h, e1 := strconv.Atoi(s[1:3])
	m, e2 := strconv.Atoi(s[3:5])
	sec := 0
	var e3 error
	if len(s) == 7 {
		sec, e3 = strconv.Atoi(s[5:7])
	}
	if e1 != nil || e2 != nil || e3 != nil || h > 23 || m > 59 || sec > 59 {
		return 0, fmt.Errorf("%w: bad UTC-OFFSET %q", ErrSyntax, s)
	}
	off := h*3600 + m*60 + sec
	if s[0] == '-' {
		off = -off
	}
	return off, nil
}

// FormatOffset writes a UTC-OFFSET.
func FormatOffset(off int) string {
	sign := '+'
	if off < 0 {
		sign, off = '-', -off
	}
	if off%60 != 0 {
		return fmt.Sprintf("%c%02d%02d%02d", sign, off/3600, off%3600/60, off%60)
	}
	return fmt.Sprintf("%c%02d%02d", sign, off/3600, off%3600/60)
}

type transition struct {
	at  time.Time
	off int
}

// transitions lists when the offsets of a VTIMEZONE change between two
// years, in order.
func transitions(vt *Component, from, to int) []transition {
	var out []transition
	end := time.Date(to, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, obs := range vt.Children {
		if obs.Name != "STANDARD" && obs.Name != "DAYLIGHT" {
			continue
		}
		offFrom, err1 := ParseOffset(obs.Value("TZOFFSETFROM"))
		offTo, err2 := ParseOffset(obs.Value("TZOFFSETTO"))
		p := obs.Get("DTSTART")
		if err1 != nil || err2 != nil || p == nil {
			continue
		}
		start, err := Zones{}.parseValue(strings.TrimSpace(p.Value), "", false)
		if err != nil {
			continue
		}
		// DTSTART of an observance is local time in the offset before it.
		loc := time.FixedZone("", offFrom)
		set := Set{Start: start.In(loc)}
		for _, rp := range obs.All("RRULE") {
			if r, err := ParseRRule(rp.Value); err == nil {
				set.Rules = append(set.Rules, r)
			}
		}
		for _, rp := range obs.All("RDATE") {
			if ds, err := (Zones{}).ParseDateTimeList(rp); err == nil {
				for _, d := range ds {
					set.RDates = append(set.RDates, d.In(loc))
				}
			}
		}
		it := set.Iter()
		for i := 0; i < 2000; i++ {
			t, ok := it.Next()
			if !ok || t.After(end) {
				break
			}
			if t.Year() >= from-1 {
				out = append(out, transition{at: t.UTC(), off: offTo})
			}
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].at.Before(out[j-1].at); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// candidates are the zones tried for a VTIMEZONE without an IANA name.
func candidates() []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range windowsZones {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range []string{"Europe/Amsterdam", "Europe/Brussels", "Europe/London", "Europe/Dublin", "Europe/Lisbon",
		"America/Toronto", "America/Vancouver", "Australia/Melbourne", "Asia/Hong_Kong", "Asia/Kolkata", "UTC"} {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func matchZone(vt *Component) *time.Location {
	year := time.Now().Year()
	ts := transitions(vt, year-1, year+3)
	std := 0
	for _, obs := range vt.Sub("STANDARD") {
		if off, err := ParseOffset(obs.Value("TZOFFSETTO")); err == nil {
			std = off
		}
	}
	if len(ts) == 0 {
		return time.FixedZone(vt.Text("TZID"), std)
	}
	// The probe instants: a day after each change, and a day before.
	type probe struct {
		at  time.Time
		off int
	}
	var probes []probe
	for i, t := range ts {
		probes = append(probes, probe{t.at.Add(24 * time.Hour), t.off})
		if i > 0 {
			probes = append(probes, probe{t.at.Add(-24 * time.Hour), ts[i-1].off})
		}
	}
	for _, name := range candidates() {
		loc, err := time.LoadLocation(name)
		if err != nil {
			continue
		}
		ok := true
		for _, p := range probes {
			if _, off := p.at.In(loc).Zone(); off != p.off {
				ok = false
				break
			}
		}
		if ok {
			return loc
		}
	}
	return time.FixedZone(vt.Text("TZID"), std)
}

// VTimezone makes a VTIMEZONE for an IANA zone, with the changes of its
// offset in the years from..to as RDATEs, for clients that want one in
// every object. A zone without changes gets one STANDARD.
func VTimezone(loc *time.Location, from, to int) *Component {
	vt := &Component{Name: "VTIMEZONE"}
	vt.SetText("TZID", loc.String())
	start := time.Date(from, 1, 1, 0, 0, 0, 0, loc)
	_, prevOff := start.Zone()
	type obs struct {
		dst     bool
		name    string
		from    int
		to      int
		changes []time.Time
	}
	var list []*obs
	find := func(dst bool, name string, from, to int) *obs {
		for _, o := range list {
			if o.dst == dst && o.name == name && o.from == from && o.to == to {
				return o
			}
		}
		o := &obs{dst: dst, name: name, from: from, to: to}
		list = append(list, o)
		return o
	}
	end := time.Date(to+1, 1, 1, 0, 0, 0, 0, loc)
	t := start
	for t.Before(end) {
		_, next := t.ZoneBounds()
		if next.IsZero() || !next.Before(end) {
			break
		}
		name, off := next.Zone()
		o := find(next.IsDST(), name, prevOff, off)
		// Local time before the change, in the old offset.
		o.changes = append(o.changes, next.In(time.FixedZone("", prevOff)))
		prevOff = off
		t = next
	}
	if len(list) == 0 {
		name, off := start.Zone()
		c := &Component{Name: "STANDARD"}
		c.Add(Prop{Name: "DTSTART", Value: "19700101T000000"})
		c.Add(Prop{Name: "TZOFFSETFROM", Value: FormatOffset(off)})
		c.Add(Prop{Name: "TZOFFSETTO", Value: FormatOffset(off)})
		c.SetText("TZNAME", name)
		vt.Children = append(vt.Children, c)
		return vt
	}
	for _, o := range list {
		kind := "STANDARD"
		if o.dst {
			kind = "DAYLIGHT"
		}
		c := &Component{Name: kind}
		c.Add(Prop{Name: "DTSTART", Value: FormatLocal(o.changes[0])})
		c.Add(Prop{Name: "TZOFFSETFROM", Value: FormatOffset(o.from)})
		c.Add(Prop{Name: "TZOFFSETTO", Value: FormatOffset(o.to)})
		if len(o.changes) > 1 {
			var vs []string
			for _, ch := range o.changes[1:] {
				vs = append(vs, FormatLocal(ch))
			}
			c.Add(Prop{Name: "RDATE", Value: strings.Join(vs, ",")})
		}
		if o.name != "" {
			c.SetText("TZNAME", o.name)
		}
		vt.Children = append(vt.Children, c)
	}
	return vt
}
