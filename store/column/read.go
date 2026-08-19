package column

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"time"
)

// Column is an encoded column opened for reading.
//
// Open does not copy the bytes it is given, so a column over a mapped segment
// costs a header parse and nothing else. The one exception is the presence
// bitmap, which is a word per sixty four rows and is turned into a [Bitmap] so
// that every scan can intersect with it directly.
type Column struct {
	typ      Type
	encoding Encoding
	bits     uint8
	mask     uint64
	rows     int
	entries  int
	base     int64
	size     int

	present *Bitmap // nil when nothing in the column is null

	codes []byte // the packed encoding
	runs  []run  // the run length encoding
	ends  []int  // one past the last row of each run, for random access

	offsets []byte // (entries+1) cumulative uint32 into blob
	blob    []byte
}

// Open reads a column out of bytes that may be anything at all.
//
// The checks are ordered so that the most specific answer comes out first. A
// version nobody knows is not a malformed column, it is a column written by
// something newer, and an operator who is told the wrong one of those goes
// looking in the wrong place.
func Open(b []byte) (*Column, error) {
	if len(b) < headerSize {
		return nil, fmt.Errorf("%w: %d bytes is shorter than the %d byte header", ErrFormat, len(b), headerSize)
	}
	if v := b[offVersion]; v != Version {
		return nil, fmt.Errorf("%w: %d, and this reads %d", ErrVersion, v, Version)
	}

	c := &Column{
		typ:      Type(b[offType]),
		encoding: Encoding(b[offEncoding]),
		bits:     b[offBits],
		rows:     int(le.Uint32(b[offRows:])),
		entries:  int(le.Uint32(b[offEntries:])),
		base:     int64(le.Uint64(b[offBase:])),
		size:     len(b),
	}
	switch {
	case !c.typ.known():
		return nil, fmt.Errorf("%w: %s", ErrFormat, c.typ)
	case c.encoding != EncodingPlain && c.encoding != EncodingRuns:
		return nil, fmt.Errorf("%w: %s", ErrFormat, c.encoding)
	case c.bits > maxPackedBits && c.bits != 64:
		return nil, fmt.Errorf("%w: %d bits a code cannot be read back", ErrFormat, c.bits)
	case b[offFlags]&^flagNulls != 0:
		return nil, fmt.Errorf("%w: flags %#x has a bit this version does not define", ErrFormat, b[offFlags])
	case b[offReserved] != 0 || b[offReserved+1] != 0 || b[offReserved+2] != 0:
		return nil, fmt.Errorf("%w: the reserved bytes are not zero", ErrFormat)
	case c.typ != TypeString && c.entries != 0:
		return nil, fmt.Errorf("%w: a %s column has a dictionary of %d", ErrFormat, c.typ, c.entries)
	}
	if c.bits < 64 {
		c.mask = 1<<c.bits - 1
	} else {
		c.mask = ^uint64(0)
	}

	rest := b[headerSize:]
	if b[offFlags]&flagNulls != 0 {
		words := (c.rows + 63) / 64
		if len(rest) < words*8 {
			return nil, fmt.Errorf("%w: the presence bitmap for %d rows does not fit", ErrFormat, c.rows)
		}
		c.present = NewBitmap(c.rows)
		for i := range words {
			c.present.words[i] = le.Uint64(rest[i*8:])
		}
		// A bit set past the last row would be a row that does not exist
		// claiming to have a value, which nothing downstream would ever look
		// at but which would make Count disagree with itself.
		c.present.trim()
		rest = rest[words*8:]
	}

	var err error
	if rest, err = c.readCodes(rest); err != nil {
		return nil, err
	}
	if rest, err = c.readDict(rest); err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: %d bytes past the end of the column", ErrFormat, len(rest))
	}
	return c, nil
}

func (c *Column) readCodes(rest []byte) ([]byte, error) {
	if c.encoding == EncodingPlain {
		n := codeBytes(c.rows, c.bits)
		if len(rest) < n {
			return nil, fmt.Errorf("%w: %d rows at %d bits need %d bytes and there are %d", ErrFormat, c.rows, c.bits, n, len(rest))
		}
		c.codes = rest[:n:n]
		return rest[n:], nil
	}

	if len(rest) < 4 {
		return nil, fmt.Errorf("%w: no run count", ErrFormat)
	}
	count := int(le.Uint32(rest))
	rest = rest[4:]
	// The capacity comes from what is actually there rather than from the
	// declared count, because the declared count is a number read out of bytes
	// that may have been written by anything. Every run costs at least two
	// bytes, so the shorter of the two is a bound the input cannot lie about.
	c.runs = make([]run, 0, min(count, len(rest)/2))
	total := 0
	for range count {
		code, n := binary.Uvarint(rest)
		if n <= 0 {
			return nil, fmt.Errorf("%w: run %d has no code", ErrFormat, len(c.runs))
		}
		rest = rest[n:]
		length, n := binary.Uvarint(rest)
		if n <= 0 {
			return nil, fmt.Errorf("%w: run %d has no length", ErrFormat, len(c.runs))
		}
		rest = rest[n:]
		if length == 0 || length > uint64(c.rows-total) {
			return nil, fmt.Errorf("%w: run %d covers %d rows and %d are left", ErrFormat, len(c.runs), length, c.rows-total)
		}
		total += int(length)
		c.runs = append(c.runs, run{code: code, length: int(length)})
		c.ends = append(c.ends, total)
	}
	if total != c.rows {
		return nil, fmt.Errorf("%w: the runs cover %d rows and the header says %d", ErrFormat, total, c.rows)
	}
	return rest, nil
}

func (c *Column) readDict(rest []byte) ([]byte, error) {
	if c.typ != TypeString {
		return rest, nil
	}
	n := 4 * (c.entries + 1)
	if len(rest) < n {
		return nil, fmt.Errorf("%w: a dictionary of %d needs %d bytes of offsets and there are %d", ErrFormat, c.entries, n, len(rest))
	}
	c.offsets = rest[:n:n]
	rest = rest[n:]

	blob := int(le.Uint32(c.offsets[n-4:]))
	if len(rest) < blob {
		return nil, fmt.Errorf("%w: the dictionary needs %d bytes of text and there are %d", ErrFormat, blob, len(rest))
	}
	c.blob = rest[:blob:blob]

	// Offsets are cumulative, so they have to be in order. A reader that
	// trusted them could be handed a pair that slices backwards, and the whole
	// point of checking here is that the accessors below then never have to.
	prev := uint32(0)
	for i := 1; i <= c.entries; i++ {
		at := le.Uint32(c.offsets[4*i:])
		if at < prev {
			return nil, fmt.Errorf("%w: dictionary offset %d goes backwards", ErrFormat, i)
		}
		prev = at
	}
	if c.entries > 0 && le.Uint32(c.offsets) != 0 {
		return nil, fmt.Errorf("%w: the dictionary does not start at zero", ErrFormat)
	}
	return rest[blob:], nil
}

// Type is what the column holds.
func (c *Column) Type() Type { return c.typ }

// Encoding is how the codes are laid out. A benchmark and a size report want
// this. A query does not.
func (c *Column) Encoding() Encoding { return c.encoding }

// Rows is how many rows the column has, nulls included.
func (c *Column) Rows() int { return c.rows }

// Size is the encoded size in bytes.
func (c *Column) Size() int { return c.size }

// Bits is the width of a packed code, and zero when every code is the same.
func (c *Column) Bits() int { return int(c.bits) }

// Dict returns the distinct values of a string column, sorted. It allocates all
// of them, so it is for a facet listing or a test rather than for a scan.
func (c *Column) Dict() []string {
	out := make([]string, c.entries)
	for i := range out {
		out[i] = string(c.term(i))
	}
	return out
}

// term returns the dictionary entry as a window into the encoded bytes. The
// bounds were checked by Open, which is why this does not check them.
func (c *Column) term(i int) []byte {
	lo := le.Uint32(c.offsets[4*i:])
	hi := le.Uint32(c.offsets[4*(i+1):])
	return c.blob[lo:hi]
}

// CodeAt returns the row's code and whether the row has a value at all. A code
// is a dictionary rank on a string column and a value less the column's base on
// the others, and it is exported so that a facet can tally rows without turning
// each one back into a value first.
func (c *Column) CodeAt(row int) (uint64, bool) {
	if row < 0 || row >= c.rows {
		return 0, false
	}
	if c.present != nil && !c.present.Get(row) {
		return 0, false
	}
	return c.code(row), true
}

func (c *Column) code(row int) uint64 {
	if c.encoding == EncodingRuns {
		i := sort.Search(len(c.ends), func(i int) bool { return c.ends[i] > row })
		if i == len(c.ends) {
			return 0
		}
		return c.runs[i].code
	}
	if c.bits == 0 {
		return 0
	}
	p := uint(row) * uint(c.bits)
	return (le.Uint64(c.codes[p>>3:]) >> (p & 7)) & c.mask
}

// StringAt returns the value of a row of a string column, and false when the
// row is null or the column is not one.
func (c *Column) StringAt(row int) (string, bool) {
	code, ok := c.CodeAt(row)
	if !ok || c.typ != TypeString || code >= uint64(c.entries) {
		return "", false
	}
	return string(c.term(int(code))), true
}

// IntAt returns the value of a row of an integer column.
func (c *Column) IntAt(row int) (int64, bool) {
	code, ok := c.CodeAt(row)
	if !ok || c.typ != TypeInt {
		return 0, false
	}
	return int64(code + uint64(c.base)), true
}

// TimeAt returns the value of a row of a time column, in UTC and to the
// millisecond.
func (c *Column) TimeAt(row int) (time.Time, bool) {
	code, ok := c.CodeAt(row)
	if !ok || c.typ != TypeTime {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(code + uint64(c.base))).UTC(), true
}

// BoolAt returns the value of a row of a boolean column.
func (c *Column) BoolAt(row int) (value, ok bool) {
	code, ok := c.CodeAt(row)
	if !ok || c.typ != TypeBool {
		return false, false
	}
	return code == 1, true
}

// Present returns the rows that have a value.
func (c *Column) Present() *Bitmap {
	if c.present == nil {
		out := NewBitmap(c.rows)
		out.SetRange(0, c.rows)
		return out
	}
	return c.present.Clone()
}

// Nulls returns the rows that do not. It is the only way a null is ever
// matched: no predicate returns one, because a missing value is not equal to
// anything and is not inside any range.
func (c *Column) Nulls() *Bitmap {
	out := c.Present()
	out.Not()
	return out
}

// MatchStrings returns the rows whose value is one of these.
//
// The values are resolved against the dictionary first, which costs a binary
// search each, and what the scan then compares is integers. A filter for six
// sources over a million rows reads no text at all.
func (c *Column) MatchStrings(vs ...string) (*Bitmap, error) {
	if c.typ != TypeString {
		return nil, fmt.Errorf("%w: strings against a %s column", ErrType, c.typ)
	}
	set := NewBitmap(c.entries)
	for _, v := range vs {
		if i, ok := c.rank([]byte(v)); ok {
			set.Set(i)
		}
	}
	return c.scan(filter{set: set, empty: set.Empty()}), nil
}

// MatchPrefix returns the rows whose value starts with the prefix. The
// dictionary is sorted, so the entries that match are contiguous and the
// predicate is a range on codes like every other one.
func (c *Column) MatchPrefix(prefix string) (*Bitmap, error) {
	if c.typ != TypeString {
		return nil, fmt.Errorf("%w: a prefix against a %s column", ErrType, c.typ)
	}
	p := []byte(prefix)
	lo := c.search(p)
	hi := c.entries
	if up := upper(p); up != nil {
		hi = c.search(up)
	}
	if lo >= hi {
		return c.scan(filter{empty: true}), nil
	}
	return c.scan(filter{lo: uint64(lo), hi: uint64(hi - 1)}), nil
}

// MatchInts returns the rows whose value is in [lo, hi].
func (c *Column) MatchInts(lo, hi int64) (*Bitmap, error) {
	if c.typ != TypeInt {
		return nil, fmt.Errorf("%w: an integer range against a %s column", ErrType, c.typ)
	}
	return c.scan(c.rangeOf(lo, hi)), nil
}

// MatchTimes returns the rows whose value is in [lo, hi], to the millisecond
// the column was built with. Both ends are inclusive, which for a date facet is
// the answer people expect and for an open ended range is what the zero and the
// far future are for.
func (c *Column) MatchTimes(lo, hi time.Time) (*Bitmap, error) {
	if c.typ != TypeTime {
		return nil, fmt.Errorf("%w: a time range against a %s column", ErrType, c.typ)
	}
	return c.scan(c.rangeOf(lo.UnixMilli(), hi.UnixMilli())), nil
}

// MatchBool returns the rows with that value.
func (c *Column) MatchBool(v bool) (*Bitmap, error) {
	if c.typ != TypeBool {
		return nil, fmt.Errorf("%w: a boolean against a %s column", ErrType, c.typ)
	}
	want := uint64(0)
	if v {
		want = 1
	}
	return c.scan(filter{lo: want, hi: want}), nil
}

// rangeOf turns a range of values into a range of codes. Every code is the
// value less the column's base, so the arithmetic is unsigned and a range that
// starts below the base or ends above the largest value simply clamps.
func (c *Column) rangeOf(lo, hi int64) filter {
	if hi < lo || hi < c.base {
		return filter{empty: true}
	}
	f := filter{hi: uint64(hi) - uint64(c.base)}
	if lo > c.base {
		f.lo = uint64(lo) - uint64(c.base)
	}
	return f
}

// rank finds a value in the dictionary.
func (c *Column) rank(v []byte) (int, bool) {
	i := c.search(v)
	if i < c.entries && bytes.Equal(c.term(i), v) {
		return i, true
	}
	return 0, false
}

// search returns the first dictionary entry that is not less than v.
func (c *Column) search(v []byte) int {
	return sort.Search(c.entries, func(i int) bool { return bytes.Compare(c.term(i), v) >= 0 })
}

// upper returns the first byte string that is greater than everything starting
// with p, or nil when there is none because p is all ones and every string that
// starts with it sorts at the very end.
func upper(p []byte) []byte {
	out := bytes.Clone(p)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xff {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}

// filter is a predicate reduced to codes: either a range, or a membership set
// over the dictionary when the predicate names values that are not adjacent.
type filter struct {
	lo, hi uint64
	set    *Bitmap
	empty  bool
}

// scan is the one loop. Every predicate in this package comes down to it,
// because every type is stored as codes in the same order as its values.
func (c *Column) scan(f filter) *Bitmap {
	out := NewBitmap(c.rows)
	if f.empty {
		return out
	}
	switch {
	case c.encoding == EncodingRuns:
		at := 0
		for _, r := range c.runs {
			if f.has(r.code) {
				out.SetRange(at, at+r.length)
			}
			at += r.length
		}
	case c.bits == 0:
		// Every row holds the same code, so the answer is all of them or none.
		if f.has(0) {
			out.SetRange(0, c.rows)
		}
	case f.set != nil:
		n := uint64(f.set.Len())
		for i := range c.rows {
			if code := c.code(i); code < n && f.set.Get(int(code)) {
				out.Set(i)
			}
		}
	default:
		for i := range c.rows {
			if code := c.code(i); code >= f.lo && code <= f.hi {
				out.Set(i)
			}
		}
	}
	// A null row carries a placeholder code, and a range that happens to
	// contain it would otherwise match a row that has no value.
	if c.present != nil {
		out.And(c.present)
	}
	return out
}

func (f filter) has(code uint64) bool {
	if f.set != nil {
		return code < uint64(f.set.Len()) && f.set.Get(int(code))
	}
	return code >= f.lo && code <= f.hi
}
