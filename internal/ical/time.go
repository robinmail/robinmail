package ical

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // the IANA zones go inside the binary: a host may lack them
)

// DateTime is a DATE or DATE-TIME value (RFC 5545 3.3.4, 3.3.5).
//
// For a value with a time zone (TZID, or UTC with "Z") Time is that
// instant in that zone. A floating time and a date have no zone: Time
// holds the wall clock in time.UTC and Floating or Date is set.
type DateTime struct {
	Time     time.Time
	Date     bool // VALUE=DATE: Time is midnight UTC of the day
	Floating bool // a DATE-TIME without zone
	TZID     string
}

// IsUTC reports a DATE-TIME in UTC ("Z").
func (d DateTime) IsUTC() bool { return !d.Date && !d.Floating && d.TZID == "" }

// In returns the instant of the value, reading a floating time or a date
// in loc.
func (d DateTime) In(loc *time.Location) time.Time {
	if d.Date || d.Floating {
		t := d.Time
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
	}
	return d.Time
}

// Zones resolves the TZID of a value to a location. The zero value finds
// IANA names, the Windows names Outlook writes, and names with an IANA name
// at their end ("/mozilla.org/20050126_1/Europe/Amsterdam"). Defined
// holds the VTIMEZONEs of the object being read, by TZID, for names that
// are none of these.
type Zones struct {
	Defined map[string]*time.Location
}

var (
	zoneCache sync.Map // name -> *time.Location or nil
	zoneCount atomic.Int64
)

// LoadZone finds an IANA zone by name, or a Windows zone name; nil if the
// name is neither.
func LoadZone(name string) *time.Location {
	if name == "" {
		return nil
	}
	if v, ok := zoneCache.Load(name); ok {
		loc, _ := v.(*time.Location)
		return loc
	}
	var loc *time.Location
	if iana, ok := windowsZones[name]; ok {
		loc, _ = time.LoadLocation(iana)
	} else if name != "Local" && !strings.HasPrefix(name, "/") && strings.Count(name, "..") == 0 {
		loc, _ = time.LoadLocation(name)
	}
	if loc == nil {
		// "/mozilla.org/.../Europe/Amsterdam", "/softwarestudio.org/Olson_20011030_5/America/New_York"
		parts := strings.Split(strings.Trim(name, "/"), "/")
		for i := 1; i < len(parts) && loc == nil; i++ {
			cand := strings.Join(parts[i:], "/")
			if strings.Contains(cand, "..") {
				continue
			}
			loc, _ = time.LoadLocation(cand)
		}
	}
	if zoneCount.Load() < 4096 {
		if _, had := zoneCache.LoadOrStore(name, loc); !had {
			zoneCount.Add(1)
		}
	}
	return loc
}

// Location resolves a TZID; nil when it cannot.
func (z Zones) Location(tzid string) *time.Location {
	if loc, ok := z.Defined[tzid]; ok && loc != nil {
		return loc
	}
	return LoadZone(tzid)
}

// ParseDateTime reads the DATE or DATE-TIME value of a property, with its
// VALUE and TZID parameters. A TZID that cannot be resolved is read as UTC
// and keeps its name, so it is written back as it came.
func (z Zones) ParseDateTime(p *Prop) (DateTime, error) {
	return z.parseValue(strings.TrimSpace(p.Value), p.Param("TZID"), strings.EqualFold(p.Param("VALUE"), "DATE"))
}

// ParseDateTimeList reads a property with several values (EXDATE, RDATE).
// PERIOD values of RDATE give their start.
func (z Zones) ParseDateTimeList(p *Prop) ([]DateTime, error) {
	var out []DateTime
	for _, v := range strings.Split(p.Value, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if start, _, ok := strings.Cut(v, "/"); ok {
			v = start
		}
		d, err := z.parseValue(v, p.Param("TZID"), strings.EqualFold(p.Param("VALUE"), "DATE"))
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (z Zones) parseValue(v, tzid string, date bool) (DateTime, error) {
	if date || len(v) == 8 {
		t, err := time.Parse("20060102", v)
		if err != nil {
			return DateTime{}, fmt.Errorf("%w: bad DATE %q", ErrSyntax, v)
		}
		return DateTime{Time: t, Date: true}, nil
	}
	utc := strings.HasSuffix(v, "Z") || strings.HasSuffix(v, "z")
	v = strings.TrimRight(v, "Zz")
	t, err := time.Parse("20060102T150405", v)
	if err != nil {
		return DateTime{}, fmt.Errorf("%w: bad DATE-TIME %q", ErrSyntax, v)
	}
	switch {
	case utc:
		return DateTime{Time: t}, nil
	case tzid != "":
		loc := z.Location(tzid)
		if loc == nil {
			return DateTime{Time: t, TZID: tzid}, nil
		}
		return DateTime{Time: time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc), TZID: tzid}, nil
	}
	return DateTime{Time: t, Floating: true}, nil
}

// FormatDate writes a DATE.
func FormatDate(t time.Time) string { return t.Format("20060102") }

// FormatUTC writes a DATE-TIME in UTC.
func FormatUTC(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// FormatLocal writes a DATE-TIME without zone (floating, or for a TZID).
func FormatLocal(t time.Time) string { return t.Format("20060102T150405") }

// Prop makes the property for a value: VALUE=DATE for a date, TZID for a
// zoned time that is not UTC.
func (d DateTime) Prop(name string) Prop {
	p := Prop{Name: name}
	switch {
	case d.Date:
		p.SetParam("VALUE", "DATE")
		p.Value = FormatDate(d.Time)
	case d.Floating:
		p.Value = FormatLocal(d.Time)
	case d.TZID != "":
		p.SetParam("TZID", d.TZID)
		p.Value = FormatLocal(d.Time)
	default:
		p.Value = FormatUTC(d.Time)
	}
	return p
}

// Duration is a DURATION value (RFC 5545 3.3.6). Weeks and days are kept
// apart from the time part: a day in a zone is not always 24 hours.
type Duration struct {
	Negative bool
	Weeks    int
	Days     int
	Time     time.Duration // hours, minutes, seconds
}

// ParseDuration reads "P15DT5H0M20S", "-PT15M", "P7W".
func ParseDuration(s string) (Duration, error) {
	var d Duration
	orig := s
	s = strings.ToUpper(strings.TrimSpace(s))
	if strings.HasPrefix(s, "-") {
		d.Negative = true
		s = s[1:]
	} else {
		s = strings.TrimPrefix(s, "+")
	}
	if !strings.HasPrefix(s, "P") || len(s) < 2 {
		return d, fmt.Errorf("%w: bad DURATION %q", ErrSyntax, orig)
	}
	s = s[1:]
	inTime := false
	any := false
	for len(s) > 0 {
		if s[0] == 'T' {
			if inTime {
				return d, fmt.Errorf("%w: bad DURATION %q", ErrSyntax, orig)
			}
			inTime = true
			s = s[1:]
			continue
		}
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || i == len(s) || i > 9 {
			return d, fmt.Errorf("%w: bad DURATION %q", ErrSyntax, orig)
		}
		n, _ := strconv.Atoi(s[:i])
		unit := s[i]
		s = s[i+1:]
		any = true
		switch {
		case !inTime && unit == 'W':
			d.Weeks += n
		case !inTime && unit == 'D':
			d.Days += n
		case inTime && unit == 'H':
			d.Time += time.Duration(n) * time.Hour
		case inTime && unit == 'M':
			d.Time += time.Duration(n) * time.Minute
		case inTime && unit == 'S':
			d.Time += time.Duration(n) * time.Second
		default:
			return d, fmt.Errorf("%w: bad DURATION %q", ErrSyntax, orig)
		}
	}
	if !any || strings.HasSuffix(orig, "T") || strings.HasSuffix(orig, "t") {
		return d, fmt.Errorf("%w: bad DURATION %q", ErrSyntax, orig)
	}
	return d, nil
}

// String writes the duration in the form of RFC 5545 (and JSCalendar).
func (d Duration) String() string {
	var b strings.Builder
	if d.Negative {
		b.WriteByte('-')
	}
	b.WriteByte('P')
	if d.Weeks > 0 && d.Days == 0 && d.Time == 0 {
		fmt.Fprintf(&b, "%dW", d.Weeks)
		return b.String()
	}
	days := d.Days + 7*d.Weeks
	if days > 0 {
		fmt.Fprintf(&b, "%dD", days)
	}
	if d.Time > 0 || days == 0 {
		b.WriteByte('T')
		t := d.Time
		h, m, s := int(t/time.Hour), int(t%time.Hour/time.Minute), int(t%time.Minute/time.Second)
		if h > 0 {
			fmt.Fprintf(&b, "%dH", h)
		}
		if m > 0 {
			fmt.Fprintf(&b, "%dM", m)
		}
		if s > 0 || (h == 0 && m == 0) {
			fmt.Fprintf(&b, "%dS", s)
		}
	}
	return b.String()
}

// AddTo adds the duration to t: weeks and days on the wall clock of t's
// zone, the rest as elapsed time (RFC 5545 3.3.6).
func (d Duration) AddTo(t time.Time) time.Time {
	sign := 1
	if d.Negative {
		sign = -1
	}
	t = t.AddDate(0, 0, sign*(d.Days+7*d.Weeks))
	return t.Add(time.Duration(sign) * d.Time)
}

// Approx is the duration as elapsed time, a day being 24 hours.
func (d Duration) Approx() time.Duration {
	x := time.Duration(d.Days+7*d.Weeks)*24*time.Hour + d.Time
	if d.Negative {
		return -x
	}
	return x
}

// DurationOf makes a Duration from elapsed time, in days and time.
func DurationOf(x time.Duration) Duration {
	var d Duration
	if x < 0 {
		d.Negative = true
		x = -x
	}
	d.Days = int(x / (24 * time.Hour))
	d.Time = x % (24 * time.Hour)
	return d
}

// windowsZones maps the zone names of Windows, which Outlook and Exchange
// write as TZID, to IANA names: the "001" territory of CLDR's windowsZones.
var windowsZones = map[string]string{
	"Dateline Standard Time":          "Etc/GMT+12",
	"UTC-11":                          "Etc/GMT+11",
	"Hawaiian Standard Time":          "Pacific/Honolulu",
	"Alaskan Standard Time":           "America/Anchorage",
	"Pacific Standard Time (Mexico)":  "America/Tijuana",
	"Pacific Standard Time":           "America/Los_Angeles",
	"US Mountain Standard Time":       "America/Phoenix",
	"Mountain Standard Time (Mexico)": "America/Mazatlan",
	"Mountain Standard Time":          "America/Denver",
	"Central America Standard Time":   "America/Guatemala",
	"Central Standard Time":           "America/Chicago",
	"Central Standard Time (Mexico)":  "America/Mexico_City",
	"Canada Central Standard Time":    "America/Regina",
	"SA Pacific Standard Time":        "America/Bogota",
	"Eastern Standard Time":           "America/New_York",
	"Eastern Standard Time (Mexico)":  "America/Cancun",
	"US Eastern Standard Time":        "America/Indianapolis",
	"Venezuela Standard Time":         "America/Caracas",
	"Atlantic Standard Time":          "America/Halifax",
	"SA Western Standard Time":        "America/La_Paz",
	"Pacific SA Standard Time":        "America/Santiago",
	"Newfoundland Standard Time":      "America/St_Johns",
	"E. South America Standard Time":  "America/Sao_Paulo",
	"Argentina Standard Time":         "America/Buenos_Aires",
	"Greenland Standard Time":         "America/Godthab",
	"UTC-02":                          "Etc/GMT+2",
	"Azores Standard Time":            "Atlantic/Azores",
	"Cape Verde Standard Time":        "Atlantic/Cape_Verde",
	"UTC":                             "Etc/UTC",
	"GMT Standard Time":               "Europe/London",
	"Greenwich Standard Time":         "Atlantic/Reykjavik",
	"Morocco Standard Time":           "Africa/Casablanca",
	"W. Europe Standard Time":         "Europe/Berlin",
	"Central Europe Standard Time":    "Europe/Budapest",
	"Romance Standard Time":           "Europe/Paris",
	"Central European Standard Time":  "Europe/Warsaw",
	"W. Central Africa Standard Time": "Africa/Lagos",
	"GTB Standard Time":               "Europe/Bucharest",
	"Middle East Standard Time":       "Asia/Beirut",
	"Egypt Standard Time":             "Africa/Cairo",
	"E. Europe Standard Time":         "Europe/Chisinau",
	"South Africa Standard Time":      "Africa/Johannesburg",
	"FLE Standard Time":               "Europe/Kiev",
	"Israel Standard Time":            "Asia/Jerusalem",
	"Turkey Standard Time":            "Europe/Istanbul",
	"Arabic Standard Time":            "Asia/Baghdad",
	"Arab Standard Time":              "Asia/Riyadh",
	"Russian Standard Time":           "Europe/Moscow",
	"E. Africa Standard Time":         "Africa/Nairobi",
	"Iran Standard Time":              "Asia/Tehran",
	"Arabian Standard Time":           "Asia/Dubai",
	"Afghanistan Standard Time":       "Asia/Kabul",
	"Pakistan Standard Time":          "Asia/Karachi",
	"West Asia Standard Time":         "Asia/Tashkent",
	"India Standard Time":             "Asia/Calcutta",
	"Nepal Standard Time":             "Asia/Katmandu",
	"Bangladesh Standard Time":        "Asia/Dhaka",
	"SE Asia Standard Time":           "Asia/Bangkok",
	"China Standard Time":             "Asia/Shanghai",
	"Singapore Standard Time":         "Asia/Singapore",
	"W. Australia Standard Time":      "Australia/Perth",
	"Taipei Standard Time":            "Asia/Taipei",
	"Tokyo Standard Time":             "Asia/Tokyo",
	"Korea Standard Time":             "Asia/Seoul",
	"Cen. Australia Standard Time":    "Australia/Adelaide",
	"AUS Central Standard Time":       "Australia/Darwin",
	"E. Australia Standard Time":      "Australia/Brisbane",
	"AUS Eastern Standard Time":       "Australia/Sydney",
	"Tasmania Standard Time":          "Australia/Hobart",
	"West Pacific Standard Time":      "Pacific/Port_Moresby",
	"Central Pacific Standard Time":   "Pacific/Guadalcanal",
	"New Zealand Standard Time":       "Pacific/Auckland",
	"Tonga Standard Time":             "Pacific/Tongatapu",
}
