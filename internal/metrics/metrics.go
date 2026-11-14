// Package metrics counts what the server does and writes the counts in the
// text format Prometheus reads (version 0.0.4 of its exposition format):
//
//	# HELP name what it counts
//	# TYPE name counter
//	name{label="value"} 12
//
// It has counters, gauges, and functions that are asked when the text is
// written. There is one registry, Default, which the packages add to; the
// server serves it on a loopback listener.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Sample is one line of a metric: the values of its labels and the number.
type Sample struct {
	Labels []string
	Value  float64
}

type metric struct {
	name, help, kind string
	labels           []string
	mu               sync.Mutex
	values           map[string]*Sample // by the label values joined
	fn               func() []Sample
}

// Registry is a set of metrics.
type Registry struct {
	mu      sync.Mutex
	metrics map[string]*metric
}

// New returns an empty registry.
func New() *Registry { return &Registry{metrics: map[string]*metric{}} }

// Default is the registry of the process.
var Default = New()

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if !(c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (r *Registry) add(name, help, kind string, labels []string, fn func() []Sample) *metric {
	if !validName(name) {
		panic("metrics: bad name " + strconv.Quote(name))
	}
	for _, l := range labels {
		if !validName(l) || strings.Contains(l, ":") || strings.HasPrefix(l, "__") {
			panic("metrics: bad label " + strconv.Quote(l))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.metrics[name]; ok {
		panic("metrics: " + name + " registered twice")
	}
	m := &metric{name: name, help: help, kind: kind, labels: labels, values: map[string]*Sample{}, fn: fn}
	r.metrics[name] = m
	return m
}

func (m *metric) change(delta float64, set bool, values []string) {
	if len(values) != len(m.labels) {
		panic(fmt.Sprintf("metrics: %s has %d labels, got %d values", m.name, len(m.labels), len(values)))
	}
	key := strings.Join(values, "\x00")
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.values[key]
	if s == nil {
		s = &Sample{Labels: append([]string(nil), values...)}
		m.values[key] = s
	}
	if set {
		s.Value = delta
	} else {
		s.Value += delta
	}
}

// Counter is a number that only goes up.
type Counter struct{ m *metric }

// Counter registers a counter with the given label names. A counter
// without labels shows as 0 before its first Add.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	m := r.add(name, help, "counter", labels, nil)
	if len(labels) == 0 {
		m.change(0, false, nil)
	}
	return &Counter{m}
}

// Add adds n, which must not be negative, for the given label values.
func (c *Counter) Add(n float64, values ...string) {
	if n < 0 {
		panic("metrics: a counter cannot go down")
	}
	c.m.change(n, false, values)
}

// Inc adds one.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

// Gauge is a number that goes up and down.
type Gauge struct{ m *metric }

func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	m := r.add(name, help, "gauge", labels, nil)
	if len(labels) == 0 {
		m.change(0, false, nil)
	}
	return &Gauge{m}
}

func (g *Gauge) Set(v float64, values ...string) { g.m.change(v, true, values) }
func (g *Gauge) Add(n float64, values ...string) { g.m.change(n, false, values) }

// Func registers a gauge whose samples are computed when the text is
// written. fn must be quick and safe to call from any goroutine.
func (r *Registry) Func(name, help string, labels []string, fn func() []Sample) {
	r.add(name, help, "gauge", labels, fn)
}

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// WriteText writes every metric, sorted by name and then by label values.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	list := make([]*metric, 0, len(r.metrics))
	for _, m := range r.metrics {
		list = append(list, m)
	}
	r.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	var b strings.Builder
	for _, m := range list {
		var samples []Sample
		if m.fn != nil {
			samples = m.fn()
		} else {
			m.mu.Lock()
			for _, s := range m.values {
				samples = append(samples, *s)
			}
			m.mu.Unlock()
		}
		if len(samples) == 0 {
			continue
		}
		sort.Slice(samples, func(i, j int) bool {
			return strings.Join(samples[i].Labels, "\x00") < strings.Join(samples[j].Labels, "\x00")
		})
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", m.name, escapeHelp(m.help), m.name, m.kind)
		for _, s := range samples {
			b.WriteString(m.name)
			if len(m.labels) > 0 && len(s.Labels) == len(m.labels) {
				b.WriteByte('{')
				for i, l := range m.labels {
					if i > 0 {
						b.WriteByte(',')
					}
					b.WriteString(l + `="` + escapeLabel(s.Labels[i]) + `"`)
				}
				b.WriteByte('}')
			}
			b.WriteString(" " + formatValue(s.Value) + "\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// What the packages of the server count. The gauges that are read from the
// database when asked are registered by cmd/robinmail.
var (
	SMTPConnections = Default.Counter("robinmail_smtp_connections_total", "Connections accepted on the SMTP and submission listeners.")
	IMAPConnections = Default.Counter("robinmail_imap_connections_total", "Connections accepted on the IMAP listeners.")
	Delivered       = Default.Counter("robinmail_delivered_total", "Messages stored in a mailbox by inbound delivery, per recipient account.")
	Rejected        = Default.Counter("robinmail_rejected_total", "Inbound messages refused after DATA, by reason.", "reason")
	QueueAttempts   = Default.Counter("robinmail_queue_attempts_total", "Attempts the outbound queue finished.")
	QueueRecipients = Default.Counter("robinmail_queue_recipients_total", "Recipients of outbound mail by what an attempt made of them.", "result")
	HTTPRequests    = Default.Counter("robinmail_http_requests_total", "Requests the web listeners answered, by status class.", "code")
	AuthEvents      = Default.Counter("robinmail_auth_events_total", "Auth events written to the audit log, by event.", "event")
	DBRequests      = Default.Counter("robinmail_rqlite_requests_total", "Requests to rqlite by outcome: ok, error (a statement failed), failed (no answer).", "result")
	ScrubChecked    = Default.Counter("robinmail_blob_scrub_checked_total", "Blob files a scrub read on this node.")
	ScrubCorrupt    = Default.Counter("robinmail_blob_scrub_corrupt_total", "Blob files a scrub found corrupt on this node.")
	ScrubRepaired   = Default.Counter("robinmail_blob_scrub_repaired_total", "Corrupt blob files a scrub replaced from another node.")
)
