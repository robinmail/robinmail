package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/robinmail/robinmail/internal/dav"
	"github.com/robinmail/robinmail/internal/delivery"
	"github.com/robinmail/robinmail/internal/imap"
	"github.com/robinmail/robinmail/internal/jmap"
	"github.com/robinmail/robinmail/internal/notify"
	"github.com/robinmail/robinmail/internal/oidc"
	"github.com/robinmail/robinmail/internal/pim"
	"github.com/robinmail/robinmail/internal/queue"
	"github.com/robinmail/robinmail/internal/rspamd"
	"github.com/robinmail/robinmail/internal/smtp"
	"github.com/robinmail/robinmail/internal/srs"
	"github.com/robinmail/robinmail/internal/store"
	"github.com/robinmail/robinmail/internal/tlsconf"
	"github.com/robinmail/robinmail/internal/web"
)

// onListen, when set, is told the address the SMTP listener got. Tests use
// it to listen on port 0. onWebListen is the same for the first web
// listener.
var (
	onListen     func(net.Addr)
	onWebListen  func(net.Addr)
	onNodeListen func(net.Addr)
	// onMetricsListen is told the address of the metrics listener.
	onMetricsListen func(net.Addr)
	// onSubmissionListen is told the STARTTLS and the implicit TLS
	// submission listener, each nil without one.
	onSubmissionListen func(starttls, implicit net.Addr)
	// onIMAPListen is told the STARTTLS and the implicit TLS IMAP
	// listener, each nil without one.
	onIMAPListen func(starttls, implicit net.Addr)
	// queueSetup lets a test give the outbound worker its own DNS, dialer
	// and clock before it starts.
	queueSetup func(*queue.Worker)
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
	defer a.close()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// The names of our domains this server also answers under.
	domainNames := &ownDomains{list: a.store.Domains}
	tc, err := tlsconf.New(a.cfg.TLS, a.cfg.Hostname, a.cfg.DataDir, extraNames(a.cfg, domainNames))
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
	d.Submissions = a.store
	d.Aliases, d.Forwards = a.store, a.store
	// Under the current master key; addresses made under a key that was
	// replaced since are still read.
	srsKeys := a.master.DeriveAll("robinmail srs", 32)
	d.SRS = &srs.Rewriter{Key: srsKeys[0], OldKeys: srsKeys[1:]}
	sender := &delivery.Submitter{
		Hostname:        a.cfg.Hostname,
		Store:           a.store,
		FailOpen:        a.cfg.Rspamd.FailOpen,
		MaxMessageBytes: a.cfg.SMTP.MaxMessageBytes,
		MaxRecipients:   a.cfg.SMTP.MaxRecipients,
		MaxPerHour:      a.cfg.Submission.MaxRecipientsPerHour,
		Hold:            time.Duration(*a.cfg.Submission.UndoSeconds) * time.Second,
		SendAs:          a.store.SendAddresses,
		Logger:          log,
	}
	if a.cfg.Rspamd.URL != "" {
		d.Rspamd = &rspamd.Client{URL: a.cfg.Rspamd.URL, Timeout: time.Duration(a.cfg.Rspamd.TimeoutSeconds) * time.Second}
		sender.Rspamd = d.Rspamd
	} else {
		log.Warn("no rspamd url configured: incoming mail is delivered unscanned, outgoing mail is sent without a DKIM signature")
	}
	// Filters: the Sieve script of an account runs on its mail, after
	// Rspamd. A vacation reply goes out like mail of the account, scanned
	// and signed, with the null reverse-path.
	d.Filters, d.SendAs = a.store, a.store.SendAddresses
	d.AutoReply = func(ctx context.Context, acct store.Account, rcpt string, raw []byte) error {
		_, err := sender.Submit(ctx, delivery.Outgoing{Account: acct, From: "", Rcpts: []string{rcpt}, Raw: raw, Protocol: delivery.ProtocolAuto})
		return err
	}
	// Calendars and contacts: invitations and replies go out like mail
	// of the account, scanned and signed; those that come in are applied
	// at delivery.
	calendars := &pim.Service{Store: a.store, ProdID: "-//robinmail//" + a.cfg.Hostname + "//EN", SendAs: a.store.SendAddresses, Logger: log,
		Send: func(ctx context.Context, acct store.Account, from string, rcpts []string, raw []byte) error {
			_, err := sender.Submit(ctx, delivery.Outgoing{Account: acct, From: from, Rcpts: rcpts, Raw: raw, Protocol: delivery.ProtocolCalendar})
			return err
		}}
	d.Calendar = calendars.IncomingMail
	domains, err := a.store.Domains(a.ctx)
	if err != nil {
		return err
	}
	if len(domains) == 0 {
		log.Warn(`no domains yet, all mail is refused; add one with "robinmail domain add"`)
	}

	// One budget for the memory of messages in transit, shared by the
	// inbound and the submission listeners (issue 43).
	budget := &smtp.Budget{Total: a.cfg.SMTP.MaxBytesInTransit, PerClient: a.cfg.SMTP.MaxBytesInTransitPerIP}
	srv := &smtp.Server{
		Hostname:        a.cfg.Hostname,
		TLSConfig:       tc,
		Handler:         d,
		Logger:          log,
		MaxMessageBytes: a.cfg.SMTP.MaxMessageBytes,
		MaxRecipients:   a.cfg.SMTP.MaxRecipients,
		MaxConnections:  a.cfg.SMTP.MaxConnections,
		MaxConnsPerIP:   a.cfg.SMTP.MaxConnectionsPerIP,
		Budget:          budget,
		Trace:           a.cfg.Dev.SMTPTrace,
	}
	l, err := net.Listen("tcp", a.cfg.SMTP.Listen)
	if err != nil {
		return err
	}
	log.Info("smtp listening", "addr", l.Addr().String(), "starttls", tc != nil, "domains", len(domains))

	// Change notifications: writes through this node's store wake its
	// listeners at once, a poll finds the writes of other nodes.
	hub := &notify.Hub{States: a.store.ModSeqs, Logger: log}
	a.store.OnChange = hub.Changed
	go hub.Run(a.ctx)
	// The same for withdrawn logins: a second hub follows auth_seq, which
	// moves when a session or app password is revoked or a password
	// changes. Open IMAP sessions and event streams listen to it.
	authHub := &notify.Hub{States: a.store.AuthSeqs, Logger: log}
	a.store.OnAuthChange = authHub.Changed
	go authHub.Run(a.ctx)

	// The web side: HTTPS for real use, plain HTTP on localhost for
	// development and tests. Both serve the same handler.
	clients := mailClients(a.cfg, domainNames)
	site := &web.Server{
		Hostname:            a.cfg.Hostname,
		Accounts:            a.store,
		Limits:              a.store,
		Audit:               a.store,
		AuthChanges:         authHub,
		RequireAppPasswords: a.cfg.Auth.RequireAppPasswords,
		DeviceKeys:          a.master.DeriveAll("robinmail web device", 32),
		SendAs:              a.store.SendAddresses,
		Clients:             clients,
		Health:              a.healthy,
		Hosts:               extraHosts(a.cfg, domainNames, clients.AutoconfigHost),
		JMAP: &jmap.Handler{Store: a.store, Hostname: a.cfg.Hostname, Send: jmapSend(sender, a.store),
			SendAs: a.store.SendAddresses, Push: hub, Closing: a.ctx.Done(), PIM: calendars},
		DAV:         &dav.Handler{PIM: calendars, Logger: log},
		WithDAVUser: dav.WithUser,
		Static:      webStatic,
		DevOrigins:  a.cfg.Web.DevOrigins,
		Logger:      log,
		WithUser: func(ctx context.Context, accountID, username string) context.Context {
			return jmap.WithUser(ctx, jmap.User{AccountID: accountID, Username: username})
		},
	}
	for _, c := range a.cfg.Auth.OIDC {
		site.OIDC = append(site.OIDC, &web.OIDCProvider{Name: c.Name, Label: c.Label, Claim: c.Claim,
			AllowUnverified: c.AllowUnverifiedEmail, RequireTOTP: c.RequireTOTP, AutoProvisionDomains: c.AutoProvisionDomains,
			Client: oidc.New(oidc.Config{Issuer: c.Issuer, ClientID: c.ClientID, ClientSecret: c.ClientSecret, Scopes: c.Scopes})})
		log.Info("oidc provider", "name", c.Name, "issuer", c.Issuer, "claim", c.Claim, "auto_provision", len(c.AutoProvisionDomains) > 0)
	}
	var webServers []*http.Server
	webErr := make(chan error, 3)
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
	// Submission: 587 with STARTTLS, 465 with TLS from the start. The same
	// server, so limits on connections hold for both together.
	var submission *smtp.Server
	subErr := make(chan error, 2)
	if a.cfg.Submission.Listen != "" || a.cfg.Submission.ListenTLS != "" {
		if tc == nil {
			return errors.New("submission listeners need a certificate")
		}
		// A session ends when its login is withdrawn, like an IMAP session:
		// a client that keeps the connection open between messages would
		// otherwise go on sending with a revoked app password.
		submit := &delivery.Handler{Submitter: sender,
			LoginSession: func(ctx context.Context, ip, username, password string) (store.Account, int, time.Duration, func() (<-chan struct{}, func())) {
				c, status, wait := site.MailLogin(ctx, ip, store.ScopeSMTP, username, password)
				return c.Account, status, wait, func() (<-chan struct{}, func()) {
					return watchLogin(authHub, site.CredentialValid, c, store.ScopeSMTP)
				}
			}}
		submission = &smtp.Server{
			Hostname:        a.cfg.Hostname,
			TLSConfig:       tc,
			Handler:         submit,
			Auth:            submit,
			Logger:          log,
			MaxMessageBytes: a.cfg.SMTP.MaxMessageBytes,
			MaxRecipients:   a.cfg.SMTP.MaxRecipients,
			MaxConnections:  a.cfg.SMTP.MaxConnections,
			MaxConnsPerIP:   a.cfg.SMTP.MaxConnectionsPerIP,
			Budget:          budget,
			Trace:           a.cfg.Dev.SMTPTrace,
		}
		var plain, implicit net.Addr
		if addr := a.cfg.Submission.Listen; addr != "" {
			sl, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			plain = sl.Addr()
			log.Info("submission listening", "addr", plain.String(), "tls", "starttls")
			go func() { subErr <- submission.Serve(sl) }()
		}
		if addr := a.cfg.Submission.ListenTLS; addr != "" {
			sl, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			implicit = sl.Addr()
			log.Info("submission listening", "addr", implicit.String(), "tls", "implicit")
			go func() { subErr <- submission.Serve(tls.NewListener(sl, tc)) }()
		}
		if onSubmissionListen != nil {
			onSubmissionListen(plain, implicit)
		}
	}

	// IMAP: 993 with TLS from the start, 143 with STARTTLS. One server, so
	// the limits hold for both together.
	var imapServer *imap.Server
	imapErrc := make(chan error, 2)
	if a.cfg.IMAP.Listen != "" || a.cfg.IMAP.ListenTLS != "" {
		if tc == nil {
			return errors.New("imap listeners need a certificate")
		}
		maxMessage := a.cfg.SMTP.MaxMessageBytes
		if maxMessage == 0 {
			maxMessage = 25 << 20
		}
		imapServer = &imap.Server{
			Hostname: a.cfg.Hostname,
			Backend: &imapBackend{st: a.store, hub: hub, auth: authHub, login: site.MailLogin,
				valid: site.CredentialValid, maxMessage: maxMessage},
			TLSConfig:       tc,
			Logger:          log,
			MaxMessageBytes: maxMessage,
			MaxConnections:  a.cfg.IMAP.MaxConnections,
			MaxConnsPerIP:   a.cfg.IMAP.MaxConnectionsPerIP,
			MaxConnsPerUser: a.cfg.IMAP.MaxConnectionsPerUser,
			Trace:           a.cfg.Dev.IMAPTrace,
		}
		var plain, implicit net.Addr
		if addr := a.cfg.IMAP.Listen; addr != "" {
			il, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			plain = il.Addr()
			log.Info("imap listening", "addr", plain.String(), "tls", "starttls")
			go func() { imapErrc <- imapServer.Serve(il) }()
		}
		if addr := a.cfg.IMAP.ListenTLS; addr != "" {
			il, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			implicit = il.Addr()
			log.Info("imap listening", "addr", implicit.String(), "tls", "implicit")
			go func() { imapErrc <- imapServer.Serve(tls.NewListener(il, tc)) }()
		}
		if onIMAPListen != nil {
			onIMAPListen(plain, implicit)
		}
	}

	// The outbound queue. Every node works it; a row belongs to whoever
	// holds its claim.
	worker := &queue.Worker{
		Hostname:       a.cfg.Hostname,
		NodeID:         a.cfg.Node.ID,
		Store:          a.store,
		Local:          d,
		Routes:         a.cfg.Outbound.Routes,
		AllowPrivate:   a.cfg.Outbound.AllowPrivateAddresses,
		Concurrency:    a.cfg.Outbound.Concurrency,
		PerDestination: a.cfg.Outbound.PerDestination,
		Logger:         log,
	}
	if worker.NodeID == "" {
		worker.NodeID = a.cfg.Hostname
	}
	// TLS reporting (RFC 8460): the worker counts how TLS went, mail that
	// is a report for us is kept, and once a day the counts go out.
	worker.TLSResults, d.Reports = a.store, a.store
	if !a.cfg.TLSRPT.Disabled {
		go tlsReportLoop(a.ctx, a.tlsReporter(sender, log), time.Duration(rand.Int64N(int64(10*time.Minute))), time.Hour)
	}
	if s := a.cfg.Dev.RetrySeconds; s > 0 {
		log.Warn("dev.retry_seconds is set: the outbound queue does not follow its retry schedule", "seconds", s)
		worker.Retry = func(int) time.Duration { return time.Duration(s) * time.Second }
	}
	worker.Trace = a.cfg.Dev.SMTPTrace
	if queueSetup != nil {
		queueSetup(worker)
	}
	queued := make(chan struct{})
	go func() {
		defer close(queued)
		worker.Run(a.ctx)
	}()

	// Last, so a test that waits for this has every listener up.
	if onListen != nil {
		onListen(l.Addr())
	}

	// The other nodes: open listens for their blobs and Raft messages
	// already; here the heartbeat in the node table, and the pass that
	// fetches what this disk lacks.
	upkeep := make(chan struct{})
	if a.node == nil {
		close(upkeep)
	} else {
		a.node.Logger, a.replicated.Logger = log, log
		members, _ := a.node.Members(a.ctx)
		log.Info("node listening", "id", a.node.ID, "addr", a.peerAddr.String(), "address", a.node.Address,
			"cert", a.node.Identity.Fingerprint[:16], "nodes", members)
		if onNodeListen != nil {
			onNodeListen(a.peerAddr)
		}
		interval := time.Duration(a.cfg.Node.RepairIntervalSeconds) * time.Second
		if interval == 0 {
			interval = 5 * time.Minute
		}
		go func() {
			defer close(upkeep)
			go a.node.Run(a.ctx)
			repairLoop(a.ctx, a.store, log, interval)
		}()
	}
	if line, err := a.clusterLine(a.ctx); err == nil {
		log.Info("database", "cluster", line)
	}

	// The metrics, for a collector on this machine.
	if a.cfg.Metrics.Listen != "" {
		ml, err := net.Listen("tcp", a.cfg.Metrics.Listen)
		if err != nil {
			return errors.New("metrics: " + err.Error())
		}
		metricsServer := &http.Server{Handler: a.metricsHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
		log.Info("metrics listening", "addr", ml.Addr().String())
		if onMetricsListen != nil {
			onMetricsListen(ml.Addr())
		}
		go metricsServer.Serve(ml)
		go func() {
			<-a.ctx.Done()
			metricsServer.Close()
		}()
	}

	// Every node scrubs its own disk, a week apart by default, the first
	// time within the first hour.
	if !a.cfg.Scrub.Disabled {
		go scrubLoop(a.ctx, a.store, log, rand.N(time.Hour), time.Duration(a.cfg.Scrub.IntervalHours)*time.Hour)
	}

	// Every node trims. The first run comes at a random moment so the nodes
	// of a cluster that start together do not all trim at once; it would be
	// harmless (see trimLoop), only wasteful.
	trimmed := make(chan struct{})
	go func() {
		defer close(trimmed)
		trimLoop(a.ctx, a.store, log, rand.N(10*time.Minute), 24*time.Hour, a.auditRetention())
	}()

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case err := <-webErr:
		return err
	case err := <-subErr:
		return err
	case err := <-imapErrc:
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
	if submission != nil {
		if err := submission.Shutdown(sctx); err != nil {
			log.Warn("closed submission sessions that did not finish in time")
		}
	}
	if imapServer != nil {
		if err := imapServer.Shutdown(sctx); err != nil {
			log.Warn("closed imap sessions that did not finish in time")
		}
	}
	// An attempt that is under way is cut off; its claim runs out and the
	// message is tried again, here after a restart or on another node.
	<-queued
	<-webDone
	<-trimmed
	<-upkeep
	if a.replicated != nil {
		// Last: until here a delivery that was finishing could still push.
		a.replicated.Wait()
	}
	a.close()
	return nil
}

// repairLoop is the anti-entropy pass: at start and then every interval it
// fetches, from the other nodes, the blobs the database refers to and this
// disk lacks. That is how a new node fills up, how a node that was away
// catches up, and what covers a push that did not arrive.
func repairLoop(ctx context.Context, st *store.Store, log *slog.Logger, interval time.Duration) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		res, err := st.RepairBlobs(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.Error("blob repair failed", "missing", res.Missing, "fetched", res.Fetched, "err", err)
		case res.Missing > 0:
			log.Info("blob repair", "missing", res.Missing, "fetched", res.Fetched)
		}
		t.Reset(interval)
	}
}

// trimLoop removes change log rows, delivery keys and web sessions past
// their time and blob files nothing refers to any more: once after first,
// then every interval, until ctx is done.
//
// Nobody picks which node trims: all of them do. Each trim is one atomic
// batch that deletes whatever is older than the cut and records, per
// account, the newest state it removed. Two nodes trimming at the same
// moment are ordered by the Raft log; the second finds
// nothing left. A node that dies mid-trim leaves either the whole batch or
// none of it.
func trimLoop(ctx context.Context, st *store.Store, log *slog.Logger, first, interval, auditRetention time.Duration) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		trimOnce(ctx, st, log, auditRetention)
		t.Reset(interval)
	}
}

func trimOnce(ctx context.Context, st *store.Store, log *slog.Logger, auditRetention time.Duration) {
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
	if _, err := st.TrimLoginFailures(ctx, now.Add(-web.LoginWindow)); err != nil {
		log.Error("trimming login failures failed", "err", err)
		return
	}
	if _, err := st.TrimAuthEvents(ctx, now.Add(-auditRetention)); err != nil {
		log.Error("trimming auth events failed", "err", err)
		return
	}
	uploads, err := st.TrimUploads(ctx, now.Add(-store.UploadRetention))
	if err != nil {
		log.Error("trimming uploads failed", "err", err)
		return
	}
	subs, err := st.TrimSubmissions(ctx, now.Add(-store.SubmissionRetention))
	if err != nil {
		log.Error("trimming the queue failed", "err", err)
		return
	}
	if err := st.TrimTLSReports(ctx, now.Add(-store.TLSResultRetention), now.Add(-store.TLSReportRetention)); err != nil {
		log.Error("trimming TLS reports failed", "err", err)
		return
	}
	if _, err := st.TrimSTSPolicies(ctx, now.Add(-24*time.Hour)); err != nil {
		log.Error("trimming MTA-STS policies failed", "err", err)
		return
	}
	// Removed calendar objects and cards, as long as the change log.
	if _, err := st.TrimTombstones(ctx, now.Add(-store.ChangesRetention)); err != nil {
		log.Error("trimming removed calendar objects failed", "err", err)
		return
	}
	// Longer than the longest period of "once per sender".
	if _, err := st.TrimVacationSent(ctx, now.Add(-store.VacationSentRetention)); err != nil {
		log.Error("trimming the record of automatic replies failed", "err", err)
		return
	}
	log.Info("trimmed", "changes", changes, "delivery_keys", keys, "sessions", sessions, "uploads", uploads, "submissions", subs)
	// The blobs are this node's own: every node sweeps its disk.
	swept, err := st.SweepBlobs(ctx, now.Add(-store.BlobGrace))
	if err != nil {
		log.Error("sweeping blobs failed", "err", err)
		return
	}
	log.Info("swept blobs", "removed", swept.Removed, "restored", swept.Restored, "temp", swept.Temp)
}

// jmapSend is how EmailSubmission/set reaches the Submitter: it turns a
// refusal into the SetError RFC 8621 7.5 has for it.
func jmapSend(sender *delivery.Submitter, st *store.Store) func(context.Context, jmap.User, jmap.Outgoing) (store.Submission, *jmap.SendError, error) {
	return func(ctx context.Context, u jmap.User, o jmap.Outgoing) (store.Submission, *jmap.SendError, error) {
		acct, err := st.AccountByID(ctx, u.AccountID)
		if err != nil {
			return store.Submission{}, nil, err
		}
		sub, err := sender.Submit(ctx, delivery.Outgoing{Account: acct, From: o.MailFrom, Rcpts: o.RcptTo, Raw: o.Raw,
			EmailID: o.EmailID, IdentityID: o.IdentityID, IfInState: o.IfInState, Protocol: "JMAP"})
		var r *delivery.Refusal
		if !errors.As(err, &r) {
			return sub, nil, err
		}
		se := &jmap.SendError{Type: r.Kind, Description: r.Msg, InvalidRecipients: r.Recipients}
		switch r.Kind {
		case delivery.RefuseTooMany:
			se.MaxRecipients = sender.MaxRecipients
			if se.MaxRecipients == 0 {
				se.MaxRecipients = 100
			}
		case delivery.RefuseTooLarge:
			se.MaxSize = sender.MaxMessageBytes
			if se.MaxSize == 0 {
				se.MaxSize = 25 << 20
			}
		case delivery.RefuseRateLimit, delivery.RefuseLater:
			// RFC 8621 has no "try later" for a submission; forbiddenToSend
			// with the reason is the nearest it has.
			se.Type = "forbiddenToSend"
		}
		return sub, se, nil
	}
}

// auditRetention is how long auth events are kept.
func (a *app) auditRetention() time.Duration {
	return time.Duration(a.cfg.Auth.AuditRetentionDays) * 24 * time.Hour
}

// tlsReporter is the sender of this node's daily TLS reports. They go out
// in the name of tls_rpt.from, or without that of postmaster at the first
// domain the server has; a server without a domain sends none.
func (a *app) tlsReporter(sender *delivery.Submitter, log *slog.Logger) *tlsReporter {
	from := func(ctx context.Context) string {
		if a.cfg.TLSRPT.From != "" {
			return a.cfg.TLSRPT.From
		}
		domains, err := a.store.Domains(ctx)
		if err != nil || len(domains) == 0 {
			return ""
		}
		return "postmaster@" + domains[0]
	}
	organization := a.cfg.TLSRPT.Organization
	if organization == "" {
		organization = a.cfg.Hostname
	}
	return &tlsReporter{
		st: a.store, host: a.cfg.Hostname, from: from, organization: organization,
		lookupTXT: net.DefaultResolver.LookupTXT,
		mail: func(ctx context.Context, key, from, rcpt string, raw []byte) error {
			return sender.Report(ctx, a.store, key, from, rcpt, raw)
		},
		// No redirects: the report goes where the record says or nowhere.
		// And where the record says is the other domain's choice: not to
		// an address on our own network (queue.OutboundHTTP).
		post: queue.OutboundHTTP(a.cfg.Outbound.AllowPrivateAddresses),
		now:  time.Now, log: log,
	}
}

// tlsReportLoop makes the reports that are due: once after first, then
// every interval. Days that are over are looked at each time, so the
// reports of a day go out within the interval after it ends, on whichever
// node comes first.
func tlsReportLoop(ctx context.Context, r *tlsReporter, first, interval time.Duration) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		if _, err := r.run(rctx); err != nil {
			r.log.Warn("tls reports: not all could be made", "err", err)
		}
		cancel()
		t.Reset(interval)
	}
}
