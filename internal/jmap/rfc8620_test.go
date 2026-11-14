package jmap

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Tests of the core protocol, written from RFC 8620 and using its examples.
// They need no storage: Core/echo is enough to carry a request through.

var testUser = User{AccountID: "a1b2c3", Username: "user@example.test"}

// post sends a body to the API endpoint as testUser.
func post(t *testing.T, h *Handler, contentType, body string) (int, http.Header, []byte) {
	t.Helper()
	req := httptest.NewRequest("POST", "https://mail.example.test/jmap/api", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req = req.WithContext(WithUser(req.Context(), testUser))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, rec.Result().Header, out
}

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, data)
	}
	return v
}

// at reads a value out of decoded JSON with a JSON Pointer.
func at(t *testing.T, v any, path string) any {
	t.Helper()
	budget := maxPathItems
	out, ok := evalPointer(v, path, &budget)
	if !ok {
		b, _ := json.Marshal(v)
		t.Fatalf("no %s in %s", path, b)
	}
	return out
}

// same compares a value with JSON text.
func same(t *testing.T, name string, got any, wantJSON string) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("%s: bad expectation %s: %v", name, wantJSON, err)
	}
	// Through JSON, so Go types (int64, []string) compare as JSON does.
	b, _ := json.Marshal(got)
	var g any
	json.Unmarshal(b, &g)
	if !reflect.DeepEqual(g, want) {
		t.Errorf("%s:\n got %s\nwant %s", name, b, wantJSON)
	}
}

// responses runs a request that must succeed and returns methodResponses.
func responses(t *testing.T, h *Handler, body string) []any {
	t.Helper()
	status, hdr, out := post(t, h, "application/json", body)
	if status != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("status %d, type %q: %s", status, hdr.Get("Content-Type"), out)
	}
	return at(t, decode(t, out), "/methodResponses").([]any)
}

// RFC 8620 3.3 and 3.4: the Request and Response objects. The example of
// 3.3.1 has three calls with client-chosen ids; each response carries the
// name, the arguments and the id of its call, in the order of the calls.
// The echo of section 4 stands in for the RFC's "method1".
func TestRFC8620RequestAndResponse(t *testing.T) {
	h := &Handler{}
	status, _, out := post(t, h, "application/json", `{
		"using": [ "urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail" ],
		"methodCalls": [
			[ "Core/echo", { "arg1": "arg1data", "arg2": "arg2data" }, "c1" ],
			[ "Core/echo", { "arg1": "arg1data" }, "c2" ],
			[ "Core/echo", {}, "c3" ]
		]
	}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, out)
	}
	resp := decode(t, out)
	same(t, "methodResponses", resp["methodResponses"], `[
		[ "Core/echo", { "arg1": "arg1data", "arg2": "arg2data" }, "c1" ],
		[ "Core/echo", { "arg1": "arg1data" }, "c2" ],
		[ "Core/echo", {}, "c3" ]
	]`)
	// 3.4: sessionState is always there; createdIds only "if a createdIds
	// property was included in the request".
	if s, _ := resp["sessionState"].(string); s == "" {
		t.Error("no sessionState")
	}
	if _, ok := resp["createdIds"]; ok {
		t.Error("createdIds in a response to a request without it")
	}
	_, _, out = post(t, h, "application/json", `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[],"createdIds":{"k1":"m5"}}`)
	same(t, "createdIds", decode(t, out)["createdIds"], `{"k1":"m5"}`)
}

// RFC 8620 4: the Core/echo example.
func TestRFC8620CoreEcho(t *testing.T) {
	got := responses(t, &Handler{}, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
		[ "Core/echo", { "hello": true, "high": 5 }, "b3ff" ]]}`)
	same(t, "echo", got, `[[ "Core/echo", { "hello": true, "high": 5 }, "b3ff" ]]`)
}

// RFC 8620 3.6.1: request-level errors are problem details (RFC 7807) with
// an HTTP error status. Its examples: an unknown capability, and a limit
// with the name of the limit.
func TestRFC8620RequestErrors(t *testing.T) {
	h := &Handler{Limits: Limits{MaxSizeRequest: 2000, MaxCallsInRequest: 3}}
	for _, tc := range []struct {
		name, contentType, body string
		wantType, wantLimit     string
	}{
		{"unknown capability", "application/json",
			`{"using":["urn:ietf:params:jmap:core","https://example.com/apis/foobar"],"methodCalls":[]}`,
			"urn:ietf:params:jmap:error:unknownCapability", ""},
		{"not JSON", "application/json", `{"using":[`, "urn:ietf:params:jmap:error:notJSON", ""},
		{"two values", "application/json", `{} {}`, "urn:ietf:params:jmap:error:notJSON", ""},
		{"invalid UTF-8", "application/json", "{\"using\":[\"\xff\"],\"methodCalls\":[]}", "urn:ietf:params:jmap:error:notJSON", ""},
		{"wrong content type", "text/plain", `{"using":[],"methodCalls":[]}`, "urn:ietf:params:jmap:error:notJSON", ""},
		{"no content type", "", `{"using":[],"methodCalls":[]}`, "urn:ietf:params:jmap:error:notJSON", ""},
		{"not an object", "application/json", `[1,2,3]`, "urn:ietf:params:jmap:error:notRequest", ""},
		{"no using", "application/json", `{"methodCalls":[]}`, "urn:ietf:params:jmap:error:notRequest", ""},
		{"no methodCalls", "application/json", `{"using":[]}`, "urn:ietf:params:jmap:error:notRequest", ""},
		{"using is not a list", "application/json", `{"using":"x","methodCalls":[]}`, "urn:ietf:params:jmap:error:notRequest", ""},
		// Member names are case-sensitive.
		{"wrong case", "application/json", `{"using":[],"methodCAlls":[]}`, "urn:ietf:params:jmap:error:notRequest", ""},
		{"call of two elements", "application/json", `{"using":[],"methodCalls":[["Core/echo",{}]]}`,
			"urn:ietf:params:jmap:error:notRequest", ""},
		{"arguments are not an object", "application/json", `{"using":[],"methodCalls":[["Core/echo",[],"c1"]]}`,
			"urn:ietf:params:jmap:error:notRequest", ""},
		{"call id is not a string", "application/json", `{"using":[],"methodCalls":[["Core/echo",{},1]]}`,
			"urn:ietf:params:jmap:error:notRequest", ""},
		{"maxCallsInRequest", "application/json",
			`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"1"],["Core/echo",{},"2"],["Core/echo",{},"3"],["Core/echo",{},"4"]]}`,
			"urn:ietf:params:jmap:error:limit", "maxCallsInRequest"},
		{"maxSizeRequest", "application/json",
			`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"x":"` + strings.Repeat("a", 2000) + `"},"1"]]}`,
			"urn:ietf:params:jmap:error:limit", "maxSizeRequest"},
	} {
		status, hdr, out := post(t, h, tc.contentType, tc.body)
		if status != 400 || hdr.Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s: status %d, type %q: %s", tc.name, status, hdr.Get("Content-Type"), out)
			continue
		}
		p := decode(t, out)
		limit, _ := p["limit"].(string)
		if p["type"] != tc.wantType || p["status"] != float64(400) || limit != tc.wantLimit {
			t.Errorf("%s: %s", tc.name, out)
		}
	}
	// Exactly at the limits is fine.
	if status, _, out := post(t, h, "application/json; charset=utf-8",
		`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"1"],["Core/echo",{},"2"],["Core/echo",{},"3"]]}`); status != 200 {
		t.Errorf("three calls: %d %s", status, out)
	}
}

// RFC 8620 3.6.1, maxConcurrentRequests: a request that comes while the
// account already has that many running is refused with the limit error.
func TestRFC8620MaxConcurrentRequests(t *testing.T) {
	h := &Handler{Limits: Limits{MaxConcurrentRequests: 2}}
	h.init()
	if !h.acquire(testUser.AccountID) || !h.acquire(testUser.AccountID) {
		t.Fatal("could not take the two slots")
	}
	status, _, out := post(t, h, "application/json", `{"using":[],"methodCalls":[]}`)
	p := decode(t, out)
	if status != 400 || p["type"] != "urn:ietf:params:jmap:error:limit" || p["limit"] != "maxConcurrentRequests" {
		t.Errorf("third request: %d %s", status, out)
	}
	// Another account is not affected.
	if !h.acquire("other") {
		t.Error("another account was refused")
	}
	h.release("other")
	h.release(testUser.AccountID)
	if status, _, _ := post(t, h, "application/json", `{"using":[],"methodCalls":[]}`); status != 200 {
		t.Errorf("after a slot came free: %d", status)
	}
	h.release(testUser.AccountID)

	// Requests in parallel never exceed the limit and all get an answer.
	var wg sync.WaitGroup
	codes := make([]int, 40)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _, _ = post(t, h, "application/json", `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"1"]]}`)
		}()
	}
	wg.Wait()
	for _, code := range codes {
		if code != 200 && code != 400 {
			t.Errorf("status %d", code)
		}
	}
	if len(h.inFlight) != 0 {
		t.Errorf("slots still taken: %v", h.inFlight)
	}
}

// RFC 8620 3.6.2: a method-level error is an "error" response with the
// call's id; its example is unknownMethod. The other calls still run.
// 3.3: a method belongs to a capability, and is only there for a request
// that names the capability in "using".
func TestRFC8620MethodErrors(t *testing.T) {
	h := &Handler{}
	got := responses(t, h, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
		["Core/echo", {"n": 1}, "c1"],
		["Foo/bar", {}, "call-id"],
		["Mailbox/get", {"accountId": "a1b2c3"}, "c3"],
		["Core/echo", {"n": 2}, "c4"]]}`)
	if len(got) != 4 {
		t.Fatalf("%d responses", len(got))
	}
	same(t, "first", got[0], `["Core/echo", {"n": 1}, "c1"]`)
	same(t, "unknown method", []any{at(t, got, "/1/0"), at(t, got, "/1/1/type"), at(t, got, "/1/2")}, `["error", "unknownMethod", "call-id"]`)
	// Mailbox/get exists, but the request does not use the mail capability.
	same(t, "capability not used", []any{at(t, got, "/2/0"), at(t, got, "/2/1/type"), at(t, got, "/2/2")}, `["error", "unknownMethod", "c3"]`)
	same(t, "last", got[3], `["Core/echo", {"n": 2}, "c4"]`)

	// And the other way around: Core/echo without the core capability.
	got = responses(t, h, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[["Core/echo", {}, "c1"]]}`)
	same(t, "core not used", at(t, got, "/0/1/type"), `"unknownMethod"`)
}

// RFC 8620 3.7: the evaluation of a result reference path. The first block
// is the RFC's own example: the response to Email/get, and the path
// "/list/*/threadId" applied to it.
func TestRFC8620PointerEvaluation(t *testing.T) {
	var response any
	json.Unmarshal([]byte(`{
		"accountId": "A1",
		"queryState": "abcdefg",
		"canCalculateChanges": true,
		"position": 0,
		"total": 101,
		"ids": [ "msg1023", "msg223", "msg110", "msg93", "msg91", "msg38", "msg36", "msg33", "msg11", "msg1" ],
		"list": [
			{ "id": "msg1023", "threadId": "trd194" },
			{ "id": "msg223", "threadId": "trd114" },
			{ "id": "trd194", "emailIds": [ "msg1020", "msg1021", "msg1023" ] },
			{ "id": "trd114", "emailIds": [ "msg201", "msg223" ] }
		],
		"a/b": {"m~n": [1, [2, 3]]},
		"": "empty key"
	}`), &response)
	for _, tc := range []struct{ path, want string }{
		{"/ids", `[ "msg1023", "msg223", "msg110", "msg93", "msg91", "msg38", "msg36", "msg33", "msg11", "msg1" ]`},
		{"/ids/0", `"msg1023"`},
		{"/ids/9", `"msg1"`},
		{"/total", `101`},
		{"/list/1/threadId", `"trd114"`},
		// "*" maps over the array; a result that is itself an array is
		// flattened into the output ("/list/*/emailIds" in the RFC).
		{"/list/2/emailIds", `[ "msg1020", "msg1021", "msg1023" ]`},
		{"/list/*/id", `[ "msg1023", "msg223", "trd194", "trd114" ]`},
		{"", ``}, // the whole document; compared below
		// RFC 6901 3: "~1" is "/", "~0" is "~".
		{"/a~1b/m~0n/0", `1`},
		{"/a~1b/m~0n/*", `[1, 2, 3]`},
		{"/", `"empty key"`},
	} {
		budget := maxPathItems
		got, ok := evalPointer(response, tc.path, &budget)
		if !ok {
			t.Errorf("%q does not resolve", tc.path)
			continue
		}
		if tc.path == "" {
			if !reflect.DeepEqual(got, response) {
				t.Error(`"" is not the whole document`)
			}
			continue
		}
		same(t, tc.path, got, tc.want)
	}
	for _, path := range []string{
		"ids",              // must start with "/"
		"/nothing",         // no such member
		"/ids/10",          // past the end
		"/ids/-1",          // RFC 6901 "-" is past the end; not a member
		"/ids/-",           // same
		"/ids/01",          // leading zero
		"/ids/1e0",         // not digits
		"/ids/0/x",         // into a string
		"/list/*/threadId", // some items have no threadId: the whole evaluation fails
		"/total/*",         // "*" on something that is not an array
		"/accountId/0",
	} {
		budget := maxPathItems
		if got, ok := evalPointer(response, path, &budget); ok {
			t.Errorf("%q resolved to %v", path, got)
		}
	}
}

// RFC 8620 3.7 end to end: an argument "#name" is replaced by what its
// ResultReference points at before the method runs. The data is that of
// the RFC's example; Core/echo plays the methods.
func TestRFC8620ResultReferences(t *testing.T) {
	h := &Handler{}
	got := responses(t, h, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
		["Core/echo", {"ids": ["msg1023", "msg223"], "list": [
			{"id": "msg1023", "threadId": "trd194"}, {"id": "msg223", "threadId": "trd114"}]}, "t0"],
		["Core/echo", {"#ids": {"resultOf": "t0", "name": "Core/echo", "path": "/ids"}, "properties": ["threadId"]}, "t1"],
		["Core/echo", {"#ids": {"resultOf": "t0", "name": "Core/echo", "path": "/list/*/threadId"}}, "t2"],
		["Core/echo", {"#ids": {"resultOf": "t2", "name": "Core/echo", "path": "/ids"}}, "t3"]]}`)
	same(t, "t1", got[1], `["Core/echo", {"ids": ["msg1023", "msg223"], "properties": ["threadId"]}, "t1"]`)
	same(t, "t2", got[2], `["Core/echo", {"ids": ["trd194", "trd114"]}, "t2"]`)
	// A reference to a response that itself came from a reference.
	same(t, "t3", got[3], `["Core/echo", {"ids": ["trd194", "trd114"]}, "t3"]`)

	for _, tc := range []struct{ name, calls, wantType string }{
		// "If an argument object contains the same argument name in normal
		// and referenced form ... the method MUST return an
		// invalidArguments error."
		{"both forms", `["Core/echo", {"ids": ["a"]}, "t0"],
			["Core/echo", {"ids": ["b"], "#ids": {"resultOf": "t0", "name": "Core/echo", "path": "/ids"}}, "t1"]`, "invalidArguments"},
		// "If any of the following steps fail ... invalidResultReference".
		{"no such call", `["Core/echo", {"ids": ["a"]}, "t0"],
			["Core/echo", {"#ids": {"resultOf": "nope", "name": "Core/echo", "path": "/ids"}}, "t1"]`, "invalidResultReference"},
		{"a later call", `["Core/echo", {"#ids": {"resultOf": "t1", "name": "Core/echo", "path": "/ids"}}, "t0"],
			["Core/echo", {"ids": ["a"]}, "t1"]`, "invalidResultReference"},
		{"its own call", `["Core/echo", {"#ids": {"resultOf": "t0", "name": "Core/echo", "path": "/ids"}}, "t0"]`, "invalidResultReference"},
		{"another name", `["Core/echo", {"ids": ["a"]}, "t0"],
			["Core/echo", {"#ids": {"resultOf": "t0", "name": "Email/query", "path": "/ids"}}, "t1"]`, "invalidResultReference"},
		// The response to a call that failed is named "error".
		{"a failed call", `["Foo/bar", {}, "t0"],
			["Core/echo", {"#ids": {"resultOf": "t0", "name": "Foo/bar", "path": "/ids"}}, "t1"]`, "invalidResultReference"},
		{"path does not resolve", `["Core/echo", {"ids": ["a"]}, "t0"],
			["Core/echo", {"#ids": {"resultOf": "t0", "name": "Core/echo", "path": "/list"}}, "t1"]`, "invalidResultReference"},
		{"not a reference", `["Core/echo", {"ids": ["a"]}, "t0"], ["Core/echo", {"#ids": ["a"]}, "t1"]`, "invalidResultReference"},
		{"path missing", `["Core/echo", {"ids": ["a"]}, "t0"],
			["Core/echo", {"#ids": {"resultOf": "t0", "name": "Core/echo"}}, "t1"]`, "invalidResultReference"},
		{"extra member", `["Core/echo", {"ids": ["a"]}, "t0"],
			["Core/echo", {"#ids": {"resultOf": "t0", "name": "Core/echo", "path": "/ids", "x": 1}}, "t1"]`, "invalidResultReference"},
	} {
		got := responses(t, h, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[`+tc.calls+`]}`)
		// The call with the reference is the one that fails.
		failed := 0
		for _, r := range got {
			if at(t, r, "/0") == "error" && at(t, r, "/1/type") == tc.wantType {
				failed++
			}
		}
		if failed != 1 {
			b, _ := json.Marshal(got)
			t.Errorf("%s: %s", tc.name, b)
		}
	}

	// The first response with the call id counts: ids are the client's and
	// may repeat.
	got = responses(t, h, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
		["Core/echo", {"ids": ["first"]}, "same"], ["Core/echo", {"ids": ["second"]}, "same"],
		["Core/echo", {"#ids": {"resultOf": "same", "name": "Core/echo", "path": "/ids"}}, "t2"]]}`)
	same(t, "repeated call id", at(t, got, "/2/1/ids"), `["first"]`)
}

// A "*" path over a large response cannot be used to make the server build
// something far larger than the request.
func TestResultReferenceBudget(t *testing.T) {
	var big strings.Builder
	big.WriteString(`{"list":[`)
	for i := 0; i < 400; i++ {
		if i > 0 {
			big.WriteByte(',')
		}
		big.WriteString(`[` + strings.Repeat(`[1,2,3,4,5,6,7,8,9,0],`, 30) + `[1]]`)
	}
	big.WriteString(`]}`)
	h := &Handler{}
	got := responses(t, h, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
		["Core/echo", `+big.String()+`, "t0"],
		["Core/echo", {"#x": {"resultOf": "t0", "name": "Core/echo", "path": "/list/*/*/*"}}, "t1"]]}`)
	if at(t, got, "/1/1/type") != "invalidResultReference" {
		t.Errorf("a path with %d results was accepted", 400*301)
	}
}

// RFC 8620 2: the Session resource, at the well-known URL of 2.2 and at
// /jmap/session. Every property the RFC lists is there with the right
// type, and the URLs are templates with the variables the RFC names.
func TestRFC8620Session(t *testing.T) {
	h := &Handler{}
	for _, path := range []string{"/.well-known/jmap", "/jmap/session"} {
		req := httptest.NewRequest("GET", "https://mail.example.test"+path, nil)
		req = req.WithContext(WithUser(req.Context(), testUser))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q", path, rec.Header().Get("Cache-Control"))
		}
		s := decode(t, rec.Body.Bytes())
		same(t, "core capability", at(t, s, "/capabilities/urn:ietf:params:jmap:core"), `{
			"maxSizeUpload": 50000000, "maxConcurrentUpload": 4, "maxSizeRequest": 10000000,
			"maxConcurrentRequests": 4, "maxCallsInRequest": 32, "maxObjectsInGet": 500, "maxObjectsInSet": 500,
			"collationAlgorithms": ["i;ascii-casemap"]}`)
		// RFC 8621 1.3.1: the value of the mail capability in the session
		// is an empty object; the account has the details.
		same(t, "mail capability", at(t, s, "/capabilities/urn:ietf:params:jmap:mail"), `{}`)
		same(t, "account", at(t, s, "/accounts/a1b2c3"), `{
			"name": "user@example.test", "isPersonal": true, "isReadOnly": false,
			"accountCapabilities": {
				"urn:ietf:params:jmap:core": {},
				"urn:ietf:params:jmap:mail": {
					"maxMailboxesPerEmail": null, "maxMailboxDepth": 10, "maxSizeMailboxName": 255,
					"maxSizeAttachmentsPerEmail": 50000000,
					"emailQuerySortOptions": ["receivedAt", "sentAt", "size", "from", "to", "subject", "hasKeyword"],
					"mayCreateTopLevelMailbox": true},
				"urn:ietf:params:jmap:submission": {"maxDelayedSend": 0, "submissionExtensions": {}}}}`)
		// RFC 8621 1.3.2: the submission capability is an empty object too.
		same(t, "submission capability", at(t, s, "/capabilities/urn:ietf:params:jmap:submission"), `{}`)
		same(t, "primaryAccounts", s["primaryAccounts"], `{"urn:ietf:params:jmap:core": "a1b2c3", "urn:ietf:params:jmap:mail": "a1b2c3",
			"urn:ietf:params:jmap:submission": "a1b2c3"}`)
		same(t, "username", s["username"], `"user@example.test"`)
		same(t, "apiUrl", s["apiUrl"], `"https://mail.example.test/jmap/api"`)
		for name, vars := range map[string][]string{
			"downloadUrl":    {"{accountId}", "{blobId}", "{name}", "{type}"},
			"uploadUrl":      {"{accountId}"},
			"eventSourceUrl": {"{types}", "{closeafter}", "{ping}"},
		} {
			u, _ := s[name].(string)
			if !strings.HasPrefix(u, "https://mail.example.test/jmap/") {
				t.Errorf("%s = %q", name, u)
			}
			for _, v := range vars {
				if !strings.Contains(u, v) {
					t.Errorf("%s lacks %s: %q", name, v, u)
				}
			}
		}
		// "sessionState ... the current value of the 'state' string on the
		// Session object" (3.4).
		_, _, out := post(t, h, "application/json", `{"using":[],"methodCalls":[]}`)
		if state, _ := s["state"].(string); state == "" || state != decode(t, out)["sessionState"] {
			t.Errorf("state %q, sessionState %v", state, decode(t, out)["sessionState"])
		}
	}
	// The state differs per user.
	if a, b := h.sessionState(testUser), h.sessionState(User{AccountID: "x", Username: "y@example.test"}); a == b {
		t.Error("two users share a session state")
	}
}

// The handler does not decide who the user is, but it refuses to answer
// without one, and it has no other paths or methods.
func TestHandlerRouting(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest("GET", "/jmap/session", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("without a user: %d", rec.Code)
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/jmap/api", 405},
		{"POST", "/jmap/session", 405},
		{"GET", "/jmap/nothing", 404},
		{"POST", "/jmap/upload/someone-else/", 404},
		{"GET", "/jmap/upload/a1b2c3/", 405},
		{"GET", "/jmap/eventsource/?types=*", 501},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req = req.WithContext(WithUser(req.Context(), testUser))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
	want := []string{"Core/echo", "Email/changes", "Email/copy", "Email/get", "Email/import", "Email/parse", "Email/query",
		"Email/queryChanges", "Email/set", "EmailSubmission/changes", "EmailSubmission/get", "EmailSubmission/query",
		"EmailSubmission/queryChanges", "EmailSubmission/set", "Identity/changes", "Identity/get", "Identity/set",
		"Mailbox/changes", "Mailbox/get", "Mailbox/query", "Mailbox/queryChanges", "Mailbox/set", "Thread/changes", "Thread/get"}
	if got := Methods(); !reflect.DeepEqual(got, want) {
		t.Errorf("methods: %v", got)
	}
}

// RFC 8620 5.5: position, anchor, anchorOffset and limit.
func TestRFC8620QueryWindow(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e"}
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name         string
		position     int64
		anchor       *string
		anchorOffset int64
		limit        int64
		wantPos      int64
		want         string
		wantErr      string
	}{
		{"from the start", 0, nil, 0, 2, 0, "a,b", ""},
		{"from the middle", 2, nil, 0, 10, 2, "c,d,e", ""},
		// "If the position is greater than or equal to the total number of
		// objects in the results list, then the ids array ... will be empty".
		{"past the end", 5, nil, 0, 10, 5, "", ""},
		// "A negative value is added to the total ... to get the position".
		{"negative", -2, nil, 0, 10, 3, "d,e", ""},
		// "If the absolute value of the negative position is greater than
		// the total, ... MUST be clamped to 0".
		{"negative past the start", -9, nil, 0, 2, 0, "a,b", ""},
		{"limit 0", 1, nil, 0, 0, 1, "", ""},
		// "anchor ... the anchorOffset is then added to its index".
		{"anchor", 0, str("c"), 0, 2, 2, "c,d", ""},
		{"anchor with offset", 0, str("c"), 1, 2, 3, "d,e", ""},
		{"anchor with negative offset", 0, str("c"), -1, 2, 1, "b,c", ""},
		// "If the result is less than 0, it is clamped to 0".
		{"anchor offset before the start", 0, str("b"), -5, 2, 0, "a,b", ""},
		// The position is ignored when there is an anchor.
		{"anchor and position", 4, str("a"), 0, 1, 0, "a", ""},
		{"anchor not found", 0, str("x"), 0, 2, 0, "", "anchorNotFound"},
	} {
		pos, got, e := window(ids, tc.position, tc.anchor, tc.anchorOffset, tc.limit)
		if tc.wantErr != "" {
			if e == nil || e.Type != tc.wantErr {
				t.Errorf("%s: error %v", tc.name, e)
			}
			continue
		}
		if e != nil || pos != tc.wantPos || strings.Join(got, ",") != tc.want {
			t.Errorf("%s: position %d ids %v error %v", tc.name, pos, got, e)
		}
	}
}

// Hostile input to the request parser.
func FuzzRequest(f *testing.F) {
	f.Add(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"a":[1,{"b":2}]},"c1"],["Core/echo",{"#a":{"resultOf":"c1","name":"Core/echo","path":"/a/*/b"}},"c2"]]}`)
	f.Add(`{"using":[],"methodCalls":[["Mailbox/get",{},"c"]],"createdIds":{"a":"b"}}`)
	f.Add(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"#":{"resultOf":"","name":"","path":"/*/*/*"}},""]]}`)
	h := &Handler{}
	f.Fuzz(func(t *testing.T, body string) {
		status, _, out := post(t, h, "application/json", body)
		if status != 200 && status != 400 {
			t.Fatalf("status %d", status)
		}
		var v map[string]any
		if err := json.Unmarshal(out, &v); err != nil {
			t.Fatalf("response is not JSON: %s", out)
		}
		if status == 200 {
			if _, ok := v["methodResponses"].([]any); !ok {
				t.Fatalf("no methodResponses: %s", out)
			}
		}
	})
}

func FuzzPointer(f *testing.F) {
	f.Add(`{"a":[1,[2,3],{"b":[4]}]}`, "/a/*/b")
	f.Add(`[[[[1]]]]`, "/*/*/*")
	f.Fuzz(func(t *testing.T, doc, path string) {
		var v any
		if json.Unmarshal([]byte(doc), &v) != nil {
			return
		}
		budget := maxPathItems
		if out, ok := evalPointer(v, path, &budget); ok {
			if _, err := json.Marshal(out); err != nil {
				t.Fatal(err)
			}
		}
	})
}
