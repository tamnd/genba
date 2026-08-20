package graph

import (
	"encoding/binary"
	"fmt"

	"github.com/tamnd/genba/store/column"
)

// A posting part holds one list of ascending document rows per entity or per
// edge.
//
//	0  4  lists
//	4  4*(lists+1)  byte offsets into the data, ascending, first zero
//	   data, one list after another
//
// A list is its first row as a variable length integer followed by the gaps to
// each row after it. The gaps are what make it small: an entity mentioned in a
// run of consecutive documents costs a byte a document, and the alternative,
// a dense bitmap per entity, costs the segment's row count in bits per entity
// whether the entity is mentioned once or everywhere. Fifty thousand entities
// over a hundred thousand documents is six hundred megabytes of mostly zeros
// that way and a few megabytes this way.
//
// A list's length in ids is not stored. The offsets bound it, and decoding
// stops when the bytes run out, so there is no count to disagree with the data.
//
// The offsets are what a reader validates, and they are the only thing it has
// to: they must ascend and the last must be the length of the data. Every list
// is then a subslice that cannot run past the part, so nothing below has to
// check again.
type postings struct {
	offsets []byte
	data    []byte
	lists   int
}

// openPostings parses a posting part and checks its offsets.
//
// want is how many lists there should be, which the caller knows from the
// header. Checking it here rather than trusting the part is what stops a
// damaged file from pairing an entity with another entity's mentions, which
// would be a wrong answer rather than an error.
func openPostings(b []byte, want int, what string) (*postings, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("%w: the %s lists are %d bytes, shorter than a count", ErrFormat, what, len(b))
	}
	lists := int(le.Uint32(b))
	if lists != want {
		return nil, fmt.Errorf("%w: %d %s lists for %d of them", ErrFormat, lists, what, want)
	}

	// lists came from the header, which has already been bounded against the
	// file, so this cannot overflow and cannot be a hostile allocation.
	end := 4 + (lists+1)*4
	if len(b) < end {
		return nil, fmt.Errorf("%w: %d %s lists need %d bytes of offsets and the part holds %d", ErrFormat, lists, what, end, len(b))
	}
	p := &postings{offsets: b[4:end:end], data: b[end:], lists: lists}

	last := uint32(0)
	for i := range lists + 1 {
		at := le.Uint32(p.offsets[i*4:])
		if at < last {
			return nil, fmt.Errorf("%w: the %s list offsets go backwards at %d", ErrFormat, what, i)
		}
		last = at
	}
	if int(last) != len(p.data) {
		return nil, fmt.Errorf("%w: the %s lists end at %d and the part holds %d bytes", ErrFormat, what, last, len(p.data))
	}
	return p, nil
}

// at returns one list's bytes. A list outside the part is empty rather than a
// panic, because the indexes reaching here come from a header that a damaged
// file wrote.
func (p *postings) at(i int) []byte {
	if i < 0 || i >= p.lists {
		return nil
	}
	lo, hi := le.Uint32(p.offsets[i*4:]), le.Uint32(p.offsets[(i+1)*4:])
	return p.data[lo:hi:hi]
}

// visible reports whether any row in a list is one the reader may see.
//
// This is the one question the posting lists exist to answer and it is asked
// once per entity and once per edge that a traversal considers, so it decodes
// only as far as the first visible row. An entity mentioned in a thousand
// documents the reader can see costs one gap, not a thousand.
//
// A nil allow means the reader may see everything, so a list is visible when it
// has anything in it at all.
func (p *postings) visible(i int, allow *column.Bitmap) bool {
	b := p.at(i)
	if allow == nil {
		return len(b) > 0
	}
	row := uint64(0)
	for len(b) > 0 {
		gap, n := binary.Uvarint(b)
		if n <= 0 {
			return false
		}
		row += gap
		if row > uint64(maxRow) {
			return false
		}
		if allow.Get(int(row)) {
			return true
		}
		b = b[n:]
	}
	return false
}

// rows decodes a whole list, which is what a caller wants when it is showing
// why an entity came back rather than deciding whether it may.
func (p *postings) rows(i int) []int {
	b := p.at(i)
	if len(b) == 0 {
		return nil
	}
	// One byte is the smallest a row can cost, so this is an upper bound on the
	// count that comes from the bytes on hand rather than from anything the
	// file claims.
	out := make([]int, 0, len(b))
	row := uint64(0)
	for len(b) > 0 {
		gap, n := binary.Uvarint(b)
		if n <= 0 || gap == 0 && len(out) > 0 {
			return out
		}
		row += gap
		if row > uint64(maxRow) {
			return out
		}
		out = append(out, int(row))
		b = b[n:]
	}
	return out
}

// maxRow is the largest document row a list can name, which is the largest a
// segment can hold.
const maxRow = 1<<32 - 1

// encodePostings writes the part. The lists arrive already sorted, distinct and
// in range, because [Builder.Entity] and [Builder.Edge] are where a caller's
// mistake becomes an error with something useful in it, and by here it is too
// late to say which call it came from.
func encodePostings(lists [][]int) []byte {
	// The gaps go straight after the header rather than into a second buffer
	// that is copied on at the end. An offset lands at a fixed place in the
	// header, so it can be written once the list before it is encoded, and one
	// buffer that grows is one allocation instead of two and a copy.
	header := 4 + (len(lists)+1)*4
	out := make([]byte, header, header+4*len(lists))
	le.PutUint32(out, uint32(len(lists)))
	le.PutUint32(out[4:], 0)
	for i, l := range lists {
		last := 0
		for _, row := range l {
			out = binary.AppendUvarint(out, uint64(row-last))
			last = row
		}
		le.PutUint32(out[4+(i+1)*4:], uint32(len(out)-header))
	}
	return out
}
