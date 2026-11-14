package clustertest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	hostname = "mail.robinmail.test"
	domain   = "cluster.test"
	blobDir  = "/var/lib/robinmail/blobs"
)

// robinmail and rqlite nodes as cluster.yaml publishes them.
type rnode struct {
	service string
	smtp    string
	https   string
	config  string
}

var (
	robin = []rnode{
		{"robinmail-1", "127.0.0.1:2531", "127.0.0.1:8441", "/etc/robinmail/n1.json"},
		{"robinmail-2", "127.0.0.1:2532", "127.0.0.1:8442", "/etc/robinmail/n2.json"},
		{"robinmail-3", "127.0.0.1:2533", "127.0.0.1:8443", "/etc/robinmail/n3.json"},
	}
	rqliteHTTP = []string{"127.0.0.1:4011", "127.0.0.1:4012", "127.0.0.1:4013"}
	users      = []string{"user1@" + domain, "user2@" + domain, "user3@" + domain, "user4@" + domain}
)

func password() string {
	if p := os.Getenv("ROBINMAIL_CLUSTER_PASSWORD"); p != "" {
		return p
	}
	return "robinmail-cluster"
}

// compose runs docker compose on the cluster and returns its output.
func compose(t testing.TB, args ...string) string {
	t.Helper()
	out, err := composeErr(args...)
	if err != nil {
		t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func composeErr(args ...string) (string, error) {
	cmd := exec.Command("docker", append([]string{"compose", "-f", "../../contrib/compose/cluster.yaml"}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// sh runs a shell command inside a robinmail container.
func sh(t testing.TB, n int, script string) string {
	t.Helper()
	return strings.TrimSpace(compose(t, "exec", "-T", robin[n].service, "sh", "-c", script))
}

// blobCount is the number of blob files on a node's disk.
func blobCount(t testing.TB, n int) int {
	t.Helper()
	out := sh(t, n, "find "+blobDir+" -type f ! -name '.*' 2>/dev/null | wc -l")
	c, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("blob count on %s: %q", robin[n].service, out)
	}
	return c
}

// rqliteLeader returns the index of the rqlite node that leads, asking
// whichever node answers.
func rqliteLeader(t testing.TB) int {
	t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		for _, addr := range rqliteHTTP {
			resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr + "/nodes")
			if err != nil {
				continue
			}
			var nodes map[string]struct {
				Leader    bool `json:"leader"`
				Reachable bool `json:"reachable"`
			}
			err = json.NewDecoder(resp.Body).Decode(&nodes)
			resp.Body.Close()
			if err != nil {
				continue
			}
			for id, n := range nodes {
				if n.Leader && n.Reachable {
					i, _ := strconv.Atoi(id)
					return i - 1
				}
			}
		}
	}
	t.Fatal("rqlite has no leader")
	return -1
}

// rqliteReachable is how many rqlite nodes the cluster sees as reachable.
func rqliteReachable() int {
	for _, addr := range rqliteHTTP {
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr + "/nodes")
		if err != nil {
			continue
		}
		var nodes map[string]struct {
			Reachable bool `json:"reachable"`
		}
		err = json.NewDecoder(resp.Body).Decode(&nodes)
		resp.Body.Close()
		if err != nil {
			continue
		}
		n := 0
		for _, x := range nodes {
			if x.Reachable {
				n++
			}
		}
		return n
	}
	return 0
}

func waitFor(t testing.TB, what string, limit time.Duration, ok func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < limit {
		if ok() {
			return time.Since(start)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
	return 0
}

// ---- SMTP ----

type smtpConn struct {
	c net.Conn
	r *bufio.Reader
}

// How long a sender waits for one reply. robinmail itself keeps trying
// rqlite for 15 seconds before it answers 451.
const smtpTimeout = 30 * time.Second

func dialSMTP(addr string) (*smtpConn, error) {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	s := &smtpConn{c: c, r: bufio.NewReader(c)}
	if code, err := s.reply(); err != nil || code != 220 {
		c.Close()
		return nil, fmt.Errorf("greeting: %d %v", code, err)
	}
	if code, err := s.cmd("EHLO cluster-test.example"); err != nil || code != 250 {
		c.Close()
		return nil, fmt.Errorf("EHLO: %d %v", code, err)
	}
	return s, nil
}

// reply reads one reply, of one or more lines, and returns its code.
func (s *smtpConn) reply() (int, error) {
	s.c.SetReadDeadline(time.Now().Add(smtpTimeout))
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return 0, err
		}
		if len(line) < 4 {
			return 0, fmt.Errorf("short reply %q", line)
		}
		if line[3] == '-' {
			continue
		}
		return strconv.Atoi(line[:3])
	}
}

func (s *smtpConn) cmd(line string) (int, error) {
	s.c.SetWriteDeadline(time.Now().Add(smtpTimeout))
	if _, err := io.WriteString(s.c, line+"\r\n"); err != nil {
		return 0, err
	}
	return s.reply()
}

// send delivers one message and returns the reply code to the end of DATA.
func (s *smtpConn) send(from, to string, msg []byte) (int, error) {
	for _, c := range []string{"MAIL FROM:<" + from + ">", "RCPT TO:<" + to + ">"} {
		if code, err := s.cmd(c); err != nil || code != 250 {
			return code, err
		}
	}
	if code, err := s.cmd("DATA"); err != nil || code != 354 {
		return code, err
	}
	s.c.SetWriteDeadline(time.Now().Add(smtpTimeout))
	if _, err := s.c.Write(append(msg, ".\r\n"...)); err != nil {
		return 0, err
	}
	return s.reply()
}

func (s *smtpConn) close() { s.c.Close() }

// message builds a message whose subject is the token and whose body is
// random, so every message is its own blob.
func message(token, to string) []byte {
	var r [48]byte
	rand.Read(r[:])
	return []byte("From: Sender <sender@elsewhere.example>\r\n" +
		"To: <" + to + ">\r\n" +
		"Subject: " + token + "\r\n" +
		"Message-ID: <" + token + "@elsewhere.example>\r\n" +
		"Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n" +
		"\r\n" +
		"token " + token + "\r\n" + hex.EncodeToString(r[:]) + "\r\n")
}

// ---- the stream of deliveries ----

type sent struct {
	to   string
	msg  []byte
	node int // the node that answered 250
	at   time.Time
}

// stream sends messages through all three nodes and keeps book: which got
// 250, and which were tried and never did.
type stream struct {
	run string

	mu       sync.Mutex
	acked    map[string]sent
	unacked  map[string]sent // tried at least once, no 250 (yet)
	tempfail int             // 4xx answers
	connfail int             // attempts that ended without an answer
	lastAck  time.Time
	gap      time.Duration // longest time without any 250 while sending
	other    []string      // anything unexpected, such as a 5xx
}

func newStream(name string) *stream {
	var r [3]byte
	rand.Read(r[:])
	return &stream{run: name + hex.EncodeToString(r[:]), acked: map[string]sent{}, unacked: map[string]sent{}}
}

// start runs workers until stop is closed. Each worker keeps to one node
// while that node takes its mail and goes round the others when it does
// not, sending the very same message again, as a sending server would
// retry. A worker ends only when its last message got 250, or after
// drain.
func (st *stream) start(workers int, stop <-chan struct{}, drain time.Duration) *sync.WaitGroup {
	var wg sync.WaitGroup
	st.mu.Lock()
	st.lastAck = time.Now()
	st.mu.Unlock()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conns := make([]*smtpConn, len(robin))
			defer func() {
				for _, c := range conns {
					if c != nil {
						c.close()
					}
				}
			}()
			for seq := 0; ; seq++ {
				select {
				case <-stop:
					return
				default:
				}
				token := fmt.Sprintf("%s-%d-%d", st.run, w, seq)
				to := users[(w+seq)%len(users)]
				m := sent{to: to, msg: message(token, to)}
				st.mu.Lock()
				st.unacked[token] = m
				st.mu.Unlock()
				var deadline time.Time
				for try := 0; ; try++ {
					select {
					case <-stop:
						if deadline.IsZero() {
							deadline = time.Now().Add(drain)
						}
					default:
					}
					if !deadline.IsZero() && time.Now().After(deadline) {
						return // stays in unacked
					}
					n := (w + try) % len(robin)
					code, err := st.attempt(conns, n, m)
					if code == 250 {
						m.node, m.at = n, time.Now()
						st.mu.Lock()
						delete(st.unacked, token)
						st.acked[token] = m
						if g := m.at.Sub(st.lastAck); g > st.gap {
							st.gap = g
						}
						st.lastAck = m.at
						st.mu.Unlock()
						break
					}
					st.mu.Lock()
					switch {
					case err != nil || code == 0:
						st.connfail++
					case code >= 400 && code < 500:
						st.tempfail++
					default:
						st.other = append(st.other, fmt.Sprintf("%s via %s: %d", token, robin[n].service, code))
					}
					st.mu.Unlock()
					if conns[n] != nil {
						conns[n].close()
						conns[n] = nil
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
		}()
	}
	return &wg
}

func (st *stream) attempt(conns []*smtpConn, n int, m sent) (int, error) {
	if conns[n] == nil {
		c, err := dialSMTP(robin[n].smtp)
		if err != nil {
			return 0, err
		}
		conns[n] = c
	}
	return conns[n].send("sender@elsewhere.example", m.to, m.msg)
}

func (st *stream) counts() (acked, unacked, tempfail, connfail int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.acked), len(st.unacked), st.tempfail, st.connfail
}

// ---- JMAP ----

// jmap talks to one robinmail node over HTTPS under the cluster's hostname.
type jmap struct {
	node    int
	service string // the container, for messages
	host    string // the name the server answers to
	hc      *http.Client
}

// The other side (cluster.yaml): one robinmail with its own database.
const (
	sinkService = "robinmail-sink"
	sinkHost    = "mail.sink.test"
	sinkHTTPS   = "127.0.0.1:8444"
	sinkUser    = "sink@sink.test"
)

func newJMAP(n int) *jmap { return newJMAPAt(n, robin[n].service, hostname, robin[n].https) }

// newSinkJMAP talks to the robinmail of the other side.
func newSinkJMAP() *jmap { return newJMAPAt(-1, sinkService, sinkHost, sinkHTTPS) }

func newJMAPAt(n int, service, host, addr string) *jmap {
	return &jmap{node: n, service: service, host: host, hc: &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			// Every node is https://mail.robinmail.test; which one answers
			// is decided here.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
			},
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true, ServerName: host}, // self-signed, local
			MaxIdleConnsPerHost: 32,
		},
	}}
}

func (j *jmap) do(method, path, cookie string, body []byte) (int, []byte, http.Header, error) {
	req, err := http.NewRequest(method, "https://"+j.host+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := j.hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header, err
}

// login returns the session cookie, as "name=value", and the account id.
func (j *jmap) login(user string) (cookie, account string, err error) {
	body, _ := json.Marshal(map[string]string{"username": user, "password": password()})
	code, data, h, err := j.do("POST", "/auth/login", "", body)
	if err != nil {
		return "", "", err
	}
	if code != 200 {
		return "", "", fmt.Errorf("login %s via %s: %d %s", user, j.service, code, data)
	}
	var out struct {
		AccountID string `json:"accountId"`
	}
	json.Unmarshal(data, &out)
	set := h.Get("Set-Cookie")
	cookie, _, _ = strings.Cut(set, ";")
	if cookie == "" || out.AccountID == "" {
		return "", "", fmt.Errorf("login %s: no cookie or account in %q %s", user, set, data)
	}
	return cookie, out.AccountID, nil
}

// call runs one JMAP method and returns its arguments.
func (j *jmap) call(cookie, method string, args map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{
		"using":       usingAll,
		"methodCalls": []any{[]any{method, args, "c0"}},
	})
	code, data, _, err := j.do("POST", "/jmap/api", cookie, body)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("%s via %s: HTTP %d %s", method, j.service, code, data)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(data, &out); err != nil || len(out.MethodResponses) != 1 || len(out.MethodResponses[0]) != 3 {
		return nil, fmt.Errorf("%s via %s: bad response %s", method, j.service, data)
	}
	var name string
	json.Unmarshal(out.MethodResponses[0][0], &name)
	var res map[string]any
	json.Unmarshal(out.MethodResponses[0][1], &res)
	if name != method {
		return nil, fmt.Errorf("%s via %s: %s %v", method, j.service, name, res)
	}
	return res, nil
}

var usingAll = []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:submission",
	"urn:ietf:params:jmap:quota", "urn:ietf:params:jmap:sieve", "urn:ietf:params:jmap:vacationresponse"}

// batch runs several method calls in one request and returns the
// arguments of every response, in order. A method error fails it.
func (j *jmap) batch(cookie string, calls []any) ([]map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"using": usingAll, "methodCalls": calls})
	code, data, _, err := j.do("POST", "/jmap/api", cookie, body)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("batch via %s: HTTP %d %s", j.service, code, data)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("batch via %s: bad response %s", j.service, data)
	}
	var results []map[string]any
	for _, r := range out.MethodResponses {
		var name string
		var res map[string]any
		json.Unmarshal(r[0], &name)
		json.Unmarshal(r[1], &res)
		if name == "error" {
			return nil, fmt.Errorf("batch via %s: %v", j.service, res)
		}
		results = append(results, res)
	}
	return results, nil
}

// subjects returns the subjects of the account's emails that contain the
// text, one entry per email.
func (j *jmap) subjects(cookie, account, text string) ([]string, error) {
	res, err := j.batch(cookie, []any{
		[]any{"Email/query", map[string]any{"accountId": account, "filter": map[string]any{"subject": text}, "limit": 500}, "q"},
		[]any{"Email/get", map[string]any{"accountId": account, "properties": []string{"subject"},
			"#ids": map[string]any{"resultOf": "q", "name": "Email/query", "path": "/ids"}}, "g"},
	})
	if err != nil {
		return nil, err
	}
	var out []string
	list, _ := res[1]["list"].([]any)
	for _, x := range list {
		subject, _ := x.(map[string]any)["subject"].(string)
		out = append(out, subject)
	}
	return out, nil
}

// loginAs logs a user in with the cluster password.
func (j *jmap) loginAs(t testing.TB, user string) session {
	t.Helper()
	cookie, account, err := j.login(user)
	if err != nil {
		t.Fatal(err)
	}
	return session{user, cookie, account}
}

// rqliteRows runs a query on the cluster's rqlite and returns the rows.
func rqliteRows(t testing.TB, sql string) [][]any {
	t.Helper()
	var last error
	for _, addr := range rqliteHTTP {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/db/query?level=weak&q=" + url.QueryEscape(sql))
		if err != nil {
			last = err
			continue
		}
		var out struct {
			Results []struct {
				Values [][]any `json:"values"`
				Error  string  `json:"error"`
			} `json:"results"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil || len(out.Results) != 1 || out.Results[0].Error != "" {
			last = fmt.Errorf("rqlite %s: %v %+v", addr, err, out)
			continue
		}
		return out.Results[0].Values
	}
	t.Fatalf("rqlite query failed: %v", last)
	return nil
}

type email struct {
	id, subject, blob string
}

// emails lists every email of the account: id, subject and blobId.
func (j *jmap) emails(cookie, account string) ([]email, error) {
	var ids []any
	for {
		res, err := j.call(cookie, "Email/query", map[string]any{"accountId": account, "position": len(ids), "limit": 500})
		if err != nil {
			return nil, err
		}
		page, _ := res["ids"].([]any)
		ids = append(ids, page...)
		if len(page) < 500 {
			break
		}
	}
	var out []email
	for len(ids) > 0 {
		n := min(len(ids), 500)
		res, err := j.call(cookie, "Email/get", map[string]any{"accountId": account, "ids": ids[:n],
			"properties": []string{"subject", "blobId"}})
		if err != nil {
			return nil, err
		}
		list, _ := res["list"].([]any)
		if len(list) != n {
			return nil, fmt.Errorf("Email/get via %s returned %d of %d emails", j.service, len(list), n)
		}
		for _, x := range list {
			m := x.(map[string]any)
			e := email{}
			e.id, _ = m["id"].(string)
			e.subject, _ = m["subject"].(string)
			e.blob, _ = m["blobId"].(string)
			out = append(out, e)
		}
		ids = ids[n:]
	}
	return out, nil
}

// download fetches the raw message of a blob.
func (j *jmap) download(cookie, account, blob string) ([]byte, error) {
	code, data, _, err := j.do("GET", "/jmap/download/"+account+"/"+blob+"/m.eml?type=message/rfc822", cookie, nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("download %s via %s: HTTP %d %s", blob, j.service, code, bytes.TrimSpace(data))
	}
	return data, nil
}

// session is one user's web login, made once on one node and then used on
// whichever node a check goes through.
type session struct {
	user, cookie, account string
}

func logins(t testing.TB, via int) []session {
	t.Helper()
	j := newJMAP(via)
	out := make([]session, len(users))
	for i, u := range users {
		var err error
		waitFor(t, "login of "+u, 30*time.Second, func() bool {
			out[i].cookie, out[i].account, err = j.login(u)
			return err == nil
		})
		out[i].user = u
	}
	return out
}

// verify checks, through each of the given nodes, that every message of
// the stream that got 250 is there exactly once and reads back as it was
// sent, and that no message of the stream is there twice, acknowledged or
// not. It returns how many of the stream's messages the store holds.
func verify(t *testing.T, st *stream, sessions []session, nodes ...int) int {
	t.Helper()
	st.mu.Lock()
	acked := make(map[string]sent, len(st.acked))
	for k, v := range st.acked {
		acked[k] = v
	}
	unacked := len(st.unacked)
	st.mu.Unlock()

	stored := 0
	for _, n := range nodes {
		j := newJMAP(n)
		seen := map[string]int{}
		type job struct {
			s session
			e email
		}
		var jobs []job
		for _, s := range sessions {
			var list []email
			var err error
			// A node that just came back may need a moment.
			waitFor(t, "the emails of "+s.user+" via "+robin[n].service, 60*time.Second, func() bool {
				list, err = j.emails(s.cookie, s.account)
				return err == nil
			})
			for _, e := range list {
				if !strings.HasPrefix(e.subject, st.run+"-") {
					continue
				}
				seen[e.subject]++
				if m, ok := acked[e.subject]; ok && m.to != s.user {
					t.Errorf("via %s: %s is in the account of %s, sent to %s", robin[n].service, e.subject, s.user, m.to)
				}
				jobs = append(jobs, job{s, e})
			}
		}
		doubles, missing := 0, 0
		for token, c := range seen {
			if c != 1 {
				doubles++
				t.Errorf("via %s: %s is stored %d times", robin[n].service, token, c)
			}
		}
		for token := range acked {
			if seen[token] == 0 {
				missing++
				t.Errorf("via %s: %s got 250 and is not there", robin[n].service, token)
			}
		}
		// Every stored message of the stream is read back in full.
		var wg sync.WaitGroup
		var mu sync.Mutex
		unreadable := 0
		work := make(chan job)
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for jb := range work {
					raw, err := j.download(jb.s.cookie, jb.s.account, jb.e.blob)
					want, isAcked := acked[jb.e.subject]
					bad := err != nil || (isAcked && !bytes.HasSuffix(raw, want.msg)) || !bytes.Contains(raw, []byte("token "+jb.e.subject+"\r\n"))
					if bad {
						mu.Lock()
						unreadable++
						if unreadable <= 5 {
							t.Errorf("via %s: %s does not read back as sent: %v", robin[n].service, jb.e.subject, err)
						}
						mu.Unlock()
					}
				}
			}()
		}
		for _, jb := range jobs {
			work <- jb
		}
		close(work)
		wg.Wait()
		t.Logf("via %s: %d acknowledged, %d stored, %d read back in full; %d missing, %d doubled, %d unreadable",
			robin[n].service, len(acked), len(seen), len(jobs)-unreadable, missing, doubles, unreadable)
		if len(seen) > len(acked)+unacked {
			t.Errorf("via %s: %d messages stored, more than the %d that were ever sent", robin[n].service, len(seen), len(acked)+unacked)
		}
		stored = len(seen)
	}
	return stored
}

// totalEmails counts all emails of the test users, through one node.
func totalEmails(t testing.TB, sessions []session, via int) int {
	t.Helper()
	j := newJMAP(via)
	total := 0
	for _, s := range sessions {
		list, err := j.emails(s.cookie, s.account)
		if err != nil {
			t.Fatal(err)
		}
		total += len(list)
	}
	return total
}

var errNotUp = errors.New("not up")

// nodeUp reports whether a robinmail node answers on HTTPS and SMTP.
func nodeUp(n int) error {
	if code, _, _, err := newJMAP(n).do("GET", "/auth/session", "", nil); err != nil || code != 401 {
		return errNotUp
	}
	c, err := dialSMTP(robin[n].smtp)
	if err != nil {
		return errNotUp
	}
	c.close()
	return nil
}
