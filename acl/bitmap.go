package acl

import "math/bits"

// Ordinal is a document's position inside a segment. Ordinals are dense within
// a segment, which is what makes a plain word bitmap the right representation
// here: the visibility set of a person with a hundred groups is computed once
// per segment and then intersected with every posting list the query touches.
type Ordinal uint32

// Bitmap is a dense set of ordinals backed by 64 bit words.
//
// The set operations deliberately mutate the receiver instead of allocating a
// result. A query resolves one visibility bitmap and then reuses it across
// every term in the query, so allocating per operation would put the garbage
// collector on the hot path of every search.
type Bitmap struct {
	words []uint64
}

// NewBitmap returns an empty bitmap with room for n ordinals.
func NewBitmap(n int) *Bitmap {
	if n < 0 {
		n = 0
	}
	return &Bitmap{words: make([]uint64, (n+63)/64)}
}

// Add puts an ordinal in the set, growing the backing store if needed.
func (b *Bitmap) Add(o Ordinal) {
	i := int(o / 64)
	b.grow(i + 1)
	b.words[i] |= 1 << (o % 64)
}

// Remove takes an ordinal out of the set.
func (b *Bitmap) Remove(o Ordinal) {
	i := int(o / 64)
	if i >= len(b.words) {
		return
	}
	b.words[i] &^= 1 << (o % 64)
}

// Contains reports whether the ordinal is in the set.
func (b *Bitmap) Contains(o Ordinal) bool {
	i := int(o / 64)
	if i >= len(b.words) {
		return false
	}
	return b.words[i]&(1<<(o%64)) != 0
}

// Count returns the number of ordinals in the set.
func (b *Bitmap) Count() int {
	n := 0
	for _, w := range b.words {
		n += bits.OnesCount64(w)
	}
	return n
}

// IsEmpty reports whether the set holds nothing. It stops at the first word
// with a bit set rather than counting the whole bitmap.
func (b *Bitmap) IsEmpty() bool {
	for _, w := range b.words {
		if w != 0 {
			return false
		}
	}
	return true
}

// Union adds every ordinal of other to b.
func (b *Bitmap) Union(other *Bitmap) {
	if other == nil {
		return
	}
	b.grow(len(other.words))
	for i, w := range other.words {
		b.words[i] |= w
	}
}

// Intersect keeps only the ordinals present in both bitmaps.
func (b *Bitmap) Intersect(other *Bitmap) {
	if other == nil {
		b.words = b.words[:0]
		return
	}
	for i := range b.words {
		if i < len(other.words) {
			b.words[i] &= other.words[i]
		} else {
			b.words[i] = 0
		}
	}
}

// AndNot removes every ordinal of other from b. This is how a deny set is
// applied, and it runs after the allow union so that deny always wins.
func (b *Bitmap) AndNot(other *Bitmap) {
	if other == nil {
		return
	}
	for i := range b.words {
		if i < len(other.words) {
			b.words[i] &^= other.words[i]
		}
	}
}

// Clone returns an independent copy.
func (b *Bitmap) Clone() *Bitmap {
	c := &Bitmap{words: make([]uint64, len(b.words))}
	copy(c.words, b.words)
	return c
}

// All calls fn for every ordinal in the set, in increasing order, and stops
// early if fn returns false.
func (b *Bitmap) All(fn func(Ordinal) bool) {
	for i, w := range b.words {
		for w != 0 {
			o := Ordinal(i*64 + bits.TrailingZeros64(w))
			if !fn(o) {
				return
			}
			w &= w - 1
		}
	}
}

// Slice returns the ordinals in the set as a sorted slice. It is meant for
// tests and for small result sets, not for the scan path.
func (b *Bitmap) Slice() []Ordinal {
	out := make([]Ordinal, 0, b.Count())
	b.All(func(o Ordinal) bool {
		out = append(out, o)
		return true
	})
	return out
}

func (b *Bitmap) grow(n int) {
	for len(b.words) < n {
		b.words = append(b.words, 0)
	}
}
