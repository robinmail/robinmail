package jmap

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mikepage/robinmail/internal/store"
)

// VacationResponse (RFC 8621 8): the automatic reply of an account. There
// is exactly one, with the id "singleton"; it can be read and updated,
// not created or destroyed.

// CapVacation is the capability of RFC 8621 1.3.3.
const CapVacation = "urn:ietf:params:jmap:vacationresponse"

const vacationID = "singleton"

var vacationProperties = []string{"id", "isEnabled", "fromDate", "toDate", "subject", "textBody", "htmlBody"}

// Bounds on what a user can store as a reply.
const (
	maxVacationSubject = 255
	maxVacationBody    = 64 << 10
)

func vacationObject(v store.VacationResponse) map[string]any {
	date := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return utcDate(*t)
	}
	text := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	return map[string]any{"id": vacationID, "isEnabled": v.Enabled, "fromDate": date(v.From), "toDate": date(v.To),
		"subject": text(v.Subject), "textBody": text(v.TextBody), "htmlBody": text(v.HTMLBody)}
}

// vacationGet is VacationResponse/get (RFC 8621 8.1).
func vacationGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(args.Properties, vacationProperties)
	if e != nil {
		return nil, e
	}
	if args.IDs != nil && len(*args.IDs) > c.h.limits.MaxObjectsInGet {
		return nil, mErr("requestTooLarge", "more ids than maxObjectsInGet")
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	v, err := c.h.Store.VacationResponse(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	list, notFound := []map[string]any{}, []string{}
	want := args.IDs == nil
	if args.IDs != nil {
		seen := map[string]bool{}
		for _, id := range *args.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			if id == vacationID {
				want = true
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	if want {
		list = append(list, pick(vacationObject(v), props))
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

// vacationPatch applies the properties of an update to a vacation
// response.
func vacationPatch(v *store.VacationResponse, patch map[string]json.RawMessage) *setError {
	var bad []string
	date := func(key string, dst **time.Time) {
		raw := patch[key]
		if string(raw) == "null" {
			*dst = nil
			return
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			bad = append(bad, key)
			return
		}
		// A UTCDate (RFC 8620 1.4): a date-time whose offset is "Z".
		t, err := time.Parse("2006-01-02T15:04:05Z", s)
		if err != nil {
			bad = append(bad, key)
			return
		}
		*dst = &t
	}
	text := func(key string, dst **string, max int, oneLine bool) {
		raw := patch[key]
		if string(raw) == "null" {
			*dst = nil
			return
		}
		var s string
		if json.Unmarshal(raw, &s) != nil || len(s) > max || !utf8.ValidString(s) || strings.ContainsRune(s, 0) ||
			(oneLine && strings.ContainsAny(s, "\r\n")) {
			bad = append(bad, key)
			return
		}
		*dst = &s
	}
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch key {
		case "isEnabled":
			if json.Unmarshal(patch[key], &v.Enabled) != nil || string(patch[key]) == "null" {
				bad = append(bad, key)
			}
		case "fromDate":
			date(key, &v.From)
		case "toDate":
			date(key, &v.To)
		case "subject":
			text(key, &v.Subject, maxVacationSubject, true)
		case "textBody":
			text(key, &v.TextBody, maxVacationBody, false)
		case "htmlBody":
			text(key, &v.HTMLBody, maxVacationBody, false)
		case "id":
			// Server-set; allowed when it is what it is (RFC 8620 5.3).
			var id string
			if json.Unmarshal(patch[key], &id) != nil || id != vacationID {
				bad = append(bad, key)
			}
		default:
			bad = append(bad, key)
		}
	}
	if len(bad) > 0 {
		return invalidProps("these properties of the vacation response cannot be set as given", bad...)
	}
	return nil
}

// vacationSet is VacationResponse/set (RFC 8621 8.2).
func vacationSet(c *call, raw json.RawMessage) (any, *methodError) {
	var args setArgs
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	run, e := c.begin(&args)
	if e != nil {
		return nil, e
	}
	// RFC 8620 5.3: "singleton: This is a singleton type, so you cannot
	// create another one or destroy the existing one."
	singleton := &setError{Type: "singleton", Description: "there is one vacation response per account"}
	for cid := range args.Create {
		run.notCreated[cid] = singleton
	}
	if args.Destroy != nil {
		for _, id := range *args.Destroy {
			if id == vacationID {
				run.notDestroyed[id] = singleton
			} else {
				run.notDestroyed[id] = &setError{Type: "notFound"}
			}
		}
	}
	for id, patch := range args.Update {
		if id != vacationID {
			run.notUpdated[id] = &setError{Type: "notFound"}
			continue
		}
		v, err := c.h.Store.VacationResponse(c.ctx, args.AccountID)
		if err != nil {
			return nil, storeErr(err)
		}
		if se := vacationPatch(&v, patch); se != nil {
			run.notUpdated[id] = se
			continue
		}
		state, err := c.h.Store.SetVacationResponse(c.ctx, args.AccountID, v, run.ifState)
		switch {
		case errors.Is(err, store.ErrStateMismatch):
			return nil, mErr("stateMismatch", "the state is not the ifInState given")
		case err != nil:
			run.notUpdated[id] = &setError{Type: "serverFail"}
		default:
			run.wroteState(state)
			run.updated = map[string]any{id: nil}
		}
	}
	return run.result(), nil
}
