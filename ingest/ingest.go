// Package ingest runs connectors and puts what they produce into a store.
//
// It is the only place a document crosses from a source system into something a
// query can reach, which makes it the right place for the rules that must hold
// for every source: a tenant on everything, permissions that resolved or a
// quarantine, batching so the store is not asked one document at a time, and a
// checkpoint written only after the documents it covers are durably stored.
//
// # Backpressure
//
// There is no queue between the connector and the store. A connector hands over
// a change by calling into this package, and that call does the batching and
// the storing, so a source that produces faster than the store can absorb is
// slowed by the handover itself. That is worth more than a buffered channel and
// a tuning knob: a queue that can grow is a queue that will, and the failure
// shows up as memory rather than as latency, which is much harder to read.
//
// # Ordering
//
// A batch is stored, and only then is the checkpoint for its last change saved.
// A crash in between replays the batch, which is safe because a put of the same
// document twice is the same as once. The other order would skip documents, and
// nothing downstream would ever notice they were missing.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// DefaultBatchSize is how many documents are held before a store write.
//
// It is a compromise. Larger batches amortise the write and are worth real
// throughput, and they also widen the window a crash replays and raise the
// memory a run holds. A few hundred is where those stop trading against each
// other usefully for the drivers here.
const DefaultBatchSize = 500

// Pipeline moves documents from a connector into a store.
//
// The zero value is not usable. Use [New].
type Pipeline struct {
	store       store.Store
	checkpoints connector.Checkpoints
	batchSize   int
	clock       func() time.Time
	log         *slog.Logger
	progress    func(Progress)

	// maint is the same store when the driver can do the two things a
	// maintenance job needs and a query path must never have: list what is
	// stored, and rewrite an access control list without the document. It is
	// nil for a driver without them, and the two operations that need it say so
	// rather than silently doing nothing.
	maint store.Maintenance
}

// Option configures a pipeline.
type Option func(*Pipeline)

// WithBatchSize sets how many documents are buffered before a store write. A
// value below one selects [DefaultBatchSize].
func WithBatchSize(n int) Option {
	return func(p *Pipeline) {
		if n > 0 {
			p.batchSize = n
		}
	}
}

// WithLogger sets where the pipeline reports progress and quarantines.
func WithLogger(l *slog.Logger) Option {
	return func(p *Pipeline) {
		if l != nil {
			p.log = l
		}
	}
}

// WithProgress sets what to tell about a run while it is still going.
//
// It is called once when the run starts and again after every store write, on
// the goroutine doing the work, so it must not block: whatever it does is time
// the sync is not spending on documents. Handing over a snapshot rather than a
// pointer is deliberate, since the caller is usually publishing it to a reader
// on another goroutine.
//
// The alternative was to have the caller read the returned [Stats], and that
// only ever arrives after the run is over, which is exactly when nobody needs
// to be told how far it has got.
func WithProgress(fn func(Progress)) Option {
	return func(p *Pipeline) {
		if fn != nil {
			p.progress = fn
		}
	}
}

// WithClock replaces the source of the indexing timestamp, for tests that need
// a run to be reproducible.
func WithClock(now func() time.Time) Option {
	return func(p *Pipeline) {
		if now != nil {
			p.clock = now
		}
	}
}

// New returns a pipeline writing into s and checkpointing into cp.
//
// A nil checkpoint store is allowed and means every run is a full sync. That is
// the right default for a one shot import and the wrong one for anything that
// runs on a schedule.
func New(s store.Store, cp connector.Checkpoints, opts ...Option) (*Pipeline, error) {
	if s == nil {
		return nil, errors.New("ingest: nil store")
	}
	p := &Pipeline{
		store:       s,
		checkpoints: cp,
		batchSize:   DefaultBatchSize,
		clock:       time.Now,
		log:         slog.New(discardHandler{}),
	}
	for _, opt := range opts {
		opt(p)
	}
	p.maint, _ = s.(store.Maintenance)
	return p, nil
}

// Progress is how far a run has got, reported while it is still running.
//
// It is a subset of [Stats] rather than the whole of it, because the numbers
// left out are ones that are not meaningful until a run is over and would
// invite somebody to draw a conclusion from a half finished one.
type Progress struct {
	// Source is the connector being synced.
	Source string

	// Tenant is whose corpus it is going into.
	Tenant string

	// Done is how many documents have been stored so far, whether they are
	// queryable or quarantined. Both are documents this run has got through, and
	// somebody watching a count climb is asking how far along it is rather than
	// how many of them they will be allowed to read.
	Done int

	// Resumed says the run started from a checkpoint, so the index already held
	// documents from this source before it began.
	//
	// It is the difference between a corpus being read for the first time and
	// one being caught up, and only the first of those means the answers on
	// screen are incomplete.
	Resumed bool
}

// Stats is what one run did.
type Stats struct {
	// Indexed is the number of documents stored and reachable by a query.
	Indexed int

	// Quarantined is the number stored but held out of every query path
	// because their permissions did not resolve. They are counted rather than
	// dropped so that an operator can see a connector that is failing to
	// resolve access control lists, which is otherwise silent.
	Quarantined int

	// Deleted is the number removed because the source no longer has them.
	Deleted int

	// Repermissioned is the number whose access control list was rewritten
	// without their content being fetched.
	//
	// It is the number that says whether the cheap path for a permission change
	// is working. A run that reindexed ten thousand documents because somebody
	// edited one group looks fine in Indexed and is a recrawl.
	Repermissioned int

	// Skipped is the number rejected before the store, because they carried no
	// id and there is nothing to file them under.
	Skipped int

	// Batches is how many store writes the run made.
	Batches int

	// Bytes is the total size of the document bodies seen, which is the number
	// a throughput figure should be quoted against. Document counts vary by
	// three orders of magnitude between corpora and say much less.
	Bytes int64

	// Duration is the wall clock time of the run.
	Duration time.Duration

	// Cursor is where the run got to, and what a later run resumes from.
	Cursor connector.Cursor
}

// Rate returns documents per second, or zero for a run too short to measure.
func (s Stats) Rate() float64 {
	if s.Duration <= 0 {
		return 0
	}
	return float64(s.Indexed+s.Quarantined) / s.Duration.Seconds()
}

// Run syncs one connector into the store for one tenant.
//
// It resumes from the stored checkpoint unless there is none. The returned
// stats describe the run whether or not it returned an error, so a caller that
// was interrupted can still report how far it got.
func (p *Pipeline) Run(ctx context.Context, tenant genba.TenantID, c connector.Connector) (Stats, error) {
	if c == nil {
		return Stats{}, errors.New("ingest: nil connector")
	}
	if tenant == "" {
		// Every content path in this system is scoped by tenant. A document
		// without one cannot be served, so accepting it here would only move
		// the failure somewhere less obvious.
		return Stats{}, errors.New("ingest: empty tenant")
	}

	source := c.Source()
	start := p.clock()

	from, err := p.loadCursor(ctx, string(tenant), source)
	if err != nil {
		return Stats{}, err
	}

	run := &run{pipeline: p, tenant: string(tenant), source: source, resumed: !from.IsZero()}
	run.stats.Cursor = from
	// Said before a single document has been read, so that whoever is watching
	// learns a run has started rather than learning it half a batch later. On a
	// source with nothing new in it this is the only report there will be.
	run.report()

	final, syncErr := c.Sync(ctx, from, run.emit)

	// Whatever happened, flush what is already in hand. A connector that failed
	// halfway still produced real documents, and throwing them away only means
	// fetching them again.
	flushErr := run.flush(ctx)

	run.stats.Duration = p.clock().Sub(start)

	if syncErr != nil {
		return run.stats, fmt.Errorf("ingest: sync %s: %w", source, syncErr)
	}
	if flushErr != nil {
		return run.stats, flushErr
	}

	// The connector's own end of walk cursor wins over the last change's, since
	// it can know the walk finished at a point no change happens to sit on.
	if !final.IsZero() {
		if err := p.saveCursor(ctx, string(tenant), source, final); err != nil {
			return run.stats, err
		}
		run.stats.Cursor = final
	}

	p.log.Info("sync finished",
		"source", source,
		"tenant", string(tenant),
		"indexed", run.stats.Indexed,
		"quarantined", run.stats.Quarantined,
		"deleted", run.stats.Deleted,
		"duration", run.stats.Duration,
	)
	return run.stats, nil
}

func (p *Pipeline) loadCursor(ctx context.Context, tenant, source string) (connector.Cursor, error) {
	if p.checkpoints == nil {
		return connector.Cursor{}, nil
	}
	from, err := p.checkpoints.Load(ctx, tenant, source)
	if err != nil {
		return connector.Cursor{}, fmt.Errorf("ingest: load checkpoint for %s: %w", source, err)
	}
	return from, nil
}

func (p *Pipeline) saveCursor(ctx context.Context, tenant, source string, c connector.Cursor) error {
	if p.checkpoints == nil || c.IsZero() {
		return nil
	}
	if err := p.checkpoints.Save(ctx, tenant, source, c); err != nil {
		return fmt.Errorf("ingest: save checkpoint for %s: %w", source, err)
	}
	return nil
}

// run is the mutable state of one sync. It is split out so that Run stays
// readable and so that emit can be a method value rather than a closure over
// half a dozen variables.
type run struct {
	pipeline *Pipeline
	tenant   string
	source   string

	batch   []doc.Document
	deletes []string
	// perms holds the permission changes in the current batch, keyed by id so
	// that two edits to the same document in one run cost one write.
	perms map[string]acl.Permissions
	// pending is the resume point of the last change in the current batch. It
	// is saved only once that batch is stored.
	pending connector.Cursor
	stats   Stats

	// resumed says this run started from a checkpoint rather than from nothing.
	resumed bool
}

// report tells the pipeline's progress function how far this run has got.
func (r *run) report() {
	if r.pipeline.progress == nil {
		return
	}
	r.pipeline.progress(Progress{
		Source:  r.source,
		Tenant:  r.tenant,
		Done:    r.stats.Indexed + r.stats.Quarantined,
		Resumed: r.resumed,
	})
}

// emit takes one change from a connector.
func (r *run) emit(ctx context.Context, ch connector.Change) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := r.pipeline

	d := ch.Document
	// The tenant and the source are set here rather than trusted from the
	// connector. A connector that gets either wrong writes into another
	// tenant's corpus, and that is not a mistake worth leaving reachable.
	d.Tenant = r.tenant
	d.Source = r.source

	if !ch.Cursor.IsZero() {
		r.pending = ch.Cursor
	}

	if d.ID == "" {
		r.stats.Skipped++
		p.log.Warn("change has no id, skipped", "source", r.source)
		return r.maybeFlush(ctx)
	}

	if ch.Deleted {
		r.deletes = append(r.deletes, d.ID)
		r.stats.Deleted++
		return r.maybeFlush(ctx)
	}

	if ch.PermissionsOnly {
		if p.maint == nil {
			// Refusing is the only honest answer. The change carries no content,
			// so there is nothing to put, and treating it as a no operation would
			// leave a revoked document readable while the log said the sync
			// succeeded.
			return fmt.Errorf("ingest: %s reported a permission change for %s and this store cannot apply one without the document", r.source, d.ID)
		}
		if r.perms == nil {
			r.perms = make(map[string]acl.Permissions, p.batchSize)
		}
		r.perms[d.ID] = d.Permissions
		return r.maybeFlush(ctx)
	}

	d.IndexedAt = p.clock()
	r.stats.Bytes += int64(len(d.Body))

	if d.Queryable() {
		r.stats.Indexed++
	} else {
		// It is still stored. A quarantined document is one an operator has to
		// be able to find and fix, and a document that was silently dropped is
		// one nobody knows to look for. The store keeps it out of every query
		// path because Queryable is false, which is the same gate every driver
		// applies.
		r.stats.Quarantined++
		p.log.Warn("document quarantined, permissions did not resolve",
			"source", r.source, "id", d.ID, "container", d.Container)
	}

	r.batch = append(r.batch, d)
	return r.maybeFlush(ctx)
}

func (r *run) maybeFlush(ctx context.Context) error {
	if len(r.batch)+len(r.deletes)+len(r.perms) < r.pipeline.batchSize {
		return nil
	}
	return r.flush(ctx)
}

// flush writes the batch and then saves the checkpoint it covers.
//
// The order matters and is the reason this is one function rather than two
// calls at the call site. Storing first and checkpointing second means a crash
// between them replays documents, which is harmless. The other order loses
// them, which is not.
func (r *run) flush(ctx context.Context) error {
	if len(r.batch) == 0 && len(r.deletes) == 0 && len(r.perms) == 0 {
		return nil
	}
	p := r.pipeline

	if len(r.batch) > 0 {
		if err := p.store.Put(ctx, r.batch...); err != nil {
			return fmt.Errorf("ingest: put %d documents from %s: %w", len(r.batch), r.source, err)
		}
	}
	if len(r.deletes) > 0 {
		if err := p.store.Delete(ctx, r.deletes...); err != nil {
			return fmt.Errorf("ingest: delete %d documents from %s: %w", len(r.deletes), r.source, err)
		}
	}
	if len(r.perms) > 0 {
		n, err := p.maint.SetPermissions(ctx, r.tenant, r.perms)
		if err != nil {
			return fmt.Errorf("ingest: set permissions on %d documents from %s: %w", len(r.perms), r.source, err)
		}
		r.stats.Repermissioned += n
		// The difference is documents the source has an access control list for
		// and the index does not hold. That is drift, and it is the kind the
		// incremental path cannot fix on its own, so it is worth saying out loud
		// rather than leaving as a gap between two counters.
		if missing := len(r.perms) - n; missing > 0 {
			p.log.Warn("permission change for documents that are not indexed",
				"source", r.source, "tenant", r.tenant, "count", missing)
		}
	}
	r.stats.Batches++
	// Reported here rather than per document, because a batch is the unit that
	// actually became visible to a query and because a callback on every
	// document would be a lock per document on the path this whole package is
	// built to keep cheap.
	r.report()

	// Reuse the backing arrays. A long sync flushes thousands of times and
	// there is no reason for each one to allocate a new batch.
	r.batch = r.batch[:0]
	r.deletes = r.deletes[:0]
	clear(r.perms)

	if err := p.saveCursor(ctx, r.tenant, r.source, r.pending); err != nil {
		return err
	}
	if !r.pending.IsZero() {
		r.stats.Cursor = r.pending
	}
	return nil
}

// discardHandler is a slog handler that drops everything, so that a pipeline
// built without a logger costs nothing rather than writing to standard error.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }
