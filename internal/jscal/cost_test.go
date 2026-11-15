package jscal

import (
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
)

// A rule of every second or minute since 1970 was walked instance by
// instance up to the range, two million steps at most: almost a second of
// CPU for each such event in each calendar query (strangers can put one in
// a calendar with an invitation), and when the steps ran out the range got
// no instances while the expansion said it was complete. Rules without
// COUNT now start near the range.
func TestInstancesOfAnOldFineRule(t *testing.T) {
	from, to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		rule  string
		want  int
		whole bool
	}{
		{"FREQ=SECONDLY", 1000, false},
		{"FREQ=MINUTELY;INTERVAL=7", 205, true},
		{"FREQ=HOURLY;BYMINUTE=0,30", 48, true},
		{"FREQ=DAILY;BYHOUR=9,17", 2, true},
		{"FREQ=WEEKLY;BYDAY=SU", 1, true},
		{"FREQ=MONTHLY;BYMONTHDAY=1", 1, true},
		{"FREQ=YEARLY;BYMONTH=11;BYMONTHDAY=1", 1, true},
	} {
		data := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:1\r\nDTSTART:19700101T000000Z\r\nDTEND:19700101T000001Z\r\nRRULE:" +
			c.rule + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		cal, err := ical.ParseOne([]byte(data), "VCALENDAR")
		if err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		inst, complete := Read(cal, nil).Instances(from, to, 1000)
		took := time.Since(began)
		// No bound on the time: under -race and load it says little. The
		// old walk from DTSTART gave no instances here at all.
		t.Logf("%s: %v", c.rule, took)
		if len(inst) != c.want || complete != c.whole {
			t.Errorf("%s: %d instances (want %d), complete %v", c.rule, len(inst), c.want, complete)
		}
		for i := 1; i < len(inst); i++ {
			if !inst[i].Start.After(inst[i-1].Start) || inst[i].Start.Before(from) || !inst[i].Start.Before(to) {
				t.Fatalf("%s: instance %d at %v", c.rule, i, inst[i].Start)
			}
		}
	}
	// With COUNT the instances have to be counted; the expansion stops at
	// its bound and says so.
	data := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:1\r\nDTSTART:19700101T000000Z\r\nRRULE:FREQ=SECONDLY;COUNT=2000000000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	cal, _ := ical.ParseOne([]byte(data), "VCALENDAR")
	if inst, complete := Read(cal, nil).Instances(from, to, 1000); complete || len(inst) != 0 {
		t.Errorf("COUNT rule: %d instances, complete %v", len(inst), complete)
	}
}
