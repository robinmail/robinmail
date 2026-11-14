package blob

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Local keeps blob files in a directory:
//
//	<dir>/<account-id>/<first 2 hex of sha256>/<sha256>
type Local struct {
	dir string
}

// NewLocal returns a store rooted in dir. Directories are created on the
// first Put.
func NewLocal(dir string) *Local { return &Local{dir: dir} }

// Path is where the file of a blob is, or would be.
func (l *Local) Path(key string) (string, error) {
	account, sum, err := SplitKey(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.dir, account, sum[:2], sum), nil
}

// Put writes to a temporary file, syncs it and renames it into place, so a
// reader sees the whole file or none of it, also after a crash.
func (l *Local) Put(ctx context.Context, key string, r io.Reader) error {
	return l.PutVerified(ctx, key, r, nil)
}

// PutVerified is Put for a file that came from somewhere else: before the
// file gets its name, check reads it from the start and decides whether it
// is what the name says. A file that fails the check is removed and never
// visible under the key. A nil check accepts everything.
//
// Like Put it replaces an existing file, also one with the same content.
// That is wanted: the new file's modification time tells the sweeper that
// someone is about to refer to this blob (see Sweep).
func (l *Local) PutVerified(ctx context.Context, key string, r io.Reader, check Verifier) (err error) {
	path, err := l.Path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = io.Copy(f, r); err != nil {
		return err
	}
	if check != nil {
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err = check(ctx, key, f); err != nil {
			return err
		}
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// Has reports whether the blob's file is on this disk.
func (l *Local) Has(key string) bool {
	path, err := l.Path(key)
	if err != nil {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// List returns the hashes of the account's blob files on this disk.
func (l *Local) List(account string) (map[string]bool, error) {
	out := map[string]bool{}
	if _, _, err := SplitKey(Key(account, strings.Repeat("0", 64))); err != nil {
		return nil, err
	}
	shards, err := os.ReadDir(filepath.Join(l.dir, account))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, sh := range shards {
		if !sh.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(l.dir, account, sh.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if _, _, err := SplitKey(Key(account, f.Name())); err == nil && f.Name()[:2] == sh.Name() {
				out[f.Name()] = true
			}
		}
	}
	return out, nil
}

func (l *Local) Get(_ context.Context, key string) (io.ReadCloser, error) {
	path, err := l.Path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (l *Local) Delete(_ context.Context, key string) error {
	path, err := l.Path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// syncDir flushes a directory so a rename into it survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
