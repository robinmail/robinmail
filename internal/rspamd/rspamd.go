// Package rspamd is a client for the Rspamd HTTP protocol (POST /checkv2).
// See https://docs.rspamd.com/developers/protocol.
package rspamd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Actions as Rspamd spells them; the spaces are part of the names.
const (
	NoAction       = "no action"
	Greylist       = "greylist"
	AddHeader      = "add header"
	RewriteSubject = "rewrite subject"
	SoftReject     = "soft reject"
	Reject         = "reject"
)

// Client talks to a normal or proxy worker, usually on port 11333.
type Client struct {
	URL     string // for example http://127.0.0.1:11333
	HTTP    *http.Client
	Timeout time.Duration // [30s]
}

// Request is the envelope that goes along with the message.
type Request struct {
	From     string // SMTP MAIL FROM, empty for <>
	Rcpts    []string
	IP       net.IP
	Helo     string
	Hostname string // reverse DNS of IP, if known
	QueueID  string
	User     string // authenticated user, empty for inbound mail
}

// Header is one header field Rspamd wants added.
type Header struct {
	Name, Value string
	Order       int
}

// Result is the part of the reply robinmail acts on.
type Result struct {
	Action        string
	Score         float64
	RequiredScore float64
	Subject       string // new subject for RewriteSubject
	SMTPMessage   string // reply text suggested by a filter, may be empty
	AddHeaders    []Header
	// RemoveHeaders maps a header name to which occurrence to remove:
	// 0 all, n > 0 the n-th from the top, n < 0 the n-th from the bottom.
	RemoveHeaders map[string]int
	// SpamHeader is the header a milter-mode MTA sets to "Yes" on spam;
	// Rspamd only names it when it differs from the default X-Spam.
	SpamHeader string
	Symbols    []string // names of the symbols that fired, sorted
	// DKIMSignatures are the values of the DKIM-Signature fields Rspamd
	// made for a message of an authenticated user (its dkim_signing
	// module), folded as they are to be written. Over the HTTP protocol
	// they come in a field of their own, not among the headers to add.
	DKIMSignatures []string
}

// Has reports whether a symbol fired.
func (r *Result) Has(symbol string) bool {
	i := sort.SearchStrings(r.Symbols, symbol)
	return i < len(r.Symbols) && r.Symbols[i] == symbol
}

const maxReply = 4 << 20

// Check scans msg and returns Rspamd's verdict.
func (c *Client) Check(ctx context.Context, req Request, msg []byte) (*Result, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/checkv2", bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	set := func(k, v string) {
		if v != "" && !strings.ContainsAny(v, "\r\n") {
			hr.Header.Add(k, v)
		}
	}
	set("From", req.From)
	for _, r := range req.Rcpts {
		set("Rcpt", r)
	}
	if req.IP != nil {
		set("IP", req.IP.String())
	}
	set("Helo", req.Helo)
	set("Hostname", req.Hostname)
	set("Queue-Id", req.QueueID)
	set("User", req.User)

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxReply {
		return nil, fmt.Errorf("rspamd: reply larger than %d bytes", maxReply)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rspamd: %s", resp.Status)
	}
	return parseReply(body)
}

type reply struct {
	Action        string                     `json:"action"`
	Score         float64                    `json:"score"`
	RequiredScore float64                    `json:"required_score"`
	Subject       string                     `json:"subject"`
	Symbols       map[string]json.RawMessage `json:"symbols"`
	Messages      map[string]json.RawMessage `json:"messages"`
	DKIMSignature json.RawMessage            `json:"dkim-signature"`
	Milter        struct {
		AddHeaders    map[string]json.RawMessage `json:"add_headers"`
		RemoveHeaders map[string]int             `json:"remove_headers"`
		SpamHeader    string                     `json:"spam_header"`
	} `json:"milter"`
}

func parseReply(body []byte) (*Result, error) {
	var r reply
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("rspamd: bad reply: %w", err)
	}
	switch r.Action {
	case NoAction, Greylist, AddHeader, RewriteSubject, SoftReject, Reject:
	default:
		return nil, fmt.Errorf("rspamd: unknown action %q", r.Action)
	}
	res := &Result{
		Action:        r.Action,
		Score:         r.Score,
		RequiredScore: r.RequiredScore,
		Subject:       r.Subject,
		RemoveHeaders: r.Milter.RemoveHeaders,
		SpamHeader:    r.Milter.SpamHeader,
	}
	if res.SpamHeader == "" {
		res.SpamHeader = "X-Spam"
	}
	if raw, ok := r.Messages["smtp_message"]; ok {
		json.Unmarshal(raw, &res.SMTPMessage)
	}
	// One signature is a string, several are a list of strings.
	if len(r.DKIMSignature) > 0 && string(r.DKIMSignature) != "null" {
		var one string
		var many []string
		switch {
		case json.Unmarshal(r.DKIMSignature, &one) == nil:
			many = []string{one}
		case json.Unmarshal(r.DKIMSignature, &many) == nil:
		default:
			return nil, fmt.Errorf("rspamd: bad dkim-signature in the reply")
		}
		for _, sig := range many {
			if sig != "" {
				res.DKIMSignatures = append(res.DKIMSignatures, sig)
			}
		}
	}
	for name := range r.Symbols {
		res.Symbols = append(res.Symbols, name)
	}
	sort.Strings(res.Symbols)

	// The documented form is {"name": {"value": "...", "order": 0}}. A plain
	// string and an array of such objects (one per occurrence) also occur.
	for name, raw := range r.Milter.AddHeaders {
		var one struct {
			Value string `json:"value"`
			Order int    `json:"order"`
		}
		var many []struct {
			Value string `json:"value"`
			Order int    `json:"order"`
		}
		var s string
		switch {
		case json.Unmarshal(raw, &s) == nil:
			res.AddHeaders = append(res.AddHeaders, Header{name, s, 0})
		case json.Unmarshal(raw, &one) == nil:
			res.AddHeaders = append(res.AddHeaders, Header{name, one.Value, one.Order})
		case json.Unmarshal(raw, &many) == nil:
			for _, h := range many {
				res.AddHeaders = append(res.AddHeaders, Header{name, h.Value, h.Order})
			}
		default:
			return nil, fmt.Errorf("rspamd: bad add_headers entry for %q", name)
		}
	}
	sort.SliceStable(res.AddHeaders, func(i, j int) bool {
		a, b := res.AddHeaders[i], res.AddHeaders[j]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.Name < b.Name
	})
	return res, nil
}
