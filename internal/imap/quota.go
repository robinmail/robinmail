package imap

import (
	"context"
	"fmt"
	"strings"
)

// QUOTA (RFC 9208): what an account may hold and holds. There is one
// quota root per account, named "", and every mailbox is under it; the
// resources are STORAGE, in units of 1024 octets, and MESSAGE. A resource
// without a limit is not listed, and an account without any limit has no
// quota root. Limits are set by the administrator, not over IMAP:
// QUOTASET is not offered.

// Quota is an account's use and limits. A limit of 0 is none.
type Quota struct {
	StorageUsed, StorageLimit int64 // octets
	MessageUsed, MessageLimit int64
}

// QuotaUser is a User whose store has quotas.
type QuotaUser interface {
	Quota(ctx context.Context) (Quota, error)
}

// quotaRoot is the name of the one quota root.
const quotaRoot = ""

// kib rounds octets up to units of 1024 (RFC 9208 5.1: "STORAGE ... in
// units of 1024 octets"): a mailbox with one octet in it is not empty.
func kib(octets int64) int64 { return (octets + 1023) / 1024 }

// quotaLine is the QUOTA response for the account, and whether it has any
// limit.
func quotaLine(q Quota) (string, bool) {
	var res []string
	if q.StorageLimit > 0 {
		// The limit is rounded down: the client must not see room that is
		// not there.
		res = append(res, fmt.Sprintf("STORAGE %d %d", kib(q.StorageUsed), q.StorageLimit/1024))
	}
	if q.MessageLimit > 0 {
		res = append(res, fmt.Sprintf("MESSAGE %d %d", q.MessageUsed, q.MessageLimit))
	}
	return fmt.Sprintf(`* QUOTA "%s" (%s)`, quotaRoot, strings.Join(res, " ")), len(res) > 0
}

func (c *conn) quota() (Quota, bool, error) {
	qu, ok := c.user.(QuotaUser)
	if !ok {
		return Quota{}, false, nil
	}
	q, err := qu.Quota(c.ctx)
	return q, true, err
}

// cmdGetQuota is GETQUOTA (RFC 9208 4.2).
func (c *conn) cmdGetQuota(tag string, p *parser) (reply, error) {
	if err := p.space(); err != nil {
		return reply{}, err
	}
	root, err := p.astring()
	if err != nil {
		return reply{}, err
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	q, _, err := c.quota()
	if err != nil {
		return c.failure("GETQUOTA", err), nil
	}
	line, limited := quotaLine(q)
	if root != quotaRoot || !limited {
		return noCode("NONEXISTENT", "no such quota root"), nil
	}
	c.printf("%s", line)
	return ok("GETQUOTA completed"), nil
}

// cmdGetQuotaRoot is GETQUOTAROOT (RFC 9208 4.3): the quota roots of a
// mailbox, and the QUOTA of each.
func (c *conn) cmdGetQuotaRoot(tag string, p *parser) (reply, error) {
	if err := p.space(); err != nil {
		return reply{}, err
	}
	path, err := c.mailboxArg(p)
	if err != nil {
		return c.failure("GETQUOTAROOT", err), nil
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	box, _, err := c.lookup(path)
	if err != nil {
		return c.failure("GETQUOTAROOT", err), nil
	}
	if box == nil {
		return noCode("NONEXISTENT", "no such mailbox"), nil
	}
	q, _, err := c.quota()
	if err != nil {
		return c.failure("GETQUOTAROOT", err), nil
	}
	line, limited := quotaLine(q)
	if !limited {
		// "the mailbox has no quota root": the list is empty.
		c.printf("* QUOTAROOT %s", c.encodeName(path))
		return ok("GETQUOTAROOT completed"), nil
	}
	c.printf(`* QUOTAROOT %s "%s"`, c.encodeName(path), quotaRoot)
	c.printf("%s", line)
	return ok("GETQUOTAROOT completed"), nil
}

// cmdSetQuota is SETQUOTA (RFC 9208 4.1). A user does not set their own
// limits; the capability QUOTASET is not announced.
func (c *conn) cmdSetQuota(tag string, p *parser) (reply, error) {
	return noCode("NOPERM", "quotas are set by the administrator"), nil
}
