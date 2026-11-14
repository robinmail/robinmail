package labtest

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	composeFile = "../../contrib/lab/compose.yaml"
	caFile      = "../../contrib/lab/secrets/ca.crt"

	robinHost   = "mx.robin.test"
	robinDomain = "robin.test"
	robinIP     = "10.77.25.10"
	gateIP      = "10.77.25.3" // what robinmail and the peers see of the harness
	gateHTTPS   = "127.0.0.1:8450"
	gateSMTP    = "127.0.0.1:2540"

	// robinmail's and the peers' limits on a message, as the lab sets them.
	robinMax = 2 << 20
	peerMax  = 1 << 20
)

// peer is one of the other mail servers.
type peer struct {
	name   string // the compose service
	domain string
	host   string
	ip     string
	gate   string // its port 25 on the host
	// sendmail is the command that takes a message on standard input.
	sendmail []string
	// smtputf8: the server knows RFC 6531.
	smtputf8 bool
	// sizeOut: the SMTP listener takes less than robinmail sends (peerMax).
	sizeOut bool
}

var peers = []peer{
	{name: "postfix", domain: "postfix.test", host: "mx.postfix.test", ip: "10.77.25.11", gate: "127.0.0.1:2541",
		sendmail: []string{"sendmail", "-i"}, smtputf8: true, sizeOut: true},
	{name: "exim", domain: "exim.test", host: "mx.exim.test", ip: "10.77.25.12", gate: "127.0.0.1:2542",
		sendmail: []string{"exim4", "-i"}, smtputf8: true, sizeOut: true},
	{name: "opensmtpd", domain: "opensmtpd.test", host: "mx.opensmtpd.test", ip: "10.77.25.13", gate: "127.0.0.1:2543",
		sendmail: []string{"sendmail"}},
}

func password() string {
	if p := os.Getenv("ROBINMAIL_LAB_PASSWORD"); p != "" {
		return p
	}
	return "robinmail-lab"
}

// newToken is a word no other message has. It goes into the X-Lab header
// of every message a test sends, which is how the message is found again.
func newToken() string {
	var r [6]byte
	rand.Read(r[:])
	return "lab" + hex.EncodeToString(r[:])
}

// ---- docker compose ----

func composeErr(stdin []byte, args ...string) (string, error) {
	cmd := exec.Command("docker", append([]string{"compose", "-f", composeFile}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func compose(t testing.TB, args ...string) string {
	t.Helper()
	out, err := composeErr(nil, args...)
	if err != nil {
		t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// execIn runs a command in a container, as user if that is not empty.
func execIn(service, user string, stdin []byte, argv ...string) (string, error) {
	args := []string{"exec", "-T"}
	if user != "" {
		args = append(args, "-u", user)
	}
	return composeErr(stdin, append(append(args, service), argv...)...)
}

func waitFor(t testing.TB, what string, limit time.Duration, ok func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < limit {
		if ok() {
			return time.Since(start)
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
	return 0
}

// dropQueued takes the messages for a recipient out of robinmail's queue,
// in its database: robinmail has no command for that, and a message that
// can never be delivered would otherwise be tried for five days.
func dropQueued(t testing.TB, rcpt string) {
	t.Helper()
	if strings.ContainsAny(rcpt, `'%_\`) {
		t.Fatalf("dropQueued: %q", rcpt)
	}
	compose(t, "exec", "-T", "rqlite", "rqlite", "-H", "localhost",
		"DELETE FROM submission WHERE recipients LIKE '%"+rcpt+"%'")
}

// ---- the peers ----

func lf(msg []byte) []byte   { return bytes.ReplaceAll(msg, []byte("\r\n"), []byte("\n")) }
func crlf(msg []byte) []byte { return bytes.ReplaceAll(lf(msg), []byte("\n"), []byte("\r\n")) }

// send hands a message to the peer's sendmail command as the local user
// alice, with the given envelope. Local programs speak LF.
func (p peer) send(t testing.TB, from string, rcpts []string, msg []byte) {
	t.Helper()
	if out, err := p.sendErr(from, rcpts, msg); err != nil {
		t.Fatalf("%s: sendmail: %v\n%s", p.name, err, out)
	}
}

func (p peer) sendErr(from string, rcpts []string, msg []byte) (string, error) {
	argv := append(append([]string{}, p.sendmail...), "-f", from)
	if p.name != "opensmtpd" {
		argv = append(argv, "--")
	}
	return execIn(p.name, "alice", lf(msg), append(argv, rcpts...)...)
}

// lookup returns the message in the user's Maildir on service that has
// the token, as the server stored it, and the file it is in.
func maildirLookup(service, user, token string) (raw []byte, file string, ok bool) {
	script := `f=$(grep -l -s -F -- "$1" /home/"$2"/Maildir/new/* | head -1); [ -n "$f" ] || exit 3; echo "$f"; cat "$f"`
	out, err := execIn(service, "", nil, "sh", "-c", script, "sh", token, user)
	if err != nil {
		return nil, "", false
	}
	file, rest, _ := strings.Cut(out, "\n")
	return []byte(rest), file, true
}

// find waits for the message with the token in the user's Maildir.
func (p peer) find(t testing.TB, user, token string) []byte {
	t.Helper()
	raw, _ := maildirFind(t, p.name, user, token)
	return raw
}

func maildirFind(t testing.TB, service, user, token string) (raw []byte, file string) {
	t.Helper()
	waitFor(t, "the message "+token+" in the Maildir of "+user+" on "+service, 60*time.Second, func() bool {
		var ok bool
		raw, file, ok = maildirLookup(service, user, token)
		return ok
	})
	return raw, file
}

// absent checks that the message has not arrived after waiting a while.
func maildirAbsent(t testing.TB, service, user, token string, wait time.Duration) {
	t.Helper()
	time.Sleep(wait)
	if _, file, ok := maildirLookup(service, user, token); ok {
		t.Errorf("%s: the message %s was delivered to %s (%s) and should not have been", service, token, user, file)
	}
}

// ---- messages ----

// header returns the unfolded values of a header field of a raw message.
func header(raw []byte, name string) []string {
	head, _, _ := bytes.Cut(lf(raw), []byte("\n\n"))
	var out []string
	var cur *string
	for _, line := range strings.Split(string(head), "\n") {
		if cur != nil && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			*cur += " " + strings.TrimSpace(line)
			continue
		}
		cur = nil
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(k, name) {
			out = append(out, strings.TrimSpace(v))
			cur = &out[len(out)-1]
		}
	}
	return out
}

// body is what follows the header, with LF line ends.
func body(raw []byte) string {
	_, b, _ := bytes.Cut(lf(raw), []byte("\n\n"))
	return string(b)
}

// rawMessage is a message with CRLF line ends and the token in X-Lab.
func rawMessage(token, from, to string, extra []string, text string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	hasSubject := false
	for _, h := range extra {
		if strings.HasPrefix(strings.ToLower(h), "subject:") {
			hasSubject = true
		}
		b.WriteString(h + "\r\n")
	}
	if !hasSubject {
		b.WriteString("Subject: lab " + token + "\r\n")
	}
	b.WriteString("Message-ID: <" + token + "@harness.lab.test>\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("X-Lab: " + token + "\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String())
}

// filler is n bytes of text in lines of 78 characters (80 with CRLF).
func filler(n int) string {
	line := strings.Repeat("0123456789abcdef", 5)[:78] + "\n"
	var b strings.Builder
	for b.Len()+len(line) <= n-n/40 { // LF becomes CRLF: 79 -> 80
		b.WriteString(line)
	}
	return b.String()
}

// ---- JMAP ----

var (
	rootsOnce sync.Once
	roots     *x509.CertPool
)

func labRoots(t testing.TB) *x509.CertPool {
	rootsOnce.Do(func() {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return
		}
		roots = x509.NewCertPool()
		roots.AppendCertsFromPEM(pem)
	})
	if roots == nil {
		t.Fatalf("the lab CA is not at %s; run contrib/lab/lab.sh up", caFile)
	}
	return roots
}

// jmap is a JMAP client for one user of robinmail, through the gate, over
// TLS checked against the lab CA.
type jmap struct {
	user    string
	account string
	hc      *http.Client
	box     map[string]string // role -> mailbox id

	mu    sync.Mutex
	blobs map[string][]byte // email id -> raw message
}

func newJMAP(t testing.TB, user string) *jmap {
	t.Helper()
	j := &jmap{user: user, blobs: map[string][]byte{}, box: map[string]string{}}
	j.hc = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: robinHost, RootCAs: labRoots(t)},
		DialContext:     (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	}}
	status, data := j.do(t, "GET", "/jmap/session", "", nil)
	if status != 200 {
		t.Fatalf("session of %s: %d %s", user, status, data)
	}
	var s struct {
		PrimaryAccounts map[string]string `json:"primaryAccounts"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	j.account = s.PrimaryAccounts["urn:ietf:params:jmap:mail"]
	res := j.call(t, []any{"Mailbox/get", map[string]any{"accountId": j.account}, "0"})
	for _, m := range res[0].args["list"].([]any) {
		mb := m.(map[string]any)
		if role, _ := mb["role"].(string); role != "" {
			j.box[role] = mb["id"].(string)
		}
	}
	for _, role := range []string{"inbox", "drafts", "sent"} {
		if j.box[role] == "" {
			t.Fatalf("%s has no %s mailbox", user, role)
		}
	}
	return j
}

func (j *jmap) do(t testing.TB, method, path, ctype string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "https://"+gateHTTPS+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = robinHost
	req.SetBasicAuth(j.user, password())
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := j.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

type response struct {
	name string
	args map[string]any
	tag  string
}

// call sends method calls, each as [name, arguments, tag].
func (j *jmap) call(t testing.TB, calls ...[]any) []response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"using":       []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:submission"},
		"methodCalls": calls,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, data := j.do(t, "POST", "/jmap/api", "application/json", body)
	if status != 200 {
		t.Fatalf("JMAP: %d %s", status, data)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("JMAP: %v in %s", err, data)
	}
	var res []response
	for _, r := range out.MethodResponses {
		var x response
		json.Unmarshal(r[0], &x.name)
		json.Unmarshal(r[1], &x.args)
		json.Unmarshal(r[2], &x.tag)
		res = append(res, x)
	}
	return res
}

func (j *jmap) upload(t testing.TB, data []byte, ctype string) string {
	t.Helper()
	status, out := j.do(t, "POST", "/jmap/upload/"+j.account+"/", ctype, data)
	if status != 201 {
		t.Fatalf("upload: %d %s", status, out)
	}
	var r struct {
		BlobID string `json:"blobId"`
	}
	json.Unmarshal(out, &r)
	return r.BlobID
}

func (j *jmap) download(t testing.TB, blob string) []byte {
	t.Helper()
	status, out := j.do(t, "GET", "/jmap/download/"+j.account+"/"+blob+"/m.eml?type=message/rfc822", "", nil)
	if status != 200 {
		t.Fatalf("download %s: %d %s", blob, status, out)
	}
	return out
}

// lookup returns the newest email in the mailbox with the role that has
// the token anywhere in it, and its id.
func (j *jmap) lookup(t testing.TB, role, token string) (id string, raw []byte, ok bool) {
	t.Helper()
	res := j.call(t,
		[]any{"Email/query", map[string]any{"accountId": j.account, "filter": map[string]any{"inMailbox": j.box[role]},
			"sort": []any{map[string]any{"property": "receivedAt", "isAscending": false}}, "limit": 60}, "0"},
		[]any{"Email/get", map[string]any{"accountId": j.account, "properties": []string{"blobId"},
			"#ids": map[string]any{"resultOf": "0", "name": "Email/query", "path": "/ids"}}, "1"})
	if len(res) < 2 || res[1].name != "Email/get" {
		t.Fatalf("Email/query: %v", res)
	}
	for _, e := range res[1].args["list"].([]any) {
		em := e.(map[string]any)
		id := em["id"].(string)
		j.mu.Lock()
		raw, have := j.blobs[id]
		j.mu.Unlock()
		if !have {
			raw = j.download(t, em["blobId"].(string))
			j.mu.Lock()
			j.blobs[id] = raw
			j.mu.Unlock()
		}
		if bytes.Contains(raw, []byte(token)) {
			return id, raw, true
		}
	}
	return "", nil, false
}

// find waits for the email with the token in the Inbox.
func (j *jmap) find(t testing.TB, token string) (id string, raw []byte) {
	t.Helper()
	waitFor(t, "the message "+token+" in the Inbox of "+j.user, 60*time.Second, func() bool {
		var ok bool
		id, raw, ok = j.lookup(t, "inbox", token)
		return ok
	})
	return id, raw
}

func (j *jmap) absent(t testing.TB, token string, wait time.Duration) {
	t.Helper()
	time.Sleep(wait)
	if id, _, ok := j.lookup(t, "inbox", token); ok {
		t.Errorf("the message %s is in the Inbox of %s (%s) and should not be", token, j.user, id)
	}
}

// get reads properties of one email.
func (j *jmap) get(t testing.TB, id string, props ...string) map[string]any {
	t.Helper()
	res := j.call(t, []any{"Email/get", map[string]any{"accountId": j.account, "ids": []string{id}, "properties": props}, "0"})
	list, _ := res[0].args["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get %s: %v", id, res[0].args)
	}
	return list[0].(map[string]any)
}

func envelope(from string, rcpts []string) map[string]any {
	to := []any{}
	for _, r := range rcpts {
		to = append(to, map[string]any{"email": r})
	}
	return map[string]any{"mailFrom": map[string]any{"email": from}, "rcptTo": to}
}

// submitted reads the id of the created submission out of the responses.
func (j *jmap) submitted(t testing.TB, res []response) string {
	t.Helper()
	for _, r := range res {
		if r.name != "EmailSubmission/set" {
			continue
		}
		if c, ok := r.args["created"].(map[string]any); ok {
			if s, ok := c["send"].(map[string]any); ok {
				return s["id"].(string)
			}
		}
	}
	var out []string
	for _, r := range res {
		args, _ := json.Marshal(r.args)
		out = append(out, r.name+" "+string(args))
	}
	t.Fatalf("the message was not submitted: %s", clip(strings.Join(out, "\n"), 1500))
	return ""
}

// sendCreate makes an Email with Email/set, as the webmail does, and
// submits it to the recipients. It returns the submission id.
func (j *jmap) sendCreate(t testing.TB, create map[string]any, rcpts ...string) string {
	t.Helper()
	create["mailboxIds"] = map[string]any{j.box["drafts"]: true}
	res := j.call(t,
		[]any{"Email/set", map[string]any{"accountId": j.account, "create": map[string]any{"draft": create}}, "0"},
		[]any{"EmailSubmission/set", map[string]any{"accountId": j.account,
			"create": map[string]any{"send": map[string]any{"identityId": "i1", "emailId": "#draft", "envelope": envelope(j.user, rcpts)}},
			"onSuccessUpdateEmail": map[string]any{"#send": map[string]any{
				"mailboxIds/" + j.box["drafts"]: nil, "mailboxIds/" + j.box["sent"]: true}}}, "1"})
	return j.submitted(t, res)
}

// sendRaw imports a message as it is and submits it.
func (j *jmap) sendRaw(t testing.TB, raw []byte, rcpts ...string) string {
	t.Helper()
	return j.submitted(t, j.sendRawRes(t, raw, rcpts...))
}

func (j *jmap) sendRawRes(t testing.TB, raw []byte, rcpts ...string) []response {
	t.Helper()
	blob := j.upload(t, raw, "message/rfc822")
	return j.call(t,
		[]any{"Email/import", map[string]any{"accountId": j.account, "emails": map[string]any{
			"draft": map[string]any{"blobId": blob, "mailboxIds": map[string]any{j.box["sent"]: true}}}}, "0"},
		[]any{"EmailSubmission/set", map[string]any{"accountId": j.account,
			"create": map[string]any{"send": map[string]any{"identityId": "i1", "emailId": "#draft", "envelope": envelope(j.user, rcpts)}}}, "1"})
}

// status is the DeliveryStatus of one recipient of a submission.
type status struct{ delivered, reply string }

func (j *jmap) statuses(t testing.TB, sub string) map[string]status {
	t.Helper()
	res := j.call(t, []any{"EmailSubmission/get", map[string]any{"accountId": j.account, "ids": []string{sub}}, "0"})
	list, _ := res[0].args["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("EmailSubmission/get %s: %v", sub, res[0].args)
	}
	out := map[string]status{}
	ds, _ := list[0].(map[string]any)["deliveryStatus"].(map[string]any)
	for addr, v := range ds {
		m := v.(map[string]any)
		d, _ := m["delivered"].(string)
		r, _ := m["smtpReply"].(string)
		out[addr] = status{d, r}
	}
	return out
}

// waitStatus waits until the recipient's "delivered" is one of want.
func (j *jmap) waitStatus(t testing.TB, sub, rcpt string, want ...string) status {
	t.Helper()
	var st status
	waitFor(t, fmt.Sprintf("deliveryStatus of %s to become %v", rcpt, want), 60*time.Second, func() bool {
		st = j.statuses(t, sub)[rcpt]
		for _, w := range want {
			if st.delivered == w {
				return true
			}
		}
		return false
	})
	return st
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- SMTP by hand ----

// smtpConn is a bare SMTP client for the things no mail server sends.
type smtpConn struct {
	c net.Conn
	r *bufio.Reader
}

type smtpReply struct {
	code int
	text string
}

func (r smtpReply) String() string { return strconv.Itoa(r.code) + " " + r.text }

func dialSMTP(t testing.TB, addr string) *smtpConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("connecting to %s: %v", addr, err)
	}
	s := &smtpConn{c: c, r: bufio.NewReader(c)}
	t.Cleanup(func() { c.Close() })
	if rep := s.read(t); rep.code != 220 {
		t.Fatalf("greeting of %s: %s", addr, rep)
	}
	if rep := s.cmd(t, "EHLO harness.lab.test"); rep.code != 250 {
		t.Fatalf("EHLO to %s: %s", addr, rep)
	}
	return s
}

func (s *smtpConn) read(t testing.TB) smtpReply {
	t.Helper()
	s.c.SetReadDeadline(time.Now().Add(30 * time.Second))
	var lines []string
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading a reply: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 3 {
			t.Fatalf("short reply %q", line)
		}
		if len(line) > 4 {
			lines = append(lines, line[4:])
		}
		if len(line) == 3 || line[3] == ' ' {
			code, _ := strconv.Atoi(line[:3])
			return smtpReply{code, strings.Join(lines, " | ")}
		}
	}
}

func (s *smtpConn) cmd(t testing.TB, line string) smtpReply {
	t.Helper()
	s.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(s.c, line+"\r\n"); err != nil {
		t.Fatalf("writing %q: %v", line, err)
	}
	return s.read(t)
}

// data sends a message after a 354 and returns the reply to it.
func (s *smtpConn) data(t testing.TB, msg []byte) smtpReply {
	t.Helper()
	if rep := s.cmd(t, "DATA"); rep.code != 354 {
		return rep
	}
	msg = crlf(msg)
	if !bytes.HasSuffix(msg, []byte("\r\n")) {
		msg = append(msg, "\r\n"...)
	}
	s.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := s.c.Write(append(msg, ".\r\n"...)); err != nil {
		t.Fatalf("writing the message: %v", err)
	}
	return s.read(t)
}

// ---- robinmail's SMTP trace ----

// traceLine is one line of robinmail's SMTP trace (dev.smtp_trace).
type traceLine struct {
	side   string // "server": robinmail was connected to; "client": it connected
	remote string // the other side's address
	dir    string // "C" for a command, "S" for a reply
	line   string
	more   bool // server side: the next command had already arrived
}

var attr = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S*)`)

// trace returns the SMTP trace lines robinmail logged since a moment.
func trace(t testing.TB, since time.Time) []traceLine {
	t.Helper()
	out := compose(t, "logs", "--no-log-prefix", "--since", since.UTC().Format(time.RFC3339), "robinmail")
	var lines []traceLine
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, `msg="smtp trace"`) {
			continue
		}
		var tl traceLine
		for _, m := range attr.FindAllStringSubmatch(l, -1) {
			v := m[2]
			if strings.HasPrefix(v, `"`) {
				if u, err := strconv.Unquote(v); err == nil {
					v = u
				}
			}
			switch m[1] {
			case "side":
				tl.side = v
			case "remote":
				tl.remote = v
			case "dir":
				tl.dir = v
			case "line":
				tl.line = v
			case "more":
				tl.more = v == "true"
			}
		}
		lines = append(lines, tl)
	}
	return lines
}

// commands returns the trace lines of one side with one remote address
// that are commands starting with verb.
func commands(lines []traceLine, side, remote, verb string) []traceLine {
	var out []traceLine
	for _, l := range lines {
		if l.side == side && l.remote == remote && l.dir == "C" && strings.HasPrefix(strings.ToUpper(l.line), strings.ToUpper(verb)) {
			out = append(out, l)
		}
	}
	return out
}

// ---- the matrix ----

// The result of every case, for the table printed at the end.
var matrix = struct {
	sync.Mutex
	rows  []string
	cells map[string]map[string]string
}{cells: map[string]map[string]string{}}

// cell records the outcome of the running test as the cell (row, col):
// pass, FAIL or skip, or what note says once the test passed.
func cell(t *testing.T, row, col string) (note func(string)) {
	var text string
	t.Cleanup(func() {
		res := "pass"
		switch {
		case t.Failed():
			res = "FAIL"
		case t.Skipped():
			res = "n/a"
		case text != "":
			res = text
		}
		matrix.Lock()
		defer matrix.Unlock()
		if matrix.cells[row] == nil {
			matrix.cells[row] = map[string]string{}
			matrix.rows = append(matrix.rows, row)
		}
		matrix.cells[row][col] = res
	})
	return func(s string) { text = s }
}

func printMatrix(w io.Writer) {
	matrix.Lock()
	defer matrix.Unlock()
	if len(matrix.rows) == 0 {
		return
	}
	cols := map[string]bool{}
	for _, c := range matrix.cells {
		for k := range c {
			cols[k] = true
		}
	}
	var order []string
	for _, p := range peers {
		if cols[p.name] {
			order = append(order, p.name)
			delete(cols, p.name)
		}
	}
	var rest []string
	for k := range cols {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	order = append(order, rest...)
	sort.Strings(matrix.rows)
	wide := 0
	for _, r := range matrix.rows {
		wide = max(wide, len(r))
	}
	fmt.Fprintf(w, "\n%-*s", wide+2, "case")
	for _, c := range order {
		fmt.Fprintf(w, "%-12s", c)
	}
	fmt.Fprintln(w)
	for _, r := range matrix.rows {
		fmt.Fprintf(w, "%-*s", wide+2, r)
		for _, c := range order {
			v := matrix.cells[r][c]
			if v == "" {
				v = "-"
			}
			fmt.Fprintf(w, "%-12s", v)
		}
		fmt.Fprintln(w)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	printMatrix(os.Stdout)
	os.Exit(code)
}

// lab skips the test unless the lab is asked for, and checks it is there.
func lab(t *testing.T) {
	t.Helper()
	if os.Getenv("ROBINMAIL_TEST_LAB") == "" {
		t.Skip("set ROBINMAIL_TEST_LAB=1 and start the lab (contrib/lab/lab.sh up, or make lab-test)")
	}
	c, err := net.DialTimeout("tcp", gateHTTPS, 3*time.Second)
	if err != nil {
		t.Fatalf("the lab does not answer on %s: %v; run contrib/lab/lab.sh up", gateHTTPS, err)
	}
	c.Close()
}

// observe writes a cell that is a finding, not a verdict.
func observe(row, col, value string) {
	matrix.Lock()
	defer matrix.Unlock()
	if matrix.cells[row] == nil {
		matrix.cells[row] = map[string]string{}
		matrix.rows = append(matrix.rows, row)
	}
	matrix.cells[row][col] = value
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
