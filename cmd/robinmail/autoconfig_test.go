package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// freePort returns a port nothing listens on at the moment.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// TestAutoconfigServed is phase 8's client autoconfig on a running
// server: Thunderbird's file under autoconfig.<domain> and under the
// hostname, with the ports of the listeners in the config, and Outlook's
// Autodiscover. The formats are tested in internal/web.
func TestAutoconfigServed(t *testing.T) {
	const host = "mail.example.test"
	certFile, keyFile, _ := selfSigned(t, t.TempDir(), host)
	imaps, submission := freePort(t), freePort(t)
	c := newCLI(t, fmt.Sprintf(`"smtp":{"listen":"127.0.0.1:0"},"web":{"listen":"127.0.0.1:0"},"tls":{"cert_file":%q,"key_file":%q},
		"imap":{"listen_tls":"127.0.0.1:%s"},"submission":{"listen":"127.0.0.1:%s"},"tls_rpt":{"disabled":true},`,
		certFile, keyFile, imaps, submission))
	domain := "auto-" + randomHex() + ".test"
	c.ok("", "key", "generate")
	c.ok("", "domain", "add", domain)
	old := ownDomainsFor
	ownDomainsFor = 0
	t.Cleanup(func() { ownDomainsFor = old })
	srv := startServe(t, c)

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // the certificate is for the hostname only
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, srv.web)
		},
	}}
	get := func(url string) (int, string) {
		t.Helper()
		resp, err := hc.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	want := []string{
		`<emailProvider id="` + domain + `">`, `<domain>` + domain + `</domain>`,
		"<incomingServer type=\"imap\">\n      <hostname>mail.example.test</hostname>\n      <port>" + imaps + "</port>\n      <socketType>SSL</socketType>",
		"<outgoingServer type=\"smtp\">\n      <hostname>mail.example.test</hostname>\n      <port>" + submission + "</port>\n      <socketType>STARTTLS</socketType>",
		"<username>%EMAILADDRESS%</username>",
	}
	for _, url := range []string{
		// Thunderbird asks the first with the address, others without.
		"https://autoconfig." + domain + "/mail/config-v1.1.xml?emailaddress=anna@" + domain,
		"https://autoconfig." + domain + "/mail/config-v1.1.xml",
		"https://" + host + "/mail/config-v1.1.xml?emailaddress=anna@" + domain,
		"https://" + host + "/.well-known/autoconfig/mail/config-v1.1.xml?emailaddress=anna@" + domain,
	} {
		status, body := get(url)
		if status != 200 {
			t.Errorf("%s: %d", url, status)
			continue
		}
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s lacks %q:\n%s", url, w, body)
			}
		}
	}
	status, body := get("https://autoconfig." + domain + "/mail/config-v1.1.xml")
	t.Logf("GET https://autoconfig.%s/mail/config-v1.1.xml: %d\n%s", domain, status, body)

	// Nothing else under the name; no such name for a domain that is not
	// ours; nothing under the hostname for a foreign address.
	for _, path := range []string{"/", "/auth/session", "/.well-known/jmap", "/autodiscover/autodiscover.xml"} {
		if status, _ := get("https://autoconfig." + domain + path); status != 404 {
			t.Errorf("autoconfig.%s%s: %d, want 404", domain, path, status)
		}
	}
	if status, _ := get("https://autoconfig.elsewhere-" + domain + "/mail/config-v1.1.xml"); status != http.StatusMisdirectedRequest {
		t.Errorf("the autoconfig host of a foreign domain: %d", status)
	}
	if status, _ := get("https://" + host + "/mail/config-v1.1.xml?emailaddress=anna@elsewhere.test"); status != 404 {
		t.Errorf("a foreign address under the hostname: %d", status)
	}

	// Outlook's Autodiscover under the hostname.
	req := `<Autodiscover xmlns="http://schemas.microsoft.com/exchange/autodiscover/outlook/requestschema/2006"><Request>` +
		`<EMailAddress>anna@` + domain + `</EMailAddress>` +
		`<AcceptableResponseSchema>http://schemas.microsoft.com/exchange/autodiscover/outlook/responseschema/2006a</AcceptableResponseSchema></Request></Autodiscover>`
	resp, err := hc.Post("https://"+host+"/Autodiscover/Autodiscover.xml", "text/xml", strings.NewReader(req))
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, w := range []string{"<Type>IMAP</Type>", "<Port>" + imaps + "</Port>", "<Type>SMTP</Type>", "<Port>" + submission + "</Port>",
		"<Encryption>TLS</Encryption>", "<LoginName>anna@" + domain + "</LoginName>"} {
		if resp.StatusCode != 200 || !strings.Contains(string(answer), w) {
			t.Errorf("autodiscover: %d, lacks %q:\n%s", resp.StatusCode, w, answer)
		}
	}
}
