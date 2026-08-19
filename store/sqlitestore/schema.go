package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order and recorded by index in user_version.
//
// They are a plain slice rather than a directory of numbered files because the
// whole schema fits on a screen and a migration tool would be more machinery
// than the thing it manages. The rules are the usual ones: a migration is never
// edited once it has shipped, and a new one is appended.
var migrations = []string{
	`CREATE TABLE document (
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
	)`,
	`CREATE INDEX document_tenant ON document (tenant, queryable)`,
	`CREATE INDEX document_source ON document (source)`,
	`CREATE INDEX document_modified ON document (modified_at)`,

	// document_ref is the allow and deny lists, one row per reference, in the
	// key form acl.Ref produces. The permission predicate is set membership
	// against this table, which is the same set membership acl.Permissions
	// performs in Go.
	`CREATE TABLE document_ref (
		doc_id TEXT NOT NULL REFERENCES document(id) ON DELETE CASCADE,
		effect INTEGER NOT NULL, -- 0 allow, 1 deny
		scope  INTEGER NOT NULL, -- 0 user,  1 group
		key    TEXT NOT NULL
	)`,
	`CREATE INDEX document_ref_doc ON document_ref (doc_id, effect, scope)`,
	`CREATE INDEX document_ref_key ON document_ref (key, effect, scope)`,

	// The full text index holds doc.Document.Analyzed, which is already the
	// terms the ranking tokenizes to. unicode61 then has nothing left to do but
	// split on the spaces we put there, so the driver's match set and the Go
	// rule cannot drift over what counts as a word. Diacritic folding is turned
	// off for the same reason: the Go analyzer does not do it.
	`CREATE VIRTUAL TABLE document_fts USING fts5(
		terms,
		content='',
		contentless_delete=1,
		tokenize='unicode61 remove_diacritics 0'
	)`,

	// document_content is the bytes of a document that is not text, in its own
	// table rather than in the data column. The reason is the shape of the
	// reads: data is selected by every query path, and a megabyte of PNG in it
	// would be a megabyte read per hit whether or not anybody ever looks at the
	// image. Here it is only read by the one endpoint that serves it.
	`CREATE TABLE document_content (
		doc_id     TEXT PRIMARY KEY REFERENCES document(id) ON DELETE CASCADE,
		width      INTEGER NOT NULL,
		height     INTEGER NOT NULL,
		bytes      BLOB NOT NULL
	)`,
}

// migrate brings an open database up to the current schema.
//
// It is safe to run against a database that is already current, which is what
// makes opening an existing file the same code path as creating a new one.
// user_version is SQLite's own four byte header field, so the version is read
// without a table having to exist first.
func migrate(ctx context.Context, db *sql.DB) error {
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

	for _, stmt := range migrations[version:] {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply migration %d: %w", version, err)
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
