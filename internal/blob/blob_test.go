package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCheckKey(t *testing.T) {
	for _, k := range []string{"acct/0a1b", "acme/account.json", "a", "A-b_c.d/e"} {
		if err := checkKey(k); err != nil {
			t.Errorf("checkKey(%q): %v", k, err)
		}
	}
	for _, k := range []string{"", "/a", "a/", "a//b", "..", "a/../b", "a/.b", ".tmp-1", "a b", "a\\b", "a/b\x00", "é", strings.Repeat("a", 513)} {
		if err := checkKey(k); err == nil {
			t.Errorf("checkKey(%q) accepted", k)
		}
	}
	if Key("acct", "abc") != "acct/abc" {
		t.Error(Key("acct", "abc"))
	}
}

// exercise runs the behaviour every backend must have.
func exercise(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	key := Key("blobtest", hex.EncodeToString([]byte(time.Now().Format("150405.000000000"))))

	if _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Errorf("Delete missing: %v", err)
	}
	for _, data := range [][]byte{[]byte("first\r\n"), bytes.Repeat([]byte("0123456789abcdef"), 100_000), {}} {
		if err := s.Put(ctx, key, data); err != nil {
			t.Fatalf("Put %d bytes: %v", len(data), err)
		}
		r, err := s.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Get returned %d bytes, %v; want %d", len(got), err, len(data))
		}
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: %v", err)
	}
	for _, bad := range []string{"../escape", "a/../../b", ""} {
		if s.Put(ctx, bad, nil) == nil || s.Delete(ctx, bad) == nil {
			t.Errorf("key %q accepted", bad)
		}
		if _, err := s.Get(ctx, bad); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q): %v", bad, err)
		}
	}
}

func TestLocal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blobs")
	l := NewLocal(dir)
	exercise(t, l)

	if err := l.Put(context.Background(), "acct/abc", []byte("x")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "acct", "abc"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("blob file: %v, %v", fi, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "acct"))
	if len(entries) != 1 {
		t.Errorf("%d files left in the directory, want 1", len(entries))
	}
}

// The two examples from "Signature Calculations for the Authorization
// Header" in the Amazon S3 API reference.
func TestSignAWSExamples(t *testing.T) {
	const (
		access = "AKIAIOSFODNN7EXAMPLE"
		secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		empty  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	)
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	get, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	get.Header.Set("Range", "bytes=0-9")
	sign(get, empty, "us-east-1", access, secret, now)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := get.Header.Get("Authorization"); got != want {
		t.Errorf("GET:\n got %s\nwant %s", got, want)
	}

	body := "Welcome to Amazon S3."
	sum := sha256.Sum256([]byte(body))
	put, _ := http.NewRequest("PUT", "https://examplebucket.s3.amazonaws.com/test$file.text", strings.NewReader(body))
	put.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	put.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	sign(put, hex.EncodeToString(sum[:]), "us-east-1", access, secret, now)
	want = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class, " +
		"Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	if got := put.Header.Get("Authorization"); got != want {
		t.Errorf("PUT:\n got %s\nwant %s", got, want)
	}

	list, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", nil)
	sign(list, empty, "us-east-1", access, secret, now)
	if got := list.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7") {
		t.Errorf("list: %s", got)
	}
}

// fakeS3 is an in-memory bucket that checks what a real one would: the path,
// the payload hash and that the request is signed for the right scope.
type fakeS3 struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	auth := r.Header.Get("Authorization")
	if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) ||
		!strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=key/") || !strings.Contains(auth, "/garage/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=") {
		f.t.Errorf("bad signature headers: %q", auth)
		http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/bucket/")
	if !ok {
		http.Error(w, "NoSuchBucket", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodPut:
		f.objects[key] = body
	case http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		w.Write(data)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed)
	}
}

func TestS3Fake(t *testing.T) {
	srv := httptest.NewServer(&fakeS3{t: t, objects: map[string][]byte{}})
	defer srv.Close()
	s, err := NewS3(S3Config{Endpoint: srv.URL, Region: "garage", Bucket: "bucket", AccessKey: "key", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, s)

	other, _ := NewS3(S3Config{Endpoint: srv.URL, Region: "garage", Bucket: "elsewhere", AccessKey: "key", SecretKey: "secret"})
	if err := other.Put(context.Background(), "a/b", []byte("x")); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("Put to a missing bucket: %v", err)
	}
	for _, cfg := range []S3Config{
		{Endpoint: "127.0.0.1:3900", Region: "r", Bucket: "b", AccessKey: "k", SecretKey: "s"},
		{Endpoint: "http://h/prefix", Region: "r", Bucket: "b", AccessKey: "k", SecretKey: "s"},
		{Endpoint: "http://h", Bucket: "b", AccessKey: "k", SecretKey: "s"},
	} {
		if _, err := NewS3(cfg); err == nil {
			t.Errorf("NewS3(%+v) accepted", cfg)
		}
	}
}

// TestS3Real runs against a real S3-compatible service:
//
//	ROBINMAIL_TEST_S3='http://ACCESSKEY:SECRETKEY@127.0.0.1:3900/bucket?region=garage' go test ./internal/blob
//
// contrib/compose has a Garage for this.
func TestS3Real(t *testing.T) {
	raw := os.Getenv("ROBINMAIL_TEST_S3")
	if raw == "" {
		t.Skip("ROBINMAIL_TEST_S3 is not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := u.User.Password()
	cfg := S3Config{Endpoint: u.Scheme + "://" + u.Host, Region: u.Query().Get("region"),
		Bucket: strings.Trim(u.Path, "/"), AccessKey: u.User.Username(), SecretKey: secret}
	s, err := NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, s)

	cfg.SecretKey += "x"
	wrong, _ := NewS3(cfg)
	if err := wrong.Put(context.Background(), "blobtest/wrong", []byte("x")); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("Put with a wrong secret: %v", err)
	}
}
