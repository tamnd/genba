package main

import (
	"context"
	"log/slog"
	"runtime"
	"slices"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/aclmap"
	"github.com/tamnd/genba/ingest"
	"github.com/tamnd/genba/store"
)

// feed is one connector wired to run on a schedule.
//
// The two sources this binary can be pointed at, a directory and a bucket,
// differ in how they are built and in nothing at all about how they are run.
// Both sync once before the listener opens, both sync again on an interval,
// both sweep the index against the source afterwards, and both want the same
// numbers in the log. Keeping that in one place is what stops the third one
// from being a third copy of the same loop with one of the numbers missing.
type feed struct {
	// Kind is the word the log lines start with, so that a server reading two
	// sources says which one it is talking about.
	Kind string

	// Source is the connector to sync.
	Source connector.Connector

	// Target is which directory or bucket this feed reads, for the
	// administration screen. Two directories are two rows a person can tell
	// apart only if each says which one it is.
	Target string

	// Tenant is who the documents belong to.
	Tenant string

	// Refresh is how often to sync again. Zero syncs once at startup.
	Refresh time.Duration

	// Reconcile is how often to sweep the index against the source. Zero sweeps
	// after every sync.
	Reconcile time.Duration

	// Managed says this feed was configured through the interface rather than
	// on the command line, which is what decides whether it can be changed from
	// there. See [supervisor].
	Managed bool

	// Trigger asks for a sync now rather than at the next interval. A nil one
	// is a feed nothing can hurry, which is every feed that came from a flag:
	// there is nothing on the command line to press.
	Trigger <-chan struct{}

	// Fields say where the documents came from and are on every line this feed
	// logs.
	Fields []any

	// Report is asked after every sync for whatever the source has to say about
	// itself, which is where a watcher's counters and a client's request counts
	// come from. A nil Report adds nothing.
	Report func() []any

	// Policy is the permission policy, for the mapping report. It is an any
	// because the two connectors have unrelated policy interfaces and what is
	// wanted here is the one method some of their implementations share.
	Policy any

	// Track is where the first read of this source is reported, so that the
	// interface can say the answers it is giving are not the whole answer yet.
	// A nil one is a feed nobody is watching.
	Track *indexing

	// Ops is where each sync is recorded for the administration screen. It is
	// the same arrangement as Track and it is separate from it because the two
	// answer different questions: Track is how far the first crawl has got, and
	// this is what every run since then did and which of them failed. A nil one
	// records nothing.
	Ops *operations

	// Release is called once the last sync has returned, and is where a watcher
	// and a source get closed.
	Release func()
}

// runFeed syncs f once and then on its interval, and returns the function that
// waits for it to stop.
//
// The first sync runs in the background, alongside the listener, rather than in
// front of it. Reading a real corpus takes a minute and a half, and a process
// that refuses connections for that long is one a load balancer takes out of
// rotation and a person watching a terminal assumes has hung. It answers from
// the first second instead, and says on every screen that what it is answering
// from is not all of it yet, which is what [indexing] is for.
func runFeed(ctx context.Context, st store.Store, f feed, log *slog.Logger) (func(), error) {
	// Checkpoints live in memory because the only storage driver that ships
	// today does too. Restarting loses the index, so a cursor that survived the
	// restart would skip everything the new empty index needs.
	opts := []ingest.Option{ingest.WithLogger(log)}
	if f.Track != nil {
		opts = append(opts, ingest.WithProgress(func(p ingest.Progress) {
			f.Track.advance(p.Source, p.Done)
		}))
	}
	pipeline, err := ingest.New(st, connector.NewMemoryCheckpoints(), opts...)
	if err != nil {
		return nil, err
	}

	// Said here rather than in the goroutine, so that the answer to whether this
	// process is still reading its sources is already correct by the time the
	// listener opens. A readiness check that arrives in the microsecond between
	// the two would otherwise be told the crawl is over because it has not
	// started.
	source := f.Source.Source()
	tracked := f.Track != nil && f.Track.expect(source)
	if f.Ops != nil {
		// Registered before the first sync, so a screen opened during the first
		// crawl of a large corpus shows the connector with no runs yet rather
		// than showing nothing at all.
		f.Ops.register(api.Connector{
			Source:  source,
			Kind:    f.Kind,
			Target:  f.Target,
			Tenant:  f.Tenant,
			Refresh: refreshOf(f.Refresh),
			Enabled: true,
			Managed: f.Managed,
		}, f.Policy)
	}

	var swept time.Time
	sync := func(ctx context.Context) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		started := time.Now()
		if f.Ops != nil {
			f.Ops.starting(source, started)
		}
		stats, err := pipeline.Run(ctx, genba.TenantID(f.Tenant), f.Source)
		if f.Ops != nil {
			f.Ops.finished(source, time.Since(started), stats, err)
		}
		if err != nil {
			log.Error(f.Kind+" sync failed", append(slices.Clone(f.Fields),
				"error", err,
				"indexed", stats.Indexed,
				"quarantined", stats.Quarantined,
			)...)
			return
		}
		runtime.ReadMemStats(&after)

		fields := append(slices.Clone(f.Fields),
			"indexed", stats.Indexed,
			"quarantined", stats.Quarantined,
			"bytes", stats.Bytes,
			"duration", stats.Duration.Round(time.Millisecond),
			"docs_per_second", int(stats.Rate()),
			"mb_per_second", throughput(stats.Bytes, stats.Duration),
			// What the corpus costs to hold, and what it cost to build. The
			// first is the number that decides how much a machine can serve,
			// and the second is the one that shows up as garbage collection.
			"heap_mb", megabytes(int64(after.HeapAlloc)),
			"allocated_mb", megabytes(int64(after.TotalAlloc-before.TotalAlloc)),
		)
		if f.Report != nil {
			fields = append(fields, f.Report()...)
		}
		log.Info(f.Kind+" synced", fields...)

		// The sweep walks the source, which on a watched directory is the whole
		// of what a refresh costs and on a bucket is a request per thousand
		// objects that the sync just made as well. Running it on its own
		// interval is what lets a change be noticed in a second while both
		// sides are still counted often enough to catch what the sync missed.
		if f.Reconcile <= 0 || time.Since(swept) >= f.Reconcile {
			swept = time.Now()
			reconcile(ctx, pipeline, f.Source, f.Tenant, log)
		}
		reportMapping(f.Policy, log)
	}

	release := f.Release
	if release == nil {
		release = func() {}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if tracked {
			// Counted before the sync rather than during it, because the whole
			// point of the second number is that it is there from the start. It
			// is one metadata pass over a source that is about to get a full
			// content pass, and it happens once per process and only on a process
			// that came up with nothing indexed.
			total := countSource(ctx, f.Source)
			log.Info(f.Kind+" first read", append(slices.Clone(f.Fields), "documents", total)...)
			f.Track.counting(source, total)
		}
		sync(ctx)
		if tracked {
			f.Track.finished(source)
		}
		// A nil channel blocks forever in a select, which is what gives a feed
		// with no interval and nothing to hurry it the same shape as one with
		// both, instead of an early return that would have to be undone the
		// moment either of them exists.
		var tick <-chan time.Time
		if f.Refresh > 0 {
			t := time.NewTicker(f.Refresh)
			defer t.Stop()
			tick = t.C
		}
		if tick == nil && f.Trigger == nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
				sync(ctx)
			case <-f.Trigger:
				sync(ctx)
			}
		}
	}()
	return func() {
		<-done
		release()
	}, nil
}

// aclCounter is a permission policy that counts what it mapped.
type aclCounter interface {
	Counts() aclmap.Counts
}

// reportMapping says what the permission mapping could not represent.
//
// A document held back because its source said something the model cannot carry
// is invisible from every other angle. It is not an error, the sync succeeded,
// and the only symptom is somebody who cannot find a document they know exists.
// So it is counted by reason, and the reasons want different actions: a foreign
// domain is a decision about the tenant, an unmappable deny is usually a source
// feature nobody has written the mapping for, and a malformed grant is a bug in
// a connector.
func reportMapping(policy any, log *slog.Logger) {
	counter, ok := policy.(aclCounter)
	if !ok {
		return
	}
	c := counter.Counts()
	if c.Quarantined() == 0 {
		return
	}
	log.Warn("permissions that could not be mapped",
		"mapped", c.Mapped,
		"quarantined", c.Quarantined(),
		"foreign_domain", c.ForeignDomain,
		"unmappable_deny", c.UnmappableDeny,
		"malformed", c.Malformed,
		"ignored_roles", c.Ignored,
	)
}

// reconcile sweeps the index against the source and repairs what the sync could
// not have seen.
//
// A sync emits what is newer than the cursor, which means a document somebody
// deleted usually produces no event at all: there is nothing left to walk past
// or to list. Without this the deleted document stays in the index and keeps
// being served, and the only thing that would ever remove it is a restart.
//
// The cost is a second pass over the source, metadata only, plus a two column
// scan of the index. On a corpus where the sync itself is already a stat per
// file that is roughly double the cheap half of the work and none of the
// expensive half, which is the right trade for the alternative being an index
// that serves documents that no longer exist.
func reconcile(ctx context.Context, pipeline *ingest.Pipeline, src connector.Connector, tenant string, log *slog.Logger) {
	rec, err := pipeline.Reconcile(ctx, genba.TenantID(tenant), src)
	if err != nil {
		// Not fatal, and not even unusual. A store that cannot list what it holds
		// or a connector that cannot enumerate simply has no sweep, and the sync
		// that just finished is still good.
		log.Warn("reconciliation skipped", "source", src.Source(), "error", err)
		return
	}
	if rec.Drift() == 0 {
		return
	}
	// Logged at warning level because drift is the interesting event. An index
	// that agrees with its source produces nothing here, so a line in the log
	// means the incremental path missed something and is worth going to look at.
	log.Warn("reconciled",
		"source", rec.Source,
		"source_items", rec.SourceItems,
		"index_items", rec.IndexItems,
		"missing", rec.Missing.Count,
		"stale", rec.Stale.Count,
		"extra", rec.Extra.Count,
		"repaired", rec.Repaired,
		"deleted", rec.Deleted,
		"sample_missing", rec.Missing.IDs,
		"sample_stale", rec.Stale.IDs,
		"sample_extra", rec.Extra.IDs,
		"requests", rec.Requests.Requests(),
		"duration", rec.Duration.Round(time.Millisecond),
	)
}

// megabytes converts a byte count for the log.
func megabytes(n int64) float64 {
	return float64(n) / (1 << 20)
}

// throughput is megabytes a second, and zero for a run too short for the clock
// to have moved, which would otherwise divide by zero and log an infinity.
func throughput(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return megabytes(bytes) / d.Seconds()
}
