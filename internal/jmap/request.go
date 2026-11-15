package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/robinmail/robinmail/internal/store"
)

// Request-level error types (RFC 8620 3.6.1).
const (
	errUnknownCapability = "urn:ietf:params:jmap:error:unknownCapability"
	errNotJSON           = "urn:ietf:params:jmap:error:notJSON"
	errNotRequest        = "urn:ietf:params:jmap:error:notRequest"
	errLimit             = "urn:ietf:params:jmap:error:limit"
)

// methodError is a method-level error (RFC 8620 3.6.2): it becomes an
// "error" response in place of the method's own.
type methodError struct {
	Type        string
	Description string
	Extra       map[string]any
}

func mErr(typ, format string, args ...any) *methodError {
	return &methodError{Type: typ, Description: fmt.Sprintf(format, args...)}
}

func invalidArgs(format string, args ...any) *methodError {
	return mErr("invalidArguments", format, args...)
}

// storeErr turns an unexpected store error into serverFail. The detail
// goes nowhere: it may name internals.
func storeErr(err error) *methodError {
	switch {
	case errors.Is(err, store.ErrNoAccount):
		return mErr("accountNotFound", "no such account")
	case errors.Is(err, store.ErrStateMismatch):
		return mErr("stateMismatch", "the state is not the ifInState given")
	case errors.Is(err, store.ErrCannotCalculateChanges):
		return mErr("cannotCalculateChanges", "the server cannot calculate the changes from that state")
	}
	return mErr("serverFail", "the server could not complete the request")
}

// invocation is one element of methodResponses.
type invocation struct {
	name   string
	args   json.RawMessage
	callID string
}

func (i invocation) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{i.name, i.args, i.callID})
}

// call is the state of one API request while its methods run.
type call struct {
	ctx     context.Context
	h       *Handler
	user    User
	using   map[string]bool
	created map[string]string // creation id -> id, from the request and from /set calls so far
	done    []invocation
	// resolved is how many bytes result references have copied into the
	// arguments of this request's calls so far (resolveReferences).
	resolved int64
	// implied collects the responses of calls a method made itself, such
	// as the Email/set of EmailSubmission/set's onSuccessUpdateEmail. They
	// follow the method's own response.
	implied []invocation
}

type method struct {
	capability string
	run        func(c *call, args json.RawMessage) (any, *methodError)
}

var methods map[string]method

func init() {
	methods = map[string]method{
		"Core/echo":            {CapCore, coreEcho},
		"Mailbox/get":          {CapMail, mailboxGet},
		"Mailbox/changes":      {CapMail, mailboxChanges},
		"Mailbox/query":        {CapMail, mailboxQuery},
		"Mailbox/queryChanges": {CapMail, mailboxQueryChanges},
		"Mailbox/set":          {CapMail, mailboxSet},
		"Thread/get":           {CapMail, threadGet},
		"Thread/changes":       {CapMail, threadChanges},
		"Email/get":            {CapMail, emailGet},
		"Email/changes":        {CapMail, emailChanges},
		"Email/query":          {CapMail, emailQuery},
		"Email/queryChanges":   {CapMail, emailQueryChanges},
		"Email/set":            {CapMail, emailSet},
		"Email/import":         {CapMail, emailImport},
		"Email/copy":           {CapMail, emailCopy},
		"Email/parse":          {CapMail, emailParse},
		"SearchSnippet/get":    {CapMail, searchSnippetGet},

		"Identity/get":                 {CapSubmission, identityGet},
		"Identity/changes":             {CapSubmission, identityChanges},
		"Identity/set":                 {CapSubmission, identitySet},
		"EmailSubmission/get":          {CapSubmission, submissionGet},
		"EmailSubmission/changes":      {CapSubmission, submissionChanges},
		"EmailSubmission/query":        {CapSubmission, submissionQuery},
		"EmailSubmission/queryChanges": {CapSubmission, submissionQueryChanges},
		"EmailSubmission/set":          {CapSubmission, submissionSet},

		"Quota/get":          {CapQuota, quotaGet},
		"Quota/changes":      {CapQuota, quotaChanges},
		"Quota/query":        {CapQuota, quotaQuery},
		"Quota/queryChanges": {CapQuota, quotaQueryChanges},

		"VacationResponse/get": {CapVacation, vacationGet},
		"VacationResponse/set": {CapVacation, vacationSet},

		"SieveScript/get":      {CapSieve, sieveScriptGet},
		"SieveScript/set":      {CapSieve, sieveScriptSet},
		"SieveScript/query":    {CapSieve, sieveScriptQuery},
		"SieveScript/validate": {CapSieve, sieveScriptValidate},

		"Calendar/get":                {CapCalendars, calendarGet},
		"Calendar/changes":            {CapCalendars, calendarChanges},
		"Calendar/set":                {CapCalendars, calendarSet},
		"CalendarEvent/get":           {CapCalendars, calendarEventGet},
		"CalendarEvent/changes":       {CapCalendars, calendarEventChanges},
		"CalendarEvent/set":           {CapCalendars, calendarEventSet},
		"CalendarEvent/query":         {CapCalendars, calendarEventQuery},
		"CalendarEvent/queryChanges":  {CapCalendars, queryChangesUnsupported(store.KindCalendar)},
		"CalendarEvent/parse":         {CapCalendarsParse, calendarEventParse},
		"ParticipantIdentity/get":     {CapCalendars, participantIdentityGet},
		"ParticipantIdentity/changes": {CapCalendars, participantIdentityChanges},
		"ParticipantIdentity/set":     {CapCalendars, participantIdentitySet},
		"AddressBook/get":             {CapContacts, addressBookGet},
		"AddressBook/changes":         {CapContacts, addressBookChanges},
		"AddressBook/set":             {CapContacts, addressBookSet},
		"ContactCard/get":             {CapContacts, contactCardGet},
		"ContactCard/changes":         {CapContacts, contactCardChanges},
		"ContactCard/set":             {CapContacts, contactCardSet},
		"ContactCard/query":           {CapContacts, contactCardQuery},
		"ContactCard/queryChanges":    {CapContacts, queryChangesUnsupported(store.KindAddressBook)},
		"Principal/get":               {CapPrincipals, principalGet},
		"Principal/changes":           {CapPrincipals, principalChanges},
		"Principal/query":             {CapPrincipals, principalQuery},
		"Principal/queryChanges":      {CapPrincipals, queryChangesUnsupported("")},
		"Principal/getAvailability":   {CapPrincipalsAvail, principalGetAvailability},
	}
}

// Methods lists the method names the server has, sorted.
func Methods() []string {
	out := make([]string, 0, len(methods))
	for name := range methods {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// coreEcho returns its arguments (RFC 8620 4).
func coreEcho(c *call, args json.RawMessage) (any, *methodError) {
	return args, nil
}

// acquire takes one of the account's maxConcurrentRequests slots.
func (h *Handler) acquire(account string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inFlight[account] >= h.limits.MaxConcurrentRequests {
		return false
	}
	h.inFlight[account]++
	return true
}

func (h *Handler) release(account string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inFlight[account]--; h.inFlight[account] <= 0 {
		delete(h.inFlight, account)
	}
	// Whoever waits for a slot looks again.
	close(h.slotFreed)
	h.slotFreed = make(chan struct{})
}

// maxReferences is how many result references one method call may hold.
// No method has that many arguments.
const maxReferences = 32

// maxWaitingRequests is how many requests of one account may wait for a
// slot; the next is refused at once.
const maxWaitingRequests = 32

// waitSlot takes one of the account's slots, and when all are taken waits
// for one, for at most QueueWait. Only then is the request refused with
// the limit error of RFC 8620 3.6.1.
//
// Why it waits: push tells every tab and client of an account about a
// change in the same moment, and each then asks for the changes. Three
// tabs are six requests at once; refused, each would have to try again,
// and the user's own click in between would be refused with them. A
// request that waits a few milliseconds is answered instead.
func (h *Handler) waitSlot(ctx context.Context, account string) bool {
	wait := h.QueueWait
	if wait == 0 {
		wait = DefaultQueueWait
	}
	var deadline <-chan time.Time
	counted := false
	defer func() {
		if counted {
			h.mu.Lock()
			if h.waiting[account]--; h.waiting[account] <= 0 {
				delete(h.waiting, account)
			}
			h.mu.Unlock()
		}
	}()
	for {
		h.mu.Lock()
		if h.inFlight[account] < h.limits.MaxConcurrentRequests {
			h.inFlight[account]++
			h.mu.Unlock()
			return true
		}
		if wait < 0 || (!counted && h.waiting[account] >= maxWaitingRequests) {
			h.mu.Unlock()
			return false
		}
		if !counted {
			counted = true
			h.waiting[account]++
			t := time.NewTimer(wait)
			defer t.Stop()
			deadline = t.C
		}
		freed := h.slotFreed
		h.mu.Unlock()
		select {
		case <-freed:
		case <-deadline:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// serveAPI is the API endpoint (RFC 8620 3).
func (h *Handler) serveAPI(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		problem(w, http.StatusBadRequest, errNotJSON, "the content type of the request must be application/json", nil)
		return
	}
	if !h.waitSlot(r.Context(), u.AccountID) {
		problem(w, http.StatusBadRequest, errLimit, "too many requests at once for this account", map[string]any{"limit": "maxConcurrentRequests"})
		return
	}
	defer h.release(u.AccountID)

	body, err := io.ReadAll(io.LimitReader(r.Body, h.limits.MaxSizeRequest+1))
	if err != nil {
		problem(w, http.StatusBadRequest, errNotJSON, "the request could not be read", nil)
		return
	}
	if int64(len(body)) > h.limits.MaxSizeRequest {
		problem(w, http.StatusBadRequest, errLimit, "the request is larger than maxSizeRequest", map[string]any{"limit": "maxSizeRequest"})
		return
	}
	// I-JSON (RFC 7493): UTF-8, and one value.
	if !utf8.Valid(body) || !json.Valid(body) {
		problem(w, http.StatusBadRequest, errNotJSON, "the request is not valid I-JSON", nil)
		return
	}
	var req struct {
		Using       *[]string          `json:"using"`
		MethodCalls *[]json.RawMessage `json:"methodCalls"`
		CreatedIDs  map[string]string  `json:"createdIds"`
	}
	// Member names are case-sensitive; encoding/json alone would take
	// "methodCAlls" for "methodCalls".
	var members map[string]json.RawMessage
	if json.Unmarshal(body, &members) == nil {
		for _, name := range []string{"using", "methodCalls"} {
			if _, ok := members[name]; !ok {
				members = nil
				break
			}
		}
	}
	if err := json.Unmarshal(body, &req); err != nil || members == nil || req.Using == nil || req.MethodCalls == nil {
		problem(w, http.StatusBadRequest, errNotRequest, "the request is not a JMAP Request object", nil)
		return
	}
	type parsed struct {
		name   string
		args   map[string]json.RawMessage
		callID string
	}
	calls := make([]parsed, 0, len(*req.MethodCalls))
	for _, raw := range *req.MethodCalls {
		var parts []json.RawMessage
		var p parsed
		if json.Unmarshal(raw, &parts) != nil || len(parts) != 3 ||
			json.Unmarshal(parts[0], &p.name) != nil || !isString(parts[0]) ||
			json.Unmarshal(parts[1], &p.args) != nil || p.args == nil ||
			json.Unmarshal(parts[2], &p.callID) != nil || !isString(parts[2]) {
			problem(w, http.StatusBadRequest, errNotRequest, "a method call must be [name, arguments, call id]", nil)
			return
		}
		calls = append(calls, p)
	}
	using := map[string]bool{}
	for _, capability := range *req.Using {
		if !capabilities[capability] {
			problem(w, http.StatusBadRequest, errUnknownCapability, "the server does not have the capability "+strconv.Quote(capability), nil)
			return
		}
		using[capability] = true
	}
	if len(calls) > h.limits.MaxCallsInRequest {
		problem(w, http.StatusBadRequest, errLimit, "more method calls than maxCallsInRequest", map[string]any{"limit": "maxCallsInRequest"})
		return
	}

	c := &call{ctx: r.Context(), h: h, user: u, using: using, created: map[string]string{}, done: []invocation{}}
	for k, v := range req.CreatedIDs {
		c.created[k] = v
	}
	for _, p := range calls {
		c.implied = nil
		inv := c.invoke(p.name, p.args, p.callID)
		c.done = append(c.done, inv)
		for _, extra := range c.implied {
			extra.callID = p.callID
			c.done = append(c.done, extra)
		}
	}
	resp := map[string]any{"methodResponses": c.done, "sessionState": h.sessionState(r.Context(), u)}
	if req.CreatedIDs != nil {
		resp["createdIds"] = c.created
	}
	writeJSON(w, http.StatusOK, resp)
}

func isString(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '"'
}

// invoke runs one method call and returns its response.
func (c *call) invoke(name string, args map[string]json.RawMessage, callID string) invocation {
	fail := func(e *methodError) invocation {
		body := map[string]any{"type": e.Type}
		if e.Description != "" {
			body["description"] = e.Description
		}
		for k, v := range e.Extra {
			body[k] = v
		}
		raw, _ := json.Marshal(body)
		return invocation{"error", raw, callID}
	}
	m, ok := methods[name]
	// A method of a capability the request does not use does not exist for
	// this request (RFC 8620 3.3).
	if !ok || !c.using[m.capability] {
		return fail(mErr("unknownMethod", "the server does not have the method %s, or the request does not use its capability", strconv.Quote(name)))
	}
	if e := c.resolveReferences(args); e != nil {
		return fail(e)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return fail(mErr("serverFail", "the server could not complete the request"))
	}
	if err := c.ctx.Err(); err != nil {
		return fail(mErr("serverFail", "the request was cancelled"))
	}
	result, e := m.run(c, raw)
	if e != nil {
		return fail(e)
	}
	out, err := json.Marshal(result)
	if err != nil {
		return fail(mErr("serverFail", "the server could not complete the request"))
	}
	return invocation{name, out, callID}
}

// resolveReferences replaces every "#name" argument by the value its
// ResultReference points at in an earlier response (RFC 8620 3.7).
//
// A reference is a few bytes that stand for a value of any size, and a
// call may hold many of them, to the same value. What they copy is
// therefore counted against maxSizeRequest for the request as a whole, as
// if the client had sent the values itself, and one call holds at most
// maxReferences of them. A response is parsed once per call, however many
// references point into it.
func (c *call) resolveReferences(args map[string]json.RawMessage) *methodError {
	refs := 0
	parsed := map[*invocation]any{}
	for key, raw := range args {
		if !strings.HasPrefix(key, "#") {
			continue
		}
		if refs++; refs > maxReferences {
			return mErr("invalidResultReference", "more than %d result references in one method call", maxReferences)
		}
		name := key[1:]
		if _, both := args[name]; both {
			return invalidArgs("both %q and %q are given", name, key)
		}
		var ref struct {
			ResultOf *string `json:"resultOf"`
			Name     *string `json:"name"`
			Path     *string `json:"path"`
		}
		if err := decodeStrict(raw, &ref); err != nil || ref.ResultOf == nil || ref.Name == nil || ref.Path == nil {
			return mErr("invalidResultReference", "%q is not a ResultReference", key)
		}
		var source *invocation
		for i := range c.done {
			if c.done[i].callID == *ref.ResultOf {
				source = &c.done[i]
				break
			}
		}
		if source == nil {
			return mErr("invalidResultReference", "no earlier method call has the id %q", *ref.ResultOf)
		}
		if source.name != *ref.Name {
			return mErr("invalidResultReference", "the response to call %q is %q, not %q", *ref.ResultOf, source.name, *ref.Name)
		}
		root, have := parsed[source]
		if !have {
			if err := json.Unmarshal(source.args, &root); err != nil {
				return mErr("serverFail", "the server could not complete the request")
			}
			parsed[source] = root
		}
		budget := maxPathItems
		value, ok := evalPointer(root, *ref.Path, &budget)
		if !ok {
			return mErr("invalidResultReference", "the path %q does not resolve in the response to call %q", *ref.Path, *ref.ResultOf)
		}
		resolved, err := json.Marshal(value)
		if err != nil {
			return mErr("serverFail", "the server could not complete the request")
		}
		if c.resolved += int64(len(resolved)); c.resolved > c.h.limits.MaxSizeRequest {
			return mErr("invalidResultReference", "the result references of this request stand for more than maxSizeRequest octets")
		}
		delete(args, key)
		args[name] = resolved
	}
	return nil
}

// evalPointer applies a JSON Pointer (RFC 6901) to a value, with the "*"
// of RFC 8620 3.7: on an array it applies the rest of the pointer to every
// item and collects the results, flattening results that are arrays.
func evalPointer(v any, path string, budget *int) (any, bool) {
	if path == "" {
		return v, true
	}
	if path[0] != '/' {
		return nil, false
	}
	tokens := strings.Split(path[1:], "/")
	for i, t := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	return evalTokens(v, tokens, budget)
}

func evalTokens(v any, tokens []string, budget *int) (any, bool) {
	if len(tokens) == 0 {
		return v, true
	}
	t, rest := tokens[0], tokens[1:]
	switch x := v.(type) {
	case map[string]any:
		child, ok := x[t]
		if !ok {
			return nil, false
		}
		return evalTokens(child, rest, budget)
	case []any:
		if t == "*" {
			out := []any{}
			for _, item := range x {
				r, ok := evalTokens(item, rest, budget)
				if !ok {
					return nil, false
				}
				if list, isList := r.([]any); isList {
					out = append(out, list...)
				} else {
					out = append(out, r)
				}
				if *budget -= 1; *budget < 0 || len(out) > maxPathItems {
					return nil, false
				}
			}
			return out, true
		}
		// "0", or digits without a leading zero (RFC 6901 4).
		if t == "" || len(t) > 9 || (len(t) > 1 && t[0] == '0') {
			return nil, false
		}
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 || n >= len(x) {
			return nil, false
		}
		return evalTokens(x[n], rest, budget)
	}
	return nil, false
}

// decodeStrict decodes one JSON value and refuses members the target does
// not have: an unknown argument is an error, not something to ignore.
func decodeStrict(raw json.RawMessage, v any) error {
	// encoding/json matches member names without regard to case. JMAP's
	// names are case-sensitive, so "AccountId" is an unknown argument.
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) == nil && members != nil {
		known := map[string]bool{}
		memberNames(reflect.TypeOf(v), known)
		for name := range members {
			if !known[name] {
				return fmt.Errorf("json: unknown field %q", name)
			}
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one value")
	}
	return nil
}

// memberNames collects the JSON member names of a struct type, those of
// embedded structs included.
func memberNames(t reflect.Type, into map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case f.Anonymous && name == "":
			memberNames(f.Type, into)
		case name != "" && name != "-":
			into[name] = true
		}
	}
}

// decodeArgs decodes the arguments of a method and checks the account.
// account points at the accountId field of the target.
func (c *call) decodeArgs(raw json.RawMessage, v any, account *string) *methodError {
	if err := decodeStrict(raw, v); err != nil {
		return invalidArgs("%s", argError(err))
	}
	if *account == "" {
		return invalidArgs("accountId is required")
	}
	if *account != c.user.AccountID {
		return mErr("accountNotFound", "no such account")
	}
	return nil
}

// argError makes a decoding error fit for a client: what is wrong, without
// Go type names.
func argError(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		if typeErr.Field != "" {
			return "the argument " + strconv.Quote(typeErr.Field) + " has the wrong type"
		}
		return "the arguments have the wrong type"
	}
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, "json: unknown field "); ok {
		return "unknown argument " + rest
	}
	return "the arguments are not valid"
}

// window applies position, anchor, anchorOffset and limit of a /query to
// the full list of ids (RFC 8620 5.5) and returns the position of the
// first id returned.
func window(ids []string, position int64, anchor *string, anchorOffset int64, limit int64) (int64, []string, *methodError) {
	n := int64(len(ids))
	start := position
	if anchor != nil {
		at := int64(-1)
		for i, id := range ids {
			if id == *anchor {
				at = int64(i)
				break
			}
		}
		if at < 0 {
			return 0, nil, mErr("anchorNotFound", "the anchor is not in the results")
		}
		start = at + anchorOffset
	} else if start < 0 {
		start += n
	}
	start = max(start, 0)
	if start >= n {
		return start, []string{}, nil
	}
	end := min(start+limit, n)
	return start, ids[start:end], nil
}

// queryArgs are the arguments every /query shares.
type queryArgs struct {
	Position       int64   `json:"position"`
	Anchor         *string `json:"anchor"`
	AnchorOffset   int64   `json:"anchorOffset"`
	Limit          *int64  `json:"limit"`
	CalculateTotal bool    `json:"calculateTotal"`
}

// limit returns the effective limit and whether it differs from what the
// client asked for, in which case the response says so.
func (q queryArgs) limit() (limit int64, clamped bool, e *methodError) {
	if q.Limit == nil {
		return maxQueryLimit, false, nil
	}
	if *q.Limit < 0 {
		return 0, false, invalidArgs("limit is negative")
	}
	if *q.Limit > maxQueryLimit {
		return maxQueryLimit, true, nil
	}
	return *q.Limit, false, nil
}
