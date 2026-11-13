// Command robinmail is a JMAP-first mail server. For now it only has the
// offline commands that manage accounts and the store; the server comes in
// phase 2.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/term"

	"github.com/mikepage/robinmail/internal/address"
	"github.com/mikepage/robinmail/internal/blob"
	"github.com/mikepage/robinmail/internal/config"
	"github.com/mikepage/robinmail/internal/rqlite"
	"github.com/mikepage/robinmail/internal/store"
	"github.com/mikepage/robinmail/internal/users"
)

const usage = `usage: robinmail [-config robinmail.json] <command>

A <mailbox> is an id (m1), a role (inbox, junk) or a path (Work/Invoices).
An <email> is an id (e1).

commands:
  key generate                                 write a new master key to master_key_file
  user add <address>                           create a user (password from the prompt or stdin)
  user list                                    list users
  mailbox list <address>                       list a user's mailboxes
  mailbox create <address> <path>              create a mailbox
  mailbox rename <address> <mailbox> <name>    rename a mailbox
  deliver <address> [mailbox]                  store the message on stdin (default: inbox)
  list <address> [mailbox]                     list the emails in a mailbox (default: inbox)
  cat <address> <email>                        print one message
  verify <address> [email]                     check blob files against their hashes (default: all)
  move <address> <email> <from> <to>           move an email to another mailbox
  label <address> <email> <mailbox>            also put an email in a mailbox
  unlabel <address> <email> <mailbox>          take an email out of a mailbox
  flag <address> <email> <keyword>...          set keywords such as $seen; -keyword removes one
  rm <address> <email>                         destroy an email
  changes <address> [state]                    show what changed since a state (default: 0)
  trim                                         remove change log and delivery keys past their retention
`

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "robinmail:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("robinmail", flag.ContinueOnError)
	cfgPath := fs.String("config", "robinmail.json", "path to the config file")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return flag.ErrHelp
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// The one command that runs without a master key: it makes one.
	if len(args) == 2 && args[0] == "key" && args[1] == "generate" {
		if err := blob.GenerateMasterKeyFile(cfg.MasterKeyFile); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s\nBack it up and give every node the same file: without it the stored mail cannot be read.\n", cfg.MasterKeyFile)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	a := app{ctx: ctx, cfg: cfg, store: st, stdin: stdin, stdout: stdout}

	cmd, args := args[0], args[1:]
	n := len(args)
	switch {
	case cmd == "user" && n == 2 && args[0] == "add":
		return a.userAdd(args[1])
	case cmd == "user" && n == 1 && args[0] == "list":
		return a.userList()
	case cmd == "mailbox" && n == 2 && args[0] == "list":
		return a.mailboxList(args[1])
	case cmd == "mailbox" && n == 3 && args[0] == "create":
		return a.mailboxCreate(args[1], args[2])
	case cmd == "mailbox" && n == 4 && args[0] == "rename":
		return a.mailboxRename(args[1], args[2], args[3])
	case cmd == "deliver" && (n == 1 || n == 2):
		return a.deliver(args[0], optional(args, 1, store.RoleInbox))
	case cmd == "list" && (n == 1 || n == 2):
		return a.list(args[0], optional(args, 1, store.RoleInbox))
	case cmd == "cat" && n == 2:
		return a.cat(args[0], args[1])
	case cmd == "verify" && (n == 1 || n == 2):
		return a.verify(args[0], optional(args, 1, ""))
	case cmd == "move" && n == 4:
		return a.move(args[0], args[1], args[2], args[3])
	case cmd == "label" && n == 3:
		return a.label(args[0], args[1], args[2], true)
	case cmd == "unlabel" && n == 3:
		return a.label(args[0], args[1], args[2], false)
	case cmd == "flag" && n >= 3:
		return a.flag(args[0], args[1], args[2:])
	case cmd == "rm" && n == 2:
		return a.rm(args[0], args[1])
	case cmd == "changes" && (n == 1 || n == 2):
		return a.changes(args[0], optional(args, 1, "0"))
	case cmd == "trim" && n == 0:
		return a.trim()
	}
	fs.Usage()
	return flag.ErrHelp
}

func optional(args []string, i int, def string) string {
	if len(args) > i {
		return args[i]
	}
	return def
}

// openStore loads the master key, connects the store to rqlite and the blob
// directory, and creates the schema in an empty database.
func openStore(ctx context.Context, cfg *config.Config) (*store.Store, error) {
	db, err := rqlite.New(cfg.Rqlite.URL, nil)
	if err != nil {
		return nil, err
	}
	master, err := blob.LoadMasterKey(cfg.MasterKeyFile)
	if err != nil {
		return nil, fmt.Errorf("%w (make one with \"robinmail key generate\")", err)
	}
	st := store.New(db, blob.NewLocal(cfg.BlobDir()), master)
	if err := st.Init(ctx); err != nil {
		return nil, err
	}
	return st, nil
}

type app struct {
	ctx    context.Context
	cfg    *config.Config
	store  *store.Store
	stdin  io.Reader
	stdout io.Writer
}

// account looks a user up by address.
func (a *app) account(s string) (store.Account, error) {
	addr, err := address.Parse(s)
	if err != nil {
		return store.Account{}, fmt.Errorf("%q: %w", s, err)
	}
	acct, err := a.store.Account(a.ctx, addr.String())
	if errors.Is(err, store.ErrNoAccount) {
		return acct, fmt.Errorf("%s: no such user", addr)
	}
	return acct, err
}

// paths returns the full path of every mailbox, such as "Work/Invoices".
func paths(list []store.Mailbox) map[int64]string {
	byID := map[int64]store.Mailbox{}
	for _, m := range list {
		byID[m.ID] = m
	}
	out := map[int64]string{}
	for _, m := range list {
		path := m.Name
		for p, depth := m.ParentID, 0; p != 0 && depth < 64; depth++ {
			path = byID[p].Name + "/" + path
			p = byID[p].ParentID
		}
		out[m.ID] = path
	}
	return out
}

// mailbox finds one of the account's mailboxes by id, role or path.
func (a *app) mailbox(acct store.Account, arg string) (store.Mailbox, error) {
	list, err := a.store.Mailboxes(a.ctx, acct.ID)
	if err != nil {
		return store.Mailbox{}, err
	}
	path := paths(list)
	id, isID := store.ParseMailboxID(arg)
	for _, match := range []func(store.Mailbox) bool{
		func(m store.Mailbox) bool { return isID && m.ID == id },
		func(m store.Mailbox) bool { return m.Role != "" && strings.EqualFold(m.Role, arg) },
		func(m store.Mailbox) bool { return path[m.ID] == arg },
		func(m store.Mailbox) bool { return strings.EqualFold(path[m.ID], arg) },
	} {
		for _, m := range list {
			if match(m) {
				return m, nil
			}
		}
	}
	return store.Mailbox{}, fmt.Errorf("%s has no mailbox %q", acct.Address, arg)
}

func emailID(arg string) (int64, error) {
	id, ok := store.ParseEmailID(arg)
	if !ok {
		return 0, fmt.Errorf("%q is not an email id (such as e1)", arg)
	}
	return id, nil
}

func (a *app) userAdd(s string) error {
	addr, err := address.Parse(s)
	if err != nil {
		return fmt.Errorf("%q: %w", s, err)
	}
	if !a.cfg.HasDomain(addr.Domain) {
		return fmt.Errorf("%s is not in the configured domains", addr.Domain)
	}
	password, err := a.readPassword()
	if err != nil {
		return err
	}
	hash, err := users.Hash(password)
	if err != nil {
		return err
	}
	acct, err := a.store.CreateAccount(a.ctx, addr.String(), hash)
	if errors.Is(err, store.ErrAccountExists) {
		return fmt.Errorf("%s: user already exists", addr)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "created %s (account %s)\n", acct.Address, acct.ID)
	return nil
}

// readPassword prompts without echo on a terminal and reads one line otherwise.
func (a *app) readPassword() (string, error) {
	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(os.Stderr, "password: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", errors.New("no password on stdin")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (a *app) userList() error {
	list, err := a.store.Accounts(a.ctx)
	if err != nil {
		return err
	}
	for _, u := range list {
		fmt.Fprintf(a.stdout, "%-40s  account %s  state %d\n", u.Address, u.ID, u.State)
	}
	return nil
}

func (a *app) mailboxList(s string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	list, err := a.store.Mailboxes(a.ctx, acct.ID)
	if err != nil {
		return err
	}
	path := paths(list)
	// Children follow their parent; top-level mailboxes keep the store's order.
	root := map[string]int{}
	for i, m := range list {
		if m.ParentID == 0 {
			root[m.Name] = i
		}
	}
	top := func(m store.Mailbox) int { return root[strings.SplitN(path[m.ID], "/", 2)[0]] }
	sort.SliceStable(list, func(i, j int) bool {
		if ti, tj := top(list[i]), top(list[j]); ti != tj {
			return ti < tj
		}
		return path[list[i].ID] < path[list[j].ID]
	})
	fmt.Fprintf(a.stdout, "%-5s  %-7s  %5s  %6s  %7s  %11s  %6s  %s\n", "id", "role", "total", "unread", "uidnext", "uidvalidity", "modseq", "path")
	for _, m := range list {
		fmt.Fprintf(a.stdout, "%-5s  %-7s  %5d  %6d  %7d  %11d  %6d  %s\n",
			store.MailboxID(m.ID), m.Role, m.Total, m.Unread, m.UIDNext, m.UIDValidity, m.Modseq, clip(path[m.ID], 60))
	}
	return nil
}

func (a *app) mailboxCreate(s, path string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	var parent int64
	name := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		p, err := a.mailbox(acct, path[:i])
		if err != nil {
			return err
		}
		parent, name = p.ID, path[i+1:]
	}
	m, err := a.store.CreateMailbox(a.ctx, acct.ID, parent, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "created mailbox %s %s, state %d\n", store.MailboxID(m.ID), path, m.Modseq)
	return nil
}

func (a *app) mailboxRename(s, arg, name string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	m, err := a.mailbox(acct, arg)
	if err != nil {
		return err
	}
	state, err := a.store.RenameMailbox(a.ctx, acct.ID, m.ID, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "renamed mailbox %s to %s, state %d\n", store.MailboxID(m.ID), name, state)
	return nil
}

// maxMessage bounds what deliver reads from stdin.
const maxMessage = 64 << 20

func (a *app) deliver(s, arg string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	m, err := a.mailbox(acct, arg)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(a.stdin, maxMessage+1))
	if err != nil {
		return err
	}
	if len(raw) > maxMessage {
		return errors.New("message too large")
	}
	e, err := a.store.Deliver(a.ctx, acct.ID, store.Message{Raw: raw, MailboxID: m.ID})
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "delivered to %s %s: email %s, thread %s, uid %d, %d bytes, state %d\n",
		acct.Address, m.Name, store.EmailID(e.ID), store.ThreadID(e.ThreadID), e.Mailboxes[m.ID], e.Size, e.Modseq)
	return nil
}

func (a *app) list(s, arg string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	m, err := a.mailbox(acct, arg)
	if err != nil {
		return err
	}
	list, err := a.store.Emails(a.ctx, acct.ID, m.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s %s (%s): %d emails, uidvalidity %d, uidnext %d\n",
		acct.Address, m.Name, store.MailboxID(m.ID), len(list), m.UIDValidity, m.UIDNext)
	for _, e := range list {
		var in []string
		for id := range e.Mailboxes {
			in = append(in, store.MailboxID(id))
		}
		sort.Strings(in)
		fmt.Fprintf(a.stdout, "%-5s  uid %-4d  %-5s  %-9s  %7d  %s  %-24s  %-40s  %s\n",
			store.EmailID(e.ID), e.Mailboxes[m.ID], store.ThreadID(e.ThreadID), strings.Join(in, ","), e.Size,
			e.ReceivedAt.Local().Format("2006-01-02 15:04"), clip(e.Sender, 24), clip(e.Subject, 40),
			clip(strings.Join(e.Keywords, " "), 40))
	}
	return nil
}

func (a *app) cat(s, arg string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	id, err := emailID(arg)
	if err != nil {
		return err
	}
	r, _, err := a.store.Open(a.ctx, acct.ID, id)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(a.stdout, r)
	return err
}

// verify checks one email's blob file, or those of all the account's emails.
func (a *app) verify(s, arg string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	var ids []int64
	if arg != "" {
		id, err := emailID(arg)
		if err != nil {
			return err
		}
		ids = []int64{id}
	} else if ids, err = a.store.EmailIDs(a.ctx, acct.ID); err != nil {
		return err
	}
	bad := 0
	for _, id := range ids {
		if err := a.store.VerifyEmail(a.ctx, acct.ID, id); err != nil {
			bad++
			fmt.Fprintf(a.stdout, "%-5s  FAILED  %v\n", store.EmailID(id), err)
		} else if arg != "" {
			fmt.Fprintf(a.stdout, "%-5s  ok\n", store.EmailID(id))
		}
	}
	fmt.Fprintf(a.stdout, "%s: %d emails checked, %d failed\n", acct.Address, len(ids), bad)
	if bad > 0 {
		return fmt.Errorf("%d of %d blobs failed", bad, len(ids))
	}
	return nil
}

func (a *app) move(s, arg, fromArg, toArg string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	id, err := emailID(arg)
	if err != nil {
		return err
	}
	from, err := a.mailbox(acct, fromArg)
	if err != nil {
		return err
	}
	to, err := a.mailbox(acct, toArg)
	if err != nil {
		return err
	}
	state, err := a.store.MoveEmail(a.ctx, acct.ID, id, from.ID, to.ID)
	if errors.Is(err, store.ErrNoEmail) {
		return fmt.Errorf("no email %s in %s", arg, from.Name)
	}
	if err != nil {
		return err
	}
	return a.report(acct, id, fmt.Sprintf("moved %s from %s to %s", arg, from.Name, to.Name), state)
}

func (a *app) label(s, arg, mailboxArg string, add bool) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	id, err := emailID(arg)
	if err != nil {
		return err
	}
	m, err := a.mailbox(acct, mailboxArg)
	if err != nil {
		return err
	}
	u, what := store.Update{AddMailboxes: []int64{m.ID}}, "added %s to %s"
	if !add {
		u, what = store.Update{RemoveMailboxes: []int64{m.ID}}, "removed %s from %s"
	}
	state, err := a.store.UpdateEmail(a.ctx, acct.ID, id, u)
	if err != nil {
		return err
	}
	return a.report(acct, id, fmt.Sprintf(what, arg, m.Name), state)
}

func (a *app) flag(s, arg string, keywords []string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	id, err := emailID(arg)
	if err != nil {
		return err
	}
	var u store.Update
	for _, k := range keywords {
		if rest, ok := strings.CutPrefix(k, "-"); ok {
			u.RemoveKeywords = append(u.RemoveKeywords, rest)
		} else {
			u.AddKeywords = append(u.AddKeywords, k)
		}
	}
	state, err := a.store.UpdateEmail(a.ctx, acct.ID, id, u)
	if err != nil {
		return err
	}
	return a.report(acct, id, "flagged "+arg, state)
}

// report prints where an email is after a change.
func (a *app) report(acct store.Account, id int64, what string, state int64) error {
	e, err := a.store.Email(a.ctx, acct.ID, id)
	if err != nil {
		return err
	}
	var in []string
	for m, uid := range e.Mailboxes {
		in = append(in, fmt.Sprintf("%s uid %d", store.MailboxID(m), uid))
	}
	sort.Strings(in)
	fmt.Fprintf(a.stdout, "%s: now in %s; keywords [%s]; state %d\n",
		what, strings.Join(in, ", "), clip(strings.Join(e.Keywords, " "), 80), state)
	return nil
}

func (a *app) rm(s, arg string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	id, err := emailID(arg)
	if err != nil {
		return err
	}
	state, err := a.store.DestroyEmail(a.ctx, acct.ID, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "destroyed %s, state %d\n", arg, state)
	return nil
}

func (a *app) changes(s, sinceArg string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	since, err := strconv.ParseInt(sinceArg, 10, 64)
	if err != nil {
		return fmt.Errorf("%q is not a state", sinceArg)
	}
	c, err := a.store.Changes(a.ctx, acct.ID, since)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s: state %d -> %d\n", acct.Address, c.OldState, c.NewState)
	for _, t := range []struct {
		name   string
		set    store.ChangeSet
		format func(int64) string
	}{{"Email", c.Email, store.EmailID}, {"Mailbox", c.Mailbox, store.MailboxID}, {"Thread", c.Thread, store.ThreadID}} {
		for _, op := range []struct {
			name string
			ids  []int64
		}{{"created", t.set.Created}, {"updated", t.set.Updated}, {"destroyed", t.set.Destroyed}} {
			if len(op.ids) == 0 {
				continue
			}
			ids := make([]string, len(op.ids))
			for i, id := range op.ids {
				ids[i] = t.format(id)
			}
			fmt.Fprintf(a.stdout, "%-7s  %-9s  %s\n", t.name, op.name, strings.Join(ids, " "))
		}
	}
	return nil
}

func (a *app) trim() error {
	now := time.Now()
	changes, err := a.store.TrimChanges(a.ctx, now.Add(-store.ChangesRetention))
	if err != nil {
		return err
	}
	keys, err := a.store.TrimDelivered(a.ctx, now.Add(-store.DeliveredRetention))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "removed %d change rows and %d delivery keys\n", changes, keys)
	return nil
}

// clip makes a value safe and short enough for one terminal column. Mail is
// untrusted input: control characters could carry escape sequences.
func clip(s string, max int) string {
	r := []rune(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, s))
	if len(r) > max {
		r = append(r[:max-1], '…')
	}
	return string(r)
}
