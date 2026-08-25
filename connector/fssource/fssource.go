// Package fssource is a connector that reads a directory tree.
//
// It is the reference connector, the one the interface was written against and
// the one to read first when writing another. It also does real work: pointed
// at a checkout of a documentation repository it produces a corpus with real
// titles, real bodies, real modification times and, with the right policy, real
// access control lists.
//
// # Permissions come from a policy, not from the walk
//
// The connector maps files to documents. It does not decide who may read them.
// That split exists because a directory tree says almost nothing about access
// on its own: the mode bits describe the account the crawler is running as, not
// the people in the company. Somewhere above the filesystem there is a real
// answer, in an OWNERS file, in a group export or in a share database, and a
// [Policy] is where that answer is plugged in.
//
// There is no permissive default. A source built without a policy quarantines
// everything, which is loud and safe, rather than publishing a directory tree
// to everybody, which is quiet and not.
//
// # Incremental sync
//
// The cursor is the highest modification time the last run saw, held back to a
// second before that run started. A later run walks the same tree and skips
// anything not newer, so the cost of a no change sync is a stat of every file
// rather than a read of every file. That is the honest limit of what a plain
// filesystem supports on its own: without a change feed there is no way to
// avoid the walk, only the read.
//
// The second is not slack in the design, it is the design. A file is stamped
// from a clock that ticks rather than from the one that runs, so a file created
// a moment after the walk went past its directory carries the same modification
// time as one the walk read, and a cursor sitting on that time would leave it
// behind for good. The cost of holding back is that a file written in the
// second before a sync is read by the next sync as well, which is a document
// arriving twice rather than a document not arriving at all.
//
// A [Watcher] is how the walk goes too. The operating system already knows
// which files were written, and a source built with [WithWatcher] asks it
// instead of asking the tree, which turns the cost of a sync from a function of
// how large the corpus is into a function of how much of it moved. A watcher
// that cannot vouch for what it recorded says so, and that sync walks, so the
// worst a watcher can do is cost what not having one costs.
package fssource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // registers the GIF config decoder
	_ "image/jpeg" // registers the JPEG config decoder
	_ "image/png"  // registers the PNG config decoder
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/extract"
)

// DefaultMaxFileSize is the largest file read into a document body.
//
// Past this the file is almost never prose somebody wants to search, and it is
// often a checked in binary that would cost far more to index than it is worth.
const DefaultMaxFileSize = 1 << 20

// DefaultMaxImageSize is the largest image read into a document's content.
//
// It is separate from the body limit and larger, because the two limits are
// about different things. A one megabyte text file is almost certainly not
// prose. A one megabyte screenshot is a screenshot.
const DefaultMaxImageSize = 4 << 20

// DefaultMaxDocumentSize is the largest PDF or Office file read for extraction.
//
// It is the third limit for the same reason there is a second one. A one
// megabyte text file is almost certainly not prose and a one megabyte
// screenshot is a screenshot, and an eight megabyte PDF is an ordinary report
// with a chart in it. The bytes here are compressed and the text inside is a
// fraction of them, so the number that actually bounds the work is
// [extract.Options.MaxDecompressed] rather than this one.
const DefaultMaxDocumentSize = 16 << 20

// Policy decides who may read a file.
//
// It is called once per file with the path relative to the root, using forward
// slashes on every platform so that a policy written against a repository
// layout does not have to care where it is running.
//
// Returning an error quarantines that one document and does not stop the walk,
// because one unreadable share should not cost a whole sync.
type Policy interface {
	Permissions(ctx context.Context, relPath string) (acl.Permissions, error)
}

// PolicyFunc adapts a function to [Policy].
type PolicyFunc func(ctx context.Context, relPath string) (acl.Permissions, error)

// Permissions calls f.
func (f PolicyFunc) Permissions(ctx context.Context, relPath string) (acl.Permissions, error) {
	return f(ctx, relPath)
}

// PublicToTenant is a policy where every file is readable by everybody in the
// tenant.
//
// It is correct for a public documentation corpus and wrong for almost
// everything else, so it has to be asked for by name.
func PublicToTenant(source string) Policy {
	return PolicyFunc(func(context.Context, string) (acl.Permissions, error) {
		return acl.Permissions{Mode: acl.ModePublicToTenant, Source: source}, nil
	})
}

// Versioned is the optional capability of a [Policy] that can say when its
// answer for a path last changed.
//
// It is what turns a permission change into a write instead of a recrawl. A
// sync that only compares modification times cannot see one at all: the file
// did not change, the rule above it did. A policy that implements this is asked
// about every file the walk decided to skip, and the ones whose rule moved get
// a permissions only change.
//
// The answer has to be cheap. It is asked once per unchanged file, which on a
// large tree is once per file, so anything that costs a read has to be cached
// for the length of the walk.
type Versioned interface {
	// ChangedAt returns when the rule governing relPath last changed. A zero
	// time means the policy has no rule for it, or has no idea, and nothing is
	// refreshed on the strength of it.
	ChangedAt(ctx context.Context, relPath string) (time.Time, error)
}

// statPolicy and statVersioned are the same two questions asked with the file
// information the walk is already holding.
//
// They are unexported on purpose, so they are not a second interface anybody
// outside has to implement. What they are is a way for a policy that reads the
// file system itself to avoid a second stat of every file in the tree, which on
// a corpus of a million files is a million system calls a sync spends finding
// out something it was told a moment ago.
type (
	statPolicy interface {
		permissionsFor(ctx context.Context, relPath string, info fs.FileInfo) (acl.Permissions, error)
	}
	statVersioned interface {
		changedAtFor(ctx context.Context, relPath string, info fs.FileInfo) (time.Time, error)
	}
)

// Ruled is the optional capability of a [Policy] whose answers come out of
// files in the tree it is being asked about.
//
// It exists for the watched sync and for nothing else. A watcher reports the
// file that changed, and an OWNERS file that changed governs who may read every
// document in the subtree below it, none of which the watcher will ever
// mention. A sync that read only the reported paths would apply the edit to the
// OWNERS file itself and to nothing it rules.
//
// So a policy says which paths are its rules, and a sync that finds one of them
// among the changes walks the tree that round instead. That is the expensive
// answer, and it is the right one: an OWNERS edit is rare, and getting it wrong
// means a revocation that silently did not happen.
//
// [OSPolicy] needs none of this. Its rule for a file is the file's own mode,
// and changing that raises an event on the file itself.
type Ruled interface {
	// IsRule reports whether a path, relative to the root and separated by
	// forward slashes, is one of the policy's rule files.
	IsRule(relPath string) bool
}

// Reloader is the optional capability of a [Policy] that caches.
//
// A source calls it once at the start of every walk. That is the contract the
// caching in a policy is allowed to assume: answers may be held for the length
// of one sync and must not be held across two, because the thing a later sync
// exists to notice is exactly the edit that would invalidate them.
type Reloader interface {
	Reload()
}

// counters is what a source spent on the filesystem.
//
// The fields are atomic because [connector.Counted] promises a reading can be
// taken while a sync is running, which is when an operator most wants one.
type counters struct {
	lists    atomic.Int64
	metadata atomic.Int64
	fetches  atomic.Int64
	bytes    atomic.Int64
}

// Source reads documents out of a directory tree.
type Source struct {
	root   string
	name   string
	policy Policy

	maxSize   int64
	maxImage  int64
	maxDoc    int64
	skipDir   func(name string) bool
	includeIf func(name string) bool
	skipped   func(path string, reason error)
	watcher   *Watcher
	pacing    func(context.Context) error

	counters counters
}

// Option configures a source.
type Option func(*Source)

// WithMaxFileSize sets the largest file that will be read. A value below one
// selects [DefaultMaxFileSize].
func WithMaxFileSize(n int64) Option {
	return func(s *Source) {
		if n > 0 {
			s.maxSize = n
		}
	}
}

// WithSkipped installs a callback for files the walk passed over.
//
// A sync does not abandon a tree because one file in it could not be read. A
// file whose owner revoked the permission, or that was deleted between the
// listing and the stat, is a fact about the tree and not a reason to lose the
// hundred thousand files after it.
//
// What it must not be is silent. An index quietly missing the files nobody
// could read looks exactly like an index that is complete, and the difference
// only shows up when somebody cannot find a document they know exists. This is
// how a caller finds out: it is called once per skipped file with the path and
// the reason, and the default does nothing.
func WithSkipped(f func(path string, reason error)) Option {
	return func(s *Source) {
		if f != nil {
			s.skipped = f
		}
	}
}

// WithMaxImageSize sets the largest image that will be read into a document's
// content. A value below one selects [DefaultMaxImageSize].
func WithMaxImageSize(n int64) Option {
	return func(s *Source) {
		if n > 0 {
			s.maxImage = n
		}
	}
}

// WithMaxDocumentSize sets the largest PDF or Office file that will be read for
// extraction. A value below one selects [DefaultMaxDocumentSize].
func WithMaxDocumentSize(n int64) Option {
	return func(s *Source) {
		if n > 0 {
			s.maxDoc = n
		}
	}
}

// WithSkipDir replaces the rule for directories that are not descended into.
// The argument is the base name.
func WithSkipDir(f func(name string) bool) Option {
	return func(s *Source) {
		if f != nil {
			s.skipDir = f
		}
	}
}

// WithInclude replaces the rule for which file names are read. The argument is
// the base name.
func WithInclude(f func(name string) bool) Option {
	return func(s *Source) {
		if f != nil {
			s.includeIf = f
		}
	}
}

// WithWatcher makes the source ask a watcher what changed instead of walking
// the tree to find out.
//
// The watcher is built separately and owned by the caller, because building one
// fails for reasons that are a property of the machine rather than of the
// program, and a caller that cannot have one should carry on with a source that
// walks rather than fail to start.
//
//	w, err := fssource.Watch(root)
//	if err != nil {
//		log.Warn("watching the tree, syncs will walk it instead", "err", err)
//	}
//	src, err := fssource.New(root, "docs", policy, fssource.WithWatcher(w))
//
// A nil watcher is allowed and means exactly what passing no option means, so
// the error above does not need a second branch.
//
// The watcher has to be watching the same tree the source is reading, and
// [New] refuses the pair if it is not. It also has to skip the same directories,
// which is what [WithWatchSkipDir] is for.
func WithWatcher(w *Watcher) Option {
	return func(s *Source) { s.watcher = w }
}

// WithPace makes the source wait before it reads each file's content.
//
// A first read of a large tree is the one moment this connector competes with
// the server it is feeding. The walk is stat calls and the reads are the disk,
// and a process doing both flat out is a process whose queries are answered off
// a disk that is busy with something else. Slowing the read down is the only
// lever that helps, because the work itself cannot be made smaller: every file
// has to be read once.
//
// It is a function rather than a number so that what the pace is stays with
// whoever chose it. A token bucket, a semaphore shared with something else and
// a plain sleep are all the same shape from here, and the connector has no
// opinion about which of them a deployment wants.
//
//	l := limit.NewLimiter(limit.Limits{Rate: 200, Burst: 1}, nil)
//	src, err := fssource.New(root, "docs", policy, fssource.WithPace(l.Wait))
//
// The error it returns stops the sync rather than skipping the file, because
// the only thing that makes a pace fail is the context being done, and a sync
// that carried on from there would spend a shutdown reading the rest of the
// tree. A nil function is what passing no option means: read as fast as the
// disk allows.
func WithPace(wait func(context.Context) error) Option {
	return func(s *Source) { s.pacing = wait }
}

// New returns a source reading root, naming itself name, and asking policy who
// may read each file.
//
// A nil policy is allowed and quarantines every document. That is deliberate:
// it makes "I have not thought about permissions yet" a visible state in the
// stats rather than an invisible one in the index.
func New(root, name string, policy Policy, opts ...Option) (*Source, error) {
	if root == "" {
		return nil, errors.New("fssource: empty root")
	}
	if name == "" {
		return nil, errors.New("fssource: empty source name")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fssource: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("fssource: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fssource: %s is not a directory", abs)
	}

	s := &Source{
		root:      abs,
		name:      name,
		policy:    policy,
		maxSize:   DefaultMaxFileSize,
		maxImage:  DefaultMaxImageSize,
		maxDoc:    DefaultMaxDocumentSize,
		skipDir:   defaultSkipDir,
		includeIf: defaultInclude,
		skipped:   func(string, error) {},
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.watcher != nil && s.watcher.root != abs {
		// A watcher on a different tree would report changes this source cannot
		// place and, far worse, would report nothing about the tree it is
		// actually reading while looking to the sync like a healthy record.
		return nil, fmt.Errorf("fssource: the watcher is on %s and the source is on %s", s.watcher.root, abs)
	}
	return s, nil
}

var _ connector.Connector = (*Source)(nil)

// Source returns the connector's name.
func (s *Source) Source() string { return s.name }

// Close releases nothing, because a walk holds nothing open between calls.
func (s *Source) Close() error { return nil }

// Sync walks the tree and emits every file modified after the cursor.
//
// The returned cursor is the highest modification time seen in the whole tree,
// including files that were skipped as unchanged, so that a run which finds
// nothing new still moves the clock forward and a run interrupted halfway does
// not lose the files it already passed. It is never later than a second before
// the sync started, for the reason in [settled].
//
// A source built with [WithWatcher] reads the paths the watcher recorded and
// does not walk at all, whenever the watcher can vouch for its record. When it
// cannot, this is what runs, which is why the two paths have to agree on which
// files are in the corpus.
func (s *Source) Sync(ctx context.Context, from connector.Cursor, emit func(context.Context, connector.Change) error) (connector.Cursor, error) {
	// Read before anything is looked at, because the cursor this hands back is
	// bounded by it. Nothing created after this moment can be vouched for by
	// this sync, whatever modification time it happens to carry. Round drops
	// the monotonic reading, which means nothing in a cursor that gets written
	// down and compared against times off a filesystem.
	started := time.Now().Round(0)

	since, err := parseCursor(from)
	if err != nil {
		return connector.Cursor{}, err
	}

	// A policy that caches its answers is told the walk is starting, so that an
	// OWNERS file edited since the last run is read again. Without this a long
	// running process would keep serving the access control list the tree had
	// when it started, which is the failure mode where a revocation appears to
	// have been applied and has not.
	if r, ok := s.policy.(Reloader); ok {
		r.Reload()
	}

	changed, gen, watching := s.watched()
	if watching {
		return s.syncWatched(ctx, from, since, started, changed, emit)
	}

	var highest time.Time
	walkErr := s.walk(ctx, func(rel string, info fs.FileInfo) error {
		mod := info.ModTime()
		if mod.After(highest) {
			highest = mod
		}
		// Not After rather than Before, so a file written in the same
		// nanosecond as the cursor is not emitted twice on every later run.
		if !since.IsZero() && !mod.After(since) {
			at, err := s.refresh(ctx, rel, info, since, emit)
			if at.After(highest) {
				highest = at
			}
			return err
		}

		// The cursor has to cover when the permissions last changed as well as
		// when the content did, because the next run compares both against it. A
		// file whose mode was last written after it was last edited would
		// otherwise come back as a permission change on the very next sync,
		// stating the same access control list it was just given, for every such
		// file in the tree. An error here is the policy's problem and read below
		// reports it.
		if at, err := s.changedAt(ctx, rel, info); err == nil && at.After(highest) {
			highest = at
		}

		if err := s.pace(ctx); err != nil {
			return err
		}

		full := filepath.Join(s.root, filepath.FromSlash(rel))
		document, err := s.read(ctx, full, rel, info)
		if err != nil {
			s.skipped(full, err)
			return nil
		}
		return emit(ctx, connector.Change{
			Document: document,
			Cursor:   connector.Cursor{Value: version(mod), Time: mod},
		})
	})
	if walkErr != nil {
		return connector.Cursor{}, walkErr
	}
	if s.watcher != nil {
		// The tree has now been read end to end, so anything the watcher records
		// from here is the whole of what changed. A walk that failed does not get
		// here, and the watcher stays untrusted, because a record that starts
		// somewhere in the middle of a tree is not a record.
		s.watcher.caughtUp(gen)
	}

	if highest.IsZero() {
		return from, nil
	}
	return cursorAt(covered(highest, started, since)), nil
}

// settled is how far behind the moment a sync started its cursor is allowed to
// sit, and it is the whole of what keeps a file from being left behind.
//
// A cursor says that everything modified at or before that time has been read,
// and a sync that hands back the newest time it saw is saying more than it
// knows. A file is stamped from a clock that ticks rather than from the one that
// runs, so a file created a millisecond after the walk went past its directory
// carries the same modification time as one the walk read. The next walk emits
// what is strictly newer than the cursor, so that file is never read, nothing is
// quarantined and nothing is reported. It is simply not in the index until
// somebody writes to it again, which is the one failure this connector must not
// have.
//
// One second covers both halves of that. It covers the clock a file is stamped
// from, which ticks every few milliseconds, and it covers a filesystem that
// keeps modification times to the second, where a file created at any point
// during a second carries the start of it.
//
// What it costs is that a file written in the second before a sync is read by
// the next sync as well, and it is paid once: the sync after that starts more
// than a second later and the cursor moves past it.
const settled = time.Second

// covered is the newest modification time a sync that started at started can
// honestly say it has read everything up to.
//
// It never goes below the cursor the sync was given, which is a claim an earlier
// sync already made and this one has no reason to withdraw. Without that a tree
// written moments ago would drag the cursor backwards on every run.
func covered(highest, started, since time.Time) time.Time {
	if safe := started.Add(-settled); safe.Before(highest) {
		highest = safe
	}
	if highest.Before(since) {
		return since
	}
	return highest
}

// cursorAt is the cursor for one moment.
func cursorAt(t time.Time) connector.Cursor {
	return connector.Cursor{Value: version(t), Time: t}
}

// watched returns the paths the watcher recorded, the generation that record
// belongs to, and false when this sync has to walk the tree instead.
func (s *Source) watched() (changed map[string]watchOps, gen int64, ok bool) {
	if s.watcher == nil {
		return nil, 0, false
	}
	changed, gen, reason := s.watcher.pending()
	if reason != nil {
		return nil, gen, false
	}
	if p, ruled := s.policy.(Ruled); ruled {
		for rel := range changed {
			if p.IsRule(rel) {
				// Throwing the record away rather than walking with it in hand is
				// deliberate. The walk covers every path in it, and the next sync
				// then has to be told the tree is known again, which is what the
				// generation this returns is for.
				gen = s.watcher.lose(fmt.Errorf("fssource: %s is a permission rule and governs files no event will name", rel))
				return nil, gen, false
			}
		}
	}
	return changed, gen, true
}

// syncWatched reads the paths the watcher recorded, and nothing else.
//
// This is the whole point of the watcher: the cost of a sync stops being a
// function of how large the tree is and becomes a function of how much of it
// moved. A corpus of two million files where thirty changed costs thirty stats
// instead of two million.
//
// What it cannot do is see a file whose modification time went backwards under
// it, so the high water mark starts at the cursor rather than at zero. A file
// restored from a backup with its old time, which is what "cp -p" does, would
// otherwise drag the cursor into the past and make the next walk re-read
// everything written since then.
func (s *Source) syncWatched(ctx context.Context, from connector.Cursor, since, started time.Time, changed map[string]watchOps, emit func(context.Context, connector.Change) error) (connector.Cursor, error) {
	highest := since
	// Sorted, so that a sync over the same set of changes does the same work in
	// the same order twice, which is what makes a failure reproducible.
	rels := slices.Sorted(maps.Keys(changed))

	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return connector.Cursor{}, err
		}
		full := filepath.Join(s.root, filepath.FromSlash(rel))

		// The stat decides whether the file is there, not the event. A file
		// written and then deleted, or deleted and then written again, produces
		// both kinds of event and only one of them is still true, and the
		// filesystem is the one that knows which.
		s.counters.metadata.Add(1)
		info, err := os.Lstat(full)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// This is the case a walk can never report. There is nothing left to
			// walk past, so a tree traversal finds a deleted file by not finding
			// it, which is to say it needs a full reconciliation sweep. A watcher
			// is told.
			if !s.includeIf(path.Base(rel)) {
				continue
			}
			if err := emit(ctx, connector.Change{
				Document: doc.Document{ID: s.id(rel)},
				Deleted:  true,
			}); err != nil {
				return connector.Cursor{}, err
			}
			continue
		case err != nil:
			s.skipped(full, err)
			continue
		case info.IsDir() || !info.Mode().IsRegular():
			continue
		case !s.includeIf(path.Base(rel)):
			continue
		}
		if limit := s.limitFor(rel); info.Size() > limit {
			s.skipped(full, errTooBig(info.Size(), limit))
			continue
		}

		// A mode change and nothing else is a permission change, which is a write
		// of one descriptor rather than a read of the file. On a filesystem this
		// is the cheap half of the whole design, and the watcher is what makes it
		// exact: a walk has to ask every file in the tree whether its rule moved,
		// and here the operating system already said which one did.
		//
		// Nothing about this file goes into the cursor. Not its modification
		// time, which was not read, and not the time its rule changed, which is
		// later still on a policy that answers with the inode change time. Some
		// backends report an ordinary write as an attribute change first and the
		// write a moment later, so a sync landing between the two would move the
		// cursor past content it never read, and a later fall back to walking
		// would skip that file for good. Leaving the cursor where it is costs one
		// permission change written twice.
		if ops := changed[rel]; ops.chmod && !ops.write && !ops.remove {
			perms, err := s.permissions(ctx, rel, info)
			if err != nil {
				perms = connector.Unresolved(s.name, "the file mode changed and the new rule did not resolve: "+err.Error())
				s.skipped(full, err)
			}
			if err := emit(ctx, connector.Change{
				Document:        doc.Document{ID: s.id(rel), Permissions: perms},
				PermissionsOnly: true,
			}); err != nil {
				return connector.Cursor{}, err
			}
			continue
		}

		mod := info.ModTime()
		if mod.After(highest) {
			highest = mod
		}
		if at, err := s.changedAt(ctx, rel, info); err == nil && at.After(highest) {
			highest = at
		}

		if err := s.pace(ctx); err != nil {
			return connector.Cursor{}, err
		}

		document, err := s.read(ctx, full, rel, info)
		if err != nil {
			s.skipped(full, err)
			continue
		}
		if err := emit(ctx, connector.Change{
			Document: document,
			Cursor:   connector.Cursor{Value: version(mod), Time: mod},
		}); err != nil {
			return connector.Cursor{}, err
		}
	}

	if highest.IsZero() {
		return from, nil
	}
	return cursorAt(covered(highest, started, since)), nil
}

// refresh emits a permission change for a file whose content did not change but
// whose access control list did, and returns when that rule last changed.
//
// This is the whole of "a permission change takes effect without a content
// recrawl" on a filesystem. Access here does not live on the file, it lives in
// the OWNERS file above it, and editing one of those changes who may read every
// document in a subtree without touching a single one of them. A sync that only
// looks at modification times sees nothing at all, and the index keeps serving
// the old answer until somebody notices.
//
// What it costs is one policy lookup per unchanged file, which for the OWNERS
// policy is a map read after the first file in each directory. What it saves is
// reading the subtree.
func (s *Source) refresh(ctx context.Context, rel string, info fs.FileInfo, since time.Time, emit func(context.Context, connector.Change) error) (time.Time, error) {
	at, err := s.changedAt(ctx, rel, info)
	if err != nil {
		// A policy that cannot answer for one path is not a reason to abandon the
		// tree, for the same reason a file that cannot be read is not. It is
		// reported and the walk carries on, and the document keeps the access
		// control list it already had until something can say otherwise.
		s.skipped(rel, err)
		return time.Time{}, nil
	}
	if at.IsZero() || !at.After(since) {
		return at, nil
	}

	perms, err := s.permissions(ctx, rel, info)
	if err != nil {
		// The rule that used to govern this file stopped resolving. Quarantining
		// is the only safe reading: a document nobody can currently say who may
		// read is not a document to keep serving on last week's answer.
		perms = connector.Unresolved(s.name, "the rule that used to govern this file stopped resolving: "+err.Error())
		s.skipped(rel, err)
	}
	return at, emit(ctx, connector.Change{
		Document:        doc.Document{ID: s.id(rel), Permissions: perms},
		PermissionsOnly: true,
		// No cursor. The time this change is derived from belongs to a file
		// somewhere else in the tree, quite possibly one the walk has not
		// reached, so it is not a statement about how far this walk got. The end
		// of walk cursor covers it, and a run interrupted before that repeats the
		// refresh, which costs one write.
	})
}

// permissions asks the policy who may read a file, handing over the file
// information where the policy is one that wants it.
//
// A nil policy is not an oversight and not an error. It is the state a source
// built without one is in, and the answer is that nothing resolved, which
// quarantines every document rather than publishing the tree.
func (s *Source) permissions(ctx context.Context, rel string, info fs.FileInfo) (acl.Permissions, error) {
	return permissionsOf(ctx, s.policy, s.name, rel, info)
}

// permissionsOf is the question itself, without a source to ask it, so that a
// [Checker] over the same tree asks it the same way, including the shortcut for
// a policy that would otherwise stat a file the caller is already holding.
func permissionsOf(ctx context.Context, policy Policy, source, rel string, info fs.FileInfo) (acl.Permissions, error) {
	if policy == nil {
		return connector.Unresolved(source, "this source was built without a permission policy, so nothing in the tree resolves"), nil
	}
	if p, ok := policy.(statPolicy); ok {
		return p.permissionsFor(ctx, rel, info)
	}
	return policy.Permissions(ctx, rel)
}

// changedAt asks the policy when the rule governing a file last changed, and
// returns the zero time for a policy that cannot say.
func (s *Source) changedAt(ctx context.Context, rel string, info fs.FileInfo) (time.Time, error) {
	if p, ok := s.policy.(statVersioned); ok {
		return p.changedAtFor(ctx, rel, info)
	}
	if p, ok := s.policy.(Versioned); ok {
		return p.ChangedAt(ctx, rel)
	}
	return time.Time{}, nil
}

// limitFor is the size limit that applies to one file name. An image gets the
// image limit, a file that has to be extracted gets the document limit, and
// everything else gets the body limit.
func (s *Source) limitFor(name string) int64 {
	switch {
	case isImage(name):
		return s.maxImage
	case isExtractable(name):
		return s.maxDoc
	default:
		return s.maxSize
	}
}

// pace waits for whatever [WithPace] installed, and for nothing when a source
// was built without one.
//
// It sits in front of the content read and not in front of the walk, because
// the walk is the cheap half. Pacing the stat calls would put the second number
// in the progress report behind the same wall as the first and leave a first
// read that says nothing about how much there is to do.
func (s *Source) pace(ctx context.Context) error {
	if s.pacing == nil {
		return ctx.Err()
	}
	return s.pacing(ctx)
}

// read turns one file into a document.
func (s *Source) read(ctx context.Context, full, rel string, info fs.FileInfo) (doc.Document, error) {
	s.counters.fetches.Add(1)
	raw, err := os.ReadFile(full)
	if err != nil {
		return doc.Document{}, err
	}
	s.counters.bytes.Add(int64(len(raw)))

	picture := isImage(rel)
	document := isExtractable(rel)
	// A file that is not valid UTF-8, is not an image we recognise and is not a
	// format the extractor reads is a binary this connector has no business
	// pretending to have read.
	if !picture && !document && !utf8.Valid(raw) {
		return doc.Document{}, errors.New("not text")
	}

	perms, err := s.permissions(ctx, rel, info)
	if err != nil {
		// The document keeps the unresolved descriptor and is quarantined by the
		// pipeline. Failing to answer is not permission to publish.
		perms = connector.Unresolved(s.name, "who may read this file did not resolve: "+err.Error())
		s.skipped(full, err)
	}

	var (
		text    string
		content *doc.Content
		title   string
		media   = mediaTypeOf(rel)
		pages   int
		partial bool
	)
	switch {
	// An image has no body. Its file name is what a query can match, which is
	// how somebody finds architecture.png by typing architecture, and the bytes
	// are what the preview shows.
	case picture:
		content = &doc.Content{Bytes: raw}
		content.Width, content.Height = pixels(raw)

	case document:
		out, err := extract.Extract(ctx, bytes.NewReader(raw), rel)
		if err != nil {
			// One unreadable file is one document skipped and reported, the
			// same as a file whose permission was revoked between the listing
			// and the read. A hostile PDF does not cost the walk the hundred
			// thousand files after it.
			return doc.Document{}, err
		}
		text, title, pages, partial = out.Text, out.Title, out.Pages, out.Truncated
		if out.Media != "" {
			// What the bytes say beats what the extension says, because the
			// extension is whatever somebody typed when they saved the file.
			media = out.Media
		}

	default:
		text = string(raw)
	}

	kind := kindOf(rel)
	if title == "" {
		title = titleOf(rel, text, kind)
	}
	props := map[string]string{
		"path":        rel,
		"extension":   strings.TrimPrefix(path.Ext(rel), "."),
		doc.MediaType: media,
		"size_bytes":  strconv.FormatInt(info.Size(), 10),
	}
	if pages > 0 {
		props["pages"] = strconv.Itoa(pages)
	}
	if partial {
		// The body is a prefix of the document rather than the whole of it, and
		// a reader that cannot tell the difference will report a document as
		// missing a phrase that is in it.
		props["truncated"] = "true"
	}

	return doc.Document{
		ID:           s.name + ":" + rel,
		Kind:         kind,
		Title:        title,
		Body:         text,
		URL:          "file://" + filepath.ToSlash(full),
		Container:    containerOf(rel),
		ModifiedAt:   info.ModTime(),
		CreatedAt:    info.ModTime(),
		SourceUpdate: version(info.ModTime()),
		Permissions:  perms,
		Properties:   props,
		Content:      content,
	}, nil
}

// pixels reads the dimensions out of an encoded image without decoding it.
//
// The standard library answers for png, jpeg and gif. It does not answer for
// webp or svg, and rather than pull in a decoder for each, those record a zero,
// which the interface reads as no box to reserve.
func pixels(raw []byte) (width, height int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// parseCursor reads the modification time out of a cursor.
func parseCursor(c connector.Cursor) (time.Time, error) {
	if c.IsZero() {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, c.Value)
	if err != nil {
		// A cursor this connector cannot read is one written by a different
		// version or a different connector. Refusing is better than silently
		// resyncing the whole tree, which on a large corpus looks like a hang.
		return time.Time{}, fmt.Errorf("fssource: unreadable cursor %q: %w", c.Value, err)
	}
	return t, nil
}

// titleOf takes the first markdown heading, or the first non empty line if
// there is no heading, and falls back to the file name.
//
// Source files are named by their path instead. Their first line is a licence
// header or a package comment, which is the same in every file in the tree and
// tells a reader scanning a result list nothing about which file they are
// looking at. The path tells them exactly that, and is also how they would say
// it out loud.
func titleOf(rel, body string, kind doc.Kind) string {
	if kind == doc.KindCode {
		return rel
	}
	for line := range strings.SplitSeq(head(body, 4096), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if h, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(h)
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "---") {
			continue
		}
		if len(line) <= 120 {
			return line
		}
		break
	}
	return path.Base(rel)
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// containerOf is the directory a file lives in, relative to the root of the
// tree. A file at the root has no container rather than a container called
// ".", because that dot travels all the way to the result row and to the
// facet list, where it means nothing to anybody.
func containerOf(rel string) string {
	if dir := path.Dir(rel); dir != "." && dir != "/" {
		return dir
	}
	return ""
}

// kindOf maps an extension onto a document kind. Anything unrecognised is a
// file, which is the kind that promises the least.
func kindOf(rel string) doc.Kind {
	if isImage(rel) {
		return doc.KindImage
	}
	switch strings.ToLower(path.Ext(rel)) {
	case ".md", ".markdown", ".rst", ".adoc", ".txt", ".html":
		return doc.KindPage
	case ".go", ".rs", ".py", ".js", ".ts", ".tsx", ".jsx", ".c", ".h", ".cc", ".cpp",
		".java", ".rb", ".sh", ".sql", ".yaml", ".yml", ".toml", ".json", ".proto":
		return doc.KindCode
	default:
		return doc.KindFile
	}
}

// mediaTypes is the extension to media type table.
//
// It is a table rather than a call to mime.TypeByExtension because that
// function reads the operating system's mime database, which means the same
// corpus crawled on two machines can produce two different answers, and because
// the code types below are ours rather than registered ones.
var mediaTypes = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".txt":      "text/plain",
	".rst":      "text/plain",
	".adoc":     "text/plain",
	".html":     "text/html",
	".css":      "text/css",
	".go":       "text/x-go",
	".rs":       "text/x-rust",
	".py":       "text/x-python",
	".js":       "text/javascript",
	".jsx":      "text/javascript",
	".ts":       "text/x-typescript",
	".tsx":      "text/x-typescript",
	".c":        "text/x-c",
	".h":        "text/x-c",
	".cc":       "text/x-c++",
	".cpp":      "text/x-c++",
	".java":     "text/x-java",
	".rb":       "text/x-ruby",
	".sh":       "text/x-shellscript",
	".sql":      "text/x-sql",
	".yaml":     "text/x-yaml",
	".yml":      "text/x-yaml",
	".toml":     "text/x-toml",
	".json":     "application/json",
	".proto":    "text/x-protobuf",
	".pdf":      "application/pdf",
	".docx":     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".pptx":     "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".xlsx":     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".png":      "image/png",
	".jpg":      "image/jpeg",
	".jpeg":     "image/jpeg",
	".gif":      "image/gif",
	".webp":     "image/webp",
	".svg":      "image/svg+xml",
}

// mediaTypeOf is what a document says it is. An extension nobody recognises is
// text/plain, because that is what the connector actually read and it is the
// type a preview can render without guessing.
func mediaTypeOf(rel string) string {
	if t, ok := mediaTypes[strings.ToLower(path.Ext(rel))]; ok {
		return t
	}
	return "text/plain"
}

// isImage reports whether a file name is one of the image types this connector
// stores bytes for.
func isImage(name string) bool {
	return strings.HasPrefix(mediaTypeOf(name), "image/")
}

// isExtractable reports whether a file name is a format whose text has to be
// extracted rather than read.
//
// The name is what decides, not the bytes, because the decision is taken during
// the walk from a directory entry and a size, before anything has been opened.
// The extractor looks at the bytes afterwards and has the final say on what the
// file actually is.
func isExtractable(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".pdf", ".docx", ".pptx", ".xlsx":
		return true
	}
	return false
}

// defaultSkipDir skips version control, dependency and build directories, which
// hold far more files than the corpus and none of the content.
func defaultSkipDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules", "vendor", "target", "dist", "build",
		".venv", "__pycache__", ".idea", ".vscode":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// defaultInclude reads text formats, the documents the extractor understands
// and the image formats a preview can show.
func defaultInclude(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	if name == "OWNERS" || name == "README" || name == "LICENSE" {
		return true
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown", ".rst", ".adoc", ".txt", ".html", ".css",
		".go", ".rs", ".py", ".js", ".ts", ".tsx", ".jsx", ".c", ".h", ".cc", ".cpp",
		".java", ".rb", ".sh", ".sql", ".yaml", ".yml", ".toml", ".json", ".proto",
		".pdf", ".docx", ".pptx", ".xlsx",
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg":
		return true
	}
	return false
}
