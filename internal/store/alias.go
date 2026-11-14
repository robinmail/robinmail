package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mikepage/robinmail/internal/address"
)

// An alias is an address, or a pattern for addresses, of one of our
// domains that has no mailbox of its own: mail for it goes to its targets.
// A target is an address here (an account, or another alias) or anywhere
// else, to which the message is then forwarded.
//
//	info@example.test        one address
//	sales-*@example.test     a pattern: "*" is any run of characters
//	*@example.test           the catch-all of a domain, the widest pattern
//
// An account always wins over an alias for its own address. Among aliases
// the exact address wins, then the pattern with the most characters
// outside its "*".
type Alias struct {
	Address string
	Targets []string
	// Wildcard says the address is a pattern.
	Wildcard bool
}

var (
	ErrAliasAddress = errors.New("not a valid alias address")
	ErrAliasTarget  = errors.New("not a valid alias target")
	ErrNoAlias      = errors.New("no such alias")
	// ErrForwarded is returned by Forward for a key that was used before:
	// the message is in the queue already, or was.
	ErrForwarded = errors.New("already forwarded")
)

// MaxAliasTargets bounds what one message can fan out to.
const MaxAliasTargets = 50

// ParseAliasAddress brings an alias address into its stored form:
// lower case, "@domain" written as "*@domain". At most one "*".
func ParseAliasAddress(s string) (addr string, wildcard bool, err error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(s, "@") {
		s = "*" + s
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || strings.Count(s[:at], "*") > 1 {
		return "", false, ErrAliasAddress
	}
	local, domain := s[:at], s[at+1:]
	wildcard = strings.Contains(local, "*")
	// What is left when the "*" stands for one letter must be an address.
	probe := strings.Replace(local, "*", "x", 1)
	if _, err := address.Parse(probe + "@" + domain); err != nil || strings.Contains(local, "+") {
		return "", false, ErrAliasAddress
	}
	return local + "@" + domain, wildcard, nil
}

// MatchAlias reports whether a pattern's local part matches a local part,
// and how many characters of the pattern are not the "*".
func MatchAlias(pattern, local string) (bool, int) {
	prefix, suffix, wild := strings.Cut(pattern, "*")
	if !wild {
		return pattern == local, len(pattern)
	}
	ok := len(local) >= len(prefix)+len(suffix) && strings.HasPrefix(local, prefix) && strings.HasSuffix(local, suffix)
	return ok, len(prefix) + len(suffix)
}

// AddAlias gives an alias these targets, next to the ones it has; it makes
// the alias if there is none. The domain has to be one of ours
// (ErrNoDomain) and the address must not be an account's
// (ErrAccountExists): the account would get the mail and the alias would
// never be used. A target is an address as SMTP takes it; one at a domain
// of ours is looked up again when mail arrives.
func (s *Store) AddAlias(ctx context.Context, addr string, targets []string) (Alias, error) {
	addr, wildcard, err := ParseAliasAddress(addr)
	if err != nil {
		return Alias{}, err
	}
	if len(targets) == 0 || len(targets) > MaxAliasTargets {
		return Alias{}, ErrAliasTarget
	}
	clean := make([]string, 0, len(targets))
	for _, t := range targets {
		t = strings.TrimSpace(t)
		at := strings.LastIndexByte(t, '@')
		if at <= 0 || at == len(t)-1 || len(t) > 254 || strings.ContainsAny(t, " \t\r\n<>\x00") {
			return Alias{}, ErrAliasTarget
		}
		// The domain compares without case; the local part is the other
		// server's business and stays as it was given.
		t = t[:at] + "@" + strings.ToLower(t[at+1:])
		if strings.ToLower(t) == addr {
			return Alias{}, ErrAliasTarget
		}
		clean = append(clean, t)
	}
	domain := addr[strings.LastIndexByte(addr, '@')+1:]
	b := s.newBatch("")
	b.idempotent() // INSERT OR IGNORE of given rows
	b.guard(ErrNoDomain, `EXISTS (SELECT 1 FROM domain WHERE name = :domain)`, "domain", domain)
	b.guard(ErrAccountExists, `NOT EXISTS (SELECT 1 FROM account WHERE address = :addr)`, "addr", addr)
	for _, t := range clean {
		b.add(`INSERT OR IGNORE INTO alias (address, target, domain, wildcard, created_at) VALUES (:addr, :target, :domain, :wildcard, :now)`,
			"addr", addr, "target", t, "domain", domain, "wildcard", boolInt(wildcard))
	}
	b.guard(ErrAliasTarget, `(SELECT count(*) FROM alias WHERE address = :addr) <= :max`, "addr", addr, "max", MaxAliasTargets)
	if _, err := s.run(ctx, b); err != nil {
		return Alias{}, err
	}
	list, err := s.Aliases(ctx, "")
	if err != nil {
		return Alias{}, err
	}
	for _, a := range list {
		if a.Address == addr {
			return a, nil
		}
	}
	return Alias{}, ErrNoAlias
}

// RemoveAlias takes a target away from an alias, or with an empty target
// the whole alias. It returns how many targets went.
func (s *Store) RemoveAlias(ctx context.Context, addr, target string) (int64, error) {
	addr, _, err := ParseAliasAddress(addr)
	if err != nil {
		return 0, err
	}
	b := s.newBatch("")
	if target == "" {
		b.add(`DELETE FROM alias WHERE address = :addr`, "addr", addr)
	} else {
		b.add(`DELETE FROM alias WHERE address = :addr AND lower(target) = lower(:target)`, "addr", addr, "target", strings.TrimSpace(target))
	}
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	if res[0].RowsAffected == 0 {
		return 0, ErrNoAlias
	}
	return res[0].RowsAffected, nil
}

func scanAliases(rows [][]any) []Alias {
	var out []Alias
	at := map[string]int{}
	for _, row := range rows {
		addr := asString(row[0])
		i, ok := at[addr]
		if !ok {
			i = len(out)
			at[addr] = i
			out = append(out, Alias{Address: addr, Wildcard: asInt(row[2]) != 0})
		}
		out[i].Targets = append(out[i].Targets, asString(row[1]))
	}
	return out
}

// Aliases lists the aliases of a domain, or of all domains for "".
func (s *Store) Aliases(ctx context.Context, domain string) ([]Alias, error) {
	res, err := s.query(ctx, "", q(`SELECT address, target, wildcard FROM alias WHERE :domain = '' OR domain = :domain
		ORDER BY domain, wildcard, address, target`, "domain", domain))
	if err != nil {
		return nil, err
	}
	return scanAliases(res[0].Values), nil
}

// LookupAlias finds the alias that takes mail for an address of ours: the
// alias with exactly this address, else the one for the address without
// its "+tag", else the pattern of the domain that fits best. ok is false
// when there is none.
func (s *Store) LookupAlias(ctx context.Context, addr address.Address) (alias Alias, ok bool, err error) {
	full := addr.String()
	base := full
	if b, _, tagged := strings.Cut(addr.Local, "+"); tagged && b != "" {
		base = b + "@" + addr.Domain
	}
	res, err := s.query(ctx, "", q(`SELECT address, target, wildcard FROM alias
		WHERE address IN (:full, :base) OR (domain = :domain AND wildcard = 1) ORDER BY address, target`,
		"full", full, "base", base, "domain", addr.Domain))
	if err != nil {
		return Alias{}, false, err
	}
	list := scanAliases(res[0].Values)
	for _, want := range []string{full, base} {
		for _, a := range list {
			if !a.Wildcard && a.Address == want {
				return a, true, nil
			}
		}
	}
	best := -1
	for _, a := range list {
		if !a.Wildcard {
			continue
		}
		pattern := a.Address[:strings.LastIndexByte(a.Address, '@')]
		for _, local := range []string{addr.Local, base[:strings.LastIndexByte(base, '@')]} {
			if match, weight := MatchAlias(pattern, local); match && weight > best {
				best, alias, ok = weight, a, true
			}
		}
	}
	return alias, ok, nil
}

// SendAddresses are the alias addresses an account may send as: the
// aliases with exactly one address (no pattern) that name the account's
// address as a target. A pattern gives no such right: the catch-all of a
// domain would let its reader write as anybody there.
func (s *Store) SendAddresses(ctx context.Context, accountAddress string) ([]string, error) {
	res, err := s.query(ctx, "", q(`SELECT DISTINCT address FROM alias WHERE lower(target) = :addr AND wildcard = 0 ORDER BY address`,
		"addr", accountAddress))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, asString(row[0]))
	}
	sort.Strings(out)
	return out, nil
}

// A system account owns queued mail that no user sent: mail forwarded for
// an alias or by a filter, and bounces passed back to the sender of
// forwarded mail. There is one per domain of ours, the domain the mail is
// sent on for: its id is SystemAccountPrefix and a hash of the domain (an
// account id is a directory name of at most 64 letters and digits; a
// domain can be longer and has dots). It has a blob
// key like any account, because queued messages are blobs, and nothing
// else: no mailbox, no password, an address ("!system@domain") that no
// message can be for. It is made the first time something is forwarded
// for the domain.
const SystemAccountPrefix = "system-"

// SystemAccountID is the id of the system account of a domain.
func SystemAccountID(domain string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(domain)))
	return SystemAccountPrefix + hex.EncodeToString(sum[:16])
}

// IsSystemAccount reports whether an account id is a system account's.
func IsSystemAccount(id string) bool { return strings.HasPrefix(id, SystemAccountPrefix) }

// sqlUserAccount is true for the account rows of users.
const sqlUserAccount = `id NOT LIKE 'system-%'`

func (s *Store) ensureSystemAccount(ctx context.Context, domain string) (string, error) {
	if _, err := address.Parse("x@" + domain); err != nil {
		return "", fmt.Errorf("store: %q is not a domain", domain)
	}
	id := SystemAccountID(domain)
	if _, ok := s.systems.Load(id); ok {
		return id, nil
	}
	_, wrapped, err := s.master.NewAccountKey(id)
	if err != nil {
		return "", err
	}
	b := s.newBatch(id)
	b.idempotent() // INSERT OR IGNORE; whoever comes second keeps the first one's key
	b.add(`INSERT OR IGNORE INTO account (id, address, password_hash, blob_key, created_at, modseq, next_email,
			next_mailbox, next_thread, uidvalidity, seq)
		VALUES (:a, :address, :hash, :blob_key, :now, 1, 1, 1, 1, :now, (SELECT COALESCE(max(seq), 0) + 1 FROM account))`,
		"address", "!system@"+domain, "hash", NoPassword, "blob_key", wrapped)
	if _, err := s.run(ctx, b); err != nil {
		return "", err
	}
	s.systems.Store(id, true)
	return id, nil
}

// Forward queues a message for a recipient elsewhere, on behalf of nobody:
// a message that an alias or a filter sends on, or a bounce on its way
// back. domain is the domain of ours it is sent on for; mailFrom is the
// envelope sender it leaves with. key makes it safe to repeat: a second
// call with the same key queues nothing and returns ErrForwarded.
func (s *Store) Forward(ctx context.Context, domain, key, mailFrom, rcpt string, raw []byte) error {
	id, err := s.ensureSystemAccount(ctx, strings.ToLower(domain))
	if err != nil {
		return err
	}
	_, err = s.Submit(ctx, id, NewSubmission{Raw: raw, MailFrom: mailFrom, Rcpts: []string{rcpt}, Key: key})
	if errors.Is(err, ErrNoAccount) {
		s.systems.Delete(id)
	}
	return err
}
