// Package jscontact converts contact cards between vCard (RFC 6350, and
// version 3.0 of RFC 2426 that Apple's programs write) and JSContact
// (RFC 9553), as RFC 9555 describes, for the properties JMAP for Contacts
// (RFC 9610) clients use.
//
// The stored form is the vCard a client sent. An update from JSContact
// rewrites the properties this package knows and keeps the others, groups
// and labels (item1.X-ABLabel) included.
package jscontact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
)

// Card is a JSContact Card as JSON.
type Card = map[string]any

func shortID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:6])
}

// labels maps the groups of a vCard to their X-ABLabel.
func labels(v *ical.Component) map[string]string {
	out := map[string]string{}
	for _, p := range v.All("X-ABLABEL") {
		if p.Group != "" {
			l := p.Text()
			// Apple writes its own labels as _$!<Work>!$_.
			l = strings.TrimSuffix(strings.TrimPrefix(l, "_$!<"), ">!$_")
			out[strings.ToLower(p.Group)] = l
		}
	}
	return out
}

func types(p *ical.Prop) map[string]bool {
	out := map[string]bool{}
	for _, t := range p.ParamValues("TYPE") {
		for _, x := range strings.Split(t, ",") {
			out[strings.ToLower(strings.TrimSpace(x))] = true
		}
	}
	return out
}

func contexts(t map[string]bool) map[string]any {
	c := map[string]any{}
	if t["work"] {
		c["work"] = true
	}
	if t["home"] {
		c["private"] = true
	}
	return c
}

func pref(p *ical.Prop, t map[string]bool) int {
	if s := p.Param("PREF"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= 100 {
			return n
		}
	}
	if t["pref"] {
		return 1
	}
	return 0
}

// ToJS converts a VCARD to a JSContact Card.
func ToJS(v *ical.Component) Card {
	c := Card{"@type": "Card", "version": "1.0"}
	lab := labels(v)
	label := func(p *ical.Prop, m map[string]any) {
		if l, ok := lab[strings.ToLower(p.Group)]; ok && p.Group != "" {
			m["label"] = l
		}
	}
	if uid := v.Text("UID"); uid != "" {
		c["uid"] = uid
	}
	kind := strings.ToLower(v.Value("KIND"))
	if kind == "" {
		kind = strings.ToLower(v.Value("X-ADDRESSBOOKSERVER-KIND"))
	}
	if kind != "" && kind != "individual" {
		c["kind"] = kind
	}
	members := map[string]any{}
	for _, name := range []string{"MEMBER", "X-ADDRESSBOOKSERVER-MEMBER"} {
		for _, p := range v.All(name) {
			members[strings.TrimPrefix(p.Value, "urn:uuid:")] = true
		}
	}
	if len(members) > 0 {
		c["members"] = members
	}
	for prop, key := range map[string]string{"REV": "updated", "CREATED": "created"} {
		if p := v.Get(prop); p != nil {
			if t, ok := parseStamp(p.Value); ok {
				c[key] = t.UTC().Format("2006-01-02T15:04:05Z")
			}
		}
	}
	name := map[string]any{"@type": "Name"}
	if p := v.Get("N"); p != nil {
		parts := ical.SplitRaw(p.Value, ';')
		kinds := []string{"surname", "given", "given2", "title", "credential", "surname2", "generation"}
		var comps []any
		for i, part := range parts {
			if i >= len(kinds) {
				break
			}
			for _, x := range ical.TextList(part) {
				if x != "" {
					comps = append(comps, map[string]any{"@type": "NameComponent", "kind": kinds[i], "value": x})
				}
			}
		}
		if len(comps) > 0 {
			// RFC 9553 2.2.1.2: the order a person's name is written in;
			// given name first is the order of most cards here.
			sort.SliceStable(comps, func(i, j int) bool {
				return nameOrder(comps[i].(map[string]any)["kind"].(string)) < nameOrder(comps[j].(map[string]any)["kind"].(string))
			})
			name["components"] = comps
			name["isOrdered"] = true
		}
	}
	if fn := v.Text("FN"); fn != "" {
		name["full"] = fn
	}
	if len(name) > 1 {
		c["name"] = name
	}
	nick := map[string]any{}
	for _, p := range v.All("NICKNAME") {
		for _, x := range ical.TextList(p.Value) {
			if x != "" {
				nick[shortID("nick", x)] = map[string]any{"@type": "Nickname", "name": x}
			}
		}
	}
	if len(nick) > 0 {
		c["nicknames"] = nick
	}
	orgs := map[string]any{}
	for _, p := range v.All("ORG") {
		parts := ical.SplitRaw(p.Value, ';')
		o := map[string]any{"@type": "Organization"}
		if n := ical.Unescape(parts[0]); n != "" {
			o["name"] = n
		}
		var units []any
		for _, u := range parts[1:] {
			if u = ical.Unescape(u); u != "" {
				units = append(units, map[string]any{"@type": "OrgUnit", "name": u})
			}
		}
		if len(units) > 0 {
			o["units"] = units
		}
		if len(o) > 1 {
			orgs[shortID("org", p.Value)] = o
		}
	}
	if len(orgs) > 0 {
		c["organizations"] = orgs
	}
	titles := map[string]any{}
	for prop, kind := range map[string]string{"TITLE": "title", "ROLE": "role"} {
		for _, p := range v.All(prop) {
			if t := p.Text(); t != "" {
				x := map[string]any{"@type": "Title", "name": t}
				if kind == "role" {
					x["kind"] = "role"
				}
				titles[shortID(kind, t)] = x
			}
		}
	}
	if len(titles) > 0 {
		c["titles"] = titles
	}
	emails := map[string]any{}
	for _, p := range v.All("EMAIL") {
		addr := strings.TrimSpace(p.Text())
		if addr == "" {
			continue
		}
		t := types(p)
		e := map[string]any{"@type": "EmailAddress", "address": addr}
		if cx := contexts(t); len(cx) > 0 {
			e["contexts"] = cx
		}
		if n := pref(p, t); n > 0 {
			e["pref"] = n
		}
		label(p, e)
		emails[shortID("email", strings.ToLower(addr))] = e
	}
	if len(emails) > 0 {
		c["emails"] = emails
	}
	phones := map[string]any{}
	for _, p := range v.All("TEL") {
		num := strings.TrimSpace(p.Text())
		if num == "" {
			continue
		}
		t := types(p)
		ph := map[string]any{"@type": "Phone", "number": num}
		feats := map[string]any{}
		for _, f := range []string{"voice", "fax", "pager", "text", "video", "textphone"} {
			if t[f] {
				feats[f] = true
			}
		}
		if t["cell"] || t["iphone"] {
			feats["mobile"] = true
		}
		if len(feats) > 0 {
			ph["features"] = feats
		}
		if cx := contexts(t); len(cx) > 0 {
			ph["contexts"] = cx
		}
		if n := pref(p, t); n > 0 {
			ph["pref"] = n
		}
		label(p, ph)
		phones[shortID("tel", num)] = ph
	}
	if len(phones) > 0 {
		c["phones"] = phones
	}
	addrs := map[string]any{}
	for _, p := range v.All("ADR") {
		parts := ical.SplitRaw(p.Value, ';')
		kinds := []string{"postOfficeBox", "apartment", "name", "locality", "region", "postcode", "country"}
		var comps []any
		for i, part := range parts {
			if i >= len(kinds) {
				break
			}
			if x := ical.Unescape(part); x != "" {
				comps = append(comps, map[string]any{"@type": "AddressComponent", "kind": kinds[i], "value": x})
			}
		}
		a := map[string]any{"@type": "Address"}
		if len(comps) > 0 {
			a["components"] = comps
		}
		if l := p.Param("LABEL"); l != "" {
			a["full"] = l
		}
		if cc := p.Param("CC"); cc != "" {
			a["countryCode"] = cc
		}
		t := types(p)
		if cx := contexts(t); len(cx) > 0 {
			a["contexts"] = cx
		}
		if n := pref(p, t); n > 0 {
			a["pref"] = n
		}
		label(p, a)
		if len(a) > 1 {
			addrs[shortID("adr", p.Value)] = a
		}
	}
	if len(addrs) > 0 {
		c["addresses"] = addrs
	}
	links := map[string]any{}
	for _, p := range v.All("URL") {
		l := map[string]any{"@type": "Link", "uri": p.Value}
		label(p, l)
		links[shortID("url", p.Value)] = l
	}
	if len(links) > 0 {
		c["links"] = links
	}
	online := map[string]any{}
	for _, p := range v.All("IMPP") {
		s := map[string]any{"@type": "OnlineService", "uri": p.Value}
		if svc := p.Param("X-SERVICE-TYPE"); svc != "" {
			s["service"] = svc
		}
		online[shortID("impp", p.Value)] = s
	}
	for _, p := range v.All("X-SOCIALPROFILE") {
		s := map[string]any{"@type": "OnlineService", "uri": p.Value}
		if t := p.Param("TYPE"); t != "" {
			s["service"] = t
		}
		online[shortID("social", p.Value)] = s
	}
	if len(online) > 0 {
		c["onlineServices"] = online
	}
	media := map[string]any{}
	for prop, kind := range map[string]string{"PHOTO": "photo", "LOGO": "logo", "SOUND": "sound"} {
		for _, p := range v.All(prop) {
			uri := p.Value
			if enc := strings.ToLower(p.Param("ENCODING")); enc == "b" || enc == "base64" {
				// vCard 3.0 inline data becomes a data: URI.
				t := strings.ToLower(p.Param("TYPE"))
				if t == "" {
					t = "jpeg"
				}
				uri = "data:image/" + t + ";base64," + strings.Map(func(r rune) rune {
					if r == ' ' || r == '\t' {
						return -1
					}
					return r
				}, p.Value)
			}
			media[shortID(kind, uri)] = map[string]any{"@type": "Media", "kind": kind, "uri": uri}
		}
	}
	if len(media) > 0 {
		c["media"] = media
	}
	ann := map[string]any{}
	for prop, kind := range map[string]string{"BDAY": "birth", "ANNIVERSARY": "wedding", "DEATHDATE": "death", "X-ANNIVERSARY": "wedding"} {
		if p := v.Get(prop); p != nil {
			if d, ok := partialDate(p.Value); ok {
				ann[kind] = map[string]any{"@type": "Anniversary", "kind": kind, "date": d}
			}
		}
	}
	if len(ann) > 0 {
		c["anniversaries"] = ann
	}
	notes := map[string]any{}
	for _, p := range v.All("NOTE") {
		if n := p.Text(); n != "" {
			notes[shortID("note", n)] = map[string]any{"@type": "Note", "note": n}
		}
	}
	if len(notes) > 0 {
		c["notes"] = notes
	}
	kw := map[string]any{}
	for _, p := range v.All("CATEGORIES") {
		for _, x := range ical.TextList(p.Value) {
			if x != "" {
				kw[x] = true
			}
		}
	}
	if len(kw) > 0 {
		c["keywords"] = kw
	}
	langs := map[string]any{}
	for _, p := range v.All("LANG") {
		langs[shortID("lang", p.Value)] = map[string]any{"@type": "LanguagePref", "language": p.Value}
	}
	if len(langs) > 0 {
		c["preferredLanguages"] = langs
	}
	return c
}

func nameOrder(kind string) int {
	switch kind {
	case "title":
		return 0
	case "given":
		return 1
	case "given2":
		return 2
	case "surname":
		return 3
	case "surname2":
		return 4
	case "generation":
		return 5
	case "credential":
		return 6
	}
	return 7
}

// parseStamp reads REV: a timestamp in either basic or extended form.
func parseStamp(s string) (time.Time, bool) {
	for _, f := range []string{"20060102T150405Z", "2006-01-02T15:04:05Z", "20060102T150405Z0700", "2006-01-02T15:04:05Z07:00", "20060102"} {
		if t, err := time.Parse(f, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// partialDate reads a date of BDAY: "19960415", "--0415" (no year),
// "1996-04-15", "1953-10-15T23:10:00Z".
func partialDate(s string) (map[string]any, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, 'T'); i > 0 {
		s = s[:i]
	}
	d := map[string]any{"@type": "PartialDate"}
	num := func(x string) (int, bool) {
		n, err := strconv.Atoi(x)
		return n, err == nil
	}
	switch {
	case strings.HasPrefix(s, "--") && len(s) == 6: // --MMDD
		m, ok1 := num(s[2:4])
		day, ok2 := num(s[4:6])
		if !ok1 || !ok2 {
			return nil, false
		}
		d["month"], d["day"] = m, day
	case len(s) == 8: // YYYYMMDD
		y, ok1 := num(s[0:4])
		m, ok2 := num(s[4:6])
		day, ok3 := num(s[6:8])
		if !ok1 || !ok2 || !ok3 {
			return nil, false
		}
		d["year"], d["month"], d["day"] = y, m, day
	case len(s) == 10 && s[4] == '-': // YYYY-MM-DD
		y, ok1 := num(s[0:4])
		m, ok2 := num(s[5:7])
		day, ok3 := num(s[8:10])
		if !ok1 || !ok2 || !ok3 {
			return nil, false
		}
		d["year"], d["month"], d["day"] = y, m, day
	case len(s) == 4: // YYYY
		y, ok := num(s)
		if !ok {
			return nil, false
		}
		d["year"] = y
	default:
		return nil, false
	}
	if m, ok := d["month"].(int); ok && (m < 1 || m > 12) {
		return nil, false
	}
	if day, ok := d["day"].(int); ok && (day < 1 || day > 31) {
		return nil, false
	}
	return d, true
}

// PropError names invalid properties of a Card.
type PropError struct {
	Props []string
	Msg   string
}

func (e *PropError) Error() string {
	return "invalid properties " + strings.Join(e.Props, ", ") + ": " + e.Msg
}

func bad(prop, format string, args ...any) error {
	return &PropError{Props: []string{prop}, Msg: fmt.Sprintf(format, args...)}
}

// handled are the vCard properties ToVCard writes; a previous card keeps
// every other one.
var handled = []string{"UID", "KIND", "X-ADDRESSBOOKSERVER-KIND", "MEMBER", "X-ADDRESSBOOKSERVER-MEMBER", "REV", "N", "FN",
	"NICKNAME", "ORG", "TITLE", "ROLE", "EMAIL", "TEL", "ADR", "URL", "IMPP", "X-SOCIALPROFILE", "PHOTO", "LOGO", "SOUND",
	"BDAY", "ANNIVERSARY", "DEATHDATE", "X-ANNIVERSARY", "NOTE", "CATEGORIES", "LANG", "CREATED"}

func objOf(c Card, key string) (map[string]any, error) {
	v, ok := c[key]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, bad(key, "not an object")
	}
	return m, nil
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func intOf(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), n == float64(int(n))
	case int:
		return n, true
	}
	return 0, false
}

// ToVCard converts a Card. prev, the stored card, keeps the properties
// this package does not write and its VERSION; a new card is vCard 3.0,
// which every CardDAV client reads (RFC 6352 5.1 requires it).
func ToVCard(c Card, prev *ical.Component, now time.Time) (*ical.Component, error) {
	v := &ical.Component{Name: "VCARD"}
	version := "3.0"
	if prev != nil {
		v = prev.Clone()
		if pv := prev.Value("VERSION"); pv != "" {
			version = pv
		}
		// Groups of the properties written here lose their labels.
		dropped := map[string]bool{}
		for _, p := range v.Props {
			for _, h := range handled {
				if p.Name == h && p.Group != "" {
					dropped[strings.ToLower(p.Group)] = true
				}
			}
		}
		v.Remove(handled...)
		var keep []ical.Prop
		for _, p := range v.Props {
			if p.Name == "X-ABLABEL" && dropped[strings.ToLower(p.Group)] {
				continue
			}
			keep = append(keep, p)
		}
		v.Props = keep
		v.Remove("VERSION", "PRODID")
	}
	v4 := strings.HasPrefix(version, "4")
	var props []ical.Prop
	add := func(p ical.Prop) { props = append(props, p) }
	text := func(name, value string) { add(ical.Prop{Name: name, Value: ical.Escape(value)}) }
	add(ical.Prop{Name: "VERSION", Value: version})
	add(ical.Prop{Name: "PRODID", Value: "-//robinmail//EN"})
	if t, _ := c["@type"].(string); t != "" && t != "Card" {
		return nil, bad("@type", "must be Card")
	}
	uid, _ := c["uid"].(string)
	if uid == "" {
		return nil, bad("uid", "required")
	}
	text("UID", uid)
	group := 0
	labelled := func(p ical.Prop, m map[string]any) {
		if l, _ := m["label"].(string); l != "" {
			group++
			g := "item" + strconv.Itoa(group)
			p.Group = g
			add(p)
			add(ical.Prop{Group: g, Name: "X-ABLABEL", Value: ical.Escape(l)})
			return
		}
		add(p)
	}
	if k, _ := c["kind"].(string); k != "" && k != "individual" {
		switch k {
		case "group", "org", "location", "device", "application":
		default:
			return nil, bad("kind", "unknown value")
		}
		if v4 {
			add(ical.Prop{Name: "KIND", Value: k})
		} else {
			add(ical.Prop{Name: "X-ADDRESSBOOKSERVER-KIND", Value: k})
		}
	}
	if m, err := objOf(c, "members"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			val := k
			if !strings.Contains(k, ":") {
				val = "urn:uuid:" + k
			}
			if v4 {
				add(ical.Prop{Name: "MEMBER", Value: val})
			} else {
				add(ical.Prop{Name: "X-ADDRESSBOOKSERVER-MEMBER", Value: val})
			}
		}
	}
	add(ical.Prop{Name: "REV", Value: ical.FormatUTC(now)})
	if s, _ := c["created"].(string); s != "" && v4 {
		if t, ok := parseStamp(s); ok {
			add(ical.Prop{Name: "CREATED", Value: ical.FormatUTC(t)})
		}
	}
	name, err := objOf(c, "name")
	if err != nil {
		return nil, err
	}
	n := make([][]string, 7)
	full, _ := name["full"].(string)
	var given []string
	if comps, ok := name["components"].([]any); ok {
		idx := map[string]int{"surname": 0, "given": 1, "given2": 2, "title": 3, "credential": 4, "surname2": 5, "generation": 6}
		for i, x := range comps {
			m, ok := x.(map[string]any)
			if !ok {
				return nil, bad("name/components/"+strconv.Itoa(i), "not a NameComponent")
			}
			kind, _ := m["kind"].(string)
			val, _ := m["value"].(string)
			if j, ok := idx[kind]; ok && val != "" {
				n[j] = append(n[j], val)
				given = append(given, val)
			}
		}
	} else if name["components"] != nil {
		return nil, bad("name/components", "not a list")
	}
	parts := make([]string, 5)
	if v4 {
		parts = make([]string, 7)
		if len(n[5]) == 0 && len(n[6]) == 0 {
			parts = parts[:5]
		}
	} else {
		n[0] = append(n[0], n[5]...)
		n[4] = append(n[4], n[6]...)
	}
	for i := range parts {
		parts[i] = ical.JoinText(n[i], ",")
	}
	if full == "" {
		full = strings.Join(given, " ")
	}
	if full == "" {
		// FN is required (RFC 6350 6.2.1): an organization or an address.
		if orgs, _ := objOf(c, "organizations"); len(orgs) > 0 {
			for _, k := range keys(orgs) {
				if o, ok := orgs[k].(map[string]any); ok {
					full, _ = o["name"].(string)
					break
				}
			}
		}
		if emails, _ := objOf(c, "emails"); full == "" && len(emails) > 0 {
			for _, k := range keys(emails) {
				if e, ok := emails[k].(map[string]any); ok {
					full, _ = e["address"].(string)
					break
				}
			}
		}
	}
	text("FN", full)
	add(ical.Prop{Name: "N", Value: strings.Join(parts, ";")})
	if m, err := objOf(c, "nicknames"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			x, _ := m[k].(map[string]any)
			if s, _ := x["name"].(string); s != "" {
				text("NICKNAME", s)
			}
		}
	}
	if m, err := objOf(c, "organizations"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			o, ok := m[k].(map[string]any)
			if !ok {
				return nil, bad("organizations/"+k, "not an Organization")
			}
			s, _ := o["name"].(string)
			vals := []string{ical.Escape(s)}
			if units, ok := o["units"].([]any); ok {
				for _, u := range units {
					if um, ok := u.(map[string]any); ok {
						un, _ := um["name"].(string)
						vals = append(vals, ical.Escape(un))
					}
				}
			}
			add(ical.Prop{Name: "ORG", Value: strings.Join(vals, ";")})
		}
	}
	if m, err := objOf(c, "titles"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			x, _ := m[k].(map[string]any)
			s, _ := x["name"].(string)
			if s == "" {
				continue
			}
			if kind, _ := x["kind"].(string); kind == "role" {
				text("ROLE", s)
			} else {
				text("TITLE", s)
			}
		}
	}
	typeParam := func(p *ical.Prop, m map[string]any, extra ...string) {
		var ts []string
		if cx, ok := m["contexts"].(map[string]any); ok {
			if cx["work"] == true {
				ts = append(ts, "work")
			}
			if cx["private"] == true {
				ts = append(ts, "home")
			}
		}
		ts = append(ts, extra...)
		if pr, ok := intOf(m["pref"]); ok && pr > 0 {
			if v4 {
				p.SetParam("PREF", strconv.Itoa(pr))
			} else {
				ts = append(ts, "pref")
			}
		}
		if len(ts) > 0 {
			p.SetParam("TYPE", ts...)
		}
	}
	if m, err := objOf(c, "emails"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			e, ok := m[k].(map[string]any)
			if !ok {
				return nil, bad("emails/"+k, "not an EmailAddress")
			}
			a, _ := e["address"].(string)
			if a == "" {
				return nil, bad("emails/"+k+"/address", "required")
			}
			p := ical.Prop{Name: "EMAIL", Value: ical.Escape(a)}
			if v4 {
				typeParam(&p, e)
			} else {
				typeParam(&p, e, "internet")
			}
			labelled(p, e)
		}
	}
	if m, err := objOf(c, "phones"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			ph, ok := m[k].(map[string]any)
			if !ok {
				return nil, bad("phones/"+k, "not a Phone")
			}
			num, _ := ph["number"].(string)
			if num == "" {
				return nil, bad("phones/"+k+"/number", "required")
			}
			var feats []string
			if f, ok := ph["features"].(map[string]any); ok {
				for _, x := range keys(f) {
					if x == "mobile" {
						x = "cell"
					}
					feats = append(feats, x)
				}
			}
			p := ical.Prop{Name: "TEL", Value: ical.Escape(num)}
			typeParam(&p, ph, feats...)
			labelled(p, ph)
		}
	}
	if m, err := objOf(c, "addresses"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			a, ok := m[k].(map[string]any)
			if !ok {
				return nil, bad("addresses/"+k, "not an Address")
			}
			slots := make([][]string, 7)
			idx := map[string]int{"postOfficeBox": 0, "apartment": 1, "name": 2, "number": 2, "locality": 3, "region": 4, "postcode": 5, "country": 6}
			if comps, ok := a["components"].([]any); ok {
				// "name" and "number" are the street; in the order given.
				for _, x := range comps {
					cm, _ := x.(map[string]any)
					kind, _ := cm["kind"].(string)
					val, _ := cm["value"].(string)
					if i, ok := idx[kind]; ok && val != "" {
						slots[i] = append(slots[i], val)
					}
				}
			}
			vals := make([]string, 7)
			for i, s := range slots {
				vals[i] = ical.Escape(strings.Join(s, " "))
			}
			p := ical.Prop{Name: "ADR", Value: strings.Join(vals, ";")}
			if f, _ := a["full"].(string); f != "" && v4 {
				p.SetParam("LABEL", f)
			}
			if cc, _ := a["countryCode"].(string); cc != "" && v4 {
				p.SetParam("CC", cc)
			}
			typeParam(&p, a)
			labelled(p, a)
		}
	}
	if m, err := objOf(c, "links"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			l, _ := m[k].(map[string]any)
			if u, _ := l["uri"].(string); u != "" {
				labelled(ical.Prop{Name: "URL", Value: u}, l)
			}
		}
	}
	if m, err := objOf(c, "onlineServices"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			s, _ := m[k].(map[string]any)
			u, _ := s["uri"].(string)
			if u == "" {
				u, _ = s["user"].(string)
			}
			if u == "" {
				continue
			}
			p := ical.Prop{Name: "IMPP", Value: u}
			if svc, _ := s["service"].(string); svc != "" {
				p.SetParam("X-SERVICE-TYPE", svc)
			}
			add(p)
		}
	}
	if m, err := objOf(c, "media"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			x, _ := m[k].(map[string]any)
			u, _ := x["uri"].(string)
			if u == "" {
				continue
			}
			prop := map[string]string{"photo": "PHOTO", "logo": "LOGO", "sound": "SOUND"}[fmt.Sprint(x["kind"])]
			if prop == "" {
				prop = "PHOTO"
			}
			p := ical.Prop{Name: prop, Value: u}
			if !v4 && strings.HasPrefix(u, "data:") {
				// vCard 3.0 has no data: URIs; inline base64 is what it reads.
				meta, data, ok := strings.Cut(u[5:], ",")
				if ok && strings.HasSuffix(meta, ";base64") {
					p.Value = data
					p.SetParam("ENCODING", "b")
					if sub, ok := strings.CutPrefix(strings.TrimSuffix(meta, ";base64"), "image/"); ok {
						p.SetParam("TYPE", strings.ToUpper(sub))
					}
				}
			} else if !v4 {
				p.SetParam("VALUE", "uri")
			}
			add(p)
		}
	}
	if m, err := objOf(c, "anniversaries"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			a, _ := m[k].(map[string]any)
			d, _ := a["date"].(map[string]any)
			if d == nil {
				continue
			}
			var val string
			y, hy := intOf(d["year"])
			mo, hm := intOf(d["month"])
			day, hd := intOf(d["day"])
			if ts, ok := d["utc"].(string); ok {
				if t, ok := parseStamp(ts); ok {
					y, mo, day, hy, hm, hd = t.Year(), int(t.Month()), t.Day(), true, true, true
				}
			}
			switch {
			case hy && hm && hd:
				val = fmt.Sprintf("%04d%02d%02d", y, mo, day)
				if !v4 {
					val = fmt.Sprintf("%04d-%02d-%02d", y, mo, day)
				}
			case hm && hd:
				val = fmt.Sprintf("--%02d%02d", mo, day)
			case hy:
				val = fmt.Sprintf("%04d", y)
			default:
				continue
			}
			switch kind, _ := a["kind"].(string); kind {
			case "birth":
				add(ical.Prop{Name: "BDAY", Value: val})
			case "death":
				if v4 {
					add(ical.Prop{Name: "DEATHDATE", Value: val})
				}
			default:
				if v4 {
					add(ical.Prop{Name: "ANNIVERSARY", Value: val})
				} else {
					add(ical.Prop{Name: "X-ANNIVERSARY", Value: val})
				}
			}
		}
	}
	if m, err := objOf(c, "notes"); err != nil {
		return nil, err
	} else {
		var all []string
		for _, k := range keys(m) {
			x, _ := m[k].(map[string]any)
			if s, _ := x["note"].(string); s != "" {
				all = append(all, s)
			}
		}
		if len(all) > 0 {
			// vCard 3.0 readers show one NOTE.
			text("NOTE", strings.Join(all, "\n\n"))
		}
	}
	if m, err := objOf(c, "keywords"); err != nil {
		return nil, err
	} else if len(m) > 0 {
		add(ical.Prop{Name: "CATEGORIES", Value: ical.JoinText(keys(m), ",")})
	}
	if m, err := objOf(c, "preferredLanguages"); err != nil {
		return nil, err
	} else {
		for _, k := range keys(m) {
			x, _ := m[k].(map[string]any)
			if s, _ := x["language"].(string); s != "" && v4 {
				add(ical.Prop{Name: "LANG", Value: s})
			}
		}
	}
	v.Props = append(props, v.Props...)
	return v, nil
}

// Names returns what a card is known by, for sorting and search: the full
// name, the given name and the surname.
func Names(c Card) (full, given, surname string) {
	name, _ := c["name"].(map[string]any)
	full, _ = name["full"].(string)
	if comps, ok := name["components"].([]any); ok {
		for _, x := range comps {
			m, _ := x.(map[string]any)
			v, _ := m["value"].(string)
			switch m["kind"] {
			case "given":
				if given == "" {
					given = v
				}
			case "surname":
				if surname == "" {
					surname = v
				}
			}
		}
	}
	return
}
