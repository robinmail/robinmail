package imap

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/message"
)

// The tests of this package run the server against a store in memory that
// follows the rules of the real one (internal/store): a mailbox is a
// label, a message in two mailboxes is one message with two UIDs, flags
// belong to the message, one modseq counts for the account, and EXPUNGE
// destroys a message only when it leaves its last mailbox.

type memMsg struct {
	raw    []byte
	flags  map[string]bool
	modseq int64
	date   time.Time
	boxes  int
}

type memBox struct {
	Mailbox
	uids map[uint32]int64 // UID -> message id
}

type memStore struct {
	mu       sync.Mutex
	name     string
	password string
	modseq   int64
	nextBox  int64
	nextMsg  int64
	boxes    map[int64]*memBox
	msgs     map[int64]*memMsg
	subs     map[chan struct{}]bool
	// limited makes logins fail with ErrLimited; down with another error.
	limited, down bool
	logins        []string // "ip name" of every attempt
	// opened counts Open calls: how many messages a command read.
	opened int
}

func newMemStore() *memStore {
	s := &memStore{name: "fred@example.test", password: "secret", modseq: 1, nextBox: 1, nextMsg: 1,
		boxes: map[int64]*memBox{}, msgs: map[int64]*memMsg{}, subs: map[chan struct{}]bool{}}
	for _, b := range []struct{ name, role string }{{"Inbox", "inbox"}, {"Drafts", "drafts"}, {"Sent", "sent"}, {"Junk", "junk"}, {"Trash", "trash"}} {
		id := s.create(0, b.name)
		s.boxes[id].Role = b.role
	}
	return s
}

func (s *memStore) create(parent int64, name string) int64 {
	id := s.nextBox
	s.nextBox++
	s.boxes[id] = &memBox{Mailbox: Mailbox{ID: id, ParentID: parent, Name: name, Subscribed: true,
		UIDValidity: uint32(3857529045 + id), UIDNext: 1}, uids: map[uint32]int64{}}
	return id
}

// changed bumps the modseq and wakes the sessions. Call with mu held.
func (s *memStore) changed() int64 {
	s.modseq++
	for c := range s.subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
	return s.modseq
}

func (s *memStore) box(path string) *memBox {
	for _, b := range s.boxes {
		if b.Name == path || (path == "INBOX" && b.Role == "inbox") {
			return b
		}
	}
	return nil
}

// add delivers a message into a mailbox, as SMTP or JMAP would.
func (s *memStore) add(box string, raw string, flags ...string) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.box(box)
	if b == nil {
		panic("no mailbox " + box)
	}
	return s.addLocked(b, []byte(strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\n", "\r\n")), flags, time.Date(1996, 7, 17, 2, 44, 25, 0, time.FixedZone("", -7*3600)))
}

func (s *memStore) addLocked(b *memBox, raw []byte, flags []string, date time.Time) uint32 {
	id := s.nextMsg
	s.nextMsg++
	m := &memMsg{raw: raw, flags: map[string]bool{}, date: date, boxes: 1}
	for _, f := range flags {
		m.flags[f] = true
	}
	m.modseq = s.changed()
	s.msgs[id] = m
	uid := b.UIDNext
	b.UIDNext++
	b.uids[uid] = id
	return uid
}

// setFlag changes a flag as another client (JMAP) would.
func (s *memStore) setFlag(box string, uid uint32, flag string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.msgs[s.box(box).uids[uid]]
	if on {
		m.flags[flag] = true
	} else {
		delete(m.flags, flag)
	}
	m.modseq = s.changed()
}

// remove destroys a message as another client would.
func (s *memStore) remove(box string, uid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.box(box)
	id := b.uids[uid]
	for _, o := range s.boxes {
		for u, mid := range o.uids {
			if mid == id {
				delete(o.uids, u)
			}
		}
	}
	delete(s.msgs, id)
	s.changed()
}

func (s *memStore) Login(ctx context.Context, ip, name, password string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logins = append(s.logins, ip+" "+name)
	switch {
	case s.down:
		return nil, fmt.Errorf("the database is away")
	case s.limited:
		return nil, ErrLimited
	case !strings.EqualFold(name, s.name) || password != s.password:
		return nil, ErrAuth
	}
	return s, nil
}

func (s *memStore) Username() string { return s.name }

func (s *memStore) Mailboxes(ctx context.Context) ([]Mailbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Mailbox, 0, len(s.boxes))
	for _, b := range s.boxes {
		m := b.Mailbox
		m.Messages, m.Unseen, m.Deleted, m.Size = 0, 0, 0, 0
		for _, id := range b.uids {
			msg := s.msgs[id]
			m.Messages++
			m.Size += int64(len(msg.raw))
			if !msg.flags["$seen"] {
				m.Unseen++
			}
			if msg.flags["$deleted"] {
				m.Deleted++
			}
		}
		m.HighestModseq = s.modseq
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *memStore) Create(ctx context.Context, parent int64, name string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if parent != 0 && s.boxes[parent] == nil {
		return 0, ErrNoMailbox
	}
	for _, b := range s.boxes {
		if b.ParentID == parent && b.Name == name {
			return 0, ErrExists
		}
	}
	id := s.create(parent, name)
	s.boxes[id].Subscribed = false // a client subscribes itself
	s.changed()
	return id, nil
}

func (s *memStore) Rename(ctx context.Context, id, parent int64, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[id]
	if b == nil {
		return ErrNoMailbox
	}
	for _, o := range s.boxes {
		if o.ID != id && o.ParentID == parent && o.Name == name {
			return ErrExists
		}
	}
	b.ParentID, b.Name = parent, name
	s.changed()
	return nil
}

func (s *memStore) Delete(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[id]
	if b == nil {
		return ErrNoMailbox
	}
	if b.Role != "" {
		return ErrSpecial
	}
	for _, o := range s.boxes {
		if o.ParentID == id {
			return ErrHasChildren
		}
	}
	for _, mid := range b.uids {
		if m := s.msgs[mid]; m != nil {
			if m.boxes--; m.boxes == 0 {
				delete(s.msgs, mid)
			}
		}
	}
	delete(s.boxes, id)
	s.changed()
	return nil
}

func (s *memStore) Subscribe(ctx context.Context, id int64, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[id]
	if b == nil {
		return ErrNoMailbox
	}
	b.Subscribed = on
	s.changed()
	return nil
}

func (s *memStore) flagsOf(m *memMsg) []string {
	var out []string
	for f := range m.flags {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func (s *memStore) View(ctx context.Context, mailbox, since int64) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[mailbox]
	if b == nil {
		return View{}, ErrNoMailbox
	}
	v := View{UIDValidity: b.UIDValidity, UIDNext: b.UIDNext, Modseq: s.modseq}
	for uid := range b.uids {
		v.UIDs = append(v.UIDs, uid)
	}
	sort.Slice(v.UIDs, func(i, j int) bool { return v.UIDs[i] < v.UIDs[j] })
	for _, uid := range v.UIDs {
		id := b.uids[uid]
		if m := s.msgs[id]; m.modseq > since {
			v.Changed = append(v.Changed, Message{UID: uid, ID: id, Modseq: m.modseq, Size: int64(len(m.raw)),
				InternalDate: m.date, Flags: s.flagsOf(m)})
		}
	}
	return v, nil
}

func (s *memStore) Open(ctx context.Context, id int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened++
	m := s.msgs[id]
	if m == nil {
		return nil, ErrNoMessage
	}
	return m.raw, nil
}

func (s *memStore) Summaries(ctx context.Context, ids []int64) (map[int64]Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64]Summary{}
	for _, id := range ids {
		m := s.msgs[id]
		if m == nil {
			continue
		}
		e := parseEntity(m.raw, 0, len(m.raw), 0, "text/plain")
		text := func(name string) string { return strings.Join(headerText(e, name), ", ") }
		sum := Summary{From: text("From"), To: text("To"), Cc: text("Cc"), Bcc: text("Bcc"), Subject: text("Subject")}
		// Like the real store, long fields are remembered cut short.
		for _, f := range []*string{&sum.From, &sum.To, &sum.Cc, &sum.Bcc, &sum.Subject} {
			if len(*f) > 60 {
				*f, sum.Partial = (*f)[:60], true
			}
		}
		if v, ok := e.get("Date"); ok {
			if t, ok := message.Date(v); ok {
				sum.SentAt = t
			}
		}
		out[id] = sum
	}
	return out, nil
}

func (s *memStore) Append(ctx context.Context, mailbox int64, raw []byte, flags []string, date time.Time) (uint32, uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[mailbox]
	if b == nil {
		return 0, 0, ErrNoMailbox
	}
	if date.IsZero() {
		date = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	}
	for _, f := range flags {
		if strings.ContainsAny(f, " (){%*\"\\]") {
			return 0, 0, ErrFlag
		}
	}
	return s.addLocked(b, append([]byte(nil), raw...), flags, date), b.UIDValidity, nil
}

func inUIDRanges(ranges []UIDRange, uid uint32) bool {
	if ranges == nil {
		return true
	}
	for _, r := range ranges {
		if uid >= r.Lo && uid <= r.Hi {
			return true
		}
	}
	return false
}

func (b *memBox) sorted(ranges []UIDRange) []uint32 {
	var out []uint32
	for uid := range b.uids {
		if inUIDRanges(ranges, uid) {
			out = append(out, uid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *memStore) Store(ctx context.Context, mailbox int64, uids []UIDRange, mode int, flags []string, unchangedSince int64) ([]uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[mailbox]
	if b == nil {
		return nil, ErrNoMailbox
	}
	for _, f := range flags {
		if strings.ContainsAny(f, " (){%*\"\\]") {
			return nil, ErrFlag
		}
	}
	var modified []uint32
	var modseq int64
	for _, uid := range b.sorted(uids) {
		m := s.msgs[b.uids[uid]]
		if unchangedSince >= 0 && m.modseq > unchangedSince {
			modified = append(modified, uid)
			continue
		}
		next := map[string]bool{}
		if mode != FlagsReplace {
			for f := range m.flags {
				next[f] = true
			}
		}
		for _, f := range flags {
			if mode == FlagsRemove {
				delete(next, f)
			} else {
				next[f] = true
			}
		}
		same := len(next) == len(m.flags)
		for f := range next {
			same = same && m.flags[f]
		}
		if same {
			continue
		}
		if modseq == 0 {
			modseq = s.changed()
		}
		m.flags, m.modseq = next, modseq
	}
	return modified, nil
}

func (s *memStore) Copy(ctx context.Context, from int64, uids []UIDRange, to int64, move bool) (uint32, []UIDPair, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, dst := s.boxes[from], s.boxes[to]
	if src == nil || dst == nil {
		return 0, nil, ErrNoMailbox
	}
	var pairs []UIDPair
	var modseq int64
	for _, uid := range src.sorted(uids) {
		id := src.uids[uid]
		if from == to && !move {
			pairs = append(pairs, UIDPair{uid, uid})
			continue
		}
		if from == to {
			// A move within the mailbox: a new UID.
			if modseq == 0 {
				modseq = s.changed()
			}
			s.msgs[id].modseq = modseq
			delete(src.uids, uid)
			src.uids[src.UIDNext] = id
			pairs = append(pairs, UIDPair{uid, src.UIDNext})
			src.UIDNext++
			continue
		}
		if modseq == 0 {
			modseq = s.changed()
		}
		m := s.msgs[id]
		m.modseq = modseq
		there := uint32(0)
		for u, mid := range dst.uids {
			if mid == id {
				there = u
			}
		}
		if there == 0 {
			there = dst.UIDNext
			dst.UIDNext++
			dst.uids[there] = id
			m.boxes++
		}
		if move {
			delete(src.uids, uid)
			m.boxes--
		}
		pairs = append(pairs, UIDPair{uid, there})
	}
	return dst.UIDValidity, pairs, nil
}

func (s *memStore) Expunge(ctx context.Context, mailbox int64, uids []UIDRange) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[mailbox]
	if b == nil {
		return ErrNoMailbox
	}
	var modseq int64
	for _, uid := range b.sorted(uids) {
		id := b.uids[uid]
		m := s.msgs[id]
		if !m.flags["$deleted"] {
			continue
		}
		if modseq == 0 {
			modseq = s.changed()
		}
		delete(b.uids, uid)
		if m.boxes--; m.boxes == 0 {
			delete(s.msgs, id)
		} else {
			delete(m.flags, "$deleted")
			m.modseq = modseq
		}
	}
	return nil
}

func (s *memStore) Changes() (<-chan struct{}, func()) {
	c := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[c] = true
	s.mu.Unlock()
	return c, func() {
		s.mu.Lock()
		delete(s.subs, c)
		s.mu.Unlock()
	}
}

// testCert is a self-signed certificate for the test listeners.
var testCert = func() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "imap.example.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), DNSNames: []string{"imap.example.test"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}()

// env is a server with a store, an implicit TLS listener and a plain one.
type env struct {
	t     *testing.T
	st    *memStore
	srv   *Server
	tls   net.Addr
	plain net.Addr
}

func newEnv(t *testing.T, setup ...func(*Server)) *env {
	t.Helper()
	st := newMemStore()
	srv := &Server{Hostname: "imap.example.test", Backend: st, TLSConfig: &tls.Config{Certificates: []tls.Certificate{testCert}}}
	for _, f := range setup {
		f(srv)
	}
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	secure, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(plain)
	go srv.Serve(tls.NewListener(secure, srv.TLSConfig))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return &env{t: t, st: st, srv: srv, tls: secure.Addr(), plain: plain.Addr()}
}

// client is a test client: it sends lines and reads responses.
type client struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	tag  int
}

// dial connects over TLS and reads the greeting.
func (e *env) dial() *client {
	e.t.Helper()
	conn, err := tls.Dial("tcp", e.tls.String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		e.t.Fatal(err)
	}
	c := &client{t: e.t, conn: conn, br: bufio.NewReader(conn)}
	e.t.Cleanup(func() { conn.Close() })
	if g := c.line(); !strings.HasPrefix(g, "* OK ") {
		e.t.Fatalf("greeting: %q", g)
	}
	return c
}

// dialPlain connects without TLS and returns the greeting too.
func (e *env) dialPlain() (*client, string) {
	e.t.Helper()
	conn, err := net.Dial("tcp", e.plain.String())
	if err != nil {
		e.t.Fatal(err)
	}
	c := &client{t: e.t, conn: conn, br: bufio.NewReader(conn)}
	e.t.Cleanup(func() { conn.Close() })
	return c, c.line()
}

// login is dial plus LOGIN as the test user.
func (e *env) login() *client {
	e.t.Helper()
	c := e.dial()
	c.ok(`LOGIN fred@example.test secret`)
	return c
}

// selected is login plus SELECT.
func (e *env) selected(mailbox string) *client {
	e.t.Helper()
	c := e.login()
	c.ok("SELECT " + mailbox)
	return c
}

func (c *client) write(s string) {
	c.t.Helper()
	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c.conn, s); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

var literalRE = regexp.MustCompile(`\{(\d+)\}$`)

// line reads one response line. A literal in it is read too and stays in
// the line, after its "{n}" and a line break.
func (c *client) line() string {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var out strings.Builder
	for {
		l, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read: %v (after %q)", err, out.String()+l)
		}
		if !strings.HasSuffix(l, "\r\n") {
			c.t.Fatalf("a line that does not end in CRLF: %q", l)
		}
		l = strings.TrimSuffix(l, "\r\n")
		out.WriteString(l)
		m := literalRE.FindStringSubmatch(l)
		if m == nil {
			return out.String()
		}
		n, _ := strconv.Atoi(m[1])
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.br, buf); err != nil {
			c.t.Fatalf("read literal: %v", err)
		}
		out.WriteString("\r\n")
		out.Write(buf)
	}
}

// eof reports whether the server closed the connection.
func (c *client) eof() bool {
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := c.br.ReadByte()
	return err == io.EOF
}

// quiet fails if the server says anything within d.
func (c *client) quiet(d time.Duration) {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(d))
	if b, err := c.br.ReadByte(); err == nil {
		c.br.UnreadByte()
		rest, _ := c.br.Peek(c.br.Buffered())
		c.t.Fatalf("the server spoke unasked: %q", string(b)+string(rest))
	}
}

// cmd sends a command under a new tag and returns the responses up to
// and including the tagged one, which comes back without its tag.
func (c *client) cmd(command string) []string {
	c.t.Helper()
	c.tag++
	tag := fmt.Sprintf("a%03d", c.tag)
	c.write(tag + " " + command + "\r\n")
	return c.until(tag)
}

func (c *client) until(tag string) []string {
	c.t.Helper()
	var lines []string
	for {
		l := c.line()
		if rest, ok := strings.CutPrefix(l, tag+" "); ok {
			return append(lines, rest)
		}
		lines = append(lines, l)
	}
}

// ok runs a command that must end in OK and returns the untagged lines.
func (c *client) ok(command string) []string {
	c.t.Helper()
	lines := c.cmd(command)
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "OK") {
		c.t.Fatalf("%s: %s", command, strings.Join(lines, "\n"))
	}
	return lines[:len(lines)-1]
}

// expect runs a command and compares all responses, the tagged one
// included (without its tag), with want.
func (c *client) expect(command string, want ...string) {
	c.t.Helper()
	got := c.cmd(command)
	if !equalLines(got, want) {
		c.t.Errorf("%s\n got:\n   %s\nwant:\n   %s", command, strings.Join(got, "\n   "), strings.Join(want, "\n   "))
	}
}

func equalLines(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// status runs a command and returns only the tagged response.
func (c *client) status(command string) string {
	c.t.Helper()
	lines := c.cmd(command)
	return lines[len(lines)-1]
}

// wantLines compares lines read by hand.
func wantLines(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !equalLines(got, want) {
		t.Errorf("%s\n got:\n   %s\nwant:\n   %s", what, strings.Join(got, "\n   "), strings.Join(want, "\n   "))
	}
}

// addAt is add with an internal date.
func (s *memStore) addAt(box, raw string, date time.Time, flags ...string) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addLocked(s.box(box), []byte(raw), flags, date)
}
