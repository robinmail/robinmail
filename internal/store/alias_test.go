package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/address"
)

func TestParseAliasAddress(t *testing.T) {
	for in, want := range map[string]string{
		"Info@Example.Test":    "info@example.test",
		"@example.test":        "*@example.test",
		"*@example.test":       "*@example.test",
		"sales-*@example.test": "sales-*@example.test",
		"*-list@example.test":  "*-list@example.test",
		"a*z@example.test":     "a*z@example.test",
	} {
		got, wild, err := ParseAliasAddress(in)
		if err != nil || got != want || wild != strings.Contains(want, "*") {
			t.Errorf("ParseAliasAddress(%q) = %q %v %v", in, got, wild, err)
		}
	}
	for _, in := range []string{"", "info", "info@", "a**@example.test", "*a*@example.test", "in fo@example.test",
		"info+tag@example.test", "info@exa mple.test", "info@*.test", "../x@example.test"} {
		if got, _, err := ParseAliasAddress(in); !errors.Is(err, ErrAliasAddress) {
			t.Errorf("ParseAliasAddress(%q) = %q, %v", in, got, err)
		}
	}
	for _, c := range []struct {
		pattern, local string
		ok             bool
		weight         int
	}{
		{"*", "anything", true, 0}, {"sales-*", "sales-nl", true, 6}, {"sales-*", "sales-", true, 6},
		{"sales-*", "sales", false, 6}, {"*-list", "dev-list", true, 5}, {"a*z", "az", true, 2}, {"a*z", "a", false, 2},
		{"info", "info", true, 4}, {"info", "info2", false, 4},
	} {
		if ok, weight := MatchAlias(c.pattern, c.local); ok != c.ok || weight != c.weight {
			t.Errorf("MatchAlias(%q, %q) = %v %d", c.pattern, c.local, ok, weight)
		}
	}
}

func TestAliases(t *testing.T) {
	s := open(t)
	a := account(t, s)
	// A domain of this test's own, so patterns of other tests do not match.
	domain := "alias-" + strings.Split(a.Address, "@")[0][5:] + ".test"
	if err := s.AddDomain(ctx, domain); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		list, _ := s.Aliases(ctx, domain)
		for _, al := range list {
			s.RemoveAlias(ctx, al.Address, "")
		}
		s.RemoveDomain(ctx, domain)
	})
	add := func(addr string, targets ...string) Alias {
		t.Helper()
		al, err := s.AddAlias(ctx, addr, targets)
		if err != nil {
			t.Fatalf("AddAlias(%s): %v", addr, err)
		}
		return al
	}
	info := add("Info@"+domain, a.Address, "Someone@Elsewhere.Example")
	want(t, "alias", info, Alias{Address: "info@" + domain, Targets: []string{"Someone@elsewhere.example", a.Address}})
	// Adding again adds targets and doubles nothing.
	info = add("info@"+domain, a.Address, "third@elsewhere.example")
	want(t, "targets", len(info.Targets), 3)
	add("@"+domain, "catch@elsewhere.example")
	add("sales-*@"+domain, "sales@elsewhere.example")
	add("sales-nl-*@"+domain, "nl@elsewhere.example")

	for _, c := range []struct {
		addr, targets []string
		err           error
	}{
		{[]string{"info@nodomain-" + domain}, []string{a.Address}, ErrNoDomain},
		{[]string{a.Address}, []string{"x@elsewhere.example"}, ErrAccountExists},
		{[]string{"loop@" + domain}, []string{"loop@" + domain}, ErrAliasTarget},
		{[]string{"x@" + domain}, nil, ErrAliasTarget},
		{[]string{"x@" + domain}, []string{"not an address"}, ErrAliasTarget},
		{[]string{"x@" + domain}, []string{"a@b\r\nRCPT TO:<c@d>"}, ErrAliasTarget},
		{[]string{"x y@" + domain}, []string{a.Address}, ErrAliasAddress},
	} {
		if _, err := s.AddAlias(ctx, c.addr[0], c.targets); !errors.Is(err, c.err) {
			t.Errorf("AddAlias(%q, %q): %v, want %v", c.addr[0], c.targets, err, c.err)
		}
	}

	lookup := func(addr string) string {
		t.Helper()
		parsed, err := address.Parse(addr)
		if err != nil {
			t.Fatal(err)
		}
		al, ok, err := s.LookupAlias(ctx, parsed)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return ""
		}
		return al.Address
	}
	for addr, wantAlias := range map[string]string{
		"info@" + domain:           "info@" + domain,
		"info+tag@" + domain:       "info@" + domain, // plus-addressing reaches the alias
		"sales-be@" + domain:       "sales-*@" + domain,
		"sales-nl-north@" + domain: "sales-nl-*@" + domain, // the longer pattern wins
		"whoever@" + domain:        "*@" + domain,
		"whoever+tag@" + domain:    "*@" + domain,
		"info@other-" + domain:     "",
	} {
		if got := lookup(addr); got != wantAlias {
			t.Errorf("LookupAlias(%s) = %q, want %q", addr, got, wantAlias)
		}
	}

	// An account may send as the aliases that name it, not as a pattern.
	add("*-team@"+domain, a.Address)
	send, err := s.SendAddresses(ctx, a.Address)
	if err != nil || !reflect.DeepEqual(send, []string{"info@" + domain}) {
		t.Errorf("SendAddresses = %v, %v", send, err)
	}

	// A domain with aliases cannot be removed.
	if err := s.RemoveDomain(ctx, domain); !errors.Is(err, ErrDomainInUse) {
		t.Errorf("RemoveDomain: %v", err)
	}
	if n, err := s.RemoveAlias(ctx, "info@"+domain, "someone@elsewhere.example"); err != nil || n != 1 {
		t.Errorf("remove one target: %d, %v", n, err)
	}
	if n, err := s.RemoveAlias(ctx, "info@"+domain, ""); err != nil || n != 2 {
		t.Errorf("remove the alias: %d, %v", n, err)
	}
	if _, err := s.RemoveAlias(ctx, "info@"+domain, ""); !errors.Is(err, ErrNoAlias) {
		t.Errorf("remove it again: %v", err)
	}
	list, err := s.Aliases(ctx, domain)
	if err != nil || len(list) != 4 {
		t.Errorf("Aliases: %v, %v", list, err)
	}
}

// Forward queues a message under the system account of a domain, once per key.
func TestForward(t *testing.T) {
	s := open(t)
	system := SystemAccountID("example.test")
	t.Cleanup(func() { s.PurgeSubmissions(ctx, system) })
	key := "test-" + account(t, s).ID
	raw := []byte("From: a@x.test\r\nSubject: forwarded\r\n\r\nbody\r\n")
	if err := s.Forward(ctx, "Example.Test", key, "SRS0=x=y=z=a@example.test", "target@elsewhere.example", raw); err != nil {
		t.Fatal(err)
	}
	if err := s.Forward(ctx, "example.test", key, "SRS0=x=y=z=a@example.test", "target@elsewhere.example", raw); !errors.Is(err, ErrForwarded) {
		t.Fatalf("second forward with the key: %v", err)
	}
	if err := s.Forward(ctx, "../etc", key, "", "target@elsewhere.example", raw); err == nil {
		t.Fatal("a domain that is none was taken")
	}
	res, err := s.query(ctx, system, q(`SELECT mail_from FROM submission WHERE account_id = :a AND recipients LIKE '%target@elsewhere.example%'`))
	if err != nil || len(res[0].Values) != 1 {
		t.Fatalf("queued rows: %v, %v", res, err)
	}
	// A worker limited to a domain finds the row through the account.
	due, err := s.DueSubmissions(ctx, s.Now().Add(time.Minute), 1000, []string{"example.test"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range due {
		found = found || ref.Account == system
	}
	if !found {
		t.Error("the forward is not due for a worker of its domain")
	}
	// A system account is no user.
	list, err := s.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range list {
		if IsSystemAccount(acct.ID) {
			t.Error("a system account is listed as a user")
		}
	}
}
