package ical

import (
	"container/heap"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Freq is the FREQ of a recurrence rule, finest first.
type Freq int

const (
	Secondly Freq = iota
	Minutely
	Hourly
	Daily
	Weekly
	Monthly
	Yearly
)

var freqNames = []string{"SECONDLY", "MINUTELY", "HOURLY", "DAILY", "WEEKLY", "MONTHLY", "YEARLY"}

func (f Freq) String() string {
	if f < 0 || int(f) >= len(freqNames) {
		return ""
	}
	return freqNames[f]
}

// WeekdayNum is one BYDAY entry: "MO", "-1SU", "+2TU". N is 0 for every
// such day of the period.
type WeekdayNum struct {
	N   int
	Day time.Weekday
}

var dayNames = []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}

func parseDay(s string) (time.Weekday, bool) {
	for i, n := range dayNames {
		if n == s {
			return time.Weekday(i), true
		}
	}
	return 0, false
}

// DayName is the two-letter name of a weekday.
func DayName(d time.Weekday) string { return dayNames[d] }

// RRule is a recurrence rule (RFC 5545 3.3.10).
type RRule struct {
	Freq       Freq
	Interval   int // 1 when not given
	Count      int // 0: not given
	Until      *DateTime
	BySecond   []int
	ByMinute   []int
	ByHour     []int
	ByDay      []WeekdayNum
	ByMonthDay []int
	ByYearDay  []int
	ByWeekNo   []int
	ByMonth    []int
	BySetPos   []int
	WKST       time.Weekday // Monday when not given
	// Extra holds parts this package does not interpret (RSCALE, SKIP of
	// RFC 7529, x-names), written back as they came.
	Extra []string
}

// ParseRRule reads the value of an RRULE property.
func ParseRRule(s string) (RRule, error) {
	r := RRule{Interval: 1, WKST: time.Monday, Freq: -1}
	bad := func(part string) error { return fmt.Errorf("%w: bad RRULE part %q", ErrSyntax, part) }
	seen := map[string]bool{}
	for _, part := range strings.Split(strings.TrimSpace(s), ";") {
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			return r, bad(part)
		}
		name = strings.ToUpper(name)
		if seen[name] {
			return r, fmt.Errorf("%w: RRULE part %s given twice", ErrSyntax, name)
		}
		seen[name] = true
		value = strings.ToUpper(value)
		ints := func(lo, hi int, zero bool) ([]int, error) {
			var out []int
			for _, x := range strings.Split(value, ",") {
				n, err := strconv.Atoi(strings.TrimPrefix(x, "+"))
				if err != nil || n < lo || n > hi || (n == 0 && !zero) {
					return nil, bad(part)
				}
				out = append(out, n)
			}
			if len(out) > 400 {
				return nil, bad(part)
			}
			return out, nil
		}
		var err error
		switch name {
		case "FREQ":
			r.Freq = -1
			for i, n := range freqNames {
				if n == value {
					r.Freq = Freq(i)
				}
			}
			if r.Freq < 0 {
				return r, bad(part)
			}
		case "INTERVAL":
			n, e := strconv.Atoi(value)
			if e != nil || n < 1 || n > 100000 {
				return r, bad(part)
			}
			r.Interval = n
		case "COUNT":
			n, e := strconv.Atoi(value)
			if e != nil || n < 1 {
				return r, bad(part)
			}
			r.Count = n
		case "UNTIL":
			d, e := Zones{}.parseValue(value, "", false)
			if e != nil {
				return r, bad(part)
			}
			r.Until = &d
		case "BYSECOND":
			r.BySecond, err = ints(0, 60, true)
		case "BYMINUTE":
			r.ByMinute, err = ints(0, 59, true)
		case "BYHOUR":
			r.ByHour, err = ints(0, 23, true)
		case "BYMONTHDAY":
			r.ByMonthDay, err = ints(-31, 31, false)
		case "BYYEARDAY":
			r.ByYearDay, err = ints(-366, 366, false)
		case "BYWEEKNO":
			r.ByWeekNo, err = ints(-53, 53, false)
		case "BYMONTH":
			r.ByMonth, err = ints(1, 12, false)
		case "BYSETPOS":
			r.BySetPos, err = ints(-366, 366, false)
		case "BYDAY":
			for _, x := range strings.Split(value, ",") {
				x = strings.TrimSpace(x)
				if len(x) < 2 {
					return r, bad(part)
				}
				d, ok := parseDay(x[len(x)-2:])
				if !ok {
					return r, bad(part)
				}
				n := 0
				if num := x[:len(x)-2]; num != "" {
					n, err = strconv.Atoi(strings.TrimPrefix(num, "+"))
					if err != nil || n == 0 || n < -53 || n > 53 {
						return r, bad(part)
					}
				}
				r.ByDay = append(r.ByDay, WeekdayNum{N: n, Day: d})
			}
		case "WKST":
			d, ok := parseDay(value)
			if !ok {
				return r, bad(part)
			}
			r.WKST = d
		default:
			r.Extra = append(r.Extra, part)
		}
		if err != nil {
			return r, err
		}
	}
	if r.Freq < 0 {
		return r, fmt.Errorf("%w: RRULE without FREQ", ErrSyntax)
	}
	if r.Count > 0 && r.Until != nil {
		return r, fmt.Errorf("%w: RRULE with both COUNT and UNTIL", ErrSyntax)
	}
	return r, nil
}

// String writes the rule as an RRULE value.
func (r RRule) String() string {
	var parts []string
	parts = append(parts, "FREQ="+r.Freq.String())
	if r.Until != nil {
		if r.Until.Date {
			parts = append(parts, "UNTIL="+FormatDate(r.Until.Time))
		} else if r.Until.Floating {
			parts = append(parts, "UNTIL="+FormatLocal(r.Until.Time))
		} else {
			parts = append(parts, "UNTIL="+FormatUTC(r.Until.Time))
		}
	}
	if r.Count > 0 {
		parts = append(parts, "COUNT="+strconv.Itoa(r.Count))
	}
	if r.Interval > 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(r.Interval))
	}
	list := func(name string, xs []int) {
		if len(xs) == 0 {
			return
		}
		s := make([]string, len(xs))
		for i, x := range xs {
			s[i] = strconv.Itoa(x)
		}
		parts = append(parts, name+"="+strings.Join(s, ","))
	}
	list("BYSECOND", r.BySecond)
	list("BYMINUTE", r.ByMinute)
	list("BYHOUR", r.ByHour)
	if len(r.ByDay) > 0 {
		s := make([]string, len(r.ByDay))
		for i, d := range r.ByDay {
			if d.N != 0 {
				s[i] = strconv.Itoa(d.N)
			}
			s[i] += dayNames[d.Day]
		}
		parts = append(parts, "BYDAY="+strings.Join(s, ","))
	}
	list("BYMONTHDAY", r.ByMonthDay)
	list("BYYEARDAY", r.ByYearDay)
	list("BYWEEKNO", r.ByWeekNo)
	list("BYMONTH", r.ByMonth)
	list("BYSETPOS", r.BySetPos)
	if r.WKST != time.Monday {
		parts = append(parts, "WKST="+dayNames[r.WKST])
	}
	parts = append(parts, r.Extra...)
	return strings.Join(parts, ";")
}

// Limits of expansion: a rule may fire every second, or never.
const (
	// maxEmptyPeriods is how many periods in a row may produce nothing
	// before a rule is taken to have ended ("every 30 February").
	maxEmptyPeriods = 2000
	// maxCandidates bounds the instances one period may produce.
	maxCandidates = 50_000
)

// ruleIter produces the instances of one rule in order.
type ruleIter struct {
	r       RRule
	start   time.Time // DTSTART, in the zone of the rule
	until   time.Time
	hasEnd  bool
	period  int
	buf     []time.Time
	emitted int
	empty   int
	done    bool
}

func newRuleIter(r RRule, start time.Time) *ruleIter {
	it := &ruleIter{r: r, start: start}
	if r.Until != nil {
		it.hasEnd = true
		u := *r.Until
		switch {
		case u.Date:
			// A date UNTIL includes that whole day.
			it.until = time.Date(u.Time.Year(), u.Time.Month(), u.Time.Day(), 23, 59, 59, 0, start.Location())
		case u.Floating:
			it.until = u.In(start.Location())
		default:
			it.until = u.Time
		}
	}
	return it
}

// longestPeriod is the most time one period of a frequency can take: a
// day can have 25 hours.
var longestPeriod = map[Freq]time.Duration{
	Secondly: time.Second, Minutely: time.Minute, Hourly: time.Hour,
	Daily: 25 * time.Hour, Weekly: 7 * 25 * time.Hour, Monthly: 31 * 25 * time.Hour, Yearly: 366 * 25 * time.Hour,
}

// seek skips the periods that end before from without making their
// instances, so that a rule of every second since 1970 costs as much as one
// that started last week. A rule with COUNT is left alone: its instances
// have to be counted. The period it moves to begins at or before from, so
// nothing at or after from is skipped; the caller drops what is before.
func (it *ruleIter) seek(from time.Time) {
	if it.r.Count > 0 || it.done || it.r.Interval <= 0 {
		return
	}
	d := from.Sub(it.start)
	per := longestPeriod[it.r.Freq] * time.Duration(it.r.Interval)
	if d <= 0 || per <= 0 {
		return
	}
	if n := int(d/per) - 2; n > it.period {
		it.period, it.buf, it.empty = n, nil, 0
	}
}

func (it *ruleIter) next() (time.Time, bool) {
	for len(it.buf) == 0 {
		if it.done {
			return time.Time{}, false
		}
		it.fill()
	}
	t := it.buf[0]
	it.buf = it.buf[1:]
	return t, true
}

func contains(xs []int, n int) bool {
	for _, x := range xs {
		if x == n {
			return true
		}
	}
	return false
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func daysInYear(y int) int {
	return time.Date(y, 12, 31, 0, 0, 0, 0, time.UTC).YearDay()
}

// weekOne returns the first day of week 1 of a year: the week, starting on
// wkst, that holds at least four days of the year (RFC 5545 3.3.10,
// BYWEEKNO).
func weekOne(y int, wkst time.Weekday) time.Time {
	jan1 := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
	off := (int(jan1.Weekday()) - int(wkst) + 7) % 7
	start := jan1.AddDate(0, 0, -off)
	if off >= 4 {
		start = start.AddDate(0, 0, 7)
	}
	return start
}

func weeksInYear(y int, wkst time.Weekday) int {
	return int(weekOne(y+1, wkst).Sub(weekOne(y, wkst)).Hours()/24) / 7
}

// fill computes the instances of the next period.
func (it *ruleIter) fill() {
	r := it.r
	k := it.period * r.Interval
	it.period++
	s := it.start
	loc := s.Location()

	// The days of the period, as dates (midnight UTC stands for a date).
	var days []time.Time
	date := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	sd := date(s.Year(), s.Month(), s.Day())
	var periodStart time.Time // for HOURLY and finer: the instant the period begins
	switch r.Freq {
	case Yearly:
		y := s.Year() + k
		if len(r.ByWeekNo) > 0 {
			n := weeksInYear(y, r.WKST)
			for _, w := range r.ByWeekNo {
				if w < 0 {
					w = n + w + 1
				}
				if w < 1 || w > n {
					continue
				}
				first := weekOne(y, r.WKST).AddDate(0, 0, 7*(w-1))
				for i := 0; i < 7; i++ {
					days = append(days, first.AddDate(0, 0, i))
				}
			}
		} else {
			first := date(y, 1, 1)
			for i := 0; i < daysInYear(y); i++ {
				days = append(days, first.AddDate(0, 0, i))
			}
		}
	case Monthly:
		m0 := int(s.Month()) - 1 + k
		y, m := s.Year()+m0/12, time.Month(m0%12+1)
		for d := 1; d <= daysIn(y, m); d++ {
			days = append(days, date(y, m, d))
		}
	case Weekly:
		off := (int(sd.Weekday()) - int(r.WKST) + 7) % 7
		first := sd.AddDate(0, 0, -off+7*k)
		for i := 0; i < 7; i++ {
			days = append(days, first.AddDate(0, 0, i))
		}
	case Daily:
		days = []time.Time{sd.AddDate(0, 0, k)}
	default:
		unit := map[Freq]time.Duration{Hourly: time.Hour, Minutely: time.Minute, Secondly: time.Second}[r.Freq]
		trunc := time.Date(s.Year(), s.Month(), s.Day(), s.Hour(), 0, 0, 0, loc)
		switch r.Freq {
		case Minutely:
			trunc = time.Date(s.Year(), s.Month(), s.Day(), s.Hour(), s.Minute(), 0, 0, loc)
		case Secondly:
			trunc = s.Truncate(time.Second)
		}
		periodStart = trunc.Add(time.Duration(k) * unit).In(loc)
		days = []time.Time{date(periodStart.Year(), periodStart.Month(), periodStart.Day())}
	}

	// Defaults from DTSTART where the rule leaves a part open.
	byDay, byMonthDay, byMonth := r.ByDay, r.ByMonthDay, r.ByMonth
	switch r.Freq {
	case Weekly:
		if len(byDay) == 0 {
			byDay = []WeekdayNum{{Day: s.Weekday()}}
		}
	case Monthly:
		if len(byDay) == 0 && len(byMonthDay) == 0 && len(r.ByYearDay) == 0 {
			byMonthDay = []int{s.Day()}
		}
	case Yearly:
		if len(byDay) == 0 && len(byMonthDay) == 0 && len(r.ByYearDay) == 0 {
			if len(r.ByWeekNo) > 0 {
				byDay = []WeekdayNum{{Day: s.Weekday()}}
			} else {
				byMonthDay = []int{s.Day()}
				if len(byMonth) == 0 {
					byMonth = []int{int(s.Month())}
				}
			}
		}
	}

	// nth reports whether d is the n-th (from the end when n < 0) such
	// weekday of its month, or of its year.
	nth := func(d time.Time, n int, inMonth bool) bool {
		var idx, total int
		if inMonth {
			idx = (d.Day()-1)/7 + 1
			total = (daysIn(d.Year(), d.Month())-d.Day())/7 + idx
		} else {
			idx = (d.YearDay()-1)/7 + 1
			total = (daysInYear(d.Year())-d.YearDay())/7 + idx
		}
		if n > 0 {
			return idx == n
		}
		return total-idx+1 == -n
	}
	inMonth := r.Freq == Monthly || (r.Freq == Yearly && len(byMonth) > 0)
	var kept []time.Time
	for _, d := range days {
		if len(byMonth) > 0 && !contains(byMonth, int(d.Month())) {
			continue
		}
		if len(r.ByYearDay) > 0 {
			yd, n := d.YearDay(), daysInYear(d.Year())
			if !contains(r.ByYearDay, yd) && !contains(r.ByYearDay, yd-n-1) {
				continue
			}
		}
		if len(byMonthDay) > 0 {
			md, n := d.Day(), daysIn(d.Year(), d.Month())
			if !contains(byMonthDay, md) && !contains(byMonthDay, md-n-1) {
				continue
			}
		}
		if len(byDay) > 0 {
			ok := false
			for _, w := range byDay {
				if w.Day != d.Weekday() {
					continue
				}
				if w.N == 0 || r.Freq < Monthly || len(r.ByWeekNo) > 0 || nth(d, w.N, inMonth) {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		kept = append(kept, d)
	}

	// The times of each day.
	pick := func(list []int, def int) []int {
		if len(list) > 0 {
			return list
		}
		return []int{def}
	}
	var hours, minutes, seconds []int
	switch {
	case r.Freq >= Daily:
		hours, minutes, seconds = pick(r.ByHour, s.Hour()), pick(r.ByMinute, s.Minute()), pick(r.BySecond, s.Second())
	case r.Freq == Hourly:
		hours, minutes, seconds = []int{periodStart.Hour()}, pick(r.ByMinute, s.Minute()), pick(r.BySecond, s.Second())
	case r.Freq == Minutely:
		hours, minutes, seconds = []int{periodStart.Hour()}, []int{periodStart.Minute()}, pick(r.BySecond, s.Second())
	default:
		hours, minutes, seconds = []int{periodStart.Hour()}, []int{periodStart.Minute()}, []int{periodStart.Second()}
	}
	if r.Freq < Daily {
		// Finer than a day the BY parts above the frequency only limit.
		if len(r.ByHour) > 0 && !contains(r.ByHour, hours[0]) {
			kept = nil
		}
		if r.Freq < Hourly && len(r.ByMinute) > 0 && !contains(r.ByMinute, minutes[0]) {
			kept = nil
		}
		if r.Freq == Secondly && len(r.BySecond) > 0 && !contains(r.BySecond, seconds[0]) {
			kept = nil
		}
	}
	if len(kept)*len(hours)*len(minutes)*len(seconds) > maxCandidates {
		it.done = true
		return
	}
	var set []time.Time
	for _, d := range kept {
		for _, h := range hours {
			for _, mi := range minutes {
				for _, sec := range seconds {
					t := time.Date(d.Year(), d.Month(), d.Day(), h, mi, sec, 0, loc)
					if r.Freq < Daily {
						// Finer than a day the instance is the period's instant,
						// which a gap or an overlap of the clock leaves alone.
						t = periodStart.Add(time.Duration(mi-periodStart.Minute())*time.Minute + time.Duration(sec-periodStart.Second())*time.Second)
					}
					set = append(set, t)
				}
			}
		}
	}
	sort.Slice(set, func(i, j int) bool { return set[i].Before(set[j]) })
	if len(r.BySetPos) > 0 && len(set) > 0 {
		var sel []time.Time
		for _, p := range r.BySetPos {
			i := p - 1
			if p < 0 {
				i = len(set) + p
			}
			if i >= 0 && i < len(set) {
				sel = append(sel, set[i])
			}
		}
		sort.Slice(sel, func(i, j int) bool { return sel[i].Before(sel[j]) })
		set = sel
	}
	var out []time.Time
	for i, t := range set {
		if i > 0 && t.Equal(set[i-1]) {
			continue
		}
		if t.Before(it.start) {
			continue
		}
		if it.hasEnd && t.After(it.until) {
			it.done = true
			break
		}
		if r.Count > 0 && it.emitted >= r.Count {
			it.done = true
			break
		}
		it.emitted++
		out = append(out, t)
	}
	if r.Count > 0 && it.emitted >= r.Count {
		it.done = true
	}
	if len(out) == 0 {
		if it.empty++; it.empty > maxEmptyPeriods {
			it.done = true
		}
	} else {
		it.empty = 0
	}
	// A period past the end of time ends the rule too.
	if len(days) > 0 && days[0].Year() > 9999 {
		it.done = true
	}
	it.buf = out
}

// Set is a recurrence set (RFC 5545 3.8.5): DTSTART, rules, RDATEs, less
// the EXDATEs. All times are instants; for a floating event or a date the
// caller reads them in one zone (DateTime.In) and back.
type Set struct {
	Start   time.Time
	Rules   []RRule
	RDates  []time.Time
	ExDates []time.Time
}

// Iter walks the instances of a set in order of time.
type Iter struct {
	h    iterHeap
	ex   map[int64]bool
	last time.Time
	any  bool
}

type source struct {
	cur  time.Time
	rule *ruleIter
	list []time.Time
}

type iterHeap []*source

func (h iterHeap) Len() int           { return len(h) }
func (h iterHeap) Less(i, j int) bool { return h[i].cur.Before(h[j].cur) }
func (h iterHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *iterHeap) Push(x any)        { *h = append(*h, x.(*source)) }
func (h *iterHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func (s *source) advance() bool {
	if s.rule != nil {
		t, ok := s.rule.next()
		s.cur = t
		return ok
	}
	if len(s.list) == 0 {
		return false
	}
	s.cur, s.list = s.list[0], s.list[1:]
	return true
}

// Iter starts a walk over the set. DTSTART is always the first instance
// (RFC 5545 3.8.2.4), unless it is excluded.
func (s *Set) Iter() *Iter { return s.IterFrom(time.Time{}) }

// IterFrom is Iter for a caller that wants nothing before from: the rules
// without COUNT start near it (ruleIter.seek). Instances before from may
// still come and are the caller's to drop.
func (s *Set) IterFrom(from time.Time) *Iter {
	it := &Iter{ex: map[int64]bool{}}
	for _, t := range s.ExDates {
		it.ex[t.Unix()] = true
	}
	list := append([]time.Time{s.Start}, s.RDates...)
	sort.Slice(list, func(i, j int) bool { return list[i].Before(list[j]) })
	src := &source{list: list}
	if src.advance() {
		it.h = append(it.h, src)
	}
	for _, r := range s.Rules {
		rs := &source{rule: newRuleIter(r, s.Start)}
		if !from.IsZero() {
			rs.rule.seek(from)
		}
		if rs.advance() {
			it.h = append(it.h, rs)
		}
	}
	heap.Init(&it.h)
	return it
}

// Next returns the next instance; false at the end.
func (it *Iter) Next() (time.Time, bool) {
	for it.h.Len() > 0 {
		src := it.h[0]
		t := src.cur
		if src.advance() {
			heap.Fix(&it.h, 0)
		} else {
			heap.Pop(&it.h)
		}
		if it.any && !t.After(it.last) {
			continue // the same instant from two sources
		}
		if it.ex[t.Unix()] {
			continue
		}
		it.last, it.any = t, true
		return t, true
	}
	return time.Time{}, false
}

// Between returns the instances that start at or after from and before to,
// at most max of them (and false when there were more). It looks at no
// more than maxSteps instances before from.
func (s *Set) Between(from, to time.Time, max int) ([]time.Time, bool) {
	const maxSteps = 1_000_000
	it := s.IterFrom(from)
	var out []time.Time
	for steps := 0; steps < maxSteps; steps++ {
		t, ok := it.Next()
		if !ok || !t.Before(to) {
			return out, true
		}
		if t.Before(from) {
			continue
		}
		if len(out) >= max {
			return out, false
		}
		out = append(out, t)
	}
	return out, false
}

// Infinite reports whether the set has no last instance as far as the
// rules say: a rule without COUNT and UNTIL.
func (s *Set) Infinite() bool {
	for _, r := range s.Rules {
		if r.Count == 0 && r.Until == nil {
			return true
		}
	}
	return false
}

// Last returns the last instance of a finite set, looking at no more than
// limit instances; false when the set is infinite or longer.
func (s *Set) Last(limit int) (time.Time, bool) {
	if s.Infinite() {
		return time.Time{}, false
	}
	it := s.Iter()
	var last time.Time
	for i := 0; ; i++ {
		t, ok := it.Next()
		if !ok {
			return last, true
		}
		if i >= limit {
			return time.Time{}, false
		}
		last = t
	}
}
