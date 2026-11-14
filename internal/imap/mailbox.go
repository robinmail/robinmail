package imap

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Mailbox names. The backend has a tree of mailboxes, each with a name in
// UTF-8. IMAP names a mailbox by its path, the names from the top joined
// by the hierarchy delimiter "/". The mailbox with the role "inbox" is
// INBOX (RFC 3501 5.1), whatever its own name, and its children are
// INBOX/child.
//
// On the wire a name is modified UTF-7 (RFC 3501 5.1.3), or UTF-8 once the
// client enabled IMAP4rev2 or UTF8=ACCEPT.
//
// Two kinds of mailbox cannot be named this way and are not shown: one
// whose own name contains "/", and one at the top that is called "INBOX"
// (in any case) without being the inbox.

const delimiter = "/"

// maxDepth is how deep a hierarchy CREATE makes and LIST follows.
const maxDepth = 10

// box is a mailbox with its path.
type box struct {
	Mailbox
	path        string // UTF-8
	hasChildren bool
}

// boxes reads the user's mailboxes and gives each its path, in the
// backend's order.
func (c *conn) boxes() ([]box, error) {
	list, err := c.user.Mailboxes(c.ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*Mailbox, len(list))
	for i := range list {
		byID[list[i].ID] = &list[i]
	}
	var pathOf func(m *Mailbox, depth int) (string, bool)
	pathOf = func(m *Mailbox, depth int) (string, bool) {
		if depth > maxDepth+1 || strings.Contains(m.Name, delimiter) || m.Name == "" {
			return "", false
		}
		name := m.Name
		if m.Role == "inbox" && m.ParentID == 0 {
			name = "INBOX"
		} else if m.ParentID == 0 && strings.EqualFold(name, "INBOX") {
			return "", false
		}
		if m.ParentID == 0 {
			return name, true
		}
		parent, ok := byID[m.ParentID]
		if !ok {
			return "", false
		}
		pp, ok := pathOf(parent, depth+1)
		if !ok {
			return "", false
		}
		return pp + delimiter + name, true
	}
	out := make([]box, 0, len(list))
	shown := map[int64]bool{}
	for i := range list {
		if path, ok := pathOf(&list[i], 0); ok {
			out = append(out, box{Mailbox: list[i], path: path})
			shown[list[i].ID] = true
		}
	}
	parents := map[int64]bool{}
	for _, b := range out {
		parents[b.ParentID] = true
	}
	for i := range out {
		out[i].hasChildren = parents[out[i].ID]
	}
	return out, nil
}

// decodeName turns a mailbox name from the wire into a path. INBOX is
// case-insensitive (RFC 3501 5.1); a "/" at the end is dropped (6.3.3: a
// client may create "foo/" to say that foo will have children).
func (c *conn) decodeName(wire string) (string, error) {
	name := wire
	if c.utf8 {
		if !utf8.ValidString(name) {
			return "", bad("the mailbox name is not UTF-8")
		}
	} else {
		var err error
		if name, err = decodeUTF7(wire); err != nil {
			return "", bad("the mailbox name is not modified UTF-7")
		}
	}
	name = strings.TrimSuffix(name, delimiter)
	first, rest, nested := strings.Cut(name, delimiter)
	if strings.EqualFold(first, "INBOX") {
		name = "INBOX"
		if nested {
			name += delimiter + rest
		}
	}
	return name, nil
}

// encodeName writes a path as the client reads names.
func (c *conn) encodeName(path string) string {
	if c.utf8 {
		return quoteName(path, true)
	}
	return quoteName(encodeUTF7(path), false)
}

// quoteName writes a mailbox name as an astring: an atom when it can be.
func quoteName(s string, utf8ok bool) string {
	atom := s != "" && !strings.EqualFold(s, "NIL")
	for i := 0; atom && i < len(s); i++ {
		if !atomChar(s[i]) {
			atom = false
		}
	}
	if atom {
		return s
	}
	return quote(s, utf8ok)
}

// mailboxArg reads a mailbox name argument as a path.
func (c *conn) mailboxArg(p *parser) (string, error) {
	wire, err := p.astring()
	if err != nil {
		return "", err
	}
	return c.decodeName(wire)
}

// find looks a mailbox up by its path.
func find(list []box, path string) *box {
	for i := range list {
		if list[i].path == path {
			return &list[i]
		}
	}
	return nil
}

// lookup reads the mailboxes and finds one by path.
func (c *conn) lookup(path string) (*box, []box, error) {
	list, err := c.boxes()
	if err != nil {
		return nil, nil, err
	}
	return find(list, path), list, nil
}

// matchPattern reports whether a path matches a LIST pattern (RFC 3501
// 6.3.8): "*" is any run of characters, "%" any run without the
// delimiter.
func matchPattern(pattern, name string) bool {
	// row[j] says whether pattern[i:] matches name[j:], for the i of the
	// round; next is the row of i+1.
	next := make([]bool, len(name)+1)
	row := make([]bool, len(name)+1)
	next[len(name)] = true
	for i := len(pattern) - 1; i >= 0; i-- {
		for j := len(name); j >= 0; j-- {
			switch pc := pattern[i]; {
			case pc == '*':
				row[j] = next[j] || (j < len(name) && row[j+1])
			case pc == '%':
				row[j] = next[j] || (j < len(name) && name[j] != delimiter[0] && row[j+1])
			default:
				row[j] = j < len(name) && name[j] == pc && next[j+1]
			}
		}
		row, next = next, row
	}
	return next[0]
}

// specialUse is the attribute of RFC 6154 for a role.
func specialUse(role string) string {
	switch role {
	case "drafts":
		return `\Drafts`
	case "sent":
		return `\Sent`
	case "junk":
		return `\Junk`
	case "trash":
		return `\Trash`
	case "archive":
		return `\Archive`
	case "all":
		return `\All`
	case "flagged":
		return `\Flagged`
	}
	return ""
}

// listOptions are the options of an extended LIST (RFC 5258).
type listOptions struct {
	extended bool
	// Selection options.
	subscribed, recursive, specialUse bool
	// Return options.
	retSubscribed, retChildren, retSpecialUse bool
	status                                    []string
}

// cmdList is RFC 3501 6.3.8, with the options of RFC 5258, the STATUS
// return option of RFC 5819 and the special-use attributes of RFC 6154.
func (c *conn) cmdList(tag string, p *parser) (reply, error) {
	if err := p.space(); err != nil {
		return reply{}, err
	}
	var opt listOptions
	// Selection options come first, in parentheses. A reference is a
	// mailbox name and cannot start with "(" unquoted.
	if p.peek() == '(' {
		opt.extended = true
		p.pos++
		for n := 0; !p.skip(')'); n++ {
			if n > 0 {
				if err := p.space(); err != nil {
					return reply{}, err
				}
			}
			name, err := p.word()
			if err != nil {
				return reply{}, err
			}
			switch name {
			case "SUBSCRIBED":
				opt.subscribed = true
			case "REMOTE": // there are no remote mailboxes
			case "RECURSIVEMATCH":
				opt.recursive = true
			case "SPECIAL-USE":
				opt.specialUse = true
			default:
				return reply{}, bad("unknown LIST selection option %s", name)
			}
		}
		// RFC 5258 3.1: RECURSIVEMATCH needs another selection option.
		if opt.recursive && !opt.subscribed {
			return reply{}, bad("RECURSIVEMATCH needs SUBSCRIBED")
		}
		if err := p.space(); err != nil {
			return reply{}, err
		}
	}
	reference, err := p.astring()
	if err != nil {
		return reply{}, err
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	var patterns []string
	if p.peek() == '(' {
		opt.extended = true
		p.pos++
		for !p.skip(')') {
			if len(patterns) > 0 {
				if err := p.space(); err != nil {
					return reply{}, err
				}
			}
			pat, err := p.listMailbox()
			if err != nil {
				return reply{}, err
			}
			if patterns = append(patterns, pat); len(patterns) > 100 {
				return reply{}, bad("too many patterns")
			}
		}
	} else {
		pat, err := p.listMailbox()
		if err != nil {
			return reply{}, err
		}
		patterns = []string{pat}
	}
	if p.peek() == ' ' {
		p.pos++
		if w, _ := p.word(); w != "RETURN" {
			return reply{}, bad("RETURN expected")
		}
		opt.extended = true
		if err := p.space(); err != nil {
			return reply{}, err
		}
		if err := p.expect('('); err != nil {
			return reply{}, err
		}
		for n := 0; !p.skip(')'); n++ {
			if n > 0 {
				if err := p.space(); err != nil {
					return reply{}, err
				}
			}
			name, err := p.word()
			if err != nil {
				return reply{}, err
			}
			switch name {
			case "SUBSCRIBED":
				opt.retSubscribed = true
			case "CHILDREN":
				opt.retChildren = true
			case "SPECIAL-USE":
				opt.retSpecialUse = true
			case "STATUS":
				if err := p.space(); err != nil {
					return reply{}, err
				}
				if opt.status, err = c.statusItems(p); err != nil {
					return reply{}, err
				}
			default:
				return reply{}, bad("unknown LIST return option %s", name)
			}
		}
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}

	ref, err := c.decodePattern(reference)
	if err != nil {
		return reply{}, err
	}
	// RFC 3501 6.3.8: an empty mailbox name asks for the delimiter.
	if !opt.extended && len(patterns) == 1 && patterns[0] == "" {
		c.printf(`* LIST (\Noselect) "%s" ""`, delimiter)
		return ok("LIST completed"), nil
	}
	list, err := c.boxes()
	if err != nil {
		return reply{}, err
	}
	var full []string
	for _, pat := range patterns {
		pt, err := c.decodePattern(pat)
		if err != nil {
			return reply{}, err
		}
		full = append(full, canonicalPattern(ref+pt))
	}
	matches := func(path string) bool {
		for _, pt := range full {
			if matchPattern(pt, path) {
				return true
			}
		}
		return false
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].path < list[j].path })
	for _, b := range list {
		if !matches(b.path) {
			continue
		}
		// RFC 5258 3.5: with RECURSIVEMATCH a mailbox that does not meet
		// the selection but has a child that does is listed with CHILDINFO.
		childInfo := false
		if opt.subscribed && !b.Subscribed {
			if !opt.recursive {
				continue
			}
			for _, o := range list {
				if o.Subscribed && strings.HasPrefix(o.path, b.path+delimiter) {
					childInfo = true
					break
				}
			}
			if !childInfo {
				continue
			}
		} else if opt.subscribed && opt.recursive {
			for _, o := range list {
				if o.Subscribed && strings.HasPrefix(o.path, b.path+delimiter) {
					childInfo = true
					break
				}
			}
		}
		use := specialUse(b.Role)
		if opt.specialUse && use == "" {
			continue
		}
		var attrs []string
		if opt.subscribed || opt.retSubscribed {
			if b.Subscribed {
				attrs = append(attrs, `\Subscribed`)
			}
		}
		// CHILDREN (RFC 3348) is always given: it costs nothing here.
		if b.hasChildren {
			attrs = append(attrs, `\HasChildren`)
		} else {
			attrs = append(attrs, `\HasNoChildren`)
		}
		if use != "" {
			attrs = append(attrs, use)
		}
		line := "* LIST (" + strings.Join(attrs, " ") + `) "` + delimiter + `" ` + c.encodeName(b.path)
		if childInfo {
			line += ` ("CHILDINFO" ("SUBSCRIBED"))`
		}
		c.printf("%s", line)
		if opt.status != nil {
			c.printf("* STATUS %s (%s)", c.encodeName(b.path), c.statusText(&b, opt.status))
		}
	}
	return ok("LIST completed"), nil
}

// decodePattern decodes a reference or pattern; wildcards stay.
func (c *conn) decodePattern(wire string) (string, error) {
	if c.utf8 {
		if !utf8.ValidString(wire) {
			return "", bad("the mailbox name is not UTF-8")
		}
		return wire, nil
	}
	s, err := decodeUTF7(wire)
	if err != nil {
		return "", bad("the mailbox name is not modified UTF-7")
	}
	return s, nil
}

// canonicalPattern makes a leading "inbox" of a pattern INBOX.
func canonicalPattern(pt string) string {
	first, rest, nested := strings.Cut(pt, delimiter)
	if strings.EqualFold(first, "INBOX") {
		pt = "INBOX"
		if nested {
			pt += delimiter + rest
		}
	}
	return pt
}

// cmdLsub is RFC 3501 6.3.9: the subscribed mailboxes.
func (c *conn) cmdLsub(tag string, p *parser) (reply, error) {
	if err := p.space(); err != nil {
		return reply{}, err
	}
	reference, err := p.astring()
	if err != nil {
		return reply{}, err
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	pattern, err := p.listMailbox()
	if err != nil {
		return reply{}, err
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	ref, err := c.decodePattern(reference)
	if err != nil {
		return reply{}, err
	}
	pt, err := c.decodePattern(pattern)
	if err != nil {
		return reply{}, err
	}
	pt = canonicalPattern(ref + pt)
	list, err := c.boxes()
	if err != nil {
		return reply{}, err
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].path < list[j].path })
	for _, b := range list {
		switch {
		case !matchPattern(pt, b.path):
		case b.Subscribed:
			c.printf(`* LSUB () "%s" %s`, delimiter, c.encodeName(b.path))
		default:
			// RFC 3501 6.3.9: with "%" a name that is not subscribed but
			// has a subscribed child is returned with \Noselect, unless
			// the pattern shows that child itself.
			for _, o := range list {
				if o.Subscribed && strings.HasPrefix(o.path, b.path+delimiter) {
					if !matchPattern(pt, o.path) {
						c.printf(`* LSUB (\Noselect) "%s" %s`, delimiter, c.encodeName(b.path))
					}
					break
				}
			}
		}
	}
	return ok("LSUB completed"), nil
}

// cmdNamespace is RFC 2342: one personal namespace, no prefix.
func (c *conn) cmdNamespace(tag string, p *parser) (reply, error) {
	if err := p.end(); err != nil {
		return reply{}, err
	}
	c.printf(`* NAMESPACE (("" "%s")) NIL NIL`, delimiter)
	return ok("NAMESPACE completed"), nil
}

// statusItems reads the item list of STATUS.
func (c *conn) statusItems(p *parser) ([]string, error) {
	if err := p.expect('('); err != nil {
		return nil, err
	}
	items := []string{}
	for !p.skip(')') {
		if len(items) > 0 {
			if err := p.space(); err != nil {
				return nil, err
			}
		}
		name, err := p.word()
		if err != nil {
			return nil, err
		}
		switch name {
		case "MESSAGES", "RECENT", "UIDNEXT", "UIDVALIDITY", "UNSEEN", "SIZE", "DELETED":
		case "HIGHESTMODSEQ":
			// RFC 7162 3.1: a "CONDSTORE enabling command".
			c.condstore = true
		default:
			return nil, bad("unknown STATUS item %s", name)
		}
		if items = append(items, name); len(items) > 20 {
			return nil, bad("too many STATUS items")
		}
	}
	if len(items) == 0 {
		return nil, bad("STATUS needs at least one item")
	}
	return items, nil
}

func (c *conn) statusText(b *box, items []string) string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		var v int64
		switch item {
		case "MESSAGES":
			v = b.Messages
		case "RECENT":
			v = 0 // \Recent is not kept (RFC 9051 dropped it)
		case "UIDNEXT":
			v = int64(b.UIDNext)
		case "UIDVALIDITY":
			v = int64(b.UIDValidity)
		case "UNSEEN":
			v = b.Unseen
		case "SIZE":
			v = b.Size
		case "DELETED":
			v = b.Deleted
		case "HIGHESTMODSEQ":
			v = b.HighestModseq
		}
		out = append(out, item+" "+strconv.FormatInt(v, 10))
	}
	return strings.Join(out, " ")
}

// cmdStatus is RFC 3501 6.3.10.
func (c *conn) cmdStatus(tag string, p *parser) (reply, error) {
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
	items, err := c.statusItems(p)
	if err != nil {
		return reply{}, err
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	b, _, err := c.lookup(path)
	if err != nil {
		return reply{}, err
	}
	if b == nil {
		return noCode("NONEXISTENT", "no such mailbox"), nil
	}
	c.printf("* STATUS %s (%s)", c.encodeName(b.path), c.statusText(b, items))
	return ok("STATUS completed"), nil
}

// validLevel checks one level of a name a client wants to create.
func validLevel(name string) bool {
	if name == "" || len(name) > 255 || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return false
		}
	}
	return true
}

// ensure makes the mailbox at path and every level above it that is
// missing (RFC 3501 6.3.3: "the server SHOULD create any superior
// hierarchical names"). It returns the id of the mailbox at path, and
// whether that one was made now.
func (c *conn) ensure(path string) (id int64, created bool, err error) {
	levels := strings.Split(path, delimiter)
	if len(levels) > maxDepth {
		return 0, false, ErrName
	}
	for _, l := range levels {
		if !validLevel(l) {
			return 0, false, ErrName
		}
	}
	list, err := c.boxes()
	if err != nil {
		return 0, false, err
	}
	var parent int64
	for i := range levels {
		sub := strings.Join(levels[:i+1], delimiter)
		if b := find(list, sub); b != nil {
			parent, created = b.ID, false
			continue
		}
		if i == 0 && sub == "INBOX" {
			return 0, false, ErrNoMailbox // an account without an inbox
		}
		id, err := c.user.Create(c.ctx, parent, levels[i])
		if err != nil {
			return 0, false, err
		}
		parent, created = id, true
	}
	return parent, created, nil
}

// cmdCreate is RFC 3501 6.3.3.
func (c *conn) cmdCreate(tag string, p *parser) (reply, error) {
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
	if path == "INBOX" {
		return noCode("ALREADYEXISTS", "INBOX exists"), nil
	}
	_, created, err := c.ensure(path)
	if err != nil {
		return reply{}, err
	}
	if !created {
		return noCode("ALREADYEXISTS", "a mailbox with that name exists"), nil
	}
	return ok("CREATE completed"), nil
}

// cmdDelete is RFC 3501 6.3.4. The messages leave the mailbox with it;
// what that means for a message with other labels is the backend's rule.
//
// A mailbox with children is not deleted. RFC 3501 would have it lose its
// messages and stay as a \Noselect name; there are no such names here.
func (c *conn) cmdDelete(tag string, p *parser) (reply, error) {
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
	if path == "INBOX" {
		return noCode("NOPERM", "INBOX cannot be deleted"), nil
	}
	b, _, err := c.lookup(path)
	if err != nil {
		return reply{}, err
	}
	if b == nil {
		return noCode("NONEXISTENT", "no such mailbox"), nil
	}
	if b.hasChildren {
		return reply{}, ErrHasChildren
	}
	if err := c.user.Delete(c.ctx, b.ID); err != nil {
		return reply{}, err
	}
	if c.sel != nil && c.sel.mailbox == b.ID {
		// RFC 3501 does not say what happens to a session that has the
		// deleted mailbox selected; this one is told and deselected.
		c.printf("* OK [CLOSED] the selected mailbox was deleted")
		c.sel = nil
		c.state = stateAuth
	}
	return ok("DELETE completed"), nil
}

// cmdRename is RFC 3501 6.3.5. The mailbox keeps its messages, their UIDs
// and its UIDVALIDITY; its children move with it.
//
// Renaming INBOX is special there: its messages move to a new mailbox and
// INBOX stays, empty.
func (c *conn) cmdRename(tag string, p *parser) (reply, error) {
	if err := p.space(); err != nil {
		return reply{}, err
	}
	from, err := c.mailboxArg(p)
	if err != nil {
		return reply{}, err
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	to, err := c.mailboxArg(p)
	if err != nil {
		return reply{}, err
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	src, list, err := c.lookup(from)
	if err != nil {
		return reply{}, err
	}
	if src == nil {
		return noCode("NONEXISTENT", "no such mailbox"), nil
	}
	if find(list, to) != nil || to == "INBOX" {
		return noCode("ALREADYEXISTS", "a mailbox with that name exists"), nil
	}
	if strings.HasPrefix(to, from+delimiter) {
		return reply{}, ErrLoop
	}
	if from == "INBOX" {
		id, _, err := c.ensure(to)
		if err != nil {
			return reply{}, err
		}
		if _, _, err := c.user.Copy(c.ctx, src.ID, nil, id, true); err != nil {
			return reply{}, err
		}
		return ok("RENAME completed: the messages of INBOX moved"), nil
	}
	var parent int64
	name := to
	if i := strings.LastIndex(to, delimiter); i >= 0 {
		if parent, _, err = c.ensure(to[:i]); err != nil {
			return reply{}, err
		}
		name = to[i+1:]
	}
	if !validLevel(name) {
		return reply{}, ErrName
	}
	if err := c.user.Rename(c.ctx, src.ID, parent, name); err != nil {
		return reply{}, err
	}
	return ok("RENAME completed"), nil
}

func (c *conn) cmdSubscribe(tag string, p *parser) (reply, error) {
	return c.subscribe(p, true, "SUBSCRIBE")
}

func (c *conn) cmdUnsubscribe(tag string, p *parser) (reply, error) {
	return c.subscribe(p, false, "UNSUBSCRIBE")
}

// subscribe is RFC 3501 6.3.6 and 6.3.7. Only mailboxes that exist can be
// subscribed to: the subscription is a property of the mailbox.
func (c *conn) subscribe(p *parser, on bool, name string) (reply, error) {
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
	b, _, err := c.lookup(path)
	if err != nil {
		return reply{}, err
	}
	if b == nil {
		if !on {
			return ok(name + " completed"), nil // gone is unsubscribed
		}
		return noCode("NONEXISTENT", "no such mailbox"), nil
	}
	if b.Subscribed != on {
		if err := c.user.Subscribe(c.ctx, b.ID, on); err != nil {
			return reply{}, err
		}
	}
	return ok(name + " completed"), nil
}
