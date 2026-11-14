package delivery

import (
	"context"
	"errors"
	"testing"

	"github.com/mikepage/robinmail/internal/smtp"
	"github.com/mikepage/robinmail/internal/store"
)

// RFC 5321 4.2.2: 452 "insufficient system storage" and 552 "exceeded
// storage allocation", with the enhanced codes of RFC 3463 3.3: X.2.2
// "Mailbox full" and X.2.3 "Message length exceeds administrative limit".
func TestQuotaAtSMTP(t *testing.T) {
	d, st, _, fw := aliasSetup(t, "")
	msg := []byte("Subject: quota\r\n\r\n" + string(make([]byte, 100)))
	limit := func(bytes, messages, usedBytes, usedMessages int64) store.Quota {
		return store.Quota{Limits: store.Limits{Bytes: bytes, Messages: messages}, UsedBytes: usedBytes, UsedMessages: usedMessages}
	}
	reply := func(err error) string {
		var se *smtp.Error
		if !errors.As(err, &se) {
			if err == nil {
				return "250"
			}
			return err.Error()
		}
		return se.Enhanced
	}
	ctx := context.Background()

	// A full mailbox is refused at RCPT, by octets and by count, so the
	// sender's server keeps the message for that recipient only.
	st.quota = map[string]store.Quota{"acct-mike": limit(1000, 0, 1000, 5), "acct-anna": limit(0, 3, 10, 3)}
	for _, to := range []string{"mike@example.test", "mike+tag@example.test", "anna@example.test", "sales-x@example.test"} {
		if got := reply(d.Rcpt(ctx, env(), to)); got != "4.2.2" {
			t.Errorf("RCPT for the full mailbox of %s: %s", to, got)
		}
	}
	// An alias that also leads elsewhere, or to an account with room,
	// is taken.
	for _, to := range []string{"both@example.test", "postmaster@example.test"} {
		if got := reply(d.Rcpt(ctx, env(), to)); got != "250" {
			t.Errorf("RCPT %s: %s", to, got)
		}
	}
	st.quota["acct-anna"] = limit(0, 3, 10, 2)
	if got := reply(d.Rcpt(ctx, env(), "info@example.test")); got != "250" { // mike is full, anna is not
		t.Errorf("RCPT for an alias with one full target: %s", got)
	}

	// Room at RCPT, none for this message at DATA: temporary, nothing
	// stored. The message alone larger than the limit: permanent.
	st.quota = map[string]store.Quota{"acct-mike": limit(int64(len(msg))+250, 0, 200, 1)}
	if got := reply(d.Deliver(ctx, env("mike@example.test"), msg)); got != "4.2.2" || len(st.mail["acct-mike/inbox"]) != 0 {
		t.Errorf("no room for this message: %s", got)
	}
	st.quota = map[string]store.Quota{"acct-mike": limit(50, 0, 0, 0)}
	if got := reply(d.Deliver(ctx, env("mike@example.test"), msg)); got != "5.2.3" {
		t.Errorf("a message larger than the quota: %s", got)
	}

	// Two recipients, one full: the other has the message, the answer is
	// temporary, and the sender's retry stores nothing twice.
	st.quota = map[string]store.Quota{"acct-mike": limit(10, 0, 0, 0)}
	for i := 0; i < 2; i++ {
		if got := reply(d.Deliver(ctx, env("anna@example.test", "mike@example.test"), msg)); got != "4.2.2" {
			t.Errorf("one of two recipients full: %s", got)
		}
	}
	if len(st.mail["acct-anna/inbox"]) != 1 || len(st.mail["acct-mike/inbox"]) != 0 {
		t.Errorf("anna has %d, mike %d", len(st.mail["acct-anna/inbox"]), len(st.mail["acct-mike/inbox"]))
	}
	// Once there is room the retry completes.
	st.quota = nil
	if got := reply(d.Deliver(ctx, env("anna@example.test", "mike@example.test"), msg)); got != "250" || len(st.mail["acct-anna/inbox"]) != 1 || len(st.mail["acct-mike/inbox"]) != 1 {
		t.Errorf("after room was made: %s", got)
	}

	// Mail between users here: the queue gets the same answers and
	// retries or bounces by them.
	st.quota = map[string]store.Quota{"acct-mike": limit(1000, 0, 1000, 5)}
	if got := reply(d.Local(ctx, "anna@example.test", "mike@example.test", msg)); got != "4.2.2" {
		t.Errorf("Local to a full mailbox: %s", got)
	}
	_ = fw
}
