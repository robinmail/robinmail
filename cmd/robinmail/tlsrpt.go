package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	mail "github.com/mikepage/robinmail/internal/message"
	"github.com/mikepage/robinmail/internal/store"
	"github.com/mikepage/robinmail/internal/tlsrpt"
)

// tlsReporter makes the daily TLS reports (RFC 8460): for every domain
// mail was sent to on a day that is over, it asks DNS whether the domain
// wants reports and where, turns the day's counts into a report and
// sends it. Any node may do it; the store gives each report to one.
type tlsReporter struct {
	st *store.Store
	// host is our name: the submitter of the reports.
	host string
	// from is the address that sends the reports and is their contact,
	// asked for when a report is made (it may depend on the domains the
	// server has by then); empty sends none. organization names us.
	from         func(ctx context.Context) string
	organization string
	lookupTXT    func(ctx context.Context, name string) ([]string, error)
	// mail queues a report mail, signed. post sends one over HTTPS.
	mail func(ctx context.Context, key, from, rcpt string, raw []byte) error
	post *http.Client
	now  func() time.Time
	log  *slog.Logger
}

// tlsReportDays is how many finished days are looked at: a report that
// could not be made yesterday (the server was down) is made today.
const tlsReportDays = 3

// run sends the reports that are due and returns how many were sent.
func (r *tlsReporter) run(ctx context.Context) (int, error) {
	from := r.from(ctx)
	if from == "" {
		return 0, nil // no domain to send from yet
	}
	today := store.TLSDay(r.now())
	sent := 0
	for day := today - tlsReportDays; day < today; day++ {
		due, err := r.st.TLSReportsDue(ctx, day)
		if err != nil {
			return sent, err
		}
		for _, domain := range due {
			claimed, err := r.st.ClaimTLSReport(ctx, day, domain)
			if err != nil {
				return sent, err
			}
			if !claimed {
				continue // another node has it
			}
			outcome, ok := r.report(ctx, from, day, domain)
			if ok {
				sent++
			}
			if err := r.st.NoteTLSReport(ctx, day, domain, outcome); err != nil {
				r.log.Warn("tls report: noting the outcome failed", "domain", domain, "err", err)
			}
			r.log.Info("tls report", "domain", domain, "day", time.Unix(day*86400, 0).UTC().Format("2006-01-02"), "outcome", outcome)
		}
	}
	return sent, nil
}

// build turns the counts of a day and domain into the report.
func (r *tlsReporter) build(from string, day int64, domain string, counts []store.TLSCount) *tlsrpt.Report {
	start := time.Unix(day*86400, 0).UTC()
	rep := &tlsrpt.Report{
		OrganizationName: r.organization,
		DateRange:        tlsrpt.DateRange{Start: start, End: start.Add(24*time.Hour - time.Second)},
		ContactInfo:      from,
		// The same for a report that is made twice, so the receiver can
		// tell it is the same one.
		ReportID: fmt.Sprintf("%s.%s@%s", start.Format("20060102"), domain, r.host),
	}
	at := map[string]int{}
	for _, c := range counts {
		i, ok := at[c.Policy]
		if !ok {
			var p tlsrpt.Policy
			if json.Unmarshal([]byte(c.Policy), &p) != nil || p.Type == "" {
				p = tlsrpt.Policy{Type: tlsrpt.PolicyNone, Domain: domain}
			}
			i = len(rep.Policies)
			at[c.Policy] = i
			rep.Policies = append(rep.Policies, tlsrpt.PolicyResult{Policy: p})
		}
		pr := &rep.Policies[i]
		if c.Result == "" {
			pr.Summary.Successful += c.Count
			continue
		}
		pr.Summary.Failed += c.Count
		pr.FailureDetails = append(pr.FailureDetails, tlsrpt.FailureDetail{ResultType: c.Result, ReceivingMXHostname: c.MX,
			ReceivingIP: c.IP, FailedSessionCount: c.Count})
	}
	return rep
}

// report makes and sends one report. It returns what became of it, and
// whether it reached at least one place.
func (r *tlsReporter) report(ctx context.Context, from string, day int64, domain string) (string, bool) {
	records, err := r.lookupTXT(ctx, "_smtp._tls."+domain)
	var dnsErr *net.DNSError
	if err != nil && !(errors.As(err, &dnsErr) && dnsErr.IsNotFound) {
		return "the TXT lookup failed: " + err.Error(), false
	}
	rua, ok := tlsrpt.ParseRecord(records)
	if !ok {
		return "the domain asks for no reports", false
	}
	counts, err := r.st.TLSResults(ctx, day, domain)
	if err != nil || len(counts) == 0 {
		return "no counts", false
	}
	rep := r.build(from, day, domain, counts)
	var done, failed []string
	for _, uri := range rua {
		var err error
		switch {
		case strings.HasPrefix(strings.ToLower(uri), "mailto:"):
			to := uri[len("mailto:"):]
			if q := strings.IndexByte(to, '?'); q >= 0 {
				to = to[:q]
			}
			name := "TLS reports"
			raw := tlsrpt.BuildMail(tlsrpt.Mail{From: mail.Address{Name: &name, Email: from}, To: to, Submitter: r.host,
				Domain: domain, MessageID: mail.NewMessageID(r.host), Date: r.now()}, rep)
			err = r.mail(ctx, "tlsrpt:"+rep.ReportID+":"+strings.ToLower(to), from, to, raw)
		default:
			err = r.postReport(ctx, uri, rep)
		}
		if err != nil {
			failed = append(failed, uri+" ("+err.Error()+")")
		} else {
			done = append(done, uri)
		}
	}
	outcome := ""
	if len(done) > 0 {
		outcome = "sent to " + strings.Join(done, ", ")
	}
	if len(failed) > 0 {
		outcome = strings.TrimSpace(outcome + " failed for " + strings.Join(failed, ", "))
	}
	return outcome, len(done) > 0
}

// postReport sends a report to an https rua (RFC 8460 5.4): a POST of the
// gzip, with its media type.
func (r *tlsReporter) postReport(ctx context.Context, uri string, rep *tlsrpt.Report) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uri, bytes.NewReader(tlsrpt.Gzip(rep.JSON())))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/tlsrpt+gzip")
	resp, err := r.post.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// tlsrptResults prints the counts of the last days: what the next reports
// will say.
func (a *app) tlsrptResults(days string) error {
	n, err := strconv.Atoi(days)
	if err != nil || n < 1 || n > 30 {
		return fmt.Errorf("%q is not a number of days from 1 to 30", days)
	}
	today := store.TLSDay(time.Now())
	for day := today - int64(n) + 1; day <= today; day++ {
		counts, err := a.store.TLSResults(a.ctx, day, "")
		if err != nil {
			return err
		}
		for _, c := range counts {
			var p tlsrpt.Policy
			json.Unmarshal([]byte(c.Policy), &p)
			result := c.Result
			if result == "" {
				result = "ok"
			}
			fmt.Fprintf(a.stdout, "%s  %-30s  %-16s  %-28s  %6d  %s\n", time.Unix(day*86400, 0).UTC().Format("2006-01-02"),
				clip(c.Domain, 30), p.Type, result, c.Count, clip(strings.TrimSpace(c.MX+" "+c.IP), 60))
		}
	}
	sent, err := a.store.TLSReportsSent(a.ctx, 50)
	if err != nil {
		return err
	}
	sort.SliceStable(sent, func(i, j int) bool { return sent[i].Day < sent[j].Day })
	for _, s := range sent {
		fmt.Fprintf(a.stdout, "report of %s for %s: %s\n", time.Unix(s.Day*86400, 0).UTC().Format("2006-01-02"), clip(s.Domain, 40), clip(s.Outcome, 200))
	}
	return nil
}

// tlsrptReceived lists the reports other servers sent us.
func (a *app) tlsrptReceived(count string) error {
	n, err := strconv.Atoi(count)
	if err != nil || n < 1 {
		return fmt.Errorf("%q is not a count", count)
	}
	list, err := a.store.TLSReportsReceived(a.ctx, n)
	if err != nil {
		return err
	}
	for _, r := range list {
		fmt.Fprintf(a.stdout, "%-6d %s  %-28s  %-24s  %s to %s  ok %d  failed %d\n", r.ID, r.ReceivedAt.Format("2006-01-02 15:04"),
			clip(r.Organization, 28), clip(r.Domain, 24), r.Start.Format("2006-01-02"), r.End.Format("2006-01-02"), r.Successful, r.Failed)
	}
	return nil
}

// tlsrptShow prints one received report, as indented JSON.
func (a *app) tlsrptShow(id string) error {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("%q is not the number of a report", id)
	}
	r, ok, err := a.store.TLSReportReceived(a.ctx, n)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no report %d", n)
	}
	var b bytes.Buffer
	if json.Indent(&b, []byte(r.JSON), "", "  ") != nil {
		b.WriteString(r.JSON)
	}
	// A report is somebody else's text: nothing in it may reach the
	// terminal as a control sequence. encoding/json writes control
	// characters as escapes, so the JSON is safe as it is.
	fmt.Fprintln(a.stdout, b.String())
	return nil
}
