package jmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/store"
)

// Push: the event source of RFC 8620 7.3. A client keeps one GET open and
// gets a StateChange object (7.1) whenever the state of a type it asked for
// changed, so it does not have to poll.
//
// PushSubscription (7.2), where the server posts changes to a URL the
// client names, is not offered: it needs an outbound HTTPS client with
// verification, expiry and retries per subscription, and message
// encryption (RFC 8291) to be worth having. Clients that are open at all
// use the event source.

// Notifier tells when an account changed. *notify.Hub has it.
type Notifier interface {
	// Subscribe returns a channel that gets a value after the account
	// changed, on this node or another, and a function that ends the
	// subscription. A wake-up may come without a change.
	Subscribe(account string) (<-chan struct{}, func())
}

// pushTypes are the types a StateChange can name: the data types of RFC
// 8621 that this server has, in the order they are written.
var pushTypes = []string{"Mailbox", "Thread", "Email", "EmailDelivery", "EmailSubmission", "Identity", "SieveScript"}

const (
	// DefaultMinPing and maxPing bound the ping interval a client may ask
	// for (RFC 8620 7.3: "The server MAY modify a requested ping interval
	// to be subject to a minimum and/or maximum value").
	DefaultMinPing = 5 * time.Second
	maxPing        = time.Hour
	// maxEventStreams is how many event sources one account may have open
	// on a node.
	maxEventStreams = 16
	// pushWriteTimeout is how long one event may take to write. A client
	// that stopped reading is dropped after it.
	pushWriteTimeout = 30 * time.Second
)

// StateChange is the object of RFC 8620 7.1.
type StateChange struct {
	Type    string                       `json:"@type"`
	Changed map[string]map[string]string `json:"changed"`
}

// changedTypes says which of the wanted types changed between two states
// of an account. Every type has the account's modseq as its state string,
// so strictly every type's state moves with every write; what is reported
// is the types whose objects changed, which is what a client can act on.
// EmailDelivery changes when an Email was created and not otherwise (RFC
// 8621 1.5). When the change log does not reach back to since, everything
// is reported.
func (h *Handler) changedTypes(r *http.Request, account string, since int64, wanted map[string]bool) ([]string, error) {
	c, err := h.Store.Changes(r.Context(), account, since)
	all := errors.Is(err, store.ErrCannotCalculateChanges)
	if err != nil && !all {
		return nil, err
	}
	some := func(s store.ChangeSet) bool { return len(s.Created)+len(s.Updated)+len(s.Destroyed) > 0 }
	changed := map[string]bool{
		"Mailbox": some(c.Mailbox), "Thread": some(c.Thread), "Email": some(c.Email),
		"EmailDelivery": len(c.Email.Created) > 0, "EmailSubmission": some(c.Submission), "Identity": some(c.Identity),
	}
	for name, set := range c.Other {
		changed[name] = some(*set)
	}
	var out []string
	for _, t := range pushTypes {
		if (all || changed[t]) && (wanted == nil || wanted[t]) {
			out = append(out, t)
		}
	}
	return out, nil
}

// serveEventSource is the event source connection of RFC 8620 7.3.
func (h *Handler) serveEventSource(w http.ResponseWriter, r *http.Request) {
	if h.Push == nil {
		problem(w, http.StatusNotImplemented, "about:blank", "this server has push turned off", nil)
		return
	}
	u, _ := UserFrom(r.Context())
	q := r.URL.Query()

	// types: "*" or a comma-separated list of type names. A name we do
	// not know is a type that never changes here.
	var wanted map[string]bool // nil is all
	if types := q.Get("types"); types == "" {
		problem(w, http.StatusBadRequest, "about:blank", `types is required: "*" or a comma-separated list of type names`, nil)
		return
	} else if types != "*" {
		wanted = map[string]bool{}
		for _, t := range strings.Split(types, ",") {
			if t == "" || t == "*" {
				problem(w, http.StatusBadRequest, "about:blank", "types is not a list of type names", nil)
				return
			}
			wanted[t] = true
		}
	}
	closeAfter := false
	switch q.Get("closeafter") {
	case "state":
		closeAfter = true
	case "no":
	default:
		problem(w, http.StatusBadRequest, "about:blank", `closeafter must be "state" or "no"`, nil)
		return
	}
	// ping: seconds between ping events, 0 for none.
	var ping time.Duration
	if p := q.Get("ping"); p == "" || len(p) > 9 || strings.TrimLeft(p, "0123456789") != "" {
		problem(w, http.StatusBadRequest, "about:blank", "ping must be a number of seconds, or 0", nil)
		return
	} else if n, _ := strconv.Atoi(p); n > 0 {
		lowest := h.MinPing
		if lowest <= 0 {
			lowest = DefaultMinPing
		}
		ping = min(max(time.Duration(n)*time.Second, lowest), maxPing)
	}

	h.mu.Lock()
	if h.streams[u.AccountID] >= maxEventStreams {
		h.mu.Unlock()
		problem(w, http.StatusTooManyRequests, "about:blank", "too many event sources open for this account", nil)
		return
	}
	h.streams[u.AccountID]++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.streams[u.AccountID]--; h.streams[u.AccountID] == 0 {
			delete(h.streams, u.AccountID)
		}
		h.mu.Unlock()
	}()

	// Subscribe before the first read of the state: a change after that
	// read then finds the subscription.
	wake, cancel := h.Push.Subscribe(u.AccountID)
	defer cancel()
	last, err := h.Store.State(r.Context(), u.AccountID)
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "about:blank", "the state cannot be read right now", nil)
		return
	}

	rc := http.NewResponseController(w)
	write := func(event, id string, data any) bool {
		body, err := json.Marshal(data)
		if err != nil {
			return false
		}
		// The server's write timeout is for a whole response; this one
		// has no end, so each event gets its own.
		rc.SetWriteDeadline(time.Now().Add(pushWriteTimeout))
		msg := "event: " + event + "\n"
		if id != "" {
			msg += "id: " + id + "\n"
		}
		if _, err := fmt.Fprintf(w, "%sdata: %s\n\n", msg, body); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// state sends a StateChange for what changed between from and to. It
	// reports whether the stream goes on.
	state := func(from, to int64) (sent, ok bool) {
		types, err := h.changedTypes(r, u.AccountID, from, wanted)
		if err != nil {
			return false, false
		}
		if len(types) == 0 {
			return false, true
		}
		states := map[string]string{}
		for _, t := range types {
			states[t] = stateString(to)
		}
		// The id is the state: a client that reconnects sends it back as
		// Last-Event-ID and is told at once what it missed.
		return true, write("state", stateString(to), StateChange{Type: "StateChange",
			Changed: map[string]map[string]string{u.AccountID: states}})
	}

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no") // a proxy must not hold events back
	w.WriteHeader(http.StatusOK)
	rc.SetWriteDeadline(time.Now().Add(pushWriteTimeout))
	if _, err := fmt.Fprint(w, ": robinmail\n\n"); err != nil || rc.Flush() != nil {
		return
	}

	// RFC 8620 7.3: with Last-Event-ID "the server can use this to work
	// out whether the client has missed some changes".
	if id, ok := parseState(r.Header.Get("Last-Event-ID")); ok && id != last {
		sent, ok := state(id, last)
		if !ok || (sent && closeAfter) {
			return
		}
	}

	var pings <-chan time.Time
	if ping > 0 {
		t := time.NewTicker(ping)
		defer t.Stop()
		pings = t.C
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.Closing:
			return
		case <-pings:
			if !write("ping", "", map[string]int{"interval": int(ping / time.Second)}) {
				return
			}
		case <-wake:
			now, err := h.Store.State(r.Context(), u.AccountID)
			if err != nil {
				return // the client reconnects, to this node or another
			}
			if now == last {
				continue
			}
			sent, ok := state(last, now)
			last = now
			if !ok || (sent && closeAfter) {
				return
			}
		}
	}
}
