package store

import (
	"context"
)

// AddDomain makes the server accept mail for a domain. name is the canonical
// (lowercase) domain.
func (s *Store) AddDomain(ctx context.Context, name string) error {
	b := s.newBatch("")
	b.unique(ErrDomainExists)
	b.add(`INSERT INTO domain (name) VALUES (:name)`, "name", name)
	_, err := s.run(ctx, b)
	return err
}

// Domains lists the accepted domains by name.
func (s *Store) Domains(ctx context.Context) ([]string, error) {
	res, err := s.query(ctx, "", q(`SELECT name FROM domain ORDER BY name`))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, asString(row[0]))
	}
	return out, nil
}

// RemoveDomain stops accepting mail for a domain. It refuses with
// ErrDomainInUse while an account has an address in it: that account could
// no longer receive mail, and nothing would say why.
func (s *Store) RemoveDomain(ctx context.Context, name string) error {
	b := s.newBatch("")
	b.guard(ErrNoDomain, `EXISTS (SELECT 1 FROM domain WHERE name = :name)`, "name", name)
	b.guard(ErrDomainInUse, `NOT EXISTS (SELECT 1 FROM account WHERE `+sqlAddressDomain+` = :name)`, "name", name)
	b.add(`DELETE FROM domain WHERE name = :name`, "name", name)
	_, err := s.run(ctx, b)
	return err
}

// sqlAddressDomain is the domain of account.address. A stored address has
// exactly one "@": internal/address allows none in the local part.
const sqlAddressDomain = `substr(address, instr(address, '@') + 1)`
