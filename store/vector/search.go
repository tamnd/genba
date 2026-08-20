package vector

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/tamnd/genba/store/column"
)

// Query is a search vector, normalised and quantised once.
//
// It is a value rather than a parameter to [Set.Search] because a corpus is
// many segments and the same question is asked of every one of them. Quantising
// the query per segment would be a few hundred divisions repeated for no
// reason, and worse, it would be a place where two segments could end up
// comparing against slightly different numbers.
type Query struct {
	codes []int8
	scale float32
}

// NewQuery quantises a search vector.
//
// It is quantised the same way the stored vectors are, rather than kept at full
// precision, because the alternative is converting a byte to a float on every
// component of every candidate. Both sides being bytes is what makes the inner
// loop an integer multiply and add, and the accuracy it costs is well inside
// the gap between a relevant document and an irrelevant one.
func NewQuery(v []float32) (*Query, error) {
	if len(v) == 0 || len(v) > MaxDim {
		return nil, fmt.Errorf("%w: %d dimensions, the limit is %d", ErrDim, len(v), MaxDim)
	}
	scale, unit := quantiser(v)
	if scale == 0 {
		return nil, ErrZero
	}
	q := &Query{codes: make([]int8, len(v)), scale: float32(scale)}
	for i, x := range v {
		q.codes[i] = round(float64(x) * unit)
	}
	return q, nil
}

// Dim is the width of the query.
func (q *Query) Dim() int { return len(q.codes) }

// Match is one row and what it scored.
type Match struct {
	// Row is the segment row, which is the same row the columns of that segment
	// use.
	Row int

	// Score is the cosine of the angle between the query and the stored vector,
	// so it runs from minus one to one and larger is closer.
	Score float32
}

// ErrQuery is a search with no query in it, which is a caller bug rather than
// anything about the data.
var ErrQuery = errors.New("vector: no query")

// Search returns the best k rows for a query, restricted to the rows a
// principal may read.
//
// allow is the permission filter and it is the loop, not a check inside one. A
// nil allow means every row, which is what a caller with no principal to apply
// passes, and it is the only way to search rows nobody vouched for. Any other
// bitmap and the scan visits exactly the rows it holds, so a document the
// reader may not see is never scored, cannot enter the heap and cannot be
// counted anywhere downstream.
//
// Rows with no vector are skipped, whatever the filter says about them.
//
// The result is ordered by score, highest first, and by row within a tie, so
// two runs of the same query over the same segment return the same list in the
// same order.
func (s *Set) Search(q *Query, k int, allow *column.Bitmap) ([]Match, error) {
	if q == nil {
		return nil, ErrQuery
	}
	if len(q.codes) != s.dim {
		return nil, fmt.Errorf("%w: the query has %d and the section has %d", ErrDim, len(q.codes), s.dim)
	}
	if k <= 0 || s.rows == 0 {
		return nil, nil
	}
	if k > s.rows {
		k = s.rows
	}

	h := &top{k: k, m: make([]Match, 0, k)}
	if allow == nil {
		for row := range s.rows {
			s.score(q, row, h)
		}
	} else {
		// Each walks the set ascending, so a row past the end of this section
		// means every row after it is too. That happens when the bitmap came
		// from a longer segment than this one, and stopping is both correct and
		// the cheap thing to do.
		allow.Each(func(row int) bool {
			if row >= s.rows {
				return false
			}
			s.score(q, row, h)
			return true
		})
	}
	return h.sorted(), nil
}

// score computes one row and offers it to the heap.
func (s *Set) score(q *Query, row int, h *top) {
	scale := math.Float32frombits(le.Uint32(s.scales[row*scaleSize:]))
	if scale == 0 {
		return
	}
	at := row * s.dim
	h.offer(Match{Row: row, Score: q.scale * scale * float32(dot(q.codes, s.codes[at:at+s.dim]))})
}

// dot is the inner product of a quantised query and a row of quantised codes.
//
// It accumulates in four independent sums so that the additions do not form one
// dependency chain the processor has to walk a step at a time. The slices are
// cut to the same length first, which is what lets the bounds check disappear
// from the body.
//
// An int32 is enough by a wide margin: the largest term is 127 times 127, and
// [MaxDim] of them is three orders of magnitude short of overflowing.
func dot(q []int8, c []byte) int32 {
	c = c[:len(q)]
	var a0, a1, a2, a3 int32
	for len(q) >= 8 {
		x, y := q[:8:8], c[:8:8]
		a0 += int32(x[0])*int32(int8(y[0])) + int32(x[4])*int32(int8(y[4]))
		a1 += int32(x[1])*int32(int8(y[1])) + int32(x[5])*int32(int8(y[5]))
		a2 += int32(x[2])*int32(int8(y[2])) + int32(x[6])*int32(int8(y[6]))
		a3 += int32(x[3])*int32(int8(y[3])) + int32(x[7])*int32(int8(y[7]))
		q, c = q[8:], c[8:]
	}
	for i := range q {
		a0 += int32(q[i]) * int32(int8(c[i]))
	}
	return a0 + a1 + a2 + a3
}

// top keeps the best k matches seen.
//
// A heap rather than a sorted slice because the whole point of a scan is that
// almost nothing it looks at is worth keeping: once the heap is full, a
// candidate that does not beat the worst of it costs one comparison and
// nothing else. Sorting everything and cutting would be the same amount of
// scanning and a million element sort on top of it.
//
// It is a minimum heap, so the root is the worst thing being kept, which is the
// only element a new candidate has to be compared against.
type top struct {
	k int
	m []Match
}

// worse orders two matches. A lower score is worse, and on a tie a higher row
// is worse, which is what makes the output stable: of two documents that score
// identically, the one earlier in the segment is the one kept.
func worse(a, b Match) bool {
	if a.Score != b.Score {
		return a.Score < b.Score
	}
	return a.Row > b.Row
}

func (t *top) offer(m Match) {
	if len(t.m) < t.k {
		t.m = append(t.m, m)
		t.up(len(t.m) - 1)
		return
	}
	if !worse(t.m[0], m) {
		return
	}
	t.m[0] = m
	t.down(0)
}

func (t *top) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !worse(t.m[i], t.m[parent]) {
			return
		}
		t.m[i], t.m[parent] = t.m[parent], t.m[i]
		i = parent
	}
}

func (t *top) down(i int) {
	for {
		left, smallest := 2*i+1, i
		if left < len(t.m) && worse(t.m[left], t.m[smallest]) {
			smallest = left
		}
		if right := left + 1; right < len(t.m) && worse(t.m[right], t.m[smallest]) {
			smallest = right
		}
		if smallest == i {
			return
		}
		t.m[i], t.m[smallest] = t.m[smallest], t.m[i]
		i = smallest
	}
}

// sorted returns what the heap holds, best first. It is called once per search
// over at most k elements, so the sort is not on any path that matters.
func (t *top) sorted() []Match {
	slices.SortFunc(t.m, func(a, b Match) int {
		switch {
		case worse(a, b):
			return 1
		case worse(b, a):
			return -1
		}
		return 0
	})
	if len(t.m) == 0 {
		return nil
	}
	return t.m
}
