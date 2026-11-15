package ical

import (
	"bytes"
	"testing"
	"time"
)

// FuzzParse: Parse never panics, and what it reads writes back to text it
// reads again into the same components.
func FuzzParse(f *testing.F) {
	f.Add([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:1\r\nSUMMARY;LANGUAGE=en:Hi\\, there\r\nDTSTART;TZID=Europe/Amsterdam:20260101T100000\r\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	f.Add([]byte("BEGIN:VCARD\nVERSION:4.0\nitem1.EMAIL;TYPE=work:a@b\nN:Doe;Jane;;;\nEND:VCARD\nBEGIN:VCARD\nFN:x\nEND:VCARD\n"))
	f.Add([]byte("BEGIN:X\r\nA;P=\"q;:,\",b:v\r\n  folded\r\nEND:X\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		cs, err := Parse(data)
		if err != nil {
			return
		}
		out := EncodeAll(cs)
		again, err := Parse(out)
		if err != nil {
			t.Fatalf("re-parse of %q: %v", out, err)
		}
		if !bytes.Equal(EncodeAll(again), out) {
			t.Fatalf("not stable:\n%q\n%q", out, EncodeAll(again))
		}
	})
}

// FuzzRRule: rules parse or not, never panic, and expand in bounded time.
func FuzzRRule(f *testing.F) {
	for _, s := range []string{"FREQ=DAILY;COUNT=10", "FREQ=YEARLY;BYWEEKNO=-1;BYDAY=-1SU", "FREQ=MONTHLY;BYDAY=MO,TU;BYSETPOS=-1,2",
		"FREQ=SECONDLY;BYHOUR=1;BYMINUTE=2", "FREQ=YEARLY;BYYEARDAY=-366,366;BYMONTHDAY=-31"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := ParseRRule(s)
		if err != nil {
			return
		}
		set := Set{Start: time.Date(2020, 2, 29, 23, 30, 0, 0, time.UTC), Rules: []RRule{r}}
		set.Between(set.Start, set.Start.AddDate(3, 0, 0), 200)
		if _, err := ParseRRule(r.String()); err != nil {
			t.Fatalf("%q writes %q, which does not parse: %v", s, r.String(), err)
		}
	})
}
