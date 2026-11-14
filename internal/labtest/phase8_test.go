package labtest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Phase 8 next to real servers: aliases, forwarding with a rewritten
// sender, a forwarding loop, automatic replies and a Sieve redirect.
// Postfix is the server the mail comes from or goes on to; where a third
// party is needed (the original sender of forwarded mail) it is Exim.

const p8Col = "postfix"

// robin runs a robinmail command in robinmail's container.
func robin(t testing.TB, stdin string, args ...string) string {
	t.Helper()
	out, err := execIn("robinmail", "", []byte(stdin), append([]string{"robinmail", "-config", "/lab/robinmail.json"}, args...)...)
	if err != nil {
		t.Fatalf("robinmail %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// callWith is jmap.call with the capabilities of phase 8.
func (j *jmap) callWith(t testing.TB, calls ...[]any) []response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:sieve",
			"urn:ietf:params:jmap:vacationresponse"},
		"methodCalls": calls,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, data := j.do(t, "POST", "/jmap/api", "application/json", body)
	if status != 200 {
		t.Fatalf("JMAP: %d %s", status, data)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("JMAP: %v in %s", err, data)
	}
	var res []response
	for _, r := range out.MethodResponses {
		var x response
		json.Unmarshal(r[0], &x.name)
		json.Unmarshal(r[1], &x.args)
		json.Unmarshal(r[2], &x.tag)
		if x.name == "error" {
			t.Fatalf("JMAP: %s", data)
		}
		res = append(res, x)
	}
	return res
}

// logLines returns the lines of robinmail's log since a moment that
// contain every one of the given texts.
func logLines(t testing.TB, since time.Time, all ...string) []string {
	t.Helper()
	out := compose(t, "logs", "--no-log-prefix", "--since", since.UTC().Format(time.RFC3339), "robinmail")
	var lines []string
next:
	for _, l := range strings.Split(out, "\n") {
		for _, want := range all {
			if !strings.Contains(l, want) {
				continue next
			}
		}
		lines = append(lines, l)
	}
	return lines
}

func TestPhase8(t *testing.T) {
	lab(t)
	postfix, exim := peers[0], peers[1]
	if postfix.name != "postfix" || exim.name != "exim" {
		t.Fatal("the peers are not in the order this test expects")
	}
	stamp := time.Now().UnixNano() % 1_000_000_000

	// An alias for two accounts: one message from Postfix, a copy for
	// each, both with the envelope as it was.
	t.Run("alias", func(t *testing.T) {
		cell(t, "p8 01 alias for two accounts: both get the message from Postfix", p8Col)
		one, two := labUser(t, "p8-one"), labUser(t, "p8-two")
		alias := fmt.Sprintf("team-%d@%s", stamp, robinDomain)
		robin(t, "", "alias", "add", alias, one, two)
		tok, from := newToken(), "alice@"+postfix.domain
		postfix.send(t, from, []string{alias}, rawMessage(tok, from, alias, nil, "To the team.\n"))
		for _, user := range []string{one, two} {
			j := newJMAP(t, user)
			_, raw := j.find(t, tok)
			if rp := header(raw, "Return-Path"); len(rp) != 1 || rp[0] != "<"+from+">" {
				t.Errorf("%s: Return-Path %q", user, rp)
			}
			if to := header(raw, "To"); len(to) != 1 || to[0] != alias {
				t.Errorf("%s: To %q", user, to)
			}
			if body(raw) != "To the team.\n" {
				t.Errorf("%s: body %q", user, body(raw))
			}
		}
		// Plus-addressing still works on an alias.
		tok = newToken()
		tagged := strings.Replace(alias, "@", "+tag@", 1)
		postfix.send(t, from, []string{tagged}, rawMessage(tok, from, tagged, nil, "Tagged.\n"))
		newJMAP(t, one).find(t, tok)
		// The catch-all of a domain takes what nothing else does: the
		// lab has one domain, so it is added and removed here.
		robin(t, "", "alias", "add", "@"+robinDomain, two)
		defer robin(t, "", "alias", "remove", "@"+robinDomain)
		tok = newToken()
		nobody := fmt.Sprintf("nobody-%d@%s", stamp, robinDomain)
		postfix.send(t, from, []string{nobody}, rawMessage(tok, from, nobody, nil, "For nobody in particular.\n"))
		newJMAP(t, two).find(t, tok)
	})

	// An alias that forwards outside. The message is Exim's alice's; it
	// reaches bob at Postfix through robinmail with a sender address of
	// robin.test that leads back to her (SRS), so Postfix's SPF check
	// would pass and a bounce finds its way.
	t.Run("forward", func(t *testing.T) {
		cell(t, "p8 02 alias forwards from Exim to Postfix, sender rewritten (SRS)", p8Col)
		alias := fmt.Sprintf("fwd-%d@%s", stamp, robinDomain)
		robin(t, "", "alias", "add", alias, "bob@"+postfix.domain)
		tok, from, start := newToken(), "alice@"+exim.domain, time.Now()
		exim.send(t, from, []string{alias}, rawMessage(tok, "Alice <"+from+">", alias, nil, "Forwarded once.\n"))
		raw := postfix.find(t, "bob", tok)
		rp := header(raw, "Return-Path")
		srs := regexp.MustCompile(`^<SRS0=[a-z2-7]{8}=[a-z2-7]{2}=exim\.test=alice@robin\.test>$`)
		if len(rp) != 1 || !srs.MatchString(rp[0]) {
			t.Errorf("the envelope sender at Postfix: %q, want SRS0=hash=day=exim.test=alice@robin.test", rp)
		}
		t.Logf("Return-Path at Postfix: %s", strings.Join(rp, " "))
		// The message itself is untouched: From, To, the body.
		if f := header(raw, "From"); len(f) != 1 || f[0] != "Alice <"+from+">" {
			t.Errorf("From at Postfix: %q", f)
		}
		if body(raw) != "Forwarded once.\n" {
			t.Errorf("body at Postfix: %q", body(raw))
		}
		// How it came: Exim to robinmail, robinmail to Postfix, with the
		// alias in Delivered-To.
		rec := received(raw)
		if len(rec) < 3 || !strings.Contains(rec[0], "from "+robinHost) || !strings.Contains(strings.Join(rec, "\n"), "by "+robinHost) {
			t.Errorf("Received at Postfix: %q", rec)
		}
		if dt := header(raw, "Delivered-To"); !strings.Contains(strings.Join(dt, " "), alias) {
			t.Errorf("Delivered-To at Postfix: %q, want the alias among them", dt)
		}
		// Postfix was told the rewritten sender in MAIL FROM.
		if lines := logLines(t, start.Add(-2*time.Second), "MAIL FROM:<SRS0=", "exim.test=alice@robin.test>"); len(lines) == 0 {
			t.Error("robinmail's trace has no MAIL FROM with the rewritten sender")
		}
	})

	// The same with a target that does not exist: Postfix refuses the
	// recipient, robinmail's notification goes to the rewritten address,
	// which is hers, and arrives at Exim.
	t.Run("forward bounce", func(t *testing.T) {
		cell(t, "p8 03 forward to an unknown user at Postfix: the bounce reaches the sender at Exim", p8Col)
		alias := fmt.Sprintf("gone-%d@%s", stamp, robinDomain)
		target := fmt.Sprintf("nosuch-%d@%s", stamp, postfix.domain)
		robin(t, "", "alias", "add", alias, target)
		tok, from := newToken(), "alice@"+exim.domain
		exim.send(t, from, []string{alias}, rawMessage(tok, from, alias, nil, "Forwarded to nobody.\n"))
		raw := exim.find(t, "alice", tok)
		if rp := header(raw, "Return-Path"); len(rp) != 1 || rp[0] != "<>" {
			t.Errorf("the notification's Return-Path at Exim: %q", rp)
		}
		d := parseDSN(t, raw)
		r := failedRecipient(t, d, target, "5.")
		t.Logf("the notification at Exim: %s failed, status %s, %q", r.Address, r.Status, r.Diagnostic)
		if !strings.Contains(strings.ToLower(r.Diagnostic), "550") {
			t.Errorf("the notification does not carry Postfix's reply: %q", r.Diagnostic)
		}
	})

	// A loop: the alias forwards to bob at Postfix, whose .forward sends
	// everything back to the alias. robinmail recognises its own
	// Delivered-To the second time and refuses; Postfix bounces to the
	// rewritten sender; the original sender hears of it, once.
	t.Run("loop", func(t *testing.T) {
		cell(t, "p8 04 forwarding loop through Postfix's .forward: refused the second time round", p8Col)
		alias := fmt.Sprintf("loop-%d@%s", stamp, robinDomain)
		robin(t, "", "alias", "add", alias, "bob@"+postfix.domain)
		if out, err := execIn("postfix", "bob", []byte(alias+"\n"), "sh", "-c", "cat > /home/bob/.forward && chmod 600 /home/bob/.forward"); err != nil {
			t.Fatalf("writing bob's .forward: %v\n%s", err, out)
		}
		defer execIn("postfix", "bob", nil, "rm", "-f", "/home/bob/.forward")
		tok, from, start := newToken(), "alice@"+exim.domain, time.Now()
		exim.send(t, from, []string{alias}, rawMessage(tok, from, alias, nil, "Round and round.\n"))
		// What comes back to the sender is Postfix's notification about
		// robinmail's refusal.
		raw := exim.find(t, "alice", tok)
		if rp := header(raw, "Return-Path"); len(rp) != 1 || rp[0] != "<>" {
			t.Errorf("the notification's Return-Path at Exim: %q", rp)
		}
		if !strings.Contains(string(raw), "5.4.6") || !strings.Contains(strings.ToLower(string(raw)), "loop") {
			t.Errorf("the notification does not name the loop:\n%s", clip(string(raw), 3000))
		}
		// robinmail forwarded once and refused once; nothing is left
		// going round.
		time.Sleep(8 * time.Second) // more than Postfix's and robinmail's retry time here
		since := start.Add(-2 * time.Second)
		forwarded := logLines(t, since, `msg=forwarded`, "via="+alias)
		refused := logLines(t, since, "msg=refused", "554 5.4.6")
		if len(forwarded) != 1 || len(refused) != 1 {
			t.Errorf("robinmail forwarded %d times and refused %d times, want 1 and 1\n%s\n%s", len(forwarded), len(refused),
				strings.Join(forwarded, "\n"), strings.Join(refused, "\n"))
		}
		if _, _, ok := maildirLookup("postfix", "bob", tok); ok {
			t.Error("bob at Postfix kept a copy: the .forward did not take the message")
		}
	})

	// An automatic reply (JMAP VacationResponse, RFC 8621 8; the rules of
	// RFC 5230 and RFC 3834): alice at Postfix writes, gets one reply,
	// and no second one; a mailing list and a notification get none.
	t.Run("vacation", func(t *testing.T) {
		note := cell(t, "p8 05 vacation reply to Postfix: once per sender, none to lists and notifications", p8Col)
		user := labUser(t, "p8-away")
		j := newJMAP(t, user)
		away := newToken()
		res := j.callWith(t, []any{"VacationResponse/set", map[string]any{"accountId": j.account, "update": map[string]any{"singleton": map[string]any{
			"isEnabled": true, "subject": "Away " + away, "textBody": "I am away until Monday.\n"}}}, "0"})
		if _, ok := res[0].args["updated"].(map[string]any)["singleton"]; !ok {
			t.Fatalf("VacationResponse/set: %v", res[0].args)
		}
		from := "alice@" + postfix.domain
		tok, start := newToken(), time.Now()
		postfix.send(t, from, []string{user}, rawMessage(tok, "Alice <"+from+">", user, nil, "Are you there?\n"))
		j.find(t, tok)
		reply := postfix.find(t, "alice", away)
		t.Logf("the reply at Postfix:\n%s", clip(string(lf(reply)), 1500))
		if as := header(reply, "Auto-Submitted"); len(as) != 1 || as[0] != "auto-replied" {
			t.Errorf("Auto-Submitted %q (RFC 3834 5)", as)
		}
		if f := header(reply, "From"); len(f) != 1 || !strings.Contains(f[0], user) {
			t.Errorf("From %q", f)
		}
		if to := header(reply, "To"); len(to) != 1 || !strings.Contains(to[0], from) {
			t.Errorf("To %q", to)
		}
		orig := "<" + tok + "@harness.lab.test>"
		if irt := header(reply, "In-Reply-To"); len(irt) != 1 || irt[0] != orig {
			t.Errorf("In-Reply-To %q, want %s (RFC 5230 5.8)", irt, orig)
		}
		if refs := header(reply, "References"); len(refs) != 1 || !strings.Contains(refs[0], orig) {
			t.Errorf("References %q", refs)
		}
		if !strings.Contains(body(reply), "I am away until Monday.") {
			t.Errorf("body %q", body(reply))
		}
		rp := strings.Join(header(reply, "Return-Path"), " ")
		note("pass, envelope sender " + rp)
		if rp != "<>" && rp != "<"+user+">" {
			t.Errorf("the reply's envelope sender: %q", rp)
		}

		// A second message from the same sender, one that says it is from
		// a list and one that is itself automatic: all are delivered,
		// none is answered.
		send := func(what string, sender string, extra ...string) {
			tok := newToken()
			postfix.send(t, sender, []string{user}, rawMessage(tok, "Alice <"+from+">", user, extra, what+"\n"))
			j.find(t, tok)
		}
		send("a second message", from)
		send("a list", "bob@"+postfix.domain, "List-Id: <announce.postfix.test>", "Precedence: list")
		send("an automatic message", "bob@"+postfix.domain, "Auto-Submitted: auto-generated")
		time.Sleep(6 * time.Second)
		declined := logLines(t, start.Add(-2*time.Second), `msg="no automatic reply"`, "account="+user)
		if len(declined) != 3 {
			t.Errorf("robinmail declined %d automatic replies, want 3:\n%s", len(declined), strings.Join(declined, "\n"))
		}
		for _, l := range declined {
			t.Logf("declined: %s", l[strings.Index(l, "why="):])
		}
		// One reply file at Postfix, not more.
		out, _ := execIn("postfix", "", nil, "sh", "-c", `grep -l -s -F -- "$1" /home/alice/Maildir/new/* | wc -l`, "sh", away)
		if strings.TrimSpace(out) != "1" {
			t.Errorf("alice at Postfix has %s replies, want 1", strings.TrimSpace(out))
		}
		if _, _, ok := maildirLookup("postfix", "bob", away); ok {
			t.Error("bob at Postfix got a reply to the list or the automatic message")
		}
	})

	// A Sieve script (RFC 5228) that redirects: what matches goes on to
	// bob at Postfix and is not kept; with :copy (RFC 3894) it is kept
	// too; the rest stays.
	t.Run("sieve redirect", func(t *testing.T) {
		cell(t, "p8 06 Sieve redirect to Postfix, with and without :copy", p8Col)
		user := labUser(t, "p8-sieve")
		j := newJMAP(t, user)
		script := "require [\"copy\"];\r\n" +
			"if header :contains \"subject\" \"send-on\" { redirect \"bob@" + postfix.domain + "\"; }\r\n" +
			"elsif header :contains \"subject\" \"send-copy\" { redirect :copy \"bob@" + postfix.domain + "\"; }\r\n"
		blob := j.upload(t, []byte(script), "application/sieve")
		res := j.callWith(t, []any{"SieveScript/set", map[string]any{"accountId": j.account,
			"create": map[string]any{"main": map[string]any{"name": "main", "blobId": blob}}, "onSuccessActivateScript": "#main"}, "0"})
		if created, _ := res[0].args["created"].(map[string]any); created["main"] == nil {
			t.Fatalf("SieveScript/set: %v", res[0].args)
		}
		from := "alice@" + exim.domain
		send := func(subject, text string) string {
			tok := newToken()
			exim.send(t, from, []string{user}, rawMessage(tok, "Alice <"+from+">", user, []string{"Subject: " + subject + " " + tok}, text))
			return tok
		}
		on, cp, stay := send("send-on", "Redirected.\n"), send("send-copy", "Redirected and kept.\n"), send("nothing special", "Kept.\n")

		raw := postfix.find(t, "bob", on)
		// RFC 5228 4.2: redirect does not change the message; the
		// envelope sender is rewritten as for any forward.
		if f := header(raw, "From"); len(f) != 1 || f[0] != "Alice <"+from+">" {
			t.Errorf("From of the redirected message: %q", f)
		}
		if body(raw) != "Redirected.\n" {
			t.Errorf("body of the redirected message: %q", body(raw))
		}
		rp := strings.Join(header(raw, "Return-Path"), " ")
		t.Logf("Return-Path of the redirected message at Postfix: %s", rp)
		if !regexp.MustCompile(`^<SRS0=[a-z2-7]{8}=[a-z2-7]{2}=exim\.test=alice@robin\.test>$`).MatchString(rp) {
			t.Errorf("the envelope sender of the redirected message: %q", rp)
		}
		postfix.find(t, "bob", cp)
		j.find(t, cp)
		j.find(t, stay)
		j.absent(t, on, 3*time.Second)
		maildirAbsent(t, "postfix", "bob", stay, 0)
	})
}
