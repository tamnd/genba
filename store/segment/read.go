package segment

import (
	"fmt"
)

// Segment is a parsed segment.
//
// It holds the bytes it was opened over and a table of where the sections are
// in them. It allocates nothing per section: [Segment.Section] returns a window
// onto the original bytes, so a segment of a hundred megabytes costs a hundred
// megabytes once, whether one reader has it open or fifty do.
//
// The bytes must not change while a segment is open. That is free for a
// published segment, since a published segment is immutable, and it is the
// caller's problem for anything else.
type Segment struct {
	b        []byte
	sequence uint64
	table    []entry
}

type entry struct {
	kind   Kind
	offset uint64
	length uint64
}

// Open parses a segment over the bytes it is given.
//
// The order of the checks is deliberate. Magic first, so a file that is not a
// segment says that rather than something about a version. Version next,
// because a newer format could put anything at all in the bytes that follow and
// nothing below this line is worth doing until the layout is known to be the
// one this build understands. The checksum before the table, because a failed
// checksum explains every structural error that would otherwise be reported
// instead of it, and "malformed at offset 240" sends somebody looking for a bug
// in a writer when the answer is a bad disk.
func Open(b []byte) (*Segment, error) {
	if len(b) < headerSize {
		return nil, fmt.Errorf("%w: %d bytes is shorter than a header", ErrMagic, len(b))
	}
	if string(b[:len(Magic)]) != Magic {
		return nil, ErrMagic
	}
	if v := le.Uint16(b[offVersion:]); v != Version {
		return nil, fmt.Errorf("%w: version %d, this build reads %d", ErrVersion, v, Version)
	}
	// A flag this build does not know changes the meaning of the bytes below in
	// a way it cannot guess, so an unknown flag is the same refusal as an
	// unknown version rather than something to ignore politely.
	if f := le.Uint16(b[offFlags:]); f != 0 {
		return nil, fmt.Errorf("%w: flags %#04x are not known to this build", ErrVersion, f)
	}
	if r := le.Uint32(b[offReserved:]); r != 0 {
		return nil, fmt.Errorf("%w: reserved header bytes are not zero", ErrFormat)
	}

	// The declared length has to match the bytes on hand exactly, and it is
	// checked before it is used for anything. This is the field a truncated
	// copy gets wrong, and it is the field that would otherwise be handed to a
	// slice expression.
	//
	// Longer is refused as well as shorter. A segment with room after it is a
	// segment somebody will one day put something after, and once one file has
	// a use for that space the format has grown a feature nobody designed.
	length := le.Uint64(b[offLength:])
	if length != uint64(len(b)-headerSize) {
		return nil, fmt.Errorf("%w: header claims %d bytes after it and the file holds %d", ErrFormat, length, len(b)-headerSize)
	}
	if got, want := checksum(b), le.Uint32(b[offChecksum:]); got != want {
		return nil, fmt.Errorf("%w: computed %#08x and the header says %#08x", ErrChecksum, got, want)
	}

	count := uint64(le.Uint32(b[offSections:]))
	// count is a uint32 and entrySize is 24, so this product cannot overflow a
	// uint64, which is why it is safe to compute before it is bounded.
	tableEnd := headerSize + count*entrySize
	if tableEnd > uint64(len(b)) {
		return nil, fmt.Errorf("%w: %d sections need %d bytes of table and the file holds %d", ErrFormat, count, count*entrySize, len(b)-headerSize)
	}

	s := &Segment{b: b, sequence: le.Uint64(b[offSequence:])}
	// The table is the one allocation Open makes, and it is bounded by the file
	// rather than by a field in it: count has already been shown to fit inside
	// the bytes on hand, so a length that claims four billion sections fails the
	// bound above rather than reserving memory for them.
	s.table = make([]entry, 0, count)

	prev := tableEnd
	for i := range count {
		e := b[headerSize+i*entrySize:]
		if le.Uint32(e[entryPad:]) != 0 {
			return nil, fmt.Errorf("%w: reserved bytes of section %d are not zero", ErrFormat, i)
		}
		kind := Kind(le.Uint32(e[entryKind:]))
		offset, size := le.Uint64(e[entryOffset:]), le.Uint64(e[entryLength:])

		// Ascending and non overlapping, checked as one condition against the
		// end of the previous section. A table that may not overlap is a table
		// where a section cannot be made to alias another one's bytes, and that
		// is worth more than the freedom to write sections in an odd order.
		if offset < prev {
			return nil, fmt.Errorf("%w: section %d starts at %d, inside or before the section that ends at %d", ErrFormat, i, offset, prev)
		}
		// Written as a subtraction so that an offset near the top of the range
		// cannot be carried past it by an addition.
		if offset > uint64(len(b)) || size > uint64(len(b))-offset {
			return nil, fmt.Errorf("%w: section %d runs from %d for %d bytes and the file holds %d", ErrFormat, i, offset, size, len(b))
		}
		if i > 0 && kind <= s.table[len(s.table)-1].kind {
			return nil, fmt.Errorf("%w: section %d is %s and does not come after %s", ErrFormat, i, kind, s.table[len(s.table)-1].kind)
		}
		s.table = append(s.table, entry{kind: kind, offset: offset, length: size})
		prev = offset + size
	}
	return s, nil
}

// Sequence is the order this segment was published in.
//
// It is what decides which of two statements about the same document is the
// current one, so a tombstone in segment nine wins over the document in segment
// four and nothing has to be rewritten for it to.
func (s *Segment) Sequence() uint64 { return s.sequence }

// Kinds returns the sections present, in ascending order.
func (s *Segment) Kinds() []Kind {
	out := make([]Kind, len(s.table))
	for i, e := range s.table {
		out[i] = e.kind
	}
	return out
}

// Section returns the bytes of a section and whether it is there.
//
// The bytes are a window onto the segment rather than a copy, so reading a
// section costs nothing and writing to what comes back corrupts the segment for
// every other reader of it. Callers do not write to it, which is the same
// contract every reader of an immutable file works under.
func (s *Segment) Section(kind Kind) ([]byte, bool) {
	for _, e := range s.table {
		if e.kind == kind {
			return s.b[e.offset : e.offset+e.length : e.offset+e.length], true
		}
	}
	return nil, false
}

// Size is the length of the whole segment in bytes.
func (s *Segment) Size() int { return len(s.b) }
