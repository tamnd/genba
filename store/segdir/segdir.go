// Package segdir is a directory of segments and the manifest that decides
// which of them a reader can see.
//
// A segment is immutable once written, so the only mutable thing in an index is
// the answer to "which segments are there". This package owns that answer. It
// keeps it in one file, it changes it with one rename, and it treats everything
// on disk that the manifest does not name as rubbish left by a crash.
//
// # Why a crash is a correctness problem and not a durability one
//
// Losing the last hour of a crawl to a power cut is annoying and it is fixable
// by crawling again. Coming back with half a segment is not, because half a
// segment is half an access list. A document whose permissions were written and
// whose content was not is a document with no readers, which is merely wrong. A
// document whose content was written and whose permissions were not is a
// document with every reader, which is the thing this package exists to make
// impossible.
//
// So the rule is not "writes are durable". The rule is that a segment is either
// wholly visible or wholly invisible, at every instant, to every reader,
// including the reader that opens the directory after the machine came back.
//
// # How
//
// A publish writes the segment to a temporary name, makes it durable, renames
// it into place, and only then rewrites the manifest, also through a temporary
// name and a rename. A rename over an existing file is atomic, so the manifest
// is at every instant either exactly the old set or exactly the new one, and it
// is never a mixture of them however the write was interrupted.
//
// Recovery is the same rule read backwards. Whatever the manifest names is what
// exists, everything else in the directory is deleted, and a manifest that names
// something that is not there is an error rather than a smaller index. That last
// part matters: quietly serving the segments that did survive is how a crash
// turns into a silent partial index that nobody notices for a month.
//
// # What this package is not
//
// It is not a write ahead log. A publish is durable when it returns and not
// before, and a crawl that was halfway through a batch redoes the batch. That
// is the right trade for a search index, where the source of truth is somebody
// else's system and everything here can be rebuilt from it. A log would buy
// back the last few seconds of an operation that is already idempotent.
//
// It is not a lock either. One process at a time is assumed and not enforced,
// which is written down again in the docs rather than hidden here.
package segdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Sync is how much durability a publish pays for.
//
// The zero value is the safe one, which is deliberate. A caller who has not
// thought about this gets the setting that survives a power cut, and the two
// weaker ones have to be spelled out by somebody who has read what they cost.
type Sync uint8

const (
	// SyncFull flushes the segment, the manifest and the directory before a
	// publish returns. An index written this way survives the machine losing
	// power at any instant.
	SyncFull Sync = iota

	// SyncManifest flushes the manifest and the directory but not the bytes of
	// the segment.
	//
	// It survives the process being killed, because the pages are the operating
	// system's by then, and it does not survive the machine losing power: the
	// manifest can reach the platter naming a segment whose bytes did not. That
	// shows up at the next open as a segment that fails to verify, which is an
	// error rather than a smaller index, so the failure is loud. It is a
	// reasonable setting for an index that can be rebuilt from its sources and
	// a bad one for anything else.
	SyncManifest

	// SyncNone flushes nothing. It survives the process being killed and
	// nothing worse, and it is here for tests and for an index that is thrown
	// away at the end of the run.
	SyncNone
)

// String names the policy, so that a log line about durability says which one
// was in force.
func (s Sync) String() string {
	switch s {
	case SyncFull:
		return "full"
	case SyncManifest:
		return "manifest"
	case SyncNone:
		return "none"
	default:
		return "sync(" + strconv.FormatUint(uint64(s), 10) + ")"
	}
}

// Options are the settings a directory is opened with. The zero value is the
// safe one.
type Options struct {
	// Sync is the durability policy. The zero value is [SyncFull].
	Sync Sync

	// Verify reads and checksums every published segment at open rather than
	// only checking that it is there and is the size the manifest says.
	//
	// It is off by default because it turns opening an index into reading all
	// of it, which is seconds on a large one, and because the cheap check
	// already catches the failures a crash produces: a missing file and a
	// truncated one. It is worth turning on when the hardware is suspect, since
	// a bad sector produces a file of exactly the right length full of
	// something else, and nothing but a checksum finds that.
	Verify bool
}

// Entry is one published segment.
type Entry struct {
	// Sequence is the segment's own sequence number, taken from its header
	// rather than assigned here, and it names the file. One number for both
	// means there is no mapping to keep in step, and it means a tombstone that
	// has to beat the document it deletes is a comparison a reader can make
	// from the file name.
	Sequence uint64

	// Size is the length of the file in bytes, which is what the cheap check at
	// open compares against.
	Size int64
}

// Dir is a directory of segments.
//
// The live set is replaced wholesale rather than edited, so a reader that took
// it and a writer that is publishing never touch the same slice. Publishes are
// serialised: one writer, which is what the format assumes anyway.
type Dir struct {
	path   string
	opt    Options
	mu     sync.RWMutex
	live   []Entry
	next   uint64
	closed bool
}

const (
	segmentExt = ".seg"
	tempExt    = ".tmp"
	manifest   = "manifest"
)

var (
	// ErrClosed is a use of a directory after Close.
	ErrClosed = errors.New("segdir: the directory is closed")

	// ErrFormat is a manifest that was damaged or was never a manifest.
	ErrFormat = errors.New("segdir: the manifest is malformed")

	// ErrVersion is a manifest written by a newer build.
	ErrVersion = errors.New("segdir: unknown manifest version")

	// ErrSequence is a publish of a sequence that is already there.
	ErrSequence = errors.New("segdir: that sequence is already published")

	// ErrMissing is a manifest that names a segment the directory does not
	// hold, or holds at the wrong size. It is the loud version of a silently
	// smaller index.
	ErrMissing = errors.New("segdir: a published segment is missing or damaged")
)

// Open reads a directory, recovers it and returns the live set.
//
// Recovery is not a separate mode. There is no clean shutdown flag to check and
// no fast path that skips it, because a path taken only after a crash is a path
// that is only ever exercised after a crash. Every open deletes the temporary
// files and the unpublished segments, and every open verifies that what the
// manifest names is there.
//
// A directory that does not exist is created. A directory with no manifest is
// an empty index, which is the same thing a fresh one is, so a caller does not
// have to tell the two apart.
func Open(path string, opt Options) (*Dir, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("the segment directory: %w", err)
	}
	d := &Dir{path: path, opt: opt}

	live, next, err := d.readManifest()
	if err != nil {
		return nil, err
	}
	d.live, d.next = live, next

	if err := d.sweep(); err != nil {
		return nil, err
	}
	if err := d.check(); err != nil {
		return nil, err
	}
	return d, nil
}

// Path is the directory this was opened on.
func (d *Dir) Path() string { return d.path }

// File is where a segment lives, which is what a caller that wants to map it
// rather than read it needs.
func (d *Dir) File(sequence uint64) string {
	return filepath.Join(d.path, name(sequence))
}

// Segments is the live set, nearest to oldest by sequence.
//
// The slice is a copy, because the alternative is a caller holding the set
// while a publish replaces it, and a reader that holds a stale set is fine
// while a reader that holds a mutating one is not.
func (d *Dir) Segments() []Entry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return slices.Clone(d.live)
}

// Next is the sequence to give the next segment, and it is handed out once.
//
// The counter only ever goes up, including across a deletion, so a file name is
// never reused. Reusing one would mean a reader that cached bytes by name could
// be handed a different segment under a name it already knows.
func (d *Dir) Next() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next++
	return d.next
}

// Sequence is the highest sequence handed out so far.
func (d *Dir) Sequence() uint64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.next
}

// Close releases the directory. There is nothing held open between publishes,
// so this only makes later calls fail rather than flushing anything.
func (d *Dir) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	d.live = nil
	return nil
}

// name is the file a sequence lives in. Zero padded so that the directory
// listing and the sequence order are the same, which makes the listing readable
// by a person and sortable by a machine without either of them parsing it.
func name(sequence uint64) string {
	return fmt.Sprintf("%016d%s", sequence, segmentExt)
}

// sequence reads a file name back, and reports false for anything that is not
// one of ours. A directory can hold a README, an editor's swap file or somebody's
// notes, and none of those are a segment that failed to get published.
func sequence(file string) (uint64, bool) {
	if !strings.HasSuffix(file, segmentExt) {
		return 0, false
	}
	digits := strings.TrimSuffix(file, segmentExt)
	if len(digits) != 16 {
		return 0, false
	}
	var out uint64
	for i := range len(digits) {
		c := digits[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		out = out*10 + uint64(c-'0')
	}
	return out, true
}

// sweep deletes what the manifest does not name.
//
// Every temporary file is a publish that was interrupted, and every segment
// that is not live is either the same thing one step further on or a segment
// that was removed before the manifest naming it could be rewritten. Both are
// rubbish, both are safe to delete because nothing has ever been able to read
// them, and deleting them here is what keeps a directory that has crashed a
// thousand times the same size as one that never has.
func (d *Dir) sweep() error {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return fmt.Errorf("listing the segment directory: %w", err)
	}
	live := make(map[uint64]struct{}, len(d.live))
	for _, e := range d.live {
		live[e.Sequence] = struct{}{}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		file := e.Name()
		switch {
		case strings.HasSuffix(file, tempExt):
		case file == manifest:
			continue
		default:
			seq, ok := sequence(file)
			if !ok {
				continue
			}
			if _, ok := live[seq]; ok {
				continue
			}
		}
		if err := os.Remove(filepath.Join(d.path, file)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", file, err)
		}
	}
	return nil
}

// check is the other half of recovery: everything the manifest names has to be
// there, at the size it was published at.
//
// A published segment that is missing is refused rather than dropped. An index
// that quietly comes back smaller is a search that quietly returns fewer
// answers, and the difference between those and no answers is what makes it
// take a month to notice.
func (d *Dir) check() error {
	for _, e := range d.live {
		file := d.File(e.Sequence)
		info, err := os.Stat(file)
		if err != nil {
			return fmt.Errorf("%w: segment %d: %w", ErrMissing, e.Sequence, err)
		}
		if info.Size() != e.Size {
			return fmt.Errorf("%w: segment %d is %d bytes and the manifest says %d", ErrMissing, e.Sequence, info.Size(), e.Size)
		}
		if !d.opt.Verify {
			continue
		}
		if err := verify(file, e.Sequence); err != nil {
			return err
		}
	}
	return nil
}
