package imap

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

// msg is a message as the session knows it.
type msg struct {
	uid    uint32
	id     int64
	modseq int64
	size   int64
	date   time.Time
	flags  []string // keywords, sorted
	// gone: the message left the mailbox, and the client has not been
	// told yet because no EXPUNGE could be sent.
	gone bool
}

// selected is the mailbox a session has open. msgs is the client's view:
// the message with sequence number n is msgs[n-1].
type selected struct {
	mailbox     int64
	path        string
	readOnly    bool
	uidValidity uint32
	uidNext     uint32
	modseq      int64 // msgs is up to date to this modseq
	msgs        []*msg
	// dirty: something changed, or may have, since msgs was read.
	dirty bool
	// gone counts the messages marked gone.
	gone int
	// keywords are the flags named in the last FLAGS response.
	keywords map[string]bool
}

// The system flags (RFC 3501 2.3.2) and the keywords they are stored as.
// \Recent is not kept: nothing can set it, and IMAP4rev2 dropped it.
var systemFlags = []struct{ flag, keyword string }{
	{`\Answered`, "$answered"}, {`\Flagged`, "$flagged"}, {`\Deleted`, "$deleted"}, {`\Seen`, "$seen"}, {`\Draft`, "$draft"},
}

// Keywords with a registered spelling (the IANA "IMAP and JMAP Keywords"
// registry); they are stored in lowercase.
var knownKeywords = map[string]string{
	"$forwarded": "$Forwarded", "$mdnsent": "$MDNSent", "$junk": "$Junk", "$notjunk": "$NotJunk", "$phishing": "$Phishing",
	"$important": "$Important", "$submitted": "$Submitted", "$submitpending": "$SubmitPending",
}

// wireFlag writes a keyword as IMAP names it.
func wireFlag(keyword string) string {
	for _, f := range systemFlags {
		if f.keyword == keyword {
			return f.flag
		}
	}
	if k, ok := knownKeywords[keyword]; ok {
		return k
	}
	return keyword
}

// wireFlags writes a flag list without its parentheses: system flags
// first, then keywords by name.
func wireFlags(keywords []string) string {
	var out []string
	for _, f := range systemFlags {
		for _, k := range keywords {
			if k == f.keyword {
				out = append(out, f.flag)
			}
		}
	}
	for _, k := range keywords {
		if !isSystemKeyword(k) {
			out = append(out, wireFlag(k))
		}
	}
	return strings.Join(out, " ")
}

func isSystemKeyword(k string) bool {
	for _, f := range systemFlags {
		if f.keyword == k {
			return true
		}
	}
	return false
}

// toKeywords turns the flags of a command into keywords. \Recent cannot be
// set by a client (RFC 3501 2.3.2) and is dropped; any other flag that
// starts with a backslash and is not a system flag is refused.
func toKeywords(flags []string) ([]string, error) {
	out := make([]string, 0, len(flags))
	seen := map[string]bool{}
next:
	for _, f := range flags {
		if strings.HasPrefix(f, `\`) {
			if strings.EqualFold(f, `\Recent`) {
				continue
			}
			for _, sf := range systemFlags {
				if strings.EqualFold(f, sf.flag) {
					if !seen[sf.keyword] {
						seen[sf.keyword] = true
						out = append(out, sf.keyword)
					}
					continue next
				}
			}
			return nil, bad("%s is not a flag", f)
		}
		k := strings.ToLower(f)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out, nil
}

func hasFlag(flags []string, k string) bool {
	for _, f := range flags {
		if f == k {
			return true
		}
	}
	return false
}

func sameFlags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newMsg(m Message) *msg {
	flags := append([]string(nil), m.Flags...)
	sort.Strings(flags)
	return &msg{uid: m.UID, id: m.ID, modseq: m.Modseq, size: m.Size, date: m.InternalDate, flags: flags}
}

// find returns the index of the message with the UID, or -1.
func (s *selected) find(uid uint32) int {
	i := sort.Search(len(s.msgs), func(i int) bool { return s.msgs[i].uid >= uid })
	if i < len(s.msgs) && s.msgs[i].uid == uid {
		return i
	}
	return -1
}

// flagsLine is the FLAGS response: the flags that are in use in the
// mailbox (RFC 3501 7.2.6).
func (s *selected) flagsLine() string {
	names := make([]string, 0, len(systemFlags)+len(s.keywords))
	for _, f := range systemFlags {
		names = append(names, f.flag)
	}
	var kws []string
	for k := range s.keywords {
		kws = append(kws, k)
	}
	sort.Strings(kws)
	for _, k := range kws {
		names = append(names, wireFlag(k))
	}
	return "* FLAGS (" + strings.Join(names, " ") + ")"
}

// noteKeywords adds the keywords of a message to the known ones and
// reports whether there was a new one.
func (s *selected) noteKeywords(flags []string) bool {
	added := false
	for _, k := range flags {
		if !isSystemKeyword(k) && !s.keywords[k] {
			s.keywords[k] = true
			added = true
		}
	}
	return added
}

// flagsFetch is an untagged FETCH with a message's flags.
func (c *conn) flagsFetch(seq int, m *msg, withFlags bool) {
	var b strings.Builder
	b.WriteString("* ")
	b.WriteString(strconv.Itoa(seq))
	b.WriteString(" FETCH (")
	var items []string
	// RFC 9051 7.5.2: a client that works with UIDs wants the UID in a
	// FETCH it did not ask for. One that has only used message numbers
	// gets what RFC 3501 shows.
	if c.usesUIDs {
		items = append(items, "UID "+strconv.FormatUint(uint64(m.uid), 10))
	}
	if withFlags {
		items = append(items, "FLAGS ("+wireFlags(m.flags)+")")
	}
	if c.condstore {
		items = append(items, "MODSEQ ("+strconv.FormatInt(m.modseq, 10)+")")
	}
	b.WriteString(strings.Join(items, " "))
	b.WriteString(")")
	c.printf("%s", b.String())
}

// poll brings the client's view up to date if something changed.
func (c *conn) poll(allowExpunge bool) error {
	if c.sel == nil {
		return nil
	}
	select {
	case <-c.changes:
		c.sel.dirty = true
	default:
	}
	if !c.sel.dirty && !(allowExpunge && c.sel.gone > 0) {
		return nil
	}
	_, err := c.sync(allowExpunge, nil)
	return err
}

// sync reads the mailbox again and tells the client what is different:
// FETCH for messages whose flags changed, EXPUNGE for messages that went
// (only when allowed; otherwise they stay in the view, marked), EXISTS
// when messages came. quiet names UIDs whose new flags the client knows
// (STORE with .SILENT). It returns the UIDs it sent a FETCH for.
func (c *conn) sync(allowExpunge bool, quiet map[uint32]bool) (told map[uint32]bool, err error) {
	s := c.sel
	// A wake-up that is waiting is answered by this read.
	select {
	case <-c.changes:
	default:
	}
	v, err := c.user.View(c.ctx, s.mailbox, s.modseq)
	if errors.Is(err, ErrNoMailbox) {
		// Deleted under the session, by another one. A client cannot be
		// told to deselect; the connection ends (RFC 3501 has no better).
		c.printf("* BYE the selected mailbox was deleted")
		c.sel = nil
		c.state = stateLogout
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.dirty = false
	told = map[uint32]bool{}

	// What went: known UIDs that the mailbox no longer has.
	j := 0
	for _, m := range s.msgs {
		for j < len(v.UIDs) && v.UIDs[j] < m.uid {
			j++
		}
		if (j >= len(v.UIDs) || v.UIDs[j] != m.uid) && !m.gone {
			m.gone = true
			s.gone++
		}
	}
	// What changed, and what came.
	var fresh []*msg
	newKeywords := false
	last := uint32(0)
	if n := len(s.msgs); n > 0 {
		last = s.msgs[n-1].uid
	}
	type change struct {
		i         int
		withFlags bool
	}
	var changes []change
	for _, cm := range v.Changed {
		if cm.UID > last {
			m := newMsg(cm)
			fresh = append(fresh, m)
			newKeywords = s.noteKeywords(m.flags) || newKeywords
			continue
		}
		i := s.find(cm.UID)
		if i < 0 {
			continue
		}
		m := s.msgs[i]
		nm := newMsg(cm)
		flagsChanged := !sameFlags(m.flags, nm.flags)
		modseqChanged := m.modseq != nm.modseq
		m.flags, m.modseq = nm.flags, nm.modseq
		newKeywords = s.noteKeywords(m.flags) || newKeywords
		switch {
		case m.gone:
		case quiet[m.uid]:
			// RFC 7162 3.1.3: with .SILENT the new MODSEQ is still sent.
			if c.condstore && modseqChanged {
				changes = append(changes, change{i, false})
			}
		case flagsChanged || (c.condstore && modseqChanged):
			changes = append(changes, change{i, true})
		}
	}
	if newKeywords {
		c.printf("%s", s.flagsLine())
	}
	for _, ch := range changes {
		c.flagsFetch(ch.i+1, s.msgs[ch.i], ch.withFlags)
		told[s.msgs[ch.i].uid] = true
	}
	if allowExpunge && s.gone > 0 {
		// In ascending order; each number is the one the message has
		// after the ones before it went (RFC 3501 7.4.1, and the example
		// of 6.4.3: messages 3, 4, 7 and 11 go as 3, 3, 5, 8).
		kept := s.msgs[:0:0]
		for i, m := range s.msgs {
			if m.gone {
				c.printf("* %d EXPUNGE", i+1-(i-len(kept)))
				continue
			}
			kept = append(kept, m)
		}
		s.msgs, s.gone = kept, 0
	}
	if len(fresh) > 0 {
		s.msgs = append(s.msgs, fresh...)
		c.printf("* %d EXISTS", len(s.msgs))
		if !c.rev2 {
			c.printf("* 0 RECENT")
		}
	}
	s.modseq, s.uidNext = v.Modseq, v.UIDNext
	return told, nil
}

// indexes resolves a sequence set to positions in msgs, ascending. With
// uid the numbers are UIDs. Messages that are gone are left out.
func (c *conn) indexes(set seqSet, uid bool) ([]int, error) {
	s := c.sel
	var out []int
	switch {
	case set.Saved:
		// RFC 5182 2.1: "$" is the saved result; messages expunged since
		// are no longer in it.
		if !c.savedValid {
			return nil, bad("there is no saved search result")
		}
		for _, u := range c.saved {
			if i := s.find(u); i >= 0 && !s.msgs[i].gone {
				out = append(out, i)
			}
		}
	case uid:
		if len(s.msgs) == 0 {
			return nil, nil
		}
		ranges := set.resolve(s.msgs[len(s.msgs)-1].uid)
		for _, r := range ranges {
			lo := sort.Search(len(s.msgs), func(i int) bool { return s.msgs[i].uid >= r.Lo })
			for i := lo; i < len(s.msgs) && s.msgs[i].uid <= r.Hi; i++ {
				out = append(out, i)
			}
		}
	default:
		n := uint32(len(s.msgs))
		for _, r := range set.Ranges {
			// RFC 9051 9: a number above the count is a protocol error;
			// "*" in an empty mailbox names nothing.
			for _, v := range []uint32{r.Lo, r.Hi} {
				if v != star && v > n {
					return nil, bad("there is no message %d: the mailbox has %d", v, n)
				}
			}
		}
		if n == 0 {
			return nil, nil
		}
		for _, r := range set.resolve(n) {
			for i := r.Lo; i <= r.Hi; i++ {
				out = append(out, int(i-1))
			}
		}
	}
	sort.Ints(out)
	uniq := out[:0]
	for k, i := range out {
		if (k == 0 || out[k-1] != i) && !s.msgs[i].gone {
			uniq = append(uniq, i)
		}
	}
	return uniq, nil
}

// uidRanges names the messages at the given positions by ranges of UIDs.
// A range spans only messages that follow each other in the view, so it
// names no message the session does not know.
func (s *selected) uidRanges(idx []int) []UIDRange {
	var out []UIDRange
	for k := 0; k < len(idx); {
		j := k
		for j+1 < len(idx) && idx[j+1] == idx[j]+1 {
			j++
		}
		out = append(out, UIDRange{s.msgs[idx[k]].uid, s.msgs[idx[j]].uid})
		k = j + 1
	}
	return out
}

// formatSet writes numbers as a sequence set, ascending runs as ranges.
func formatSet(nums []uint32) string {
	var b strings.Builder
	for i := 0; i < len(nums); {
		j := i
		for j+1 < len(nums) && nums[j+1] == nums[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(uint64(nums[i]), 10))
		if j > i {
			b.WriteByte(':')
			b.WriteString(strconv.FormatUint(uint64(nums[j]), 10))
		}
		i = j + 1
	}
	return b.String()
}

// formatPairs writes the two sets of COPYUID (RFC 4315 3): the UIDs
// copied, and the UIDs they got, in the same order.
func formatPairs(pairs []UIDPair) (from, to string) {
	// Each side is written on its own, runs as ranges, in the order of
	// the pairs: the n-th UID of one set belongs to the n-th of the other.
	side := func(get func(UIDPair) uint32) string {
		var b strings.Builder
		for i := 0; i < len(pairs); {
			j := i
			for j+1 < len(pairs) && get(pairs[j+1]) == get(pairs[j])+1 {
				j++
			}
			if b.Len() > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.FormatUint(uint64(get(pairs[i])), 10))
			if j > i {
				b.WriteByte(':')
				b.WriteString(strconv.FormatUint(uint64(get(pairs[j])), 10))
			}
			i = j + 1
		}
		return b.String()
	}
	return side(func(p UIDPair) uint32 { return p.From }), side(func(p UIDPair) uint32 { return p.To })
}

func (c *conn) cmdSelect(tag string, p *parser) (reply, error)  { return c.selectMailbox(p, false) }
func (c *conn) cmdExamine(tag string, p *parser) (reply, error) { return c.selectMailbox(p, true) }

// selectMailbox is SELECT and EXAMINE (RFC 3501 6.3.1 and 6.3.2, RFC 9051
// 6.3.2), with the CONDSTORE parameter of RFC 7162 3.1.8.
func (c *conn) selectMailbox(p *parser, readOnly bool) (reply, error) {
	name := "SELECT"
	if readOnly {
		name = "EXAMINE"
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	path, err := c.mailboxArg(p)
	if err != nil {
		return reply{}, err
	}
	if p.peek() == ' ' {
		p.pos++
		if err := p.expect('('); err != nil {
			return reply{}, err
		}
		for n := 0; !p.skip(')'); n++ {
			if n > 0 {
				if err := p.space(); err != nil {
					return reply{}, err
				}
			}
			param, err := p.word()
			if err != nil {
				return reply{}, err
			}
			if param != "CONDSTORE" {
				return reply{}, bad("unknown %s parameter %s", name, param)
			}
			c.condstore = true
		}
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	// RFC 3501 6.3.1: the mailbox that was selected is deselected whether
	// or not this command succeeds.
	if c.sel != nil {
		c.sel = nil
		c.state = stateAuth
		if c.rev2 {
			c.printf("* OK [CLOSED] the previous mailbox is closed")
		}
	}
	b, _, err := c.lookup(path)
	if err != nil {
		return reply{}, err
	}
	if b == nil {
		return noCode("NONEXISTENT", "no such mailbox"), nil
	}
	v, err := c.user.View(c.ctx, b.ID, 0)
	if err != nil {
		return reply{}, err
	}
	// Changes up to here are in the view.
	select {
	case <-c.changes:
	default:
	}
	s := &selected{mailbox: b.ID, path: b.path, readOnly: readOnly, uidValidity: v.UIDValidity, uidNext: v.UIDNext,
		modseq: v.Modseq, msgs: make([]*msg, 0, len(v.Changed)), keywords: map[string]bool{}}
	for _, m := range v.Changed {
		nm := newMsg(m)
		s.msgs = append(s.msgs, nm)
		s.noteKeywords(nm.flags)
	}
	c.sel = s
	c.state = stateSelected

	c.printf("%s", s.flagsLine())
	c.printf("* %d EXISTS", len(s.msgs))
	if !c.rev2 {
		c.printf("* 0 RECENT")
		for i, m := range s.msgs {
			if !hasFlag(m.flags, "$seen") {
				c.printf("* OK [UNSEEN %d] the first message that is not seen", i+1)
				break
			}
		}
	}
	if readOnly {
		c.printf("* OK [PERMANENTFLAGS ()] no flag can be changed")
	} else {
		c.printf(`* OK [PERMANENTFLAGS (\Answered \Flagged \Deleted \Seen \Draft \*)] flags and keywords can be changed`)
	}
	c.printf("* OK [UIDVALIDITY %d] UIDs are valid", s.uidValidity)
	c.printf("* OK [UIDNEXT %d] the next UID", s.uidNext)
	c.printf("* OK [HIGHESTMODSEQ %d] the highest modseq", s.modseq)
	if c.rev2 {
		// RFC 9051 6.3.2: a LIST response with the name as the server has it.
		c.printf(`* LIST () "%s" %s`, delimiter, c.encodeName(b.path))
	}
	if readOnly {
		return okCode("READ-ONLY", name+" completed"), nil
	}
	return okCode("READ-WRITE", name+" completed"), nil
}

func (c *conn) cmdCheck(tag string, p *parser) (reply, error) {
	if err := p.end(); err != nil {
		return reply{}, err
	}
	c.sel.dirty = true
	return ok("CHECK completed"), nil
}

// cmdClose is RFC 3501 6.4.2: the \Deleted messages go, without a word.
func (c *conn) cmdClose(tag string, p *parser) (reply, error) {
	if err := p.end(); err != nil {
		return reply{}, err
	}
	s := c.sel
	c.sel = nil
	c.state = stateAuth
	if !s.readOnly {
		if err := c.user.Expunge(c.ctx, s.mailbox, nil); err != nil && !errors.Is(err, ErrNoMailbox) {
			return reply{}, err
		}
	}
	return ok("CLOSE completed"), nil
}

// cmdUnselect is RFC 3691: CLOSE without the expunge.
func (c *conn) cmdUnselect(tag string, p *parser) (reply, error) {
	if err := p.end(); err != nil {
		return reply{}, err
	}
	c.sel = nil
	c.state = stateAuth
	return ok("UNSELECT completed"), nil
}

// cmdExpunge is RFC 3501 6.4.3. The EXPUNGE responses come from the sync
// that follows every command.
func (c *conn) cmdExpunge(tag string, p *parser) (reply, error) {
	if err := p.end(); err != nil {
		return reply{}, err
	}
	if c.sel.readOnly {
		return noCode("READ-ONLY", "the mailbox is open read-only"), nil
	}
	if err := c.user.Expunge(c.ctx, c.sel.mailbox, nil); err != nil {
		return reply{}, err
	}
	c.sel.dirty = true
	return ok("EXPUNGE completed"), nil
}

// cmdUIDExpunge is RFC 4315 2.1: only the \Deleted messages among the
// given UIDs.
func (c *conn) cmdUIDExpunge(tag string, p *parser) (reply, error) {
	if err := p.space(); err != nil {
		return reply{}, err
	}
	set, err := p.seqSet()
	if err != nil {
		return reply{}, err
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	if c.sel.readOnly {
		return noCode("READ-ONLY", "the mailbox is open read-only"), nil
	}
	idx, err := c.indexes(set, true)
	if err != nil {
		return reply{}, err
	}
	if len(idx) > 0 {
		if err := c.user.Expunge(c.ctx, c.sel.mailbox, c.sel.uidRanges(idx)); err != nil {
			return reply{}, err
		}
		c.sel.dirty = true
	}
	return ok("UID EXPUNGE completed"), nil
}

// cmdStore is RFC 3501 6.4.6, with UNCHANGEDSINCE of RFC 7162 3.1.3.
func (c *conn) cmdStore(tag string, p *parser, uid bool) (reply, error) {
	if err := p.space(); err != nil {
		return reply{}, err
	}
	set, err := p.seqSet()
	if err != nil {
		return reply{}, err
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	unchangedSince := int64(-1)
	if p.skip('(') {
		if w, _ := p.word(); w != "UNCHANGEDSINCE" {
			return reply{}, bad("unknown STORE modifier")
		}
		if err := p.space(); err != nil {
			return reply{}, err
		}
		if unchangedSince, err = p.number64(); err != nil {
			return reply{}, err
		}
		if err := p.expect(')'); err != nil {
			return reply{}, err
		}
		if err := p.space(); err != nil {
			return reply{}, err
		}
		c.condstore = true
	}
	mode := FlagsReplace
	switch {
	case p.skip('+'):
		mode = FlagsAdd
	case p.skip('-'):
		mode = FlagsRemove
	}
	item, err := p.word()
	if err != nil {
		return reply{}, err
	}
	silent := false
	switch item {
	case "FLAGS":
	case "FLAGS.SILENT":
		silent = true
	default:
		return reply{}, bad("STORE changes FLAGS or FLAGS.SILENT")
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	flags, err := p.flagList(true)
	if err != nil {
		return reply{}, err
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	keywords, err := toKeywords(flags)
	if err != nil {
		return reply{}, err
	}
	if c.sel.readOnly {
		return noCode("READ-ONLY", "the mailbox is open read-only"), nil
	}
	idx, err := c.indexes(set, uid)
	if err != nil {
		return reply{}, err
	}
	name := "STORE"
	if uid {
		name = "UID STORE"
	}
	if len(idx) == 0 {
		return ok(name + " completed"), nil
	}
	targets := make([]uint32, len(idx))
	for k, i := range idx {
		targets[k] = c.sel.msgs[i].uid
	}
	modified, err := c.user.Store(c.ctx, c.sel.mailbox, c.sel.uidRanges(idx), mode, keywords, unchangedSince)
	if err != nil {
		return reply{}, err
	}
	failed := map[uint32]bool{}
	for _, u := range modified {
		failed[u] = true
	}
	var quiet map[uint32]bool
	if silent {
		quiet = map[uint32]bool{}
		for _, u := range targets {
			if !failed[u] {
				quiet[u] = true
			}
		}
	}
	told, err := c.sync(uid, quiet)
	if err != nil {
		return reply{}, err
	}
	if c.sel == nil {
		return reply{}, nil
	}
	if !silent {
		// RFC 3501 6.4.6: without .SILENT the new flags are returned for
		// every message named, changed or not.
		for _, u := range targets {
			if told[u] || failed[u] {
				continue
			}
			if i := c.sel.find(u); i >= 0 && !c.sel.msgs[i].gone {
				c.flagsFetch(i+1, c.sel.msgs[i], true)
			}
		}
	}
	if len(modified) > 0 {
		// RFC 7162 3.1.3: the messages that failed the condition, as
		// message numbers for STORE and UIDs for UID STORE.
		nums := modified
		if !uid {
			nums = nums[:0:0]
			for _, u := range modified {
				if i := c.sel.find(u); i >= 0 {
					nums = append(nums, uint32(i+1))
				}
			}
		}
		return okCode("MODIFIED "+formatSet(nums), "conditional STORE failed for some messages"), nil
	}
	return ok(name + " completed"), nil
}

// cmdCopy is COPY (RFC 3501 6.4.7) and MOVE (RFC 6851), with the COPYUID
// response code of RFC 4315.
func (c *conn) cmdCopy(tag string, p *parser, uid, move bool) (reply, error) {
	name := "COPY"
	if move {
		name = "MOVE"
	}
	if uid {
		name = "UID " + name
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	set, err := p.seqSet()
	if err != nil {
		return reply{}, err
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	path, err := c.mailboxArg(p)
	if err != nil {
		return reply{}, err
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	if move && c.sel.readOnly {
		return noCode("READ-ONLY", "the mailbox is open read-only"), nil
	}
	idx, err := c.indexes(set, uid)
	if err != nil {
		return reply{}, err
	}
	target, _, err := c.lookup(path)
	if err != nil {
		return reply{}, err
	}
	if target == nil {
		// RFC 3501 6.4.7: tell the client that creating it would help.
		return noCode("TRYCREATE", "no such mailbox"), nil
	}
	if len(idx) == 0 {
		return ok(name + " completed, no messages"), nil
	}
	validity, pairs, err := c.user.Copy(c.ctx, c.sel.mailbox, c.sel.uidRanges(idx), target.ID, move)
	if err != nil {
		return reply{}, err
	}
	c.sel.dirty = true
	if len(pairs) == 0 {
		return ok(name + " completed, no messages"), nil
	}
	from, to := formatPairs(pairs)
	code := "COPYUID " + strconv.FormatUint(uint64(validity), 10) + " " + from + " " + to
	if move {
		// RFC 6851 4.3: COPYUID in an untagged OK, before the EXPUNGE
		// responses, which the sync after the command sends.
		c.printf("* OK [%s] moved", code)
		return ok(name + " completed"), nil
	}
	return okCode(code, name+" completed"), nil
}

// cmdAppend is RFC 3501 6.3.11, with APPENDUID of RFC 4315 and the UTF8
// form of RFC 6855 4.
func (c *conn) cmdAppend(tag string, p *parser) (reply, error) {
	if err := p.space(); err != nil {
		return reply{}, err
	}
	path, err := c.mailboxArg(p)
	if err != nil {
		return reply{}, err
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	var flags []string
	if p.peek() == '(' {
		if flags, err = p.flagList(false); err != nil {
			return reply{}, err
		}
		if err := p.space(); err != nil {
			return reply{}, err
		}
	}
	var date time.Time
	if p.peek() == '"' {
		if date, err = p.dateTime(); err != nil {
			return reply{}, err
		}
		if err := p.space(); err != nil {
			return reply{}, err
		}
	}
	utf8Form := false
	if strings.HasPrefix(strings.ToUpper(p.cur()), "UTF8 (") {
		if !c.utf8 {
			return reply{}, bad("APPEND with UTF8 needs ENABLE UTF8=ACCEPT")
		}
		p.pos += len("UTF8 (")
		utf8Form = true
	}
	raw, isLiteral := p.literal()
	if !isLiteral {
		return reply{}, bad("the message is missing: APPEND takes it as a literal")
	}
	if utf8Form {
		if err := p.expect(')'); err != nil {
			return reply{}, err
		}
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	keywords, err := toKeywords(flags)
	if err != nil {
		return reply{}, err
	}
	if len(raw) == 0 {
		// RFC 9051 6.3.12: a client that changed its mind sends an empty
		// message; nothing is stored.
		return no("an empty message is not stored"), nil
	}
	b, _, err := c.lookup(path)
	if err != nil {
		return reply{}, err
	}
	if b == nil {
		return noCode("TRYCREATE", "no such mailbox"), nil
	}
	uid, validity, err := c.user.Append(c.ctx, b.ID, []byte(raw), keywords, date)
	if err != nil {
		return reply{}, err
	}
	if c.sel != nil && c.sel.mailbox == b.ID {
		c.sel.dirty = true
	}
	return okCode("APPENDUID "+strconv.FormatUint(uint64(validity), 10)+" "+strconv.FormatUint(uint64(uid), 10), "APPEND completed"), nil
}
