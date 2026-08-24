package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tamnd/genba/connector/fssource"
	"github.com/tamnd/genba/connector/limit"
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

	// Rate is how many files a second the read may take, and zero is as fast as
	// the disk allows.
	//
	// Zero means unlimited here, which is the opposite of what the same number
	// means for a bucket, and the difference is who is on the other end. A remote
	// service that is asked for too much revokes the token, so there the absence
	// of a ceiling is the bug. A local disk refuses nobody: the only thing a fast
	// read costs is the queries this server is answering out of the same disk
	// while it happens, and on most trees it does not cost them enough to be
	// worth slowing anything down for.
	//
	// It is worth setting on the trees where it does. A first read of a large
	// corpus is minutes of a disk at full tilt, and the whole reason the server
	// answers during it is that those minutes are meant to be usable.
	Rate float64

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
	if o.Rate < 0 {
		return errors.New("corpus rate is negative")
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
// Every sync runs in the background, including the first, so the server answers
// from the moment it says it is up and says on screen that the answers are
// partial while the first read finishes. Later syncs are incremental: the
// connector reports only what changed since the cursor the last one saved.
func ingestCorpus(ctx context.Context, st store.Store, cfg corpusOptions, tenant string, track *indexing, ops *operations, log *slog.Logger) (func(), error) {
	if cfg.Dir == "" {
		return func() {}, nil
	}
	if tenant == "" {
		// A single tenant deployment names its tenant. Without one there is
		// nothing to file these documents under, and a guess here would put a
		// corpus somewhere no query looks.
		return nil, errors.New("ingesting a corpus needs -tenant")
	}

	f, err := corpusFeed(cfg, tenant, track, ops, log)
	if err != nil {
		return nil, err
	}
	f.Managed = false
	return runFeed(ctx, st, f, log)
}

// corpusFeed builds the directory connector, ready to run.
//
// It is separate from the function above because there are two ways a
// connector arrives now. One is the command line, which is this file, and the
// other is somebody adding it from the interface, which is [supervisor]. Both
// want the same watcher, the same policy and the same feed, and the way that
// goes wrong is two copies that drift until a connector added from the screen
// behaves subtly differently from the same connector in a unit file.
func corpusFeed(cfg corpusOptions, tenant string, track *indexing, ops *operations, log *slog.Logger) (feed, error) {
	policy, err := policyFor(cfg)
	if err != nil {
		return feed{}, err
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

	opts := []fssource.Option{fssource.WithWatcher(watcher)}
	if cfg.Rate > 0 {
		// Burst of one, so the number is a pace rather than a ceiling averaged
		// over a window. A burst is worth having against a service that counts
		// requests per minute and does not care how they were spaced inside it,
		// and it is worth nothing against a disk, where ten reads back to back is
		// exactly the spike the rate was set to avoid.
		opts = append(opts, fssource.WithPace(limit.NewLimiter(limit.Limits{Rate: cfg.Rate, Burst: 1}, nil).Wait))
	}

	src, err := fssource.New(cfg.Dir, cfg.Name, policy, opts...)
	if err != nil {
		if watcher != nil {
			_ = watcher.Close()
		}
		return feed{}, err
	}

	return feed{
		Kind:      "corpus",
		Source:    src,
		Target:    cfg.Dir,
		Tenant:    tenant,
		Refresh:   cfg.Refresh,
		Reconcile: cfg.Reconcile,
		Fields:    []any{"dir", cfg.Dir, "source", cfg.Name},
		Report:    func() []any { return watching(watcher) },
		Policy:    policy,
		Track:     track,
		Ops:       ops,
		Release: func() {
			if watcher != nil {
				_ = watcher.Close()
			}
			_ = src.Close()
		},
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
