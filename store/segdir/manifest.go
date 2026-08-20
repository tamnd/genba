package segdir

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"

	"github.com/tamnd/genba/store/segment"
)

// The manifest is the only mutable thing in an index, so it is small, it is
// written whole, and it is checked before a byte of it is believed.
//
// Writing it whole rather than appending to it is the decision worth defending.
// A log of changes is the usual answer and it is faster to append to, but it
// has a torn tail after a crash, which means a reader has to decide how much of
// the tail to believe, and that decision is the bug. A file that is replaced by
// a rename has no torn state at all: the old one is complete and the new one is
// complete, and there is no third possibility to write code for. The cost is
// rewriting a few tens of bytes per live segment on every publish, which is
// nothing next to writing the segment itself.
const (
	// manifestMagic is eight bytes for the same reason the segment's is: four
	// bytes of anything turn up by accident in files that are not this.
	manifestMagic = "genbaman"

	// manifestVersion is a refusal rather than a negotiation, as it is
	// everywhere else in the store.
	manifestVersion uint16 = 1

	manifestHeader = 32
	manifestEntry  = 16
)

// Manifest layout, byte for byte, little endian throughout.
//
//	 0  8  magic
//	 8  2  version
//	10  2  flags, reserved and zero
//	12  4  the number of entries
//	16  8  the next sequence to hand out, so a name is never reused
//	24  4  checksum, crc32 Castagnoli over every other byte of the file
//	28  4  reserved, zero
//
// Then one entry per live segment, ascending by sequence:
//
//	0  8  sequence
//	8  8  size in bytes
const (
	offMagic    = 0
	offVersion  = 8
	offFlags    = 10
	offCount    = 12
	offNext     = 16
	offChecksum = 24
	offReserved = 28
)

var (
	le    = binary.LittleEndian
	table = crc32.MakeTable(crc32.Castagnoli)
)

// encodeManifest returns the bytes of a manifest.
func encodeManifest(live []Entry, next uint64) []byte {
	out := make([]byte, manifestHeader+manifestEntry*len(live))
	copy(out[offMagic:], manifestMagic)
	le.PutUint16(out[offVersion:], manifestVersion)
	le.PutUint16(out[offFlags:], 0)
	le.PutUint32(out[offCount:], uint32(len(live)))
	le.PutUint64(out[offNext:], next)
	le.PutUint32(out[offReserved:], 0)
	for i, e := range live {
		at := manifestHeader + i*manifestEntry
		le.PutUint64(out[at:], e.Sequence)
		le.PutUint64(out[at+8:], uint64(e.Size))
	}
	le.PutUint32(out[offChecksum:], checksum(out))
	return out
}

// checksum is the crc over every byte except the four the checksum itself
// occupies, which is the same convention the segment header uses.
func checksum(b []byte) uint32 {
	h := crc32.New(table)
	h.Write(b[:offChecksum])
	h.Write(b[offReserved:])
	return h.Sum32()
}

// decodeManifest parses one.
//
// These bytes come off a disk, so the count is not trusted to be the count.
// Nothing is allocated from it: it is turned into the size the file would have
// to be, and that has to be the size the file actually is, exactly, with
// nothing left over. A manifest claiming four billion entries fails that
// comparison rather than reserving memory for them.
func decodeManifest(b []byte) ([]Entry, uint64, error) {
	if len(b) < manifestHeader {
		return nil, 0, fmt.Errorf("%w: %d bytes is shorter than a header", ErrFormat, len(b))
	}
	if string(b[offMagic:offMagic+len(manifestMagic)]) != manifestMagic {
		return nil, 0, fmt.Errorf("%w: this is not a manifest", ErrFormat)
	}
	if v := le.Uint16(b[offVersion:]); v != manifestVersion {
		return nil, 0, fmt.Errorf("%w: version %d, this build reads %d", ErrVersion, v, manifestVersion)
	}
	if f := le.Uint16(b[offFlags:]); f != 0 {
		return nil, 0, fmt.Errorf("%w: flags %#04x are not known to this build", ErrVersion, f)
	}
	if r := le.Uint32(b[offReserved:]); r != 0 {
		return nil, 0, fmt.Errorf("%w: reserved bytes are not zero", ErrFormat)
	}
	count := uint64(le.Uint32(b[offCount:]))
	if want := uint64(manifestHeader) + count*manifestEntry; want != uint64(len(b)) {
		return nil, 0, fmt.Errorf("%w: %d entries need %d bytes and the file holds %d", ErrFormat, count, want, len(b))
	}
	if got, want := checksum(b), le.Uint32(b[offChecksum:]); got != want {
		return nil, 0, fmt.Errorf("%w: checksum %#08x, the file says %#08x", ErrFormat, got, want)
	}

	next := le.Uint64(b[offNext:])
	out := make([]Entry, 0, count)
	last := uint64(0)
	for i := range int(count) {
		at := manifestHeader + i*manifestEntry
		e := Entry{Sequence: le.Uint64(b[at:]), Size: int64(le.Uint64(b[at+8:]))}
		// Ascending and distinct, because a reader is entitled to treat the
		// sequence as an identity and two entries sharing one would make the
		// live set depend on which of them was looked at first.
		if i > 0 && e.Sequence <= last {
			return nil, 0, fmt.Errorf("%w: sequence %d follows %d", ErrFormat, e.Sequence, last)
		}
		if e.Size < 0 {
			return nil, 0, fmt.Errorf("%w: segment %d has a size of %d", ErrFormat, e.Sequence, e.Size)
		}
		if e.Sequence > next {
			return nil, 0, fmt.Errorf("%w: segment %d is ahead of the next sequence %d", ErrFormat, e.Sequence, next)
		}
		last = e.Sequence
		out = append(out, e)
	}
	return out, next, nil
}

// readManifest loads the live set from disk.
//
// No manifest is an empty index rather than an error, which is what makes a
// fresh directory and a directory whose first publish was interrupted the same
// case. Both of them have never had a reader see anything, so there is nothing
// to tell apart.
func (d *Dir) readManifest() ([]Entry, uint64, error) {
	b, err := os.ReadFile(filepath.Join(d.path, manifest))
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("reading the manifest: %w", err)
	}
	live, next, err := decodeManifest(b)
	if err != nil {
		return nil, 0, err
	}
	return live, next, nil
}

// writeManifest publishes a live set.
//
// This is the instant the index changes. Everything before it has been writing
// files nobody can see, and everything after it is tidying up. The rename is
// what makes it an instant rather than an interval.
func (d *Dir) writeManifest(live []Entry, next uint64) error {
	b := encodeManifest(live, next)
	final := filepath.Join(d.path, manifest)
	temp := final + tempExt

	if err := writeFile(temp, b, d.opt.Sync != SyncNone); err != nil {
		return fmt.Errorf("writing the manifest: %w", err)
	}
	crashPoint("manifest.write")

	if err := os.Rename(temp, final); err != nil {
		return fmt.Errorf("publishing the manifest: %w", err)
	}
	crashPoint("manifest.rename")

	if d.opt.Sync != SyncNone {
		if err := syncDir(d.path); err != nil {
			return fmt.Errorf("flushing the directory: %w", err)
		}
	}
	crashPoint("manifest.dir.sync")
	return nil
}

// verify reads a published segment back and checks it parses and is the one the
// manifest thinks it is.
//
// The sequence comparison is the part that is not obvious. A file with the
// right name and the right length could still be a different segment, if a
// crash happened between two publishes that reused a name, and the header says
// which one it is. Names are never reused here, so this should be impossible,
// and a check that holds an invariant nothing else enforces is worth the read
// it costs.
func verify(file string, want uint64) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("%w: segment %d: %w", ErrMissing, want, err)
	}
	s, err := segment.Open(b)
	if err != nil {
		return fmt.Errorf("%w: segment %d: %w", ErrMissing, want, err)
	}
	if s.Sequence() != want {
		return fmt.Errorf("%w: the file for segment %d holds segment %d", ErrMissing, want, s.Sequence())
	}
	return nil
}
