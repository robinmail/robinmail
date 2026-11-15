package clustertest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// selfSigned is a certificate of somebody's own: not a member's.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "robinmail node"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// nodeAPI calls node 1's API for the other nodes from inside node 2's
// container (on the network the nodes share), with the given client
// certificates, and returns the status or the error.
func nodeAPI(certs []tls.Certificate, method, path string) (int, error) {
	hc := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			raw, err := dialIn(robin[1].service, "robinmail-1", "7443")
			if err != nil {
				return nil, err
			}
			conn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, Certificates: certs, MinVersion: tls.VersionTLS13})
			if err := conn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return conn, nil
		},
	}}
	req, err := http.NewRequest(method, "https://robinmail-1:7443"+path, strings.NewReader(""))
	if err != nil {
		return 0, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// (k) The database has no port of its own. Its Raft messages, snapshots
// and member list are on the node API, which takes only the certificates
// of members: without a certificate, and with one of somebody's own, no
// path answers. The control listener for the commands of a node is on
// that node's loopback and cannot be reached from another container.
func databaseClosed(t *testing.T) {
	paths := [][2]string{{"POST", "/v1/raft/message"}, {"GET", "/v1/raft/snapshot"}, {"GET", "/v1/raft/members"},
		{"GET", "/v1/ping"}, {"GET", "/v1/blob/x/y"}}
	for _, who := range []struct {
		name  string
		certs []tls.Certificate
	}{{"no certificate", nil}, {"a certificate of its own", []tls.Certificate{selfSigned(t)}}} {
		for _, p := range paths {
			code, err := nodeAPI(who.certs, p[0], p[1])
			if err == nil {
				t.Errorf("%s: %s %s answered %d", who.name, p[0], p[1], code)
			}
		}
	}
	// The data are still the cluster's: nothing above changed membership.
	if got := membersUp(); got != 3 {
		t.Errorf("%d of 3 nodes in the cluster afterwards", got)
	}

	// The control listener: where node 1's server listens for its own
	// commands, and that nobody from another container gets there.
	addr := sh(t, 0, "cat /var/lib/robinmail/db/control")
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("control address %q: not on loopback (%v)", addr, err)
	}
	conn, err := dialIn(robin[1].service, "robinmail-1", port)
	if err == nil {
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 1)
		conn.Write([]byte{0x16, 3, 1, 0, 0})
		n, _ := conn.Read(buf)
		conn.Close()
		if n > 0 {
			t.Errorf("the control port %s of robinmail-1 answered from robinmail-2", port)
		}
	}
	// On node 1 itself only its own certificate gets in: robinmail's
	// commands there work (they went through it all along).
	if _, err := robinmailIn(0, "db", "query", "SELECT 1"); err != nil {
		t.Errorf("a command on node 1: %v", err)
	}
}

func clipTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
