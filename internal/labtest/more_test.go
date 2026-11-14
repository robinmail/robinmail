package labtest

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---- TLS that is broken or missing ----

func TestTLS(t *testing.T) {
	lab(t)
	alice := newJMAP(t, "alice@robin.test")
	from := alice.user
	// A message that must not be delivered stays queued: robinmail tries
	// every five seconds here (dev.retry_seconds), so a few tries pass
	// while the test waits.
	refused := func(t *testing.T, service, to, reason string) {
		tok := newToken()
		sub := alice.sendCreate(t, textCreate(tok, from, to, "lab "+tok, "must not arrive\n"), to)
		var st status
		waitFor(t, "a first try", 30*time.Second, func() bool {
			st = alice.statuses(t, sub)[to]
			return st.reply != ""
		})
		maildirAbsent(t, service, "alice", tok, 12*time.Second)
		st = alice.statuses(t, sub)[to]
		if st.delivered != "queued" || !strings.Contains(st.reply, "4.7.0") || !strings.Contains(st.reply, reason) {
			t.Errorf("deliveryStatus %+v, want queued with 4.7.0 and %q", st, reason)
		}
		t.Logf("robinmail holds it: %s", st.reply)
		// Enough tried: take it out of the queue.
		dropQueued(t, to)
	}
	delivered := func(t *testing.T, service, to string) []byte {
		tok := newToken()
		sub := alice.sendCreate(t, textCreate(tok, from, to, "lab "+tok, "arrives\n"), to)
		raw, _ := maildirFind(t, service, "alice", tok)
		if st := alice.waitStatus(t, sub, to, "unknown", "no"); st.delivered != "unknown" {
			t.Errorf("deliveryStatus %+v", st)
		}
		return raw
	}

	t.Run("self-signed, no policy", func(t *testing.T) {
		// RFC 3207 and common practice: without a policy TLS is
		// opportunistic, and a certificate nobody vouches for still
		// encrypts.
		cell(t, "tls 01 self-signed certificate, no MTA-STS policy: delivered over TLS", "other")
		raw := delivered(t, "badtls", "alice@badtls.test")
		if rec := received(raw); len(rec) == 0 || !strings.Contains(rec[0], "TLS") {
			t.Errorf("not over TLS: %q", rec)
		}
	})
	t.Run("self-signed, enforce", func(t *testing.T) {
		// RFC 8461 4.2 and 5.
		cell(t, "tls 02 self-signed certificate, MTA-STS enforce: not delivered", "other")
		refused(t, "badtls", "alice@badtls-sts.test", "TLS with mx.badtls.test failed")
	})
	t.Run("no STARTTLS, no policy", func(t *testing.T) {
		cell(t, "tls 03 no STARTTLS offered, no policy: delivered in the clear", "other")
		raw := delivered(t, "notls", "alice@notls.test")
		if rec := received(raw); len(rec) == 0 || strings.Contains(rec[0], "TLS") || !strings.Contains(rec[0], "ESMTP") {
			t.Errorf("Received: %q", rec)
		}
	})
	t.Run("no STARTTLS, enforce", func(t *testing.T) {
		cell(t, "tls 04 no STARTTLS offered, MTA-STS enforce: not delivered", "other")
		refused(t, "notls", "alice@notls-sts.test", "does not offer STARTTLS")
	})
}

// ---- bounces ----

func TestBounces(t *testing.T) {
	lab(t)
	alice := newJMAP(t, "alice@robin.test")
	from := alice.user

	t.Run("late", func(t *testing.T) {
		for _, p := range peers {
			t.Run(p.name, func(t *testing.T) {
				t.Parallel()
				// The peer says 250 and bounces afterwards. Its
				// notification comes back with the null reverse-path,
				// lands in the Inbox, and is matched to the submission.
				cell(t, "bounce 01 peer accepts, then bounces: its DSN arrives and sets deliveryStatus", p.name)
				tok, start := newToken(), time.Now()
				to := "ghost@late." + p.domain
				sub := alice.sendCreate(t, textCreate(tok, from, to, "lab "+tok, "to nobody\n"), to)
				_, raw := alice.find(t, tok)
				if rp := header(raw, "Return-Path"); len(rp) != 1 || rp[0] != "<>" {
					t.Errorf("Return-Path of the peer's notification: %q", rp)
				}
				t.Logf("%s's notification: Content-Type %q", p.name, header(raw, "Content-Type"))
				if null := commands(trace(t, start.Add(-2*time.Second)), "server", p.ip, "MAIL FROM:<>"); len(null) == 0 {
					t.Errorf("no MAIL FROM:<> from %s in the trace", p.name)
				}
				d := peerDSN(t, p, raw)
				r := failedRecipient(t, d, to, "5.")
				t.Logf("%s reports: status %s, diagnostic %q, original Message-ID %q", p.name, r.Status, r.Diagnostic, d.OriginalMessageID)
				st := alice.waitStatus(t, sub, to, "no")
				t.Logf("deliveryStatus: %+v", st)
			})
		}
	})

	t.Run("greylisting", func(t *testing.T) {
		// postgrey answers 450 to a sender and recipient it has not
		// seen, and 250 once eight seconds have passed.
		cell(t, "bounce 02 greylisting: 450 first, delivered on a later try", "postfix")
		p := peers[0]
		tok, start := newToken(), time.Now()
		to := "grey+" + tok + "@" + p.domain
		sub := alice.sendCreate(t, textCreate(tok, from, to, "lab "+tok, "after a while\n"), to)
		var first status
		waitFor(t, "the first try", 30*time.Second, func() bool {
			first = alice.statuses(t, sub)[to]
			return first.reply != ""
		})
		if first.delivered != "queued" || !strings.HasPrefix(first.reply, "450") {
			t.Errorf("after the first try: %+v, want queued with a 450", first)
		}
		t.Logf("first: %s", first.reply)
		p.find(t, "alice", tok)
		if st := alice.waitStatus(t, sub, to, "unknown", "no"); st.delivered != "unknown" {
			t.Errorf("deliveryStatus %+v", st)
		}
		tries := 0
		for _, l := range trace(t, start.Add(-2*time.Second)) {
			if l.side == "client" && l.remote == p.ip && l.dir == "C" && strings.HasPrefix(l.line, "RCPT TO:<"+to+">") {
				tries++
			}
		}
		t.Logf("delivered on try %d", tries)
		if tries < 2 {
			t.Errorf("%d tries, want at least 2", tries)
		}
	})

	// Last, and one after the other: these stop containers.
	t.Run("down", func(t *testing.T) {
		for _, p := range peers {
			t.Run(p.name, func(t *testing.T) {
				cell(t, "bounce 03 peer is down: queued, delivered when it is back", p.name)
				compose(t, "stop", p.name)
				started := false
				start := func() {
					if !started {
						started = true
						compose(t, "start", p.name)
					}
				}
				t.Cleanup(start)
				tok := newToken()
				to := "alice@" + p.domain
				sub := alice.sendCreate(t, textCreate(tok, from, to, "lab "+tok, "wait for it\n"), to)
				var st status
				waitFor(t, "two tries", 60*time.Second, func() bool {
					st = alice.statuses(t, sub)[to]
					return st.reply != ""
				})
				if st.delivered != "queued" || !strings.Contains(st.reply, "4.4.") {
					t.Errorf("while %s is down: %+v, want queued with 4.4.x", p.name, st)
				}
				t.Logf("while down: %s", st.reply)
				start()
				p.find(t, "alice", tok)
				if st := alice.waitStatus(t, sub, to, "unknown", "no"); st.delivered != "unknown" {
					t.Errorf("deliveryStatus %+v", st)
				}
			})
		}
	})
}

// ---- peculiarities ----

func TestPeculiarities(t *testing.T) {
	lab(t)
	alice := newJMAP(t, "alice@robin.test")
	to := alice.user

	t.Run("underscore", func(t *testing.T) {
		for _, p := range peers {
			t.Run(p.name, func(t *testing.T) {
				t.Parallel()
				// A sender whose domain has an underscore: not a host
				// name by RFC 5321 4.1.2, yet the peer sends it.
				note := cell(t, "odd 01 sender domain with an underscore, sent by the peer: accepted", p.name)
				tok := newToken()
				sender := "alice@under_score." + p.domain
				if out, err := p.sendErr(sender, []string{to}, rawMessage(tok, sender, to, nil, "underscore\n")); err != nil {
					// Exim keeps to the grammar, also for its own users.
					note("peer refuses")
					t.Logf("%s does not send it: %s", p.name, clip(out, 300))
					return
				}
				_, raw := alice.find(t, tok)
				if rp := header(raw, "Return-Path"); len(rp) != 1 || rp[0] != "<"+sender+">" {
					t.Errorf("Return-Path %q", rp)
				}
			})
		}
	})

	t.Run("underscore at peers", func(t *testing.T) {
		// What the others do with such a sender: the answer to the
		// plan's open question.
		for _, p := range peers {
			c := dialSMTP(t, p.gate)
			mail := c.cmd(t, "MAIL FROM:<someone@under_score.robin.test>")
			rcpt := c.cmd(t, "RCPT TO:<alice@"+p.domain+">")
			t.Logf("%s: MAIL with an underscore in the domain: %s; RCPT: %s", p.name, mail, rcpt)
			observe("odd 90 peer accepts MAIL FROM with an underscore in the domain", p.name, fmt.Sprint(mail.code))
			c.cmd(t, "RSET")
			route := c.cmd(t, "MAIL FROM:<@a.test,@b.test:someone@robin.test>")
			t.Logf("%s: MAIL with a source route: %s", p.name, route)
			observe("odd 91 peer accepts MAIL FROM with a source route", p.name, fmt.Sprint(route.code))
			c.cmd(t, "QUIT")
		}
	})

	t.Run("underscore reply", func(t *testing.T) {
		// Mail from such a domain can be answered: Postfix is the
		// final destination of under_score.postfix.test.
		cell(t, "odd 04 mail to a domain with an underscore: delivered", "postfix")
		tok := newToken()
		rcpt := "alice@under_score.postfix.test"
		sub := alice.sendCreate(t, textCreate(tok, to, rcpt, "lab "+tok, "back to you\n"), rcpt)
		peers[0].find(t, "alice", tok)
		if st := alice.waitStatus(t, sub, rcpt, "unknown", "no"); st.delivered != "unknown" {
			t.Errorf("deliveryStatus %+v", st)
		}
	})

	t.Run("null reverse-path", func(t *testing.T) {
		cell(t, "odd 02 null reverse-path by hand: accepted, Return-Path: <>", "other")
		tok := newToken()
		c := dialSMTP(t, gateSMTP)
		if rep := c.cmd(t, "MAIL FROM:<>"); rep.code != 250 {
			t.Fatalf("MAIL FROM:<>: %s", rep)
		}
		if rep := c.cmd(t, "RCPT TO:<"+to+">"); rep.code != 250 {
			t.Fatalf("RCPT: %s", rep)
		}
		if rep := c.data(t, rawMessage(tok, "MAILER-DAEMON@harness.lab.test", to, nil, "a notification\n")); rep.code != 250 {
			t.Fatalf("DATA: %s", rep)
		}
		_, raw := alice.find(t, tok)
		if rp := header(raw, "Return-Path"); len(rp) != 1 || rp[0] != "<>" {
			t.Errorf("Return-Path %q", rp)
		}
	})

	t.Run("source route", func(t *testing.T) {
		// RFC 5321 4.1.2 and appendix C: a source route must be
		// accepted and should be ignored.
		cell(t, "odd 03 source routes by hand: accepted and ignored", "other")
		tok := newToken()
		c := dialSMTP(t, gateSMTP)
		if rep := c.cmd(t, "MAIL FROM:<@a.test,@b.test:alice@postfix.test>"); rep.code != 250 {
			t.Fatalf("MAIL: %s", rep)
		}
		if rep := c.cmd(t, "RCPT TO:<@"+robinHost+":"+to+">"); rep.code != 250 {
			t.Fatalf("RCPT: %s", rep)
		}
		if rep := c.data(t, rawMessage(tok, "alice@postfix.test", to, nil, "routed\n")); rep.code != 250 {
			t.Fatalf("DATA: %s", rep)
		}
		_, raw := alice.find(t, tok)
		if rp := header(raw, "Return-Path"); len(rp) != 1 || rp[0] != "<alice@postfix.test>" {
			t.Errorf("Return-Path %q", rp)
		}
	})
}

// ---- submission ----

// TestSubmission sends through ports 587 and 465 with swaks, a mail client
// that is not ours, from inside the lab (the Postfix container has it).
func TestSubmission(t *testing.T) {
	lab(t)
	from := "alice@" + robinDomain
	swaks := func(tok, to string, args ...string) (string, error) {
		argv := []string{"swaks", "--server", robinHost, "--helo", "client.lab.test",
			"--tls-verify", "--tls-ca-path", "/lab/secrets/ca.crt",
			"--auth-user", from, "--from", from, "--to", to,
			"--header", "Subject: lab " + tok, "--header", "X-Lab: " + tok, "--body", "sent with swaks"}
		return execIn("postfix", "", nil, append(argv, args...)...)
	}
	cases := []struct {
		row  string
		args []string
	}{
		{"sub 01 port 587, STARTTLS, AUTH PLAIN", []string{"--port", "587", "--tls", "--auth", "PLAIN"}},
		{"sub 02 port 587, STARTTLS, AUTH LOGIN", []string{"--port", "587", "--tls", "--auth", "LOGIN"}},
		{"sub 03 port 465, implicit TLS, AUTH PLAIN", []string{"--port", "465", "--tls-on-connect", "--auth", "PLAIN"}},
		{"sub 04 port 465, implicit TLS, AUTH LOGIN", []string{"--port", "465", "--tls-on-connect", "--auth", "LOGIN"}},
	}
	for i, c := range cases {
		t.Run(c.row, func(t *testing.T) {
			cell(t, c.row, "swaks")
			p := peers[i%len(peers)]
			tok := newToken()
			out, err := swaks(tok, "alice@"+p.domain, append(c.args, "--auth-password", password())...)
			if err != nil {
				t.Fatalf("swaks: %v\n%s", err, out)
			}
			if !strings.Contains(out, "235 2.7.0") {
				t.Errorf("no 235 in the session:\n%s", out)
			}
			raw := p.find(t, "alice", tok)
			// The submission Received line names no client (PLAN,
			// phase 5).
			rec := received(raw)
			if len(rec) != 2 || !strings.Contains(rec[1], "with ESMTPSA") || strings.Contains(rec[1], "client.lab.test") {
				t.Errorf("Received: %q", rec)
			}
		})
	}
	t.Run("wrong password", func(t *testing.T) {
		cell(t, "sub 05 wrong password: 535, nothing sent", "swaks")
		tok := newToken()
		out, err := swaks(tok, "alice@postfix.test", "--port", "587", "--tls", "--auth", "PLAIN", "--auth-password", "not-the-password")
		if err == nil || !strings.Contains(out, "535 5.7.8") {
			t.Errorf("swaks: %v\n%s", err, out)
		}
	})
	t.Run("no TLS", func(t *testing.T) {
		// RFC 4954 4: no AUTH is offered before STARTTLS.
		cell(t, "sub 06 port 587 without STARTTLS: no AUTH offered, MAIL refused", "swaks")
		tok := newToken()
		out, err := execIn("postfix", "", nil, "swaks", "--server", robinHost, "--port", "587", "--from", from, "--to", "alice@postfix.test",
			"--header", "X-Lab: "+tok, "--body", "x")
		if err == nil || !strings.Contains(out, "530 5.7.0") || strings.Contains(out, "AUTH PLAIN") {
			t.Errorf("swaks: %v\n%s", err, out)
		}
	})
}
