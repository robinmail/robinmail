package dav

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/jscal"
	"github.com/robinmail/robinmail/internal/store"
)

// report dispatches REPORT by the name of its body.
func (h *Handler) report(w http.ResponseWriter, r *http.Request, res *resource) {
	body, err := parseXML(r.Body)
	if err != nil || body == nil {
		h.fail(w, http.StatusBadRequest, "")
		return
	}
	switch body.Name {
	case name(nsCalDAV, "calendar-multiget"), name(nsCardDAV, "addressbook-multiget"):
		h.multiget(w, r, res, body)
	case name(nsCalDAV, "calendar-query"):
		h.calendarQuery(w, r, res, body)
	case name(nsCardDAV, "addressbook-query"):
		h.addressbookQuery(w, r, res, body)
	case name(nsDAV, "sync-collection"):
		h.syncCollection(w, r, res, body)
	case name(nsCalDAV, "free-busy-query"):
		h.freeBusyQuery(w, r, res, body)
	case name(nsDAV, "expand-property"):
		h.expandProperty(w, r, res, body)
	case name(nsDAV, "principal-property-search"):
		h.principalSearch(w, r, res, body)
	case name(nsDAV, "principal-search-property-set"):
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>` + "\n" + `<d:principal-search-property-set xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
			`<d:principal-search-property><d:prop><d:displayname/></d:prop><d:description xml:lang="en">Name</d:description></d:principal-search-property>` +
			`<d:principal-search-property><d:prop><c:calendar-user-address-set/></d:prop><d:description xml:lang="en">Address</d:description></d:principal-search-property>` +
			`</d:principal-search-property-set>`))
	default:
		// RFC 3253 3.6: a report the resource does not support.
		h.precondition(w, http.StatusForbidden, nsDAV, "supported-report")
	}
}

// requested reads the properties a report asks for.
func requested(body *node) []*node {
	if p := body.child(nsDAV, "prop"); p != nil {
		return p.Children
	}
	return nil
}

// objectData answers calendar-data and address-data, with the object's
// data loaded when needed. spec is the request's element (its content-type
// and restrictions); whole objects are returned.
func (h *Handler) objectData(ctx context.Context, res *resource, n xml.Name, spec *node) (string, bool) {
	if res.kind != kObject || res.obj == nil {
		return "", false
	}
	if (n != name(nsCalDAV, "calendar-data") || res.obj.Kind == store.ObjectCard) &&
		(n != name(nsCardDAV, "address-data") || res.obj.Kind != store.ObjectCard) {
		return "", false
	}
	if res.obj.Data == "" {
		list, err := h.PIM.Store.ObjectsByID(ctx, res.acc.Owner, []int64{res.obj.ID})
		if err != nil || len(list) == 0 {
			return "", false
		}
		res.obj.Data = list[0].Data
	}
	data := res.obj.Data
	if spec != nil {
		if v, _ := spec.attr("version"); v != "" && res.obj.Kind == store.ObjectCard {
			data = cardVersion(data, v)
		}
		if exp := spec.child(nsCalDAV, "expand"); exp != nil && res.obj.Kind != store.ObjectCard {
			data = expandData(data, exp, res.acc.Collection)
		}
	}
	return esc(data), true
}

// cardVersion gives a card in the asked version where only the VERSION
// line differs in what robinmail writes; others are returned as stored.
func cardVersion(data, version string) string {
	v, err := ical.ParseOne([]byte(data), "VCARD")
	if err != nil || v.Value("VERSION") == version {
		return data
	}
	v.Set(ical.Prop{Name: "VERSION", Value: version})
	return string(ical.Encode(v))
}

// expandData answers calendar-data with expand (RFC 4791 9.6.5): the
// instances in the range as separate components with RECURRENCE-ID, in
// UTC, without rules.
func expandData(data string, exp *node, coll store.Collection) string {
	start, _ := exp.attr("start")
	end, _ := exp.attr("end")
	from, err1 := time.Parse("20060102T150405Z", start)
	to, err2 := time.Parse("20060102T150405Z", end)
	cal, err := ical.ParseOne([]byte(data), "VCALENDAR")
	if err1 != nil || err2 != nil || err != nil {
		return data
	}
	o := jscal.Read(cal, zoneFor(coll))
	inst, _ := o.Instances(from, to, 1000)
	out := &ical.Component{Name: "VCALENDAR"}
	for _, p := range cal.Props {
		out.Add(p)
	}
	for _, i := range inst {
		c := i.Component.Clone()
		c.Remove("RRULE", "RDATE", "EXDATE", "DTSTART", "DTEND", "DURATION", "RECURRENCE-ID")
		c.Add(ical.Prop{Name: "DTSTART", Value: ical.FormatUTC(i.Start)})
		c.Add(ical.Prop{Name: "DTEND", Value: ical.FormatUTC(i.End)})
		if !i.RecurrenceID.IsZero() {
			c.Add(ical.Prop{Name: "RECURRENCE-ID", Value: ical.FormatUTC(i.RecurrenceID)})
		}
		out.Children = append(out.Children, c)
	}
	return string(ical.Encode(out))
}

func zoneFor(c store.Collection) *time.Location {
	if c.TimeZone != "" {
		if loc := ical.LoadZone(c.TimeZone); loc != nil {
			return loc
		}
	}
	return time.UTC
}

// objectProps writes the response for an object with the asked
// properties, calendar-data and address-data included.
func (h *Handler) objectProps(ctx context.Context, wr *writer, res *resource, props []*node) {
	resp := response{href: res.href, found: map[xml.Name]string{}}
	for _, p := range props {
		if v, ok := h.objectData(ctx, res, p.Name, p); ok {
			resp.found[p.Name] = v
			continue
		}
		if v, ok := h.prop(ctx, wr, res, p.Name); ok {
			resp.found[p.Name] = v
		} else {
			resp.missing = append(resp.missing, p.Name)
		}
	}
	wr.response(resp)
}

// multiget answers calendar-multiget (RFC 4791 7.9) and
// addressbook-multiget (RFC 6352 8.7).
func (h *Handler) multiget(w http.ResponseWriter, r *http.Request, res *resource, body *node) {
	if res.kind != kCalendar && res.kind != kAddressBook && res.kind != kInbox {
		h.precondition(w, http.StatusForbidden, nsDAV, "supported-report")
		return
	}
	props := requested(body)
	hrefs := body.all(nsDAV, "href")
	if len(hrefs) > 5000 {
		h.fail(w, http.StatusForbidden, "too many hrefs")
		return
	}
	// Load the collection's objects once, with data.
	objs, err := h.PIM.Store.Objects(r.Context(), res.acc.Owner, store.ObjectFilter{Collections: []int64{res.acc.Collection.ID}, WithData: true})
	if err != nil {
		h.fail(w, http.StatusServiceUnavailable, "")
		return
	}
	byName := map[string]*store.Object{}
	for i := range objs {
		byName[objs[i].Name] = &objs[i]
	}
	wr := &writer{}
	for _, hn := range hrefs {
		p := cleanPath(hn.Text)
		nm := ""
		if strings.HasPrefix(p, res.href) {
			nm, _ = urlUnescape(strings.TrimPrefix(p, res.href))
		}
		o := byName[nm]
		if o == nil {
			wr.response(response{href: strings.TrimSpace(hn.Text), status: http.StatusNotFound})
			continue
		}
		h.objectProps(r.Context(), wr, h.objectResource(res, o), props)
	}
	writeMultistatus(w, wr.multistatus(""))
}

// calendarQuery answers calendar-query (RFC 4791 7.8) with the filter of
// 9.7.
func (h *Handler) calendarQuery(w http.ResponseWriter, r *http.Request, res *resource, body *node) {
	if res.kind != kCalendar && res.kind != kInbox {
		h.precondition(w, http.StatusForbidden, nsDAV, "supported-report")
		return
	}
	filter := body.child(nsCalDAV, "filter")
	root := filter.child(nsCalDAV, "comp-filter")
	if filter == nil || root == nil {
		h.precondition(w, http.StatusForbidden, nsCalDAV, "valid-filter")
		return
	}
	if v, _ := root.attr("name"); !strings.EqualFold(v, "VCALENDAR") {
		h.precondition(w, http.StatusForbidden, nsCalDAV, "valid-filter")
		return
	}
	f := store.ObjectFilter{Collections: []int64{res.acc.Collection.ID}, WithData: true}
	// A time range on the component narrows by the index first.
	if cf := root.child(nsCalDAV, "comp-filter"); cf != nil {
		if tr := cf.child(nsCalDAV, "time-range"); tr != nil {
			from, to, ok := timeRange(tr)
			if !ok {
				h.precondition(w, http.StatusForbidden, nsCalDAV, "valid-filter")
				return
			}
			f.From, f.To = &from, &to
		}
	}
	objs, err := h.PIM.Store.Objects(r.Context(), res.acc.Owner, f)
	if err != nil {
		h.fail(w, http.StatusServiceUnavailable, "")
		return
	}
	props := requested(body)
	wr := &writer{}
	loc := zoneFor(res.acc.Collection)
	if tz := body.child(nsCalDAV, "timezone"); tz != nil {
		if cal, err := ical.ParseOne([]byte(tz.Text), "VCALENDAR"); err == nil {
			for _, vt := range cal.Sub("VTIMEZONE") {
				if l := ical.ZonesOf(cal).Location(vt.Text("TZID")); l != nil {
					loc = l
				}
			}
		}
	}
	for i := range objs {
		cal, err := ical.ParseOne([]byte(objs[i].Data), "VCALENDAR")
		if err != nil {
			continue
		}
		o := jscal.Read(cal, loc)
		if !matchComp(o, []*ical.Component{cal}, root) {
			continue
		}
		h.objectProps(r.Context(), wr, h.objectResource(res, &objs[i]), props)
	}
	writeMultistatus(w, wr.multistatus(""))
}

// timeRange reads the start and end of a time-range (RFC 4791 9.9); a
// missing one is open.
func timeRange(n *node) (time.Time, time.Time, bool) {
	from, to := time.Unix(0, 0).UTC(), time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if s, ok := n.attr("start"); ok {
		t, err := time.Parse("20060102T150405Z", s)
		if err != nil {
			return from, to, false
		}
		from = t
	}
	if s, ok := n.attr("end"); ok {
		t, err := time.Parse("20060102T150405Z", s)
		if err != nil {
			return from, to, false
		}
		to = t
	}
	return from, to, to.After(from)
}

// matchComp reports whether any of the components named by the filter
// satisfies it (RFC 4791 9.7.1).
func matchComp(o *jscal.Object, parents []*ical.Component, f *node) bool {
	nm, _ := f.attr("name")
	nm = strings.ToUpper(nm)
	var cands []*ical.Component
	for _, p := range parents {
		if p.Name == nm && p == o.Cal {
			cands = append(cands, p)
			continue
		}
		for _, c := range p.Children {
			if c.Name == nm {
				cands = append(cands, c)
			}
		}
	}
	if f.child(nsCalDAV, "is-not-defined") != nil {
		return len(cands) == 0
	}
	for _, c := range cands {
		if matchOne(o, c, f) {
			return true
		}
	}
	return false
}

func matchOne(o *jscal.Object, c *ical.Component, f *node) bool {
	if tr := f.child(nsCalDAV, "time-range"); tr != nil {
		from, to, ok := timeRange(tr)
		if !ok {
			return false
		}
		switch c.Name {
		case "VEVENT", "VTODO", "VJOURNAL":
			inst, _ := o.Instances(from, to, 1)
			found := false
			for _, i := range inst {
				if i.Component.Value("UID") == c.Value("UID") {
					found = true
				}
			}
			if !found {
				// A VTODO without dates matches every range (RFC 4791 9.9).
				if !(c.Name == "VTODO" && c.Get("DTSTART") == nil && c.Get("DUE") == nil) {
					return false
				}
			}
		case "VALARM":
			// Alarms are not expanded: a time range on them matches when
			// the event does.
		}
	}
	for _, pf := range f.all(nsCalDAV, "prop-filter") {
		if !matchPropFilter(c, pf, nsCalDAV) {
			return false
		}
	}
	for _, cf := range f.all(nsCalDAV, "comp-filter") {
		if !matchComp(o, []*ical.Component{c}, cf) {
			return false
		}
	}
	return true
}

// textMatch applies a text-match: case-insensitive, by the match-type of
// RFC 6352 10.5.4 ("contains" for CalDAV).
func textMatch(value string, tm *node) bool {
	want := tm.Text
	mt, _ := tm.attr("match-type")
	coll, _ := tm.attr("collation")
	v, x := value, want
	if coll != "i;octet" {
		v, x = strings.ToLower(value), strings.ToLower(want)
	}
	var ok bool
	switch mt {
	case "equals":
		ok = v == x
	case "starts-with":
		ok = strings.HasPrefix(v, x)
	case "ends-with":
		ok = strings.HasSuffix(v, x)
	default:
		ok = strings.Contains(v, x)
	}
	if neg, _ := tm.attr("negate-condition"); neg == "yes" {
		return !ok
	}
	return ok
}

// matchPropFilter applies a prop-filter to a component (RFC 4791 9.7.2,
// RFC 6352 10.5.1).
func matchPropFilter(c *ical.Component, pf *node, ns string) bool {
	nm, _ := pf.attr("name")
	props := c.All(strings.ToUpper(nm))
	if pf.child(ns, "is-not-defined") != nil {
		return len(props) == 0
	}
	if len(props) == 0 {
		return false
	}
	anyof := true
	if t, _ := pf.attr("test"); t == "allof" {
		anyof = false
	}
	tests := 0
	for _, p := range props {
		results := []bool{}
		for _, tm := range pf.all(ns, "text-match") {
			results = append(results, textMatch(p.Text(), tm))
		}
		for _, pa := range pf.all(ns, "param-filter") {
			pn, _ := pa.attr("name")
			vals := p.ParamValues(strings.ToUpper(pn))
			switch {
			case pa.child(ns, "is-not-defined") != nil:
				results = append(results, !p.HasParam(strings.ToUpper(pn)))
			case pa.child(ns, "text-match") != nil:
				m := false
				for _, v := range vals {
					if textMatch(v, pa.child(ns, "text-match")) {
						m = true
					}
				}
				results = append(results, m)
			default:
				results = append(results, p.HasParam(strings.ToUpper(pn)))
			}
		}
		if tr := pf.child(nsCalDAV, "time-range"); tr != nil && ns == nsCalDAV {
			from, to, _ := timeRange(tr)
			d, err := (ical.Zones{}).ParseDateTime(p)
			results = append(results, err == nil && !d.In(time.UTC).Before(from) && d.In(time.UTC).Before(to))
		}
		tests = len(results)
		if tests == 0 {
			return true
		}
		ok := !anyof
		for _, x := range results {
			if anyof && x {
				ok = true
			}
			if !anyof && !x {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// addressbookQuery answers addressbook-query (RFC 6352 8.6).
func (h *Handler) addressbookQuery(w http.ResponseWriter, r *http.Request, res *resource, body *node) {
	if res.kind != kAddressBook {
		h.precondition(w, http.StatusForbidden, nsDAV, "supported-report")
		return
	}
	filter := body.child(nsCardDAV, "filter")
	anyof := true
	if t, _ := filter.attr("test"); filter != nil && t == "allof" {
		anyof = false
	}
	limit := 0
	if l := body.child(nsCardDAV, "limit"); l != nil {
		limit, _ = strconv.Atoi(strings.TrimSpace(l.child(nsCardDAV, "nresults").Text))
	}
	objs, err := h.PIM.Store.Objects(r.Context(), res.acc.Owner, store.ObjectFilter{Collections: []int64{res.acc.Collection.ID}, WithData: true})
	if err != nil {
		h.fail(w, http.StatusServiceUnavailable, "")
		return
	}
	props := requested(body)
	wr := &writer{}
	n := 0
	truncated := false
	for i := range objs {
		v, err := ical.ParseOne([]byte(objs[i].Data), "VCARD")
		if err != nil {
			continue
		}
		pfs := filter.all(nsCardDAV, "prop-filter")
		ok := len(pfs) == 0 || !anyof
		for _, pf := range pfs {
			m := matchPropFilter(v, pf, nsCardDAV)
			if anyof && m {
				ok = true
			}
			if !anyof && !m {
				ok = false
			}
		}
		if !ok {
			continue
		}
		if limit > 0 && n >= limit {
			truncated = true
			break
		}
		n++
		h.objectProps(r.Context(), wr, h.objectResource(res, &objs[i]), props)
	}
	if truncated {
		// RFC 6352 8.6.1: say that the results were cut.
		wr.response(response{href: res.href, status: http.StatusInsufficientStorage,
			extra: "<d:error><d:number-of-matches-within-limits/></d:error>"})
	}
	writeMultistatus(w, wr.multistatus(""))
}

// syncCollection answers sync-collection (RFC 6578 3).
func (h *Handler) syncCollection(w http.ResponseWriter, r *http.Request, res *resource, body *node) {
	if res.kind != kCalendar && res.kind != kAddressBook && res.kind != kInbox {
		h.precondition(w, http.StatusForbidden, nsDAV, "supported-report")
		return
	}
	if lvl := body.child(nsDAV, "sync-level"); lvl != nil && strings.TrimSpace(lvl.Text) != "1" {
		// Depth infinity has nothing more to find: collections hold no
		// collections. Answered as level 1.
	}
	since := int64(0)
	if tok := body.child(nsDAV, "sync-token"); tok != nil && strings.TrimSpace(tok.Text) != "" {
		n, ok := parseSyncToken(tok.Text)
		if !ok {
			h.precondition(w, http.StatusForbidden, nsDAV, "valid-sync-token")
			return
		}
		since = n
	}
	ch, err := h.PIM.Store.CollectionChanges(r.Context(), res.acc.Owner, []int64{res.acc.Collection.ID}, since)
	if err != nil {
		if since != 0 {
			h.precondition(w, http.StatusForbidden, nsDAV, "valid-sync-token")
			return
		}
		h.fail(w, http.StatusServiceUnavailable, "")
		return
	}
	if since != 0 {
		if epoch, _ := h.PIM.Store.ShareEpoch(r.Context(), res.acc.Owner, res.user.ID); res.acc.Owner != res.user.ID && epoch > since {
			h.precondition(w, http.StatusForbidden, nsDAV, "valid-sync-token")
			return
		}
	}
	props := requested(body)
	wr := &writer{}
	for i := range ch.Changed {
		h.objectProps(r.Context(), wr, h.objectResource(res, &ch.Changed[i]), props)
	}
	for _, nm := range ch.Removed {
		wr.response(response{href: res.href + esch(nm), status: http.StatusNotFound})
	}
	writeMultistatus(w, wr.multistatus("<d:sync-token>"+esc(syncToken(ch.State))+"</d:sync-token>"))
}

// freeBusyQuery answers free-busy-query on a calendar (RFC 4791 7.10).
func (h *Handler) freeBusyQuery(w http.ResponseWriter, r *http.Request, res *resource, body *node) {
	if res.kind != kCalendar {
		h.precondition(w, http.StatusForbidden, nsDAV, "supported-report")
		return
	}
	tr := body.child(nsCalDAV, "time-range")
	from, to, ok := timeRange(tr)
	if tr == nil || !ok {
		h.fail(w, http.StatusBadRequest, "")
		return
	}
	owner, err := h.PIM.Store.AccountByID(r.Context(), res.acc.Owner)
	if err != nil {
		h.fail(w, http.StatusServiceUnavailable, "")
		return
	}
	busy, err := h.PIM.BusyPeriods(r.Context(), owner, from, to)
	if err != nil {
		h.fail(w, http.StatusServiceUnavailable, "")
		return
	}
	cal := &ical.Component{Name: "VCALENDAR"}
	cal.Add(ical.Prop{Name: "VERSION", Value: "2.0"})
	cal.SetText("PRODID", "-//robinmail//robinmail//EN")
	fb := &ical.Component{Name: "VFREEBUSY"}
	fb.Add(ical.Prop{Name: "DTSTAMP", Value: ical.FormatUTC(time.Now())})
	fb.Add(ical.Prop{Name: "DTSTART", Value: ical.FormatUTC(from)})
	fb.Add(ical.Prop{Name: "DTEND", Value: ical.FormatUTC(to)})
	for _, b := range busy {
		if b.Object.CollectionID != res.acc.Collection.ID {
			continue
		}
		p := ical.Prop{Name: "FREEBUSY", Value: ical.FormatUTC(b.Start) + "/" + ical.FormatUTC(b.End)}
		if b.Status == "tentative" {
			p.SetParam("FBTYPE", "BUSY-TENTATIVE")
		}
		fb.Add(p)
	}
	cal.Children = append(cal.Children, fb)
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Write(ical.Encode(cal))
}

// expandProperty answers expand-property (RFC 3253 3.8), which clients
// use to read properties of the principals a property points at.
func (h *Handler) expandProperty(w http.ResponseWriter, r *http.Request, res *resource, body *node) {
	wr := &writer{}
	resp := response{href: res.href, found: map[xml.Name]string{}}
	for _, p := range body.all(nsDAV, "property") {
		ns, _ := p.attr("namespace")
		if ns == "" {
			ns = nsDAV
		}
		local, _ := p.attr("name")
		n := xml.Name{Space: ns, Local: local}
		v, ok := h.prop(r.Context(), wr, res, n)
		if !ok {
			resp.missing = append(resp.missing, n)
			continue
		}
		nested := p.all(nsDAV, "property")
		if len(nested) == 0 || !strings.Contains(v, "<d:href>") {
			resp.found[n] = v
			continue
		}
		// Each href in the value becomes a response with the nested
		// properties of what it names.
		var b strings.Builder
		for _, part := range strings.Split(v, "<d:href>")[1:] {
			href := strings.SplitN(part, "</d:href>", 2)[0]
			target, status := h.resolve(r.Context(), res.user, cleanPath(unescXML(href)))
			sub := &writer{extra: wr.extra}
			if status != 0 {
				sub.response(response{href: unescXML(href), status: http.StatusNotFound})
			} else {
				sr := response{href: target.href, found: map[xml.Name]string{}}
				for _, np := range nested {
					nns, _ := np.attr("namespace")
					if nns == "" {
						nns = nsDAV
					}
					nl, _ := np.attr("name")
					nn := xml.Name{Space: nns, Local: nl}
					if val, ok := h.prop(r.Context(), sub, target, nn); ok {
						sr.found[nn] = val
					} else {
						sr.missing = append(sr.missing, nn)
					}
				}
				sub.response(sr)
			}
			b.WriteString(sub.b.String())
		}
		resp.found[n] = b.String()
	}
	wr.response(resp)
	writeMultistatus(w, wr.multistatus(""))
}

func unescXML(s string) string {
	r := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&#34;", `"`, "&#39;", "'")
	return r.Replace(s)
}

// principalSearch answers principal-property-search (RFC 3744 9.4): the
// accounts whose address contains the text, at most 50, for finding
// attendees.
func (h *Handler) principalSearch(w http.ResponseWriter, r *http.Request, res *resource, body *node) {
	text := ""
	for _, ps := range body.all(nsDAV, "property-search") {
		if m := ps.child(nsDAV, "match"); m != nil {
			text = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(m.Text), "mailto:"))
		}
	}
	wr := &writer{}
	if len([]rune(text)) >= 2 {
		list, err := h.PIM.Store.Principals(r.Context(), text, 50)
		if err != nil {
			h.fail(w, http.StatusServiceUnavailable, "")
			return
		}
		props := requested(body)
		for _, p := range list {
			pr := &resource{kind: kPrincipal, user: res.user, href: principalHref(p.ID), principal: p}
			resp := response{href: pr.href, found: map[xml.Name]string{}}
			for _, n := range props {
				if v, ok := h.prop(r.Context(), wr, pr, n.Name); ok {
					resp.found[n.Name] = v
				} else {
					resp.missing = append(resp.missing, n.Name)
				}
			}
			wr.response(resp)
		}
	}
	writeMultistatus(w, wr.multistatus(""))
}

func urlUnescape(s string) (string, error) { return url.PathUnescape(s) }
