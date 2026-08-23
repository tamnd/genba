package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

var _ store.Ownership = (*Store)(nil)

// SetOwner records a correction and writes the new owner into the document.
//
// It takes the write lock and does its work in one transaction, because it is a
// read of what the document says now followed by two writes that depend on it,
// and the whole point of the feature is that the answer does not change under
// somebody.
func (s *Store) SetOwner(ctx context.Context, p *acl.Principal, c store.Correction) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	if err := c.Check(); err != nil {
		return fmt.Errorf("sqlitestore: set owner: %w", err)
	}

	s.write.Lock()
	defer s.write.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: set owner: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The document is read through the visibility predicate, exactly as
	// recording an open is, so a correction to something this principal may not
	// read reads no rows and writes nothing.
	//
	// The source's answer comes from the standing correction when there is one
	// and from the document otherwise. Reading it from the document in both
	// cases would remember the previous person's guess as what the connector
	// said, and clearing the correction would then hand the document to somebody
	// who was themselves a correction.
	v := visible(p)
	args := append([]any{c.Doc}, v.args...)
	var (
		current string
		was     sql.NullString
	)
	s.counters.statements.Add(1)
	err = tx.QueryRowContext(ctx, `
		SELECT json_extract(dd.data, '$.Owner'), o.was
		FROM document d
		JOIN document_data dd ON dd.doc_id = d.id
		LEFT JOIN document_own o ON o.doc_id = d.id
		WHERE d.id = ? AND `+v.where(), args...).Scan(&current, &was)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sqlitestore: set owner: %w", err)
	}
	if !was.Valid {
		was = sql.NullString{String: current, Valid: true}
	}

	owner, err := marshalPerson(c.Owner)
	if err != nil {
		return fmt.Errorf("sqlitestore: set owner: %w", err)
	}
	by, err := marshalPerson(c.By)
	if err != nil {
		return fmt.Errorf("sqlitestore: set owner: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO document_own (doc_id, owner, was, corrected_by, corrected_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (doc_id) DO UPDATE SET
			owner        = excluded.owner,
			was          = excluded.was,
			corrected_by = excluded.corrected_by,
			corrected_at = excluded.corrected_at`,
		c.Doc, owner, was.String, by, c.At.UnixNano()); err != nil {
		return fmt.Errorf("sqlitestore: set owner: %w", err)
	}
	if err := writeOwner(ctx, tx, c.Doc, owner, c.Owner); err != nil {
		return fmt.Errorf("sqlitestore: set owner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: set owner: %w", err)
	}
	return nil
}

// ClearOwner drops the correction and puts the source's answer back.
func (s *Store) ClearOwner(ctx context.Context, p *acl.Principal, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}

	s.write.Lock()
	defer s.write.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: clear owner: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	v := visible(p)
	args := append([]any{id}, v.args...)
	var was string
	s.counters.statements.Add(1)
	err = tx.QueryRowContext(ctx, `
		SELECT o.was FROM document_own o
		JOIN document d ON d.id = o.doc_id
		WHERE o.doc_id = ? AND `+v.where(), args...).Scan(&was)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing to clear, or nothing this principal may see. Both are quiet,
		// which is what makes undoing a mistake safe to do twice.
		return nil
	}
	if err != nil {
		return fmt.Errorf("sqlitestore: clear owner: %w", err)
	}

	source, err := unmarshalPerson(was)
	if err != nil {
		return fmt.Errorf("sqlitestore: clear owner: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM document_own WHERE doc_id = ?`, id); err != nil {
		return fmt.Errorf("sqlitestore: clear owner: %w", err)
	}
	if err := writeOwner(ctx, tx, id, was, source); err != nil {
		return fmt.Errorf("sqlitestore: clear owner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: clear owner: %w", err)
	}
	return nil
}

// Corrections returns the corrections on the documents the principal may read.
//
// The join order is pinned for the same reason it is in Verifications, and the
// measurement quoted there applies here unchanged: the visibility clause carries
// correlated subqueries, this table has no statistics saying it is nearly empty,
// and letting the planner drive the join from document turns a handful of
// primary key probes into a walk of the corpus.
func (s *Store) Corrections(ctx context.Context, p *acl.Principal, ids []string) (map[string]store.Correction, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, genba.ErrNoPrincipal
	}
	if len(ids) == 0 {
		return nil, nil
	}

	v := visible(p)
	args := append([]any{jsonList(ids)}, v.args...)
	rows, err := s.query(ctx, `
		SELECT o.doc_id, o.owner, o.was, o.corrected_by, o.corrected_at
		FROM json_each(?) j
		CROSS JOIN document_own o ON o.doc_id = j.value
		CROSS JOIN document d ON d.id = o.doc_id
		WHERE `+v.where(), args...)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: corrections: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out map[string]store.Correction
	for rows.Next() {
		var (
			c              store.Correction
			owner, was, by string
			corrected      int64
			people         [3]doc.Person
			bad            error
		)
		if err := rows.Scan(&c.Doc, &owner, &was, &by, &corrected); err != nil {
			return nil, fmt.Errorf("sqlitestore: corrections: %w", err)
		}
		s.counters.rows.Add(1)
		for i, stored := range [3]string{owner, was, by} {
			who, err := unmarshalPerson(stored)
			people[i], bad = who, errors.Join(bad, err)
		}
		if bad != nil {
			return nil, fmt.Errorf("sqlitestore: corrections: %w", bad)
		}
		c.Owner, c.Was, c.By = people[0], people[1], people[2]
		c.At = unixNano(corrected)
		if out == nil {
			out = make(map[string]store.Correction, len(ids))
		}
		out[c.Doc] = c
	}
	return out, rows.Err()
}

// writeOwner puts a person into the stored document and into the column the
// index reads.
//
// Both, always. The document is what a reader is shown and the column is what an
// owner: filter and a facet count are computed from, and a change that touches
// one of them leaves the interface and the search disagreeing about who owns the
// same document.
//
// The document is edited in place with json_set rather than decoded, changed and
// encoded again, so a body of any size is the database's business rather than
// something this driver reads into memory to change one field.
func writeOwner(ctx context.Context, tx *sql.Tx, id, person string, who doc.Person) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE document_data SET data = json_set(data, '$.Owner', json(?)) WHERE doc_id = ?`,
		person, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE document SET owner_keys = ? WHERE id = ?`,
		jsonKeys(store.PersonKeys(who)), id); err != nil {
		return err
	}
	return nil
}

// keepOwners applies the standing corrections to a batch on its way in.
//
// This is the contract in [store.Ownership] that says a crawl does not undo a
// correction, and it is one statement per batch rather than one per document.
// The source's answer is written back as it goes past, so a connector that fixes
// its own metadata is what clearing the correction restores.
func keepOwners(ctx context.Context, tx *sql.Tx, docs []doc.Document) (map[string]doc.Person, error) {
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT o.doc_id, o.owner FROM json_each(?) j
		CROSS JOIN document_own o ON o.doc_id = j.value`, jsonList(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out map[string]doc.Person
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			return nil, err
		}
		who, err := unmarshalPerson(owner)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = make(map[string]doc.Person, len(docs))
		}
		out[id] = who
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	for _, d := range docs {
		if _, ok := out[d.ID]; !ok {
			continue
		}
		was, err := marshalPerson(d.Owner)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE document_own SET was = ? WHERE doc_id = ?`, was, d.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func marshalPerson(p doc.Person) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode a person: %w", err)
	}
	return string(b), nil
}

func unmarshalPerson(s string) (doc.Person, error) {
	var p doc.Person
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return doc.Person{}, fmt.Errorf("decode a person: %w", err)
	}
	return p, nil
}
