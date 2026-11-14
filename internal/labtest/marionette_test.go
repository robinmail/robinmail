package labtest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// marionette is a client of Marionette, the remote control protocol of
// Mozilla's programs (Thunderbird is started with --marionette and listens
// on 127.0.0.1:2828 in its container). The protocol is small: after the
// server's hello every message is "<length>:<JSON>", a command is
// [0, id, name, parameters] and its answer [1, id, error, result].
//
// The connection is the standard input and output of a socat that
// "docker compose exec" runs next to Thunderbird, so the port never leaves
// the container.
type marionette struct {
	mu  sync.Mutex
	cmd *exec.Cmd
	w   io.WriteCloser
	r   *bufio.Reader
	id  int
}

// marionetteError is an error Thunderbird answered a command with.
type marionetteError struct {
	Kind    string `json:"error"`
	Message string `json:"message"`
	Stack   string `json:"stacktrace"`
}

func (e *marionetteError) Error() string { return e.Kind + ": " + e.Message }

func dialMarionette() (*marionette, error) {
	cmd := exec.Command("docker", "compose", "-f", composeFile, "exec", "-T", thunderbird, "socat", "STDIO", "TCP:127.0.0.1:2828")
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	m := &marionette{cmd: cmd, w: w, r: bufio.NewReader(out)}
	// The hello: {"applicationType":"gecko","marionetteProtocol":3}.
	hello, err := m.read()
	if err != nil {
		m.close()
		return nil, fmt.Errorf("no hello from Marionette: %w", err)
	}
	var h struct {
		Protocol int `json:"marionetteProtocol"`
	}
	if json.Unmarshal(hello, &h) != nil || h.Protocol != 3 {
		m.close()
		return nil, fmt.Errorf("not Marionette protocol 3: %s", hello)
	}
	// A dialog that Thunderbird opens stays open until a test looks at it:
	// by default Marionette dismisses it and fails the next command.
	if _, err := m.send("WebDriver:NewSession", map[string]any{"unhandledPromptBehavior": "ignore"}); err != nil {
		m.close()
		return nil, err
	}
	// Scripts run in Thunderbird's own windows, not in a web page.
	if _, err := m.send("Marionette:SetContext", map[string]any{"value": "chrome"}); err != nil {
		m.close()
		return nil, err
	}
	return m, nil
}

func (m *marionette) close() {
	m.w.Close()
	m.cmd.Process.Kill()
	m.cmd.Wait()
}

func (m *marionette) read() ([]byte, error) {
	head, err := m.r.ReadString(':')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSuffix(head, ":"))
	if err != nil || n < 0 || n > 64<<20 {
		return nil, fmt.Errorf("a frame of length %q", head)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(m.r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// send runs one command and returns its result.
func (m *marionette) send(name string, params any) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.id++
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal([]any{0, m.id, name, params})
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(m.w, "%d:%s", len(body), body); err != nil {
		return nil, err
	}
	for {
		frame, err := m.read()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		var msg []json.RawMessage
		if err := json.Unmarshal(frame, &msg); err != nil || len(msg) != 4 {
			return nil, fmt.Errorf("%s: answer %s", name, clip(string(frame), 300))
		}
		var id int
		json.Unmarshal(msg[1], &id)
		if id != m.id {
			continue
		}
		if string(msg[2]) != "null" {
			var me marionetteError
			json.Unmarshal(msg[2], &me)
			return nil, &me
		}
		return msg[3], nil
	}
}

// script runs JavaScript with the privileges of Thunderbird's own code, in
// its main window, and returns what the script returns as JSON. The source
// is the body of an async function, which is waited for up to limit. args are its "arguments".
func (m *marionette) script(limit time.Duration, js string, args ...any) (json.RawMessage, error) {
	if args == nil {
		args = []any{}
	}
	if _, err := m.send("WebDriver:SetTimeouts", map[string]any{"script": limit.Milliseconds()}); err != nil {
		return nil, err
	}
	res, err := m.send("WebDriver:ExecuteScript", map[string]any{
		"script": "return (async function () {\n" + js + "\n}).apply(null, arguments)", "args": args, "sandbox": "system", "newSandbox": true})
	if err != nil {
		return nil, err
	}
	var v struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(res, &v); err != nil {
		return nil, err
	}
	return v.Value, nil
}

// TestThunderbirdEval is a tool, not a test: it runs the script in the
// file ROBINMAIL_TB_EVAL in the Thunderbird that is running in the lab,
// after the library of thunderbird.js, and prints what it returns. For
// finding out what Thunderbird's interfaces do before writing a test:
//
//	ROBINMAIL_TEST_LAB=1 ROBINMAIL_TB_KEEP=1 go test -run TestThunderbirdReconnect ./internal/labtest
//	echo 'return folders();' > /tmp/a.js
//	ROBINMAIL_TB_EVAL=/tmp/a.js go test -v -run TestThunderbirdEval ./internal/labtest
func TestThunderbirdEval(t *testing.T) {
	file := os.Getenv("ROBINMAIL_TB_EVAL")
	if file == "" {
		t.Skip("ROBINMAIL_TB_EVAL is not set")
	}
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	m, err := dialMarionette()
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	lib, _ := os.ReadFile("thunderbird.js")
	out, err := m.script(evalLimit(), string(lib)+"\n"+string(src))
	var me *marionetteError
	if errors.As(err, &me) {
		t.Fatalf("%v\n%s", me, me.Stack)
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("%s\n", out)
}

const thunderbird = "thunderbird"

// evalLimit is how long TestThunderbirdEval waits for its script:
// ROBINMAIL_TB_EVAL_SECONDS, or 20 seconds.
func evalLimit() time.Duration {
	if n, err := strconv.Atoi(os.Getenv("ROBINMAIL_TB_EVAL_SECONDS")); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 20 * time.Second
}
