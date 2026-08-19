// Package column encodes a segment's columns and scans them into bitmaps.
//
// A column is one field across every row of a segment: the source a document
// came from, the time it was modified, its author, whether it is archived. The
// query path reads columns for two things, filtering and faceting, and both of
// them want the same shape of answer. Which rows match. Not the rows
// themselves, which is the point: a filter that materialises a row before
// deciding whether to keep it has paid for every row it is about to discard,
// and on a corpus where a filter cuts a million rows to a thousand that is
// three orders of magnitude of wasted decoding.
//
// So a scan returns a [Bitmap], and the permission filter is an AND against
// another one. Only what survives both gets read.
//
// # One physical representation
//
// Every type is stored as a sequence of unsigned integer codes, and the codes
// are in the same order as the values they stand for. That single decision is
// what keeps this package small:
//
//   - A string column has a dictionary of its distinct values, sorted, and the
//     code is a rank in it. Equality, set membership and prefix all resolve
//     against the dictionary first, so a scan compares integers and never looks
//     at a string.
//   - An integer or time column subtracts the smallest value it saw and stores
//     the difference. A column of timestamps a week apart needs the bits to
//     tell a week apart, not the bits to hold a Unix epoch.
//   - A boolean column is one bit.
//
// Because the codes preserve order, a range on values is a range on codes for
// every type, including strings. There is one scan loop underneath all of it.
//
// # Two encodings
//
// Plain packs the codes at the narrowest width that fits: a thousand distinct
// values is ten bits a row, not a string and not a pointer. Runs stores each
// value once with a length, for the columns that arrive sorted or nearly so,
// which in a search index is most of the interesting ones.
//
// The builder measures both and keeps the smaller. There is no knob, because
// the builder has the data in front of it and the caller does not.
//
// # Nulls
//
// A null is absence, not a value, so it does not get a code. Columns that have
// any carry a presence bitmap, and every scan intersects with it. A null never
// matches a predicate, including a range that would contain whatever the
// placeholder happens to be, and [Column.Nulls] is how you ask for them
// deliberately.
package column

import (
	"encoding/binary"
	"errors"
	"strconv"
)

// Version is the encoding this package writes. A reader that meets a version it
// does not know refuses the bytes rather than guessing at them, the same way
// the segment format does.
const Version uint8 = 1

// Type is what a column holds.
type Type uint8

// The supported types. A search index needs to filter on which source a
// document came from, when it changed, who owns it and whether it is archived,
// and that is a string, a time, a string and a boolean.
const (
	TypeString Type = 1
	TypeInt    Type = 2
	TypeTime   Type = 3
	TypeBool   Type = 4
)

func (t Type) String() string {
	switch t {
	case TypeString:
		return "string"
	case TypeInt:
		return "int"
	case TypeTime:
		return "time"
	case TypeBool:
		return "bool"
	}
	return "type(" + strconv.Itoa(int(t)) + ")"
}

func (t Type) known() bool {
	return t == TypeString || t == TypeInt || t == TypeTime || t == TypeBool
}

// Encoding is how the codes are laid out. It is chosen by the builder from the
// data rather than by the caller, and it is on the reader only so that a
// benchmark and a size report can say which one they got.
type Encoding uint8

// The supported encodings.
const (
	// EncodingPlain packs every code at a fixed width.
	EncodingPlain Encoding = 1
	// EncodingRuns stores each value once with the number of rows it covers.
	EncodingRuns Encoding = 2
)

func (e Encoding) String() string {
	switch e {
	case EncodingPlain:
		return "plain"
	case EncodingRuns:
		return "runs"
	}
	return "encoding(" + strconv.Itoa(int(e)) + ")"
}

// The errors this package returns. They are distinct because they mean
// different things to an operator: a version is a column written by something
// newer, a format error is bytes that were damaged or were never a column, and
// a type error is a query asking a string column for a date range.
var (
	ErrVersion = errors.New("column: unknown version")
	ErrFormat  = errors.New("column: malformed")
	ErrType    = errors.New("column: wrong type for this predicate")
	ErrRows    = errors.New("column: too many rows")
)

// The header. Fixed size, at the front, and the version is the first byte so a
// reader can refuse an unknown one before it has interpreted anything else.
const headerSize = 24

const (
	offVersion  = 0  // uint8
	offType     = 1  // uint8
	offEncoding = 2  // uint8
	offBits     = 3  // uint8, width of a packed code
	offRows     = 4  // uint32
	offEntries  = 8  // uint32, dictionary entries, zero unless the type is string
	offFlags    = 12 // uint8
	offReserved = 13 // three bytes, must be zero
	offBase     = 16 // int64, subtracted from every value before it was packed
)

// flagNulls says a presence bitmap follows the header.
const flagNulls = 1 << 0

// maxRows is what fits in the header's row count. It is far above any segment
// this will ever hold, and it is checked anyway, because the alternative is a
// silent wrap that produces a column claiming to be short.
const maxRows = 1<<32 - 1

// maxPackedBits is the widest a packed code may be short of the full sixty four.
//
// A code is read by loading eight bytes at the byte its first bit lives in and
// shifting, so the code has to end inside those bytes: seven bits of offset
// plus the width has to stay within sixty four. Anything wider than this is
// stored at the full width, where the packing is byte aligned and the question
// does not arise. It takes a column of more than a hundred quintillion distinct
// values to reach that, so this costs nothing and removes a case.
const maxPackedBits = 57

var le = binary.LittleEndian

// width returns the number of bits needed to hold every code up to max, and the
// mask that extracts one.
func width(largest uint64) (bits uint8, mask uint64) {
	for largest > 0 {
		bits++
		largest >>= 1
	}
	if bits > maxPackedBits {
		bits = 64
	}
	if bits >= 64 {
		return 64, ^uint64(0)
	}
	return bits, 1<<bits - 1
}

// codeBytes is the size of a plain codes section, including the eight bytes of
// slack the reader's eight byte load needs at the end.
func codeBytes(rows int, bits uint8) int {
	if bits == 0 {
		return 0
	}
	return (rows*int(bits)+7)/8 + 8
}
