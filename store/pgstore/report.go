package pgstore

import (
	"context"
	"fmt"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

var _ store.Reporter = (*Store)(nil)

// latest is the one report per document a screen draws, in SQL.
//
// A document has as many rows in this table as it has people who complained
// about it, and both read paths want the same two things out of them: how many
// there are and what the most recent one says. A window function answers both at
// once and returns one row per document however many people have piled on, so a
// runbook that annoyed a hundred of them is still one row on its way out.
//
// The tie break on by_key is not decoration. Two reports written in the same
// nanosecond are possible on a test clock and only just impossible on a real
// one, and a query that picks either of them picks a different one on the next
// call, which is a screen that changes when nothing did.
//
// The third window is whether the person asking is one of the people who
// complained, which is a question about a row the other two throw away: the one
// they keep is the most recent, and the person asking is usually not the most
// recent person to have complained. Asking it here costs the partition that is
// already being walked rather than a second visit to the table, and it takes the
// key as a parameter, so both callers bind one.
const latest = `
	SELECT doc_id, reporter, reported_at, note,
	       count(*)     OVER (PARTITION BY doc_id) AS n,
	       bool_or(by_key = ?) OVER (PARTITION BY doc_id) AS mine,
	       row_number() OVER (PARTITION BY doc_id ORDER BY reported_at DESC, by_key) AS rn
	FROM document_report`

// Report records that somebody says the document is out of date.
//
// The insert selects from document with the visibility predicate on it, exactly
// as recording an open and recording a verification do, so a report about
// something this principal may not read inserts no rows and says nothing about
// whether the id exists.
//
// It does not take the write lock the ingestion path holds, for the reason
// Verify does not: telling us a runbook is wrong is not corpus bookkeeping, and
// somebody doing it while a crawl runs should not be waiting for the crawl.
func (s *Store) Report(ctx context.Context, p *acl.Principal, r store.Report) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	if err := r.Check(); err != nil {
		return fmt.Errorf("pgstore: report: %w", err)
	}
	key := store.ReportKey(p)
	if key == "" {
		return fmt.Errorf("pgstore: report: %w", store.ErrNoReporter)
	}
	reporter, err := marshalPerson(r.By)
	if err != nil {
		return fmt.Errorf("pgstore: report: %w", err)
	}

	c := visible(p)
	args := append([]any{key, reporter, r.At.UnixNano(), r.Note, r.Doc}, c.args...)
	err = s.retry(ctx, func(ctx context.Context) error {
		s.counters.statements.Add(1)
		_, err := s.pool.Exec(ctx, rebind(`
			INSERT INTO document_report (doc_id, by_key, reporter, reported_at, note)
			SELECT d.id, ?, ?, ?, ? FROM document d WHERE d.id = ? AND `+c.where()+`
			ON CONFLICT (doc_id, by_key) DO UPDATE SET
				reporter    = excluded.reporter,
				reported_at = excluded.reported_at,
				note        = excluded.note`), args...)
		return err
	})
	if err != nil {
		return fmt.Errorf("pgstore: report: %w", err)
	}
	return nil
}

// Resolve clears every report on one document.
//
// Every one of them, because what was reported was the document and whoever
// dealt with it dealt with all of it. Clearing a document nobody reported
// deletes no rows and is not an error, which is what lets the API call this
// after a verification without asking first.
func (s *Store) Resolve(ctx context.Context, p *acl.Principal, id string) error {
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
			DELETE FROM document_report r
			WHERE r.doc_id IN (SELECT d.id FROM document d WHERE d.id = ? AND `+c.where()+`)`), args...)
		return err
	})
	if err != nil {
		return fmt.Errorf("pgstore: resolve: %w", err)
	}
	return nil
}

// Withdraw removes the report this principal wrote, and only that one.
//
// The delete is Resolve's with by_key on it, so the visibility predicate stays
// where it is and the two cannot disagree about which documents can be touched.
// What it adds is the whole difference between the two calls: a key that can
// only ever match the row this principal wrote, which is why taking your own
// report back needs no permission and clearing everybody's needs the same one as
// verifying.
func (s *Store) Withdraw(ctx context.Context, p *acl.Principal, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	key := store.ReportKey(p)
	if key == "" {
		return fmt.Errorf("pgstore: withdraw: %w", store.ErrNoReporter)
	}

	c := visible(p)
	args := append([]any{key, id}, c.args...)
	err := s.retry(ctx, func(ctx context.Context) error {
		s.counters.statements.Add(1)
		_, err := s.pool.Exec(ctx, rebind(`
			DELETE FROM document_report r
			WHERE r.by_key = ?
			  AND r.doc_id IN (SELECT d.id FROM document d WHERE d.id = ? AND `+c.where()+`)`), args...)
		return err
	})
	if err != nil {
		return fmt.Errorf("pgstore: withdraw: %w", err)
	}
	return nil
}

// Reports returns what has been said about the documents the principal may read.
//
// One statement for the whole page, with the ids bound as a text array, exactly
// as Verifications does it.
//
// Visibility is applied after the counting rather than inside it, which is
// correct because the rule is about the document and not about who complained:
// every row for a document is either readable by this principal or none of them
// is.
func (s *Store) Reports(ctx context.Context, p *acl.Principal, ids []string) (map[string]store.Staleness, error) {
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
	args := append([]any{store.ReportKey(p), ids}, c.args...)

	var out map[string]store.Staleness
	err := s.retry(ctx, func(ctx context.Context) error {
		out = nil
		rows, err := s.query(ctx, `
			SELECT r.doc_id, r.reporter, r.reported_at, r.note, r.n, r.mine
			FROM (`+latest+` WHERE doc_id = ANY(?)) r
			JOIN document d ON d.id = r.doc_id
			WHERE r.rn = 1 AND `+c.where(), args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			said, err := scanStaleness(rows)
			if err != nil {
				return err
			}
			s.counters.rows.Add(1)
			if out == nil {
				out = make(map[string]store.Staleness, len(ids))
			}
			out[said.Doc] = said
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: reports: %w", err)
	}
	return out, nil
}

// Reported returns the documents the principal owns or wrote that somebody has
// reported, most recently reported first.
//
// Owns or wrote is the same array overlap an owner: filter is, against the two
// key columns that already carry a gin index, so the question is answered from
// the index rather than by reading documents and deciding in Go.
func (s *Store) Reported(ctx context.Context, p *acl.Principal, limit int) ([]store.Flagged, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, genba.ErrNoPrincipal
	}
	if limit <= 0 {
		return nil, nil
	}

	keys := nonEmpty(store.PrincipalKeys(p))
	c := visible(p)
	args := append([]any{store.ReportKey(p), keys, keys}, c.args...)
	args = append(args, limit)

	var out []store.Flagged
	err := s.retry(ctx, func(ctx context.Context) error {
		out = nil
		rows, err := s.query(ctx, `
			SELECT r.doc_id, r.reporter, r.reported_at, r.note, r.n, r.mine, x.data
			FROM (`+latest+`) r
			JOIN document d ON d.id = r.doc_id
			JOIN document_data x ON x.doc_id = d.id
			WHERE r.rn = 1
			  AND (d.owner_keys && ?::text[] OR d.author_keys && ?::text[])
			  AND `+c.where()+`
			ORDER BY r.reported_at DESC, r.doc_id
			LIMIT ?`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var data string
			said, err := scanStaleness(rows, &data)
			if err != nil {
				return err
			}
			s.counters.rows.Add(1)
			d, err := s.decoded(data)
			if err != nil {
				return err
			}
			out = append(out, store.Flagged{Document: d, Stale: said})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: reported: %w", err)
	}
	return out, nil
}

// scanStaleness reads the six columns both read paths select, and whatever else
// the caller put after them.
func scanStaleness(rows scanner, rest ...any) (store.Staleness, error) {
	var (
		said     store.Staleness
		reporter string
		at       int64
	)
	dest := append([]any{&said.Doc, &reporter, &at, &said.Last.Note, &said.Count, &said.Mine}, rest...)
	if err := rows.Scan(dest...); err != nil {
		return store.Staleness{}, err
	}
	who, err := unmarshalPerson(reporter)
	if err != nil {
		return store.Staleness{}, err
	}
	said.Last.Doc, said.Last.By, said.Last.At = said.Doc, who, unixNano(at)
	return said, nil
}

// scanner is the one method of the row iterator the helper above needs.
type scanner interface {
	Scan(dest ...any) error
}
