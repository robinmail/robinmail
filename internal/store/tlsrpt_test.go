package store

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTLSResults(t *testing.T) {
	s := open(t)
	// A day and domains of this test's own: the tables are not per account.
	domain := "tlsrpt-" + account(t, s).ID + ".test"
	now := time.Date(2031, 3, 4, 10, 0, 0, 0, time.UTC).Add(time.Duration(time.Now().UnixNano()%1000) * 24 * time.Hour)
	s.Now = func() time.Time { return now }
	day := TLSDay(now)
	policy := `{"policy-type":"sts","policy-domain":"` + domain + `"}`
	ok := TLSResult{Domain: domain, Policy: policy}
	bad := TLSResult{Domain: domain, Policy: policy, Result: "certificate-expired", MX: "mx." + domain, IP: "192.0.2.1"}
	for i := 0; i < 3; i++ {
		if err := s.CountTLSResult(ctx, ok); err != nil {
			t.Fatal(err)
		}
	}
	// Counting from many callers at once loses nothing.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.CountTLSResult(ctx, bad) }()
	}
	wg.Wait()
	got, err := s.TLSResults(ctx, day, domain)
	if err != nil || len(got) != 2 || got[0].Result != "" || got[0].Count != 3 || got[1] != (TLSCount{bad, 20}) {
		t.Fatalf("results: %+v, %v", got, err)
	}
	// The next day counts apart.
	now = now.Add(24 * time.Hour)
	s.CountTLSResult(ctx, ok)
	if got, _ := s.TLSResults(ctx, day+1, domain); len(got) != 1 || got[0].Count != 1 {
		t.Errorf("the next day: %+v", got)
	}

	// A report is due once, and one of many claimers gets it.
	due, err := s.TLSReportsDue(ctx, day)
	if err != nil || !has(due, domain) {
		t.Fatalf("due: %v, %v", due, err)
	}
	var claimed atomic.Int64
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := s.ClaimTLSReport(ctx, day, domain); err == nil && ok {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	if claimed.Load() != 1 {
		t.Errorf("%d of 10 claimed the report", claimed.Load())
	}
	if due, _ := s.TLSReportsDue(ctx, day); has(due, domain) {
		t.Error("a claimed report is still due")
	}
	if err := s.NoteTLSReport(ctx, day, domain, "sent to mailto:x@"+domain); err != nil {
		t.Fatal(err)
	}
	sent, err := s.TLSReportsSent(ctx, day, day)
	found := false
	for _, r := range sent {
		found = found || (r.Day == day && r.Domain == domain && r.Outcome == "sent to mailto:x@"+domain)
	}
	if err != nil || !found {
		t.Errorf("sent: %v", err)
	}

	// A report sent to us is kept once.
	in := TLSReportIn{Organization: "Company-X", ReportID: "id-" + domain, Domain: domain, Start: now.Add(-24 * time.Hour), End: now,
		Successful: 5326, Failed: 303, JSON: `{"report-id":"x"}`}
	if saved, err := s.SaveTLSReport(ctx, in); err != nil || !saved {
		t.Fatalf("save: %v, %v", saved, err)
	}
	if saved, err := s.SaveTLSReport(ctx, in); err != nil || saved {
		t.Fatalf("the same report again: %v, %v", saved, err)
	}
	list, err := s.TLSReportsReceived(ctx, 1000)
	var id int64
	for _, r := range list {
		if r.ReportID == in.ReportID {
			id = r.ID
		}
	}
	if err != nil || id == 0 {
		t.Fatalf("received: %v", err)
	}
	one, ok2, err := s.TLSReportReceived(ctx, id)
	if err != nil || !ok2 || one.JSON != in.JSON || one.Successful != 5326 || one.Failed != 303 || one.Organization != "Company-X" {
		t.Errorf("one report: %+v, %v", one, err)
	}
	if _, ok2, _ := s.TLSReportReceived(ctx, 1<<50); ok2 {
		t.Error("a report that is not there was found")
	}
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
