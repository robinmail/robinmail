package jmap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/pim"
	"github.com/robinmail/robinmail/internal/store"
)

const usingPIM = `"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:calendars","urn:ietf:params:jmap:contacts",` +
	`"urn:ietf:params:jmap:principals","urn:ietf:params:jmap:principals:availability","urn:ietf:params:jmap:calendars:parse"]`

type mailOut struct {
	mu   sync.Mutex
	sent [][]byte
}

func newPIMEnv(t *testing.T) (*env, *mailOut) {
	e := newEnv(t)
	out := &mailOut{}
	e.h.PIM = &pim.Service{Store: e.st, Send: func(ctx context.Context, a store.Account, from string, rcpts []string, raw []byte) error {
		out.mu.Lock()
		defer out.mu.Unlock()
		out.sent = append(out.sent, raw)
		return nil
	}}
	return e, out
}

// other is a second user of the same server.
func (e *env) other() *env {
	var r [8]byte
	rand.Read(r[:])
	acct, err := e.st.CreateAccount(e.ctx, "jmap2-"+hex.EncodeToString(r[:])+"@example.test", "$argon2id$test")
	if err != nil {
		e.t.Fatal(err)
	}
	return &env{t: e.t, st: e.st, h: e.h, acct: acct, ctx: e.ctx, local: e.local, user: User{AccountID: acct.ID, Username: acct.Address}}
}

func (e *env) pim(name, args string) map[string]any {
	e.t.Helper()
	got := e.doWith(usingPIM + `,"methodCalls":[["` + name + `", ` + args + `, "c"]]`)
	if at(e.t, got, "/0/0") != name {
		e.t.Fatalf("%s %s answered %s", name, args, jsonOf(got))
	}
	return at(e.t, got, "/0/1").(map[string]any)
}

func (e *env) pimFail(name, args string) string {
	e.t.Helper()
	got := e.doWith(usingPIM + `,"methodCalls":[["` + name + `", ` + args + `, "c"]]`)
	if at(e.t, got, "/0/0") != "error" {
		e.t.Fatalf("%s %s did not fail: %s", name, args, jsonOf(got))
	}
	return at(e.t, got, "/0/1/type").(string)
}

func (e *env) acc() string { return `"accountId":"` + e.user.AccountID + `"` }

// JMAP Calendars 1.5 and RFC 9670 2: the capabilities in the session.
func TestJMAPCalendars_Session(t *testing.T) {
	e, _ := newPIMEnv(t)
	rec := e.get("/jmap/session")
	var s map[string]any
	json.Unmarshal(rec.Body.Bytes(), &s)
	for _, c := range []string{CapCalendars, CapContacts, CapPrincipals, CapPrincipalsAvail, CapCalendarsParse} {
		if _, ok := s["capabilities"].(map[string]any)[c]; !ok {
			t.Errorf("no session capability %s", c)
		}
	}
	ac := s["accounts"].(map[string]any)[e.user.AccountID].(map[string]any)["accountCapabilities"].(map[string]any)
	cal := ac[CapCalendars].(map[string]any)
	for _, k := range []string{"maxCalendarsPerEvent", "minDateTime", "maxDateTime", "maxExpandedQueryDuration", "maxParticipantsPerEvent", "mayCreateCalendar"} {
		if _, ok := cal[k]; !ok {
			t.Errorf("calendars account capability lacks %s", k)
		}
	}
	if ac[CapPrincipals].(map[string]any)["currentUserPrincipalId"] != e.user.AccountID {
		t.Errorf("principals: %v", ac[CapPrincipals])
	}
	if s["primaryAccounts"].(map[string]any)[CapCalendars] != e.user.AccountID {
		t.Error("no primary account for calendars")
	}
}

// JMAP Calendars 4 and 5: a calendar, an event in it with a recurrence,
// its instances by expandRecurrences, an instance changed and one removed
// through their ids, and the changes.
func TestJMAPCalendars_Events(t *testing.T) {
	e, _ := newPIMEnv(t)
	got := e.pim("Calendar/get", `{`+e.acc()+`}`)
	list := got["list"].([]any)
	if len(list) != 1 || at(t, list, "/0/isDefault") != true || at(t, list, "/0/myRights/mayDelete") != true {
		t.Fatalf("default calendar: %s", jsonOf(got))
	}
	state0 := got["state"].(string)
	set := e.pim("Calendar/set", `{`+e.acc()+`,"create":{"w":{"name":"Work","color":"#3366ff","timeZone":"Europe/Amsterdam"}}}`)
	work := at(t, set, "/created/w/id").(string)
	if at(t, set, "/created/w/isDefault") != false {
		t.Errorf("a second calendar is not the default: %s", jsonOf(set))
	}
	ch := e.pim("Calendar/changes", `{`+e.acc()+`,"sinceState":"`+state0+`"}`)
	if !strings.Contains(jsonOf(ch["created"]), work) {
		t.Errorf("Calendar/changes: %s", jsonOf(ch))
	}

	set = e.pim("CalendarEvent/set", `{`+e.acc()+`,"create":{"ev":{"calendarIds":{"`+work+`":true},"title":"Standup",
		"start":"2026-12-07T09:00:00","timeZone":"Europe/Amsterdam","duration":"PT15M",
		"recurrenceRule":{"@type":"RecurrenceRule","frequency":"daily","count":5},
		"locations":{"l1":{"@type":"Location","name":"Room 1"}},
		"alerts":{"a1":{"@type":"Alert","trigger":{"@type":"OffsetTrigger","offset":"-PT5M"}}}}}}`)
	id, ok := at(t, set, "/created/ev/id").(string)
	if !ok {
		t.Fatalf("create: %s", jsonOf(set))
	}
	uid := at(t, set, "/created/ev/uid").(string)
	ev := e.pim("CalendarEvent/get", `{`+e.acc()+`,"ids":["`+id+`"],"properties":["title","start","utcStart","utcEnd","recurrenceRule","alerts","uid"]}`)
	if at(t, ev, "/list/0/utcStart") != "2026-12-07T08:00:00Z" || at(t, ev, "/list/0/utcEnd") != "2026-12-07T08:15:00Z" ||
		at(t, ev, "/list/0/uid") != uid || at(t, ev, "/list/0/isOrigin") != true || at(t, ev, "/list/0/calendarIds/"+work) != true {
		t.Fatalf("get: %s", jsonOf(ev))
	}
	// 5.11: expandRecurrences gives an id per instance.
	q := e.pim("CalendarEvent/query", `{`+e.acc()+`,"filter":{"after":"2026-12-08T00:00:00","before":"2026-12-10T00:00:00"},
		"expandRecurrences":true,"timeZone":"Europe/Amsterdam"}`)
	ids := q["ids"].([]any)
	if len(ids) != 2 || !strings.HasPrefix(ids[0].(string), id+"_") {
		t.Fatalf("expanded: %s", jsonOf(q))
	}
	inst := ids[0].(string)
	g := e.pim("CalendarEvent/get", `{`+e.acc()+`,"ids":["`+inst+`"]}`)
	if at(t, g, "/list/0/baseEventId") != id || at(t, g, "/list/0/start") != "2026-12-08T09:00:00" || at(t, g, "/list/0/recurrenceRule") != nil {
		t.Fatalf("instance: %s", jsonOf(g))
	}
	// Move one instance, remove another.
	state1 := e.pim("CalendarEvent/get", `{`+e.acc()+`,"ids":[]}`)["state"].(string)
	set = e.pim("CalendarEvent/set", `{`+e.acc()+`,"update":{"`+inst+`":{"start":"2026-12-08T10:00:00","title":"Late standup"}},"destroy":["`+ids[1].(string)+`"]}`)
	if set["notUpdated"] != nil || set["notDestroyed"] != nil {
		t.Fatalf("instance set: %s", jsonOf(set))
	}
	full := e.pim("CalendarEvent/get", `{`+e.acc()+`,"ids":["`+id+`"],"properties":["recurrenceOverrides","sequence"]}`)
	ro := at(t, full, "/list/0/recurrenceOverrides").(map[string]any)
	if at(t, ro, "/2026-12-08T09:00:00/title") != "Late standup" || at(t, ro, "/2026-12-08T09:00:00/start") != "2026-12-08T10:00:00" ||
		at(t, ro, "/2026-12-09T09:00:00/excluded") != true {
		t.Fatalf("overrides: %s", jsonOf(full))
	}
	if n, _ := at(t, full, "/list/0/sequence").(float64); n < 1 {
		t.Errorf("the origin's change did not raise sequence: %s", jsonOf(full))
	}
	ch = e.pim("CalendarEvent/changes", `{`+e.acc()+`,"sinceState":"`+state1+`"}`)
	if !strings.Contains(jsonOf(ch["updated"]), id) || strings.Contains(jsonOf(ch), "_") {
		t.Errorf("changes name the base id only: %s", jsonOf(ch))
	}
	// Text and title filters; sort by start.
	e.pim("CalendarEvent/set", `{`+e.acc()+`,"create":{"b":{"calendarIds":{"`+work+`":true},"title":"Board meeting","start":"2026-12-01T14:00:00","timeZone":"Europe/Amsterdam","duration":"PT1H"}}}`)
	q = e.pim("CalendarEvent/query", `{`+e.acc()+`,"filter":{"text":"board"}}`)
	if len(q["ids"].([]any)) != 1 {
		t.Errorf("text: %s", jsonOf(q))
	}
	q = e.pim("CalendarEvent/query", `{`+e.acc()+`,"filter":{"inCalendar":"`+work+`"},"sort":[{"property":"start"}],"calculateTotal":true}`)
	if q["total"] != 2.0 || at(t, q, "/ids/1") != id {
		t.Errorf("sort: %s", jsonOf(q))
	}
	if e.pimFail("CalendarEvent/query", `{`+e.acc()+`,"filter":{"after":"2026-01-01T00:00:00","before":"2028-01-01T00:00:00"},"expandRecurrences":true}`) != "expandDurationTooLarge" {
		t.Error("a two-year expansion")
	}
	// The calendar with events cannot go unless asked (4.3).
	set = e.pim("Calendar/set", `{`+e.acc()+`,"destroy":["`+work+`"]}`)
	if at(t, set, "/notDestroyed/"+work+"/type") != "calendarHasEvent" {
		t.Errorf("destroy: %s", jsonOf(set))
	}
	set = e.pim("Calendar/set", `{`+e.acc()+`,"destroy":["`+work+`"],"onDestroyRemoveEvents":true}`)
	if set["destroyed"] == nil {
		t.Errorf("destroy with events: %s", jsonOf(set))
	}
}

// JMAP Calendars 5.9 and 7: an event with a participant of this server
// and one elsewhere, sent with sendSchedulingMessages; the attendee
// answers through their own CalendarEvent/set.
func TestJMAPCalendars_Scheduling(t *testing.T) {
	e, out := newPIMEnv(t)
	b := e.other()
	set := e.pim("CalendarEvent/set", `{`+e.acc()+`,"sendSchedulingMessages":true,"create":{"m":{"title":"Review",
		"start":"2026-12-10T15:00:00","timeZone":"Europe/Amsterdam","duration":"PT1H",
		"participants":{"o":{"@type":"Participant","calendarAddress":"mailto:`+e.user.Username+`","roles":{"owner":true,"attendee":true},"participationStatus":"accepted"},
			"b":{"@type":"Participant","calendarAddress":"mailto:`+b.user.Username+`","roles":{"attendee":true},"participationStatus":"needs-action","expectReply":true},
			"x":{"@type":"Participant","calendarAddress":"mailto:x@elsewhere.example","roles":{"attendee":true},"expectReply":true}}}}}`)
	id, ok := at(t, set, "/created/m/id").(string)
	if !ok {
		t.Fatalf("%s", jsonOf(set))
	}
	if len(out.sent) != 1 || !strings.Contains(string(out.sent[0]), "method=REQUEST") {
		t.Fatalf("mail out: %d", len(out.sent))
	}
	got := e.pim("CalendarEvent/get", `{`+e.acc()+`,"ids":["`+id+`"],"properties":["organizerCalendarAddress"]}`)
	if at(t, got, "/list/0/organizerCalendarAddress") != "mailto:"+e.user.Username {
		t.Errorf("organizer: %s", jsonOf(got))
	}
	q := b.pim("CalendarEvent/query", `{`+b.acc()+`}`)
	if len(q["ids"].([]any)) != 1 {
		t.Fatalf("the attendee's calendar: %s", jsonOf(q))
	}
	bid := at(t, q, "/ids/0").(string)
	bev := b.pim("CalendarEvent/get", `{`+b.acc()+`,"ids":["`+bid+`"]}`)
	if at(t, bev, "/list/0/isOrigin") != false {
		t.Errorf("an invitation is not origin: %s", jsonOf(bev))
	}
	var pid string
	for k, v := range at(t, bev, "/list/0/participants").(map[string]any) {
		if strings.EqualFold(v.(map[string]any)["calendarAddress"].(string), "mailto:"+b.user.Username) {
			pid = k
		}
	}
	b.pim("CalendarEvent/set", `{`+b.acc()+`,"sendSchedulingMessages":true,"update":{"`+bid+`":{"participants/`+pid+`/participationStatus":"accepted"}}}`)
	got = e.pim("CalendarEvent/get", `{`+e.acc()+`,"ids":["`+id+`"],"properties":["participants"]}`)
	if !strings.Contains(jsonOf(got), `"participationStatus":"accepted","roles":{"attendee":true}`) && !strings.Contains(jsonOf(got), "accepted") {
		t.Errorf("organizer's copy: %s", jsonOf(got))
	}
	accepted := 0
	for _, v := range at(t, got, "/list/0/participants").(map[string]any) {
		if v.(map[string]any)["participationStatus"] == "accepted" {
			accepted++
		}
	}
	if accepted != 2 {
		t.Errorf("accepted: %d %s", accepted, jsonOf(got))
	}
	// Free/busy of the attendee (2.2).
	av := e.pim("Principal/getAvailability", `{`+e.acc()+`,"id":"`+b.user.AccountID+`","utcStart":"2026-12-10T00:00:00Z","utcEnd":"2026-12-11T00:00:00Z"}`)
	if at(t, av, "/list/0/utcStart") != "2026-12-10T14:00:00Z" || at(t, av, "/list/0/event") != nil {
		t.Errorf("availability: %s", jsonOf(av))
	}
}

// RFC 9670 and JMAP Calendars 4: a calendar shared read-only shows up as
// another account of the sharee, read-only, with its events; sharing with
// write lets the sharee add one.
func TestJMAPCalendars_Sharing(t *testing.T) {
	e, _ := newPIMEnv(t)
	b := e.other()
	cals := e.pim("Calendar/get", `{`+e.acc()+`}`)
	cal := at(t, cals, "/list/0/id").(string)
	e.pim("CalendarEvent/set", `{`+e.acc()+`,"create":{"x":{"title":"Shared","start":"2026-12-01T10:00:00","duration":"PT1H"}}}`)
	set := e.pim("Calendar/set", `{`+e.acc()+`,"update":{"`+cal+`":{"shareWith":{"`+b.user.AccountID+`":{"mayReadItems":true,"mayReadFreeBusy":true}}}}}`)
	if set["notUpdated"] != nil {
		t.Fatalf("%s", jsonOf(set))
	}
	rec := b.get("/jmap/session")
	var s map[string]any
	json.Unmarshal(rec.Body.Bytes(), &s)
	shared, ok := s["accounts"].(map[string]any)[e.user.AccountID].(map[string]any)
	if !ok || shared["isPersonal"] != false || shared["isReadOnly"] != true {
		t.Fatalf("the sharee's session: %s", rec.Body.String())
	}
	other := `"accountId":"` + e.user.AccountID + `"`
	g := b.pim("Calendar/get", `{`+other+`}`)
	if at(t, g, "/list/0/myRights/mayWriteAll") != false || at(t, g, "/list/0/shareWith") != nil {
		t.Errorf("as a sharee: %s", jsonOf(g))
	}
	q := b.pim("CalendarEvent/query", `{`+other+`}`)
	if len(q["ids"].([]any)) != 1 {
		t.Errorf("the shared event: %s", jsonOf(q))
	}
	set = b.pim("CalendarEvent/set", `{`+other+`,"create":{"y":{"title":"Mine","start":"2026-12-02T10:00:00"}}}`)
	if at(t, set, "/notCreated/y/type") != "forbidden" {
		t.Errorf("read-only create: %s", jsonOf(set))
	}
	state := q["queryState"].(string)
	e.pim("Calendar/set", `{`+e.acc()+`,"update":{"`+cal+`":{"shareWith":{"`+b.user.AccountID+`":{"mayReadItems":true,"mayWriteAll":true}}}}}`)
	if typ := b.pimFail("CalendarEvent/changes", `{`+other+`,"sinceState":"`+state+`"}`); typ != "cannotCalculateChanges" {
		t.Errorf("changes across a change of the share: %s", typ)
	}
	state = b.pim("CalendarEvent/get", `{`+other+`,"ids":[]}`)["state"].(string)
	set = b.pim("CalendarEvent/set", `{`+other+`,"create":{"y":{"title":"Mine","start":"2026-12-02T10:00:00"}}}`)
	if set["created"] == nil {
		t.Fatalf("write create: %s", jsonOf(set))
	}
	ch := b.pim("CalendarEvent/changes", `{`+other+`,"sinceState":"`+state+`"}`)
	if len(ch["created"].([]any)) != 1 {
		t.Errorf("sharee's changes: %s", jsonOf(ch))
	}
	// A stranger has no such account.
	if typ := e.other().pimFail("Calendar/get", `{`+other+`}`); typ != "accountNotFound" {
		t.Errorf("stranger: %s", typ)
	}
	// Principals: B finds A by address.
	pq := b.pim("Principal/query", `{`+b.acc()+`,"filter":{"email":"`+e.user.Username+`"}}`)
	if at(t, pq, "/ids/0") != e.user.AccountID {
		t.Errorf("principal query: %s", jsonOf(pq))
	}
	pg := b.pim("Principal/get", `{`+b.acc()+`,"ids":["`+e.user.AccountID+`"]}`)
	if at(t, pg, "/list/0/capabilities/"+strings.ReplaceAll(CapCalendars, "/", "~1")+"/accountId") != e.user.AccountID {
		t.Errorf("principal: %s", jsonOf(pg))
	}
}

// RFC 9610: address books and cards, a query by name and email, an
// update by patch, and a card through vCard (the stored form) and back.
func TestRFC9610_Contacts(t *testing.T) {
	e, _ := newPIMEnv(t)
	books := e.pim("AddressBook/get", `{`+e.acc()+`}`)
	book := at(t, books, "/list/0/id").(string)
	if at(t, books, "/list/0/myRights/mayWrite") != true {
		t.Fatalf("%s", jsonOf(books))
	}
	set := e.pim("ContactCard/set", `{`+e.acc()+`,"create":{
		"a":{"addressBookIds":{"`+book+`":true},"name":{"components":[{"kind":"given","value":"Jane"},{"kind":"surname","value":"Doe"}]},
			"emails":{"e1":{"@type":"EmailAddress","address":"jane@example.com","contexts":{"work":true}}},
			"phones":{"p1":{"@type":"Phone","number":"+31 20 1234567","features":{"mobile":true}}}},
		"b":{"name":{"full":"Zed Alpha"},"organizations":{"o":{"@type":"Organization","name":"Acme"}}}}}`)
	a, ok := at(t, set, "/created/a/id").(string)
	if !ok || at(t, set, "/created/b/id") == nil {
		t.Fatalf("%s", jsonOf(set))
	}
	q := e.pim("ContactCard/query", `{`+e.acc()+`,"filter":{"email":"jane@"}}`)
	if at(t, q, "/ids/0") != a || len(q["ids"].([]any)) != 1 {
		t.Errorf("email: %s", jsonOf(q))
	}
	q = e.pim("ContactCard/query", `{`+e.acc()+`,"filter":{"operator":"OR","conditions":[{"organization":"acme"},{"name/surname":"doe"}]},"sort":[{"property":"name/given"}]}`)
	if len(q["ids"].([]any)) != 2 {
		t.Errorf("OR: %s", jsonOf(q))
	}
	state := q["queryState"].(string)
	set = e.pim("ContactCard/set", `{`+e.acc()+`,"update":{"`+a+`":{"name/components/1/value":"Smith"}}}`)
	if set["notUpdated"] != nil {
		// A path into an array is not a PatchObject path (RFC 8620 5.3);
		// the whole name is replaced instead.
		set = e.pim("ContactCard/set", `{`+e.acc()+`,"update":{"`+a+`":{"name":{"components":[{"kind":"given","value":"Jane"},{"kind":"surname","value":"Smith"}]}}}}`)
	}
	if set["updated"] == nil {
		t.Fatalf("update: %s", jsonOf(set))
	}
	g := e.pim("ContactCard/get", `{`+e.acc()+`,"ids":["`+a+`"]}`)
	if !strings.Contains(jsonOf(g), "Smith") || at(t, g, "/list/0/addressBookIds/"+book) != true {
		t.Errorf("get: %s", jsonOf(g))
	}
	ch := e.pim("ContactCard/changes", `{`+e.acc()+`,"sinceState":"`+state+`"}`)
	if at(t, ch, "/updated/0") != a {
		t.Errorf("changes: %s", jsonOf(ch))
	}
	// The stored form is a vCard a CardDAV client reads.
	id, _ := store.ParseContactCardID(a)
	objs, _ := e.st.ObjectsByID(e.ctx, e.user.AccountID, []int64{id})
	if len(objs) != 1 || !strings.Contains(objs[0].Data, "N:Smith;Jane;;;") || !strings.Contains(objs[0].Data, "TEL;TYPE=cell:+31 20 1234567") {
		t.Errorf("vCard:\n%s", objs[0].Data)
	}
	set = e.pim("ContactCard/set", `{`+e.acc()+`,"destroy":["`+a+`"]}`)
	if set["destroyed"] == nil {
		t.Errorf("destroy: %s", jsonOf(set))
	}
}

// JMAP Calendars 5.13: CalendarEvent/parse of an uploaded invitation.
func TestJMAPCalendars_Parse(t *testing.T) {
	e, _ := newPIMEnv(t)
	blob := e.upload([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:p1\r\nDTSTAMP:20260101T000000Z\r\n"+
		"DTSTART:20260105T100000Z\r\nDURATION:PT30M\r\nSUMMARY:Lunch\r\nORGANIZER:mailto:o@elsewhere.example\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), "text/calendar")
	got := e.pim("CalendarEvent/parse", `{`+e.acc()+`,"blobIds":["`+blob+`","nope"]}`)
	if at(t, got, "/parsed/"+blob+"/0/title") != "Lunch" || at(t, got, "/parsed/"+blob+"/0/method") != "request" || at(t, got, "/notFound/0") != "nope" {
		t.Errorf("%s", jsonOf(got))
	}
	_ = time.Now
}
