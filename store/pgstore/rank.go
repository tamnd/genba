package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// Rank answers one query out of the database's own indexes, and returns the
// candidates worth scoring rather than everything that matched.
//
// This is the whole difference between a search that costs a second and one
// that costs a few milliseconds. Retrieve hands back the match set, which on a
// common term is most of the corpus, and the caller then throws all but twenty
// of them away after re-analysing every one. Here the cut happens inside the
// statement that applies the permission rule, the numbers the scorer needs are
// read out of columns rather than derived from text, and nothing on the path is
// proportional to the corpus outside the index walk Postgres does for itself.
//
// Three statements: the candidates, their term frequencies, and one that
// carries the total and every facet count over the same predicate. The third
// runs only for a caller that asked for the counts, because it is the only one
// of the three whose cost follows the match set rather than the page.
func (s *Store) Rank(ctx context.Context, p *acl.Principal, r store.Request, sel store.Selection) (store.Ranked, error) {
	if err := s.ready(ctx); err != nil {
		return store.Ranked{}, err
	}
	if p == nil {
		return store.Ranked{}, genba.ErrNoPrincipal
	}
	if sel.Limit <= 0 {
		return store.Ranked{}, errors.New("pgstore: rank: no candidate limit")
	}

	c := visible(p)
	filters(r, c)

	// The cut order. Ties break on the id so that two runs of the same query
	// return the same page, which a client paging through results depends on and
	// which nothing in the plan would give us for free.
	order := `d.id`
	var orderArgs []any
	if sel.Recent {
		// NULLs last, because a document whose date the source never gave us is
		// not the most recent thing in the corpus.
		order = `d.modified_at DESC NULLS LAST, d.id`
	}
	if q, ok := tsquery(r.Terms); ok {
		c.add(`d.terms @@ ?::tsquery`, q)
		if !sel.Recent {
			// ts_rank descending is best first. It is only the cut, not the
			// ranking: the score that orders the results is computed in Go over
			// what comes back, which is what keeps one ranking function for
			// every driver. See [tsvector] for why the positions this reads are
			// there at all, and what the ordering would degenerate into without
			// them.
			order = `ts_rank(d.terms, ?::tsquery) DESC, d.id`
			orderArgs = append(orderArgs, q)
		}
	}

	out := store.Ranked{}
	err := s.retry(ctx, func(ctx context.Context) error {
		cands, err := s.candidates(ctx, c, order, orderArgs, sel.Limit)
		if err != nil {
			return err
		}
		if len(cands) > 0 && len(r.Terms) > 0 {
			if err := s.frequencies(ctx, cands, r.Terms); err != nil {
				return err
			}
		}
		if !sel.Counts {
			// The counts are the one statement here whose cost follows the match
			// set, so a caller that shows neither a total nor a sidebar does not
			// run it.
			out = store.Ranked{Candidates: cands}
			return nil
		}
		total, facets, err := s.counts(ctx, c)
		if err != nil {
			return err
		}
		out = store.Ranked{Candidates: cands, Total: total, Facets: facets, Truncated: total > len(cands)}
		return nil
	})
	if err != nil {
		return store.Ranked{}, fmt.Errorf("pgstore: rank: %w", err)
	}
	s.counters.candidates.Add(int64(len(out.Candidates)))
	return out, nil
}

// candidates runs phase one: the cut, in one statement, with the permission
// rule inside it.
func (s *Store) candidates(ctx context.Context, c *clause, order string, orderArgs []any, limit int) ([]store.Candidate, error) {
	args := append(append(append([]any{}, c.args...), orderArgs...), limit)
	rows, err := s.query(ctx, `
		SELECT d.id, d.source, d.kind, d.container, d.author_name, d.modified_at,
		       d.title_tokens, d.body_tokens
		FROM document d
		WHERE `+c.where()+`
		ORDER BY `+order+`
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Candidate
	for rows.Next() {
		var (
			cand     store.Candidate
			kind     string
			modified *int64
		)
		if err := rows.Scan(&cand.ID, &cand.Source, &kind, &cand.Container, &cand.Author,
			&modified, &cand.TitleTokens, &cand.BodyTokens); err != nil {
			return nil, err
		}
		s.counters.rows.Add(1)
		cand.Kind = doc.Kind(kind)
		if modified != nil {
			cand.ModifiedAt = unixNano(*modified)
		}
		out = append(out, cand)
	}
	return out, rows.Err()
}

// frequencies fills in the per term counts for the candidates, in one
// statement.
//
// It reads the query terms for the chosen documents and nothing else. The
// posting table is keyed by document first for exactly this: a few hundred
// candidates is a few hundred primary key ranges, and the terms nobody asked
// about are never touched.
func (s *Store) frequencies(ctx context.Context, cands []store.Candidate, terms []string) error {
	ids := make([]string, len(cands))
	at := make(map[string]int, len(cands))
	for i, cand := range cands {
		ids[i] = cand.ID
		at[cand.ID] = i
	}

	rows, err := s.query(ctx, `
		SELECT doc_id, term, title_tf, body_tf FROM posting
		WHERE doc_id = ANY(?::text[]) AND term = ANY(?::text[])`,
		ids, nonEmpty(terms))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id          string
			term        string
			title, body int
		)
		if err := rows.Scan(&id, &term, &title, &body); err != nil {
			return err
		}
		s.counters.rows.Add(1)
		i, ok := at[id]
		if !ok {
			continue
		}
		if cands[i].Terms == nil {
			cands[i].Terms = make(map[string]doc.TermCount, len(terms))
		}
		cands[i].Terms[term] = doc.TermCount{Title: title, Body: body}
	}
	return rows.Err()
}

// counts is the total and every facet, over the same predicate, in one
// statement.
//
// One rather than five, because the predicate is the expensive part and the
// common table expression is materialised so it is evaluated once. Five
// statements would apply the permission rule to the match set five times over
// to answer five questions about the same rows. MATERIALIZED is spelled out
// because Postgres inlines a common table expression referenced more than once
// only when it is cheap to, and it has no way of knowing that this one is not.
func (s *Store) counts(ctx context.Context, c *clause) (total int, facets map[string][]store.Facet, err error) {
	const fields = `
		SELECT 'total'::text AS field, ''::text AS value, count(*) AS n FROM m
		UNION ALL SELECT * FROM (SELECT 'source'::text, source, count(*) FROM m WHERE source <> '' GROUP BY source ORDER BY 3 DESC, 2 LIMIT ?) f1
		UNION ALL SELECT * FROM (SELECT 'kind'::text, kind, count(*) FROM m WHERE kind <> '' GROUP BY kind ORDER BY 3 DESC, 2 LIMIT ?) f2
		UNION ALL SELECT * FROM (SELECT 'container'::text, container, count(*) FROM m WHERE container <> '' GROUP BY container ORDER BY 3 DESC, 2 LIMIT ?) f3
		UNION ALL SELECT * FROM (SELECT 'author'::text, author, count(*) FROM m WHERE author <> '' GROUP BY author ORDER BY 3 DESC, 2 LIMIT ?) f4`

	args := append([]any{}, c.args...)
	for range 4 {
		args = append(args, store.MaxFacetValues)
	}

	rows, err := s.query(ctx, `
		WITH m AS MATERIALIZED (
			SELECT d.source AS source, d.kind AS kind, d.container AS container, d.author_name AS author
			FROM document d WHERE `+c.where()+`
		)`+fields, args...)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	facets = map[string][]store.Facet{}
	for rows.Next() {
		var (
			field, value string
			n            int
		)
		if err := rows.Scan(&field, &value, &n); err != nil {
			return 0, nil, err
		}
		s.counters.rows.Add(1)
		if field == "total" {
			total = n
			continue
		}
		facets[field] = append(facets[field], store.Facet{Value: value, Count: n})
	}
	return total, facets, rows.Err()
}

// Statistics reads the corpus numbers the scorer needs, in two key lookups.
//
// They are maintained on every write, which is what turns "how many documents
// carry this term" from a count over the term's whole posting list into a
// primary key hit. See [store.Corpus] for why they are counted over the tenant
// rather than over what the asker may read.
func (s *Store) Statistics(ctx context.Context, p *acl.Principal, terms []string) (store.Corpus, error) {
	if err := s.ready(ctx); err != nil {
		return store.Corpus{}, err
	}
	if p == nil {
		return store.Corpus{}, genba.ErrNoPrincipal
	}

	out := store.Corpus{DocFreq: make(map[string]int, len(terms))}
	err := s.retry(ctx, func(ctx context.Context) error {
		clear(out.DocFreq)
		out.Documents, out.TitleTokens, out.BodyTokens = 0, 0, 0

		switch err := s.queryRow(ctx,
			`SELECT documents, title_tokens, body_tokens FROM corpus WHERE tenant = ?`, p.Tenant,
		).Scan(&out.Documents, &out.TitleTokens, &out.BodyTokens); {
		case errors.Is(err, pgx.ErrNoRows):
			// A tenant nobody has written to yet. Zero is the honest answer and
			// the scorer treats it as an empty corpus rather than as an error.
			return nil
		case err != nil:
			return err
		}
		s.counters.rows.Add(1)

		if len(terms) == 0 {
			return nil
		}
		rows, err := s.query(ctx, `
			SELECT term, documents FROM term_stat
			WHERE tenant = ? AND term = ANY(?::text[]) AND documents > 0`, p.Tenant, nonEmpty(terms))
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var (
				term string
				n    int
			)
			if err := rows.Scan(&term, &n); err != nil {
				return err
			}
			s.counters.rows.Add(1)
			out.DocFreq[term] = n
		}
		return rows.Err()
	})
	if err != nil {
		return store.Corpus{}, fmt.Errorf("pgstore: statistics: %w", err)
	}
	return out, nil
}

// Fetch returns a page of documents in one statement.
//
// An id the principal may not read is absent from the answer rather than an
// error, because the alternative is a whole page failing because one document
// was revoked between the ranking and the fetch.
func (s *Store) Fetch(ctx context.Context, p *acl.Principal, ids []string) ([]doc.Document, error) {
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

	var out []doc.Document
	err := s.retry(ctx, func(ctx context.Context) error {
		out = out[:0]
		// The id list is the driving predicate and the planner works that out on
		// its own here, because = ANY over a primary key is something it has
		// statistics for. That is the one place this driver has an easier time
		// than the SQLite one, which has to be told the join order by hand.
		rows, err := s.query(ctx, `
			SELECT x.data FROM document d
			JOIN document_data x ON x.doc_id = d.id
			WHERE d.id = ANY(?::text[]) AND `+c.where(), args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var data string
			if err := rows.Scan(&data); err != nil {
				return err
			}
			s.counters.rows.Add(1)
			d, err := s.decoded(data)
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: fetch: %w", err)
	}
	return out, nil
}
