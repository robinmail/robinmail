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
	c, err := load(t, `{"hostname":"Mail.Example.Test","data_dir":"data","domains":["Example.Test"],
		"rqlite":{"url":"http://127.0.0.1:4001"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Hostname != "mail.example.test" || !c.HasDomain("example.test") || c.SMTP.Listen != ":25" {
		t.Errorf("config = %+v", c)
	}
	if c.Blob.Backend != BlobLocal || !filepath.IsAbs(c.BlobDir()) || !strings.HasSuffix(c.BlobDir(), "/data/blobs") {
		t.Errorf("blob = %+v, dir %s", c.Blob, c.BlobDir())
	}

	c, err = load(t, `{"hostname":"mail.example.test","domains":["example.test"],
		"rqlite":{"url":"http://127.0.0.1:4001"},
		"blob":{"backend":"s3","s3":{"endpoint":"http://127.0.0.1:3900","region":"garage","bucket":"mail","access_key":"k","secret_key":"s"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Blob.S3.Bucket != "mail" {
		t.Errorf("s3 = %+v", c.Blob.S3)
	}
}

func TestLoadRejects(t *testing.T) {
	const head = `{"hostname":"mail.example.test","domains":["example.test"],`
	for name, body := range map[string]string{
		"no rqlite":       head + `"data_dir":"data"}`,
		"bad rqlite url":  head + `"data_dir":"data","rqlite":{"url":"127.0.0.1:4001"}}`,
		"local, no dir":   head + `"rqlite":{"url":"http://x:4001"}}`,
		"unknown backend": head + `"rqlite":{"url":"http://x:4001"},"blob":{"backend":"tape"}}`,
		"s3 incomplete":   head + `"rqlite":{"url":"http://x:4001"},"blob":{"backend":"s3","s3":{"endpoint":"http://x"}}}`,
		"unknown field":   head + `"data_dir":"data","rqlite":{"url":"http://x:4001"},"nope":1}`,
		"bad domain":      `{"hostname":"mail.example.test","domains":["exa mple"],"data_dir":"d","rqlite":{"url":"http://x:4001"}}`,
		"half a tls pair": head + `"data_dir":"data","rqlite":{"url":"http://x:4001"},"tls":{"cert_file":"c"}}`,
		"negative limit":  head + `"data_dir":"data","rqlite":{"url":"http://x:4001"},"smtp":{"max_recipients":-1}}`,
	} {
		if _, err := load(t, body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
