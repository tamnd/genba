package pgstore

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// migrationFS holds the schema, as SQL an operator can read before running it.
//
// The SQLite driver keeps its migrations in a Go slice because that schema is
// managed entirely by the process that owns the file. This one is not. A
// Postgres database is somebody's cluster, it has a DBA, and the answer to
// "what is this about to do to my database" has to be a file, not a build.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one versioned schema change.
//
// Up is applied in ascending order and never edited once it has shipped. Down
// undoes it, and is empty for a change that cannot be undone, which is a fact
// about the change rather than an omission: a migration that drops a column
// cannot put the values back, and pretending otherwise in a file called
// something.down.sql is worse than saying so.
type Migration struct {
	Version int
	Name    string
	Up      string
	Down    string
}

// Reversible reports whether this migration ships a way back.
func (m Migration) Reversible() bool { return strings.TrimSpace(m.Down) != "" }

// Migrations returns the schema, in order.
//
// It is exported because an operator running a cluster they did not install
// wants to read the SQL, and because the CI job that checks the files parse has
// to have something to call.
func Migrations() ([]Migration, error) { return parse(migrationFS) }

// parse reads the migration directory.
//
// The file name is the contract: four digits, an underscore, a name, and either
// .up.sql or .down.sql. Anything else in the directory is an error rather than
// something quietly ignored, because a migration that is not applied because it
// was misnamed is the kind of bug that is found in production.
func parse(fsys fs.FS) ([]Migration, error) {
	names, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	slices.Sort(names)

	byVersion := map[int]*Migration{}
	for _, name := range names {
		base := path.Base(name)
		version, rest, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("pgstore: %s is not named like a migration", base)
		}
		n, err := strconv.Atoi(version)
		if err != nil {
			return nil, fmt.Errorf("pgstore: %s does not start with a version: %w", base, err)
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		m, ok := byVersion[n]
		if !ok {
			m = &Migration{Version: n}
			byVersion[n] = m
		}
		switch {
		case strings.HasSuffix(rest, ".up.sql"):
			m.Name = strings.TrimSuffix(rest, ".up.sql")
			m.Up = string(body)
		case strings.HasSuffix(rest, ".down.sql"):
			m.Down = string(body)
		default:
			return nil, fmt.Errorf("pgstore: %s is neither an up nor a down migration", base)
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		out = append(out, *m)
	}
	slices.SortFunc(out, func(a, b Migration) int { return a.Version - b.Version })

	for i, m := range out {
		if m.Up == "" {
			return nil, fmt.Errorf("pgstore: migration %d has a down and no up", m.Version)
		}
		// Versions are contiguous from one. A gap means a migration was deleted
		// rather than superseded, and a database that applied it is now at a
		// version this build cannot reason about.
		if m.Version != i+1 {
			return nil, fmt.Errorf("pgstore: migration %d is out of sequence, expected %d", m.Version, i+1)
		}
	}
	return out, nil
}

// migrationLock is the advisory lock every schema change is taken under.
//
// Two servers starting at once against a fresh database would otherwise both
// find no schema and both try to create one, and the loser is a process that
// exits at boot with a duplicate table error. It is a fixed number rather than
// a hash of anything, so that it means the same thing in every version of this
// code: 0x67656e6261 is "genba" in ASCII.
const migrationLock = 0x67656e6261

// migrate brings a database up to the current schema.
//
// It is safe to run against a database that is already current, which is what
// makes starting a second server the same code path as installing the first.
// Every pending migration and the row that records it are one transaction, so a
// migration that fails leaves a database at the version it was at rather than
// half way into the next one.
func migrate(ctx context.Context, conn *pgx.Conn, ms []Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Before the table is read, because the first thing a fresh database needs
	// is for exactly one of the servers starting against it to create that
	// table. The lock is transaction scoped, so it is released by the commit
	// and there is no path where a crashed process holds it.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrationLock)); err != nil {
		return fmt.Errorf("take the schema lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migration (
			version    integer     NOT NULL PRIMARY KEY,
			name       text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create the migration table: %w", err)
	}

	applied, err := appliedVersions(ctx, tx)
	if err != nil {
		return err
	}
	if len(applied) > len(ms) {
		return fmt.Errorf("database is at schema version %d and this build only knows %d, refusing to open it", applied[len(applied)-1], len(ms))
	}
	// A prefix, not a set. A database that skipped a version was migrated by
	// something other than this code, and guessing what to do about it is worse
	// than stopping.
	for i, v := range applied {
		if v != i+1 {
			return fmt.Errorf("schema version %d was applied without version %d, refusing to open it", v, i+1)
		}
	}

	for _, m := range ms[len(applied):] {
		if _, err := tx.Exec(ctx, m.Up); err != nil {
			return fmt.Errorf("apply migration %d %s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migration (version, name) VALUES ($1, $2)`, m.Version, m.Name); err != nil {
			return fmt.Errorf("record migration %d: %w", m.Version, err)
		}
	}
	return tx.Commit(ctx)
}

// Rollback undoes every migration above version, newest first.
//
// It is here rather than left to a DBA with a copy of the down files because
// the version table has to be kept in step with them, and because a rollback
// that runs the steps in the order they appear on disk undoes them in the order
// they were applied, which is backwards. Version 0 is an empty database.
//
// It refuses rather than skipping when a migration in the range does not ship a
// down, because a partial rollback leaves a schema that matches no version of
// this code.
func Rollback(ctx context.Context, dsn string, version int) error {
	opts, clean, err := ParseOptions(dsn)
	if err != nil {
		return err
	}
	ms, err := Migrations()
	if err != nil {
		return err
	}
	if version < 0 || version > len(ms) {
		return fmt.Errorf("pgstore: rollback: version %d is not one this build knows", version)
	}

	cfg, err := pgx.ParseConfig(clean)
	if err != nil {
		return fmt.Errorf("pgstore: rollback: %w", err)
	}
	cfg.ConnectTimeout = opts.ConnectTimeout
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("pgstore: rollback: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: rollback: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrationLock)); err != nil {
		return fmt.Errorf("pgstore: rollback: take the schema lock: %w", err)
	}
	applied, err := appliedVersions(ctx, tx)
	if err != nil {
		return fmt.Errorf("pgstore: rollback: %w", err)
	}

	for i := len(applied) - 1; i >= 0; i-- {
		v := applied[i]
		if v <= version {
			break
		}
		m := ms[v-1]
		if !m.Reversible() {
			return fmt.Errorf("pgstore: rollback: migration %d %s cannot be undone", m.Version, m.Name)
		}
		if _, err := tx.Exec(ctx, m.Down); err != nil {
			return fmt.Errorf("pgstore: rollback: undo migration %d %s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM schema_migration WHERE version = $1`, v); err != nil {
			return fmt.Errorf("pgstore: rollback: forget migration %d: %w", v, err)
		}
	}
	return tx.Commit(ctx)
}

// appliedVersions reads the recorded schema versions in ascending order.
func appliedVersions(ctx context.Context, tx pgx.Tx) ([]int, error) {
	rows, err := tx.Query(ctx, `SELECT version FROM schema_migration ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read the schema version: %w", err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("read the schema version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
