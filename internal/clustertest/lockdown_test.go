package clustertest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The network the cluster's nodes share, as compose names it.
const clusterNetwork = "robinmail-cluster_cluster"

// rogueCert writes a certificate and key for an rqlite node called
// "rogue" into dir: self-signed, or signed by the cluster's own CA when
// byCA is set (which only somebody with the CA's key can do).
func rogueCert(t *testing.T, dir string, byCA bool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "rogue"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames: []string{"rogue"}, BasicConstraintsValid: true,
	}
	parent, signer := tmpl, any(key)
	if byCA {
		caPEM, err := os.ReadFile(secretsDir + "/rqlite/ca.crt")
		if err != nil {
			t.Fatal(err)
		}
		keyPEM, err := os.ReadFile(secretsDir + "/rqlite-ca.key")
		if err != nil {
			t.Fatal(err)
		}
		cb, _ := pem.Decode(caPEM)
		kb, _ := pem.Decode(keyPEM)
		if parent, err = x509.ParseCertificate(cb.Bytes); err != nil {
			t.Fatal(err)
		}
		if signer, err = x509.ParsePKCS8PrivateKey(kb.Bytes); err != nil {
			t.Fatal(err)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	// Read by rqlite's own user inside a container, through a mount.
	os.WriteFile(filepath.Join(dir, "rogue.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(dir, "rogue.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o644)
}

// rogueJoin starts an rqlite node of somebody's own on the network the
// cluster shares and lets it try to join. It returns what the node logged.
func rogueJoin(t *testing.T, name string, flags ...string) string {
	t.Helper()
	dir, err := os.MkdirTemp(secretsDir, "rogue-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o755)
	t.Cleanup(func() { os.RemoveAll(dir) })
	abs, _ := filepath.Abs(dir)
	switch name {
	case "self-signed":
		rogueCert(t, dir, false)
	case "signed by the CA":
		rogueCert(t, dir, true)
	}
	container := fmt.Sprintf("robinmail-cluster-rogue-%d", time.Now().UnixNano()%1_000_000)
	args := append([]string{"run", "-d", "--name", container, "--network", clusterNetwork, "--hostname", "rogue", "-v", abs + ":/rogue:ro",
		"rqlite/rqlite:10.7.1", "-node-id", "rogue", "-raft-adv-addr", "rogue:4002",
		"-join", "rqlite-1:4002,rqlite-2:4002,rqlite-3:4002", "-join-attempts", "3", "-join-interval", "1s"}, flags...)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", container).Run() })
	// It gives up after its attempts and exits; wait for that.
	waitFor(t, "the rogue node ("+name+") to give up", 60*time.Second, func() bool {
		out, _ := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", container).Output()
		return strings.TrimSpace(string(out)) == "false"
	})
	logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
	return string(logs)
}

// (k) Whoever reaches rqlite's HTTP port without a login owns every
// account, and whoever can join the cluster gets a copy of the database.
// Here neither works: the API is not on the network the nodes share, it
// wants a login where it is, the robinmail login cannot touch the
// cluster's membership, and an rqlite node without a certificate of the
// cluster's CA, or with one and without the login to join, stays out.
func rqliteClosed(t *testing.T) {
	// 1. From a container on the shared network (robinmail-2 is on it,
	// and not on the network of rqlite-1 and robinmail-1): the API of
	// rqlite-1 is not there to connect to.
	for _, target := range []string{"rqlite-1 4001", "rqlite-1-db 4001"} {
		out, err := composeErr("exec", "-T", robin[1].service, "sh", "-c", "echo | nc -w 3 "+target)
		if err == nil {
			t.Errorf("from %s, nc %s connected:\n%s", robin[1].service, target, out)
		} else {
			t.Logf("from %s: nc %s: %s", robin[1].service, target, strings.TrimSpace(out+" "+err.Error()))
		}
	}
	// The Raft port is there, for the other rqlite nodes. Without a
	// certificate of the CA nothing is said on it.
	raw, err := dialIn(robin[1].service, "rqlite-1", "4002")
	if err != nil {
		t.Fatal(err)
	}
	conn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	conn.Write([]byte{1}) // the first byte of rqlite's own protocol
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	raw.Close()
	if err == nil {
		t.Errorf("the Raft port answered a client without a certificate: %q", buf[:n])
	} else {
		t.Logf("the Raft port to a client without a certificate: %v", err)
	}

	// 2. From where the API is (robinmail-1's side of the network),
	// without a login and with a wrong one: no read, no write, nothing
	// about the cluster.
	rmUser, rmPass, err := login("rqlite.auth")
	if err != nil {
		t.Fatal(err)
	}
	write := []byte(`["CREATE TABLE intruder (x)"]`)
	for _, who := range []struct{ name, user, pass string }{{"no login", "", ""}, {"a wrong password", rmUser, "not-the-password"}, {"an unknown user", "root", rmPass}} {
		for _, r := range []struct {
			method, path string
			body         []byte
		}{{"GET", "/db/query?q=SELECT+address+FROM+account", nil}, {"POST", "/db/execute", write}, {"POST", "/db/request", write},
			{"GET", "/db/backup", nil}, {"GET", "/nodes", nil}, {"GET", "/status", nil}, {"DELETE", "/remove", []byte(`{"id":"2"}`)}} {
			code, data, err := rqliteDoAs(robin[0].service, 0, r.method, r.path, who.user, who.pass, r.body)
			if err != nil || code != 401 {
				t.Errorf("%s: %s %s: HTTP %d %v %s", who.name, r.method, r.path, code, err, data)
			}
			if strings.Contains(string(data), "@"+domain) {
				t.Errorf("%s: %s %s gave data away: %s", who.name, r.method, r.path, data)
			}
		}
	}
	if rows := rqliteRows(t, "SELECT count(*) FROM sqlite_master WHERE name = 'intruder'"); fmt.Sprint(rows[0][0]) != "0" {
		t.Error("a write without a login was applied")
	}
	// robinmail's own login reads and writes, and that is all: it cannot
	// look at or change who is in the cluster.
	code, data, err := rqliteDoAs(robin[0].service, 0, "POST", "/db/request", rmUser, rmPass, []byte(`["SELECT count(*) FROM account"]`))
	if err != nil || code != 200 || !strings.Contains(string(data), `"values"`) {
		t.Errorf("robinmail's login, a read: HTTP %d %v %s", code, err, data)
	}
	for _, r := range [][2]string{{"GET", "/nodes"}, {"GET", "/status"}, {"DELETE", "/remove"}} {
		code, data, err := rqliteDoAs(robin[0].service, 0, r[0], r[1], rmUser, rmPass, []byte(`{"id":"2"}`))
		if err != nil || code != 401 {
			t.Errorf("robinmail's login: %s %s: HTTP %d %v %s", r[0], r[1], code, err, data)
		}
	}
	t.Logf("without a login, with a wrong one and as an unknown user: 401 for query, execute, request, backup, nodes, status and remove; robinmail's login reads and writes and gets 401 for nodes, status and remove")

	// 3. Nodes that try to join.
	since := time.Now().Add(-time.Second)
	for _, r := range []struct {
		name  string
		flags []string
		want  string // what its log has to say
	}{
		{"no TLS", nil, ""},
		{"self-signed", []string{"-node-cert", "/rogue/rogue.crt", "-node-key", "/rogue/rogue.key", "-node-no-verify"}, ""},
		// The right certificate and no login: what a stolen node
		// certificate alone gets.
		// (It does not check the cluster's certificates, which is not
		// the point here.)
		{"signed by the CA", []string{"-node-cert", "/rogue/rogue.crt", "-node-key", "/rogue/rogue.key", "-node-no-verify"}, "unauthorized"},
	} {
		logs := rogueJoin(t, r.name, r.flags...)
		var said []string
		shown := false
		for _, l := range strings.Split(logs, "\n") {
			if strings.Contains(l, "join") && strings.Contains(l, "fail") {
				said = append(said, l)
			}
			if r.want != "" && !shown && strings.Contains(strings.ToLower(l), r.want) {
				shown = true
				t.Logf("a node with %s: %s", r.name, clipTail(l, 300))
			}
		}
		if len(said) == 0 {
			t.Errorf("a node with %s: its log does not say the join failed:\n%s", r.name, clipTail(logs, 2000))
		} else {
			t.Logf("a node with %s: %s", r.name, clipTail(said[len(said)-1], 300))
		}
		if r.want != "" && !strings.Contains(strings.ToLower(logs), r.want) {
			t.Errorf("a node with %s: its log does not say %q:\n%s", r.name, r.want, clipTail(logs, 2000))
		}
		nodes, err := rqliteNodes()
		if err != nil || len(nodes) != 3 {
			t.Errorf("after a node with %s tried: the cluster has %d nodes (%v), want 3", r.name, len(nodes), err)
		}
		for id := range nodes {
			if id != "1" && id != "2" && id != "3" {
				t.Errorf("after a node with %s tried: node %q is in the cluster", r.name, id)
			}
		}
	}
	// What the cluster's side saw of them.
	out := compose(t, "logs", "--no-log-prefix", "--since", since.UTC().Format(time.RFC3339), "rqlite-1", "rqlite-2", "rqlite-3")
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		for _, sign := range []string{"first record does not look like a TLS handshake", "bad certificate", "unknown certificate authority", "certificate signed by unknown authority", "unauthorized", "not authorized"} {
			if strings.Contains(l, sign) && !seen[sign] {
				seen[sign] = true
				t.Logf("the cluster's rqlite: %s", clipTail(l, 300))
			}
		}
	}
	if len(seen) == 0 {
		t.Error("the cluster's rqlite nodes logged nothing about the refused nodes")
	}
	if got := rqliteReachable(); got != 3 {
		t.Errorf("%d of 3 rqlite nodes reachable afterwards", got)
	}
}

func clipTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
