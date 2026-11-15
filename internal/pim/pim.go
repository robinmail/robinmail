// Package pim is what calendars and contacts mean, between the store and
// the two protocols that serve them (internal/dav, internal/jmap): which
// collections a user may see and change, what a valid calendar object or
// card is, what is indexed of it, and the scheduling that follows a change
// (iTIP, RFC 5546), by email (iMIP, RFC 6047) to the world and directly to
// the accounts of this server (RFC 6638's implicit scheduling).
package pim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/jscal"
	"github.com/robinmail/robinmail/internal/store"
)

var (
	// ErrForbidden is an operation the user's rights do not allow.
	ErrForbidden = errors.New("pim: not allowed")
	// ErrNotFound is a collection the user cannot see.
	ErrNotFound = errors.New("pim: no such calendar or address book")
)

// InvalidError is data that is not a valid calendar object or card. Cond
// is the precondition of CalDAV or CardDAV it breaks (RFC 4791 5.3.2.1,
// RFC 6352 6.3.2.1), such as "valid-calendar-data".
type InvalidError struct {
	Cond string
	Msg  string
}

func (e *InvalidError) Error() string { return e.Cond + ": " + e.Msg }

func invalid(cond, format string, args ...any) error {
	return &InvalidError{Cond: cond, Msg: fmt.Sprintf(format, args...)}
}

// Rights of a user on a collection.
const (
	RightOwner = "owner"
	RightWrite = store.RightWrite
	RightRead  = store.RightRead
)

// Access is a collection as one user sees it.
type Access struct {
	Owner        string // account id
	OwnerAddress string
	Collection   store.Collection
	Rights       string
}

// MayWrite reports whether the user may change the collection's members.
func (a Access) MayWrite() bool { return a.Rights == RightOwner || a.Rights == RightWrite }

// Service is the shared logic. Fill the fields, then use it from several
// goroutines.
type Service struct {
	Store *store.Store
	// ProdID is the PRODID of what robinmail writes.
	ProdID string
	// IMIPUnverified takes iTIP messages by email whose sender could not be
	// verified as if it had been (see IncomingMail). For tests.
	IMIPUnverified bool
	// Send queues a message of an account for others: an invitation or a
	// reply by email. nil sends none; scheduling then only reaches the
	// accounts of this server.
	Send func(ctx context.Context, account store.Account, from string, rcpts []string, raw []byte) error
	// SendAs lists the addresses of an account besides its own: they are
	// its calendar user addresses too.
	SendAs func(ctx context.Context, address string) ([]string, error)
	Logger *slog.Logger
	Now    func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (s *Service) prodID() string {
	if s.ProdID != "" {
		return s.ProdID
	}
	return "-//robinmail//robinmail//EN"
}

// Collections lists what a user sees of a kind: their own collections
// (made on first use) and those shared with them.
func (s *Service) Collections(ctx context.Context, user store.Account, kind string) ([]Access, error) {
	if err := s.Store.EnsureCollections(ctx, user.ID); err != nil {
		return nil, err
	}
	own, err := s.Store.Collections(ctx, user.ID, kind)
	if err != nil {
		return nil, err
	}
	out := make([]Access, 0, len(own))
	for _, c := range own {
		out = append(out, Access{Owner: user.ID, OwnerAddress: user.Address, Collection: c, Rights: RightOwner})
	}
	if kind == store.KindInbox {
		return out, nil
	}
	shared, err := s.Store.SharedWith(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	for _, sh := range shared {
		if kind != "" && sh.Kind != kind {
			continue
		}
		c, err := s.Store.CollectionByID(ctx, sh.Owner, sh.CollectionID)
		if err != nil {
			continue // removed meanwhile
		}
		out = append(out, Access{Owner: sh.Owner, OwnerAddress: sh.OwnerAddress, Collection: c, Rights: sh.Rights})
	}
	return out, nil
}

// SharedCollections lists what an owner shares with a user, as the user
// sees it.
func (s *Service) SharedCollections(ctx context.Context, user store.Account, owner string, kind string) ([]Access, error) {
	shared, err := s.Store.SharedWith(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	var out []Access
	for _, sh := range shared {
		if sh.Owner != owner || (kind != "" && sh.Kind != kind) {
			continue
		}
		c, err := s.Store.CollectionByID(ctx, sh.Owner, sh.CollectionID)
		if err != nil {
			continue
		}
		out = append(out, Access{Owner: sh.Owner, OwnerAddress: sh.OwnerAddress, Collection: c, Rights: sh.Rights})
	}
	return out, nil
}

// Collection returns one collection of an owner as a user sees it.
func (s *Service) Collection(ctx context.Context, user store.Account, owner string, id int64) (Access, error) {
	if owner == user.ID {
		c, err := s.Store.CollectionByID(ctx, owner, id)
		if errors.Is(err, store.ErrNoCollection) {
			return Access{}, ErrNotFound
		}
		if err != nil {
			return Access{}, err
		}
		return Access{Owner: owner, OwnerAddress: user.Address, Collection: c, Rights: RightOwner}, nil
	}
	shared, err := s.SharedCollections(ctx, user, owner, "")
	if err != nil {
		return Access{}, err
	}
	for _, a := range shared {
		if a.Collection.ID == id {
			return a, nil
		}
	}
	return Access{}, ErrNotFound
}

// NewName makes a resource name for an object made by robinmail.
func NewName(uid, ext string) string {
	clean := true
	for _, r := range uid {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			clean = false
			break
		}
	}
	if clean && uid != "" && len(uid) <= 100 {
		return uid + ext
	}
	return RandomID() + ext
}

// RandomID is 128 random bits in hex.
func RandomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// NewUID makes a UID in the form of RFC 4122.
func NewUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ParseCalendarObject reads and checks a calendar object resource (RFC
// 4791 4.1): one VCALENDAR whose components share one UID and one type.
// A METHOD, which a resource must not have, is taken out.
func ParseCalendarObject(data []byte, allowed string) (*ical.Component, string, string, error) {
	cal, err := ical.ParseOne(data, "VCALENDAR")
	if err != nil {
		return nil, "", "", invalid("valid-calendar-data", "%v", err)
	}
	cal.Remove("METHOD")
	comps := jscal.Components(cal)
	if len(comps) == 0 {
		return nil, "", "", invalid("valid-calendar-object-resource", "no VEVENT, VTODO or VJOURNAL")
	}
	kind, uid := comps[0].Name, comps[0].Text("UID")
	if uid == "" {
		return nil, "", "", invalid("valid-calendar-object-resource", "a component without UID")
	}
	for _, c := range comps {
		if c.Name != kind || c.Text("UID") != uid {
			return nil, "", "", invalid("valid-calendar-object-resource", "components of more than one UID or type")
		}
		if c.Name == "VFREEBUSY" {
			return nil, "", "", invalid("supported-calendar-component", "VFREEBUSY is not stored")
		}
		if p := c.Get("DTSTART"); p != nil {
			if _, err := (ical.Zones{}).ParseDateTime(p); err != nil {
				return nil, "", "", invalid("valid-calendar-data", "DTSTART: %v", err)
			}
		} else if c.Name == "VEVENT" && c.Get("RECURRENCE-ID") == nil {
			// RFC 5545 3.6.1: DTSTART is required unless a METHOD is.
			return nil, "", "", invalid("valid-calendar-data", "a VEVENT without DTSTART")
		}
		for _, r := range c.All("RRULE") {
			if _, err := ical.ParseRRule(r.Value); err != nil {
				return nil, "", "", invalid("valid-calendar-data", "RRULE: %v", err)
			}
		}
	}
	if allowed != "" && !strings.Contains(" "+allowed+" ", " "+kind+" ") {
		return nil, "", "", invalid("supported-calendar-component", "this calendar does not take %s", kind)
	}
	return cal, uid, kind, nil
}

// zoneOf is the zone floating times of a collection are read in.
func zoneOf(c store.Collection) *time.Location {
	if c.TimeZone != "" {
		if loc := ical.LoadZone(c.TimeZone); loc != nil {
			return loc
		}
	}
	return time.UTC
}

// index computes the columns of an event that queries use.
func index(cal *ical.Component, c store.Collection) (start, end *int64, summary string) {
	o := jscal.Read(cal, zoneOf(c))
	if o.Master != nil {
		summary = o.Master.Text("SUMMARY")
	}
	s, e, forever := o.Span()
	if s.IsZero() {
		return nil, nil, summary
	}
	su := s.Unix()
	start = &su
	if !forever {
		eu := e.Unix()
		if eu <= su {
			eu = su + 1
		}
		end = &eu
	}
	return start, end, summary
}

// Cond are the preconditions of a write.
type Cond struct {
	ID          int64 // replace this object (JMAP); 0 finds it by name
	IfMatch     string
	IfNoneMatch bool
	MustExist   bool
	IfState     int64
}

// PutEvent stores a calendar object in a collection the user may write,
// then sends what scheduling it calls for when schedule is set.
func (s *Service) PutEvent(ctx context.Context, user store.Account, acc Access, name string, cal *ical.Component, cond Cond, schedule bool) (store.Object, error) {
	if !acc.MayWrite() {
		return store.Object{}, ErrForbidden
	}
	if acc.Collection.Kind != store.KindCalendar {
		return store.Object{}, invalid("supported-calendar-component", "not a calendar")
	}
	comps := jscal.Components(cal)
	if len(comps) == 0 {
		return store.Object{}, invalid("valid-calendar-object-resource", "no component")
	}
	var old *ical.Component
	if schedule {
		if prev, err := s.existing(ctx, acc.Owner, acc.Collection.ID, name, cond.ID); err == nil {
			old = prev
		}
	}
	data := string(ical.Encode(cal))
	start, end, summary := index(cal, acc.Collection)
	o, err := s.Store.Put(ctx, acc.Owner, store.PutObject{CollectionID: acc.Collection.ID, ID: cond.ID, Name: name,
		Kind: store.ObjectEvent, UID: comps[0].Text("UID"), Data: data, Component: comps[0].Name, Start: start, End: end,
		Summary: summary, IfMatch: cond.IfMatch, IfNoneMatch: cond.IfNoneMatch, MustExist: cond.MustExist, IfState: cond.IfState})
	if err != nil {
		return o, err
	}
	if schedule {
		s.afterChange(ctx, acc.Owner, old, cal)
	}
	return o, nil
}

func (s *Service) existing(ctx context.Context, owner string, collection int64, name string, id int64) (*ical.Component, error) {
	var o store.Object
	var err error
	if id != 0 {
		var list []store.Object
		list, err = s.Store.ObjectsByID(ctx, owner, []int64{id})
		if err == nil && len(list) == 0 {
			err = store.ErrNoObject
		}
		if err == nil {
			o = list[0]
		}
	} else {
		o, err = s.Store.ObjectByName(ctx, owner, collection, name)
	}
	if err != nil {
		return nil, err
	}
	return ical.ParseOne([]byte(o.Data), "VCALENDAR")
}

// DeleteEvent removes a calendar object and, when schedule is set, tells
// the attendees (or the organizer) of it.
func (s *Service) DeleteEvent(ctx context.Context, user store.Account, acc Access, o store.Object, ifMatch string, ifState int64, schedule bool) (int64, error) {
	if !acc.MayWrite() {
		return 0, ErrForbidden
	}
	state, err := s.Store.DeleteObject(ctx, acc.Owner, o.ID, ifMatch, ifState)
	if err != nil {
		return 0, err
	}
	if schedule && o.Kind == store.ObjectEvent && o.Data != "" {
		if old, err := ical.ParseOne([]byte(o.Data), "VCALENDAR"); err == nil {
			s.afterChange(ctx, acc.Owner, old, nil)
		}
	}
	return state, nil
}

// ParseCard reads and checks an address object resource (RFC 6352 5.1):
// one VCARD with a UID and FN (RFC 6350 6.2.1; vCard 3.0 also needs N,
// which is added when missing).
func ParseCard(data []byte) (*ical.Component, string, error) {
	v, err := ical.ParseOne(data, "VCARD")
	if err != nil {
		return nil, "", invalid("valid-address-data", "%v", err)
	}
	ver := v.Value("VERSION")
	if ver != "3.0" && ver != "4.0" {
		return nil, "", invalid("supported-address-data", "vCard version %q", ver)
	}
	uid := v.Text("UID")
	if uid == "" {
		return nil, "", invalid("valid-address-data", "no UID")
	}
	if v.Get("FN") == nil {
		return nil, "", invalid("valid-address-data", "no FN")
	}
	return v, uid, nil
}

// PutCard stores a card in an address book the user may write.
func (s *Service) PutCard(ctx context.Context, acc Access, name string, v *ical.Component, cond Cond) (store.Object, error) {
	if !acc.MayWrite() {
		return store.Object{}, ErrForbidden
	}
	if acc.Collection.Kind != store.KindAddressBook {
		return store.Object{}, invalid("supported-address-data", "not an address book")
	}
	return s.Store.Put(ctx, acc.Owner, store.PutObject{CollectionID: acc.Collection.ID, ID: cond.ID, Name: name,
		Kind: store.ObjectCard, UID: v.Text("UID"), Data: string(ical.Encode(v)), Summary: v.Text("FN"),
		IfMatch: cond.IfMatch, IfNoneMatch: cond.IfNoneMatch, MustExist: cond.MustExist, IfState: cond.IfState})
}

// Addresses are the calendar user addresses of an account, lower case,
// as "mailto:" URIs: its address and the aliases that deliver to it.
func (s *Service) Addresses(ctx context.Context, address string) []string {
	out := []string{"mailto:" + strings.ToLower(address)}
	if s.SendAs != nil {
		if more, err := s.SendAs(ctx, address); err == nil {
			for _, a := range more {
				if !strings.Contains(a, "*") {
					out = append(out, "mailto:"+strings.ToLower(a))
				}
			}
		}
	}
	return out
}

// SameAddress compares two calendar user addresses: "mailto:" without
// regard to case (RFC 6638 says to compare them as URIs; addresses are
// compared case-insensitively in practice).
func SameAddress(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > 7 && strings.EqualFold(s[:7], "mailto:") {
			return "mailto:" + strings.ToLower(s[7:])
		}
		return strings.ToLower(s)
	}
	return norm(a) == norm(b)
}

func isOwn(addrs []string, a string) bool {
	for _, x := range addrs {
		if SameAddress(x, a) {
			return true
		}
	}
	return false
}
