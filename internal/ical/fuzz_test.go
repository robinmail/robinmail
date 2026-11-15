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

// FuzzSeek: IterFrom gives the same instances at and after from as a walk
// from the start (Iter) does.
func FuzzSeek(f *testing.F) {
	for _, s := range []string{"FREQ=SECONDLY;INTERVAL=7", "FREQ=MINUTELY;BYSECOND=5,50", "FREQ=HOURLY;INTERVAL=5;BYMINUTE=0,59",
		"FREQ=DAILY;BYHOUR=1,23", "FREQ=WEEKLY;INTERVAL=3;BYDAY=MO,SU;WKST=SU", "FREQ=MONTHLY;BYDAY=-1FR;BYSETPOS=1",
		"FREQ=YEARLY;BYWEEKNO=53;BYDAY=TH", "FREQ=MONTHLY;BYMONTHDAY=31;UNTIL=20300101T000000Z"} {
		f.Add(s, int64(86400*400+17), "Europe/Amsterdam")
	}
	f.Fuzz(func(t *testing.T, s string, offset int64, zone string) {
		r, err := ParseRRule(s)
		if err != nil || offset < 0 || offset > 60*86400*366 {
			return
		}
		loc, err := time.LoadLocation(zone)
		if err != nil || zone == "" || zone == "Local" {
			return
		}
		set := Set{Start: time.Date(2024, 3, 30, 22, 30, 15, 0, loc), Rules: []RRule{r}}
		from := set.Start.Add(time.Duration(offset) * time.Second)
		want := collect(set.Iter(), from, 20, 300_000)
		if want == nil {
			return // too far for the walk from the start
		}
		got := collect(set.IterFrom(from), from, 20, 300_000)
		if len(got) != len(want) {
			t.Fatalf("%s from %v: %d instances, want %d\n%v\n%v", s, from, len(got), len(want), got, want)
		}
		for i := range want {
			if !got[i].Equal(want[i]) {
				t.Fatalf("%s from %v: instance %d %v, want %v", s, from, i, got[i], want[i])
			}
		}
	})
}

// collect returns up to n instances at or after from, nil when that took
// more than steps.
func collect(it *Iter, from time.Time, n, steps int) []time.Time {
	out := []time.Time{}
	for i := 0; i < steps; i++ {
		t, ok := it.Next()
		if !ok {
			return out
		}
		if t.Before(from) {
			continue
		}
		if out = append(out, t); len(out) == n {
			return out
		}
	}
	return nil
}
