package labtest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/pim"
)

// Phase 9 next to clients and servers that are not ours: the Python
// caldav library and vdirsyncer against robinmail's CalDAV and CardDAV,
// and invitations (iTIP over iMIP) between robinmail and Postfix and
// Exim, whose users answer by mail as a calendar program would.

const davclients = "davclients"

// The user of the calendar and contact clients (lab.sh makes it).
const carol = "carol@" + robinDomain

// pimCall is jmap.call with the capabilities of phase 9.
func (j *jmap) pimCall(t testing.TB, calls ...[]any) []response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:calendars", "urn:ietf:params:jmap:contacts",
			"urn:ietf:params:jmap:principals"},
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

// events returns the events of the user whose title holds the text.
func (j *jmap) events(t testing.TB, title string) []map[string]any {
	t.Helper()
	res := j.pimCall(t,
		[]any{"CalendarEvent/query", map[string]any{"accountId": j.account, "filter": map[string]any{"title": title}}, "q"},
		[]any{"CalendarEvent/get", map[string]any{"accountId": j.account, "#ids": map[string]any{"resultOf": "q", "name": "CalendarEvent/query", "path": "/ids"}}, "g"})
	var out []map[string]any
	for _, e := range res[1].args["list"].([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

// partstat is the participationStatus of an address in an event.
func partstat(ev map[string]any, addr string) string {
	parts, _ := ev["participants"].(map[string]any)
	for _, p := range parts {
		pm := p.(map[string]any)
		if a, _ := pm["calendarAddress"].(string); strings.EqualFold(a, "mailto:"+addr) {
			s, _ := pm["participationStatus"].(string)
			return s
		}
	}
	return ""
}

func davExec(t testing.TB, argv ...string) string {
	t.Helper()
	out, err := execIn(davclients, "", nil, argv...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, out)
	}
	return out
}

// The Python caldav library (version in contrib/lab/davclients/Dockerfile):
// discovery, MKCALENDAR, PUT, an expanded time-range query, an update, a
// sync token, free/busy and a delete, each step checked by the script, and
// what it made seen over JMAP.
func TestDAVClients(t *testing.T) {
	lab(t)
	cell(t, "dav: python caldav", "robinmail")
	token := newToken()
	out := davExec(t, "python3", "/lab/caldav_check.py", "https://"+robinHost+"/dav/", carol, password(), token)
	for _, step := range []string{"principal", "calendars", "make_calendar", "save_event", "search_expand", "search_empty", "update",
		"sync_token", "freebusy", "delete"} {
		if !strings.Contains(out, "ok "+step) {
			t.Errorf("step %s missing:\n%s", step, out)
		}
	}
	j := newJMAP(t, carol)
	evs := j.events(t, "lab event changed "+token)
	if len(evs) != 1 {
		t.Fatalf("over JMAP: %d events", len(evs))
	}
	if rule, _ := evs[0]["recurrenceRule"].(map[string]any); rule["frequency"] != "daily" || rule["count"] != 3.0 {
		t.Errorf("the rule: %v", evs[0]["recurrenceRule"])
	}
	res := j.pimCall(t, []any{"Calendar/get", map[string]any{"accountId": j.account}, "c"})
	found := ""
	for _, c := range res[0].args["list"].([]any) {
		cm := c.(map[string]any)
		if cm["name"] == "lab "+token {
			found = cm["id"].(string)
		}
	}
	if found == "" {
		t.Fatalf("the calendar the library made is not there: %v", res[0].args["list"])
	}
	j.pimCall(t, []any{"Calendar/set", map[string]any{"accountId": j.account, "destroy": []string{found}, "onDestroyRemoveEvents": true}, "d"})
}

// vdirsyncer: discovery of the calendars and the address book, a sync to
// directories, then changes on both sides synced the other way.
func TestVdirsyncer(t *testing.T) {
	lab(t)
	cell(t, "dav: vdirsyncer", "robinmail")
	token := newToken()
	j := newJMAP(t, carol)
	davExec(t, "/lab/vdirsyncer.sh", carol, password(), "init")
	files := davExec(t, "/lab/vdirsyncer.sh", carol, password(), "ls")
	t.Logf("after the first sync:\n%s", files)

	// A card written on the client's side reaches the server.
	res := j.pimCall(t, []any{"AddressBook/get", map[string]any{"accountId": j.account}, "b"})
	book := res[0].args["list"].([]any)[0].(map[string]any)["id"].(string)
	card := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:" + token + "\r\nFN:Lab " + token + "\r\nN:" + token + ";Lab;;;\r\nEMAIL:lab@example.test\r\nEND:VCARD\r\n"
	if out, err := execIn(davclients, "", []byte(card), "sh", "-c", `d=$(ls -d /tmp/vds/`+carol+`/card/*/ | head -1) && cat > "$d/`+token+`.vcf"`); err != nil {
		t.Fatalf("writing the card: %v %s", err, out)
	}
	davExec(t, "/lab/vdirsyncer.sh", carol, password(), "sync")
	got := j.pimCall(t, []any{"ContactCard/query", map[string]any{"accountId": j.account, "filter": map[string]any{"uid": token}}, "q"})
	if ids := got[0].args["ids"].([]any); len(ids) != 1 {
		t.Fatalf("the card is not on the server: %v", got[0].args)
	}

	// A card and an event made on the server reach the client.
	made := j.pimCall(t,
		[]any{"ContactCard/set", map[string]any{"accountId": j.account, "create": map[string]any{"c": map[string]any{
			"addressBookIds": map[string]bool{book: true}, "uid": "srv-" + token, "name": map[string]any{"full": "Server " + token}}}}, "c"},
		[]any{"CalendarEvent/set", map[string]any{"accountId": j.account, "create": map[string]any{"e": map[string]any{
			"uid": "ev-" + token, "title": "Server " + token, "start": "2031-07-01T10:00:00", "timeZone": "Europe/Amsterdam", "duration": "PT1H"}}}, "e"})
	if made[0].args["created"] == nil || made[1].args["created"] == nil {
		t.Fatalf("JMAP: %v %v", made[0].args, made[1].args)
	}
	davExec(t, "/lab/vdirsyncer.sh", carol, password(), "sync")
	files = davExec(t, "/lab/vdirsyncer.sh", carol, password(), "ls")
	cardFile := davExec(t, "sh", "-c", `grep -l -r "Server `+token+`" /tmp/vds/`+carol+`/card || true`)
	eventFile := davExec(t, "sh", "-c", `grep -l -r "Server `+token+`" /tmp/vds/`+carol+`/cal || true`)
	if strings.TrimSpace(cardFile) == "" || strings.TrimSpace(eventFile) == "" {
		t.Fatalf("the server's card and event are not on the client:\n%s", files)
	}
	// A removal on the server is a removal on the client.
	cid := made[0].args["created"].(map[string]any)["c"].(map[string]any)["id"]
	j.pimCall(t, []any{"ContactCard/set", map[string]any{"accountId": j.account, "destroy": []any{cid}}, "d"})
	davExec(t, "/lab/vdirsyncer.sh", carol, password(), "sync")
	if s := davExec(t, "sh", "-c", `grep -l -r "Server `+token+`" /tmp/vds/`+carol+`/card || true`); strings.TrimSpace(s) != "" {
		t.Errorf("a card removed on the server stays on the client: %s", s)
	}
}

// An invitation from robinmail to a user of a peer, by email (RFC 6047):
// it arrives as a text/calendar part with METHOD:REQUEST; the peer's user
// answers with a REPLY by mail through their server; robinmail records the
// answer in the organizer's event. Between two accounts of robinmail the
// same goes without mail, and the attendee's answer through JMAP reaches
// the organizer.
func TestInvitations(t *testing.T) {
	lab(t)
	alice := newJMAP(t, "alice@"+robinDomain)
	create := func(t *testing.T, title, attendee string) string {
		t.Helper()
		res := alice.pimCall(t, []any{"CalendarEvent/set", map[string]any{"accountId": alice.account, "sendSchedulingMessages": true,
			"create": map[string]any{"e": map[string]any{"title": title, "start": "2031-08-04T14:00:00", "timeZone": "Europe/Amsterdam", "duration": "PT1H",
				"participants": map[string]any{
					"o": map[string]any{"@type": "Participant", "calendarAddress": "mailto:alice@" + robinDomain, "roles": map[string]any{"owner": true, "attendee": true},
						"participationStatus": "accepted"},
					"a": map[string]any{"@type": "Participant", "calendarAddress": "mailto:" + attendee, "roles": map[string]any{"attendee": true},
						"participationStatus": "needs-action", "expectReply": true}}}}}, "s"})
		created, _ := res[0].args["created"].(map[string]any)
		if created == nil {
			t.Fatalf("create: %v", res[0].args)
		}
		return created["e"].(map[string]any)["id"].(string)
	}
	cleanup := func(id string) {
		alice.pimCall(t, []any{"CalendarEvent/set", map[string]any{"accountId": alice.account, "destroy": []string{id}}, "d"})
	}

	t.Run("robin", func(t *testing.T) {
		cell(t, "imip: invitation and reply", "robinmail")
		title := "lab " + newToken()
		id := create(t, title, "bob@"+robinDomain)
		defer cleanup(id)
		bob := newJMAP(t, "bob@"+robinDomain)
		var evs []map[string]any
		waitFor(t, "the invitation in bob's calendar", 30*time.Second, func() bool {
			evs = bob.events(t, title)
			return len(evs) == 1
		})
		if evs[0]["isOrigin"] != false || partstat(evs[0], "bob@"+robinDomain) != "needs-action" {
			t.Fatalf("bob's copy: %v", evs[0])
		}
		var pid string
		for k, p := range evs[0]["participants"].(map[string]any) {
			if strings.EqualFold(p.(map[string]any)["calendarAddress"].(string), "mailto:bob@"+robinDomain) {
				pid = k
			}
		}
		bob.pimCall(t, []any{"CalendarEvent/set", map[string]any{"accountId": bob.account, "sendSchedulingMessages": true,
			"update": map[string]any{evs[0]["id"].(string): map[string]any{"participants/" + pid + "/participationStatus": "tentative"}}}, "u"})
		waitFor(t, "bob's answer in alice's event", 30*time.Second, func() bool {
			ev := alice.events(t, title)
			return len(ev) == 1 && partstat(ev[0], "bob@"+robinDomain) == "tentative"
		})
	})

	for _, p := range peers {
		if p.name == "opensmtpd" {
			continue // its sendmail is enough for mail, the two above are the ones asked for
		}
		t.Run(p.name, func(t *testing.T) {
			cell(t, "imip: invitation and reply", p.name)
			title := "lab " + newToken()
			attendee := "alice@" + p.domain
			id := create(t, title, attendee)
			defer cleanup(id)
			raw := p.find(t, "alice", title)
			msg, from, ok := pim.FindITIP(raw)
			if !ok || from != "alice@"+robinDomain || msg.Value("METHOD") != "REQUEST" {
				t.Fatalf("no iTIP REQUEST from alice in what %s delivered:\n%s", p.name, clip(string(raw), 3000))
			}
			if ct := strings.Join(header(raw, "Content-Type"), " "); !strings.Contains(ct, "multipart/mixed") {
				t.Errorf("Content-Type %s", ct)
			}
			ev := msg.Sub("VEVENT")[0]
			if ev.Text("SUMMARY") != title || !strings.EqualFold(ev.Value("ORGANIZER"), "mailto:alice@"+robinDomain) {
				t.Errorf("the VEVENT:\n%s", ical.Encode(ev))
			}
			// The peer's user accepts, as a calendar program would: a REPLY
			// with their own ATTENDEE line, by mail through their server.
			reply := &ical.Component{Name: "VCALENDAR"}
			reply.Add(ical.Prop{Name: "VERSION", Value: "2.0"})
			reply.SetText("PRODID", "-//lab//"+p.name+"//EN")
			reply.Add(ical.Prop{Name: "METHOD", Value: "REPLY"})
			rv := &ical.Component{Name: "VEVENT"}
			for _, name := range []string{"UID", "ORGANIZER", "DTSTART", "SEQUENCE"} {
				if x := ev.Get(name); x != nil {
					rv.Add(*x)
				}
			}
			rv.Add(ical.Prop{Name: "DTSTAMP", Value: ical.FormatUTC(time.Now())})
			rv.Add(ical.Prop{Name: "ATTENDEE", Params: []ical.Param{{Name: "PARTSTAT", Values: []string{"ACCEPTED"}}}, Value: "mailto:" + attendee})
			reply.Children = append(reply.Children, rv)
			p.send(t, attendee, []string{"alice@" + robinDomain}, pim.BuildIMIP(attendee, "alice@"+robinDomain, reply, time.Now()))
			waitFor(t, "the answer of "+attendee+" in alice's event", 60*time.Second, func() bool {
				evs := alice.events(t, title)
				return len(evs) == 1 && partstat(evs[0], attendee) == "accepted"
			})
			// alice cancels: the peer's user gets a CANCEL.
			alice.pimCall(t, []any{"CalendarEvent/set", map[string]any{"accountId": alice.account, "sendSchedulingMessages": true,
				"destroy": []string{id}}, "d"})
			waitFor(t, "the CANCEL at "+p.name, 60*time.Second, func() bool {
				out, err := execIn(p.name, "", nil, "sh", "-c", `grep -l -s -F -- "Cancelled: `+title+`" /home/alice/Maildir/new/*`)
				return err == nil && strings.TrimSpace(out) != ""
			})
		})
	}
}
