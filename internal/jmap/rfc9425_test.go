package jmap

import (
	"testing"

	"github.com/robinmail/robinmail/internal/store"
)

func (e *env) quota(name, args string) map[string]any {
	e.t.Helper()
	got := e.doWith(`"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:quota"],"methodCalls":[["` + name + `", ` + args + `, "c"]]`)
	if at(e.t, got, "/0/0") != name {
		e.t.Fatalf("%s %s answered %s", name, args, jsonOf(got))
	}
	return at(e.t, got, "/0/1").(map[string]any)
}

// RFC 9425 3: the capability; 4: the Quota object; 4.1 to 4.4: /get,
// /changes with updatedProperties, /query and /queryChanges.
func TestRFC9425Quota(t *testing.T) {
	e := newEnv(t)
	session := decode(t, e.get("/jmap/session").Body.Bytes())
	// "This represents support for the Quota data type ... The value of
	// this property is an empty object in both the JMAP Session
	// "capabilities" property and an account's "accountCapabilities"".
	same(t, "capability", []any{at(t, session, "/capabilities/urn:ietf:params:jmap:quota"),
		at(t, session, "/accounts/"+e.acct.ID+"/accountCapabilities/urn:ietf:params:jmap:quota")}, `[{}, {}]`)
	if got := e.fail("Quota/get", `{"accountId": "$A", "ids": null}`); got != "unknownMethod" {
		t.Errorf("without the capability in using: %s", got)
	}

	// An account without limits has no Quota objects.
	same(t, "no limits", e.quota("Quota/get", `{"accountId": "$A", "ids": null}`)["list"], `[]`)

	m1 := e.deliver(store.Message{Raw: simple("q1@x", "a@x.test", "one")})
	m2 := e.deliver(store.Message{Raw: simple("q2@x", "a@x.test", "two")})
	bytes, count := int64(5000), int64(3)
	if err := e.st.SetQuota(e.ctx, e.acct.ID, &bytes, &count); err != nil {
		t.Fatal(err)
	}
	used := m1.Size + m2.Size
	got := e.quota("Quota/get", `{"accountId": "$A", "ids": null}`)
	// "resourceType: count | octets", "used", "hardLimit", "scope:
	// account | domain | global", "name", "types: A list of all the type
	// names ... to which this quota applies".
	same(t, "both quotas", got["list"], `[
		{"id": "octets", "resourceType": "octets", "used": `+jsonOf(used)+`, "hardLimit": 5000, "scope": "account", "name": "Storage",
		 "types": ["Email", "CalendarEvent", "ContactCard"], "warnLimit": null, "softLimit": null,
		 "description": "The size of all messages, events and contacts in the account, in octets. Each message counts once, whatever number of mailboxes it is in."},
		{"id": "count", "resourceType": "count", "used": 2, "hardLimit": 3, "scope": "account", "name": "Messages",
		 "types": ["Email", "CalendarEvent", "ContactCard"], "warnLimit": null, "softLimit": null, "description": "The number of messages, events and contacts in the account."}]`)
	some := e.quota("Quota/get", `{"accountId": "$A", "ids": ["count", "nonesuch"], "properties": ["used", "hardLimit"]}`)
	same(t, "by id", []any{some["list"], some["notFound"]}, `[[{"id": "count", "used": 2, "hardLimit": 3}], ["nonesuch"]]`)

	// 4.2: "updatedProperties: ... If only the "used" Quota properties has
	// changed since the old state, this will be a list containing only
	// that property."
	state := got["state"].(string)
	ch := e.quota("Quota/changes", `{"accountId": "$A", "sinceState": "`+state+`"}`)
	same(t, "nothing changed", []any{ch["updated"], ch["updatedProperties"], ch["hasMoreChanges"], ch["newState"]}, `[[], null, false, "`+state+`"]`)
	m3 := e.deliver(store.Message{Raw: simple("q3@x", "a@x.test", "three")})
	ch = e.quota("Quota/changes", `{"accountId": "$A", "sinceState": "`+state+`"}`)
	same(t, "after a delivery", []any{ch["created"], ch["updated"], ch["destroyed"], ch["updatedProperties"]}, `[[], ["octets", "count"], [], ["used"]]`)
	after := ch["newState"].(string)
	// A change that is not about mail in the account does not touch it.
	e.one("Mailbox/set", `{"accountId": "$A", "create": {"m": {"name": "Other", "parentId": null}}}`)
	ch = e.quota("Quota/changes", `{"accountId": "$A", "sinceState": "`+after+`"}`)
	same(t, "after a mailbox was made", ch["updated"], `[]`)
	// A limit that was taken away: that quota is gone, the other changed
	// in more than its use.
	none := int64(0)
	if err := e.st.SetQuota(e.ctx, e.acct.ID, &bytes, &none); err != nil {
		t.Fatal(err)
	}
	ch = e.quota("Quota/changes", `{"accountId": "$A", "sinceState": "`+after+`"}`)
	same(t, "after a limit went", []any{ch["updated"], ch["destroyed"], ch["updatedProperties"]}, `[["octets"], ["count"], null]`)
	for args, want := range map[string]string{
		`{"accountId": "$A"}`:                                     "invalidArguments",
		`{"accountId": "$A", "sinceState": "x"}`:                  "cannotCalculateChanges",
		`{"accountId": "$A", "sinceState": "1", "maxChanges": 0}`: "invalidArguments",
	} {
		res := e.doWith(`"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:quota"],"methodCalls":[["Quota/changes", ` + args + `, "c"]]`)
		if got := at(t, res, "/0/1/type"); got != want {
			t.Errorf("Quota/changes %s: %v, want %s", args, got, want)
		}
	}

	// 4.3: Quota/query. Filter: name, scope, resourceType, type. Sort:
	// name and used.
	if err := e.st.SetQuota(e.ctx, e.acct.ID, &bytes, &count); err != nil {
		t.Fatal(err)
	}
	ids := func(args string) any { t.Helper(); return e.quota("Quota/query", `{"accountId": "$A"`+args+`}`)["ids"] }
	same(t, "all", ids(``), `["octets", "count"]`)
	same(t, "by name", ids(`, "sort": [{"property": "name"}]`), `["count", "octets"]`)
	same(t, "by used, descending", ids(`, "sort": [{"property": "used", "isAscending": false}]`), `["octets", "count"]`)
	same(t, "filter name", ids(`, "filter": {"name": "stor"}`), `["octets"]`)
	same(t, "filter resourceType", ids(`, "filter": {"resourceType": "count"}`), `["count"]`)
	same(t, "filter scope", ids(`, "filter": {"scope": "domain"}`), `[]`)
	same(t, "filter type", ids(`, "filter": {"type": "Email"}`), `["octets", "count"]`)
	same(t, "filter type, another", ids(`, "filter": {"type": "Calendar"}`), `[]`)
	same(t, "operators", ids(`, "filter": {"operator": "NOT", "conditions": [{"resourceType": "octets"}]}`), `["count"]`)
	for args, want := range map[string]string{
		`{"accountId": "$A", "filter": {"colour": "red"}}`:         "unsupportedFilter",
		`{"accountId": "$A", "sort": [{"property": "hardLimit"}]}`: "unsupportedSort",
	} {
		res := e.doWith(`"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:quota"],"methodCalls":[["Quota/query", ` + args + `, "c"]]`)
		if got := at(t, res, "/0/1/type"); got != want {
			t.Errorf("Quota/query %s: %v, want %s", args, got, want)
		}
	}
	// 4.4: Quota/queryChanges.
	qs := e.quota("Quota/query", `{"accountId": "$A"}`)["queryState"].(string)
	qc := e.quota("Quota/queryChanges", `{"accountId": "$A", "sinceQueryState": "`+qs+`"}`)
	same(t, "queryChanges, nothing", []any{qc["removed"], qc["added"]}, `[[], []]`)
	if _, err := e.st.DestroyEmail(e.ctx, e.acct.ID, m3.ID); err != nil {
		t.Fatal(err)
	}
	qc = e.quota("Quota/queryChanges", `{"accountId": "$A", "sinceQueryState": "`+qs+`", "sort": [{"property": "used"}]}`)
	same(t, "queryChanges after a destroy", []any{qc["removed"], qc["added"]},
		`[["octets", "count"], [{"id": "count", "index": 0}, {"id": "octets", "index": 1}]]`)

	// RFC 8620 5.3, "overQuota": creating an Email past the limit.
	e.deliver(store.Message{Raw: simple("q4@x", "a@x.test", "four")})
	set := e.one("Email/set", `{"accountId": "$A", "create": {"x": {"mailboxIds": {"m1": true}, "subject": "one too many",
		"textBody": [{"partId": "t", "type": "text/plain"}], "bodyValues": {"t": {"value": "x"}}}}}`)
	same(t, "overQuota", at(t, set, "/notCreated/x/type"), `"overQuota"`)
	// And an upload the account has no room for.
	rec := e.post("/jmap/upload/"+e.acct.ID+"/", make([]byte, 6000), "text/plain")
	if rec.Code != 507 {
		t.Errorf("an upload past the quota: %d %s", rec.Code, rec.Body)
	}
}
