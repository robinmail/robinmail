package jmap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"

	"github.com/mikepage/robinmail/internal/message"
	"github.com/mikepage/robinmail/internal/store"
)

// Tests of what makes Emails: the upload endpoint (RFC 8620 6.1),
// Email/set create (RFC 8621 4.6), Email/import (4.8), Email/copy (4.7) and
// Email/parse (4.9).

// upload posts a blob and returns its id.
func (e *env) upload(data []byte, typ string) string {
	e.t.Helper()
	rec := e.post("/jmap/upload/"+e.acct.ID+"/", data, typ)
	if rec.Code != 201 {
		e.t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	return decode(e.t, rec.Body.Bytes())["blobId"].(string)
}

func (e *env) post(path string, data []byte, typ string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "https://mail.example.test"+path, bytes.NewReader(data))
	if typ != "" {
		req.Header.Set("Content-Type", typ)
	}
	req = req.WithContext(WithUser(req.Context(), e.user))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// raw returns the stored message of an email.
func (e *env) raw(id string) []byte {
	e.t.Helper()
	n, _ := store.ParseEmailID(id)
	data, _, err := e.st.Raw(e.ctx, e.acct.ID, n)
	if err != nil {
		e.t.Fatal(err)
	}
	return data
}

// create runs an Email/set with one creation "c" and returns the created
// object, or fails the test.
func (e *env) create(obj string) map[string]any {
	e.t.Helper()
	got := e.one("Email/set", `{"accountId": "$A", "create": {"c": `+obj+`}}`)
	if got["created"] == nil {
		b, _ := json.Marshal(got["notCreated"])
		e.t.Fatalf("not created: %s", b)
	}
	return at(e.t, got, "/created/c").(map[string]any)
}

// refused runs an Email/set with one creation and returns its SetError.
func (e *env) refused(obj string) map[string]any {
	e.t.Helper()
	got := e.one("Email/set", `{"accountId": "$A", "create": {"c": `+obj+`}}`)
	if got["notCreated"] == nil {
		e.t.Fatalf("created: %s", obj)
	}
	return at(e.t, got, "/notCreated/c").(map[string]any)
}

// RFC 8620 6.1: "the server MUST return [...] accountId, blobId, type,
// size" with status 201; the blob can then be downloaded.
func TestRFC8620_6_1_Upload(t *testing.T) {
	e := newEnv(t)
	data := []byte("%PDF-1.7\n\x00\xff binary")
	rec := e.post("/jmap/upload/"+e.acct.ID+"/", data, "application/pdf")
	if rec.Code != 201 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	sum := sha256.Sum256(data)
	same(t, "response", decode(t, rec.Body.Bytes()), fmt.Sprintf(`{"accountId": %q, "blobId": %q, "type": "application/pdf", "size": %d}`,
		e.acct.ID, hex.EncodeToString(sum[:]), len(data)))
	blobID := hex.EncodeToString(sum[:])
	dl := e.get("/jmap/download/" + e.acct.ID + "/" + blobID + "/file.pdf?type=application/pdf")
	if dl.Code != 200 || !bytes.Equal(dl.Body.Bytes(), data) {
		t.Errorf("download: %d, %d bytes", dl.Code, dl.Body.Len())
	}
	// The URL of the session works with and without the final slash.
	if rec := e.post("/jmap/upload/"+e.acct.ID, []byte("x"), ""); rec.Code != 201 || decode(t, rec.Body.Bytes())["type"] != "application/octet-stream" {
		t.Errorf("without a type: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.post("/jmap/upload/someone-else/", data, "application/pdf"); rec.Code != 404 {
		t.Errorf("upload to another account: %d", rec.Code)
	}
	// maxSizeUpload.
	small := newEnv(t)
	small.h.Limits.MaxSizeUpload = 100
	rec = small.post("/jmap/upload/"+small.acct.ID+"/", bytes.Repeat([]byte("x"), 101), "text/plain")
	if rec.Code != 413 || decode(t, rec.Body.Bytes())["limit"] != "maxSizeUpload" {
		t.Errorf("over maxSizeUpload: %d %s", rec.Code, rec.Body.String())
	}
	if rec := small.post("/jmap/upload/"+small.acct.ID+"/", bytes.Repeat([]byte("x"), 100), "text/plain"); rec.Code != 201 {
		t.Errorf("at maxSizeUpload: %d", rec.Code)
	}
}

// RFC 8621 4.6, creating an Email, with the draft of the example in 7.5.1
// (and 4.10.1 for the body).
func TestRFC8621_4_6_CreateExample(t *testing.T) {
	e := newEnv(t)
	before := e.state()
	created := e.create(`{
		"mailboxIds": {"m2": true},
		"keywords": {"$seen": true, "$draft": true},
		"from": [{"name": "Joe Bloggs", "email": "joe@example.com"}],
		"to": [{"name": "John", "email": "john@example.com"}],
		"subject": "World domination",
		"receivedAt": "2015-07-02T16:21:35Z",
		"sentAt": "2015-07-02T16:21:35+10:00",
		"bodyStructure": {
			"type": "multipart/alternative",
			"subParts": [{"partId": "49db", "type": "text/html"}, {"partId": "bd48", "type": "text/plain"}]
		},
		"bodyValues": {
			"bd48": {"value": "I have the most brilliant plan.  Let me tell you all about it.  What we do is, we", "isTruncated": false},
			"49db": {"value": "<!DOCTYPE html><html><head><title></title><style type=\"text/css\">div{font-size:16px}</style></head><body><div>I have the most brilliant plan.  Let me tell you all about it.  What we do is, we</div></body></html>", "isTruncated": false}
		}}`)
	// "The response [...] with the server-set properties": id, blobId,
	// threadId and size.
	if len(created) != 4 {
		t.Errorf("created has %v", created)
	}
	id := created["id"].(string)
	raw := e.raw(id)
	t.Logf("the message:\n%s", strings.ReplaceAll(string(raw), "\r\n", "\n"))
	sum := sha256.Sum256(raw)
	same(t, "blobId and size", []any{created["blobId"], created["size"]}, fmt.Sprintf(`[%q, %d]`, hex.EncodeToString(sum[:]), len(raw)))

	got := e.one("Email/get", `{"accountId": "$A", "ids": ["`+id+`"], "fetchAllBodyValues": true,
		"properties": ["mailboxIds", "keywords", "from", "to", "subject", "receivedAt", "sentAt", "messageId", "bodyStructure", "bodyValues", "textBody", "htmlBody", "hasAttachment", "preview", "threadId"],
		"bodyProperties": ["partId", "type", "charset"]}`)
	em := at(t, got, "/list/0").(map[string]any)
	same(t, "placement", []any{em["mailboxIds"], em["keywords"], em["receivedAt"]}, `[{"m2": true}, {"$seen": true, "$draft": true}, "2015-07-02T16:21:35Z"]`)
	same(t, "header", []any{em["from"], em["to"], em["subject"], em["sentAt"]},
		`[[{"name": "Joe Bloggs", "email": "joe@example.com"}], [{"name": "John", "email": "john@example.com"}], "World domination", "2015-07-02T16:21:35+10:00"]`)
	same(t, "structure", em["bodyStructure"], `{"partId": null, "type": "multipart/alternative", "charset": null, "subParts": [
		{"partId": "1", "type": "text/html", "charset": "utf-8", "subParts": null},
		{"partId": "2", "type": "text/plain", "charset": "utf-8", "subParts": null}]}`)
	same(t, "text", at(t, em, "/bodyValues/2/value"), `"I have the most brilliant plan.  Let me tell you all about it.  What we do is, we"`)
	if html := at(t, em, "/bodyValues/1/value").(string); !strings.HasPrefix(html, "<!DOCTYPE html><html>") || !strings.HasSuffix(strings.TrimSpace(html), "</body></html>") {
		t.Errorf("html body %q", html)
	}
	same(t, "hasAttachment", em["hasAttachment"], `false`)
	// "If not included, the server MUST generate and set a Message-ID
	// header field [...] and a Date header field". Date was given as
	// sentAt; the Message-ID is ours.
	ids := em["messageId"].([]any)
	if len(ids) != 1 || !strings.HasSuffix(ids[0].(string), "@mail.example.test") {
		t.Errorf("messageId %v", ids)
	}
	if n := strings.Count(string(raw), "\r\nDate: ") + strings.Count(string(raw), "Date: "); n < 1 || strings.Count(string(raw), "Date:") != 1 {
		t.Errorf("Date fields: %d", strings.Count(string(raw), "Date:"))
	}
	// A mail client reads it too.
	sm, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if mt, _, _ := mime.ParseMediaType(sm.Header.Get("Content-Type")); mt != "multipart/alternative" || sm.Header.Get("Mime-Version") != "1.0" {
		t.Errorf("Content-Type %q, MIME-Version %q", sm.Header.Get("Content-Type"), sm.Header.Get("Mime-Version"))
	}
	// The creation is a change like any other.
	ch := e.one("Email/changes", `{"accountId": "$A", "sinceState": "`+before+`"}`)
	same(t, "changes", []any{ch["created"], ch["updated"], ch["destroyed"]}, `[["`+id+`"], [], []]`)

	// Without a date of its own the message gets one, and a draft is
	// replaced by creating the new one and destroying the old in one call.
	got = e.one("Email/set", `{"accountId": "$A", "create": {"v2": {"mailboxIds": {"m2": true}, "keywords": {"$draft": true},
		"from": [{"name": null, "email": "joe@example.com"}], "subject": "World domination, take two",
		"textBody": [{"partId": "t", "type": "text/plain"}], "bodyValues": {"t": {"value": "Shorter."}}}}, "destroy": ["`+id+`"]}`)
	same(t, "destroyed", got["destroyed"], `["`+id+`"]`)
	v2 := at(t, got, "/created/v2/id").(string)
	if date, ok := message.Get(message.ParseHeader(e.raw(v2)), "Date"); !ok || len(date) < 20 {
		t.Errorf("no Date was added: %q", date)
	}
}

// RFC 8621 4.6: textBody, htmlBody and attachments instead of
// bodyStructure, and parts from blobs.
func TestRFC8621_4_6_CreateWithAttachments(t *testing.T) {
	e := newEnv(t)
	png := []byte("\x89PNG\r\n\x1a\n pretend pixels")
	pdf := bytes.Repeat([]byte("%PDF \xff\x00"), 300)
	pngID, pdfID := e.upload(png, "image/png"), e.upload(pdf, "application/pdf")
	// A message with an attachment to forward a part of.
	old := e.deliver(store.Message{Raw: []byte("From: a@x.test\r\nSubject: old\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n--b\r\nContent-Type: text/csv; name=data.csv\r\nContent-Disposition: attachment; filename=data.csv\r\n\r\na,b\r\n1,2\r\n--b--\r\n")})

	created := e.create(`{
		"mailboxIds": {"m2": true, "m3": true},
		"from": [{"name": "Zoë de Vries", "email": "` + e.acct.Address + `"}],
		"to": [{"name": "山田 太郎", "email": "taro@example.jp"}, {"name": "O'Neil, Pat", "email": "pat@example.com"}],
		"cc": [{"name": null, "email": "cc@example.com"}],
		"bcc": [{"name": null, "email": "hidden@example.com"}],
		"replyTo": [{"name": "Zoë", "email": "reply@example.com"}],
		"subject": "Offerte één: “de grote” — met bijlagen, en een onderwerp dat veel te lang is voor één regel",
		"inReplyTo": ["earlier@x.test"], "references": ["first@x.test", "earlier@x.test"],
		"header:X-Project:asText": "Geïnteresseerd",
		"header:List-Unsubscribe:asURLs": ["https://example.com/unsub?u=1", "mailto:unsub@example.com"],
		"header:X-Trace:asRaw:all": [" one", " two"],
		"header:Resent-Date:asDate": "2026-11-14T09:00:00+01:00",
		"textBody": [{"partId": "text", "type": "text/plain"}],
		"htmlBody": [{"partId": "html", "type": "text/html"}],
		"attachments": [
			{"blobId": "` + pngID + `", "type": "image/png", "name": "logo.png", "disposition": "inline", "cid": "logo@mail"},
			{"blobId": "` + pdfID + `", "type": "application/pdf", "name": "Offerte één (definitief).pdf", "size": ` + fmt.Sprint(len(pdf)) + `, "language": ["nl"]},
			{"blobId": "` + PartBlobID(old.Blob, "2") + `", "type": "text/csv", "name": "data.csv", "disposition": "attachment"},
			{"blobId": "` + old.Blob + `", "type": "message/rfc822", "name": "old.eml"}
		],
		"bodyValues": {
			"text": {"value": "Beste Tarō,\n\nZie de bijlagen.\n\nGroet,\nZoë\n"},
			"html": {"value": "<p>Beste Tarō,</p><p>Zie de bijlagen. <img src=\"cid:logo@mail\"></p>"}
		}}`)
	id := created["id"].(string)
	raw := e.raw(id)
	t.Logf("the message header:\n%s", strings.ReplaceAll(string(raw[:bytes.Index(raw, []byte("\r\n\r\n"))]), "\r\n", "\n"))
	for _, l := range bytes.Split(raw, []byte("\r\n")) {
		if len(l) > 998 {
			t.Errorf("a line of %d octets", len(l))
		}
	}
	for _, c := range raw[:bytes.Index(raw, []byte("\r\n\r\n"))] {
		if c >= 0x80 {
			t.Fatal("the header has 8-bit octets")
		}
	}

	got := e.one("Email/get", `{"accountId": "$A", "ids": ["`+id+`"], "fetchAllBodyValues": true,
		"properties": ["mailboxIds", "from", "to", "cc", "bcc", "replyTo", "subject", "inReplyTo", "references", "hasAttachment",
			"header:X-Project:asText", "header:List-Unsubscribe:asURLs", "header:X-Trace:asRaw:all", "header:Resent-Date:asDate",
			"bodyStructure", "textBody", "htmlBody", "attachments", "bodyValues"],
		"bodyProperties": ["partId", "blobId", "type", "name", "disposition", "cid", "size", "language"]}`)
	em := at(t, got, "/list/0").(map[string]any)
	same(t, "mailboxes", em["mailboxIds"], `{"m2": true, "m3": true}`)
	same(t, "addresses", []any{em["from"], em["to"], em["cc"], em["bcc"], em["replyTo"]}, `[
		[{"name": "Zoë de Vries", "email": "`+e.acct.Address+`"}],
		[{"name": "山田 太郎", "email": "taro@example.jp"}, {"name": "O'Neil, Pat", "email": "pat@example.com"}],
		[{"name": null, "email": "cc@example.com"}], [{"name": null, "email": "hidden@example.com"}],
		[{"name": "Zoë", "email": "reply@example.com"}]]`)
	same(t, "subject", em["subject"], `"Offerte één: “de grote” — met bijlagen, en een onderwerp dat veel te lang is voor één regel"`)
	same(t, "ids", []any{em["inReplyTo"], em["references"]}, `[["earlier@x.test"], ["first@x.test", "earlier@x.test"]]`)
	same(t, "other headers", []any{em["header:X-Project:asText"], em["header:List-Unsubscribe:asURLs"], em["header:X-Trace:asRaw:all"], em["header:Resent-Date:asDate"]},
		`["Geïnteresseerd", ["https://example.com/unsub?u=1", "mailto:unsub@example.com"], [" one", " two"], "2026-11-14T09:00:00+01:00"]`)
	same(t, "hasAttachment", em["hasAttachment"], `true`)

	// The tree: mixed[alternative[text, related[html, image]], pdf, csv, message].
	var shape func(v any) string
	shape = func(v any) string {
		p := v.(map[string]any)
		subs, ok := p["subParts"].([]any)
		if !ok {
			return p["type"].(string)
		}
		var out []string
		for _, s := range subs {
			out = append(out, shape(s))
		}
		return p["type"].(string) + "[" + strings.Join(out, " ") + "]"
	}
	if got, want := shape(em["bodyStructure"]), "multipart/mixed[multipart/alternative[text/plain multipart/related[text/html image/png]] application/pdf text/csv message/rfc822]"; got != want {
		t.Errorf("structure %s\n      want %s", got, want)
	}
	same(t, "text and html", []any{at(t, em, "/textBody/0/type"), at(t, em, "/htmlBody/0/type"), at(t, em, "/bodyValues/1/value")},
		`["text/plain", "text/html", "Beste Tarō,\n\nZie de bijlagen.\n\nGroet,\nZoë\n"]`)
	atts := em["attachments"].([]any)
	if len(atts) != 4 {
		t.Fatalf("%d attachments", len(atts))
	}
	same(t, "inline image", pick(atts[0].(map[string]any), []string{"type", "name", "disposition", "cid", "size"}),
		fmt.Sprintf(`{"id": null, "type": "image/png", "name": "logo.png", "disposition": "inline", "cid": "logo@mail", "size": %d}`, len(png)))
	same(t, "pdf", pick(atts[1].(map[string]any), []string{"type", "name", "disposition", "size", "language"}),
		fmt.Sprintf(`{"id": null, "type": "application/pdf", "name": "Offerte één (definitief).pdf", "disposition": "attachment", "size": %d, "language": ["nl"]}`, len(pdf)))
	// What comes back out of each attachment is what went in.
	for i, want := range [][]byte{png, pdf, []byte("a,b\r\n1,2"), e.raw(eid(old))} {
		blobID := atts[i].(map[string]any)["blobId"].(string)
		dl := e.get("/jmap/download/" + e.acct.ID + "/" + blobID + "/x")
		if dl.Code != 200 || !bytes.Equal(bytes.TrimRight(dl.Body.Bytes(), "\r\n"), bytes.TrimRight(want, "\r\n")) {
			t.Errorf("attachment %d: %d, %d bytes, want %d", i, dl.Code, dl.Body.Len(), len(want))
		}
	}
	// A blob id that reaches into the attached message (4.9).
	nested := atts[3].(map[string]any)["blobId"].(string) + "-2"
	if dl := e.get("/jmap/download/" + e.acct.ID + "/" + nested + "/data.csv"); dl.Code != 200 || dl.Body.String() != "a,b\r\n1,2" {
		t.Errorf("part of an attached message: %d %q", dl.Code, dl.Body.String())
	}
}

// A text file that is attached comes back byte for byte: its line ends
// are not made CRLF, as those of a text body are. (RFC 8621 4.6 lets the
// server choose the encoding; RFC 2045 2.8 wants CRLF in text that is not
// base64, so such a file goes out as base64.) Found by the webmail's
// end-to-end test, which attached a file with LF line ends.
func TestCreateKeepsTheBytesOfATextAttachment(t *testing.T) {
	e := newEnv(t)
	files := [][]byte{
		[]byte("#!/bin/sh\necho one\necho two\n"),
		[]byte("old mac\rline ends\r"),
		[]byte("a,b\r\n1,2\r\n"),
		[]byte("no line end at all"),
		[]byte("caf\xe9 in latin-1\n"),
	}
	var parts []string
	for i, f := range files {
		parts = append(parts, `{"blobId": "`+e.upload(f, "text/plain")+`", "type": "text/plain", "name": "f`+fmt.Sprint(i)+`.txt", "disposition": "attachment"}`)
	}
	created := e.create(`{"mailboxIds": {"m2": true}, "from": [{"name": null, "email": "` + e.acct.Address + `"}], "subject": "files",
		"textBody": [{"partId": "t", "type": "text/plain"}], "bodyValues": {"t": {"value": "see attached\n"}},
		"attachments": [` + strings.Join(parts, ",") + `]}`)
	got := e.one("Email/get", `{"accountId": "$A", "ids": ["`+created["id"].(string)+`"], "properties": ["attachments"], "bodyProperties": ["blobId", "size"]}`)
	atts := at(t, got, "/list/0/attachments").([]any)
	if len(atts) != len(files) {
		t.Fatalf("%d attachments, want %d", len(atts), len(files))
	}
	for i, want := range files {
		a := atts[i].(map[string]any)
		dl := e.get("/jmap/download/" + e.acct.ID + "/" + a["blobId"].(string) + "/x")
		if dl.Code != 200 || !bytes.Equal(dl.Body.Bytes(), want) {
			t.Errorf("file %d: %d %q, want %q", i, dl.Code, dl.Body.String(), want)
		}
		if a["size"].(float64) != float64(len(want)) {
			t.Errorf("file %d: size %v, want %d", i, a["size"], len(want))
		}
	}
	// The message is still 7-bit clean lines of at most 998 octets.
	for _, l := range bytes.Split(e.raw(created["id"].(string)), []byte("\r\n")) {
		if len(l) > 998 || bytes.ContainsAny(l, "\r\n") {
			t.Errorf("a line of %d octets, or a bare line end: %q", len(l), l)
		}
	}

	// A text file as the whole body, without a line end at its end.
	one := e.create(`{"mailboxIds": {"m2": true}, "from": [{"name": null, "email": "` + e.acct.Address + `"}], "subject": "one file",
		"bodyStructure": {"blobId": "` + e.upload([]byte("just this"), "text/plain") + `", "type": "text/plain"}}`)
	got = e.one("Email/get", `{"accountId": "$A", "ids": ["`+one["id"].(string)+`"], "properties": ["bodyStructure"], "bodyProperties": ["blobId"]}`)
	if dl := e.get("/jmap/download/" + e.acct.ID + "/" + at(t, got, "/list/0/bodyStructure/blobId").(string) + "/x"); dl.Body.String() != "just this" {
		t.Errorf("a file as the body: %q", dl.Body.String())
	}
}

// RFC 8621 4.6: what a creation may not have.
func TestRFC8621_4_6_CreateRefusals(t *testing.T) {
	e := newEnv(t)
	const body = `"textBody": [{"partId": "t", "type": "text/plain"}], "bodyValues": {"t": {"value": "x"}}`
	missing := strings.Repeat("ab", 32)
	for name, tc := range map[string]struct {
		obj   string
		typ   string
		props string // the properties named, joined; "" is not checked
	}{
		"no mailbox":            {`{` + body + `}`, "invalidProperties", "mailboxIds"},
		"empty mailboxIds":      {`{"mailboxIds": {}, ` + body + `}`, "invalidProperties", "mailboxIds"},
		"an unknown mailbox":    {`{"mailboxIds": {"m99": true}, ` + body + `}`, "invalidProperties", "mailboxIds"},
		"a keyword set false":   {`{"mailboxIds": {"m2": true}, "keywords": {"$seen": false}, ` + body + `}`, "invalidProperties", "keywords"},
		"an invalid keyword":    {`{"mailboxIds": {"m2": true}, "keywords": {"with space": true}, ` + body + `}`, "invalidProperties", "keywords"},
		"server-set properties": {`{"mailboxIds": {"m2": true}, "id": "e1", "size": 3, "threadId": "t1", ` + body + `}`, "invalidProperties", "id size threadId"},
		// "The headers property MUST NOT be given on either the top-level
		// Email or an EmailBodyPart".
		"headers":         {`{"mailboxIds": {"m2": true}, "headers": [{"name": "X", "value": " y"}], ` + body + `}`, "invalidProperties", "headers"},
		"headers on part": {`{"mailboxIds": {"m2": true}, "textBody": [{"partId": "t", "headers": []}], "bodyValues": {"t": {"value": "x"}}}`, "invalidProperties", "textBody/0/headers"},
		// "header fields MUST NOT be specified more than once".
		"a field twice":      {`{"mailboxIds": {"m2": true}, "subject": "a", "header:Subject:asText": "b", ` + body + `}`, "invalidProperties", "subject"},
		"a field twice, raw": {`{"mailboxIds": {"m2": true}, "from": [{"name": null, "email": "a@b.test"}], "header:From": " x@y.test", ` + body + `}`, "invalidProperties", "header:From"},
		// "MUST NOT include [...] Content-* headers [at the top level]".
		"Content-Type on top": {`{"mailboxIds": {"m2": true}, "header:Content-Type": " text/x", ` + body + `}`, "invalidProperties", "header:Content-Type"},
		// "Content-Transfer-Encoding MUST NOT be given".
		"a transfer encoding": {`{"mailboxIds": {"m2": true}, "textBody": [{"partId": "t", "header:Content-Transfer-Encoding": " base64"}], "bodyValues": {"t": {"value": "x"}}}`,
			"invalidProperties", "textBody/0/header:Content-Transfer-Encoding"},
		"a form the field cannot have": {`{"mailboxIds": {"m2": true}, "header:Subject:asAddresses": [], ` + body + `}`, "invalidProperties", "header:Subject:asAddresses"},
		"a value of the wrong type":    {`{"mailboxIds": {"m2": true}, "subject": ["a"], ` + body + `}`, "invalidProperties", "subject"},
		"a header that breaks out":     {`{"mailboxIds": {"m2": true}, "header:X-A": " a\r\nBcc: victim@x.test", ` + body + `}`, "invalidProperties", "header:X-A"},
		"an address that breaks out":   {`{"mailboxIds": {"m2": true}, "to": [{"name": null, "email": "a@b.test>\r\nBcc: <v@x.test"}], ` + body + `}`, "invalidProperties", "to"},
		"an unknown property":          {`{"mailboxIds": {"m2": true}, "colour": "red", ` + body + `}`, "invalidProperties", "colour"},
		// "textBody [...] MUST contain exactly one body part and it MUST
		// be of type text/plain"; bodyStructure excludes the three lists.
		"two text bodies":       {`{"mailboxIds": {"m2": true}, "textBody": [{"partId": "t"}, {"partId": "t"}], "bodyValues": {"t": {"value": "x"}}}`, "invalidProperties", "textBody"},
		"an html textBody":      {`{"mailboxIds": {"m2": true}, "textBody": [{"partId": "t", "type": "text/html"}], "bodyValues": {"t": {"value": "x"}}}`, "invalidProperties", "textBody"},
		"structure and a list":  {`{"mailboxIds": {"m2": true}, "bodyStructure": {"partId": "t"}, ` + body + `}`, "invalidProperties", "textBody"},
		"partId without value":  {`{"mailboxIds": {"m2": true}, "textBody": [{"partId": "nope"}]}`, "invalidProperties", "textBody/0/partId"},
		"partId and blobId":     {`{"mailboxIds": {"m2": true}, "textBody": [{"partId": "t", "blobId": "` + missing + `"}], "bodyValues": {"t": {"value": "x"}}}`, "invalidProperties", "textBody/0/partId"},
		"neither":               {`{"mailboxIds": {"m2": true}, "textBody": [{"type": "text/plain"}]}`, "invalidProperties", "textBody/0/partId"},
		"a charset with partId": {`{"mailboxIds": {"m2": true}, "textBody": [{"partId": "t", "charset": "latin1"}], "bodyValues": {"t": {"value": "x"}}}`, "invalidProperties", "textBody/0/charset"},
		// "isEncodingProblem [...] isTruncated: MUST be false".
		"a truncated value":       {`{"mailboxIds": {"m2": true}, "textBody": [{"partId": "t"}], "bodyValues": {"t": {"value": "x", "isTruncated": true}}}`, "invalidProperties", ""},
		"multipart without parts": {`{"mailboxIds": {"m2": true}, "bodyStructure": {"type": "multipart/mixed"}}`, "invalidProperties", "bodyStructure/subParts"},
		"a leaf with subParts":    {`{"mailboxIds": {"m2": true}, "bodyStructure": {"partId": "t", "subParts": []}, "bodyValues": {"t": {"value": "x"}}}`, "invalidProperties", "bodyStructure/subParts"},
		"not an object":           {`"an email"`, "invalidProperties", ""},
	} {
		se := e.refused(tc.obj)
		props := ""
		if list, ok := se["properties"].([]any); ok {
			var names []string
			for _, p := range list {
				names = append(names, p.(string))
			}
			props = strings.Join(names, " ")
		}
		if se["type"] != tc.typ || tc.props != "" && props != tc.props {
			t.Errorf("%s: %v with properties %q, want %s with %q", name, se["type"], props, tc.typ, tc.props)
		}
	}
	// "blobNotFound: At least one blob id given for an EmailBodyPart
	// doesn't exist. An extra notFound property of type Id[] MUST be
	// included".
	se := e.refused(`{"mailboxIds": {"m2": true}, "attachments": [{"blobId": "` + missing + `", "type": "a/b"}, {"blobId": "zz", "type": "a/b"}], ` + body + `}`)
	same(t, "blobNotFound", se, `{"type": "blobNotFound", "notFound": ["`+missing+`", "zz"]}`)
	// A size that is not the blob's.
	blob := e.upload([]byte("12345"), "text/plain")
	se = e.refused(`{"mailboxIds": {"m2": true}, "attachments": [{"blobId": "` + blob + `", "type": "a/b", "size": 4}], ` + body + `}`)
	same(t, "wrong size", se["properties"], `["attachments/0/size"]`)
	// Nothing was stored for any of them.
	if q := e.one("Email/query", `{"accountId": "$A"}`); len(q["ids"].([]any)) != 0 {
		t.Errorf("emails exist after refused creations: %v", q["ids"])
	}
	// ifInState holds for a creation too.
	if got := e.fail("Email/set", `{"accountId": "$A", "ifInState": "999", "create": {"c": {"mailboxIds": {"m2": true}, `+body+`}}}`); got != "stateMismatch" {
		t.Errorf("ifInState: %s", got)
	}
	// A mailbox made in the same request can be named by its creation id.
	got := e.do(`["Mailbox/set", {"accountId": "$A", "create": {"box": {"name": "Projects", "parentId": null}}}, "0"],
		["Email/set", {"accountId": "$A", "create": {"c": {"mailboxIds": {"#box": true}, ` + body + `}}}, "1"]`)
	if at(t, got, "/1/1/created/c/id") == nil {
		t.Errorf("creation into a new mailbox: %v", at(t, got, "/1/1"))
	}
}

// RFC 8621 4.8: Email/import.
func TestRFC8621_4_8_Import(t *testing.T) {
	e := newEnv(t)
	msg := simple("imported@x", "Anna <anna@x.test>", "An old message", "Date: Tue, 1 Jul 2003 10:52:37 +0200\r\n")
	blob := e.upload(msg, "message/rfc822")
	before := e.state()
	got := e.one("Email/import", `{"accountId": "$A", "ifInState": "`+before+`", "emails": {
		"one": {"blobId": "`+blob+`", "mailboxIds": {"m1": true, "m5": true}, "keywords": {"$seen": true, "old": true}, "receivedAt": "2003-07-01T08:52:40Z"},
		"gone": {"blobId": "`+strings.Repeat("cd", 32)+`", "mailboxIds": {"m1": true}},
		"nobox": {"blobId": "`+blob+`", "mailboxIds": {"m77": true}},
		"extra": {"blobId": "`+blob+`", "mailboxIds": {"m1": true}, "subject": "no"}}}`)
	same(t, "response members", []any{got["accountId"], got["oldState"]}, fmt.Sprintf(`[%q, %q]`, e.acct.ID, before))
	if got["newState"] == before {
		t.Error("newState did not move")
	}
	same(t, "created", at(t, got, "/created/one/blobId"), `"`+blob+`"`)
	same(t, "notCreated", got["notCreated"], `{
		"gone": {"type": "blobNotFound", "notFound": ["`+strings.Repeat("cd", 32)+`"]},
		"nobox": {"type": "invalidProperties", "description": "one of the mailboxes does not exist", "properties": ["mailboxIds"]},
		"extra": {"type": "invalidProperties", "description": "an import has blobId, mailboxIds, keywords and receivedAt", "properties": ["subject"]}}`)
	id := at(t, got, "/created/one/id").(string)
	em := at(t, e.one("Email/get", `{"accountId": "$A", "ids": ["`+id+`"], "properties": ["mailboxIds", "keywords", "receivedAt", "subject", "sentAt", "size"]}`), "/list/0")
	same(t, "the email", em, fmt.Sprintf(`{"id": %q, "mailboxIds": {"m1": true, "m5": true}, "keywords": {"$seen": true, "old": true},
		"receivedAt": "2003-07-01T08:52:40Z", "subject": "An old message", "sentAt": "2003-07-01T10:52:37+02:00", "size": %d}`, id, len(msg)))
	if !bytes.Equal(e.raw(id), msg) {
		t.Error("the imported message is not the blob")
	}
	// "invalidEmail: the blob [...] is not a valid message".
	junk := e.upload([]byte("\x00\x01\x02 no header here"), "application/octet-stream")
	got = e.one("Email/import", `{"accountId": "$A", "emails": {"j": {"blobId": "`+junk+`", "mailboxIds": {"m1": true}}}}`)
	same(t, "not a message", at(t, got, "/notCreated/j/type"), `"invalidEmail"`)
	if got := e.fail("Email/import", `{"accountId": "$A", "ifInState": "1", "emails": {"j": {"blobId": "`+blob+`", "mailboxIds": {"m1": true}}}}`); got != "stateMismatch" {
		t.Errorf("ifInState: %s", got)
	}
	// A message inside another one can be imported by its part's blob id.
	outer := e.deliver(store.Message{Raw: []byte("From: a@x.test\r\nSubject: fwd\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nsee\r\n--b\r\nContent-Type: message/rfc822\r\n\r\n" + string(msg) + "\r\n--b--\r\n")})
	got = e.one("Email/import", `{"accountId": "$A", "emails": {"inner": {"blobId": "`+PartBlobID(outer.Blob, "2")+`", "mailboxIds": {"m1": true}}}}`)
	if inner := at(t, got, "/created/inner/id"); inner == nil || !bytes.Equal(bytes.TrimRight(e.raw(inner.(string)), "\r\n"), bytes.TrimRight(msg, "\r\n")) {
		t.Errorf("import of an attached message: %v", got)
	}
}

// RFC 8621 4.7 with RFC 8620 5.4: Email/copy. A user has one account, so
// it is offered within the account: see emailCopy for why that departs
// from "MUST be different".
func TestRFC8621_4_7_CopyWithinTheAccount(t *testing.T) {
	e := newEnv(t)
	orig := e.deliver(store.Message{Raw: simple("orig@x", "a@x.test", "to copy"), Keywords: []string{"$seen", "$flagged"}})
	before := e.state()
	got := e.do(`["Email/copy", {"fromAccountId": "$A", "accountId": "$A", "ifFromInState": "` + before + `", "create": {
			"same": {"id": "` + eid(orig) + `"},
			"moved": {"id": "` + eid(orig) + `", "mailboxIds": {"m3": true}, "keywords": {"copied": true}, "receivedAt": "2020-01-02T03:04:05Z"},
			"missing": {"id": "e9999"},
			"bad": {"id": "` + eid(orig) + `", "subject": "x"}},
		"onSuccessDestroyOriginal": true}, "c1"]`)
	if len(got) != 2 || at(t, got, "/0/0") != "Email/copy" || at(t, got, "/1/0") != "Email/set" || at(t, got, "/1/2") != "c1" {
		b, _ := json.Marshal(got)
		t.Fatalf("responses: %s", b)
	}
	res := at(t, got, "/0/1").(map[string]any)
	same(t, "accounts", []any{res["fromAccountId"], res["accountId"], res["oldState"]}, fmt.Sprintf(`[%q, %q, %q]`, e.acct.ID, e.acct.ID, before))
	same(t, "notCreated", []any{at(t, res, "/notCreated/missing/type"), at(t, res, "/notCreated/bad/type")}, `["notFound", "invalidProperties"]`)
	// "created: [...] the id [...] and the server-set properties"; the
	// message, and so the blob, is the same.
	same(t, "blob", []any{at(t, res, "/created/same/blobId"), at(t, res, "/created/moved/blobId")}, fmt.Sprintf(`[%q, %q]`, orig.Blob, orig.Blob))
	sameID, movedID := at(t, res, "/created/same/id").(string), at(t, res, "/created/moved/id").(string)
	if sameID == eid(orig) || movedID == eid(orig) || sameID == movedID {
		t.Errorf("ids: %s %s %s", eid(orig), sameID, movedID)
	}
	// RFC 8620 5.4: "any other properties included are used instead of
	// the current value for that property on the original".
	list := e.one("Email/get", `{"accountId": "$A", "ids": ["`+sameID+`", "`+movedID+`", "`+eid(orig)+`"], "properties": ["mailboxIds", "keywords", "receivedAt"]}`)
	same(t, "copy with nothing changed", pick(at(t, list, "/list/0").(map[string]any), []string{"mailboxIds", "keywords"}),
		`{"id": "`+sameID+`", "mailboxIds": {"m1": true}, "keywords": {"$seen": true, "$flagged": true}}`)
	same(t, "copy with its own placement", at(t, list, "/list/1"),
		`{"id": "`+movedID+`", "mailboxIds": {"m3": true}, "keywords": {"copied": true}, "receivedAt": "2020-01-02T03:04:05Z"}`)
	// onSuccessDestroyOriginal: "an implicit Foo/set call", whose response
	// follows; the original is gone.
	same(t, "the implied destroy", at(t, got, "/1/1/destroyed"), `["`+eid(orig)+`"]`)
	same(t, "notFound", list["notFound"], `["`+eid(orig)+`"]`)

	if got := e.fail("Email/copy", `{"fromAccountId": "another", "accountId": "$A", "create": {}}`); got != "fromAccountNotFound" {
		t.Errorf("another fromAccountId: %s", got)
	}
	if got := e.fail("Email/copy", `{"fromAccountId": "$A", "accountId": "$A", "ifFromInState": "1", "create": {}}`); got != "stateMismatch" {
		t.Errorf("ifFromInState: %s", got)
	}
	// Without onSuccessDestroyOriginal there is one response.
	if got := e.do(`["Email/copy", {"fromAccountId": "$A", "accountId": "$A", "create": {"again": {"id": "` + sameID + `"}}}, "c2"]`); len(got) != 1 {
		t.Errorf("%d responses", len(got))
	}
}

// RFC 8621 4.9: Email/parse.
func TestRFC8621_4_9_Parse(t *testing.T) {
	e := newEnv(t)
	inner := "From: Anna <anna@x.test>\r\nTo: mike@y.test\r\nSubject: =?utf-8?q?Caf=C3=A9?=\r\nDate: Tue, 1 Jul 2003 10:52:37 +0200\r\nMessage-ID: <inner@x.test>\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=in\r\n\r\n--in\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nThe inner text.\r\n" +
		"--in\r\nContent-Type: application/pdf; name=a.pdf\r\nContent-Disposition: attachment; filename=a.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERg==\r\n--in--\r\n"
	outer := e.deliver(store.Message{Raw: []byte("From: mike@y.test\r\nSubject: Fwd\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=out\r\n\r\n" +
		"--out\r\nContent-Type: text/plain\r\n\r\nSee below.\r\n--out\r\nContent-Type: message/rfc822\r\n\r\n" + inner + "\r\n--out--\r\n")})
	attached := PartBlobID(outer.Blob, "2")
	uploaded := e.upload([]byte(inner), "message/rfc822")
	missing := strings.Repeat("ef", 32)
	junk := e.upload([]byte("\x00\x01 not a message"), "application/octet-stream")

	got := e.one("Email/parse", `{"accountId": "$A", "blobIds": ["`+attached+`", "`+uploaded+`", "`+missing+`", "`+junk+`"],
		"properties": ["id", "blobId", "threadId", "mailboxIds", "keywords", "receivedAt", "size", "from", "subject", "sentAt", "messageId",
			"hasAttachment", "preview", "textBody", "attachments", "bodyValues"],
		"bodyProperties": ["partId", "blobId", "type", "name", "size"], "fetchTextBodyValues": true}`)
	same(t, "notFound and notParsable", []any{got["notFound"], got["notParsable"]}, `[["`+missing+`"], ["`+junk+`"]]`)
	for _, blobID := range []string{attached, uploaded} {
		em := at(t, got, "/parsed/"+blobID).(map[string]any)
		// "The following metadata properties on the Email objects will be
		// null if requested: id, mailboxIds, keywords, receivedAt";
		// threadId may be.
		same(t, "metadata of "+blobID[:8], pick(em, []string{"blobId", "threadId", "mailboxIds", "keywords", "receivedAt"}),
			`{"id": null, "blobId": "`+blobID+`", "threadId": null, "mailboxIds": null, "keywords": null, "receivedAt": null}`)
		same(t, "header", []any{em["from"], em["subject"], em["sentAt"], em["messageId"], em["hasAttachment"], em["preview"]},
			`[[{"name": "Anna", "email": "anna@x.test"}], "Café", "2003-07-01T10:52:37+02:00", ["inner@x.test"], true, "The inner text."]`)
		same(t, "text", at(t, em, "/bodyValues/1/value"), `"The inner text."`)
		// The parts of a parsed message have blob ids of their own, which
		// download.
		same(t, "attachment", at(t, em, "/attachments/0"), `{"partId": "2", "blobId": "`+blobID+`-2", "type": "application/pdf", "name": "a.pdf", "size": 4}`)
		if dl := e.get("/jmap/download/" + e.acct.ID + "/" + blobID + "-2/a.pdf"); dl.Code != 200 || dl.Body.String() != "%PDF" {
			t.Errorf("part of a parsed message: %d %q", dl.Code, dl.Body.String())
		}
	}
	// The default properties are those of 4.9.
	def := at(t, e.one("Email/parse", `{"accountId": "$A", "blobIds": ["`+uploaded+`"]}`), "/parsed/"+uploaded).(map[string]any)
	for _, p := range []string{"messageId", "inReplyTo", "references", "sender", "from", "to", "cc", "bcc", "replyTo", "subject", "sentAt",
		"hasAttachment", "preview", "bodyValues", "textBody", "htmlBody", "attachments"} {
		if _, ok := def[p]; !ok {
			t.Errorf("the default properties lack %s", p)
		}
	}
	if len(def) != 17 {
		t.Errorf("%d default properties", len(def))
	}
	same(t, "nothing parsed", pick(e.one("Email/parse", `{"accountId": "$A", "blobIds": ["`+missing+`"]}`), []string{"parsed", "notParsable"}),
		`{"id": null, "parsed": null, "notParsable": null}`)
	for args, want := range map[string]string{
		`{"accountId": "$A"}`: "invalidArguments",
		`{"accountId": "$A", "blobIds": ["` + uploaded + `"], "properties": ["colour"]}`:     "invalidArguments",
		`{"accountId": "$A", "blobIds": ["` + uploaded + `"], "bodyProperties": ["colour"]}`: "invalidArguments",
		`{"accountId": "other", "blobIds": []}`:                                              "accountNotFound",
	} {
		if got := e.fail("Email/parse", args); got != want {
			t.Errorf("Email/parse %s: %s, want %s", args, got, want)
		}
	}
}
