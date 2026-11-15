package jmap

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/jscal"
	"github.com/robinmail/robinmail/internal/store"
)

// Principals (RFC 9670): every account of the server is a principal, an
// individual, so that a user can find whom to share with and whom to
// invite, and ask when they are free (JMAP Calendars 2).

var principalProperties = []string{"id", "type", "name", "description", "email", "timeZone", "capabilities", "accounts"}

func (c *call) principalObject(p store.Principal) map[string]any {
	accounts := map[string]any{}
	visible := p.ID == c.user.AccountID
	if !visible {
		_, e := c.pimAccount(p.ID, "")
		visible = e == nil
	}
	var calAccount any
	if visible {
		calAccount = p.ID
		accounts[p.ID] = map[string]any{CapPrincipalsOwner: map[string]any{"accountIdForPrincipal": p.ID, "principalId": p.ID}}
	}
	return map[string]any{"id": p.ID, "type": "individual", "name": p.Address, "description": nil, "email": p.Address, "timeZone": nil,
		"capabilities": map[string]any{CapCalendars: map[string]any{"accountId": calAccount, "mayGetAvailability": true,
			"mayShareWith": true, "calendarAddress": "mailto:" + p.Address}},
		"accounts": accounts}
}

// principalGet is Principal/get (RFC 9670 3.1). Without ids it returns
// the first maxObjectsInGet principals.
func principalGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(args.Properties, principalProperties)
	if e != nil {
		return nil, e
	}
	if c.h.PIM == nil {
		return nil, mErr("unknownMethod", "principals are off")
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	list, notFound := []map[string]any{}, []string{}
	if args.IDs == nil {
		all, err := c.h.PIM.Store.Principals(c.ctx, "", c.h.limits.MaxObjectsInGet)
		if err != nil {
			return nil, storeErr(err)
		}
		for _, p := range all {
			list = append(list, pick(c.principalObject(p), props))
		}
	} else {
		if len(*args.IDs) > c.h.limits.MaxObjectsInGet {
			return nil, mErr("requestTooLarge", "more ids than maxObjectsInGet")
		}
		for _, id := range *args.IDs {
			p, err := c.h.PIM.Store.PrincipalByID(c.ctx, id)
			if errors.Is(err, store.ErrNoAccount) || strings.HasPrefix(id, "system-") {
				notFound = append(notFound, id)
				continue
			}
			if err != nil {
				return nil, storeErr(err)
			}
			list = append(list, pick(c.principalObject(p), props))
		}
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

// principalChanges: the list of accounts is not in the change log, so a
// client is told to fetch it again (RFC 8620 5.2 cannotCalculateChanges)
// unless nothing at all happened.
func principalChanges(c *call, raw json.RawMessage) (any, *methodError) {
	var args changesArgs
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if args.SinceState == nil {
		return nil, invalidArgs("sinceState is required")
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	if since, ok := parseState(*args.SinceState); !ok || since != state {
		return nil, mErr("cannotCalculateChanges", "fetch the principals again")
	}
	return map[string]any{"accountId": args.AccountID, "oldState": *args.SinceState, "newState": stateString(state),
		"hasMoreChanges": false, "created": []string{}, "updated": []string{}, "destroyed": []string{}}, nil
}

// principalQuery is Principal/query (RFC 9670 3.3) with the
// calendarAddress condition of JMAP Calendars 2.3.
func principalQuery(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID string `json:"accountId"`
		queryArgs
		Filter *struct {
			AccountIDs      []string `json:"accountIds"`
			Email           *string  `json:"email"`
			Name            *string  `json:"name"`
			Text            *string  `json:"text"`
			Type            *string  `json:"type"`
			TimeZone        *string  `json:"timeZone"`
			CalendarAddress *string  `json:"calendarAddress"`
		} `json:"filter"`
		Sort []sortArg `json:"sort"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if c.h.PIM == nil {
		return nil, mErr("unknownMethod", "principals are off")
	}
	limit, clamped, e := args.limit()
	if e != nil {
		return nil, e
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	text := ""
	f := args.Filter
	if f != nil {
		for _, s := range []*string{f.Text, f.Name, f.Email} {
			if s != nil {
				text = *s
			}
		}
		if f.CalendarAddress != nil {
			text = strings.TrimPrefix(strings.ToLower(*f.CalendarAddress), "mailto:")
		}
	}
	all, err := c.h.PIM.Store.Principals(c.ctx, text, 1000)
	if err != nil {
		return nil, storeErr(err)
	}
	var ids []string
	for _, p := range all {
		if f != nil {
			if f.Type != nil && *f.Type != "individual" {
				continue
			}
			if f.TimeZone != nil {
				continue
			}
			if f.CalendarAddress != nil && !strings.EqualFold(p.Address, text) {
				continue
			}
			if f.Email != nil && !strings.EqualFold(p.Address, *f.Email) {
				continue
			}
			if len(f.AccountIDs) > 0 {
				ok := false
				for _, a := range f.AccountIDs {
					ok = ok || a == p.ID
				}
				if !ok {
					continue
				}
			}
		}
		ids = append(ids, p.ID)
	}
	pos, page, e := window(ids, args.Position, args.Anchor, args.AnchorOffset, limit)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"accountId": args.AccountID, "queryState": stateString(state), "canCalculateChanges": false, "position": pos, "ids": page}
	if args.CalculateTotal {
		out["total"] = len(ids)
	}
	if clamped {
		out["limit"] = limit
	}
	return out, nil
}

// principalGetAvailability is Principal/getAvailability (JMAP Calendars
// 2.2): the busy periods of a principal; with details for one whose
// calendars the user can read.
func principalGetAvailability(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID       string    `json:"accountId"`
		ID              string    `json:"id"`
		UTCStart        string    `json:"utcStart"`
		UTCEnd          string    `json:"utcEnd"`
		ShowDetails     bool      `json:"showDetails"`
		EventProperties *[]string `json:"eventProperties"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if c.h.PIM == nil {
		return nil, mErr("unknownMethod", "principals are off")
	}
	from, ok1 := jscal.ParseUTC(args.UTCStart)
	to, ok2 := jscal.ParseUTC(args.UTCEnd)
	if !ok1 || !ok2 || !to.After(from) {
		return nil, invalidArgs("utcStart and utcEnd must be UTCDateTimes, the end after the start")
	}
	if to.Sub(from) > 366*24*time.Hour {
		return nil, mErr("tooLarge", "longer than maxAvailabilityDuration")
	}
	pr, err := c.h.PIM.Store.PrincipalByID(c.ctx, args.ID)
	if err != nil || strings.HasPrefix(args.ID, "system-") {
		return nil, mErr("notFound", "no such principal")
	}
	acct := store.Account{ID: pr.ID, Address: pr.Address}
	busy, err := c.h.PIM.BusyPeriods(c.ctx, acct, from, to)
	if err != nil {
		return nil, storeErr(err)
	}
	var readable *pimCtx
	if args.ShowDetails {
		if p, e := c.pimAccount(pr.ID, store.KindCalendar); e == nil {
			readable = p
		}
	}
	list := []map[string]any{}
	for _, b := range busy {
		item := map[string]any{"utcStart": jscal.UTCDateTime(b.Start), "utcEnd": jscal.UTCDateTime(b.End), "busyStatus": b.Status,
			"event": nil, "accountId": nil}
		if readable != nil && !strings.EqualFold(b.Instance.Component.Value("CLASS"), "PRIVATE") {
			if _, ok := readable.byID(b.Object.CollectionID); ok {
				if ed := c.parseEvent(readable, b.Object); ed != nil {
					rid := ""
					if !b.Instance.RecurrenceID.IsZero() {
						rid = jscal.LocalDateTime(b.Instance.RecurrenceID.In(eventZone(ed)))
					}
					ev := c.eventObject(readable, ed, rid, ical.LoadZone("UTC"))
					if args.EventProperties != nil {
						ev = pickEvent(ev, *args.EventProperties)
					}
					item["event"], item["accountId"] = ev, pr.ID
				}
			}
		}
		list = append(list, item)
	}
	return map[string]any{"list": list}, nil
}
