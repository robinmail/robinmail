package ical

import (
	"strings"
	"testing"
	"time"
)

// RFC 5545 3.1: a long line is folded by CRLF followed by one space; the
// unfolded line is the original.
func TestRFC5545_3_1_Folding(t *testing.T) {
	in := "BEGIN:VCALENDAR\r\nDESCRIPTION:This is a lo\r\n ng description\r\n  that exists on a long line.\r\nEND:VCALENDAR\r\n"
	cs, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := cs[0].Text("DESCRIPTION"); got != "This is a long description that exists on a long line." {
		t.Fatalf("unfolded %q", got)
	}
	// Writing folds at 75 octets and never splits a UTF-8 sequence.
	c := &Component{Name: "VEVENT"}
	c.SetText("SUMMARY", strings.Repeat("é", 100))
	out := Encode(c)
	for _, line := range strings.Split(string(out), "\r\n") {
		if len(line) > 75 {
			t.Fatalf("line of %d octets: %q", len(line), line)
		}
	}
	back, err := ParseOne(out, "VEVENT")
	if err != nil || back.Text("SUMMARY") != strings.Repeat("é", 100) {
		t.Fatalf("round trip: %v %q", err, back.Text("SUMMARY"))
	}
}

// RFC 5545 3.3.11: TEXT escapes backslash, semicolon, comma and newline.
func TestRFC5545_3_3_11_Text(t *testing.T) {
	cs, err := Parse([]byte("BEGIN:VEVENT\nDESCRIPTION:Project XYZ Final Review\\nConference Room - 3B\\nCome Prepared.\nLOCATION:a\\, b\\; c\\\\d\nEND:VEVENT\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cs[0].Text("DESCRIPTION"); got != "Project XYZ Final Review\nConference Room - 3B\nCome Prepared." {
		t.Fatalf("got %q", got)
	}
	if got := cs[0].Text("LOCATION"); got != `a, b; c\d` {
		t.Fatalf("got %q", got)
	}
	if Escape("a, b; c\\d\ne") != `a\, b\; c\\d\ne` {
		t.Fatal(Escape("a, b; c\\d\ne"))
	}
}

// RFC 5545 3.2: parameters, quoted values, several values; RFC 6868 3:
// the caret escapes in parameter values.
func TestRFC5545_3_2_Params(t *testing.T) {
	in := "BEGIN:VEVENT\r\n" +
		`ATTENDEE;DELEGATED-TO="mailto:jsmith@example.com","mailto:jdoe@example.com";CN=Jane Doe:mailto:jdoe@example.com` + "\r\n" +
		`ATTENDEE;CN="George Herman ^'Babe^' Ruth":mailto:babe@example.com` + "\r\n" +
		`X-ADDRESS;LABEL="Tokyo Tower^nMinato-ku":geo:35.658,139.745` + "\r\n" +
		"END:VEVENT\r\n"
	c, err := ParseOne([]byte(in), "VEVENT")
	if err != nil {
		t.Fatal(err)
	}
	a := c.All("ATTENDEE")
	if got := a[0].ParamValues("DELEGATED-TO"); len(got) != 2 || got[1] != "mailto:jdoe@example.com" {
		t.Fatalf("delegated-to %q", got)
	}
	if a[0].Param("CN") != "Jane Doe" || a[0].Value != "mailto:jdoe@example.com" {
		t.Fatalf("attendee %+v", a[0])
	}
	if a[1].Param("CN") != `George Herman "Babe" Ruth` {
		t.Fatalf("RFC 6868 quote: %q", a[1].Param("CN"))
	}
	if c.Get("X-ADDRESS").Param("LABEL") != "Tokyo Tower\nMinato-ku" {
		t.Fatalf("RFC 6868 newline: %q", c.Get("X-ADDRESS").Param("LABEL"))
	}
	// Written back, the caret escapes and quotes return.
	out := string(Encode(c))
	if !strings.Contains(out, `CN=George Herman ^'Babe^' Ruth:`) || !strings.Contains(out, `LABEL=Tokyo Tower^nMinato-ku:`) {
		t.Fatalf("encoded:\n%s", out)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"SUMMARY:x\n",
		"BEGIN:VEVENT\nSUMMARY:x\n",
		"BEGIN:VEVENT\nEND:VTODO\n",
		"BEGIN:VEVENT\nNOCOLON\nEND:VEVENT\n",
		"BEGIN:VEVENT\nX;P=\"unterminated:x\nEND:VEVENT\n",
		strings.Repeat("BEGIN:X\n", MaxDepth+1),
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("no error for %q", in)
		}
	}
}

// RFC 5545 3.3.6: DURATION.
func TestRFC5545_3_3_6_Duration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"P15DT5H0M20S": 15*24*time.Hour + 5*time.Hour + 20*time.Second,
		"P7W":          7 * 7 * 24 * time.Hour,
		"-PT15M":       -15 * time.Minute,
		"PT1H":         time.Hour,
	} {
		d, err := ParseDuration(in)
		if err != nil || d.Approx() != want {
			t.Errorf("%s: %v %v", in, d.Approx(), err)
		}
	}
	for _, bad := range []string{"P", "15D", "PT", "P1H", "PT1D", "P1DT"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if s := (Duration{Days: 1, Time: 90 * time.Minute}).String(); s != "P1DT1H30M" {
		t.Error(s)
	}
	if s := (Duration{}).String(); s != "PT0S" {
		t.Error(s)
	}
}

// RFC 5545 3.3.5: the three forms of DATE-TIME.
func TestRFC5545_3_3_5_DateTime(t *testing.T) {
	c, _ := ParseOne([]byte("BEGIN:VEVENT\nA:19980118T230000\nB:19980119T070000Z\nC;TZID=America/New_York:19980119T020000\nD;VALUE=DATE:19970714\nEND:VEVENT\n"), "VEVENT")
	z := Zones{}
	a, _ := z.ParseDateTime(c.Get("A"))
	b, _ := z.ParseDateTime(c.Get("B"))
	cc, _ := z.ParseDateTime(c.Get("C"))
	d, _ := z.ParseDateTime(c.Get("D"))
	if !a.Floating || a.Time.Hour() != 23 {
		t.Errorf("floating %+v", a)
	}
	if !b.IsUTC() || !b.Time.Equal(time.Date(1998, 1, 19, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("utc %+v", b)
	}
	if !cc.Time.Equal(time.Date(1998, 1, 19, 7, 0, 0, 0, time.UTC)) || cc.TZID != "America/New_York" {
		t.Errorf("zoned %+v", cc)
	}
	if !d.Date || d.Time.Day() != 14 {
		t.Errorf("date %+v", d)
	}
	if p := cc.Prop("DTSTART"); p.Param("TZID") != "America/New_York" || p.Value != "19980119T020000" {
		t.Errorf("written %+v", p)
	}
	// A Windows name, as Outlook writes it.
	w, _ := z.parseValue("20240301T100000", "W. Europe Standard Time", false)
	if !w.Time.Equal(time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("windows zone %v", w.Time)
	}
}

// RFC 5545 3.6.5: a VTIMEZONE by rules, with a TZID that is no IANA name,
// is found to be America/New_York.
func TestRFC5545_3_6_5_VTimezone(t *testing.T) {
	in := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:Custom Eastern\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20070311T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0400\r\nTZNAME:EDT\r\nEND:DAYLIGHT\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20071104T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nTZOFFSETFROM:-0400\r\nTZOFFSETTO:-0500\r\nTZNAME:EST\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\nEND:VCALENDAR\r\n"
	cal, err := ParseOne([]byte(in), "VCALENDAR")
	if err != nil {
		t.Fatal(err)
	}
	z := ZonesOf(cal)
	loc := z.Location("Custom Eastern")
	if loc == nil {
		t.Fatal("not resolved")
	}
	for _, at := range []time.Time{time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)} {
		ny, _ := time.LoadLocation("America/New_York")
		_, want := at.In(ny).Zone()
		if _, got := at.In(loc).Zone(); got != want {
			t.Errorf("%v: offset %d, want %d (%s)", at, got, want, loc)
		}
	}
	// The other way: a VTIMEZONE made from an IANA zone resolves to it.
	ams, _ := time.LoadLocation("Europe/Amsterdam")
	vt := VTimezone(ams, 2025, 2027)
	vt.SetText("TZID", "Made Up")
	if got := matchZone(vt); got.String() != "Europe/Amsterdam" && got.String() != "Europe/Berlin" && got.String() != "Europe/Brussels" &&
		got.String() != "Europe/Paris" && got.String() != "Europe/Budapest" && got.String() != "Europe/Warsaw" {
		t.Errorf("matched %s", got)
	}
}

// expect runs a rule from DTSTART in America/New_York, as the examples of
// RFC 5545 3.8.5.3 do, and compares the first instances.
func expect(t *testing.T, name, dtstart, rule string, exdates []string, want []string, all bool) {
	t.Helper()
	ny, _ := time.LoadLocation("America/New_York")
	start, _ := time.ParseInLocation("20060102T150405", dtstart, ny)
	r, err := ParseRRule(rule)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	set := Set{Start: start, Rules: []RRule{r}}
	for _, e := range exdates {
		x, _ := time.ParseInLocation("20060102T150405", e, ny)
		set.ExDates = append(set.ExDates, x)
	}
	it := set.Iter()
	var got []string
	for len(got) < len(want)+1 {
		x, ok := it.Next()
		if !ok {
			break
		}
		got = append(got, x.In(ny).Format("20060102T1504"))
	}
	if !all && len(got) > len(want) {
		got = got[:len(want)]
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("%s:\n got %v\nwant %v", name, got, want)
	}
	// The rule writes back as it was read, part for part.
	if r2, err := ParseRRule(r.String()); err != nil || r2.String() != r.String() {
		t.Errorf("%s: round trip %q", name, r.String())
	}
}

func days(prefix string, from, to int, hm string) []string {
	var out []string
	for d := from; d <= to; d++ {
		out = append(out, prefix+twoDigits(d)+"T"+hm)
	}
	return out
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// RFC 5545 3.8.5.3, the examples, each by its description in the RFC.
func TestRFC5545_3_8_5_3_Examples(t *testing.T) {
	expect(t, "Daily for 10 occurrences", "19970902T090000", "FREQ=DAILY;COUNT=10", nil,
		days("199709", 2, 11, "0900"), true)

	all := append(append(append(days("199709", 2, 30, "0900"), days("199710", 1, 31, "0900")...), days("199711", 1, 30, "0900")...), days("199712", 1, 23, "0900")...)
	expect(t, "Daily until December 24, 1997", "19970902T090000", "FREQ=DAILY;UNTIL=19971224T000000Z", nil, all, true)

	expect(t, "Every other day - forever", "19970902T090000", "FREQ=DAILY;INTERVAL=2", nil,
		[]string{"19970902T0900", "19970904T0900", "19970906T0900", "19970908T0900"}, false)

	expect(t, "Every 10 days, 5 occurrences", "19970902T090000", "FREQ=DAILY;INTERVAL=10;COUNT=5", nil,
		[]string{"19970902T0900", "19970912T0900", "19970922T0900", "19971002T0900", "19971012T0900"}, true)

	jan := append(append(days("199801", 1, 31, "0900"), days("199901", 1, 31, "0900")...), days("200001", 1, 31, "0900")...)
	expect(t, "Every day in January, for 3 years (yearly)", "19980101T090000",
		"FREQ=YEARLY;UNTIL=20000131T140000Z;BYMONTH=1;BYDAY=SU,MO,TU,WE,TH,FR,SA", nil, jan, true)
	expect(t, "Every day in January, for 3 years (daily)", "19980101T090000",
		"FREQ=DAILY;UNTIL=20000131T140000Z;BYMONTH=1", nil, jan, true)

	expect(t, "Weekly for 10 occurrences", "19970902T090000", "FREQ=WEEKLY;COUNT=10", nil,
		[]string{"19970902T0900", "19970909T0900", "19970916T0900", "19970923T0900", "19970930T0900",
			"19971007T0900", "19971014T0900", "19971021T0900", "19971028T0900", "19971104T0900"}, true)

	expect(t, "Weekly on Tuesday and Thursday for five weeks", "19970902T090000",
		"FREQ=WEEKLY;UNTIL=19971007T000000Z;WKST=SU;BYDAY=TU,TH", nil,
		[]string{"19970902T0900", "19970904T0900", "19970909T0900", "19970911T0900", "19970916T0900",
			"19970918T0900", "19970923T0900", "19970925T0900", "19970930T0900", "19971002T0900"}, true)

	expect(t, "Every other week on Monday, Wednesday, and Friday until December 24, 1997", "19970901T090000",
		"FREQ=WEEKLY;INTERVAL=2;UNTIL=19971224T000000Z;WKST=SU;BYDAY=MO,WE,FR", nil,
		[]string{"19970901T0900", "19970903T0900", "19970905T0900", "19970915T0900", "19970917T0900", "19970919T0900",
			"19970929T0900", "19971001T0900", "19971003T0900", "19971013T0900", "19971015T0900", "19971017T0900",
			"19971027T0900", "19971029T0900", "19971031T0900", "19971110T0900", "19971112T0900", "19971114T0900",
			"19971124T0900", "19971126T0900", "19971128T0900", "19971208T0900", "19971210T0900", "19971212T0900",
			"19971222T0900"}, true)

	expect(t, "Monthly on the first Friday for 10 occurrences", "19970905T090000", "FREQ=MONTHLY;COUNT=10;BYDAY=1FR", nil,
		[]string{"19970905T0900", "19971003T0900", "19971107T0900", "19971205T0900", "19980102T0900",
			"19980206T0900", "19980306T0900", "19980403T0900", "19980501T0900", "19980605T0900"}, true)

	expect(t, "Monthly on the second-to-last Monday of the month for 6 months", "19970922T090000",
		"FREQ=MONTHLY;COUNT=6;BYDAY=-2MO", nil,
		[]string{"19970922T0900", "19971020T0900", "19971117T0900", "19971222T0900", "19980119T0900", "19980216T0900"}, true)

	expect(t, "Monthly on the third-to-the-last day of the month, forever", "19970928T090000", "FREQ=MONTHLY;BYMONTHDAY=-3", nil,
		[]string{"19970928T0900", "19971029T0900", "19971128T0900", "19971229T0900", "19980129T0900", "19980226T0900"}, false)

	expect(t, "Every 18 months on the 10th thru 15th of the month for 10 occurrences", "19970910T090000",
		"FREQ=MONTHLY;INTERVAL=18;COUNT=10;BYMONTHDAY=10,11,12,13,14,15", nil,
		append(days("199709", 10, 15, "0900"), days("199903", 10, 13, "0900")...), true)

	expect(t, "Yearly in June and July for 10 occurrences", "19970610T090000", "FREQ=YEARLY;COUNT=10;BYMONTH=6,7", nil,
		[]string{"19970610T0900", "19970710T0900", "19980610T0900", "19980710T0900", "19990610T0900",
			"19990710T0900", "20000610T0900", "20000710T0900", "20010610T0900", "20010710T0900"}, true)

	expect(t, "Every 20th Monday of the year, forever", "19970519T090000", "FREQ=YEARLY;BYDAY=20MO", nil,
		[]string{"19970519T0900", "19980518T0900", "19990517T0900"}, false)

	expect(t, "Monday of week number 20, forever", "19970512T090000", "FREQ=YEARLY;BYWEEKNO=20;BYDAY=MO", nil,
		[]string{"19970512T0900", "19980511T0900", "19990517T0900"}, false)

	expect(t, "Every Thursday in March, forever", "19970313T090000", "FREQ=YEARLY;BYMONTH=3;BYDAY=TH", nil,
		[]string{"19970313T0900", "19970320T0900", "19970327T0900", "19980305T0900", "19980312T0900", "19980319T0900",
			"19980326T0900", "19990304T0900", "19990311T0900", "19990318T0900", "19990325T0900"}, false)

	expect(t, "Every Friday the 13th, forever", "19970902T090000", "FREQ=MONTHLY;BYDAY=FR;BYMONTHDAY=13", []string{"19970902T090000"},
		[]string{"19980213T0900", "19980313T0900", "19981113T0900", "19990813T0900", "20001013T0900"}, false)

	expect(t, "Every 4 years, the first Tuesday after a Monday in November (U.S. Presidential Election day)", "19961105T090000",
		"FREQ=YEARLY;INTERVAL=4;BYMONTH=11;BYDAY=TU;BYMONTHDAY=2,3,4,5,6,7,8", nil,
		[]string{"19961105T0900", "20001107T0900", "20041102T0900"}, false)

	expect(t, "The third instance into the month of one of Tuesday, Wednesday, or Thursday, for the next 3 months", "19970904T090000",
		"FREQ=MONTHLY;COUNT=3;BYDAY=TU,WE,TH;BYSETPOS=3", nil,
		[]string{"19970904T0900", "19971007T0900", "19971106T0900"}, true)

	expect(t, "The second-to-last weekday of the month", "19970929T090000", "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-2", nil,
		[]string{"19970929T0900", "19971030T0900", "19971127T0900", "19971230T0900", "19980129T0900", "19980226T0900", "19980330T0900"}, false)

	expect(t, "Every 15 minutes for 6 occurrences", "19970902T090000", "FREQ=MINUTELY;INTERVAL=15;COUNT=6", nil,
		[]string{"19970902T0900", "19970902T0915", "19970902T0930", "19970902T0945", "19970902T1000", "19970902T1015"}, true)

	expect(t, "Every hour and a half for 4 occurrences", "19970902T090000", "FREQ=MINUTELY;INTERVAL=90;COUNT=4", nil,
		[]string{"19970902T0900", "19970902T1030", "19970902T1200", "19970902T1330"}, true)

	var day1 []string
	for h := 9; h <= 16; h++ {
		for _, m := range []string{"00", "20", "40"} {
			day1 = append(day1, "19970902T"+twoDigits(h)+m)
		}
	}
	expect(t, "Every 20 minutes from 9:00 AM to 4:40 PM every day (daily)", "19970902T090000",
		"FREQ=DAILY;BYHOUR=9,10,11,12,13,14,15,16;BYMINUTE=0,20,40", nil, append(day1, "19970903T0900"), false)
	expect(t, "Every 20 minutes from 9:00 AM to 4:40 PM every day (minutely)", "19970902T090000",
		"FREQ=MINUTELY;INTERVAL=20;BYHOUR=9,10,11,12,13,14,15,16", nil, append(day1, "19970903T0900"), false)

	expect(t, "An example where the days generated makes a difference because of WKST (MO)", "19970805T090000",
		"FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=MO", nil,
		[]string{"19970805T0900", "19970810T0900", "19970819T0900", "19970824T0900"}, true)
	expect(t, "An example where the days generated makes a difference because of WKST (SU)", "19970805T090000",
		"FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=SU", nil,
		[]string{"19970805T0900", "19970817T0900", "19970819T0900", "19970831T0900"}, true)

	expect(t, "An example where an invalid date (i.e., February 30) is ignored", "20070115T090000",
		"FREQ=MONTHLY;BYMONTHDAY=15,30;COUNT=5", nil,
		[]string{"20070115T0900", "20070130T0900", "20070215T0900", "20070315T0900", "20070330T0900"}, true)
}

// A rule that never fires ends instead of running for ever (RFC 5545
// 3.3.10 leaves invalid dates out; robinmail stops looking after a while).
func TestRuleThatNeverFires(t *testing.T) {
	r, _ := ParseRRule("FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30")
	set := Set{Start: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Rules: []RRule{r}}
	it := set.Iter()
	it.Next() // DTSTART
	if _, ok := it.Next(); ok {
		t.Fatal("an instance on 30 February")
	}
	// Every second for ever: Between stops at its maximum.
	r, _ = ParseRRule("FREQ=SECONDLY")
	set = Set{Start: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Rules: []RRule{r}}
	got, complete := set.Between(set.Start, set.Start.Add(time.Hour), 100)
	if len(got) != 100 || complete {
		t.Fatalf("%d %v", len(got), complete)
	}
}

func TestBadRules(t *testing.T) {
	for _, s := range []string{"", "COUNT=3", "FREQ=WHENEVER", "FREQ=DAILY;COUNT=0", "FREQ=DAILY;COUNT=2;UNTIL=20000101T000000Z",
		"FREQ=DAILY;BYMONTH=13", "FREQ=DAILY;BYDAY=XX", "FREQ=DAILY;FREQ=WEEKLY", "FREQ=MONTHLY;BYMONTHDAY=0"} {
		if _, err := ParseRRule(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	r, err := ParseRRule("FREQ=MONTHLY;RSCALE=GREGORIAN;SKIP=FORWARD;BYMONTHDAY=31")
	if err != nil || !strings.Contains(r.String(), "RSCALE=GREGORIAN") {
		t.Fatalf("RFC 7529 parts are kept: %v %q", err, r.String())
	}
}
