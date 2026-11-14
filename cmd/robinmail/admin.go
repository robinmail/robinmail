package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/store"
)

// note writes what an administrator did to the log of auth events, for
// things that are not about one user's ways in: keys, backups, the queue,
// accounts that come and go. "robinmail audit" shows them with the rest.
func (a *app) note(event, address, detail string) {
	acct := store.Account{Address: address}
	if address != "" {
		if found, err := a.store.Account(a.ctx, address); err == nil {
			acct = found
		}
	}
	a.adminEvent(acct, event, detail)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 0:
		return "in " + (-d).Round(time.Second).String()
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < 48*time.Hour:
		return d.Round(time.Minute).String()
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// queueRef reads "<address> <id>" of a queue command.
func (a *app) queueRef(addr, id string) (store.Account, int64, error) {
	acct, err := a.account(addr)
	if err != nil {
		return acct, 0, err
	}
	n, ok := store.ParseSubmissionID(id)
	if !ok {
		return acct, 0, fmt.Errorf("%q is not the id of a queued message (such as s1)", id)
	}
	return acct, n, nil
}

func waiting(list []store.Recipient) (n int, last store.Recipient) {
	for _, r := range list {
		if r.State == store.RcptQueued {
			n++
			if r.At >= last.At {
				last = r
			}
		}
	}
	return n, last
}

// queueList shows the messages that still have work, the one due first
// first.
func (a *app) queueList(countArg string) error {
	count, err := strconv.Atoi(countArg)
	if err != nil || count < 1 || count > 10000 {
		return fmt.Errorf("%q is not a number of messages from 1 to 10000", countArg)
	}
	list, err := a.store.Queue(a.ctx, count)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, e := range list {
		n, last := waiting(e.Recipients)
		state := "next try " + ago(e.NextTry)
		switch {
		case e.ClaimedUntil.After(now):
			state = "being sent now"
		case !e.NextTry.After(now):
			state = "due"
		case e.Tries == 0:
			state = "starts " + ago(e.NextTry)
		}
		from := e.MailFrom
		if from == "" {
			from = "<>"
		}
		fmt.Fprintf(a.stdout, "%-32s  %-5s  %-6s  tries %-2d  %-22s  %s -> %d of %d waiting", clip(e.Address, 32), store.SubmissionID(e.ID),
			ago(e.CreatedAt), e.Tries, state, clip(from, 40), n, len(e.Recipients))
		if last.Reply != "" {
			fmt.Fprintf(a.stdout, "  [%s: %s]", clip(last.Address, 40), clip(last.Reply, 80))
		}
		fmt.Fprintln(a.stdout)
	}
	st, err := a.store.QueueStats(a.ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%d in the queue: %d due, %d being sent, %d waiting\n", st.Active, st.Due, st.Claimed, st.Waiting)
	return nil
}

func (a *app) queueShow(addr, id string) error {
	acct, n, err := a.queueRef(addr, id)
	if err != nil {
		return err
	}
	e, err := a.store.Queued(a.ctx, acct.ID, n)
	if errors.Is(err, store.ErrNoSubmission) {
		return fmt.Errorf("%s has no message %s in the queue", acct.Address, id)
	}
	if err != nil {
		return err
	}
	from := e.MailFrom
	if from == "" {
		from = "<> (a notification)"
	}
	fmt.Fprintf(a.stdout, "message     %s of %s\nqueue       %s\nfrom        %s\nmessage-id  %s\nsize        %d\nsubmitted   %s\ntries       %d\n",
		store.SubmissionID(e.ID), e.Address, e.Queue, from, e.MessageID, e.Size, e.CreatedAt.Local().Format("2006-01-02 15:04:05"), e.Tries)
	if e.Queue == store.QueueActive {
		fmt.Fprintf(a.stdout, "next try    %s\n", e.NextTry.Local().Format("2006-01-02 15:04:05"))
		if e.ClaimedUntil.After(time.Now()) {
			fmt.Fprintf(a.stdout, "claimed     by a node, until %s\n", e.ClaimedUntil.Local().Format("15:04:05"))
		}
	}
	for _, r := range e.Recipients {
		fmt.Fprintf(a.stdout, "  %-9s  %s", r.State, r.Address)
		if r.Reply != "" {
			fmt.Fprintf(a.stdout, "  %s %s", r.Status, r.Reply)
		}
		if r.MTA != "" {
			fmt.Fprintf(a.stdout, "  (%s)", r.MTA)
		}
		fmt.Fprintln(a.stdout)
	}
	return nil
}

// queueRetry makes a message, or all of them, due now. A running server
// takes them at its next look at the queue.
func (a *app) queueRetry(args []string) error {
	if len(args) == 1 {
		n, err := a.store.RetryQueued(a.ctx, "", 0)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "%d messages are due now\n", n)
		return nil
	}
	acct, id, err := a.queueRef(args[0], args[1])
	if err != nil {
		return err
	}
	n, err := a.store.RetryQueued(a.ctx, acct.ID, id)
	switch {
	case errors.Is(err, store.ErrNoSubmission):
		return fmt.Errorf("%s has no message %s in the queue", acct.Address, args[1])
	case err != nil:
		return err
	case n == 0:
		fmt.Fprintf(a.stdout, "%s was due already\n", args[1])
	default:
		fmt.Fprintf(a.stdout, "%s is due now\n", args[1])
	}
	return nil
}

// queueDelete gives a message up. Nobody is told by mail; the sender's
// mail program shows the recipients as failed.
func (a *app) queueDelete(addr, id string) error {
	acct, n, err := a.queueRef(addr, id)
	if err != nil {
		return err
	}
	dropped, err := a.store.DropQueued(a.ctx, acct.ID, n, "Removed from the queue by the administrator")
	switch {
	case errors.Is(err, store.ErrNoSubmission):
		return fmt.Errorf("%s has no message %s in the queue", acct.Address, id)
	case errors.Is(err, store.ErrQueueBusy):
		return fmt.Errorf("%s is being sent right now: try again in a moment", id)
	case err != nil:
		return err
	}
	a.adminEvent(acct, "queue-deleted", id+" for "+clip(strings.Join(dropped, ", "), 200))
	fmt.Fprintf(a.stdout, "removed %s from the queue; not sent to %s\nNo bounce is sent for it.\n", id, strings.Join(dropped, ", "))
	return nil
}

// userShow prints what the server has about an account.
func (a *app) userShow(s string) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "address       %s\naccount       %s\ncreated       %s\nstate         %d\n", acct.Address, acct.ID,
		acct.CreatedAt.Local().Format("2006-01-02 15:04:05"), acct.State)
	q, err := a.store.Quota(a.ctx, acct.ID)
	if err != nil {
		return err
	}
	limit := func(n int64, f func(int64) string) string {
		if n == 0 {
			return "no limit"
		}
		return "limit " + f(n)
	}
	plain := func(n int64) string { return strconv.FormatInt(n, 10) }
	fmt.Fprintf(a.stdout, "mail          %d messages (%s), %s (%s)\n", q.UsedMessages, limit(q.Messages, plain), octets(q.UsedBytes), limit(q.Bytes, octets))
	boxes, err := a.store.Mailboxes(a.ctx, acct.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "mailboxes     %d\n", len(boxes))
	password := "set"
	if acct.PasswordHash == "!" {
		password = "none (logs in through a provider)"
	}
	fmt.Fprintf(a.stdout, "password      %s\n", password)
	if t, err := a.store.TOTPState(a.ctx, acct.ID); err == nil && t.Confirmed {
		fmt.Fprintf(a.stdout, "two-step      on, %d recovery codes left\n", t.RecoveryCodes)
	} else {
		fmt.Fprintf(a.stdout, "two-step      off\n")
	}
	apps, err := a.store.AppPasswords(a.ctx, acct.ID)
	if err != nil {
		return err
	}
	sessions, err := a.store.Sessions(a.ctx, acct.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "app passwords %d\nweb sessions  %d\n", len(apps), len(sessions))
	aliases, err := a.store.SendAddresses(a.ctx, acct.Address)
	if err != nil {
		return err
	}
	if len(aliases) > 0 {
		fmt.Fprintf(a.stdout, "aliases       %s\n", strings.Join(aliases, ", "))
	}
	return nil
}

// userRemove deletes an account with everything it has. It wants "--yes":
// there is no way back but a backup.
func (a *app) userRemove(s string, confirmed bool) error {
	acct, err := a.account(s)
	if err != nil {
		return err
	}
	q, err := a.store.Quota(a.ctx, acct.ID)
	if err != nil {
		return err
	}
	if !confirmed {
		return fmt.Errorf("%s has %d messages (%s). Removing the account deletes them and cannot be undone: run \"robinmail user remove %s --yes\"",
			acct.Address, q.UsedMessages, octets(q.UsedBytes), acct.Address)
	}
	if err := a.store.RemoveAccount(a.ctx, acct.ID); err != nil {
		return err
	}
	a.adminEvent(store.Account{Address: acct.Address}, "account-removed", fmt.Sprintf("account %s, %d messages", acct.ID, q.UsedMessages))
	fmt.Fprintf(a.stdout, "removed %s with %d messages\nIts blob files leave each node's disk with the next sweep, a day from now at the earliest.\n", acct.Address, q.UsedMessages)
	return nil
}
