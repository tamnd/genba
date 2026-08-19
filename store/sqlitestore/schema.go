package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// step is one migration: a statement, or a piece of Go for the ones that need
// to read a document to decide what to write.
type step struct {
	sql string
	run func(ctx context.Context, tx *sql.Tx) error
}

func ddl(s string) step { return step{sql: s} }

// migrations are applied in order and recorded by index in user_version.
//
// They are a plain slice rather than a directory of numbered files because the
// whole schema fits on a screen and a migration tool would be more machinery
// than the thing it manages. The rules are the usual ones: a migration is never
// edited once it has shipped, and a new one is appended.
var migrations = []step{
	ddl(`CREATE TABLE document (
		id            TEXT PRIMARY KEY,
		tenant        TEXT NOT NULL,
		source        TEXT NOT NULL,
		kind          TEXT NOT NULL,

		-- container_fold, author_keys and owner_keys are folded copies of what a
		-- text filter compares against, computed by store.Fold and
		-- store.PersonKeys so that the comparison in SQL is the comparison in
		-- Go rather than SQLite's idea of lower case.
		container_fold TEXT NOT NULL,
		author_keys    TEXT NOT NULL,
		owner_keys     TEXT NOT NULL,

		-- modified_at is unix nanoseconds, and NULL when the source never told
		-- us. NULL is not zero here: a document with no known date is excluded
		-- by updated:>x and kept by updated:<x, which is what the Go rule does.
		modified_at   INTEGER,

		-- mode and owner_key are the parts of the permission descriptor the
		-- visibility predicate needs in a column. The rest lives in data.
		mode          INTEGER NOT NULL,
		owner_key     TEXT NOT NULL,

		-- queryable is doc.Document.Queryable. A row with a zero here is
		-- quarantined and no query path may return it.
		queryable     INTEGER NOT NULL,

		-- data is the document itself, as JSON. Storing it whole means Get
		-- returns exactly what Put was given, and the columns above are only
		-- ever an index over it.
		data          TEXT NOT NULL
	)`),
	ddl(`CREATE INDEX document_tenant ON document (tenant, queryable)`),
	ddl(`CREATE INDEX document_source ON document (source)`),
	ddl(`CREATE INDEX document_modified ON document (modified_at)`),

	// document_ref is the allow and deny lists, one row per reference, in the
	// key form acl.Ref produces. The permission predicate is set membership
	// against this table, which is the same set membership acl.Permissions
	// performs in Go.
	ddl(`CREATE TABLE document_ref (
		doc_id TEXT NOT NULL REFERENCES document(id) ON DELETE CASCADE,
		effect INTEGER NOT NULL, -- 0 allow, 1 deny
		scope  INTEGER NOT NULL, -- 0 user,  1 group
		key    TEXT NOT NULL
	)`),
	ddl(`CREATE INDEX document_ref_doc ON document_ref (doc_id, effect, scope)`),
	ddl(`CREATE INDEX document_ref_key ON document_ref (key, effect, scope)`),

	// The full text index holds doc.Document.Analyzed, which is already the
	// terms the ranking tokenizes to. unicode61 then has nothing left to do but
	// split on the spaces we put there, so the driver's match set and the Go
	// rule cannot drift over what counts as a word. Diacritic folding is turned
	// off for the same reason: the Go analyzer does not do it.
	ddl(`CREATE VIRTUAL TABLE document_fts USING fts5(
		terms,
		content='',
		contentless_delete=1,
		tokenize='unicode61 remove_diacritics 0'
	)`),

	// document_content is the bytes of a document that is not text, in its own
	// table rather than in the data column. The reason is the shape of the
	// reads: data is selected by every query path, and a megabyte of PNG in it
	// would be a megabyte read per hit whether or not anybody ever looks at the
	// image. Here it is only read by the one endpoint that serves it.
	ddl(`CREATE TABLE document_content (
		doc_id     TEXT PRIMARY KEY REFERENCES document(id) ON DELETE CASCADE,
		width      INTEGER NOT NULL,
		height     INTEGER NOT NULL,
		bytes      BLOB NOT NULL
	)`),

	// Everything below is the ranking data model. It exists so that no part of
	// answering a query is proportional to the corpus outside an index walk
	// SQLite does for itself.
	//
	// Token counts, not a weighted length. The weight a ranker puts on a title
	// is part of the ranking function, and a column that had it baked in would
	// be a second place that function lives.
	ddl(`ALTER TABLE document ADD COLUMN title_tokens INTEGER NOT NULL DEFAULT 0`),
	ddl(`ALTER TABLE document ADD COLUMN body_tokens INTEGER NOT NULL DEFAULT 0`),

	// The display forms of the two fields that are faceted on. container_fold
	// and author_keys are already here for filtering, and a facet needs the
	// string a person reads. Deriving it by decoding a document per row is
	// precisely the cost this is removing.
	ddl(`ALTER TABLE document ADD COLUMN container TEXT NOT NULL DEFAULT ''`),
	ddl(`ALTER TABLE document ADD COLUMN author_name TEXT NOT NULL DEFAULT ''`),

	// posting is the term frequency store, not a second matcher. FTS5 stays the
	// thing that decides which documents are candidates, because it is good at
	// it and it handles the phrase queries the operator grammar will grow.
	//
	// The key is (doc_rowid, term) rather than (term, doc_rowid), which is the
	// other way round from a classic inverted index. Nothing here ever asks
	// which documents carry a term: that question is answered by FTS5 for
	// matching and by term_stat for document frequency. What it does ask is
	// which terms a handful of already chosen candidates carry, and this order
	// answers that from the primary key, which also means the delete on rewrite
	// is one range rather than a scan and there is no second index to keep.
	ddl(`CREATE TABLE posting (
		doc_rowid INTEGER NOT NULL,
		term      TEXT    NOT NULL,
		title_tf  INTEGER NOT NULL,
		body_tf   INTEGER NOT NULL,
		PRIMARY KEY (doc_rowid, term)
	) WITHOUT ROWID`),

	// corpus and term_stat are the numbers BM25 needs about the corpus rather
	// than about a document. They are maintained on every write so that a query
	// reads a row instead of running an aggregate over everything.
	ddl(`CREATE TABLE corpus (
		tenant       TEXT PRIMARY KEY,
		documents    INTEGER NOT NULL,
		title_tokens INTEGER NOT NULL,
		body_tokens  INTEGER NOT NULL
	) WITHOUT ROWID`),
	ddl(`CREATE TABLE term_stat (
		tenant    TEXT    NOT NULL,
		term      TEXT    NOT NULL,
		documents INTEGER NOT NULL,
		PRIMARY KEY (tenant, term)
	) WITHOUT ROWID`),

	// A database written before the statistics existed has none of them, and
	// every document in it would score zero. Rebuilding them is a read and a
	// rewrite of every row, which is slow and is also exactly right: it happens
	// once, at open, and the alternative is a corpus that silently ranks badly
	// until somebody notices.
	{run: backfill},

	// The document body moves out of the document row.
	//
	// This one is worth explaining, because it looks like tidiness and it is
	// worth ten milliseconds a query. A row in SQLite is stored whole, and a
	// row longer than a page spills onto overflow pages. The document JSON
	// averages four kilobytes here, so every document row is a page of its own
	// plus a chain of overflow pages, and reading any column of it reads the
	// start of that chain.
	//
	// Phase one of a search does exactly that, for every document that matched
	// the terms, because the permission predicate is columns on this row. On
	// twenty thousand documents that measured at seventy five milliseconds for
	// the candidate cut alone, against a budget of ten for the whole query, and
	// the same statement against a table without the body in it measured at
	// three. The bodies were never read. They were in the way.
	//
	// So the columns a query filters, counts and ranks on stay in a narrow row
	// that a page holds hundreds of, and the document itself lives in a table
	// that only the twenty results on the page ever touch. It is the same
	// argument document_content was split out on, one level further in.
	ddl(`CREATE TABLE document_data (
		doc_id TEXT PRIMARY KEY REFERENCES document(id) ON DELETE CASCADE,
		data   TEXT NOT NULL
	) WITHOUT ROWID`),
	ddl(`INSERT INTO document_data (doc_id, data) SELECT id, data FROM document`),
	ddl(`ALTER TABLE document DROP COLUMN data`),
}

// backfill recomputes the ranking statistics for every document already stored.
func backfill(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT rowid, data FROM document ORDER BY rowid`)
	if err != nil {
		return err
	}
	type row struct {
		rowid int64
		data  string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.rowid, &r.data); err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}

	w, err := newWriter(ctx, tx)
	if err != nil {
		return err
	}
	defer w.close()

	for _, r := range all {
		d, err := decode(r.data)
		if err != nil {
			return err
		}
		if err := w.statistics(ctx, r.rowid, d); err != nil {
			return fmt.Errorf("rebuild %s: %w", d.ID, err)
		}
	}
	return nil
}

// migrate brings an open database up to the current schema.
//
// It is safe to run against a database that is already current, which is what
// makes opening an existing file the same code path as creating a new one.
// user_version is SQLite's own four byte header field, so the version is read
// without a table having to exist first.
//
// The steps are a parameter rather than the package variable so that a test can
// build a database at an older version and then upgrade it, which is the only
// way to exercise a migration against data that was written before it existed.
func migrate(ctx context.Context, db *sql.DB, migrations []step) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database is at schema version %d and this build only knows %d, refusing to open it", version, len(migrations))
	}
	if version == len(migrations) {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, m := range migrations[version:] {
		switch {
		case m.sql != "":
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("apply migration %d: %w", version, err)
			}
		case m.run != nil:
			if err := m.run(ctx, tx); err != nil {
				return fmt.Errorf("apply migration %d: %w", version, err)
			}
		}
		version++
	}
	// PRAGMA user_version does not take a bound parameter, and version is an
	// int this function counted, not anything a caller supplied.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return tx.Commit()
}
