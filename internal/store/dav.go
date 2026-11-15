package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/robinmail/robinmail/internal/rqlite"
)

// Calendars and address books (phase 9). A collection is a calendar, an
// address book, or the scheduling inbox of RFC 6638 (one per account). An
// object is one calendar object (the VEVENTs of one UID, RFC 4791 4.1) or
// one vCard, stored as the text a client sent or JMAP made: once, in rqlite,
// with a few columns taken from it for queries. Like mail, every write is
// one batch that moves the account's modseq and logs the change; that log
// gives JMAP its /changes, and with the tombstones of removed objects it
// gives DAV its sync tokens (RFC 6578).

// Collection kinds.
const (
	KindCalendar    = "calendar"
	KindAddressBook = "addressbook"
	KindInbox       = "inbox" // the scheduling inbox (RFC 6638 2.2)
)

// Object kinds.
const (
	ObjectEvent = "event" // a calendar object: VEVENT, VTODO or VJOURNAL
	ObjectCard  = "card"  // a vCard
	ObjectITIP  = "itip"  // a scheduling message in the inbox
)

var (
	ErrNoCollection      = errors.New("no such calendar or address book")
	ErrNoObject          = errors.New("no such calendar object or card")
	ErrPrecondition      = errors.New("the object is not in the state the request expects")
	ErrUIDConflict       = errors.New("the collection has another object with that UID")
	ErrCollectionName    = errors.New("not a name for a calendar or address book")
	ErrCollectionLimit   = errors.New("too many calendars or address books")
	ErrCollectionInUse   = errors.New("the calendar or address book is not empty")
	ErrDefaultCollection = errors.New("the default calendar or address book cannot be destroyed")
	ErrObjectTooLarge    = errors.New("the calendar object or card is too large")
	ErrNoShare           = errors.New("no such account to share with")
)

// Limits on collections and objects.
const (
	MaxCollections    = 100
	MaxCollectionName = 255
	MaxObjectSize     = 1 << 20 // RFC 4791 5.2.5, announced as max-resource-size
)

const (
	calendarPrefix    = 'c'
	addressBookPrefix = 'b'
	eventPrefix       = 'v'
	cardPrefix        = 'p'
)

func CalendarID(n int64) string                 { return formatID(calendarPrefix, n) }
func AddressBookID(n int64) string              { return formatID(addressBookPrefix, n) }
func CalendarEventID(n int64) string            { return formatID(eventPrefix, n) }
func ContactCardID(n int64) string              { return formatID(cardPrefix, n) }
func ParseCalendarID(s string) (int64, bool)    { return parseID(calendarPrefix, s) }
func ParseAddressBookID(s string) (int64, bool) { return parseID(addressBookPrefix, s) }
func ParseCalendarEventID(s string) (int64, bool) {
	return parseID(eventPrefix, s)
}
func ParseContactCardID(s string) (int64, bool) { return parseID(cardPrefix, s) }

// Collection is a calendar, an address book or a scheduling inbox.
type Collection struct {
	ID           int64
	Kind         string
	Name         string
	Description  string
	Color        string
	SortOrder    int64
	IsDefault    bool
	IsSubscribed bool
	IsVisible    bool
	// IncludeInAvailability is "all", "attending" or "none" (JMAP
	// Calendars 4).
	IncludeInAvailability string
	TimeZone              string // IANA name, for floating times; "" is UTC
	// Components are the components a calendar takes, "VEVENT VTODO"
	// (RFC 4791 5.2.3).
	Components string
	// Props are the properties a DAV client set that robinmail does not
	// interpret (RFC 4918 dead properties), and the JMAP default alerts,
	// by name.
	Props         map[string]string
	CreatedAt     time.Time
	Modseq        int64 // of the collection's own properties
	ContentModseq int64 // of the newest change to a member
}

const collectionColumns = `id, kind, name, description, color, sort_order, is_default, is_subscribed, is_visible,
	include_availability, timezone, components, props, created_at, modseq, content_modseq`

func scanCollection(row []any) Collection {
	c := Collection{ID: asInt(row[0]), Kind: asString(row[1]), Name: asString(row[2]), Description: asString(row[3]),
		Color: asString(row[4]), SortOrder: asInt(row[5]), IsDefault: asInt(row[6]) != 0, IsSubscribed: asInt(row[7]) != 0,
		IsVisible: asInt(row[8]) != 0, IncludeInAvailability: asString(row[9]), TimeZone: asString(row[10]),
		Components: asString(row[11]), CreatedAt: time.Unix(asInt(row[13]), 0).UTC(), Modseq: asInt(row[14]),
		ContentModseq: asInt(row[15])}
	if s := asString(row[12]); s != "" {
		json.Unmarshal([]byte(s), &c.Props)
	}
	return c
}

func validCollectionName(s string) bool {
	if s == "" || len(s) > MaxCollectionName || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func propsJSON(p map[string]string) string {
	if len(p) == 0 {
		return ""
	}
	b, _ := json.Marshal(p)
	return string(b)
}

// Collections lists an account's collections of a kind ("" for all), by
// sort order and id.
func (s *Store) Collections(ctx context.Context, account, kind string) ([]Collection, error) {
	res, err := s.query(ctx, account, q(`SELECT `+collectionColumns+` FROM collection
		WHERE account_id = :a AND (:kind = '' OR kind = :kind) ORDER BY sort_order, id`, "kind", kind))
	if err != nil {
		return nil, err
	}
	out := make([]Collection, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, scanCollection(row))
	}
	return out, nil
}

// CollectionByID returns one collection.
func (s *Store) CollectionByID(ctx context.Context, account string, id int64) (Collection, error) {
	res, err := s.query(ctx, account, q(`SELECT `+collectionColumns+` FROM collection WHERE account_id = :a AND id = :id`, "id", id))
	if err != nil {
		return Collection{}, err
	}
	if len(res[0].Values) == 0 {
		return Collection{}, ErrNoCollection
	}
	return scanCollection(res[0].Values[0]), nil
}

// EnsureCollections gives an account its default calendar, address book
// and scheduling inbox when it lacks them. Each insert is conditional on
// there being none, so the batch may run twice and on several nodes.
func (s *Store) EnsureCollections(ctx context.Context, account string) error {
	res, err := s.query(ctx, account, q(`SELECT
		EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND kind = 'calendar' AND is_default = 1),
		EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND kind = 'addressbook' AND is_default = 1),
		EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND kind = 'inbox')`))
	if err != nil {
		return err
	}
	if len(res[0].Values) > 0 && asInt(res[0].Values[0][0]) != 0 && asInt(res[0].Values[0][1]) != 0 && asInt(res[0].Values[0][2]) != 0 {
		return nil
	}
	b := s.newBatch(account)
	b.guardAccount()
	b.bump()
	for _, d := range []struct{ kind, name, typ string }{
		{KindCalendar, "Calendar", "Calendar"}, {KindAddressBook, "Contacts", "AddressBook"}, {KindInbox, "Inbox", ""},
	} {
		cond := `NOT EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND kind = :kind AND is_default = 1)`
		if d.kind == KindInbox {
			cond = `NOT EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND kind = :kind)`
		}
		b.add(`UPDATE account SET next_collection = next_collection + 1 WHERE id = :a AND `+cond, "kind", d.kind)
		b.add(`INSERT INTO collection (account_id, id, kind, name, is_default, created_at, modseq, content_modseq)
			SELECT :a, next_collection - 1, :kind, :name, :def, :now, modseq, modseq FROM account WHERE id = :a AND `+cond,
			"kind", d.kind, "name", d.name, "def", map[bool]int{true: 0, false: 1}[d.kind == KindInbox])
		if d.typ != "" {
			b.change(d.typ, `id, 'created' AS op FROM collection WHERE account_id = :a AND kind = :kind AND modseq = `+sqlModseq+` AND is_default = 1`,
				"kind", d.kind)
		}
	}
	_, err = s.run(ctx, b)
	return err
}

// CollectionUpdate changes the properties that are not nil.
type CollectionUpdate struct {
	Name                  *string
	Description           *string
	Color                 *string
	SortOrder             *int64
	IsSubscribed          *bool
	IsVisible             *bool
	IncludeInAvailability *string
	TimeZone              *string
	Components            *string
	// Props sets dead properties; an empty value removes one.
	Props map[string]string
	// MakeDefault makes this the default of its kind.
	MakeDefault bool
}

func collectionType(kind string) string {
	if kind == KindAddressBook {
		return "AddressBook"
	}
	return "Calendar"
}

// CreateCollection makes a calendar or an address book.
func (s *Store) CreateCollection(ctx context.Context, account, kind string, u CollectionUpdate, ifState int64) (Collection, error) {
	if kind != KindCalendar && kind != KindAddressBook {
		return Collection{}, errors.New("store: not a collection kind: " + kind)
	}
	if u.Name == nil || !validCollectionName(*u.Name) {
		return Collection{}, ErrCollectionName
	}
	str := func(p *string, def string) string {
		if p == nil {
			return def
		}
		return *p
	}
	boolean := func(p *bool, def bool) int {
		if p != nil {
			def = *p
		}
		if def {
			return 1
		}
		return 0
	}
	var order int64
	if u.SortOrder != nil {
		order = *u.SortOrder
	}
	comps := str(u.Components, "")
	if kind == KindCalendar && comps == "" {
		comps = "VEVENT VTODO"
	}
	b := s.newBatch(account)
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrCollectionLimit, `(SELECT count(*) FROM collection WHERE account_id = :a) < :max`, "max", MaxCollections)
	b.bump("next_collection")
	b.add(`INSERT INTO collection (account_id, id, kind, name, description, color, sort_order, is_default, is_subscribed,
			is_visible, include_availability, timezone, components, props, created_at, modseq, content_modseq)
		SELECT :a, next_collection - 1, :kind, :name, :descr, :color, :ord,
			NOT EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND kind = :kind AND is_default = 1),
			:sub, :vis, :avail, :tz, :comps, :props, :now, modseq, modseq FROM account WHERE id = :a`,
		"kind", kind, "name", *u.Name, "descr", str(u.Description, ""), "color", str(u.Color, ""), "ord", order,
		"sub", boolean(u.IsSubscribed, true), "vis", boolean(u.IsVisible, true), "avail", str(u.IncludeInAvailability, "all"),
		"tz", str(u.TimeZone, ""), "comps", comps, "props", propsJSON(u.Props))
	b.change(collectionType(kind), `next_collection - 1 AS id, 'created' AS op FROM account WHERE id = :a`)
	b.add(`SELECT ` + collectionColumns + ` FROM collection WHERE account_id = :a AND id = (SELECT next_collection - 1 FROM account WHERE id = :a)`)
	res, err := s.run(ctx, b)
	if err != nil {
		return Collection{}, err
	}
	return scanCollection(res[len(res)-1].Values[0]), nil
}

// UpdateCollection changes a collection's properties.
func (s *Store) UpdateCollection(ctx context.Context, account string, id int64, u CollectionUpdate, ifState int64) (int64, error) {
	if u.Name != nil && !validCollectionName(*u.Name) {
		return 0, ErrCollectionName
	}
	cur, err := s.CollectionByID(ctx, account, id)
	if err != nil {
		return 0, err
	}
	b := s.newBatch(account)
	b.base["id"] = id
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrNoCollection, `EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND id = :id)`)
	b.bump()
	set := func(col string, v any) {
		b.add(`UPDATE collection SET `+col+` = :v, modseq = `+sqlModseq+` WHERE account_id = :a AND id = :id`, "v", v)
	}
	if u.Name != nil {
		set("name", *u.Name)
	}
	if u.Description != nil {
		set("description", *u.Description)
	}
	if u.Color != nil {
		set("color", *u.Color)
	}
	if u.SortOrder != nil {
		set("sort_order", *u.SortOrder)
	}
	if u.IsSubscribed != nil {
		set("is_subscribed", *u.IsSubscribed)
	}
	if u.IsVisible != nil {
		set("is_visible", *u.IsVisible)
	}
	if u.IncludeInAvailability != nil {
		set("include_availability", *u.IncludeInAvailability)
	}
	if u.TimeZone != nil {
		set("timezone", *u.TimeZone)
	}
	if u.Components != nil {
		set("components", *u.Components)
	}
	if len(u.Props) > 0 {
		props := map[string]string{}
		for k, v := range cur.Props {
			props[k] = v
		}
		for k, v := range u.Props {
			if v == "" {
				delete(props, k)
			} else {
				props[k] = v
			}
		}
		// The props are read and written whole; the guard makes sure no
		// other write changed them meanwhile.
		b.guard(ErrStateMismatch, `(SELECT modseq FROM collection WHERE account_id = :a AND id = :id) = :cm`, "cm", cur.Modseq)
		set("props", propsJSON(props))
	}
	typ := collectionType(cur.Kind)
	if u.MakeDefault && cur.Kind != KindInbox {
		b.change(typ, `id, 'updated' AS op FROM collection WHERE account_id = :a AND kind = :kind AND is_default = 1 AND id <> :id`, "kind", cur.Kind)
		b.add(`UPDATE collection SET is_default = 0, modseq = `+sqlModseq+` WHERE account_id = :a AND kind = :kind AND is_default = 1 AND id <> :id`, "kind", cur.Kind)
		set("is_default", 1)
	}
	b.change(typ, `:id AS id, 'updated' AS op`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// DestroyCollection removes a collection. With removeContents its objects
// go with it; without, a collection that has any is refused. The default
// one is refused; make another the default first.
func (s *Store) DestroyCollection(ctx context.Context, account string, id int64, removeContents bool, ifState int64) (int64, error) {
	cur, err := s.CollectionByID(ctx, account, id)
	if err != nil {
		return 0, err
	}
	if cur.Kind == KindInbox {
		return 0, ErrDefaultCollection
	}
	b := s.newBatch(account)
	b.base["id"] = id
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrNoCollection, `EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND id = :id)`)
	b.guard(ErrDefaultCollection, `NOT EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND id = :id AND is_default = 1)`)
	if !removeContents {
		b.guard(ErrCollectionInUse, `NOT EXISTS (SELECT 1 FROM dav_object WHERE account_id = :a AND collection_id = :id)`)
	}
	b.bump()
	objType := "CalendarEvent"
	if cur.Kind == KindAddressBook {
		objType = "ContactCard"
	}
	b.change(objType, `id, 'destroyed' AS op FROM dav_object WHERE account_id = :a AND collection_id = :id`)
	b.add(`DELETE FROM dav_object WHERE account_id = :a AND collection_id = :id`)
	b.add(`DELETE FROM dav_tombstone WHERE account_id = :a AND collection_id = :id`)
	b.add(`DELETE FROM collection_share WHERE account_id = :a AND collection_id = :id`)
	b.add(`DELETE FROM collection WHERE account_id = :a AND id = :id`)
	b.change(collectionType(cur.Kind), `:id AS id, 'destroyed' AS op`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	s.notifySharees(ctx, account, id)
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// Object is a calendar object or a card.
type Object struct {
	ID           int64
	CollectionID int64
	Kind         string
	Name         string // the DAV resource name, "<uid>.ics"
	UID          string
	Data         string // empty when listed without data
	ETag         string
	Size         int64
	Component    string // VEVENT, VTODO, VJOURNAL; "" for a card
	// Start and End bound the instances of an event, in Unix seconds; a
	// NULL End is an event that recurs for ever. Both NULL: unknown.
	Start, End  *int64
	Summary     string
	ScheduleTag string
	CreatedAt   time.Time
	// Created is the modseq the object was made at, Modseq that of its
	// last change.
	Created int64
	Modseq  int64
}

const objectColumns = `id, collection_id, kind, name, uid, etag, size, component, start_at, end_at, summary, schedule_tag, created_at, created_modseq, modseq`

func scanObject(row []any, withData bool) Object {
	o := Object{ID: asInt(row[0]), CollectionID: asInt(row[1]), Kind: asString(row[2]), Name: asString(row[3]),
		UID: asString(row[4]), ETag: asString(row[5]), Size: asInt(row[6]), Component: asString(row[7]),
		Summary: asString(row[10]), ScheduleTag: asString(row[11]), CreatedAt: time.Unix(asInt(row[12]), 0).UTC(),
		Created: asInt(row[13]), Modseq: asInt(row[14])}
	if row[8] != nil {
		v := asInt(row[8])
		o.Start = &v
	}
	if row[9] != nil {
		v := asInt(row[9])
		o.End = &v
	}
	if withData && len(row) > 15 {
		o.Data = asString(row[15])
	}
	return o
}

func cols(withData bool) string {
	if withData {
		return objectColumns + `, data`
	}
	return objectColumns
}

func scanObjects(res []rqlite.Result, withData bool) []Object {
	out := make([]Object, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, scanObject(row, withData))
	}
	return out
}

// ObjectFilter selects objects of an account.
type ObjectFilter struct {
	Kind        string  // "" for all kinds
	Collections []int64 // nil for all
	UID         string
	// From and To select events that may have an instance in [From, To):
	// by the span columns; the caller expands to be exact.
	From, To *time.Time
	WithData bool
}

// Objects lists objects by id.
func (s *Store) Objects(ctx context.Context, account string, f ObjectFilter) ([]Object, error) {
	sql := `SELECT ` + cols(f.WithData) + ` FROM dav_object WHERE account_id = :a`
	kv := []any{}
	if f.Kind != "" {
		sql += ` AND kind = :kind`
		kv = append(kv, "kind", f.Kind)
	}
	if f.Collections != nil {
		if len(f.Collections) == 0 {
			return nil, nil
		}
		ids, _ := json.Marshal(f.Collections)
		sql += ` AND collection_id IN (SELECT value FROM json_each(:colls))`
		kv = append(kv, "colls", string(ids))
	}
	if f.UID != "" {
		sql += ` AND uid = :uid`
		kv = append(kv, "uid", f.UID)
	}
	if f.To != nil {
		sql += ` AND (start_at IS NULL OR start_at < :to)`
		kv = append(kv, "to", f.To.Unix())
	}
	if f.From != nil {
		sql += ` AND (start_at IS NULL OR end_at IS NULL OR end_at >= :from)`
		kv = append(kv, "from", f.From.Unix())
	}
	res, err := s.query(ctx, account, q(sql+` ORDER BY id`, kv...))
	if err != nil {
		return nil, err
	}
	return scanObjects(res, f.WithData), nil
}

// ObjectsByID returns the objects with the given ids that exist, with
// their data.
func (s *Store) ObjectsByID(ctx context.Context, account string, ids []int64) ([]Object, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	list, _ := json.Marshal(ids)
	res, err := s.query(ctx, account, q(`SELECT `+cols(true)+` FROM dav_object WHERE account_id = :a
		AND id IN (SELECT value FROM json_each(:ids)) ORDER BY id`, "ids", string(list)))
	if err != nil {
		return nil, err
	}
	return scanObjects(res, true), nil
}

// ObjectByName returns one object of a collection, with its data.
func (s *Store) ObjectByName(ctx context.Context, account string, collection int64, name string) (Object, error) {
	res, err := s.query(ctx, account, q(`SELECT `+cols(true)+` FROM dav_object WHERE account_id = :a
		AND collection_id = :c AND name = :name`, "c", collection, "name", name))
	if err != nil {
		return Object{}, err
	}
	if len(res[0].Values) == 0 {
		return Object{}, ErrNoObject
	}
	return scanObject(res[0].Values[0], true), nil
}

// ETag is the entity tag of an object's data: a strong validator.
func ETag(data string) string {
	sum := sha256.Sum256([]byte(data))
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

// PutObject is what a write of an object stores.
type PutObject struct {
	CollectionID int64
	// ID, when not 0, is the object to replace (JMAP); otherwise the
	// object is found by Name in the collection (DAV), or made.
	ID          int64
	Name        string
	Kind        string
	UID         string
	Data        string
	Component   string
	Start, End  *int64
	Summary     string
	ScheduleTag string
	// Preconditions: IfMatch is the ETag the object must have; IfNoneMatch
	// refuses an existing object (RFC 4918 10.4, "If-None-Match: *");
	// MustExist refuses a new one.
	IfMatch     string
	IfNoneMatch bool
	MustExist   bool
	IfState     int64
}

// guardObjectQuota makes the batch fail unless the account has room for
// the object, counting what it replaces.
func (b *batch) guardObjectQuota(def Limits, size int, newObject string) {
	b.guard(ErrOverQuota, `(SELECT (COALESCE(quota_bytes, :qbytes) = 0 OR used_bytes + :size - COALESCE((`+newObject+`), 0) <= COALESCE(quota_bytes, :qbytes))
		AND (COALESCE(quota_count, :qmessages) = 0 OR (`+newObject+`) IS NOT NULL OR used_count + 1 <= COALESCE(quota_count, :qmessages))
		FROM account WHERE id = :a)`, "qbytes", def.Bytes, "qmessages", def.Messages, "size", size)
}

func objectType(kind string) string {
	if kind == ObjectCard {
		return "ContactCard"
	}
	if kind == ObjectITIP {
		return "SchedulingMessage"
	}
	return "CalendarEvent"
}

// Put stores an object: a new one, or a new version of one, in one batch
// with its preconditions. It returns the object as stored, without data.
func (s *Store) Put(ctx context.Context, account string, p PutObject) (Object, error) {
	if len(p.Data) > MaxObjectSize {
		return Object{}, ErrObjectTooLarge
	}
	etag := ETag(p.Data)
	b := s.newBatch(account)
	b.base["c"] = p.CollectionID
	b.base["id"] = p.ID
	b.base["name"] = p.Name
	b.guardAccount()
	b.ifState(p.IfState)
	b.guard(ErrNoCollection, `EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND id = :c)`)
	// The row this write replaces, found by id or by name.
	var target string
	if p.ID != 0 {
		target = `SELECT %s FROM dav_object WHERE account_id = :a AND id = :id`
		b.guard(ErrNoObject, `EXISTS (SELECT 1 FROM dav_object WHERE account_id = :a AND id = :id)`)
		// The name stays; a move to another collection is MoveObject.
		b.guard(ErrNoObject, `EXISTS (SELECT 1 FROM dav_object WHERE account_id = :a AND id = :id AND collection_id = :c)`)
	} else {
		target = `SELECT %s FROM dav_object WHERE account_id = :a AND collection_id = :c AND name = :name`
	}
	sel := func(col string) string { return strings.Replace(target, "%s", col, 1) }
	if p.IfNoneMatch {
		b.guard(ErrPrecondition, `NOT EXISTS (`+sel("1")+`)`)
	}
	if p.MustExist || p.IfMatch != "" {
		b.guard(ErrPrecondition, `EXISTS (`+sel("1")+`)`)
	}
	if p.IfMatch != "" {
		b.guard(ErrPrecondition, `(`+sel("etag")+`) = :ifmatch`, "ifmatch", p.IfMatch)
	}
	b.guardObjectQuota(s.DefaultQuota, len(p.Data), sel("size"))
	// A new object takes the counter; the bump decides by whether the row
	// exists before it is written.
	b.bumped = true
	b.add(`UPDATE account SET modseq = modseq + 1, next_object = next_object + (NOT EXISTS (` + sel("1") + `)) WHERE id = :a`)
	kv := []any{"kind", p.Kind, "uid", p.UID, "data", p.Data, "etag", etag, "size", len(p.Data), "comp", p.Component,
		"start", p.Start, "end", p.End, "summary", p.Summary, "stag", p.ScheduleTag}
	if p.ID == 0 {
		b.unique(ErrUIDConflict)
		b.add(`INSERT INTO dav_object (account_id, id, collection_id, kind, name, uid, data, etag, size, component, start_at, end_at,
				summary, schedule_tag, created_at, created_modseq, modseq)
			SELECT :a, next_object - 1, :c, :kind, :name, :uid, :data, :etag, :size, :comp, :start, :end, :summary, :stag, :now, modseq, modseq
			FROM account WHERE id = :a AND NOT EXISTS (`+sel("1")+`)`, kv...)
	}
	b.unique(ErrUIDConflict)
	b.add(`UPDATE dav_object SET uid = :uid, data = :data, etag = :etag, size = :size, component = :comp, start_at = :start,
			end_at = :end, summary = :summary, schedule_tag = :stag, modseq = `+sqlModseq+`
		WHERE account_id = :a AND id = (`+sel("id")+`) AND created_modseq <> `+sqlModseq, kv...)
	b.add(`UPDATE collection SET content_modseq = ` + sqlModseq + ` WHERE account_id = :a AND id = :c`)
	b.change(objectType(p.Kind), `id, CASE WHEN created_modseq = modseq THEN 'created' ELSE 'updated' END AS op FROM dav_object
		WHERE account_id = :a AND id = (`+sel("id")+`)`)
	b.add(`SELECT ` + objectColumns + ` FROM dav_object WHERE account_id = :a AND id = (` + sel("id") + `)`)
	res, err := s.run(ctx, b)
	if err != nil {
		return Object{}, err
	}
	o := scanObject(res[len(res)-1].Values[0], false)
	s.notifySharees(ctx, account, p.CollectionID)
	return o, nil
}

// DeleteObject removes an object, leaving a tombstone for DAV sync. ifMatch
// is the ETag it must have, or "".
func (s *Store) DeleteObject(ctx context.Context, account string, id int64, ifMatch string, ifState int64) (int64, error) {
	b := s.newBatch(account)
	b.base["id"] = id
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrNoObject, `EXISTS (SELECT 1 FROM dav_object WHERE account_id = :a AND id = :id)`)
	if ifMatch != "" {
		b.guard(ErrPrecondition, `(SELECT etag FROM dav_object WHERE account_id = :a AND id = :id) = :ifmatch`, "ifmatch", ifMatch)
	}
	b.bump()
	b.add(`INSERT OR REPLACE INTO dav_tombstone (account_id, collection_id, name, object_id, modseq, at)
		SELECT :a, collection_id, name, id, ` + sqlModseq + `, :now FROM dav_object WHERE account_id = :a AND id = :id`)
	b.add(`UPDATE collection SET content_modseq = ` + sqlModseq + ` WHERE account_id = :a
		AND id = (SELECT collection_id FROM dav_object WHERE account_id = :a AND id = :id)`)
	b.change("CalendarEvent", `id, 'destroyed' AS op FROM dav_object WHERE account_id = :a AND id = :id AND kind = 'event'`)
	b.change("ContactCard", `id, 'destroyed' AS op FROM dav_object WHERE account_id = :a AND id = :id AND kind = 'card'`)
	b.change("SchedulingMessage", `id, 'destroyed' AS op FROM dav_object WHERE account_id = :a AND id = :id AND kind = 'itip'`)
	b.add(`SELECT collection_id FROM dav_object WHERE account_id = :a AND id = :id`)
	b.add(`DELETE FROM dav_object WHERE account_id = :a AND id = :id`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	if v := res[len(res)-3].Values; len(v) > 0 {
		s.notifySharees(ctx, account, asInt(v[0][0]))
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// MoveObject moves an object to another collection of the same account,
// keeping its id; name is its name there ("" keeps it). The old place gets
// a tombstone.
func (s *Store) MoveObject(ctx context.Context, account string, id, to int64, name, ifMatch string, ifState int64) (Object, error) {
	cur, err := s.ObjectsByID(ctx, account, []int64{id})
	if err != nil {
		return Object{}, err
	}
	if len(cur) == 0 {
		return Object{}, ErrNoObject
	}
	b := s.newBatch(account)
	b.base["id"] = id
	b.base["to"] = to
	b.base["from"] = cur[0].CollectionID
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrNoObject, `EXISTS (SELECT 1 FROM dav_object WHERE account_id = :a AND id = :id AND collection_id = :from)`)
	b.guard(ErrNoCollection, `EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND id = :to
		AND kind = (SELECT CASE (SELECT kind FROM dav_object WHERE account_id = :a AND id = :id) WHEN 'card' THEN 'addressbook' ELSE 'calendar' END))`)
	if ifMatch != "" {
		b.guard(ErrPrecondition, `(SELECT etag FROM dav_object WHERE account_id = :a AND id = :id) = :ifmatch`, "ifmatch", ifMatch)
	}
	b.bump()
	b.add(`INSERT OR REPLACE INTO dav_tombstone (account_id, collection_id, name, object_id, modseq, at)
		SELECT :a, collection_id, name, id, ` + sqlModseq + `, :now FROM dav_object WHERE account_id = :a AND id = :id`)
	b.add(`UPDATE collection SET content_modseq = ` + sqlModseq + ` WHERE account_id = :a AND id IN (:to, :from)`)
	b.unique(ErrUIDConflict)
	b.add(`UPDATE dav_object SET collection_id = :to, name = CASE WHEN :name = '' THEN name ELSE :name END, modseq = `+sqlModseq+`
		WHERE account_id = :a AND id = :id`, "name", name)
	b.add(`DELETE FROM dav_tombstone WHERE account_id = :a AND collection_id = :to
		AND name = (SELECT name FROM dav_object WHERE account_id = :a AND id = :id)`)
	b.change(objectType(cur[0].Kind), `:id AS id, 'updated' AS op`)
	b.add(`SELECT ` + objectColumns + ` FROM dav_object WHERE account_id = :a AND id = :id`)
	res, err := s.run(ctx, b)
	if err != nil {
		return Object{}, err
	}
	s.notifySharees(ctx, account, cur[0].CollectionID)
	s.notifySharees(ctx, account, to)
	return scanObject(res[len(res)-1].Values[0], false), nil
}

// SyncChanges is what changed in a collection after a state: the objects
// changed or made since (without data), and the names of those removed or
// moved away (RFC 6578 3.5).
type SyncChanges struct {
	State   int64
	Changed []Object
	Removed []string
	// RemovedIDs are the ids of the removed objects, for JMAP.
	RemovedIDs []int64
}

// CollectionChanges answers a DAV sync-collection report, or a JMAP
// /changes of a sharee. since 0 is everything. A state older than what the
// tombstones reach gives ErrCannotCalculateChanges.
func (s *Store) CollectionChanges(ctx context.Context, account string, collections []int64, since int64) (SyncChanges, error) {
	list, _ := json.Marshal(collections)
	res, err := s.query(ctx, account,
		q(`SELECT modseq, trimmed_modseq FROM account WHERE id = :a`),
		q(`SELECT `+objectColumns+` FROM dav_object WHERE account_id = :a AND collection_id IN (SELECT value FROM json_each(:c))
			AND modseq > :since ORDER BY id`, "c", string(list), "since", since),
		q(`SELECT t.name, t.object_id FROM dav_tombstone t WHERE t.account_id = :a AND t.collection_id IN (SELECT value FROM json_each(:c))
			AND t.modseq > :since AND NOT EXISTS (SELECT 1 FROM dav_object o WHERE o.account_id = :a AND o.collection_id = t.collection_id AND o.name = t.name)
			AND NOT EXISTS (SELECT 1 FROM dav_object o WHERE o.account_id = :a AND o.id = t.object_id
				AND o.collection_id IN (SELECT value FROM json_each(:c)))
			ORDER BY t.modseq`, "c", string(list), "since", since))
	if err != nil {
		return SyncChanges{}, err
	}
	if len(res[0].Values) == 0 {
		return SyncChanges{}, ErrNoAccount
	}
	state, trimmed := asInt(res[0].Values[0][0]), asInt(res[0].Values[0][1])
	if since != 0 && (since < trimmed || since > state) {
		return SyncChanges{}, ErrCannotCalculateChanges
	}
	out := SyncChanges{State: state}
	for _, row := range res[1].Values {
		out.Changed = append(out.Changed, scanObject(row, false))
	}
	if since != 0 {
		seen := map[int64]bool{}
		for _, row := range res[2].Values {
			out.Removed = append(out.Removed, asString(row[0]))
			if id := asInt(row[1]); !seen[id] {
				seen[id] = true
				out.RemovedIDs = append(out.RemovedIDs, id)
			}
		}
	}
	return out, nil
}

// TrimTombstones forgets removals older than the change log keeps.
func (s *Store) TrimTombstones(ctx context.Context, before time.Time) (int64, error) {
	b := s.newBatch("")
	b.idempotent() // a delete by condition
	b.add(`DELETE FROM dav_tombstone WHERE at < :before`, "before", before.Unix())
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return res[0].RowsAffected, nil
}

// Share rights.
const (
	RightRead  = "read"
	RightWrite = "write"
)

// Share is one collection shared with one account.
type Share struct {
	Owner        string // account id
	OwnerAddress string
	CollectionID int64
	Kind         string
	Sharee       string
	ShareeAddr   string
	Rights       string
	Modseq       int64
}

// SetShare gives an account read or write access to a collection, or with
// rights "" takes it away. The owner's modseq moves (the collection's
// shareWith changed) and the sharee's epoch for the owner is set to it:
// a sharee whose state is older cannot be given changes and syncs anew.
func (s *Store) SetShare(ctx context.Context, owner string, collection int64, sharee, rights string) error {
	if rights != "" && rights != RightRead && rights != RightWrite {
		return errors.New("store: not a right: " + rights)
	}
	if sharee == owner {
		return ErrNoShare
	}
	c, err := s.CollectionByID(ctx, owner, collection)
	if err != nil {
		return err
	}
	if c.Kind == KindInbox {
		return ErrNoCollection
	}
	b := s.newBatch(owner)
	b.base["c"] = collection
	b.base["sharee"] = sharee
	b.guardAccount()
	b.guard(ErrNoShare, `EXISTS (SELECT 1 FROM account WHERE id = :sharee)`)
	b.guard(ErrNoCollection, `EXISTS (SELECT 1 FROM collection WHERE account_id = :a AND id = :c)`)
	b.bump()
	if rights == "" {
		b.add(`DELETE FROM collection_share WHERE account_id = :a AND collection_id = :c AND sharee = :sharee`)
	} else {
		b.add(`INSERT INTO collection_share (account_id, collection_id, sharee, rights, modseq) VALUES (:a, :c, :sharee, :rights, `+sqlModseq+`)
			ON CONFLICT (account_id, collection_id, sharee) DO UPDATE SET rights = excluded.rights, modseq = excluded.modseq`, "rights", rights)
	}
	b.add(`INSERT INTO share_epoch (account_id, sharee, modseq) VALUES (:a, :sharee, ` + sqlModseq + `)
		ON CONFLICT (account_id, sharee) DO UPDATE SET modseq = excluded.modseq`)
	b.add(`UPDATE collection SET modseq = ` + sqlModseq + ` WHERE account_id = :a AND id = :c`)
	b.change(collectionType(c.Kind), `:c AS id, 'updated' AS op`)
	if _, err := s.run(ctx, b); err != nil {
		return err
	}
	if s.OnChange != nil {
		s.OnChange(sharee)
	}
	return nil
}

const shareColumns = `s.account_id, o.address, s.collection_id, c.kind, s.sharee, e.address, s.rights, s.modseq`

func scanShare(row []any) Share {
	return Share{Owner: asString(row[0]), OwnerAddress: asString(row[1]), CollectionID: asInt(row[2]), Kind: asString(row[3]),
		Sharee: asString(row[4]), ShareeAddr: asString(row[5]), Rights: asString(row[6]), Modseq: asInt(row[7])}
}

const shareFrom = ` FROM collection_share s JOIN account o ON o.id = s.account_id JOIN account e ON e.id = s.sharee
	JOIN collection c ON c.account_id = s.account_id AND c.id = s.collection_id`

// SharesOf lists who an owner shares a collection with (collection 0: all
// of the owner's collections).
func (s *Store) SharesOf(ctx context.Context, owner string, collection int64) ([]Share, error) {
	res, err := s.query(ctx, owner, q(`SELECT `+shareColumns+shareFrom+` WHERE s.account_id = :a AND (:c = 0 OR s.collection_id = :c)
		ORDER BY s.collection_id, e.address`, "c", collection))
	if err != nil {
		return nil, err
	}
	var out []Share
	for _, row := range res[0].Values {
		out = append(out, scanShare(row))
	}
	return out, nil
}

// SharedWith lists the collections others share with an account.
func (s *Store) SharedWith(ctx context.Context, sharee string) ([]Share, error) {
	res, err := s.query(ctx, sharee, q(`SELECT `+shareColumns+shareFrom+` WHERE s.sharee = :a ORDER BY o.address, s.collection_id`))
	if err != nil {
		return nil, err
	}
	var out []Share
	for _, row := range res[0].Values {
		out = append(out, scanShare(row))
	}
	return out, nil
}

// ShareEpoch is the owner's modseq at the last change of what the owner
// shares with the sharee; 0 if never.
func (s *Store) ShareEpoch(ctx context.Context, owner, sharee string) (int64, error) {
	res, err := s.query(ctx, owner, q(`SELECT modseq FROM share_epoch WHERE account_id = :a AND sharee = :s`, "s", sharee))
	if err != nil {
		return 0, err
	}
	if len(res[0].Values) == 0 {
		return 0, nil
	}
	return asInt(res[0].Values[0][0]), nil
}

// notifySharees wakes the listeners of the accounts a collection is shared
// with: their view of the owner's account changed.
func (s *Store) notifySharees(ctx context.Context, owner string, collection int64) {
	if s.OnChange == nil {
		return
	}
	res, err := s.query(ctx, owner, q(`SELECT sharee FROM collection_share WHERE account_id = :a AND collection_id = :c`, "c", collection))
	if err != nil {
		return
	}
	for _, row := range res[0].Values {
		s.OnChange(asString(row[0]))
	}
}

// Principal is an account as JMAP Principals (RFC 9670) and DAV see it.
type Principal struct {
	ID      string
	Address string
}

// Principals lists the accounts, by address; with a text, those whose
// address contains it. At most limit.
func (s *Store) Principals(ctx context.Context, text string, limit int) ([]Principal, error) {
	res, err := s.query(ctx, "", q(`SELECT id, address FROM account WHERE `+sqlUserAccount+`
		AND (:t = '' OR instr(lower(address), lower(:t)) > 0) ORDER BY address LIMIT :l`, "t", text, "l", limit))
	if err != nil {
		return nil, err
	}
	var out []Principal
	for _, row := range res[0].Values {
		out = append(out, Principal{ID: asString(row[0]), Address: asString(row[1])})
	}
	return out, nil
}

// PrincipalByID returns one account as a principal.
func (s *Store) PrincipalByID(ctx context.Context, id string) (Principal, error) {
	res, err := s.query(ctx, id, q(`SELECT id, address FROM account WHERE id = :a`))
	if err != nil {
		return Principal{}, err
	}
	if len(res[0].Values) == 0 {
		return Principal{}, ErrNoAccount
	}
	return Principal{ID: asString(res[0].Values[0][0]), Address: asString(res[0].Values[0][1])}, nil
}
