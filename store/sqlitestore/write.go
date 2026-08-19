package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tamnd/genba/doc"
)

// writer holds the statements a batch of writes reuses.
//
// Indexing one four hundred word document is a few hundred inserts into
// posting, so preparing a statement per insert would be preparing a few hundred
// statements per document. Preparing them once per transaction is the
// difference between the ingestion budget and a tenth of it, and it is the
// reason the write path is a type rather than a function.
type writer struct {
	stmts []*sql.Stmt

	prior      *sql.Stmt
	retireTerm *sql.Stmt
	dropPost   *sql.Stmt
	addPost    *sql.Stmt
	addPosts   *sql.Stmt
	bumpTerm   *sql.Stmt
	corpusAdd  *sql.Stmt
	corpusSub  *sql.Stmt
	columns    *sql.Stmt

	// terms is the scratch space one document's postings are sorted in, reused
	// across a batch so that indexing five hundred documents does not allocate
	// five hundred slices of a few hundred strings.
	terms []string
	args  []any
}

// postingChunk is how many postings go into one insert.
//
// The cost of writing a posting is almost entirely the cost of executing a
// statement rather than the cost of the row, and a four hundred word document
// has a few hundred distinct terms in it. Sending them one at a time is a few
// hundred round trips through database/sql per document, which measured at
// three hundred documents a second against a budget of two thousand. Sending
// them sixty four at a time is the same rows and a sixty fourth of the
// overhead.
//
// It is not larger because SQLite has a limit on bound parameters and because
// the returns flatten out well before it: most of the win is in the first
// factor of ten.
const postingChunk = 64

func newWriter(ctx context.Context, tx *sql.Tx) (*writer, error) {
	w := &writer{}
	prep := func(dst **sql.Stmt, query string) error {
		s, err := tx.PrepareContext(ctx, query)
		if err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
		w.stmts = append(w.stmts, s)
		*dst = s
		return nil
	}

	err := errors.Join(
		prep(&w.prior, `SELECT rowid, tenant, title_tokens, body_tokens, queryable FROM document WHERE id = ?`),

		// The decrement reads the terms out of the postings that are still in
		// place, so the set it walks is exactly the set that was counted when
		// the document was written. Deriving it from the document instead would
		// be re-analysing text to undo an old analysis, and the two would
		// disagree the moment the analyzer changed.
		prep(&w.retireTerm, `
			UPDATE term_stat SET documents = documents - 1
			WHERE tenant = ? AND term IN (SELECT term FROM posting WHERE doc_rowid = ?)`),
		prep(&w.dropPost, `DELETE FROM posting WHERE doc_rowid = ?`),
		prep(&w.addPost, `INSERT INTO posting (doc_rowid, term, title_tf, body_tf) VALUES (?, ?, ?, ?)`),
		prep(&w.addPosts, `INSERT INTO posting (doc_rowid, term, title_tf, body_tf) VALUES `+
			strings.TrimSuffix(strings.Repeat(`(?, ?, ?, ?), `, postingChunk), `, `)),
		prep(&w.bumpTerm, `
			INSERT INTO term_stat (tenant, term, documents)
			SELECT ?, term, 1 FROM posting WHERE doc_rowid = ?
			ON CONFLICT(tenant, term) DO UPDATE SET documents = documents + 1`),
		prep(&w.corpusAdd, `
			INSERT INTO corpus (tenant, documents, title_tokens, body_tokens) VALUES (?, 1, ?, ?)
			ON CONFLICT(tenant) DO UPDATE SET
				documents    = documents + 1,
				title_tokens = title_tokens + excluded.title_tokens,
				body_tokens  = body_tokens + excluded.body_tokens`),
		prep(&w.corpusSub, `
			UPDATE corpus SET
				documents    = documents - 1,
				title_tokens = title_tokens - ?,
				body_tokens  = body_tokens - ?
			WHERE tenant = ?`),
		prep(&w.columns, `UPDATE document SET title_tokens = ?, body_tokens = ?, container = ?, author_name = ? WHERE rowid = ?`),
	)
	if err != nil {
		w.close()
		return nil, err
	}
	return w, nil
}

func (w *writer) close() {
	for _, s := range w.stmts {
		_ = s.Close()
	}
	w.stmts = nil
}

// retire takes whatever the stored version of a document contributed back out
// of the statistics and drops its postings.
//
// It runs before the row is rewritten, because the columns it reads are the
// ones about to be overwritten. It returns whether the document was there at
// all, which is what tells the delete path there is nothing to do.
func (w *writer) retire(ctx context.Context, id string) (rowid int64, found bool, err error) {
	var (
		tenant            string
		titleTok, bodyTok int64
		queryable         int
	)
	switch err := w.prior.QueryRowContext(ctx, id).Scan(&rowid, &tenant, &titleTok, &bodyTok, &queryable); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}

	// A quarantined document was never counted, so taking it out again would
	// count it backwards.
	if queryable == 1 {
		if _, err := w.retireTerm.ExecContext(ctx, tenant, rowid); err != nil {
			return 0, false, err
		}
		if _, err := w.corpusSub.ExecContext(ctx, titleTok, bodyTok, tenant); err != nil {
			return 0, false, err
		}
	}
	if _, err := w.dropPost.ExecContext(ctx, rowid); err != nil {
		return 0, false, err
	}
	return rowid, true, nil
}

// index writes the postings for a document and folds it into the corpus
// statistics.
//
// The analysis is passed in rather than recomputed, because the caller already
// ran it to produce the full text index row and analysing a document twice per
// write is half the ingestion budget spent on the same answer.
func (w *writer) index(ctx context.Context, rowid int64, d doc.Document, a doc.Analysis) error {
	if !d.Queryable() {
		return nil
	}
	if err := w.postings(ctx, rowid, a); err != nil {
		return err
	}
	if _, err := w.bumpTerm.ExecContext(ctx, d.Tenant, rowid); err != nil {
		return err
	}
	_, err := w.corpusAdd.ExecContext(ctx, d.Tenant, a.TitleTokens, a.BodyTokens)
	return err
}

// postings writes one document's term vector, in chunks.
//
// The terms are sorted first. posting is a WITHOUT ROWID table keyed by
// document and then term, so sorted inserts land in one ascending run of a
// b-tree page range instead of scattering across it, and map iteration order
// stops being something the write cost depends on.
func (w *writer) postings(ctx context.Context, rowid int64, a doc.Analysis) error {
	w.terms = w.terms[:0]
	for term := range a.Terms {
		w.terms = append(w.terms, term)
	}
	slices.Sort(w.terms)

	var at int
	for ; at+postingChunk <= len(w.terms); at += postingChunk {
		w.args = w.args[:0]
		for _, term := range w.terms[at : at+postingChunk] {
			c := a.Terms[term]
			w.args = append(w.args, rowid, term, c.Title, c.Body)
		}
		if _, err := w.addPosts.ExecContext(ctx, w.args...); err != nil {
			return err
		}
	}
	for _, term := range w.terms[at:] {
		c := a.Terms[term]
		if _, err := w.addPost.ExecContext(ctx, rowid, term, c.Title, c.Body); err != nil {
			return err
		}
	}
	return nil
}

// statistics rebuilds everything derived from one already stored document,
// which is what a database written before any of this existed needs.
func (w *writer) statistics(ctx context.Context, rowid int64, d doc.Document) error {
	a := d.Analyze()
	if _, err := w.columns.ExecContext(ctx, a.TitleTokens, a.BodyTokens, d.Container, d.Author.Display(), rowid); err != nil {
		return err
	}
	return w.index(ctx, rowid, d, a)
}
