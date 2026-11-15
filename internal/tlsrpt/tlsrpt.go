// Package tlsrpt is SMTP TLS Reporting (RFC 8460): the report a sending
// server makes once a day for each domain it sent to, about how TLS went;
// the DNS record that says where a domain wants its reports; and the
// message a report travels in. It builds reports for sending and reads
// reports that were sent to us. Both are JSON of the same shape.
package tlsrpt

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/message"
)

// Policy types (RFC 8460 4.3.2, "policy-type").
const (
	PolicySTS  = "sts"
	PolicyTLSA = "tlsa"
	PolicyNone = "no-policy-found"
)

// Result types this server reports (RFC 8460 4.3): a failure to negotiate
// TLS, a certificate that does not fit, and what can go wrong with an
// MTA-STS policy. The empty string is a session that worked.
const (
	StartTLSNotSupported    = "starttls-not-supported"
	CertificateHostMismatch = "certificate-host-mismatch"
	CertificateExpired      = "certificate-expired"
	CertificateNotTrusted   = "certificate-not-trusted"
	ValidationFailure       = "validation-failure"
	STSPolicyFetchError     = "sts-policy-fetch-error"
	STSPolicyInvalid        = "sts-policy-invalid"
	STSWebPKIInvalid        = "sts-webpki-invalid"
)

// Report is the JSON report of RFC 8460 4.4.
type Report struct {
	OrganizationName string         `json:"organization-name"`
	DateRange        DateRange      `json:"date-range"`
	ContactInfo      string         `json:"contact-info"`
	ReportID         string         `json:"report-id"`
	Policies         []PolicyResult `json:"policies"`
}

// DateRange is the time the report covers, in UTC.
type DateRange struct {
	Start time.Time `json:"start-datetime"`
	End   time.Time `json:"end-datetime"`
}

// PolicyResult is what happened under one policy.
type PolicyResult struct {
	Policy         Policy          `json:"policy"`
	Summary        Summary         `json:"summary"`
	FailureDetails []FailureDetail `json:"failure-details,omitempty"`
}

// Policy is the policy that was applied: for MTA-STS its lines, the
// domain it is of and the MX patterns it names.
type Policy struct {
	Type   string   `json:"policy-type"`
	String []string `json:"policy-string,omitempty"`
	Domain string   `json:"policy-domain"`
	MXHost Hosts    `json:"mx-host,omitempty"`
}

// Hosts is the "mx-host" of a policy: a list of patterns. RFC 8460 4.4
// defines a list, and the example in its appendix B writes one string;
// both are read, a list is written.
type Hosts []string

func (h *Hosts) UnmarshalJSON(data []byte) error {
	var one string
	if json.Unmarshal(data, &one) == nil {
		*h = Hosts{one}
		return nil
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	*h = list
	return nil
}

// Summary counts the sessions.
type Summary struct {
	Successful int64 `json:"total-successful-session-count"`
	Failed     int64 `json:"total-failure-session-count"`
}

// FailureDetail is one kind of failure and how often it happened.
type FailureDetail struct {
	ResultType          string `json:"result-type"`
	SendingMTAIP        string `json:"sending-mta-ip,omitempty"`
	ReceivingMXHostname string `json:"receiving-mx-hostname,omitempty"`
	ReceivingMXHelo     string `json:"receiving-mx-helo,omitempty"`
	ReceivingIP         string `json:"receiving-ip,omitempty"`
	FailedSessionCount  int64  `json:"failed-session-count"`
	AdditionalInfo      string `json:"additional-information,omitempty"`
	FailureReasonCode   string `json:"failure-reason-code,omitempty"`
}

// MaxReportSize is the most JSON a report sent to us may be once it is
// unpacked: a gzip stream can be far larger inside than outside.
const MaxReportSize = 4 << 20

// ErrNotReport is returned for what is not a TLS report.
var ErrNotReport = errors.New("tlsrpt: not a TLS report")

// JSON writes the report.
func (r *Report) JSON() []byte {
	b, _ := json.Marshal(r)
	return b
}

// Gzip packs data as RFC 8460 3 and 5.2 prefer it sent.
func Gzip(data []byte) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write(data)
	w.Close()
	return b.Bytes()
}

// Parse reads a report: JSON, or gzip of JSON. It checks that the fields
// a report must have are there, and nothing about whether it is true.
func Parse(data []byte) (*Report, error) {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotReport, err)
		}
		plain, err := io.ReadAll(io.LimitReader(zr, MaxReportSize+1))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotReport, err)
		}
		data = plain
	}
	if len(data) > MaxReportSize {
		return nil, fmt.Errorf("%w: larger than %d octets", ErrNotReport, MaxReportSize)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotReport, err)
	}
	if r.ReportID == "" || r.DateRange.Start.IsZero() || r.DateRange.End.IsZero() || len(r.Policies) == 0 || len(r.Policies) > 1000 {
		return nil, fmt.Errorf("%w: report-id, date-range and policies are required", ErrNotReport)
	}
	for _, p := range r.Policies {
		if p.Policy.Type == "" || p.Policy.Domain == "" || p.Summary.Successful < 0 || p.Summary.Failed < 0 {
			return nil, fmt.Errorf("%w: a policy needs its type and domain", ErrNotReport)
		}
	}
	return &r, nil
}

// Totals adds up the sessions of a report.
func (r *Report) Totals() (successful, failed int64) {
	for _, p := range r.Policies {
		successful += p.Summary.Successful
		failed += p.Summary.Failed
	}
	return successful, failed
}

// ParseRecord reads the TXT records at _smtp._tls.<domain> (RFC 8460 3)
// and returns where reports go: "mailto:" and "https:" URIs. ok is false
// when the domain publishes no usable record. Exactly one record may
// start with "v=TLSRPTv1": with several, there is no policy.
func ParseRecord(records []string) (rua []string, ok bool) {
	var found []string
	for _, rec := range records {
		rec = strings.TrimSpace(rec)
		// "v=TLSRPTv1" is the first field, and case-sensitive.
		if rest, isV := strings.CutPrefix(rec, "v=TLSRPTv1"); isV {
			rest = strings.TrimLeft(rest, " \t")
			if rest == "" || rest[0] == ';' {
				found = append(found, rest)
			}
		}
	}
	if len(found) != 1 {
		return nil, false
	}
	for _, field := range strings.Split(found[0], ";") {
		name, value, has := strings.Cut(strings.TrimSpace(field), "=")
		if !has || strings.TrimSpace(name) != "rua" {
			continue // unknown fields are ignored
		}
		for _, uri := range strings.Split(value, ",") {
			uri = strings.TrimSpace(uri)
			// In a URI "," and "!" and ";" are written as %2C, %21, %3B.
			if strings.ContainsAny(uri, " \t!;") || len(uri) > 2000 {
				continue
			}
			switch {
			case strings.HasPrefix(strings.ToLower(uri), "mailto:") && strings.Contains(uri[7:], "@"):
				rua = append(rua, uri)
			case strings.HasPrefix(strings.ToLower(uri), "https://") && len(uri) > 8:
				rua = append(rua, uri)
			}
		}
		break // the first rua counts
	}
	return rua, len(rua) > 0
}

// FileName is the name of the report as an attachment (RFC 8460 5.1):
// the sender, the policy domain, the start and the end in seconds, and
// an id, joined by "!".
func FileName(submitter, domain string, r *Report) string {
	unique := strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' {
			return c
		}
		return -1
	}, r.ReportID)
	return fmt.Sprintf("%s!%s!%d!%d!%s.json.gz", submitter, domain, r.DateRange.Start.Unix(), r.DateRange.End.Unix(), unique)
}

// Mail is what a report mail needs besides the report.
type Mail struct {
	From      message.Address
	To        string
	Submitter string // our host name
	Domain    string // the domain the report is about
	MessageID string
	Date      time.Time
}

// BuildMail writes the message a report is sent in (RFC 8460 5.3): a
// multipart/report with report-type tlsrpt, a text for people and the
// report as application/tlsrpt+gzip, with the header fields
// TLS-Report-Domain and TLS-Report-Submitter and the subject of 5.3.
func BuildMail(m Mail, r *Report) []byte {
	text := fmt.Sprintf("This is an aggregate TLS report from %s for mail sent to %s between %s and %s.\r\n\r\n"+
		"It is a machine-readable report as defined in RFC 8460; the attachment is gzip-compressed JSON.\r\n",
		m.Submitter, m.Domain, r.DateRange.Start.UTC().Format(time.RFC3339), r.DateRange.End.UTC().Format(time.RFC3339))
	headers := []message.Header{
		{Name: "From", Value: message.FormatAddresses("From", []message.Address{m.From})},
		{Name: "To", Value: message.FormatAddresses("To", []message.Address{{Email: m.To}})},
		{Name: "Subject", Value: message.FormatText("Subject", fmt.Sprintf("Report Domain: %s Submitter: %s Report-ID: <%s>", m.Domain, m.Submitter, r.ReportID))},
		{Name: "Date", Value: message.FormatDate(m.Date)},
		{Name: "Message-ID", Value: message.FormatMessageIDs("Message-ID", []string{m.MessageID})},
		message.Field("TLS-Report-Domain", m.Domain),
		message.Field("TLS-Report-Submitter", m.Submitter),
		message.Field("Auto-Submitted", "auto-generated"),
	}
	return message.Build(headers, &message.BuildPart{
		Type:   "multipart/report",
		Params: map[string]string{"report-type": "tlsrpt"},
		SubParts: []*message.BuildPart{
			{Type: "text/plain", Body: []byte(text)},
			{Type: "application/tlsrpt+gzip", Name: FileName(m.Submitter, m.Domain, r), Disposition: "attachment", Body: Gzip(r.JSON()), Exact: true},
		},
	})
}

// FromMessage finds the report in a message that was sent to us: a part
// of the type application/tlsrpt+gzip or application/tlsrpt+json, in a
// message that says it is a TLS report (a multipart/report with
// report-type tlsrpt, or the TLS-Report-Domain field). ErrNotReport for
// any other message.
func FromMessage(m *message.Message) (*Report, error) {
	_, marked := message.Get(m.Headers, "TLS-Report-Domain")
	if ct, ok := message.Get(m.Headers, "Content-Type"); ok {
		typ, params := message.Params(ct)
		marked = marked || (typ == "multipart/report" && strings.EqualFold(params["report-type"], "tlsrpt"))
	}
	if !marked {
		return nil, ErrNotReport
	}
	for _, leaf := range m.Leaves {
		if leaf.Type == "application/tlsrpt+gzip" || leaf.Type == "application/tlsrpt+json" {
			return Parse(leaf.Content())
		}
	}
	return nil, ErrNotReport
}
