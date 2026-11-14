package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mikepage/robinmail/internal/store"
)

// parseLimit reads a limit from the command line: a number, for octets
// also with K, M or G (powers of 1024); "none" is no limit (0), "default"
// the default of the config (nil).
func parseLimit(s string, units bool) (*int64, error) {
	switch strings.ToLower(s) {
	case "default":
		return nil, nil
	case "none", "0":
		zero := int64(0)
		return &zero, nil
	}
	shift := uint(0)
	digits := s
	if units && len(s) > 1 {
		if sh, ok := map[byte]uint{'k': 10, 'm': 20, 'g': 30, 't': 40}[s[len(s)-1]|0x20]; ok {
			shift, digits = sh, s[:len(s)-1]
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n < 0 || n > (1<<62)>>shift {
		return nil, fmt.Errorf("%q is not a limit: a number%s, \"none\" or \"default\"", s, map[bool]string{true: " of octets, with K, M or G", false: ""}[units])
	}
	n <<= shift
	return &n, nil
}

// octets writes a size the way a person reads it.
func octets(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d octets", n)
}

func (a *app) userQuota(addr string, set []string) error {
	acct, err := a.account(addr)
	if err != nil {
		return err
	}
	if len(set) == 2 {
		bytes, err := parseLimit(set[0], true)
		if err != nil {
			return err
		}
		messages, err := parseLimit(set[1], false)
		if err != nil {
			return err
		}
		if err := a.store.SetQuota(a.ctx, acct.ID, bytes, messages); err != nil {
			return err
		}
	}
	q, err := a.store.Quota(a.ctx, acct.ID)
	if err != nil {
		return err
	}
	limit := func(limit int64, own *int64, show func(int64) string) string {
		from := "the default"
		if own != nil {
			from = "its own"
		}
		if limit == 0 {
			return "no limit (" + from + ")"
		}
		return "of " + show(limit) + " (" + from + ")"
	}
	fmt.Fprintf(a.stdout, "%s\n  storage   %s %s\n  messages  %d %s\n", acct.Address,
		octets(q.UsedBytes), limit(q.Bytes, q.OwnBytes, octets),
		q.UsedMessages, limit(q.Messages, q.OwnMessages, func(n int64) string { return strconv.FormatInt(n, 10) }))
	return nil
}

// quotaRecount sets the quota counters of accounts to what their emails
// add up to. The triggers of the schema keep them right; this is for a
// database that was restored or changed by hand.
func (a *app) quotaRecount(addr string) error {
	var accounts []store.Account
	if addr != "" {
		acct, err := a.account(addr)
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
	corrected := 0
	for _, acct := range accounts {
		changed, err := a.store.RecountQuota(a.ctx, acct.ID)
		if err != nil {
			return fmt.Errorf("%s: %w", acct.Address, err)
		}
		if changed {
			corrected++
			fmt.Fprintf(a.stdout, "%s: corrected\n", acct.Address)
		}
	}
	fmt.Fprintf(a.stdout, "%d accounts counted, %d corrected\n", len(accounts), corrected)
	return nil
}
