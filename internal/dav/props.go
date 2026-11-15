package dav

import (
	"context"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/pim"
	"github.com/robinmail/robinmail/internal/store"
)

// Limits on dead properties of a collection.
const (
	maxDeadProps    = 50
	maxDeadPropSize = 8 << 10
)

// deadKey is how a dead property is kept: "{namespace}local".
func deadKey(n xml.Name) string { return "{" + n.Space + "}" + n.Local }

func parseDeadKey(k string) (xml.Name, bool) {
	if !strings.HasPrefix(k, "{") {
		return xml.Name{}, false
	}
	i := strings.IndexByte(k, '}')
	if i < 0 {
		return xml.Name{}, false
	}
	return xml.Name{Space: k[1:i], Local: k[i+1:]}, true
}

// allprop are the properties PROPFIND allprop returns (RFC 4918 9.1: the
// live properties defined there, and dead ones; RFC 4791 and 6352 leave
// out calendar and address data).
var allprop = []xml.Name{
	name(nsDAV, "resourcetype"), name(nsDAV, "displayname"), name(nsDAV, "getetag"), name(nsDAV, "getcontenttype"),
	name(nsDAV, "getcontentlength"), name(nsDAV, "getlastmodified"), name(nsDAV, "creationdate"),
}

// privileges lists what the user may do (RFC 3744 5.4).
func privileges(res *resource) string {
	priv := func(ns, local string) string {
		p := prefixes[ns]
		return "<d:privilege><" + p + ":" + local + "/></d:privilege>"
	}
	read := priv(nsDAV, "read") + priv(nsDAV, "read-current-user-privilege-set") + priv(nsCalDAV, "read-free-busy")
	switch res.kind {
	case kCalendar, kAddressBook, kObject, kInbox:
		switch res.acc.Rights {
		case pim.RightOwner:
			s := read + priv(nsDAV, "write") + priv(nsDAV, "write-properties") + priv(nsDAV, "write-content") +
				priv(nsDAV, "bind") + priv(nsDAV, "unbind") + priv(nsDAV, "read-acl")
			if res.kind == kInbox {
				s += priv(nsCalDAV, "schedule-deliver")
			}
			return s
		case pim.RightWrite:
			return read + priv(nsDAV, "write-content") + priv(nsDAV, "bind") + priv(nsDAV, "unbind")
		}
		return read
	case kOutbox:
		return read + priv(nsCalDAV, "schedule-send") + priv(nsCalDAV, "schedule-query-freebusy")
	case kCalHome, kCardHome:
		return read + priv(nsDAV, "bind") + priv(nsDAV, "unbind") + priv(nsDAV, "write-properties")
	}
	return read
}

func reportSet(reports ...[2]string) string {
	var b strings.Builder
	for _, r := range reports {
		b.WriteString("<d:supported-report><d:report><" + prefixes[r[0]] + ":" + r[1] + "/></d:report></d:supported-report>")
	}
	return b.String()
}

// syncToken is the URI of a sync token (RFC 6578 6.2): an account's modseq.
func syncToken(state int64) string {
	return "http://robinmail.invalid/sync/" + strconv.FormatInt(state, 10)
}

func parseSyncToken(s string) (int64, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), "http://robinmail.invalid/sync/")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return n, err == nil && n >= 0
}

// prop computes one property of a resource. ok is false for a property
// the resource does not have.
func (h *Handler) prop(ctx context.Context, w *writer, res *resource, n xml.Name) (string, bool) {
	user := res.user
	href := w.href
	switch n.Space + " " + n.Local {
	case nsDAV + " current-user-principal":
		return href(principalHref(user.ID)), true
	case nsDAV + " principal-collection-set":
		return href(Prefix + "principals/"), true
	case nsDAV + " current-user-privilege-set":
		return privileges(res), true
	case nsDAV + " supported-report-set":
		switch res.kind {
		case kCalendar, kInbox:
			return reportSet([2]string{nsCalDAV, "calendar-query"}, [2]string{nsCalDAV, "calendar-multiget"},
				[2]string{nsCalDAV, "free-busy-query"}, [2]string{nsDAV, "sync-collection"}, [2]string{nsDAV, "expand-property"}), true
		case kAddressBook:
			return reportSet([2]string{nsCardDAV, "addressbook-query"}, [2]string{nsCardDAV, "addressbook-multiget"},
				[2]string{nsDAV, "sync-collection"}, [2]string{nsDAV, "expand-property"}), true
		default:
			return reportSet([2]string{nsDAV, "expand-property"}, [2]string{nsDAV, "principal-property-search"},
				[2]string{nsDAV, "principal-search-property-set"}), true
		}
	case nsDAV + " owner":
		switch res.kind {
		case kCalendar, kAddressBook, kInbox, kObject:
			return href(principalHref(res.acc.Owner)), true
		case kCalHome, kCardHome, kOutbox, kPrincipal:
			return href(principalHref(user.ID)), true
		}
		return "", false
	case nsDAV + " resourcetype":
		switch res.kind {
		case kRoot, kPrincipals, kCalHome, kCardHome:
			return "<d:collection/>", true
		case kPrincipal:
			return "<d:collection/><d:principal/>", true
		case kCalendar:
			s := "<d:collection/><c:calendar/>"
			if res.acc.Rights != pim.RightOwner {
				s += "<cs:shared/>"
			}
			return s, true
		case kAddressBook:
			s := "<d:collection/><card:addressbook/>"
			if res.acc.Rights != pim.RightOwner {
				s += "<cs:shared/>"
			}
			return s, true
		case kInbox:
			return "<d:collection/><c:schedule-inbox/>", true
		case kOutbox:
			return "<d:collection/><c:schedule-outbox/>", true
		}
		return "", true
	case nsDAV + " displayname":
		switch res.kind {
		case kPrincipal:
			return esc(res.principal.Address), true
		case kCalendar, kAddressBook:
			name := res.acc.Collection.Name
			if res.acc.Rights != pim.RightOwner {
				name += " (" + res.acc.OwnerAddress + ")"
			}
			return esc(name), true
		case kInbox:
			return "Inbox", true
		case kOutbox:
			return "Outbox", true
		case kCalHome:
			return "Calendars", true
		case kCardHome:
			return "Address books", true
		}
		return "", false
	case nsDAV + " getetag":
		if res.kind == kObject && res.obj != nil {
			return esc(res.obj.ETag), true
		}
		if res.kind == kCalendar || res.kind == kAddressBook || res.kind == kInbox {
			return esc(`"` + strconv.FormatInt(res.acc.Collection.ContentModseq, 10) + "-" + strconv.FormatInt(res.acc.Collection.Modseq, 10) + `"`), true
		}
		return "", false
	case nsCS + " getctag":
		if res.kind == kCalendar || res.kind == kAddressBook || res.kind == kInbox {
			return strconv.FormatInt(max(res.acc.Collection.ContentModseq, res.acc.Collection.Modseq), 10), true
		}
		return "", false
	case nsDAV + " sync-token":
		if res.kind == kCalendar || res.kind == kAddressBook || res.kind == kInbox {
			state, err := h.PIM.Store.State(ctx, res.acc.Owner)
			if err != nil {
				return "", false
			}
			return esc(syncToken(state)), true
		}
		return "", false
	case nsDAV + " getcontenttype":
		if res.kind == kObject && res.obj != nil {
			if res.obj.Kind == store.ObjectCard {
				return "text/vcard; charset=utf-8", true
			}
			comp := res.obj.Component
			if comp == "" {
				comp = "VEVENT"
			}
			return "text/calendar; charset=utf-8; component=" + strings.ToLower(comp), true
		}
		return "", false
	case nsDAV + " getcontentlength":
		if res.kind == kObject && res.obj != nil {
			return strconv.FormatInt(res.obj.Size, 10), true
		}
		return "", false
	case nsDAV + " creationdate":
		if res.kind == kObject && res.obj != nil {
			return res.obj.CreatedAt.UTC().Format(time.RFC3339), true
		}
		return "", false
	case nsDAV + " getlastmodified":
		if res.kind == kObject && res.obj != nil {
			return res.obj.CreatedAt.UTC().Format(http.TimeFormat), true
		}
		return "", false
	case nsDAV + " quota-used-bytes", nsDAV + " quota-available-bytes":
		switch res.kind {
		case kCalHome, kCardHome, kCalendar, kAddressBook:
			owner := user.ID
			if res.kind == kCalendar || res.kind == kAddressBook {
				owner = res.acc.Owner
			}
			q, err := h.PIM.Store.Quota(ctx, owner)
			if err != nil {
				return "", false
			}
			if n.Local == "quota-used-bytes" {
				return strconv.FormatInt(q.UsedBytes, 10), true
			}
			if q.Bytes == 0 {
				return "", false // RFC 4331 3: no limit, no property
			}
			return strconv.FormatInt(max(q.Bytes-q.UsedBytes, 0), 10), true
		}
		return "", false
	case nsDAV + " principal-URL":
		if res.kind == kPrincipal {
			return href(principalHref(res.principal.ID)), true
		}
		return "", false
	case nsDAV + " alternate-URI-set":
		if res.kind == kPrincipal {
			return href("mailto:" + res.principal.Address), true
		}
		return "", false
	case nsDAV + " group-membership", nsDAV + " group-member-set":
		if res.kind == kPrincipal {
			return "", true
		}
		return "", false
	case nsCalDAV + " calendar-home-set":
		if res.kind == kPrincipal && res.principal.ID == user.ID {
			return href(Prefix + "calendars/" + user.ID + "/"), true
		}
		return "", false
	case nsCardDAV + " addressbook-home-set":
		if res.kind == kPrincipal && res.principal.ID == user.ID {
			return href(Prefix + "addressbooks/" + user.ID + "/"), true
		}
		return "", false
	case nsCalDAV + " calendar-user-address-set":
		if res.kind == kPrincipal {
			var b strings.Builder
			addr := res.principal.Address
			for _, a := range h.PIM.Addresses(ctx, addr) {
				b.WriteString(href(a))
			}
			b.WriteString(href(principalHref(res.principal.ID)))
			return b.String(), true
		}
		return "", false
	case nsCS + " email-address-set":
		if res.kind == kPrincipal {
			var b strings.Builder
			for _, a := range h.PIM.Addresses(ctx, res.principal.Address) {
				b.WriteString("<cs:email-address>" + esc(strings.TrimPrefix(a, "mailto:")) + "</cs:email-address>")
			}
			return b.String(), true
		}
		return "", false
	case nsCalDAV + " calendar-user-type":
		if res.kind == kPrincipal {
			return "INDIVIDUAL", true
		}
		return "", false
	case nsCalDAV + " schedule-inbox-URL":
		if res.kind == kPrincipal && res.principal.ID == user.ID {
			return href(Prefix + "calendars/" + user.ID + "/inbox/"), true
		}
		return "", false
	case nsCalDAV + " schedule-outbox-URL":
		if res.kind == kPrincipal && res.principal.ID == user.ID {
			return href(Prefix + "calendars/" + user.ID + "/outbox/"), true
		}
		return "", false
	case nsCalDAV + " schedule-default-calendar-URL":
		if res.kind == kInbox || (res.kind == kPrincipal && res.principal.ID == user.ID) {
			list, err := h.PIM.Collections(ctx, user, store.KindCalendar)
			if err != nil {
				return "", false
			}
			for _, a := range list {
				if a.Owner == user.ID && a.Collection.IsDefault {
					return href(collectionHref(user, a)), true
				}
			}
		}
		return "", false
	case nsCardDAV + " principal-address":
		return "", false
	case nsCalDAV + " calendar-description":
		if res.kind == kCalendar {
			return esc(res.acc.Collection.Description), true
		}
		return "", false
	case nsCardDAV + " addressbook-description":
		if res.kind == kAddressBook {
			return esc(res.acc.Collection.Description), true
		}
		return "", false
	case nsApple + " calendar-color":
		if res.kind == kCalendar && res.acc.Collection.Color != "" {
			return esc(res.acc.Collection.Color), true
		}
		return "", false
	case nsApple + " calendar-order":
		if res.kind == kCalendar {
			return strconv.FormatInt(res.acc.Collection.SortOrder, 10), true
		}
		return "", false
	case nsCalDAV + " supported-calendar-component-set":
		if res.kind == kCalendar || res.kind == kInbox {
			comps := res.acc.Collection.Components
			if comps == "" {
				comps = "VEVENT VTODO"
			}
			var b strings.Builder
			for _, c := range strings.Fields(comps) {
				b.WriteString(`<c:comp name="` + esc(c) + `"/>`)
			}
			return b.String(), true
		}
		return "", false
	case nsCalDAV + " supported-calendar-data":
		if res.kind == kCalendar {
			return `<c:calendar-data content-type="text/calendar" version="2.0"/>`, true
		}
		return "", false
	case nsCardDAV + " supported-address-data":
		if res.kind == kAddressBook {
			return `<card:address-data-type content-type="text/vcard" version="3.0"/><card:address-data-type content-type="text/vcard" version="4.0"/>`, true
		}
		return "", false
	case nsCalDAV + " max-resource-size":
		if res.kind == kCalendar {
			return strconv.Itoa(store.MaxObjectSize), true
		}
		return "", false
	case nsCardDAV + " max-resource-size":
		if res.kind == kAddressBook {
			return strconv.Itoa(store.MaxObjectSize), true
		}
		return "", false
	case nsCalDAV + " calendar-timezone":
		if res.kind == kCalendar && res.acc.Collection.TimeZone != "" {
			if loc := ical.LoadZone(res.acc.Collection.TimeZone); loc != nil {
				cal := &ical.Component{Name: "VCALENDAR"}
				cal.Add(ical.Prop{Name: "VERSION", Value: "2.0"})
				cal.SetText("PRODID", "-//robinmail//robinmail//EN")
				y := time.Now().Year()
				cal.Children = append(cal.Children, ical.VTimezone(loc, y, y+1))
				return esc(string(ical.Encode(cal))), true
			}
		}
		return "", false
	case nsCalDAV + " schedule-calendar-transp":
		if res.kind == kCalendar {
			if res.acc.Collection.IncludeInAvailability == "none" {
				return "<c:transparent/>", true
			}
			return "<c:opaque/>", true
		}
		return "", false
	case nsCalDAV + " schedule-tag":
		if res.kind == kObject && res.obj != nil && res.obj.ScheduleTag != "" {
			return esc(res.obj.ScheduleTag), true
		}
		return "", false
	case nsCS + " invite":
		return "", false
	}
	// Dead properties of a collection.
	if res.kind == kCalendar || res.kind == kAddressBook {
		if v, ok := res.acc.Collection.Props[deadKey(n)]; ok {
			return v, true
		}
	}
	return "", false
}

// propfind answers PROPFIND (RFC 4918 9.1).
func (h *Handler) propfind(w http.ResponseWriter, r *http.Request, res *resource) {
	d := depth(r, -1)
	if d < 0 {
		// RFC 4918 9.1: a server may refuse Depth: infinity.
		h.precondition(w, http.StatusForbidden, nsDAV, "propfind-finite-depth")
		return
	}
	body, err := parseXML(r.Body)
	if err != nil || (body != nil && (body.Name != name(nsDAV, "propfind"))) {
		h.fail(w, http.StatusBadRequest, "")
		return
	}
	var names []xml.Name
	mode := "allprop"
	if body != nil {
		switch {
		case body.child(nsDAV, "prop") != nil:
			mode = "prop"
			for _, p := range body.child(nsDAV, "prop").Children {
				names = append(names, p.Name)
			}
		case body.child(nsDAV, "propname") != nil:
			mode = "propname"
		case body.child(nsDAV, "allprop") != nil:
			if inc := body.child(nsDAV, "include"); inc != nil {
				for _, p := range inc.Children {
					names = append(names, p.Name)
				}
			}
		}
	}
	if res.kind == kObject && res.obj == nil {
		h.fail(w, http.StatusNotFound, "")
		return
	}
	list := []*resource{res}
	if d == 1 && res.isCollection() {
		kids, err := h.children(r.Context(), res)
		if err != nil {
			h.fail(w, http.StatusServiceUnavailable, "")
			return
		}
		list = append(list, kids...)
	}
	wr := &writer{}
	for _, x := range list {
		h.respondProps(r.Context(), wr, x, mode, names)
	}
	writeMultistatus(w, wr.multistatus(""))
}

// knownProps are the live properties propname lists.
var knownProps = []xml.Name{
	name(nsDAV, "resourcetype"), name(nsDAV, "displayname"), name(nsDAV, "current-user-principal"), name(nsDAV, "owner"),
	name(nsDAV, "current-user-privilege-set"), name(nsDAV, "supported-report-set"), name(nsDAV, "getetag"),
	name(nsDAV, "sync-token"), name(nsCS, "getctag"), name(nsDAV, "getcontenttype"), name(nsDAV, "getcontentlength"),
	name(nsCalDAV, "calendar-home-set"), name(nsCardDAV, "addressbook-home-set"), name(nsCalDAV, "calendar-user-address-set"),
	name(nsCalDAV, "schedule-inbox-URL"), name(nsCalDAV, "schedule-outbox-URL"), name(nsCalDAV, "calendar-description"),
	name(nsCardDAV, "addressbook-description"), name(nsApple, "calendar-color"), name(nsCalDAV, "supported-calendar-component-set"),
}

func (h *Handler) respondProps(ctx context.Context, wr *writer, res *resource, mode string, names []xml.Name) {
	resp := response{href: res.href, found: map[xml.Name]string{}}
	switch mode {
	case "propname":
		for _, n := range knownProps {
			if _, ok := h.prop(ctx, wr, res, n); ok {
				resp.found[n] = ""
			}
		}
	case "allprop":
		for _, n := range append(allprop, names...) {
			if v, ok := h.prop(ctx, wr, res, n); ok {
				resp.found[n] = v
			}
		}
		if res.kind == kCalendar || res.kind == kAddressBook {
			for k, v := range res.acc.Collection.Props {
				if n, ok := parseDeadKey(k); ok {
					resp.found[n] = v
				}
			}
		}
	default:
		for _, n := range names {
			if v, ok := h.objectData(ctx, res, n, nil); ok {
				resp.found[n] = v
				continue
			}
			if v, ok := h.prop(ctx, wr, res, n); ok {
				resp.found[n] = v
			} else {
				resp.missing = append(resp.missing, n)
			}
		}
	}
	wr.response(resp)
}

func writeMultistatus(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	w.Write(body)
}

// protected are the properties a client cannot set.
var protected = map[xml.Name]bool{
	name(nsDAV, "resourcetype"): true, name(nsDAV, "getetag"): true, name(nsDAV, "sync-token"): true,
	name(nsCS, "getctag"): true, name(nsDAV, "current-user-privilege-set"): true, name(nsDAV, "owner"): true,
	name(nsCalDAV, "supported-calendar-data"): true, name(nsCalDAV, "max-resource-size"): true,
	name(nsDAV, "getcontentlength"): true, name(nsDAV, "getcontenttype"): true, name(nsDAV, "supported-report-set"): true,
}

// applyProps turns properties of a PROPPATCH set or an extended MKCOL into
// a collection update, and returns those it cannot set.
func (h *Handler) applyProps(u *store.CollectionUpdate, props *node) []xml.Name {
	var bad []xml.Name
	if props == nil {
		return nil
	}
	for _, p := range props.Children {
		text := strings.TrimSpace(p.Text)
		switch p.Name {
		case name(nsDAV, "displayname"):
			if text != "" {
				u.Name = &text
			}
		case name(nsCalDAV, "calendar-description"), name(nsCardDAV, "addressbook-description"):
			t := p.Text
			u.Description = &t
		case name(nsApple, "calendar-color"):
			u.Color = &text
		case name(nsApple, "calendar-order"):
			if n, err := strconv.ParseInt(text, 10, 64); err == nil {
				u.SortOrder = &n
			} else {
				bad = append(bad, p.Name)
			}
		case name(nsCalDAV, "calendar-timezone"):
			tz := ""
			if cal, err := ical.ParseOne([]byte(p.Text), "VCALENDAR"); err == nil {
				for _, vt := range cal.Sub("VTIMEZONE") {
					if loc := ical.ZonesOf(cal).Location(vt.Text("TZID")); loc != nil {
						tz = loc.String()
					}
				}
			}
			if ical.LoadZone(tz) == nil {
				bad = append(bad, p.Name)
				continue
			}
			u.TimeZone = &tz
		case name(nsCalDAV, "supported-calendar-component-set"):
			var comps []string
			for _, c := range p.all(nsCalDAV, "comp") {
				if v, ok := c.attr("name"); ok {
					switch strings.ToUpper(v) {
					case "VEVENT", "VTODO", "VJOURNAL":
						comps = append(comps, strings.ToUpper(v))
					}
				}
			}
			if len(comps) == 0 {
				bad = append(bad, p.Name)
				continue
			}
			s := strings.Join(comps, " ")
			u.Components = &s
		case name(nsCalDAV, "schedule-calendar-transp"):
			v := "all"
			if p.child(nsCalDAV, "transparent") != nil {
				v = "none"
			}
			u.IncludeInAvailability = &v
		case name(nsDAV, "resourcetype"):
			// Allowed in an extended MKCOL, where it was read already.
		default:
			if protected[p.Name] || p.Name.Space == nsDAV {
				bad = append(bad, p.Name)
				continue
			}
			v := inner(p)
			if len(v) > maxDeadPropSize {
				bad = append(bad, p.Name)
				continue
			}
			if u.Props == nil {
				u.Props = map[string]string{}
			}
			if len(u.Props) >= maxDeadProps {
				bad = append(bad, p.Name)
				continue
			}
			u.Props[deadKey(p.Name)] = v
		}
	}
	return bad
}

// proppatch sets and removes properties of a collection (RFC 4918 9.2),
// all or none.
func (h *Handler) proppatch(w http.ResponseWriter, r *http.Request, res *resource) {
	body, err := parseXML(r.Body)
	if err != nil || body == nil || body.Name != name(nsDAV, "propertyupdate") {
		h.fail(w, http.StatusBadRequest, "")
		return
	}
	var all []xml.Name
	u := store.CollectionUpdate{}
	var bad []xml.Name
	for _, op := range body.Children {
		props := op.child(nsDAV, "prop")
		if props == nil {
			continue
		}
		for _, p := range props.Children {
			all = append(all, p.Name)
		}
		switch op.Name {
		case name(nsDAV, "set"):
			bad = append(bad, h.applyProps(&u, props)...)
		case name(nsDAV, "remove"):
			for _, p := range props.Children {
				switch p.Name {
				case name(nsCalDAV, "calendar-description"), name(nsCardDAV, "addressbook-description"):
					e := ""
					u.Description = &e
				case name(nsApple, "calendar-color"):
					e := ""
					u.Color = &e
				default:
					if protected[p.Name] || p.Name.Space == nsDAV {
						bad = append(bad, p.Name)
						continue
					}
					if u.Props == nil {
						u.Props = map[string]string{}
					}
					u.Props[deadKey(p.Name)] = ""
				}
			}
		}
	}
	allowed := (res.kind == kCalendar || res.kind == kAddressBook) && res.acc.Rights == pim.RightOwner
	resp := response{href: res.href}
	switch {
	case !allowed:
		resp.denied = all
	case len(bad) > 0:
		// RFC 4918 9.2.1: the others fail with 424.
		isBad := map[xml.Name]bool{}
		for _, b := range bad {
			isBad[b] = true
		}
		resp.denied = bad
		wr := &writer{}
		var dep []string
		for _, n := range all {
			if !isBad[n] {
				dep = append(dep, wr.el(n, ""))
			}
		}
		if len(dep) > 0 {
			resp.extra = "<d:propstat><d:prop>" + strings.Join(dep, "") + "</d:prop><d:status>" + statusLine(424) + "</d:status></d:propstat>"
		}
		wr.response(resp)
		writeMultistatus(w, wr.multistatus(""))
		return
	default:
		if _, err := h.PIM.Store.UpdateCollection(r.Context(), res.acc.Owner, res.acc.Collection.ID, u, 0); err != nil {
			h.storeError(w, err, res.kind == kCalendar)
			return
		}
		resp.found = map[xml.Name]string{}
		for _, n := range all {
			resp.found[n] = ""
		}
	}
	wr := &writer{}
	wr.response(resp)
	writeMultistatus(w, wr.multistatus(""))
}
