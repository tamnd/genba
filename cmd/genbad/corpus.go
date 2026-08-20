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

	// ACL selects how permissions are decided: "tenant", "owners" or "os".
	ACL string

	// Identity names the identity source the account names in the tree belong
	// to, and is what the "os" policy writes its references under. Getting it
	// right is what lets somebody who signed in through the company directory
	// match a list that came out of a password file.
	Identity string

	// Domain is the domain the accounts on this host belong to, and is what
	// the "os" policy needs before a world readable file means anything. Empty
	// leaves the world bit granting nothing, which is the safe reading.
	Domain string

	// Refresh is how often to sync again. Zero syncs once at startup.
	Refresh time.Duration

	// Watch asks the operating system what changed instead of walking the tree
	// to find out.
	//
	// It turns the cost of a refresh from a function of how large the corpus is
	// into a function of how much of it moved, which is the difference between
	// a minute and a millisecond on a checkout of any size. A machine that
	// cannot give out that many watches gets a line in the log and a server
	// that walks, which is what it would have done anyway.
	Watch bool

	// Reconcile is how often to sweep the index against the tree. Zero sweeps
	// after every sync.
	//
	// It is a separate interval because the sweep walks. On a server that syncs
	// once a minute that hardly matters, and on one watching a large tree it is
	// the whole remaining cost, so the two want to be set apart: notice a change
	// in a second, and count both sides every quarter of an hour.
	Reconcile time.Duration
}

// The values -corpus-acl takes.
const (
	aclTenant = "tenant"
	aclOwners = "owners"
	aclOS     = "os"
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
	case aclOS:
		if o.Identity == "" {
			// Every reference the policy writes carries this name, and without
			// one they would all be compared against the bare account name. A
			// person called "alice" here would then match a person called
			// "alice" at another company.
			return fmt.Errorf("corpus acl %q needs an identity source", aclOS)
		}
	default:
		return fmt.Errorf("unknown corpus acl %q, want %q, %q or %q", o.ACL, aclTenant, aclOwners, aclOS)
	}
	if o.Refresh < 0 {
		return errors.New("corpus refresh is negative")
	}
	if o.Reconcile < 0 {
		return errors.New("corpus reconcile interval is negative")
	}
	if o.Watch && o.Refresh <= 0 {
		// A watcher records what changes between one sync and the next, and with
		// no next sync it records nothing anybody reads while holding a watch on
		// every directory in the tree.
		return errors.New("corpus watch needs a corpus refresh interval, since a watcher only saves anything across syncs")
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
//
// The os policy has the opposite thing worth saying about it. It is right for a
// tree that is the file server and wrong for a copy of one, because a tree that
// was rsynced here carries the permissions the copy has, which are this
// process's own.
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
	case aclOS:
		var domains []string
		if o.Domain != "" {
			domains = append(domains, o.Domain)
		}
		p, err := fssource.NewOSPolicy(o.Dir, o.Name, o.Identity, domains...)
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
	// A watcher that cannot be built is a line in the log and nothing else. The
	// machine is at its inotify limit, or the tree is on a filesystem the
	// backend does not support, and none of that is a reason for the server not
	// to start: a source with no watcher walks, which is what every server did
	// before there was one.
	var watcher *fssource.Watcher
	if cfg.Watch {
		watcher, err = fssource.Watch(cfg.Dir)
		if err != nil {
			log.Warn("watching the corpus, refreshes will walk the tree instead", "dir", cfg.Dir, "error", err)
		}
	}

	src, err := fssource.New(cfg.Dir, cfg.Name, policy, fssource.WithWatcher(watcher))
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

	var swept time.Time
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

		log.Info("corpus synced", append([]any{
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
			"allocated_mb", megabytes(int64(after.TotalAlloc - before.TotalAlloc)),
		}, watching(watcher)...)...)

		// The sweep walks the tree, so on a server that is watching it is the
		// whole of what a refresh costs. Running it on its own interval is what
		// lets a change be noticed in a second while both sides are still
		// counted often enough to catch what the watcher missed.
		if cfg.Reconcile <= 0 || time.Since(swept) >= cfg.Reconcile {
			swept = time.Now()
			reconcile(ctx, pipeline, src, tenant, log)
		}
		reportMapping(policy, log)
	}

	sync(ctx)

	stop := func() {
		if watcher != nil {
			_ = watcher.Close()
		}
		_ = src.Close()
	}

	if cfg.Refresh <= 0 {
		return stop, nil
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
		stop()
	}, nil
}

// watching is what the watcher has to say, for the sync log line.
//
// Walks is the number worth reading. On a healthy watcher it is one, from the
// first sync, and it does not move again. Anything more says the record could
// not be trusted that often, and the reason says which way, so an operator can
// tell "the tree is being rewritten faster than the backend will report" apart
// from "somebody edited an OWNERS file".
func watching(w *fssource.Watcher) []any {
	if w == nil {
		return nil
	}
	s := w.Stats()
	out := []any{"watches", s.Watches, "events", s.Events, "walks", s.Walks}
	if s.Reason != "" {
		out = append(out, "walking_because", s.Reason)
	}
	return out
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
