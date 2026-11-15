package jmap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/address"
	"github.com/robinmail/robinmail/internal/blob"
	"github.com/robinmail/robinmail/internal/raftdb/raftdbtest"
	"github.com/robinmail/robinmail/internal/store"
)

// Tests of the mail methods, written from RFC 8621 and using its examples.
// They run against a real store, on a database of its own.
//
// Every test works in an account of its own. A new account has the
// mailboxes m1 Inbox, m2 Drafts, m3 Sent, m4 Junk and m5 Trash.

type env struct {
	t     *testing.T
	st    *store.Store
	h     *Handler
	acct  store.Account
	user  User
	ctx   context.Context
	local *blob.Local
	// refuse is what send answers instead of queueing.
	refuse *SendError
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := raftdbtest.Open(t)
	master, err := blob.NewMasterKey(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	local := blob.NewLocal(t.TempDir())
	st := store.New(db, local, master)
	ctx := context.Background()
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDomain(ctx, "example.test"); err != nil && !errors.Is(err, store.ErrDomainExists) {
		t.Fatal(err)
	}
	var r [8]byte
	rand.Read(r[:])
	acct, err := st.CreateAccount(ctx, "jmap-"+hex.EncodeToString(r[:])+"@example.test", "$argon2id$test")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, h: &Handler{Store: st, Hostname: "mail.example.test"}, acct: acct, ctx: ctx, local: local,
		user: User{AccountID: acct.ID, Username: acct.Address}}
	e.h.Send = e.send
	// The database outlives the test; its queue should not.
	t.Cleanup(func() { st.PurgeSubmissions(ctx, acct.ID) })
	return e
}

// send stands in for the Submitter that cmd/robinmail wires up: it checks
// the envelope sender and queues, with an hour in which to cancel. refuse
// makes it refuse instead.
func (e *env) send(ctx context.Context, u User, o Outgoing) (store.Submission, *SendError, error) {
	if e.refuse != nil {
		return store.Submission{}, e.refuse, nil
	}
	if o.MailFrom != "" && !address.Owns(u.Username, o.MailFrom) {
		return store.Submission{}, &SendError{Type: "forbiddenMailFrom", Description: "not your address"}, nil
	}
	sub, err := e.st.Submit(ctx, u.AccountID, store.NewSubmission{EmailID: o.EmailID, IdentityID: o.IdentityID, Raw: o.Raw,
		MailFrom: o.MailFrom, Rcpts: o.RcptTo, SendAt: e.st.Now().Add(time.Hour), IfInState: o.IfInState})
	return sub, nil, err
}

// do runs method calls and returns methodResponses. "$A" in the calls is
// the account id.
func (e *env) do(calls string) []any {
	e.t.Helper()
	return e.doWith(`"methodCalls":[` + calls + `]`)
}

func (e *env) doWith(members string) []any {
	e.t.Helper()
	body := `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],` + strings.ReplaceAll(members, "$A", e.acct.ID) + `}`
	req := httptest.NewRequest("POST", "https://mail.example.test/jmap/api", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(WithUser(req.Context(), e.user))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		e.t.Fatalf("status %d: %s\nrequest: %s", rec.Code, rec.Body.String(), body)
	}
	return at(e.t, decode(e.t, rec.Body.Bytes()), "/methodResponses").([]any)
}

// one runs a single call that must succeed and returns its arguments.
func (e *env) one(name, args string) map[string]any {
	e.t.Helper()
	got := e.do(fmt.Sprintf(`[%q, %s, "c"]`, name, args))
	if len(got) != 1 || at(e.t, got, "/0/0") != name {
		b, _ := json.Marshal(got)
		e.t.Fatalf("%s %s answered %s", name, args, b)
	}
	return at(e.t, got, "/0/1").(map[string]any)
}

// fail runs a single call that must fail and returns the error type.
func (e *env) fail(name, args string) string {
	e.t.Helper()
	got := e.do(fmt.Sprintf(`[%q, %s, "c"]`, name, args))
	if at(e.t, got, "/0/0") != "error" {
		b, _ := json.Marshal(got)
		e.t.Fatalf("%s %s did not fail: %s", name, args, b)
	}
	return at(e.t, got, "/0/1/type").(string)
}

func (e *env) deliver(m store.Message) store.Email {
	e.t.Helper()
	em, err := e.st.Deliver(e.ctx, e.acct.ID, m)
	if err != nil {
		e.t.Fatal(err)
	}
	return em
}

func (e *env) state() string {
	e.t.Helper()
	n, err := e.st.State(e.ctx, e.acct.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return stateString(n)
}

// get fetches a path of the handler as the user.
func (e *env) get(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "https://mail.example.test"+path, nil)
	req = req.WithContext(WithUser(req.Context(), e.user))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func eid(e store.Email) string { return store.EmailID(e.ID) }
func tid(e store.Email) string { return store.ThreadID(e.ThreadID) }

// simple builds a plain message.
func simple(id, from, subject string, extra ...string) []byte {
	return []byte("From: " + from + "\r\nTo: me@example.test\r\nSubject: " + subject + "\r\nMessage-ID: <" + id + ">\r\n" +
		strings.Join(extra, "") + "\r\nBody of " + subject + ".\r\n")
}

// RFC 8621 2: the Mailbox object, and 2.1 Mailbox/get with RFC 8620 5.1:
// ids null returns all, properties limits what comes back but always
// includes the id, an unknown id is listed in notFound.
func TestRFC8621MailboxGet(t *testing.T) {
	e := newEnv(t)
	seen := e.deliver(store.Message{Raw: simple("g1@x", "a@x.test", "one"), Keywords: []string{"$seen"}})
	e.deliver(store.Message{Raw: simple("g2@x", "a@x.test", "two", "References: <g1@x>\r\n")})
	e.deliver(store.Message{Raw: simple("g3@x", "a@x.test", "three")})
	_ = seen

	got := e.one("Mailbox/get", `{"accountId": "$A", "ids": null}`)
	same(t, "accountId and state", []any{got["accountId"], got["state"], got["notFound"]}, fmt.Sprintf(`[%q, %q, []]`, e.acct.ID, e.state()))
	if n := len(got["list"].([]any)); n != 5 {
		t.Fatalf("%d mailboxes", n)
	}
	// Three emails in two threads; one email is read, but its thread has an
	// unread email, so both threads are unread (RFC 8621 2, unreadThreads).
	same(t, "inbox", at(t, got, "/list/0"), `{
		"id": "m1", "name": "Inbox", "parentId": null, "role": "inbox", "sortOrder": 1,
		"totalEmails": 3, "unreadEmails": 2, "totalThreads": 2, "unreadThreads": 2,
		"myRights": {"mayReadItems": true, "mayAddItems": true, "mayRemoveItems": true, "maySetSeen": true,
			"maySetKeywords": true, "mayCreateChild": true, "mayRename": true, "mayDelete": false, "maySubmit": true},
		"isSubscribed": true}`)
	roles := at(t, got, "/list/*/role")
	same(t, "roles", roles, `["inbox", "drafts", "sent", "junk", "trash"]`)

	got = e.one("Mailbox/get", `{"accountId": "$A", "ids": ["m4", "m99", "nonsense", "m4"], "properties": ["name", "unreadEmails"]}`)
	same(t, "by id", got["list"], `[{"id": "m4", "name": "Junk", "unreadEmails": 0}]`)
	same(t, "notFound", got["notFound"], `["m99", "nonsense"]`)

	// RFC 8620 5.1 and 3.6.2.
	for args, want := range map[string]string{
		`{"accountId": "$A", "properties": ["colour"]}`: "invalidArguments",
		`{"accountId": "$A", "idz": null}`:              "invalidArguments",
		`{"AccountId": "$A", "ids": null}`:              "invalidArguments",
		`{"accountId": "$A", "IDS": null}`:              "invalidArguments",
		`{"accountId": "$A", "ids": "m1"}`:              "invalidArguments",
		`{"ids": null}`:                                 "invalidArguments",
		`{"accountId": "someone-else", "ids": null}`:    "accountNotFound",
	} {
		if got := e.fail("Mailbox/get", args); got != want {
			t.Errorf("Mailbox/get %s: %s, want %s", args, got, want)
		}
	}
	// "requestTooLarge: The number of ids requested by the client exceeds
	// the maximum number the server is willing to process".
	many := `["m1"` + strings.Repeat(`,"m1"`, 500) + `]`
	if got := e.fail("Mailbox/get", `{"accountId": "$A", "ids": `+many+`}`); got != "requestTooLarge" {
		t.Errorf("501 ids: %s", got)
	}
}

// RFC 8621 2.5: Mailbox/set, with RFC 8620 5.3 for creation ids, SetErrors
// and ifInState.
func TestRFC8621MailboxSet(t *testing.T) {
	e := newEnv(t)
	old := e.state()

	// Create a mailbox and, in the same call, a child that names its
	// parent by creation id (RFC 8620 5.3: "#" and the creation id).
	got := e.one("Mailbox/set", `{"accountId": "$A", "ifInState": "`+old+`", "create": {
		"child": {"name": "Invoices", "parentId": "#work"},
		"work": {"name": "Work", "parentId": null, "sortOrder": 20, "isSubscribed": false}}}`)
	same(t, "oldState", got["oldState"], `"`+old+`"`)
	if got["newState"] == old || got["newState"] != e.state() {
		t.Errorf("newState %v, state %s", got["newState"], e.state())
	}
	work, child := at(t, got, "/created/work/id").(string), at(t, got, "/created/child/id").(string)
	// "created: ... the id and any properties set by the server".
	same(t, "created work", at(t, got, "/created/work"), `{"id": "`+work+`", "parentId": null, "role": null, "sortOrder": 20,
		"isSubscribed": false, "totalEmails": 0, "unreadEmails": 0, "totalThreads": 0, "unreadThreads": 0,
		"myRights": {"mayReadItems": true, "mayAddItems": true, "mayRemoveItems": true, "maySetSeen": true,
			"maySetKeywords": true, "mayCreateChild": true, "mayRename": true, "mayDelete": true, "maySubmit": true}}`)
	same(t, "child's parent", at(t, got, "/created/child/parentId"), `"`+work+`"`)
	same(t, "nothing else", []any{got["updated"], got["destroyed"], got["notCreated"], got["notUpdated"], got["notDestroyed"]},
		`[null, null, null, null, null]`)

	// Refused creates, each with its SetError.
	got = e.one("Mailbox/set", `{"accountId": "$A", "create": {
		"dup": {"name": "Work"},
		"noname": {"parentId": null},
		"badname": {"name": " padded "},
		"role": {"name": "Archive", "role": "archive"},
		"orphan": {"name": "X", "parentId": "m99"},
		"badref": {"name": "X", "parentId": "#nothing"},
		"serverset": {"name": "X", "totalEmails": 5, "id": "m7"},
		"notobject": "Work"}}`)
	for cid, want := range map[string]string{
		"dup": `["name"]`, "noname": `["name"]`, "badname": `["name"]`, "role": `["role"]`, "orphan": `["parentId"]`,
		"badref": `["parentId"]`, "serverset": `["id", "totalEmails"]`,
	} {
		same(t, cid, []any{at(t, got, "/notCreated/"+cid+"/type"), at(t, got, "/notCreated/"+cid+"/properties")},
			`["invalidProperties", `+want+`]`)
	}
	same(t, "not an object", at(t, got, "/notCreated/notobject/type"), `"invalidProperties"`)
	same(t, "created nothing", got["created"], `null`)
	same(t, "state unchanged", got["newState"], `"`+got["oldState"].(string)+`"`)

	// Update: a PatchObject (RFC 8620 5.3). Rename, move, reorder.
	got = e.one("Mailbox/set", `{"accountId": "$A", "update": {
		"`+child+`": {"name": "Bills", "parentId": null, "sortOrder": 3, "isSubscribed": false},
		"`+work+`": {"parentId": "`+work+`"},
		"m1": {"totalEmails": 9},
		"m99": {"name": "x"},
		"m2": {"name": "Sent"}}}`)
	same(t, "updated", got["updated"], `{"`+child+`": null}`)
	same(t, "own parent", at(t, got, "/notUpdated/"+work+"/properties"), `["parentId"]`)
	same(t, "server-set property", at(t, got, "/notUpdated/m1/properties"), `["totalEmails"]`)
	same(t, "unknown", at(t, got, "/notUpdated/m99/type"), `"notFound"`)
	same(t, "name taken", at(t, got, "/notUpdated/m2/properties"), `["name"]`)
	box := e.one("Mailbox/get", `{"accountId": "$A", "ids": ["`+child+`"], "properties": ["name", "parentId", "sortOrder", "isSubscribed"]}`)
	same(t, "after update", at(t, box, "/list/0"), `{"id": "`+child+`", "name": "Bills", "parentId": null, "sortOrder": 3, "isSubscribed": false}`)

	// Destroy (RFC 8621 2.5): mailboxHasChild, mailboxHasEmail, and
	// onDestroyRemoveEmails.
	e.one("Mailbox/set", `{"accountId": "$A", "update": {"`+child+`": {"parentId": "`+work+`"}}}`)
	only := e.deliver(store.Message{MailboxID: mustMailbox(t, child), Raw: simple("s1@x", "a@x.test", "only here")})
	both := e.deliver(store.Message{MailboxID: mustMailbox(t, child), Raw: simple("s2@x", "a@x.test", "also in the inbox")})
	e.one("Email/set", `{"accountId": "$A", "update": {"`+eid(both)+`": {"mailboxIds/m1": true}}}`)
	got = e.one("Mailbox/set", `{"accountId": "$A", "destroy": ["`+work+`", "m1", "m99", "junk"]}`)
	same(t, "has child", at(t, got, "/notDestroyed/"+work+"/type"), `"mailboxHasChild"`)
	same(t, "role", at(t, got, "/notDestroyed/m1/type"), `"forbidden"`)
	same(t, "unknown", []any{at(t, got, "/notDestroyed/m99/type"), at(t, got, "/notDestroyed/junk/type")}, `["notFound", "notFound"]`)
	got = e.one("Mailbox/set", `{"accountId": "$A", "destroy": ["`+child+`"]}`)
	same(t, "has email", at(t, got, "/notDestroyed/"+child+"/type"), `"mailboxHasEmail"`)
	// Parent listed before its child: both go, in one call.
	got = e.one("Mailbox/set", `{"accountId": "$A", "destroy": ["`+work+`", "`+child+`"], "onDestroyRemoveEmails": true}`)
	same(t, "destroyed", got["destroyed"], `["`+child+`", "`+work+`"]`)
	same(t, "none refused", got["notDestroyed"], `null`)
	// "Any Emails that were in the Mailbox ... are destroyed if they are
	// in no other Mailbox", and stay otherwise.
	emails := e.one("Email/get", `{"accountId": "$A", "ids": ["`+eid(only)+`", "`+eid(both)+`"], "properties": ["mailboxIds"]}`)
	same(t, "emails after destroy", []any{emails["list"], emails["notFound"]},
		`[[{"id": "`+eid(both)+`", "mailboxIds": {"m1": true}}], ["`+eid(only)+`"]]`)

	// RFC 8620 5.3: "ifInState ... if supplied, the string must match the
	// current state; otherwise, the method will be aborted and a
	// stateMismatch error returned."
	if got := e.fail("Mailbox/set", `{"accountId": "$A", "ifInState": "`+old+`", "create": {"x": {"name": "Never"}}}`); got != "stateMismatch" {
		t.Errorf("old ifInState: %s", got)
	}
	if got := e.fail("Mailbox/set", `{"accountId": "$A", "ifInState": "nonsense", "create": {"x": {"name": "Never"}}}`); got != "stateMismatch" {
		t.Errorf("nonsense ifInState: %s", got)
	}
	names := at(t, e.one("Mailbox/get", `{"accountId": "$A", "ids": null, "properties": ["name"]}`), "/list/*/name")
	same(t, "nothing was created", names, `["Inbox", "Drafts", "Sent", "Junk", "Trash"]`)

	// RFC 8621 1.3.1, maxMailboxDepth: a chain of ten is the limit.
	parent := "null"
	for depth := 1; depth <= MaxMailboxDepth+1; depth++ {
		got := e.one("Mailbox/set", fmt.Sprintf(`{"accountId": "$A", "create": {"d": {"name": "L%d", "parentId": %s}}}`, depth, parent))
		if depth <= MaxMailboxDepth {
			parent = `"` + at(t, got, "/created/d/id").(string) + `"`
			continue
		}
		same(t, "level 11", at(t, got, "/notCreated/d/properties"), `["parentId"]`)
	}
	// Moving a mailbox with a subtree counts the subtree.
	got = e.one("Mailbox/set", `{"accountId": "$A", "create": {"a": {"name": "A"}, "b": {"name": "B", "parentId": "#a"}}}`)
	a := at(t, got, "/created/a/id").(string)
	got = e.one("Mailbox/set", `{"accountId": "$A", "update": {"`+a+`": {"parentId": `+parent+`}}}`)
	same(t, "move makes it too deep", at(t, got, "/notUpdated/"+a+"/properties"), `["parentId"]`)

	// More objects than maxObjectsInSet.
	var many strings.Builder
	for i := 0; i < 501; i++ {
		fmt.Fprintf(&many, `%q,`, fmt.Sprintf("m%d", i+100))
	}
	if got := e.fail("Mailbox/set", `{"accountId": "$A", "destroy": [`+strings.TrimSuffix(many.String(), ",")+`]}`); got != "requestTooLarge" {
		t.Errorf("501 destroys: %s", got)
	}
}

func mustMailbox(t *testing.T, id string) int64 {
	t.Helper()
	n, ok := store.ParseMailboxID(id)
	if !ok {
		t.Fatalf("%q is not a mailbox id", id)
	}
	return n
}

// RFC 8620 3.3, createdIds: a creation id from one call can be used in a
// later call of the same request, and the response has the map of all.
func TestRFC8620CreatedIds(t *testing.T) {
	e := newEnv(t)
	em := e.deliver(store.Message{Raw: simple("c1@x", "a@x.test", "to label")})
	got := e.doWith(`"createdIds": {}, "methodCalls":[
		["Mailbox/set", {"accountId": "$A", "create": {"proj": {"name": "Project"}}}, "c1"],
		["Email/set", {"accountId": "$A", "update": {"` + eid(em) + `": {"mailboxIds/#proj": true}}}, "c2"],
		["Email/get", {"accountId": "$A", "ids": ["` + eid(em) + `"], "properties": ["mailboxIds"]}, "c3"]]`)
	proj := at(t, got, "/0/1/created/proj/id").(string)
	same(t, "labelled through the creation id", at(t, got, "/2/1/list/0/mailboxIds"), `{"m1": true, "`+proj+`": true}`)

	// A creation id the client says it made earlier.
	got = e.doWith(`"createdIds": {"old": "` + proj + `"}, "methodCalls":[
		["Email/set", {"accountId": "$A", "update": {"` + eid(em) + `": {"mailboxIds": {"#old": true}}}}, "c1"]]`)
	same(t, "updated", at(t, got, "/0/1/updated"), `{"`+eid(em)+`": null}`)
	box := e.one("Email/get", `{"accountId": "$A", "ids": ["`+eid(em)+`"], "properties": ["mailboxIds"]}`)
	same(t, "moved", at(t, box, "/list/0/mailboxIds"), `{"`+proj+`": true}`)
}

// RFC 8621 2.3 Mailbox/query and 2.2 Mailbox/changes.
func TestRFC8621MailboxQueryAndChanges(t *testing.T) {
	e := newEnv(t)
	start := e.state()
	got := e.one("Mailbox/set", `{"accountId": "$A", "create": {
		"z": {"name": "Zebra", "sortOrder": 10}, "a": {"name": "alpha", "sortOrder": 10},
		"k": {"name": "Kids", "parentId": "#a", "sortOrder": 1, "isSubscribed": false}}}`)
	z, a, k := at(t, got, "/created/z/id").(string), at(t, got, "/created/a/id").(string), at(t, got, "/created/k/id").(string)
	ids := func(args string) any { t.Helper(); return e.one("Mailbox/query", args)["ids"] }
	q := func(v string) string { return `"` + v + `"` }

	// No sort: a stable order (by id).
	same(t, "all", ids(`{"accountId": "$A"}`), `["m1","m2","m3","m4","m5",`+q(a)+`,`+q(k)+`,`+q(z)+`]`)
	same(t, "by name", ids(`{"accountId": "$A", "sort": [{"property": "name"}]}`),
		`[`+q(a)+`,"m2","m1","m4",`+q(k)+`,"m3","m5",`+q(z)+`]`)
	same(t, "by name descending", at(t, ids(`{"accountId": "$A", "sort": [{"property": "name", "isAscending": false}]}`), "/0"), q(z))
	same(t, "by sortOrder then name", ids(`{"accountId": "$A", "sort": [{"property": "sortOrder"}, {"property": "name"}]}`),
		`["m1",`+q(k)+`,"m2","m3","m4","m5",`+q(a)+`,`+q(z)+`]`)
	// sortAsTree: "the mailboxes are sorted as a tree ... a parent always
	// before its children", each level in the comparator's order.
	same(t, "as a tree", ids(`{"accountId": "$A", "sort": [{"property": "sortOrder"}, {"property": "name"}], "sortAsTree": true}`),
		`["m1","m2","m3","m4","m5",`+q(a)+`,`+q(k)+`,`+q(z)+`]`)

	same(t, "top level", ids(`{"accountId": "$A", "filter": {"parentId": null}}`), `["m1","m2","m3","m4","m5",`+q(a)+`,`+q(z)+`]`)
	same(t, "children", ids(`{"accountId": "$A", "filter": {"parentId": `+q(a)+`}}`), `[`+q(k)+`]`)
	same(t, "name", ids(`{"accountId": "$A", "filter": {"name": "RA"}}`), `["m2","m5",`+q(z)+`]`)
	same(t, "role", ids(`{"accountId": "$A", "filter": {"role": "junk"}}`), `["m4"]`)
	same(t, "no role", ids(`{"accountId": "$A", "filter": {"role": null}}`), `[`+q(a)+`,`+q(k)+`,`+q(z)+`]`)
	same(t, "hasAnyRole", ids(`{"accountId": "$A", "filter": {"hasAnyRole": true}}`), `["m1","m2","m3","m4","m5"]`)
	same(t, "isSubscribed", ids(`{"accountId": "$A", "filter": {"isSubscribed": false}}`), `[`+q(k)+`]`)
	same(t, "operators", ids(`{"accountId": "$A", "filter": {"operator": "AND", "conditions": [
		{"hasAnyRole": false}, {"operator": "NOT", "conditions": [{"name": "zebra"}]}]}}`), `[`+q(a)+`,`+q(k)+`]`)
	// filterAsTree: "a Mailbox is only included ... if all its ancestors
	// are also included". Kids matches, its parent alpha does not.
	same(t, "filter", ids(`{"accountId": "$A", "filter": {"name": "kids"}}`), `[`+q(k)+`]`)
	same(t, "filterAsTree", ids(`{"accountId": "$A", "filter": {"name": "kids"}, "filterAsTree": true}`), `[]`)

	// RFC 8620 5.5: the window, and the properties of the response.
	page := e.one("Mailbox/query", `{"accountId": "$A", "position": 2, "limit": 3, "calculateTotal": true}`)
	same(t, "window", page, `{"accountId": "`+e.acct.ID+`", "queryState": "`+e.state()+`", "canCalculateChanges": true,
		"position": 2, "ids": ["m3","m4","m5"], "total": 8}`)
	page = e.one("Mailbox/query", `{"accountId": "$A", "anchor": "m4", "anchorOffset": -1, "limit": 2}`)
	same(t, "anchor", []any{page["position"], page["ids"]}, `[2, ["m3","m4"]]`)
	// A limit above the server's maximum is lowered, and the response says so.
	page = e.one("Mailbox/query", `{"accountId": "$A", "limit": 100000}`)
	same(t, "clamped limit", page["limit"], `500`)

	for args, want := range map[string]string{
		`{"accountId": "$A", "filter": {"colour": "red"}}`:                      "unsupportedFilter",
		`{"accountId": "$A", "sort": [{"property": "totalEmails"}]}`:            "unsupportedSort",
		`{"accountId": "$A", "sort": [{"property": "name", "collation": "x"}]}`: "unsupportedSort",
		`{"accountId": "$A", "anchor": "m99"}`:                                  "anchorNotFound",
		`{"accountId": "$A", "limit": -1}`:                                      "invalidArguments",
		`{"accountId": "$A", "filter": {"operator": "XOR", "conditions": []}}`:  "invalidArguments",
		`{"accountId": "$A", "filter": {"operator": "AND"}}`:                    "invalidArguments",
		`{"accountId": "$A", "filter": {"name": 5}}`:                            "invalidArguments",
		`{"accountId": "$A", "filter": "inbox"}`:                                "invalidArguments",
	} {
		if got := e.fail("Mailbox/query", args); got != want {
			t.Errorf("Mailbox/query %s: %s, want %s", args, got, want)
		}
	}

	// Mailbox/changes since the start: three created. Then an email
	// arrives and a mailbox goes.
	ch := e.one("Mailbox/changes", `{"accountId": "$A", "sinceState": "`+start+`"}`)
	same(t, "changes", ch, `{"accountId": "`+e.acct.ID+`", "oldState": "`+start+`", "newState": "`+e.state()+`",
		"hasMoreChanges": false, "created": [`+q(a)+`,`+q(k)+`,`+q(z)+`], "updated": [], "destroyed": [], "updatedProperties": null}`)
	mid := e.state()
	e.deliver(store.Message{Raw: simple("mq1@x", "a@x.test", "new")})
	e.one("Mailbox/set", `{"accountId": "$A", "destroy": [`+q(z)+`]}`)
	ch = e.one("Mailbox/changes", `{"accountId": "$A", "sinceState": "`+mid+`"}`)
	same(t, "later changes", []any{ch["created"], ch["updated"], ch["destroyed"]}, `[[], ["m1"], [`+q(z)+`]]`)
	// Created and destroyed in between: not reported at all (RFC 8620 5.2).
	ch = e.one("Mailbox/changes", `{"accountId": "$A", "sinceState": "`+start+`"}`)
	same(t, "created and destroyed", []any{ch["created"], ch["updated"], ch["destroyed"]}, `[[`+q(a)+`,`+q(k)+`], ["m1"], []]`)
	// maxChanges: an intermediate state and hasMoreChanges.
	ch = e.one("Mailbox/changes", `{"accountId": "$A", "sinceState": "`+mid+`", "maxChanges": 1}`)
	if ch["hasMoreChanges"] != true || ch["newState"] == e.state() {
		t.Errorf("maxChanges 1: %v", ch)
	}
	ch = e.one("Mailbox/changes", `{"accountId": "$A", "sinceState": "`+ch["newState"].(string)+`", "maxChanges": 1}`)
	same(t, "second step", []any{ch["hasMoreChanges"], ch["newState"], ch["destroyed"]}, `[false, "`+e.state()+`", [`+q(z)+`]]`)

	for args, want := range map[string]string{
		`{"accountId": "$A"}`: "invalidArguments",
		`{"accountId": "$A", "sinceState": "1", "maxChanges": 0}`: "invalidArguments",
		`{"accountId": "$A", "sinceState": "999999999"}`:          "cannotCalculateChanges",
		`{"accountId": "$A", "sinceState": "abc"}`:                "cannotCalculateChanges",
	} {
		if got := e.fail("Mailbox/changes", args); got != want {
			t.Errorf("Mailbox/changes %s: %s, want %s", args, got, want)
		}
	}

	// Mailbox/queryChanges (RFC 8620 5.6): what was touched is removed and,
	// if still in the results, added again at its index.
	qc := e.one("Mailbox/queryChanges", `{"accountId": "$A", "sinceQueryState": "`+mid+`", "calculateTotal": true}`)
	same(t, "queryChanges", qc, `{"accountId": "`+e.acct.ID+`", "oldQueryState": "`+mid+`", "newQueryState": "`+e.state()+`",
		"removed": ["m1", `+q(z)+`], "added": [{"id": "m1", "index": 0}], "total": 7}`)
	qc = e.one("Mailbox/queryChanges", `{"accountId": "$A", "sinceQueryState": "`+e.state()+`"}`)
	same(t, "no changes", []any{qc["removed"], qc["added"]}, `[[], []]`)
	if got := e.fail("Mailbox/queryChanges", `{"accountId": "$A", "sinceQueryState": "`+mid+`", "maxChanges": 1}`); got != "tooManyChanges" {
		t.Errorf("maxChanges 1: %s", got)
	}
	if got := e.fail("Mailbox/queryChanges", `{"accountId": "$A", "sinceQueryState": "`+mid+`", "limit": 1}`); got != "invalidArguments" {
		t.Errorf("limit in queryChanges: %s", got)
	}
}

// RFC 8621 3.1.1: the Thread/get example. A thread lists its emails
// "sorted by the receivedAt date of the Email, oldest first".
func TestRFC8621ThreadGet(t *testing.T) {
	e := newEnv(t)
	at0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	// Delivered out of order: the reply is stored before the original
	// arrives, but was received later.
	reply := e.deliver(store.Message{ReceivedAt: at0.Add(2 * time.Hour), Raw: simple("t2@x", "b@x.test", "Re: plan", "References: <t1@x>\r\n")})
	first := e.deliver(store.Message{ReceivedAt: at0, Raw: simple("t1@x", "a@x.test", "plan")})
	other := e.deliver(store.Message{ReceivedAt: at0, Raw: simple("t3@x", "c@x.test", "else")})

	got := e.do(`["Thread/get", {"accountId": "$A", "ids": ["` + tid(reply) + `", "` + tid(other) + `", "t999"]}, "#1"]`)
	same(t, "Thread/get", got, `[["Thread/get", {
		"accountId": "`+e.acct.ID+`", "state": "`+e.state()+`",
		"list": [
			{"id": "`+tid(reply)+`", "emailIds": ["`+eid(first)+`", "`+eid(reply)+`"]},
			{"id": "`+tid(other)+`", "emailIds": ["`+eid(other)+`"]}],
		"notFound": ["t999"]}, "#1"]]`)

	if got := e.fail("Thread/get", `{"accountId": "$A", "ids": null}`); got != "requestTooLarge" {
		t.Errorf("ids null: %s", got)
	}
	// Thread/changes (3.2): destroying the last email of a thread destroys
	// the thread; another thread losing one of two emails is updated.
	before := e.state()
	e.one("Email/set", `{"accountId": "$A", "destroy": ["`+eid(other)+`", "`+eid(first)+`"]}`)
	ch := e.one("Thread/changes", `{"accountId": "$A", "sinceState": "`+before+`"}`)
	same(t, "Thread/changes", []any{ch["created"], ch["updated"], ch["destroyed"], ch["hasMoreChanges"]},
		`[[], ["`+tid(reply)+`"], ["`+tid(other)+`"], false]`)
	if _, has := ch["updatedProperties"]; has {
		t.Error("Thread/changes has updatedProperties, which is Mailbox/changes only")
	}
}

// dinner is a message shaped like the one in the example of RFC 8621
// 4.2.1: an HTML body, then a plain text footer a list added, both inline
// in a multipart/mixed. The line break in front of a boundary belongs to
// the boundary (RFC 2046 5.1.1), so the footer does not end in one.
func dinner() []byte {
	html := "<html><body><p>This is my proposal for dinner on Thursday. " + strings.Repeat("Bring wine and good company. ", 20) + "</p></body></html>"
	return []byte("From: Joe Bloggs <joe@example.com>\r\nTo: partytime@lists.example.com\r\nSubject: Dinner on Thursday?\r\n" +
		"Date: Sun, 13 Oct 2013 16:12:00 +0200\r\nMessage-ID: <dinner@example.com>\r\n" +
		"List-Post: <mailto:partytime@lists.example.com>\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=frontier\r\n\r\n" +
		"--frontier\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + html + "\r\n" +
		"--frontier\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n-- \r\nSent by the partytime list.\r\n" +
		"--frontier--\r\n")
}

// RFC 8621 4.2.1: the Email/get example, request and response. The ids and
// sizes are ours; everything else is as in the RFC.
func TestRFC8621EmailGetExample(t *testing.T) {
	e := newEnv(t)
	em := e.deliver(store.Message{Raw: dinner(), ReceivedAt: time.Date(2013, 10, 13, 14, 12, 0, 0, time.UTC)})
	got := e.do(`["Email/get", {
		"accountId": "$A",
		"ids": [ "e999", "` + eid(em) + `" ],
		"properties": [ "threadId", "mailboxIds", "from", "subject", "receivedAt", "header:List-POST:asURLs", "htmlBody", "bodyValues" ],
		"bodyProperties": [ "partId", "blobId", "size", "type" ],
		"fetchHTMLBodyValues": true,
		"maxBodyValueBytes": 256
	}, "#1" ]`)
	html := at(t, got, "/0/1/list/0/bodyValues/1/value").(string)
	if len(html) != 256 || !strings.HasPrefix(html, "<html><body><p>This is my proposal for dinner on Thursday.") {
		t.Errorf("truncated value of %d bytes: %q", len(html), html)
	}
	htmlSize := strings.Index(string(dinner()), "</html>") + len("</html>") - strings.Index(string(dinner()), "<html>")
	htmlJSON, _ := json.Marshal(html)
	same(t, "Email/get", got, `[[ "Email/get", {
		"accountId": "`+e.acct.ID+`",
		"state": "`+e.state()+`",
		"list": [{
			"id": "`+eid(em)+`",
			"threadId": "`+tid(em)+`",
			"mailboxIds": { "m1": true },
			"from": [{ "name": "Joe Bloggs", "email": "joe@example.com" }],
			"subject": "Dinner on Thursday?",
			"receivedAt": "2013-10-13T14:12:00Z",
			"header:List-POST:asURLs": [ "mailto:partytime@lists.example.com" ],
			"htmlBody": [
				{ "partId": "1", "blobId": "`+em.Blob+`-1", "size": `+fmt.Sprint(htmlSize)+`, "type": "text/html" },
				{ "partId": "2", "blobId": "`+em.Blob+`-2", "size": 32, "type": "text/plain" }],
			"bodyValues": {
				"1": { "isEncodingProblem": false, "isTruncated": true, "value": `+string(htmlJSON)+` },
				"2": { "isEncodingProblem": false, "isTruncated": false, "value": "-- \nSent by the partytime list." }}
		}],
		"notFound": [ "e999" ]
	}, "#1" ]]`)
}

// RFC 8621 4.1 and 4.2: the properties of an Email.
func TestRFC8621EmailProperties(t *testing.T) {
	e := newEnv(t)
	raw := []byte("Return-Path: <bounce@example.com>\r\n" +
		"From: =?UTF-8?Q?Ren=C3=A9e?= <renee@example.com>\r\nSender: Office <office@example.com>\r\n" +
		"To: Bob <bob@example.test>, Team: carol@example.test, dave@example.test;\r\nCc: erin@example.test\r\n" +
		"Reply-To: replies@example.com\r\nSubject: =?UTF-8?B?UmFwcG9ydCDigJMgUTM=?=\r\n" +
		"Date: Fri, 13 Nov 2026 10:00:00 +0100\r\nMessage-ID: <report@example.com>\r\n" +
		"In-Reply-To: <ask@example.test>\r\nReferences: <first@example.test> <ask@example.test>\r\n" +
		"X-Tag: one\r\nX-Tag: two\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Voil=E0 le rapport.\r\n" +
		"--inner\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Voil\xc3\xa0 le <b>rapport</b>.</p>\r\n" +
		"--inner--\r\n" +
		"--outer\r\nContent-Type: application/pdf; name=old.pdf\r\n" +
		"Content-Disposition: attachment; filename*=UTF-8''rapport%20Q3.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"JVBERi0xLjQK\r\n--outer--\r\n")
	em := e.deliver(store.Message{Raw: raw, Keywords: []string{"$seen", "$flagged"},
		ReceivedAt: time.Date(2026, 11, 13, 9, 0, 5, 0, time.UTC)})
	id := eid(em)

	// Without "properties": the default list of RFC 8621 4.2.
	got := at(t, e.one("Email/get", `{"accountId": "$A", "ids": ["`+id+`"]}`), "/list/0").(map[string]any)
	part := func(pid, typ string, size int, charset, name, disposition string) string {
		return fmt.Sprintf(`{"partId": %q, "blobId": %q, "size": %d, "name": %s, "type": %q, "charset": %s, "disposition": %s,
			"cid": null, "language": null, "location": null}`, pid, em.Blob+"-"+pid, size, name, typ, charset, disposition)
	}
	same(t, "default properties", got, `{
		"id": "`+id+`", "blobId": "`+em.Blob+`", "threadId": "`+tid(em)+`", "mailboxIds": {"m1": true},
		"keywords": {"$seen": true, "$flagged": true}, "size": `+fmt.Sprint(len(raw))+`,
		"receivedAt": "2026-11-13T09:00:05Z",
		"messageId": ["report@example.com"], "inReplyTo": ["ask@example.test"],
		"references": ["first@example.test", "ask@example.test"],
		"sender": [{"name": "Office", "email": "office@example.com"}],
		"from": [{"name": "Renée", "email": "renee@example.com"}],
		"to": [{"name": "Bob", "email": "bob@example.test"}, {"name": null, "email": "carol@example.test"}, {"name": null, "email": "dave@example.test"}],
		"cc": [{"name": null, "email": "erin@example.test"}], "bcc": null,
		"replyTo": [{"name": null, "email": "replies@example.com"}],
		"subject": "Rapport – Q3", "sentAt": "2026-11-13T10:00:00+01:00",
		"hasAttachment": true, "preview": "Voilà le rapport.",
		"bodyValues": {},
		"textBody": [`+part("1", "text/plain", 17, `"iso-8859-1"`, "null", "null")+`],
		"htmlBody": [`+part("2", "text/html", 32, `"utf-8"`, "null", "null")+`],
		"attachments": [`+part("3", "application/pdf", 9, "null", `"rapport Q3.pdf"`, `"attachment"`)+`]}`)

	prop := func(name string, more ...string) any {
		t.Helper()
		args := `{"accountId": "$A", "ids": ["` + id + `"], "properties": ["` + name + `"]` + strings.Join(more, "") + `}`
		obj := at(t, e.one("Email/get", args), "/list/0").(map[string]any)
		if len(obj) != 2 {
			t.Errorf("%s: %d properties", name, len(obj))
		}
		return obj[name]
	}
	// 4.1.2 and 4.1.3: header:Name:asForm:all.
	same(t, "raw", prop("header:Subject"), `" =?UTF-8?B?UmFwcG9ydCDigJMgUTM=?="`)
	same(t, "asRaw", prop("header:subject:asRaw"), `" =?UTF-8?B?UmFwcG9ydCDigJMgUTM=?="`)
	same(t, "asText", prop("header:Subject:asText"), `"Rapport – Q3"`)
	same(t, "asAddresses", prop("header:To:asAddresses"),
		`[{"name": "Bob", "email": "bob@example.test"}, {"name": null, "email": "carol@example.test"}, {"name": null, "email": "dave@example.test"}]`)
	same(t, "asGroupedAddresses", prop("header:To:asGroupedAddresses"), `[
		{"name": null, "addresses": [{"name": "Bob", "email": "bob@example.test"}]},
		{"name": "Team", "addresses": [{"name": null, "email": "carol@example.test"}, {"name": null, "email": "dave@example.test"}]}]`)
	same(t, "asMessageIds", prop("header:References:asMessageIds"), `["first@example.test", "ask@example.test"]`)
	same(t, "asDate", prop("header:Date:asDate"), `"2026-11-13T10:00:00+01:00"`)
	// "If the header field is not present ... null", and with :all an
	// empty list. Several instances: the last one, or all in order.
	same(t, "absent", prop("header:X-Nothing:asText"), `null`)
	same(t, "absent, all", prop("header:X-Nothing:asText:all"), `[]`)
	same(t, "last instance", prop("header:X-Tag:asText"), `"two"`)
	same(t, "all instances", prop("header:X-Tag:asText:all"), `["one", "two"]`)
	same(t, "all, raw", prop("header:X-Tag:all"), `[" one", " two"]`)
	same(t, "headers", at(t, prop("headers"), "/0"), `{"name": "Return-Path", "value": " <bounce@example.com>"}`)

	// 4.1.4: bodyStructure. Deviation, on purpose: a multipart always has
	// its subParts in the bodyStructure, although the default
	// bodyProperties of 4.2 do not list it. A structure without its parts
	// is of no use to anyone, and the servers we know do the same. A part
	// that is not a multipart has the property only when it is asked for.
	same(t, "bodyStructure", prop("bodyStructure", `, "bodyProperties": ["partId", "type"]`), `{
		"partId": null, "type": "multipart/mixed", "subParts": [
			{"partId": null, "type": "multipart/alternative", "subParts": [
				{"partId": "1", "type": "text/plain"},
				{"partId": "2", "type": "text/html"}]},
			{"partId": "3", "type": "application/pdf"}]}`)
	same(t, "bodyStructure, subParts asked for", prop("bodyStructure", `, "bodyProperties": ["partId", "subParts"]`), `{
		"partId": null, "subParts": [
			{"partId": null, "subParts": [{"partId": "1", "subParts": null}, {"partId": "2", "subParts": null}]},
			{"partId": "3", "subParts": null}]}`)
	same(t, "a list of parts, subParts asked for", prop("attachments", `, "bodyProperties": ["partId", "subParts"]`), `[{"partId": "3", "subParts": null}]`)
	same(t, "body part headers", prop("attachments", `, "bodyProperties": ["header:Content-Type:asRaw", "headers", "size"]`), `[{
		"header:Content-Type:asRaw": " application/pdf; name=old.pdf", "size": 9,
		"headers": [
			{"name": "Content-Type", "value": " application/pdf; name=old.pdf"},
			{"name": "Content-Disposition", "value": " attachment; filename*=UTF-8''rapport%20Q3.pdf"},
			{"name": "Content-Transfer-Encoding", "value": " base64"}]}]`)

	// bodyValues: only what the fetch arguments ask for; charset undone.
	same(t, "text values", prop("bodyValues", `, "fetchTextBodyValues": true`),
		`{"1": {"value": "Voilà le rapport.", "isEncodingProblem": false, "isTruncated": false}}`)
	same(t, "all values", prop("bodyValues", `, "fetchAllBodyValues": true, "maxBodyValueBytes": 5`), `{
		"1": {"value": "Voil", "isEncodingProblem": false, "isTruncated": true},
		"2": {"value": "<p>Vo", "isEncodingProblem": false, "isTruncated": true}}`)

	for args, want := range map[string]string{
		`{"accountId": "$A", "ids": ["` + id + `"], "properties": ["colour"]}`:                     "invalidArguments",
		`{"accountId": "$A", "ids": ["` + id + `"], "properties": ["header:Subject:asAddresses"]}`: "invalidArguments",
		`{"accountId": "$A", "ids": ["` + id + `"], "properties": ["header:From:asText"]}`:         "invalidArguments",
		`{"accountId": "$A", "ids": ["` + id + `"], "properties": ["header:X:asNothing"]}`:         "invalidArguments",
		`{"accountId": "$A", "ids": ["` + id + `"], "properties": ["header:X:all:asText"]}`:        "invalidArguments",
		`{"accountId": "$A", "ids": ["` + id + `"], "bodyProperties": ["colour"]}`:                 "invalidArguments",
		`{"accountId": "$A", "ids": ["` + id + `"], "maxBodyValueBytes": -1}`:                      "invalidArguments",
		`{"accountId": "$A", "ids": null}`:                                                         "requestTooLarge",
	} {
		if got := e.fail("Email/get", args); got != want {
			t.Errorf("Email/get %s: %s, want %s", args, got, want)
		}
	}
}

// The example of RFC 8620 3.7 (and RFC 8621 4.10): the first page of the
// inbox, one email per thread, then every email of those threads, in one
// request of four calls chained by result references.
func TestRFC8621QueryChainExample(t *testing.T) {
	e := newEnv(t)
	day := func(d int) time.Time { return time.Date(2026, 11, d, 12, 0, 0, 0, time.UTC) }
	a1 := e.deliver(store.Message{ReceivedAt: day(1), Raw: simple("qa1@x", "Anna <anna@x.test>", "Plan")})
	b1 := e.deliver(store.Message{ReceivedAt: day(2), Raw: simple("qb1@x", "Bert <bert@x.test>", "Other")})
	a2 := e.deliver(store.Message{ReceivedAt: day(3), Raw: simple("qa2@x", "Bert <bert@x.test>", "Re: Plan", "References: <qa1@x>\r\n")})
	e.deliver(store.Message{ReceivedAt: day(4), MailboxID: 4, Raw: simple("qj@x", "Spam <s@y.test>", "Junk")})

	got := e.do(`[ "Email/query", {
			"accountId": "$A",
			"filter": { "inMailbox": "m1" },
			"sort": [{ "property": "receivedAt", "isAscending": false }],
			"collapseThreads": true,
			"position": 0,
			"limit": 10,
			"calculateTotal": true
		}, "t0" ],
		[ "Email/get", {
			"accountId": "$A",
			"#ids": { "resultOf": "t0", "name": "Email/query", "path": "/ids" },
			"properties": [ "threadId" ]
		}, "t1" ],
		[ "Thread/get", {
			"accountId": "$A",
			"#ids": { "resultOf": "t1", "name": "Email/get", "path": "/list/*/threadId" }
		}, "t2" ],
		[ "Email/get", {
			"accountId": "$A",
			"#ids": { "resultOf": "t2", "name": "Thread/get", "path": "/list/*/emailIds" },
			"properties": [ "from", "receivedAt", "subject" ]
		}, "t3" ]`)
	state := e.state()
	same(t, "the four responses", got, `[
		[ "Email/query", {
			"accountId": "`+e.acct.ID+`", "queryState": "`+state+`", "canCalculateChanges": true,
			"position": 0, "total": 2, "ids": [ "`+eid(a2)+`", "`+eid(b1)+`" ]
		}, "t0" ],
		[ "Email/get", {
			"accountId": "`+e.acct.ID+`", "state": "`+state+`",
			"list": [ { "id": "`+eid(a2)+`", "threadId": "`+tid(a1)+`" }, { "id": "`+eid(b1)+`", "threadId": "`+tid(b1)+`" } ],
			"notFound": []
		}, "t1" ],
		[ "Thread/get", {
			"accountId": "`+e.acct.ID+`", "state": "`+state+`",
			"list": [
				{ "id": "`+tid(a1)+`", "emailIds": [ "`+eid(a1)+`", "`+eid(a2)+`" ] },
				{ "id": "`+tid(b1)+`", "emailIds": [ "`+eid(b1)+`" ] } ],
			"notFound": []
		}, "t2" ],
		[ "Email/get", {
			"accountId": "`+e.acct.ID+`", "state": "`+state+`",
			"list": [
				{ "id": "`+eid(a1)+`", "from": [{ "name": "Anna", "email": "anna@x.test" }], "receivedAt": "2026-11-01T12:00:00Z", "subject": "Plan" },
				{ "id": "`+eid(a2)+`", "from": [{ "name": "Bert", "email": "bert@x.test" }], "receivedAt": "2026-11-03T12:00:00Z", "subject": "Re: Plan" },
				{ "id": "`+eid(b1)+`", "from": [{ "name": "Bert", "email": "bert@x.test" }], "receivedAt": "2026-11-02T12:00:00Z", "subject": "Other" } ],
			"notFound": []
		}, "t3" ]]`)
}

// RFC 8621 4.4: Email/query. The filters and sorts themselves are tested
// in internal/store; this is their JSON form and the errors.
func TestRFC8621EmailQuery(t *testing.T) {
	e := newEnv(t)
	day := func(d int) time.Time { return time.Date(2026, 11, d, 12, 0, 0, 0, time.UTC) }
	e1 := e.deliver(store.Message{ReceivedAt: day(1), Raw: simple("f1@x", "Anna <anna@x.test>", "Plan")})
	e2 := e.deliver(store.Message{ReceivedAt: day(2), MailboxID: 4, Raw: simple("f2@x", "Spam <win@y.test>", "You WON")})
	e3 := e.deliver(store.Message{ReceivedAt: day(3), Keywords: []string{"$seen"},
		Raw: simple("f3@x", "Bert <bert@x.test>", "Re: Plan", "References: <f1@x>\r\n", "Cc: anna@x.test\r\n")})
	e4 := e.deliver(store.Message{ReceivedAt: day(4), Keywords: []string{"$seen", "$flagged"}, Raw: []byte(
		"From: Carl <carl@y.test>\r\nTo: list@example.test\r\nSubject: Invoice\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
			"--b\r\n\r\nsee the file\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment\r\n\r\n" +
			strings.Repeat("PDF ", 200) + "\r\n--b--\r\n")})
	ids := func(args string) any { t.Helper(); return e.one("Email/query", args)["ids"] }
	list := func(es ...store.Email) string {
		parts := make([]string, len(es))
		for i, x := range es {
			parts[i] = `"` + eid(x) + `"`
		}
		return "[" + strings.Join(parts, ",") + "]"
	}

	// Without filter and sort: everything, newest first.
	same(t, "all", ids(`{"accountId": "$A"}`), list(e4, e3, e2, e1))
	for _, tc := range []struct {
		filter string
		want   string
	}{
		{`{"inMailbox": "m1"}`, list(e4, e3, e1)},
		{`{"inMailbox": "m4"}`, list(e2)},
		{`{"inMailbox": "m99"}`, `[]`},
		{`{"inMailboxOtherThan": ["m4", "m5"]}`, list(e4, e3, e1)},
		{`{"before": "2026-11-03T12:00:00Z"}`, list(e2, e1)},
		{`{"after": "2026-11-03T12:00:00Z"}`, list(e4, e3)},
		{`{"after": "2026-11-03T14:00:00+02:00"}`, list(e4, e3)},
		{`{"minSize": 500}`, list(e4)},
		{`{"maxSize": 500}`, list(e3, e2, e1)},
		{`{"hasKeyword": "$seen"}`, list(e4, e3)},
		{`{"notKeyword": "$seen"}`, list(e2, e1)},
		{`{"allInThreadHaveKeyword": "$seen"}`, list(e4)},
		{`{"someInThreadHaveKeyword": "$seen"}`, list(e4, e3, e1)},
		{`{"noneInThreadHaveKeyword": "$seen"}`, list(e2)},
		{`{"hasAttachment": true}`, list(e4)},
		{`{"from": "anna"}`, list(e1)},
		{`{"to": "list@"}`, list(e4)},
		{`{"cc": "anna"}`, list(e3)},
		{`{"bcc": "anna"}`, `[]`},
		{`{"subject": "plan"}`, list(e3, e1)},
		{`{"inMailbox": "m1", "hasKeyword": "$seen", "subject": "plan"}`, list(e3)},
		{`{}`, list(e4, e3, e2, e1)},
		// The example of RFC 8621 4.4.1's operators, in our terms: in the
		// inbox, and not (seen or from Carl).
		{`{"operator": "AND", "conditions": [{"inMailbox": "m1"},
			{"operator": "NOT", "conditions": [{"hasKeyword": "$seen"}, {"from": "carl"}]}]}`, list(e1)},
		{`{"operator": "OR", "conditions": [{"hasAttachment": true}, {"inMailbox": "m4"}]}`, list(e4, e2)},
	} {
		same(t, tc.filter, ids(`{"accountId": "$A", "filter": `+tc.filter+`}`), tc.want)
	}

	// Sort: isAscending defaults to true (RFC 8620 5.5).
	same(t, "receivedAt ascending by default", ids(`{"accountId": "$A", "sort": [{"property": "receivedAt"}]}`), list(e1, e2, e3, e4))
	same(t, "size descending", at(t, ids(`{"accountId": "$A", "sort": [{"property": "size", "isAscending": false}]}`), "/0"), `"`+eid(e4)+`"`)
	same(t, "from", ids(`{"accountId": "$A", "sort": [{"property": "from"}]}`), list(e1, e3, e4, e2))
	same(t, "subject", ids(`{"accountId": "$A", "sort": [{"property": "subject", "collation": "i;ascii-casemap"}]}`), list(e4, e1, e3, e2))
	// RFC 8621 4.4.2's example: flagged first, then newest first.
	same(t, "flagged first", ids(`{"accountId": "$A", "sort": [
		{"property": "hasKeyword", "keyword": "$flagged", "isAscending": false},
		{"property": "receivedAt", "isAscending": false}]}`), list(e4, e3, e2, e1))

	// collapseThreads (4.4.3): only the first email of each thread.
	same(t, "collapsed", ids(`{"accountId": "$A", "collapseThreads": true}`), list(e4, e3, e2))
	same(t, "collapsed, oldest first", ids(`{"accountId": "$A", "collapseThreads": true, "sort": [{"property": "receivedAt"}]}`), list(e1, e2, e4))

	// The window (RFC 8620 5.5).
	page := e.one("Email/query", `{"accountId": "$A", "position": 1, "limit": 2, "calculateTotal": true}`)
	same(t, "window", page, `{"accountId": "`+e.acct.ID+`", "queryState": "`+e.state()+`", "canCalculateChanges": true,
		"position": 1, "ids": `+list(e3, e2)+`, "total": 4}`)
	page = e.one("Email/query", `{"accountId": "$A", "position": -1, "calculateTotal": true}`)
	same(t, "from the end", []any{page["position"], page["ids"], page["total"]}, `[3, `+list(e1)+`, 4]`)
	page = e.one("Email/query", `{"accountId": "$A", "anchor": "`+eid(e2)+`", "anchorOffset": -1, "limit": 2}`)
	same(t, "anchor", []any{page["position"], page["ids"]}, `[1, `+list(e3, e2)+`]`)
	page = e.one("Email/query", `{"accountId": "$A", "position": 50}`)
	same(t, "past the end", []any{page["position"], page["ids"]}, `[50, []]`)
	page = e.one("Email/query", `{"accountId": "$A", "limit": 0, "calculateTotal": true}`)
	same(t, "limit 0", []any{page["ids"], page["total"]}, `[[], 4]`)
	if _, has := e.one("Email/query", `{"accountId": "$A"}`)["total"]; has {
		t.Error("total without calculateTotal")
	}
	same(t, "clamped limit", e.one("Email/query", `{"accountId": "$A", "limit": 9999}`)["limit"], `500`)

	for args, want := range map[string]string{
		// 4.4.1: header is a list of one or two strings.
		`{"accountId": "$A", "filter": {"text": 5}}`:                                   "invalidArguments",
		`{"accountId": "$A", "filter": {"body": null}}`:                                "invalidArguments",
		`{"accountId": "$A", "filter": {"header": []}}`:                                "invalidArguments",
		`{"accountId": "$A", "filter": {"header": ["X-Tag", "one", "two"]}}`:           "invalidArguments",
		`{"accountId": "$A", "filter": {"header": "X-Tag"}}`:                           "invalidArguments",
		`{"accountId": "$A", "filter": {"colour": "red"}}`:                             "invalidArguments",
		`{"accountId": "$A", "filter": {"before": "yesterday"}}`:                       "invalidArguments",
		`{"accountId": "$A", "filter": {"minSize": -1}}`:                               "invalidArguments",
		`{"accountId": "$A", "filter": {"hasKeyword": ""}}`:                            "invalidArguments",
		`{"accountId": "$A", "filter": {"inMailbox": 1}}`:                              "invalidArguments",
		`{"accountId": "$A", "filter": {"operator": "AND", "conditions": [], "x": 1}}`: "invalidArguments",
		`{"accountId": "$A", "sort": [{"property": "allInThreadHaveKeyword"}]}`:        "unsupportedSort",
		`{"accountId": "$A", "sort": [{"property": "preview"}]}`:                       "unsupportedSort",
		`{"accountId": "$A", "sort": [{"property": "hasKeyword"}]}`:                    "invalidArguments",
		`{"accountId": "$A", "anchor": "e999"}`:                                        "anchorNotFound",
		`{"accountId": "$A", "limit": -5}`:                                             "invalidArguments",
		`{"accountId": "$A", "collapseThreads": "yes"}`:                                "invalidArguments",
	} {
		if got := e.fail("Email/query", args); got != want {
			t.Errorf("Email/query %s: %s, want %s", args, got, want)
		}
	}
	// A filter nested without end is refused, not followed.
	deep := strings.Repeat(`{"operator":"NOT","conditions":[`, 50) + `{}` + strings.Repeat(`]}`, 50)
	if got := e.fail("Email/query", `{"accountId": "$A", "filter": `+deep+`}`); got != "invalidArguments" {
		t.Errorf("deep filter: %s", got)
	}
}

// RFC 8621 4.6: Email/set. Its examples of patches: "keywords/$flagged":
// true to add a keyword, null to remove one, and a move between mailboxes
// as one null and one true. Then 4.3 Email/changes and 4.5
// Email/queryChanges over what happened.
func TestRFC8621EmailSet(t *testing.T) {
	e := newEnv(t)
	e1 := e.deliver(store.Message{Raw: simple("u1@x", "a@x.test", "one")})
	e2 := e.deliver(store.Message{Raw: simple("u2@x", "a@x.test", "two")})
	e3 := e.deliver(store.Message{Raw: simple("u3@x", "a@x.test", "three")})
	start := e.state()
	props := func(em store.Email) any {
		t.Helper()
		got := e.one("Email/get", `{"accountId": "$A", "ids": ["`+eid(em)+`"], "properties": ["keywords", "mailboxIds"]}`)
		return at(t, got, "/list/0")
	}

	// Set a keyword.
	got := e.one("Email/set", `{"accountId": "$A", "ifInState": "`+start+`", "update": {"`+eid(e1)+`": {"keywords/$seen": true}}}`)
	same(t, "response", got, `{"accountId": "`+e.acct.ID+`", "oldState": "`+start+`", "newState": "`+e.state()+`",
		"created": null, "updated": {"`+eid(e1)+`": null}, "destroyed": null,
		"notCreated": null, "notUpdated": null, "notDestroyed": null}`)
	same(t, "seen", props(e1), `{"id": "`+eid(e1)+`", "keywords": {"$seen": true}, "mailboxIds": {"m1": true}}`)
	// Add one, remove one, in one patch. Keywords are case-insensitive and
	// stored in lowercase (4.1.1).
	e.one("Email/set", `{"accountId": "$A", "update": {"`+eid(e1)+`": {"keywords/$Flagged": true, "keywords/$seen": null}}}`)
	same(t, "flagged", props(e1), `{"id": "`+eid(e1)+`", "keywords": {"$flagged": true}, "mailboxIds": {"m1": true}}`)
	// Replace all keywords.
	e.one("Email/set", `{"accountId": "$A", "update": {"`+eid(e1)+`": {"keywords": {"$seen": true, "project~x/y": true}}}}`)
	same(t, "replaced", at(t, props(e1), "/keywords"), `{"$seen": true, "project~x/y": true}`)
	// RFC 6901 escapes in a patch path: "~0" is "~", "~1" is "/".
	e.one("Email/set", `{"accountId": "$A", "update": {"`+eid(e1)+`": {"keywords/project~0x~1y": null}}}`)
	same(t, "escaped path", at(t, props(e1), "/keywords"), `{"$seen": true}`)

	// Move: out of the inbox, into the trash, in one update.
	e.one("Email/set", `{"accountId": "$A", "update": {"`+eid(e2)+`": {"mailboxIds/m1": null, "mailboxIds/m5": true}}}`)
	same(t, "moved", at(t, props(e2), "/mailboxIds"), `{"m5": true}`)
	// Label: also in another mailbox. Then set the whole set.
	e.one("Email/set", `{"accountId": "$A", "update": {"`+eid(e2)+`": {"mailboxIds/m1": true}}}`)
	same(t, "labelled", at(t, props(e2), "/mailboxIds"), `{"m1": true, "m5": true}`)
	e.one("Email/set", `{"accountId": "$A", "update": {"`+eid(e2)+`": {"mailboxIds": {"m4": true}, "keywords": {}}}}`)
	same(t, "replaced mailboxes", props(e2), `{"id": "`+eid(e2)+`", "keywords": {}, "mailboxIds": {"m4": true}}`)

	// What is refused, per email, while the others in the call go through.
	before := e.state()
	got = e.one("Email/set", `{"accountId": "$A",
		"create": {"draft": {"subject": "hi"}},
		"update": {
			"`+eid(e3)+`": {"keywords/$seen": true},
			"e998": {"keywords/$seen": true},
			"nonsense": {"keywords/$seen": true},
			"`+eid(e1)+`": {"subject": "changed", "size": 1},
			"`+eid(e2)+`": {"mailboxIds/m4": null}},
		"destroy": ["e997"]}`)
	same(t, "updated", got["updated"], `{"`+eid(e3)+`": null}`)
	// An email has to be in a mailbox (RFC 8621 4.6); creating is tested
	// in rfc8621_create_test.go.
	same(t, "create", at(t, got, "/notCreated/draft"), `{"type": "invalidProperties",
		"description": "an email must be in at least one mailbox", "properties": ["mailboxIds"]}`)
	same(t, "unknown ids", []any{at(t, got, "/notUpdated/e998/type"), at(t, got, "/notUpdated/nonsense/type"), at(t, got, "/notDestroyed/e997/type")},
		`["notFound", "notFound", "notFound"]`)
	// "invalidProperties ... properties: lists all the properties that were invalid."
	same(t, "immutable", at(t, got, "/notUpdated/"+eid(e1)), `{"type": "invalidProperties",
		"description": "only keywords and mailboxIds of an email can change", "properties": ["size", "subject"]}`)
	// 4.6: "an Email MUST belong to one or more Mailboxes".
	same(t, "no mailbox left", at(t, got, "/notUpdated/"+eid(e2)+"/properties"), `["mailboxIds"]`)
	same(t, "one write", []any{got["oldState"], got["newState"]}, fmt.Sprintf(`[%q, %q]`, before, e.state()))

	for patch, want := range map[string]string{
		`{"mailboxIds": {}}`:             "invalidProperties",
		`{"mailboxIds": {"m99": true}}`:  "invalidProperties",
		`{"mailboxIds/m99": true}`:       "invalidProperties",
		`{"mailboxIds": {"m1": false}}`:  "invalidProperties",
		`{"mailboxIds/m1": false}`:       "invalidProperties",
		`{"keywords": {"$seen": false}}`: "invalidProperties",
		`{"keywords/$seen": "yes"}`:      "invalidProperties",
		`{"keywords/has space": true}`:   "invalidProperties",
		`{"keywords": ["$seen"]}`:        "invalidProperties",
		// RFC 8620 5.3: a patch that sets a property and something in it.
		`{"keywords": {"a": true}, "keywords/b": true}`:       "invalidPatch",
		`{"mailboxIds": {"m1": true}, "mailboxIds/m4": true}`: "invalidPatch",
	} {
		got := e.one("Email/set", `{"accountId": "$A", "update": {"`+eid(e3)+`": `+patch+`}}`)
		if typ := at(t, got, "/notUpdated/"+eid(e3)+"/type"); typ != want {
			t.Errorf("patch %s: %v, want %s", patch, typ, want)
		}
	}
	same(t, "e3 untouched by refused patches", props(e3), `{"id": "`+eid(e3)+`", "keywords": {"$seen": true}, "mailboxIds": {"m1": true}}`)
	// Removing a mailbox the email is not in, and one that does not exist,
	// changes nothing and is not an error: the patch's end state holds.
	got = e.one("Email/set", `{"accountId": "$A", "update": {"`+eid(e3)+`": {"mailboxIds/m5": null, "mailboxIds/m99": null}}}`)
	same(t, "remove absent", got["updated"], `{"`+eid(e3)+`": null}`)

	// ifInState.
	if got := e.fail("Email/set", `{"accountId": "$A", "ifInState": "`+start+`", "update": {"`+eid(e3)+`": {"keywords/x": true}}}`); got != "stateMismatch" {
		t.Errorf("old ifInState: %s", got)
	}
	same(t, "not applied", at(t, props(e3), "/keywords"), `{"$seen": true}`)

	// Email/changes from the start: all three updated, none created.
	ch := e.one("Email/changes", `{"accountId": "$A", "sinceState": "`+start+`"}`)
	same(t, "Email/changes", ch, `{"accountId": "`+e.acct.ID+`", "oldState": "`+start+`", "newState": "`+e.state()+`",
		"hasMoreChanges": false, "created": [], "updated": ["`+eid(e1)+`", "`+eid(e2)+`", "`+eid(e3)+`"], "destroyed": []}`)

	// Destroy.
	mid := e.state()
	got = e.one("Email/set", `{"accountId": "$A", "destroy": ["`+eid(e1)+`", "`+eid(e1)+`"]}`)
	same(t, "destroyed", []any{got["destroyed"], at(t, got, "/notDestroyed/"+eid(e1)+"/type")}, `[["`+eid(e1)+`"], "notFound"]`)
	same(t, "gone", e.one("Email/get", `{"accountId": "$A", "ids": ["`+eid(e1)+`"]}`)["notFound"], `["`+eid(e1)+`"]`)
	e4 := e.deliver(store.Message{Raw: simple("u4@x", "a@x.test", "four")})
	ch = e.one("Email/changes", `{"accountId": "$A", "sinceState": "`+mid+`"}`)
	same(t, "after destroy and delivery", []any{ch["created"], ch["updated"], ch["destroyed"]}, `[["`+eid(e4)+`"], [], ["`+eid(e1)+`"]]`)

	// Email/queryChanges for the inbox, newest first, since mid: e1 left,
	// e4 came in at the top. e2 is in Junk and e3 did not change.
	qc := e.one("Email/queryChanges", `{"accountId": "$A", "filter": {"inMailbox": "m1"}, "sinceQueryState": "`+mid+`", "calculateTotal": true}`)
	same(t, "queryChanges", qc, `{"accountId": "`+e.acct.ID+`", "oldQueryState": "`+mid+`", "newQueryState": "`+e.state()+`",
		"removed": ["`+eid(e4)+`", "`+eid(e1)+`"], "added": [{"id": "`+eid(e4)+`", "index": 0}], "total": 2}`)
	// Applying it to the old results gives the new results.
	old := []string{eid(e3), eid(e1)} // the inbox at mid, newest first
	now := e.one("Email/query", `{"accountId": "$A", "filter": {"inMailbox": "m1"}}`)["ids"]
	same(t, "old results patched", applyQueryChanges(old, qc), jsonOf(now))
	// With collapseThreads a change to one email of a thread makes every
	// email of the thread a candidate.
	e5 := e.deliver(store.Message{Raw: simple("u5@x", "a@x.test", "Re: three", "References: <u3@x>\r\n")})
	before5 := stateString(mustState(t, e.state()) - 1)
	qc = e.one("Email/queryChanges", `{"accountId": "$A", "collapseThreads": true, "sinceQueryState": "`+before5+`"}`)
	// Newest first, one per thread: e5 now stands for the thread of e3.
	same(t, "collapsed queryChanges", []any{qc["removed"], qc["added"]},
		`[["`+eid(e5)+`", "`+eid(e3)+`"], [{"id": "`+eid(e5)+`", "index": 0}]]`)
	for args, want := range map[string]string{
		`{"accountId": "$A"}`: "invalidArguments",
		`{"accountId": "$A", "sinceQueryState": "` + mid + `", "maxChanges": 1}`:                "tooManyChanges",
		`{"accountId": "$A", "sinceQueryState": "` + mid + `", "position": 0}`:                  "invalidArguments",
		`{"accountId": "$A", "sinceQueryState": "x"}`:                                           "cannotCalculateChanges",
		`{"accountId": "$A", "sinceQueryState": "` + mid + `", "filter": {"text": 1}}`:          "invalidArguments",
		`{"accountId": "$A", "sinceQueryState": "` + mid + `", "sort": [{"property": "nope"}]}`: "unsupportedSort",
	} {
		if got := e.fail("Email/queryChanges", args); got != want {
			t.Errorf("Email/queryChanges %s: %s, want %s", args, got, want)
		}
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// applyQueryChanges does what RFC 8620 5.6 tells a client to do: remove
// the removed ids, then insert the added ones at their index, lowest first.
func applyQueryChanges(old []string, qc map[string]any) []string {
	removed := map[string]bool{}
	for _, id := range qc["removed"].([]any) {
		removed[id.(string)] = true
	}
	out := []string{}
	for _, id := range old {
		if !removed[id] {
			out = append(out, id)
		}
	}
	for _, a := range qc["added"].([]any) {
		item := a.(map[string]any)
		i := int(item["index"].(float64))
		out = append(out[:i], append([]string{item["id"].(string)}, out[i:]...)...)
	}
	return out
}

// RFC 8620 5.2: "cannotCalculateChanges" for a state the server no longer
// has the changes for. The change log is trimmed after 30 days.
func TestRFC8620CannotCalculateChanges(t *testing.T) {
	e := newEnv(t)
	start := e.state()
	// A change written "40 days ago", then the trim, as serve runs it once a
	// day. Only this account is trimmed here: the database is shared with
	// other tests.
	e.st.Now = func() time.Time { return time.Now().Add(-40 * 24 * time.Hour) }
	e.deliver(store.Message{Raw: simple("o1@x", "a@x.test", "old")})
	e.st.Now = time.Now
	fresh := e.deliver(store.Message{Raw: simple("o2@x", "a@x.test", "fresh")})
	if _, err := e.st.TrimAccountChanges(e.ctx, e.acct.ID, time.Now().Add(-store.ChangesRetention)); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"Email/changes", "Mailbox/changes", "Thread/changes"} {
		if got := e.fail(m, `{"accountId": "$A", "sinceState": "`+start+`"}`); got != "cannotCalculateChanges" {
			t.Errorf("%s from a trimmed state: %s", m, got)
		}
	}
	for _, m := range []string{"Email/queryChanges", "Mailbox/queryChanges"} {
		if got := e.fail(m, `{"accountId": "$A", "sinceQueryState": "`+start+`"}`); got != "cannotCalculateChanges" {
			t.Errorf("%s from a trimmed state: %s", m, got)
		}
	}
	// From the state right after what was trimmed it works.
	after := stateString(mustState(t, start) + 1)
	ch := e.one("Email/changes", `{"accountId": "$A", "sinceState": "`+after+`"}`)
	same(t, "from the trim point", ch["created"], `["`+eid(fresh)+`"]`)
}

func mustState(t *testing.T, s string) int64 {
	t.Helper()
	n, ok := parseState(s)
	if !ok {
		t.Fatalf("%q is not a state", s)
	}
	return n
}

// RFC 8620 6.2: downloading. The blobId of an Email gives the message as
// it was received; the blobId of a body part gives the part with its
// transfer encoding undone.
func TestRFC8620Download(t *testing.T) {
	e := newEnv(t)
	raw := []byte("From: a@x.test\r\nSubject: files\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nhello\r\n" +
		"--b\r\nContent-Type: image/png\r\nContent-Disposition: attachment; filename=dot.png\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"iVBORw0KGgo=\r\n--b--\r\n")
	em := e.deliver(store.Message{Raw: raw})

	rec := e.get("/jmap/download/" + e.acct.ID + "/" + em.Blob + "/message.eml?type=message/rfc822")
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatalf("message: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	h := rec.Header()
	// "type: The type for the server to set in the Content-Type header",
	// "name: The name for the file".
	same(t, "headers", []string{h.Get("Content-Type"), h.Get("Content-Disposition"), h.Get("Content-Length"),
		h.Get("X-Content-Type-Options"), h.Get("Content-Security-Policy")},
		fmt.Sprintf(`["message/rfc822", "attachment; filename*=UTF-8''message.eml", "%d", "nosniff", "default-src 'none'; sandbox"]`, len(raw)))
	// "the blob is immutable ... so the server SHOULD ... allow caching".
	if cc := h.Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control %q", cc)
	}

	rec = e.get("/jmap/download/" + e.acct.ID + "/" + em.Blob + "-2/dot.png?type=image/png")
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), []byte("\x89PNG\r\n\x1a\n")) || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("part: %d %q %q", rec.Code, rec.Body.Bytes(), rec.Header().Get("Content-Type"))
	}
	rec = e.get("/jmap/download/" + e.acct.ID + "/" + em.Blob + "-1/" + "r%C3%A9sum%C3%A9%20%22x%22.txt")
	if rec.Code != 200 || rec.Body.String() != "hello" {
		t.Errorf("text part: %d %q", rec.Code, rec.Body.String())
	}
	// Without a type, or with one that is not a media type: a download.
	same(t, "name and default type", []string{rec.Header().Get("Content-Type"), rec.Header().Get("Content-Disposition")},
		`["application/octet-stream", "attachment; filename*=UTF-8''r%C3%A9sum%C3%A9%20%22x%22.txt"]`)

	other := newEnv(t)
	for name, path := range map[string]string{
		"unknown blob":       "/jmap/download/" + e.acct.ID + "/" + strings.Repeat("0", 64) + "/x",
		"unknown part":       "/jmap/download/" + e.acct.ID + "/" + em.Blob + "-9/x",
		"not a blob id":      "/jmap/download/" + e.acct.ID + "/..%2F..%2Fetc%2Fpasswd/x",
		"part id nonsense":   "/jmap/download/" + e.acct.ID + "/" + em.Blob + "-x/x",
		"another account id": "/jmap/download/" + other.acct.ID + "/" + em.Blob + "/x",
	} {
		rec := e.get(path)
		if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/problem+json" || rec.Header().Get("Content-Disposition") != "" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Another user cannot read this account's blob, also not under their
	// own account id.
	req := httptest.NewRequest("GET", "/jmap/download/"+other.acct.ID+"/"+em.Blob+"/x", nil)
	req = req.WithContext(WithUser(req.Context(), other.user))
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("another user's download: %d", rec.Code)
	}
	// A destroyed email's blob is gone at once, also while its file is
	// still on disk waiting for the sweeper.
	e.one("Email/set", `{"accountId": "$A", "destroy": ["`+eid(em)+`"]}`)
	if rec := e.get("/jmap/download/" + e.acct.ID + "/" + em.Blob + "/x"); rec.Code != 404 {
		t.Errorf("after destroy: %d", rec.Code)
	}
}

// The HTML view: sanitised, with a policy that lets nothing run or load,
// and remote images only when the query asks for them.
func TestHTMLView(t *testing.T) {
	e := newEnv(t)
	raw := []byte("From: a@x.test\r\nSubject: view\r\nContent-Type: multipart/related; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		`<html><head><style>p{color:red}</style><script>alert(1)</script></head><body onload="alert(2)">` +
		`<p style="color:blue;background:url(http://evil.test/x)">Hi <a href="javascript:alert(3)">there</a>, ` +
		`<a href="https://example.test/page">a link</a></p>` +
		`<img src="cid:logo@x"><img src="https://cdn.example.test/pixel.gif"><iframe src="https://evil.test"></iframe>` +
		`<form action="https://evil.test"><input name="password"></form></body></html>` + "\r\n" +
		"--b\r\nContent-Type: image/png\r\nContent-ID: <logo@x>\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw0KGgo=\r\n--b--\r\n")
	em := e.deliver(store.Message{Raw: raw})
	path := "/jmap/view/" + e.acct.ID + "/" + eid(em)

	rec := e.get(path)
	body := rec.Body.String()
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "img-src data:;", "sandbox", "frame-ancestors 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "script") || strings.Contains(csp, "http") {
		t.Errorf("CSP: %s", csp)
	}
	same(t, "headers", []string{rec.Header().Get("X-Remote-Images"), rec.Header().Get("X-Content-Type-Options"),
		rec.Header().Get("X-Frame-Options"), rec.Header().Get("Referrer-Policy"), rec.Header().Get("Cache-Control")},
		`["blocked", "nosniff", "SAMEORIGIN", "no-referrer", "no-store"]`)
	for _, bad := range []string{"<script", "alert(", "onload", "javascript:", "evil.test", "<iframe", "<form", "<input", "cdn.example.test", "cid:", "color:red"} {
		if strings.Contains(body, bad) {
			t.Errorf("the view has %q:\n%s", bad, body)
		}
	}
	for _, want := range []string{`<p style="color: blue">Hi <a>there</a>, `,
		`<a href="https://example.test/page" target="_blank" rel="noopener noreferrer nofollow">a link</a>`,
		`<img src="data:image/png;base64,iVBORw0KGgo="`, `<!doctype html>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the view lacks %q:\n%s", want, body)
		}
	}

	rec = e.get(path + "?images=1")
	if !strings.Contains(rec.Body.String(), `src="https://cdn.example.test/pixel.gif"`) || rec.Header().Get("X-Remote-Images") != "shown" ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "img-src data: https: http:;") {
		t.Errorf("with images: %s\n%s", rec.Header().Get("Content-Security-Policy"), rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "evil.test") || strings.Contains(rec.Header().Get("Content-Security-Policy"), "script") {
		t.Error("images=1 let more than images through")
	}

	// A message without HTML: its text, escaped.
	plain := e.deliver(store.Message{Raw: []byte("Subject: plain\r\n\r\n1 < 2 & <script>alert(1)</script>\r\n")})
	rec = e.get("/jmap/view/" + e.acct.ID + "/" + eid(plain))
	if !strings.Contains(rec.Body.String(), "<pre>1 &lt; 2 &amp; &lt;script&gt;alert(1)&lt;/script&gt;\n</pre>") || rec.Header().Get("X-Remote-Images") != "none" {
		t.Errorf("plain view: %s", rec.Body.String())
	}
	for _, p := range []string{"/jmap/view/" + e.acct.ID + "/e999", "/jmap/view/" + e.acct.ID + "/x", "/jmap/view/other/" + eid(em)} {
		if rec := e.get(p); rec.Code != 404 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}
