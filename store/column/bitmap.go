package column

import "math/bits"

// Bitmap is a set of row numbers, one bit per row.
//
// It is dense on purpose. A scan already costs one pass over every row of a
// column, so the answer it produces is a word per sixty four rows whether one
// row matched or all of them, and a compressed bitmap would win on the selective
// filters and lose on the ones that match half the segment. More to the point,
// the operation this type exists for is the intersection between what a filter
// matched and what a principal is allowed to read, and that intersection is on
// the hot path of every query. Dense words make it an AND instruction. Anything
// compressed puts a decoder in front of it.
type Bitmap struct {
	n     int
	words []uint64
}

// NewBitmap returns an empty bitmap over n rows.
func NewBitmap(n int) *Bitmap {
	if n < 0 {
		n = 0
	}
	return &Bitmap{n: n, words: make([]uint64, (n+63)/64)}
}

// Len is the number of rows the bitmap is over, not the number set.
func (b *Bitmap) Len() int { return b.n }

// Set adds a row. A row outside the bitmap is ignored rather than a panic,
// because a scan that walks a run past the end of a truncated column should
// produce a wrong answer at worst and never take the process down.
func (b *Bitmap) Set(i int) {
	if i < 0 || i >= b.n {
		return
	}
	b.words[i>>6] |= 1 << (uint(i) & 63)
}

// Clear removes a row.
func (b *Bitmap) Clear(i int) {
	if i < 0 || i >= b.n {
		return
	}
	b.words[i>>6] &^= 1 << (uint(i) & 63)
}

// Get reports whether a row is in the set.
func (b *Bitmap) Get(i int) bool {
	if i < 0 || i >= b.n {
		return false
	}
	return b.words[i>>6]&(1<<(uint(i)&63)) != 0
}

// SetRange adds every row in [lo, hi). It is what a run length encoded scan
// calls once per matching run, so it fills whole words rather than looping over
// bits.
func (b *Bitmap) SetRange(lo, hi int) {
	if lo < 0 {
		lo = 0
	}
	if hi > b.n {
		hi = b.n
	}
	if lo >= hi {
		return
	}
	first, last := lo>>6, (hi-1)>>6
	head := ^uint64(0) << (uint(lo) & 63)
	tail := ^uint64(0) >> (63 - (uint(hi-1) & 63))
	if first == last {
		b.words[first] |= head & tail
		return
	}
	b.words[first] |= head
	for i := first + 1; i < last; i++ {
		b.words[i] = ^uint64(0)
	}
	b.words[last] |= tail
}

// Count is the number of rows in the set.
func (b *Bitmap) Count() int {
	n := 0
	for _, w := range b.words {
		n += bits.OnesCount64(w)
	}
	return n
}

// Empty reports whether nothing is set, without counting all of it.
func (b *Bitmap) Empty() bool {
	for _, w := range b.words {
		if w != 0 {
			return false
		}
	}
	return true
}

// And intersects in place. This is the permission filter: a scan produces the
// rows a predicate matched, the ACL produces the rows a principal may read, and
// the answer is what survives both.
//
// The two bitmaps do not have to be the same length. Rows the other side does
// not have are treated as unset, so intersecting with a shorter bitmap clears
// the tail rather than leaving rows in the answer that nothing vouched for.
func (b *Bitmap) And(o *Bitmap) {
	for i := range b.words {
		if i < len(o.words) {
			b.words[i] &= o.words[i]
		} else {
			b.words[i] = 0
		}
	}
}

// Or unions in place. Rows past the end of the receiver are dropped, because
// the receiver is what says how many rows there are.
func (b *Bitmap) Or(o *Bitmap) {
	for i := range b.words {
		if i >= len(o.words) {
			break
		}
		b.words[i] |= o.words[i]
	}
	b.trim()
}

// AndNot removes every row the other bitmap has.
func (b *Bitmap) AndNot(o *Bitmap) {
	for i := range b.words {
		if i >= len(o.words) {
			break
		}
		b.words[i] &^= o.words[i]
	}
}

// Not flips every row in place.
func (b *Bitmap) Not() {
	for i := range b.words {
		b.words[i] = ^b.words[i]
	}
	b.trim()
}

// Clone returns a copy that shares nothing with the original.
func (b *Bitmap) Clone() *Bitmap {
	c := &Bitmap{n: b.n, words: make([]uint64, len(b.words))}
	copy(c.words, b.words)
	return c
}

// Equal compares two bitmaps by the rows they hold. It exists for tests: the
// query path compares counts, not sets.
func (b *Bitmap) Equal(o *Bitmap) bool {
	if b.n != o.n {
		return false
	}
	for i := range b.words {
		if b.words[i] != o.words[i] {
			return false
		}
	}
	return true
}

// Each calls f with every row in the set, ascending, and stops early when f
// returns false. It allocates nothing, which is the difference between it and
// Rows.
func (b *Bitmap) Each(f func(row int) bool) {
	for i, w := range b.words {
		for w != 0 {
			if !f(i*64 + bits.TrailingZeros64(w)) {
				return
			}
			w &= w - 1
		}
	}
}

// Rows materialises the set as row numbers, ascending. A caller that is about
// to fetch every one of them wants this. A caller that is about to intersect
// with something else wants And.
func (b *Bitmap) Rows() []int {
	out := make([]int, 0, b.Count())
	b.Each(func(row int) bool {
		out = append(out, row)
		return true
	})
	return out
}

// trim clears the bits past the last row. Every operation that can set them has
// to call it, because Count and Each read the last word whole and a stray bit
// up there is a row number that does not exist.
func (b *Bitmap) trim() {
	if rem := uint(b.n) & 63; rem != 0 && len(b.words) > 0 {
		b.words[len(b.words)-1] &= ^uint64(0) >> (64 - rem)
	}
}
