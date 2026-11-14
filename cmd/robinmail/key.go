package main

import (
	"errors"
	"fmt"
	"sort"

	"github.com/mikepage/robinmail/internal/blob"
)

// Replacing the master key, in steps that never leave a node unable to
// read an account's key:
//
//	key add                 a new key joins the file, accepted, not used
//	  (copy the file to every node and restart them)
//	key rotate <fingerprint>  the new key becomes the current one in this
//	                        node's file and every account's key is wrapped
//	                        under it
//	  (copy the file to every node and restart them; run key rotate again:
//	   it picks up accounts a node made under the old key meanwhile)
//	key prune               the keys no account uses leave the file
//	  (copy the file to every node and restart them)
//
// Every step can be stopped and run again.

func (a *app) keyStatus() error {
	use, err := a.store.AccountKeyUse(a.ctx)
	if err != nil {
		return err
	}
	for i, fp := range a.master.Fingerprints() {
		role := "accepted"
		if i == 0 {
			role = "current "
		}
		fmt.Fprintf(a.stdout, "%s  %s  %d accounts\n", fp, role, use.ByKey[fp])
	}
	if use.Unreadable > 0 {
		fmt.Fprintf(a.stdout, "%d accounts have a key that no master key in %s unwraps\n", use.Unreadable, a.cfg.MasterKeyFile)
		return fmt.Errorf("%d accounts cannot be read with this key file", use.Unreadable)
	}
	return nil
}

func (a *app) keyAdd() error {
	raw, err := blob.NewRandomKey()
	if err != nil {
		return err
	}
	m, err := a.master.With(raw)
	if err != nil {
		return err
	}
	if err := m.WriteFile(a.cfg.MasterKeyFile); err != nil {
		return err
	}
	fps := m.Fingerprints()
	fp := fps[len(fps)-1]
	a.note("key-added", "", fp)
	fmt.Fprintf(a.stdout, `added key %s to %s; it is accepted and not used yet.
Next:
  1. Back the file up, copy it to every node and restart each of them.
  2. robinmail key rotate %s
`, fp, a.cfg.MasterKeyFile, fp)
	return nil
}

func (a *app) keyRotate(fingerprint string) error {
	m, err := a.master.Promote(fingerprint)
	if err != nil {
		return fmt.Errorf("%s has no key %s (see \"robinmail key status\")", a.cfg.MasterKeyFile, fingerprint)
	}
	if a.master.Fingerprints()[0] != fingerprint {
		// The file first: from here on this node wraps under the new key,
		// and a second run finds the same state.
		if err := m.WriteFile(a.cfg.MasterKeyFile); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "%s is now the current key in %s\n", fingerprint, a.cfg.MasterKeyFile)
	}
	a.master = m
	a.store.UseMasterKey(m)
	changed, already, err := a.store.RewrapAccountKeys(a.ctx)
	fmt.Fprintf(a.stdout, "wrapped the keys of %d accounts under %s; %d already were\n", changed, fingerprint, already)
	if err != nil {
		return fmt.Errorf("%w (run the command again: it continues where it stopped)", err)
	}
	a.note("key-rotated", "", fmt.Sprintf("%s, %d accounts", fingerprint, changed))
	if len(m.Fingerprints()) > 1 {
		fmt.Fprintf(a.stdout, `Next:
  1. Copy %s to every node and restart each of them.
  2. robinmail key rotate %s    again: accounts made by a node that still had the old order
  3. robinmail key prune        once no node runs with the old file
`, a.cfg.MasterKeyFile, fingerprint)
	}
	return nil
}

func (a *app) keyPrune() error {
	fps := a.master.Fingerprints()
	if len(fps) == 1 {
		fmt.Fprintln(a.stdout, "the key file holds one key: nothing to prune")
		return nil
	}
	use, err := a.store.AccountKeyUse(a.ctx)
	if err != nil {
		return err
	}
	if use.Unreadable > 0 {
		return fmt.Errorf("%d accounts have a key that no master key in the file unwraps: not changing the file", use.Unreadable)
	}
	keep := map[string]bool{fps[0]: true}
	var dropped, used []string
	for _, fp := range fps[1:] {
		if use.ByKey[fp] > 0 {
			keep[fp] = true
			used = append(used, fmt.Sprintf("%s (%d accounts)", fp, use.ByKey[fp]))
		} else {
			dropped = append(dropped, fp)
		}
	}
	sort.Strings(used)
	if len(used) > 0 {
		for _, u := range used {
			fmt.Fprintf(a.stdout, "still in use: %s\n", u)
		}
		return errors.New("accounts are still wrapped under an old key: run \"robinmail key rotate " + fps[0] + "\" first")
	}
	m, err := a.master.Only(keep)
	if err != nil {
		return err
	}
	if err := m.WriteFile(a.cfg.MasterKeyFile); err != nil {
		return err
	}
	a.master = m
	a.store.UseMasterKey(m)
	for _, fp := range dropped {
		a.note("key-pruned", "", fp)
		fmt.Fprintf(a.stdout, "removed key %s from %s\n", fp, a.cfg.MasterKeyFile)
	}
	fmt.Fprintf(a.stdout, `Copy the file to every node and restart each of them.
A backup made before the rotation needs the old key: keep a copy of the old file with it.
Forwarded mail whose sender address was rewritten under the old key can no longer bounce back through this server.
`)
	return nil
}
