package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/store"
)

// Tests of Identity (RFC 8621 6) and EmailSubmission (RFC 8621 7).

// RFC 8621 6 and 6.1: the Identity object and Identity/get. Every account
// has an identity for its own address, which cannot be deleted.
func TestRFC8621_6_Identity(t *testing.T) {
	e := newEnv(t)
	got := e.one("Identity/get", `{"accountId": "$A", "ids": null}`)
	same(t, "the first identity", got["list"], `[{"id": "i1", "name": "", "email": "`+e.acct.Address+`", "replyTo": null, "bcc": null,
		"textSignature": "", "htmlSignature": "", "mayDelete": false}]`)
	same(t, "state", got["state"], `"`+e.state()+`"`)
	before := e.state()
	local, domain, _ := strings.Cut(e.acct.Address, "@")

	// 6.3: Identity/set. The example of the section sets a signature.
	got = e.one("Identity/set", `{"accountId": "$A", "ifInState": "`+before+`", "create": {
		"work": {"name": "Joe Bloggs", "email": "`+local+`+work@`+domain+`", "replyTo": [{"name": null, "email": "team@example.com"}],
			"bcc": [{"name": "Archive", "email": "archive@example.com"}],
			"textSignature": "-- \nJoe", "htmlSignature": "<p>Joe</p>"},
		"boss": {"name": "The Boss", "email": "boss@`+domain+`"},
		"other": {"email": "me@elsewhere.example"},
		"noemail": {"name": "x"},
		"extra": {"email": "`+e.acct.Address+`", "mayDelete": false},
		"badreply": {"email": "`+e.acct.Address+`", "replyTo": [{"name": null, "email": "not an address"}]}}}`)
	same(t, "created", got["created"], `{"work": {"id": "i2", "mayDelete": true}}`)
	// "forbiddenFrom: The user is not allowed to send from the address
	// given as the email property of the Identity."
	same(t, "notCreated", []any{at(t, got, "/notCreated/boss/type"), at(t, got, "/notCreated/other/type"), at(t, got, "/notCreated/noemail/type"),
		at(t, got, "/notCreated/extra/properties"), at(t, got, "/notCreated/badreply/properties")},
		`["forbiddenFrom", "forbiddenFrom", "invalidProperties", ["mayDelete"], ["replyTo"]]`)

	// An alias that delivers to the account is an address to send from
	// too, with or without a tag; another alias is not.
	e.h.SendAs = func(_ context.Context, username string) ([]string, error) {
		if username != e.acct.Address {
			t.Errorf("SendAs asked for %q", username)
		}
		return []string{"office@" + domain}, nil
	}
	alias := e.one("Identity/set", `{"accountId": "$A", "create": {
		"office": {"email": "Office@`+domain+`"}, "tagged": {"email": "office+shop@`+domain+`"}, "boss": {"email": "boss@`+domain+`"}}}`)
	same(t, "alias identities", []any{at(t, alias, "/created/office/id"), at(t, alias, "/created/tagged/id"), at(t, alias, "/notCreated/boss/type")},
		`["i3", "i4", "forbiddenFrom"]`)
	e.one("Identity/set", `{"accountId": "$A", "destroy": ["i3", "i4"]}`)
	e.h.SendAs = nil

	// Update: everything but the email ("email: immutable").
	got = e.one("Identity/set", `{"accountId": "$A", "update": {
		"i2": {"name": "Joe B.", "replyTo": null, "textSignature": "-- \nJoe B."},
		"i1": {"email": "someone@else.example"},
		"i9": {"name": "x"}, "nonsense": {"name": "x"}}}`)
	same(t, "updated", got["updated"], `{"i2": null}`)
	same(t, "notUpdated", []any{at(t, got, "/notUpdated/i1/type"), at(t, got, "/notUpdated/i1/properties"), at(t, got, "/notUpdated/i9/type"), at(t, got, "/notUpdated/nonsense/type")},
		`["invalidProperties", ["email"], "notFound", "notFound"]`)
	got = e.one("Identity/get", `{"accountId": "$A", "ids": ["i2", "i7"], "properties": ["name", "replyTo", "bcc", "textSignature", "htmlSignature"]}`)
	same(t, "after the update", got["list"], `[{"id": "i2", "name": "Joe B.", "replyTo": null, "bcc": [{"name": "Archive", "email": "archive@example.com"}],
		"textSignature": "-- \nJoe B.", "htmlSignature": "<p>Joe</p>"}]`)
	same(t, "notFound", got["notFound"], `["i7"]`)

	// 6.2: Identity/changes.
	ch := e.one("Identity/changes", `{"accountId": "$A", "sinceState": "`+before+`"}`)
	same(t, "changes", []any{ch["created"], ch["updated"], ch["destroyed"], ch["newState"]}, `[["i2"], [], [], "`+e.state()+`"]`)

	// Destroy: "mayDelete: false" means the server says no.
	got = e.one("Identity/set", `{"accountId": "$A", "destroy": ["i2", "i1", "i9"]}`)
	same(t, "destroyed", got["destroyed"], `["i2"]`)
	same(t, "notDestroyed", []any{at(t, got, "/notDestroyed/i1/type"), at(t, got, "/notDestroyed/i9/type")}, `["forbidden", "notFound"]`)

	if got := e.fail("Identity/set", `{"accountId": "$A", "ifInState": "1", "destroy": ["i1"]}`); got != "stateMismatch" {
		t.Errorf("ifInState: %s", got)
	}
	if got := e.fail("Identity/get", `{"accountId": "$A", "ids": null, "properties": ["password"]}`); got != "invalidArguments" {
		t.Errorf("unknown property: %s", got)
	}
	// RFC 8620 3.3: the methods exist only for a request that uses the
	// capability.
	req := `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Identity/get", {"accountId": "` + e.acct.ID + `"}, "0"]]}`
	if got := at(t, responsesAs(t, e, req), "/0/1/type"); got != "unknownMethod" {
		t.Errorf("Identity/get without the submission capability: %v", got)
	}
}

func responsesAs(t *testing.T, e *env, body string) []any {
	t.Helper()
	rec := e.post("/jmap/api", []byte(body), "application/json")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	return at(t, decode(t, rec.Body.Bytes()), "/methodResponses").([]any)
}

const draftBody = `"textBody": [{"partId": "t", "type": "text/plain"}], "bodyValues": {"t": {"value": "I have the most brilliant plan."}}`

// RFC 8621 7.5.1, the example: one request creates a draft, submits it
// and, when that worked, moves it from Drafts to Sent and removes $draft.
func TestRFC8621_7_5_1_Example(t *testing.T) {
	e := newEnv(t)
	before := e.state()
	got := e.do(`["Email/set", {"accountId": "$A", "create": {"k192": {
			"mailboxIds": {"m2": true}, "keywords": {"$seen": true, "$draft": true},
			"from": [{"name": "Joe Bloggs", "email": "` + e.acct.Address + `"}],
			"to": [{"name": "John", "email": "john@example.com"}],
			"subject": "World domination", ` + draftBody + `}}}, "0"],
		["EmailSubmission/set", {"accountId": "$A", "create": {"k1490": {
			"identityId": "i1", "emailId": "#k192",
			"envelope": {"mailFrom": {"email": "` + e.acct.Address + `", "parameters": null},
				"rcptTo": [{"email": "john@example.com", "parameters": null}]}}},
			"onSuccessUpdateEmail": {"#k1490": {"mailboxIds/m2": null, "mailboxIds/m3": true, "keywords/$draft": null}}}, "1"]`)
	b, _ := json.Marshal(got)
	t.Logf("responses: %s", b)
	// "The response to the EmailSubmission/set call, [then] an Email/set
	// response" with the same method call id.
	if len(got) != 3 || at(t, got, "/1/0") != "EmailSubmission/set" || at(t, got, "/2/0") != "Email/set" || at(t, got, "/2/2") != "1" {
		t.Fatalf("responses: %s", b)
	}
	emailID := at(t, got, "/0/1/created/k192/id").(string)
	created := at(t, got, "/1/1/created/k1490").(map[string]any)
	subID := created["id"].(string)
	// The server-set properties of a new submission.
	same(t, "created", pick(created, []string{"threadId", "undoStatus", "deliveryStatus", "dsnBlobIds", "mdnBlobIds"}), `{
		"id": "`+subID+`", "threadId": "`+at(t, got, "/0/1/created/k192/threadId").(string)+`", "undoStatus": "pending",
		"deliveryStatus": {"john@example.com": {"smtpReply": "", "delivered": "queued", "displayed": "unknown"}},
		"dsnBlobIds": [], "mdnBlobIds": []}`)
	if _, err := time.Parse(time.RFC3339, created["sendAt"].(string)); err != nil {
		t.Errorf("sendAt %v", created["sendAt"])
	}
	same(t, "the implied Email/set", at(t, got, "/2/1/updated"), `{"`+emailID+`": null}`)
	em := at(t, e.one("Email/get", `{"accountId": "$A", "ids": ["`+emailID+`"], "properties": ["mailboxIds", "keywords"]}`), "/list/0")
	same(t, "the email afterwards", em, `{"id": "`+emailID+`", "mailboxIds": {"m3": true}, "keywords": {"$seen": true}}`)

	// 7.1: EmailSubmission/get.
	sub := at(t, e.one("EmailSubmission/get", `{"accountId": "$A", "ids": ["`+subID+`", "s99"]}`), "/list/0").(map[string]any)
	same(t, "the submission", pick(sub, []string{"identityId", "emailId", "envelope", "undoStatus"}), `{
		"id": "`+subID+`", "identityId": "i1", "emailId": "`+emailID+`", "undoStatus": "pending",
		"envelope": {"mailFrom": {"email": "`+e.acct.Address+`", "parameters": null}, "rcptTo": [{"email": "john@example.com", "parameters": null}]}}`)
	if len(sub) != 10 {
		t.Errorf("an EmailSubmission has %d properties: %v", len(sub), sub)
	}
	// 7.2: EmailSubmission/changes.
	ch := e.one("EmailSubmission/changes", `{"accountId": "$A", "sinceState": "`+before+`"}`)
	same(t, "changes", []any{ch["created"], ch["updated"], ch["destroyed"]}, `[["`+subID+`"], [], []]`)

	// What goes out is the message of the Email.
	queued, err := e.st.Submissions(e.ctx, e.acct.ID, nil)
	if err != nil || len(queued) != 1 {
		t.Fatalf("%d submissions, %v", len(queued), err)
	}
	raw, _ := e.st.SubmissionMessage(e.ctx, queued[0])
	if string(raw) != string(e.raw(emailID)) {
		t.Error("the queued message is not the email")
	}
}

// RFC 8621 7: undoStatus and deliveryStatus, as the queue changes them.
func TestRFC8621_7_UndoAndDeliveryStatus(t *testing.T) {
	e := newEnv(t)
	mk := func(to string) (emailID, subID string) {
		t.Helper()
		got := e.do(`["Email/set", {"accountId": "$A", "create": {"d": {"mailboxIds": {"m2": true},
				"from": [{"name": null, "email": "` + e.acct.Address + `"}], "to": [` + to + `], "subject": "s", ` + draftBody + `}}}, "0"],
			["EmailSubmission/set", {"accountId": "$A", "create": {"s": {"identityId": "i1", "emailId": "#d"}}}, "1"]`)
		if at(t, got, "/1/1/created") == nil {
			b, _ := json.Marshal(got)
			t.Fatalf("not submitted: %s", b)
		}
		return at(t, got, "/0/1/created/d/id").(string), at(t, got, "/1/1/created/s/id").(string)
	}
	status := func(subID string) map[string]any {
		t.Helper()
		return at(t, e.one("EmailSubmission/get", `{"accountId": "$A", "ids": ["`+subID+`"], "properties": ["undoStatus", "deliveryStatus"]}`), "/list/0").(map[string]any)
	}

	// "canceled: The email submission was canceled and will not be
	// delivered"; the only update a client can make.
	_, first := mk(`{"name": null, "email": "a@x.test"}`)
	got := e.one("EmailSubmission/set", `{"accountId": "$A", "update": {
		"`+first+`": {"undoStatus": "canceled"}, "s999": {"undoStatus": "canceled"}}}`)
	same(t, "cancel", []any{got["updated"], at(t, got, "/notUpdated/s999/type")}, `[{"`+first+`": null}, "notFound"]`)
	same(t, "after cancel", status(first)["undoStatus"], `"canceled"`)
	// "cannotUnsend: The client attempted to update the undoStatus of a
	// valid EmailSubmission object from pending to canceled, but the
	// message cannot be unsent."
	got = e.one("EmailSubmission/set", `{"accountId": "$A", "update": {"`+first+`": {"undoStatus": "canceled"}}}`)
	same(t, "cancel twice", at(t, got, "/notUpdated/"+first+"/type"), `"cannotUnsend"`)
	for patch, props := range map[string]string{
		`{"undoStatus": "final"}`:   `["undoStatus"]`,
		`{"undoStatus": "pending"}`: `["undoStatus"]`,
		`{"emailId": "e1"}`:         `["emailId"]`,
		`{"sendAt": "2030-01-01T00:00:00Z", "undoStatus": "canceled"}`: `["sendAt"]`,
	} {
		got := e.one("EmailSubmission/set", `{"accountId": "$A", "update": {"`+first+`": `+patch+`}}`)
		same(t, "update "+patch, []any{at(t, got, "/notUpdated/"+first+"/type"), at(t, got, "/notUpdated/"+first+"/properties")}, `["invalidProperties", `+props+`]`)
	}

	// A node takes the message: undoStatus is final from then on, and
	// each recipient gets its deliveryStatus.
	_, second := mk(`{"name": null, "email": "local@here.test"}, {"name": null, "email": "remote@x.test"}, {"name": null, "email": "gone@x.test"}, {"name": null, "email": "later@slow.test"}`)
	id, _ := store.ParseSubmissionID(second)
	ref := store.SubmissionRef{Account: e.acct.ID, ID: id}
	due := e.st.Now().Add(2 * time.Hour)
	before := e.state()
	claimed, err := e.st.ClaimSubmission(e.ctx, ref, "node:1", due, due.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	same(t, "claimed", status(second)["undoStatus"], `"final"`)
	got = e.one("EmailSubmission/set", `{"accountId": "$A", "update": {"`+second+`": {"undoStatus": "canceled"}}}`)
	same(t, "cancel after the claim", at(t, got, "/notUpdated/"+second+"/type"), `"cannotUnsend"`)
	claimed.Recipients[0] = store.Recipient{Address: "local@here.test", State: store.RcptDelivered, Reply: "250 2.0.0 delivered to the mailbox", Mailbox: true}
	claimed.Recipients[1] = store.Recipient{Address: "remote@x.test", State: store.RcptDelivered, Reply: "250 2.0.0 OK: queued as 4F9B"}
	claimed.Recipients[2] = store.Recipient{Address: "gone@x.test", State: store.RcptFailed, Reply: "550 5.1.1 No such user"}
	claimed.Recipients[3] = store.Recipient{Address: "later@slow.test", State: store.RcptQueued, Reply: "451 4.7.1 Greylisted"}
	if err := e.st.SaveSubmission(e.ctx, claimed, "node:1", true, due.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// "delivered: queued [...] yes: The message was successfully
	// delivered to the mail store of the recipient [...] no [...]
	// unknown: The final delivery status is unknown, (e.g., it was
	// relayed to an external machine and no further information is
	// available)"; "smtpReply: The SMTP reply string returned for this
	// recipient when the server last tried to relay the message".
	same(t, "deliveryStatus", status(second)["deliveryStatus"], `{
		"local@here.test": {"smtpReply": "250 2.0.0 delivered to the mailbox", "delivered": "yes", "displayed": "unknown"},
		"remote@x.test": {"smtpReply": "250 2.0.0 OK: queued as 4F9B", "delivered": "unknown", "displayed": "unknown"},
		"gone@x.test": {"smtpReply": "550 5.1.1 No such user", "delivered": "no", "displayed": "unknown"},
		"later@slow.test": {"smtpReply": "451 4.7.1 Greylisted", "delivered": "queued", "displayed": "unknown"}}`)
	// The client hears of it through /changes.
	ch := e.one("EmailSubmission/changes", `{"accountId": "$A", "sinceState": "`+before+`"}`)
	same(t, "changes", []any{ch["created"], ch["updated"]}, `[[], ["`+second+`"]]`)

	// 7.5: destroying a submission that is still being delivered removes
	// the record, not the message from the queue.
	got = e.one("EmailSubmission/set", `{"accountId": "$A", "destroy": ["`+second+`", "`+first+`", "s999"]}`)
	same(t, "destroy", []any{got["destroyed"], at(t, got, "/notDestroyed/s999/type")}, `[["`+second+`", "`+first+`"], "notFound"]`)
	same(t, "after destroy", e.one("EmailSubmission/get", `{"accountId": "$A", "ids": null}`)["list"], `[]`)
	if row, err := e.st.Submission(e.ctx, e.acct.ID, id); err != nil || row.Queue != store.QueueActive {
		t.Errorf("the queue lost a destroyed submission with work left: %+v %v", row, err)
	}
}

// RFC 8621 7.5: the envelope a submission gets when none is given, and
// what a creation is refused for.
func TestRFC8621_7_5_EnvelopeAndRefusals(t *testing.T) {
	e := newEnv(t)
	mail := func(headers string) string {
		t.Helper()
		em := e.deliver(store.Message{Raw: []byte(headers + "Subject: s\r\nMessage-ID: <" + fmt.Sprint(time.Now().UnixNano()) + "@x>\r\n\r\nbody\r\n"), Role: store.RoleDrafts})
		return eid(em)
	}
	submit := func(obj string) (created, notCreated map[string]any) {
		t.Helper()
		got := e.one("EmailSubmission/set", `{"accountId": "$A", "create": {"s": `+obj+`}}`)
		if got["created"] != nil {
			return at(t, got, "/created/s").(map[string]any), nil
		}
		return nil, at(t, got, "/notCreated/s").(map[string]any)
	}
	from := "From: " + e.acct.Address + "\r\n"

	// "mailFrom: The email address in the Sender header field, if
	// present; otherwise, it's the email address in the From header
	// field [...] rcptTo: The deduplicated set of email addresses from
	// the To, Cc, and Bcc header fields".
	id := mail(from + "To: Anna <anna@x.test>, bob@x.test\r\nCc: ANNA@x.test, carol@y.test\r\nBcc: dave@z.test\r\n")
	created, _ := submit(`{"identityId": "i1", "emailId": "` + id + `"}`)
	if created == nil {
		t.Fatal("not created")
	}
	sub := at(t, e.one("EmailSubmission/get", `{"accountId": "$A", "ids": ["`+created["id"].(string)+`"], "properties": ["envelope"]}`), "/list/0/envelope")
	same(t, "derived envelope", sub, `{"mailFrom": {"email": "`+e.acct.Address+`", "parameters": null}, "rcptTo": [
		{"email": "anna@x.test", "parameters": null}, {"email": "bob@x.test", "parameters": null},
		{"email": "carol@y.test", "parameters": null}, {"email": "dave@z.test", "parameters": null}]}`)
	// A Sender, when there is one, is the envelope sender.
	local, domain, _ := strings.Cut(e.acct.Address, "@")
	id = mail(from + "Sender: " + local + "+assistant@" + domain + "\r\nTo: anna@x.test\r\n")
	created, _ = submit(`{"identityId": "i1", "emailId": "` + id + `"}`)
	sub = at(t, e.one("EmailSubmission/get", `{"accountId": "$A", "ids": ["`+created["id"].(string)+`"]}`), "/list/0/envelope/mailFrom/email")
	same(t, "Sender", sub, `"`+local+`+assistant@`+domain+`"`)
	// An envelope with the null reverse-path, as a read receipt has.
	created, _ = submit(`{"identityId": "i1", "emailId": "` + id + `", "envelope": {"mailFrom": {"email": "", "parameters": null}, "rcptTo": [{"email": "x@y.test", "parameters": null}]}}`)
	if created == nil {
		t.Error("the null reverse-path is refused")
	}

	okMail := mail(from + "To: anna@x.test\r\n")
	for name, tc := range map[string]struct{ obj, typ, props string }{
		// "noRecipients: The envelope [...] has no recipients."
		"no recipients":   {`{"identityId": "i1", "emailId": "` + mail(from) + `"}`, "noRecipients", ""},
		"an empty rcptTo": {`{"identityId": "i1", "emailId": "` + okMail + `", "envelope": {"mailFrom": {"email": "` + e.acct.Address + `"}, "rcptTo": []}}`, "noRecipients", ""},
		// "If multiple addresses are present in one of these header
		// fields [...] the server SHOULD reject the EmailSubmission as
		// invalid": invalidEmail, with the properties at fault.
		"two authors":           {`{"identityId": "i1", "emailId": "` + mail("From: "+e.acct.Address+", other@x.test\r\nTo: anna@x.test\r\n") + `"}`, "invalidEmail", "from"},
		"no From":               {`{"identityId": "i1", "emailId": "` + mail("To: anna@x.test\r\n") + `"}`, "invalidEmail", "from"},
		"an unknown email":      {`{"identityId": "i1", "emailId": "e9999"}`, "invalidProperties", "emailId"},
		"an unknown identity":   {`{"identityId": "i77", "emailId": "` + okMail + `"}`, "invalidProperties", "identityId"},
		"no identity":           {`{"emailId": "` + okMail + `"}`, "invalidProperties", "identityId"},
		"server-set properties": {`{"identityId": "i1", "emailId": "` + okMail + `", "undoStatus": "final", "sendAt": "2030-01-01T00:00:00Z"}`, "invalidProperties", "sendAt undoStatus"},
		// "forbiddenMailFrom: The server does not permit the user to
		// send a message with the envelope From address".
		"someone else's envelope": {`{"identityId": "i1", "emailId": "` + okMail + `", "envelope": {"mailFrom": {"email": "boss@` + domain + `"}, "rcptTo": [{"email": "a@x.test"}]}}`, "forbiddenMailFrom", ""},
		// submissionExtensions is empty, so no parameter can be asked for.
		"an envelope parameter":        {`{"identityId": "i1", "emailId": "` + okMail + `", "envelope": {"mailFrom": {"email": "` + e.acct.Address + `", "parameters": {"HOLDFOR": "60"}}, "rcptTo": [{"email": "a@x.test"}]}}`, "invalidProperties", "envelope"},
		"an envelope without mailFrom": {`{"identityId": "i1", "emailId": "` + okMail + `", "envelope": {"rcptTo": [{"email": "a@x.test"}]}}`, "invalidProperties", "envelope"},
	} {
		_, se := submit(tc.obj)
		if se == nil {
			t.Errorf("%s: created", name)
			continue
		}
		props := ""
		if list, ok := se["properties"].([]any); ok {
			for _, p := range list {
				props = strings.TrimSpace(props + " " + p.(string))
			}
		}
		if se["type"] != tc.typ || props != tc.props {
			t.Errorf("%s: %v %q, want %s %q", name, se["type"], props, tc.typ, tc.props)
		}
	}
	// What the sending side refuses comes back as the SetError it names,
	// with the extra properties RFC 8621 7.5 gives each.
	for _, refuse := range []*SendError{
		{Type: "forbiddenFrom", Description: "the From field has an address that is not of this account"},
		{Type: "tooManyRecipients", MaxRecipients: 100},
		{Type: "invalidRecipients", InvalidRecipients: []string{"anna at home"}},
		{Type: "tooLarge", MaxSize: 26214400},
		{Type: "forbiddenToSend", Description: "Gtube pattern"},
	} {
		e.refuse = refuse
		_, se := submit(`{"identityId": "i1", "emailId": "` + okMail + `"}`)
		want := map[string]any{"type": refuse.Type}
		if refuse.Description != "" {
			want["description"] = refuse.Description
		}
		if refuse.MaxRecipients != 0 {
			want["maxRecipients"] = refuse.MaxRecipients
		}
		if refuse.InvalidRecipients != nil {
			want["invalidRecipients"] = refuse.InvalidRecipients
		}
		if refuse.MaxSize != 0 {
			want["maxSize"] = refuse.MaxSize
		}
		b, _ := json.Marshal(want)
		same(t, refuse.Type, se, string(b))
	}
	e.refuse = nil
	// Nothing is changed on an Email whose submission was refused.
	got := e.do(`["EmailSubmission/set", {"accountId": "$A", "create": {"s": {"identityId": "i77", "emailId": "` + okMail + `"}},
		"onSuccessUpdateEmail": {"#s": {"keywords/$draft": null}}, "onSuccessDestroyEmail": ["#s"]}, "0"]`)
	if len(got) != 1 {
		t.Errorf("an implied Email/set after a refused submission: %d responses", len(got))
	}
	// onSuccessDestroyEmail: for a message nobody keeps a copy of.
	got = e.do(`["EmailSubmission/set", {"accountId": "$A", "create": {"s": {"identityId": "i1", "emailId": "` + okMail + `"}},
		"onSuccessDestroyEmail": ["#s"]}, "0"]`)
	if len(got) != 2 || at(t, got, "/1/0") != "Email/set" {
		t.Fatalf("responses %v", got)
	}
	same(t, "destroyed", at(t, got, "/1/1/destroyed"), `["`+okMail+`"]`)
	// The queue still has the message: it holds its own copy.
	subID, _ := store.ParseSubmissionID(at(t, got, "/0/1/created/s/id").(string))
	row, _ := e.st.Submission(e.ctx, e.acct.ID, subID)
	if raw, err := e.st.SubmissionMessage(e.ctx, row); err != nil || !strings.Contains(string(raw), "Subject: s") {
		t.Errorf("the queued message after its Email was destroyed: %v", err)
	}
	if got := e.fail("EmailSubmission/set", `{"accountId": "$A", "ifInState": "1", "create": {"s": {"identityId": "i1", "emailId": "`+okMail+`"}}}`); got != "stateMismatch" {
		t.Errorf("ifInState: %s", got)
	}
}

// RFC 8621 7.3 and 7.4: EmailSubmission/query and /queryChanges.
func TestRFC8621_7_3_Query(t *testing.T) {
	e := newEnv(t)
	clock := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	e.st.Now = func() time.Time { return clock }
	local, domain, _ := strings.Cut(e.acct.Address, "@")
	e.one("Identity/set", `{"accountId": "$A", "create": {"w": {"email": "`+local+`+work@`+domain+`"}}}`)
	var subs, emails, threads []string
	for i, identity := range []string{"i1", "i2", "i1"} {
		refs := ""
		if i == 2 {
			refs = "References: <q0@x>\r\n" // the third is in the thread of the first
		}
		em := e.deliver(store.Message{Raw: []byte(fmt.Sprintf("From: %s\r\nTo: anna@x.test\r\nSubject: q%d\r\nMessage-ID: <q%d@x>\r\n%s\r\nbody\r\n", e.acct.Address, i, i, refs)), Role: store.RoleDrafts})
		got := e.one("EmailSubmission/set", `{"accountId": "$A", "create": {"s": {"identityId": "`+identity+`", "emailId": "`+eid(em)+`"}}}`)
		subs = append(subs, at(t, got, "/created/s/id").(string))
		emails, threads = append(emails, eid(em)), append(threads, tid(em))
		clock = clock.Add(time.Hour)
	}
	state := e.state()
	e.one("EmailSubmission/set", `{"accountId": "$A", "update": {"`+subs[1]+`": {"undoStatus": "canceled"}}}`)
	q := func(args string) string {
		t.Helper()
		got := e.one("EmailSubmission/query", `{"accountId": "$A"`+args+`}`)
		b, _ := json.Marshal(got["ids"])
		return string(b)
	}
	list := func(ids ...int) string {
		out := []string{}
		for _, i := range ids {
			out = append(out, subs[i])
		}
		b, _ := json.Marshal(out)
		return string(b)
	}
	for args, want := range map[string]string{
		``:                                    list(0, 1, 2),
		`, "filter": {"identityIds": ["i2"]}`: list(1),
		`, "filter": {"emailIds": ["` + emails[0] + `", "` + emails[2] + `"]}`: list(0, 2),
		`, "filter": {"threadIds": ["` + threads[0] + `"]}`:                    list(0, 2),
		`, "filter": {"undoStatus": "pending"}`:                                list(0, 2),
		`, "filter": {"undoStatus": "canceled"}`:                               list(1),
		// sendAt is the submission time plus the hour in which to cancel.
		`, "filter": {"before": "2026-11-14T10:00:00Z"}`:                                                               list(0),
		`, "filter": {"after": "2026-11-14T10:00:00Z"}`:                                                                list(1, 2),
		`, "filter": {"operator": "NOT", "conditions": [{"identityIds": ["i1"]}]}`:                                     list(1),
		`, "filter": {"operator": "OR", "conditions": [{"identityIds": ["i2"]}, {"emailIds": ["` + emails[2] + `"]}]}`: list(1, 2),
		`, "sort": [{"property": "sentAt", "isAscending": false}]`:                                                     list(2, 1, 0),
		`, "sort": [{"property": "threadId"}, {"property": "emailId", "isAscending": false}]`:                          list(2, 0, 1),
		`, "position": 1, "limit": 1`:                                                                                  list(1),
	} {
		if got := q(args); got != want {
			t.Errorf("query%s: %s, want %s", args, got, want)
		}
	}
	got := e.one("EmailSubmission/query", `{"accountId": "$A", "calculateTotal": true, "limit": 2}`)
	same(t, "the response", pick(got, []string{"queryState", "canCalculateChanges", "position", "total"}),
		`{"id": null, "queryState": "`+e.state()+`", "canCalculateChanges": true, "position": 0, "total": 3}`)
	for args, want := range map[string]string{
		`{"accountId": "$A", "filter": {"colour": "red"}}`:       "invalidArguments",
		`{"accountId": "$A", "filter": {"before": "yesterday"}}`: "invalidArguments",
		`{"accountId": "$A", "sort": [{"property": "subject"}]}`: "unsupportedSort",
		`{"accountId": "$A", "anchor": "s99"}`:                   "anchorNotFound",
	} {
		if got := e.fail("EmailSubmission/query", args); got != want {
			t.Errorf("EmailSubmission/query %s: %s, want %s", args, got, want)
		}
	}
	// 7.4: what changed in a query since a state.
	ch := e.one("EmailSubmission/queryChanges", `{"accountId": "$A", "sinceQueryState": "`+state+`", "filter": {"undoStatus": "pending"}}`)
	same(t, "queryChanges", []any{ch["removed"], ch["added"], ch["newQueryState"]}, `[["`+subs[1]+`"], [], "`+e.state()+`"]`)
	ch = e.one("EmailSubmission/queryChanges", `{"accountId": "$A", "sinceQueryState": "`+state+`"}`)
	same(t, "queryChanges without a filter", []any{ch["removed"], ch["added"]}, `[["`+subs[1]+`"], [{"id": "`+subs[1]+`", "index": 1}]]`)
}

// A server that does not send (no Send) says so for every submission.
func TestSubmissionWithoutSending(t *testing.T) {
	e := newEnv(t)
	e.h.Send = nil
	em := e.deliver(store.Message{Raw: []byte("From: " + e.acct.Address + "\r\nTo: a@x.test\r\n\r\nx\r\n")})
	got := e.one("EmailSubmission/set", `{"accountId": "$A", "create": {"s": {"identityId": "i1", "emailId": "`+eid(em)+`"}}}`)
	same(t, "refused", at(t, got, "/notCreated/s/type"), `"forbiddenToSend"`)
}
