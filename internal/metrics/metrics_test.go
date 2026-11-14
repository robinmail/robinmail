package metrics

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// The text format is Prometheus' exposition format 0.0.4 ("Text-based
// format" in its documentation): lines "# HELP <name> <text>" and
// "# TYPE <name> <type>" before the samples of a metric, a sample as
// name, optional {label="value",...}, a space and a float; in HELP text
// backslash and newline are escaped, in a label value also the double
// quote; all lines of one metric together, each ending in a line feed.
func TestTextFormat(t *testing.T) {
	r := New()
	hits := r.Counter("http_requests_total", "The total number of HTTP requests.", "method", "code")
	hits.Add(1027, "post", "200")
	hits.Add(3, "post", "400")
	hits.Inc("post", "400")
	plain := r.Counter("plain_total", "A counter without labels.")
	g := r.Gauge("queue_messages", `Help with a \ and a
newline.`, "state")
	g.Set(5, "due")
	g.Add(-2, "due")
	g.Set(0.5, `odd "state"\with
newline`)
	r.Func("computed", "Asked when written.", []string{"node"}, func() []Sample {
		return []Sample{{[]string{"b"}, 2}, {[]string{"a"}, 1}}
	})
	r.Func("nothing", "Without samples it is left out.", nil, func() []Sample { return nil })
	_ = plain

	var out bytes.Buffer
	if err := r.WriteText(&out); err != nil {
		t.Fatal(err)
	}
	want := `# HELP computed Asked when written.
# TYPE computed gauge
computed{node="a"} 1
computed{node="b"} 2
# HELP http_requests_total The total number of HTTP requests.
# TYPE http_requests_total counter
http_requests_total{method="post",code="200"} 1027
http_requests_total{method="post",code="400"} 4
# HELP plain_total A counter without labels.
# TYPE plain_total counter
plain_total 0
# HELP queue_messages Help with a \\ and a\nnewline.
# TYPE queue_messages gauge
queue_messages{state="due"} 3
queue_messages{state="odd \"state\"\\with\nnewline"} 0.5
`
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}
	// Every line is a comment or a sample of the documented shape.
	line := regexp.MustCompile(`^(# (HELP|TYPE) [a-zA-Z_:][a-zA-Z0-9_:]* .+|[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-zA-Z_][a-zA-Z0-9_]*="([^"\\\n]|\\\\|\\"|\\n)*",?)+\})? [-+0-9.eEInfNa]+)$`)
	for _, l := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if !line.MatchString(l) {
			t.Errorf("not a line of the format: %q", l)
		}
	}
}

func TestMisuse(t *testing.T) {
	r := New()
	c := r.Counter("ok_total", "x", "a")
	for name, f := range map[string]func(){
		"a bad name":           func() { r.Counter("0bad", "x") },
		"a bad label":          func() { r.Counter("x_total", "x", "__reserved") },
		"a name twice":         func() { r.Gauge("ok_total", "x") },
		"too few label values": func() { c.Inc() },
		"a counter going down": func() { c.Add(-1, "v") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", name)
				}
			}()
			f()
		}()
	}
}

func TestConcurrent(t *testing.T) {
	r := New()
	c := r.Counter("n_total", "x", "who")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Inc("a")
				var b bytes.Buffer
				r.WriteText(&b)
			}
		}()
	}
	wg.Wait()
	var b bytes.Buffer
	r.WriteText(&b)
	if !strings.Contains(b.String(), `n_total{who="a"} 5000`+"\n") {
		t.Errorf("after 5000 increments:\n%s", b.String())
	}
}

// The names the server registers are valid and say what they count.
func TestDefaultRegistry(t *testing.T) {
	Delivered.Add(0)
	var b bytes.Buffer
	if err := Default.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"robinmail_smtp_connections_total", "robinmail_delivered_total", "robinmail_queue_attempts_total"} {
		if !strings.Contains(b.String(), "# TYPE "+name+" counter\n"+name+" ") {
			t.Errorf("%s is not in:\n%s", name, b.String())
		}
	}
}
