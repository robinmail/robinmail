package jmap

// What Fastmail's JMAP-TestSuite found when it ran against robinmail in
// the interoperability lab (contrib/lab/jmapsuite), kept here as tests
// that need neither Perl nor Docker. Each names the text it holds to.

import (
	"encoding/json"
	"strings"
	"testing"
)

// RFC 8621 4.6: "The client may specify a partId OR a blobId": nothing
// keeps a value from bodyValues to text types. The suite attaches
// {"name":"thing"} as image/jpeg; an invitation as text/calendar or JSON
// as application/json are the real cases. It was refused before.
func TestCreatePartFromAValueOfAnyType(t *testing.T) {
	e := newEnv(t)
	created := e.create(`{"mailboxIds": {"m2": true}, "subject": "a value as an attachment",
		"textBody": [{"partId": "text"}],
		"attachments": [{"partId": "att", "type": "application/json", "name": "thing.json", "disposition": "attachment", "cid": "fooz", "language": ["en"]}],
		"bodyValues": {"text": {"value": "this is a text part"}, "att": {"value": "{\"name\":\"thing\"}"}}}`)
	got := e.one("Email/get", `{"accountId": "$A", "ids": ["`+created["id"].(string)+`"], "fetchAllBodyValues": true,
		"properties": ["attachments", "bodyValues", "textBody"], "bodyProperties": ["partId", "type", "name", "cid", "disposition", "language", "size"]}`)
	same(t, "the attachment", at(t, got, "/list/0/attachments"),
		`[{"partId": "2", "type": "application/json", "name": "thing.json", "cid": "fooz", "disposition": "attachment", "language": ["en"], "size": 16}]`)
	same(t, "the text", at(t, got, "/list/0/bodyValues/1/value"), `"this is a text part"`)
}

// RFC 8621 4.6: "The size property MUST be omitted if a partId is given.
// If a blobId is given, it may be included but is ignored by the server
// (the size is actually calculated from the blob content itself)." A wrong
// size was refused.
func TestCreateIgnoresTheSizeOfABlobPart(t *testing.T) {
	e := newEnv(t)
	blob := e.upload([]byte("12345"), "text/plain")
	created := e.create(`{"mailboxIds": {"m2": true},
		"attachments": [{"blobId": "` + blob + `", "type": "a/b", "size": 4}],
		"textBody": [{"partId": "t", "type": "text/plain"}], "bodyValues": {"t": {"value": "x"}}}`)
	got := e.one("Email/get", `{"accountId": "$A", "ids": ["`+created["id"].(string)+`"], "properties": ["attachments"], "bodyProperties": ["size"]}`)
	same(t, "the size is the blob's", at(t, got, "/list/0/attachments"), `[{"size": 5}]`)
	// With a partId it stays refused.
	se := e.refused(`{"mailboxIds": {"m2": true}, "textBody": [{"partId": "t", "size": 1}], "bodyValues": {"t": {"value": "x"}}}`)
	same(t, "size with a partId", se["properties"], `["textBody/0/size"]`)
}

// A body value comes back as it was given: a text without a line end at
// its end gained one when it was the only part of the message (the suite's
// text-body, html-body and body-structure tests; the webmail had to drop
// the line end of a reopened draft). The message still ends in CRLF: the
// text is quoted-printable and its last line ends in a soft line break
// (RFC 2045 6.7), which is no part of the text.
func TestCreateKeepsABodyValueAsItIs(t *testing.T) {
	e := newEnv(t)
	for name, value := range map[string]string{
		"no line end":              "this is a text part",
		"one line end":             "this is a text part\n",
		"two lines, no line end":   "first\nsecond",
		"not ASCII":                "Grüße, 日本語",
		"a full line of 76":        strings.Repeat("x", 76),
		"a full line, 75 and =3D":  strings.Repeat("x", 73) + "=",
		"two full lines":           strings.Repeat("y", 152),
		"long":                     strings.Repeat("0123456789 ", 40),
		"ends in a space":          "trailing ",
		"empty":                    "",
		"only an equals sign":      "=",
		"a line that is one point": "a\n.\nb",
	} {
		for _, kind := range []string{"textBody", "htmlBody", "bodyStructure"} {
			typ := "text/plain"
			if kind == "htmlBody" {
				typ = "text/html"
			}
			part := `[{"partId": "t", "type": "` + typ + `"}]`
			if kind == "bodyStructure" {
				part = `{"partId": "t", "type": "text/plain"}`
			}
			// Line ends are LF in a value, both ways (RFC 8621 4.1.4).
			quoted := strings.ReplaceAll(value, "\n", `\n`)
			created := e.create(`{"mailboxIds": {"m2": true}, "` + kind + `": ` + part + `, "bodyValues": {"t": {"value": "` + quoted + `"}}}`)
			id := created["id"].(string)
			got := e.one("Email/get", `{"accountId": "$A", "ids": ["`+id+`"], "fetchAllBodyValues": true, "properties": ["bodyValues"]}`)
			if v := at(t, got, "/list/0/bodyValues/1/value"); v != value {
				t.Errorf("%s as %s: read back %q, want %q", name, kind, v, value)
			}
			raw := string(e.raw(id))
			if !strings.HasSuffix(raw, "\r\n") {
				t.Errorf("%s as %s: the message does not end in CRLF", name, kind)
			}
			for _, line := range strings.Split(raw, "\r\n") {
				if len(line) > 78 {
					t.Errorf("%s as %s: a line of %d characters: %q", name, kind, len(line), line)
				}
			}
		}
	}
}

// RFC 8621 4.2: maxBodyValueBytes is an UnsignedInt. null, a string, a
// fraction and anything else are invalidArguments; null passed for 0.
func TestMaxBodyValueBytesIsAnUnsignedInt(t *testing.T) {
	e := newEnv(t)
	created := e.create(`{"mailboxIds": {"m2": true}, "textBody": [{"partId": "t"}], "bodyValues": {"t": {"value": "0123456789"}}}`)
	id := created["id"].(string)
	for _, bad := range []string{`-5`, `"cat"`, `"1"`, `{}`, `[]`, `true`, `null`, `1.5`, `1e3`} {
		if typ := e.fail("Email/get", `{"accountId": "$A", "ids": ["`+id+`"], "properties": ["bodyValues"], "maxBodyValueBytes": `+bad+`}`); typ != "invalidArguments" {
			t.Errorf("maxBodyValueBytes %s: %s, want invalidArguments", bad, typ)
		}
	}
	got := e.one("Email/get", `{"accountId": "$A", "ids": ["`+id+`"], "properties": ["bodyValues"], "fetchAllBodyValues": true, "maxBodyValueBytes": 4}`)
	same(t, "cut at 4", at(t, got, "/list/0/bodyValues/1"), `{"value": "0123", "isEncodingProblem": false, "isTruncated": true}`)
	got = e.one("Email/get", `{"accountId": "$A", "ids": ["`+id+`"], "properties": ["bodyValues"], "fetchAllBodyValues": true, "maxBodyValueBytes": 0}`)
	same(t, "0 is no limit", at(t, got, "/list/0/bodyValues/1/value"), `"0123456789"`)
}

// RFC 8621 4.8: "If the blobId, mailboxIds, or keywords properties are
// invalid (e.g., missing, wrong type, id not found), the server MUST
// reject the import with an invalidProperties SetError." A blob that is
// not there got blobNotFound, which is Email/set's (4.6), and of two
// missing properties only one was named.
func TestImportNamesWhatIsWrong(t *testing.T) {
	e := newEnv(t)
	blob := e.upload(simple("imp@x", "Anna <anna@x.test>", "x"), "message/rfc822")
	got := e.one("Email/import", `{"accountId": "$A", "emails": {
		"nothing": {},
		"noblob": {"mailboxIds": {"m1": true}},
		"nobox": {"blobId": "`+blob+`"},
		"gone": {"blobId": "foo", "mailboxIds": {"m1": true}},
		"keywords": {"blobId": "`+blob+`", "mailboxIds": {"m1": true}, "keywords": "foo"}}}`)
	for cid, props := range map[string]string{
		"nothing":  `["blobId", "mailboxIds"]`,
		"noblob":   `["blobId"]`,
		"nobox":    `["mailboxIds"]`,
		"gone":     `["blobId"]`,
		"keywords": `["keywords"]`,
	} {
		same(t, cid+": type", at(t, got, "/notCreated/"+cid+"/type"), `"invalidProperties"`)
		same(t, cid+": properties", at(t, got, "/notCreated/"+cid+"/properties"), props)
	}
}

// RFC 8620 5.3: "Any server-set properties MAY be included in the patch
// if their value is identical to the current server value": a client may
// get a Mailbox, change its name and send the whole object back. That was
// refused. A value that differs still is, and each right is named.
func TestMailboxUpdateWithTheWholeObject(t *testing.T) {
	e := newEnv(t)
	made := e.one("Mailbox/set", `{"accountId": "$A", "create": {"a": {"name": "Projects", "sortOrder": 53}}}`)
	id := at(t, made, "/created/a/id").(string)
	whole := func() string {
		got := e.one("Mailbox/get", `{"accountId": "$A", "ids": ["`+id+`"]}`)
		return string(marshal(t, at(t, got, "/list/0")))
	}
	obj := strings.Replace(whole(), `"name":"Projects"`, `"name":"A Newer Name"`, 1)
	if !strings.Contains(obj, `"A Newer Name"`) || !strings.Contains(obj, `"myRights"`) || !strings.Contains(obj, `"totalEmails"`) {
		t.Fatalf("the object: %s", obj)
	}
	set := e.one("Mailbox/set", `{"accountId": "$A", "update": {"`+id+`": `+obj+`}}`)
	same(t, "updated", set["updated"], `{"`+id+`": null}`)
	same(t, "not refused", set["notUpdated"], `null`)
	if !strings.Contains(whole(), `"name":"A Newer Name"`) || !strings.Contains(whole(), `"sortOrder":53`) {
		t.Errorf("after the update: %s", whole())
	}

	// The same for a mailbox with a role, whose role is server-set here.
	inbox := string(marshal(t, at(t, e.one("Mailbox/get", `{"accountId": "$A", "ids": ["m1"]}`), "/list/0")))
	set = e.one("Mailbox/set", `{"accountId": "$A", "update": {"m1": `+strings.Replace(inbox, `"sortOrder":`, `"sortOrder":7`, 1)+`}}`)
	same(t, "a role mailbox", set["notUpdated"], `null`)

	// Values that differ are refused, by name.
	changed := strings.NewReplacer(`"totalEmails":0`, `"totalEmails":52`, `"mayRename":true`, `"mayRename":false`, `"mayDelete":true`, `"mayDelete":false`).Replace(whole())
	changed = strings.Replace(changed, `"id":"`+id+`"`, `"id":"m1"`, 1)
	set = e.one("Mailbox/set", `{"accountId": "$A", "update": {"`+id+`": `+changed+`}}`)
	same(t, "refused", at(t, set, "/notUpdated/"+id+"/type"), `"invalidProperties"`)
	same(t, "what differs", at(t, set, "/notUpdated/"+id+"/properties"), `["id", "myRights/mayDelete", "myRights/mayRename", "totalEmails"]`)

	// On a create nothing server-set is the client's to give.
	set = e.one("Mailbox/set", `{"accountId": "$A", "create": {"b": {"name": "B", "id": "m99", "totalEmails": 1, "myRights": {"mayRename": false, "mayDelete": true}}}}`)
	same(t, "create", at(t, set, "/notCreated/b/properties"), `["id", "myRights/mayDelete", "myRights/mayRename", "totalEmails"]`)
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A header given in Raw form without the space that by convention follows
// the colon gets it: "header:Foo": "cat" is written as "Foo: cat" and reads
// back as " cat". One that has its own white space is left as it is.
func TestCreateRawHeaderGetsItsSpace(t *testing.T) {
	e := newEnv(t)
	created := e.create(`{"mailboxIds": {"m2": true}, "header:X-Foo:all": ["cat", " dog", "\tmouse"], "header:X-Bar": "one",
		"textBody": [{"partId": "t"}], "bodyValues": {"t": {"value": "x\n"}}}`)
	id := created["id"].(string)
	got := e.one("Email/get", `{"accountId": "$A", "ids": ["`+id+`"], "properties": ["header:X-Foo:all", "header:X-Bar"]}`)
	same(t, "all", at(t, got, "/list/0/header:X-Foo:all"), `[" cat", " dog", "\tmouse"]`)
	same(t, "one", at(t, got, "/list/0/header:X-Bar"), `" one"`)
	if raw := string(e.raw(id)); !strings.Contains(raw, "\r\nX-Foo: cat\r\nX-Foo: dog\r\nX-Foo:\tmouse\r\n") {
		t.Errorf("the message:\n%s", raw)
	}
}
