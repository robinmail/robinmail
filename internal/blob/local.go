package blob

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	account, sum, err := splitKey(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.dir, account, sum[:2], sum), nil
}

// Put writes to a temporary file, syncs it and renames it into place, so a
// reader sees the whole file or none of it, also after a crash.
func (l *Local) Put(_ context.Context, key string, r io.Reader) (err error) {
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
