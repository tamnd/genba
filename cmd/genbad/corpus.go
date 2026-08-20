package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/aclmap"
	"github.com/tamnd/genba/connector/fssource"
	"github.com/tamnd/genba/ingest"
	"github.com/tamnd/genba/store"
)

// corpusOptions is what the -corpus flags add up to.
//
// They exist so that a first run is one command with a directory in it. A
// server with nothing in it is difficult to judge, and pointing it at a
// checkout somebody already has is the shortest path from downloading the
// binary to typing a query and getting back something they recognise.
type corpusOptions struct {
	// Dir is the directory to read. Empty means do not ingest.
	Dir string

	// Name is the source name the documents carry, and what a query filters on.
	Name string

	// ACL selects how permissions are decided, either "tenant" or "owners".
	ACL string

	// Refresh is how often to sync again. Zero syncs once at startup.
	Refresh time.Duration
}

// The values -corpus-acl takes.
const (
	aclTenant = "tenant"
	aclOwners = "owners"
)

func (o corpusOptions) validate() error {
	if o.Dir == "" {
		return nil
	}
	if o.Name == "" {
		return errors.New("corpus name is empty")
	}
	switch o.ACL {
	case aclTenant, aclOwners:
	default:
		return fmt.Errorf("unknown corpus acl %q, want %q or %q", o.ACL, aclTenant, aclOwners)
	}
	if o.Refresh < 0 {
		return errors.New("corpus refresh is negative")
	}
	return nil
}

// policyFor builds the permission policy named by the flags.
//
// The owners policy deliberately has no fallback. A path in the tree that no
// OWNERS file governs has no answer about who may read it, and the pipeline
// quarantines it, which shows up in the log and in the stats. Giving it a
// fallback here would turn "nobody has said" into "everybody may", which is the
// one substitution this system is built to avoid.
func policyFor(o corpusOptions) (fssource.Policy, error) {
	switch o.ACL {
	case aclTenant:
		return fssource.PublicToTenant(o.Name), nil
	case aclOwners:
		p, err := fssource.NewOwnersPolicy(o.Dir, o.Name, "github")
		if err != nil {
			return nil, err
		}
		return p, nil
	default:
		return nil, fmt.Errorf("unknown corpus acl %q", o.ACL)
	}
}

// ingestCorpus syncs the configured directory into the store.
//
// The first sync runs before the server listens, so that a request arriving the
// moment the log says the server is up finds a corpus rather than an empty
// index. Later syncs run in the background on the refresh interval, and are
// incremental: the connector reports only what changed since the cursor the
// last one saved.
func ingestCorpus(ctx context.Context, st store.Store, cfg corpusOptions, tenant string, log *slog.Logger) (func(), error) {
	if cfg.Dir == "" {
		return func() {}, nil
	}
	if tenant == "" {
		// A single tenant deployment names its tenant. Without one there is
		// nothing to file these documents under, and a guess here would put a
		// corpus somewhere no query looks.
		return nil, errors.New("ingesting a corpus needs -tenant")
	}

	policy, err := policyFor(cfg)
	if err != nil {
		return nil, err
	}
	src, err := fssource.New(cfg.Dir, cfg.Name, policy)
	if err != nil {
		return nil, err
	}

	// Checkpoints live in memory because the only storage driver that ships
	// today does too. Restarting loses the index, so a cursor that survived the
	// restart would skip everything the new empty index needs.
	pipeline, err := ingest.New(st, connector.NewMemoryCheckpoints(), ingest.WithLogger(log))
	if err != nil {
		return nil, err
	}

	sync := func(ctx context.Context) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		stats, err := pipeline.Run(ctx, genba.TenantID(tenant), src)
		if err != nil {
			log.Error("corpus sync failed", "dir", cfg.Dir, "error", err,
				"indexed", stats.Indexed, "quarantined", stats.Quarantined)
			return
		}
		runtime.ReadMemStats(&after)

		log.Info("corpus synced",
			"dir", cfg.Dir,
			"source", cfg.Name,
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

		reconcile(ctx, pipeline, src, tenant, log)
		reportMapping(policy, log)
	}

	sync(ctx)

	if cfg.Refresh <= 0 {
		return func() { _ = src.Close() }, nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(cfg.Refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sync(ctx)
			}
		}
	}()
	return func() {
		<-done
		_ = src.Close()
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
func reportMapping(policy fssource.Policy, log *slog.Logger) {
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

// reconcile sweeps the index against the tree and repairs what the sync could
// not have seen.
//
// It runs after every sync rather than on a schedule of its own, and the reason
// is that a directory tree has no change feed. A sync walks the tree and emits
// what is newer than the cursor, which means a file somebody deleted produces
// no event at all: there is nothing left to walk past. Without this the deleted
// document stays in the index and keeps being served, and the only thing that
// would ever remove it is a restart.
//
// The cost is a second walk of the same tree, stat only, plus a two column scan
// of the index. On a corpus where the sync itself is already a stat per file
// that is roughly double the cheap half of the work and none of the expensive
// half, which is the right trade for the alternative being an index that serves
// documents that no longer exist.
func reconcile(ctx context.Context, pipeline *ingest.Pipeline, src connector.Connector, tenant string, log *slog.Logger) {
	rec, err := pipeline.Reconcile(ctx, genba.TenantID(tenant), src)
	if err != nil {
		// Not fatal, and not even unusual. A store that cannot list what it holds
		// or a connector that cannot enumerate simply has no sweep, and the sync
		// that just finished is still good.
		log.Warn("corpus reconciliation skipped", "source", src.Source(), "error", err)
		return
	}
	if rec.Drift() == 0 {
		return
	}
	// Logged at warning level because drift is the interesting event. An index
	// that agrees with its source produces nothing here, so a line in the log
	// means the incremental path missed something and is worth going to look at.
	log.Warn("corpus reconciled",
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
