package jmap

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/message"
	"github.com/robinmail/robinmail/internal/store"
)

var submissionProperties = []string{"id", "identityId", "emailId", "threadId", "envelope", "sendAt", "undoStatus",
	"deliveryStatus", "dsnBlobIds", "mdnBlobIds"}

// deliveryStatus is the DeliveryStatus of every recipient (RFC 8621 7).
//
// delivered is "queued" while we still have the message, "no" when it
// could not be delivered, "yes" when it is known to be in the recipient's
// mailbox (a mailbox on this server, or a notification that says so), and
// "unknown" when another server took it: what happens after that we only
// hear about if it fails. displayed is always "unknown": we do not ask for
// read receipts.
func deliveryStatus(recipients []store.Recipient) map[string]any {
	out := map[string]any{}
	for _, r := range recipients {
		delivered := "queued"
		switch {
		case r.State == store.RcptFailed:
			delivered = "no"
		case r.State == store.RcptDelivered && r.Mailbox:
			delivered = "yes"
		case r.State == store.RcptDelivered:
			delivered = "unknown"
		}
		out[r.Address] = map[string]any{"smtpReply": r.Reply, "delivered": delivered, "displayed": "unknown"}
	}
	return out
}

// submissionObject is an EmailSubmission (RFC 8621 7).
func submissionObject(s store.Submission) map[string]any {
	rcpts := make([]map[string]any, len(s.Recipients))
	for i, r := range s.Recipients {
		rcpts[i] = map[string]any{"email": r.Address, "parameters": nil}
	}
	return map[string]any{
		"id":         store.SubmissionID(s.ID),
		"identityId": store.IdentityID(s.IdentityID),
		"emailId":    store.EmailID(s.EmailID),
		"threadId":   store.ThreadID(s.ThreadID),
		"envelope": map[string]any{
			"mailFrom": map[string]any{"email": s.MailFrom, "parameters": nil},
			"rcptTo":   rcpts,
		},
		"sendAt":         utcDate(s.SendAt),
		"undoStatus":     s.Undo,
		"deliveryStatus": deliveryStatus(s.Recipients),
		// Notifications arrive as ordinary mail in the Inbox; they are
		// not linked here.
		"dsnBlobIds": []string{},
		"mdnBlobIds": []string{},
	}
}

// submissionGet is EmailSubmission/get (RFC 8621 7.1).
func submissionGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(args.Properties, submissionProperties)
	if e != nil {
		return nil, e
	}
	if args.IDs != nil && len(*args.IDs) > c.h.limits.MaxObjectsInGet {
		return nil, mErr("requestTooLarge", "more ids than maxObjectsInGet")
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	var ids []int64
	if args.IDs != nil {
		ids = []int64{}
		for _, s := range *args.IDs {
			if id, ok := store.ParseSubmissionID(s); ok {
				ids = append(ids, id)
			}
		}
	}
	subs, err := c.h.Store.Submissions(c.ctx, args.AccountID, ids)
	if err != nil {
		return nil, storeErr(err)
	}
	list, notFound := []map[string]any{}, []string{}
	if args.IDs == nil {
		for _, s := range subs {
			list = append(list, pick(submissionObject(s), props))
		}
	} else {
		byID := map[string]store.Submission{}
		for _, s := range subs {
			byID[store.SubmissionID(s.ID)] = s
		}
		seen := map[string]bool{}
		for _, id := range *args.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			if s, ok := byID[id]; ok {
				list = append(list, pick(submissionObject(s), props))
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

// submissionChanges is EmailSubmission/changes (RFC 8621 7.2).
func submissionChanges(c *call, raw json.RawMessage) (any, *methodError) {
	return c.changes(raw, "EmailSubmission", store.SubmissionID)
}

type submissionQueryArgs struct {
	AccountID string          `json:"accountId"`
	Filter    json.RawMessage `json:"filter"`
	Sort      *[]struct {
		Property    string  `json:"property"`
		IsAscending *bool   `json:"isAscending"`
		Collation   *string `json:"collation"`
	} `json:"sort"`
	queryArgs
}

// submissionFilter is a parsed filter of EmailSubmission/query (RFC 8621
// 7.3): a condition, or an operator over conditions.
type submissionFilter struct {
	operator   string
	conditions []*submissionFilter

	identityIDs, emailIDs, threadIDs *[]string
	undoStatus                       *string
	before, after                    *time.Time
}

func parseSubmissionFilter(raw json.RawMessage, depth int, nodes *int) (*submissionFilter, *methodError) {
	if *nodes++; *nodes > maxFilterNodes || depth > maxFilterDepth {
		return nil, invalidArgs("the filter is too large")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, invalidArgs("a filter must be an object")
	}
	f := &submissionFilter{}
	if op, ok := obj["operator"]; ok {
		var conds []json.RawMessage
		if json.Unmarshal(op, &f.operator) != nil || (f.operator != "AND" && f.operator != "OR" && f.operator != "NOT") {
			return nil, invalidArgs("a filter operator must be AND, OR or NOT")
		}
		if len(obj) != 2 || json.Unmarshal(obj["conditions"], &conds) != nil || conds == nil {
			return nil, invalidArgs("a filter operator needs conditions and nothing else")
		}
		for _, cr := range conds {
			sub, e := parseSubmissionFilter(cr, depth+1, nodes)
			if e != nil {
				return nil, e
			}
			f.conditions = append(f.conditions, sub)
		}
		return f, nil
	}
	for key, v := range obj {
		bad := invalidArgs("the filter property %q has the wrong type", key)
		date := func(dst **time.Time) *methodError {
			var s string
			if json.Unmarshal(v, &s) != nil {
				return bad
			}
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return bad
			}
			*dst = &t
			return nil
		}
		ids := func(dst **[]string) *methodError {
			if json.Unmarshal(v, dst) != nil || *dst == nil || len(**dst) > maxFilterNodes {
				return bad
			}
			return nil
		}
		var e *methodError
		switch key {
		case "identityIds":
			e = ids(&f.identityIDs)
		case "emailIds":
			e = ids(&f.emailIDs)
		case "threadIds":
			e = ids(&f.threadIDs)
		case "undoStatus":
			if json.Unmarshal(v, &f.undoStatus) != nil || f.undoStatus == nil {
				e = bad
			}
		case "before":
			e = date(&f.before)
		case "after":
			e = date(&f.after)
		default:
			e = invalidArgs("unknown filter property %q", key)
		}
		if e != nil {
			return nil, e
		}
	}
	return f, nil
}

func (f *submissionFilter) match(s store.Submission) bool {
	switch f.operator {
	case "AND":
		for _, c := range f.conditions {
			if !c.match(s) {
				return false
			}
		}
		return true
	case "OR":
		for _, c := range f.conditions {
			if c.match(s) {
				return true
			}
		}
		return false
	case "NOT":
		for _, c := range f.conditions {
			if c.match(s) {
				return false
			}
		}
		return true
	}
	in := func(list *[]string, id string) bool { return list == nil || contains(*list, id) }
	return in(f.identityIDs, store.IdentityID(s.IdentityID)) && in(f.emailIDs, store.EmailID(s.EmailID)) &&
		in(f.threadIDs, store.ThreadID(s.ThreadID)) &&
		(f.undoStatus == nil || *f.undoStatus == s.Undo) &&
		(f.before == nil || s.SendAt.Before(*f.before)) &&
		(f.after == nil || !s.SendAt.Before(*f.after))
}

// runSubmissionQuery returns the ids of all submissions that match, in
// order. Submissions are few per account, so this filters and sorts in Go.
func (c *call) runSubmissionQuery(args *submissionQueryArgs) ([]string, *methodError) {
	var filter *submissionFilter
	if len(args.Filter) > 0 && string(args.Filter) != "null" {
		nodes := 0
		f, e := parseSubmissionFilter(args.Filter, 0, &nodes)
		if e != nil {
			return nil, e
		}
		filter = f
	}
	type key struct {
		property  string
		ascending bool
	}
	var keys []key
	if args.Sort != nil {
		for _, s := range *args.Sort {
			if s.Property != "emailId" && s.Property != "threadId" && s.Property != "sentAt" {
				return nil, mErr("unsupportedSort", "the server cannot sort submissions by %q", s.Property)
			}
			if s.Collation != nil {
				return nil, mErr("unsupportedSort", "none of these properties takes a collation")
			}
			keys = append(keys, key{s.Property, s.IsAscending == nil || *s.IsAscending})
		}
	}
	subs, err := c.h.Store.Submissions(c.ctx, args.AccountID, nil)
	if err != nil {
		return nil, storeErr(err)
	}
	var match []store.Submission
	for _, s := range subs {
		if filter == nil || filter.match(s) {
			match = append(match, s)
		}
	}
	sort.SliceStable(match, func(i, j int) bool {
		a, b := match[i], match[j]
		for _, k := range keys {
			var d int64
			switch k.property {
			case "emailId":
				d = a.EmailID - b.EmailID
			case "threadId":
				d = a.ThreadID - b.ThreadID
			case "sentAt":
				d = a.SendAt.Unix() - b.SendAt.Unix()
			}
			if d != 0 {
				return (d < 0) == k.ascending
			}
		}
		return a.ID < b.ID
	})
	ids := make([]string, len(match))
	for i, s := range match {
		ids[i] = store.SubmissionID(s.ID)
	}
	return ids, nil
}

// submissionQuery is EmailSubmission/query (RFC 8621 7.3).
func submissionQuery(c *call, raw json.RawMessage) (any, *methodError) {
	var args submissionQueryArgs
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	limit, clamped, e := args.limit()
	if e != nil {
		return nil, e
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	all, e := c.runSubmissionQuery(&args)
	if e != nil {
		return nil, e
	}
	position, ids, e := window(all, args.Position, args.Anchor, args.AnchorOffset, limit)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"accountId": args.AccountID, "queryState": stateString(state), "canCalculateChanges": true,
		"position": position, "ids": ids}
	if args.CalculateTotal {
		out["total"] = len(all)
	}
	if clamped {
		out["limit"] = limit
	}
	return out, nil
}

// submissionQueryChanges is EmailSubmission/queryChanges (RFC 8621 7.4).
func submissionQueryChanges(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		submissionQueryArgs
		queryChangesArgs
	}
	var probe map[string]json.RawMessage
	json.Unmarshal(raw, &probe)
	for _, k := range []string{"position", "anchor", "anchorOffset", "limit"} {
		if _, ok := probe[k]; ok {
			return nil, invalidArgs("unknown argument %q", k)
		}
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if args.SinceQueryState == nil {
		return nil, invalidArgs("sinceQueryState is required")
	}
	since, ok := parseState(*args.SinceQueryState)
	if !ok {
		return nil, mErr("cannotCalculateChanges", "the server cannot calculate the changes from that state")
	}
	set, now, _, err := c.h.Store.ChangesOf(c.ctx, args.AccountID, "EmailSubmission", since, 0)
	if err != nil {
		return nil, storeErr(err)
	}
	results, e := c.runSubmissionQuery(&args.submissionQueryArgs)
	if e != nil {
		return nil, e
	}
	var candidates []string
	for _, list := range [][]int64{set.Created, set.Updated, set.Destroyed} {
		for _, id := range list {
			candidates = append(candidates, store.SubmissionID(id))
		}
	}
	return queryChanges(args.AccountID, args.queryChangesArgs, since, now, candidates, results)
}

// envelopeAddress is an Address of an Envelope (RFC 8621 7).
type envelopeAddress struct {
	Email      *string        `json:"email"`
	Parameters map[string]any `json:"parameters"`
}

// envelopeOf works out who a message is from and to, on the envelope.
//
// A given envelope is taken as it is. Without one it comes from the
// message (RFC 8621 7.5): the sender is the address in Sender if there is
// one, otherwise the one in From, and it must be exactly one; the
// recipients are the addresses of To, Cc and Bcc, each once.
func envelopeOf(raw json.RawMessage, msg []byte) (from string, rcpts []string, se *setError) {
	if len(raw) > 0 && string(raw) != "null" {
		var env struct {
			MailFrom *envelopeAddress  `json:"mailFrom"`
			RcptTo   []envelopeAddress `json:"rcptTo"`
		}
		if decodeStrict(raw, &env) != nil || env.MailFrom == nil || env.MailFrom.Email == nil {
			return "", nil, invalidProps("an envelope has mailFrom and rcptTo", "envelope")
		}
		// No SMTP extension with parameters of its own is offered
		// (submissionExtensions is empty), so none can be asked for.
		if len(env.MailFrom.Parameters) > 0 {
			return "", nil, invalidProps("the server takes no parameters on an envelope address", "envelope")
		}
		for _, r := range env.RcptTo {
			if r.Email == nil || len(r.Parameters) > 0 {
				return "", nil, invalidProps("the server takes no parameters on an envelope address", "envelope")
			}
			rcpts = append(rcpts, *r.Email)
		}
		if len(rcpts) == 0 {
			return "", nil, &setError{Type: "noRecipients", Description: "the envelope has no recipients"}
		}
		return *env.MailFrom.Email, rcpts, nil
	}
	fields := message.ParseHeader(msg)
	source := message.All(fields, "Sender")
	if len(source) == 0 {
		source = message.All(fields, "From")
	}
	var senders []message.Address
	if len(source) == 1 {
		senders = message.Addresses(source[0])
	}
	if len(senders) != 1 {
		return "", nil, &setError{Type: "invalidEmail", Description: "the email needs one sender: one address in Sender, or else in From",
			Properties: []string{"from"}}
	}
	seen := map[string]bool{}
	for _, name := range []string{"To", "Cc", "Bcc"} {
		for _, v := range message.All(fields, name) {
			for _, a := range message.Addresses(v) {
				if key := strings.ToLower(a.Email); !seen[key] {
					seen[key] = true
					rcpts = append(rcpts, a.Email)
				}
			}
		}
	}
	if len(rcpts) == 0 {
		return "", nil, &setError{Type: "noRecipients", Description: "the email has no address in To, Cc or Bcc"}
	}
	return senders[0].Email, rcpts, nil
}

// submissionSet is EmailSubmission/set (RFC 8621 7.5).
//
// A creation sends an Email: it is checked, scanned and signed (Send) and
// put in the queue. For a moment the server sets it can still be taken
// back by setting undoStatus to "canceled", the only update there is;
// after that it answers cannotUnsend. Destroying a submission removes the
// record, not the message from the queue.
//
// onSuccessUpdateEmail and onSuccessDestroyEmail become one Email/set that
// runs after this call and answers after it: moving the message from
// Drafts to Sent and dropping $draft is how a client finishes a send.
func submissionSet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		setArgs
		OnSuccessUpdateEmail  map[string]map[string]json.RawMessage `json:"onSuccessUpdateEmail"`
		OnSuccessDestroyEmail []string                              `json:"onSuccessDestroyEmail"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	run, e := c.begin(&args.setArgs)
	if e != nil {
		return nil, e
	}
	st := c.h.Store
	mismatch := mErr("stateMismatch", "the state is not the ifInState given")
	// emailOf maps what onSuccess* may name, "#creation id" or the id of
	// the submission, to the Email of a submission this call made.
	emailOf := map[string]string{}

	cids := make([]string, 0, len(args.Create))
	for cid := range args.Create {
		cids = append(cids, cid)
	}
	sort.Strings(cids)
	for _, cid := range cids {
		var obj map[string]json.RawMessage
		if json.Unmarshal(args.Create[cid], &obj) != nil || obj == nil {
			run.notCreated[cid] = invalidProps("a submission to create must be an object")
			continue
		}
		var bad []string
		for p := range obj {
			if p != "identityId" && p != "emailId" && p != "envelope" {
				bad = append(bad, p)
			}
		}
		var identitySID, emailSID string
		identityID, okI := int64(0), false
		if json.Unmarshal(obj["identityId"], &identitySID) == nil {
			identityID, okI = store.ParseIdentityID(identitySID)
		}
		if !okI {
			bad = append(bad, "identityId")
		}
		emailID, okE := int64(0), false
		if json.Unmarshal(obj["emailId"], &emailSID) == nil {
			emailID, okE = c.emailRef(emailSID)
		}
		if !okE {
			bad = append(bad, "emailId")
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			run.notCreated[cid] = invalidProps("a submission is created from identityId, emailId and envelope", bad...)
			continue
		}
		msg, _, err := st.Raw(c.ctx, args.AccountID, emailID)
		if errors.Is(err, store.ErrNoEmail) {
			run.notCreated[cid] = invalidProps("there is no such email", "emailId")
			continue
		}
		if err != nil {
			run.notCreated[cid] = &setError{Type: "serverFail"}
			continue
		}
		from, rcpts, se := envelopeOf(obj["envelope"], msg)
		if se != nil {
			run.notCreated[cid] = se
			continue
		}
		if c.h.Send == nil {
			run.notCreated[cid] = &setError{Type: "forbiddenToSend", Description: "this server does not send mail"}
			continue
		}
		sub, sendErr, err := c.h.Send(c.ctx, c.user, Outgoing{EmailID: emailID, IdentityID: identityID, MailFrom: from,
			RcptTo: rcpts, Raw: msg, IfInState: run.ifState})
		switch {
		case errors.Is(err, store.ErrStateMismatch):
			return nil, mismatch
		case errors.Is(err, store.ErrNoIdentity):
			run.notCreated[cid] = invalidProps("there is no such identity", "identityId")
		case errors.Is(err, store.ErrNoEmail):
			run.notCreated[cid] = invalidProps("there is no such email", "emailId")
		case err != nil:
			run.notCreated[cid] = &setError{Type: "serverFail"}
		case sendErr != nil:
			run.notCreated[cid] = &setError{Type: sendErr.Type, Description: sendErr.Description,
				InvalidRecipients: sendErr.InvalidRecipients, MaxRecipients: sendErr.MaxRecipients, MaxSize: sendErr.MaxSize}
		default:
			run.wroteState(sub.Modseq)
			if run.created == nil {
				run.created = map[string]any{}
			}
			full := submissionObject(sub)
			sid := full["id"].(string)
			run.created[cid] = pick(full, []string{"threadId", "sendAt", "undoStatus", "deliveryStatus", "dsnBlobIds", "mdnBlobIds"})
			c.created[cid] = sid
			emailOf["#"+cid], emailOf[sid] = store.EmailID(emailID), store.EmailID(emailID)
		}
	}

	sids := make([]string, 0, len(args.Update))
	for sid := range args.Update {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	for _, sid := range sids {
		id, ok := store.ParseSubmissionID(sid)
		if !ok {
			run.notUpdated[sid] = &setError{Type: "notFound"}
			continue
		}
		patch := args.Update[sid]
		var bad []string
		for p := range patch {
			if p != "undoStatus" {
				bad = append(bad, p)
			}
		}
		var undo string
		if v, given := patch["undoStatus"]; given && (json.Unmarshal(v, &undo) != nil || undo != store.UndoCanceled) {
			bad = append(bad, "undoStatus")
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			run.notUpdated[sid] = invalidProps(`the only change to a submission is undoStatus to "canceled"`, bad...)
			continue
		}
		if undo == "" {
			// An empty patch: nothing to do, if the submission exists.
			if found, err := st.Submissions(c.ctx, args.AccountID, []int64{id}); err != nil || len(found) == 0 {
				run.notUpdated[sid] = &setError{Type: "notFound"}
				continue
			}
		} else {
			state, err := st.CancelSubmission(c.ctx, args.AccountID, id, run.ifState)
			switch {
			case errors.Is(err, store.ErrStateMismatch):
				return nil, mismatch
			case errors.Is(err, store.ErrNoSubmission):
				run.notUpdated[sid] = &setError{Type: "notFound"}
				continue
			case errors.Is(err, store.ErrCannotUnsend):
				run.notUpdated[sid] = &setError{Type: "cannotUnsend", Description: "the message is already on its way"}
				continue
			case err != nil:
				run.notUpdated[sid] = &setError{Type: "serverFail"}
				continue
			}
			run.wroteState(state)
		}
		if run.updated == nil {
			run.updated = map[string]any{}
		}
		run.updated[sid] = nil
	}

	if args.Destroy != nil {
		for _, sid := range *args.Destroy {
			id, ok := store.ParseSubmissionID(sid)
			if !ok {
				run.notDestroyed[sid] = &setError{Type: "notFound"}
				continue
			}
			state, err := st.DestroySubmission(c.ctx, args.AccountID, id, run.ifState)
			switch {
			case errors.Is(err, store.ErrStateMismatch):
				return nil, mismatch
			case errors.Is(err, store.ErrNoSubmission):
				run.notDestroyed[sid] = &setError{Type: "notFound"}
			case err != nil:
				run.notDestroyed[sid] = &setError{Type: "serverFail"}
			default:
				run.wroteState(state)
				run.destroyed = append(run.destroyed, sid)
			}
		}
	}

	// RFC 8621 7.5: the changes to Emails apply only for submissions that
	// were made, and come as an Email/set of their own.
	update := map[string]map[string]json.RawMessage{}
	for key, patch := range args.OnSuccessUpdateEmail {
		if emailID, ok := emailOf[key]; ok {
			update[emailID] = patch
		}
	}
	var destroy []string
	for _, key := range args.OnSuccessDestroyEmail {
		if emailID, ok := emailOf[key]; ok {
			destroy = append(destroy, emailID)
		}
	}
	if len(update) > 0 || len(destroy) > 0 {
		set := map[string]any{"accountId": args.AccountID}
		if len(update) > 0 {
			set["update"] = update
		}
		if len(destroy) > 0 {
			set["destroy"] = destroy
		}
		c.imply("Email/set", set, emailSet)
	}
	return run.result(), nil
}
