package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

var _ store.Ownership = (*Store)(nil)

// SetOwner records a correction and writes the new owner into the document.
//
// Unlike Verify it does take the write lock, because it writes the document row
// rather than a row beside it. A crawl landing between the read of what the
// document says now and the write of who owns it would leave the correction
// recorded and the document still owned by the account that ran the import,
// which is the one outcome this feature exists to prevent.
func (s *Store) SetOwner(ctx context.Context, p *acl.Principal, c store.Correction) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	if err := c.Check(); err != nil {
		return fmt.Errorf("pgstore: set owner: %w", err)
	}

	v := visible(p)
	args := append([]any{c.Doc}, v.args...)
	err := s.retry(ctx, func(ctx context.Context) error {
		return s.transact(ctx, func(tx pgx.Tx) error {
			// The document is read through the visibility predicate, exactly as
			// recording an open is, so a correction to something this principal
			// may not read reads no rows and writes nothing.
			//
			// The source's answer comes from the standing correction when there
			// is one and from the document otherwise. Reading it from the
			// document in both cases would remember the previous person's guess
			// as what the connector said, and clearing the correction would then
			// hand the document to somebody who was themselves a correction.
			var data, was string
			s.counters.statements.Add(1)
			row := tx.QueryRow(ctx, rebind(`
				SELECT dd.data, coalesce(o.was, '')
				FROM document d
				JOIN document_data dd ON dd.doc_id = d.id
				LEFT JOIN document_own o ON o.doc_id = d.id
				WHERE d.id = ? AND `+v.where()), args...)
			if err := row.Scan(&data, &was); errors.Is(err, pgx.ErrNoRows) {
				return nil
			} else if err != nil {
				return err
			}

			d, err := decode(data)
			if err != nil {
				return err
			}
			if was == "" {
				if was, err = marshalPerson(d.Owner); err != nil {
					return err
				}
			}
			owner, err := marshalPerson(c.Owner)
			if err != nil {
				return err
			}
			by, err := marshalPerson(c.By)
			if err != nil {
				return err
			}

			b := &pgx.Batch{}
			b.Queue(`
				INSERT INTO document_own (doc_id, owner, was, corrected_by, corrected_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (doc_id) DO UPDATE SET
					owner        = excluded.owner,
					was          = excluded.was,
					corrected_by = excluded.corrected_by,
					corrected_at = excluded.corrected_at`,
				c.Doc, owner, was, by, c.At.UnixNano())
			if err := writeOwner(b, d, c.Owner); err != nil {
				return err
			}
			return tx.SendBatch(ctx, b).Close()
		})
	})
	if err != nil {
		return fmt.Errorf("pgstore: set owner: %w", err)
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

	v := visible(p)
	args := append([]any{id}, v.args...)
	err := s.retry(ctx, func(ctx context.Context) error {
		return s.transact(ctx, func(tx pgx.Tx) error {
			var data, was string
			s.counters.statements.Add(1)
			row := tx.QueryRow(ctx, rebind(`
				SELECT dd.data, o.was
				FROM document_own o
				JOIN document d ON d.id = o.doc_id
				JOIN document_data dd ON dd.doc_id = o.doc_id
				WHERE o.doc_id = ? AND `+v.where()), args...)
			if err := row.Scan(&data, &was); errors.Is(err, pgx.ErrNoRows) {
				// Nothing to clear, or nothing this principal may see. Both are
				// quiet, which is what makes undoing a mistake safe to do twice.
				return nil
			} else if err != nil {
				return err
			}

			d, err := decode(data)
			if err != nil {
				return err
			}
			source, err := unmarshalPerson(was)
			if err != nil {
				return err
			}

			b := &pgx.Batch{}
			b.Queue(`DELETE FROM document_own WHERE doc_id = $1`, id)
			if err := writeOwner(b, d, source); err != nil {
				return err
			}
			return tx.SendBatch(ctx, b).Close()
		})
	})
	if err != nil {
		return fmt.Errorf("pgstore: clear owner: %w", err)
	}
	return nil
}

// Corrections returns the corrections on the documents the principal may read.
//
// One statement for the whole page, with the ids bound as a text array, exactly
// as Verifications does it.
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
	args := append([]any{ids}, v.args...)

	var out map[string]store.Correction
	err := s.retry(ctx, func(ctx context.Context) error {
		out = nil
		rows, err := s.query(ctx, `
			SELECT o.doc_id, o.owner, o.was, o.corrected_by, o.corrected_at
			FROM document_own o
			JOIN document d ON d.id = o.doc_id
			WHERE o.doc_id = ANY(?) AND `+v.where(), args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var (
				c              store.Correction
				owner, was, by string
				corrected      int64
			)
			if err := rows.Scan(&c.Doc, &owner, &was, &by, &corrected); err != nil {
				return err
			}
			s.counters.rows.Add(1)
			people := [3]doc.Person{}
			var bad error
			for i, stored := range [3]string{owner, was, by} {
				who, err := unmarshalPerson(stored)
				people[i], bad = who, errors.Join(bad, err)
			}
			if bad != nil {
				return bad
			}
			c.Owner, c.Was, c.By = people[0], people[1], people[2]
			c.At = unixNano(corrected)
			if out == nil {
				out = make(map[string]store.Correction, len(ids))
			}
			out[c.Doc] = c
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: corrections: %w", err)
	}
	return out, nil
}

// writeOwner queues the two writes that put a person on a document: the stored
// document a reader is shown, and the column an owner: filter and a facet count
// are computed from.
//
// Both, always. A change that touches one of them leaves the interface and the
// search disagreeing about who owns the same document.
//
// The document is decoded, changed and encoded again rather than edited in
// place with jsonb_set, because the column is text on purpose and converting it
// to jsonb to change one field would rewrite every document that went through a
// correction into Postgres's normal form. The SQLite driver can edit its copy
// where it lies, and this one reads a few kilobytes on a write that happens
// about once a year per document.
func writeOwner(b *pgx.Batch, d doc.Document, who doc.Person) error {
	d.Owner = who
	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode %s: %w", d.ID, err)
	}
	b.Queue(`UPDATE document_data SET data = $1 WHERE doc_id = $2`, string(data), d.ID)
	b.Queue(`UPDATE document SET owner_keys = $1 WHERE id = $2`, nonEmpty(store.PersonKeys(who)), d.ID)
	return nil
}

// keepOwners applies the standing corrections to a batch on its way in.
//
// This is the contract in [store.Ownership] that says a crawl does not undo a
// correction, and it is one statement for the batch rather than one per
// document. The source's answer is written back as it goes past, so a connector
// that fixes its own metadata is what clearing the correction restores.
func keepOwners(ctx context.Context, tx pgx.Tx, docs []doc.Document) (map[string]doc.Person, error) {
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	rows, err := tx.Query(ctx, `SELECT doc_id, owner FROM document_own WHERE doc_id = ANY($1::text[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the standing corrections: %w", err)
	}
	defer rows.Close()

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
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}

	b := &pgx.Batch{}
	for _, d := range docs {
		if _, ok := out[d.ID]; !ok {
			continue
		}
		was, err := marshalPerson(d.Owner)
		if err != nil {
			return nil, err
		}
		b.Queue(`UPDATE document_own SET was = $1 WHERE doc_id = $2`, was, d.ID)
	}
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return nil, err
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
