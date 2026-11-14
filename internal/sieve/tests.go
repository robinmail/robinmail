package sieve

import (
	"strconv"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/message"
)

// test evaluates one test.
func (rt *runtime) test(t *test) (bool, error) {
	if err := rt.step(t.line); err != nil {
		return false, err
	}
	m := matcher{match: t.match, relation: t.relation, comparator: t.comparator}
	keys := func() []string { return rt.xs(t.keys) }
	// done sets the match variables after a :matches that held (RFC 5229
	// 3.2), and passes the answer on.
	done := func(ok bool, vars []string) (bool, error) {
		if ok && t.match == "matches" && rt.s.usesVariables() {
			if len(vars) > 10 {
				vars = vars[:10]
			}
			rt.matches = vars
		}
		return ok, nil
	}
	switch t.name {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "not":
		ok, err := rt.test(t.kids[0])
		return !ok, err
	case "allof":
		for _, kid := range t.kids {
			if ok, err := rt.test(kid); !ok || err != nil {
				return false, err
			}
		}
		return true, nil
	case "anyof":
		for _, kid := range t.kids {
			if ok, err := rt.test(kid); ok || err != nil {
				return ok, err
			}
		}
		return false, nil
	case "exists":
		// RFC 5228 5.5: true if all of the named fields are there.
		for _, name := range rt.xs(t.headers) {
			if len(rt.fields(name, 0, false)) == 0 {
				return false, nil
			}
		}
		return true, nil
	case "size":
		// RFC 5228 5.9: neither is true for a message of exactly the size.
		size := int64(len(rt.env.Raw))
		if t.over {
			return size > t.limit, nil
		}
		return size < t.limit, nil
	case "header":
		var values []string
		for _, name := range rt.xs(t.headers) {
			for _, v := range rt.fields(name, t.index, t.last) {
				values = append(values, strings.TrimSpace(message.Text(v)))
			}
		}
		return done(m.any(values, keys()))
	case "address":
		var values []string
		for _, name := range rt.xs(t.headers) {
			for _, v := range rt.fields(name, t.index, t.last) {
				for _, a := range message.Addresses(v) {
					if part, ok := addressPart(a.Email, t.part); ok {
						values = append(values, part)
					}
				}
			}
		}
		return done(m.any(values, keys()))
	case "envelope":
		var values []string
		for _, which := range t.headers {
			addr := rt.env.From
			if which == "to" {
				addr = rt.env.To
			}
			if addr == "" {
				// RFC 5228 5.4: the null reverse-path is the empty
				// string, whatever part is asked for.
				values = append(values, "")
			} else if part, ok := addressPart(addr, t.part); ok {
				values = append(values, part)
			}
		}
		return done(m.any(values, keys()))
	case "body":
		// RFC 5173 5: a body test sets no match variables.
		ok, _ := m.any(rt.bodies(t), keys())
		return ok, nil
	case "string":
		return done(m.any(rt.xs(t.sources), keys()))
	case "hasflag":
		have := rt.flags
		if len(t.sources) > 0 {
			have = nil
			for _, name := range t.sources {
				have = append(have, strings.Fields(rt.vars[name])...)
			}
		}
		// RFC 5232 4: a key may hold several flags; each stands alone.
		return done(m.any(have, splitFlags(keys())))
	case "mailboxexists":
		// RFC 5490 3.1: true if all of them exist.
		for _, name := range keys() {
			if rt.env.MailboxExists == nil || !rt.env.MailboxExists(name) {
				return false, nil
			}
		}
		return true, nil
	case "date":
		var values []string
		for _, v := range rt.fields(rt.x(t.headers[0]), t.index, t.last) {
			when, ok := headerDate(v)
			if !ok {
				continue
			}
			if !t.originalZone {
				when = when.In(rt.zone(t.zone))
			}
			values = append(values, datePart(when, t.datePart))
		}
		return done(m.any(values, keys()))
	case "currentdate":
		return done(m.any([]string{datePart(rt.env.Now.In(rt.zone(t.zone)), t.datePart)}, keys()))
	}
	return false, nil
}

// fields returns the raw values of the header fields with a name; with an
// index only the n-th of them, counted from the end with last (RFC 5260 6).
func (rt *runtime) fields(name string, index int64, last bool) []string {
	all := message.All(rt.hdr, strings.TrimSpace(name))
	if index == 0 {
		return all
	}
	if index > int64(len(all)) {
		return nil
	}
	if last {
		return all[int64(len(all))-index : int64(len(all))-index+1]
	}
	return all[index-1 : index]
}

// addressPart is the part of an address a test compares (RFC 5228 2.7.4,
// RFC 5233 4). ok is false when the address has no such part: :detail of
// an address without a "+".
func addressPart(addr, part string) (string, bool) {
	at := strings.LastIndexByte(addr, '@')
	local, domain := addr, ""
	if at >= 0 {
		local, domain = addr[:at], addr[at+1:]
	}
	switch part {
	case "localpart":
		return local, true
	case "domain":
		return domain, true
	case "user":
		user, _, _ := strings.Cut(local, "+")
		return user, true
	case "detail":
		_, detail, ok := strings.Cut(local, "+")
		return detail, ok
	}
	return addr, true
}

// bodies are the values a body test looks at (RFC 5173 5): the body as it
// was sent (:raw), the text of the text parts (:text), or the decoded
// content of the parts of the named types (:content).
func (rt *runtime) bodies(t *test) []string {
	if t.transform == "raw" {
		raw := rt.env.Raw
		// The body starts after the empty line that ends the header.
		for _, sep := range []string{"\r\n\r\n", "\n\n"} {
			if i := strings.Index(string(raw[:min(len(raw), message.MaxHeaderBytes)]), sep); i >= 0 {
				return []string{cut(string(raw[i+len(sep):]), maxBodyValue)}
			}
		}
		return []string{""}
	}
	if rt.msg == nil {
		rt.msg = message.Parse(rt.env.Raw)
	}
	var out []string
	for _, leaf := range rt.msg.Leaves {
		isText := strings.HasPrefix(leaf.Type, "text/")
		if t.transform == "text" {
			if isText {
				out = append(out, message.PartText(leaf, maxBodyValue))
			}
			continue
		}
		for _, want := range rt.xs(t.contentTypes) {
			want = strings.ToLower(strings.TrimSpace(want))
			// "text" is every text type, "text/html" that one, "" all.
			typ, sub, _ := strings.Cut(leaf.Type, "/")
			wt, ws, both := strings.Cut(want, "/")
			if want != "" && (wt != typ || (both && ws != sub)) {
				continue
			}
			if isText {
				text, _ := leaf.Text()
				out = append(out, cut(text, maxBodyValue))
			} else {
				out = append(out, cut(strings.ToValidUTF8(string(leaf.Content()), "�"), maxBodyValue))
			}
			break
		}
	}
	return out
}

func (rt *runtime) zone(zone string) *time.Location {
	if zone != "" {
		secs, _ := parseZone(zone)
		return time.FixedZone(zone, secs)
	}
	if rt.env.Zone != nil {
		return rt.env.Zone
	}
	return rt.env.Now.Location()
}

// headerDate reads the date of a header field (RFC 5260 4.1): the field
// itself, or for a field like Received what follows its last semicolon.
func headerDate(v string) (time.Time, bool) {
	if when, ok := message.Date(v); ok {
		return when, true
	}
	if i := strings.LastIndexByte(v, ';'); i >= 0 {
		return message.Date(v[i+1:])
	}
	return time.Time{}, false
}

// datePart formats one part of a date (RFC 5260 4.2).
func datePart(t time.Time, part string) string {
	two := func(n int) string { return strconv.Itoa(100 + n)[1:] }
	_, offset := t.Zone()
	sign := "+"
	if offset < 0 {
		sign, offset = "-", -offset
	}
	zone := sign + two(offset/3600) + two(offset%3600/60)
	switch part {
	case "year":
		return strconv.Itoa(10000 + t.Year()%10000)[1:]
	case "month":
		return two(int(t.Month()))
	case "day":
		return two(t.Day())
	case "date":
		return t.Format("2006-01-02")
	case "julian":
		// The Modified Julian Day: days since 1858-11-17.
		days := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
		return strconv.FormatInt(days+40587, 10)
	case "hour":
		return two(t.Hour())
	case "minute":
		return two(t.Minute())
	case "second":
		return two(t.Second())
	case "time":
		return t.Format("15:04:05")
	case "iso8601":
		return t.Format("2006-01-02T15:04:05") + zone[:3] + ":" + zone[3:]
	case "std11":
		return t.Format("Mon, 2 Jan 2006 15:04:05 ") + zone
	case "zone":
		return zone
	case "weekday":
		return strconv.Itoa(int(t.Weekday()))
	}
	return ""
}
