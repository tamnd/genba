package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

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
// Four statements: the candidates, their term frequencies, the total, and the
// facet counts. The last two run only for a caller that asked for the counts,
// because they are the two whose cost follows the match set rather than the
// page, and the facets are bounded on top of that. See [Store.counts].
func (s *Store) Rank(ctx context.Context, p *acl.Principal, r store.Request, sel store.Selection) (store.Ranked, error) {
	if err := s.ready(ctx); err != nil {
		return store.Ranked{}, err
	}
	if p == nil {
		return store.Ranked{}, genba.ErrNoPrincipal
	}
	if sel.Limit <= 0 && !sel.Counts {
		return store.Ranked{}, errors.New("pgstore: rank: asked for neither candidates nor counts")
	}

	// The predicate for a request, built the same way whatever is being asked
	// about. The counting asks about a request with one facet constraint lifted,
	// and building that by hand somewhere else is how two predicates that are
	// meant to be the same drift apart.
	pred := func(r store.Request) *clause {
		c := visible(p)
		filters(r, c)
		if q, ok := tsquery(r.Terms); ok {
			c.add(`d.terms @@ ?::tsquery`, q)
		}
		return c
	}
	c := pred(r)

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
		// A selection that asked for no pool skips both of the statements that
		// read the match set row by row. What is left is the counting, which is
		// what it asked for.
		var cands []store.Candidate
		if sel.Limit > 0 {
			var err error
			if cands, err = s.candidates(ctx, c, order, orderArgs, sel.Limit); err != nil {
				return err
			}
		}
		if len(cands) > 0 && len(r.Terms) > 0 {
			if err := s.frequencies(ctx, cands, r.Terms); err != nil {
				return err
			}
		}
		if !sel.Counts {
			// The counting is the part here whose cost follows the match set, so
			// a caller that shows neither a total nor a sidebar does not run it.
			out = store.Ranked{Candidates: cands}
			return nil
		}
		total, err := s.total(ctx, c)
		if err != nil {
			return err
		}
		counted, stopped, facets, err := s.counts(ctx, r, pred, sel.Facets)
		if err != nil {
			return err
		}
		out = store.Ranked{
			Candidates: cands,
			Total:      total,
			Facets:     facets,
			// Counted rather than the bound, because what makes the counts a
			// lower bound is the statement having stopped early, and the
			// statement is the only thing that knows whether it did. A lifted
			// count has no total to be compared against, so stopped carries the
			// same claim for those.
			Approximate: counted < total || stopped,
			Truncated:   sel.Limit > 0 && total > len(cands),
		}
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
		SELECT d.id, d.modified_at, d.title_tokens, d.body_tokens
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
			modified *int64
		)
		if err := rows.Scan(&cand.ID, &modified, &cand.TitleTokens, &cand.BodyTokens); err != nil {
			return nil, err
		}
		s.counters.rows.Add(1)
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

// total is how many documents matched, in one statement over the predicate.
//
// It is a plain count and it reads no columns, so Postgres answers it from the
// index scan the predicate needs anyway. The same number used to come out of the
// common table expression below, which meant a query for a common word wrote
// every matching row into a work table to count them.
func (s *Store) total(ctx context.Context, c *clause) (int, error) {
	var n int
	if err := s.queryRow(ctx, `SELECT count(*) FROM document d WHERE `+c.where(), c.args...).Scan(&n); err != nil {
		return 0, err
	}
	s.counters.rows.Add(1)
	return n, nil
}

// counts is every facet, in one statement, and returns how many documents it
// counted the match set over and whether any of the counting stopped at the
// bound.
//
// One statement rather than one per field, because the predicate is the
// expensive part and a materialised common table expression is evaluated once.
// Four statements would apply the permission rule to the match set four times
// over to answer four questions about the same rows. MATERIALIZED is spelled out
// because Postgres inlines a common table expression referenced more than once
// only when it is cheap to, and it has no way of knowing that this one is not.
//
// A field with a constraint on it gets a second expression with that constraint
// lifted, because a count under its own filter is the number that made the
// sidebar useless: tick one type and every other type reports zero. See
// [store.Request.Without]. That is one extra expression per field somebody has
// actually ticked, which is nearly always none or one, and never more than four.
//
// The bound is the LIMIT inside each expression, and it is what keeps this from
// being proportional to the match set: the four columns are read for the first
// bound documents and the scan stops there. They are the first in the order the
// planner produces rather than the best matching, because ordering by the score
// would mean reading those four columns of every matching document to find out
// which ones to keep, which is the cost being removed. A sample is what the
// sidebar gets, and the total above is exact.
//
// The values somebody has ticked are counted twice, once in the lifted
// expression and once over the match set itself, and the second is the one that
// is kept. Both are counts of the same documents, since a lifted field's bucket
// for a ticked value is the match set, and the one taken over the smaller set is
// the one the bound is less likely to have cut short.
func (s *Store) counts(ctx context.Context, r store.Request, pred func(store.Request) *clause, bound int) (counted int, stopped bool, facets map[string][]store.Facet, err error) {
	const columns = `SELECT d.source AS source, d.kind AS kind, d.container AS container, d.author_name AS author`

	var (
		ctes  []string
		parts []string
		args  []any
		caps  []any
	)
	// pool declares one expression and the row that says how many documents it
	// was allowed to see. No bound means no LIMIT rather than a limit large
	// enough to stand in for one, because the exact answer is what a selection
	// without a bound asked for and a very large number is not the same claim.
	pool := func(name string, r store.Request) {
		c := pred(r)
		limit := ``
		if bound > 0 {
			limit = ` LIMIT ?`
		}
		ctes = append(ctes, name+` AS MATERIALIZED (`+columns+` FROM document d WHERE `+c.where()+limit+`)`)
		args = append(args, c.args...)
		if bound > 0 {
			args = append(args, bound)
		}
		parts = append(parts, `SELECT 'counted:`+name+`'::text AS field, ''::text AS value, count(*) AS n FROM `+name)
	}
	// The subquery is named because Postgres requires it, and the name is the
	// position so that two counts of the same field are still two names.
	group := func(label, name, column string) {
		parts = append(parts, `SELECT * FROM (SELECT '`+label+`'::text, `+column+`, count(*) FROM `+name+
			` WHERE `+column+` <> '' GROUP BY `+column+` ORDER BY 3 DESC, 2 LIMIT ?) f`+strconv.Itoa(len(caps)))
		caps = append(caps, store.MaxFacetValues)
	}

	// The match set first, so that its counted row is the one carrying the
	// column names the rest of the union inherits.
	pool("m", r)
	for _, field := range store.FacetFields {
		over := "m"
		if r.Constrains(field) {
			over = "m_" + field
			pool(over, r.Without(field))
			// The ticked values, counted over the match set, which override the
			// lifted counts of the same values below.
			group(field+"!", "m", field)
		}
		group(field, over, field)
	}

	rows, err := s.query(ctx, `WITH `+strings.Join(ctes, `, `)+"\n"+
		strings.Join(parts, "\nUNION ALL "), append(args, caps...)...)
	if err != nil {
		return 0, false, nil, err
	}
	defer rows.Close()

	var (
		lifted  = map[string][]store.Facet{}
		ticked  = map[string]map[string]int{}
		scanned = map[string]int{}
	)
	for rows.Next() {
		var (
			field, value string
			n            int
		)
		if err := rows.Scan(&field, &value, &n); err != nil {
			return 0, false, nil, err
		}
		s.counters.rows.Add(1)
		switch {
		case strings.HasPrefix(field, "counted:"):
			scanned[strings.TrimPrefix(field, "counted:")] = n
			// The documents the aggregate read, which no other counter can see:
			// see [store.Counters.Faceted] for why it is counted separately from
			// the rows the statement returned.
			s.counters.faceted.Add(int64(n))
		case strings.HasSuffix(field, "!"):
			field = strings.TrimSuffix(field, "!")
			if ticked[field] == nil {
				ticked[field] = map[string]int{}
			}
			ticked[field][value] = n
		default:
			lifted[field] = append(lifted[field], store.Facet{Value: value, Count: n})
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, nil, err
	}

	facets = make(map[string][]store.Facet, len(lifted))
	for field, values := range lifted {
		for i, v := range values {
			if n, ok := ticked[field][v.Value]; ok {
				values[i].Count = n
			}
		}
		facets[field] = values
	}
	// A lifted expression counts over a larger set than the match set and there
	// is no total to compare it against, so reaching the bound is what stands in
	// for having stopped early. It says approximate on a count that landed
	// exactly on the bound and was in fact exact, which is the harmless way to be
	// wrong about it.
	for name, n := range scanned {
		if name != "m" && bound > 0 && n >= bound {
			stopped = true
		}
	}
	return scanned["m"], stopped, facets, nil
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
