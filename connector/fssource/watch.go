package fssource

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

// Watching a tree rather than walking it.
//
// A sync of a directory tree cannot avoid the walk. There is no change feed to
// ask, so finding the files that changed means asking the filesystem about
// every file, and on a corpus of a few million that is a few million system
// calls to find out that thirty of them moved.
//
// The operating system already knows. A watcher is how it is asked to say so,
// and the whole of what this file does is turn that stream of events into the
// same set of paths a walk would have produced, plus an honest answer about
// when it cannot.
//
// That last part is the important half. A watcher is an optimisation that is
// wrong rather than slow when it fails: a dropped event is a document that
// never gets reindexed and nothing anywhere reports it. So this one starts out
// untrusted, goes back to untrusted the moment anything is out of the ordinary,
// and a sync that finds it untrusted walks the tree. The failure mode is a slow
// sync, which is what would have happened anyway.

// DefaultMaxWatches bounds how many directories one watcher holds.
//
// Every backend charges for a watch and they charge differently. Linux keeps a
// per user limit on inotify watches that a large tree will reach, and the
// kqueue backend the BSDs and macOS use needs an open file descriptor per
// watched file, which reaches the process limit a great deal sooner. Rather
// than find that out as a stream of errors halfway through a tree, a watcher
// that would need more than this refuses to be built and says how many it
// wanted, and the caller falls back to walking.
const DefaultMaxWatches = 4096

// DefaultMaxPending bounds how many changed paths one watcher will hold between
// syncs.
//
// Past this the record has stopped being a saving. Walking the tree is bounded
// work and remembering an unbounded list of paths is not, so a tree that is
// being rewritten wholesale, which is what a build directory nobody excluded
// looks like, goes back to the walk instead of growing a map until the process
// is killed.
const DefaultMaxPending = 100_000

// errNotWalkedYet is the state a watcher starts in.
//
// It has watches on the tree and no idea what is in it, so the first sync has
// to walk. That is not a limitation worth working around: the first sync of an
// empty index reads the whole corpus anyway.
var errNotWalkedYet = errors.New("fssource: the tree has not been walked yet")

// watchOps is what happened to one path since the last sync, folded together.
//
// Several events on one path are one piece of work. A file written twice is
// read once, and a file that was written and then had its mode changed is read
// once as well, because reading it answers both questions.
type watchOps struct {
	write  bool
	remove bool
	chmod  bool
}

// Watcher records what changed in a tree, so that a sync can read those files
// instead of walking to find them.
//
// It is built separately from the [Source] and owned by the caller, because
// building one can fail in ways that are a property of the machine rather than
// of the program: a tree over the inotify limit, a filesystem the backend does
// not support, a process near its descriptor limit. A caller that gets an error
// here logs it and carries on with a source that walks, which is the behaviour
// it would have had anyway.
type Watcher struct {
	// root is the tree as the caller named it, and resolved is the same tree
	// with symbolic links followed. Both are kept because the backend reports
	// paths under whichever one it was given, and on macOS a temporary directory
	// is reached through a link, so a watcher that only knew one of them would
	// fail to place every event it received.
	root     string
	resolved string

	skipDir    func(name string) bool
	maxWatches int
	maxPending int

	notify *fsnotify.Watcher
	done   chan struct{}

	events atomic.Int64
	walks  atomic.Int64

	mu   sync.Mutex
	seen map[string]watchOps
	lost error
	// lostAt counts the times the record was thrown away, and is how a sync
	// finds out whether that happened while it was walking. A walk that started
	// at one generation and finished at another read a tree that was moving
	// under it, and cannot be the thing that makes the record trustworthy.
	lostAt  int64
	watches int
}

// WatchOption configures a watcher.
type WatchOption func(*Watcher)

// WithMaxWatches sets how many directories a watcher will hold. A value below
// one selects [DefaultMaxWatches].
func WithMaxWatches(n int) WatchOption {
	return func(w *Watcher) {
		if n > 0 {
			w.maxWatches = n
		}
	}
}

// WithMaxPending sets how many changed paths a watcher will remember between
// syncs. A value below one selects [DefaultMaxPending].
func WithMaxPending(n int) WatchOption {
	return func(w *Watcher) {
		if n > 0 {
			w.maxPending = n
		}
	}
}

// WithWatchSkipDir replaces the rule for directories that are not watched. The
// argument is the base name.
//
// It should be the same rule the source walks with. A watcher that descended
// into a directory the source skips would spend its watches on a dependency
// tree and report changes to files nothing indexes, and one that skipped a
// directory the source reads would silently miss every change in it.
func WithWatchSkipDir(f func(name string) bool) WatchOption {
	return func(w *Watcher) {
		if f != nil {
			w.skipDir = f
		}
	}
}

// Watch starts watching root and everything under it.
//
// The watcher is not trusted until a sync has walked the tree, so the first
// sync after this is a full walk whatever happens.
func Watch(root string, opts ...WatchOption) (*Watcher, error) {
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

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = abs
	}

	w := &Watcher{
		root:       abs,
		resolved:   resolved,
		skipDir:    defaultSkipDir,
		maxWatches: DefaultMaxWatches,
		maxPending: DefaultMaxPending,
		done:       make(chan struct{}),
		seen:       make(map[string]watchOps),
		lost:       errNotWalkedYet,
	}
	for _, opt := range opts {
		opt(w)
	}

	notify, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fssource: watching %s: %w", abs, err)
	}
	w.notify = notify

	if err := w.watchTree(resolved); err != nil {
		_ = notify.Close()
		return nil, err
	}

	go w.collect()
	return w, nil
}

// Close stops the watcher and releases every watch it holds.
//
// A source built with this watcher goes back to walking, which is what it does
// with no watcher at all, so closing one is safe while a sync is running.
func (w *Watcher) Close() error {
	err := w.notify.Close()
	<-w.done

	// Whatever was recorded stops meaning anything the moment the watches are
	// gone, and leaving it behind would let one last sync trust a record that
	// stopped being kept.
	w.lose(errors.New("fssource: the watcher is closed"))
	return err
}

// WatchStats is what a watcher has done.
//
// Walks is the number worth looking at. It is how many times the record could
// not be trusted and a sync had to walk the tree, and on a healthy watcher it
// is one, from the first sync. Anything more says the tree is churning past
// what the backend will carry, and Reason says which way.
type WatchStats struct {
	Watches int
	Events  int64
	Pending int
	Walks   int64
	Reason  string
}

// Stats reports what the watcher has done.
func (w *Watcher) Stats() WatchStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := WatchStats{
		Watches: w.watches,
		Events:  w.events.Load(),
		Pending: len(w.seen),
		Walks:   w.walks.Load(),
	}
	if w.lost != nil {
		s.Reason = w.lost.Error()
	}
	return s
}

// watchTree adds a watch to dir and to every directory under it.
func (w *Watcher) watchTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be read is one the source cannot read
			// either, so there is nothing under it to miss.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if p != dir && w.skipDir(d.Name()) {
			return fs.SkipDir
		}
		return w.add(p)
	})
}

// add puts a watch on one directory, refusing past the limit.
func (w *Watcher) add(dir string) error {
	w.mu.Lock()
	over := w.watches >= w.maxWatches
	w.mu.Unlock()
	if over {
		return fmt.Errorf("fssource: %s needs more than %d watches, which is the limit", w.root, w.maxWatches)
	}
	if err := w.notify.Add(dir); err != nil {
		return fmt.Errorf("fssource: watching %s: %w", dir, err)
	}
	w.mu.Lock()
	w.watches++
	w.mu.Unlock()
	return nil
}

// collect turns the backend's events into the record, until the watcher is
// closed.
func (w *Watcher) collect() {
	defer close(w.done)
	for {
		select {
		case ev, ok := <-w.notify.Events:
			if !ok {
				return
			}
			w.record(ev)
		case err, ok := <-w.notify.Errors:
			if !ok {
				return
			}
			// Every error the backend reports means it stopped being able to
			// tell us everything, and the one that matters most is the queue
			// overflowing, which is precisely the case where the record is
			// missing exactly the changes there were too many of.
			w.lose(err)
		}
	}
}

// record folds one event into the set of paths that changed.
func (w *Watcher) record(ev fsnotify.Event) {
	w.events.Add(1)

	rel, ok := w.relative(ev.Name)
	if !ok {
		// An event about something outside the tree is a watch this watcher did
		// not make, which means it does not understand its own state.
		w.lose(fmt.Errorf("fssource: an event arrived for %s, which is not under %s", ev.Name, w.root))
		return
	}
	if rel == "." {
		// The root directory itself. Backends report attribute changes on a
		// watched directory as an event about the directory, and there is no
		// document behind it to record.
		return
	}

	// A directory that appeared has to be watched before anything in it can be
	// noticed, and by the time this runs files may already have been written
	// into it, so it is walked as well as watched. A directory the source skips
	// is neither walked nor watched, for the same reason the walk does not
	// descend into it.
	if ev.Has(fsnotify.Create) {
		if info, err := os.Lstat(ev.Name); err == nil && info.IsDir() {
			if w.skipDir(filepath.Base(ev.Name)) {
				return
			}
			if err := w.watchTree(ev.Name); err != nil {
				w.lose(err)
				return
			}
			w.recordTree(ev.Name)
			return
		}
	}

	w.mark(rel, watchOps{
		write:  ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write),
		remove: ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename),
		chmod:  ev.Has(fsnotify.Chmod),
	})
}

// recordTree marks every file under a directory that has just appeared.
func (w *Watcher) recordTree(dir string) {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != dir && w.skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if rel, ok := w.relative(p); ok {
			w.mark(rel, watchOps{write: true})
		}
		return nil
	})
	if err != nil {
		w.lose(err)
	}
}

// mark folds one path's operations into the record.
//
// It keeps recording while the record is untrusted and a sync is off walking
// the tree, which looks like waste and is not. An event that lands after the
// walk has already passed that file is the one case where dropping it would
// lose a change for good, and the cost of keeping it is reading a file that was
// just read.
func (w *Watcher) mark(rel string, ops watchOps) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, held := w.seen[rel]; !held && len(w.seen) >= w.maxPending {
		w.loseLocked(fmt.Errorf("fssource: more than %d paths changed, which is more than a walk costs", w.maxPending))
		return
	}
	was := w.seen[rel]
	w.seen[rel] = watchOps{
		write:  was.write || ops.write,
		remove: was.remove || ops.remove,
		chmod:  was.chmod || ops.chmod,
	}
}

// lose says the record can no longer be trusted, and why, and returns the
// generation that says so.
func (w *Watcher) lose(err error) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loseLocked(err)
	return w.lostAt
}

func (w *Watcher) loseLocked(err error) {
	w.lost = err
	w.lostAt++
	clear(w.seen)
}

// pending returns what changed since the last sync and hands the record over,
// or the reason the tree has to be walked instead. The generation it returns is
// what a walk gives back to [Watcher.caughtUp].
//
// It clears the record either way. On the walking path that is safe because the
// walk covers everything that was in it, and events that arrive while the walk
// is running go into the new record and are looked at next time, which costs
// re-reading a file that was just read.
func (w *Watcher) pending() (changed map[string]watchOps, gen int64, reason error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lost != nil {
		w.walks.Add(1)
		clear(w.seen)
		return nil, w.lostAt, w.lost
	}
	seen := w.seen
	w.seen = make(map[string]watchOps)
	return seen, w.lostAt, nil
}

// caughtUp says a walk finished, so the record can be trusted from here.
//
// The generation is the one [Watcher.pending] gave out at the start of that
// walk. A different one now means the record was thrown away while the walk was
// running, which is the case where a file the walk had already passed changed
// again and nothing is left that remembers it. Then the walk does not count and
// the next sync walks as well.
//
// It is also only called after a walk that completed. One that failed halfway
// leaves the watcher untrusted, because the alternative is trusting a record
// that begins somewhere in the middle of a tree.
func (w *Watcher) caughtUp(gen int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lostAt != gen {
		return
	}
	w.lost = nil
}

// relative places an event's path inside the tree, and returns "." for the root
// itself.
func (w *Watcher) relative(p string) (string, bool) {
	up := ".." + string(filepath.Separator)
	for _, base := range [...]string{w.resolved, w.root} {
		rel, err := filepath.Rel(base, p)
		switch {
		case err != nil, filepath.IsAbs(rel), rel == "..", strings.HasPrefix(rel, up):
			continue
		}
		return filepath.ToSlash(rel), true
	}
	return "", false
}
