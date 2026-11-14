package blob

import (
	"context"
	"fmt"
)

// ScrubResult counts what a scrub found.
type ScrubResult struct {
	Checked  int // referenced blob files on this disk that were read
	Corrupt  int // of those, files that are not the blob their name says
	Repaired int // of those, replaced by a good copy from another node
	// Bad names the files that are still corrupt afterwards (at most
	// maxScrubNames of them).
	Bad []string
}

const maxScrubNames = 100

// Scrub reads every blob file on this disk that the database refers to
// and lets verify decide whether it still is the blob its name says: a
// disk that returns other bytes than were written does not say so by
// itself. For a file that fails, repair is asked to put a good one in its
// place; with a nil repair, or when that fails, the file stays where it is
// and is reported. A file the disk lacks is not Scrub's business (Repair).
//
// It reads one file after the other, which is all the pacing it has.
func (l *Local) Scrub(ctx context.Context, cat Catalog, verify Verifier, repair func(ctx context.Context, key string) error) (ScrubResult, error) {
	var res ScrubResult
	accounts, err := cat.Accounts(ctx)
	if err != nil {
		return res, err
	}
	for _, account := range accounts {
		want, err := cat.AccountBlobs(ctx, account)
		if err != nil {
			return res, err
		}
		if len(want) == 0 {
			continue
		}
		have, err := l.List(account)
		if err != nil {
			return res, err
		}
		for sum := range want {
			if !have[sum] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return res, err
			}
			key := Key(account, sum)
			f, err := l.Get(ctx, key)
			if err != nil {
				continue // removed meanwhile
			}
			err = verify(ctx, key, f)
			f.Close()
			res.Checked++
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				res.Checked--
				return res, ctx.Err()
			}
			res.Corrupt++
			if repair != nil && repair(ctx, key) == nil {
				res.Repaired++
				continue
			}
			if len(res.Bad) < maxScrubNames {
				res.Bad = append(res.Bad, key)
			}
		}
	}
	if n := res.Corrupt - res.Repaired; n > 0 {
		return res, fmt.Errorf("blob: %d corrupt files could not be repaired, the first: %s", n, res.Bad[0])
	}
	return res, nil
}

// Scrub is Local.Scrub with the other nodes to repair from: a corrupt file
// is replaced by the first good copy a peer has.
func (r *Replicated) Scrub(ctx context.Context, cat Catalog) (ScrubResult, error) {
	res, err := r.Local.Scrub(ctx, cat, r.Verify, r.fetch)
	if res.Corrupt > 0 {
		r.log().Warn("blob scrub found corrupt files", "checked", res.Checked, "corrupt", res.Corrupt, "repaired", res.Repaired)
	}
	return res, err
}
