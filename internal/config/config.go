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
	// DataDir is only used by the "local" blob backend, which keeps blobs in
	// <data_dir>/blobs. A relative path is resolved against the directory of
	// the config file.
	DataDir string `json:"data_dir"`
	// Domains lists the domains this server accepts mail for.
	Domains []string `json:"domains"`
	Rqlite  Rqlite   `json:"rqlite"`
	Blob    Blob     `json:"blob"`
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

// Blob selects where message files are kept.
type Blob struct {
	// Backend is "local" (development, the default) or "s3".
	Backend string `json:"backend"`
	S3      S3     `json:"s3"`
}

// S3 configures the "s3" blob backend. Requests are path-style:
// <endpoint>/<bucket>/<key>.
type S3 struct {
	Endpoint  string `json:"endpoint"` // "https://s3.example.test"
	Region    string `json:"region"`   // "garage" for Garage, "auto" for R2
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
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

const (
	BlobLocal = "local"
	BlobS3    = "s3"
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
	if c.Blob.Backend == "" {
		c.Blob.Backend = BlobLocal
	}
	switch c.Blob.Backend {
	case BlobLocal:
		if c.DataDir == "" {
			return nil, fmt.Errorf("%s: blob backend \"local\" needs data_dir", path)
		}
	case BlobS3:
		s := c.Blob.S3
		if !httpURL(s.Endpoint) {
			return nil, fmt.Errorf("%s: blob s3 endpoint must be an http:// or https:// URL", path)
		}
		if s.Region == "" || s.Bucket == "" || s.AccessKey == "" || s.SecretKey == "" {
			return nil, fmt.Errorf("%s: blob s3 needs region, bucket, access_key and secret_key", path)
		}
	default:
		return nil, fmt.Errorf("%s: unknown blob backend %q", path, c.Blob.Backend)
	}
	if c.DataDir != "" && !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(filepath.Dir(path), c.DataDir)
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

// BlobDir is where the "local" blob backend keeps its files.
func (c *Config) BlobDir() string { return filepath.Join(c.DataDir, "blobs") }
