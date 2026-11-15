package pim

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/blob"
	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/raftdb/raftdbtest"
	"github.com/robinmail/robinmail/internal/store"
)

var ctx = context.Background()

type sent struct {
	from  string
	rcpts []string
	raw   []byte
}

type fixture struct {
	svc  *Service
	st   *store.Store
	mu   sync.Mutex
	mail []sent
}

func setup(t *testing.T) *fixture {
	t.Helper()
	db := raftdbtest.Open(t)
	master, _ := blob.NewMasterKey(bytes.Repeat([]byte{0x42}, 32))
	st := store.New(db, blob.NewLocal(t.TempDir()), master)
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDomain(ctx, "example.test"); err != nil && !errors.Is(err, store.ErrDomainExists) {
		t.Fatal(err)
	}
	f := &fixture{st: st}
	f.svc = &Service{Store: st, Now: func() time.Time { return time.Date(1997, 6, 11, 19, 0, 0, 0, time.UTC) }}
	f.svc.Send = func(ctx context.Context, a store.Account, from string, rcpts []string, raw []byte) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.mail = append(f.mail, sent{from, rcpts, raw})
		return nil
	}
	return f
}

func (f *fixture) account(t *testing.T) store.Account {
	t.Helper()
	var r [6]byte
	rand.Read(r[:])
	a, err := f.st.CreateAccount(ctx, "cal-"+hex.EncodeToString(r[:])+"@example.test", "$argon2id$test")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fixture) calendar(t *testing.T, a store.Account) Access {
	t.Helper()
	list, err := f.svc.Collections(ctx, a, store.KindCalendar)
	if err != nil || len(list) == 0 {
		t.Fatal(err)
	}
	return list[0]
}

func (f *fixture) event(t *testing.T, a store.Account, uid string) *ical.Component {
	t.Helper()
	objs, err := f.st.Objects(ctx, a.ID, store.ObjectFilter{Kind: store.ObjectEvent, UID: uid, WithData: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) == 0 {
		return nil
	}
	cal, err := ical.ParseOne([]byte(objs[0].Data), "VCALENDAR")
	if err != nil {
		t.Fatal(err)
	}
	return cal
}

func partstatOf(cal *ical.Component, who string) string {
	for _, c := range cal.Sub("VEVENT") {
		for _, a := range c.All("ATTENDEE") {
			if SameAddress(a.Value, "mailto:"+who) {
				return a.Param("PARTSTAT")
			}
		}
	}
	return ""
}

func inbox(t *testing.T, f *fixture, a store.Account) []store.Object {
	in, _ := f.st.Collections(ctx, a.ID, store.KindInbox)
	objs, err := f.st.Objects(ctx, a.ID, store.ObjectFilter{Collections: []int64{in[0].ID}, WithData: true})
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

// The group event of RFC 5546 4.2.1, with A, B and C accounts of this
// server and D somewhere else, then B's reply (4.2.2) and the
// cancellation (4.2.9), all through implicit scheduling (RFC 6638 3.2).
func TestRFC5546_4_2_GroupEvent(t *testing.T) {
	f := setup(t)
	a, b, c := f.account(t), f.account(t), f.account(t)
	uid := "calsrv.example.com-" + RandomID() + "@example.com"
	object := "BEGIN:VCALENDAR\r\nPRODID:-//Example/ExampleCalendarClient//EN\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\n" +
		"ORGANIZER:mailto:" + a.Address + "\r\n" +
		"ATTENDEE;ROLE=CHAIR;PARTSTAT=ACCEPTED:mailto:" + a.Address + "\r\n" +
		"ATTENDEE;RSVP=TRUE;CUTYPE=INDIVIDUAL:mailto:" + b.Address + "\r\n" +
		"ATTENDEE;RSVP=TRUE;CUTYPE=INDIVIDUAL:mailto:" + c.Address + "\r\n" +
		"ATTENDEE;RSVP=TRUE;CUTYPE=INDIVIDUAL:mailto:d@elsewhere.example\r\n" +
		"DTSTAMP:19970611T190000Z\r\nDTSTART:19970701T200000Z\r\nDTEND:19970701T210000Z\r\nSUMMARY:Conference\r\n" +
		"UID:" + uid + "\r\nSEQUENCE:0\r\nSTATUS:CONFIRMED\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	cal, _, _, err := ParseCalendarObject([]byte(object), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PutEvent(ctx, a, f.calendar(t, a), "conf.ics", cal, Cond{IfNoneMatch: true}, true); err != nil {
		t.Fatal(err)
	}
	// B and C have the event with their answer to give, and the request
	// in their scheduling inbox; D got an email (RFC 6047).
	for _, x := range []store.Account{b, c} {
		got := f.event(t, x, uid)
		if got == nil {
			t.Fatalf("%s has no event", x.Address)
		}
		if ps := partstatOf(got, x.Address); ps != "" && ps != "NEEDS-ACTION" {
			t.Errorf("%s: PARTSTAT %q", x.Address, ps)
		}
		if in := inbox(t, f, x); len(in) != 1 || !strings.Contains(in[0].Data, "METHOD:REQUEST") {
			t.Errorf("%s inbox %d", x.Address, len(in))
		}
	}
	if len(f.mail) != 1 || f.mail[0].rcpts[0] != "d@elsewhere.example" || f.mail[0].from != a.Address {
		t.Fatalf("mail %+v", f.mail)
	}
	msg, from, ok := FindITIP(f.mail[0].raw)
	if !ok || from != a.Address || msg.Value("METHOD") != "REQUEST" || len(msg.Sub("VEVENT")[0].All("ATTENDEE")) != 4 {
		t.Fatalf("iMIP: %v %s\n%s", ok, from, f.mail[0].raw)
	}
	if !bytes.Contains(f.mail[0].raw, []byte("Subject: Invitation: Conference")) {
		t.Errorf("subject:\n%s", f.mail[0].raw)
	}

	// B accepts in their own calendar: A's copy shows it (4.2.2).
	bcal := f.event(t, b, uid)
	for i := range bcal.Sub("VEVENT")[0].Props {
		p := &bcal.Sub("VEVENT")[0].Props[i]
		if p.Name == "ATTENDEE" && SameAddress(p.Value, "mailto:"+b.Address) {
			p.SetParam("PARTSTAT", "ACCEPTED")
		}
	}
	objs, _ := f.st.Objects(ctx, b.ID, store.ObjectFilter{UID: uid})
	if _, err := f.svc.PutEvent(ctx, b, f.calendar(t, b), objs[0].Name, bcal, Cond{IfMatch: objs[0].ETag}, true); err != nil {
		t.Fatal(err)
	}
	if ps := partstatOf(f.event(t, a, uid), b.Address); ps != "ACCEPTED" {
		t.Errorf("A sees B as %q", ps)
	}
	if in := inbox(t, f, a); len(in) != 1 || !strings.Contains(in[0].Data, "METHOD:REPLY") {
		t.Errorf("A's inbox: %d", len(in))
	}
	// D replies by email, declining.
	reply := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nMETHOD:REPLY\r\nBEGIN:VEVENT\r\nATTENDEE;PARTSTAT=DECLINED:mailto:d@elsewhere.example\r\n" +
		"ORGANIZER:mailto:" + a.Address + "\r\nUID:" + uid + "\r\nSEQUENCE:0\r\nREQUEST-STATUS:2.0;Success\r\nDTSTAMP:19970612T190000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	rc, _ := ical.ParseOne([]byte(reply), "VCALENDAR")
	mail := BuildIMIP("d@elsewhere.example", a.Address, rc, f.svc.now())
	f.svc.IncomingMail(ctx, a, mail)
	if ps := partstatOf(f.event(t, a, uid), "d@elsewhere.example"); ps != "DECLINED" {
		t.Errorf("D: %q", ps)
	}
	// A reply in someone else's name is not taken.
	forged := strings.Replace(reply, "PARTSTAT=DECLINED:mailto:d@", "PARTSTAT=ACCEPTED:mailto:d@", 1)
	fc, _ := ical.ParseOne([]byte(forged), "VCALENDAR")
	f.svc.IncomingMail(ctx, a, BuildIMIP("mallory@elsewhere.example", a.Address, fc, f.svc.now()))
	if ps := partstatOf(f.event(t, a, uid), "d@elsewhere.example"); ps != "DECLINED" {
		t.Errorf("a forged reply changed D to %q", ps)
	}

	// A moves the event: a new REQUEST goes out; A deletes it: CANCEL.
	f.mail = nil
	acal := f.event(t, a, uid)
	acal.Sub("VEVENT")[0].Set(ical.Prop{Name: "DTSTART", Value: "19970702T200000Z"})
	acal.Sub("VEVENT")[0].Set(ical.Prop{Name: "DTEND", Value: "19970702T210000Z"})
	acal.Sub("VEVENT")[0].Set(ical.Prop{Name: "SEQUENCE", Value: "1"})
	if _, err := f.svc.PutEvent(ctx, a, f.calendar(t, a), "conf.ics", acal, Cond{}, true); err != nil {
		t.Fatal(err)
	}
	if len(f.mail) != 1 || !bytes.Contains(f.mail[0].raw, []byte("method=REQUEST")) {
		t.Errorf("update mail %d", len(f.mail))
	}
	if v := f.event(t, c, uid).Sub("VEVENT")[0].Value("DTSTART"); v != "19970702T200000Z" {
		t.Errorf("C has %s", v)
	}
	// B's answer stays only while the sequence stays: it moved, so B is
	// asked again (RFC 5546 2.1.4).
	o, _ := f.st.ObjectByName(ctx, a.ID, f.calendar(t, a).Collection.ID, "conf.ics")
	f.mail = nil
	if _, err := f.svc.DeleteEvent(ctx, a, f.calendar(t, a), o, "", 0, true); err != nil {
		t.Fatal(err)
	}
	if got := f.event(t, b, uid); got == nil || got.Sub("VEVENT")[0].Value("STATUS") != "CANCELLED" {
		t.Errorf("B's copy after the cancel: %v", got)
	}
	if len(f.mail) != 1 || !bytes.Contains(f.mail[0].raw, []byte("method=CANCEL")) {
		t.Errorf("cancel mail %d", len(f.mail))
	}
}

// RFC 5546 2.1.5: a REQUEST with a lower SEQUENCE than the one held is
// ignored; a request from someone who is not the organizer too.
func TestRFC5546_2_1_5_Sequence(t *testing.T) {
	f := setup(t)
	b := f.account(t)
	uid := RandomID()
	req := func(seq, summary, organizer string) *ical.Component {
		c, _ := ical.ParseOne([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:"+uid+
			"\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260201T100000Z\r\nSEQUENCE:"+seq+"\r\nSUMMARY:"+summary+
			"\r\nORGANIZER:mailto:"+organizer+"\r\nATTENDEE:mailto:"+b.Address+"\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), "VCALENDAR")
		return c
	}
	if err := f.svc.Apply(ctx, b, req("2", "Second", "o@elsewhere.example"), "o@elsewhere.example"); err != nil {
		t.Fatal(err)
	}
	f.svc.Apply(ctx, b, req("1", "First", "o@elsewhere.example"), "o@elsewhere.example")
	if s := f.event(t, b, uid).Sub("VEVENT")[0].Text("SUMMARY"); s != "Second" {
		t.Errorf("an older sequence was applied: %s", s)
	}
	if err := f.svc.Apply(ctx, b, req("3", "Spoofed", "o@elsewhere.example"), "mallory@elsewhere.example"); !errors.Is(err, ErrNotForUs) {
		t.Errorf("a request from another sender: %v", err)
	}
}

// RFC 4791 7.10 and RFC 5545 3.6.4: free/busy of an account, with a
// transparent and a declined event left out and a tentative one marked.
func TestFreeBusy(t *testing.T) {
	f := setup(t)
	a := f.account(t)
	put := func(uid, extra string) {
		cal, _, _, err := ParseCalendarObject([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:"+uid+
			"\r\nDTSTAMP:20260101T000000Z\r\n"+extra+"END:VEVENT\r\nEND:VCALENDAR\r\n"), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.PutEvent(ctx, a, f.calendar(t, a), uid+".ics", cal, Cond{}, false); err != nil {
			t.Fatal(err)
		}
	}
	put("busy", "DTSTART:20260301T090000Z\r\nDTEND:20260301T100000Z\r\nRRULE:FREQ=DAILY;COUNT=3\r\n")
	put("free", "DTSTART:20260301T110000Z\r\nDTEND:20260301T120000Z\r\nTRANSP:TRANSPARENT\r\n")
	put("tent", "DTSTART:20260301T093000Z\r\nDTEND:20260301T110000Z\r\nSTATUS:TENTATIVE\r\n")
	put("declined", "DTSTART:20260301T130000Z\r\nDTEND:20260301T140000Z\r\nORGANIZER:mailto:x@elsewhere.example\r\nATTENDEE;PARTSTAT=DECLINED:mailto:"+a.Address+"\r\n")
	fb, err := f.svc.FreeBusy(ctx, a, "", "mailto:"+a.Address, "q1", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range fb.Sub("VFREEBUSY")[0].All("FREEBUSY") {
		got = append(got, p.Param("FBTYPE")+" "+p.Value)
	}
	want := []string{" 20260301T090000Z/20260301T100000Z", "BUSY-TENTATIVE 20260301T100000Z/20260301T110000Z", " 20260302T090000Z/20260302T100000Z"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// Sharing: a sharee sees the owner's calendar with its rights and can
// write only with write rights.
func TestSharedAccess(t *testing.T) {
	f := setup(t)
	owner, sharee := f.account(t), f.account(t)
	cal := f.calendar(t, owner)
	if err := f.st.SetShare(ctx, owner.ID, cal.Collection.ID, sharee.ID, store.RightRead); err != nil {
		t.Fatal(err)
	}
	list, _ := f.svc.Collections(ctx, sharee, store.KindCalendar)
	var shared *Access
	for i := range list {
		if list[i].Owner == owner.ID {
			shared = &list[i]
		}
	}
	if shared == nil || shared.Rights != RightRead || shared.MayWrite() {
		t.Fatalf("%+v", list)
	}
	ev, _, _, _ := ParseCalendarObject([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:s1\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260101T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), "")
	if _, err := f.svc.PutEvent(ctx, sharee, *shared, "s1.ics", ev, Cond{}, false); !errors.Is(err, ErrForbidden) {
		t.Errorf("read-only write: %v", err)
	}
	f.st.SetShare(ctx, owner.ID, cal.Collection.ID, sharee.ID, store.RightWrite)
	acc, err := f.svc.Collection(ctx, sharee, owner.ID, cal.Collection.ID)
	if err != nil || !acc.MayWrite() {
		t.Fatalf("%+v %v", acc, err)
	}
	if _, err := f.svc.PutEvent(ctx, sharee, acc, "s1.ics", ev, Cond{}, false); err != nil {
		t.Errorf("write: %v", err)
	}
	if _, err := f.svc.Collection(ctx, f.account(t), owner.ID, cal.Collection.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger: %v", err)
	}
}

func TestParseCalendarObject(t *testing.T) {
	for cond, in := range map[string]string{
		"valid-calendar-data":            "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n",
		"valid-calendar-object-resource": "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nDTSTART:20260101T000000Z\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:b\r\nDTSTART:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		"supported-calendar-component":   "BEGIN:VCALENDAR\r\nBEGIN:VJOURNAL\r\nUID:a\r\nEND:VJOURNAL\r\nEND:VCALENDAR\r\n",
	} {
		_, _, _, err := ParseCalendarObject([]byte(in), "VEVENT VTODO")
		var ie *InvalidError
		if !errors.As(err, &ie) || ie.Cond != cond {
			t.Errorf("%s: %v", cond, err)
		}
	}
	cal, uid, kind, err := ParseCalendarObject([]byte("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VTODO\r\nUID:t\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"), "")
	if err != nil || uid != "t" || kind != "VTODO" || cal.Get("METHOD") != nil {
		t.Errorf("%v %s %s", err, uid, kind)
	}
}
