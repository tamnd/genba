// Package sqlitestore is a storage driver backed by SQLite.
//
// It is the first durable driver, and it is the one a single node deployment
// runs on: one file, no server to operate, and enough of an index that a
// laptop sized corpus answers queries without reading every document. The
// build stays pure Go, so a clone still cross compiles and still needs no C
// toolchain.
//
// # Where the permission check happens
//
// It happens in SQL, in the same statement that applies the terms and the
// filters. The principal's identity and group keys are bound into the query as
// JSON arrays and the allow and deny lists are rows, so the visibility rule is
// set membership that SQLite evaluates while it walks its own index. Nothing
// filters afterwards, which is what makes a count or a facet computed from
// those rows safe to show.
//
// The rule itself is not written twice. The key forms come from acl and the
// fold from store, so the strings compared here are the strings
// acl.Permissions.Allows compares in Go, and store/storetest checks the two
// agree on a corpus.
package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"

	_ "modernc.org/sqlite" // the pure Go SQLite driver
)

// Store is a [store.Store] over one SQLite database.
type Store struct {
	db *sql.DB

	// write serialises writers. SQLite allows one at a time and in WAL mode a
	// second one gets SQLITE_BUSY, so the choice is to queue here or to retry
	// there. Queueing is cheaper and it makes the ingestion pipeline's
	// behaviour under load predictable rather than dependent on a timeout.
	write sync.Mutex

	// counters are what the performance gate asserts on. See [Counters].
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
)

// Counters is the work one query cost, counted exactly.
//
// It is here because a latency assertion on a shared CI runner is a coin flip
// and these are not. Rows read, statements issued, documents decoded and
// candidates scored do not vary with how busy the machine is, they are where a
// performance regression shows up first, and an assertion on them names the
// mistake precisely: a per hit refetch reappearing in a year moves Decodes from
// twenty to twenty plus the match set, on any runner, at any speed.
type Counters struct {
	// Rows is the rows the database handed back. It is what the test that
	// proves the permission filter is in the SQL asserts on: a reader who may
	// see nothing has to cost zero rows, not a full walk that Go then discards.
	Rows int64

	// Statements is the statements executed against the database, read paths
	// only. A search that issues one per result rather than one per page is the
	// regression this counts.
	Statements int64

	// Decodes is document JSON decoded into a [doc.Document]. A search should
	// decode the page and nothing else.
	Decodes int64

	// Candidates is the documents handed to the ranker. It is bounded by the
	// candidate pool rather than by the match set, which is the whole point of
	// two phase retrieval.
	Candidates int64
}

type counters struct {
	rows       atomic.Int64
	statements atomic.Int64
	decodes    atomic.Int64
	candidates atomic.Int64
}

// Counters reports the work this store has done since it was opened or last
// reset.
func (s *Store) Counters() Counters {
	return Counters{
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
// statement counter cannot be forgotten at a new call site.
func (s *Store) query(ctx context.Context, sql string, args ...any) (*sql.Rows, error) {
	s.counters.statements.Add(1)
	return s.db.QueryContext(ctx, sql, args...)
}

func (s *Store) queryRow(ctx context.Context, sql string, args ...any) *sql.Row {
	s.counters.statements.Add(1)
	return s.db.QueryRowContext(ctx, sql, args...)
}

// Open opens or creates a database at path and brings its schema up to date.
//
// The path is a file name. The special value ":memory:" gives a private
// database that lives as long as the [Store], which is what the tests use.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlitestore: no path")
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open %s: %w", path, err)
	}
	// An in memory database belongs to its connection, so a pool of them would
	// be a pool of different databases. Naming it and sharing the cache is the
	// other way round the problem, and this one has fewer surprises.
	if memory(path) {
		db.SetMaxOpenConns(1)
	}
	if err := migrate(ctx, db, migrations); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlitestore: migrate %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// CacheBytes is the page cache each connection is given.
//
// SQLite's default is two thousand pages, which is eight megabytes, and it was
// chosen for a world where a database was a settings file. It is far too small
// for either of the things this store does. A write of five hundred documents
// touches a few hundred thousand b-tree pages across the full text index and
// the postings, and a cache that cannot hold them spills the difference to the
// write ahead log and reads it back, which measured as ninety percent of
// ingestion time spent in write syscalls. A search wants the same headroom for
// the opposite reason: the latency budget assumes a warm cache, and a cache
// that cannot hold the working set makes every query pay for a page fault.
//
// Sixty four megabytes is per connection, so a pool of eight is half a gigabyte
// in the worst case. That is a deliberate trade and it is the right one for a
// search server, which is a thing people run on a machine with memory.
const CacheBytes = 64 << 20

// dsn adds the pragmas every connection needs.
//
// WAL is what lets a reader run while the ingestion pipeline writes, which is
// the normal state of this system rather than an edge case. busy_timeout covers
// the moments when two writers still meet, and foreign_keys is what makes the
// cascade from document to document_ref actually happen, because SQLite leaves
// it off by default.
func dsn(path string) string {
	q := url.Values{}
	// There is deliberately no page_size here. Setting it looked like a win in
	// a first measurement and turned out to do nothing at all: the pragma only
	// takes effect before the first page is written, the driver applies these
	// after the file exists, and a database opened with it reports the default
	// four kilobytes. A setting that does not take effect is worse than one
	// that is absent, because the next person reads it and believes it.
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	// Negative is kibibytes rather than pages, which is the only form of this
	// pragma that means the same thing whatever the page size turns out to be.
	q.Add("_pragma", fmt.Sprintf("cache_size(-%d)", CacheBytes/1024))
	// Sorting and the intermediate results of a grouped aggregate, which the
	// facet counts are. On disk they are a temporary file per query.
	q.Add("_pragma", "temp_store(MEMORY)")
	return path + "?" + q.Encode()
}

func memory(path string) bool {
	return path == ":memory:" || strings.HasPrefix(path, "file::memory:")
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
			return errors.New("sqlitestore: put: document has no id")
		}
	}

	s.write.Lock()
	defer s.write.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: put: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	w, err := newWriter(ctx, tx)
	if err != nil {
		return fmt.Errorf("sqlitestore: put: %w", err)
	}
	defer w.close()

	for _, d := range docs {
		if err := putOne(ctx, tx, w, d); err != nil {
			return fmt.Errorf("sqlitestore: put %s: %w", d.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: put: %w", err)
	}
	return nil
}

// putOne writes one document and everything derived from it.
//
// The upsert keeps the rowid, which matters because the full text index is
// joined on it. A delete and insert would give the row a new identity and leave
// the old terms behind under the old one.
func putOne(ctx context.Context, tx *sql.Tx, w *writer, d doc.Document) error {
	// The content comes off the document before it is encoded, so the data
	// column stays the size of the text and Get cannot hand image bytes back to
	// a query path that has no use for them.
	content := d.Content
	d.Content = nil

	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	var ownerKey string
	if d.Permissions.Owner.Value != "" {
		ownerKey = d.Permissions.Owner.UserKey()
	}

	// Whatever is stored under this id stops counting before the row that
	// carries the numbers is overwritten, because after the write there is no
	// way left to know what it used to contribute.
	if _, _, err := w.retire(ctx, d.ID); err != nil {
		return err
	}

	// One analysis, used three times over: the full text index row, the
	// postings and the length columns. Running the analyzer once per use is how
	// indexing ends up three times slower than it needs to be.
	a := d.Analyze()

	var rowid int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO document (
			id, tenant, source, kind, container_fold, author_keys, owner_keys,
			modified_at, mode, owner_key, queryable,
			title_tokens, body_tokens, container, author_name
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			tenant = excluded.tenant,
			source = excluded.source,
			kind = excluded.kind,
			container_fold = excluded.container_fold,
			author_keys = excluded.author_keys,
			owner_keys = excluded.owner_keys,
			modified_at = excluded.modified_at,
			mode = excluded.mode,
			owner_key = excluded.owner_key,
			queryable = excluded.queryable,
			title_tokens = excluded.title_tokens,
			body_tokens = excluded.body_tokens,
			container = excluded.container,
			author_name = excluded.author_name
		RETURNING rowid`,
		d.ID, d.Tenant, d.Source, string(d.Kind),
		store.Fold(d.Container), jsonKeys(store.PersonKeys(d.Author)), jsonKeys(store.PersonKeys(d.Owner)),
		nullableTime(d.ModifiedAt), int(d.Permissions.Mode), ownerKey, boolInt(d.Queryable()),
		a.TitleTokens, a.BodyTokens, d.Container, d.Author.Display(),
	).Scan(&rowid)
	if err != nil {
		return err
	}

	// The document itself goes to its own table. See the migration that split
	// it out for why: it is the one part of a row no query path reads until it
	// knows which twenty documents it is returning.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO document_data (doc_id, data) VALUES (?, ?)
		ON CONFLICT(doc_id) DO UPDATE SET data = excluded.data`, d.ID, string(data)); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM document_ref WHERE doc_id = ?`, d.ID); err != nil {
		return err
	}
	for _, r := range refsOf(d.Permissions) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO document_ref (doc_id, effect, scope, key) VALUES (?, ?, ?, ?)`,
			d.ID, r.effect, r.scope, r.key,
		); err != nil {
			return err
		}
	}

	// A put that carries no content clears whatever was there, because a source
	// replacing a screenshot with a text file has to leave nothing behind.
	if _, err := tx.ExecContext(ctx, `DELETE FROM document_content WHERE doc_id = ?`, d.ID); err != nil {
		return err
	}
	if content != nil {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO document_content (doc_id, width, height, bytes) VALUES (?, ?, ?, ?)`,
			d.ID, content.Width, content.Height, content.Bytes,
		); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM document_fts WHERE rowid = ?`, rowid); err != nil {
		return err
	}
	// A quarantined document is not indexed at all. The visibility predicate
	// already excludes it, and leaving it out of the index means a mistake in
	// that predicate cannot turn into a search result either.
	if !d.Queryable() {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO document_fts (rowid, terms) VALUES (?, ?)`, rowid, d.Analyzed()); err != nil {
		return err
	}
	return w.index(ctx, rowid, d, a)
}

// ref is one row of document_ref.
type ref struct {
	effect int // 0 allow, 1 deny
	scope  int // 0 user, 1 group
	key    string
}

// refsOf flattens a permission descriptor into rows, in the key forms acl
// compares.
func refsOf(perm acl.Permissions) []ref {
	var out []ref
	add := func(effect, scope int, refs []acl.Ref, key func(acl.Ref) string) {
		for _, r := range refs {
			if k := key(r); k != "" {
				out = append(out, ref{effect: effect, scope: scope, key: k})
			}
		}
	}
	add(0, 0, perm.AllowUsers, acl.Ref.UserKey)
	add(0, 1, perm.AllowGroups, acl.Ref.GroupKey)
	add(1, 0, perm.DenyUsers, acl.Ref.UserKey)
	add(1, 1, perm.DenyGroups, acl.Ref.GroupKey)
	return out
}

// Delete removes documents by id.
func (s *Store) Delete(ctx context.Context, ids ...string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	s.write.Lock()
	defer s.write.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	w, err := newWriter(ctx, tx)
	if err != nil {
		return fmt.Errorf("sqlitestore: delete: %w", err)
	}
	defer w.close()

	for _, id := range ids {
		// The rowid is read first because the full text index is keyed on it
		// and knows nothing about document ids. Deleting a document that is not
		// there is not an error, which is what makes a retry safe. retire is
		// what takes the document back out of the corpus statistics, which has
		// to happen while its postings are still there to be counted.
		rowid, found, err := w.retire(ctx, id)
		if err != nil {
			return fmt.Errorf("sqlitestore: delete %s: %w", id, err)
		}
		if !found {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM document_fts WHERE rowid = ?`, rowid); err != nil {
			return fmt.Errorf("sqlitestore: delete %s: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM document WHERE id = ?`, id); err != nil {
			return fmt.Errorf("sqlitestore: delete %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: delete: %w", err)
	}
	return nil
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
	row := s.queryRow(ctx, `
		SELECT x.data FROM document d
		JOIN document_data x ON x.doc_id = d.id
		WHERE d.id = ? AND `+c.where(), args...)

	var data string
	switch err := row.Scan(&data); {
	case errors.Is(err, sql.ErrNoRows):
		// A document the caller may not read and one that is not there produce
		// the same error, with nothing in it that tells the two apart.
		return doc.Document{}, genba.ErrNotFound
	case err != nil:
		return doc.Document{}, fmt.Errorf("sqlitestore: get: %w", err)
	}
	s.counters.rows.Add(1)
	return s.decoded(data)
}

// Content returns the bytes of one document if the principal may read it.
//
// The join is the point. The permission predicate is applied in the same
// statement that reads the blob, so a caller who may not see the document never
// causes the bytes to be read at all, let alone returned.
func (s *Store) Content(ctx context.Context, p *acl.Principal, id string) (doc.Content, error) {
	if err := s.ready(ctx); err != nil {
		return doc.Content{}, err
	}
	if p == nil {
		return doc.Content{}, genba.ErrNoPrincipal
	}

	c := visible(p)
	args := append([]any{id}, c.args...)
	row := s.queryRow(ctx, `
		SELECT c.width, c.height, c.bytes
		FROM document_content c
		JOIN document d ON d.id = c.doc_id
		WHERE c.doc_id = ? AND `+c.where(), args...)

	var out doc.Content
	switch err := row.Scan(&out.Width, &out.Height, &out.Bytes); {
	case errors.Is(err, sql.ErrNoRows):
		// Missing, invisible and text only are one answer here, for the reason
		// they are one answer in Get.
		return doc.Content{}, genba.ErrNotFound
	case err != nil:
		return doc.Content{}, fmt.Errorf("sqlitestore: content: %w", err)
	}
	s.counters.rows.Add(1)
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
// This is the whole reason the driver exists. The terms go to the full text
// index, the filters and the permission rule go into the same WHERE clause, and
// what comes back is the match set. Compare that with Scan, which is the same
// answer reached by reading every document in the tenant.
func (s *Store) Retrieve(ctx context.Context, p *acl.Principal, r store.Request, fn func(doc.Document) bool) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}

	c := visible(p)
	filters(r, c)

	from := `document d`
	if q, ok := match(r.Terms); ok {
		// The full text table leads the join. It is the most selective thing in
		// the statement on any real query, and joining from it means the
		// permission predicate is evaluated for the documents that matched
		// rather than for the corpus.
		from = `document_fts JOIN document d ON d.rowid = document_fts.rowid`
		c.add(`document_fts MATCH ?`, q)
	}

	return s.stream(ctx, `
		SELECT x.data FROM `+from+`
		JOIN document_data x ON x.doc_id = d.id
		WHERE `+c.where()+` ORDER BY d.id`, c.args, fn)
}

// stream runs a query returning document JSON and hands each row to fn.
func (s *Store) stream(ctx context.Context, query string, args []any, fn func(doc.Document) bool) error {
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("sqlitestore: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return fmt.Errorf("sqlitestore: scan: %w", err)
		}
		s.counters.rows.Add(1)
		d, err := s.decoded(data)
		if err != nil {
			return err
		}
		if !fn(d) {
			return nil
		}
	}
	return rows.Err()
}

// Stats reports what the store holds.
func (s *Store) Stats(ctx context.Context) (store.Stats, error) {
	if err := s.ready(ctx); err != nil {
		return store.Stats{}, err
	}
	var st store.Stats
	err := s.queryRow(ctx, `
		SELECT
			coalesce(sum(queryable = 1), 0),
			coalesce(sum(queryable = 0), 0)
		FROM document`).Scan(&st.Documents, &st.Quarantined)
	if err != nil {
		return store.Stats{}, fmt.Errorf("sqlitestore: stats: %w", err)
	}
	return st, nil
}

// Close closes the database. It is safe to call twice.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return s.db.Close()
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
		return doc.Document{}, fmt.Errorf("sqlitestore: decode: %w", err)
	}
	return d, nil
}

// decoded is decode with the counter, for the read paths. The migration path
// calls decode directly, because a rebuild is not a query and counting it would
// make a reopened database look like a regression.
func (s *Store) decoded(data string) (doc.Document, error) {
	s.counters.decodes.Add(1)
	return decode(data)
}

func jsonKeys(keys []string) string {
	if keys == nil {
		keys = []string{}
	}
	b, _ := json.Marshal(keys)
	return string(b)
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

// unixNano is the other direction, in UTC, so that a document read back out of
// a column compares equal to the one that went in.
func unixNano(n int64) time.Time { return time.Unix(0, n).UTC() }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
