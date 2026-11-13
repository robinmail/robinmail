package main

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"time"

	"github.com/mikepage/robinmail/internal/delivery"
	"github.com/mikepage/robinmail/internal/rspamd"
	"github.com/mikepage/robinmail/internal/smtp"
	"github.com/mikepage/robinmail/internal/store"
	"github.com/mikepage/robinmail/internal/tlsconf"
)

// onListen, when set, is told the address the SMTP listener got. Tests use
// it to listen on port 0.
var onListen func(net.Addr)

// How long open SMTP sessions get to finish after a stop signal.
const shutdownGrace = 30 * time.Second

// serve runs the SMTP listener and the daily trim until a.ctx is done, which
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
	case a.cfg.TLS.ACME:
		log.Warn("ACME validates over port 443, which robinmail does not serve yet; STARTTLS fails until a certificate is cached")
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
	case <-a.ctx.Done():
	}
	stop()
	log.Info("shutting down", "grace", shutdownGrace)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("closed sessions that did not finish in time")
	}
	<-trimmed
	return nil
}

// trimLoop removes change log rows and delivery keys past their retention:
// once after first, then every interval, until ctx is done.
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
	log.Info("trimmed", "changes", changes, "delivery_keys", keys)
}
