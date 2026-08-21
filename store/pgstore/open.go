package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

var _ store.OpenLog = (*Store)(nil)

// RecordOpen notes that the principal opened a document.
//
// The insert selects from document rather than binding the id directly, and the
// select carries the visibility predicate, so a write for something this person
// may not read inserts no rows. That is also the only form of this that cannot
// race: nothing can change between the check and the write when they are the
// same statement.
//
// It deliberately does not take the write lock the ingestion path holds. This
// table is not part of the corpus bookkeeping, and a person opening a document
// while a crawl is running should not be waiting for the crawl.
func (s *Store) RecordOpen(ctx context.Context, p *acl.Principal, id string, at time.Time) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}

	c := visible(p)
	insert := append([]any{p.Tenant, p.Subject, at.UnixNano(), id}, c.args...)
	err := s.retry(ctx, func(ctx context.Context) error {
		s.counters.statements.Add(1)
		if _, err := s.pool.Exec(ctx, rebind(`
			INSERT INTO document_open (tenant, subject, doc_id, opened_at)
			SELECT ?, ?, d.id, ? FROM document d WHERE d.id = ? AND `+c.where()+`
			ON CONFLICT (tenant, subject, doc_id) DO UPDATE SET opened_at = excluded.opened_at`),
			insert...); err != nil {
			return err
		}
		// The history is trimmed here rather than by a job, because the only
		// moment it can grow is this one and a deployment nobody prunes is the
		// normal case.
		s.counters.statements.Add(1)
		_, err := s.pool.Exec(ctx, rebind(`
			DELETE FROM document_open o
			WHERE o.tenant = ? AND o.subject = ? AND o.doc_id NOT IN (
				SELECT doc_id FROM document_open
				WHERE tenant = ? AND subject = ?
				ORDER BY opened_at DESC LIMIT ?
			)`), p.Tenant, p.Subject, p.Tenant, p.Subject, store.OpenHistory)
		return err
	})
	if err != nil {
		return fmt.Errorf("pgstore: record open: %w", err)
	}
	return nil
}

// Opens returns what the principal opened, most recent first.
//
// The visibility predicate is in the same statement as the history, so a
// document that stopped being readable is gone from the list without a second
// query and without a chance to forget the rule.
func (s *Store) Opens(ctx context.Context, p *acl.Principal, limit int) ([]store.Open, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, genba.ErrNoPrincipal
	}
	if limit <= 0 {
		return nil, nil
	}

	c := visible(p)
	args := append([]any{p.Tenant, p.Subject}, c.args...)
	args = append(args, limit)

	var out []store.Open
	err := s.retry(ctx, func(ctx context.Context) error {
		out = out[:0]
		rows, err := s.query(ctx, `
			SELECT o.opened_at, x.data
			FROM document_open o
			JOIN document d ON d.id = o.doc_id
			JOIN document_data x ON x.doc_id = d.id
			WHERE o.tenant = ? AND o.subject = ? AND `+c.where()+`
			ORDER BY o.opened_at DESC
			LIMIT ?`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var (
				at   int64
				data string
			)
			if err := rows.Scan(&at, &data); err != nil {
				return err
			}
			s.counters.rows.Add(1)
			d, err := s.decoded(data)
			if err != nil {
				return err
			}
			out = append(out, store.Open{Document: d, At: unixNano(at)})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: opens: %w", err)
	}
	return out, nil
}
