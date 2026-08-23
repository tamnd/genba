package sqlitestore

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
// Reports below writes this out again rather than using the constant, because it
// has a page of ids to pin the join order with and the pinning has to happen
// inside the subquery. The two have to say the same thing, and the conformance
// suite is what holds them to it.
//
// The tie break on by_key is not decoration. Two reports written in the same
// nanosecond are possible on a test clock and only just impossible on a real
// one, and a query that picks either of them picks a different one on the next
// call, which is a screen that changes when nothing did.
const latest = `
	SELECT doc_id, reporter, reported_at, note,
	       COUNT(*)     OVER (PARTITION BY doc_id) AS n,
	       ROW_NUMBER() OVER (PARTITION BY doc_id ORDER BY reported_at DESC, by_key) AS rn
	FROM document_report`

// Report records that somebody says the document is out of date.
//
// The insert selects from document with the visibility predicate on it, exactly
// as recording an open and recording a verification do, so a report about
// something this principal may not read inserts no rows and says nothing about
// whether the id exists.
func (s *Store) Report(ctx context.Context, p *acl.Principal, r store.Report) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	if err := r.Check(); err != nil {
		return fmt.Errorf("sqlitestore: report: %w", err)
	}
	key := store.ReportKey(p)
	if key == "" {
		return fmt.Errorf("sqlitestore: report: %w", store.ErrNoReporter)
	}
	reporter, err := marshalPerson(r.By)
	if err != nil {
		return fmt.Errorf("sqlitestore: report: %w", err)
	}

	c := visible(p)
	args := append([]any{key, reporter, r.At.UnixNano(), r.Note, r.Doc}, c.args...)
	s.counters.statements.Add(1)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO document_report (doc_id, by_key, reporter, reported_at, note)
		SELECT d.id, ?, ?, ?, ? FROM document d WHERE d.id = ? AND `+c.where()+`
		ON CONFLICT (doc_id, by_key) DO UPDATE SET
			reporter    = excluded.reporter,
			reported_at = excluded.reported_at,
			note        = excluded.note`,
		args...); err != nil {
		return fmt.Errorf("sqlitestore: report: %w", err)
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
	s.counters.statements.Add(1)
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM document_report
		WHERE doc_id IN (SELECT d.id FROM document d WHERE d.id = ? AND `+c.where()+`)`,
		args...); err != nil {
		return fmt.Errorf("sqlitestore: resolve: %w", err)
	}
	return nil
}

// Reports returns what has been said about the documents the principal may read.
//
// The join order is pinned for the reason written out over Verifications, and
// the measurement there applies unchanged: the visibility clause carries
// correlated subqueries, this table has no statistics saying it is nearly empty
// for a given page of ids, and letting the planner drive the join from document
// turns a handful of primary key probes into a walk of the corpus.
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
	args := append([]any{jsonList(ids)}, c.args...)
	rows, err := s.query(ctx, `
		SELECT r.doc_id, r.reporter, r.reported_at, r.note, r.n
		FROM (
			SELECT r.doc_id, r.reporter, r.reported_at, r.note,
			       COUNT(*)     OVER (PARTITION BY r.doc_id) AS n,
			       ROW_NUMBER() OVER (PARTITION BY r.doc_id ORDER BY r.reported_at DESC, r.by_key) AS rn
			FROM json_each(?) j
			CROSS JOIN document_report r ON r.doc_id = j.value
		) r
		CROSS JOIN document d ON d.id = r.doc_id
		WHERE r.rn = 1 AND `+c.where(), args...)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: reports: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out map[string]store.Staleness
	for rows.Next() {
		said, err := scanStaleness(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlitestore: reports: %w", err)
		}
		s.counters.rows.Add(1)
		if out == nil {
			out = make(map[string]store.Staleness, len(ids))
		}
		out[said.Doc] = said
	}
	return out, rows.Err()
}

// Reported returns the documents the principal owns or wrote that somebody has
// reported, most recently reported first.
//
// Owns or wrote is the same membership test an owner: filter is, against the
// folded key columns the index already keeps, so the question is answered while
// SQLite walks its own rows rather than by reading documents and deciding in Go.
//
// The join order is pinned for the reason written out over Verifications and
// Reports, and this is the query that proves the point rather than restating it.
// Left to itself the planner drives from document, because it has no statistics
// saying the report table holds six rows against the corpus's twenty thousand,
// and then evaluates the ownership test and the visibility predicate over every
// document in the tenant. Measured on the benchmark corpus that was 900
// milliseconds for a panel on the front page. Driven from the reports, which is
// what the CROSS JOIN forces, the same answer is a few hundred microseconds:
// the work is proportional to what has been reported, which is the only thing
// this endpoint is about.
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

	keys := jsonKeys(store.PrincipalKeys(p))
	c := visible(p)
	args := append([]any{keys, keys}, c.args...)
	args = append(args, limit)
	rows, err := s.query(ctx, `
		SELECT r.doc_id, r.reporter, r.reported_at, r.note, r.n, x.data
		FROM (`+latest+`) r
		CROSS JOIN document d ON d.id = r.doc_id
		CROSS JOIN document_data x ON x.doc_id = d.id
		WHERE r.rn = 1
		  AND (
			EXISTS (SELECT 1 FROM json_each(d.owner_keys) k WHERE k.value IN (SELECT value FROM json_each(?)))
			OR EXISTS (SELECT 1 FROM json_each(d.author_keys) k WHERE k.value IN (SELECT value FROM json_each(?)))
		  )
		  AND `+c.where()+`
		ORDER BY r.reported_at DESC, r.doc_id
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: reported: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]store.Flagged, 0, limit)
	for rows.Next() {
		var data string
		said, err := scanStaleness(rows, &data)
		if err != nil {
			return nil, fmt.Errorf("sqlitestore: reported: %w", err)
		}
		s.counters.rows.Add(1)
		d, err := s.decoded(data)
		if err != nil {
			return nil, err
		}
		out = append(out, store.Flagged{Document: d, Stale: said})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// scanStaleness reads the five columns both read paths select, and whatever
// else the caller put after them.
func scanStaleness(rows scanner, rest ...any) (store.Staleness, error) {
	var (
		said     store.Staleness
		reporter string
		at       int64
	)
	dest := append([]any{&said.Doc, &reporter, &at, &said.Last.Note, &said.Count}, rest...)
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

// scanner is the one method of *sql.Rows the helper above needs.
type scanner interface {
	Scan(dest ...any) error
}
