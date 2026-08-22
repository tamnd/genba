package pgstore

import (
	"context"
	"fmt"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

var _ store.Verifier = (*Store)(nil)

// Verify records that somebody vouches for a document.
//
// The insert selects from document with the visibility predicate on it, exactly
// as recording an open does, so a claim about something this principal may not
// read inserts no rows. One statement rather than a read, a decision in Go and
// a write, which is also the only form of this that cannot race.
//
// It does not take the write lock the ingestion path holds. Verifying is not
// part of the corpus bookkeeping, and somebody putting their name to a runbook
// while a crawl is running should not be waiting for the crawl.
func (s *Store) Verify(ctx context.Context, p *acl.Principal, v store.Verification) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	if err := v.Check(); err != nil {
		return fmt.Errorf("pgstore: verify: %w", err)
	}

	c := visible(p)
	args := append(
		[]any{v.By.Subject, v.By.Name, v.By.Email, v.At.UnixNano(), v.Until.UnixNano(), v.Note, v.Doc},
		c.args...,
	)
	err := s.retry(ctx, func(ctx context.Context) error {
		s.counters.statements.Add(1)
		_, err := s.pool.Exec(ctx, rebind(`
			INSERT INTO document_verify (doc_id, by_subject, by_name, by_email, verified_at, expires_at, note)
			SELECT d.id, ?, ?, ?, ?, ?, ? FROM document d WHERE d.id = ? AND `+c.where()+`
			ON CONFLICT (doc_id) DO UPDATE SET
				by_subject  = excluded.by_subject,
				by_name     = excluded.by_name,
				by_email    = excluded.by_email,
				verified_at = excluded.verified_at,
				expires_at  = excluded.expires_at,
				note        = excluded.note`), args...)
		return err
	})
	if err != nil {
		return fmt.Errorf("pgstore: verify: %w", err)
	}
	return nil
}

// Unverify withdraws the claim.
func (s *Store) Unverify(ctx context.Context, p *acl.Principal, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}

	c := visible(p)
	args := append([]any{id}, c.args...)
	err := s.retry(ctx, func(ctx context.Context) error {
		s.counters.statements.Add(1)
		_, err := s.pool.Exec(ctx, rebind(`
			DELETE FROM document_verify v
			WHERE v.doc_id IN (SELECT d.id FROM document d WHERE d.id = ? AND `+c.where()+`)`), args...)
		return err
	})
	if err != nil {
		return fmt.Errorf("pgstore: unverify: %w", err)
	}
	return nil
}

// Verifications returns the claims on the documents the principal may read.
//
// One statement for the whole page. The ids go in as a bound text array rather
// than as a list of placeholders, which is what the filters in query.go do, and
// it means a page of twenty and a page of two hundred prepare the same
// statement.
func (s *Store) Verifications(ctx context.Context, p *acl.Principal, ids []string) (map[string]store.Verification, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, genba.ErrNoPrincipal
	}
	if len(ids) == 0 {
		return nil, nil
	}

	c := visible(p)
	args := append([]any{ids}, c.args...)

	var out map[string]store.Verification
	err := s.retry(ctx, func(ctx context.Context) error {
		out = nil
		rows, err := s.query(ctx, `
			SELECT v.doc_id, v.by_subject, v.by_name, v.by_email, v.verified_at, v.expires_at, v.note
			FROM document_verify v
			JOIN document d ON d.id = v.doc_id
			WHERE v.doc_id = ANY(?) AND `+c.where(), args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var (
				v                store.Verification
				verified, expiry int64
			)
			if err := rows.Scan(&v.Doc, &v.By.Subject, &v.By.Name, &v.By.Email, &verified, &expiry, &v.Note); err != nil {
				return err
			}
			s.counters.rows.Add(1)
			v.At, v.Until = unixNano(verified), unixNano(expiry)
			if out == nil {
				out = make(map[string]store.Verification, len(ids))
			}
			out[v.Doc] = v
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: verifications: %w", err)
	}
	return out, nil
}
