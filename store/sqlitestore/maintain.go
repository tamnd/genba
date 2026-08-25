package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

var _ store.Maintenance = (*Store)(nil)
var _ store.Quarantine = (*Store)(nil)

// Quarantined returns documents this driver is holding back, newest first.
//
// The two fields that come out of the stored JSON are pulled out in SQL rather
// than by decoding the row. Decoding is how every other read path here gets at
// a document, and it is the wrong tool for this one: the body is in that JSON,
// a held document is as large as any other, and a hundred of them is megabytes
// read and parsed to reach two short strings. json_extract reads the same
// column and returns the two.
//
// Newest first because the question somebody has when they open this screen is
// whether the connector they just fixed has stopped producing them, and that is
// answered by the top of the list rather than by a hundred entries from
// whenever the corpus was first crawled.
func (s *Store) Quarantined(ctx context.Context, tenant string, limit int) ([]store.Held, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.query(ctx, `
		SELECT
			d.id,
			d.source,
			d.modified_at,
			json_extract(dd.data, '$.Title'),
			json_extract(dd.data, '$.Permissions.Reason')
		FROM document d
		JOIN document_data dd ON dd.doc_id = d.id
		WHERE d.tenant = ? AND d.queryable = 0
		ORDER BY d.modified_at DESC
		LIMIT ?`, tenant, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: quarantined: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]store.Held, 0, limit)
	for rows.Next() {
		var (
			h        store.Held
			modified sql.NullInt64
			title    sql.NullString
			reason   sql.NullString
		)
		if err := rows.Scan(&h.ID, &h.Source, &modified, &title, &reason); err != nil {
			return nil, fmt.Errorf("sqlitestore: quarantined: %w", err)
		}
		h.Title, h.Reason = title.String, reason.String
		if modified.Valid {
			h.At = time.Unix(0, modified.Int64).UTC()
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: quarantined: %w", err)
	}
	return out, nil
}

// Inventory calls fn for every document held for one tenant and source.
//
// It reads three columns of one table and nothing else. A reconciliation over a
// corpus of a few million documents is a few million ids, and decoding the
// stored JSON for each of them to reach a field the comparison does not use
// would turn a scan of an index into a read of the whole corpus. The third
// column is the flag the query path already filters on, so a held document
// costs a boolean here rather than a second pass.
func (s *Store) Inventory(ctx context.Context, tenant, source string, fn func(store.Item) bool) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, source_update, queryable FROM document WHERE tenant = ? AND source = ?`, tenant, source)
	if err != nil {
		return fmt.Errorf("sqlitestore: inventory: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id        string
			version   sql.NullString
			queryable bool
		)
		if err := rows.Scan(&id, &version, &queryable); err != nil {
			return fmt.Errorf("sqlitestore: inventory: %w", err)
		}
		if !fn(store.Item{ID: id, Version: version.String, Held: !queryable}) {
			return nil
		}
	}
	return rows.Err()
}

// SetPermissions replaces the access control lists of stored documents.
//
// The work it deliberately does not do is re-analyse the text. A permission
// change touches the columns the visibility predicate reads, the reference
// rows it joins against, and the copy of the document in the data table.
// Everything derived from the words is untouched, because the words did not
// change, which is what makes a company wide access control change cost a write
// per document rather than a recrawl.
//
// The exception is a document that crosses the quarantine line in either
// direction. A quarantined document is not in the full text index or the corpus
// statistics at all, so one that becomes readable has to be put in and one that
// stops being readable has to come out, and that is the one case where the
// analyzer runs.
func (s *Store) SetPermissions(ctx context.Context, tenant string, perms map[string]acl.Permissions) (int, error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	if len(perms) == 0 {
		return 0, nil
	}

	s.write.Lock()
	defer s.write.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: set permissions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	w, err := newWriter(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: set permissions: %w", err)
	}
	defer w.close()

	changed := 0
	written := make(map[string][]string)
	for id, p := range perms {
		ok, err := setOne(ctx, tx, w, tenant, id, p)
		if err != nil {
			return 0, fmt.Errorf("sqlitestore: set permissions %s: %w", id, err)
		}
		if ok {
			written[tenant] = append(written[tenant], id)
			changed++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlitestore: set permissions: %w", err)
	}
	s.Changes(written, false)
	return changed, nil
}

// setOne rewrites the permissions of one document and reports whether there was
// one to rewrite.
func setOne(ctx context.Context, tx *sql.Tx, w *writer, tenant, id string, p acl.Permissions) (bool, error) {
	var (
		rowid int64
		was   int
		data  string
	)
	// The tenant is part of the predicate rather than something checked
	// afterwards, so a caller that names an id belonging to somebody else
	// changes nothing rather than changing their access control list.
	err := tx.QueryRowContext(ctx, `
		SELECT d.rowid, d.queryable, dd.data
		FROM document d JOIN document_data dd ON dd.doc_id = d.id
		WHERE d.id = ? AND d.tenant = ?`, id, tenant).Scan(&rowid, &was, &data)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}

	d, err := decode(data)
	if err != nil {
		return false, err
	}
	d.Permissions = p
	now := boolInt(d.Queryable())

	// Crossing the line out of the index. retire is what takes the postings and
	// the corpus statistics back out, and it has to run while the row still
	// says what it used to contribute.
	if was != now {
		if _, _, _, err := w.retire(ctx, id); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM document_fts WHERE rowid = ?`, rowid); err != nil {
			return false, err
		}
	}

	var ownerKey string
	if p.Owner.Value != "" {
		ownerKey = p.Owner.UserKey()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE document SET mode = ?, owner_key = ?, queryable = ? WHERE rowid = ?`,
		int(p.Mode), ownerKey, now, rowid); err != nil {
		return false, err
	}

	raw, err := json.Marshal(d)
	if err != nil {
		return false, fmt.Errorf("encode: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE document_data SET data = ? WHERE doc_id = ?`, string(raw), id); err != nil {
		return false, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM document_ref WHERE doc_id = ?`, id); err != nil {
		return false, err
	}
	for _, r := range refsOf(p) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO document_ref (doc_id, effect, scope, key) VALUES (?, ?, ?, ?)`,
			id, r.effect, r.scope, r.key); err != nil {
			return false, err
		}
	}

	// Crossing the line back into the index, which is the only path here that
	// pays for the analyzer.
	if was != now && now == 1 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO document_fts (rowid, terms) VALUES (?, ?)`, rowid, d.Analyzed()); err != nil {
			return false, err
		}
		if err := w.index(ctx, rowid, d, d.Analyze()); err != nil {
			return false, err
		}
	}
	return true, nil
}
