package sieve

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mikepage/robinmail/internal/message"
)

// Limits on what one message costs and causes.
const (
	// MaxRedirects is how many redirects one run of a script may do.
	MaxRedirects = 4
	// maxOps is how many commands and tests one run may execute.
	maxOps = 20000
	// maxWork is how much comparing one run may do, in octets looked at:
	// every value and key of every comparison, the header fields gone
	// through for a name, the body text made for a body test, and the
	// steps of a :matches (globStepCost each). maxOps bounds how many
	// commands and tests run; this bounds what they cost, which for a
	// test with lists of names and keys against a message with many
	// fields is otherwise the product of all of them. A script within
	// reason spends a thousandth of it.
	maxWork = 256 << 20
	// maxVariables and maxVariableSize bound the variables of a run
	// (RFC 5229 6 asks for at least 128 and 4000 octets).
	maxVariables    = 256
	maxVariableSize = 4000
	// maxExpanded bounds a string after substitution.
	maxExpanded = 64 << 10
	// maxBodyValue is how much of a body part the body test looks at.
	maxBodyValue = 1 << 20
	// maxFileInto is how many mailboxes one message can be filed into.
	maxFileInto = 16
)

// Env is what a script runs against: one message on its way to one
// recipient.
type Env struct {
	Raw  []byte
	From string // the envelope sender; empty is the null reverse-path
	To   string // the envelope recipient the message is being delivered for
	Now  time.Time
	// Zone is the time zone of date tests without :zone; nil is Now's.
	Zone *time.Location
	// MailboxExists answers the mailboxexists test; nil answers no.
	MailboxExists func(name string) bool
}

// Kinds of action.
const (
	Keep = iota + 1
	FileInto
	Redirect
	Discard
	Reject
	Vacation
)

// Action is one thing a script wants done with the message.
type Action struct {
	Kind int
	// FileInto: the mailbox as the script names it, and whether to make
	// it when it is not there.
	Mailbox string
	Create  bool
	// Keep, FileInto: the flags the message gets, as IMAP flags or
	// keywords ("\\Seen", "$label1").
	Flags []string
	// Redirect: where to.
	Address string
	// Reject: why, and whether the script said ereject.
	Reason  string
	EReject bool
	// Vacation: the reply to send, if the caller's rules allow one.
	Vacation *VacationReply
}

// VacationReply is the vacation action of RFC 5230.
type VacationReply struct {
	// Days is the :days argument, or 0 when the script gave none.
	Days int
	// Subject is the :subject argument; empty is the default of RFC 5230
	// 5.2 ("Auto: " and the subject of the message).
	Subject string
	// From is the :from argument; empty is the recipient.
	From string
	// Addresses are the other addresses that are the recipient's own.
	Addresses []string
	// MIME says Reason is a MIME entity, header fields and all.
	MIME bool
	// Handle names the reply for "once per sender"; empty when the
	// script gave none.
	Handle string
	Reason string
}

// Result is what a run decided.
type Result struct {
	// Actions in the order the script took them, each mailbox and each
	// address once. A Keep is among them when the message stays in the
	// default mailbox, whether the script said keep or did nothing that
	// replaces it (the implicit keep of RFC 5228 2.10.2).
	Actions []Action
	// Err is a failure while the script ran: a limit, or actions that
	// do not go together. The message is then kept and nothing else
	// (RFC 5228 2.10.6).
	Err error
}

var errStop = errors.New("stop")

type runtime struct {
	s       *Script
	env     *Env
	hdr     []message.Header
	msg     *message.Message
	ops     int
	work    int64 // spent of maxWork
	vars    map[string]string
	matches []string
	flags   []string // the internal flags variable of RFC 5232

	actions   []Action
	implicit  bool // the implicit keep still stands
	redirects int
	rejected  bool
	vacation  bool
}

// Run runs a script against a message. It never fails: a script that goes
// wrong keeps the message.
func (s *Script) Run(env Env) Result {
	rt := &runtime{s: s, env: &env, hdr: message.ParseHeader(env.Raw), vars: map[string]string{}, implicit: true}
	err := rt.block(s.cmds)
	if err != nil && err != errStop {
		return Result{Actions: []Action{{Kind: Keep}}, Err: err}
	}
	if rt.implicit && !rt.has(Keep) {
		rt.actions = append(rt.actions, Action{Kind: Keep, Flags: append([]string(nil), rt.flags...)})
	}
	return Result{Actions: rt.actions}
}

func (rt *runtime) has(kind int) bool {
	for _, a := range rt.actions {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func (rt *runtime) step(line int) error {
	if rt.ops++; rt.ops > maxOps {
		return fmt.Errorf("line %d: the script did more than %d steps for one message", line, maxOps)
	}
	return nil
}

func (rt *runtime) block(cmds []*command) error {
	taken := false // a branch of the current if/elsif chain ran
	for _, c := range cmds {
		if err := rt.step(c.line); err != nil {
			return err
		}
		switch c.name {
		case "if", "elsif":
			if c.name == "elsif" && taken {
				continue
			}
			ok, err := rt.test(c.test)
			if err != nil {
				return err
			}
			taken = ok
			if ok {
				if err := rt.block(c.block); err != nil {
					return err
				}
			}
		case "else":
			if !taken {
				if err := rt.block(c.block); err != nil {
					return err
				}
			}
			taken = false
		case "stop":
			return errStop
		default:
			if err := rt.action(c); err != nil {
				return err
			}
		}
	}
	return nil
}

// x substitutes variables in a string (RFC 5229 3): "${name}" and "${1}".
// What is not a variable reference stays as it is; an unknown variable is
// the empty string.
func (rt *runtime) x(s string) string {
	if !rt.s.usesVariables() || !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for len(s) > 0 && b.Len() < maxExpanded {
		i := strings.Index(s, "${")
		if i < 0 {
			break
		}
		b.WriteString(s[:i])
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			break
		}
		name := s[i+2 : i+end]
		switch {
		case isDigits(name) && len(name) <= 9:
			if n, _ := strconv.Atoi(name); n < len(rt.matches) {
				b.WriteString(rt.matches[n])
			}
		case validVariableName(name):
			b.WriteString(rt.vars[strings.ToLower(name)])
		default:
			// Not a reference: the "${" is text, and what follows it is
			// looked at again.
			b.WriteString("${")
			s = s[i+2:]
			continue
		}
		s = s[i+end+1:]
	}
	b.WriteString(s)
	out := b.String()
	if len(out) > maxExpanded {
		out = cut(out, maxExpanded)
	}
	return out
}

func (rt *runtime) xs(list []string) []string {
	if !rt.s.usesVariables() {
		return list
	}
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = rt.x(s)
	}
	return out
}

// cut shortens s to at most n octets without splitting a character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// splitFlags turns a list of strings, each of which may hold several
// flags separated by spaces, into the flags, each once (RFC 5232 3).
func splitFlags(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range list {
		for _, f := range strings.Fields(s) {
			if key := strings.ToLower(f); !seen[key] {
				seen[key] = true
				out = append(out, f)
			}
		}
	}
	return out
}

func removeFlags(have, remove []string) []string {
	gone := map[string]bool{}
	for _, f := range remove {
		gone[strings.ToLower(f)] = true
	}
	var out []string
	for _, f := range have {
		if !gone[strings.ToLower(f)] {
			out = append(out, f)
		}
	}
	return out
}

func (rt *runtime) setVar(name, value string) error {
	if _, ok := rt.vars[name]; !ok && len(rt.vars) >= maxVariables {
		return fmt.Errorf("the script uses more than %d variables", maxVariables)
	}
	rt.vars[name] = cut(value, maxVariableSize)
	return nil
}

func (rt *runtime) action(c *command) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("line %d: "+format, append([]any{c.line}, args...)...)
	}
	// What a keep or fileinto gives the message: its :flags, or the
	// flags set so far.
	flags := func() []string {
		if c.hasFlags {
			return splitFlags(rt.xs(c.flags))
		}
		return append([]string(nil), rt.flags...)
	}
	switch c.name {
	case "keep":
		if rt.rejected {
			return fail("keep after reject")
		}
		for i := range rt.actions {
			if rt.actions[i].Kind == Keep {
				rt.actions[i].Flags = flags()
				return nil
			}
		}
		rt.actions = append(rt.actions, Action{Kind: Keep, Flags: flags()})
	case "fileinto":
		if rt.rejected {
			return fail("fileinto after reject")
		}
		mailbox := rt.x(c.str)
		if mailbox == "" || !utf8.ValidString(mailbox) {
			return fail("fileinto: not a mailbox name")
		}
		if !c.copy {
			rt.implicit = false
		}
		n := 0
		for i := range rt.actions {
			if rt.actions[i].Kind != FileInto {
				continue
			}
			n++
			if rt.actions[i].Mailbox == mailbox {
				rt.actions[i].Flags = splitFlags(append(rt.actions[i].Flags, flags()...))
				rt.actions[i].Create = rt.actions[i].Create || c.create
				return nil
			}
		}
		if n >= maxFileInto {
			return fail("more than %d mailboxes for one message", maxFileInto)
		}
		rt.actions = append(rt.actions, Action{Kind: FileInto, Mailbox: mailbox, Create: c.create, Flags: flags()})
	case "redirect":
		if rt.rejected {
			return fail("redirect after reject")
		}
		addr := rt.x(c.str)
		if !validAddress(addr) {
			return fail("redirect: %q is not an address", addr)
		}
		if !c.copy {
			rt.implicit = false
		}
		for _, a := range rt.actions {
			if a.Kind == Redirect && strings.EqualFold(a.Address, addr) {
				return nil
			}
		}
		if rt.redirects++; rt.redirects > MaxRedirects {
			return fail("more than %d redirects for one message", MaxRedirects)
		}
		rt.actions = append(rt.actions, Action{Kind: Redirect, Address: addr})
	case "discard":
		rt.implicit = false
		if !rt.has(Discard) {
			rt.actions = append(rt.actions, Action{Kind: Discard})
		}
	case "reject", "ereject":
		// RFC 5429 2.1.1 and 2.2.1: not together with keep, fileinto,
		// redirect or vacation, and once.
		if rt.rejected || rt.has(Keep) || rt.has(FileInto) || rt.has(Redirect) || rt.vacation {
			return fail("%s together with another action", c.name)
		}
		rt.rejected, rt.implicit = true, false
		rt.actions = append(rt.actions, Action{Kind: Reject, Reason: rt.x(c.str), EReject: c.name == "ereject"})
	case "vacation":
		// RFC 5230 4.7: once per script, and not with reject.
		if rt.vacation || rt.rejected {
			return fail("vacation twice, or together with reject")
		}
		rt.vacation = true
		v := c.vacation
		reply := &VacationReply{Subject: rt.x(v.subject), From: rt.x(v.from), Addresses: rt.xs(v.addresses), MIME: v.mime,
			Reason: rt.x(v.reason)}
		if v.hasDays {
			// "the minimum value is 1"; what is too large is the caller's to cut.
			reply.Days = int(min(max(v.days, 1), 1<<20))
		}
		if v.hasHandle {
			reply.Handle = rt.x(v.handle)
		}
		if reply.From != "" && !validFrom(reply.From) {
			return fail("vacation: :from %q has no address", reply.From)
		}
		rt.actions = append(rt.actions, Action{Kind: Vacation, Vacation: reply})
	case "setflag", "addflag", "removeflag":
		given := splitFlags(rt.xs(c.flags))
		have := rt.flags
		if c.variable != "" {
			have = strings.Fields(rt.vars[c.variable])
		}
		switch c.name {
		case "setflag":
			have = given
		case "addflag":
			have = splitFlags(append(append([]string(nil), have...), given...))
		case "removeflag":
			have = removeFlags(have, given)
		}
		if c.variable != "" {
			return rt.setVar(c.variable, strings.Join(have, " "))
		}
		rt.flags = have
	case "set":
		v := rt.x(c.str)
		for _, m := range c.modifiers {
			switch m {
			case "lower":
				v = strings.ToLower(v)
			case "upper":
				v = strings.ToUpper(v)
			case "lowerfirst", "upperfirst":
				if r, size := utf8.DecodeRuneInString(v); size > 0 && r != utf8.RuneError {
					if m == "lowerfirst" {
						r = unicode.ToLower(r)
					} else {
						r = unicode.ToUpper(r)
					}
					v = string(r) + v[size:]
				}
			case "quotewildcard":
				v = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`).Replace(v)
			case "length":
				v = strconv.Itoa(utf8.RuneCountInString(v))
			}
		}
		return rt.setVar(c.variable, v)
	}
	return nil
}
