package rspamd

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheck(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, `{"is_skipped":false,"score":6.5,"required_score":15,"action":"add header",
			"symbols":{"GTUBE":{"name":"GTUBE","score":0},"R_SPF_NA":{"name":"R_SPF_NA","score":0}},
			"messages":{"smtp_message":"go away"},
			"milter":{"add_headers":{
				"X-Spamd-Result":{"value":"default: False [6.5 / 15.0]","order":0},
				"X-Spam":"Yes",
				"X-Multi":[{"value":"a","order":1},{"value":"b","order":2}]},
			"remove_headers":{"X-Spam":0}}}`)
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL + "/"}
	res, err := c.Check(context.Background(), Request{
		From: "a@b.test", Rcpts: []string{"u@x.test", "v@x.test"}, IP: net.ParseIP("192.0.2.1"),
		Helo: "client.test", QueueID: "q1", Hostname: "bad\r\nInjected: 1",
	}, []byte("Subject: hi\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/checkv2" || body != "Subject: hi\r\n\r\nbody\r\n" {
		t.Errorf("request %s %q", got.URL.Path, body)
	}
	if got.Header.Get("From") != "a@b.test" || len(got.Header.Values("Rcpt")) != 2 ||
		got.Header.Get("IP") != "192.0.2.1" || got.Header.Get("Helo") != "client.test" ||
		got.Header.Get("Queue-Id") != "q1" || got.Header.Get("Hostname") != "" {
		t.Errorf("headers %v", got.Header)
	}
	if res.Action != AddHeader || res.Score != 6.5 || res.SMTPMessage != "go away" || res.RemoveHeaders["X-Spam"] != 0 {
		t.Errorf("result %+v", res)
	}
	if len(res.Symbols) != 2 || res.Symbols[0] != "GTUBE" {
		t.Errorf("symbols %v", res.Symbols)
	}
	want := []Header{{"X-Spam", "Yes", 0}, {"X-Spamd-Result", "default: False [6.5 / 15.0]", 0}, {"X-Multi", "a", 1}, {"X-Multi", "b", 2}}
	if len(res.AddHeaders) != len(want) {
		t.Fatalf("headers %+v", res.AddHeaders)
	}
	for i := range want {
		if res.AddHeaders[i] != want[i] {
			t.Errorf("header %d = %+v, want %+v", i, res.AddHeaders[i], want[i])
		}
	}
}

func TestCheckErrors(t *testing.T) {
	for _, reply := range []string{`{"action":"explode"}`, `not json`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, reply)
		}))
		_, err := (&Client{URL: srv.URL}).Check(context.Background(), Request{}, nil)
		if err == nil {
			t.Errorf("%s: no error", reply)
		}
		srv.Close()
	}
	// Nothing listening.
	if _, err := (&Client{URL: "http://127.0.0.1:1"}).Check(context.Background(), Request{}, nil); err == nil {
		t.Error("no error without a server")
	}
}

func FuzzParseReply(f *testing.F) {
	f.Add([]byte(`{"action":"reject","milter":{"add_headers":{"A":[{"value":"x"}]}}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		parseReply(b)
	})
}
