package jmap

import (
	"context"
	"strings"
	"testing"

	"github.com/robinmail/robinmail/internal/sieve"
	"github.com/robinmail/robinmail/internal/store"
)

// doSieve runs calls with the Sieve capability in "using".
func (e *env) sieve(name, args string) map[string]any {
	e.t.Helper()
	got := e.doWith(`"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:sieve"],"methodCalls":[["` + name + `", ` + args + `, "c"]]`)
	if at(e.t, got, "/0/0") != name {
		e.t.Fatalf("%s %s answered %s", name, args, jsonOf(got))
	}
	return at(e.t, got, "/0/1").(map[string]any)
}

func (e *env) sieveFail(name, args string) string {
	e.t.Helper()
	got := e.doWith(`"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:sieve"],"methodCalls":[["` + name + `", ` + args + `, "c"]]`)
	if at(e.t, got, "/0/0") != "error" {
		e.t.Fatalf("%s %s did not fail: %s", name, args, jsonOf(got))
	}
	return at(e.t, got, "/0/1/type").(string)
}

// script stores a blob the way POST /jmap/upload does and returns its id.
func (e *env) script(content string) string {
	e.t.Helper()
	id, err := e.st.Upload(context.Background(), e.acct.ID, []byte(content), "application/sieve")
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// RFC 9661 1.3: the capability and its account properties.
func TestRFC9661_1_3_Capability(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/jmap/session")
	session := decode(t, rec.Body.Bytes())
	// "implementation: String. The name and, optionally, version of the
	// Sieve implementation."
	if impl, _ := at(t, session, "/capabilities/urn:ietf:params:jmap:sieve/implementation").(string); impl == "" {
		t.Error("no implementation in the capability")
	}
	cap := at(t, session, "/accounts/"+e.acct.ID+"/accountCapabilities/urn:ietf:params:jmap:sieve").(map[string]any)
	// "maxSizeScriptName ... MUST be at least 512".
	if n, _ := cap["maxSizeScriptName"].(float64); n < 512 {
		t.Errorf("maxSizeScriptName %v", cap["maxSizeScriptName"])
	}
	same(t, "limits", []any{cap["maxSizeScript"], cap["maxNumberScripts"], cap["maxNumberRedirects"], cap["notificationMethods"], cap["externalLists"]},
		`[65536, 16, 4, null, null]`)
	// "sieveExtensions: A list of case-sensitive Sieve capability strings
	// (as listed in the Sieve "require" action)". Each is one the
	// interpreter takes.
	exts, _ := cap["sieveExtensions"].([]any)
	if len(exts) != len(sieve.Extensions) {
		t.Fatalf("sieveExtensions: %v", exts)
	}
	for _, x := range exts {
		if _, err := sieve.Parse([]byte(`require "` + x.(string) + `";`)); err != nil {
			t.Errorf("the session names %v, which the interpreter refuses: %v", x, err)
		}
	}
	same(t, "primary account", at(t, session, "/primaryAccounts/urn:ietf:params:jmap:sieve"), `"`+e.acct.ID+`"`)
	// Without the capability in "using" the methods do not exist.
	if got := e.fail("SieveScript/get", `{"accountId": "$A", "ids": null}`); got != "unknownMethod" {
		t.Errorf("SieveScript/get without the capability: %s", got)
	}
}

// RFC 9661 2.1 and 2.2: SieveScript/get and /set.
func TestRFC9661_2_GetAndSet(t *testing.T) {
	e := newEnv(t)
	good := e.script("require [\"fileinto\"];\r\nif header :contains \"subject\" \"invoice\" { fileinto \"Work\"; }\r\n")
	other := e.script("discard;\r\n")
	bad := e.script("if header :contains \"subject\" { fileinto \"Work\"; }\r\n")
	unsupported := e.script("require \"editheader\";\r\nkeep;\r\n")

	// Create. "name: String|null ... the server assigns one if null".
	got := e.sieve("SieveScript/set", `{"accountId": "$A", "create": {
		"a": {"name": "Filters", "blobId": "`+good+`"},
		"b": {"name": null, "blobId": "`+other+`"},
		"dup": {"name": "Filters", "blobId": "`+other+`"},
		"bad": {"name": "bad", "blobId": "`+bad+`"},
		"ext": {"name": "ext", "blobId": "`+unsupported+`"},
		"noblob": {"name": "x", "blobId": "`+strings.Repeat("0", 64)+`"},
		"nothing": {"name": "x"},
		"active": {"name": "y", "blobId": "`+other+`", "isActive": true},
		"ctl": {"name": "line\nbreak", "blobId": "`+other+`"}}}`)
	same(t, "created", got["created"], `{"a": {"id": "s1", "isActive": false}, "b": {"id": "s2", "isActive": false, "name": "Script 1"}}`)
	// "invalidSieve: The SieveScript content violates the Sieve grammar
	// [RFC5228], and/or one or more extensions mentioned in the script's
	// "require" statement(s) are not supported by the Sieve interpreter."
	same(t, "notCreated", []any{at(t, got, "/notCreated/dup/type"), at(t, got, "/notCreated/bad/type"), at(t, got, "/notCreated/ext/type"),
		at(t, got, "/notCreated/noblob/type"), at(t, got, "/notCreated/nothing/properties"), at(t, got, "/notCreated/active/properties"),
		at(t, got, "/notCreated/ctl/properties")},
		`["alreadyExists", "invalidSieve", "invalidSieve", "blobNotFound", ["blobId"], ["isActive"], ["name"]]`)
	if desc, _ := at(t, got, "/notCreated/bad/description").(string); !strings.Contains(desc, "line 1") {
		t.Errorf("an invalidSieve error should say where: %q", desc)
	}

	// Get: all, some, and the properties asked for.
	all := e.sieve("SieveScript/get", `{"accountId": "$A", "ids": null}`)
	same(t, "get all", all["list"], `[{"id": "s1", "name": "Filters", "blobId": "`+good+`", "isActive": false},
		{"id": "s2", "name": "Script 1", "blobId": "`+other+`", "isActive": false}]`)
	some := e.sieve("SieveScript/get", `{"accountId": "$A", "ids": ["s2", "s9", "nonsense"], "properties": ["name"]}`)
	same(t, "get some", []any{some["list"], some["notFound"]}, `[[{"id": "s2", "name": "Script 1"}], ["s9", "nonsense"]]`)
	// The script is a blob that can be downloaded.
	if rec := e.get("/jmap/download/" + e.acct.ID + "/" + good + "/filters.sieve?type=application/sieve"); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `fileinto "Work"`) {
		t.Errorf("download of a script: %d %q", rec.Code, rec.Body.String())
	}

	// "onSuccessActivateScript: Id ... The id of the SieveScript to
	// activate if and only if all of the creations, modifications, and
	// destructions (if any) succeed. ... The currently active script (if
	// any) will be deactivated before activating the specified script."
	got = e.sieve("SieveScript/set", `{"accountId": "$A", "onSuccessActivateScript": "s1"}`)
	same(t, "activate", got["updated"], `{"s1": {"isActive": true}}`)
	// A creation id, with "#": the new script takes over.
	third := e.script("keep;\r\n")
	got = e.sieve("SieveScript/set", `{"accountId": "$A", "create": {"new": {"name": "Third", "blobId": "`+third+`"}}, "onSuccessActivateScript": "#new"}`)
	same(t, "create and activate", []any{got["created"], got["updated"]}, `[{"new": {"id": "s3", "isActive": true}}, {"s1": {"isActive": false}}]`)
	active := e.sieve("SieveScript/query", `{"accountId": "$A", "filter": {"isActive": true}}`)
	same(t, "one active script", active["ids"], `["s3"]`)
	// Not when something in the call failed.
	got = e.sieve("SieveScript/set", `{"accountId": "$A", "update": {"s9": {"name": "x"}}, "onSuccessActivateScript": "s2"}`)
	same(t, "no activation after a failure", []any{at(t, got, "/notUpdated/s9/type"), got["updated"]}, `["notFound", null]`)
	same(t, "still the third", e.sieve("SieveScript/query", `{"accountId": "$A", "filter": {"isActive": true}}`)["ids"], `["s3"]`)

	// "sieveIsActive: The SieveScript is active" when it is destroyed.
	got = e.sieve("SieveScript/set", `{"accountId": "$A", "destroy": ["s3", "s2", "s9"]}`)
	same(t, "destroy", []any{got["destroyed"], at(t, got, "/notDestroyed/s3/type"), at(t, got, "/notDestroyed/s9/type")}, `[["s2"], "sieveIsActive", "notFound"]`)

	// Update: another name, other content; the id stays, isActive stays.
	got = e.sieve("SieveScript/set", `{"accountId": "$A", "update": {
		"s3": {"name": "Renamed", "blobId": "`+other+`"},
		"s1": {"blobId": "`+bad+`"}}}`)
	same(t, "update", []any{got["updated"], at(t, got, "/notUpdated/s1/type")}, `[{"s3": null}, "invalidSieve"]`)
	same(t, "after the update", e.sieve("SieveScript/get", `{"accountId": "$A", "ids": ["s3"]}`)["list"],
		`[{"id": "s3", "name": "Renamed", "blobId": "`+other+`", "isActive": true}]`)

	// "onSuccessDeactivateScript: Boolean ... If true, the currently
	// active script (if any) will be deactivated", and activating wins
	// when both are given.
	got = e.sieve("SieveScript/set", `{"accountId": "$A", "onSuccessActivateScript": "s1", "onSuccessDeactivateScript": true}`)
	same(t, "both", got["updated"], `{"s1": {"isActive": true}, "s3": {"isActive": false}}`)
	got = e.sieve("SieveScript/set", `{"accountId": "$A", "onSuccessDeactivateScript": true}`)
	same(t, "deactivate", got["updated"], `{"s1": {"isActive": false}}`)
	same(t, "none active", e.sieve("SieveScript/query", `{"accountId": "$A", "filter": {"isActive": true}}`)["ids"], `[]`)
	// ifInState, and what is not a script id.
	for args, want := range map[string]string{
		`{"accountId": "$A", "ifInState": "1", "destroy": ["s1"]}`:   "stateMismatch",
		`{"accountId": "$A", "onSuccessActivateScript": "s9"}`:       "invalidArguments",
		`{"accountId": "$A", "onSuccessActivateScript": "#nothing"}`: "invalidArguments",
		`{"accountId": "$A", "onSuccessActivateScript": "e1"}`:       "invalidArguments",
		`{"accountId": "nope"}`:                                      "accountNotFound",
	} {
		if got := e.sieveFail("SieveScript/set", args); got != want {
			t.Errorf("SieveScript/set %s: %s, want %s", args, got, want)
		}
	}
	// A script over the size limit.
	huge := e.script("#" + strings.Repeat("x", sieve.MaxScriptSize) + "\r\nkeep;\r\n")
	got = e.sieve("SieveScript/set", `{"accountId": "$A", "create": {"h": {"name": "huge", "blobId": "`+huge+`"}}}`)
	same(t, "too large", at(t, got, "/notCreated/h/type"), `"tooLarge"`)
}

// RFC 9661 2.3: SieveScript/query, and 2.4: SieveScript/validate.
func TestRFC9661_2_3_QueryAndValidate(t *testing.T) {
	e := newEnv(t)
	blob := e.script("keep;\r\n")
	e.sieve("SieveScript/set", `{"accountId": "$A", "create": {
		"a": {"name": "beta", "blobId": "`+blob+`"}, "b": {"name": "Alpha", "blobId": "`+blob+`"}, "c": {"name": "gamma filters", "blobId": "`+blob+`"}},
		"onSuccessActivateScript": "#c"}`)
	ids := func(args string) any {
		t.Helper()
		return e.sieve("SieveScript/query", `{"accountId": "$A"`+args+`}`)["ids"]
	}
	same(t, "all", ids(``), `["s1", "s2", "s3"]`)
	// "name: String. The SieveScript "name" property contains the given string."
	same(t, "name", ids(`, "filter": {"name": "filter"}`), `["s3"]`)
	same(t, "isActive", ids(`, "filter": {"isActive": false}`), `["s1", "s2"]`)
	same(t, "operators", ids(`, "filter": {"operator": "NOT", "conditions": [{"name": "a"}]}`), `[]`)
	same(t, "OR", ids(`, "filter": {"operator": "OR", "conditions": [{"name": "alpha"}, {"isActive": true}]}`), `["s2", "s3"]`)
	// "The following SieveScript properties MUST be supported for sorting: name, isActive".
	same(t, "by name", ids(`, "sort": [{"property": "name"}]`), `["s2", "s1", "s3"]`)
	same(t, "by name, descending", ids(`, "sort": [{"property": "name", "isAscending": false}]`), `["s3", "s1", "s2"]`)
	same(t, "active first", ids(`, "sort": [{"property": "isActive", "isAscending": false}, {"property": "name"}]`), `["s3", "s2", "s1"]`)
	page := e.sieve("SieveScript/query", `{"accountId": "$A", "position": 1, "limit": 1, "calculateTotal": true}`)
	same(t, "window", []any{page["ids"], page["position"], page["total"]}, `[["s2"], 1, 3]`)
	for args, want := range map[string]string{
		`{"accountId": "$A", "filter": {"colour": "red"}}`:      "unsupportedFilter",
		`{"accountId": "$A", "filter": {"name": 1}}`:            "invalidArguments",
		`{"accountId": "$A", "sort": [{"property": "blobId"}]}`: "unsupportedSort",
	} {
		if got := e.sieveFail("SieveScript/query", args); got != want {
			t.Errorf("SieveScript/query %s: %s, want %s", args, got, want)
		}
	}

	// 2.4: "error: SetError|null. An "invalidSieve" SetError object if the
	// script content is invalid ... or null if the script content is valid."
	good := e.sieve("SieveScript/validate", `{"accountId": "$A", "blobId": "`+blob+`"}`)
	same(t, "valid", good["error"], `null`)
	bad := e.sieve("SieveScript/validate", `{"accountId": "$A", "blobId": "`+e.script("keep")+`"}`)
	same(t, "invalid", at(t, bad, "/error/type"), `"invalidSieve"`)
	missing := e.sieve("SieveScript/validate", `{"accountId": "$A", "blobId": "`+strings.Repeat("a", 64)+`"}`)
	same(t, "no blob", at(t, missing, "/error/type"), `"blobNotFound"`)
	if got := e.sieveFail("SieveScript/validate", `{"accountId": "$A"}`); got != "invalidArguments" {
		t.Errorf("validate without a blob: %s", got)
	}
	// Nothing was stored by validating.
	same(t, "still three", ids(``), `["s1", "s2", "s3"]`)

	// The scripts are the account's: the state moved, and a change is
	// what push reports as SieveScript.
	c, err := e.st.Changes(e.ctx, e.acct.ID, 1)
	if err != nil || c.Other["SieveScript"] == nil || len(c.Other["SieveScript"].Created) != 3 {
		t.Errorf("changes: %+v %v", c.Other, err)
	}
	_ = store.SieveScriptID
}
