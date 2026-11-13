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
