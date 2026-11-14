package imap

import (
	"context"
	"strings"
	"testing"
	"time"
)

// quotaStore is the test store with a quota: it answers Quota and refuses
// an APPEND that does not fit.
type quotaStore struct {
	*memStore
	q Quota
}

func (s *quotaStore) Login(ctx context.Context, ip, username, password string) (User, error) {
	if _, err := s.memStore.Login(ctx, ip, username, password); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *quotaStore) Quota(context.Context) (Quota, error) { return s.q, nil }

func (s *quotaStore) Append(ctx context.Context, mailbox int64, raw []byte, flags []string, date time.Time) (uint32, uint32, error) {
	if (s.q.StorageLimit > 0 && s.q.StorageUsed+int64(len(raw)) > s.q.StorageLimit) || (s.q.MessageLimit > 0 && s.q.MessageUsed+1 > s.q.MessageLimit) {
		return 0, 0, ErrOverQuota
	}
	s.q.StorageUsed += int64(len(raw))
	s.q.MessageUsed++
	return s.memStore.Append(ctx, mailbox, raw, flags, date)
}

func quotaEnv(t *testing.T, q Quota) (*env, *quotaStore) {
	var qs *quotaStore
	e := newEnv(t, func(srv *Server) {
		qs = &quotaStore{memStore: srv.Backend.(*memStore), q: q}
		srv.Backend = qs
	})
	return e, qs
}

// RFC 9208 3: the capabilities; 4.2 and 4.3: GETQUOTA and GETQUOTAROOT, in
// the shape of the examples there; 5.1: the units of STORAGE.
func TestRFC9208Quota(t *testing.T) {
	e, _ := quotaEnv(t, Quota{StorageUsed: 106496, StorageLimit: 10 << 20, MessageUsed: 42, MessageLimit: 1000})
	c := e.dial()
	// "QUOTA" and "a capability of the form QUOTA=RES-<name>" for every
	// resource; not before login, and no QUOTASET: limits are not the
	// user's to set.
	if greet := c.cmd("CAPABILITY"); strings.Contains(strings.Join(greet, " "), "QUOTA") {
		t.Errorf("QUOTA announced before login: %v", greet)
	}
	caps := c.status(`LOGIN fred@example.test secret`)
	for _, want := range []string{" QUOTA ", " QUOTA=RES-STORAGE ", " QUOTA=RES-MESSAGE]"} {
		if !strings.Contains(caps, want) {
			t.Errorf("no%sin %s", want, caps)
		}
	}
	if strings.Contains(caps, "QUOTASET") {
		t.Errorf("QUOTASET announced: %s", caps)
	}

	//	C: G0002 GETQUOTAROOT INBOX
	//	S: * QUOTAROOT INBOX "#user/alice" "!partition/sda4"
	//	S: * QUOTA "#user/alice" (MESSAGE 42 1000)
	//	S: * QUOTA "!partition/sda4" (STORAGE 104 10923847)
	//	S: G0002 OK Getquotaroot complete
	// Here there is one root, "", with both resources. STORAGE is in
	// units of 1024 octets: 106496 octets are 104.
	c.expect("GETQUOTAROOT INBOX", `* QUOTAROOT INBOX ""`, `* QUOTA "" (STORAGE 104 10240 MESSAGE 42 1000)`, "OK GETQUOTAROOT completed")
	c.expect(`GETQUOTAROOT "Sent"`, `* QUOTAROOT Sent ""`, `* QUOTA "" (STORAGE 104 10240 MESSAGE 42 1000)`, "OK GETQUOTAROOT completed")
	//	C: G0001 GETQUOTA "!partition/sda4"
	//	S: * QUOTA "!partition/sda4" (STORAGE 104 10923847)
	//	S: G0001 OK Getquota complete
	c.expect(`GETQUOTA ""`, `* QUOTA "" (STORAGE 104 10240 MESSAGE 42 1000)`, "OK GETQUOTA completed")
	// A root or a mailbox that is not there.
	if got := c.status(`GETQUOTA "other"`); !strings.HasPrefix(got, "NO") {
		t.Errorf("GETQUOTA of an unknown root: %s", got)
	}
	if got := c.status("GETQUOTAROOT Nonesuch"); !strings.HasPrefix(got, "NO [NONEXISTENT]") {
		t.Errorf("GETQUOTAROOT of an unknown mailbox: %s", got)
	}
	// 4.1: SETQUOTA without QUOTASET is refused.
	if got := c.status(`SETQUOTA "" (STORAGE 512)`); !strings.HasPrefix(got, "NO [NOPERM]") {
		t.Errorf("SETQUOTA: %s", got)
	}
	for _, bad := range []string{"GETQUOTA", "GETQUOTAROOT", `GETQUOTA "" extra`} {
		if got := c.status(bad); !strings.HasPrefix(got, "BAD") {
			t.Errorf("%s: %s", bad, got)
		}
	}
}

// One octet is not nothing, a resource without a limit is not listed, and
// an account without limits has no quota root.
func TestRFC9208Resources(t *testing.T) {
	e, qs := quotaEnv(t, Quota{StorageUsed: 1, StorageLimit: 2047})
	c := e.login()
	c.expect("GETQUOTAROOT INBOX", `* QUOTAROOT INBOX ""`, `* QUOTA "" (STORAGE 1 1)`, "OK GETQUOTAROOT completed")
	qs.q = Quota{MessageUsed: 3, MessageLimit: 5, StorageUsed: 5000}
	c.expect(`GETQUOTA ""`, `* QUOTA "" (MESSAGE 3 5)`, "OK GETQUOTA completed")
	qs.q = Quota{MessageUsed: 3, StorageUsed: 5000}
	c.expect("GETQUOTAROOT INBOX", `* QUOTAROOT INBOX`, "OK GETQUOTAROOT completed")
	if got := c.status(`GETQUOTA ""`); !strings.HasPrefix(got, "NO") {
		t.Errorf("GETQUOTA without any limit: %s", got)
	}
}

// RFC 9208 5.2: "OVERQUOTA ... in the tagged NO response to an APPEND/
// COPY/MOVE when the addition of the message(s) puts the target mailbox
// over any one of its quota limits":
//
//	C: A003 APPEND saved-messages (\Seen) {326}
//	S: A003 NO [OVERQUOTA] APPEND Failed
func TestRFC9208OverQuota(t *testing.T) {
	e, qs := quotaEnv(t, Quota{MessageUsed: 0, MessageLimit: 1})
	c := e.login()
	msg := "Subject: one\r\n\r\nbody\r\n"
	appendMsg := func() string {
		t.Helper()
		c.tag++
		c.write("q" + string(rune('0'+c.tag%10)) + " APPEND INBOX {" + itoa(len(msg)) + "+}\r\n" + msg + "\r\n")
		for {
			line := c.line()
			if strings.HasPrefix(line, "q") {
				return line[strings.IndexByte(line, ' ')+1:]
			}
		}
	}
	if got := appendMsg(); !strings.HasPrefix(got, "OK [APPENDUID") {
		t.Fatalf("the first message: %s", got)
	}
	if got := appendMsg(); !strings.HasPrefix(got, "NO [OVERQUOTA]") {
		t.Fatalf("a message over the limit: %s", got)
	}
	// Room again: it is taken.
	qs.q.MessageLimit = 2
	if got := appendMsg(); !strings.HasPrefix(got, "OK [APPENDUID") {
		t.Fatalf("after the limit was raised: %s", got)
	}
	c.expect(`GETQUOTA ""`, `* QUOTA "" (MESSAGE 2 2)`, "OK GETQUOTA completed")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A backend without quotas (the plain test store) announces none and has
// no quota root.
func TestQuotaWithoutBackend(t *testing.T) {
	e := newEnv(t)
	c := e.dial()
	if login := c.status(`LOGIN fred@example.test secret`); strings.Contains(login, "QUOTA") {
		t.Errorf("QUOTA announced by a backend without quotas: %v", login)
	}
	c.expect("GETQUOTAROOT INBOX", `* QUOTAROOT INBOX`, "OK GETQUOTAROOT completed")
}
