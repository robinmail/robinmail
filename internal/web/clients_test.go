package web

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The formats here have no RFC. The tests follow what their makers
// publish: Mozilla's "Autoconfig: file format definition" (config-v1.1.xml),
// Microsoft's "POX Autodiscover" request and response for an IMAP/SMTP
// account ([MS-OXDSCLI]), and Apple's "Mail" payload of a configuration
// profile (com.apple.mail.managed).

func testClients() *MailClients {
	return &MailClients{Host: "mail.example.test", IMAPS: 993, IMAP: 143, Submissions: 465, Submission: 587,
		Domain: func(_ context.Context, d string) bool { return d == "example.test" || d == "other.test" }}
}

// clientConfig is config-v1.1.xml as a client reads it.
type clientConfig struct {
	XMLName  xml.Name `xml:"clientConfig"`
	Version  string   `xml:"version,attr"`
	Provider struct {
		ID       string       `xml:"id,attr"`
		Domains  []string     `xml:"domain"`
		Name     string       `xml:"displayName"`
		Incoming []autoServer `xml:"incomingServer"`
		Outgoing []autoServer `xml:"outgoingServer"`
	} `xml:"emailProvider"`
}

type autoServer struct {
	Type     string `xml:"type,attr"`
	Hostname string `xml:"hostname"`
	Port     int    `xml:"port"`
	Socket   string `xml:"socketType"`
	Auth     string `xml:"authentication"`
	Username string `xml:"username"`
}

func parseAutoconfig(t *testing.T, body []byte) clientConfig {
	t.Helper()
	var c clientConfig
	if err := xml.Unmarshal(body, &c); err != nil {
		t.Fatalf("the autoconfig is not XML: %v\n%s", err, body)
	}
	return c
}

// Mozilla's format: <clientConfig version="1.1">, one <emailProvider> with
// its <domain>, incomingServer type "imap" and outgoingServer type "smtp",
// each with hostname, port, socketType ("SSL" for TLS from the start,
// "STARTTLS"), authentication ("password-cleartext": the password itself,
// inside TLS) and username with the placeholder %EMAILADDRESS%. Several
// servers are in order of preference.
func TestAutoconfigFormat(t *testing.T) {
	c := parseAutoconfig(t, testClients().Autoconfig("example.test"))
	if c.Version != "1.1" || c.Provider.ID != "example.test" || len(c.Provider.Domains) != 1 || c.Provider.Domains[0] != "example.test" || c.Provider.Name == "" {
		t.Errorf("the provider: %+v", c)
	}
	want := func(got []autoServer, kind string, ports ...int) {
		t.Helper()
		if len(got) != len(ports) {
			t.Fatalf("%s: %d servers, want %d", kind, len(got), len(ports))
		}
		for i, s := range got {
			socket := "STARTTLS"
			if i == 0 {
				socket = "SSL"
			}
			if s != (autoServer{kind, "mail.example.test", ports[i], socket, "password-cleartext", "%EMAILADDRESS%"}) {
				t.Errorf("%s server %d: %+v", kind, i, s)
			}
		}
	}
	want(c.Provider.Incoming, "imap", 993, 143)
	want(c.Provider.Outgoing, "smtp", 465, 587)

	// Only what is there is offered.
	only := &MailClients{Host: "mail.example.test", IMAPS: 993, Submission: 587}
	c = parseAutoconfig(t, only.Autoconfig("example.test"))
	if len(c.Provider.Incoming) != 1 || c.Provider.Incoming[0].Socket != "SSL" || len(c.Provider.Outgoing) != 1 || c.Provider.Outgoing[0].Socket != "STARTTLS" || c.Provider.Outgoing[0].Port != 587 {
		t.Errorf("with two listeners: %+v", c.Provider)
	}
	// Names are written as text, whatever they hold.
	odd := &MailClients{Host: "mail.example.test", IMAPS: 993, Submissions: 465}
	if c := parseAutoconfig(t, odd.Autoconfig(`a<b>&"c".test`)); c.Provider.ID != `a<b>&"c".test` {
		t.Errorf("an odd domain: %q", c.Provider.ID)
	}
}

// Where Thunderbird looks: autoconfig.<domain>/mail/config-v1.1.xml?emailaddress=…
// and <domain>/.well-known/autoconfig/mail/config-v1.1.xml. The first is
// served through Hosts (cmd/robinmail); here the paths under the hostname.
func TestAutoconfigServed(t *testing.T) {
	st := newSite(t)
	for _, path := range []string{"/mail/config-v1.1.xml", "/.well-known/autoconfig/mail/config-v1.1.xml"} {
		if rec := st.do("GET", tlsBase+path+"?emailaddress=anna@example.test", ""); rec.Code != 404 {
			t.Errorf("%s without settings: %d", path, rec.Code)
		}
	}
	st.s.Clients = testClients()
	for _, path := range []string{"/mail/config-v1.1.xml", "/.well-known/autoconfig/mail/config-v1.1.xml"} {
		// No login, no Origin: a mail program is not a browser.
		rec := st.do("GET", tlsBase+path+"?emailaddress=nobody@other.test", "")
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/xml") {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
		// The same for an address that exists and one that does not:
		// the answer is about the domain.
		if c := parseAutoconfig(t, rec.Body.Bytes()); c.Provider.ID != "other.test" || c.Provider.Incoming[0].Hostname != "mail.example.test" {
			t.Errorf("%s: %+v", path, c.Provider)
		}
		for _, q := range []string{"", "?emailaddress=", "?emailaddress=anna", "?emailaddress=anna@elsewhere.test"} {
			if rec := st.do("GET", tlsBase+path+q, ""); rec.Code != 404 {
				t.Errorf("%s%s: %d, want 404", path, q, rec.Code)
			}
		}
		if rec := st.do("POST", tlsBase+path+"?emailaddress=anna@example.test", "{}"); rec.Code != 405 {
			t.Errorf("POST %s: %d", path, rec.Code)
		}
	}
	// The host of a domain serves the file for that domain and nothing else.
	h := st.s.Clients.AutoconfigHost("other.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "https://autoconfig.other.test/mail/config-v1.1.xml", nil))
	if c := parseAutoconfig(t, rec.Body.Bytes()); rec.Code != 200 || c.Provider.ID != "other.test" {
		t.Errorf("the autoconfig host: %d %+v", rec.Code, c.Provider)
	}
	for _, path := range []string{"/", "/auth/session", "/mail/config-v1.1.xml/x", "/.well-known/jmap"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "https://autoconfig.other.test"+path, nil))
		if rec.Code != 404 {
			t.Errorf("autoconfig host %s: %d", path, rec.Code)
		}
	}
}

// The request of [MS-OXDSCLI] 4.1 (an Outlook client asking for the
// settings of an address), as Microsoft's documentation shows it.
const autodiscoverRequest = `<?xml version="1.0" encoding="utf-8"?>
<Autodiscover xmlns="http://schemas.microsoft.com/exchange/autodiscover/outlook/requestschema/2006">
  <Request>
    <EMailAddress>anna@example.test</EMailAddress>
    <AcceptableResponseSchema>http://schemas.microsoft.com/exchange/autodiscover/outlook/responseschema/2006a</AcceptableResponseSchema>
  </Request>
</Autodiscover>`

type autodiscoverResponse struct {
	XMLName  xml.Name `xml:"http://schemas.microsoft.com/exchange/autodiscover/responseschema/2006 Autodiscover"`
	Response struct {
		XMLName xml.Name
		Account struct {
			Type      string `xml:"AccountType"`
			Action    string `xml:"Action"`
			Protocols []struct {
				Type       string `xml:"Type"`
				Server     string `xml:"Server"`
				Port       int    `xml:"Port"`
				LoginName  string `xml:"LoginName"`
				SPA        string `xml:"SPA"`
				SSL        string `xml:"SSL"`
				Encryption string `xml:"Encryption"`
				Auth       string `xml:"AuthRequired"`
			} `xml:"Protocol"`
		} `xml:"Account"`
		Error struct {
			Code    int    `xml:"ErrorCode"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	} `xml:"Response"`
}

// The answer for an account that is not Exchange: AccountType "email",
// Action "settings", a Protocol of Type IMAP and one of Type SMTP with
// Server, Port, LoginName, SPA off (no secure password authentication),
// SSL on and AuthRequired on; the Response element in the Outlook response
// namespace the request named.
func TestAutodiscover(t *testing.T) {
	st := newSite(t)
	post := func(path, body string) *httptest.ResponseRecorder {
		return st.do("POST", tlsBase+path, body, header("Content-Type", "text/xml; charset=utf-8"))
	}
	if rec := post("/autodiscover/autodiscover.xml", autodiscoverRequest); rec.Code != 404 {
		t.Errorf("without settings: %d", rec.Code)
	}
	st.s.Clients = testClients()
	// Outlook asks /autodiscover/autodiscover.xml and /Autodiscover/Autodiscover.xml.
	for _, path := range []string{"/autodiscover/autodiscover.xml", "/Autodiscover/Autodiscover.xml", "/AutoDiscover/AutoDiscover.xml"} {
		rec := post(path, autodiscoverRequest)
		var res autodiscoverResponse
		if err := xml.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/xml") {
			t.Fatalf("%s: %d %v\n%s", path, rec.Code, err, rec.Body)
		}
		a := res.Response.Account
		if res.Response.XMLName.Space != autodiscoverOutlookNS || a.Type != "email" || a.Action != "settings" || len(a.Protocols) != 2 {
			t.Fatalf("%s: %+v", path, res.Response)
		}
		for i, want := range []struct {
			kind string
			port int
		}{{"IMAP", 993}, {"SMTP", 465}} {
			p := a.Protocols[i]
			if p.Type != want.kind || p.Server != "mail.example.test" || p.Port != want.port || p.LoginName != "anna@example.test" ||
				p.SPA != "off" || p.SSL != "on" || p.Encryption != "SSL" || p.Auth != "on" {
				t.Errorf("%s protocol %d: %+v", path, i, p)
			}
		}
	}
	// STARTTLS is "TLS" in Outlook's words.
	st.s.Clients = &MailClients{Host: "mail.example.test", IMAP: 143, Submission: 587, Domain: testClients().Domain}
	var res autodiscoverResponse
	xml.Unmarshal(post("/autodiscover/autodiscover.xml", autodiscoverRequest).Body.Bytes(), &res)
	if p := res.Response.Account.Protocols; len(p) != 2 || p[0].Port != 143 || p[0].Encryption != "TLS" || p[1].Port != 587 || p[1].Encryption != "TLS" {
		t.Errorf("with STARTTLS only: %+v", p)
	}
	st.s.Clients = testClients()

	// What cannot be answered gets the protocol's error (ErrorCode 600),
	// the same for a foreign domain as for nonsense.
	for name, body := range map[string]string{
		"a foreign domain":  strings.Replace(autodiscoverRequest, "anna@example.test", "anna@elsewhere.test", 1),
		"no address":        strings.Replace(autodiscoverRequest, "anna@example.test", "", 1),
		"the mobile schema": strings.Replace(autodiscoverRequest, "outlook/responseschema/2006a", "mobilesync/responseschema/2006", 1),
		"not XML":           "hello",
		"empty":             "",
	} {
		rec := post("/autodiscover/autodiscover.xml", body)
		var res autodiscoverResponse
		if err := xml.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.Response.Error.Code != 600 || len(res.Response.Account.Protocols) != 0 {
			t.Errorf("%s: %d %v %s", name, rec.Code, err, rec.Body)
		}
	}
	if rec := st.do("GET", tlsBase+"/autodiscover/autodiscover.xml", ""); rec.Code != 405 {
		t.Errorf("GET: %d", rec.Code)
	}
	if rec := post("/autodiscover/autodiscover.xml", autodiscoverRequest+strings.Repeat(" ", 20<<10)); rec.Code != 413 {
		t.Errorf("a large request: %d", rec.Code)
	}
	// An address is written into the answer as text.
	odd := strings.Replace(autodiscoverRequest, "anna@example.test", `"a&lt;b"@example.test`, 1)
	if rec := post("/autodiscover/autodiscover.xml", odd); xml.Unmarshal(rec.Body.Bytes(), &res) != nil {
		t.Errorf("an odd address makes bad XML: %s", rec.Body)
	}
}

func FuzzAutodiscoverAddress(f *testing.F) {
	f.Add([]byte(autodiscoverRequest))
	f.Add([]byte("<Autodiscover><Request><EMailAddress>a@b</EMailAddress></Request></Autodiscover>"))
	f.Add([]byte("<Autodiscover><Request>"))
	f.Add([]byte(`<!DOCTYPE x [<!ENTITY a "aaaa">]><Autodiscover><Request><EMailAddress>&a;</EMailAddress></Request></Autodiscover>`))
	c := testClients()
	f.Fuzz(func(t *testing.T, body []byte) {
		addr, ok := AutodiscoverAddress(body)
		if !ok {
			return
		}
		if addr == "" || addr != strings.TrimSpace(addr) {
			t.Fatalf("address %q", addr)
		}
		// Whatever the address, the answer is XML that says it back.
		var res autodiscoverResponse
		if err := xml.Unmarshal(c.Autodiscover(addr), &res); err != nil {
			t.Fatalf("the answer for %q is not XML: %v", addr, err)
		}
		if got := res.Response.Account.Protocols[0].LoginName; got != addr && strings.ToValidUTF8(addr, "�") == addr && !strings.ContainsAny(addr, "\r") && validXMLChars(addr) {
			t.Fatalf("login name %q for %q", got, addr)
		}
	})
}

func validXMLChars(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\t' && r != '\n' || r == 0xFFFE || r == 0xFFFF {
			return false
		}
	}
	return true
}

// plist reads a property list of dicts, arrays, strings, integers and
// booleans into Go values.
func plist(t *testing.T, body []byte) map[string]any {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(string(body)))
	var value func(start xml.StartElement) any
	next := func() (xml.StartElement, bool) {
		for {
			tok, err := d.Token()
			if err == io.EOF {
				return xml.StartElement{}, false
			}
			if err != nil {
				t.Fatalf("the profile is not XML: %v", err)
			}
			switch e := tok.(type) {
			case xml.StartElement:
				return e, true
			case xml.EndElement:
				return xml.StartElement{}, false
			}
		}
	}
	text := func() string {
		var s string
		for {
			tok, err := d.Token()
			if err != nil {
				t.Fatal(err)
			}
			switch e := tok.(type) {
			case xml.CharData:
				s += string(e)
			case xml.EndElement:
				return s
			}
		}
	}
	value = func(start xml.StartElement) any {
		switch start.Name.Local {
		case "dict":
			m := map[string]any{}
			for {
				k, ok := next()
				if !ok {
					return m
				}
				if k.Name.Local != "key" {
					t.Fatalf("<%s> where a key belongs", k.Name.Local)
				}
				key := text()
				v, ok := next()
				if !ok {
					t.Fatalf("key %q has no value", key)
				}
				m[key] = value(v)
			}
		case "array":
			var list []any
			for {
				v, ok := next()
				if !ok {
					return list
				}
				list = append(list, value(v))
			}
		case "string":
			return text()
		case "integer":
			var n json.Number = json.Number(text())
			i, err := n.Int64()
			if err != nil {
				t.Fatalf("integer %q", n)
			}
			return i
		case "true", "false":
			text()
			return start.Name.Local == "true"
		}
		t.Fatalf("unknown element <%s>", start.Name.Local)
		return nil
	}
	root, ok := next()
	if !ok || root.Name.Local != "plist" {
		t.Fatal("no <plist>")
	}
	top, ok := next()
	if !ok {
		t.Fatal("an empty plist")
	}
	m, _ := value(top).(map[string]any)
	return m
}

// Apple's configuration profile: a top-level dict with PayloadType
// "Configuration", PayloadVersion 1, PayloadIdentifier, PayloadUUID and
// PayloadContent; the mail payload has PayloadType "com.apple.mail.managed"
// with EmailAccountType "EmailTypeIMAP", EmailAddress, the Incoming… and
// Outgoing… server keys, and authentication "EmailAuthPassword".
func TestMobileConfig(t *testing.T) {
	c := testClients()
	body := c.MobileConfig("anna@example.test")
	if !strings.HasPrefix(string(body), `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`) {
		t.Errorf("the head of the profile:\n%.200s", body)
	}
	p := plist(t, body)
	uuid := regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)
	if p["PayloadType"] != "Configuration" || p["PayloadVersion"] != int64(1) || !uuid.MatchString(p["PayloadUUID"].(string)) ||
		!strings.HasPrefix(p["PayloadIdentifier"].(string), "test.example.mail.robinmail.") || p["PayloadDisplayName"] == "" {
		t.Errorf("the profile: %v", p)
	}
	content := p["PayloadContent"].([]any)
	if len(content) != 1 {
		t.Fatalf("%d payloads", len(content))
	}
	m := content[0].(map[string]any)
	for key, want := range map[string]any{
		"PayloadType": "com.apple.mail.managed", "PayloadVersion": int64(1),
		"EmailAccountType": "EmailTypeIMAP", "EmailAddress": "anna@example.test",
		"IncomingMailServerAuthentication": "EmailAuthPassword", "IncomingMailServerHostName": "mail.example.test",
		"IncomingMailServerPortNumber": int64(993), "IncomingMailServerUseSSL": true, "IncomingMailServerUsername": "anna@example.test",
		"OutgoingMailServerAuthentication": "EmailAuthPassword", "OutgoingMailServerHostName": "mail.example.test",
		"OutgoingMailServerPortNumber": int64(465), "OutgoingMailServerUseSSL": true, "OutgoingMailServerUsername": "anna@example.test",
		"OutgoingPasswordSameAsIncomingPassword": true,
	} {
		if m[key] != want {
			t.Errorf("%s: %v, want %v", key, m[key], want)
		}
	}
	if !uuid.MatchString(m["PayloadUUID"].(string)) || m["PayloadUUID"] == p["PayloadUUID"] {
		t.Errorf("the payload's UUID: %v", m["PayloadUUID"])
	}
	// No password is in it, under any key.
	for key := range m {
		if strings.Contains(key, "Password") && key != "OutgoingPasswordSameAsIncomingPassword" {
			t.Errorf("the profile has %s", key)
		}
	}
	// The same account gives the same profile, so installing it again
	// replaces it; another account gives another.
	if string(c.MobileConfig("anna@example.test")) != string(body) {
		t.Error("two profiles of one account differ")
	}
	if other := plist(t, c.MobileConfig("bob@example.test")); other["PayloadUUID"] == p["PayloadUUID"] {
		t.Error("two accounts share a UUID")
	}
	if p := plist(t, c.MobileConfig(`"a<b&c"@example.test`)); p["PayloadContent"].([]any)[0].(map[string]any)["EmailAddress"] != `"a<b&c"@example.test` {
		t.Error("an odd address is not written as text")
	}
	// With a web listener, the profile also sets up CalDAV and CardDAV
	// (Apple's com.apple.caldav.account and com.apple.carddav.account).
	c.HTTPS = 8443
	dav := plist(t, c.MobileConfig("anna@example.test"))["PayloadContent"].([]any)
	if len(dav) != 3 {
		t.Fatalf("%d payloads with HTTPS", len(dav))
	}
	for i, kind := range []string{"CalDAV", "CardDAV"} {
		m := dav[i+1].(map[string]any)
		for key, want := range map[string]any{
			"PayloadType": "com.apple." + strings.ToLower(kind) + ".account", kind + "HostName": "mail.example.test", kind + "Port": int64(8443),
			kind + "PrincipalURL": "https://mail.example.test:8443/dav/", kind + "UseSSL": true, kind + "Username": "anna@example.test",
		} {
			if m[key] != want {
				t.Errorf("%s %s: %v, want %v", kind, key, m[key], want)
			}
		}
	}
}

// The profile and the settings are for the user who is logged in.
func TestMailSettingsForUser(t *testing.T) {
	st := newSite(t)
	st.s.Clients = testClients()
	for _, path := range []string{"/auth/mobileconfig", "/auth/mail-settings"} {
		if rec := st.do("GET", tlsBase+path, ""); rec.Code != 401 {
			t.Errorf("%s without a login: %d", path, rec.Code)
		}
	}
	c := setCookie(st.login(tlsBase, "anna@example.test", "correct horse"))
	rec := st.do("GET", tlsBase+"/auth/mobileconfig", "", cookie(c))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-apple-aspen-config" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="mail.mobileconfig"` || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("the profile: %d %v", rec.Code, rec.Header())
	}
	if p := plist(t, rec.Body.Bytes()); p["PayloadContent"].([]any)[0].(map[string]any)["EmailAddress"] != "anna@example.test" {
		t.Errorf("the profile is for %v", p)
	}
	rec = st.do("GET", tlsBase+"/auth/mail-settings", "", cookie(c))
	var got struct {
		Username string
		Profile  bool
		IMAP     []struct {
			Host, Security string
			Port           int
		}
		SMTP []struct {
			Host, Security string
			Port           int
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("the settings: %d %v", rec.Code, err)
	}
	if got.Username != "anna@example.test" || !got.Profile || len(got.IMAP) != 2 || len(got.SMTP) != 2 ||
		got.IMAP[0].Host != "mail.example.test" || got.IMAP[0].Port != 993 || got.IMAP[0].Security != "TLS" ||
		got.SMTP[1].Port != 587 || got.SMTP[1].Security != "STARTTLS" {
		t.Errorf("the settings: %+v", got)
	}
	// A server without IMAP has no profile and says so.
	st.s.Clients = &MailClients{Host: "mail.example.test", Submissions: 465}
	if rec := st.do("GET", tlsBase+"/auth/mobileconfig", "", cookie(c)); rec.Code != 404 {
		t.Errorf("the profile without IMAP: %d", rec.Code)
	}
	rec = st.do("GET", tlsBase+"/auth/mail-settings", "", cookie(c))
	if json.Unmarshal(rec.Body.Bytes(), &got); got.Profile || len(got.IMAP) != 0 || len(got.SMTP) != 1 {
		t.Errorf("the settings without IMAP: %s", rec.Body)
	}
	st.s.Clients = nil
	if rec := st.do("GET", tlsBase+"/auth/mail-settings", "", cookie(c)); rec.Code != 200 {
		t.Errorf("the settings without clients: %d", rec.Code)
	}
}
