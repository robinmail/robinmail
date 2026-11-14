package imap

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/message"
)

// SEARCH (RFC 3501 6.4.4), with the MODSEQ key of RFC 7162 3.1.5, the
// result options of RFC 4731 (ESEARCH) and SAVE of RFC 5182.
//
// What it costs. Flags, UIDs, sizes, the internal date and the modseq are
// in the session. FROM, TO, CC, BCC, SUBJECT and the SENT dates are
// answered from the header fields the store remembers for every message,
// one read for all messages. HEADER, BODY and TEXT have no index: each
// candidate message is read and decoded, so a body search over a large
// mailbox reads the whole mailbox. Keys are evaluated left to right and a
// message is only opened when the cheaper keys have not ruled it out.

// searchKey is one node of a search program.
type searchKey struct {
	op   string
	kids []*searchKey // AND: all; OR: two; NOT: one
	str  string       // the text looked for, lowercase; or the keyword
	name string       // HEADER: the field name
	num  int64
	date time.Time
	set  seqSet
}

func (k *searchKey) walk(f func(*searchKey)) {
	f(k)
	for _, kid := range k.kids {
		kid.walk(f)
	}
}

// searchKeys reads keys up to the end of the command or a ")": together
// they are an AND.
func (p *parser) searchKeys(depth int) (*searchKey, error) {
	and := &searchKey{op: "AND"}
	for {
		k, err := p.searchKey(depth)
		if err != nil {
			return nil, err
		}
		if and.kids = append(and.kids, k); len(and.kids) > maxListItems {
			return nil, bad("too many search keys")
		}
		if p.peek() != ' ' {
			return and, nil
		}
		p.pos++
	}
}

func (p *parser) searchKey(depth int) (*searchKey, error) {
	if depth > maxSearchDepth {
		return nil, bad("the search is nested too deep")
	}
	c := p.peek()
	switch {
	case c == '(':
		p.pos++
		k, err := p.searchKeys(depth + 1)
		if err != nil {
			return nil, err
		}
		return k, p.expect(')')
	case (c >= '0' && c <= '9') || c == '*' || c == '$':
		set, err := p.seqSet()
		return &searchKey{op: "SEQ", set: set}, err
	}
	op, err := p.word()
	if err != nil {
		return nil, bad("a search key is missing")
	}
	k := &searchKey{op: op}
	arg := func() error {
		if err := p.space(); err != nil {
			return err
		}
		s, err := p.astring()
		k.str = strings.ToLower(s)
		return err
	}
	switch op {
	case "ALL", "ANSWERED", "DELETED", "DRAFT", "FLAGGED", "NEW", "OLD", "RECENT", "SEEN",
		"UNANSWERED", "UNDELETED", "UNDRAFT", "UNFLAGGED", "UNSEEN":
		return k, nil
	case "BCC", "BODY", "CC", "FROM", "SUBJECT", "TEXT", "TO":
		return k, arg()
	case "KEYWORD", "UNKEYWORD":
		if err := p.space(); err != nil {
			return nil, err
		}
		f, err := p.flag()
		if err != nil {
			return nil, err
		}
		kw, err := toKeywords([]string{f})
		if err != nil || len(kw) != 1 {
			return nil, bad("not a keyword")
		}
		k.str = kw[0]
		return k, nil
	case "BEFORE", "ON", "SINCE", "SENTBEFORE", "SENTON", "SENTSINCE":
		if err := p.space(); err != nil {
			return nil, err
		}
		k.date, err = p.date()
		return k, err
	case "LARGER", "SMALLER":
		if err := p.space(); err != nil {
			return nil, err
		}
		k.num, err = p.number64()
		return k, err
	case "HEADER":
		if err := p.space(); err != nil {
			return nil, err
		}
		if k.name, err = p.astring(); err != nil {
			return nil, err
		}
		return k, arg()
	case "UID":
		if err := p.space(); err != nil {
			return nil, err
		}
		k.set, err = p.seqSet()
		return k, err
	case "NOT":
		if err := p.space(); err != nil {
			return nil, err
		}
		kid, err := p.searchKey(depth + 1)
		k.kids = []*searchKey{kid}
		return k, err
	case "OR":
		for i := 0; i < 2; i++ {
			if err := p.space(); err != nil {
				return nil, err
			}
			kid, err := p.searchKey(depth + 1)
			if err != nil {
				return nil, err
			}
			k.kids = append(k.kids, kid)
		}
		return k, nil
	case "MODSEQ":
		// RFC 7162 3.1.5: MODSEQ [entry-name entry-type] value. The entry
		// (a flag's own modseq) is read and not used: a message has one
		// modseq here.
		if err := p.space(); err != nil {
			return nil, err
		}
		if p.peek() == '"' {
			if _, err := p.quoted(); err != nil {
				return nil, err
			}
			if err := p.space(); err != nil {
				return nil, err
			}
			if w, _ := p.word(); w != "PRIV" && w != "SHARED" && w != "ALL" {
				return nil, bad("MODSEQ entry type is priv, shared or all")
			}
			if err := p.space(); err != nil {
				return nil, err
			}
		}
		k.num, err = p.number64()
		return k, err
	}
	return nil, bad("unknown search key %s", clip(op, 30))
}

// searcher evaluates a search program over the messages of the session.
type searcher struct {
	c         *conn
	summaries map[int64]Summary
	// The message being looked at, opened at most once.
	rawFor int
	root   *entity
	parsed *message.Message
	rawErr error
}

func (s *searcher) open(i int) (*entity, error) {
	if s.rawFor == i+1 {
		return s.root, s.rawErr
	}
	s.rawFor, s.root, s.parsed = i+1, nil, nil
	raw, err := s.c.user.Open(s.c.ctx, s.c.sel.msgs[i].id)
	s.rawErr = err
	if err == nil {
		s.root = parseEntity(raw, 0, len(raw), 0, "text/plain")
	}
	return s.root, err
}

func containsFold(haystack, lowerNeedle string) bool {
	return strings.Contains(strings.ToLower(haystack), lowerNeedle)
}

// headerText is the decoded text of every field with the name.
func headerText(e *entity, name string) (texts []string) {
	for _, v := range message.All(e.hdr, name) {
		texts = append(texts, message.Text(v))
	}
	return texts
}

// field matches a header field that the store remembers.
func (s *searcher) field(i int, name, remembered string, partial bool, needle string) (bool, error) {
	if containsFold(remembered, needle) {
		return true, nil
	}
	// What is remembered is the field as text. Cut short it may lack
	// the match; with comments or quoting in it, an address may be
	// written in pieces ("<user (the) @ example.org>"). Then the message
	// decides, with its addresses read as addresses.
	if !partial && !strings.ContainsAny(remembered, "(\"\\") {
		return false, nil
	}
	e, err := s.open(i)
	if err != nil {
		return false, err
	}
	for _, v := range message.All(e.hdr, name) {
		if containsFold(message.Text(v), needle) {
			return true, nil
		}
		if name == "Subject" {
			continue
		}
		for _, a := range message.Addresses(v) {
			if containsFold(a.Email, needle) || (a.Name != nil && containsFold(*a.Name, needle)) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *searcher) bodyHas(i int, needle string) (bool, error) {
	e, err := s.open(i)
	if err != nil {
		return false, err
	}
	if bytes.Contains(bytes.ToLower(e.body()), []byte(needle)) {
		return true, nil
	}
	// As written it is not there; it may be inside a transfer encoding
	// or in another charset.
	if s.parsed == nil {
		s.parsed = message.Parse(e.raw)
	}
	for _, leaf := range s.parsed.Leaves {
		if !strings.HasPrefix(leaf.Type, "text/") {
			continue
		}
		if text, _ := leaf.Text(); containsFold(text, needle) {
			return true, nil
		}
	}
	return false, nil
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// day is a time's date at midnight UTC: SEARCH compares dates
// "disregarding time and timezone" (RFC 3501 6.4.4).
func day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func (s *searcher) match(k *searchKey, i int) (bool, error) {
	sel := s.c.sel
	m := sel.msgs[i]
	flag := func(kw string) bool { return hasFlag(m.flags, kw) }
	sum := s.summaries[m.id]
	switch k.op {
	case "AND":
		for _, kid := range k.kids {
			if ok, err := s.match(kid, i); !ok || err != nil {
				return false, err
			}
		}
		return true, nil
	case "OR":
		for _, kid := range k.kids {
			if ok, err := s.match(kid, i); ok || err != nil {
				return ok, err
			}
		}
		return false, nil
	case "NOT":
		ok, err := s.match(k.kids[0], i)
		return !ok, err
	case "ALL":
		return true, nil
	case "SEQ":
		// A number above the count names no message here; it is not an
		// error as in FETCH (the example of RFC 4731 3.1 searches 1:5000).
		if k.set.Saved {
			for _, u := range s.c.saved {
				if u == m.uid {
					return true, nil
				}
			}
			return false, nil
		}
		return inRanges(k.set.resolve(uint32(len(sel.msgs))), uint32(i+1)), nil
	case "UID":
		if k.set.Saved {
			return s.match(&searchKey{op: "SEQ", set: k.set}, i)
		}
		return inRanges(k.set.resolve(sel.msgs[len(sel.msgs)-1].uid), m.uid), nil
	case "ANSWERED":
		return flag("$answered"), nil
	case "UNANSWERED":
		return !flag("$answered"), nil
	case "DELETED":
		return flag("$deleted"), nil
	case "UNDELETED":
		return !flag("$deleted"), nil
	case "DRAFT":
		return flag("$draft"), nil
	case "UNDRAFT":
		return !flag("$draft"), nil
	case "FLAGGED":
		return flag("$flagged"), nil
	case "UNFLAGGED":
		return !flag("$flagged"), nil
	case "SEEN":
		return flag("$seen"), nil
	case "UNSEEN":
		return !flag("$seen"), nil
	case "KEYWORD":
		return flag(k.str), nil
	case "UNKEYWORD":
		return !flag(k.str), nil
	case "NEW", "RECENT":
		return false, nil // no message is \Recent
	case "OLD":
		return true, nil
	case "LARGER":
		return m.size > k.num, nil
	case "SMALLER":
		return m.size < k.num, nil
	case "BEFORE":
		return day(m.date).Before(k.date), nil
	case "ON":
		return sameDay(day(m.date), k.date), nil
	case "SINCE":
		return !day(m.date).Before(k.date), nil
	case "SENTBEFORE", "SENTON", "SENTSINCE":
		// The date of the Date field as it is written there,
		// "disregarding time and timezone". The store remembers the
		// moment, not the zone; the date written is that moment's UTC
		// date or the day before or after. Only when those three do not
		// agree is the message opened to read the field.
		test := func(d time.Time) bool {
			switch k.op {
			case "SENTBEFORE":
				return d.Before(k.date)
			case "SENTON":
				return sameDay(d, k.date)
			}
			return !d.Before(k.date)
		}
		if sent := sum.SentAt; !sent.IsZero() {
			d := day(sent)
			if r := test(d); r == test(d.AddDate(0, 0, -1)) && r == test(d.AddDate(0, 0, 1)) {
				return r, nil
			}
		} else if !sum.Partial {
			return false, nil // no Date field that parses
		}
		e, err := s.open(i)
		if err != nil {
			return false, err
		}
		v, ok := e.get("Date")
		if !ok {
			return false, nil
		}
		t, ok := message.Date(v)
		if !ok {
			return false, nil
		}
		y, mo, d := t.Date()
		return test(time.Date(y, mo, d, 0, 0, 0, 0, time.UTC)), nil
	case "MODSEQ":
		return m.modseq >= k.num, nil
	case "FROM":
		return s.field(i, "From", sum.From, sum.Partial, k.str)
	case "TO":
		return s.field(i, "To", sum.To, sum.Partial, k.str)
	case "CC":
		return s.field(i, "Cc", sum.Cc, sum.Partial, k.str)
	case "BCC":
		return s.field(i, "Bcc", sum.Bcc, sum.Partial, k.str)
	case "SUBJECT":
		return s.field(i, "Subject", sum.Subject, sum.Partial, k.str)
	case "HEADER":
		e, err := s.open(i)
		if err != nil {
			return false, err
		}
		for _, t := range headerText(e, k.name) {
			// RFC 3501 6.4.4: an empty string matches every message that
			// has the field.
			if k.str == "" || containsFold(t, k.str) {
				return true, nil
			}
		}
		return false, nil
	case "BODY":
		return s.bodyHas(i, k.str)
	case "TEXT":
		e, err := s.open(i)
		if err != nil {
			return false, err
		}
		if bytes.Contains(bytes.ToLower(e.header()), []byte(k.str)) {
			return true, nil
		}
		for _, h := range e.hdr {
			if containsFold(message.Text(h.Value), k.str) {
				return true, nil
			}
		}
		return s.bodyHas(i, k.str)
	}
	return false, nil
}

// cmdSearch is SEARCH and UID SEARCH.
func (c *conn) cmdSearch(tag string, p *parser, uid bool) (reply, error) {
	name := "SEARCH"
	if uid {
		name = "UID SEARCH"
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	// RFC 4731 3.1: RETURN (options) comes first.
	var opts map[string]bool
	if strings.HasPrefix(strings.ToUpper(p.cur()), "RETURN ") {
		p.pos += len("RETURN ")
		if err := p.expect('('); err != nil {
			return reply{}, err
		}
		opts = map[string]bool{}
		for n := 0; !p.skip(')'); n++ {
			if n > 0 {
				if err := p.space(); err != nil {
					return reply{}, err
				}
			}
			o, err := p.word()
			if err != nil {
				return reply{}, err
			}
			switch o {
			case "MIN", "MAX", "ALL", "COUNT", "SAVE":
				opts[o] = true
			default:
				return reply{}, bad("unknown SEARCH return option %s", o)
			}
		}
		if err := p.space(); err != nil {
			return reply{}, err
		}
	}
	if strings.HasPrefix(strings.ToUpper(p.cur()), "CHARSET ") {
		p.pos += len("CHARSET ")
		cs, err := p.astring()
		if err != nil {
			return reply{}, err
		}
		if err := p.space(); err != nil {
			return reply{}, err
		}
		if cs = strings.ToUpper(cs); cs != "UTF-8" && cs != "US-ASCII" {
			// RFC 3501 6.4.4.
			return noCode("BADCHARSET (US-ASCII UTF-8)", "that charset is not supported"), nil
		}
		p.utf8 = true // the strings of this search may be UTF-8
	}
	prog, err := p.searchKeys(0)
	if err != nil {
		return reply{}, err
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	// RFC 5182 2.1: a search that fails leaves "$" empty. One that uses
	// "$" needs a result to be there.
	var usesSaved, usesModseq, needSummary bool
	prog.walk(func(k *searchKey) {
		switch k.op {
		case "SEQ", "UID":
			usesSaved = usesSaved || k.set.Saved
		case "MODSEQ":
			usesModseq = true
		case "FROM", "TO", "CC", "BCC", "SUBJECT", "SENTBEFORE", "SENTON", "SENTSINCE":
			needSummary = true
		}
	})
	if usesSaved && !c.savedValid {
		return reply{}, bad("there is no saved search result")
	}
	if usesModseq {
		c.enableCondstore()
	}
	s := c.sel
	sr := &searcher{c: c}
	if needSummary && len(s.msgs) > 0 {
		ids := make([]int64, 0, len(s.msgs))
		for _, m := range s.msgs {
			ids = append(ids, m.id)
		}
		if sr.summaries, err = c.user.Summaries(c.ctx, ids); err != nil {
			return reply{}, err
		}
	}
	var hits []int
	var highest int64
	// A message that another session expunged and that this client has
	// not been told about yet (no EXPUNGE may be sent during SEARCH) is
	// still a message of the mailbox for the client. It is searched like
	// the others by what the session holds: its number, UID, flags, size
	// and dates. Its header and body can only be searched while the
	// message still exists somewhere; one that is destroyed matches no
	// key that needs them.
	for i, m := range s.msgs {
		ok, err := sr.match(prog, i)
		if errors.Is(err, ErrNoMessage) {
			s.dirty = true
			continue
		}
		if err != nil {
			if opts["SAVE"] {
				c.saved, c.savedValid = nil, true
			}
			return reply{}, err
		}
		if ok {
			hits = append(hits, i)
			highest = max(highest, m.modseq)
		}
	}
	nums := make([]uint32, len(hits))
	uids := make([]uint32, len(hits))
	for k, i := range hits {
		uids[k] = s.msgs[i].uid
		nums[k] = uint32(i + 1)
		if uid {
			nums[k] = uids[k]
		}
	}

	if opts == nil && !c.rev2 {
		// RFC 3501 7.2.5, and RFC 7162 3.1.5 for the MODSEQ at the end.
		var b strings.Builder
		b.WriteString("* SEARCH")
		for _, n := range nums {
			b.WriteByte(' ')
			b.WriteString(strconv.FormatUint(uint64(n), 10))
		}
		if usesModseq && len(nums) > 0 {
			b.WriteString(" (MODSEQ " + strconv.FormatInt(highest, 10) + ")")
		}
		c.printf("%s", b.String())
		return ok(name + " completed"), nil
	}

	// ESEARCH (RFC 4731 3.1; RFC 9051 7.3.4, where it is the only form).
	if opts == nil || (len(opts) == 0) {
		opts = map[string]bool{"ALL": true}
	}
	if opts["SAVE"] {
		// RFC 5182 2.4: what "$" holds depends on the other options.
		switch {
		case len(uids) == 0:
			c.saved = nil
		case opts["ALL"] || opts["COUNT"] || (!opts["MIN"] && !opts["MAX"]):
			c.saved = uids
		default:
			c.saved = nil
			if opts["MIN"] {
				c.saved = append(c.saved, uids[0])
			}
			if opts["MAX"] && (!opts["MIN"] || len(uids) > 1) {
				c.saved = append(c.saved, uids[len(uids)-1])
			}
		}
		c.savedValid = true
	}
	var b strings.Builder
	b.WriteString(`* ESEARCH (TAG ` + quote(tag, false) + `)`)
	if uid {
		b.WriteString(" UID")
	}
	if len(nums) > 0 {
		if opts["MIN"] {
			b.WriteString(" MIN " + strconv.FormatUint(uint64(nums[0]), 10))
		}
		if opts["MAX"] {
			b.WriteString(" MAX " + strconv.FormatUint(uint64(nums[len(nums)-1]), 10))
		}
		if opts["ALL"] {
			b.WriteString(" ALL " + formatSet(nums))
		}
	}
	if opts["COUNT"] {
		b.WriteString(" COUNT " + strconv.Itoa(len(nums)))
	}
	if usesModseq && len(nums) > 0 {
		// RFC 7162 3.1.5 (and RFC 4731 3.2): with only MIN, MAX or both,
		// the MODSEQ is that of those messages, not of all that matched.
		if !opts["ALL"] && !opts["COUNT"] && (opts["MIN"] || opts["MAX"]) {
			highest = 0
			if opts["MIN"] {
				highest = s.msgs[hits[0]].modseq
			}
			if opts["MAX"] {
				highest = max(highest, s.msgs[hits[len(hits)-1]].modseq)
			}
		}
		b.WriteString(" MODSEQ " + strconv.FormatInt(highest, 10))
	}
	// RFC 5182 2.1: with only SAVE nothing needs to be returned.
	if !(len(opts) == 1 && opts["SAVE"]) {
		c.printf("%s", b.String())
	}
	return ok(name + " completed"), nil
}
