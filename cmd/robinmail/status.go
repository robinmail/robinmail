package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/robinmail/robinmail/internal/metrics"
	"github.com/robinmail/robinmail/internal/store"
)

// A check is one thing a node needs to do its work.
type check struct {
	name   string
	detail string
	err    error
}

// certificateExpiry is when the first certificate of a PEM file ends.
func certificateExpiry(path string) (time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return time.Time{}, errors.New("no certificate in " + path)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, err
		}
		return cert.NotAfter, nil
	}
}

// loopbackHost reports whether a host name or address is this machine.
func loopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// clusterLine describes the database's cluster as this node sees it: the
// members, the leader, and how far behind the leader this node is.
func (a *app) clusterLine(ctx context.Context) (string, error) {
	st, err := a.db.ClusterStatus(ctx)
	if err != nil {
		return "", err
	}
	if st.Leader == 0 {
		return "", fmt.Errorf("no leader known; %d members", len(st.Members))
	}
	lead := "another node"
	for _, m := range st.Members {
		if m.ID == st.Leader && m.Node != "" {
			lead = m.Node
		}
	}
	if st.Leader == st.ID {
		lead = "this node"
	}
	return fmt.Sprintf("%d members, leader %s, term %d, applied %d of %d, snapshot at %d", len(st.Members), lead, st.Term, st.Applied, st.Commit, st.Snapshot), nil
}

// certificateWarning is how long before its end a certificate fails the
// check.
const certificateWarning = 7 * 24 * time.Hour

// checks looks at what this node depends on: the database, the blob
// directory and, with full, the master key against every account and the
// certificate. All of it is what "robinmail health" prints; /healthz asks
// the first two, which cost one row and one small file.
func (a *app) checks(ctx context.Context, full bool) []check {
	var out []check
	c := check{name: "database"}
	if c.err = a.store.Ping(ctx); c.err == nil {
		if totals, err := a.store.Totals(ctx); err != nil {
			c.err = err
		} else {
			c.detail = fmt.Sprintf("answers, schema %d", totals.Schema)
		}
	}
	out = append(out, c)

	if full {
		c = check{name: "cluster"}
		c.detail, c.err = a.clusterLine(ctx)
		out = append(out, c)
	}

	c = check{name: "master key"}
	if !full {
		// left out
	} else if use, err := a.store.AccountKeyUse(ctx); err != nil {
		c.err = err
	} else if use.Unreadable > 0 {
		c.err = fmt.Errorf("%d accounts have a key that no key of %s unwraps", use.Unreadable, a.cfg.MasterKeyFile)
	} else {
		old := 0
		for _, fp := range a.master.Fingerprints()[1:] {
			old += use.ByKey[fp]
		}
		c.detail = fmt.Sprintf("%d keys in the file, current %s", len(a.master.Fingerprints()), a.master.Fingerprints()[0])
		if old > 0 {
			c.detail += fmt.Sprintf(", %d accounts still under an older key", old)
		}
	}
	if full {
		out = append(out, c)
	}

	c = check{name: "blob directory", detail: a.cfg.BlobDir()}
	if err := os.MkdirAll(a.cfg.BlobDir(), 0o700); err != nil {
		c.err = err
	} else if f, err := os.CreateTemp(a.cfg.BlobDir(), ".health-*"); err != nil {
		c.err = err
	} else {
		_, werr := f.WriteString("ok")
		cerr := f.Close()
		os.Remove(f.Name())
		c.err = errors.Join(werr, cerr)
	}
	out = append(out, c)

	if full && a.cfg.TLS.CertFile != "" {
		c = check{name: "certificate"}
		if end, err := certificateExpiry(a.cfg.TLS.CertFile); err != nil {
			c.err = err
		} else if left := time.Until(end); left < certificateWarning {
			c.err = fmt.Errorf("%s ends %s", a.cfg.TLS.CertFile, end.Local().Format("2006-01-02 15:04"))
		} else {
			c.detail = fmt.Sprintf("valid until %s (%d days)", end.Local().Format("2006-01-02"), int(left.Hours()/24))
		}
		out = append(out, c)
	}
	return out
}

// healthy is web.Server.Health: nil when the node can do its work.
func (a *app) healthy(ctx context.Context) error {
	var errs []error
	for _, c := range a.checks(ctx, false) {
		if c.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.name, c.err))
		}
	}
	return errors.Join(errs...)
}

// probe asks the server of this config for /healthz on its web listener.
// It returns "" when the config has no web listener.
func (a *app) probe(ctx context.Context) (where string, err error) {
	scheme, listen := "http", a.cfg.Web.HTTPListen
	if a.cfg.Web.Listen != "" {
		scheme, listen = "https", a.cfg.Web.Listen
	}
	if listen == "" {
		return "", nil
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen, err
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	where = scheme + "://" + net.JoinHostPort(host, port) + "/healthz"
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		// This asks the server on this machine whether it is up; whose
		// certificate it shows is not the question.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: a.cfg.Hostname},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, where, nil)
	if err != nil {
		return where, err
	}
	resp, err := client.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return where, errors.New("no server is listening")
		}
		return where, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 100))
	if resp.StatusCode != http.StatusOK {
		return where, fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return where, nil
}

func printChecks(w io.Writer, list []check) (failed int) {
	for _, c := range list {
		if c.err != nil {
			failed++
			fmt.Fprintf(w, "FAIL  %-15s %v\n", c.name, c.err)
		} else {
			fmt.Fprintf(w, "ok    %-15s %s\n", c.name, c.detail)
		}
	}
	return failed
}

// health says whether this node can do its work and whether its server
// answers, one line each. It fails (exit status 1) when something does
// not: for a monitor.
func (a *app) health() error {
	list := a.checks(a.ctx, true)
	c := check{name: "server"}
	where, err := a.probe(a.ctx)
	switch {
	case where == "":
		c.detail = "the config has no web listener to ask"
	case err != nil:
		c.err = fmt.Errorf("%s: %w", where, err)
	default:
		c.detail = "answers on " + where
	}
	list = append(list, c)
	if failed := printChecks(a.stdout, list); failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(list))
	}
	return nil
}

// status is health with numbers: what the server holds, the queue, the
// nodes.
func (a *app) status() error {
	totals, err := a.store.Totals(a.ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s: %d domains, %d accounts, %d aliases, %d emails (%s)\n", a.cfg.Hostname, totals.Domains, totals.Accounts,
		totals.Aliases, totals.Emails, octets(totals.Bytes))
	q, err := a.store.QueueStats(a.ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "queue: %d messages: %d due, %d being sent, %d waiting", q.Active, q.Due, q.Claimed, q.Waiting)
	if !q.Oldest.IsZero() {
		fmt.Fprintf(a.stdout, "; the oldest is %s old", ago(q.Oldest))
	}
	fmt.Fprintln(a.stdout)
	nodes, err := a.store.Nodes(a.ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		seen := "never ran"
		if n.SeenAt.Unix() > 0 {
			seen = "seen " + ago(n.SeenAt) + " ago"
		}
		me := ""
		if n.ID == a.cfg.Node.ID {
			me = "  (this node)"
		}
		fmt.Fprintf(a.stdout, "node %-12s %-28s %s%s\n", n.ID, n.Address, seen, me)
	}
	return a.health()
}

// scrub checks this node's blob files and repairs what it can.
func (a *app) scrub() error {
	res, err := a.store.ScrubBlobs(a.ctx)
	metrics.ScrubChecked.Add(float64(res.Checked))
	metrics.ScrubCorrupt.Add(float64(res.Corrupt))
	metrics.ScrubRepaired.Add(float64(res.Repaired))
	fmt.Fprintf(a.stdout, "%d blob files checked, %d corrupt, %d repaired from another node\n", res.Checked, res.Corrupt, res.Repaired)
	for _, key := range res.Bad {
		fmt.Fprintf(a.stdout, "corrupt: %s\n", key)
	}
	if res.Corrupt > 0 {
		a.note("scrub", "", fmt.Sprintf("%d checked, %d corrupt, %d repaired", res.Checked, res.Corrupt, res.Repaired))
	}
	return err
}

// scrubLoop scrubs this node's blob files once after first and then every
// interval. A corrupt file that another node has a good copy of is
// replaced; one that nobody has is logged as an error every time.
func scrubLoop(ctx context.Context, st *store.Store, log *slog.Logger, first, interval time.Duration) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		start := time.Now()
		res, err := st.ScrubBlobs(ctx)
		metrics.ScrubChecked.Add(float64(res.Checked))
		metrics.ScrubCorrupt.Add(float64(res.Corrupt))
		metrics.ScrubRepaired.Add(float64(res.Repaired))
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.Error("blob scrub failed", "checked", res.Checked, "corrupt", res.Corrupt, "repaired", res.Repaired, "err", err)
		default:
			log.Info("blob scrub", "checked", res.Checked, "corrupt", res.Corrupt, "repaired", res.Repaired, "took", time.Since(start).Round(time.Millisecond))
		}
		t.Reset(interval)
	}
}

// gauges are the numbers of /metrics that are read when asked: from the
// database, at most once in ten seconds.
type gauges struct {
	a       *app
	started time.Time
	mu      sync.Mutex
	at      time.Time
	text    []byte
}

func (g *gauges) write(w io.Writer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.at) < 10*time.Second && g.text != nil {
		w.Write(g.text)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := metrics.New()
	one := func(name, help string, v float64) { r.Gauge(name, help).Set(v) }
	one("robinmail_start_time_seconds", "When this server started, in Unix seconds.", float64(g.started.Unix()))
	one("robinmail_goroutines", "Goroutines of the process.", float64(runtime.NumGoroutine()))
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	one("robinmail_memory_alloc_bytes", "Bytes of heap in use.", float64(mem.HeapAlloc))
	up := 1.0
	if err := g.a.healthy(ctx); err != nil {
		up = 0
	}
	one("robinmail_healthy", "1 when the node can do its work (database, blob directory), as /healthz says.", up)
	if totals, err := g.a.store.Totals(ctx); err == nil {
		one("robinmail_domains", "Domains mail is accepted for.", float64(totals.Domains))
		one("robinmail_accounts", "Accounts.", float64(totals.Accounts))
		one("robinmail_emails", "Emails stored, as the quota counts them.", float64(totals.Emails))
		one("robinmail_email_bytes", "Octets of the emails stored.", float64(totals.Bytes))
	}
	if q, err := g.a.store.QueueStats(ctx); err == nil {
		queue := r.Gauge("robinmail_queue_messages", "Messages in the outbound queue, by state.", "state")
		queue.Set(float64(q.Due), "due")
		queue.Set(float64(q.Claimed), "sending")
		queue.Set(float64(q.Waiting), "waiting")
		age := 0.0
		if !q.Oldest.IsZero() {
			age = time.Since(q.Oldest).Seconds()
		}
		one("robinmail_queue_oldest_seconds", "Age of the oldest message in the outbound queue.", age)
	}
	if nodes, err := g.a.store.Nodes(ctx); err == nil && len(nodes) > 0 {
		seen := 0
		for _, n := range nodes {
			if time.Since(n.SeenAt) < 2*time.Minute {
				seen++
			}
		}
		ng := r.Gauge("robinmail_nodes", "Nodes of the cluster, by whether they were seen in the last two minutes.", "state")
		ng.Set(float64(seen), "seen")
		ng.Set(float64(len(nodes)-seen), "away")
	}
	if g.a.cfg.TLS.CertFile != "" {
		if end, err := certificateExpiry(g.a.cfg.TLS.CertFile); err == nil {
			one("robinmail_certificate_not_after_seconds", "When the certificate of the config ends, in Unix seconds.", float64(end.Unix()))
		}
	}
	var b strings.Builder
	r.WriteText(&b)
	g.text, g.at = []byte(b.String()), time.Now()
	w.Write(g.text)
}

// metricsHandler serves GET /metrics: the counters of the process, then
// the gauges.
func (a *app) metricsHandler() http.Handler {
	g := &gauges{a: a, started: time.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		metrics.Default.WriteText(w)
		g.write(w)
	})
	return mux
}
