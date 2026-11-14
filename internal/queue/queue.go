// Package queue sends the mail in the submission table: it finds where a
// recipient's mail goes (MX records, RFC 5321 section 5), connects, speaks
// SMTP through the client of internal/smtp, keeps what cannot be delivered
// yet for a later try, and tells the sender what could not be delivered at
// all (RFC 3464).
//
// Every node of a cluster runs a Worker on the same table. A row is worked
// on by the node that holds its claim, a guarded update in rqlite that
// lasts two minutes and is renewed while the work goes on. A node that
// dies leaves a claim that runs out, and another node takes the row.
//
// What that guarantees: a message is never lost, and it is delivered at
// least once. It can be delivered twice when a node dies after the remote
// server said 250 and before that was written down. SMTP has no way around
// that; a receiving robinmail recognises the second copy (its delivery key)
// and stores nothing.
package queue

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mikepage/robinmail/internal/message"
	"github.com/mikepage/robinmail/internal/rqlite"
	"github.com/mikepage/robinmail/internal/smtp"
	"github.com/mikepage/robinmail/internal/store"
)

// Store is what a Worker needs from the store. *store.Store has it.
type Store interface {
	DueSubmissions(ctx context.Context, now time.Time, limit int, domains []string) ([]store.SubmissionRef, error)
	ClaimSubmission(ctx context.Context, ref store.SubmissionRef, token string, now, until time.Time) (store.Submission, error)
	RenewSubmission(ctx context.Context, ref store.SubmissionRef, token string, until time.Time) error
	SaveSubmission(ctx context.Context, sub store.Submission, token string, release bool, nextTry time.Time) error
	Submission(ctx context.Context, account string, id int64) (store.Submission, error)
	SubmissionMessage(ctx context.Context, sub store.Submission) ([]byte, error)
	Deliver(ctx context.Context, account string, m store.Message) (store.Email, error)
	STSPolicy(ctx context.Context, domain string) (store.STSPolicy, bool, error)
	SaveSTSPolicy(ctx context.Context, p store.STSPolicy) error
}

// Local delivers to recipients at our own domains. *delivery.Deliverer
// has it.
type Local interface {
	LocalDomain(ctx context.Context, domain string) (bool, error)
	// Local answers with an *smtp.Error when the recipient cannot have
	// the message.
	Local(ctx context.Context, from, to string, msg []byte) error
}

// Worker sends queued mail. Fill the exported fields, then call Run, or
// RunOnce from a test. Zero values get the defaults in brackets.
type Worker struct {
	// Hostname is our name: in EHLO, in bounces, and what we look for in
	// MX records to avoid sending to ourselves.
	Hostname string
	// NodeID names this node in claims.
	NodeID string
	Store  Store
	Local  Local
	// Resolver is the DNS [net.DefaultResolver].
	Resolver Resolver
	// Dial connects to a mail server [a net.Dialer with a 30 s timeout].
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// HTTP fetches MTA-STS policies [a client that follows no redirects].
	HTTP *http.Client
	// RootCAs are the roots a certificate must chain to where MTA-STS
	// demands a valid one [the system's].
	RootCAs *x509.CertPool
	// Routes sends mail for a domain to a fixed "host:port" instead of
	// its MX hosts. "*" is every domain without a route of its own. For
	// development, and for a relay host.
	Routes map[string]string
	// Port is the port mail servers listen on ["25"].
	Port string
	// Concurrency is how many messages are worked on at once [8];
	// PerDestination how many connections one recipient domain gets [2].
	Concurrency    int
	PerDestination int
	// Domains limits the worker to senders in these domains. Not for a
	// cluster: for tests that share one database.
	Domains  []string
	Timeouts smtp.Timeouts
	// Interval is how often Run looks for work [5 s]. ClaimFor is how
	// long a claim lasts without renewal [2 min]. MaxAge is how long a
	// message is tried before it is returned [5 days].
	Interval time.Duration
	ClaimFor time.Duration
	MaxAge   time.Duration
	Now      func() time.Time
	// Retry is how long a message waits after its n-th try [Backoff].
	Retry func(n int) time.Duration
	// Trace logs every command and reply of the SMTP client.
	Trace  bool
	Logger *slog.Logger

	once sync.Once
	mu   sync.Mutex
	dest map[string]chan struct{} // recipient domain -> connection slots
	idle map[string][]*pooled     // recipient domain -> open connections
}

// pooled is a connection kept open for the next message to the same
// domain.
type pooled struct {
	c      *smtp.Client
	host   string
	secure bool // TLS with a certificate that was verified for host
	since  time.Time
}

const (
	// idleFor is how long an unused connection is kept. Servers close
	// idle connections after a few minutes at the earliest (RFC 5321
	// 4.5.3.2.7 asks them for five).
	idleFor = 30 * time.Second
	// maxPerConnection is how many messages go over one connection.
	maxPerConnection = 100
	// maxDials bounds the connection attempts for one domain in one try.
	maxDials = 8
	dueLimit = 200
)

func (w *Worker) init() {
	w.once.Do(func() {
		if w.Port == "" {
			w.Port = "25"
		}
		if w.Concurrency <= 0 {
			w.Concurrency = 8
		}
		if w.PerDestination <= 0 {
			w.PerDestination = 2
		}
		if w.Interval <= 0 {
			w.Interval = 5 * time.Second
		}
		if w.ClaimFor <= 0 {
			w.ClaimFor = 2 * time.Minute
		}
		if w.MaxAge <= 0 {
			w.MaxAge = 5 * 24 * time.Hour
		}
		w.dest = map[string]chan struct{}{}
		w.idle = map[string][]*pooled{}
	})
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) retry(n int) time.Duration {
	if w.Retry != nil {
		return w.Retry(n)
	}
	return Backoff(n)
}

func (w *Worker) log() *slog.Logger {
	if w.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return w.Logger
}

func (w *Worker) resolver() Resolver {
	if w.Resolver != nil {
		return w.Resolver
	}
	return net.DefaultResolver
}

func (w *Worker) dial(ctx context.Context, address string) (net.Conn, error) {
	if w.Dial != nil {
		return w.Dial(ctx, "tcp", address)
	}
	d := net.Dialer{Timeout: 30 * time.Second}
	return d.DialContext(ctx, "tcp", address)
}

// Backoff is how long a message waits after its n-th try (n from 1): five
// minutes, then twice as long each time, up to four hours. With MaxAge of
// five days that is some 35 tries.
func Backoff(n int) time.Duration {
	d := 5 * time.Minute
	for i := 1; i < n && d < 4*time.Hour; i++ {
		d *= 2
	}
	return min(d, 4*time.Hour)
}

// Run works the queue until ctx is done, then closes idle connections.
func (w *Worker) Run(ctx context.Context) {
	w.init()
	t := time.NewTimer(0)
	defer t.Stop()
	defer w.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.log().Error("queue: looking for work failed", "err", err)
		}
		w.closeIdle(false)
		t.Reset(w.Interval)
	}
}

// RunOnce works on everything that is due now and returns how many
// messages it had a claim on. It returns when those are done.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	w.init()
	total := 0
	for ctx.Err() == nil {
		refs, err := w.Store.DueSubmissions(ctx, w.now(), dueLimit, w.Domains)
		if err != nil {
			return total, err
		}
		var wg sync.WaitGroup
		var claimed sync.Map
		slots := make(chan struct{}, w.Concurrency)
		for _, ref := range refs {
			slots <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-slots; wg.Done() }()
				if w.work(ctx, ref) {
					claimed.Store(ref, true)
				}
			}()
		}
		wg.Wait()
		n := 0
		claimed.Range(func(_, _ any) bool { n++; return true })
		total += n
		if n == 0 {
			break
		}
	}
	return total, ctx.Err()
}

// Close closes the connections kept open for reuse.
func (w *Worker) Close() {
	w.init()
	w.closeIdle(true)
}

func (w *Worker) closeIdle(all bool) {
	w.mu.Lock()
	var stale []*pooled
	for domain, list := range w.idle {
		keep := list[:0]
		for _, p := range list {
			if all || time.Since(p.since) > idleFor {
				stale = append(stale, p)
			} else {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			delete(w.idle, domain)
		} else {
			w.idle[domain] = keep
		}
	}
	w.mu.Unlock()
	for _, p := range stale {
		p.c.Quit()
	}
}

func newToken(node string) string {
	var r [8]byte
	rand.Read(r[:])
	return node + ":" + hex.EncodeToString(r[:])
}

// work claims one row and makes one delivery attempt for every recipient
// that is still queued. It reports whether it got the claim.
func (w *Worker) work(ctx context.Context, ref store.SubmissionRef) bool {
	token := newToken(w.NodeID)
	sub, err := w.Store.ClaimSubmission(ctx, ref, token, w.now(), w.now().Add(w.ClaimFor))
	if err != nil {
		if !errors.Is(err, store.ErrNotClaimed) && ctx.Err() == nil {
			// Also after an unknown outcome: if the claim was written it
			// runs out by itself.
			w.log().Warn("queue: claiming failed", "submission", store.SubmissionID(ref.ID), "err", err)
		}
		return false
	}
	log := w.log().With("submission", store.SubmissionID(sub.ID), "account", sub.Account, "try", sub.Tries)

	// The claim is renewed while the attempt runs. If it is lost, the
	// attempt stops: the row is someone else's now.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		t := time.NewTicker(w.ClaimFor / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				err := w.Store.RenewSubmission(ctx, ref, token, w.now().Add(w.ClaimFor))
				if errors.Is(err, store.ErrNotClaimed) {
					log.Warn("queue: lost the claim, stopping")
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-renewed }()

	msg, err := w.Store.SubmissionMessage(ctx, sub)
	if err != nil {
		// The blob may be on another node that is away: the row is tried
		// again later, here or elsewhere. Only when its time is up is the
		// message given up, like any other that could not be delivered.
		log.Error("queue: reading the message failed", "err", err)
		if w.now().Sub(sub.CreatedAt) < w.MaxAge {
			w.release(ctx, sub, token, log)
			return true
		}
		for i, r := range sub.Recipients {
			if r.State == store.RcptQueued {
				w.set(&sub, i, outcome{reply: "the server could not read the message from its own store", status: "4.3.0"})
			}
		}
	}

	// Recipients by where they go: one connection and one transaction per
	// domain.
	groups := map[string][]int{}
	var order []string
	before := map[int]string{} // the states this attempt started from
	for i, r := range sub.Recipients {
		before[i] = r.State
	}
	for i, r := range sub.Recipients {
		if r.State != store.RcptQueued {
			continue
		}
		domain, ok := asciiDomain(r.Address[strings.LastIndexByte(r.Address, '@')+1:])
		if !ok {
			w.set(&sub, i, outcome{state: store.RcptFailed, status: "5.1.2", reply: "the domain of the address is not one mail can be sent to"})
			continue
		}
		if _, seen := groups[domain]; !seen {
			order = append(order, domain)
		}
		groups[domain] = append(groups[domain], i)
	}
	sort.Strings(order)
	for _, domain := range order {
		if ctx.Err() != nil {
			return true // shutting down, or the claim is gone
		}
		if msg == nil {
			break // unreadable and out of time: see above
		}
		idx := groups[domain]
		rcpts := make([]string, len(idx))
		for k, i := range idx {
			rcpts[k] = sub.Recipients[i].Address
		}
		var results []outcome
		if local, err := w.Local.LocalDomain(ctx, domain); err != nil {
			results = sameOutcome(len(idx), outcome{status: "4.3.0", reply: "the server could not look up its own domains"})
		} else if local {
			results = w.deliverLocal(ctx, sub.MailFrom, rcpts, msg)
		} else {
			results = w.deliverDomain(ctx, domain, sub.MailFrom, rcpts, msg, log)
		}
		if ctx.Err() != nil {
			return true
		}
		for k, i := range idx {
			w.set(&sub, i, results[k])
		}
		// What is known is written down before the next domain is tried.
		if len(order) > 1 {
			if err := w.Store.SaveSubmission(ctx, sub, token, false, time.Time{}); errors.Is(err, store.ErrNotClaimed) {
				return true
			}
		}
	}

	// A message that has waited long enough is given up.
	if w.now().Sub(sub.CreatedAt) >= w.MaxAge {
		for i, r := range sub.Recipients {
			if r.State == store.RcptQueued {
				reply := "delivery was tried for " + w.MaxAge.String() + " without success"
				if r.Reply != "" {
					reply += "; the last attempt: " + r.Reply
				}
				w.set(&sub, i, outcome{state: store.RcptFailed, status: "4.4.7", reply: reply, mta: r.MTA})
			}
		}
	}

	var failed []store.Recipient
	delivered, waiting := 0, 0
	for i, r := range sub.Recipients {
		switch {
		case r.State == store.RcptFailed && before[i] == store.RcptQueued:
			failed = append(failed, r)
		case r.State == store.RcptDelivered && before[i] == store.RcptQueued:
			delivered++
		case r.State == store.RcptQueued:
			waiting++
		}
	}
	if len(failed) > 0 {
		if err := w.bounce(ctx, sub, msg, failed); err != nil {
			// Without the notification the failure must not become
			// final: leave the row as it was, to be tried again.
			log.Error("queue: storing the bounce failed", "err", err)
			w.release(ctx, sub, token, log)
			return true
		}
	}
	next := w.now().Add(w.retry(sub.Tries))
	log.Info("queue: attempt done", "delivered", delivered, "failed", len(failed), "waiting", waiting, "next_try", next.Format(time.RFC3339))
	w.save(ctx, sub, token, next, log)
	return true
}

// save writes the result of an attempt and ends the claim. When rqlite
// cannot say whether the write was applied, the row tells: without our
// claim on it, it was.
func (w *Worker) save(ctx context.Context, sub store.Submission, token string, next time.Time, log *slog.Logger) {
	// The result is written even when the worker is being stopped.
	ctx = context.WithoutCancel(ctx)
	for attempt := 0; attempt < 5; attempt++ {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := w.Store.SaveSubmission(sctx, sub, token, true, next)
		cancel()
		switch {
		case err == nil, errors.Is(err, store.ErrNotClaimed):
			return
		case errors.Is(err, rqlite.ErrUnknownOutcome):
			if now, rerr := w.Store.Submission(ctx, sub.Account, sub.ID); rerr == nil && now.ClaimedBy != token ||
				errors.Is(rerr, store.ErrNoSubmission) {
				return
			}
		}
		log.Warn("queue: writing the result failed, trying again", "err", err)
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	log.Error("queue: the result of an attempt could not be written; the message is tried again when the claim runs out")
}

// release gives a row back without a result: it is due again at once for
// whoever claims it after a short wait.
func (w *Worker) release(ctx context.Context, sub store.Submission, token string, log *slog.Logger) {
	fresh, err := w.Store.Submission(context.WithoutCancel(ctx), sub.Account, sub.ID)
	if err != nil {
		return
	}
	w.save(ctx, fresh, token, w.now().Add(w.retry(1)), log)
}

// outcome is what an attempt found for one recipient. An empty state
// leaves the recipient queued.
type outcome struct {
	state  string
	reply  string
	status string
	mta    string
	// mailbox: delivered here, into the recipient's mailbox.
	mailbox bool
}

func sameOutcome(n int, o outcome) []outcome {
	out := make([]outcome, n)
	for i := range out {
		out[i] = o
	}
	return out
}

func (w *Worker) set(sub *store.Submission, i int, o outcome) {
	r := &sub.Recipients[i]
	if o.state != "" {
		r.State = o.state
	}
	// What robinmail found out by itself reads like a reply too, with a
	// code, so every reason has one form: in deliveryStatus (RFC 8621 7,
	// smtpReply) and in the Diagnostic-Code of a bounce.
	if o.reply != "" {
		o.reply = smtp.StatusReply(o.status, o.reply)
	}
	r.Reply, r.Status, r.MTA, r.At, r.Mailbox = o.reply, o.status, o.mta, w.now().Unix(), o.mailbox
}

// fromFailure turns "no server said anything" into an outcome.
func fromFailure(f *failure) outcome {
	o := outcome{reply: f.msg, status: f.status}
	if f.permanent {
		o.state = store.RcptFailed
	}
	return o
}

// fromReply turns an SMTP reply into an outcome. The status is the
// server's enhanced code if it gave one, otherwise the class alone (RFC
// 3463 2: X.0.0, "other undefined status").
func fromReply(rep smtp.Reply, mta string) outcome {
	o := outcome{reply: rep.String(), status: rep.Enhanced}
	if !rep.Local {
		o.mta = mta
	}
	switch {
	case rep.Positive():
		o.state = store.RcptDelivered
	case rep.Permanent():
		o.state = store.RcptFailed
	}
	if o.status == "" {
		o.status = string(rune('0'+rep.Code/100)) + ".0.0"
	}
	return o
}

func (w *Worker) deliverLocal(ctx context.Context, from string, rcpts []string, msg []byte) []outcome {
	out := make([]outcome, len(rcpts))
	for i, r := range rcpts {
		err := w.Local.Local(ctx, from, r, msg)
		var se *smtp.Error
		switch {
		case err == nil:
			out[i] = outcome{state: store.RcptDelivered, reply: "250 2.0.0 delivered to the mailbox", status: "2.0.0", mta: w.Hostname, mailbox: true}
		case errors.As(err, &se):
			out[i] = fromReply(smtp.Reply{Code: se.Code, Enhanced: se.Enhanced, Lines: []string{se.Enhanced + " " + se.Msg}}, w.Hostname)
		default:
			out[i] = outcome{reply: "local delivery failed", status: "4.3.0"}
		}
	}
	return out
}

// slot takes one of the connection slots of a domain and returns the
// function that gives it back.
func (w *Worker) slot(ctx context.Context, domain string) (func(), bool) {
	w.mu.Lock()
	ch, ok := w.dest[domain]
	if !ok {
		ch = make(chan struct{}, w.PerDestination)
		w.dest[domain] = ch
	}
	w.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	case <-ctx.Done():
		return nil, false
	}
}

func (w *Worker) takeIdle(domain string, needSecure bool) *pooled {
	w.mu.Lock()
	defer w.mu.Unlock()
	list := w.idle[domain]
	for i := len(list) - 1; i >= 0; i-- {
		p := list[i]
		if needSecure && !p.secure {
			continue
		}
		w.idle[domain] = append(list[:i], list[i+1:]...)
		return p
	}
	return nil
}

func (w *Worker) putIdle(domain string, p *pooled) {
	if p.c.Broken() || p.c.Sent >= maxPerConnection {
		p.c.Quit()
		return
	}
	p.since = time.Now()
	w.mu.Lock()
	w.idle[domain] = append(w.idle[domain], p)
	w.mu.Unlock()
}

// deliverDomain makes one attempt to hand the message to a mail server of
// the domain, for all the given recipients in one transaction.
//
// A connection left open by an earlier message to the same domain is used
// again. Otherwise the hosts are tried in order of preference, and every
// address of a host, until one takes the connection through EHLO and, if a
// policy demands it, TLS with a valid certificate. What that server then
// answers for each recipient is final for this attempt: a 4yz reply is not
// taken to the next host, it waits for the next try.
func (w *Worker) deliverDomain(ctx context.Context, domain, from string, rcpts []string, msg []byte, log *slog.Logger) []outcome {
	done, ok := w.slot(ctx, domain)
	if !ok {
		return sameOutcome(len(rcpts), outcome{})
	}
	defer done()
	log = log.With("domain", domain)

	var hosts []hostPort
	var policy *store.STSPolicy
	route, routed := w.Routes[domain]
	if !routed {
		route, routed = w.Routes["*"]
	}
	if routed {
		host, port, err := net.SplitHostPort(route)
		if err != nil {
			return sameOutcome(len(rcpts), fromFailure(temporary("4.3.5", "the route for "+domain+" is not host:port")))
		}
		hosts = []hostPort{{host, port}}
	} else {
		policy = w.policy(ctx, domain)
		dest, fail := w.lookupMX(ctx, domain)
		if fail != nil {
			log.Info("queue: no mail server", "reason", fail.msg)
			return sameOutcome(len(rcpts), fromFailure(fail))
		}
		for _, h := range dest.hosts {
			hosts = append(hosts, hostPort{h, w.Port})
		}
		if dest.implicit {
			// Without MX and without an address the domain does not exist
			// for mail: that is final (RFC 5321 5.1, 3.6.2).
			if _, fail := w.addresses(ctx, domain); fail != nil && fail.permanent {
				return sameOutcome(len(rcpts), fromFailure(permanent("5.1.2", domain+" has no mail server: no MX record and no address")))
			}
		}
	}
	enforce := policy != nil && policy.Mode == stsEnforce

	send := func(p *pooled) ([]outcome, bool) {
		results, err := p.c.Send(from, rcpts, msg)
		if err != nil {
			p.c.Close()
			log.Warn("queue: the connection failed during the transaction", "host", p.host, "err", err)
			return nil, false
		}
		out := make([]outcome, len(results))
		for i, r := range results {
			out[i] = fromReply(r.Reply, p.host)
			log.Info("queue: reply", "to", r.Addr, "host", p.host, "reply", clip(r.Reply.String()))
		}
		w.putIdle(domain, p)
		return out, true
	}

	// RSET tells whether the server is still there before a transaction
	// is risked on an old connection.
	for p := w.takeIdle(domain, enforce); p != nil; p = w.takeIdle(domain, enforce) {
		if time.Since(p.since) > idleFor || p.c.Reset() != nil {
			p.c.Close()
			continue
		}
		if out, ok := send(p); ok {
			return out
		}
		// A transaction that broke on a reused connection is undecided,
		// like one that broke on a new one.
		return sameOutcome(len(rcpts), fromFailure(temporary("4.4.2", "the connection to "+p.host+" was lost")))
	}

	last := temporary("4.4.1", "no mail server for "+domain+" could be reached")
	dials := 0
	for _, hp := range hosts {
		if enforce && !stsMatch(policy.MX, hp.host) {
			// RFC 8461 5: a host the policy does not name is not used.
			log.Warn("queue: MX host is not in the MTA-STS policy, skipping", "host", hp.host)
			last = temporary("4.7.0", "the mail server "+hp.host+" is not allowed by the MTA-STS policy of "+domain)
			continue
		}
		if policy != nil && policy.Mode == stsTesting && !stsMatch(policy.MX, hp.host) {
			log.Warn("queue: MTA-STS (testing): MX host is not in the policy", "host", hp.host)
		}
		addrs, fail := w.addresses(ctx, hp.host)
		if fail != nil {
			last = temporary("4.4.4", fail.msg)
			continue
		}
		for _, a := range addrs {
			if dials++; dials > maxDials || ctx.Err() != nil {
				return sameOutcome(len(rcpts), fromFailure(last))
			}
			p, fail := w.connect(ctx, hp, a.IP, policy, log)
			if fail != nil {
				last = fail
				continue
			}
			if out, ok := send(p); ok {
				return out
			}
			return sameOutcome(len(rcpts), fromFailure(temporary("4.4.2", "the connection to "+hp.host+" was lost")))
		}
	}
	return sameOutcome(len(rcpts), fromFailure(last))
}

type hostPort struct{ host, port string }

// connect opens a connection to one address of a host and takes it up to
// the point where mail can be sent: greeting, EHLO, and TLS.
//
// TLS is opportunistic (RFC 3207): used when the server offers STARTTLS,
// without checking the certificate, because most servers have one that
// proves nothing and an attacker who can change the connection can remove
// the STARTTLS offer anyway. If the handshake fails the server is tried
// once more without TLS. An MTA-STS policy in mode enforce changes that
// (RFC 8461 4.2): STARTTLS is required, with TLS 1.2 or later and a
// certificate that is valid for the host; anything less and this host is
// not used. In mode testing the same is checked and only logged.
func (w *Worker) connect(ctx context.Context, hp hostPort, ip net.IP, policy *store.STSPolicy, log *slog.Logger) (*pooled, *failure) {
	enforce := policy != nil && policy.Mode == stsEnforce
	address := net.JoinHostPort(ip.String(), hp.port)
	for _, useTLS := range []bool{true, false} {
		conn, err := w.dial(ctx, address)
		if err != nil {
			return nil, temporary("4.4.1", "connecting to "+hp.host+" ("+ip.String()+") failed: "+netText(err))
		}
		var trace func(dir, line string)
		if w.Trace {
			trace = func(dir, line string) {
				log.Info("smtp trace", "side", "client", "host", hp.host, "remote", ip.String(), "dir", dir, "line", line)
			}
		}
		c, err := smtp.NewClientTrace(conn, w.Hostname, w.Timeouts, trace)
		if err == nil {
			err = c.Hello()
			if err != nil {
				c.Close()
			}
		}
		if err != nil {
			return nil, temporary("4.4.2", hp.host+" did not take the connection: "+replyText(err))
		}
		p := &pooled{c: c, host: hp.host}
		_, offered := c.Extension("STARTTLS")
		switch {
		case enforce && !offered:
			c.Quit()
			return nil, temporary("4.7.0", hp.host+" does not offer STARTTLS, which the MTA-STS policy requires")
		case enforce:
			cfg := &tls.Config{ServerName: hp.host, RootCAs: w.RootCAs, MinVersion: tls.VersionTLS12}
			if err := c.StartTLS(cfg); err != nil {
				return nil, temporary("4.7.0", "TLS with "+hp.host+" failed, and the MTA-STS policy requires it: "+replyText(err))
			}
			p.secure = true
			return p, nil
		case offered && useTLS:
			if err := c.StartTLS(&tls.Config{ServerName: hp.host, InsecureSkipVerify: true}); err != nil {
				log.Info("queue: STARTTLS failed, trying without", "host", hp.host, "err", err)
				continue
			}
			if policy != nil {
				if err := verifyPeer(c.TLS(), hp.host, w.RootCAs, time.Now()); err != nil {
					log.Warn("queue: MTA-STS (testing): the certificate would not pass", "host", hp.host, "err", err)
				}
			}
			return p, nil
		}
		if policy != nil && !offered {
			log.Warn("queue: MTA-STS (testing): the server does not offer STARTTLS", "host", hp.host)
		}
		return p, nil
	}
	return nil, temporary("4.4.2", "no usable connection to "+hp.host)
}

func netText(err error) string {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return oe.Err.Error()
	}
	return err.Error()
}

func replyText(err error) string {
	var rep smtp.Reply
	if errors.As(err, &rep) {
		return rep.String()
	}
	return err.Error()
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// bounce stores a delivery status notification (RFC 3464) for the
// recipients that failed in the sender's Inbox.
//
// A sender who is a user of this server gets it without it ever leaving:
// it is delivered like local mail, with the null reverse-path in
// Return-Path as RFC 3464 2 and RFC 5321 4.5.5 want it. A message that
// itself has the null reverse-path is a notification already; nothing is
// sent in reply to it (RFC 5321 6.1), or two servers could bounce each
// other's bounces forever.
func (w *Worker) bounce(ctx context.Context, sub store.Submission, msg []byte, failed []store.Recipient) error {
	if sub.MailFrom == "" {
		return nil
	}
	now := w.now()
	d := message.DSN{ReportingMTA: w.Hostname, ArrivalDate: sub.CreatedAt.In(now.Location())}
	h := sha256.New()
	for _, r := range failed {
		rec := message.DSNRecipient{Address: r.Address, Action: "failed", Status: r.Status, RemoteMTA: r.MTA,
			Diagnostic: r.Reply, LastAttempt: now}
		if !message.ValidStatus(rec.Status) || rec.Status[0] == '2' {
			rec.Status = "5.0.0"
		}
		d.Recipients = append(d.Recipients, rec)
		h.Write([]byte(r.Address + "\x00"))
	}
	name := "Mail Delivery System"
	dsn := message.BuildDSN(message.DSNMessage{
		From:      message.Address{Name: &name, Email: "MAILER-DAEMON@" + w.Hostname},
		To:        sub.MailFrom,
		MessageID: message.NewMessageID(w.Hostname),
		Date:      now,
		Original:  msg,
	}, d)
	if store.IsSystemAccount(sub.Account) {
		// Forwarded mail: the sender is somebody elsewhere, and the
		// address the message left with is one this server made from
		// theirs. Local delivery reads the sender back out of it and
		// queues the notification for them, with the null reverse-path.
		err := w.Local.Local(context.WithoutCancel(ctx), "", sub.MailFrom, dsn)
		var se *smtp.Error
		if errors.As(err, &se) && se.Code >= 500 {
			w.log().Warn("queue: a bounce of forwarded mail has nowhere to go", "to", sub.MailFrom, "reply", se.Msg)
			return nil
		}
		return err
	}
	dsn = append([]byte("Return-Path: <>\r\n"), dsn...)
	// The key makes a second run of the same attempt store nothing.
	key := "dsn:" + store.SubmissionID(sub.ID) + ":" + hex.EncodeToString(h.Sum(nil)[:12])
	// A full mailbox still gets it: the notification is the only word the
	// sender gets about the message.
	_, err := w.Store.Deliver(context.WithoutCancel(ctx), sub.Account, store.Message{Raw: dsn, Key: key, ReceivedAt: now, IgnoreQuota: true})
	if errors.Is(err, store.ErrDuplicate) {
		return nil
	}
	return err
}
