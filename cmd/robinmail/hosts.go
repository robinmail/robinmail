package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robinmail/robinmail/internal/config"
	"github.com/robinmail/robinmail/internal/web"
)

// Besides its hostname the server answers under names that belong to its
// domains: mta-sts.<domain> for the MTA-STS policy (RFC 8461 3.3) and
// autoconfig.<domain> for mail clients that look there. Which names those
// are follows the domain table, so a domain added on any node is served
// by all of them without a restart.

// ownDomains is the domain table, remembered for a short while: a TLS
// handshake and a request for another host both ask.
type ownDomains struct {
	list func(ctx context.Context) ([]string, error)
	mu   sync.Mutex
	at   time.Time
	set  map[string]bool
}

// ownDomainsFor is how long the domain table is remembered.
var ownDomainsFor = 30 * time.Second

func (o *ownDomains) has(ctx context.Context, domain string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.set == nil || time.Since(o.at) > ownDomainsFor {
		list, err := o.list(ctx)
		if err != nil {
			// What was known stays in use: a database that is away for a
			// moment must not take the policy offline.
			return o.set[domain]
		}
		o.set = map[string]bool{}
		for _, d := range list {
			o.set[d] = true
		}
		o.at = time.Now()
	}
	return o.set[domain]
}

// stsPolicy is the MTA-STS policy of the config as the file of RFC 8461
// 3.2, with CRLF line ends, and whether there is one to serve.
func stsPolicy(c config.MTASTS) (string, bool) {
	if c.Mode == "" {
		return "", false
	}
	var b strings.Builder
	b.WriteString("version: STSv1\r\nmode: " + c.Mode + "\r\n")
	if c.Mode != "none" {
		for _, mx := range c.MX {
			b.WriteString("mx: " + strings.ToLower(mx) + "\r\n")
		}
	}
	fmt.Fprintf(&b, "max_age: %d\r\n", c.MaxAgeSeconds)
	return b.String(), true
}

// stsPolicyID is the id of the policy for the TXT record at _mta-sts (RFC
// 8461 3.1): it has to change when the policy does and only then, so it is
// taken from the policy itself.
func stsPolicyID(policy string) string {
	sum := sha256.Sum256([]byte(policy))
	return hex.EncodeToString(sum[:])[:20]
}

// extraNames reports whether name is one of the names of our domains that
// this server answers under. It is the ACME host policy besides the
// hostname.
func extraNames(cfg *config.Config, domains *ownDomains) func(ctx context.Context, name string) bool {
	return func(ctx context.Context, name string) bool {
		if d, ok := strings.CutPrefix(name, "mta-sts."); ok && cfg.MTASTS.Mode != "" {
			return domains.has(ctx, d)
		}
		if d, ok := strings.CutPrefix(name, "autoconfig."); ok {
			return domains.has(ctx, d)
		}
		return false
	}
}

// extraHosts is web.Server.Hosts: the handler for a request under one of
// those names.
func extraHosts(cfg *config.Config, domains *ownDomains, autoconfig func(domain string) http.Handler) func(host string) http.Handler {
	policy, hasPolicy := stsPolicy(cfg.MTASTS)
	sts := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RFC 8461 3.3: fetched with GET over HTTPS at this one path, as
		// text/plain. Nothing else lives under the name.
		if r.URL.Path != "/.well-known/mta-sts.txt" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.TLS == nil {
			http.Error(w, "the policy is only served over HTTPS", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=300")
		w.Header().Del("Content-Security-Policy")
		w.Write([]byte(policy))
	})
	return func(host string) http.Handler {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if d, ok := strings.CutPrefix(host, "mta-sts."); ok && hasPolicy && domains.has(ctx, d) {
			return sts
		}
		if d, ok := strings.CutPrefix(host, "autoconfig."); ok && autoconfig != nil && domains.has(ctx, d) {
			return autoconfig(d)
		}
		return nil
	}
}

// mailClients are the settings mail programs are given (autoconfig,
// autodiscover, the Apple profile): the hostname and the ports of the
// listeners in the config.
func mailClients(cfg *config.Config, domains *ownDomains) *web.MailClients {
	port := func(listen string) int {
		n, _ := strconv.Atoi(listenPort(listen))
		return n
	}
	return &web.MailClients{
		Host:  cfg.Hostname,
		IMAPS: port(cfg.IMAP.ListenTLS), IMAP: port(cfg.IMAP.Listen),
		Submissions: port(cfg.Submission.ListenTLS), Submission: port(cfg.Submission.Listen),
		Domain: domains.has,
	}
}
