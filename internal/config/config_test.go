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
	c, err := load(t, `{"hostname":"Mail.Example.Test","data_dir":"data","master_key_file":"master.key",
		"domains":["Example.Test"],"rqlite":{"url":"http://127.0.0.1:4001"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Hostname != "mail.example.test" || !c.HasDomain("example.test") || c.SMTP.Listen != ":25" {
		t.Errorf("config = %+v", c)
	}
	if !filepath.IsAbs(c.BlobDir()) || !strings.HasSuffix(c.BlobDir(), "/data/blobs") {
		t.Errorf("blob dir %s", c.BlobDir())
	}
	if !filepath.IsAbs(c.MasterKeyFile) || !strings.HasSuffix(c.MasterKeyFile, "/master.key") {
		t.Errorf("master key file %s", c.MasterKeyFile)
	}

	c, err = load(t, `{"hostname":"mail.example.test","data_dir":"/var/lib/robinmail","master_key_file":"/etc/robinmail/master.key",
		"domains":["example.test"],"rqlite":{"url":"http://127.0.0.1:4001"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.BlobDir() != "/var/lib/robinmail/blobs" || c.MasterKeyFile != "/etc/robinmail/master.key" {
		t.Errorf("absolute paths changed: %s, %s", c.BlobDir(), c.MasterKeyFile)
	}
}

func TestLoadRejects(t *testing.T) {
	const head = `{"hostname":"mail.example.test","domains":["example.test"],`
	const paths = `"data_dir":"data","master_key_file":"master.key",`
	for name, body := range map[string]string{
		"no rqlite":       head + paths + `"smtp":{}}`,
		"bad rqlite url":  head + paths + `"rqlite":{"url":"127.0.0.1:4001"}}`,
		"no data dir":     head + `"master_key_file":"k","rqlite":{"url":"http://x:4001"}}`,
		"no key file":     head + `"data_dir":"data","rqlite":{"url":"http://x:4001"}}`,
		"unknown field":   head + paths + `"rqlite":{"url":"http://x:4001"},"blob":{"backend":"s3"}}`,
		"bad domain":      `{"hostname":"mail.example.test","domains":["exa mple"],` + paths + `"rqlite":{"url":"http://x:4001"}}`,
		"half a tls pair": head + paths + `"rqlite":{"url":"http://x:4001"},"tls":{"cert_file":"c"}}`,
		"negative limit":  head + paths + `"rqlite":{"url":"http://x:4001"},"smtp":{"max_recipients":-1}}`,
	} {
		if _, err := load(t, body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
