package blob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Sweeper is a Store that can remove the blobs nothing refers to.
type Sweeper interface {
	Sweep(ctx context.Context, before time.Time, refs References) (SweepResult, error)
}

// References is how a sweep asks which blobs are in use.
type References interface {
	// AccountBlobs returns the hashes of all blobs the account refers to.
	// It is the cheap first look: one call per account.
	AccountBlobs(ctx context.Context, account string) (map[string]bool, error)
	// Referenced says, as of now and not from a cache or a stale replica,
	// whether the account refers to the blob. It decides a removal.
	Referenced(ctx context.Context, account, sha256hex string) (bool, error)
}

// SweepResult counts what a sweep did.
type SweepResult struct {
	Removed  int // unreferenced blob files removed
	Restored int // files it had claimed and put back
	Temp     int // leftover temporary files removed
}

// claimPrefix marks a file a sweep has taken out of its place and is about
// to remove.
const claimPrefix = ".sweep-"

// Sweep removes the blob files that no email refers to and that were
// written before the given time, and temporary files of that age that a
// crash left behind.
//
// A delivery writes the blob file first and the row that refers to it
// second, so for a moment a wanted file looks unreferenced. Two things keep
// a sweep from removing it, also when the delivery runs in another process:
//
//   - Age. Put replaces the file, which renews its modification time, also
//     when the same content was stored before. A sweep only touches files
//     older than before, which the caller sets a day back, far longer than
//     any delivery takes between its Put and its row.
//   - A claim. To remove a file the sweep first renames it aside, which is
//     atomic. Then it looks at the claimed file's time again and asks
//     Referenced. A Put that came in before the rename shows as a fresh
//     time; a Put that comes after it writes a new file in the old place,
//     which the sweep no longer touches. Only a claimed file that is still
//     old and still unreferenced is removed; otherwise it is put back,
//     unless a new file is already there.
//
// It is safe to run on several nodes, each on its own disk, and to run
// twice at once on one disk: a claim succeeds once.
func (l *Local) Sweep(ctx context.Context, before time.Time, refs References) (SweepResult, error) {
	var res SweepResult
	accounts, err := os.ReadDir(l.dir)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	for _, a := range accounts {
		if !a.IsDir() {
			continue
		}
		account := a.Name()
		if _, _, err := splitKey(Key(account, strings.Repeat("0", 64))); err != nil {
			continue // not a directory of ours
		}
		var inUse map[string]bool // loaded when the first old file shows up
		shards, err := os.ReadDir(filepath.Join(l.dir, account))
		if err != nil {
			return res, err
		}
		for _, sh := range shards {
			if !sh.IsDir() {
				continue
			}
			dir := filepath.Join(l.dir, account, sh.Name())
			files, err := os.ReadDir(dir)
			if err != nil {
				return res, err
			}
			for _, f := range files {
				if err := ctx.Err(); err != nil {
					return res, err
				}
				info, err := f.Info()
				if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(before) {
					continue // gone meanwhile, not a file, or too young to judge
				}
				name := f.Name()
				path := filepath.Join(dir, name)
				switch {
				case strings.HasPrefix(name, ".tmp-"):
					if os.Remove(path) == nil {
						res.Temp++
					}
					continue
				case strings.HasPrefix(name, claimPrefix):
					// A sweep died between its claim and its decision.
					sum := strings.TrimPrefix(name, claimPrefix)
					if _, _, err := splitKey(Key(account, sum)); err != nil {
						continue
					}
					if err := l.settle(ctx, refs, account, sum, path, filepath.Join(dir, sum), before, &res); err != nil {
						return res, err
					}
					continue
				}
				if _, _, err := splitKey(Key(account, name)); err != nil || name[:2] != sh.Name() {
					continue
				}
				if inUse == nil {
					if inUse, err = refs.AccountBlobs(ctx, account); err != nil {
						return res, err
					}
				}
				if inUse[name] {
					continue
				}
				claim := filepath.Join(dir, claimPrefix+name)
				if err := os.Rename(path, claim); err != nil {
					continue // another sweep has it, or it is gone
				}
				if err := l.settle(ctx, refs, account, name, claim, path, before, &res); err != nil {
					return res, err
				}
			}
			syncDir(dir)
		}
	}
	return res, nil
}

// settle decides about a claimed file: remove it, or put it back at path.
func (l *Local) settle(ctx context.Context, refs References, account, sum, claim, path string, before time.Time, res *SweepResult) error {
	keep := false
	info, err := os.Stat(claim)
	if err != nil {
		return nil // someone else settled it
	}
	if !info.ModTime().Before(before) {
		keep = true // a Put replaced the file just before the claim
	} else {
		used, err := refs.Referenced(ctx, account, sum)
		if err != nil {
			// Not knowing is no reason to remove. Put it back and stop.
			l.restore(claim, path)
			return err
		}
		keep = used
	}
	if keep {
		l.restore(claim, path)
		res.Restored++
		return nil
	}
	if err := os.Remove(claim); err == nil {
		res.Removed++
	}
	return nil
}

// restore puts a claimed file back unless a new file took its place. Link
// does not replace an existing file, which is what makes this safe against
// a Put that happens right now.
func (l *Local) restore(claim, path string) {
	if err := os.Link(claim, path); err == nil || errors.Is(err, os.ErrExist) {
		os.Remove(claim)
	}
}
