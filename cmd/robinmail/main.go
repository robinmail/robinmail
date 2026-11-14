// Command robinmail is a JMAP-first mail server. "serve" runs the inbound
// SMTP listener, the submission listeners, the outbound queue and the web
// side (login, JMAP, webmail); the other commands manage domains, accounts
// and the store.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/term"

	"github.com/mikepage/robinmail/internal/address"
	"github.com/mikepage/robinmail/internal/blob"
	"github.com/mikepage/robinmail/internal/cluster"
	"github.com/mikepage/robinmail/internal/config"
	"github.com/mikepage/robinmail/internal/rqlite"
	"github.com/mikepage/robinmail/internal/store"
	"github.com/mikepage/robinmail/internal/users"
	"github.com/mikepage/robinmail/internal/web"
)

const usage = `usage: robinmail [-config robinmail.json] <command>

A <mailbox> is an id (m1), a role (inbox, junk) or a path (Work/Invoices).
An <email> is an id (e1).

commands:
  serve                                        run the server until SIGINT or SIGTERM
  status                                       what the server holds, its queue and nodes, and the checks of "health"
  health                                       check the database, the master key, the blob directory, the certificate
                                               and whether the server answers; exit status 1 when one fails
  key generate                                 write a new master key to master_key_file
  key status                                   list the master keys of the file and how many accounts use each
  key add                                      add a new master key to the file, accepted and not used yet
  key rotate <fingerprint>                     make that key the current one and wrap every account's key under it
  key prune                                    take the keys no account uses out of the file
  backup <directory>                           write the database and all blob files into a new directory
  restore <directory>                          load a backup into an empty database and this node's blob directory
  restore-blobs <directory>                    copy the blob files of a backup that this node lacks (after a failed restore)
  domain add <domain>                          accept mail for a domain
  domain list                                  list the accepted domains
  domain remove <domain>                       stop accepting mail for a domain without users
  alias add <alias> <target>...                deliver mail for an alias to accounts here or forward it elsewhere;
                                               an alias is an address, a pattern (sales-*@example.org) or @example.org
  alias list [domain]                          list the aliases
  alias remove <alias> [target]                remove one target of an alias, or the alias
  user add <address>                           create a user (password from the prompt or stdin)
  user list                                    list users
  user show <address>                          show a user's account: mail, quota, ways in, aliases
  user remove <address> --yes                  delete a user with all mail; it cannot be undone
  queue list [count]                           list the outbound messages that still have work (default: 100)
  queue show <address> <id>                    show one of them with its recipients
  queue retry <address> <id> | all             try a message, or all that wait, now instead of at their next try
  queue delete <address> <id>                  give a message up; no bounce is sent
  user quota <address> [<storage> <messages>]  show a user's quota and use, or set the limits: a number of octets
                                               (with K, M or G) and a number of messages; "none" for no limit,
                                               "default" for the default of the config
  tlsrpt results [days]                        show how the TLS of outbound mail went (default: 2 days), and the reports made
  tlsrpt received [count]                      list the TLS reports other servers sent us (default: 20)
  tlsrpt show <id>                             print one of them
  quota recount [address]                      count the mail of accounts again and correct their quota counters
  user password <address>                      set a new password (prompt or stdin); ends the user's sessions
  user totp-disable <address>                  turn TOTP off for a user who lost the authenticator
  user app-passwords <address>                 list a user's app passwords
  user app-password-revoke <address> <id>      revoke one; its open IMAP sessions end
  user sessions <address>                      list a user's web sessions
  user session-revoke <address> <id|all>       end one web session, or all
  user oidc-unlink <address>                   forget the user's subject at the OIDC providers
  audit [address|all] [count]                  show the newest auth events and what administrators did (default: all, 50)
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
  trim                                         remove change log, delivery keys, sessions and auth events past their time
  sweep                                        remove this node's blob files that no email refers to
  repair                                       fetch the blobs this node lacks from the other nodes
  scrub                                        check this node's blob files against their hashes; in a cluster
                                               replace a corrupt one from another node
  node list                                    list the nodes of the cluster
  node remove <id>                             take a stopped node out of the cluster
  reindex [address]                            read stored messages again and refresh the cached columns
  dns <domain> [dkim-key-file]                 print the DNS records of a domain (MX, SPF, DKIM, DMARC, MTA-STS);
                                               a key file that does not exist is made
`

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "robinmail:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
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
	if len(args) == 1 && args[0] == "serve" {
		// The server runs until it is told to stop; only starting is bounded.
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		ictx, cancel := context.WithTimeout(ctx, time.Minute)
		a, err := open(ictx, cfg, true)
		cancel()
		if err != nil {
			return err
		}
		a.ctx, a.stdin, a.stdout = ctx, stdin, stdout
		return a.serve(stop)
	}
	// A command has ten minutes, except those whose time grows with the
	// mail stored.
	limit := 10 * time.Minute
	switch args[0] {
	case "backup", "restore", "restore-blobs", "scrub", "repair", "reindex":
		limit = 24 * time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	a, err := open(ctx, cfg, false)
	if err != nil {
		return err
	}
	a.ctx, a.stdin, a.stdout = ctx, stdin, stdout
	// A command may leave pushes to other nodes running when enough of
	// them have a blob. They finish before the process ends.
	defer a.wait()

	cmd, args := args[0], args[1:]
	n := len(args)
	switch {
	case cmd == "status" && n == 0:
		return a.status()
	case cmd == "health" && n == 0:
		return a.health()
	case cmd == "key" && n == 1 && args[0] == "status":
		return a.keyStatus()
	case cmd == "key" && n == 1 && args[0] == "add":
		return a.keyAdd()
	case cmd == "key" && n == 2 && args[0] == "rotate":
		return a.keyRotate(args[1])
	case cmd == "key" && n == 1 && args[0] == "prune":
		return a.keyPrune()
	case cmd == "backup" && n == 1:
		return a.backup(args[0])
	case cmd == "restore" && n == 1:
		return a.restore(args[0])
	case cmd == "restore-blobs" && n == 1:
		return a.restoreBlobs(args[0])
	case cmd == "scrub" && n == 0:
		return a.scrub()
	case cmd == "user" && n == 2 && args[0] == "show":
		return a.userShow(args[1])
	case cmd == "user" && (n == 2 || (n == 3 && args[2] == "--yes")) && args[0] == "remove":
		return a.userRemove(args[1], n == 3)
	case cmd == "queue" && (n == 1 || n == 2) && args[0] == "list":
		return a.queueList(optional(args, 1, "100"))
	case cmd == "queue" && n == 3 && args[0] == "show":
		return a.queueShow(args[1], args[2])
	case cmd == "queue" && ((n == 2 && args[1] == "all") || n == 3) && args[0] == "retry":
		return a.queueRetry(args[1:])
	case cmd == "queue" && n == 3 && args[0] == "delete":
		return a.queueDelete(args[1], args[2])
	case cmd == "domain" && n == 2 && args[0] == "add":
		return a.domainAdd(args[1])
	case cmd == "domain" && n == 1 && args[0] == "list":
		return a.domainList()
	case cmd == "domain" && n == 2 && args[0] == "remove":
		return a.domainRemove(args[1])
	case cmd == "user" && n == 2 && args[0] == "add":
		return a.userAdd(args[1])
	case cmd == "user" && n == 1 && args[0] == "list":
		return a.userList()
	case cmd == "alias" && n >= 3 && args[0] == "add":
		return a.aliasAdd(args[1], args[2:])
	case cmd == "alias" && (n == 1 || n == 2) && args[0] == "list":
		return a.aliasList(optional(args, 1, ""))
	case cmd == "alias" && (n == 2 || n == 3) && args[0] == "remove":
		return a.aliasRemove(args[1], optional(args, 2, ""))
	case cmd == "user" && (n == 2 || n == 4) && args[0] == "quota":
		return a.userQuota(args[1], args[2:])
	case cmd == "tlsrpt" && (n == 1 || n == 2) && args[0] == "results":
		return a.tlsrptResults(optional(args, 1, "2"))
	case cmd == "tlsrpt" && (n == 1 || n == 2) && args[0] == "received":
		return a.tlsrptReceived(optional(args, 1, "20"))
	case cmd == "tlsrpt" && n == 2 && args[0] == "show":
		return a.tlsrptShow(args[1])
	case cmd == "quota" && (n == 1 || n == 2) && args[0] == "recount":
		return a.quotaRecount(optional(args, 1, ""))
	case cmd == "user" && n == 2 && args[0] == "password":
		return a.userPassword(args[1])
	case cmd == "user" && n == 2 && args[0] == "totp-disable":
		return a.userTOTPDisable(args[1])
	case cmd == "user" && n == 2 && args[0] == "app-passwords":
		return a.userAppPasswords(args[1])
	case cmd == "user" && n == 3 && args[0] == "app-password-revoke":
		return a.userAppPasswordRevoke(args[1], args[2])
	case cmd == "user" && n == 2 && args[0] == "sessions":
		return a.userSessions(args[1])
	case cmd == "user" && n == 3 && args[0] == "session-revoke":
		return a.userSessionRevoke(args[1], args[2])
	case cmd == "user" && n == 2 && args[0] == "oidc-unlink":
		return a.userOIDCUnlink(args[1])
	case cmd == "audit" && n <= 2:
		return a.audit(optional(args, 0, "all"), optional(args, 1, "50"))
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
	case cmd == "sweep" && n == 0:
		return a.sweep()
	case cmd == "repair" && n == 0:
		return a.repair()
	case cmd == "node" && n == 1 && args[0] == "list":
		return a.nodeList()
	case cmd == "node" && n == 2 && args[0] == "remove":
		return a.nodeRemove(args[1])
	case cmd == "dns" && (n == 1 || n == 2):
		return a.dns(args[0], optional(args, 1, ""))
	case cmd == "reindex" && (n == 0 || n == 1):
		return a.reindex(optional(args, 0, ""))
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

// open loads the master key, connects the store to rqlite and the blob
// directory, and creates the schema in an empty database. With a node
// section in the config it also makes this process a member of the
// cluster: blobs are replicated to the other nodes and fetched from them.
// serving is true for the server, which announces itself as running; a
// command only makes sure the other nodes know its certificate.
func open(ctx context.Context, cfg *config.Config, serving bool) (*app, error) {
	db, err := rqlite.NewCluster(cfg.Rqlite.Nodes(), nil)
	if err != nil {
		return nil, err
	}
	master, err := blob.LoadMasterKey(cfg.MasterKeyFile)
	if err != nil {
		return nil, fmt.Errorf("%w (make one with \"robinmail key generate\")", err)
	}
	a := &app{cfg: cfg, local: blob.NewLocal(cfg.BlobDir()), master: master}
	limits := store.Limits{Bytes: cfg.Quota.DefaultBytes, Messages: cfg.Quota.DefaultMessages}
	if cfg.Node.ID == "" {
		a.store = store.New(db, a.local, master)
		a.store.DefaultQuota = limits
		return a, a.store.Init(ctx)
	}
	identity, err := cluster.LoadOrCreateIdentity(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	a.replicated = &blob.Replicated{Local: a.local, Copies: cfg.Node.BlobCopies}
	a.store = store.New(db, a.replicated, master)
	a.store.DefaultQuota = limits
	if err := a.store.Init(ctx); err != nil {
		return nil, err
	}
	a.node = &cluster.Node{ID: cfg.Node.ID, Address: cfg.Node.Address, Identity: identity, Dir: a.store}
	a.replicated.Peers = a.node
	a.replicated.Verify = a.store.VerifyBlob
	if err := a.node.Register(ctx, serving); err != nil {
		return nil, err
	}
	return a, nil
}

type app struct {
	ctx    context.Context
	cfg    *config.Config
	store  *store.Store
	master *blob.MasterKey
	stdin  io.Reader
	stdout io.Writer
	// local is this node's blob directory. node and replicated are nil
	// unless the config has a node section.
	local      *blob.Local
	node       *cluster.Node
	replicated *blob.Replicated
}

func (a *app) wait() {
	if a.replicated != nil {
		a.replicated.Wait()
	}
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

// domain validates a domain name from the command line.
func domain(s string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(s))
	if !address.ValidDomain(name) {
		return "", fmt.Errorf("%q is not a domain name", s)
	}
	return name, nil
}

func (a *app) domainAdd(s string) error {
	name, err := domain(s)
	if err != nil {
		return err
	}
	if err := a.store.AddDomain(a.ctx, name); errors.Is(err, store.ErrDomainExists) {
		return fmt.Errorf("%s: domain already exists", name)
	} else if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "added %s\n", name)
	return nil
}

func (a *app) domainList() error {
	list, err := a.store.Domains(a.ctx)
	if err != nil {
		return err
	}
	for _, d := range list {
		fmt.Fprintln(a.stdout, d)
	}
	return nil
}

func (a *app) domainRemove(s string) error {
	name, err := domain(s)
	if err != nil {
		return err
	}
	switch err := a.store.RemoveDomain(a.ctx, name); {
	case errors.Is(err, store.ErrNoDomain):
		return fmt.Errorf("%s: no such domain", name)
	case errors.Is(err, store.ErrDomainInUse):
		return fmt.Errorf("%s still has users", name)
	case err != nil:
		return err
	}
	fmt.Fprintf(a.stdout, "removed %s\n", name)
	return nil
}

func (a *app) userAdd(s string) error {
	addr, err := address.Parse(s)
	if err != nil {
		return fmt.Errorf("%q: %w", s, err)
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
	if errors.Is(err, store.ErrNoDomain) {
		return fmt.Errorf("%s is not an accepted domain (add it with \"robinmail domain add %s\")", addr.Domain, addr.Domain)
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
	sessions, err := a.store.TrimSessions(a.ctx, now)
	if err != nil {
		return err
	}
	if _, err := a.store.TrimLoginFailures(a.ctx, now.Add(-web.LoginWindow)); err != nil {
		return err
	}
	events, err := a.store.TrimAuthEvents(a.ctx, now.Add(-a.auditRetention()))
	if err != nil {
		return err
	}
	if _, err := a.store.TrimVacationSent(a.ctx, now.Add(-store.VacationSentRetention)); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "removed %d change rows, %d delivery keys, %d sessions and %d auth events\n", changes, keys, sessions, events)
	return nil
}

// repair fetches the blobs this node lacks from the other nodes. serve
// does the same at start and every few minutes.
func (a *app) repair() error {
	res, err := a.store.RepairBlobs(a.ctx)
	if errors.Is(err, store.ErrNoRepair) {
		return errors.New("this node is not part of a cluster (no node section in the config)")
	}
	fmt.Fprintf(a.stdout, "%d blobs were missing here, fetched %d\n", res.Missing, res.Fetched)
	return err
}

func (a *app) nodeList() error {
	nodes, err := a.store.Nodes(a.ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		seen := "never ran"
		if n.SeenAt.Unix() > 0 {
			seen = "seen " + time.Since(n.SeenAt).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(a.stdout, "%-12s  %-28s  %s  %s\n", n.ID, n.Address, n.Cert[:min(16, len(n.Cert))], seen)
	}
	return nil
}

// nodeRemove takes a node out of the node table. The others then stop
// counting it as a place a blob should be.
func (a *app) nodeRemove(id string) error {
	ok, err := a.store.RemoveNode(a.ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s: no such node", id)
	}
	fmt.Fprintf(a.stdout, "removed %s\n", id)
	return nil
}

// sweep removes the blob files on this node that no email refers to and
// that are older than a day. serve does the same once a day.
func (a *app) sweep() error {
	res, err := a.store.SweepBlobs(a.ctx, time.Now().Add(-store.BlobGrace))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "removed %d blob files and %d temporary files, put %d back\n", res.Removed, res.Temp, res.Restored)
	return nil
}

// reindex refreshes what the email table caches from each message: the
// fields for lists and filters, the preview, hasAttachment. For mail stored
// by a version that cached less.
func (a *app) reindex(s string) error {
	var accounts []store.Account
	if s != "" {
		acct, err := a.account(s)
		if err != nil {
			return err
		}
		accounts = []store.Account{acct}
	} else {
		var err error
		if accounts, err = a.store.Accounts(a.ctx); err != nil {
			return err
		}
	}
	for _, acct := range accounts {
		ids, err := a.store.EmailIDs(a.ctx, acct.ID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := a.store.Reindex(a.ctx, acct.ID, id); err != nil && !errors.Is(err, store.ErrNoEmail) {
				return fmt.Errorf("%s %s: %w", acct.Address, store.EmailID(id), err)
			}
		}
		fmt.Fprintf(a.stdout, "%s: %d emails\n", acct.Address, len(ids))
	}
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
