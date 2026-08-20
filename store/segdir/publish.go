package segdir

import (
	"fmt"
	"os"
	"slices"

	"github.com/tamnd/genba/store/segment"
)

// Publish makes a segment visible, or does not.
//
// The bytes are parsed before anything is written, so a segment that is not a
// segment is refused here rather than becoming a file that the next open has to
// have an opinion about. The sequence comes out of the header, which is what
// makes the file name and the segment's own identity one thing instead of two
// that can disagree.
//
// The order is the whole of the crash safety, and it is worth reading as a
// sequence of instants rather than as a list of steps:
//
//  1. The segment is written to a temporary name. Nothing can see it, and if
//     the process dies here the next open deletes it.
//  2. It is flushed, so the bytes are on the platter and not only in a cache.
//  3. It is renamed into its real name. It is now readable by anything that
//     looks, and still invisible, because nothing looks at anything the
//     manifest does not name.
//  4. The manifest is rewritten, flushed and renamed. This is the instant the
//     segment becomes visible, and it is a single rename, so there is no
//     instant at which it is half visible.
//
// A crash between any two of those leaves either the old index or the new one.
// There is no third state to recover from, which is why recovery is a sweep and
// a check rather than a repair.
func (d *Dir) Publish(b []byte) (Entry, error) {
	s, err := segment.Open(b)
	if err != nil {
		return Entry{}, fmt.Errorf("publishing: %w", err)
	}
	e := Entry{Sequence: s.Sequence(), Size: int64(len(b))}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return Entry{}, ErrClosed
	}
	at, found := slices.BinarySearchFunc(d.live, e, bySequence)
	if found {
		return Entry{}, fmt.Errorf("%w: %d", ErrSequence, e.Sequence)
	}

	file := d.File(e.Sequence)
	temp := file + tempExt
	if err := writeFile(temp, b, d.opt.Sync == SyncFull); err != nil {
		return Entry{}, fmt.Errorf("writing segment %d: %w", e.Sequence, err)
	}
	crashPoint("segment.write")

	if err := os.Rename(temp, file); err != nil {
		return Entry{}, fmt.Errorf("moving segment %d into place: %w", e.Sequence, err)
	}
	crashPoint("segment.rename")

	// The directory entry for the segment is flushed before the manifest is
	// written rather than after. Otherwise a crash could leave a manifest that
	// is durable naming a file whose name is not, which is the one ordering
	// that produces an index that will not open.
	if d.opt.Sync == SyncFull {
		if err := syncDir(d.path); err != nil {
			return Entry{}, fmt.Errorf("flushing the directory: %w", err)
		}
	}
	crashPoint("segment.dir.sync")

	next := max(d.next, e.Sequence)
	live := slices.Insert(slices.Clone(d.live), at, e)
	if err := d.writeManifest(live, next); err != nil {
		// The segment file is left where it is. Removing it here would be a
		// second thing that can fail while handling the first, and the next
		// open deletes it anyway because the manifest does not name it.
		return Entry{}, err
	}
	d.live, d.next = live, next
	return e, nil
}

// Remove unpublishes segments and then deletes them.
//
// The manifest is rewritten first, so the files are already invisible by the
// time they are unlinked, and a crash in between leaves files nothing names,
// which the next open sweeps. Doing it the other way round would leave a window
// where the manifest names a file that is gone, and that window is an index
// that will not open.
//
// A sequence that is not published is not an error, because a compaction that
// has to be safe to run twice should be able to run twice.
func (d *Dir) Remove(sequences ...uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrClosed
	}

	live := slices.Clone(d.live)
	gone := make([]uint64, 0, len(sequences))
	for _, seq := range sequences {
		at, found := slices.BinarySearchFunc(live, Entry{Sequence: seq}, bySequence)
		if !found {
			continue
		}
		live = slices.Delete(live, at, at+1)
		gone = append(gone, seq)
	}
	if len(gone) == 0 {
		return nil
	}
	if err := d.writeManifest(live, d.next); err != nil {
		return err
	}
	d.live = live

	for _, seq := range gone {
		if err := os.Remove(d.File(seq)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing segment %d: %w", seq, err)
		}
	}
	return nil
}

// Read returns the bytes of a published segment.
//
// It reads the file rather than mapping it, which is the simple thing and not
// the fast one. Mapping belongs with the layer that caches segments across
// queries, because the decision that matters there is when to unmap, and that
// is a question about a cache rather than about a directory.
func (d *Dir) Read(sequence uint64) ([]byte, error) {
	d.mu.RLock()
	closed := d.closed
	_, found := slices.BinarySearchFunc(d.live, Entry{Sequence: sequence}, bySequence)
	d.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}
	if !found {
		return nil, fmt.Errorf("%w: %d is not published", ErrMissing, sequence)
	}
	b, err := os.ReadFile(d.File(sequence))
	if err != nil {
		return nil, fmt.Errorf("%w: segment %d: %w", ErrMissing, sequence, err)
	}
	return b, nil
}

// bySequence orders the live set, which is kept sorted so that a lookup is a
// binary search and a publish is an insert rather than a resort.
func bySequence(a, b Entry) int {
	switch {
	case a.Sequence < b.Sequence:
		return -1
	case a.Sequence > b.Sequence:
		return 1
	default:
		return 0
	}
}

// writeFile writes a whole file and optionally flushes it.
//
// The flush is inside the same function as the write because the two belong
// together: a caller that can write without flushing is a caller that will one
// day forget, and the argument makes the choice visible at every call site.
func writeFile(path string, b []byte, sync bool) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if sync {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// crashPoint is where the crash tests kill the process.
//
// It is a variable in the package rather than a test helper because the points
// worth killing at are between two system calls, and there is nowhere else to
// stand. It costs an indirect call through a no op on a path that already did a
// write and an fsync, which is not a cost worth avoiding.
var crashPoint = func(string) {}
