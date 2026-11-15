package jscal

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
)

var now = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func mustJS(t *testing.T, s string) Event {
	t.Helper()
	var ev Event
	if err := json.Unmarshal([]byte(s), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// roundTrip takes JSCalendar to iCalendar and back, and checks that what
// comes back is what went in (Normalize makes Go's ints JSON's numbers).
func roundTrip(t *testing.T, ev Event) (*ical.Component, Event) {
	t.Helper()
	cal, err := ToICal(ev, nil, "-//robinmail//test//EN", now)
	if err != nil {
		t.Fatal(err)
	}
	text := ical.Encode(cal)
	back, err := ical.ParseOne(text, "VCALENDAR")
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	js := Normalize(ToJS(Read(back, nil))).(map[string]any)
	delete(js, "prodId")
	if _, given := ev["updated"]; !given {
		delete(js, "updated") // DTSTAMP, the time of the conversion
	}
	return back, js
}

// RFC 8984 6.1, a simple event, through iCalendar and back.
func TestRFC8984_6_1_SimpleEvent(t *testing.T) {
	ev := mustJS(t, `{"@type":"Event","uid":"a8df6573-0474-496d-8496-033ad45d7fea","updated":"2020-01-02T18:23:04Z",
		"title":"Some event","start":"2020-01-15T13:00:00","timeZone":"America/New_York","duration":"PT1H"}`)
	cal, js := roundTrip(t, ev)
	if !reflect.DeepEqual(js, ev) {
		t.Fatalf("got  %v\nwant %v", js, ev)
	}
	vev := cal.Sub("VEVENT")[0]
	if p := vev.Get("DTSTART"); p.Param("TZID") != "America/New_York" || p.Value != "20200115T130000" {
		t.Errorf("DTSTART %+v", p)
	}
	if len(cal.Sub("VTIMEZONE")) != 1 {
		t.Error("no VTIMEZONE for the TZID (RFC 5545 3.6.5)")
	}
}

// RFC 8984 6.2: an all-day event (showWithoutTime) is a DATE in iCalendar.
func TestRFC8984_6_2_AllDayEvent(t *testing.T) {
	ev := mustJS(t, `{"@type":"Event","uid":"a8df6573-0474-496d-8496-033ad45d7fea","title":"April Fool's Day",
		"showWithoutTime":true,"start":"1900-04-01T00:00:00","duration":"P1D",
		"recurrenceRule":{"@type":"RecurrenceRule","frequency":"yearly"}}`)
	cal, js := roundTrip(t, ev)
	if !reflect.DeepEqual(js, ev) {
		t.Fatalf("got  %v\nwant %v", js, ev)
	}
	v := cal.Sub("VEVENT")[0]
	if v.Value("DTSTART") != "19000401" || v.Value("DTEND") != "19000402" || v.Get("DTSTART").Param("VALUE") != "DATE" {
		t.Errorf("%s", ical.Encode(v))
	}
}

// RFC 8984 6.4, a recurring event with overrides (in JSCalendar 2.0 the
// rule is recurrenceRule): an extra instance, an excluded one, and one
// that moves.
func TestRFC8984_6_4_RecurringEventWithOverrides(t *testing.T) {
	ev := mustJS(t, `{"@type":"Event","uid":"22e8b8d2-a76a-4d7a-9ba1-0f6c5e0f4b2f","title":"Calculus I",
		"start":"2020-01-08T09:00:00","timeZone":"Europe/London","duration":"PT1H30M",
		"locations":{"`+shortID("loc", "Math lab room 1")+`":{"@type":"Location","name":"Math lab room 1"}},
		"recurrenceRule":{"@type":"RecurrenceRule","frequency":"weekly","until":"2020-06-24T09:00:00"},
		"recurrenceOverrides":{
			"2020-01-07T14:00:00":{"title":"Introduction to Calculus I (optional)"},
			"2020-04-01T09:00:00":{"excluded":true},
			"2020-06-25T09:00:00":{"title":"Calculus I Exam","start":"2020-06-25T10:00:00","duration":"PT2H"}}}`)
	cal, js := roundTrip(t, ev)
	if !reflect.DeepEqual(js, ev) {
		a, _ := json.MarshalIndent(js, "", " ")
		t.Fatalf("got %s\n%s", a, ical.Encode(cal))
	}
	o := Read(cal, nil)
	london, _ := time.LoadLocation("Europe/London")
	from, to := time.Date(2020, 1, 1, 0, 0, 0, 0, london), time.Date(2020, 7, 1, 0, 0, 0, 0, london)
	inst, complete := o.Instances(from, to, 100)
	if !complete {
		t.Fatal("not complete")
	}
	// 25 weekly instances from 8 January to 24 June, one excluded, plus the
	// extra one on 7 January and the exam on 25 June.
	if len(inst) != 26 {
		t.Fatalf("%d instances", len(inst))
	}
	if inst[0].Start.Day() != 7 || inst[0].Component.Text("SUMMARY") != "Introduction to Calculus I (optional)" {
		t.Errorf("first %v %s", inst[0].Start, inst[0].Component.Text("SUMMARY"))
	}
	last := inst[len(inst)-1]
	if !last.Start.Equal(time.Date(2020, 6, 25, 10, 0, 0, 0, london)) || last.End.Sub(last.Start) != 2*time.Hour {
		t.Errorf("exam %v %v", last.Start, last.End)
	}
	for _, i := range inst {
		if i.Start.Month() == 4 && i.Start.Day() == 1 {
			t.Error("the excluded instance is there")
		}
	}
	start, end, forever := o.Span()
	if forever || start.Day() != 7 || !end.Equal(last.End) {
		t.Errorf("span %v %v %v", start, end, forever)
	}
}

// RFC 8984 6.5 and 6.6, participants and alerts; JSCalendar 2.0 names the
// address of a participant calendarAddress.
func TestRFC8984_6_6_Participants(t *testing.T) {
	org, att := shortID("p", "mailto:organizer@example.com"), shortID("p", "mailto:attendee@example.com")
	ev := mustJS(t, `{"@type":"Event","uid":"2a358cee-6489-4f14-a57f-c104db4dc2f2","title":"Team meeting",
		"start":"2026-12-01T10:00:00","timeZone":"Europe/Amsterdam","duration":"PT1H","sequence":2,
		"organizerCalendarAddress":"mailto:organizer@example.com",
		"participants":{
			"`+org+`":{"@type":"Participant","name":"Organizer","email":"organizer@example.com","calendarAddress":"mailto:organizer@example.com",
				"roles":{"owner":true,"attendee":true,"chair":true},"participationStatus":"accepted"},
			"`+att+`":{"@type":"Participant","email":"attendee@example.com","calendarAddress":"mailto:attendee@example.com",
				"roles":{"attendee":true,"optional":true},"participationStatus":"tentative","expectReply":true}},
		"alerts":{"a1":{"@type":"Alert","trigger":{"@type":"OffsetTrigger","offset":"-PT15M"}}},
		"freeBusyStatus":"free","privacy":"private","status":"tentative","keywords":{"work":true,"team":true}}`)
	cal, js := roundTrip(t, ev)
	if !reflect.DeepEqual(js, ev) {
		a, _ := json.MarshalIndent(js, "", " ")
		t.Fatalf("got %s\n%s", a, ical.Encode(cal))
	}
	v := cal.Sub("VEVENT")[0]
	if v.Value("ORGANIZER") != "mailto:organizer@example.com" || len(v.All("ATTENDEE")) != 2 {
		t.Fatalf("%s", ical.Encode(v))
	}
	for _, a := range v.All("ATTENDEE") {
		if a.Value == "mailto:attendee@example.com" && (a.Param("PARTSTAT") != "TENTATIVE" || a.Param("ROLE") != "OPT-PARTICIPANT" || a.Param("RSVP") != "TRUE") {
			t.Errorf("attendee %+v", a)
		}
	}
	if v.Value("TRANSP") != "TRANSPARENT" || v.Value("CLASS") != "PRIVATE" || len(v.Sub("VALARM")) != 1 {
		t.Errorf("%s", ical.Encode(v))
	}
}

// What JSCalendar does not know survives an update: the X- properties
// and the parameters a client put there stay (draft-ietf-calext-jscalendar-
// icalendar keeps them in iCalendar's own form; robinmail keeps the stored
// iCalendar).
func TestUnknownPropertiesSurvive(t *testing.T) {
	in := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Apple Inc.//iCal//EN\r\nX-WR-CALNAME:Work\r\nBEGIN:VEVENT\r\nUID:u1\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART;TZID=Europe/Amsterdam:20260105T090000\r\nDTEND;TZID=Europe/Amsterdam:20260105T100000\r\nSUMMARY:Standup\r\n" +
		"X-APPLE-TRAVEL-ADVISORY-BEHAVIOR:AUTOMATIC\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	prev, err := ical.ParseOne([]byte(in), "VCALENDAR")
	if err != nil {
		t.Fatal(err)
	}
	ev := ToJS(Read(prev, nil))
	if ev["duration"] != "PT1H" || ev["start"] != "2026-01-05T09:00:00" || ev["timeZone"] != "Europe/Amsterdam" {
		t.Fatalf("%v", ev)
	}
	ev, _ = ApplyPatch(ev, map[string]any{"title": "Daily standup"})
	cal, err := ToICal(ev, prev, "-//robinmail//test//EN", now)
	if err != nil {
		t.Fatal(err)
	}
	out := string(ical.Encode(cal))
	for _, want := range []string{"X-WR-CALNAME:Work", "X-APPLE-TRAVEL-ADVISORY-BEHAVIOR:AUTOMATIC", "SUMMARY:Daily standup", "DURATION:PT1H"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
}

// RFC 8620 5.3: a PatchObject's paths and null.
func TestPatchObject(t *testing.T) {
	ev := Event{"title": "a", "participants": map[string]any{"p1": map[string]any{"participationStatus": "needs-action", "name": "x"}}}
	out, err := ApplyPatch(ev, map[string]any{"participants/p1/participationStatus": "accepted", "participants/p1/name": nil, "title": "b"})
	if err != nil {
		t.Fatal(err)
	}
	p := out["participants"].(map[string]any)["p1"].(map[string]any)
	if p["participationStatus"] != "accepted" || p["name"] != nil || out["title"] != "b" {
		t.Fatalf("%v", out)
	}
	if ev["title"] != "a" {
		t.Fatal("the original changed")
	}
	if _, err := ApplyPatch(ev, map[string]any{"nothere/x": 1}); err == nil {
		t.Error("a path through nothing")
	}
	if _, err := ApplyPatch(ev, map[string]any{"participants": map[string]any{}, "participants/p1/name": "y"}); err == nil {
		t.Error("a path inside another")
	}
}

// Invalid JSCalendar is refused with the property named.
func TestInvalidProperties(t *testing.T) {
	for prop, js := range map[string]string{
		"start":                          `{"@type":"Event","uid":"u","start":"2026-13-01T00:00:00"}`,
		"timeZone":                       `{"@type":"Event","uid":"u","start":"2026-01-01T00:00:00","timeZone":"Mars/Olympus"}`,
		"duration":                       `{"@type":"Event","uid":"u","start":"2026-01-01T00:00:00","duration":"1 hour"}`,
		"recurrenceRule/frequency":       `{"@type":"Event","uid":"u","start":"2026-01-01T00:00:00","recurrenceRule":{"frequency":"often"}}`,
		"uid":                            `{"@type":"Event","start":"2026-01-01T00:00:00"}`,
		"participants/x/calendarAddress": `{"@type":"Event","uid":"u","start":"2026-01-01T00:00:00","participants":{"x":{"name":"n"}}}`,
	} {
		_, err := ToICal(mustJS(t, js), nil, "p", now)
		pe, ok := err.(*PropError)
		if !ok || pe.Props[0] != prop {
			t.Errorf("%s: %v", prop, err)
		}
	}
}

// RFC 4791 9.9: time-range overlap, with the zero-length and floating
// cases.
func TestRFC4791_9_9_Overlap(t *testing.T) {
	in := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:x\r\nDTSTART:20260101T100000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	cal, _ := ical.ParseOne([]byte(in), "VCALENDAR")
	ams, _ := time.LoadLocation("Europe/Amsterdam")
	o := Read(cal, ams) // floating, read in the calendar's zone
	at := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	if got, _ := o.Instances(at, at.Add(time.Minute), 10); len(got) != 1 {
		t.Errorf("a zero-length event at the start of the range: %d", len(got))
	}
	if got, _ := o.Instances(at.Add(-time.Hour), at, 10); len(got) != 0 {
		t.Errorf("a zero-length event at the end of the range: %d", len(got))
	}
}

// draft-ietf-calext-jscalendar-icalendar 2.2.4 and 2.3.25: one location
// is a LOCATION, with a JSID parameter when its key is not the derived
// one; more are a VLOCATION each, keyed by UID, with a
// LOCATION;DERIVED=TRUE that names the main one. Both come back as they
// went in.
func TestJSCalICal_Locations(t *testing.T) {
	one := mustJS(t, `{"@type":"Event","uid":"loc-1","title":"x","start":"2026-11-20T10:00:00","duration":"PT1H",
		"locations":{"loc1":{"@type":"Location","name":"Taj Mahal","coordinates":"geo:27.175,78.041944"}}}`)
	cal, js := roundTrip(t, one)
	if !reflect.DeepEqual(js["locations"], one["locations"]) {
		t.Errorf("one location: got %v", js["locations"])
	}
	vev := cal.Sub("VEVENT")[0]
	if p := vev.Get("LOCATION"); p == nil || p.Param("JSID") != "loc1" || p.Text() != "Taj Mahal" {
		t.Errorf("LOCATION %+v", p)
	}
	if p := vev.Get("GEO"); p == nil || p.Value != "27.175;78.041944" || p.Param("JSID") != "loc1" {
		t.Errorf("GEO %+v", p)
	}

	two := mustJS(t, `{"@type":"Event","uid":"loc-2","title":"x","start":"2026-11-20T10:00:00","duration":"PT1H",
		"mainLocationId":"park",
		"locations":{"bar":{"@type":"Location","name":"Fred's Bar"},
		"park":{"@type":"Location","name":"XYZ Parking","coordinates":"geo:40.71427,-74.00597"}}}`)
	cal, js = roundTrip(t, two)
	if !reflect.DeepEqual(js["locations"], two["locations"]) || js["mainLocationId"] != "park" {
		t.Errorf("two locations: got %v main %v", js["locations"], js["mainLocationId"])
	}
	vev = cal.Sub("VEVENT")[0]
	if p := vev.Get("LOCATION"); p == nil || p.Param("DERIVED") != "TRUE" || p.Text() != "XYZ Parking" {
		t.Errorf("derived LOCATION %+v", p)
	}
	if v := vev.Sub("VLOCATION"); len(v) != 2 || v[0].Text("UID") != "bar" || v[1].Value("GEO") != "40.71427;-74.00597" {
		t.Errorf("VLOCATION %v", ical.Encode(cal))
	}

	// Back to one: the VLOCATIONs of the stored object go.
	cal2, err := ToICal(one, cal, "-//robinmail//test//EN", now)
	if err != nil {
		t.Fatal(err)
	}
	if v := cal2.Sub("VEVENT")[0].Sub("VLOCATION"); len(v) != 0 {
		t.Errorf("stale VLOCATION: %v", ical.Encode(cal2))
	}
}
