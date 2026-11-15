package pim

import (
	"strings"
	"testing"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/store"
)

// An iTIP message by email is applied when its From field names the
// organizer (REQUEST, CANCEL) or the attendee (REPLY). The From field of
// mail from outside is whatever the sender wrote.
func TestIMIPFromIsNotProof(t *testing.T) {
	f := setup(t)
	b := f.account(t)
	uid := "spoof-" + RandomID() + "@elsewhere.example"
	invite := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n" +
		"ORGANIZER:mailto:boss@elsewhere.example\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:" + b.Address + "\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:colleague@third.example\r\n" +
		"DTSTAMP:20260101T090000Z\r\nDTSTART:20260105T090000Z\r\nDTEND:20260105T100000Z\r\nSUMMARY:Board\r\n" +
		"LOCATION:Room 1\r\nUID:" + uid + "\r\nSEQUENCE:0\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	ic, _ := ical.ParseOne([]byte(invite), "VCALENDAR")
	f.svc.IncomingMail(ctx, b, BuildIMIP("boss@elsewhere.example", b.Address, ic, f.svc.now()), false)
	if f.event(t, b, uid) == nil {
		t.Fatal("the invitation was not taken")
	}
	// The colleague, who got the same invitation and so knows its UID,
	// writes the boss's address into From and moves the meeting.
	moved := strings.NewReplacer("SEQUENCE:0", "SEQUENCE:1", "LOCATION:Room 1", "LOCATION:https://evil.example/join").Replace(invite)
	mc, _ := ical.ParseOne([]byte(moved), "VCALENDAR")
	f.svc.IncomingMail(ctx, b, BuildIMIP("boss@elsewhere.example", b.Address, mc, f.svc.now()), false)
	if loc := f.event(t, b, uid).Sub("VEVENT")[0].Value("LOCATION"); loc != "Room 1" {
		t.Errorf("a forged update from outside changed the location to %q", loc)
	}
	// It is kept in the scheduling inbox for the user to look at.
	if in := inbox(t, f, b); len(in) != 2 || !strings.Contains(in[1].Data+in[0].Data, "evil.example") {
		t.Errorf("scheduling inbox: %d", len(in))
	}
	// A cancel the same way does not cancel.
	cancel := strings.NewReplacer("METHOD:REQUEST", "METHOD:CANCEL", "SEQUENCE:0", "SEQUENCE:2").Replace(invite)
	cc, _ := ical.ParseOne([]byte(cancel), "VCALENDAR")
	f.svc.IncomingMail(ctx, b, BuildIMIP("boss@elsewhere.example", b.Address, cc, f.svc.now()), false)
	if st := f.event(t, b, uid).Sub("VEVENT")[0].Value("STATUS"); st == "CANCELLED" {
		t.Error("a forged cancel from outside cancelled the event")
	}
	// From a sender whose domain passed DMARC the update is applied.
	f.svc.IncomingMail(ctx, b, BuildIMIP("boss@elsewhere.example", b.Address, mc, f.svc.now()), true)
	if loc := f.event(t, b, uid).Sub("VEVENT")[0].Value("LOCATION"); loc != "https://evil.example/join" {
		t.Errorf("an authenticated update was not applied: %q", loc)
	}
	_ = store.ObjectEvent
}

// A reply whose sender is not verified does not change the organizer's
// copy, even when its From field names the attendee.
func TestIMIPUnverifiedReply(t *testing.T) {
	f := setup(t)
	a := f.account(t)
	uid := "reply-" + RandomID() + "@example.test"
	object := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nORGANIZER:mailto:" + a.Address + "\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:d@elsewhere.example\r\nDTSTAMP:20260101T090000Z\r\n" +
		"DTSTART:20260105T090000Z\r\nDTEND:20260105T100000Z\r\nSUMMARY:x\r\nUID:" + uid + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	cal, _, _, err := ParseCalendarObject([]byte(object), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PutEvent(ctx, a, f.calendar(t, a), "x.ics", cal, Cond{IfNoneMatch: true}, false); err != nil {
		t.Fatal(err)
	}
	reply := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nMETHOD:REPLY\r\nBEGIN:VEVENT\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:d@elsewhere.example\r\n" +
		"ORGANIZER:mailto:" + a.Address + "\r\nUID:" + uid + "\r\nDTSTAMP:20260102T090000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	rc, _ := ical.ParseOne([]byte(reply), "VCALENDAR")
	f.svc.IncomingMail(ctx, a, BuildIMIP("d@elsewhere.example", a.Address, rc, f.svc.now()), false)
	if ps := partstatOf(f.event(t, a, uid), "d@elsewhere.example"); ps != "NEEDS-ACTION" {
		t.Errorf("an unverified reply set %q", ps)
	}
	// calendar.imip_unverified (the lab, which has no Rspamd) takes it.
	f.svc.IMIPUnverified = true
	f.svc.IncomingMail(ctx, a, BuildIMIP("d@elsewhere.example", a.Address, rc, f.svc.now()), false)
	if ps := partstatOf(f.event(t, a, uid), "d@elsewhere.example"); ps != "ACCEPTED" {
		t.Errorf("with imip_unverified the reply set %q", ps)
	}
}
