package labtest

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real Thunderbird against robinmail. The container "thunderbird" of
// the lab has the program and nothing running; a test writes a profile
// with one IMAP account (993, TLS) and one SMTP server (465, TLS) for a
// user of robin.test, starts Thunderbird on it without a display and
// drives it over Marionette (marionette_test.go) with the functions of
// thunderbird.js, which call Thunderbird's own mail interfaces. Every
// case looks at both sides: what Thunderbird shows, and what robinmail
// has over JMAP; and at robinmail's IMAP and SMTP traces for what
// Thunderbird put on the wire.

// tbFolder is a folder as Thunderbird's folder pane has it.
type tbFolder struct {
	Path    string
	Name    string
	Special []string
	Total   int
	Unread  int
}

// tbMessage is a row of Thunderbird's message list.
type tbMessage struct {
	UID        int
	ID         string
	Subject    string
	From       string
	To         string
	Size       int
	Read       bool
	Flagged    bool
	Answered   bool
	Forwarded  bool
	Deleted    bool
	Attachment bool
	Keywords   []string
}

// tb is a running Thunderbird with one account.
type tb struct {
	t       *testing.T
	user    string
	profile string
	m       *marionette
}

var (
	tbLibOnce sync.Once
	tbLib     string
	tbSerial  int
)

// alertError is a dialog Thunderbird opened while a script ran.
type alertError struct{ text string }

func (e *alertError) Error() string { return "Thunderbird shows a dialog: " + e.text }

// tbExec runs a shell script in Thunderbird's container.
func tbExec(t testing.TB, stdin []byte, script string) string {
	t.Helper()
	out, err := execIn(thunderbird, "", stdin, "sh", "-c", script)
	if err != nil {
		t.Fatalf("in %s: %s: %v\n%s", thunderbird, clip(script, 200), err, out)
	}
	return out
}

// tbPrefs is the profile: the account, and what keeps Thunderbird from
// doing things on its own account while a test looks at it. No password:
// the test stores it once Thunderbird runs, as its password dialog would.
func tbPrefs(user string) string {
	prefs := []struct {
		name  string
		value any
	}{
		{"mail.accountmanager.accounts", "account1"},
		{"mail.accountmanager.defaultaccount", "account1"},
		{"mail.account.account1.server", "server1"},
		{"mail.account.account1.identities", "id1"},
		{"mail.server.server1.type", "imap"},
		{"mail.server.server1.hostname", robinHost},
		{"mail.server.server1.port", 993},
		{"mail.server.server1.socketType", 3}, // SSL/TLS
		{"mail.server.server1.authMethod", 3}, // normal password
		{"mail.server.server1.userName", user},
		{"mail.server.server1.name", user},
		// Nothing happens until the test asks: no login at startup, no
		// timer that checks for mail, no download of bodies for offline
		// use in idle moments, no junk filter that moves mail.
		{"mail.server.server1.login_at_startup", false},
		{"mail.server.server1.check_new_mail", false},
		{"mail.server.server1.offline_download", false},
		{"mail.server.server1.autosync_offline_stores", false},
		{"mail.server.server1.spamLevel", 0},
		{"mail.identity.id1.fullName", "Thunderbird User"},
		{"mail.identity.id1.useremail", user},
		{"mail.identity.id1.smtpServer", "smtp1"},
		{"mail.identity.id1.compose_html", false},
		{"mail.smtpservers", "smtp1"},
		{"mail.smtp.defaultserver", "smtp1"},
		{"mail.smtpserver.smtp1.hostname", robinHost},
		{"mail.smtpserver.smtp1.port", 465},
		{"mail.smtpserver.smtp1.try_ssl", 3},
		{"mail.smtpserver.smtp1.authMethod", 3},
		{"mail.smtpserver.smtp1.username", user},
		// No dialogs and pages of a first start, no questions.
		{"mail.shell.checkDefaultClient", false},
		{"mail.provider.suppress_dialog_on_startup", true},
		{"mail.rights.version", 1},
		{"mailnews.start_page.enabled", false},
		{"mailnews.start_page_override.mstone", "ignore"},
		{"datareporting.policy.dataSubmissionPolicyBypassNotification", true},
		{"datareporting.healthreport.uploadEnabled", false},
		{"toolkit.telemetry.enabled", false},
		{"mailnews.database.global.indexer.enabled", false},
		{"mail.biff.show_alert", false},
		{"mail.biff.play_sound", false},
		{"mail.compose.autosave", false},
		{"mail.SpellCheckBeforeSend", false},
		{"mail.compose.attachment_reminder", false},
		{"mailnews.confirm.moveFoldersToTrash", false},
		{"mailnews.emptyTrash.dontAskAgain", true},
	}
	var b strings.Builder
	for _, p := range prefs {
		v, _ := json.Marshal(p.value)
		fmt.Fprintf(&b, "user_pref(%q, %s);\n", p.name, v)
	}
	return b.String()
}

// stopThunderbird ends whatever Thunderbird runs in the container.
func stopThunderbird(t testing.TB) {
	t.Helper()
	tbExec(t, nil, `pkill -f "[/]usr/lib/thunderbird/" || true; i=0; while pgrep -f "[/]usr/lib/thunderbird/" >/dev/null; do i=$((i+1)); [ $i -gt 50 ] && pkill -9 -f "[/]usr/lib/thunderbird/"; sleep 0.2; done`)
}

// startThunderbird starts Thunderbird on a new profile for the user and
// stores the secret as the password of its IMAP and SMTP servers. One
// Thunderbird runs at a time.
func startThunderbird(t *testing.T, user, secret string) *tb {
	t.Helper()
	tbLibOnce.Do(func() {
		src, err := os.ReadFile("thunderbird.js")
		if err != nil {
			t.Fatal(err)
		}
		tbLib = string(src)
	})
	tbSerial++
	x := &tb{t: t, user: user, profile: fmt.Sprintf("p%d-%d", time.Now().Unix(), tbSerial)}
	stopThunderbird(t)
	forgetThunderbirdFailures(t)
	tbExec(t, []byte(tbPrefs(user)), "mkdir -p /home/tb/profiles/"+x.profile+" && cat > /home/tb/profiles/"+x.profile+"/user.js")
	x.launch()
	t.Cleanup(func() {
		if x.m != nil {
			x.m.close()
		}
		if t.Failed() {
			t.Logf("Thunderbird's output:\n%s", clip(tbExec(t, nil, "grep -v 'Gtk-\\|glib warning\\|^$' /home/tb/profiles/"+x.profile+".log | tail -30"), 4000))
			t.Logf("the end of Thunderbird's IMAP log:\n%s", clip(tbExec(t, nil,
				"grep 'SendData\\|CreateNewLineFromSocket' /home/tb/profiles/"+x.profile+".imap.log.moz_log | sed 's/^.*mx.robin.test://' | cut -c1-160 | tail -40"), 6000))
		}
		// ROBINMAIL_TB_KEEP leaves the last Thunderbird running, to look
		// at with TestThunderbirdEval.
		if os.Getenv("ROBINMAIL_TB_KEEP") == "" {
			stopThunderbird(t)
		}
	})
	x.run(`await login(arguments[0]);`, secret)
	return x
}

var failureCount = regexp.MustCompile(`(?m)^\[(\d+)\]$`)

// forgetThunderbirdFailures keeps the tests clear of robinmail's limit of
// 50 failed logins per address of a client in fifteen minutes. The cases
// with a refused password cost about ten a run (a refused Thunderbird
// tries IMAP twice, and SMTP with PLAIN and then LOGIN), all from the one
// address of Thunderbird's container: the fifth run against a lab that
// was kept would be refused with "too many failed logins". When the
// counter is past half, it is deleted and robinmail restarted, which
// empties the copy the node keeps in memory.
func forgetThunderbirdFailures(t testing.TB) {
	t.Helper()
	m := failureCount.FindStringSubmatch(labSQL(t, "SELECT count FROM login_failure WHERE key = 'i:"+tbIP+"'"))
	if m == nil {
		return
	}
	if n, _ := strconv.Atoi(m[1]); n < 25 {
		return
	}
	t.Logf("robinmail counts %s failed logins from Thunderbird's address: forgetting them and restarting robinmail", m[1])
	labSQL(t, "DELETE FROM login_failure WHERE key = 'i:"+tbIP+"'")
	restartRobinmail(t)
}

// restartRobinmail restarts robinmail's container and waits until it
// answers again.
func restartRobinmail(t testing.TB) {
	t.Helper()
	compose(t, "restart", "robinmail")
	waitFor(t, "robinmail to answer again", 90*time.Second, func() bool {
		out, err := composeErr(nil, "exec", "-T", "robinmail", "robinmail", "-config", "/lab/robinmail.json", "domain", "list")
		if err != nil || !strings.Contains(out, robinDomain) {
			return false
		}
		c, err := net.DialTimeout("tcp", gateHTTPS, 3*time.Second)
		if err != nil {
			return false
		}
		c.Close()
		return true
	})
}

// launch starts the program on the profile and connects to it.
func (x *tb) launch() {
	t := x.t
	t.Helper()
	if out, err := composeErr(nil, "exec", "-d", thunderbird, "/lab/thunderbird.sh", x.profile); err != nil {
		t.Fatalf("starting Thunderbird: %v\n%s", err, out)
	}
	waitFor(t, "Thunderbird's Marionette", 90*time.Second, func() bool {
		m, err := dialMarionette()
		if err != nil {
			return false
		}
		x.m = m
		return true
	})
	// The main window and the account have to be there before the library
	// can be used.
	waitFor(t, "Thunderbird's main window", 60*time.Second, func() bool {
		out, err := x.m.script(10*time.Second, `
			const { MailServices } = ChromeUtils.importESModule("resource:///modules/MailServices.sys.mjs");
			const w = Services.wm.getMostRecentWindow("mail:3pane");
			return !!(w && w.msgWindow && w.document.readyState == "complete" && MailServices.accounts.accounts.some(a => a.incomingServer.type == "imap"));`)
		return err == nil && string(out) == "true"
	})
}

// restart ends Thunderbird and starts it again on the same profile, as
// closing and opening the program does. The passwords it stored stay.
func (x *tb) restart() {
	x.t.Helper()
	x.m.close()
	x.m = nil
	stopThunderbird(x.t)
	x.launch()
}

// try runs a script after the library and decodes what it returns into
// out. A dialog that Thunderbird opened meanwhile comes back as an
// *alertError; the dialog stays open.
func (x *tb) try(out any, js string, args ...any) error {
	return x.tryFor(2*time.Minute, out, js, args...)
}

func (x *tb) tryFor(limit time.Duration, out any, js string, args ...any) error {
	// The script runs against a watcher that loses nothing but notices a
	// dialog: Thunderbird's dialogs do not stop its scripts, they stop
	// what the script waits for.
	res, err := x.m.script(limit, tbLib+`
const finished = { done: false };
try {
  return await Promise.race([(async () => {
`+js+`
  })(), watchDialogs(finished)]);
} finally {
  finished.done = true;
}`, args...)
	var me *marionetteError
	if errors.As(err, &me) {
		if _, text, ok := strings.Cut(me.Message, "DIALOG "); ok {
			return &alertError{text}
		}
	}
	// Marionette notices some dialogs itself and ends the script for them.
	if text, ok := x.alert(); ok {
		return &alertError{text}
	}
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(res, out); err != nil {
			return fmt.Errorf("%v in %s", err, clip(string(res), 300))
		}
	}
	return nil
}

// alert returns the text of the dialog Thunderbird has open, if any.
func (x *tb) alert() (string, bool) {
	out, err := x.m.script(10*time.Second, tbLib+"\nreturn dialogText();")
	var text *string
	if err == nil && json.Unmarshal(out, &text) == nil && text != nil {
		return *text, true
	}
	res, err := x.m.send("WebDriver:GetAlertText", nil)
	if err != nil {
		return "", false
	}
	var v struct{ Value string }
	json.Unmarshal(res, &v)
	return v.Value, true
}

// dismiss closes the dialogs as Cancel does.
func (x *tb) dismiss() {
	x.t.Helper()
	out, err := x.m.script(10*time.Second, tbLib+"\nreturn closeDialogs();")
	if err == nil && string(out) != "0" {
		return
	}
	if _, err := x.m.send("WebDriver:DismissAlert", nil); err != nil {
		x.t.Fatalf("closing Thunderbird's dialog: %v", err)
	}
}

// windows describes the windows Thunderbird has open, for a failure
// message.
func (x *tb) windows() string {
	out, err := x.m.script(10*time.Second, `
		return [...Services.wm.getEnumerator(null)].map(w => w.location.href + " [" + w.document.title + "] " +
			[...w.document.querySelectorAll("dialog, description, label, button, notification-message")]
				.map(e => (e.getAttribute("label") || e.getAttribute("value") || e.textContent || "").replace(/\s+/g, " ").trim())
				.filter(Boolean).join(" / ").slice(0, 700)).join("\n");`)
	if err != nil {
		return err.Error()
	}
	var s string
	json.Unmarshal(out, &s)
	return s
}

func (x *tb) eval(out any, js string, args ...any) {
	x.t.Helper()
	if err := x.try(out, js, args...); err != nil {
		var me *marionetteError
		if errors.As(err, &me) {
			x.t.Fatalf("Thunderbird: %v\n%s\nscript: %s", me, clip(me.Stack, 600), clip(strings.TrimSpace(js), 300))
		}
		x.t.Fatalf("Thunderbird: %v\nscript: %s", err, clip(strings.TrimSpace(js), 300))
	}
}

func (x *tb) run(js string, args ...any) {
	x.t.Helper()
	x.eval(nil, js, args...)
}

// sync logs in, lists the folders and brings every folder up to date.
func (x *tb) sync() {
	x.t.Helper()
	x.run(`await sync();`)
}

// update brings one folder up to date, as clicking on it does.
func (x *tb) update(folder string) {
	x.t.Helper()
	x.run(`await update(arguments[0]);`, folder)
}

func (x *tb) folders() map[string]tbFolder {
	x.t.Helper()
	var list []tbFolder
	x.eval(&list, `return folders();`)
	out := map[string]tbFolder{}
	for _, f := range list {
		out[f.Path] = f
	}
	return out
}

// messages is the message list of a folder as Thunderbird has it, by
// Message-ID, without asking the server.
func (x *tb) messages(folder string) map[string]tbMessage {
	x.t.Helper()
	var list []tbMessage
	x.eval(&list, `return messages(arguments[0]);`, folder)
	out := map[string]tbMessage{}
	for _, m := range list {
		if _, dup := out[m.ID]; dup {
			x.t.Errorf("Thunderbird shows the message %s twice in %s", m.ID, folder)
		}
		out[m.ID] = m
	}
	return out
}

// source opens a message and returns it as Thunderbird got it.
func (x *tb) source(folder, id string) []byte {
	x.t.Helper()
	var b64 string
	x.eval(&b64, `return source(arguments[0], arguments[1]);`, folder, id)
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		x.t.Fatal(err)
	}
	return raw
}

// waitMessages waits until Thunderbird's list of a folder satisfies ok,
// asking the server each time when refresh is set and only looking when
// not.
func (x *tb) waitMessages(what, folder string, refresh bool, limit time.Duration, ok func(map[string]tbMessage) bool) map[string]tbMessage {
	x.t.Helper()
	var got map[string]tbMessage
	waitFor(x.t, what+" in Thunderbird's "+folder, limit, func() bool {
		if refresh {
			x.update(folder)
		}
		got = x.messages(folder)
		return ok(got)
	})
	return got
}

// ---- robinmail's side ----

// labUser makes a user of robin.test with a name of its own and the lab's
// password.
func labUser(t testing.TB, prefix string) string {
	t.Helper()
	user := fmt.Sprintf("%s-%d@%s", prefix, time.Now().UnixNano()%1_000_000_000, robinDomain)
	if out, err := execIn("robinmail", "", []byte(password()+"\n"), "robinmail", "-config", "/lab/robinmail.json", "user", "add", user); err != nil {
		t.Fatalf("user add: %v\n%s", err, out)
	}
	return user
}

// jmapEmail is an Email as robinmail has it.
type jmapEmail struct {
	id        string
	blob      string
	messageID string
	keywords  map[string]bool
	mailboxes map[string]bool // mailbox ids
}

// emails returns every Email of the account by Message-ID. An account
// with two Emails of one Message-ID fails the test unless dups is set:
// then the map holds one of them and the count is returned too.
func (j *jmap) emails(t testing.TB) map[string]jmapEmail {
	t.Helper()
	out, counts := j.emailsCounted(t)
	for id, n := range counts {
		if n > 1 {
			t.Errorf("robinmail has %d Emails with the Message-ID %s", n, id)
		}
	}
	return out
}

func (j *jmap) emailsCounted(t testing.TB) (map[string]jmapEmail, map[string]int) {
	t.Helper()
	out := map[string]jmapEmail{}
	counts := map[string]int{}
	for pos := 0; ; pos += 200 {
		res := j.call(t,
			[]any{"Email/query", map[string]any{"accountId": j.account, "position": pos, "limit": 200}, "0"},
			[]any{"Email/get", map[string]any{"accountId": j.account, "properties": []string{"blobId", "keywords", "mailboxIds", "messageId"},
				"#ids": map[string]any{"resultOf": "0", "name": "Email/query", "path": "/ids"}}, "1"})
		if len(res) < 2 || res[1].name != "Email/get" {
			t.Fatalf("Email/query: %v", res)
		}
		list := res[1].args["list"].([]any)
		for _, e := range list {
			em := e.(map[string]any)
			x := jmapEmail{id: em["id"].(string), blob: em["blobId"].(string), keywords: map[string]bool{}, mailboxes: map[string]bool{}}
			if ids, _ := em["messageId"].([]any); len(ids) > 0 {
				x.messageID = ids[0].(string)
			}
			kw, _ := em["keywords"].(map[string]any)
			for k := range kw {
				x.keywords[k] = true
			}
			mb, _ := em["mailboxIds"].(map[string]any)
			for k := range mb {
				x.mailboxes[k] = true
			}
			out[x.messageID] = x
			counts[x.messageID]++
		}
		if len(list) < 200 {
			return out, counts
		}
	}
}

// in lists the Message-IDs of the Emails in a mailbox, sorted.
func in(emails map[string]jmapEmail, mailbox string) []string {
	var out []string
	for id, e := range emails {
		if e.mailboxes[mailbox] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// set changes one Email with Email/set: the patch as RFC 8621 4.6 has it.
func (j *jmap) set(t testing.TB, id string, patch map[string]any) {
	t.Helper()
	res := j.call(t, []any{"Email/set", map[string]any{"accountId": j.account, "update": map[string]any{id: patch}}, "0"})
	if u, _ := res[0].args["updated"].(map[string]any); u == nil {
		t.Fatalf("Email/set %s %v: %v", id, patch, res[0].args)
	} else if _, ok := u[id]; !ok {
		t.Fatalf("Email/set %s %v: %v", id, patch, res[0].args)
	}
}

func (j *jmap) destroy(t testing.TB, ids ...string) {
	t.Helper()
	res := j.call(t, []any{"Email/set", map[string]any{"accountId": j.account, "destroy": ids}, "0"})
	if d, _ := res[0].args["destroyed"].([]any); len(d) != len(ids) {
		t.Fatalf("Email/set destroy %v: %v", ids, res[0].args)
	}
}

// waitEmails waits until the account's Emails satisfy ok.
func (j *jmap) waitEmails(t testing.TB, what string, ok func(map[string]jmapEmail) bool) map[string]jmapEmail {
	t.Helper()
	var got map[string]jmapEmail
	waitFor(t, what+" in robinmail", 30*time.Second, func() bool {
		got, _ = j.emailsCounted(t)
		return ok(got)
	})
	return got
}

// ---- robinmail's IMAP trace ----

// imapLine is a command of a client or the tagged reply to one, from
// robinmail's IMAP trace (dev.imap_trace), without the tag.
type imapLine struct {
	reply bool
	text  string
}

// imapTrace returns what the user's IMAP sessions sent and were answered
// since a moment. The AUTHENTICATE of a session is logged before the
// session has a user and is not among them; logins counts those.
func imapTrace(t testing.TB, since time.Time, user string) (lines []imapLine, logins int) {
	t.Helper()
	out := compose(t, "logs", "--no-log-prefix", "--since", since.UTC().Format(time.RFC3339), "robinmail")
	for _, l := range strings.Split(out, "\n") {
		reply := strings.Contains(l, `msg="imap >"`)
		if !reply && !strings.Contains(l, `msg="imap <"`) {
			if strings.Contains(l, `msg="imap: login"`) && strings.Contains(l, "user="+user+" ") {
				logins++
			}
			continue
		}
		var who, text string
		for _, m := range attr.FindAllStringSubmatch(l, -1) {
			v := m[2]
			if strings.HasPrefix(v, `"`) {
				if u, err := strconv.Unquote(v); err == nil {
					v = u
				}
			}
			switch m[1] {
			case "user":
				who = v
			case "cmd", "reply":
				text = v
			}
		}
		if who != user {
			continue
		}
		if _, rest, ok := strings.Cut(text, " "); ok {
			text = rest
		}
		lines = append(lines, imapLine{reply, text})
	}
	return lines, logins
}

// sent returns the commands in the trace that start with prefix, compared
// without regard to case.
func sent(lines []imapLine, prefix string) []string {
	var out []string
	for _, l := range lines {
		if !l.reply && len(l.text) >= len(prefix) && strings.EqualFold(l.text[:len(prefix)], prefix) {
			out = append(out, l.text)
		}
	}
	return out
}

// verbs lists the distinct commands of a trace in the order of their
// first use: "UID FETCH", "IDLE".
func verbs(lines []imapLine) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lines {
		if l.reply {
			continue
		}
		f := strings.Fields(l.text)
		if len(f) == 0 {
			continue
		}
		v := strings.ToUpper(f[0])
		if v == "UID" && len(f) > 1 {
			v += " " + strings.ToUpper(f[1])
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

const tbCol = "thunderbird"
