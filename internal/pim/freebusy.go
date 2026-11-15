package pim

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/jscal"
	"github.com/robinmail/robinmail/internal/store"
)

// Busy is a period in which an account is not free.
type Busy struct {
	Start, End time.Time
	// Status is "confirmed", "tentative" or "unavailable" (JMAP Calendars
	// 2.2); in iCalendar BUSY, BUSY-TENTATIVE and BUSY-UNAVAILABLE.
	Status   string
	Object   store.Object
	Instance jscal.Instance
}

// FBType is the FBTYPE of a status (RFC 5545 3.2.9).
func FBType(status string) string {
	switch status {
	case "tentative":
		return "BUSY-TENTATIVE"
	case "unavailable":
		return "BUSY-UNAVAILABLE"
	}
	return "BUSY"
}

// BusyPeriods lists the busy time of an account between two instants:
// the instances of the events in its calendars that count for its
// availability, that are not transparent, cancelled or secret, and that
// it has not declined (JMAP Calendars 2.2, RFC 4791 7.10).
func (s *Service) BusyPeriods(ctx context.Context, acct store.Account, from, to time.Time) ([]Busy, error) {
	if err := s.Store.EnsureCollections(ctx, acct.ID); err != nil {
		return nil, err
	}
	cals, err := s.Store.Collections(ctx, acct.ID, store.KindCalendar)
	if err != nil {
		return nil, err
	}
	mine := s.Addresses(ctx, acct.Address)
	var out []Busy
	for _, c := range cals {
		if c.IncludeInAvailability == "none" || !c.IsSubscribed {
			continue
		}
		objs, err := s.Store.Objects(ctx, acct.ID, store.ObjectFilter{Kind: store.ObjectEvent, Collections: []int64{c.ID},
			From: &from, To: &to, WithData: true})
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			cal, err := ical.ParseOne([]byte(o.Data), "VCALENDAR")
			if err != nil {
				continue
			}
			obj := jscal.Read(cal, zoneOf(c))
			if obj.Kind != "VEVENT" {
				continue
			}
			inst, _ := obj.Instances(from, to, 1000)
			for _, i := range inst {
				comp := i.Component
				if strings.EqualFold(comp.Value("TRANSP"), "TRANSPARENT") || strings.EqualFold(comp.Value("CLASS"), "CONFIDENTIAL") {
					continue
				}
				status := "confirmed"
				if strings.EqualFold(comp.Value("STATUS"), "TENTATIVE") {
					status = "tentative"
				}
				attending := c.IncludeInAvailability != "attending"
				own := false
				for _, a := range comp.All("ATTENDEE") {
					if isOwn(mine, a.Value) {
						own = true
						switch strings.ToUpper(a.Param("PARTSTAT")) {
						case "DECLINED":
							status = ""
						case "TENTATIVE", "NEEDS-ACTION", "":
							if status != "" {
								status = "tentative"
							}
						case "ACCEPTED":
							attending = true
						}
					}
				}
				if org := comp.Value("ORGANIZER"); org == "" || isOwn(mine, org) || !own {
					attending = true
				}
				if status == "" || !attending {
					continue
				}
				start, end := i.Start, i.End
				if start.Before(from) {
					start = from
				}
				if end.After(to) {
					end = to
				}
				if !end.After(start) {
					continue
				}
				out = append(out, Busy{Start: start, End: end, Status: status, Object: o, Instance: i})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// FreeBusy answers a VFREEBUSY request (RFC 5546 3.3.2, RFC 6638 5) for an
// account: a VCALENDAR with METHOD:REPLY and one VFREEBUSY.
func (s *Service) FreeBusy(ctx context.Context, acct store.Account, organizer, attendee, uid string, from, to time.Time) (*ical.Component, error) {
	busy, err := s.BusyPeriods(ctx, acct, from, to)
	if err != nil {
		return nil, err
	}
	m := baseMessage("REPLY", s.prodID())
	fb := &ical.Component{Name: "VFREEBUSY"}
	if uid != "" {
		fb.SetText("UID", uid)
	}
	fb.Add(ical.Prop{Name: "DTSTAMP", Value: ical.FormatUTC(s.now())})
	fb.Add(ical.Prop{Name: "DTSTART", Value: ical.FormatUTC(from)})
	fb.Add(ical.Prop{Name: "DTEND", Value: ical.FormatUTC(to)})
	if organizer != "" {
		fb.Add(ical.Prop{Name: "ORGANIZER", Value: organizer})
	}
	fb.Add(ical.Prop{Name: "ATTENDEE", Value: attendee})
	for _, b := range merge(busy) {
		p := ical.Prop{Name: "FREEBUSY", Value: ical.FormatUTC(b.Start) + "/" + ical.FormatUTC(b.End)}
		if t := FBType(b.Status); t != "BUSY" {
			p.SetParam("FBTYPE", t)
		}
		fb.Add(p)
	}
	m.Children = append(m.Children, fb)
	return m, nil
}

// merge joins overlapping periods of one status; where statuses overlap
// the stronger wins: confirmed, then unavailable, then tentative.
func merge(in []Busy) []Busy {
	rank := map[string]int{"confirmed": 3, "unavailable": 2, "tentative": 1}
	// Cut time into the edges of all periods and give each slice the
	// strongest status over it.
	var edges []time.Time
	for _, b := range in {
		edges = append(edges, b.Start, b.End)
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].Before(edges[j]) })
	var out []Busy
	for i := 0; i+1 < len(edges); i++ {
		a, z := edges[i], edges[i+1]
		if !z.After(a) {
			continue
		}
		best := ""
		for _, b := range in {
			if b.Start.Before(z) && b.End.After(a) && rank[b.Status] > rank[best] {
				best = b.Status
			}
		}
		if best == "" {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Status == best && out[n-1].End.Equal(a) {
			out[n-1].End = z
			continue
		}
		out = append(out, Busy{Start: a, End: z, Status: best})
	}
	return out
}
