package tlsrpt

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/message"
)

// The example report of RFC 8460 appendix B.
const rfcExample = `{
  "organization-name": "Company-X",
  "date-range": {
    "start-datetime": "2016-04-01T00:00:00Z",
    "end-datetime": "2016-04-01T23:59:59Z"
  },
  "contact-info": "sts-reporting@company-x.example",
  "report-id": "5065427c-23d3-47ca-b6e0-946ea0e8c4be",
  "policies": [{
    "policy": {
      "policy-type": "sts",
      "policy-string": ["version: STSv1","mode: testing",
            "mx: *.mail.company-y.example","max_age: 86400"],
      "policy-domain": "company-y.example",
      "mx-host": "*.mail.company-y.example"
    },
    "summary": {
      "total-successful-session-count": 5326,
      "total-failure-session-count": 303
    },
    "failure-details": [{
      "result-type": "certificate-expired",
      "sending-mta-ip": "2001:db8:abcd:0012::1",
      "receiving-mx-hostname": "mx1.mail.company-y.example",
      "failed-session-count": 100
    }, {
      "result-type": "starttls-not-supported",
      "sending-mta-ip": "2001:db8:abcd:0013::1",
      "receiving-mx-hostname": "mx2.mail.company-y.example",
      "receiving-ip": "203.0.113.56",
      "failed-session-count": 200,
      "additional-information": "https://reports.company-x.example/report_info ? id = 5065427c-23d3# StarttlsNotSupported "
    }, {
      "result-type": "validation-failure",
      "sending-mta-ip": "198.51.100.62",
      "receiving-ip": "203.0.113.58",
      "receiving-mx-hostname": "mx-backup.mail.company-y.example",
      "failed-session-count": 3,
      "failure-reason-code": "X509_V_ERR_PROXY_PATH_LENGTH_EXCEEDED"
    }]
  }]
}`

// RFC 8460 4.4 and appendix B: the report schema, read from the RFC's own
// example and written back in the same shape.
func TestRFC8460_4_4_Report(t *testing.T) {
	r, err := Parse([]byte(rfcExample))
	if err != nil {
		t.Fatal(err)
	}
	if r.OrganizationName != "Company-X" || r.ContactInfo != "sts-reporting@company-x.example" || r.ReportID != "5065427c-23d3-47ca-b6e0-946ea0e8c4be" {
		t.Errorf("header of the report: %+v", r)
	}
	if !r.DateRange.Start.Equal(time.Date(2016, 4, 1, 0, 0, 0, 0, time.UTC)) || !r.DateRange.End.Equal(time.Date(2016, 4, 1, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("date-range: %+v", r.DateRange)
	}
	p := r.Policies[0]
	if p.Policy.Type != PolicySTS || p.Policy.Domain != "company-y.example" || len(p.Policy.String) != 4 ||
		!reflect.DeepEqual([]string(p.Policy.MXHost), []string{"*.mail.company-y.example"}) {
		t.Errorf("policy: %+v", p.Policy)
	}
	if ok, failed := r.Totals(); ok != 5326 || failed != 303 {
		t.Errorf("totals: %d %d", ok, failed)
	}
	if len(p.FailureDetails) != 3 || p.FailureDetails[0].ResultType != CertificateExpired || p.FailureDetails[1].ResultType != StartTLSNotSupported ||
		p.FailureDetails[1].ReceivingIP != "203.0.113.56" || p.FailureDetails[2].FailureReasonCode != "X509_V_ERR_PROXY_PATH_LENGTH_EXCEEDED" ||
		p.FailureDetails[0].FailedSessionCount+p.FailureDetails[1].FailedSessionCount+p.FailureDetails[2].FailedSessionCount != 303 {
		t.Errorf("failure-details: %+v", p.FailureDetails)
	}

	// What we write has the member names of the schema, and reads back.
	var written map[string]any
	if err := json.Unmarshal(r.JSON(), &written); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"organization-name", "date-range", "contact-info", "report-id", "policies"} {
		if _, ok := written[name]; !ok {
			t.Errorf("no %q in what was written", name)
		}
	}
	policy := written["policies"].([]any)[0].(map[string]any)
	if _, isList := policy["policy"].(map[string]any)["mx-host"].([]any); !isList {
		t.Error("mx-host is not written as a list (RFC 8460 4.4)")
	}
	dr := written["date-range"].(map[string]any)
	if dr["start-datetime"] != "2016-04-01T00:00:00Z" || dr["end-datetime"] != "2016-04-01T23:59:59Z" {
		t.Errorf("date-range written as %v", dr)
	}
	again, err := Parse(r.JSON())
	if err != nil || !reflect.DeepEqual(again, r) {
		t.Errorf("written and read again: %v", err)
	}
	// 5.2: "the report SHOULD be subjected to GZIP compression".
	zipped, err := Parse(Gzip(r.JSON()))
	if err != nil || !reflect.DeepEqual(zipped, r) {
		t.Errorf("through gzip: %v", err)
	}
}

func TestParseRefuses(t *testing.T) {
	bomb := func() []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		w.Write([]byte(`{"report-id":"x","pad":"`))
		w.Write(bytes.Repeat([]byte("a"), MaxReportSize+10))
		w.Write([]byte(`"}`))
		w.Close()
		return b.Bytes()
	}()
	for name, data := range map[string][]byte{
		"empty":             nil,
		"not JSON":          []byte("hello"),
		"a list":            []byte("[]"),
		"no report-id":      []byte(strings.Replace(rfcExample, `"report-id"`, `"other"`, 1)),
		"no policies":       []byte(`{"report-id":"x","date-range":{"start-datetime":"2016-04-01T00:00:00Z","end-datetime":"2016-04-01T23:59:59Z"},"policies":[]}`),
		"no date":           []byte(`{"report-id":"x","policies":[{"policy":{"policy-type":"sts","policy-domain":"d"}}]}`),
		"policy w/o domain": []byte(`{"report-id":"x","date-range":{"start-datetime":"2016-04-01T00:00:00Z","end-datetime":"2016-04-01T23:59:59Z"},"policies":[{"policy":{"policy-type":"sts"}}]}`),
		"broken gzip":       {0x1f, 0x8b, 1, 2, 3},
		"a gzip bomb":       bomb,
	} {
		if _, err := Parse(data); !errors.Is(err, ErrNotReport) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// RFC 8460 3: the TXT record, with the examples of 3.1.1 and 3.1.2.
func TestRFC8460_3_Record(t *testing.T) {
	for _, c := range []struct {
		records []string
		rua     []string
	}{
		// 3.1.1: "_smtp._tls.example.com. IN TXT "v=TLSRPTv1;rua=mailto:reports@example.com""
		{[]string{"v=TLSRPTv1;rua=mailto:reports@example.com"}, []string{"mailto:reports@example.com"}},
		// 3.1.2: "v=TLSRPTv1; rua=https://reporting.example.com/v1/tlsrpt"
		{[]string{"v=TLSRPTv1; rua=https://reporting.example.com/v1/tlsrpt"}, []string{"https://reporting.example.com/v1/tlsrpt"}},
		// "rua: A URI ... a comma-separated list"
		{[]string{"v=TLSRPTv1; rua=mailto:a@example.com, https://b.example/r ;"}, []string{"mailto:a@example.com", "https://b.example/r"}},
		// Other TXT records at the name are not ours; unknown fields are ignored.
		{[]string{"something else", "v=TLSRPTv1; ext=1; rua=mailto:a@example.com; more=x"}, []string{"mailto:a@example.com"}},
		// "the record MUST begin with v=TLSRPTv1": case and place matter.
		{[]string{"rua=mailto:a@example.com; v=TLSRPTv1"}, nil},
		{[]string{"v=tlsrptv1; rua=mailto:a@example.com"}, nil},
		{[]string{"v=TLSRPTv12; rua=mailto:a@example.com"}, nil},
		// "If the number of resulting records is not one, senders MUST
		// assume the recipient domain does not implement TLSRPT."
		{[]string{"v=TLSRPTv1; rua=mailto:a@example.com", "v=TLSRPTv1; rua=mailto:b@example.com"}, nil},
		{nil, nil},
		{[]string{"v=TLSRPTv1"}, nil},
		// Schemes that are not for reports, and URIs that are none.
		{[]string{"v=TLSRPTv1; rua=http://plain.example/r, ftp://x, mailto:nobody, https://"}, nil},
		{[]string{"v=TLSRPTv1; rua=mailto:a b@example.com"}, nil},
	} {
		rua, ok := ParseRecord(c.records)
		if !reflect.DeepEqual(rua, c.rua) || ok != (len(c.rua) > 0) {
			t.Errorf("ParseRecord(%q) = %q, %v; want %q", c.records, rua, ok, c.rua)
		}
	}
}

// RFC 8460 5.1 and 5.3: the report as mail. The example there:
//
//	TLS-Report-Domain: example.net
//	TLS-Report-Submitter: mail.sender.example.com
//	Subject: Report Domain: example.net
//	    Submitter: mail.sender.example.com
//	    Report-ID: <735ff.e317+bf22029@example.net>
//	Content-Type: multipart/report; report-type="tlsrpt"; boundary=...
//	  ...
//	Content-Type: application/tlsrpt+gzip
//	Content-Disposition: attachment;
//	    filename="mail.sender.example!example.com!1013662812!1013749130.json.gz"
func TestRFC8460_5_Mail(t *testing.T) {
	r, _ := Parse([]byte(rfcExample))
	r.ReportID = "735ff.e317+bf22029@example.net"
	r.DateRange = DateRange{Start: time.Unix(1013662812, 0).UTC(), End: time.Unix(1013749130, 0).UTC()}
	name := "TLS reports"
	raw := BuildMail(Mail{From: message.Address{Name: &name, Email: "postmaster@sender.example.com"}, To: "tlsrpt@example.net",
		Submitter: "mail.sender.example.com", Domain: "example.net", MessageID: "id@mail.sender.example.com",
		Date: time.Date(2016, 4, 2, 0, 5, 0, 0, time.UTC)}, r)
	m := message.Parse(raw)
	get := func(h string) string { v, _ := message.Get(m.Headers, h); return strings.TrimSpace(message.Text(v)) }
	for h, want := range map[string]string{
		"TLS-Report-Domain":    "example.net",
		"TLS-Report-Submitter": "mail.sender.example.com",
		"Subject":              "Report Domain: example.net Submitter: mail.sender.example.com Report-ID: <735ff.e317+bf22029@example.net>",
		"To":                   "tlsrpt@example.net",
	} {
		if got := get(h); got != want {
			t.Errorf("%s: %q, want %q", h, got, want)
		}
	}
	ct, _ := message.Get(m.Headers, "Content-Type")
	typ, params := message.Params(ct)
	if typ != "multipart/report" || params["report-type"] != "tlsrpt" {
		t.Errorf("Content-Type: %q", ct)
	}
	if len(m.Leaves) != 2 || m.Leaves[0].Type != "text/plain" || m.Leaves[1].Type != "application/tlsrpt+gzip" {
		t.Fatalf("parts: %+v", m.Leaves)
	}
	// 5.1: the file name. Ours carries the report id as its last part,
	// which the RFC allows ("unique-id"); only what is safe in a name.
	att := m.Leaves[1]
	if att.Name == nil || *att.Name != "mail.sender.example.com!example.net!1013662812!1013749130!735ff.e317bf22029example.net.json.gz" ||
		att.Disposition == nil || *att.Disposition != "attachment" {
		t.Errorf("attachment: %v %v", att.Name, att.Disposition)
	}
	if !bytes.Equal(att.Content(), Gzip(r.JSON())) {
		t.Error("the attachment is not the gzip of the report")
	}
	// The message has no line over 78 octets in its header and is 7-bit:
	// it goes through any server.
	for _, c := range raw {
		if c >= 0x80 {
			t.Fatal("8-bit octets in the report mail")
		}
	}

	// And read by the receiving side.
	back, err := FromMessage(m)
	if err != nil || back.ReportID != r.ReportID {
		t.Fatalf("FromMessage: %v", err)
	}
	// A message that only has the header field is one too, with the JSON
	// not compressed.
	plain := "TLS-Report-Domain: example.net\r\nContent-Type: application/tlsrpt+json\r\n\r\n" + rfcExample
	if back, err := FromMessage(message.Parse([]byte(plain))); err != nil || back.OrganizationName != "Company-X" {
		t.Errorf("a bare JSON report: %v", err)
	}
	// Ordinary mail, and mail that attaches a report without saying it is one.
	for _, raw := range []string{
		"Subject: hello\r\n\r\nhi\r\n",
		"Content-Type: application/tlsrpt+json\r\n\r\n" + rfcExample,
		"TLS-Report-Domain: example.net\r\n\r\nno report in here\r\n",
		"Content-Type: multipart/report; report-type=delivery-status; boundary=b\r\n\r\n--b\r\nContent-Type: application/tlsrpt+json\r\n\r\n" + rfcExample + "\r\n--b--\r\n",
	} {
		if _, err := FromMessage(message.Parse([]byte(raw))); !errors.Is(err, ErrNotReport) {
			t.Errorf("%.40q: %v", raw, err)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(rfcExample))
	f.Add(Gzip([]byte(rfcExample)))
	f.Add([]byte{0x1f, 0x8b})
	f.Add([]byte(`{"policies":[{"policy":{"mx-host":[1]}}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := Parse(data)
		if err != nil {
			if !errors.Is(err, ErrNotReport) {
				t.Fatalf("error %v is not ErrNotReport", err)
			}
			return
		}
		// What parses can be written and parses again.
		if _, err := Parse(r.JSON()); err != nil {
			t.Fatalf("a parsed report does not parse again: %v", err)
		}
		ParseRecord([]string{string(data)})
	})
}
