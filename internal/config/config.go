// Package config loads the JSON configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mikepage/robinmail/internal/address"
)

type Config struct {
	// Hostname is the name the server announces and requests certificates for.
	Hostname string `json:"hostname"`
	// DataDir holds this node's blobs, encrypted, in <data_dir>/blobs. A
	// relative path is resolved against the directory of the config file.
	DataDir string `json:"data_dir"`
	// MasterKeyFile holds the key that wraps the account keys blobs are
	// encrypted with. Make one with "robinmail key generate"; every node of
	// a cluster needs the same file. Relative paths as for DataDir.
	MasterKeyFile string     `json:"master_key_file"`
	Rqlite        Rqlite     `json:"rqlite"`
	Node          Node       `json:"node"`
	TLS           TLS        `json:"tls"`
	SMTP          SMTP       `json:"smtp"`
	Web           Web        `json:"web"`
	Rspamd        Rspamd     `json:"rspamd"`
	Submission    Submission `json:"submission"`
	Outbound      Outbound   `json:"outbound"`
	IMAP          IMAP       `json:"imap"`
	Dev           Dev        `json:"dev"`
}

// Dev holds settings for development and test labs (contrib/lab). None of
// them belongs in a real installation.
type Dev struct {
	// RetrySeconds replaces the retry schedule of the outbound queue (five
	// minutes, doubling up to four hours) by this fixed wait, so a test
	// can see a second try without waiting.
	RetrySeconds int `json:"retry_seconds"`
	// SMTPTrace logs every SMTP command and reply, of the listeners and
	// of the outbound client, without message content and without
	// credentials.
	SMTPTrace bool `json:"smtp_trace"`
	// IMAPTrace logs every IMAP command and tagged reply, without
	// literals and without the arguments of LOGIN and AUTHENTICATE.
	IMAPTrace bool `json:"imap_trace"`
}

// The domains the server accepts mail for are not in this file: the nodes of
// a cluster share them through the domain table ("robinmail domain add").

// Rqlite is where all metadata lives.
type Rqlite struct {
	// URL of one rqlite node, for example "http://127.0.0.1:4001". Basic
	// auth credentials may be given as "http://user:password@host:4001".
	URL string `json:"url"`
	// URLs lists several nodes of the same rqlite cluster instead. The
	// first is tried first, so put the nearest one there; when it stops
	// answering the others are used.
	URLs []string `json:"urls"`
}

// Nodes returns the configured rqlite URLs, nearest first.
func (r Rqlite) Nodes() []string {
	if r.URL != "" {
		return append([]string{r.URL}, r.URLs...)
	}
	return r.URLs
}

// Node makes this robinmail a member of a cluster. Without an id it runs
// alone: blobs stay on its own disk and nothing listens for other nodes.
// In a cluster every node needs this section, the same master key file and
// the same rqlite cluster.
type Node struct {
	// ID names the node in the node table: lowercase letters, digits and
	// "-". It must be unique in the cluster and should not change.
	ID string `json:"id"`
	// Listen is where the node listens for the other nodes, for example
	// ":7443". Mutual TLS; see internal/cluster.
	Listen string `json:"listen"`
	// Address is the host:port under which the other nodes reach Listen.
	Address string `json:"address"`
	// BlobCopies is how many nodes must have a blob before a write is
	// acknowledged, this node included. With fewer nodes registered than
	// this, all of them. [2]
	BlobCopies int `json:"blob_copies"`
	// RepairIntervalSeconds is how often the node compares what the
	// database refers to with its disk and fetches missing blobs. It also
	// does so at start. [300]
	RepairIntervalSeconds int `json:"repair_interval_seconds"`
}

// SMTP configures the inbound listener. Zero values get the server defaults.
type SMTP struct {
	Listen              string `json:"listen"` // default ":25"
	MaxMessageBytes     int64  `json:"max_message_bytes"`
	MaxRecipients       int    `json:"max_recipients"`
	MaxConnections      int    `json:"max_connections"`
	MaxConnectionsPerIP int    `json:"max_connections_per_ip"`
}

// Submission configures the listeners users send mail through (RFC 6409,
// RFC 8314). Both need a certificate (tls): a password never crosses them
// in the clear. Without a listener only JMAP submits. The size and
// recipient limits are those of smtp.
type Submission struct {
	// Listen is the STARTTLS listener, usually ":587".
	Listen string `json:"listen"`
	// ListenTLS is the listener with TLS from the first byte, usually
	// ":465".
	ListenTLS string `json:"listen_tls"`
	// MaxRecipientsPerHour is how many recipients one account may send to
	// within an hour, over SMTP and JMAP together and on all nodes. [300]
	MaxRecipientsPerHour int `json:"max_recipients_per_hour"`
	// UndoSeconds is how long a message submitted over JMAP waits before
	// it is sent, so the user can still cancel it. [10]; 0 sends at once.
	UndoSeconds *int `json:"undo_seconds"`
}

// IMAP configures the IMAP listeners. Both need a certificate (tls):
// nobody can log in without TLS. Without a listener IMAP is off.
type IMAP struct {
	// ListenTLS is the listener with TLS from the first byte, usually
	// ":993". It is the one to give to mail clients (RFC 8314).
	ListenTLS string `json:"listen_tls"`
	// Listen is the listener that starts in the clear and offers
	// STARTTLS, usually ":143". Until STARTTLS it announces LOGINDISABLED.
	Listen string `json:"listen"`
	// Limits on connections: in all [1000], from one address [20], logged
	// in as one user [20].
	MaxConnections        int `json:"max_connections"`
	MaxConnectionsPerIP   int `json:"max_connections_per_ip"`
	MaxConnectionsPerUser int `json:"max_connections_per_user"`
}

// Outbound configures the queue that delivers to other servers.
type Outbound struct {
	// Concurrency is how many messages a node works on at once [8];
	// PerDestination how many connections one recipient domain gets [2].
	Concurrency    int `json:"concurrency"`
	PerDestination int `json:"per_destination"`
	// Routes sends the mail for a domain to a fixed "host:port" instead of
	// its MX hosts; "*" is every other domain. For a relay host, and for
	// development without DNS.
	Routes map[string]string `json:"routes"`
}

// Web configures the HTTP side: login, JMAP and the webmail on one origin.
// Without a listener it is off.
type Web struct {
	// Listen is the HTTPS listener, usually ":443". It needs a certificate
	// (tls). With ACME this is also the listener the certificate is
	// validated over.
	Listen string `json:"listen"`
	// HTTPListen is a plain HTTP listener for development and tests. It
	// must be on a loopback address: sessions and passwords cross it
	// unencrypted.
	HTTPListen string `json:"http_listen"`
	// DevOrigins are origins besides the server's own that may call the
	// API, such as "http://localhost:5173" for the Vite dev server.
	DevOrigins []string `json:"dev_origins"`
}

// Rspamd configures spam scanning. An empty URL turns scanning off.
type Rspamd struct {
	URL string `json:"url"`
	// FailOpen delivers mail unscanned when Rspamd is down, instead of
	// telling the sender to retry.
	FailOpen       bool `json:"fail_open"`
	TimeoutSeconds int  `json:"timeout_seconds"`
}

type TLS struct {
	CertFile  string `json:"cert_file"`
	KeyFile   string `json:"key_file"`
	ACME      bool   `json:"acme"`
	ACMEEmail string `json:"acme_email"`
}

// The smallest limits RFC 5321 4.5.3.1 lets a server have.
const (
	MinMessageBytes = 64 << 10
	MinRecipients   = 100
)

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var c Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	c.Hostname = strings.ToLower(c.Hostname)
	if !address.ValidDomain(c.Hostname) {
		return nil, fmt.Errorf("%s: invalid hostname %q", path, c.Hostname)
	}

	if len(c.Rqlite.Nodes()) == 0 {
		return nil, fmt.Errorf("%s: rqlite url (or urls) is required", path)
	}
	for _, u := range c.Rqlite.Nodes() {
		if !httpURL(u) {
			return nil, fmt.Errorf("%s: rqlite url must be an http:// or https:// URL", path)
		}
	}
	if n := c.Node; n.ID == "" {
		if n.Listen != "" || n.Address != "" || n.BlobCopies != 0 || n.RepairIntervalSeconds != 0 {
			return nil, fmt.Errorf("%s: node needs an id", path)
		}
	} else {
		if !nodeID.MatchString(n.ID) {
			return nil, fmt.Errorf("%s: node id %q must be 1 to 32 lowercase letters, digits or \"-\"", path, n.ID)
		}
		if _, _, err := net.SplitHostPort(n.Listen); err != nil {
			return nil, fmt.Errorf("%s: node listen must be an address such as :7443", path)
		}
		if host, _, err := net.SplitHostPort(n.Address); err != nil || host == "" {
			return nil, fmt.Errorf("%s: node address must be the host:port the other nodes reach this node on", path)
		}
		if n.BlobCopies < 0 || n.RepairIntervalSeconds < 0 {
			return nil, errors.New(path + ": limits cannot be negative")
		}
	}
	if c.DataDir == "" {
		return nil, fmt.Errorf("%s: data_dir is required", path)
	}
	if c.MasterKeyFile == "" {
		return nil, fmt.Errorf("%s: master_key_file is required", path)
	}
	dir := filepath.Dir(path)
	if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(dir, c.DataDir)
	}
	if !filepath.IsAbs(c.MasterKeyFile) {
		c.MasterKeyFile = filepath.Join(dir, c.MasterKeyFile)
	}

	if !c.TLS.ACME && (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return nil, errors.New(path + ": tls needs both cert_file and key_file")
	}
	if c.SMTP.Listen == "" {
		c.SMTP.Listen = ":25"
	}
	if c.SMTP.MaxMessageBytes < 0 || c.SMTP.MaxRecipients < 0 || c.SMTP.MaxConnections < 0 ||
		c.SMTP.MaxConnectionsPerIP < 0 || c.Rspamd.TimeoutSeconds < 0 {
		return nil, errors.New(path + ": limits cannot be negative")
	}
	// RFC 5321 4.5.3.1.7 and 4.5.3.1.8: every server takes a message of
	// 64K octets and 100 recipients. Zero means the server's default.
	if n := c.SMTP.MaxMessageBytes; n != 0 && n < MinMessageBytes {
		return nil, fmt.Errorf("%s: smtp max_message_bytes must be at least %d (RFC 5321 4.5.3.1.7)", path, MinMessageBytes)
	}
	if n := c.SMTP.MaxRecipients; n != 0 && n < MinRecipients {
		return nil, fmt.Errorf("%s: smtp max_recipients must be at least %d (RFC 5321 4.5.3.1.8)", path, MinRecipients)
	}
	sub := c.Submission
	if (sub.Listen != "" || sub.ListenTLS != "") && !c.TLS.ACME && c.TLS.CertFile == "" {
		return nil, errors.New(path + ": submission listeners need a certificate (tls)")
	}
	if sub.MaxRecipientsPerHour < 0 || (sub.UndoSeconds != nil && (*sub.UndoSeconds < 0 || *sub.UndoSeconds > 3600)) {
		return nil, errors.New(path + ": submission: max_recipients_per_hour cannot be negative, undo_seconds is 0 to 3600")
	}
	if c.Submission.MaxRecipientsPerHour == 0 {
		c.Submission.MaxRecipientsPerHour = 300
	}
	if c.Submission.UndoSeconds == nil {
		ten := 10
		c.Submission.UndoSeconds = &ten
	}
	if (c.IMAP.Listen != "" || c.IMAP.ListenTLS != "") && !c.TLS.ACME && c.TLS.CertFile == "" {
		return nil, errors.New(path + ": imap listeners need a certificate (tls)")
	}
	if c.IMAP.MaxConnections < 0 || c.IMAP.MaxConnectionsPerIP < 0 || c.IMAP.MaxConnectionsPerUser < 0 {
		return nil, errors.New(path + ": limits cannot be negative")
	}
	if c.Outbound.Concurrency < 0 || c.Outbound.PerDestination < 0 || c.Dev.RetrySeconds < 0 {
		return nil, errors.New(path + ": limits cannot be negative")
	}
	for domain, route := range c.Outbound.Routes {
		if host, _, err := net.SplitHostPort(route); err != nil || host == "" || (domain != "*" && !address.ValidDomain(domain)) {
			return nil, fmt.Errorf("%s: outbound routes: %q: %q must be a domain (or \"*\") to a host:port", path, domain, route)
		}
	}
	if c.Web.Listen != "" && !c.TLS.ACME && c.TLS.CertFile == "" {
		return nil, errors.New(path + ": web listen needs a certificate (tls); for development use web http_listen")
	}
	if c.Web.HTTPListen != "" {
		host, _, err := net.SplitHostPort(c.Web.HTTPListen)
		ip := net.ParseIP(host)
		if err != nil || !(host == "localhost" || (ip != nil && ip.IsLoopback())) {
			return nil, fmt.Errorf("%s: web http_listen must be on a loopback address such as 127.0.0.1:8080", path)
		}
	}
	for _, o := range c.Web.DevOrigins {
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Scheme+"://"+u.Host != o {
			return nil, fmt.Errorf("%s: web dev_origins: %q is not an origin such as http://localhost:5173", path, o)
		}
	}
	if c.Rspamd.URL != "" && !httpURL(c.Rspamd.URL) {
		return nil, fmt.Errorf("%s: rspamd url must start with http:// or https://", path)
	}
	return &c, nil
}

var nodeID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// BlobDir is where this node keeps its blob files.
func (c *Config) BlobDir() string { return filepath.Join(c.DataDir, "blobs") }
