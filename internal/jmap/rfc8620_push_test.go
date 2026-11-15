package jmap

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/notify"
	"github.com/robinmail/robinmail/internal/store"
)

// Tests of push, written from RFC 8620 section 7. They need rqlite, as the
// other tests here.

// pushEnv is an env whose handler has a hub and is reachable over real
// HTTP: an event source is a response that does not end.
type pushEnv struct {
	*env
	hub *notify.Hub
	srv *httptest.Server
}

func newPushEnv(t *testing.T) *pushEnv {
	e := newEnv(t)
	hub := &notify.Hub{}
	e.st.OnChange = hub.Changed
	e.h.Push = hub
	e.h.MinPing = 50 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.h.ServeHTTP(w, r.WithContext(WithUser(r.Context(), e.user)))
	}))
	t.Cleanup(srv.Close)
	return &pushEnv{env: e, hub: hub, srv: srv}
}

// event is one event of a text/event-stream (the HTML standard's format,
// which RFC 8620 7.3 refers to).
type event struct{ name, id, data string }

type stream struct {
	t      *testing.T
	resp   *http.Response
	events chan event
	closed chan struct{}
	cancel context.CancelFunc
}

// open connects an event source and waits until the server has subscribed.
func (p *pushEnv) open(query string, header ...string) *stream {
	p.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", p.srv.URL+"/jmap/eventsource/?"+query, nil)
	req.Header.Set("Accept", "text/event-stream")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		p.t.Fatal(err)
	}
	s := &stream{t: p.t, resp: resp, events: make(chan event, 100), closed: make(chan struct{}), cancel: cancel}
	p.t.Cleanup(func() { cancel(); resp.Body.Close() })
	if resp.StatusCode != 200 {
		close(s.closed)
		return s
	}
	go func() {
		defer close(s.closed)
		sc := bufio.NewScanner(resp.Body)
		var ev event
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" || ev.data != "" {
					s.events <- ev
				}
				ev = event{}
			case strings.HasPrefix(line, ":"): // a comment
			case strings.HasPrefix(line, "event: "):
				ev.name = line[7:]
			case strings.HasPrefix(line, "id: "):
				ev.id = line[4:]
			case strings.HasPrefix(line, "data: "):
				ev.data += line[6:]
			default:
				p.t.Errorf("event stream: unexpected line %q", line)
			}
		}
	}()
	return s
}

func (s *stream) next(within time.Duration) (event, bool) {
	select {
	case ev := <-s.events:
		return ev, true
	case <-time.After(within):
		return event{}, false
	}
}

// state waits for the next "state" event and returns its StateChange.
func (s *stream) state() (StateChange, event) {
	s.t.Helper()
	for {
		ev, ok := s.next(5 * time.Second)
		if !ok {
			s.t.Fatal("no state event within 5 seconds")
		}
		if ev.name != "state" {
			continue
		}
		var sc StateChange
		dec := json.NewDecoder(strings.NewReader(ev.data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sc); err != nil {
			s.t.Fatalf("state event %q: %v", ev.data, err)
		}
		return sc, ev
	}
}

func (s *stream) quiet(d time.Duration) {
	s.t.Helper()
	select {
	case ev := <-s.events:
		if ev.name == "state" {
			s.t.Fatalf("unexpected state event: %s", ev.data)
		}
	case <-time.After(d):
	}
}

func typesOf(sc StateChange, account string) []string {
	var out []string
	for t := range sc.Changed[account] {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// RFC 8620 7.3: "The server MUST respond with a ... content type of
// text/event-stream", and 7.1: a StateChange has "@type": "StateChange" and
// "changed", a map of account id to a map of type name to that type's
// state, "the state property that would currently be returned by Foo/get".
// The example of 7.1:
//
//	{ "@type": "StateChange",
//	  "changed": { "a456": { "Mailbox": "0af7a512ce70", "Email": "d35ecb040aab", ... } } }
func TestRFC8620EventSourceStateChange(t *testing.T) {
	p := newPushEnv(t)
	s := p.open("types=*&closeafter=no&ping=0")
	if ct := s.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}
	if cc := s.resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q", cc)
	}

	e := p.deliver(store.Message{Raw: []byte("Subject: push\r\n\r\nhello\r\n")})
	sc, ev := s.state()
	if sc.Type != "StateChange" || len(sc.Changed) != 1 {
		t.Fatalf("StateChange: %s", ev.data)
	}
	// New mail: the Email, its thread, the mailbox it went into,
	// EmailDelivery (RFC 8621 1.5), and the account's Quota, whose "used"
	// moved (RFC 9425 5).
	if got, want := typesOf(sc, p.acct.ID), []string{"Email", "EmailDelivery", "Mailbox", "Quota", "Thread"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("types after a delivery: %v, want %v", got, want)
	}
	// The state pushed is the state /get returns now, and /changes from
	// the state before the write up to it shows the new email.
	state := p.state()
	for typ, st := range sc.Changed[p.acct.ID] {
		if st != state {
			t.Errorf("%s: pushed state %q, Email/get says %q", typ, st, state)
		}
	}
	if got := p.one("Email/get", `{"accountId":"`+p.acct.ID+`","ids":[]}`)["state"]; got != state {
		t.Errorf("Email/get state %v", got)
	}

	// A keyword change is no delivery, and touches no thread.
	if _, err := p.st.UpdateEmail(p.ctx, p.acct.ID, e.ID, store.Update{AddKeywords: []string{"$seen"}}); err != nil {
		t.Fatal(err)
	}
	sc, _ = s.state()
	if got, want := typesOf(sc, p.acct.ID), []string{"Email", "Mailbox"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("types after a keyword change: %v, want %v", got, want)
	}
	// RFC 8621 1.5: EmailDelivery "doesn't change when an Email is ... destroyed".
	if _, err := p.st.DestroyEmail(p.ctx, p.acct.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	sc, _ = s.state()
	if got, want := typesOf(sc, p.acct.ID), []string{"Email", "Mailbox", "Quota", "Thread"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("types after a destroy: %v, want %v", got, want)
	}
}

// RFC 8620 7.3, types: "Either '*', indicating the client wants push
// events for all types, or a comma-separated list of type names. If the
// latter, the server MUST only push changes for the types in this list."
func TestRFC8620EventSourceTypes(t *testing.T) {
	p := newPushEnv(t)
	mailbox := p.open("types=Mailbox,Nonsense&closeafter=no&ping=0")
	identity := p.open("types=Identity,EmailSubmission&closeafter=no&ping=0")
	p.deliver(store.Message{Raw: []byte("Subject: one\r\n\r\nhello\r\n")})
	sc, _ := mailbox.state()
	if got := typesOf(sc, p.acct.ID); !reflect.DeepEqual(got, []string{"Mailbox"}) {
		t.Fatalf("asked for Mailbox, got %v", got)
	}
	identity.quiet(300 * time.Millisecond)
	// The stream that had nothing to say about the delivery still hears
	// of an identity.
	if _, _, err := p.st.CreateIdentity(p.ctx, p.acct.ID, store.Identity{Email: p.acct.Address, Name: "Me"}, 0); err != nil {
		t.Fatal(err)
	}
	sc, _ = identity.state()
	if got := typesOf(sc, p.acct.ID); !reflect.DeepEqual(got, []string{"Identity"}) {
		t.Fatalf("asked for Identity, got %v", got)
	}
}

// RFC 8620 7.3, closeafter: "'state' ... the server MUST end the HTTP
// response after pushing a state event"; "'no'", the connection stays.
func TestRFC8620EventSourceCloseAfter(t *testing.T) {
	p := newPushEnv(t)
	once := p.open("types=*&closeafter=state&ping=0")
	stay := p.open("types=*&closeafter=no&ping=0")
	p.deliver(store.Message{Raw: []byte("Subject: one\r\n\r\nhello\r\n")})
	once.state()
	stay.state()
	select {
	case <-once.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closeafter=state: the response did not end after the state event")
	}
	p.deliver(store.Message{Raw: []byte("Subject: two\r\n\r\nhello\r\n")})
	stay.state()
	select {
	case <-stay.closed:
		t.Fatal("closeafter=no: the response ended")
	default:
	}
}

// RFC 8620 7.3, ping: "A positive integer value representing a length of
// time in seconds ... If non-zero, the server MUST send an event called
// 'ping' whenever this time elapses since the previous event was sent. This
// MUST NOT set a new event id ... The data ... MUST be a JSON object
// containing an interval property, the value of which is the interval in
// seconds the server is using". "If the value is 0, the server MUST NOT
// send ping events." The server "MAY modify a requested ping interval to be
// subject to a minimum and/or maximum value".
func TestRFC8620EventSourcePing(t *testing.T) {
	p := newPushEnv(t)
	p.h.MinPing = time.Second
	s := p.open("types=*&closeafter=no&ping=1")
	for i := 0; i < 2; i++ {
		ev, ok := s.next(3 * time.Second)
		if !ok {
			t.Fatal("no ping within 3 seconds of ping=1")
		}
		if ev.name != "ping" || ev.id != "" {
			t.Fatalf("event %+v, want a ping without an id", ev)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(ev.data), &data); err != nil || len(data) != 1 || data["interval"] != float64(1) {
			t.Fatalf("ping data %q", ev.data)
		}
	}
	none := p.open("types=*&closeafter=no&ping=0")
	if ev, ok := none.next(1500 * time.Millisecond); ok {
		t.Fatalf("ping=0 got an event: %+v", ev)
	}

	// A minimum: asked for 1 second, the server uses 5 and says so.
	p2 := newPushEnv(t)
	p2.h.MinPing = 0 // the default, 5 seconds
	s = p2.open("types=*&closeafter=no&ping=1")
	ev, ok := s.next(7 * time.Second)
	if !ok || ev.name != "ping" || ev.data != `{"interval":5}` {
		t.Fatalf("ping under the minimum: %+v %v, want interval 5", ev, ok)
	}
}

// RFC 8620 7.3: "the client ... will send a Last-Event-ID header ... the
// server can use this to work out whether the client has missed some
// changes". Our event ids are the state, so a client that comes back is
// told at once what changed while it was away, and a client that missed
// nothing is told nothing.
func TestRFC8620EventSourceLastEventID(t *testing.T) {
	p := newPushEnv(t)
	first := p.open("types=*&closeafter=state&ping=0")
	p.deliver(store.Message{Raw: []byte("Subject: one\r\n\r\nhello\r\n")})
	_, ev := first.state()
	if ev.id != p.state() {
		t.Fatalf("event id %q, state %q", ev.id, p.state())
	}
	<-first.closed

	// Away: an identity is made. Back with the id of the last event.
	if _, _, err := p.st.CreateIdentity(p.ctx, p.acct.ID, store.Identity{Email: p.acct.Address}, 0); err != nil {
		t.Fatal(err)
	}
	back := p.open("types=*&closeafter=no&ping=0", "Last-Event-ID", ev.id)
	sc, ev2 := back.state()
	if got := typesOf(sc, p.acct.ID); !reflect.DeepEqual(got, []string{"Identity"}) {
		t.Fatalf("missed changes: %v, want Identity", got)
	}
	if ev2.id != p.state() {
		t.Errorf("event id %q, state %q", ev2.id, p.state())
	}
	current := p.open("types=*&closeafter=no&ping=0", "Last-Event-ID", ev2.id)
	current.quiet(300 * time.Millisecond)
	// An id that is no state of ours is ignored.
	odd := p.open("types=*&closeafter=no&ping=0", "Last-Event-ID", "x-17")
	odd.quiet(300 * time.Millisecond)
}

// The parameters of RFC 8620 7.3 are all required by the URL template of
// the session object; a request that leaves one out or gives it a value
// the RFC does not have is refused. Only GET.
func TestRFC8620EventSourceArguments(t *testing.T) {
	p := newPushEnv(t)
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"types=*&closeafter=no&ping=0", 200},
		{"types=Email,Mailbox&closeafter=state&ping=300", 200},
		{"closeafter=no&ping=0", 400},
		{"types=&closeafter=no&ping=0", 400},
		{"types=Email,,Mailbox&closeafter=no&ping=0", 400},
		{"types=*&closeafter=maybe&ping=0", 400},
		{"types=*&ping=0", 400},
		{"types=*&closeafter=no", 400},
		{"types=*&closeafter=no&ping=-1", 400},
		{"types=*&closeafter=no&ping=soon", 400},
	} {
		s := p.open(tc.query)
		if s.resp.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d", tc.query, s.resp.StatusCode, tc.want)
		}
		s.cancel()
	}
	resp, err := http.Post(p.srv.URL+"/jmap/eventsource/?types=*&closeafter=no&ping=0", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("POST: %d", resp.StatusCode)
	}
	// The session names the endpoint with all three parameters.
	var sess struct {
		EventSourceURL string `json:"eventSourceUrl"`
	}
	json.Unmarshal(p.get("/jmap/session").Body.Bytes(), &sess)
	if !strings.HasSuffix(sess.EventSourceURL, "/jmap/eventsource/?types={types}&closeafter={closeafter}&ping={ping}") {
		t.Errorf("eventSourceUrl: %s", sess.EventSourceURL)
	}
}

// A stream ends when the client goes away or the server stops, and its
// subscription with it; an account cannot hold more than its share.
func TestEventSourceEnds(t *testing.T) {
	p := newPushEnv(t)
	closing := make(chan struct{})
	p.h.Closing = closing
	a := p.open("types=*&closeafter=no&ping=0")
	b := p.open("types=*&closeafter=no&ping=0")
	if n := p.hub.Listeners(); n != 2 {
		t.Fatalf("listeners: %d", n)
	}
	a.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for p.hub.Listeners() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the subscription of a client that went away is still there")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(closing)
	select {
	case <-b.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end when the server stopped")
	}

	p2 := newPushEnv(t)
	for i := 0; i < maxEventStreams; i++ {
		if s := p2.open("types=*&closeafter=no&ping=0"); s.resp.StatusCode != 200 {
			t.Fatalf("stream %d: %d", i, s.resp.StatusCode)
		}
	}
	if s := p2.open("types=*&closeafter=no&ping=0"); s.resp.StatusCode != 429 {
		t.Fatalf("one stream too many: %d", s.resp.StatusCode)
	}
}

// Without a notifier the endpoint says so.
func TestEventSourceOff(t *testing.T) {
	e := newEnv(t)
	if rec := e.get("/jmap/eventsource/?types=*&closeafter=no&ping=0"); rec.Code != 501 {
		t.Fatalf("status %d", rec.Code)
	}
}
