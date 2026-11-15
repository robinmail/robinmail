package jscal

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
)

// Event is a JSCalendar object as JSON: what JMAP sends and patches.
type Event = map[string]any

// Date formats of JSCalendar (RFC 8984 1.4.3, 1.4.4).
const (
	LocalFormat = "2006-01-02T15:04:05"
	UTCFormat   = "2006-01-02T15:04:05Z"
)

// LocalDateTime writes the wall clock of t.
func LocalDateTime(t time.Time) string { return t.Format(LocalFormat) }

// UTCDateTime writes t in UTC.
func UTCDateTime(t time.Time) string { return t.UTC().Format(UTCFormat) }

// ParseLocal reads a LocalDateTime into the wall clock of loc.
func ParseLocal(s string, loc *time.Location) (time.Time, bool) {
	t, err := time.ParseInLocation(LocalFormat, s, loc)
	return t, err == nil && t.Format(LocalFormat) == s
}

// ParseUTC reads a UTCDateTime. Fractions of seconds are allowed.
func ParseUTC(s string) (time.Time, bool) {
	if !strings.HasSuffix(s, "Z") {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// shortID makes a stable id for a sub-object from what identifies it, so
// that the same participant has the same id on every read (JMAP patches
// address it as "participants/<id>").
func shortID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:6])
}

// zoneOf returns the JSCalendar time zone of a DATE-TIME: an IANA name,
// or "" for floating. A zone that has no IANA name is UTC.
func zoneOf(d ical.DateTime) string {
	switch {
	case d.Date || d.Floating:
		return ""
	case d.TZID != "":
		if loc := d.Time.Location(); loc != time.UTC && ical.LoadZone(loc.String()) != nil && !strings.HasPrefix(loc.String(), "/") {
			return loc.String()
		}
		return "Etc/UTC"
	}
	return "Etc/UTC"
}

// ToJS converts a calendar object to a JSCalendar Event. Its overrides
// become recurrenceOverrides; EXDATEs are overrides that exclude; RDATEs
// that no rule makes are overrides with an empty patch. An object without
// a master (an invitation to some instances only) gives the first
// override as the event, with its recurrenceId.
func ToJS(o *Object) Event {
	master := o.Master
	if master == nil {
		var keys []int64
		for k := range o.Overrides {
			keys = append(keys, k)
		}
		if len(keys) == 0 {
			return Event{"@type": "Event", "uid": o.UID}
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		return o.componentJS(o.Overrides[keys[0]], true)
	}
	ev := o.componentJS(master, true)
	if prod := o.Cal.Text("PRODID"); prod != "" {
		ev["prodId"] = prod
	}
	tz, _ := ev["timeZone"].(string)
	loc := o.Floating
	if tz != "" {
		if l := ical.LoadZone(tz); l != nil {
			loc = l
		}
	}
	key := func(t time.Time) string { return LocalDateTime(t.In(loc)) }
	overrides := map[string]any{}
	set, _ := o.Set()
	for _, x := range set.ExDates {
		overrides[key(x)] = map[string]any{"excluded": true}
	}
	if len(set.RDates) > 0 {
		rules := ical.Set{Start: set.Start, Rules: set.Rules}
		for _, r := range set.RDates {
			if !inSet(rules, r) {
				overrides[key(r)] = map[string]any{}
			}
		}
	}
	for rid, ov := range o.Overrides {
		k := key(time.Unix(rid, 0))
		patch := diff(ev, o.componentJS(ov, false))
		if patch["start"] == k {
			// An instance starts at its recurrence id unless moved.
			delete(patch, "start")
		}
		if prev, ok := overrides[k].(map[string]any); ok {
			for pk, pv := range prev {
				if _, has := patch[pk]; !has && pk != "excluded" {
					patch[pk] = pv
				}
			}
		}
		overrides[k] = patch
	}
	if len(overrides) > 0 {
		ev["recurrenceOverrides"] = overrides
	}
	return ev
}

func inSet(s ical.Set, t time.Time) bool {
	it := s.Iter()
	for i := 0; i < 1_000_000; i++ {
		x, ok := it.Next()
		if !ok || x.After(t) {
			return false
		}
		if x.Equal(t) {
			return true
		}
	}
	return false
}

// notPatched are the properties an override never changes.
var notPatched = map[string]bool{"@type": true, "uid": true, "recurrenceRule": true, "recurrenceOverrides": true,
	"prodId": true, "recurrenceId": true, "recurrenceIdTimeZone": true, "relatedTo": true}

// diff is the patch that turns base into ov, property by property.
func diff(base, ov Event) map[string]any {
	patch := map[string]any{}
	for k, v := range ov {
		if notPatched[k] {
			continue
		}
		if bv, ok := base[k]; !ok || !reflect.DeepEqual(bv, v) {
			patch[k] = v
		}
	}
	for k := range base {
		if notPatched[k] {
			continue
		}
		if _, ok := ov[k]; !ok {
			patch[k] = nil
		}
	}
	return patch
}

// componentJS converts one VEVENT (or VTODO) to a JSCalendar object.
func (o *Object) componentJS(c *ical.Component, isMaster bool) Event {
	ev := Event{"@type": "Event"}
	if c.Name == "VTODO" {
		ev["@type"] = "Task"
	}
	if uid := c.Text("UID"); uid != "" {
		ev["uid"] = uid
	}
	z := o.Zones
	var startDT ical.DateTime
	if p := c.Get("DTSTART"); p != nil {
		if d, err := z.ParseDateTime(p); err == nil {
			startDT = d
			tz := zoneOf(d)
			if tz != "" {
				ev["timeZone"] = tz
				loc := ical.LoadZone(tz)
				ev["start"] = LocalDateTime(d.Time.In(loc))
			} else {
				ev["start"] = LocalDateTime(d.Time)
			}
			if d.Date {
				ev["showWithoutTime"] = true
			}
			if s, e, _, ok := o.times(c); ok && c.Name == "VEVENT" {
				var dur time.Duration
				if d.Date {
					dur = time.Duration(int(e.Sub(s).Hours()+12)/24) * 24 * time.Hour
				} else if de := c.Get("DTEND"); de != nil {
					ed, _ := z.ParseDateTime(de)
					if ed.TZID == d.TZID || ed.Floating == d.Floating {
						// Nominal: the difference of the two wall clocks.
						sw, ew := wall(s), wall(e)
						dur = ew.Sub(sw)
					} else {
						dur = e.Sub(s)
					}
				} else {
					dur = e.Sub(s)
				}
				if p := c.Get("DURATION"); p != nil {
					if du, err := ical.ParseDuration(p.Value); err == nil && !du.Negative {
						ev["duration"] = du.String()
					}
				} else if dur > 0 {
					ev["duration"] = ical.DurationOf(dur).String()
				}
			}
		}
	}
	if c.Name == "VTODO" {
		if p := c.Get("DUE"); p != nil {
			if d, err := z.ParseDateTime(p); err == nil {
				ev["due"] = LocalDateTime(d.Time)
			}
		}
	}
	if p := c.Get("RECURRENCE-ID"); p != nil && isMaster {
		if d, err := z.ParseDateTime(p); err == nil {
			ev["recurrenceId"] = LocalDateTime(d.Time)
			if tz := zoneOf(d); tz != "" {
				ev["recurrenceIdTimeZone"] = tz
			}
		}
	}
	text := func(prop, key string) {
		if p := c.Get(prop); p != nil {
			ev[key] = p.Text()
		}
	}
	text("SUMMARY", "title")
	text("DESCRIPTION", "description")
	if p := c.Get("STYLED-DESCRIPTION"); p != nil && strings.EqualFold(p.Param("FMTTYPE"), "text/html") {
		ev["description"] = p.Text()
		ev["descriptionContentType"] = "text/html"
	}
	text("COLOR", "color")
	if p := c.Get("CLASS"); p != nil {
		switch strings.ToUpper(p.Value) {
		case "PRIVATE":
			ev["privacy"] = "private"
		case "CONFIDENTIAL":
			ev["privacy"] = "secret"
		}
	}
	if strings.EqualFold(c.Value("TRANSP"), "TRANSPARENT") {
		ev["freeBusyStatus"] = "free"
	}
	if s := strings.ToLower(c.Value("STATUS")); s != "" {
		switch s {
		case "confirmed", "cancelled", "tentative":
			ev["status"] = s
		case "needs-action", "completed", "in-process":
			if c.Name == "VTODO" {
				ev["progress"] = s
			}
		}
	}
	if p := c.Get("PRIORITY"); p != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(p.Value)); err == nil && n > 0 && n <= 9 {
			ev["priority"] = n
		}
	}
	if p := c.Get("SEQUENCE"); p != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(p.Value)); err == nil && n > 0 {
			ev["sequence"] = n
		}
	}
	for prop, key := range map[string]string{"CREATED": "created", "LAST-MODIFIED": "updated"} {
		if p := c.Get(prop); p != nil {
			if d, err := z.ParseDateTime(p); err == nil {
				ev[key] = UTCDateTime(d.In(time.UTC))
			}
		}
	}
	if _, ok := ev["updated"]; !ok {
		if p := c.Get("DTSTAMP"); p != nil {
			if d, err := z.ParseDateTime(p); err == nil {
				ev["updated"] = UTCDateTime(d.In(time.UTC))
			}
		}
	}
	if cats := c.All("CATEGORIES"); len(cats) > 0 {
		kw := map[string]any{}
		for _, p := range cats {
			for _, w := range ical.TextList(p.Value) {
				if w != "" {
					kw[w] = true
				}
			}
		}
		if len(kw) > 0 {
			ev["keywords"] = kw
		}
	}
	// Locations: LOCATION, with GEO; CONFERENCE (RFC 7986) are virtual.
	if p := c.Get("LOCATION"); p != nil && p.Text() != "" {
		loc := map[string]any{"@type": "Location", "name": p.Text()}
		if g := c.Value("GEO"); g != "" {
			if lat, lon, ok := strings.Cut(g, ";"); ok {
				loc["coordinates"] = "geo:" + strings.TrimSpace(lat) + "," + strings.TrimSpace(lon)
			}
		}
		ev["locations"] = map[string]any{shortID("loc", p.Text()): loc}
	}
	if confs := c.All("CONFERENCE"); len(confs) > 0 {
		vl := map[string]any{}
		for _, p := range confs {
			x := map[string]any{"@type": "VirtualLocation", "uri": p.Value}
			if l := p.Param("LABEL"); l != "" {
				x["name"] = l
			}
			vl[shortID("vloc", p.Value)] = x
		}
		ev["virtualLocations"] = vl
	}
	links := map[string]any{}
	for _, p := range c.All("URL") {
		links[shortID("link", p.Value)] = map[string]any{"@type": "Link", "href": p.Value}
	}
	for _, p := range c.All("ATTACH") {
		if strings.EqualFold(p.Param("VALUE"), "BINARY") || strings.EqualFold(p.Param("ENCODING"), "BASE64") {
			continue // inline data stays in the iCalendar only
		}
		l := map[string]any{"@type": "Link", "href": p.Value, "rel": "enclosure"}
		if t := p.Param("FMTTYPE"); t != "" {
			l["contentType"] = t
		}
		if t := p.Param("FILENAME"); t != "" {
			l["title"] = t
		}
		links[shortID("attach", p.Value)] = l
	}
	if len(links) > 0 {
		ev["links"] = links
	}
	if rel := c.All("RELATED-TO"); len(rel) > 0 {
		m := map[string]any{}
		for _, p := range rel {
			kind := strings.ToLower(p.Param("RELTYPE"))
			if kind == "" {
				kind = "parent"
			}
			m[p.Text()] = map[string]any{"@type": "Relation", "relation": map[string]any{kind: true}}
		}
		ev["relatedTo"] = m
	}
	if isMaster || c.Get("RRULE") != nil {
		if p := c.Get("RRULE"); p != nil {
			if r, err := ical.ParseRRule(p.Value); err == nil {
				ev["recurrenceRule"] = ruleJS(r, startDT)
			}
		}
	}
	if parts := participantsJS(c); len(parts) > 0 {
		ev["participants"] = parts
	}
	if p := c.Get("ORGANIZER"); p != nil {
		ev["organizerCalendarAddress"] = p.Value
	}
	if alerts := alertsJS(c, z); len(alerts) > 0 {
		ev["alerts"] = alerts
	}
	return ev
}

// wall is the wall clock of t as if it were UTC.
func wall(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

var freqJS = []string{"secondly", "minutely", "hourly", "daily", "weekly", "monthly", "yearly"}

// ruleJS writes a RecurrenceRule object (RFC 8984 4.3.3).
func ruleJS(r ical.RRule, start ical.DateTime) map[string]any {
	m := map[string]any{"@type": "RecurrenceRule", "frequency": freqJS[r.Freq]}
	if r.Interval > 1 {
		m["interval"] = r.Interval
	}
	if r.WKST != time.Monday {
		m["firstDayOfWeek"] = strings.ToLower(ical.DayName(r.WKST))
	}
	if len(r.ByDay) > 0 {
		var days []any
		for _, d := range r.ByDay {
			nd := map[string]any{"@type": "NDay", "day": strings.ToLower(ical.DayName(d.Day))}
			if d.N != 0 {
				nd["nthOfPeriod"] = d.N
			}
			days = append(days, nd)
		}
		m["byDay"] = days
	}
	ints := func(key string, xs []int) {
		if len(xs) > 0 {
			l := make([]any, len(xs))
			for i, x := range xs {
				l[i] = x
			}
			m[key] = l
		}
	}
	ints("byMonthDay", r.ByMonthDay)
	if len(r.ByMonth) > 0 {
		l := make([]any, len(r.ByMonth))
		for i, x := range r.ByMonth {
			l[i] = strconv.Itoa(x)
		}
		m["byMonth"] = l
	}
	ints("byYearDay", r.ByYearDay)
	ints("byWeekNo", r.ByWeekNo)
	ints("byHour", r.ByHour)
	ints("byMinute", r.ByMinute)
	ints("bySecond", r.BySecond)
	ints("bySetPosition", r.BySetPos)
	if r.Count > 0 {
		m["count"] = r.Count
	}
	if r.Until != nil {
		u := r.Until.Time
		if r.Until.IsUTC() && !start.Date && !start.Floating && start.Time.Location() != nil {
			u = u.In(start.Time.Location())
		}
		if r.Until.Date {
			m["until"] = LocalDateTime(time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC))
		} else {
			m["until"] = LocalDateTime(u)
		}
	}
	for _, x := range r.Extra {
		if k, v, ok := strings.Cut(x, "="); ok {
			switch k {
			case "RSCALE":
				m["rscale"] = strings.ToLower(v)
			case "SKIP":
				m["skip"] = strings.ToLower(v)
			}
		}
	}
	return m
}

// participantsJS makes participants from ORGANIZER and ATTENDEEs. One
// address is one participant; the organizer who also attends has both
// roles.
func participantsJS(c *ical.Component) map[string]any {
	out := map[string]any{}
	get := func(addr string) map[string]any {
		id := shortID("p", strings.ToLower(addr))
		if p, ok := out[id].(map[string]any); ok {
			return p
		}
		p := map[string]any{"@type": "Participant", "calendarAddress": addr, "roles": map[string]any{}}
		if email := mailto(addr); email != "" {
			p["email"] = email
		}
		out[id] = p
		return p
	}
	common := func(p map[string]any, prop *ical.Prop) {
		if cn := prop.Param("CN"); cn != "" {
			p["name"] = cn
		}
		if e := prop.Param("EMAIL"); e != "" {
			p["email"] = e
		}
		if s := prop.Param("SENT-BY"); s != "" {
			p["sentBy"] = s
		}
		if s := prop.Param("SCHEDULE-AGENT"); s != "" {
			p["scheduleAgent"] = strings.ToLower(s)
		}
		if s := prop.ParamValues("SCHEDULE-STATUS"); len(s) > 0 {
			l := make([]any, len(s))
			for i, x := range s {
				l[i] = x
			}
			p["scheduleStatus"] = l
		}
	}
	if o := c.Get("ORGANIZER"); o != nil {
		p := get(o.Value)
		p["roles"].(map[string]any)["owner"] = true
		common(p, o)
	}
	for _, a := range c.All("ATTENDEE") {
		p := get(a.Value)
		roles := p["roles"].(map[string]any)
		switch strings.ToUpper(a.Param("ROLE")) {
		case "CHAIR":
			roles["attendee"], roles["chair"] = true, true
		case "OPT-PARTICIPANT":
			roles["attendee"], roles["optional"] = true, true
		case "NON-PARTICIPANT":
			roles["informational"] = true
		default:
			roles["attendee"] = true
		}
		common(p, a)
		if s := strings.ToLower(a.Param("PARTSTAT")); s != "" {
			p["participationStatus"] = s
		} else {
			p["participationStatus"] = "needs-action"
		}
		if strings.EqualFold(a.Param("RSVP"), "TRUE") {
			p["expectReply"] = true
		}
		switch k := strings.ToLower(a.Param("CUTYPE")); k {
		case "group", "resource", "individual":
			p["kind"] = k
		case "room":
			p["kind"] = "location"
		}
		if d := a.ParamValues("DELEGATED-TO"); len(d) > 0 {
			m := map[string]any{}
			for _, x := range d {
				m[shortID("p", strings.ToLower(x))] = true
			}
			p["delegatedTo"] = m
		}
		if d := a.ParamValues("DELEGATED-FROM"); len(d) > 0 {
			m := map[string]any{}
			for _, x := range d {
				m[shortID("p", strings.ToLower(x))] = true
			}
			p["delegatedFrom"] = m
		}
		if s := a.Param("X-SCHEDULE-SEQUENCE"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				p["scheduleSequence"] = n
			}
		}
	}
	return out
}

// mailto returns the address of a mailto: URI, or "".
func mailto(uri string) string {
	if len(uri) > 7 && strings.EqualFold(uri[:7], "mailto:") {
		return uri[7:]
	}
	return ""
}

// alertsJS converts VALARMs (RFC 8984 4.5.2).
func alertsJS(c *ical.Component, z ical.Zones) map[string]any {
	out := map[string]any{}
	for i, a := range c.Sub("VALARM") {
		alert := map[string]any{"@type": "Alert"}
		action := strings.ToLower(a.Value("ACTION"))
		if action == "email" {
			alert["action"] = "email"
		}
		tp := a.Get("TRIGGER")
		if tp == nil {
			continue
		}
		if strings.EqualFold(tp.Param("VALUE"), "DATE-TIME") {
			d, err := z.ParseDateTime(tp)
			if err != nil {
				continue
			}
			alert["trigger"] = map[string]any{"@type": "AbsoluteTrigger", "when": UTCDateTime(d.In(time.UTC))}
		} else {
			du, err := ical.ParseDuration(tp.Value)
			if err != nil {
				continue
			}
			tr := map[string]any{"@type": "OffsetTrigger", "offset": du.String()}
			if strings.EqualFold(tp.Param("RELATED"), "END") {
				tr["relativeTo"] = "end"
			}
			alert["trigger"] = tr
		}
		if p := a.Get("ACKNOWLEDGED"); p != nil {
			if d, err := z.ParseDateTime(p); err == nil {
				alert["acknowledged"] = UTCDateTime(d.In(time.UTC))
			}
		}
		id := a.Text("UID")
		if id == "" {
			id = a.Text("X-WR-ALARMUID")
		}
		if id == "" {
			id = "alert" + strconv.Itoa(i+1)
		}
		out[id] = alert
	}
	return out
}
