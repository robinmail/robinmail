// Package config loads the JSON configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
	MasterKeyFile string `json:"master_key_file"`
	Rqlite        Rqlite `json:"rqlite"`
	TLS           TLS    `json:"tls"`
	SMTP          SMTP   `json:"smtp"`
	Rspamd        Rspamd `json:"rspamd"`
}

// The domains the server accepts mail for are not in this file: the nodes of
// a cluster share them through the domain table ("robinmail domain add").

// Rqlite is where all metadata lives.
type Rqlite struct {
	// URL of one rqlite node, for example "http://127.0.0.1:4001". Basic
	// auth credentials may be given as "http://user:password@host:4001".
	URL string `json:"url"`
}

// SMTP configures the inbound listener. Zero values get the server defaults.
type SMTP struct {
	Listen              string `json:"listen"` // default ":25"
	MaxMessageBytes     int64  `json:"max_message_bytes"`
	MaxRecipients       int    `json:"max_recipients"`
	MaxConnections      int    `json:"max_connections"`
	MaxConnectionsPerIP int    `json:"max_connections_per_ip"`
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

	if !httpURL(c.Rqlite.URL) {
		return nil, fmt.Errorf("%s: rqlite url must be an http:// or https:// URL", path)
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
	if c.Rspamd.URL != "" && !httpURL(c.Rspamd.URL) {
		return nil, fmt.Errorf("%s: rspamd url must start with http:// or https://", path)
	}
	return &c, nil
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// BlobDir is where this node keeps its blob files.
func (c *Config) BlobDir() string { return filepath.Join(c.DataDir, "blobs") }
