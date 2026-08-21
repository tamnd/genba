package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
// proportional to the corpus outside the index walk SQLite does for itself.
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
		return store.Ranked{}, errors.New("sqlitestore: rank: no candidate limit")
	}

	c := visible(p)
	filters(r, c)

	// The full text table leads the join for the same reason it does in
	// Retrieve: it is the most selective thing in the statement on any real
	// query, so the permission predicate is evaluated for the documents that
	// matched rather than for the corpus.
	//
	// CROSS JOIN is how that is said out loud, and it has to be said. Written as
	// an ordinary join the planner is free to reorder, and for the counts it
	// does: it leads with the tenant index over document and runs the MATCH once
	// per row, which measured at a second on twenty thousand documents against
	// a hundred and forty milliseconds for the same statement with the order
	// fixed. The candidate cut got away with it only because its ORDER BY
	// bm25(document_fts) pinned the order by accident. Two statements share this
	// string, so it is stated here once and neither of them relies on luck.
	from := `document d`
	order := `d.id`
	if sel.Recent {
		// A document whose date the source never gave us is not the most recent
		// thing in the corpus, and it does not have to be said: SQLite orders
		// NULL below every value, so a descending scan puts them last for
		// itself. Saying it anyway, as an ORDER BY d.modified_at IS NULL term in
		// front of this one, cost a full sort of the match set. No index can
		// satisfy an ordering that leads with an expression, so the planner
		// walked every visible document into a temporary b-tree to answer a
		// query for twenty rows, which is the whole of why an empty browse was
		// slow. See document_recent in schema.go for the index this walks.
		order = `d.modified_at DESC, d.id`
	}
	if q, ok := match(r.Terms); ok {
		from = `document_fts CROSS JOIN document d ON d.rowid = document_fts.rowid`
		c.add(`document_fts MATCH ?`, q)
		if !sel.Recent {
			// bm25 returns a smaller number for a better match, so ascending is
			// best first. It is only the cut, not the ranking: the score that
			// orders the results is computed in Go over what comes back, which
			// is what keeps one ranking function for every driver.
			order = `bm25(document_fts)`
		}
	}

	cands, rowids, err := s.candidates(ctx, from, c, order, sel.Limit)
	if err != nil {
		return store.Ranked{}, err
	}
	s.counters.candidates.Add(int64(len(cands)))

	out := store.Ranked{Candidates: cands}
	if len(cands) > 0 && len(r.Terms) > 0 {
		if err := s.frequencies(ctx, cands, rowids, r.Terms); err != nil {
			return store.Ranked{}, err
		}
	}
	if !sel.Counts {
		// The counts are the one statement here whose cost follows the match set,
		// so a caller that shows neither a total nor a sidebar does not run it.
		return out, nil
	}
	if out.Total, out.Facets, err = s.counts(ctx, from, c); err != nil {
		return store.Ranked{}, err
	}
	out.Truncated = out.Total > len(cands)
	return out, nil
}

// candidates runs phase one: the cut, in one statement, with the permission
// rule inside it.
func (s *Store) candidates(ctx context.Context, from string, c *clause, order string, limit int) ([]store.Candidate, []int64, error) {
	args := append(append([]any{}, c.args...), limit)
	rows, err := s.query(ctx, `
		SELECT d.id, d.source, d.kind, d.container, d.author_name, d.modified_at,
		       d.title_tokens, d.body_tokens, d.rowid
		FROM `+from+`
		WHERE `+c.where()+`
		ORDER BY `+order+`
		LIMIT ?`, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("sqlitestore: rank: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		out    []store.Candidate
		rowids []int64
	)
	for rows.Next() {
		var (
			cand     store.Candidate
			kind     string
			modified sql.NullInt64
			rowid    int64
		)
		if err := rows.Scan(&cand.ID, &cand.Source, &kind, &cand.Container, &cand.Author,
			&modified, &cand.TitleTokens, &cand.BodyTokens, &rowid); err != nil {
			return nil, nil, fmt.Errorf("sqlitestore: rank: %w", err)
		}
		s.counters.rows.Add(1)
		cand.Kind = doc.Kind(kind)
		if modified.Valid {
			cand.ModifiedAt = unixNano(modified.Int64)
		}
		out = append(out, cand)
		rowids = append(rowids, rowid)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("sqlitestore: rank: %w", err)
	}
	return out, rowids, nil
}

// frequencies fills in the per term counts for the candidates, in one
// statement.
//
// It reads the query terms for the chosen documents and nothing else. The
// posting table is keyed by document first for exactly this: a few hundred
// candidates is a few hundred primary key ranges, and the terms nobody asked
// about are never touched.
func (s *Store) frequencies(ctx context.Context, cands []store.Candidate, rowids []int64, terms []string) error {
	at := make(map[int64]int, len(rowids))
	for i, id := range rowids {
		at[id] = i
	}

	rows, err := s.query(ctx, `
		SELECT doc_rowid, term, title_tf, body_tf FROM posting
		WHERE doc_rowid IN (SELECT value FROM json_each(?))
		  AND term IN (SELECT value FROM json_each(?))`,
		jsonInts(rowids), jsonList(terms))
	if err != nil {
		return fmt.Errorf("sqlitestore: rank: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			rowid       int64
			term        string
			title, body int
		)
		if err := rows.Scan(&rowid, &term, &title, &body); err != nil {
			return fmt.Errorf("sqlitestore: rank: %w", err)
		}
		s.counters.rows.Add(1)
		i, ok := at[rowid]
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
// to answer five questions about the same rows.
func (s *Store) counts(ctx context.Context, from string, c *clause) (total int, facets map[string][]store.Facet, err error) {
	const fields = `
		SELECT 'total' AS field, '' AS value, count(*) AS n FROM m
		UNION ALL SELECT * FROM (SELECT 'source', source, count(*) FROM m WHERE source <> '' GROUP BY source ORDER BY 3 DESC, 2 LIMIT ?)
		UNION ALL SELECT * FROM (SELECT 'kind', kind, count(*) FROM m WHERE kind <> '' GROUP BY kind ORDER BY 3 DESC, 2 LIMIT ?)
		UNION ALL SELECT * FROM (SELECT 'container', container, count(*) FROM m WHERE container <> '' GROUP BY container ORDER BY 3 DESC, 2 LIMIT ?)
		UNION ALL SELECT * FROM (SELECT 'author', author, count(*) FROM m WHERE author <> '' GROUP BY author ORDER BY 3 DESC, 2 LIMIT ?)`

	args := append([]any{}, c.args...)
	for range 4 {
		args = append(args, store.MaxFacetValues)
	}

	rows, err := s.query(ctx, `
		WITH m AS MATERIALIZED (
			SELECT d.source AS source, d.kind AS kind, d.container AS container, d.author_name AS author
			FROM `+from+` WHERE `+c.where()+`
		)`+fields, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("sqlitestore: rank: %w", err)
	}
	defer func() { _ = rows.Close() }()

	facets = map[string][]store.Facet{}
	for rows.Next() {
		var (
			field, value string
			n            int
		)
		if err := rows.Scan(&field, &value, &n); err != nil {
			return 0, nil, fmt.Errorf("sqlitestore: rank: %w", err)
		}
		s.counters.rows.Add(1)
		if field == "total" {
			total = n
			continue
		}
		facets[field] = append(facets[field], store.Facet{Value: value, Count: n})
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("sqlitestore: rank: %w", err)
	}
	return total, facets, nil
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
	switch err := s.queryRow(ctx,
		`SELECT documents, title_tokens, body_tokens FROM corpus WHERE tenant = ?`, p.Tenant,
	).Scan(&out.Documents, &out.TitleTokens, &out.BodyTokens); {
	case errors.Is(err, sql.ErrNoRows):
		// A tenant nobody has written to yet. Zero is the honest answer and the
		// scorer treats it as an empty corpus rather than as an error.
		return out, nil
	case err != nil:
		return store.Corpus{}, fmt.Errorf("sqlitestore: statistics: %w", err)
	}
	s.counters.rows.Add(1)

	if len(terms) == 0 {
		return out, nil
	}
	rows, err := s.query(ctx, `
		SELECT term, documents FROM term_stat
		WHERE tenant = ? AND term IN (SELECT value FROM json_each(?)) AND documents > 0`,
		p.Tenant, jsonList(terms))
	if err != nil {
		return store.Corpus{}, fmt.Errorf("sqlitestore: statistics: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			term string
			n    int
		)
		if err := rows.Scan(&term, &n); err != nil {
			return store.Corpus{}, fmt.Errorf("sqlitestore: statistics: %w", err)
		}
		s.counters.rows.Add(1)
		out.DocFreq[term] = n
	}
	if err := rows.Err(); err != nil {
		return store.Corpus{}, fmt.Errorf("sqlitestore: statistics: %w", err)
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

	// The id list leads the join, and CROSS JOIN is how that is said out loud.
	//
	// This is twenty primary key lookups and the planner has to be told so.
	// Given the ids in an IN clause it preferred the tenant index and walked
	// every document in the tenant, which measured at eight milliseconds a page
	// on twenty thousand documents. Given them as an ordinary join it did the
	// same thing, because it has no idea how many rows a json_each will produce
	// and guesses high. CROSS JOIN is SQLite's documented way of fixing the join
	// order, and it is the difference between twenty key lookups and a scan.
	c := visible(p)
	args := append([]any{jsonList(ids)}, c.args...)
	rows, err := s.query(ctx, `
		SELECT x.data
		FROM json_each(?) j
		CROSS JOIN document d ON d.id = j.value
		CROSS JOIN document_data x ON x.doc_id = d.id
		WHERE `+c.where(), args...)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: fetch: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]doc.Document, 0, len(ids))
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("sqlitestore: fetch: %w", err)
		}
		s.counters.rows.Add(1)
		d, err := s.decoded(data)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: fetch: %w", err)
	}
	return out, nil
}
