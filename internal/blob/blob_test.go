package blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func random(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSplitKey(t *testing.T) {
	sum := sha([]byte("x"))
	for _, k := range []string{Key("acct", sum), Key("mit6k7r3v6xjkz3y", sum), Key("A-b_9", sum)} {
		if _, _, err := SplitKey(k); err != nil {
			t.Errorf("SplitKey(%q): %v", k, err)
		}
	}
	for _, k := range []string{"", sum, "/" + sum, "acct/", "acct/" + sum[:63], "acct/" + sum + "0", "acct/" + strings.ToUpper(sum),
		"../" + sum, "a/b/" + sum, "a.b/" + sum, "a b/" + sum, "acct/../" + sum[:61], "acct/" + sum[:62] + "/.",
		strings.Repeat("a", 65) + "/" + sum, "é/" + sum, "a\x00/" + sum} {
		if _, _, err := SplitKey(k); err == nil {
			t.Errorf("SplitKey(%q) accepted", k)
		}
	}
}

func TestLocal(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "blobs")
	l := NewLocal(dir)
	sum := sha([]byte("x"))
	key := Key("acct", sum)

	if _, err := l.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
	if err := l.Delete(ctx, key); err != nil {
		t.Errorf("Delete missing: %v", err)
	}
	for _, data := range [][]byte{[]byte("first\r\n"), random(t, 300_000), {}} {
		if err := l.Put(ctx, key, bytes.NewReader(data)); err != nil {
			t.Fatalf("Put %d bytes: %v", len(data), err)
		}
		r, err := l.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Get returned %d bytes, %v; want %d", len(got), err, len(data))
		}
	}

	// <dir>/<account>/<first 2 hex>/<sha256>, private, and no temp files left.
	path := filepath.Join(dir, "acct", sum[:2], sum)
	if got, _ := l.Path(key); got != path {
		t.Errorf("Path = %s, want %s", got, path)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("blob file: %v, %v", fi, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("%d files in the directory, want 1", len(entries))
	}

	// A failing source leaves nothing behind.
	other := Key("acct", sha([]byte("y")))
	if err := l.Put(ctx, other, iotest.ErrReader(errors.New("boom"))); err == nil {
		t.Error("Put with a failing reader succeeded")
	}
	if _, err := l.Get(ctx, other); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after a failed Put: %v", err)
	}
	otherPath, _ := l.Path(other)
	if entries, _ := os.ReadDir(filepath.Dir(otherPath)); len(entries) != 0 {
		t.Errorf("%d files left after a failed Put", len(entries))
	}

	if err := l.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: %v", err)
	}
	for _, bad := range []string{"../" + sum, "acct/../../x", ""} {
		if l.Put(ctx, bad, strings.NewReader("x")) == nil || l.Delete(ctx, bad) == nil {
			t.Errorf("key %q accepted", bad)
		}
		if _, err := l.Get(ctx, bad); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q): %v", bad, err)
		}
	}
}

func seal(t testing.TB, key []byte, account string, plain []byte) []byte {
	t.Helper()
	r, err := Encrypt(key, account, sha(plain), bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// open decrypts a whole file. It returns what was readable before any error.
func open(key []byte, account, sum string, file []byte) ([]byte, error) {
	r, err := Decrypt(key, account, sum, io.NopCloser(bytes.NewReader(file)))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func TestEncryptRoundTrip(t *testing.T) {
	key := random(t, KeySize)
	sizes := []int{0, 1, 100, chunkSize - 1, chunkSize, chunkSize + 1, 3 * chunkSize, 3*chunkSize + 7}
	if !testing.Short() {
		sizes = append(sizes, 25<<20)
	}
	for _, size := range sizes {
		plain := random(t, size)
		file := seal(t, key, "acct", plain)

		chunks := (size + chunkSize - 1) / chunkSize
		if chunks == 0 {
			chunks = 1
		}
		if want := headerLen + size + chunks*tagLen; len(file) != want {
			t.Errorf("%d bytes encrypt to %d, want %d", size, len(file), want)
		}
		if size >= 100 && bytes.Contains(file, plain[:32]) {
			t.Errorf("%d bytes: plaintext visible in the file", size)
		}
		got, err := open(key, "acct", sha(plain), file)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("%d bytes: decrypt returned %d bytes, %v", size, len(got), err)
		}
		// The same through a reader that hands out one byte at a time.
		r, err := Decrypt(key, "acct", sha(plain), io.NopCloser(iotest.OneByteReader(bytes.NewReader(file))))
		if err != nil {
			t.Fatal(err)
		}
		if size <= 3*chunkSize+7 {
			if got, err := io.ReadAll(iotest.OneByteReader(r)); err != nil || !bytes.Equal(got, plain) {
				t.Errorf("%d bytes, byte by byte: %d bytes, %v", size, len(got), err)
			}
		}
	}

	// Two encryptions of one plaintext share nothing but the fixed header.
	a, b := seal(t, key, "acct", []byte("same")), seal(t, key, "acct", []byte("same"))
	if bytes.Equal(a[8:], b[8:]) || !bytes.Equal(a[:8], b[:8]) || string(a[:4]) != "RMB1" {
		t.Errorf("two files: %x and %x", a, b)
	}
	if _, err := Encrypt(key[:16], "acct", sha(nil), bytes.NewReader(nil)); err == nil {
		t.Error("accepted a short key")
	}
	if _, err := Encrypt(key, "acct", sha(nil), iotest.ErrReader(errors.New("boom"))); err != nil {
		t.Fatal(err)
	} else if r, _ := Encrypt(key, "acct", sha(nil), iotest.ErrReader(errors.New("boom"))); r != nil {
		if _, err := io.ReadAll(r); err == nil {
			t.Error("a failing source went unnoticed")
		}
	}
}

// Every way of damaging a file must end in ErrCorrupt, and never in
// plaintext that passes as complete.
func TestDecryptRejects(t *testing.T) {
	key := random(t, KeySize)
	plain := random(t, 3*chunkSize+1000) // four chunks
	sum := sha(plain)
	file := seal(t, key, "acct", plain)
	const sealed = chunkSize + tagLen
	chunk := func(i int) []byte { return file[headerLen+i*sealed : min(headerLen+(i+1)*sealed, len(file))] }
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	header := file[:headerLen]

	exact := random(t, 2*chunkSize) // ends on a chunk boundary
	exactFile := seal(t, key, "acct", exact)

	cases := map[string][]byte{
		"empty file":                {},
		"header only":               header,
		"half a header":             file[:10],
		"cut inside the last chunk": file[:len(file)-1],
		"cut inside the last tag":   file[:len(file)-tagLen+1],
		"cut at a chunk boundary":   file[:headerLen+3*sealed],
		"cut after the first chunk": file[:headerLen+sealed],
		"cut inside a middle chunk": file[:headerLen+sealed+100],
		"last chunk dropped, exact": exactFile[:headerLen+sealed],
		"chunks swapped":            join(header, chunk(1), chunk(0), chunk(2), chunk(3)),
		"chunk repeated":            join(header, chunk(0), chunk(0), chunk(1), chunk(2), chunk(3)),
		"middle chunk dropped":      join(header, chunk(0), chunk(2), chunk(3)),
		"first chunk dropped":       join(header, chunk(1), chunk(2), chunk(3)),
		"only the last chunk":       join(header, chunk(3)),
		"data appended":             join(file, []byte("x")),
		"chunk appended":            join(file, chunk(3)),
		"another file's chunk":      join(header, chunk(0), exactFile[headerLen:headerLen+sealed], chunk(2), chunk(3)),
		"another file's header":     join(exactFile[:headerLen], file[headerLen:]),
		"not a blob file":           plain,
	}
	for name, at := range map[string]int{
		"magic": 0, "chunk size": 6, "nonce prefix": 20, "first byte of data": headerLen,
		"middle chunk": headerLen + sealed + 500, "tag of the first chunk": headerLen + sealed - 1,
		"last byte": len(file) - 1,
	} {
		flipped := bytes.Clone(file)
		flipped[at] ^= 0x01
		cases["bit flipped in the "+name] = flipped
	}
	for name, damaged := range cases {
		got, err := open(key, "acct", sum, damaged)
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt (%d bytes returned)", name, err, len(got))
		}
		if !bytes.HasPrefix(plain, got) {
			t.Errorf("%s: returned bytes that are not the plaintext", name)
		}
	}

	// The right file under the wrong name or key.
	other := random(t, KeySize)
	for name, err := range map[string]error{
		"another account": func() error { _, err := open(key, "acct2", sum, file); return err }(),
		"another hash":    func() error { _, err := open(key, "acct", sha([]byte("x")), file); return err }(),
		"another key":     func() error { _, err := open(other, "acct", sum, file); return err }(),
		"account and hash run together": func() error {
			_, err := open(key, "acc", "t"+sum, file)
			return err
		}(),
	} {
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
	if got, err := open(key, "acct", sum, file); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("the undamaged file: %v", err)
	}
}

func FuzzDecrypt(f *testing.F) {
	key := bytes.Repeat([]byte{7}, KeySize)
	plain := []byte("hello")
	good := seal(f, key, "acct", plain)
	f.Add(good)
	f.Add(good[:headerLen])
	f.Add([]byte("RMB1"))
	f.Fuzz(func(t *testing.T, file []byte) {
		got, err := open(key, "acct", sha(plain), file)
		// The nonce prefix is random, so the fuzzer cannot build a second
		// valid file: whatever decrypts without error is this plaintext.
		if err == nil && !bytes.Equal(got, plain) {
			t.Fatalf("accepted a forged file: %q", got)
		}
		if err != nil && len(got) != 0 {
			t.Fatalf("returned %q with error %v", got, err)
		}
	})
}

func TestMasterKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if _, err := LoadMasterKey(path); err == nil {
		t.Fatal("loaded a missing key file")
	}
	if err := GenerateMasterKeyFile(path); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", fi.Mode().Perm())
	}
	if err := GenerateMasterKeyFile(path); err == nil {
		t.Error("overwrote an existing key file")
	}
	m, err := LoadMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}

	key, wrapped, err := m.NewAccountKey("acct")
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != KeySize || !strings.HasPrefix(wrapped, "1:") || strings.Contains(wrapped, keyEncoding.EncodeToString(key)) {
		t.Errorf("key of %d bytes, wrapped %q", len(key), wrapped)
	}
	if got, err := m.Unwrap("acct", wrapped); err != nil || !bytes.Equal(got, key) {
		t.Errorf("Unwrap: %v", err)
	}
	// The same master key loaded again, as another node would.
	again, _ := LoadMasterKey(path)
	if got, err := again.Unwrap("acct", wrapped); err != nil || !bytes.Equal(got, key) {
		t.Errorf("Unwrap on a second load: %v", err)
	}
	if _, w2, _ := m.NewAccountKey("acct"); w2 == wrapped {
		t.Error("two account keys wrap to the same text")
	}

	otherPath := filepath.Join(dir, "other.key")
	GenerateMasterKeyFile(otherPath)
	other, _ := LoadMasterKey(otherPath)
	tampered := []byte(wrapped)
	tampered[len(tampered)-1] ^= 1
	for name, err := range map[string]error{
		"another master key": func() error { _, err := other.Unwrap("acct", wrapped); return err }(),
		"another account":    func() error { _, err := m.Unwrap("acct2", wrapped); return err }(),
		"tampered":           func() error { _, err := m.Unwrap("acct", string(tampered)); return err }(),
		"unknown version":    func() error { _, err := m.Unwrap("acct", "2:"+wrapped[2:]); return err }(),
		"truncated":          func() error { _, err := m.Unwrap("acct", wrapped[:20]); return err }(),
		"empty":              func() error { _, err := m.Unwrap("acct", ""); return err }(),
	} {
		if err == nil {
			t.Errorf("Unwrap accepted: %s", name)
		}
	}

	// Key files others can read, or that hold no key, are refused.
	os.Chmod(path, 0o644)
	if _, err := LoadMasterKey(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Errorf("loaded a world-readable key file: %v", err)
	}
	for name, content := range map[string]string{"empty": "", "short": "YWJj\n", "junk": "not a key\n"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o600)
		if _, err := LoadMasterKey(p); err == nil {
			t.Errorf("loaded a key file that is %s", name)
		}
	}
	if _, err := LoadMasterKey(dir); err == nil {
		t.Error("loaded a directory")
	}
	if _, err := NewMasterKey(make([]byte, 16)); err == nil {
		t.Error("accepted a 16-byte master key")
	}
}
