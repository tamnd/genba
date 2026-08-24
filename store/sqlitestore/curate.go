package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

var _ store.Curator = (*Store)(nil)

// answerColumns is the row every read below selects, in one place so that the
// three of them cannot select it in three orders.
const answerColumns = `a.id, a.question, a.variants, a.body, a.sources, a.author, a.written_at, a.until`

// Curate writes an answer, replacing any earlier one with the same id.
//
// It takes the write lock and does its work in one transaction, because it is
// three statements that have to land together: the answer, the phrasings it no
// longer claims going away, and the ones it claims now arriving. A reader who
// caught the middle of that would find a question with no answer behind it.
func (s *Store) Curate(ctx context.Context, p *acl.Principal, a store.Answer) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	if err := a.Check(); err != nil {
		return fmt.Errorf("sqlitestore: curate: %w", err)
	}
	author, err := marshalPerson(a.By)
	if err != nil {
		return fmt.Errorf("sqlitestore: curate: %w", err)
	}

	s.write.Lock()
	defer s.write.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: curate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	s.counters.statements.Add(1)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO answer (id, tenant, question, variants, body, sources, author, written_at, until)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant, id) DO UPDATE SET
			question   = excluded.question,
			variants   = excluded.variants,
			body       = excluded.body,
			sources    = excluded.sources,
			author     = excluded.author,
			written_at = excluded.written_at,
			until      = excluded.until`,
		a.ID, p.Tenant, a.Question, jsonKeys(a.Variants), a.Body, jsonKeys(a.Sources),
		author, a.At.UnixNano(), a.Until.UnixNano()); err != nil {
		return fmt.Errorf("sqlitestore: curate: %w", err)
	}

	// Every phrasing this answer used to claim goes before the ones it claims
	// now arrive, or an edit that drops a variant leaves the old variant
	// pointing at an answer that no longer says it.
	s.counters.statements.Add(1)
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM answer_phrasing WHERE tenant = ? AND id = ?`, p.Tenant, a.ID); err != nil {
		return fmt.Errorf("sqlitestore: curate: %w", err)
	}
	for _, key := range a.Keys() {
		// The conflict is a phrasing another answer already claims, and it moves
		// to this one, because the writer who wrote it most recently is the one
		// with the better idea of what the question means today.
		s.counters.statements.Add(1)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO answer_phrasing (tenant, key, id) VALUES (?, ?, ?)
			ON CONFLICT (tenant, key) DO UPDATE SET id = excluded.id`,
			p.Tenant, key, a.ID); err != nil {
			return fmt.Errorf("sqlitestore: curate: %w", err)
		}
	}
	return tx.Commit()
}

// Retract removes an answer, and the phrasings that found it with it.
func (s *Store) Retract(ctx context.Context, p *acl.Principal, id string) error {
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
		return fmt.Errorf("sqlitestore: retract: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range []string{
		`DELETE FROM answer_phrasing WHERE tenant = ? AND id = ?`,
		`DELETE FROM answer WHERE tenant = ? AND id = ?`,
	} {
		s.counters.statements.Add(1)
		if _, err := tx.ExecContext(ctx, stmt, p.Tenant, id); err != nil {
			return fmt.Errorf("sqlitestore: retract: %w", err)
		}
	}
	return tx.Commit()
}

// Curated returns the answer to a question.
//
// It is one primary key probe on the phrasing and one on the answer, which is
// the reason the phrasings are a table. This runs on every search, beside the
// ranking, so it has to cost about nothing whether or not anybody has ever
// written an answer down.
func (s *Store) Curated(ctx context.Context, p *acl.Principal, question string) (store.Answer, error) {
	if err := s.ready(ctx); err != nil {
		return store.Answer{}, err
	}
	if p == nil {
		return store.Answer{}, genba.ErrNoPrincipal
	}
	key := store.AnswerKey(question)
	if key == "" {
		return store.Answer{}, genba.ErrNotFound
	}

	row := s.queryRow(ctx, `
		SELECT `+answerColumns+`
		FROM answer_phrasing k
		CROSS JOIN answer a ON a.tenant = k.tenant AND a.id = k.id
		WHERE k.tenant = ? AND k.key = ?`, p.Tenant, key)
	a, err := scanAnswer(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return store.Answer{}, genba.ErrNotFound
	case err != nil:
		return store.Answer{}, fmt.Errorf("sqlitestore: curated: %w", err)
	}
	s.counters.rows.Add(1)
	return a, nil
}

// Answers lists the answers in the tenant, most recently written first.
func (s *Store) Answers(ctx context.Context, p *acl.Principal, limit int) ([]store.Answer, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, genba.ErrNoPrincipal
	}
	if limit <= 0 {
		return nil, nil
	}

	// The id breaks the tie for the reason the report query breaks its own: two
	// answers written in the same nanosecond are possible on a test clock, and a
	// list that orders them differently on every call is a screen that changes
	// when nothing did.
	rows, err := s.query(ctx, `
		SELECT `+answerColumns+`
		FROM answer a
		WHERE a.tenant = ?
		ORDER BY a.written_at DESC, a.id
		LIMIT ?`, p.Tenant, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: answers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]store.Answer, 0, limit)
	for rows.Next() {
		a, err := scanAnswer(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlitestore: answers: %w", err)
		}
		s.counters.rows.Add(1)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func scanAnswer(row scanner) (store.Answer, error) {
	var (
		a          store.Answer
		variants   string
		sources    string
		author     string
		written    int64
		until      int64
		unmarshals = func(s string, into *[]string) error { return json.Unmarshal([]byte(s), into) }
	)
	if err := row.Scan(&a.ID, &a.Question, &variants, &a.Body, &sources, &author, &written, &until); err != nil {
		return store.Answer{}, err
	}
	if err := unmarshals(variants, &a.Variants); err != nil {
		return store.Answer{}, fmt.Errorf("decode the variants: %w", err)
	}
	if err := unmarshals(sources, &a.Sources); err != nil {
		return store.Answer{}, fmt.Errorf("decode the sources: %w", err)
	}
	who, err := unmarshalPerson(author)
	if err != nil {
		return store.Answer{}, err
	}
	a.By, a.At, a.Until = who, unixNano(written), unixNano(until)
	return a, nil
}
