package store

import (
	"context"
	"time"
)

// TLS reporting (RFC 8460). Outbound: every node counts how the TLS of its
// deliveries went, per day, recipient domain, policy and outcome; once a
// day one node turns the counts of a domain into a report and sends it.
// Inbound: reports other servers send us are kept for the administrator.
// None of it is per account.

// TLSResult is one outcome to count: a session with a mail server of a
// recipient domain.
type TLSResult struct {
	Domain string
	// Policy is the policy that applied, as the JSON object of RFC 8460
	// 4.4 ("policy"): sessions under the same policy are counted together.
	Policy string
	// Result is the result-type of a failure, or "" for a session that
	// worked.
	Result string
	// MX and IP are the host that failed; empty for a success, so that
	// successes are one row per policy.
	MX, IP string
}

// TLSCount is a row of counts.
type TLSCount struct {
	TLSResult
	Count int64
}

// TLSDay is the number of the UTC day a moment falls in.
func TLSDay(t time.Time) int64 { return t.Unix() / 86400 }

// TLSResultRetention is how long counts are kept: a report is sent the day
// after, and a few days more allow for a server that was down.
const (
	TLSResultRetention = 7 * 24 * time.Hour
	TLSReportRetention = 90 * 24 * time.Hour
)

// CountTLSResult adds one to the count of an outcome on the current day.
func (s *Store) CountTLSResult(ctx context.Context, r TLSResult) error {
	b := s.newBatch("")
	// Not sent again after an unknown outcome: it counts.
	b.add(`INSERT INTO tls_result (day, domain, policy, result, mx, ip, count) VALUES (:day, :domain, :policy, :result, :mx, :ip, 1)
		ON CONFLICT (day, domain, policy, result, mx, ip) DO UPDATE SET count = count + 1`,
		"day", TLSDay(s.Now()), "domain", r.Domain, "policy", r.Policy, "result", r.Result, "mx", r.MX, "ip", r.IP)
	_, err := s.run(ctx, b)
	return err
}

// TLSResults returns the counts of a day: of one domain, or of all for "".
func (s *Store) TLSResults(ctx context.Context, day int64, domain string) ([]TLSCount, error) {
	res, err := s.query(ctx, "", q(`SELECT domain, policy, result, mx, ip, count FROM tls_result
		WHERE day = :day AND (:domain = '' OR domain = :domain) ORDER BY domain, policy, result, mx, ip`, "day", day, "domain", domain))
	if err != nil {
		return nil, err
	}
	out := make([]TLSCount, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, TLSCount{TLSResult{Domain: asString(row[0]), Policy: asString(row[1]), Result: asString(row[2]),
			MX: asString(row[3]), IP: asString(row[4])}, asInt(row[5])})
	}
	return out, nil
}

// TLSReportsDue lists the domains that have counts for a day and no
// report of that day yet.
func (s *Store) TLSReportsDue(ctx context.Context, day int64) ([]string, error) {
	res, err := s.query(ctx, "", q(`SELECT DISTINCT domain FROM tls_result r WHERE day = :day
		AND NOT EXISTS (SELECT 1 FROM tls_report_sent t WHERE t.day = r.day AND t.domain = r.domain) ORDER BY domain`, "day", day))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, asString(row[0]))
	}
	return out, nil
}

// ClaimTLSReport makes the report of a day and domain this caller's to
// send: of several nodes that try, one gets true. outcome is what became
// of it, for the administrator ("sent to ...", "no record").
func (s *Store) ClaimTLSReport(ctx context.Context, day int64, domain string) (bool, error) {
	b := s.newBatch("")
	b.add(`INSERT OR IGNORE INTO tls_report_sent (day, domain, at, outcome) VALUES (:day, :domain, :now, '')`, "day", day, "domain", domain)
	res, err := s.run(ctx, b)
	if err != nil {
		return false, err
	}
	return res[0].RowsAffected > 0, nil
}

// NoteTLSReport records what became of a claimed report.
func (s *Store) NoteTLSReport(ctx context.Context, day int64, domain, outcome string) error {
	b := s.newBatch("")
	b.idempotent() // sets a row to a given value
	b.add(`UPDATE tls_report_sent SET outcome = :outcome WHERE day = :day AND domain = :domain`, "day", day, "domain", domain, "outcome", outcome)
	_, err := s.run(ctx, b)
	return err
}

// TLSReportSent is a report this server made, or decided not to.
type TLSReportSent struct {
	Day     int64
	Domain  string
	At      time.Time
	Outcome string
}

// TLSReportsSent lists the reports this server made for the days from
// first to last, oldest day first. It asks by day and not for the newest
// few: a count would let rows of other days push the asked ones out.
func (s *Store) TLSReportsSent(ctx context.Context, first, last int64) ([]TLSReportSent, error) {
	res, err := s.query(ctx, "", q(`SELECT day, domain, at, outcome FROM tls_report_sent WHERE day >= :first AND day <= :last ORDER BY day, domain`, "first", first, "last", last))
	if err != nil {
		return nil, err
	}
	out := make([]TLSReportSent, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, TLSReportSent{asInt(row[0]), asString(row[1]), time.Unix(asInt(row[2]), 0).UTC(), asString(row[3])})
	}
	return out, nil
}

// TLSReportIn is a report another server sent us.
type TLSReportIn struct {
	ID         int64
	ReceivedAt time.Time
	// Organization and ReportID are the report's own; Domain is the
	// policy domain of its first policy: which of our domains it is about.
	Organization, ReportID, Domain string
	Start, End                     time.Time
	Successful, Failed             int64
	// JSON is the report, unpacked.
	JSON string
}

// SaveTLSReport keeps a report that was sent to us. The same report (by
// organization and report id) arriving twice is kept once; saved reports
// whether it was new.
func (s *Store) SaveTLSReport(ctx context.Context, r TLSReportIn) (saved bool, err error) {
	b := s.newBatch("")
	b.idempotent() // INSERT OR IGNORE by the report's own id
	b.add(`INSERT OR IGNORE INTO tls_report_in (received_at, organization, report_id, domain, start_at, end_at, successful, failed, report)
		VALUES (:now, :org, :rid, :domain, :start, :end, :ok, :failed, :report)`,
		"org", r.Organization, "rid", r.ReportID, "domain", r.Domain, "start", r.Start.Unix(), "end", r.End.Unix(),
		"ok", r.Successful, "failed", r.Failed, "report", r.JSON)
	res, err := s.run(ctx, b)
	if err != nil {
		return false, err
	}
	return res[0].RowsAffected > 0, nil
}

const tlsReportInColumns = `id, received_at, organization, report_id, domain, start_at, end_at, successful, failed`

func scanTLSReportIn(row []any) TLSReportIn {
	return TLSReportIn{ID: asInt(row[0]), ReceivedAt: time.Unix(asInt(row[1]), 0).UTC(), Organization: asString(row[2]),
		ReportID: asString(row[3]), Domain: asString(row[4]), Start: time.Unix(asInt(row[5]), 0).UTC(),
		End: time.Unix(asInt(row[6]), 0).UTC(), Successful: asInt(row[7]), Failed: asInt(row[8])}
}

// TLSReportsReceived lists the newest reports sent to us, without their JSON.
func (s *Store) TLSReportsReceived(ctx context.Context, limit int) ([]TLSReportIn, error) {
	res, err := s.query(ctx, "", q(`SELECT `+tlsReportInColumns+` FROM tls_report_in ORDER BY id DESC LIMIT :limit`, "limit", limit))
	if err != nil {
		return nil, err
	}
	out := make([]TLSReportIn, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, scanTLSReportIn(row))
	}
	return out, nil
}

// TLSReportReceived returns one report sent to us, with its JSON.
func (s *Store) TLSReportReceived(ctx context.Context, id int64) (TLSReportIn, bool, error) {
	res, err := s.query(ctx, "", q(`SELECT `+tlsReportInColumns+`, report FROM tls_report_in WHERE id = :id`, "id", id))
	if err != nil || len(res[0].Values) == 0 {
		return TLSReportIn{}, false, err
	}
	r := scanTLSReportIn(res[0].Values[0])
	r.JSON = asString(res[0].Values[0][9])
	return r, true, nil
}

// TrimTLSReports removes counts and sent-marks of days before results,
// and received reports from before received.
func (s *Store) TrimTLSReports(ctx context.Context, results, received time.Time) error {
	b := s.newBatch("")
	b.idempotent() // deletes by condition
	b.add(`DELETE FROM tls_result WHERE day < :day`, "day", TLSDay(results))
	b.add(`DELETE FROM tls_report_sent WHERE day < :day`, "day", TLSDay(results))
	b.add(`DELETE FROM tls_report_in WHERE received_at < :before`, "before", received.Unix())
	_, err := s.run(ctx, b)
	return err
}
