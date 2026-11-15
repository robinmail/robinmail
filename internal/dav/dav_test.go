package dav

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/robinmail/robinmail/internal/blob"
	"github.com/robinmail/robinmail/internal/pim"
	"github.com/robinmail/robinmail/internal/rqlite"
	"github.com/robinmail/robinmail/internal/store"
)

var ctx = context.Background()

type env struct {
	t     *testing.T
	st    *store.Store
	h     *Handler
	users map[string]store.Account
}

func setup(t *testing.T) *env {
	t.Helper()
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	db, err := rqlite.New(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	master, _ := blob.NewMasterKey(bytes.Repeat([]byte{0x42}, 32))
	st := store.New(db, blob.NewLocal(t.TempDir()), master)
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDomain(ctx, "example.test"); err != nil && !errors.Is(err, store.ErrDomainExists) {
		t.Fatal(err)
	}
	return &env{t: t, st: st, h: &Handler{PIM: &pim.Service{Store: st}, Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))}, users: map[string]store.Account{}}
}

func (e *env) user(label string) store.Account {
	var r [6]byte
	rand.Read(r[:])
	a, err := e.st.CreateAccount(ctx, label+"-"+hex.EncodeToString(r[:])+"@example.test", "$argon2id$test")
	if err != nil {
		e.t.Fatal(err)
	}
	e.users[label] = a
	return a
}

type reply struct {
	status int
	header http.Header
	body   string
}

func (e *env) do(u store.Account, method, path, body string, hdr ...string) reply {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r = r.WithContext(WithUser(r.Context(), u))
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	return reply{w.Code, w.Result().Header, string(b)}
}

// ms is a parsed multistatus.
type ms struct {
	Responses []struct {
		Href     string `xml:"href"`
		Status   string `xml:"status"`
		Propstat []struct {
			Prop struct {
				Inner string `xml:",innerxml"`
			} `xml:"prop"`
			Status string `xml:"status"`
		} `xml:"propstat"`
	} `xml:"response"`
	SyncToken string `xml:"sync-token"`
}

func parseMS(t *testing.T, body string) ms {
	t.Helper()
	var m ms
	if err := xml.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	return m
}

func (m ms) hrefs() []string {
	var out []string
	for _, r := range m.Responses {
		out = append(out, r.Href)
	}
	return out
}

// RFC 6764 5 and RFC 5397: the well-known URI leads to the root, where
// current-user-principal names the principal, whose properties name the
// homes (RFC 4791 6.2.1, RFC 6352 7.1.1, RFC 6638 2.1.1, 2.2.1).
func TestRFC6764_Discovery(t *testing.T) {
	e := setup(t)
	u := e.user("disc")
	r := e.do(u, "GET", "/.well-known/caldav", "")
	if r.status != http.StatusMovedPermanently || r.header.Get("Location") != "/dav/" {
		t.Fatalf("well-known: %d %s", r.status, r.header.Get("Location"))
	}
	r = e.do(u, "PROPFIND", "/.well-known/carddav", `<d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`, "Depth", "0")
	if r.status != 207 || !strings.Contains(r.body, "/dav/principals/"+u.ID+"/") {
		t.Fatalf("root: %d %s", r.status, r.body)
	}
	r = e.do(u, "PROPFIND", "/dav/principals/"+u.ID+"/", `<propfind xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:A="urn:ietf:params:xml:ns:carddav">
		<prop><C:calendar-home-set/><A:addressbook-home-set/><C:calendar-user-address-set/><C:schedule-inbox-URL/><C:schedule-outbox-URL/><displayname/><resourcetype/><foo xmlns="urn:x"/></prop></propfind>`, "Depth", "0")
	for _, want := range []string{"/dav/calendars/" + u.ID + "/", "/dav/addressbooks/" + u.ID + "/", "mailto:" + u.Address,
		"/dav/calendars/" + u.ID + "/inbox/", "/dav/calendars/" + u.ID + "/outbox/", "<d:principal/>", "404 Not Found"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("principal: no %q in\n%s", want, r.body)
		}
	}
	// The home lists the default calendar, the inbox and the outbox.
	r = e.do(u, "PROPFIND", "/dav/calendars/"+u.ID+"/", `<propfind xmlns="DAV:"><prop><resourcetype/><displayname/></prop></propfind>`, "Depth", "1")
	m := parseMS(t, r.body)
	if len(m.Responses) != 4 || !strings.Contains(r.body, "<c:calendar/>") || !strings.Contains(r.body, "<c:schedule-inbox/>") {
		t.Errorf("home: %v\n%s", m.hrefs(), r.body)
	}
	// RFC 4918 9.1: Depth infinity may be refused.
	if r := e.do(u, "PROPFIND", "/dav/calendars/"+u.ID+"/", "", "Depth", "infinity"); r.status != 403 || !strings.Contains(r.body, "propfind-finite-depth") {
		t.Errorf("infinity: %d %s", r.status, r.body)
	}
	// Nobody reaches another's home.
	other := e.user("other")
	if r := e.do(u, "PROPFIND", "/dav/calendars/"+other.ID+"/", "", "Depth", "0"); r.status != 403 {
		t.Errorf("another's home: %d", r.status)
	}
	if r := e.do(u, "OPTIONS", "/dav/", ""); !strings.Contains(r.header.Get("DAV"), "calendar-access") || !strings.Contains(r.header.Get("DAV"), "addressbook") {
		t.Errorf("OPTIONS DAV %q", r.header.Get("DAV"))
	}
}

// RFC 4791 5.3.1.2: the MKCALENDAR example, then RFC 4791 5.3.2's PUT.
func TestRFC4791_MkcalendarAndPut(t *testing.T) {
	e := setup(t)
	u := e.user("cal")
	r := e.do(u, "MKCALENDAR", "/dav/calendars/"+u.ID+"/work/", `<?xml version="1.0" encoding="utf-8" ?>
   <C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
     <D:set><D:prop>
         <D:displayname>Lisa's Events</D:displayname>
         <C:calendar-description xml:lang="en">Calendar restricted to events.</C:calendar-description>
         <C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>
         <C:calendar-timezone><![CDATA[BEGIN:VCALENDAR
PRODID:-//Example Corp.//CalDAV Client//EN
VERSION:2.0
BEGIN:VTIMEZONE
TZID:US-Eastern
LAST-MODIFIED:19870101T000000Z
BEGIN:STANDARD
DTSTART:19671029T020000
RRULE:FREQ=YEARLY;BYDAY=1SU;BYMONTH=11
TZOFFSETFROM:-0400
TZOFFSETTO:-0500
TZNAME:Eastern Standard Time (US & Canada)
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:19870405T020000
RRULE:FREQ=YEARLY;BYDAY=2SU;BYMONTH=3
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
TZNAME:Eastern Daylight Time (US & Canada)
END:DAYLIGHT
END:VTIMEZONE
END:VCALENDAR
]]></C:calendar-timezone>
       </D:prop></D:set></C:mkcalendar>`)
	if r.status != http.StatusCreated {
		t.Fatalf("MKCALENDAR: %d %s", r.status, r.body)
	}
	// The calendar is where the client made it.
	r = e.do(u, "PROPFIND", "/dav/calendars/"+u.ID+"/work/", `<propfind xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><prop>
		<displayname/><C:calendar-description/><C:supported-calendar-component-set/><C:calendar-timezone/></prop></propfind>`, "Depth", "0")
	for _, want := range []string{"Lisa&#39;s Events", "Calendar restricted to events.", `<c:comp name="VEVENT"/>`, "TZID:America/New_York"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("no %q in\n%s", want, r.body)
		}
	}
	if r := e.do(u, "MKCALENDAR", "/dav/calendars/"+u.ID+"/work/", ""); r.status != http.StatusMethodNotAllowed {
		t.Errorf("MKCALENDAR again: %d", r.status)
	}

	// RFC 4791 5.3.2: a new resource with If-None-Match: *.
	event := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example Corp.//CalDAV Client//EN\r\nBEGIN:VEVENT\r\nUID:20010712T182145Z-123401@example.com\r\n" +
		"DTSTAMP:20060712T182145Z\r\nDTSTART:20060714T170000Z\r\nDTEND:20060715T040000Z\r\nSUMMARY:Independence Day\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	path := "/dav/calendars/" + u.ID + "/work/new.ics"
	r = e.do(u, "PUT", path, event, "Content-Type", "text/calendar", "If-None-Match", "*")
	if r.status != http.StatusCreated || r.header.Get("ETag") == "" {
		t.Fatalf("PUT: %d %s", r.status, r.body)
	}
	etag := r.header.Get("ETag")
	if r := e.do(u, "PUT", path, event, "Content-Type", "text/calendar", "If-None-Match", "*"); r.status != http.StatusPreconditionFailed {
		t.Errorf("If-None-Match on an existing resource: %d", r.status)
	}
	if r := e.do(u, "GET", path, ""); r.status != 200 || r.header.Get("ETag") != etag || !strings.Contains(r.body, "SUMMARY:Independence Day") {
		t.Errorf("GET %d %s", r.status, r.header.Get("ETag"))
	}
	// 5.3.2.1 preconditions.
	todo := strings.ReplaceAll(event, "VEVENT", "VTODO")
	if r := e.do(u, "PUT", "/dav/calendars/"+u.ID+"/work/todo.ics", todo, "Content-Type", "text/calendar"); r.status != 403 || !strings.Contains(r.body, "supported-calendar-component") {
		t.Errorf("a VTODO in an event calendar: %d %s", r.status, r.body)
	}
	if r := e.do(u, "PUT", "/dav/calendars/"+u.ID+"/work/dup.ics", event, "Content-Type", "text/calendar"); r.status != 409 || !strings.Contains(r.body, "no-uid-conflict") {
		t.Errorf("the same UID twice: %d %s", r.status, r.body)
	}
	if r := e.do(u, "PUT", "/dav/calendars/"+u.ID+"/work/bad.ics", "BEGIN:VCALENDAR\r\nnonsense\r\n", "Content-Type", "text/calendar"); r.status != 403 || !strings.Contains(r.body, "valid-calendar-data") {
		t.Errorf("bad data: %d %s", r.status, r.body)
	}
	if r := e.do(u, "PUT", "/dav/calendars/"+u.ID+"/work/x.ics", event, "Content-Type", "text/plain"); r.status != 415 {
		t.Errorf("wrong type: %d", r.status)
	}
	// An update with the right ETag, then a delete with a stale one.
	changed := strings.Replace(event, "Independence Day", "4th of July", 1)
	r = e.do(u, "PUT", path, changed, "Content-Type", "text/calendar", "If-Match", etag)
	if r.status != http.StatusNoContent {
		t.Fatalf("update: %d", r.status)
	}
	if r := e.do(u, "DELETE", path, "", "If-Match", etag); r.status != http.StatusPreconditionFailed {
		t.Errorf("stale delete: %d", r.status)
	}
	if r := e.do(u, "DELETE", path, ""); r.status != http.StatusNoContent {
		t.Errorf("delete: %d", r.status)
	}
	if r := e.do(u, "GET", path, ""); r.status != 404 {
		t.Errorf("after delete: %d", r.status)
	}
}

// The events of RFC 4791 Appendix B, shortened: one plain, one recurring
// with an override, one all day.
var appendixB = map[string]string{
	"abcd1.ics": "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example Corp.//CalDAV Client//EN\r\nBEGIN:VEVENT\r\nDTSTAMP:20060206T001102Z\r\n" +
		"DTSTART;TZID=US/Eastern:20060102T100000\r\nDURATION:PT1H\r\nSUMMARY:Event #1\r\nDescription:Go Steelers!\r\nUID:74855313FA803DA593CD579A@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
	"abcd2.ics": "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example Corp.//CalDAV Client//EN\r\nBEGIN:VEVENT\r\nDTSTAMP:20060206T001121Z\r\n" +
		"DTSTART;TZID=US/Eastern:20060102T120000\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=5\r\nSUMMARY:Event #2\r\nUID:00959BC664CA650E933C892C@example.com\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nDTSTAMP:20060206T001121Z\r\nDTSTART;TZID=US/Eastern:20060104T140000\r\nDURATION:PT1H\r\nRECURRENCE-ID;TZID=US/Eastern:20060104T120000\r\n" +
		"SUMMARY:Event #2 bis\r\nUID:00959BC664CA650E933C892C@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
	"abcd3.ics": "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example Corp.//CalDAV Client//EN\r\nBEGIN:VEVENT\r\nATTENDEE;PARTSTAT=ACCEPTED;ROLE=CHAIR:mailto:cyrus@example.com\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:lisa@example.com\r\nDTSTAMP:20060206T001220Z\r\nDTSTART;TZID=US/Eastern:20060104T100000\r\nDURATION:PT1H\r\n" +
		"LAST-MODIFIED:20060206T001330Z\r\nORGANIZER:mailto:cyrus@example.com\r\nSEQUENCE:1\r\nSTATUS:TENTATIVE\r\nSUMMARY:Event #3\r\nUID:DC6C50A017428C5216A2F1CD@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
}

func (e *env) calendarWithAppendixB(u store.Account) string {
	e.t.Helper()
	cal := "/dav/calendars/" + u.ID + "/" + store.CalendarID(e.defaultCalendar(u)) + "/"
	for n, data := range appendixB {
		if r := e.do(u, "PUT", cal+n, data, "Content-Type", "text/calendar"); r.status != http.StatusCreated {
			e.t.Fatalf("PUT %s: %d %s", n, r.status, r.body)
		}
	}
	return cal
}

func (e *env) defaultCalendar(u store.Account) int64 {
	e.st.EnsureCollections(ctx, u.ID)
	cals, _ := e.st.Collections(ctx, u.ID, store.KindCalendar)
	return cals[0].ID
}

// RFC 4791 7.8.1: calendar-query with a time range; 7.8.7: a text match
// on an attendee's PARTSTAT; 7.9.1: calendar-multiget with a missing href.
func TestRFC4791_Reports(t *testing.T) {
	e := setup(t)
	u := e.user("rep")
	cal := e.calendarWithAppendixB(u)
	r := e.do(u, "REPORT", cal, `<?xml version="1.0" encoding="utf-8" ?>
   <C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
     <D:prop><D:getetag/><C:calendar-data/></D:prop>
     <C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">
       <C:time-range start="20060104T000000Z" end="20060105T000000Z"/>
     </C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`, "Depth", "1")
	m := parseMS(t, r.body)
	got := strings.Join(m.hrefs(), " ")
	if len(m.Responses) != 2 || !strings.Contains(got, "abcd2.ics") || !strings.Contains(got, "abcd3.ics") {
		t.Errorf("time range: %v", m.hrefs())
	}
	if !strings.Contains(r.body, "Event #2 bis") {
		t.Errorf("calendar-data missing:\n%s", r.body)
	}
	r = e.do(u, "REPORT", cal, `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/></D:prop>
     <C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">
       <C:prop-filter name="ATTENDEE"><C:text-match collation="i;ascii-casemap">mailto:lisa@example.com</C:text-match>
         <C:param-filter name="PARTSTAT"><C:text-match collation="i;ascii-casemap">NEEDS-ACTION</C:text-match></C:param-filter>
       </C:prop-filter></C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`, "Depth", "1")
	if m := parseMS(t, r.body); len(m.Responses) != 1 || !strings.HasSuffix(m.Responses[0].Href, "abcd3.ics") {
		t.Errorf("partstat filter: %v", m.hrefs())
	}
	r = e.do(u, "REPORT", cal, `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop>
       <D:href>`+cal+`abcd1.ics</D:href><D:href>`+cal+`mtg1.ics</D:href></C:calendar-multiget>`, "Depth", "1")
	m = parseMS(t, r.body)
	if len(m.Responses) != 2 || !strings.Contains(m.Responses[1].Status, "404") || !strings.Contains(r.body, "Go Steelers!") {
		t.Errorf("multiget:\n%s", r.body)
	}
	// RFC 4791 9.6.5: expand.
	r = e.do(u, "REPORT", cal, `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data>
		<C:expand start="20060103T000000Z" end="20060105T000000Z"/></C:calendar-data></D:prop><D:href>`+cal+`abcd2.ics</D:href></C:calendar-multiget>`, "Depth", "1")
	if strings.Count(r.body, "BEGIN:VEVENT") != 2 || strings.Contains(r.body, "RRULE") || !strings.Contains(r.body, "RECURRENCE-ID:20060103T170000Z") {
		t.Errorf("expand:\n%s", r.body)
	}
	// RFC 4791 7.10: free-busy-query.
	r = e.do(u, "REPORT", cal, `<C:free-busy-query xmlns:C="urn:ietf:params:xml:ns:caldav"><C:time-range start="20060104T140000Z" end="20060105T220000Z"/></C:free-busy-query>`, "Depth", "1")
	// Event #3 is tentative; Event #2 of the 4th was moved to 14:00 EST.
	if !strings.Contains(r.body, "FREEBUSY;FBTYPE=BUSY-TENTATIVE:20060104T150000Z/20060104T160000Z") || !strings.Contains(r.body, "FREEBUSY:20060104T190000Z/20060104T200000Z") {
		t.Errorf("free-busy:\n%s", r.body)
	}
	// RFC 3253 3.6: an unknown report.
	if r := e.do(u, "REPORT", cal, `<x:nope xmlns:x="urn:x"/>`); r.status != 403 || !strings.Contains(r.body, "supported-report") {
		t.Errorf("unknown report %d", r.status)
	}
}

// RFC 6578 3.4 to 3.6: the initial sync, a change and a removal, and an
// invalid token.
func TestRFC6578_SyncCollection(t *testing.T) {
	e := setup(t)
	u := e.user("sync")
	cal := e.calendarWithAppendixB(u)
	q := func(token string) reply {
		return e.do(u, "REPORT", cal, `<?xml version="1.0" encoding="utf-8" ?><D:sync-collection xmlns:D="DAV:"><D:sync-token>`+token+
			`</D:sync-token><D:sync-level>1</D:sync-level><D:prop><D:getetag/></D:prop></D:sync-collection>`)
	}
	r := q("")
	m := parseMS(t, r.body)
	if len(m.Responses) != 3 || m.SyncToken == "" {
		t.Fatalf("initial: %v %q", m.hrefs(), m.SyncToken)
	}
	e.do(u, "DELETE", cal+"abcd1.ics", "")
	e.do(u, "PUT", cal+"abcd3.ics", strings.Replace(appendixB["abcd3.ics"], "Event #3", "Event #3!", 1), "Content-Type", "text/calendar")
	r = q(m.SyncToken)
	m2 := parseMS(t, r.body)
	if len(m2.Responses) != 2 || m2.SyncToken == m.SyncToken {
		t.Fatalf("second: %s", r.body)
	}
	for _, x := range m2.Responses {
		switch {
		case strings.HasSuffix(x.Href, "abcd1.ics") && !strings.Contains(x.Status, "404"):
			t.Errorf("removed member without 404: %+v", x)
		case strings.HasSuffix(x.Href, "abcd3.ics") && len(x.Propstat) == 0:
			t.Errorf("changed member without props")
		}
	}
	if r := q(m2.SyncToken); len(parseMS(t, r.body).Responses) != 0 {
		t.Errorf("nothing changed: %s", r.body)
	}
	if r := q("http://example.com/ns/sync/1234"); r.status != 403 || !strings.Contains(r.body, "valid-sync-token") {
		t.Errorf("invalid token: %d %s", r.status, r.body)
	}
	// A property sync-token on the collection (RFC 6578 4).
	r = e.do(u, "PROPFIND", cal, `<propfind xmlns="DAV:"><prop><sync-token/><getctag xmlns="http://calendarserver.org/ns/"/></prop></propfind>`, "Depth", "0")
	if !strings.Contains(r.body, m2.SyncToken) {
		t.Errorf("sync-token property:\n%s", r.body)
	}
}

// RFC 6352 6.3.2 and 8.6: a card is stored and found by addressbook-query.
func TestRFC6352_CardDAV(t *testing.T) {
	e := setup(t)
	u := e.user("card")
	e.st.EnsureCollections(ctx, u.ID)
	books, _ := e.st.Collections(ctx, u.ID, store.KindAddressBook)
	book := "/dav/addressbooks/" + u.ID + "/" + store.AddressBookID(books[0].ID) + "/"
	card := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:1234-5678-9000-1\r\nFN:Cyrus Daboo\r\nN:Daboo;Cyrus;;;\r\nEMAIL;TYPE=INTERNET,PREF:cyrus@example.com\r\nNICKNAME:me\r\nEND:VCARD\r\n"
	if r := e.do(u, "PUT", book+"newcard.vcf", card, "Content-Type", "text/vcard", "If-None-Match", "*"); r.status != 201 {
		t.Fatalf("PUT: %d %s", r.status, r.body)
	}
	if r := e.do(u, "PUT", book+"other.vcf", strings.Replace(card, "1234-5678-9000-1", "x", 1), "Content-Type", "text/vcard"); r.status != 201 {
		t.Fatalf("PUT 2: %d", r.status)
	}
	// RFC 6352 6.3.2.1: no UID is invalid.
	if r := e.do(u, "PUT", book+"bad.vcf", "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:x\r\nEND:VCARD\r\n", "Content-Type", "text/vcard"); r.status != 403 || !strings.Contains(r.body, "valid-address-data") {
		t.Errorf("no UID: %d %s", r.status, r.body)
	}
	// RFC 6352 8.6.4, the example: "NICKNAME equals me" (here with one more
	// card that only allof would drop).
	r := e.do(u, "REPORT", book, `<?xml version="1.0" encoding="utf-8" ?>
   <C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
     <D:prop><D:getetag/><C:address-data><C:prop name="VERSION"/><C:prop name="UID"/><C:prop name="NICKNAME"/><C:prop name="EMAIL"/><C:prop name="FN"/></C:address-data></D:prop>
     <C:filter><C:prop-filter name="NICKNAME"><C:text-match collation="i;unicode-casemap" match-type="equals">me</C:text-match></C:prop-filter></C:filter>
   </C:addressbook-query>`, "Depth", "1")
	if m := parseMS(t, r.body); len(m.Responses) != 2 || !strings.Contains(r.body, "FN:Cyrus Daboo") {
		t.Errorf("query:\n%s", r.body)
	}
	r = e.do(u, "REPORT", book, `<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop><D:getetag/></D:prop>
     <C:filter test="allof"><C:prop-filter name="UID"><C:text-match match-type="starts-with">1234</C:text-match></C:prop-filter>
       <C:prop-filter name="EMAIL"><C:text-match match-type="ends-with">@EXAMPLE.COM</C:text-match></C:prop-filter></C:filter>
     <C:limit><C:nresults>5</C:nresults></C:limit></C:addressbook-query>`, "Depth", "1")
	if m := parseMS(t, r.body); len(m.Responses) != 1 || !strings.HasSuffix(m.Responses[0].Href, "newcard.vcf") {
		t.Errorf("allof: %v", m.hrefs())
	}
	r = e.do(u, "REPORT", book, `<C:addressbook-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop><C:address-data version="4.0"/></D:prop>
		<D:href>`+book+`newcard.vcf</D:href></C:addressbook-multiget>`, "Depth", "1")
	if !strings.Contains(r.body, "VERSION:4.0") {
		t.Errorf("multiget as 4.0:\n%s", r.body)
	}
	// The whole address book as one file.
	if r := e.do(u, "GET", book, ""); strings.Count(r.body, "BEGIN:VCARD") != 2 || r.header.Get("Content-Type") != "text/vcard; charset=utf-8" {
		t.Errorf("export: %s", r.body)
	}
	// Extended MKCOL (RFC 5689 3.4) for a second address book.
	r = e.do(u, "MKCOL", "/dav/addressbooks/"+u.ID+"/friends/", `<D:mkcol xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:set><D:prop>
		<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype><D:displayname>Friends</D:displayname></D:prop></D:set></D:mkcol>`)
	if r.status != 201 {
		t.Errorf("MKCOL: %d %s", r.status, r.body)
	}
	if r := e.do(u, "MOVE", book+"other.vcf", "", "Destination", "http://example.test/dav/addressbooks/"+u.ID+"/friends/moved.vcf"); r.status != 201 {
		t.Errorf("MOVE: %d %s", r.status, r.body)
	}
	if r := e.do(u, "GET", "/dav/addressbooks/"+u.ID+"/friends/moved.vcf", ""); r.status != 200 {
		t.Errorf("after MOVE: %d", r.status)
	}
}

// RFC 4918 9.2: PROPPATCH is all or nothing; a protected property fails
// and the others are 424.
func TestRFC4918_Proppatch(t *testing.T) {
	e := setup(t)
	u := e.user("pp")
	cal := "/dav/calendars/" + u.ID + "/" + store.CalendarID(e.defaultCalendar(u)) + "/"
	r := e.do(u, "PROPPATCH", cal, `<D:propertyupdate xmlns:D="DAV:" xmlns:I="http://apple.com/ns/ical/" xmlns:Z="http://ns.example.com/standards/z39.50/">
		<D:set><D:prop><D:displayname>Home</D:displayname><I:calendar-color>#FF0000FF</I:calendar-color>
		<Z:Authors><Z:Author>Jim Whitehead</Z:Author></Z:Authors></D:prop></D:set></D:propertyupdate>`)
	if r.status != 207 || !strings.Contains(r.body, "200 OK") {
		t.Fatalf("%d %s", r.status, r.body)
	}
	r = e.do(u, "PROPFIND", cal, `<propfind xmlns="DAV:"><prop><displayname/><calendar-color xmlns="http://apple.com/ns/ical/"/><Authors xmlns="http://ns.example.com/standards/z39.50/"/></prop></propfind>`, "Depth", "0")
	for _, want := range []string{"Home", "#FF0000FF", "Jim Whitehead"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("no %q in\n%s", want, r.body)
		}
	}
	r = e.do(u, "PROPPATCH", cal, `<D:propertyupdate xmlns:D="DAV:"><D:set><D:prop><D:displayname>X</D:displayname><D:getetag>"x"</D:getetag></D:prop></D:set></D:propertyupdate>`)
	if !strings.Contains(r.body, "403 Forbidden") || !strings.Contains(r.body, "424 Failed Dependency") {
		t.Errorf("protected:\n%s", r.body)
	}
	r = e.do(u, "PROPFIND", cal, `<propfind xmlns="DAV:"><prop><displayname/></prop></propfind>`, "Depth", "0")
	if !strings.Contains(r.body, "Home") {
		t.Errorf("a failed PROPPATCH changed something:\n%s", r.body)
	}
}

// RFC 6638 5: a free/busy request POSTed to the outbox; RFC 6638 3.2: an
// event with an attendee of this server reaches that attendee's calendar
// and inbox.
func TestRFC6638_Scheduling(t *testing.T) {
	e := setup(t)
	org, att := e.user("org"), e.user("att")
	cal := "/dav/calendars/" + org.ID + "/" + store.CalendarID(e.defaultCalendar(org)) + "/"
	inv := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:sched-1\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260310T100000Z\r\nDTEND:20260310T110000Z\r\n" +
		"SUMMARY:Planning\r\nORGANIZER:mailto:" + org.Address + "\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:" + org.Address + "\r\nATTENDEE;RSVP=TRUE:mailto:" + att.Address + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if r := e.do(org, "PUT", cal+"sched-1.ics", inv, "Content-Type", "text/calendar"); r.status != 201 {
		t.Fatalf("PUT %d %s", r.status, r.body)
	}
	r := e.do(att, "PROPFIND", "/dav/calendars/"+att.ID+"/inbox/", `<propfind xmlns="DAV:"><prop><getetag/></prop></propfind>`, "Depth", "1")
	if m := parseMS(t, r.body); len(m.Responses) != 2 {
		t.Errorf("attendee's inbox: %v", m.hrefs())
	}
	acal := "/dav/calendars/" + att.ID + "/" + store.CalendarID(e.defaultCalendar(att)) + "/"
	r = e.do(att, "GET", acal+"sched-1.ics", "")
	if r.status != 200 || !strings.Contains(r.body, "SUMMARY:Planning") {
		t.Fatalf("attendee's copy: %d", r.status)
	}
	// The attendee accepts by PUT; the organizer's copy shows it.
	accepted := strings.Replace(r.body, "RSVP=TRUE", "PARTSTAT=ACCEPTED", 1)
	if r := e.do(att, "PUT", acal+"sched-1.ics", accepted, "Content-Type", "text/calendar"); r.status != 204 {
		t.Fatalf("accept: %d %s", r.status, r.body)
	}
	if r := e.do(org, "GET", cal+"sched-1.ics", ""); !strings.Contains(strings.ReplaceAll(r.body, "\r\n ", ""), "PARTSTAT=ACCEPTED;SCHEDULE-STATUS=2.0:mailto:"+att.Address) {
		t.Errorf("organizer's copy:\n%s", r.body)
	}
	// Free/busy of the attendee, asked by the organizer.
	fb := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nMETHOD:REQUEST\r\nBEGIN:VFREEBUSY\r\nUID:fb-1\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260310T000000Z\r\nDTEND:20260311T000000Z\r\nORGANIZER:mailto:" + org.Address + "\r\nATTENDEE:mailto:" + att.Address + "\r\n" +
		"ATTENDEE:mailto:nobody@example.test\r\nEND:VFREEBUSY\r\nEND:VCALENDAR\r\n"
	r = e.do(org, "POST", "/dav/calendars/"+org.ID+"/outbox/", fb, "Content-Type", "text/calendar")
	if !strings.Contains(r.body, "FREEBUSY:20260310T100000Z/20260310T110000Z") || !strings.Contains(r.body, "3.7;") {
		t.Errorf("free/busy:\n%s", r.body)
	}
	// Only as oneself.
	if r := e.do(att, "POST", "/dav/calendars/"+att.ID+"/outbox/", fb, "Content-Type", "text/calendar"); r.status != 403 {
		t.Errorf("as another organizer: %d", r.status)
	}
}

// Sharing: a calendar shared read-only shows up in the sharee's home with
// read privileges only; writing fails; with write rights it works.
func TestSharedCalendar(t *testing.T) {
	e := setup(t)
	owner, sharee := e.user("owner"), e.user("sharee")
	cal := e.defaultCalendar(owner)
	e.st.SetShare(ctx, owner.ID, cal, sharee.ID, store.RightRead)
	seg := owner.ID + "-" + store.CalendarID(cal)
	path := "/dav/calendars/" + sharee.ID + "/" + seg + "/"
	r := e.do(sharee, "PROPFIND", path, `<propfind xmlns="DAV:"><prop><current-user-privilege-set/><resourcetype/><displayname/></prop></propfind>`, "Depth", "0")
	if r.status != 207 || strings.Contains(r.body, "write-content") || !strings.Contains(r.body, "<d:read/>") || !strings.Contains(r.body, owner.Address) {
		t.Fatalf("%d %s", r.status, r.body)
	}
	ev := appendixB["abcd1.ics"]
	if r := e.do(sharee, "PUT", path+"x.ics", ev, "Content-Type", "text/calendar"); r.status != 403 {
		t.Errorf("read-only PUT: %d", r.status)
	}
	e.st.SetShare(ctx, owner.ID, cal, sharee.ID, store.RightWrite)
	if r := e.do(sharee, "PUT", path+"x.ics", ev, "Content-Type", "text/calendar"); r.status != 201 {
		t.Errorf("read-write PUT: %d %s", r.status, r.body)
	}
	if r := e.do(owner, "GET", "/dav/calendars/"+owner.ID+"/"+store.CalendarID(cal)+"/x.ics", ""); r.status != 200 {
		t.Errorf("the owner sees it: %d", r.status)
	}
	// The sharee "deletes" the calendar: it leaves their home only.
	if r := e.do(sharee, "DELETE", path, ""); r.status != 204 {
		t.Errorf("unsubscribe: %d", r.status)
	}
	if r := e.do(owner, "GET", "/dav/calendars/"+owner.ID+"/"+store.CalendarID(cal)+"/x.ics", ""); r.status != 200 {
		t.Errorf("the owner's calendar went: %d", r.status)
	}
}

// XML bombs and DTDs are refused (encoding/xml expands no entities).
func TestHostileXML(t *testing.T) {
	e := setup(t)
	u := e.user("xml")
	for _, body := range []string{
		`<!DOCTYPE d [<!ENTITY a "aaaa">]><propfind xmlns="DAV:"><prop>&a;</prop></propfind>`,
		strings.Repeat("<a>", maxDepth+5),
		`<propfind xmlns="DAV:"><prop>`,
	} {
		if r := e.do(u, "PROPFIND", "/dav/", body, "Depth", "0"); r.status != 400 {
			t.Errorf("%.40s: %d", body, r.status)
		}
	}
}
