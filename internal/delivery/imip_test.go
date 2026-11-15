package delivery

import (
	"context"
	"testing"

	"github.com/robinmail/robinmail/internal/store"
)

// The calendar hook learns whether the From field of a message is known
// to be right: DMARC passed (Rspamd's DMARC_POLICY_ALLOW), or the message
// came from an account of this server. Without that, iMIP may only add new
// invitations (pim.IncomingMail).
func TestCalendarHookAuthenticated(t *testing.T) {
	msg := []byte("From: a@sender.test\r\nTo: mike@example.test\r\nSubject: s\r\n\r\nbody\r\n")
	for _, c := range []struct {
		name, reply string
		local, want bool
	}{
		{"no rspamd", "", false, false},
		{"dmarc not passed", `{"action":"no action","score":0,"symbols":{"DMARC_POLICY_SOFTFAIL":{"name":"DMARC_POLICY_SOFTFAIL"}}}`, false, false},
		{"dmarc passed", `{"action":"no action","score":0,"symbols":{"DMARC_POLICY_ALLOW":{"name":"DMARC_POLICY_ALLOW"}}}`, false, true},
		{"local", "", true, true},
	} {
		d, _ := setup(t, c.reply)
		var got []bool
		d.Calendar = func(_ context.Context, _ store.Account, _ []byte, authenticated bool) {
			got = append(got, authenticated)
		}
		var err error
		if c.local {
			err = d.Local(context.Background(), "a@example.test", "mike@example.test", msg)
		} else {
			err = d.Deliver(context.Background(), env("mike@example.test"), msg)
		}
		if err != nil || len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: err %v, hook saw %v, want %v", c.name, err, got, c.want)
		}
	}
}
