package smtp

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Server.Trace (dev.smtp_trace) logs commands and replies. It must show
// what the interoperability lab reads from it, and must not show what no
// log may hold: credentials and mail.
func TestTraceShowsCommandsAndNoSecrets(t *testing.T) {
	var out syncBuffer
	srv := &Server{Handler: &owner{}, Hostname: "smtp.example.com", TLSConfig: testTLSConfig(t),
		Auth: passwords{"test": "1234", "tim": "tanstaaftanstaaf"}, Trace: true,
		Logger: slog.New(slog.NewTextHandler(&out, nil))}
	c := secure(t, serveWith(t, srv))
	c.cmd("AUTH PLAIN "+b64("\x00tim\x00tanstaaftanstaaf"), "235")
	c.send("MAIL FROM:<tim@example.com> SIZE=40\r\nRCPT TO:<a@b.test>\r\nDATA\r\n")
	c.expect("250")
	c.expect("250")
	c.expect("354")
	c.cmd("Subject: the message itself\r\n\r\nnot for the log\r\n.", "250")
	c.cmd("QUIT", "221")

	// A second session, with the two-step forms.
	c = secure(t, serveWith(t, srv))
	c.cmd("AUTH LOGIN", "334")
	c.cmd(b64("test"), "334")
	c.cmd(b64("1234"), "235")
	c.cmd("QUIT", "221")
	log := out.String()

	for _, want := range []string{
		`dir=S line="220 smtp.example.com ESMTP robinmail"`,
		`dir=C line="EHLO client.example.com"`,
		`dir=S line="250-SIZE 26214400"`,
		`dir=C line="AUTH PLAIN ..."`,
		`dir=C line="AUTH LOGIN ..."`,
		`dir=S line="235 2.7.0 Authentication successful"`,
		// more=true: the next command was already there, a pipelining client.
		`dir=C line="MAIL FROM:<tim@example.com> SIZE=40" more=true`,
		`dir=C line="RCPT TO:<a@b.test>" more=true`,
		`dir=C line=DATA more=false`,
		`dir=C line=QUIT more=false`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the trace lacks %s", want)
		}
	}
	for _, secret := range []string{b64("\x00tim\x00tanstaaftanstaaf"), "tanstaaf", b64("1234"), b64("test"), "not for the log", "the message itself"} {
		if strings.Contains(log, secret) {
			t.Errorf("the trace has %q", secret)
		}
	}

	// Without Trace nothing of the kind is logged.
	out.b.Reset()
	srv2 := &Server{Handler: &recorder{}, Logger: slog.New(slog.NewTextHandler(&out, nil))}
	c = dial(t, serveWith(t, srv2))
	c.cmd("EHLO client.example.com", "250")
	c.cmd("QUIT", "221")
	if strings.Contains(out.String(), "smtp trace") {
		t.Errorf("traced without Trace:\n%s", out.String())
	}
}
