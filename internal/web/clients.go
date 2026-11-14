package web

import (
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mikepage/robinmail/internal/address"
)

// Mail programs that configure themselves. Three ways are served, all from
// the same settings:
//
//   - Thunderbird's autoconfig (the "config-v1.1.xml" format of Mozilla's
//     ISPDB, also read by K-9, Evolution and others): GET
//     /mail/config-v1.1.xml under autoconfig.<domain>, and under the
//     hostname at that path and at /.well-known/autoconfig/mail/config-v1.1.xml.
//   - Outlook's Autodiscover in its plain XML form ("POX"): POST
//     /autodiscover/autodiscover.xml under the hostname, found through the
//     SRV record _autodiscover._tcp.<domain>.
//   - An Apple configuration profile for the logged-in user: GET
//     /auth/mobileconfig. It is not signed, so iOS and macOS show it as
//     "Unsigned" when it is installed; it holds no password.
//
// The first two are public and say the same for every address of a domain
// of ours, existing or not: they give away the names and ports of the
// server and nothing about accounts.

// MailClients are the settings mail programs are given.
type MailClients struct {
	// Host is the name of the IMAP and submission servers: the hostname.
	Host string
	// The ports, 0 for a listener that is not there. IMAPS and
	// Submissions have TLS from the first byte (RFC 8314), the other two
	// STARTTLS.
	IMAPS, IMAP, Submissions, Submission int
	// Domain reports whether a domain is ours.
	Domain func(ctx context.Context, domain string) bool
}

// MailServer is one way to reach a service.
type MailServer struct {
	Port     int
	Implicit bool // TLS from the first byte; otherwise STARTTLS
}

func servers(implicit, starttls int) []MailServer {
	var out []MailServer
	if implicit > 0 {
		out = append(out, MailServer{implicit, true})
	}
	if starttls > 0 {
		out = append(out, MailServer{starttls, false})
	}
	return out
}

// Incoming and Outgoing are the IMAP and submission servers, the preferred
// one first: implicit TLS before STARTTLS (RFC 8314 3).
func (c *MailClients) Incoming() []MailServer { return servers(c.IMAPS, c.IMAP) }
func (c *MailClients) Outgoing() []MailServer { return servers(c.Submissions, c.Submission) }

// usable reports whether there is something to configure: a mail program
// needs both to read and to send.
func (c *MailClients) usable() bool {
	return c != nil && c.Host != "" && len(c.Incoming()) > 0 && len(c.Outgoing()) > 0
}

func xmlText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Autoconfig is the config-v1.1.xml of a domain, as Thunderbird fetches
// it. The user name is the placeholder %EMAILADDRESS%: accounts log in
// with their address.
func (c *MailClients) Autoconfig(domain string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&b, "<clientConfig version=\"1.1\">\n  <emailProvider id=\"%s\">\n    <domain>%s</domain>\n", xmlText(domain), xmlText(domain))
	fmt.Fprintf(&b, "    <displayName>%s</displayName>\n    <displayShortName>%s</displayShortName>\n", xmlText(domain), xmlText(domain))
	server := func(element, kind string, s MailServer) {
		socket := "STARTTLS"
		if s.Implicit {
			socket = "SSL"
		}
		fmt.Fprintf(&b, "    <%s type=\"%s\">\n      <hostname>%s</hostname>\n      <port>%d</port>\n      <socketType>%s</socketType>\n"+
			"      <authentication>password-cleartext</authentication>\n      <username>%%EMAILADDRESS%%</username>\n    </%s>\n",
			element, kind, xmlText(c.Host), s.Port, socket, element)
	}
	for _, s := range c.Incoming() {
		server("incomingServer", "imap", s)
	}
	for _, s := range c.Outgoing() {
		server("outgoingServer", "smtp", s)
	}
	b.WriteString("  </emailProvider>\n</clientConfig>\n")
	return []byte(b.String())
}

// AutoconfigHost is the handler for autoconfig.<domain>: the file and
// nothing else.
func (c *MailClients) AutoconfigHost(domain string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mail/config-v1.1.xml" || !c.usable() {
			http.NotFound(w, r)
			return
		}
		c.serveAutoconfig(w, r, domain)
	})
}

func (c *MailClients) serveAutoconfig(w http.ResponseWriter, r *http.Request, domain string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "max-age=300")
	w.Write(c.Autoconfig(domain))
}

// autoconfig serves the file under the hostname. The domain is that of
// the emailaddress parameter Thunderbird sends; without one, or for a
// domain that is not ours, there is nothing to say.
func (s *Server) autoconfig(w http.ResponseWriter, r *http.Request) {
	c := s.Clients
	if !c.usable() {
		http.NotFound(w, r)
		return
	}
	domain := ownDomain(r.Context(), c, r.URL.Query().Get("emailaddress"))
	if domain == "" {
		http.NotFound(w, r)
		return
	}
	c.serveAutoconfig(w, r, domain)
}

// ownDomain is the domain of an address when it is one of ours, or "".
func ownDomain(ctx context.Context, c *MailClients, addr string) string {
	a, err := address.Parse(strings.TrimSpace(addr))
	if err != nil || c.Domain == nil || !c.Domain(ctx, a.Domain) {
		return ""
	}
	return a.Domain
}

// The namespaces of Autodiscover's plain XML.
const (
	autodiscoverRequestNS  = "http://schemas.microsoft.com/exchange/autodiscover/outlook/requestschema/2006"
	autodiscoverResponseNS = "http://schemas.microsoft.com/exchange/autodiscover/responseschema/2006"
	autodiscoverOutlookNS  = "http://schemas.microsoft.com/exchange/autodiscover/outlook/responseschema/2006a"
)

// AutodiscoverAddress reads the address out of an Autodiscover request:
//
//	<Autodiscover xmlns=".../outlook/requestschema/2006">
//	  <Request>
//	    <EMailAddress>user@example.org</EMailAddress>
//	    <AcceptableResponseSchema>.../outlook/responseschema/2006a</AcceptableResponseSchema>
//	  </Request>
//	</Autodiscover>
func AutodiscoverAddress(body []byte) (string, bool) {
	var req struct {
		XMLName xml.Name `xml:"Autodiscover"`
		Request struct {
			EMailAddress string `xml:"EMailAddress"`
			Schema       string `xml:"AcceptableResponseSchema"`
		} `xml:"Request"`
	}
	d := xml.NewDecoder(strings.NewReader(string(body)))
	d.Strict = true
	if err := d.Decode(&req); err != nil {
		return "", false
	}
	// Only the Outlook schema is answered: the mobile one
	// (mobilesync) is for ActiveSync, which is not here.
	if s := strings.TrimSpace(req.Request.Schema); s != "" && s != autodiscoverOutlookNS {
		return "", false
	}
	addr := strings.TrimSpace(req.Request.EMailAddress)
	return addr, addr != ""
}

// Autodiscover is the answer for an address: IMAP and SMTP, the preferred
// server of each.
func (c *MailClients) Autodiscover(addr string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	fmt.Fprintf(&b, "<Autodiscover xmlns=\"%s\">\n  <Response xmlns=\"%s\">\n    <Account>\n      <AccountType>email</AccountType>\n      <Action>settings</Action>\n",
		autodiscoverResponseNS, autodiscoverOutlookNS)
	protocol := func(kind string, s MailServer, extra string) {
		encryption := "TLS" // STARTTLS, in Outlook's words
		if s.Implicit {
			encryption = "SSL"
		}
		fmt.Fprintf(&b, "      <Protocol>\n        <Type>%s</Type>\n        <Server>%s</Server>\n        <Port>%d</Port>\n"+
			"        <LoginName>%s</LoginName>\n        <DomainRequired>off</DomainRequired>\n        <SPA>off</SPA>\n"+
			"        <SSL>on</SSL>\n        <Encryption>%s</Encryption>\n        <AuthRequired>on</AuthRequired>\n%s      </Protocol>\n",
			kind, xmlText(c.Host), s.Port, xmlText(addr), encryption, extra)
	}
	protocol("IMAP", c.Incoming()[0], "")
	protocol("SMTP", c.Outgoing()[0], "        <UsePOPAuth>off</UsePOPAuth>\n        <SMTPLast>off</SMTPLast>\n")
	b.WriteString("    </Account>\n  </Response>\n</Autodiscover>\n")
	return []byte(b.String())
}

// autodiscoverError is the answer to a request that cannot be served, in
// the error form of the protocol (ErrorCode 600: invalid request).
func autodiscoverError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<Autodiscover xmlns=\"%s\">\n  <Response>\n    <Error Id=\"0\">\n"+
		"      <ErrorCode>600</ErrorCode>\n      <Message>%s</Message>\n      <DebugData />\n    </Error>\n  </Response>\n</Autodiscover>\n",
		autodiscoverResponseNS, xmlText(message))
}

// autodiscover serves POST /autodiscover/autodiscover.xml. It asks for no
// login (Outlook sends none on the first try, and the answer holds no
// secret) and says the same for every address of a domain of ours.
func (s *Server) autodiscover(w http.ResponseWriter, r *http.Request) {
	c := s.Clients
	if !c.usable() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	addr, ok := AutodiscoverAddress(body)
	if !ok || ownDomain(r.Context(), c, addr) == "" {
		autodiscoverError(w, "Invalid Request")
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(c.Autodiscover(addr))
}

// profileUUID makes the UUID of a payload from what it is for, so that a
// profile downloaded again replaces the one that is installed instead of
// adding a second account.
func profileUUID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	u := sum[:16]
	u[6] = u[6]&0x0f | 0x50 // version 5 in form: a name-based UUID
	u[8] = u[8]&0x3f | 0x80
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]))
}

// reverseDNS turns mail.example.org into org.example.mail, the form of a
// payload identifier.
func reverseDNS(host string) string {
	labels := strings.Split(strings.ToLower(host), ".")
	for i, j := 0, len(labels)-1; i < j; i, j = i+1, j-1 {
		labels[i], labels[j] = labels[j], labels[i]
	}
	return strings.Join(labels, ".")
}

// MobileConfig is an Apple configuration profile with one mail account
// (payload type com.apple.mail.managed) for an address. It is a property
// list and is not signed. The password is left out: the device asks for
// it when the profile is installed.
func (c *MailClients) MobileConfig(addr string) []byte {
	in, out := c.Incoming()[0], c.Outgoing()[0]
	id := reverseDNS(c.Host) + ".robinmail"
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n" +
		`<plist version="1.0">` + "\n<dict>\n\t<key>PayloadContent</key>\n\t<array>\n\t\t<dict>\n")
	str := func(indent, key, value string) {
		fmt.Fprintf(&b, "%s<key>%s</key>\n%s<string>%s</string>\n", indent, key, indent, xmlText(value))
	}
	num := func(indent, key string, value int) {
		fmt.Fprintf(&b, "%s<key>%s</key>\n%s<integer>%s</integer>\n", indent, key, indent, strconv.Itoa(value))
	}
	flag := func(indent, key string, value bool) {
		fmt.Fprintf(&b, "%s<key>%s</key>\n%s<%t/>\n", indent, key, indent, value)
	}
	const in2 = "\t\t\t"
	str(in2, "EmailAccountDescription", addr)
	str(in2, "EmailAccountType", "EmailTypeIMAP")
	str(in2, "EmailAddress", addr)
	str(in2, "IncomingMailServerAuthentication", "EmailAuthPassword")
	str(in2, "IncomingMailServerHostName", c.Host)
	num(in2, "IncomingMailServerPortNumber", in.Port)
	// "UseSSL" is true for both: on a port that starts in the clear the
	// device does STARTTLS.
	flag(in2, "IncomingMailServerUseSSL", true)
	str(in2, "IncomingMailServerUsername", addr)
	str(in2, "OutgoingMailServerAuthentication", "EmailAuthPassword")
	str(in2, "OutgoingMailServerHostName", c.Host)
	num(in2, "OutgoingMailServerPortNumber", out.Port)
	flag(in2, "OutgoingMailServerUseSSL", true)
	str(in2, "OutgoingMailServerUsername", addr)
	flag(in2, "OutgoingPasswordSameAsIncomingPassword", true)
	str(in2, "PayloadDescription", "Mail account "+addr)
	str(in2, "PayloadDisplayName", addr)
	str(in2, "PayloadIdentifier", id+".mail."+profileUUID("mail", c.Host, addr))
	str(in2, "PayloadType", "com.apple.mail.managed")
	str(in2, "PayloadUUID", profileUUID("mail", c.Host, addr))
	num(in2, "PayloadVersion", 1)
	b.WriteString("\t\t</dict>\n\t</array>\n")
	str("\t", "PayloadDescription", "Mail account "+addr+" on "+c.Host)
	str("\t", "PayloadDisplayName", "Mail: "+addr)
	str("\t", "PayloadIdentifier", id+"."+profileUUID("profile", c.Host, addr))
	flag("\t", "PayloadRemovalDisallowed", false)
	str("\t", "PayloadType", "Configuration")
	str("\t", "PayloadUUID", profileUUID("profile", c.Host, addr))
	num("\t", "PayloadVersion", 1)
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

// mobileConfig serves the profile of the logged-in user as a download.
func (s *Server) mobileConfig(w http.ResponseWriter, r *http.Request, u user) {
	if !s.Clients.usable() {
		jsonError(w, http.StatusNotFound, "this server has no IMAP or no submission listener")
		return
	}
	w.Header().Set("Content-Type", "application/x-apple-aspen-config")
	w.Header().Set("Content-Disposition", `attachment; filename="mail.mobileconfig"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(s.Clients.MobileConfig(u.acct.Address))
}

// mailSettings tells the webmail what to show a user who sets up a mail
// program by hand.
func (s *Server) mailSettings(w http.ResponseWriter, r *http.Request, u user) {
	type server struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Security string `json:"security"`
	}
	list := func(in []MailServer) []server {
		out := []server{}
		for _, m := range in {
			security := "STARTTLS"
			if m.Implicit {
				security = "TLS"
			}
			out = append(out, server{s.Clients.Host, m.Port, security})
		}
		return out
	}
	c := s.Clients
	if c == nil {
		c = &MailClients{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": u.acct.Address,
		"imap":     list(c.Incoming()),
		"smtp":     list(c.Outgoing()),
		"profile":  c.usable(),
	})
}
