package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

// jmapClient is a browser as far as the server can tell: it keeps the
// session cookie and sends JSON.
type jmapClient struct {
	t    *testing.T
	base string
	hc   *http.Client
}

func newJMAPClient(t *testing.T, base string) *jmapClient {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &jmapClient{t: t, base: base, hc: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

func (c *jmapClient) do(method, url, body string, header map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	if strings.HasPrefix(url, "/") {
		url = c.base + url
	}
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp, data
}

// api posts method calls and returns methodResponses. It logs both sides,
// so "go test -v" shows the conversation.
func (c *jmapClient) api(url, calls string) []any {
	c.t.Helper()
	body := `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[` + calls + `]}`
	c.t.Logf("C: %s", compact(body))
	resp, data := c.do("POST", url, body, nil)
	c.t.Logf("S: %d %s", resp.StatusCode, clipLog(string(data)))
	if resp.StatusCode != 200 {
		c.t.Fatalf("API status %d: %s", resp.StatusCode, data)
	}
	var out struct {
		MethodResponses []any `json:"methodResponses"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		c.t.Fatal(err)
	}
	for _, r := range out.MethodResponses {
		if name, _ := r.([]any)[0].(string); name == "error" {
			c.t.Fatalf("method error: %s", data)
		}
	}
	return out.MethodResponses
}

func compact(s string) string {
	var b bytes.Buffer
	if json.Compact(&b, []byte(s)) != nil {
		return s
	}
	return clipLog(b.String())
}

func clipLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1500 {
		return s[:1500] + " ..."
	}
	return s
}

// dig walks decoded JSON: a string key into an object, an int into a list.
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("no object at %v in %v", p, v)
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("no item %d in %v", k, v)
			}
			v = l[k]
		}
	}
	return v
}

func sameJSON(t *testing.T, name string, got any, want string) {
	t.Helper()
	g, _ := json.Marshal(got)
	var a, b any
	json.Unmarshal(g, &a)
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("%s: bad expectation: %v", name, err)
	}
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	if string(ga) != string(gb) {
		t.Errorf("%s:\n got %s\nwant %s", name, ga, gb)
	}
}

// reportMessage is a message as a mail client sends it: a text and an HTML
// version, and a PDF.
func reportMessage(to string) string {
	return "Date: Fri, 13 Nov 2026 10:00:00 +0100\r\n" +
		"From: =?UTF-8?Q?Ren=C3=A9e_Dubois?= <renee@sender.example>\r\n" +
		"To: Mike <" + to + ">\r\n" +
		"Subject: =?UTF-8?B?UmFwcG9ydCDigJMgUTM=?=\r\n" +
		"Message-ID: <report@sender.example>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"outer\"\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/alternative; boundary=\"inner\"\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Hoi Mike,\r\n\r\nhet rapport staat in de bijlage. Groet, Ren=C3=A9e\r\n" +
		"--inner\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		"<html><body><p>Hoi Mike,</p><p>het rapport staat in de <b>bijlage</b>. Groet, Ren\xc3\xa9e</p>" +
		"<script>alert('x')</script><img src=\"https://tracker.example/p.gif\"></body></html>\r\n" +
		"--inner--\r\n" +
		"--outer\r\nContent-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename*=UTF-8''rapport%20Q3.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"JVBERi0xLjQKJSBub3QgYSByZWFsIFBERgo=\r\n" +
		"--outer--\r\n"
}

// TestJMAPEndToEnd is the Go side of phase 3's exit: mail is delivered over
// SMTP into a running "serve", a client logs in over HTTP and runs what a
// webmail does over JMAP: session, mailboxes, a page of threads, an email
// with its body, mark read, move, changes, download, the HTML view, logout.
//
//	go test -v -run TestJMAPEndToEnd ./cmd/robinmail
func TestJMAPEndToEnd(t *testing.T) {
	c := newCLI(t, `"smtp":{"listen":"127.0.0.1:0"},"web":{"http_listen":"127.0.0.1:0"},`)
	domain := "jmap-" + randomHex() + ".test"
	user := "mike@" + domain
	c.ok("", "key", "generate")
	c.ok("", "domain", "add", domain)
	c.ok("correct horse\n", "user", "add", user)
	srv := startServe(t, c)
	if srv.web == "" {
		t.Fatal("serve did not report a web listener")
	}

	// Mail arrives over SMTP: a message, a reply to it, and one with an
	// HTML body and an attachment.
	s := dialSMTP(t, srv.addr)
	s.cmd("EHLO client.sender.example", 250)
	s.send("anna@sender.example", user, message(user, "1", "Lunch on Tuesday", "Hi Mike, are you free for lunch on Tuesday? Anna"), 250)
	s.send("anna@sender.example", user, strings.Replace(message(user, "2", "Re: Lunch on Tuesday", "Or Wednesday?"),
		"MIME-Version", "References: <1@sender.example>\r\nMIME-Version", 1), 250)
	s.send("renee@sender.example", user, reportMessage(user), 250)
	s.cmd("QUIT", 221)

	web := newJMAPClient(t, "http://"+srv.web)

	// Nothing without a login.
	if resp, _ := web.do("GET", "/.well-known/jmap", "", nil); resp.StatusCode != 401 {
		t.Fatalf("session without login: %d", resp.StatusCode)
	}
	if resp, _ := web.do("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":"wrong"}`, user), nil); resp.StatusCode != 401 {
		t.Fatalf("login with a wrong password: %d", resp.StatusCode)
	}
	resp, data := web.do("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":"correct horse"}`, user), nil)
	t.Logf("login: %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %s", resp.StatusCode, data)
	}
	if set := resp.Header.Get("Set-Cookie"); !strings.Contains(set, "HttpOnly") || !strings.Contains(set, "SameSite=Strict") {
		t.Errorf("Set-Cookie: %s", set)
	}
	var who struct{ AccountID, Username string }
	json.Unmarshal(data, &who)
	if who.Username != user || who.AccountID == "" {
		t.Fatalf("login answered %s", data)
	}
	A := who.AccountID

	// The session resource names the account and the endpoints.
	resp, data = web.do("GET", "/.well-known/jmap", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("session: %d %s", resp.StatusCode, data)
	}
	var session map[string]any
	json.Unmarshal(data, &session)
	apiURL := session["apiUrl"].(string)
	if apiURL != "http://"+srv.web+"/jmap/api" || dig(t, session, "primaryAccounts", "urn:ietf:params:jmap:mail") != A ||
		session["username"] != user || dig(t, session, "accounts", A, "name") != user {
		t.Errorf("session: %s", data)
	}
	download := func(blobID, name, typ string) (*http.Response, []byte) {
		u := session["downloadUrl"].(string)
		for k, v := range map[string]string{"{accountId}": A, "{blobId}": blobID, "{name}": name, "{type}": typ} {
			u = strings.ReplaceAll(u, k, v)
		}
		return web.do("GET", u, "", nil)
	}

	// Mailboxes.
	got := web.api(apiURL, `["Mailbox/get", {"accountId": "`+A+`", "ids": null}, "m"]`)
	state0 := dig(t, got, 0, 1, "state").(string)
	box := map[string]string{} // role -> id
	for _, m := range dig(t, got, 0, 1, "list").([]any) {
		if role, _ := dig(t, m, "role").(string); role != "" {
			box[role] = dig(t, m, "id").(string)
		}
		if dig(t, m, "role") == "inbox" {
			sameJSON(t, "inbox counts", []any{dig(t, m, "totalEmails"), dig(t, m, "unreadEmails"), dig(t, m, "totalThreads"), dig(t, m, "unreadThreads")}, `[3, 3, 2, 2]`)
		}
	}
	if len(box) != 5 {
		t.Fatalf("roles: %v", box)
	}

	// The first page of the inbox as a webmail asks for it: one row per
	// thread, newest first, with what a list shows, in one request.
	got = web.api(apiURL, `
		["Email/query", {"accountId": "`+A+`", "filter": {"inMailbox": "`+box["inbox"]+`"},
			"sort": [{"property": "receivedAt", "isAscending": false}],
			"collapseThreads": true, "position": 0, "limit": 30, "calculateTotal": true}, "q"],
		["Email/get", {"accountId": "`+A+`",
			"#ids": {"resultOf": "q", "name": "Email/query", "path": "/ids"},
			"properties": ["threadId", "from", "subject", "receivedAt", "preview", "keywords", "hasAttachment"]}, "g"],
		["Thread/get", {"accountId": "`+A+`",
			"#ids": {"resultOf": "g", "name": "Email/get", "path": "/list/*/threadId"}}, "t"]`)
	sameJSON(t, "total threads", dig(t, got, 0, 1, "total"), `2`)
	rows := dig(t, got, 1, 1, "list").([]any)
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	report, lunch := rows[0], rows[1]
	sameJSON(t, "report row", []any{dig(t, report, "subject"), dig(t, report, "from"), dig(t, report, "hasAttachment"), dig(t, report, "keywords"),
		dig(t, report, "preview")},
		`["Rapport – Q3", [{"name": "Renée Dubois", "email": "renee@sender.example"}], true, {},
		  "Hoi Mike, het rapport staat in de bijlage. Groet, Renée"]`)
	// The thread row is its newest email, the reply.
	sameJSON(t, "lunch row", []any{dig(t, lunch, "subject"), dig(t, lunch, "hasAttachment"), dig(t, lunch, "preview")},
		`["Re: Lunch on Tuesday", false, "Or Wednesday?"]`)
	lunchThread := dig(t, got, 2, 1, "list", 1, "emailIds").([]any)
	if len(lunchThread) != 2 || lunchThread[1] != dig(t, lunch, "id") {
		t.Errorf("lunch thread: %v", lunchThread)
	}
	reportID, firstLunch, replyID := dig(t, report, "id").(string), lunchThread[0].(string), lunchThread[1].(string)

	// Open the report: both bodies and the attachment.
	got = web.api(apiURL, `["Email/get", {"accountId": "`+A+`", "ids": ["`+reportID+`"],
		"properties": ["blobId", "subject", "from", "to", "sentAt", "mailboxIds", "textBody", "htmlBody", "attachments", "bodyValues"],
		"bodyProperties": ["partId", "blobId", "type", "name", "size", "disposition"],
		"fetchTextBodyValues": true, "fetchHTMLBodyValues": true}, "o"]`)
	email := dig(t, got, 0, 1, "list", 0)
	sameJSON(t, "to and sentAt", []any{dig(t, email, "to"), dig(t, email, "sentAt"), dig(t, email, "mailboxIds")},
		`[[{"name": "Mike", "email": "`+user+`"}], "2026-11-13T10:00:00+01:00", {"`+box["inbox"]+`": true}]`)
	textPart, htmlPart := dig(t, email, "textBody", 0, "partId").(string), dig(t, email, "htmlBody", 0, "partId").(string)
	sameJSON(t, "text value", dig(t, email, "bodyValues", textPart, "value"), `"Hoi Mike,\n\nhet rapport staat in de bijlage. Groet, Renée"`)
	if html := dig(t, email, "bodyValues", htmlPart, "value").(string); !strings.Contains(html, "<b>bijlage</b>") {
		t.Errorf("html value %q", html)
	}
	att := dig(t, email, "attachments", 0)
	sameJSON(t, "attachment", []any{dig(t, att, "name"), dig(t, att, "type"), dig(t, att, "size"), dig(t, att, "disposition")},
		`["rapport Q3.pdf", "application/pdf", 26, "attachment"]`)

	// Download the attachment and the message itself.
	resp, data = download(dig(t, att, "blobId").(string), "rapport%20Q3.pdf", "application/pdf")
	if resp.StatusCode != 200 || string(data) != "%PDF-1.4\n% not a real PDF\n" || resp.Header.Get("Content-Type") != "application/pdf" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("attachment download: %d %q %v", resp.StatusCode, data, resp.Header)
	}
	resp, data = download(dig(t, email, "blobId").(string), "message.eml", "message/rfc822")
	if resp.StatusCode != 200 || !bytes.Contains(data, []byte("\r\nMessage-ID: <report@sender.example>\r\n")) ||
		!bytes.HasPrefix(data, []byte("Return-Path: <renee@sender.example>\r\nReceived: ")) {
		t.Errorf("message download: %d\n%s", resp.StatusCode, data)
	}

	// The HTML view for the iframe: no script, the remote image held back.
	resp, data = web.do("GET", "/jmap/view/"+A+"/"+reportID, "", nil)
	view := string(data)
	if resp.StatusCode != 200 || !strings.Contains(view, "<b>bijlage</b>") || strings.Contains(view, "script") ||
		strings.Contains(view, "tracker.example") || resp.Header.Get("X-Remote-Images") != "blocked" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("view: %d %v\n%s", resp.StatusCode, resp.Header, view)
	}
	resp, data = web.do("GET", "/jmap/view/"+A+"/"+reportID+"?images=1", "", nil)
	if !strings.Contains(string(data), `src="https://tracker.example/p.gif"`) || resp.Header.Get("X-Remote-Images") != "shown" {
		t.Errorf("view with images: %s", data)
	}

	// Mark the report read, guarded by the state we saw.
	got = web.api(apiURL, `["Email/set", {"accountId": "`+A+`", "ifInState": "`+state0+`",
		"update": {"`+reportID+`": {"keywords/$seen": true}}}, "s"]`)
	sameJSON(t, "marked read", []any{dig(t, got, 0, 1, "oldState"), dig(t, got, 0, 1, "updated")}, `["`+state0+`", {"`+reportID+`": null}]`)
	state1 := dig(t, got, 0, 1, "newState").(string)
	// The same again with the old state is refused: someone else changed
	// the account in between (we did).
	got = nil
	_, data = web.do("POST", apiURL, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/set", {"accountId": "`+A+`", "ifInState": "`+state0+`", "update": {"`+reportID+`": {"keywords/$flagged": true}}}, "s"]]}`, nil)
	if !strings.Contains(string(data), `"stateMismatch"`) {
		t.Errorf("stale ifInState: %s", data)
	}

	// Move the first lunch message to the Trash, flag the reply.
	got = web.api(apiURL, `["Email/set", {"accountId": "`+A+`", "update": {
		"`+firstLunch+`": {"mailboxIds/`+box["inbox"]+`": null, "mailboxIds/`+box["trash"]+`": true},
		"`+replyID+`": {"keywords/$flagged": true}}}, "s"]`)
	sameJSON(t, "moved and flagged", dig(t, got, 0, 1, "updated"), `{"`+firstLunch+`": null, "`+replyID+`": null}`)

	// What changed since the first look, as a client syncing would ask.
	got = web.api(apiURL, `
		["Email/changes", {"accountId": "`+A+`", "sinceState": "`+state0+`"}, "ec"],
		["Mailbox/changes", {"accountId": "`+A+`", "sinceState": "`+state0+`"}, "mc"],
		["Thread/changes", {"accountId": "`+A+`", "sinceState": "`+state0+`"}, "tc"],
		["Mailbox/get", {"accountId": "`+A+`", "#ids": {"resultOf": "mc", "name": "Mailbox/changes", "path": "/updated"},
			"properties": ["name", "totalEmails", "unreadEmails", "totalThreads", "unreadThreads"]}, "mg"],
		["Email/queryChanges", {"accountId": "`+A+`", "filter": {"inMailbox": "`+box["inbox"]+`"},
			"sort": [{"property": "receivedAt", "isAscending": false}], "sinceQueryState": "`+state1+`"}, "qc"]`)
	sameJSON(t, "Email/changes", []any{dig(t, got, 0, 1, "created"), dig(t, got, 0, 1, "updated"), dig(t, got, 0, 1, "destroyed"),
		dig(t, got, 0, 1, "hasMoreChanges")}, `[[], ["`+firstLunch+`", "`+replyID+`", "`+reportID+`"], [], false]`)
	sameJSON(t, "Mailbox/changes", dig(t, got, 1, 1, "updated"), `["`+box["inbox"]+`", "`+box["trash"]+`"]`)
	sameJSON(t, "Thread/changes", []any{dig(t, got, 2, 1, "created"), dig(t, got, 2, 1, "updated"), dig(t, got, 2, 1, "destroyed")}, `[[], [], []]`)
	sameJSON(t, "mailboxes after", dig(t, got, 3, 1, "list"), `[
		{"id": "`+box["inbox"]+`", "name": "Inbox", "totalEmails": 2, "unreadEmails": 1, "totalThreads": 2, "unreadThreads": 1},
		{"id": "`+box["trash"]+`", "name": "Trash", "totalEmails": 1, "unreadEmails": 1, "totalThreads": 1, "unreadThreads": 1}]`)
	// Since state1 the inbox lost the first lunch message; the reply was
	// flagged and so is reported, in the same place.
	sameJSON(t, "Email/queryChanges", []any{dig(t, got, 4, 1, "removed"), dig(t, got, 4, 1, "added")},
		`[["`+firstLunch+`", "`+replyID+`"], [{"id": "`+replyID+`", "index": 1}]]`)

	// A label of our own, and the report in it too; then delete the label.
	got = web.api(apiURL, `
		["Mailbox/set", {"accountId": "`+A+`", "create": {"work": {"name": "Work", "parentId": null}}}, "c"],
		["Email/set", {"accountId": "`+A+`", "update": {"`+reportID+`": {"mailboxIds/#work": true}}}, "u"]`)
	work := dig(t, got, 0, 1, "created", "work", "id").(string)
	got = web.api(apiURL, `
		["Email/query", {"accountId": "`+A+`", "filter": {"inMailbox": "`+work+`"}}, "q"],
		["Mailbox/set", {"accountId": "`+A+`", "destroy": ["`+work+`"], "onDestroyRemoveEmails": true}, "d"],
		["Email/get", {"accountId": "`+A+`", "ids": ["`+reportID+`"], "properties": ["mailboxIds", "keywords"]}, "g"]`)
	sameJSON(t, "in the label", dig(t, got, 0, 1, "ids"), `["`+reportID+`"]`)
	sameJSON(t, "label destroyed", dig(t, got, 1, 1, "destroyed"), `["`+work+`"]`)
	sameJSON(t, "report kept", dig(t, got, 2, 1, "list", 0), `{"id": "`+reportID+`", "mailboxIds": {"`+box["inbox"]+`": true}, "keywords": {"$seen": true}}`)

	// Delete for good.
	got = web.api(apiURL, `
		["Email/set", {"accountId": "`+A+`", "destroy": ["`+firstLunch+`"]}, "d"],
		["Email/query", {"accountId": "`+A+`", "calculateTotal": true}, "q"]`)
	sameJSON(t, "destroyed", []any{dig(t, got, 0, 1, "destroyed"), dig(t, got, 1, 1, "total")}, `[["`+firstLunch+`"], 2]`)
	// The command line sees the same store.
	c.expect(c.ok("", "list", user), `Inbox \(m1\): 2 emails`)

	// A page on another site cannot use the session.
	resp, _ = web.do("POST", apiURL, `{"using":[],"methodCalls":[]}`, map[string]string{"Origin": "https://evil.example"})
	if resp.StatusCode != 403 {
		t.Errorf("cross-site request: %d", resp.StatusCode)
	}
	// Our own origin can.
	resp, _ = web.do("POST", apiURL, `{"using":[],"methodCalls":[]}`, map[string]string{"Origin": "http://" + srv.web})
	if resp.StatusCode != 200 {
		t.Errorf("same-origin request: %d", resp.StatusCode)
	}

	// A standard JMAP client without cookies: Basic.
	req, _ := http.NewRequest("GET", "http://"+srv.web+"/.well-known/jmap", nil)
	req.SetBasicAuth(user, "correct horse")
	bresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bresp.Body.Close()
	if bresp.StatusCode != 200 {
		t.Errorf("Basic auth: %d", bresp.StatusCode)
	}

	// Logout ends it.
	if resp, _ := web.do("POST", "/auth/logout", "{}", nil); resp.StatusCode != 204 {
		t.Errorf("logout: %d", resp.StatusCode)
	}
	if resp, _ := web.do("POST", apiURL, `{"using":[],"methodCalls":[]}`, nil); resp.StatusCode != 401 {
		t.Errorf("after logout: %d", resp.StatusCode)
	}
	// Everything else is the webmail's: for now the placeholder.
	resp, data = web.do("GET", "/mail/inbox", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(data), "<title>robinmail</title>") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("placeholder: %d %v", resp.StatusCode, resp.Header)
	}

	if err := srv.stop(); err != nil {
		t.Errorf("serve returned %v", err)
	}
	if resp, err := http.Get("http://" + srv.web + "/"); err == nil {
		resp.Body.Close()
		t.Error("the web listener still answers after shutdown")
	}
}
