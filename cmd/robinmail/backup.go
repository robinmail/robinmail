package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/blob"
	"github.com/mikepage/robinmail/internal/store"
)

// A backup is a directory:
//
//	manifest.json                     what is in it; written last
//	db.sqlite                         the database, one SQLite file
//	blobs/<account>/<xx>/<sha256>     the blob files, as they are on disk
//
// The database file is a copy rqlite makes at one point of its log, so
// all tables in it are from one moment. The blob files are the encrypted
// files of the nodes' disks: the messages in a backup can be read by
// nobody without the master key, and the master key is not in it. The
// database file is as readable as the database is: addresses, subjects,
// previews, the search index and password hashes are in it in the clear.
// A backup wants the same care as the disks of the rqlite nodes.
//
// Which blobs: everything the database refers to just before and just
// after the copy is made, fetched from another node where this one lacks
// a file, plus whatever unreferenced files this node still has for those
// accounts (a message destroyed a moment ago keeps its file for a day).
// That covers everything the copy refers to, except a blob that was both
// stored and dropped again within the seconds around the copy and lies on
// another node only; restore reports a message without its blob.

const (
	backupVersion  = 1
	backupManifest = "manifest.json"
	backupDatabase = "db.sqlite"
	backupBlobs    = "blobs"
)

type manifest struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Hostname  string    `json:"hostname"`
	// Schema is the schema version of the database file.
	Schema int `json:"schema"`
	// Keys are the fingerprints of the master keys the accounts' keys are
	// wrapped under: what a restore needs in its key file.
	Keys           []string `json:"master_keys"`
	Accounts       int      `json:"accounts"`
	Emails         int64    `json:"emails"`
	DatabaseBytes  int64    `json:"database_bytes"`
	DatabaseSHA256 string   `json:"database_sha256"`
	Blobs          int      `json:"blobs"`
	BlobBytes      int64    `json:"blob_bytes"`
	// Missing are referenced blobs no node had a good file of.
	Missing []string `json:"missing,omitempty"`
}

func blobPath(dir, account, sum string) string {
	return filepath.Join(dir, backupBlobs, account, sum[:2], sum)
}

// writeFileSynced writes what fill produces to path, synced.
func writeFileSynced(path string, fill func(io.Writer) error) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n := &countWriter{w: f}
	if err := fill(n); err != nil {
		f.Close()
		os.Remove(path)
		return 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, err
	}
	return n.n, f.Close()
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func merge(into, from map[string]map[string]bool) {
	for account, sums := range from {
		if into[account] == nil {
			into[account] = map[string]bool{}
		}
		for sum := range sums {
			into[account][sum] = true
		}
	}
}

// backup writes a backup of the whole server into a new directory.
func (a *app) backup(dir string) error {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty: a backup wants a new directory", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	m := manifest{Version: backupVersion, CreatedAt: time.Now().UTC().Truncate(time.Second), Hostname: a.cfg.Hostname}

	before, err := a.store.BlobCatalog(a.ctx)
	if err != nil {
		return err
	}
	sum := sha256.New()
	m.DatabaseBytes, err = writeFileSynced(filepath.Join(dir, backupDatabase), func(w io.Writer) error {
		return a.store.BackupDatabase(a.ctx, io.MultiWriter(w, sum))
	})
	if err != nil {
		return err
	}
	m.DatabaseSHA256 = hex.EncodeToString(sum.Sum(nil))
	want, err := a.store.BlobCatalog(a.ctx)
	if err != nil {
		return err
	}
	merge(want, before)
	if m.Keys, err = a.store.AccountKeyFingerprints(a.ctx); err != nil {
		return err
	}
	sort.Strings(m.Keys)
	totals, err := a.store.Totals(a.ctx)
	if err != nil {
		return err
	}
	m.Schema, m.Accounts, m.Emails = totals.Schema, totals.Accounts, totals.Emails
	fmt.Fprintf(a.stdout, "database: %s, schema %d, %d accounts, %d emails\n", octets(m.DatabaseBytes), m.Schema, m.Accounts, m.Emails)

	copyBlob := func(account, sum string, referenced bool) error {
		key := blob.Key(account, sum)
		src, err := a.store.BlobFile(a.ctx, account, sum)
		if err != nil {
			return err
		}
		path := blobPath(dir, account, sum)
		n, err := writeFileSynced(path, func(w io.Writer) error {
			_, err := io.Copy(w, src)
			return err
		})
		src.Close()
		if err != nil {
			return err
		}
		// What goes into the backup is read back and checked: a file
		// that does not decrypt to its name would restore to nothing.
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		err = a.store.VerifyBlob(a.ctx, key, f)
		f.Close()
		if err != nil {
			os.Remove(path)
			return err
		}
		m.Blobs++
		m.BlobBytes += n
		return nil
	}
	accounts := make([]string, 0, len(want))
	for account := range want {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	for _, account := range accounts {
		for sum := range want[account] {
			if err := a.ctx.Err(); err != nil {
				return err
			}
			if err := copyBlob(account, sum, true); err != nil {
				m.Missing = append(m.Missing, blob.Key(account, sum))
				fmt.Fprintf(a.stdout, "blob %s: %v\n", blob.Key(account, sum), err)
			}
		}
		// Files nothing refers to any more: taken along when they are
		// good, in silence when not.
		have, err := a.local.List(account)
		if err != nil {
			return err
		}
		for sum := range have {
			if !want[account][sum] {
				copyBlob(account, sum, false)
			}
		}
	}
	sort.Strings(m.Missing)

	data, _ := json.MarshalIndent(m, "", "  ")
	if _, err := writeFileSynced(filepath.Join(dir, backupManifest), func(w io.Writer) error {
		_, err := w.Write(append(data, '\n'))
		return err
	}); err != nil {
		return err
	}
	a.note("backup", "", fmt.Sprintf("%d accounts, %d blobs, %d missing", m.Accounts, m.Blobs, len(m.Missing)))
	fmt.Fprintf(a.stdout, "blobs: %d files, %s\nbackup written to %s\n", m.Blobs, octets(m.BlobBytes), dir)
	fmt.Fprintf(a.stdout, "The master key is not in the backup. Without %s (keys %s) no message in it can be read: keep a copy of that file somewhere else.\n",
		a.cfg.MasterKeyFile, strings.Join(m.Keys, ", "))
	fmt.Fprintf(a.stdout, "%s holds what the database holds in the clear (addresses, subjects, previews, the search index, password hashes): guard the backup like the database.\n", backupDatabase)
	if len(m.Missing) > 0 {
		return fmt.Errorf("%d blobs the database refers to could not be backed up (listed in %s)", len(m.Missing), backupManifest)
	}
	return nil
}

func readManifest(dir string) (manifest, error) {
	var m manifest
	data, err := os.ReadFile(filepath.Join(dir, backupManifest))
	if errors.Is(err, os.ErrNotExist) {
		return m, fmt.Errorf("%s has no %s: not a backup, or one that was not finished", dir, backupManifest)
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%s: %w", backupManifest, err)
	}
	if m.Version != backupVersion {
		return m, fmt.Errorf("%s is a backup of version %d; this binary reads version %d", dir, m.Version, backupVersion)
	}
	return m, nil
}

// restore loads a backup into an empty server: the database into rqlite,
// the blob files into this node's blob directory (and from there, in a
// cluster, to the other nodes). No server may be running on the database.
//
// The master key is not restored, because it is not in a backup: the key
// file of the config has to hold the key the backup was made under, which
// is checked before anything is touched.
func (a *app) restore(dir string) error {
	m, err := readManifest(dir)
	if err != nil {
		return err
	}
	if m.Schema > store.SchemaVersion() {
		return fmt.Errorf("the backup has schema version %d, this binary knows up to %d: restore with the version that made it, or a newer one", m.Schema, store.SchemaVersion())
	}
	have := map[string]bool{}
	for _, fp := range a.master.Fingerprints() {
		have[fp] = true
	}
	var lacking []string
	for _, fp := range m.Keys {
		if !have[fp] {
			lacking = append(lacking, fp)
		}
	}
	if len(lacking) > 0 {
		return fmt.Errorf("%s does not hold the master key the backup was made under (%s; the file has %s): put that key file in place first. Nothing was changed",
			a.cfg.MasterKeyFile, strings.Join(lacking, ", "), strings.Join(a.master.Fingerprints(), ", "))
	}
	empty, err := a.store.Empty(a.ctx)
	if err != nil {
		return err
	}
	if !empty {
		return errors.New("this database has domains or accounts: a backup is restored into an empty one. Nothing was changed")
	}

	db, err := os.Open(filepath.Join(dir, backupDatabase))
	if err != nil {
		return err
	}
	defer db.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, db); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != m.DatabaseSHA256 {
		return fmt.Errorf("%s is not the file the backup wrote (SHA-256 %s, the manifest has %s). Nothing was changed", backupDatabase, got, m.DatabaseSHA256)
	}
	if err := a.store.LoadDatabase(a.ctx, db); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "database loaded: schema %d", m.Schema)
	if m.Schema < store.SchemaVersion() {
		fmt.Fprintf(a.stdout, ", migrated to %d", store.SchemaVersion())
	}
	fmt.Fprintln(a.stdout)
	// The node of this config was in the table open() wrote, which the
	// load replaced: back in.
	if a.node != nil {
		if err := a.node.Register(a.ctx, false); err != nil {
			return err
		}
	}

	want, err := a.store.BlobCatalog(a.ctx)
	if err != nil {
		return err
	}
	restored, skipped := 0, 0
	root := filepath.Join(dir, backupBlobs)
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && path == root {
				return filepath.SkipAll // a backup without mail
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if err := a.ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 {
			return nil
		}
		account, sum := parts[0], parts[2]
		if _, _, err := blob.SplitKey(blob.Key(account, sum)); err != nil {
			return nil
		}
		err = a.store.PutBlobFile(a.ctx, account, sum, func() (io.ReadCloser, error) { return os.Open(path) })
		switch {
		case err == nil:
			restored++
		case want[account][sum]:
			return fmt.Errorf("blob %s: %w", blob.Key(account, sum), err)
		default:
			skipped++ // a file nothing refers to, of an account that is gone or damaged: left out
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("%w (the database is loaded; the blobs are not all in place: fix the cause and copy them with \"robinmail restore-blobs %s\")", walkErr, dir)
	}
	a.wait()
	var missing []string
	for account, sums := range want {
		for sum := range sums {
			if !a.local.Has(blob.Key(account, sum)) {
				missing = append(missing, blob.Key(account, sum))
			}
		}
	}
	sort.Strings(missing)
	fmt.Fprintf(a.stdout, "blobs: %d restored", restored)
	if skipped > 0 {
		fmt.Fprintf(a.stdout, ", %d unreferenced files left out", skipped)
	}
	fmt.Fprintln(a.stdout)
	a.note("restore", "", fmt.Sprintf("backup of %s from %s: %d accounts, %d blobs, %d missing", m.Hostname, m.CreatedAt.Format(time.RFC3339), m.Accounts, restored, len(missing)))
	fmt.Fprintf(a.stdout, "restored the backup of %s made %s: %d accounts, %d emails\n", m.Hostname, m.CreatedAt.Local().Format("2006-01-02 15:04:05"), m.Accounts, m.Emails)
	fmt.Fprintln(a.stdout, "The node table is empty but for this node: other nodes register when they start, and fetch their blobs from this one.")
	if len(missing) > 0 {
		for _, key := range missing[:min(len(missing), 20)] {
			fmt.Fprintf(a.stdout, "missing: %s\n", key)
		}
		return fmt.Errorf("%d blobs the database refers to are not in the backup: those messages cannot be read", len(missing))
	}
	return nil
}

// restoreBlobs copies the blob files of a backup into this node again,
// for a restore that loaded the database and then failed.
func (a *app) restoreBlobs(dir string) error {
	if _, err := readManifest(dir); err != nil {
		return err
	}
	want, err := a.store.BlobCatalog(a.ctx)
	if err != nil {
		return err
	}
	restored, failed := 0, 0
	for account, sums := range want {
		for sum := range sums {
			if a.local.Has(blob.Key(account, sum)) {
				continue
			}
			path := blobPath(dir, account, sum)
			if err := a.store.PutBlobFile(a.ctx, account, sum, func() (io.ReadCloser, error) { return os.Open(path) }); err != nil {
				failed++
				fmt.Fprintf(a.stdout, "blob %s: %v\n", blob.Key(account, sum), err)
				continue
			}
			restored++
		}
	}
	fmt.Fprintf(a.stdout, "blobs: %d restored, %d failed\n", restored, failed)
	if failed > 0 {
		return fmt.Errorf("%d blobs could not be restored", failed)
	}
	return nil
}
