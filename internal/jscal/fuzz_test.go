package jscal

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
)

var fuzzNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// FuzzCalendar: an iCalendar object from anyone (CalDAV PUT, an invitation
// by email) is read, turned into JSCalendar and back, and expanded,
// without a panic and in bounded time.
func FuzzCalendar(f *testing.F) {
	f.Add([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VTIMEZONE\r\nTZID:X\r\nBEGIN:STANDARD\r\nDTSTART:19701025T030000\r\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:1\r\nDTSTART;TZID=X:20260101T100000\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=5\r\nEXDATE;TZID=X:20260102T100000\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:a@b\r\nORGANIZER:mailto:c@d\r\nLOCATION:Room\r\nBEGIN:VALARM\r\nTRIGGER:-PT15M\r\nACTION:DISPLAY\r\nEND:VALARM\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:1\r\nRECURRENCE-ID;TZID=X:20260103T100000\r\nDTSTART;TZID=X:20260103T110000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	f.Add([]byte("BEGIN:VCALENDAR\r\nBEGIN:VTODO\r\nUID:t\r\nDUE;VALUE=DATE:20260101\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		cal, err := ical.ParseOne(data, "VCALENDAR")
		if err != nil {
			return
		}
		began := time.Now()
		o := Read(cal, nil)
		ev := ToJS(o)
		o.Instances(fuzzNow.AddDate(-1, 0, 0), fuzzNow.AddDate(1, 0, 0), 500)
		o.Span()
		if back, err := ToICal(ev, cal, "-//fuzz//EN", fuzzNow); err == nil {
			ToJS(Read(back, nil))
		}
		if d := time.Since(began); d > 5*time.Second {
			t.Fatalf("took %v", d)
		}
	})
}

// FuzzEvent: a JSCalendar object from an account holder (JMAP
// CalendarEvent/set) is written as iCalendar without a panic.
func FuzzEvent(f *testing.F) {
	f.Add([]byte(`{"@type":"Event","uid":"1","title":"t","start":"2026-01-01T10:00:00","timeZone":"Europe/Amsterdam","duration":"PT1H","recurrenceRules":[{"frequency":"weekly","byDay":[{"day":"mo"}]}],"recurrenceOverrides":{"2026-01-05T10:00:00":{"title":"x"}},"participants":{"a":{"email":"a@b","roles":{"attendee":true}}},"locations":{"l":{"name":"Room"}},"alerts":{"a":{"trigger":{"offset":"-PT15M"}}}}`))
	f.Add([]byte(`{"@type":"Task","uid":"2","due":"2026-01-01T10:00:00","showWithoutTime":true}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var ev Event
		if json.Unmarshal(data, &ev) != nil || ev == nil {
			return
		}
		cal, err := ToICal(ev, nil, "-//fuzz//EN", fuzzNow)
		if err != nil {
			return
		}
		again, err := ical.ParseOne(ical.Encode(cal), "VCALENDAR")
		if err != nil {
			t.Fatalf("ToICal wrote what does not parse: %v\n%s", err, ical.Encode(cal))
		}
		ToJS(Read(again, nil))
	})
}
