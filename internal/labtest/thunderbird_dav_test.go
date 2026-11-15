package labtest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type tbEvent struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Start     string `json:"start"`
	Status    string `json:"status"`
	Attendees []struct {
		ID       string `json:"id"`
		Partstat string `json:"partstat"`
	} `json:"attendees"`
}

type tbCard struct {
	UID   string `json:"uid"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// TestThunderbirdCalDAV is Thunderbird's own calendar and address book on
// robinmail: the default calendar and address book registered by their
// URLs, as the dialogs do; an event and a contact made on each side seen
// on the other, an event changed and deleted in Thunderbird, and an
// invitation from another user of robinmail that shows in Thunderbird.
func TestThunderbirdCalDAV(t *testing.T) {
	lab(t)
	cell(t, "dav: Thunderbird calendar and contacts", "robinmail")
	user := labUser(t, "tb-dav")
	other := labUser(t, "tb-dav-org")
	j := newJMAP(t, user)
	res := j.pimCall(t,
		[]any{"Calendar/get", map[string]any{"accountId": j.account}, "c"},
		[]any{"AddressBook/get", map[string]any{"accountId": j.account}, "b"})
	cal := res[0].args["list"].([]any)[0].(map[string]any)["id"].(string)
	book := res[1].args["list"].([]any)[0].(map[string]any)["id"].(string)
	calURL := "https://" + robinHost + "/dav/calendars/" + j.account + "/" + cal + "/"
	bookURL := "https://" + robinHost + "/dav/addressbooks/" + j.account + "/" + book + "/"

	// What is on the server before Thunderbird comes.
	token := newToken()
	j.pimCall(t,
		[]any{"CalendarEvent/set", map[string]any{"accountId": j.account, "create": map[string]any{"e": map[string]any{
			"title": "Server " + token, "start": "2031-09-01T10:00:00", "timeZone": "Europe/Amsterdam", "duration": "PT1H"}}}, "e"},
		[]any{"ContactCard/set", map[string]any{"accountId": j.account, "create": map[string]any{"c": map[string]any{
			"name": map[string]any{"full": "Server " + token}, "emails": map[string]any{"e": map[string]any{"address": "server@example.test"}}}}}, "c"})

	x := startThunderbird(t, user, password())
	x.run(`await storeDAVPassword("https://`+robinHost+`", "robinmail", arguments[0], arguments[1]);`, user, password())
	var calID string
	x.eval(&calID, `return addCalDAV("robinmail", arguments[0], arguments[1]);`, calURL, user)
	var items []tbEvent
	waitFor(t, "the server's event in Thunderbird", 60*time.Second, func() bool {
		x.run(`await refreshCalendar(arguments[0]);`, calID)
		x.eval(&items, `return await calendarItems(arguments[0]);`, calID)
		for _, i := range items {
			if i.Title == "Server "+token {
				return true
			}
		}
		return false
	})
	for _, i := range items {
		if i.Title == "Server "+token && !strings.HasPrefix(i.Start, "20310901T100000") {
			t.Errorf("the start in Thunderbird: %s", i.Start)
		}
	}

	// An event made in Thunderbird.
	var uid string
	x.eval(&uid, `return await addEvent(arguments[0], arguments[1], "20310902T120000Z", "20310902T130000Z");`, calID, "Thunderbird "+token)
	waitFor(t, "Thunderbird's event on the server", 30*time.Second, func() bool {
		evs := j.events(t, "Thunderbird "+token)
		return len(evs) == 1 && evs[0]["uid"] == uid
	})
	x.run(`await modifyEvent(arguments[0], arguments[1], arguments[2]);`, calID, uid, "Thunderbird changed "+token)
	waitFor(t, "the change on the server", 30*time.Second, func() bool {
		return len(j.events(t, "Thunderbird changed "+token)) == 1
	})
	x.run(`await deleteEvent(arguments[0], arguments[1]);`, calID, uid)
	waitFor(t, "the deletion on the server", 30*time.Second, func() bool {
		return len(j.events(t, token)) == 1 // the server's event is all that is left
	})

	// An invitation from another user shows up after a refresh.
	o := newJMAP(t, other)
	o.pimCall(t, []any{"CalendarEvent/set", map[string]any{"accountId": o.account, "sendSchedulingMessages": true,
		"create": map[string]any{"i": map[string]any{"title": "Invitation " + token, "start": "2031-09-03T09:00:00", "timeZone": "Europe/Amsterdam",
			"duration": "PT30M", "participants": map[string]any{
				"o": map[string]any{"calendarAddress": "mailto:" + other, "roles": map[string]any{"owner": true, "attendee": true}, "participationStatus": "accepted"},
				"u": map[string]any{"calendarAddress": "mailto:" + user, "roles": map[string]any{"attendee": true}, "participationStatus": "needs-action", "expectReply": true}}}}}, "s"})
	waitFor(t, "the invitation in Thunderbird", 60*time.Second, func() bool {
		x.run(`await refreshCalendar(arguments[0]);`, calID)
		x.eval(&items, `return await calendarItems(arguments[0]);`, calID)
		for _, i := range items {
			if i.Title == "Invitation "+token {
				for _, a := range i.Attendees {
					if strings.EqualFold(a.ID, "mailto:"+user) && a.Partstat == "NEEDS-ACTION" {
						return true
					}
				}
			}
		}
		return false
	})

	// The address book.
	var bookUID string
	x.eval(&bookUID, `return await addCardDAV("robinmail", arguments[0], arguments[1]);`, bookURL, user)
	var cards []tbCard
	x.eval(&cards, `return bookCards(arguments[0]);`, bookUID)
	found := false
	for _, c := range cards {
		found = found || (c.Name == "Server "+token && c.Email == "server@example.test")
	}
	if !found {
		t.Fatalf("the server's card is not in Thunderbird: %+v", cards)
	}
	var cardUID string
	x.eval(&cardUID, `return await addCard(arguments[0], "Ada", arguments[1], "ada@example.test");`, bookUID, "Thunderbird"+token)
	waitFor(t, "Thunderbird's card on the server", 30*time.Second, func() bool {
		got := j.pimCall(t, []any{"ContactCard/query", map[string]any{"accountId": j.account, "filter": map[string]any{"name/surname": "Thunderbird" + token}}, "q"})
		return len(got[0].args["ids"].([]any)) == 1
	})
	// A card made on the server after Thunderbird's first fetch comes with
	// its next sync (RFC 6578).
	j.pimCall(t, []any{"ContactCard/set", map[string]any{"accountId": j.account, "create": map[string]any{"c": map[string]any{
		"name": map[string]any{"full": "Later " + token}}}}, "c"})
	waitFor(t, "the later card in Thunderbird", 30*time.Second, func() bool {
		x.run(`await syncBook(arguments[0]);`, bookUID)
		x.eval(&cards, `return bookCards(arguments[0]);`, bookUID)
		for _, c := range cards {
			if c.Name == "Later "+token {
				return true
			}
		}
		return false
	})
	b, _ := json.Marshal(cards)
	t.Logf("Thunderbird's address book: %s", clip(string(b), 600))
}
