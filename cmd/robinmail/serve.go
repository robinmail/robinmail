package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/mikepage/robinmail/internal/delivery"
	"github.com/mikepage/robinmail/internal/jmap"
	"github.com/mikepage/robinmail/internal/rspamd"
	"github.com/mikepage/robinmail/internal/smtp"
	"github.com/mikepage/robinmail/internal/store"
	"github.com/mikepage/robinmail/internal/tlsconf"
	"github.com/mikepage/robinmail/internal/web"
)

// onListen, when set, is told the address the SMTP listener got. Tests use
// it to listen on port 0. onWebListen is the same for the first web
// listener.
var (
	onListen    func(net.Addr)
	onWebListen func(net.Addr)
)

// webStatic is the webmail served outside the API paths: the one built
// into the binary with the "webmail" tag ("make"), or nil, which is the
// placeholder page.
var webStatic fs.FS = web.Embedded()

// How long open SMTP sessions get to finish after a stop signal.
const shutdownGrace = 30 * time.Second

// serve runs the SMTP listener, the web listeners and the daily upkeep until a.ctx is done, which
// happens on SIGINT or SIGTERM. stop gives the signals their default action
// back, so a second one ends the process without waiting.
func (a *app) serve(stop func()) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	tc, err := tlsconf.New(a.cfg.TLS, a.cfg.Hostname, a.cfg.DataDir)
	switch {
	case errors.Is(err, tlsconf.ErrNotConfigured):
		log.Warn("no certificate configured, STARTTLS is off")
	case err != nil:
		return err
	case a.cfg.TLS.ACME && a.cfg.Web.Listen == "":
		log.Warn("ACME validates over port 443, but web listen is not set; TLS fails until a certificate is cached")
	}

	d := &delivery.Deliverer{
		Hostname: a.cfg.Hostname,
		Store:    a.store,
		FailOpen: a.cfg.Rspamd.FailOpen,
		Logger:   log,
	}
	if a.cfg.Rspamd.URL != "" {
		d.Rspamd = &rspamd.Client{URL: a.cfg.Rspamd.URL, Timeout: time.Duration(a.cfg.Rspamd.TimeoutSeconds) * time.Second}
	} else {
		log.Warn("no rspamd url configured, mail is delivered unscanned")
	}
	domains, err := a.store.Domains(a.ctx)
	if err != nil {
		return err
	}
	if len(domains) == 0 {
		log.Warn(`no domains yet, all mail is refused; add one with "robinmail domain add"`)
	}

	srv := &smtp.Server{
		Hostname:        a.cfg.Hostname,
		TLSConfig:       tc,
		Handler:         d,
		Logger:          log,
		MaxMessageBytes: a.cfg.SMTP.MaxMessageBytes,
		MaxRecipients:   a.cfg.SMTP.MaxRecipients,
		MaxConnections:  a.cfg.SMTP.MaxConnections,
		MaxConnsPerIP:   a.cfg.SMTP.MaxConnectionsPerIP,
	}
	l, err := net.Listen("tcp", a.cfg.SMTP.Listen)
	if err != nil {
		return err
	}
	log.Info("smtp listening", "addr", l.Addr().String(), "starttls", tc != nil, "domains", len(domains))

	// The web side: HTTPS for real use, plain HTTP on localhost for
	// development and tests. Both serve the same handler.
	site := &web.Server{
		Hostname:   a.cfg.Hostname,
		Accounts:   a.store,
		JMAP:       &jmap.Handler{Store: a.store},
		Static:     webStatic,
		DevOrigins: a.cfg.Web.DevOrigins,
		Logger:     log,
		WithUser: func(ctx context.Context, accountID, username string) context.Context {
			return jmap.WithUser(ctx, jmap.User{AccountID: accountID, Username: username})
		},
	}
	var webServers []*http.Server
	webErr := make(chan error, 2)
	if a.cfg.Web.Listen != "" {
		if tc == nil {
			return errors.New("web listen needs a certificate")
		}
		wl, err := net.Listen("tcp", a.cfg.Web.Listen)
		if err != nil {
			return err
		}
		hs := site.HTTPServer()
		hs.TLSConfig = tc.Clone()
		webServers = append(webServers, hs)
		log.Info("https listening", "addr", wl.Addr().String())
		if onWebListen != nil {
			onWebListen(wl.Addr())
		}
		go func() { webErr <- hs.ServeTLS(wl, "", "") }()
	}
	if a.cfg.Web.HTTPListen != "" {
		wl, err := net.Listen("tcp", a.cfg.Web.HTTPListen)
		if err != nil {
			return err
		}
		hs := site.HTTPServer()
		webServers = append(webServers, hs)
		log.Warn("plain http listening, for development only", "addr", wl.Addr().String())
		if onWebListen != nil && a.cfg.Web.Listen == "" {
			onWebListen(wl.Addr())
		}
		go func() { webErr <- hs.Serve(wl) }()
	}
	if len(webServers) == 0 {
		log.Warn("no web listener configured, JMAP and the webmail are off")
	}
	// Last, so a test that waits for this has every listener up.
	if onListen != nil {
		onListen(l.Addr())
	}

	// Every node trims. The first run comes at a random moment so the nodes
	// of a cluster that start together do not all trim at once; it would be
	// harmless (see trimLoop), only wasteful.
	trimmed := make(chan struct{})
	go func() {
		defer close(trimmed)
		trimLoop(a.ctx, a.store, log, rand.N(10*time.Minute), 24*time.Hour)
	}()

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case err := <-webErr:
		return err
	case <-a.ctx.Done():
	}
	stop()
	log.Info("shutting down", "grace", shutdownGrace)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	// Requests in flight finish; new ones are refused.
	webDone := make(chan struct{})
	go func() {
		defer close(webDone)
		for _, hs := range webServers {
			if err := hs.Shutdown(sctx); err != nil {
				hs.Close()
			}
		}
	}()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("closed sessions that did not finish in time")
	}
	<-webDone
	<-trimmed
	return nil
}

// trimLoop removes change log rows, delivery keys and web sessions past
// their time and blob files nothing refers to any more: once after first,
// then every interval, until ctx is done.
//
// Nobody picks which node trims: all of them do. Each trim is one atomic
// batch in rqlite that deletes whatever is older than the cut and records,
// per account, the newest state it removed. Two nodes trimming at the same
// moment run one after the other on the rqlite leader; the second finds
// nothing left. A node that dies mid-trim leaves either the whole batch or
// none of it.
func trimLoop(ctx context.Context, st *store.Store, log *slog.Logger, first, interval time.Duration) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		trimOnce(ctx, st, log)
		t.Reset(interval)
	}
}

func trimOnce(ctx context.Context, st *store.Store, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	now := time.Now()
	changes, err := st.TrimChanges(ctx, now.Add(-store.ChangesRetention))
	if err != nil {
		log.Error("trimming the change log failed", "err", err)
		return
	}
	keys, err := st.TrimDelivered(ctx, now.Add(-store.DeliveredRetention))
	if err != nil {
		log.Error("trimming delivery keys failed", "err", err)
		return
	}
	sessions, err := st.TrimSessions(ctx, now)
	if err != nil {
		log.Error("trimming sessions failed", "err", err)
		return
	}
	log.Info("trimmed", "changes", changes, "delivery_keys", keys, "sessions", sessions)
	// The blobs are this node's own: every node sweeps its disk.
	swept, err := st.SweepBlobs(ctx, now.Add(-store.BlobGrace))
	if err != nil {
		log.Error("sweeping blobs failed", "err", err)
		return
	}
	log.Info("swept blobs", "removed", swept.Removed, "restored", swept.Restored, "temp", swept.Temp)
}
