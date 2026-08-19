package pgstore

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
)

// TestMigrationsAreVersionedAndOrdered reads the files that ship. It needs no
// database, which is the point: a migration named wrong is a migration that
// does not run, and that is not something to find out from a server at boot.
func TestMigrationsAreVersionedAndOrdered(t *testing.T) {
	ms, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations ship, so an empty database would stay empty")
	}
	for i, m := range ms {
		if m.Version != i+1 {
			t.Fatalf("migration %d is at position %d", m.Version, i)
		}
		if m.Name == "" {
			t.Fatalf("migration %d has no name, so the version table would say nothing", m.Version)
		}
		if strings.TrimSpace(m.Up) == "" {
			t.Fatalf("migration %d %s does nothing", m.Version, m.Name)
		}
		if !m.Reversible() {
			t.Fatalf("migration %d %s ships no way back", m.Version, m.Name)
		}
	}
}

// TestBadlyNamedMigrationsAreRefused checks the parser stops rather than
// quietly ignoring a file, because a migration skipped because somebody typed
// the name wrong is a bug found in production.
func TestBadlyNamedMigrationsAreRefused(t *testing.T) {
	cases := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{
			name: "no version",
			fsys: fstest.MapFS{"migrations/documents.up.sql": {Data: []byte("SELECT 1")}},
			want: "not named like a migration",
		},
		{
			name: "a version that is not a number",
			fsys: fstest.MapFS{"migrations/two_documents.up.sql": {Data: []byte("SELECT 1")}},
			want: "does not start with a version",
		},
		{
			name: "neither up nor down",
			fsys: fstest.MapFS{"migrations/0001_documents.sql": {Data: []byte("SELECT 1")}},
			want: "neither an up nor a down",
		},
		{
			name: "a down with no up",
			fsys: fstest.MapFS{"migrations/0001_documents.down.sql": {Data: []byte("SELECT 1")}},
			want: "has a down and no up",
		},
		{
			name: "a gap in the sequence",
			fsys: fstest.MapFS{
				"migrations/0001_documents.up.sql": {Data: []byte("SELECT 1")},
				"migrations/0003_vectors.up.sql":   {Data: []byte("SELECT 1")},
			},
			want: "out of sequence",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse(c.fsys)
			if err == nil {
				t.Fatal("parse accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the error says %q, want something about %q", err, c.want)
			}
		})
	}
}

// TestMigrationsAreIdempotent opens the same database twice. The second open
// runs the same migration code against a schema that is already current, which
// is the case every restart hits and the one that is easy to get wrong.
func TestMigrationsAreIdempotent(t *testing.T) {
	dsn := schemaDSN(t)

	first, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Put(t.Context(), readable("d1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if _, err := second.Get(t.Context(), reader(), "d1"); err != nil {
		t.Fatalf("the document written before the reopen is gone: %v", err)
	}
	st, err := second.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Documents != 1 {
		t.Fatalf("after reopening, Stats reports %d documents, want 1", st.Documents)
	}
}

// TestRollbackUndoesEverything runs the down scripts the way an operator backing
// a release out would, and then migrates forward again.
//
// The second migration is half the test. A down script that dropped most of what
// its up created would leave a schema that the next deployment cannot install
// over, and nothing about running the down alone would say so.
func TestRollbackUndoesEverything(t *testing.T) {
	dsn := schemaDSN(t)

	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Put(t.Context(), readable("d1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := Rollback(t.Context(), dsn, 0); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	conn := connect(t, dsn)
	var left []string
	rows, err := conn.Query(t.Context(), `
		SELECT tablename FROM pg_tables
		WHERE schemaname = current_schema() AND tablename <> 'schema_migration'
		ORDER BY tablename`)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("listing tables: %v", err)
		}
		left = append(left, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("rolling back to nothing left %v behind", left)
	}

	var applied int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM schema_migration`).Scan(&applied); err != nil {
		t.Fatalf("reading the version table: %v", err)
	}
	if applied != 0 {
		t.Fatalf("the version table still records %d migrations", applied)
	}

	again, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("migrating forward again after a rollback: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if _, err := again.Get(t.Context(), reader(), "d1"); err == nil {
		t.Fatal("a document survived a rollback that dropped the table it was in")
	}
}

// TestRollbackRefusesAVersionItDoesNotKnow keeps a typo from being read as a
// request to drop everything.
func TestRollbackRefusesAVersionItDoesNotKnow(t *testing.T) {
	dsn := schemaDSN(t)
	if err := Rollback(t.Context(), dsn, 99); err == nil {
		t.Fatal("Rollback accepted a version this build has never heard of")
	}
	if err := Rollback(t.Context(), dsn, -1); err == nil {
		t.Fatal("Rollback accepted a negative version")
	}
}

// TestOpeningANewerDatabaseIsRefused covers a rollback of the binaries without a
// rollback of the schema. An old server that carried on against a schema it does
// not understand would write rows the new one cannot read.
func TestOpeningANewerDatabaseIsRefused(t *testing.T) {
	dsn := schemaDSN(t)
	if _, err := Open(t.Context(), dsn); err != nil {
		t.Fatalf("Open: %v", err)
	}

	conn := connect(t, dsn)
	ms, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	if _, err := conn.Exec(t.Context(),
		`INSERT INTO schema_migration (version, name) VALUES ($1, 'from the future')`, len(ms)+1); err != nil {
		t.Fatalf("recording a migration from a newer build: %v", err)
	}

	_, err = Open(t.Context(), dsn)
	if err == nil {
		t.Fatal("a build opened a database that a newer one had already migrated")
	}
	if !strings.Contains(err.Error(), "refusing to open it") {
		t.Fatalf("the error says %q, and an operator needs to be told the build is too old", err)
	}
}

func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	_, clean, err := ParseOptions(dsn)
	if err != nil {
		t.Fatalf("ParseOptions: %v", err)
	}
	conn, err := pgx.Connect(t.Context(), clean)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(t.Context()) })
	return conn
}
