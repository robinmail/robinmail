package jmap

import (
	"testing"
)

func (e *env) vacation(name, args string) map[string]any {
	e.t.Helper()
	got := e.doWith(`"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:vacationresponse"],"methodCalls":[["` + name + `", ` + args + `, "c"]]`)
	if at(e.t, got, "/0/0") != name {
		e.t.Fatalf("%s %s answered %s", name, args, jsonOf(got))
	}
	return at(e.t, got, "/0/1").(map[string]any)
}

// RFC 8621 8: the VacationResponse object, /get (8.1) and /set (8.2).
func TestRFC8621_8_VacationResponse(t *testing.T) {
	e := newEnv(t)
	// 1.3.3: the capability, "an empty object in both the account's
	// accountCapabilities property and the capabilities property".
	session := decode(t, e.get("/jmap/session").Body.Bytes())
	same(t, "capability", []any{at(t, session, "/capabilities/urn:ietf:params:jmap:vacationresponse"),
		at(t, session, "/accounts/"+e.acct.ID+"/accountCapabilities/urn:ietf:params:jmap:vacationresponse")}, `[{}, {}]`)
	if got := e.fail("VacationResponse/get", `{"accountId": "$A", "ids": null}`); got != "unknownMethod" {
		t.Errorf("without the capability in using: %s", got)
	}

	// "There MUST only be exactly one VacationResponse object in an
	// account. It MUST have the id "singleton"." Before anything was set:
	// not enabled, everything else null.
	got := e.vacation("VacationResponse/get", `{"accountId": "$A", "ids": null}`)
	same(t, "the singleton", got["list"], `[{"id": "singleton", "isEnabled": false, "fromDate": null, "toDate": null,
		"subject": null, "textBody": null, "htmlBody": null}]`)
	got = e.vacation("VacationResponse/get", `{"accountId": "$A", "ids": ["singleton", "other"], "properties": ["isEnabled"]}`)
	same(t, "by id", []any{got["list"], got["notFound"]}, `[[{"id": "singleton", "isEnabled": false}], ["other"]]`)

	// 8.2: update.
	before := got["state"].(string)
	set := e.vacation("VacationResponse/set", `{"accountId": "$A", "ifInState": "`+before+`", "update": {"singleton": {
		"isEnabled": true, "fromDate": "2026-12-24T00:00:00Z", "toDate": "2027-01-02T00:00:00Z",
		"subject": "Away", "textBody": "Back in January.\nMike", "htmlBody": "<p>Back in January.</p>"}}}`)
	same(t, "updated", []any{set["updated"], set["oldState"]}, `[{"singleton": null}, "`+before+`"]`)
	got = e.vacation("VacationResponse/get", `{"accountId": "$A", "ids": ["singleton"]}`)
	same(t, "read back", got["list"], `[{"id": "singleton", "isEnabled": true, "fromDate": "2026-12-24T00:00:00Z",
		"toDate": "2027-01-02T00:00:00Z", "subject": "Away", "textBody": "Back in January.\nMike", "htmlBody": "<p>Back in January.</p>"}]`)
	if got["state"] == before {
		t.Error("the state did not move")
	}
	// A patch changes what it names and leaves the rest.
	e.vacation("VacationResponse/set", `{"accountId": "$A", "update": {"singleton": {"isEnabled": false, "toDate": null, "htmlBody": null}}}`)
	got = e.vacation("VacationResponse/get", `{"accountId": "$A", "ids": ["singleton"]}`)
	same(t, "after a patch", got["list"], `[{"id": "singleton", "isEnabled": false, "fromDate": "2026-12-24T00:00:00Z",
		"toDate": null, "subject": "Away", "textBody": "Back in January.\nMike", "htmlBody": null}]`)

	// What cannot be set, and RFC 8620 5.3: "singleton: This is a
	// singleton type, so you cannot create another one or destroy the
	// existing one."
	bad := e.vacation("VacationResponse/set", `{"accountId": "$A",
		"create": {"new": {"isEnabled": true}},
		"update": {"other": {"isEnabled": true}},
		"destroy": ["singleton", "other"]}`)
	same(t, "refusals", []any{at(t, bad, "/notCreated/new/type"), at(t, bad, "/notUpdated/other/type"),
		at(t, bad, "/notDestroyed/singleton/type"), at(t, bad, "/notDestroyed/other/type"), bad["updated"]},
		`["singleton", "notFound", "singleton", "notFound", null]`)
	for patch, props := range map[string]string{
		`{"isEnabled": "yes"}`:                      `["isEnabled"]`,
		`{"isEnabled": null}`:                       `["isEnabled"]`,
		`{"fromDate": "2026-12-24"}`:                `["fromDate"]`,
		`{"fromDate": "2026-12-24T00:00:00+01:00"}`: `["fromDate"]`, // a UTCDate ends in Z
		`{"toDate": 5}`:                             `["toDate"]`,
		`{"subject": "two\nlines"}`:                 `["subject"]`,
		`{"textBody": 1, "colour": "red"}`:          `["colour", "textBody"]`,
		`{"id": "other"}`:                           `["id"]`,
	} {
		res := e.vacation("VacationResponse/set", `{"accountId": "$A", "update": {"singleton": `+patch+`}}`)
		same(t, patch, []any{at(t, res, "/notUpdated/singleton/type"), at(t, res, "/notUpdated/singleton/properties")}, `["invalidProperties", `+props+`]`)
	}
	// The id as it is may be given (RFC 8620 5.3).
	ok := e.vacation("VacationResponse/set", `{"accountId": "$A", "update": {"singleton": {"id": "singleton", "isEnabled": true}}}`)
	same(t, "id unchanged", ok["updated"], `{"singleton": null}`)
	got = e.doWith(`"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:vacationresponse"],"methodCalls":[["VacationResponse/set", {"accountId": "$A", "ifInState": "1", "update": {"singleton": {"isEnabled": false}}}, "c"]]`)[0].([]any)[1].(map[string]any)
	same(t, "ifInState", got["type"], `"stateMismatch"`)

	// Push names the type (RFC 8621 1.5).
	c, err := e.st.Changes(e.ctx, e.acct.ID, mustState(t, before))
	if err != nil || c.Other["VacationResponse"] == nil {
		t.Errorf("changes: %+v, %v", c.Other, err)
	}
}
