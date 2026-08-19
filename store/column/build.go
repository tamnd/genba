package column

import (
	"encoding/binary"
	"fmt"
	"slices"
	"time"
)

// Builder collects values and encodes them.
//
// The append methods do not return errors. A column is built by a loop over a
// million rows and an error check on every one of them is noise around the two
// mistakes that are actually possible, which are appending the wrong type and
// appending more rows than a column can hold. Both are recorded and both come
// back from [Builder.Build], which refuses to produce bytes rather than
// producing bytes that are wrong.
type Builder struct {
	typ  Type
	err  error
	rows int

	// codes holds a dictionary id for a string column and the value itself for
	// the others. It is turned into the final codes by Build, once it knows the
	// smallest value or the sorted order of the dictionary.
	codes []uint64

	// present is a row per bit and is only written out when something was null.
	present *Bitmap
	nulls   bool

	// terms is the distinct strings in the order they arrived and ids maps back
	// into it. The sort happens once, in Build.
	terms []string
	ids   map[string]uint64
}

// NewBuilder returns a builder for a column of the given type.
func NewBuilder(t Type) *Builder {
	b := &Builder{typ: t}
	if !t.known() {
		b.err = fmt.Errorf("%w: %s", ErrType, t)
	}
	if t == TypeString {
		b.ids = make(map[string]uint64)
	}
	return b
}

// Type is what the builder was made for.
func (b *Builder) Type() Type { return b.typ }

// Rows is how many values have been appended, nulls included.
func (b *Builder) Rows() int { return b.rows }

// AppendString adds a value to a string column.
func (b *Builder) AppendString(s string) {
	if !b.check(TypeString) {
		return
	}
	id, ok := b.ids[s]
	if !ok {
		id = uint64(len(b.terms))
		// The map key is the string the caller handed over. Go's map keeps its
		// own copy of the key bytes for a string key, so nothing here pins the
		// caller's buffer, and terms holds the same immutable value.
		b.ids[s] = id
		b.terms = append(b.terms, s)
	}
	b.push(id)
}

// AppendInt adds a value to an integer column.
func (b *Builder) AppendInt(v int64) {
	if !b.check(TypeInt) {
		return
	}
	b.push(uint64(v))
}

// AppendTime adds a value to a time column.
//
// Time is kept to the millisecond. A facet by month and a sort by modified date
// are what a time column is read for, neither of them can see a nanosecond, and
// the six digits below a millisecond are six digits of entropy that widen every
// code in the column and compress away to nothing.
func (b *Builder) AppendTime(t time.Time) {
	if !b.check(TypeTime) {
		return
	}
	b.push(uint64(t.UnixMilli()))
}

// AppendBool adds a value to a boolean column.
func (b *Builder) AppendBool(v bool) {
	if !b.check(TypeBool) {
		return
	}
	if v {
		b.push(1)
		return
	}
	b.push(0)
}

// AppendNull adds a row with no value. It is legal on every type.
func (b *Builder) AppendNull() {
	if b.err != nil {
		return
	}
	if b.present == nil {
		b.present = NewBitmap(0)
	}
	b.nulls = true
	b.push(0)
	// push already marked the row present, and a null is the one row that is
	// not.
	b.present.Clear(b.rows - 1)
}

func (b *Builder) check(want Type) bool {
	if b.err != nil {
		return false
	}
	if b.typ != want {
		b.err = fmt.Errorf("%w: appended a %s to a %s column", ErrType, want, b.typ)
		return false
	}
	return true
}

func (b *Builder) push(code uint64) {
	if b.rows >= maxRows {
		b.err = fmt.Errorf("%w: %d is the most a column can hold", ErrRows, maxRows)
		return
	}
	b.codes = append(b.codes, code)
	b.rows++
	if b.present != nil {
		b.growPresence()
		b.present.Set(b.rows - 1)
	}
}

// growPresence extends the presence bitmap to cover every row appended so far,
// including the ones that arrived before the first null.
func (b *Builder) growPresence() {
	if b.present.Len() >= b.rows {
		return
	}
	next := NewBitmap(max(b.rows, 2*b.present.Len()))
	copy(next.words, b.present.words)
	// Every row before the first null had a value, and none of them set a bit
	// because there was no bitmap yet.
	for i := b.present.Len(); i < b.rows-1; i++ {
		next.Set(i)
	}
	b.present = next
}

// Build encodes the column.
//
// It picks the encoding by measuring both and keeping the smaller, which is the
// only honest way to choose: a column of a thousand rows in four runs wants run
// lengths, the same column shuffled wants packing, and nothing about the schema
// says which one arrived.
func (b *Builder) Build() ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}

	codes, base, entries, dict := b.encode()
	var maxCode uint64
	for _, c := range codes {
		maxCode = max(maxCode, c)
	}
	bits, _ := width(maxCode)

	runs, runBytes := b.runs(codes)
	encoding := EncodingPlain
	body := codeBytes(b.rows, bits)
	if runBytes <= body {
		encoding, body = EncodingRuns, runBytes
	}

	presence := 0
	if b.nulls {
		presence = ((b.rows + 63) / 64) * 8
	}

	out := make([]byte, headerSize, headerSize+presence+body+len(dict))
	out[offVersion] = Version
	out[offType] = uint8(b.typ)
	out[offEncoding] = uint8(encoding)
	out[offBits] = bits
	le.PutUint32(out[offRows:], uint32(b.rows))
	le.PutUint32(out[offEntries:], uint32(entries))
	if b.nulls {
		out[offFlags] = flagNulls
	}
	le.PutUint64(out[offBase:], uint64(base))

	if b.nulls {
		for i := range presence / 8 {
			out = le.AppendUint64(out, b.present.words[i])
		}
	}

	if encoding == EncodingRuns {
		out = le.AppendUint32(out, uint32(len(runs)))
		for _, r := range runs {
			out = binary.AppendUvarint(out, r.code)
			out = binary.AppendUvarint(out, uint64(r.length))
		}
	} else {
		out = appendPacked(out, codes, bits)
	}

	return append(out, dict...), nil
}

// encode turns the collected values into their final codes, and returns the
// base that was subtracted, the dictionary size and the encoded dictionary.
func (b *Builder) encode() (codes []uint64, base int64, entries int, dict []byte) {
	if b.typ == TypeString {
		return b.encodeStrings()
	}
	if b.typ == TypeBool || b.rows == 0 {
		return b.codes, 0, 0, nil
	}
	// The smallest value present, which becomes zero. Nulls are skipped: a
	// column whose only null happens to sit at a placeholder of zero should not
	// have its base dragged down to zero along with it.
	first := true
	var lo int64
	for i, c := range b.codes {
		if b.nulls && !b.present.Get(i) {
			continue
		}
		if v := int64(c); first || v < lo {
			lo, first = v, false
		}
	}
	if first {
		// Every row is null, so there is nothing to offset against.
		return b.codes, 0, 0, nil
	}
	out := make([]uint64, len(b.codes))
	for i, c := range b.codes {
		if b.nulls && !b.present.Get(i) {
			continue
		}
		out[i] = c - uint64(lo)
	}
	return out, lo, 0, nil
}

// encodeStrings sorts the dictionary and remaps every row onto a rank in it.
//
// The sort is what makes a prefix or a range on a string column a range on
// codes, so the scan loop underneath is the same one the numeric columns use
// and it never touches a byte of text.
func (b *Builder) encodeStrings() (codes []uint64, base int64, entries int, dict []byte) {
	sorted := slices.Clone(b.terms)
	slices.Sort(sorted)

	rank := make([]uint64, len(b.terms))
	for i, s := range sorted {
		rank[b.ids[s]] = uint64(i)
	}
	out := make([]uint64, len(b.codes))
	for i, id := range b.codes {
		out[i] = rank[id]
	}

	// Offsets first, then the bytes. The offsets are cumulative and there is one
	// more of them than there are entries, so the length of the last one needs
	// no special case in the reader.
	blob := 0
	for _, s := range sorted {
		blob += len(s)
	}
	dict = make([]byte, 0, 4*(len(sorted)+1)+blob)
	at := uint32(0)
	for _, s := range sorted {
		dict = le.AppendUint32(dict, at)
		at += uint32(len(s))
	}
	dict = le.AppendUint32(dict, at)
	for _, s := range sorted {
		dict = append(dict, s...)
	}
	return out, 0, len(sorted), dict
}

type run struct {
	code   uint64
	length int
}

// runs collapses the codes into runs and reports what encoding them would cost.
// It is called before the encoding is chosen, so it does the counting whether
// or not the answer is used.
func (b *Builder) runs(codes []uint64) (out []run, encoded int) {
	if len(codes) == 0 {
		return nil, 4
	}
	out = []run{{code: codes[0], length: 1}}
	for _, c := range codes[1:] {
		if last := &out[len(out)-1]; last.code == c {
			last.length++
			continue
		}
		out = append(out, run{code: c, length: 1})
	}
	encoded = 4
	for _, r := range out {
		encoded += uvarintLen(r.code) + uvarintLen(uint64(r.length))
	}
	return out, encoded
}

func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// appendPacked writes the codes at a fixed width, least significant bit first,
// and pads the end so a reader may always load eight bytes at the byte a code
// starts in.
func appendPacked(out []byte, codes []uint64, bits uint8) []byte {
	if bits == 0 {
		return out
	}
	at := len(out)
	out = append(out, make([]byte, codeBytes(len(codes), bits))...)
	// No code ever straddles the eight bytes written here. A width up to
	// maxPackedBits leaves room for the seven bits of offset it can start at,
	// and the only wider width is the full sixty four, where every code starts
	// on a byte. That is what maxPackedBits is for, and it is why this is one
	// read, one or, one write.
	for i, c := range codes {
		p := uint(i) * uint(bits)
		j := at + int(p>>3)
		le.PutUint64(out[j:], le.Uint64(out[j:])|c<<(p&7))
	}
	return out
}
