package store

import (
	"errors"
	"testing"
	"time"
)

func put(t *testing.T, s *Store, a Account, p PutObject) Object {
	t.Helper()
	if p.Kind == "" {
		p.Kind = ObjectEvent
	}
	o, err := s.Put(ctx, a.ID, p)
	if err != nil {
		t.Fatalf("put %s: %v", p.Name, err)
	}
	return o
}

func TestCollectionsAndObjects(t *testing.T) {
	s := open(t)
	a := account(t, s)
	if err := s.EnsureCollections(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	// Twice changes nothing.
	before := state(t, s, a)
	if err := s.EnsureCollections(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	want(t, "state after a second EnsureCollections", state(t, s, a), before)
	cals, err := s.Collections(ctx, a.ID, KindCalendar)
	if err != nil || len(cals) != 1 || !cals[0].IsDefault || cals[0].Name != "Calendar" {
		t.Fatalf("calendars %+v %v", cals, err)
	}
	books, _ := s.Collections(ctx, a.ID, KindAddressBook)
	inbox, _ := s.Collections(ctx, a.ID, KindInbox)
	if len(books) != 1 || len(inbox) != 1 {
		t.Fatalf("books %d inbox %d", len(books), len(inbox))
	}
	cal := cals[0].ID
	c := changes(t, s, a, 0)
	if c.Other["Calendar"] == nil || len(c.Other["Calendar"].Created) != 1 || c.Other["AddressBook"] == nil {
		t.Errorf("changes %+v", c.Other)
	}

	start, end := time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC).Unix(), time.Date(2026, 12, 1, 11, 0, 0, 0, time.UTC).Unix()
	since := state(t, s, a)
	one := put(t, s, a, PutObject{CollectionID: cal, Name: "u1.ics", UID: "u1", Data: "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n",
		Component: "VEVENT", Start: &start, End: &end, Summary: "Meeting", IfNoneMatch: true})
	if one.ID == 0 || one.ETag != ETag("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n") || one.Created != one.Modseq {
		t.Fatalf("created %+v", one)
	}
	c = changes(t, s, a, since)
	want(t, "created", c.Other["CalendarEvent"].Created, []int64{one.ID})

	// RFC 4918 9.7.2 / RFC 7232: If-None-Match: * refuses an existing
	// resource; a wrong If-Match refuses too.
	_, err = s.Put(ctx, a.ID, PutObject{CollectionID: cal, Name: "u1.ics", Kind: ObjectEvent, UID: "u1", Data: "x", IfNoneMatch: true})
	wantErr(t, "If-None-Match on an existing object", err, ErrPrecondition)
	_, err = s.Put(ctx, a.ID, PutObject{CollectionID: cal, Name: "u1.ics", Kind: ObjectEvent, UID: "u1", Data: "x", IfMatch: `"nope"`})
	wantErr(t, "a wrong If-Match", err, ErrPrecondition)
	// RFC 4791 5.3.2.1: no two objects of one UID in a calendar.
	_, err = s.Put(ctx, a.ID, PutObject{CollectionID: cal, Name: "other.ics", Kind: ObjectEvent, UID: "u1", Data: "x"})
	wantErr(t, "a second object with the UID", err, ErrUIDConflict)

	since = state(t, s, a)
	two := put(t, s, a, PutObject{CollectionID: cal, Name: "u1.ics", UID: "u1", Data: "BEGIN:VCALENDAR\r\nX:1\r\nEND:VCALENDAR\r\n", IfMatch: one.ETag})
	want(t, "an update keeps the id", two.ID, one.ID)
	c = changes(t, s, a, since)
	want(t, "updated", c.Other["CalendarEvent"].Updated, []int64{one.ID})
	// By id (JMAP).
	put(t, s, a, PutObject{CollectionID: cal, ID: one.ID, UID: "u1", Data: "y", Summary: "Renamed", Start: &start, End: &end})
	got, err := s.ObjectByName(ctx, a.ID, cal, "u1.ics")
	if err != nil || got.Data != "y" || got.Summary != "Renamed" {
		t.Fatalf("%+v %v", got, err)
	}

	// The quota counts objects like messages.
	q, _ := s.Quota(ctx, a.ID)
	want(t, "quota", []int64{q.UsedMessages, q.UsedBytes}, []int64{1, 1})

	// Time range: by the span columns.
	from, to := time.Date(2026, 12, 1, 10, 30, 0, 0, time.UTC), time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	in, _ := s.Objects(ctx, a.ID, ObjectFilter{Collections: []int64{cal}, From: &from, To: &to})
	want(t, "in range", len(in), 1)
	late := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	out, _ := s.Objects(ctx, a.ID, ObjectFilter{Collections: []int64{cal}, From: &late})
	want(t, "out of range", len(out), 0)

	// Sync: a token, a change, a removal (RFC 6578 3.5).
	sync0, err := s.CollectionChanges(ctx, a.ID, []int64{cal}, 0)
	if err != nil || len(sync0.Changed) != 1 {
		t.Fatalf("initial sync %+v %v", sync0, err)
	}
	three := put(t, s, a, PutObject{CollectionID: cal, Name: "u3.ics", UID: "u3", Data: "z"})
	if _, err := s.DeleteObject(ctx, a.ID, one.ID, `"wrong"`, 0); !errors.Is(err, ErrPrecondition) {
		t.Errorf("delete with a wrong etag: %v", err)
	}
	if _, err := s.DeleteObject(ctx, a.ID, one.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	sync1, err := s.CollectionChanges(ctx, a.ID, []int64{cal}, sync0.State)
	if err != nil || len(sync1.Changed) != 1 || sync1.Changed[0].ID != three.ID || len(sync1.Removed) != 1 || sync1.Removed[0] != "u1.ics" {
		t.Fatalf("sync %+v %v", sync1, err)
	}
	q, _ = s.Quota(ctx, a.ID)
	want(t, "quota after delete", q.UsedMessages, int64(1))

	// A move leaves a tombstone where the object was.
	other, err := s.CreateCollection(ctx, a.ID, KindCalendar, CollectionUpdate{Name: strp("Work")}, 0)
	if err != nil || other.IsDefault {
		t.Fatalf("%+v %v", other, err)
	}
	if _, err := s.MoveObject(ctx, a.ID, three.ID, books[0].ID, "", "", 0); !errors.Is(err, ErrNoCollection) {
		t.Errorf("an event into an address book: %v", err)
	}
	moved, err := s.MoveObject(ctx, a.ID, three.ID, other.ID, "", "", 0)
	if err != nil || moved.CollectionID != other.ID || moved.ID != three.ID {
		t.Fatalf("move %+v %v", moved, err)
	}
	sync2, _ := s.CollectionChanges(ctx, a.ID, []int64{cal}, sync1.State)
	want(t, "moved away", sync2.Removed, []string{"u3.ics"})
	sync3, _ := s.CollectionChanges(ctx, a.ID, []int64{other.ID}, sync1.State)
	want(t, "moved in", len(sync3.Changed), 1)

	// Destroying: not the default, not with contents unless asked.
	_, err = s.DestroyCollection(ctx, a.ID, cal, true, 0)
	wantErr(t, "destroy the default", err, ErrDefaultCollection)
	_, err = s.DestroyCollection(ctx, a.ID, other.ID, false, 0)
	wantErr(t, "destroy a calendar with events", err, ErrCollectionInUse)
	if _, err := s.UpdateCollection(ctx, a.ID, other.ID, CollectionUpdate{MakeDefault: true, Props: map[string]string{"{x:}y": "z"}}, 0); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.CollectionByID(ctx, a.ID, other.ID)
	if !got2.IsDefault || got2.Props["{x:}y"] != "z" {
		t.Errorf("%+v", got2)
	}
	if _, err := s.DestroyCollection(ctx, a.ID, cal, false, 0); err != nil {
		t.Fatal(err)
	}
	q, _ = s.Quota(ctx, a.ID)
	want(t, "quota", q.UsedMessages, int64(1))
	if changed, err := s.RecountQuota(ctx, a.ID); err != nil || changed {
		t.Errorf("recount: %v %v", changed, err)
	}
}

func strp(s string) *string { return &s }

func TestShares(t *testing.T) {
	s := open(t)
	owner, sharee := account(t, s), account(t, s)
	s.EnsureCollections(ctx, owner.ID)
	cals, _ := s.Collections(ctx, owner.ID, KindCalendar)
	cal := cals[0].ID
	woken := map[string]int{}
	s.OnChange = func(a string) { woken[a]++ }
	defer func() { s.OnChange = nil }()
	if err := s.SetShare(ctx, owner.ID, cal, sharee.ID, RightRead); err != nil {
		t.Fatal(err)
	}
	if woken[sharee.ID] == 0 {
		t.Error("the sharee was not told")
	}
	list, err := s.SharedWith(ctx, sharee.ID)
	if err != nil || len(list) != 1 || list[0].Owner != owner.ID || list[0].Rights != RightRead || list[0].OwnerAddress != owner.Address {
		t.Fatalf("%+v %v", list, err)
	}
	epoch, _ := s.ShareEpoch(ctx, owner.ID, sharee.ID)
	want(t, "epoch", epoch, state(t, s, owner))
	woken = map[string]int{}
	put(t, s, owner, PutObject{CollectionID: cal, Name: "a.ics", UID: "a", Data: "x"})
	if woken[sharee.ID] == 0 {
		t.Error("the sharee was not told of a new event")
	}
	if err := s.SetShare(ctx, owner.ID, cal, sharee.ID, RightWrite); err != nil {
		t.Fatal(err)
	}
	of, _ := s.SharesOf(ctx, owner.ID, 0)
	want(t, "upgraded", of[0].Rights, RightWrite)
	wantErr(t, "share with oneself", s.SetShare(ctx, owner.ID, cal, owner.ID, RightRead), ErrNoShare)
	wantErr(t, "share with nobody", s.SetShare(ctx, owner.ID, cal, "nobody", RightRead), ErrNoShare)
	if err := s.SetShare(ctx, owner.ID, cal, sharee.ID, ""); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.SharedWith(ctx, sharee.ID); len(list) != 0 {
		t.Errorf("still shared %+v", list)
	}
	ps, err := s.Principals(ctx, sharee.Address[:5], 10)
	if err != nil || len(ps) == 0 {
		t.Errorf("principals %v %v", ps, err)
	}
	// Removing the sharee's account removes what was shared with it.
	s.SetShare(ctx, owner.ID, cal, sharee.ID, RightRead)
	if err := s.RemoveAccount(ctx, sharee.ID); err != nil {
		t.Fatal(err)
	}
	if of, _ := s.SharesOf(ctx, owner.ID, 0); len(of) != 0 {
		t.Errorf("share of a removed account %+v", of)
	}
}
