package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/robinmail/robinmail/internal/store"
)

// aliasAdd gives an alias targets. The address is one address, a pattern
// with one "*", or "@domain" for everything else of a domain.
func (a *app) aliasAdd(addr string, targets []string) error {
	al, err := a.store.AddAlias(a.ctx, addr, targets)
	switch {
	case errors.Is(err, store.ErrAliasAddress):
		return fmt.Errorf("%q is not an alias address: an address, a pattern with one \"*\" such as sales-*@example.org, or @example.org", addr)
	case errors.Is(err, store.ErrAliasTarget):
		return fmt.Errorf("a target must be an address other than the alias itself, at most %d per alias", store.MaxAliasTargets)
	case errors.Is(err, store.ErrNoDomain):
		return fmt.Errorf("%s: the domain is not one robinmail accepts mail for (domain add)", addr)
	case errors.Is(err, store.ErrAccountExists):
		return fmt.Errorf("%s is a user: a user's own mailbox comes before an alias", addr)
	case err != nil:
		return err
	}
	fmt.Fprintf(a.stdout, "%s -> %s\n", al.Address, strings.Join(al.Targets, ", "))
	return nil
}

func (a *app) aliasList(domain string) error {
	list, err := a.store.Aliases(a.ctx, strings.ToLower(domain))
	if err != nil {
		return err
	}
	for _, al := range list {
		fmt.Fprintf(a.stdout, "%s -> %s\n", al.Address, strings.Join(al.Targets, ", "))
	}
	return nil
}

func (a *app) aliasRemove(addr, target string) error {
	n, err := a.store.RemoveAlias(a.ctx, addr, target)
	switch {
	case errors.Is(err, store.ErrAliasAddress):
		return fmt.Errorf("%q is not an alias address", addr)
	case errors.Is(err, store.ErrNoAlias) && target != "":
		return fmt.Errorf("%s has no target %s", addr, target)
	case errors.Is(err, store.ErrNoAlias):
		return fmt.Errorf("%s: no such alias", addr)
	case err != nil:
		return err
	}
	fmt.Fprintf(a.stdout, "removed %d target(s) of %s\n", n, strings.ToLower(addr))
	return nil
}
