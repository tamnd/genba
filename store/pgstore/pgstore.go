// Package pgstore is a storage driver backed by PostgreSQL 18.
//
// It exists for the shops that already run Postgres and are not going to be
// talked out of it. The trade is stated plainly in the storage design and it is
// worth repeating here: this driver gives a customer correctness on a database
// they already operate, and it is not the one to reach for when the corpus is
// large or the latency budget is tight. Everything below is written to make the
// first half of that true and the second half as small as it can be made.
//
// # Where the permission check happens
//
// It happens in SQL, in the same statement that applies the terms and the
// filters. The principal's identity and group keys are bound as text arrays and
// the allow and deny lists are rows, so the visibility rule is set membership
// that Postgres evaluates while it walks its own index. Nothing filters
// afterwards, which is what makes a count or a facet computed from those rows
// safe to show, and there is a test that reads the query plan to prove the
// predicate is in it rather than in Go.
//
// The rule itself is not written twice. The key forms come from acl and the
// fold from store, so the strings compared here are the strings
// acl.Permissions.Allows compares in Go, and store/storetest checks the two
// agree on a corpus.
//
// # Where the full text index comes from
//
// The tsvector is built in Go from the terms doc.Tokenize produced, rather than
// by handing Postgres the text and letting to_tsvector tokenize it a second
// time. See [tsvector] for why that matters more than it looks like it does.
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// Store is a [store.Store] over one PostgreSQL database.
type Store struct {
	// Notify makes this driver report its own writes. A cache above it drops
	// what the write made wrong, and the browser's event stream tells the page
	// on screen that the corpus moved. Both used to be a timer, which is the
	// answer that is either too slow to be useful or too expensive to be cheap.
	store.Notify

	pool *pgxpool.Pool
	opts Options

	// counters are what the performance gate asserts on. See [store.Counters].
	counters counters

	closed atomic.Bool
}

var (
	_ store.Store        = (*Store)(nil)
	_ store.Retriever    = (*Store)(nil)
	_ store.ContentStore = (*Store)(nil)
	_ store.Ranker       = (*Store)(nil)
	_ store.Statistician = (*Store)(nil)
	_ store.Fetcher      = (*Store)(nil)
	_ store.Notifier     = (*Store)(nil)
	_ store.Counted      = (*Store)(nil)
)

type counters struct {
	rows       atomic.Int64
	statements atomic.Int64
	decodes    atomic.Int64
	candidates atomic.Int64
}

// Counters reports the work this store has done since it was opened or last
// reset.
func (s *Store) Counters() store.Counters {
	return store.Counters{
		Rows:       s.counters.rows.Load(),
		Statements: s.counters.statements.Load(),
		Decodes:    s.counters.decodes.Load(),
		Candidates: s.counters.candidates.Load(),
	}
}

// ResetCounters zeroes them, so that a measurement can be scoped to one query.
func (s *Store) ResetCounters() {
	s.counters.rows.Store(0)
	s.counters.statements.Store(0)
	s.counters.decodes.Store(0)
	s.counters.candidates.Store(0)
}

// query and queryRow are the only two ways this driver reads, so that the
// statement counter cannot be forgotten at a new call site and so that the
// question marks are renumbered in one place.
func (s *Store) query(ctx context.Context, stmt string, args ...any) (pgx.Rows, error) {
	s.counters.statements.Add(1)
	return s.pool.Query(ctx, rebind(stmt), args...)
}

func (s *Store) queryRow(ctx context.Context, stmt string, args ...any) pgx.Row {
	s.counters.statements.Add(1)
	return s.pool.QueryRow(ctx, rebind(stmt), args...)
}

// Open connects to the database named by the DSN and brings its schema up to
// date.
//
// The DSN carries the pool, timeout and retry settings as well as the
// connection, which is what keeps a driver swap to one string. See [Options]
// and docs/postgres.md for the keys.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("pgstore: no dsn")
	}
	opts, clean, err := ParseOptions(dsn)
	if err != nil {
		return nil, err
	}

	cfg, err := pgxpool.ParseConfig(clean)
	if err != nil {
		return nil, fmt.Errorf("pgstore: dsn: %w", err)
	}
	cfg.MaxConns = opts.MaxConns
	cfg.MinConns = opts.MinConns
	cfg.MaxConnLifetime = opts.MaxConnLifetime
	cfg.MaxConnIdleTime = opts.MaxConnIdleTime
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = opts.ConnectTimeout
	}
	// A DSN that named a statement_timeout wins, because an operator who set
	// one has a reason. This is the backstop for the deployments that did not
	// think about it, which is most of them.
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["statement_timeout"]; !ok {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgstore: open: %w", err)
	}

	ms, err := Migrations()
	if err != nil {
		pool.Close()
		return nil, err
	}
	// The migration runs on one connection rather than through the pool,
	// because it takes an advisory lock and a pool is entitled to hand the next
	// statement to a different connection.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: open: %w", err)
	}
	err = migrate(ctx, conn.Conn(), ms)
	conn.Release()
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: migrate: %w", err)
	}
	return &Store{pool: pool, opts: opts}, nil
}

// retry runs fn until it succeeds, gives up, or fails with something a retry
// would not help.
//
// The operations it wraps are all either a single statement or a whole
// transaction, so running one twice produces what running it once would have.
// The streaming reads are the exception and they say so at their call site: a
// retry after the caller has already seen a row would deliver that row twice.
func (s *Store) retry(ctx context.Context, fn func(context.Context) error) error {
	wait := s.opts.Backoff
	var err error
	for attempt := 1; ; attempt++ {
		if err = fn(ctx); err == nil || attempt >= s.opts.Attempts || !retryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// Put inserts or replaces documents, in one transaction.
//
// The whole batch commits or none of it does, which is what the ingestion
// pipeline needs in order to be able to retry a batch without wondering how
// much of the last attempt landed.
func (s *Store) Put(ctx context.Context, docs ...doc.Document) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if len(docs) == 0 {
		return nil
	}
	for _, d := range docs {
		if d.ID == "" {
			return errors.New("pgstore: put: document has no id")
		}
	}
	// Later wins, which is what a batch carrying the same id twice means. It is
	// deduplicated rather than applied twice because the bookkeeping here is
	// aggregated over the batch: the corpus counters take one document out and
	// put one back, and doing that for two copies of the same row would count
	// it twice.
	docs = lastOfEach(docs)

	written := make(map[string][]string, 1)
	err := s.retry(ctx, func(ctx context.Context) error {
		clear(written)
		return s.transact(ctx, func(tx pgx.Tx) error {
			if err := write(ctx, tx, docs); err != nil {
				return err
			}
			for _, d := range docs {
				written[d.Tenant] = append(written[d.Tenant], d.ID)
			}
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("pgstore: put: %w", err)
	}
	// After the commit, never before. A subscriber that dropped a cache entry
	// for a write that then rolled back would refill it from the state the
	// write was about to replace, which is a worse cache than no cache.
	s.Changes(written, false)
	return nil
}

// Delete removes documents by id.
func (s *Store) Delete(ctx context.Context, ids ...string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	ids = distinct(ids)

	removed := make(map[string][]string, 1)
	err := s.retry(ctx, func(ctx context.Context) error {
		clear(removed)
		return s.transact(ctx, func(tx pgx.Tx) error {
			var err error
			removed, err = remove(ctx, tx, ids)
			return err
		})
	})
	if err != nil {
		return fmt.Errorf("pgstore: delete: %w", err)
	}
	s.Changes(removed, true)
	return nil
}

// transact runs fn in a transaction that holds the write lock.
//
// The lock is why the write path can do its corpus bookkeeping in aggregate
// statements instead of a read modify write per document. See [lockWrites] for
// what it costs and what it would take to make it finer grained.
func (s *Store) transact(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockWrites(ctx, tx); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Get returns one document if the principal may read it.
func (s *Store) Get(ctx context.Context, p *acl.Principal, id string) (doc.Document, error) {
	if err := s.ready(ctx); err != nil {
		return doc.Document{}, err
	}
	if p == nil {
		return doc.Document{}, genba.ErrNoPrincipal
	}

	c := visible(p)
	args := append([]any{id}, c.args...)

	var out doc.Document
	err := s.retry(ctx, func(ctx context.Context) error {
		var data string
		row := s.queryRow(ctx, `
			SELECT x.data FROM document d
			JOIN document_data x ON x.doc_id = d.id
			WHERE d.id = ? AND `+c.where(), args...)
		switch err := row.Scan(&data); {
		case errors.Is(err, pgx.ErrNoRows):
			// A document the caller may not read and one that is not there
			// produce the same error, with nothing in it that tells the two
			// apart.
			return genba.ErrNotFound
		case err != nil:
			return err
		}
		s.counters.rows.Add(1)
		var derr error
		out, derr = s.decoded(data)
		return derr
	})
	switch {
	case errors.Is(err, genba.ErrNotFound):
		return doc.Document{}, genba.ErrNotFound
	case err != nil:
		return doc.Document{}, fmt.Errorf("pgstore: get: %w", err)
	}
	return out, nil
}

// Content returns the bytes of one document if the principal may read it.
//
// The join is the point. The permission predicate is applied in the same
// statement that reads the bytes, so a caller who may not see the document
// never causes them to be read at all, let alone returned.
func (s *Store) Content(ctx context.Context, p *acl.Principal, id string) (doc.Content, error) {
	if err := s.ready(ctx); err != nil {
		return doc.Content{}, err
	}
	if p == nil {
		return doc.Content{}, genba.ErrNoPrincipal
	}

	c := visible(p)
	args := append([]any{id}, c.args...)

	var out doc.Content
	err := s.retry(ctx, func(ctx context.Context) error {
		row := s.queryRow(ctx, `
			SELECT c.width, c.height, c.bytes
			FROM document_content c
			JOIN document d ON d.id = c.doc_id
			WHERE c.doc_id = ? AND `+c.where(), args...)
		switch err := row.Scan(&out.Width, &out.Height, &out.Bytes); {
		case errors.Is(err, pgx.ErrNoRows):
			// Missing, invisible and text only are one answer here, for the
			// reason they are one answer in Get.
			return genba.ErrNotFound
		case err != nil:
			return err
		}
		s.counters.rows.Add(1)
		return nil
	})
	switch {
	case errors.Is(err, genba.ErrNotFound):
		return doc.Content{}, genba.ErrNotFound
	case err != nil:
		return doc.Content{}, fmt.Errorf("pgstore: content: %w", err)
	}
	return out, nil
}

// Scan calls fn for every document the principal may read, in id order.
func (s *Store) Scan(ctx context.Context, p *acl.Principal, fn func(doc.Document) bool) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}

	c := visible(p)
	return s.stream(ctx, `
		SELECT x.data FROM document d
		JOIN document_data x ON x.doc_id = d.id
		WHERE `+c.where()+` ORDER BY d.id`, c.args, fn)
}

// Retrieve answers a request out of the database's own indexes.
//
// This is the whole reason the driver exists. The terms go to the GIN index
// over the tsvector, the filters and the permission rule go into the same WHERE
// clause, and what comes back is the match set. Compare that with Scan, which
// is the same answer reached by reading every document in the tenant.
func (s *Store) Retrieve(ctx context.Context, p *acl.Principal, r store.Request, fn func(doc.Document) bool) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}

	query, args := retrieveQuery(p, r)
	return s.stream(ctx, query, args, fn)
}

// retrieveQuery is the statement Retrieve runs, built somewhere a test can get
// at it.
//
// It is a function rather than four lines inside Retrieve because the claim
// that the permission rule is in the query is only worth as much as the test
// that checks it, and a test that assembled its own copy of the statement would
// be checking a plan for a query nothing runs.
func retrieveQuery(p *acl.Principal, r store.Request) (query string, args []any) {
	c := visible(p)
	filters(r, c)
	if q, ok := tsquery(r.Terms); ok {
		c.add(`d.terms @@ ?::tsquery`, q)
	}
	return `
		SELECT x.data FROM document d
		JOIN document_data x ON x.doc_id = d.id
		WHERE ` + c.where() + ` ORDER BY d.id`, c.args
}

// stream runs a query returning document JSON and hands each row to fn.
//
// A failed connection is retried only up to the first row. After that the
// caller has seen documents, and a second attempt would hand it the same ones
// again, which is a worse failure than the error it is trying to avoid.
func (s *Store) stream(ctx context.Context, query string, args []any, fn func(doc.Document) bool) error {
	delivered := false
	err := s.retry(ctx, func(ctx context.Context) error {
		if delivered {
			return nil
		}
		rows, err := s.query(ctx, query, args...)
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
			delivered = true
			if !fn(d) {
				return nil
			}
		}
		return rows.Err()
	})
	if err != nil {
		return fmt.Errorf("pgstore: query: %w", err)
	}
	return nil
}

// Stats reports what the store holds.
func (s *Store) Stats(ctx context.Context) (store.Stats, error) {
	if err := s.ready(ctx); err != nil {
		return store.Stats{}, err
	}
	var st store.Stats
	err := s.retry(ctx, func(ctx context.Context) error {
		return s.queryRow(ctx, `
			SELECT
				count(*) FILTER (WHERE queryable),
				count(*) FILTER (WHERE NOT queryable)
			FROM document`).Scan(&st.Documents, &st.Quarantined)
	})
	if err != nil {
		return store.Stats{}, fmt.Errorf("pgstore: stats: %w", err)
	}
	return st, nil
}

// Close releases the pool. It is safe to call twice.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.pool.Close()
	return nil
}

// ready is the check every method starts with: a live context and an open
// store.
func (s *Store) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return genba.ErrClosed
	}
	return nil
}

func decode(data string) (doc.Document, error) {
	var d doc.Document
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return doc.Document{}, fmt.Errorf("pgstore: decode: %w", err)
	}
	return d, nil
}

// decoded is decode with the counter, for the read paths.
func (s *Store) decoded(data string) (doc.Document, error) {
	s.counters.decodes.Add(1)
	return decode(data)
}

// lastOfEach keeps the final version of each document id, in the order the ids
// first appeared.
func lastOfEach(docs []doc.Document) []doc.Document {
	at := make(map[string]int, len(docs))
	out := make([]doc.Document, 0, len(docs))
	for _, d := range docs {
		if i, ok := at[d.ID]; ok {
			out[i] = d
			continue
		}
		at[d.ID] = len(out)
		out = append(out, d)
	}
	return out
}

func distinct(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// nullableNanos is a timestamp as the column holds it: unix nanoseconds, and
// NULL when the source never told us. NULL is not zero here, because a document
// with no known date is excluded by updated:>x and kept by updated:<x, which is
// what the Go rule does.
func nullableNanos(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	n := t.UnixNano()
	return &n
}

// unixNano is the other direction, in UTC, so that a document read back out of
// a column compares equal to the one that went in.
func unixNano(n int64) time.Time { return time.Unix(0, n).UTC() }
