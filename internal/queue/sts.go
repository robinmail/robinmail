package queue

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robinmail/robinmail/internal/store"
	"github.com/robinmail/robinmail/internal/tlsrpt"
)

// MTA-STS (RFC 8461) lets a domain say that mail for it must go over TLS
// to a server with a valid certificate. The policy is announced by a TXT
// record and fetched over HTTPS; this file finds, fetches, parses and
// caches it. DANE (RFC 7672), the other way to require TLS, is not
// implemented: it needs a resolver that validates DNSSEC.

// STS modes (RFC 8461 5).
const (
	stsEnforce = "enforce"
	stsTesting = "testing"
	stsNone    = "none"
)

const (
	stsMaxPolicyBytes = 64 << 10 // RFC 8461 3.2 suggests this cap
	stsMaxAge         = 31557600 // one year, the most max_age may be
)

// stsRecordID reads the TXT records at _mta-sts.<domain> and returns the
// id of the policy (RFC 8461 3.1). Records that do not start with
// "v=STSv1" are not ours; more than one that does means there is no
// policy.
func stsRecordID(records []string) (string, bool) {
	id, found := "", 0
	for _, rec := range records {
		fields := strings.Split(rec, ";")
		if strings.TrimSpace(fields[0]) != "v=STSv1" {
			continue
		}
		found++
		id = ""
		for _, f := range fields[1:] {
			k, v, ok := strings.Cut(strings.TrimSpace(f), "=")
			if ok && k == "id" {
				id = v
			}
		}
	}
	if found != 1 || id == "" || len(id) > 32 {
		return "", false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return "", false
		}
	}
	return id, true
}

// parseSTSPolicy reads a policy file (RFC 8461 3.2): "key: value" lines
// that end in CRLF or LF. version, mode and max_age are required, and at
// least one mx unless the mode is none. Unknown keys are ignored.
func parseSTSPolicy(body []byte) (mode string, mx []string, maxAge time.Duration, err error) {
	version, age := "", int64(-1)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return "", nil, 0, fmt.Errorf("mta-sts: not a key: value line: %q", line)
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "version":
			version = v
		case "mode":
			mode = v
		case "max_age":
			n, perr := strconv.ParseInt(v, 10, 64)
			if perr != nil || n < 0 {
				return "", nil, 0, errors.New("mta-sts: max_age is not a number")
			}
			age = min(n, stsMaxAge)
		case "mx":
			v = strings.ToLower(v)
			if v == "" || len(mx) >= 100 {
				return "", nil, 0, errors.New("mta-sts: bad mx")
			}
			mx = append(mx, v)
		}
	}
	switch {
	case version != "STSv1":
		return "", nil, 0, errors.New("mta-sts: version is not STSv1")
	case mode != stsEnforce && mode != stsTesting && mode != stsNone:
		return "", nil, 0, errors.New("mta-sts: unknown mode")
	case age < 0:
		return "", nil, 0, errors.New("mta-sts: no max_age")
	case len(mx) == 0 && mode != stsNone:
		return "", nil, 0, errors.New("mta-sts: no mx")
	}
	return mode, mx, time.Duration(age) * time.Second, nil
}

// stsMatch reports whether a host is one the policy names (RFC 8461 4.1).
// A pattern "*.example.com" matches a host with exactly one more label.
func stsMatch(patterns []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSuffix(p, "."))
		if p == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(p, "*"); ok && strings.HasPrefix(suffix, ".") && strings.HasSuffix(host, suffix) {
			if label := host[:len(host)-len(suffix)]; label != "" && !strings.Contains(label, ".") {
				return true
			}
		}
	}
	return false
}

// fetchSTSPolicy gets the policy file of a domain (RFC 8461 3.3): over
// HTTPS from mta-sts.<domain>, with a certificate valid for that name,
// status 200, type text/plain, and no redirect followed.
func (w *Worker) fetchSTSPolicy(ctx context.Context, domain string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://mta-sts."+domain+"/.well-known/mta-sts.txt", nil)
	if err != nil {
		return nil, err
	}
	hc := w.HTTP
	if hc == nil {
		hc = defaultSTSClient
		if w.AllowPrivate {
			hc = privateSTSClient
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mta-sts: HTTP status %d", resp.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "text/plain" {
		return nil, errors.New("mta-sts: the policy is not text/plain")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, stsMaxPolicyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > stsMaxPolicyBytes {
		return nil, errors.New("mta-sts: the policy is too large")
	}
	return body, nil
}

// defaultSTSClient fetches policies. Like the mail itself, the request
// goes to a name the recipient's domain chose, so it connects to public
// addresses only (publicAddress): the check is on the address the
// connection is made to, after the name was looked up, and no proxy is
// used. privateSTSClient is the same without the check, for a Worker
// with AllowPrivate.
var defaultSTSClient, privateSTSClient = stsClient(true), stsClient(false)

// OutboundHTTP is the HTTP client for a request to a place that another
// domain named: its MTA-STS policy host, the https address its TLS
// reports go to. It follows no redirects, uses no proxy, and unless
// allowPrivate is set connects to public addresses only.
func OutboundHTTP(allowPrivate bool) *http.Client {
	if allowPrivate {
		return privateSTSClient
	}
	return defaultSTSClient
}

func stsClient(publicOnly bool) *http.Client {
	d := &net.Dialer{Timeout: 30 * time.Second}
	if publicOnly {
		d.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if ip := net.ParseIP(host); err != nil || !publicAddress(ip) {
				return fmt.Errorf("mta-sts: %s is not a public address", address)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: time.Minute,
		// "HTTP 3xx redirects MUST NOT be followed" (RFC 8461 3.3).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext:           d.DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// policy returns the MTA-STS policy to apply to a domain, or nil for none.
//
// The TXT record is looked up for every delivery; its id says whether the
// cached policy is still the current one. A new id, or no cached policy,
// means a fetch. When the record or the fetch fails, a cached policy that
// has not expired keeps applying (RFC 8461 3.3, 5.1): otherwise an
// attacker who can break DNS or HTTPS for a moment could switch the
// protection off. The cache is a table of the database, so all nodes share it.
func (w *Worker) policy(ctx context.Context, domain string) *store.STSPolicy {
	now := w.now()
	cached, have, err := w.Store.STSPolicy(ctx, domain)
	if err != nil {
		w.log().Warn("mta-sts: reading the cache failed", "domain", domain, "err", err)
		have = false
	}
	fresh := have && now.Before(cached.ExpiresAt)
	use := func() *store.STSPolicy {
		if !fresh || cached.Mode == stsNone {
			return nil
		}
		return &cached
	}
	records, err := w.resolver().LookupTXT(ctx, "_mta-sts."+domain)
	if err != nil && !notFound(err) {
		w.log().Warn("mta-sts: TXT lookup failed", "domain", domain, "err", err)
		return use()
	}
	id, ok := stsRecordID(records)
	if !ok {
		return use()
	}
	if fresh && cached.ID == id {
		return use()
	}
	body, err := w.fetchSTSPolicy(ctx, domain)
	if err != nil {
		w.log().Warn("mta-sts: fetching the policy failed", "domain", domain, "err", err)
		w.tlsResult(ctx, domain, &store.STSPolicy{Domain: domain}, tlsrpt.STSPolicyFetchError, "", "")
		return use()
	}
	mode, mx, maxAge, err := parseSTSPolicy(body)
	if err != nil {
		w.log().Warn("mta-sts: the policy is not valid", "domain", domain, "err", err)
		w.tlsResult(ctx, domain, &store.STSPolicy{Domain: domain}, tlsrpt.STSPolicyInvalid, "", "")
		return use()
	}
	cached = store.STSPolicy{Domain: domain, ID: id, Mode: mode, MX: mx, FetchedAt: now, ExpiresAt: now.Add(maxAge)}
	fresh = maxAge > 0
	if err := w.Store.SaveSTSPolicy(ctx, cached); err != nil {
		w.log().Warn("mta-sts: caching the policy failed", "domain", domain, "err", err)
	}
	w.log().Info("mta-sts: policy fetched", "domain", domain, "mode", mode, "mx", strings.Join(mx, ","), "max_age", maxAge)
	return use()
}

// verifyPeer checks the certificate a server showed against the host name
// we connected to, as MTA-STS wants it (RFC 8461 4.2): a chain to a trusted
// root, not expired, and the host among its DNS names.
func verifyPeer(st *tls.ConnectionState, host string, roots *x509.CertPool, now time.Time) error {
	if st == nil || len(st.PeerCertificates) == 0 {
		return errors.New("the server showed no certificate")
	}
	opts := x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: x509.NewCertPool(), CurrentTime: now}
	for _, c := range st.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := st.PeerCertificates[0].Verify(opts)
	return err
}

// TLSRecorder counts the outcome of a TLS session for the daily report.
type TLSRecorder interface {
	CountTLSResult(ctx context.Context, r store.TLSResult) error
}

// tlsResult counts one session with a mail server of a recipient domain:
// "" for one that worked, or the result-type of RFC 8460 4.3. A success is
// counted per policy, a failure per policy, host and address. Counting
// that fails is logged and nothing more: mail matters more than a report
// about it.
func (w *Worker) tlsResult(ctx context.Context, domain string, policy *store.STSPolicy, result, mx, ip string) {
	if w.TLSResults == nil {
		return
	}
	p := tlsrpt.Policy{Type: tlsrpt.PolicyNone, Domain: domain}
	if policy != nil {
		p.Type = tlsrpt.PolicySTS
		p.MXHost = policy.MX
		if policy.Mode != "" {
			// The policy as it was fetched, line by line (RFC 8460 4.4:
			// "policy-string").
			p.String = []string{"version: STSv1", "mode: " + policy.Mode}
			for _, m := range policy.MX {
				p.String = append(p.String, "mx: "+m)
			}
			if age := policy.ExpiresAt.Sub(policy.FetchedAt); age > 0 {
				p.String = append(p.String, "max_age: "+strconv.FormatInt(int64(age/time.Second), 10))
			}
		}
	}
	text, _ := json.Marshal(p)
	r := store.TLSResult{Domain: domain, Policy: string(text), Result: result}
	if result != "" {
		r.MX, r.IP = mx, ip
	}
	if err := w.TLSResults.CountTLSResult(context.WithoutCancel(ctx), r); err != nil {
		w.log().Warn("tls report: counting a result failed", "domain", domain, "err", err)
	}
}

// tlsFailure names what was wrong with a server's certificate in the
// words of RFC 8460 4.3.1.
func tlsFailure(err error) string {
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var unknown x509.UnknownAuthorityError
	switch {
	case errors.As(err, &host):
		return tlsrpt.CertificateHostMismatch
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return tlsrpt.CertificateExpired
	case errors.As(err, &unknown), errors.As(err, &invalid):
		return tlsrpt.CertificateNotTrusted
	}
	return tlsrpt.ValidationFailure
}
