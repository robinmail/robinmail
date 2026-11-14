package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "robinmail.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoad(t *testing.T) {
	c, err := load(t, `{"hostname":"Mail.Example.Test","data_dir":"data","master_key_file":"master.key", "rqlite":{"url":"http://127.0.0.1:4001"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Hostname != "mail.example.test" || c.SMTP.Listen != ":25" {
		t.Errorf("config = %+v", c)
	}
	if !filepath.IsAbs(c.BlobDir()) || !strings.HasSuffix(c.BlobDir(), "/data/blobs") {
		t.Errorf("blob dir %s", c.BlobDir())
	}
	if !filepath.IsAbs(c.MasterKeyFile) || !strings.HasSuffix(c.MasterKeyFile, "/master.key") {
		t.Errorf("master key file %s", c.MasterKeyFile)
	}

	c, err = load(t, `{"hostname":"mail.example.test","data_dir":"/var/lib/robinmail","master_key_file":"/etc/robinmail/master.key", "rqlite":{"url":"http://127.0.0.1:4001"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.BlobDir() != "/var/lib/robinmail/blobs" || c.MasterKeyFile != "/etc/robinmail/master.key" {
		t.Errorf("absolute paths changed: %s, %s", c.BlobDir(), c.MasterKeyFile)
	}
}

func TestLoadRejects(t *testing.T) {
	const head = `{"hostname":"mail.example.test",`
	const paths = `"data_dir":"data","master_key_file":"master.key",`
	for name, body := range map[string]string{
		"no rqlite":      head + paths + `"smtp":{}}`,
		"bad rqlite url": head + paths + `"rqlite":{"url":"127.0.0.1:4001"}}`,
		"no data dir":    head + `"master_key_file":"k","rqlite":{"url":"http://x:4001"}}`,
		"no key file":    head + `"data_dir":"data","rqlite":{"url":"http://x:4001"}}`,
		"unknown field":  head + paths + `"rqlite":{"url":"http://x:4001"},"blob":{"backend":"s3"}}`,
		// The accepted domains moved to the domain table.
		"domains":         head + paths + `"domains":["example.test"],"rqlite":{"url":"http://x:4001"}}`,
		"bad hostname":    `{"hostname":"exa mple",` + paths + `"rqlite":{"url":"http://x:4001"}}`,
		"half a tls pair": head + paths + `"rqlite":{"url":"http://x:4001"},"tls":{"cert_file":"c"}}`,
		"negative limit":  head + paths + `"rqlite":{"url":"http://x:4001"},"smtp":{"max_recipients":-1}}`,
	} {
		if _, err := load(t, body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// RFC 5321 4.5.3.1.7: "The maximum total length of a message content ...
// MUST BE at least 64K octets." 4.5.3.1.8: "The minimum total number of
// recipients that MUST be buffered is 100 recipients. Rejection of messages
// (for excessive recipients) with fewer than 100 RCPT commands is a
// violation of this specification." A config cannot set less.
func TestRFC5321_4_5_3_1_MinimumLimits(t *testing.T) {
	const head = `{"hostname":"mail.example.test","data_dir":"data","master_key_file":"master.key","rqlite":{"url":"http://x:4001"},"smtp":`
	for body, ok := range map[string]bool{
		`{"max_message_bytes":65536,"max_recipients":100}`: true,
		`{"max_message_bytes":0,"max_recipients":0}`:       true, // the defaults, 25 MiB and 100
		`{"max_message_bytes":65535}`:                      false,
		`{"max_recipients":99}`:                            false,
		`{"max_recipients":1}`:                             false,
	} {
		if _, err := load(t, head+body+"}"); (err == nil) != ok {
			t.Errorf("smtp %s: err = %v, want ok = %v", body, err, ok)
		}
	}
}

// The web listeners: HTTPS needs a certificate, plain HTTP only exists on a
// loopback address, and a dev origin is an origin, not a URL.
func TestWeb(t *testing.T) {
	const head = `{"hostname":"mail.example.test","data_dir":"data","master_key_file":"master.key","rqlite":{"url":"http://x:4001"},`
	for body, ok := range map[string]bool{
		`"web":{}`:                               true,
		`"web":{"http_listen":"127.0.0.1:8080"}`: true,
		`"web":{"http_listen":"localhost:8080"}`: true,
		`"web":{"http_listen":"[::1]:8080"}`:     true,
		`"web":{"http_listen":"127.0.0.1:8080","dev_origins":["http://localhost:5173"]}`: true,
		`"web":{"listen":":443"},"tls":{"cert_file":"c.pem","key_file":"k.pem"}`:         true,
		`"web":{"listen":":443"},"tls":{"acme":true}`:                                    true,
		`"web":{"listen":":443"}`:                                                        false, // no certificate
		`"web":{"http_listen":":8080"}`:                                                  false, // every interface
		`"web":{"http_listen":"0.0.0.0:8080"}`:                                           false,
		`"web":{"http_listen":"192.0.2.1:8080"}`:                                         false,
		`"web":{"http_listen":"mail.example.test:8080"}`:                                 false,
		`"web":{"http_listen":"127.0.0.1"}`:                                              false, // no port
		`"web":{"dev_origins":["http://localhost:5173/"]}`:                               false, // a path
		`"web":{"dev_origins":["localhost:5173"]}`:                                       false,
		`"web":{"dev_origins":["*"]}`:                                                    false,
		`"web":{"dev_origins":["ftp://localhost"]}`:                                      false,
	} {
		if _, err := load(t, head+body+"}"); (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok = %v", body, err, ok)
		}
	}
}

// A node section makes the instance a cluster member; several rqlite URLs
// may be given, alone or next to url.
func TestNodeAndRqliteURLs(t *testing.T) {
	head := `{"hostname":"mail.example.test","data_dir":"data","master_key_file":"k",`
	c, err := load(t, head+`"rqlite":{"urls":["http://a:4001","http://b:4001"]},
		"node":{"id":"n1","listen":":7443","address":"n1.internal:7443","blob_copies":3,"repair_interval_seconds":60}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Rqlite.Nodes(); len(got) != 2 || got[0] != "http://a:4001" {
		t.Errorf("Nodes = %v", got)
	}
	if c.Node.ID != "n1" || c.Node.BlobCopies != 3 {
		t.Errorf("node = %+v", c.Node)
	}
	c, err = load(t, head+`"rqlite":{"url":"http://near:4001","urls":["http://far:4001"]}}`)
	if err != nil || len(c.Rqlite.Nodes()) != 2 || c.Rqlite.Nodes()[0] != "http://near:4001" || c.Node.ID != "" {
		t.Errorf("url and urls: %v, %+v", err, c)
	}
	rq := `"rqlite":{"url":"http://x:4001"},`
	for name, body := range map[string]string{
		"no rqlite at all":       head + `"rqlite":{}}`,
		"bad url in urls":        head + `"rqlite":{"urls":["http://a:4001","b:4001"]}}`,
		"node without id":        head + rq + `"node":{"listen":":7443","address":"n1:7443"}}`,
		"bad node id":            head + rq + `"node":{"id":"Node 1","listen":":7443","address":"n1:7443"}}`,
		"node without listen":    head + rq + `"node":{"id":"n1","address":"n1:7443"}}`,
		"node without address":   head + rq + `"node":{"id":"n1","listen":":7443"}}`,
		"address without a host": head + rq + `"node":{"id":"n1","listen":":7443","address":":7443"}}`,
		"negative copies":        head + rq + `"node":{"id":"n1","listen":":7443","address":"n1:7443","blob_copies":-1}}`,
	} {
		if _, err := load(t, body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The sections for sending: defaults, and what is refused.
func TestSubmissionAndOutbound(t *testing.T) {
	const base = `"hostname":"mail.example.test","data_dir":"data","master_key_file":"master.key","rqlite":{"url":"http://127.0.0.1:4001"}`
	c, err := load(t, `{`+base+`}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Submission.Listen != "" || c.Submission.MaxRecipientsPerHour != 300 || c.Submission.UndoSeconds == nil || *c.Submission.UndoSeconds != 10 {
		t.Errorf("defaults: %+v", c.Submission)
	}
	c, err = load(t, `{`+base+`,"tls":{"cert_file":"c.pem","key_file":"k.pem"},
		"submission":{"listen":":587","listen_tls":":465","max_recipients_per_hour":50,"undo_seconds":0},
		"outbound":{"concurrency":4,"per_destination":1,"routes":{"example.org":"relay.example.net:25","*":"127.0.0.1:2525"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	// 0 is a value: send at once.
	if *c.Submission.UndoSeconds != 0 || c.Submission.MaxRecipientsPerHour != 50 || c.Outbound.Routes["*"] != "127.0.0.1:2525" || c.Outbound.Concurrency != 4 {
		t.Errorf("config: %+v %+v", c.Submission, c.Outbound)
	}
	for name, extra := range map[string]string{
		// A password must not cross a listener that cannot encrypt.
		"submission without a certificate":   `"submission":{"listen":":587"}`,
		"implicit TLS without a certificate": `"submission":{"listen_tls":":465"}`,
		"a negative rate limit":              `"submission":{"max_recipients_per_hour":-1}`,
		"an undo time of more than an hour":  `"submission":{"undo_seconds":3601}`,
		"a negative undo time":               `"submission":{"undo_seconds":-1}`,
		"a route without a port":             `"outbound":{"routes":{"example.org":"relay.example.net"}}`,
		"a route for something not a domain": `"outbound":{"routes":{"not a domain":"127.0.0.1:25"}}`,
		"negative concurrency":               `"outbound":{"concurrency":-1}`,
		"an unknown member":                  `"submission":{"port":587}`,
	} {
		if _, err := load(t, `{`+base+`,`+extra+`}`); err == nil {
			t.Errorf("%s is accepted", name)
		}
	}
}
