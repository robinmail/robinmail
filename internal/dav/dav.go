// Package dav serves calendars and address books over WebDAV (RFC 4918):
// CalDAV (RFC 4791) with scheduling (RFC 6638), CardDAV (RFC 6352),
// collection synchronization (RFC 6578), the parts of WebDAV ACL (RFC
// 3744) that clients read, extended MKCOL (RFC 5689), quota properties
// (RFC 4331) and the discovery of RFC 6764.
//
// The layout under /dav/:
//
//	principals/<account>/                 the account's principal
//	calendars/<account>/                  its calendar home
//	calendars/<account>/<calendar>/       a calendar: c<n>, or <owner>-c<n> for one shared with it
//	calendars/<account>/inbox/            the scheduling inbox (RFC 6638 2.2)
//	calendars/<account>/outbox/           the scheduling outbox (RFC 6638 2.1), for free/busy
//	addressbooks/<account>/               its address book home
//	addressbooks/<account>/<book>/        an address book: b<n>, or <owner>-b<n>
//
// A user reaches their own homes only; what others share with them shows
// up in those homes. The request context carries the user (WithUser).
package dav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/pim"
	"github.com/robinmail/robinmail/internal/store"
)

// Prefix is where the DAV tree is served.
const Prefix = "/dav/"

type userKey struct{}

// WithUser marks a request context as authenticated.
func WithUser(ctx context.Context, a store.Account) context.Context {
	return context.WithValue(ctx, userKey{}, a)
}

func userFrom(ctx context.Context) (store.Account, bool) {
	a, ok := ctx.Value(userKey{}).(store.Account)
	return a, ok
}

// Handler serves /dav/ and the well-known URIs.
type Handler struct {
	PIM    *pim.Service
	Logger *slog.Logger
}

func (h *Handler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// Resource kinds.
type kind int

const (
	kRoot kind = iota
	kPrincipals
	kPrincipal
	kCalHome
	kCardHome
	kCalendar
	kAddressBook
	kInbox
	kOutbox
	kObject
)

// resource is what a request path names.
type resource struct {
	kind kind
	href string
	user store.Account
	// principal is the account a principal URL names (another account's
	// for kPrincipal of someone else).
	principal store.Principal
	// acc is the collection of a collection or an object.
	acc pim.Access
	// home is "calendars" or "addressbooks" for a collection or object.
	home string
	// seg is the collection's segment in the home.
	seg string
	// name and obj are an object's name and, when it exists, the object.
	name string
	obj  *store.Object
}

func (r *resource) isCollection() bool { return r.kind != kObject }

// ServeHTTP answers a DAV request of an authenticated user.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// RFC 6764 5: the well-known URIs lead to the root, where
	// current-user-principal is found.
	if strings.HasPrefix(r.URL.Path, "/.well-known/caldav") || strings.HasPrefix(r.URL.Path, "/.well-known/carddav") {
		if r.Method == "PROPFIND" {
			// Some clients send PROPFIND to the well-known URI and do not
			// follow a redirect for it; answer as the root does.
			r.URL.Path = Prefix
		} else {
			http.Redirect(w, r, Prefix, http.StatusMovedPermanently)
			return
		}
	}
	if r.Method == http.MethodOptions {
		h.options(w, r)
		return
	}
	switch r.Method {
	case "MKCALENDAR":
		h.mkcolAt(w, r, store.KindCalendar)
		return
	case "MKCOL":
		h.mkcolAt(w, r, "")
		return
	}
	res, status := h.resolve(r.Context(), user, r.URL.Path)
	if status != 0 {
		h.fail(w, status, "")
		return
	}
	switch r.Method {
	case "PROPFIND":
		h.propfind(w, r, res)
	case "PROPPATCH":
		h.proppatch(w, r, res)
	case "REPORT":
		h.report(w, r, res)
	case http.MethodGet, http.MethodHead:
		h.get(w, r, res)
	case http.MethodPut:
		h.put(w, r, res)
	case http.MethodDelete:
		h.delete(w, r, res)
	case "MOVE":
		h.move(w, r, res)
	case http.MethodPost:
		h.post(w, r, res)
	case "COPY", "LOCK", "UNLOCK", "ACL":
		w.Header().Set("Allow", allow(res))
		h.fail(w, http.StatusForbidden, "")
	default:
		w.Header().Set("Allow", allow(res))
		h.fail(w, http.StatusMethodNotAllowed, "")
	}
}

func allow(res *resource) string {
	switch res.kind {
	case kObject:
		return "OPTIONS, GET, HEAD, PUT, DELETE, MOVE, PROPFIND, PROPPATCH, REPORT"
	case kCalendar, kAddressBook:
		return "OPTIONS, GET, HEAD, DELETE, PROPFIND, PROPPATCH, REPORT"
	case kOutbox:
		return "OPTIONS, POST, PROPFIND"
	case kCalHome, kCardHome:
		return "OPTIONS, PROPFIND, PROPPATCH, MKCALENDAR, MKCOL, REPORT"
	}
	return "OPTIONS, PROPFIND, REPORT"
}

func (h *Handler) options(w http.ResponseWriter, r *http.Request) {
	// RFC 4791 5.1, RFC 6352 6.1, RFC 6638 2, RFC 5689 3, RFC 3744 7.2.
	w.Header().Set("DAV", "1, 3, access-control, calendar-access, calendar-schedule, calendar-auto-schedule, addressbook, extended-mkcol")
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, MOVE, PROPFIND, PROPPATCH, REPORT, MKCALENDAR, MKCOL, POST")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string) {
	if msg == "" {
		msg = http.StatusText(status)
	}
	http.Error(w, msg, status)
}

// precondition answers with a DAV error element (RFC 4918 16).
func (h *Handler) precondition(w http.ResponseWriter, status int, ns, cond string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)
	w.Write(xmlError(ns, cond))
}

// storeError answers for an error of a write.
func (h *Handler) storeError(w http.ResponseWriter, err error, calendar bool) {
	ns := nsCalDAV
	if !calendar {
		ns = nsCardDAV
	}
	var ie *pim.InvalidError
	switch {
	case errors.As(err, &ie):
		h.precondition(w, http.StatusForbidden, ns, ie.Cond)
	case errors.Is(err, store.ErrPrecondition):
		h.fail(w, http.StatusPreconditionFailed, "")
	case errors.Is(err, store.ErrUIDConflict):
		cond := "no-uid-conflict"
		h.precondition(w, http.StatusConflict, ns, cond)
	case errors.Is(err, store.ErrOverQuota):
		h.precondition(w, http.StatusInsufficientStorage, nsDAV, "quota-not-exceeded")
	case errors.Is(err, store.ErrObjectTooLarge):
		h.precondition(w, http.StatusForbidden, ns, "max-resource-size")
	case errors.Is(err, pim.ErrForbidden):
		h.precondition(w, http.StatusForbidden, nsDAV, "need-privileges")
	case errors.Is(err, store.ErrNoObject), errors.Is(err, store.ErrNoCollection), errors.Is(err, pim.ErrNotFound):
		h.fail(w, http.StatusNotFound, "")
	case errors.Is(err, store.ErrCollectionLimit):
		h.fail(w, http.StatusForbidden, "too many collections")
	case errors.Is(err, store.ErrCollectionName):
		h.fail(w, http.StatusForbidden, "not a valid name")
	case errors.Is(err, store.ErrDefaultCollection):
		h.fail(w, http.StatusForbidden, "the default collection cannot be deleted")
	default:
		h.log().Error("dav: store error", "err", err)
		h.fail(w, http.StatusServiceUnavailable, "")
	}
}

func esch(s string) string { return url.PathEscape(s) }

// collectionSegment is the segment of a collection in a home.
func collectionSegment(user store.Account, a pim.Access) string {
	var id string
	switch a.Collection.Kind {
	case store.KindAddressBook:
		id = store.AddressBookID(a.Collection.ID)
	case store.KindInbox:
		return "inbox"
	default:
		id = store.CalendarID(a.Collection.ID)
	}
	if a.Owner != user.ID {
		return a.Owner + "-" + id
	}
	if seg := a.Collection.Props[segmentProp]; seg != "" {
		return seg
	}
	return id
}

func homeOf(kind string) string {
	if kind == store.KindAddressBook {
		return "addressbooks"
	}
	return "calendars"
}

func collectionHref(user store.Account, a pim.Access) string {
	return Prefix + homeOf(a.Collection.Kind) + "/" + user.ID + "/" + collectionSegment(user, a) + "/"
}

func principalHref(id string) string { return Prefix + "principals/" + id + "/" }

// resolve finds what a path names. A missing object is a resource with obj
// nil (PUT makes it); anything else missing is a status.
func (h *Handler) resolve(ctx context.Context, user store.Account, p string) (*resource, int) {
	if !strings.HasPrefix(p, Prefix) && p+"/" != Prefix {
		return nil, http.StatusNotFound
	}
	rest := strings.TrimPrefix(p, Prefix)
	trailing := strings.HasSuffix(rest, "/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		parts = nil
	}
	for _, x := range parts {
		if x == "" || x == "." || x == ".." {
			return nil, http.StatusNotFound
		}
	}
	res := &resource{user: user}
	switch {
	case len(parts) == 0:
		res.kind, res.href = kRoot, Prefix
		return res, 0
	case parts[0] == "principals":
		if len(parts) == 1 {
			res.kind, res.href = kPrincipals, Prefix+"principals/"
			return res, 0
		}
		if len(parts) > 2 {
			return nil, http.StatusNotFound
		}
		if parts[1] == user.ID {
			res.principal = store.Principal{ID: user.ID, Address: user.Address}
		} else {
			pr, err := h.PIM.Store.PrincipalByID(ctx, parts[1])
			if err != nil {
				return nil, http.StatusNotFound
			}
			res.principal = pr
		}
		res.kind, res.href = kPrincipal, principalHref(parts[1])
		return res, 0
	case parts[0] == "calendars" || parts[0] == "addressbooks":
	default:
		return nil, http.StatusNotFound
	}
	if len(parts) < 2 {
		return nil, http.StatusNotFound
	}
	if parts[1] != user.ID {
		// Nobody reaches another's home: what is shared is in one's own.
		return nil, http.StatusForbidden
	}
	res.home = parts[0]
	cal := parts[0] == "calendars"
	if len(parts) == 2 {
		res.kind = kCardHome
		if cal {
			res.kind = kCalHome
		}
		res.href = Prefix + parts[0] + "/" + user.ID + "/"
		return res, 0
	}
	seg := parts[2]
	res.seg = seg
	if cal && seg == "outbox" {
		if len(parts) > 3 {
			return nil, http.StatusNotFound
		}
		res.kind, res.href = kOutbox, Prefix+"calendars/"+user.ID+"/outbox/"
		return res, 0
	}
	kindName := store.KindAddressBook
	if cal {
		kindName = store.KindCalendar
	}
	if cal && seg == "inbox" {
		kindName = store.KindInbox
	}
	list, err := h.PIM.Collections(ctx, user, kindName)
	if err != nil {
		h.log().Error("dav: listing collections", "err", err)
		return nil, http.StatusServiceUnavailable
	}
	found := false
	for _, a := range list {
		if collectionSegment(user, a) == seg || (a.Owner == user.ID && (seg == store.CalendarID(a.Collection.ID) || seg == store.AddressBookID(a.Collection.ID)) && a.Collection.Kind == kindName) {
			res.acc, found = a, true
		}
	}
	if !found {
		return nil, http.StatusNotFound
	}
	switch kindName {
	case store.KindInbox:
		res.kind = kInbox
	case store.KindCalendar:
		res.kind = kCalendar
	default:
		res.kind = kAddressBook
	}
	res.href = collectionHref(user, res.acc)
	if len(parts) == 3 {
		return res, 0
	}
	if len(parts) > 4 || trailing {
		return nil, http.StatusNotFound
	}
	nm, err := url.PathUnescape(parts[3])
	if err != nil || nm == "" || len(nm) > 255 || strings.ContainsAny(nm, "/\x00") {
		return nil, http.StatusNotFound
	}
	res.kind, res.name = kObject, nm
	res.href = collectionHref(user, res.acc) + esch(nm)
	o, err := h.PIM.Store.ObjectByName(ctx, res.acc.Owner, res.acc.Collection.ID, nm)
	switch {
	case err == nil:
		res.obj = &o
	case errors.Is(err, store.ErrNoObject):
	default:
		return nil, http.StatusServiceUnavailable
	}
	return res, 0
}

// depth reads the Depth header: 0, 1, or infinity (-1). Missing is
// infinity for PROPFIND (RFC 4918 9.1) and 0 for REPORT (RFC 3253 3.6).
func depth(r *http.Request, def int) int {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Depth"))) {
	case "0":
		return 0
	case "1":
		return 1
	case "infinity":
		return -1
	}
	return def
}

// children lists the members of a collection resource.
func (h *Handler) children(ctx context.Context, res *resource) ([]*resource, error) {
	var out []*resource
	switch res.kind {
	case kRoot:
		p := &resource{kind: kPrincipal, user: res.user, href: principalHref(res.user.ID), principal: store.Principal{ID: res.user.ID, Address: res.user.Address}}
		out = append(out, p)
	case kPrincipals:
		out = append(out, &resource{kind: kPrincipal, user: res.user, href: principalHref(res.user.ID), principal: store.Principal{ID: res.user.ID, Address: res.user.Address}})
	case kCalHome, kCardHome:
		kinds := []string{store.KindAddressBook}
		if res.kind == kCalHome {
			kinds = []string{store.KindCalendar, store.KindInbox}
		}
		for _, k := range kinds {
			list, err := h.PIM.Collections(ctx, res.user, k)
			if err != nil {
				return nil, err
			}
			for _, a := range list {
				c := &resource{user: res.user, acc: a, home: res.home, seg: collectionSegment(res.user, a), href: collectionHref(res.user, a)}
				switch a.Collection.Kind {
				case store.KindCalendar:
					c.kind = kCalendar
				case store.KindInbox:
					c.kind = kInbox
				default:
					c.kind = kAddressBook
				}
				out = append(out, c)
			}
		}
		if res.kind == kCalHome {
			out = append(out, &resource{kind: kOutbox, user: res.user, home: res.home, href: Prefix + "calendars/" + res.user.ID + "/outbox/"})
		}
	case kCalendar, kAddressBook, kInbox:
		objs, err := h.PIM.Store.Objects(ctx, res.acc.Owner, store.ObjectFilter{Collections: []int64{res.acc.Collection.ID}})
		if err != nil {
			return nil, err
		}
		for i := range objs {
			out = append(out, h.objectResource(res, &objs[i]))
		}
	}
	return out, nil
}

func (h *Handler) objectResource(coll *resource, o *store.Object) *resource {
	return &resource{kind: kObject, user: coll.user, acc: coll.acc, home: coll.home, seg: coll.seg, name: o.Name,
		href: coll.href + esch(o.Name), obj: o}
}

// get serves an object, or a whole collection as one file (export).
func (h *Handler) get(w http.ResponseWriter, r *http.Request, res *resource) {
	switch res.kind {
	case kObject:
		if res.obj == nil {
			h.fail(w, http.StatusNotFound, "")
			return
		}
		o := res.obj
		if o.Data == "" {
			list, err := h.PIM.Store.ObjectsByID(r.Context(), res.acc.Owner, []int64{o.ID})
			if err != nil || len(list) == 0 {
				h.fail(w, http.StatusNotFound, "")
				return
			}
			o = &list[0]
		}
		if inm := r.Header.Get("If-None-Match"); inm != "" && (inm == o.ETag || inm == "*") {
			w.Header().Set("ETag", o.ETag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		ct := "text/calendar; charset=utf-8"
		if o.Kind == store.ObjectCard {
			ct = "text/vcard; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("ETag", o.ETag)
		if o.ScheduleTag != "" {
			w.Header().Set("Schedule-Tag", o.ScheduleTag)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.Data)))
		if r.Method == http.MethodHead {
			return
		}
		io.WriteString(w, o.Data)
	case kCalendar, kAddressBook:
		h.export(w, r, res)
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "robinmail DAV: %s\n", res.href)
	}
}

// export writes a calendar as one .ics, or an address book as one .vcf.
func (h *Handler) export(w http.ResponseWriter, r *http.Request, res *resource) {
	objs, err := h.PIM.Store.Objects(r.Context(), res.acc.Owner, store.ObjectFilter{Collections: []int64{res.acc.Collection.ID}, WithData: true})
	if err != nil {
		h.fail(w, http.StatusServiceUnavailable, "")
		return
	}
	name := res.acc.Collection.Name
	if res.kind == kAddressBook {
		w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+safeFile(name)+`.vcf"`)
		if r.Method == http.MethodHead {
			return
		}
		for _, o := range objs {
			io.WriteString(w, o.Data)
		}
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeFile(name)+`.ics"`)
	if r.Method == http.MethodHead {
		return
	}
	out := &ical.Component{Name: "VCALENDAR"}
	out.Add(ical.Prop{Name: "VERSION", Value: "2.0"})
	out.SetText("PRODID", "-//robinmail//robinmail//EN")
	out.SetText("X-WR-CALNAME", name)
	zones := map[string]bool{}
	for _, o := range objs {
		cal, err := ical.ParseOne([]byte(o.Data), "VCALENDAR")
		if err != nil {
			continue
		}
		for _, c := range cal.Children {
			if c.Name == "VTIMEZONE" {
				if zones[c.Text("TZID")] {
					continue
				}
				zones[c.Text("TZID")] = true
			}
			out.Children = append(out.Children, c)
		}
	}
	w.Write(ical.Encode(out))
}

func safeFile(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '"' || r == '\\' || r < 0x20 || r == '/' {
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "export"
	}
	return b.String()
}

// put stores an object (RFC 4791 5.3.2, RFC 6352 6.3.2).
func (h *Handler) put(w http.ResponseWriter, r *http.Request, res *resource) {
	if res.kind != kObject || (res.acc.Collection.Kind != store.KindCalendar && res.acc.Collection.Kind != store.KindAddressBook) {
		h.fail(w, http.StatusMethodNotAllowed, "")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, store.MaxObjectSize+1))
	if err != nil {
		h.fail(w, http.StatusBadRequest, "")
		return
	}
	isCal := res.acc.Collection.Kind == store.KindCalendar
	if len(data) > store.MaxObjectSize {
		ns := nsCardDAV
		if isCal {
			ns = nsCalDAV
		}
		h.precondition(w, http.StatusForbidden, ns, "max-resource-size")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil {
		switch {
		case isCal && mt != "text/calendar":
			h.precondition(w, http.StatusUnsupportedMediaType, nsCalDAV, "supported-calendar-data")
			return
		case !isCal && mt != "text/vcard" && mt != "text/x-vcard" && mt != "text/directory":
			h.precondition(w, http.StatusUnsupportedMediaType, nsCardDAV, "supported-address-data")
			return
		}
	}
	cond := pim.Cond{IfMatch: strings.TrimSpace(r.Header.Get("If-Match"))}
	if cond.IfMatch == "*" {
		cond.IfMatch, cond.MustExist = "", true
	}
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == "*" {
		cond.IfNoneMatch = true
	}
	if st := r.Header.Get("If-Schedule-Tag-Match"); st != "" && res.obj != nil && res.obj.ScheduleTag != "" && st != res.obj.ScheduleTag {
		h.fail(w, http.StatusPreconditionFailed, "")
		return
	}
	var o store.Object
	if isCal {
		cal, uid, _, perr := pim.ParseCalendarObject(data, res.acc.Collection.Components)
		if perr != nil {
			h.storeError(w, perr, true)
			return
		}
		if res.obj != nil && res.obj.UID != uid {
			// RFC 4791 5.3.2.1: the UID of a resource does not change.
			h.precondition(w, http.StatusConflict, nsCalDAV, "no-uid-conflict")
			return
		}
		o, err = h.PIM.PutEvent(r.Context(), res.user, res.acc, res.name, cal, cond, true)
	} else {
		card, uid, perr := pim.ParseCard(data)
		if perr != nil {
			h.storeError(w, perr, false)
			return
		}
		if res.obj != nil && res.obj.UID != uid {
			h.precondition(w, http.StatusConflict, nsCardDAV, "no-uid-conflict")
			return
		}
		o, err = h.PIM.PutCard(r.Context(), res.acc, res.name, card, cond)
	}
	if err != nil {
		h.storeError(w, err, isCal)
		return
	}
	// The stored text is written again by robinmail, so it is not always
	// byte for byte what was sent; the ETag is still that of what a GET
	// returns, which is what clients compare (RFC 4791 5.3.4 allows a
	// server to send it when the client can use it to fetch that version).
	w.Header().Set("ETag", o.ETag)
	if res.obj == nil {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

// delete removes an object, or a whole collection.
func (h *Handler) delete(w http.ResponseWriter, r *http.Request, res *resource) {
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	switch res.kind {
	case kObject:
		if res.obj == nil {
			h.fail(w, http.StatusNotFound, "")
			return
		}
		if ifMatch == "*" {
			ifMatch = ""
		}
		o := res.obj
		if o.Data == "" || o.Kind == store.ObjectEvent {
			if list, err := h.PIM.Store.ObjectsByID(r.Context(), res.acc.Owner, []int64{o.ID}); err == nil && len(list) == 1 {
				o = &list[0]
			}
		}
		var err error
		if o.Kind == store.ObjectITIP || o.Kind == store.ObjectCard {
			if !res.acc.MayWrite() && res.acc.Collection.Kind != store.KindInbox {
				err = pim.ErrForbidden
			} else {
				_, err = h.PIM.Store.DeleteObject(r.Context(), res.acc.Owner, o.ID, ifMatch, 0)
			}
		} else {
			// RFC 6638 3.2.2: Schedule-Reply: F asks not to tell anyone.
			schedule := !strings.EqualFold(r.Header.Get("Schedule-Reply"), "F")
			_, err = h.PIM.DeleteEvent(r.Context(), res.user, res.acc, *o, ifMatch, 0, schedule)
		}
		if err != nil {
			h.storeError(w, err, res.home == "calendars")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case kCalendar, kAddressBook:
		if res.acc.Rights != pim.RightOwner {
			// A sharee who deletes a shared calendar takes it out of their
			// own view; the owner keeps it.
			if err := h.PIM.Store.SetShare(r.Context(), res.acc.Owner, res.acc.Collection.ID, res.user.ID, ""); err != nil {
				h.storeError(w, err, res.kind == kCalendar)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if _, err := h.PIM.Store.DestroyCollection(r.Context(), res.user.ID, res.acc.Collection.ID, true, 0); err != nil {
			h.storeError(w, err, res.kind == kCalendar)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		h.fail(w, http.StatusForbidden, "")
	}
}

// move moves an object to another collection of the user (RFC 4918 9.9).
func (h *Handler) move(w http.ResponseWriter, r *http.Request, res *resource) {
	if res.kind != kObject || res.obj == nil {
		h.fail(w, http.StatusForbidden, "")
		return
	}
	dest, err := url.Parse(r.Header.Get("Destination"))
	if err != nil || dest.Path == "" {
		h.fail(w, http.StatusBadRequest, "no Destination")
		return
	}
	to, status := h.resolve(r.Context(), res.user, dest.Path)
	if status != 0 || to.kind != kObject {
		h.fail(w, http.StatusConflict, "")
		return
	}
	if to.obj != nil && !strings.EqualFold(r.Header.Get("Overwrite"), "T") && r.Header.Get("Overwrite") != "" {
		h.fail(w, http.StatusPreconditionFailed, "")
		return
	}
	if to.acc.Owner != res.acc.Owner || !res.acc.MayWrite() || !to.acc.MayWrite() || to.acc.Collection.Kind != res.acc.Collection.Kind {
		h.precondition(w, http.StatusForbidden, nsDAV, "need-privileges")
		return
	}
	if to.obj != nil && to.obj.ID != res.obj.ID {
		if _, err := h.PIM.Store.DeleteObject(r.Context(), to.acc.Owner, to.obj.ID, "", 0); err != nil {
			h.storeError(w, err, res.home == "calendars")
			return
		}
	}
	if _, err := h.PIM.Store.MoveObject(r.Context(), res.acc.Owner, res.obj.ID, to.acc.Collection.ID, to.name, strings.TrimSpace(r.Header.Get("If-Match")), 0); err != nil {
		h.storeError(w, err, res.home == "calendars")
		return
	}
	if to.obj != nil {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

// mkcolAt makes a calendar (MKCALENDAR, RFC 4791 5.3.1) or, with extended
// MKCOL (RFC 5689), a calendar or an address book, with properties, at
// the path the client chose.
func (h *Handler) mkcolAt(w http.ResponseWriter, r *http.Request, kind string) {
	user, _ := userFrom(r.Context())
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, Prefix), "/"), "/")
	if len(parts) != 3 || parts[1] != user.ID || (parts[0] != "calendars" && parts[0] != "addressbooks") {
		h.fail(w, http.StatusForbidden, "")
		return
	}
	body, err := parseXML(r.Body)
	if err != nil {
		h.fail(w, http.StatusBadRequest, "")
		return
	}
	var set *node
	if body != nil {
		set = body.child(nsDAV, "set")
		if set == nil {
			set = body.child(nsCalDAV, "set")
		}
	}
	props := set.child(nsDAV, "prop")
	if kind == "" {
		// Extended MKCOL: the resourcetype says what it is.
		rt := props.child(nsDAV, "resourcetype")
		switch {
		case rt.child(nsCalDAV, "calendar") != nil:
			kind = store.KindCalendar
		case rt.child(nsCardDAV, "addressbook") != nil:
			kind = store.KindAddressBook
		default:
			h.precondition(w, http.StatusForbidden, nsDAV, "valid-resourcetype")
			return
		}
	}
	if (kind == store.KindCalendar) != (parts[0] == "calendars") {
		h.precondition(w, http.StatusForbidden, nsDAV, "valid-resourcetype")
		return
	}
	seg, err := url.PathUnescape(parts[2])
	if err != nil || !validSegment(seg) {
		h.fail(w, http.StatusForbidden, "not a name for a collection")
		return
	}
	if _, status := h.resolve(r.Context(), user, r.URL.Path); status != http.StatusNotFound {
		// RFC 4918 9.3.1: MKCOL on what exists is 405.
		h.fail(w, http.StatusMethodNotAllowed, "")
		return
	}
	u := store.CollectionUpdate{Props: map[string]string{segmentProp: seg}}
	nameText := seg
	u.Name = &nameText
	if bad := h.applyProps(&u, props); len(bad) > 0 {
		h.fail(w, http.StatusForbidden, "")
		return
	}
	c, err := h.PIM.Store.CreateCollection(r.Context(), user.ID, kind, u, 0)
	if err != nil {
		h.storeError(w, err, kind == store.KindCalendar)
		return
	}
	w.Header().Set("Location", collectionHref(user, pim.Access{Owner: user.ID, Collection: c}))
	w.WriteHeader(http.StatusCreated)
}

// segmentProp keeps the path segment a client chose for a collection it
// made: it is reached under that segment as well as under its id.
const segmentProp = "robinmail:segment"

// validSegment is a segment a client may give a new collection: not one
// of ours, and nothing that could be read as another path.
func validSegment(s string) bool {
	if s == "" || len(s) > 100 || s == "inbox" || s == "outbox" || (len(s) > 17 && s[16] == '-') {
		return false // ours, or the form of a shared collection's segment
	}
	if _, ok := store.ParseCalendarID(s); ok {
		return false
	}
	if _, ok := store.ParseAddressBookID(s); ok {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '@') {
			return false
		}
	}
	return s != "." && s != ".."
}

// post answers a free/busy request to the outbox (RFC 6638 5).
func (h *Handler) post(w http.ResponseWriter, r *http.Request, res *resource) {
	if res.kind != kOutbox {
		h.fail(w, http.StatusMethodNotAllowed, "")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, store.MaxObjectSize))
	if err != nil {
		h.fail(w, http.StatusBadRequest, "")
		return
	}
	cal, err := ical.ParseOne(data, "VCALENDAR")
	if err != nil {
		h.precondition(w, http.StatusBadRequest, nsCalDAV, "valid-calendar-data")
		return
	}
	fbs := cal.Sub("VFREEBUSY")
	if !strings.EqualFold(cal.Value("METHOD"), "REQUEST") || len(fbs) != 1 {
		h.precondition(w, http.StatusForbidden, nsCalDAV, "valid-scheduling-message")
		return
	}
	fb := fbs[0]
	org := fb.Value("ORGANIZER")
	if !h.ownAddress(r.Context(), res.user, org) {
		h.precondition(w, http.StatusForbidden, nsCalDAV, "organizer-allowed")
		return
	}
	z := ical.ZonesOf(cal)
	sp, ep := fb.Get("DTSTART"), fb.Get("DTEND")
	if sp == nil || ep == nil {
		h.precondition(w, http.StatusForbidden, nsCalDAV, "valid-scheduling-message")
		return
	}
	sd, err1 := z.ParseDateTime(sp)
	ed, err2 := z.ParseDateTime(ep)
	from, to := sd.In(time.UTC), ed.In(time.UTC)
	if err1 != nil || err2 != nil || !to.After(from) || to.Sub(from) > 366*24*time.Hour {
		h.precondition(w, http.StatusForbidden, nsCalDAV, "valid-scheduling-message")
		return
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n" + `<c:schedule-response xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`)
	for _, a := range fb.All("ATTENDEE") {
		b.WriteString("<c:response><c:recipient><d:href>" + esc(a.Value) + "</d:href></c:recipient>")
		email := strings.TrimPrefix(strings.ToLower(a.Value), "mailto:")
		acct, err := h.PIM.Store.Account(r.Context(), email)
		if err != nil {
			b.WriteString("<c:request-status>3.7;Invalid calendar user</c:request-status></c:response>")
			continue
		}
		reply, err := h.PIM.FreeBusy(r.Context(), acct, org, a.Value, fb.Text("UID"), from, to)
		if err != nil {
			b.WriteString("<c:request-status>5.1;Service unavailable</c:request-status></c:response>")
			continue
		}
		b.WriteString("<c:request-status>2.0;Success</c:request-status><c:calendar-data>" + esc(string(ical.Encode(reply))) + "</c:calendar-data></c:response>")
	}
	b.WriteString("</c:schedule-response>")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	io.WriteString(w, b.String())
}

func (h *Handler) ownAddress(ctx context.Context, user store.Account, addr string) bool {
	for _, a := range h.PIM.Addresses(ctx, user.Address) {
		if pim.SameAddress(a, addr) {
			return true
		}
	}
	return false
}

// cleanPath is the path of an href in a request body.
func cleanPath(href string) string {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return ""
	}
	p := u.Path
	if p == "" {
		return ""
	}
	trail := strings.HasSuffix(p, "/")
	p = path.Clean(p)
	if trail && p != "/" {
		p += "/"
	}
	return p
}
