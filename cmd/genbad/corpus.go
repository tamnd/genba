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
