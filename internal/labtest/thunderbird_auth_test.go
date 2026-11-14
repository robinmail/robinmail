package labtest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// refusals counts the logins robinmail refused Thunderbird's container
// since a moment, IMAP and SMTP, and the submission sessions it ended
// because their login was withdrawn, from the traces.
func refusals(t testing.TB, since time.Time) (imap, smtp, ended int) {
	t.Helper()
	out := compose(t, "logs", "--no-log-prefix", "--since", since.UTC().Format(time.RFC3339), "robinmail")
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, tbIP) {
			continue
		}
		switch {
		case strings.Contains(l, `msg="imap >"`) && strings.Contains(l, "AUTHENTICATIONFAILED"):
			imap++
		case strings.Contains(l, `msg="smtp trace"`) && strings.Contains(l, `line="535`):
			smtp++
		case strings.Contains(l, `msg="smtp trace"`) && strings.Contains(l, `line="421 4.7.0`):
			ended++
		}
	}
	return imap, smtp, ended
}

// wantAlert checks that a script ended in a dialog of Thunderbird, and
// closes the dialog as Cancel does.
func (x *tb) wantAlert(what string, err error) string {
	x.t.Helper()
	var a *alertError
	if !errors.As(err, &a) {
		x.t.Fatalf("%s: Thunderbird shows no dialog (%v); its windows:\n%s", what, err, x.windows())
	}
	x.dismiss()
	return strings.Join(strings.Fields(a.text), " ")
}

// TestThunderbirdAppPassword is the exit of phase 7 in Thunderbird.
//
//  1. An account without app passwords: Thunderbird logs in with the
//     account password, for IMAP and for SMTP.
//  2. The user makes an app password in the webmail. robinmail ends the
//     account's IMAP sessions; Thunderbird logs in again with the password
//     it has stored, is refused, and tells the user.
//  3. With the app password stored instead it syncs and sends.
//  4. The app password is revoked: the sessions end again, the next login
//     is refused and Thunderbird says so.
//  5. No app password is left, so the account password works once more.
func TestThunderbirdAppPassword(t *testing.T) {
	lab(t)
	defer cell(t, "Thunderbird: app password: account password refused, app password works, revoke ends it", tbCol)("")
	user := labUser(t, "tb-app")
	other := labUser(t, "tb-app-other")
	j := newJMAP(t, user)
	boxes := j.mailboxes(t)
	j.seed(t, boxes, []seedEmail{{raw: syncMessage("p01", "Before the app password", "plain"), mailboxes: []string{"INBOX"}}})

	// 1.
	x := startThunderbird(t, user, password())
	x.sync()
	if n := compareFolders(t, x, j); n != 1 {
		t.Fatalf("%d messages with the account password", n)
	}
	tok := newToken()
	x.run(`await send({to: arguments[0], subject: "With the account password " + arguments[1], body: "Sent with the account password.\n"});`, other, tok)
	jo := newJMAP(t, other)

	// 2. At once: Thunderbird keeps the submission connection of that
	// message open for about five seconds, to send the next one on it.
	web := newWeb(t, user)
	smtpSince := time.Now().Add(-time.Second)
	var app struct{ ID, Secret string }
	if status, data := web.do(t, "POST", "/auth/app-passwords", map[string]any{"name": "Thunderbird", "scopes": []string{"imap", "smtp"}, "password": password()}); status != 201 {
		t.Fatalf("creating the app password: %d %s", status, data)
	} else if err := json.Unmarshal(data, &app); err != nil || app.Secret == "" {
		t.Fatalf("the answer: %s", data)
	}
	// The harness is a client too: the account password no longer opens
	// JMAP for it, so it gets an app password of its own. Two of them:
	// revoking Thunderbird's must not need the other to go.
	var mine struct{ ID, Secret string }
	if status, data := web.do(t, "POST", "/auth/app-passwords", map[string]any{"name": "harness", "scopes": []string{"jmap"}, "password": password()}); status != 201 {
		t.Fatalf("creating the harness's app password: %d %s", status, data)
	} else if err := json.Unmarshal(data, &mine); err != nil || mine.Secret == "" {
		t.Fatalf("the answer: %s", data)
	}
	j.secret = mine.Secret
	// robinmail ended that submission session: it had logged in with a
	// password that is no longer a way in.
	waitFor(t, "robinmail to end Thunderbird's open submission session", 10*time.Second, func() bool {
		_, _, ended := refusals(t, smtpSince)
		return ended > 0
	})
	jo.find(t, tok) // what was sent before is delivered all the same
	since := time.Now().Add(-time.Second)
	j.seed(t, boxes, []seedEmail{{raw: syncMessage("p02", "After the app password", "plain"), mailboxes: []string{"INBOX"}}})
	said := x.wantAlert("the account password, once the account has an app password", x.try(nil, `await update("INBOX");`))
	t.Logf("Thunderbird, refused with the account password: %q", said)
	if !strings.Contains(said, "failed") || !strings.Contains(said, robinHost) {
		t.Errorf("the dialog does not say that the login to %s failed", robinHost)
	}
	if n, _, _ := refusals(t, since); n == 0 {
		t.Errorf("robinmail's trace shows no refused IMAP login")
	}
	if _, ok := x.messages("INBOX")["p02@postfix.test"]; ok {
		t.Errorf("Thunderbird got mail without a valid password")
	}
	// Sending is refused as well.
	tok2 := newToken()
	err := x.tryFor(40*time.Second, nil, `await send({to: arguments[0], subject: "Refused " + arguments[1], body: "Must not arrive.\n"});`, other, tok2)
	said = x.wantAlert("sending with the account password", err)
	t.Logf("Thunderbird, sending with the account password: %q", said)
	if _, refused, _ := refusals(t, since); refused == 0 {
		t.Errorf("robinmail's trace shows no refused SMTP login")
	}
	// The compose window is still there with the message; close it.
	x.try(nil, `for (const w of Services.wm.getEnumerator("msgcompose")) { w.gMsgCompose.bodyModified = false; w.gContentChanged = false; w.close(); }`)
	if text, ok := x.alert(); ok {
		t.Logf("closing the compose window asks: %q", strings.Join(strings.Fields(text), " "))
		x.dismiss()
	}
	jo.absent(t, tok2, 2*time.Second)

	// 3.
	x.run(`await login(arguments[0]);`, app.Secret)
	x.sync()
	if n := compareFolders(t, x, j); n < 2 {
		t.Fatalf("%d messages with the app password", n)
	}
	tok3 := newToken()
	x.run(`for (const w of Services.wm.getEnumerator("msgcompose")) { w.close(); }
		await send({to: arguments[0], subject: "With the app password " + arguments[1], body: "Sent with the app password.\n"});`, other, tok3)

	// 4. Again at once, while the submission connection is open.
	smtpSince = time.Now().Add(-time.Second)
	if status, data := web.do(t, "DELETE", "/auth/app-passwords/"+app.ID, nil); status != 204 {
		t.Fatalf("revoke: %d %s", status, data)
	}
	waitFor(t, "robinmail to end the submission session of the revoked app password", 10*time.Second, func() bool {
		_, _, ended := refusals(t, smtpSince)
		return ended > 0
	})
	jo.find(t, tok3)
	since = time.Now().Add(-time.Second)
	j.seed(t, boxes, []seedEmail{{raw: syncMessage("p03", "After the revoke", "plain"), mailboxes: []string{"INBOX"}}})
	said = x.wantAlert("the revoked app password", x.try(nil, `await update("INBOX");`))
	t.Logf("Thunderbird, refused with the revoked app password: %q", said)
	if n, _, _ := refusals(t, since); n == 0 {
		t.Errorf("robinmail's trace shows no refused IMAP login after the revoke")
	}
	if _, ok := x.messages("INBOX")["p03@postfix.test"]; ok {
		t.Errorf("Thunderbird got mail with a revoked app password")
	}

	tok4 := newToken()
	err = x.tryFor(40*time.Second, nil, `await send({to: arguments[0], subject: "Revoked " + arguments[1], body: "Must not arrive.\n"});`, other, tok4)
	said = x.wantAlert("sending with the revoked app password", err)
	t.Logf("Thunderbird, sending with the revoked app password: %q", said)
	x.try(nil, `for (const w of Services.wm.getEnumerator("msgcompose")) { w.gMsgCompose.bodyModified = false; w.gContentChanged = false; w.close(); }`)
	if _, ok := x.alert(); ok {
		x.dismiss()
	}
	x.try(nil, `for (const w of Services.wm.getEnumerator("msgcompose")) { w.close(); }`)
	jo.absent(t, tok4, 2*time.Second)

	// 5. Still refused with the account password while the harness's app
	// password exists; with that one gone too it is the way in again.
	x.run(`await login(arguments[0]);`, password())
	x.wantAlert("the account password while another app password exists", x.try(nil, `await update("INBOX");`))
	if status, data := web.do(t, "DELETE", "/auth/app-passwords/"+mine.ID, nil); status != 204 {
		t.Fatalf("revoke: %d %s", status, data)
	}
	j.secret = ""
	x.run(`await login(arguments[0]);`, password())
	x.sync()
	if n := compareFolders(t, x, j); n < 3 {
		t.Fatalf("%d messages with the account password after the revoke", n)
	}
}

// TestThunderbirdReconnect restarts robinmail under a Thunderbird that is
// idle in the Inbox. Thunderbird has to find its way back by itself at
// the next thing the user does, with the password it has, and see what
// arrived meanwhile once and only once.
func TestThunderbirdReconnect(t *testing.T) {
	lab(t)
	defer cell(t, "Thunderbird: reconnects after robinmail restarts", tbCol)("")
	user := labUser(t, "tb-re")
	j := newJMAP(t, user)
	boxes := j.mailboxes(t)
	j.seed(t, boxes, []seedEmail{
		{raw: syncMessage("r01", "Before the restart 1", "plain"), mailboxes: []string{"INBOX"}},
		{raw: syncMessage("r02", "Before the restart 2", "utf8"), mailboxes: []string{"INBOX"}}})
	x := startThunderbird(t, user, password())
	x.sync()
	compareFolders(t, x, j)
	waitFor(t, "Thunderbird to idle", 30*time.Second, func() bool {
		lines, _ := imapTrace(t, time.Now().Add(-time.Minute), user)
		return len(sent(lines, "IDLE")) > 0
	})

	restartRobinmail(t)
	since := time.Now().Add(-time.Second)
	waitFor(t, "JMAP to answer again", 60*time.Second, func() bool {
		status, _ := j.doErr("GET", "/jmap/session", "", nil)
		return status == 200
	})
	j.seed(t, boxes, []seedEmail{{raw: syncMessage("r03", "After the restart", "plain"), mailboxes: []string{"INBOX"}}})

	// The next click: Thunderbird notices the dead connection and makes a
	// new one. It may take it a try.
	x.waitMessages("the message that arrived after the restart", "INBOX", true, 60*time.Second, func(m map[string]tbMessage) bool {
		_, ok := m["r03@postfix.test"]
		return ok
	})
	if n := compareFolders(t, x, j); n != 3 {
		t.Errorf("%d messages after the reconnect", n)
	}
	// And it works both ways again.
	x.run(`markFlagged("INBOX", [arguments[0]], true);`, "r03@postfix.test")
	j.waitEmails(t, "the flag set after the reconnect", func(e map[string]jmapEmail) bool { return e["r03@postfix.test"].keywords["$flagged"] })
	lines, logins := imapTrace(t, since, user)
	t.Logf("after the restart: %d new IMAP logins; %s", logins, strings.Join(verbs(lines), ", "))
	if logins == 0 {
		t.Errorf("no new IMAP login after the restart")
	}
}
