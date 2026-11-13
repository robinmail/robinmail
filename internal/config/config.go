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
	// Domains lists the domains this server accepts mail for.
	Domains []string `json:"domains"`
	Rqlite  Rqlite   `json:"rqlite"`
	TLS     TLS      `json:"tls"`
	SMTP    SMTP     `json:"smtp"`
	Rspamd  Rspamd   `json:"rspamd"`
}

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
	if len(c.Domains) == 0 {
		return nil, fmt.Errorf("%s: at least one domain is required", path)
	}
	for i, d := range c.Domains {
		d = strings.ToLower(d)
		if !address.ValidDomain(d) {
			return nil, fmt.Errorf("%s: invalid domain %q", path, d)
		}
		c.Domains[i] = d
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
	if c.Rspamd.URL != "" && !httpURL(c.Rspamd.URL) {
		return nil, fmt.Errorf("%s: rspamd url must start with http:// or https://", path)
	}
	return &c, nil
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// HasDomain reports whether the server accepts mail for domain.
func (c *Config) HasDomain(domain string) bool {
	for _, d := range c.Domains {
		if d == domain {
			return true
		}
	}
	return false
}

// BlobDir is where this node keeps its blob files.
func (c *Config) BlobDir() string { return filepath.Join(c.DataDir, "blobs") }
