package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

var _ store.Maintenance = (*Store)(nil)
var _ store.Quarantine = (*Store)(nil)

// Quarantined returns documents this driver is holding back, newest first.
//
// The title and the reason are pulled out of the stored JSON in SQL rather than
// by decoding the row, for the same reason [Store.Inventory] reads two columns:
// the body is in that JSON, a held document is as large as any other, and a
// hundred of them is megabytes read and parsed to reach two short strings. The
// cast to json is per row and it is a hundred rows, which is the whole of what
// this pays and is a great deal less than shipping the bodies.
//
// Newest first because the question somebody has when they open this screen is
// whether the connector they just fixed has stopped producing them, and that is
// answered by the top of the list rather than by a hundred entries from
// whenever the corpus was first crawled. A document whose source never gave a
// date sorts last, because the alternative in Postgres is for it to sort first
// and push the answer off the screen.
func (s *Store) Quarantined(ctx context.Context, tenant string, limit int) ([]store.Held, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	var out []store.Held
	err := s.retry(ctx, func(ctx context.Context) error {
		rows, err := s.query(ctx, `
			SELECT
				d.id,
				d.source,
				d.modified_at,
				dd.data::json ->> 'Title',
				dd.data::json -> 'Permissions' ->> 'Reason'
			FROM document d
			JOIN document_data dd ON dd.doc_id = d.id
			WHERE d.tenant = $1 AND NOT d.queryable
			ORDER BY d.modified_at DESC NULLS LAST
			LIMIT $2`, tenant, limit)
		if err != nil {
			return err
		}
		defer rows.Close()

		// Cleared on every attempt. A retry that appended to what the failed
		// attempt had already read would return the first page twice.
		out = make([]store.Held, 0, limit)
		for rows.Next() {
			var (
				h        store.Held
				modified *int64
				title    *string
				reason   *string
			)
			if err := rows.Scan(&h.ID, &h.Source, &modified, &title, &reason); err != nil {
				return err
			}
			if title != nil {
				h.Title = *title
			}
			if reason != nil {
				h.Reason = *reason
			}
			if modified != nil {
				h.At = time.Unix(0, *modified).UTC()
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: quarantined: %w", err)
	}
	return out, nil
}

// Inventory calls fn for every document held for one tenant and source.
//
// It reads two columns of one table and nothing else. A reconciliation over a
// corpus of a few million documents is a few million ids, and decoding the
// stored JSON for each of them to reach a field the comparison does not use
// would turn an index scan into a read of the corpus.
func (s *Store) Inventory(ctx context.Context, tenant, source string, fn func(store.Item) bool) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, source_update FROM document WHERE tenant = $1 AND source = $2 ORDER BY id`, tenant, source)
	if err != nil {
		return fmt.Errorf("pgstore: inventory: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var item store.Item
		if err := rows.Scan(&item.ID, &item.Version); err != nil {
			return fmt.Errorf("pgstore: inventory: %w", err)
		}
		if !fn(item) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pgstore: inventory: %w", err)
	}
	return nil
}

// SetPermissions replaces the access control lists of stored documents.
//
// What it deliberately does not do is re-analyse the text. A permission change
// touches the columns the visibility predicate reads, the reference rows it
// joins against, and the stored copy of the document. Everything derived from
// the words is left alone, because the words did not change, which is what
// makes a company wide access control change cost a write per document rather
// than a recrawl.
//
// The exception is a document that crosses the quarantine line in either
// direction, since a quarantined document is not in the full text index or the
// corpus statistics at all. That is the one path here that pays for the
// analyzer, and it is the path that matters most: a revocation has to take
// effect as fast as a grant.
func (s *Store) SetPermissions(ctx context.Context, tenant string, perms map[string]acl.Permissions) (int, error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	if len(perms) == 0 {
		return 0, nil
	}

	var changed int
	written := make(map[string][]string, 1)
	err := s.retry(ctx, func(ctx context.Context) error {
		changed = 0
		clear(written)
		return s.transact(ctx, func(tx pgx.Tx) error {
			ids, err := setPermissions(ctx, tx, tenant, perms)
			if err != nil {
				return err
			}
			changed = len(ids)
			if len(ids) > 0 {
				written[tenant] = ids
			}
			return nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("pgstore: set permissions: %w", err)
	}
	s.Changes(written, false)
	return changed, nil
}

// setPermissions does the work inside the transaction and returns the ids that
// were actually there to change.
func setPermissions(ctx context.Context, tx pgx.Tx, tenant string, perms map[string]acl.Permissions) ([]string, error) {
	ids := make([]string, 0, len(perms))
	for id := range perms {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	priors, err := readPriors(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	stored, err := readStored(ctx, tx, tenant, ids)
	if err != nil {
		return nil, err
	}

	// Three groups, because the statistics only care about the line between
	// quarantined and queryable. A document that changes from one access
	// control list to another has not moved, and touching its postings would be
	// deleting rows to write the same rows back.
	var (
		found   = make([]string, 0, len(ids))
		flipped = make([]string, 0, len(ids))
		back    = make([]doc.Document, 0, len(ids))
	)
	for _, id := range ids {
		d, ok := stored[id]
		if !ok {
			continue
		}
		p := priors[id]
		d.Permissions = perms[id]
		stored[id] = d
		found = append(found, id)
		if p.queryable != d.Queryable() {
			flipped = append(flipped, id)
			if d.Queryable() {
				back = append(back, d)
			}
		}
	}
	if len(found) == 0 {
		return nil, nil
	}

	b := &pgx.Batch{}
	retire(b, priors, flipped)
	b.Queue(`DELETE FROM document_ref WHERE doc_id = ANY($1::text[])`, found)

	rows := make([]posting, 0, len(back)*64)
	var titleTokens, bodyTokens int64
	for _, id := range found {
		d := stored[id]
		data, err := json.Marshal(d)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", id, err)
		}
		var ownerKey string
		if d.Permissions.Owner.Value != "" {
			ownerKey = d.Permissions.Owner.UserKey()
		}
		b.Queue(`
			UPDATE document SET mode = $2, owner_key = $3, queryable = $4
			WHERE id = $1`, id, int16(d.Permissions.Mode), ownerKey, d.Queryable())
		b.Queue(`UPDATE document_data SET data = $2 WHERE doc_id = $1`, id, string(data))

		refs := refsOf(d.Permissions)
		if len(refs) > 0 {
			effects := make([]int16, len(refs))
			scopes := make([]int16, len(refs))
			keys := make([]string, len(refs))
			for i, r := range refs {
				effects[i], scopes[i], keys[i] = r.effect, r.scope, r.key
			}
			b.Queue(`
				INSERT INTO document_ref (doc_id, effect, scope, key)
				SELECT $1, e, s, k FROM unnest($2::smallint[], $3::smallint[], $4::text[]) AS t(e, s, k)`,
				id, effects, scopes, keys)
		}
	}

	// The full text column follows the quarantine line in both directions:
	// emptied on the way out, rebuilt on the way in.
	for _, id := range flipped {
		d := stored[id]
		terms := ""
		if d.Queryable() {
			a := d.Analyze()
			terms = tsvector(a)
			rows = appendPostings(rows, id, a)
			titleTokens += int64(a.TitleTokens)
			bodyTokens += int64(a.BodyTokens)
		}
		b.Queue(`UPDATE document SET terms = $2::tsvector WHERE id = $1`, id, terms)
	}
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return nil, err
	}

	if len(rows) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"posting"},
			[]string{"doc_id", "term", "title_tf", "body_tf"},
			pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
				r := rows[i]
				return []any{r.docID, r.term, r.titleTF, r.bodyTF}, nil
			})); err != nil {
			return nil, fmt.Errorf("copy postings: %w", err)
		}
	}

	if len(back) > 0 {
		ids := make([]string, len(back))
		for i, d := range back {
			ids[i] = d.ID
		}
		b = &pgx.Batch{}
		b.Queue(`
			INSERT INTO term_stat (tenant, term, documents)
			SELECT $1, term, count(*) FROM posting WHERE doc_id = ANY($2::text[]) GROUP BY term
			ON CONFLICT (tenant, term) DO UPDATE SET documents = term_stat.documents + excluded.documents`,
			tenant, ids)
		b.Queue(`
			INSERT INTO corpus (tenant, documents, title_tokens, body_tokens) VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant) DO UPDATE SET
				documents    = corpus.documents + excluded.documents,
				title_tokens = corpus.title_tokens + excluded.title_tokens,
				body_tokens  = corpus.body_tokens + excluded.body_tokens`,
			tenant, int64(len(back)), titleTokens, bodyTokens)
		if err := tx.SendBatch(ctx, b).Close(); err != nil {
			return nil, err
		}
	}
	return found, nil
}

// readStored reads the documents behind a set of ids, for one tenant.
//
// The tenant is in the predicate rather than checked afterwards, so a caller
// that names an id belonging to somebody else changes nothing rather than
// changing their access control list.
func readStored(ctx context.Context, tx pgx.Tx, tenant string, ids []string) (map[string]doc.Document, error) {
	rows, err := tx.Query(ctx, `
		SELECT x.data FROM document d JOIN document_data x ON x.doc_id = d.id
		WHERE d.tenant = $1 AND d.id = ANY($2::text[])`, tenant, ids)
	if err != nil {
		return nil, fmt.Errorf("read the stored documents: %w", err)
	}
	defer rows.Close()

	out := make(map[string]doc.Document, len(ids))
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("read the stored documents: %w", err)
		}
		var d doc.Document
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return nil, fmt.Errorf("decode a stored document: %w", err)
		}
		out[d.ID] = d
	}
	return out, rows.Err()
}
