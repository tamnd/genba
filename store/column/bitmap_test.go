package column_test

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tamnd/genba/store/column"
)

func TestABitmapHoldsTheRowsPutInIt(t *testing.T) {
	b := column.NewBitmap(200)
	for _, row := range []int{0, 1, 63, 64, 65, 127, 128, 199} {
		b.Set(row)
	}
	if got := b.Rows(); !slices.Equal(got, []int{0, 1, 63, 64, 65, 127, 128, 199}) {
		t.Errorf("the rows are %v", got)
	}
	b.Clear(64)
	if b.Get(64) || !b.Get(65) {
		t.Error("clearing row 64 took row 65 with it")
	}
	if got := b.Count(); got != 7 {
		t.Errorf("%d rows are set and seven should be", got)
	}
}

// TestARowOutsideTheBitmapIsIgnored keeps a truncated column from taking the
// process down. A run that claims more rows than there are is a wrong answer,
// not a crash.
func TestARowOutsideTheBitmapIsIgnored(t *testing.T) {
	b := column.NewBitmap(10)
	b.Set(-1)
	b.Set(10)
	b.Set(1 << 40)
	b.Clear(-1)
	b.Clear(10)
	if !b.Empty() || b.Get(-1) || b.Get(10) {
		t.Errorf("%d rows outside the bitmap got in", b.Count())
	}
	b.SetRange(-5, 1000)
	if got := b.Count(); got != 10 {
		t.Errorf("a range over everything set %d of ten rows", got)
	}
}

// TestSetRangeAgreesWithSettingEachRow is what the run length scan depends on.
// It fills whole words, so every boundary between them is a chance to be off by
// one.
func TestSetRangeAgreesWithSettingEachRow(t *testing.T) {
	const n = 300
	for lo := range n {
		for _, hi := range []int{lo, lo + 1, lo + 63, lo + 64, lo + 65, lo + 130, n} {
			got := column.NewBitmap(n)
			got.SetRange(lo, hi)
			want := column.NewBitmap(n)
			for i := lo; i < hi && i < n; i++ {
				want.Set(i)
			}
			if !got.Equal(want) {
				t.Fatalf("SetRange(%d, %d) set %d rows and setting each gives %d", lo, hi, got.Count(), want.Count())
			}
		}
	}
}

func TestTheOperationsAgreeWithSets(t *testing.T) {
	const n = 500
	r := rand.New(rand.NewPCG(2121, 17))
	left, right := column.NewBitmap(n), column.NewBitmap(n)
	var inLeft, inRight [n]bool
	for i := range n {
		if inLeft[i] = r.IntN(2) == 0; inLeft[i] {
			left.Set(i)
		}
		if inRight[i] = r.IntN(2) == 0; inRight[i] {
			right.Set(i)
		}
	}

	cases := []struct {
		name string
		op   func(*column.Bitmap)
		want func(i int) bool
	}{
		{"and", func(b *column.Bitmap) { b.And(right) }, func(i int) bool { return inLeft[i] && inRight[i] }},
		{"or", func(b *column.Bitmap) { b.Or(right) }, func(i int) bool { return inLeft[i] || inRight[i] }},
		{"andnot", func(b *column.Bitmap) { b.AndNot(right) }, func(i int) bool { return inLeft[i] && !inRight[i] }},
		{"not", func(b *column.Bitmap) { b.Not() }, func(i int) bool { return !inLeft[i] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := left.Clone()
			tc.op(got)
			want := column.NewBitmap(n)
			for i := range n {
				if tc.want(i) {
					want.Set(i)
				}
			}
			if !got.Equal(want) {
				t.Errorf("%s gave %d rows and the set answer is %d", tc.name, got.Count(), want.Count())
			}
		})
	}
	if left.Count() == 0 {
		t.Error("the operations wrote through to the original")
	}
}

// TestNotDoesNotInventRows is the reason trim exists. Five hundred rows is not
// a multiple of sixty four, and flipping the last word turns the padding above
// the last row into rows that do not exist.
func TestNotDoesNotInventRows(t *testing.T) {
	b := column.NewBitmap(100)
	b.Not()
	if got := b.Count(); got != 100 {
		t.Errorf("flipping an empty bitmap of a hundred rows gives %d", got)
	}
	for _, row := range b.Rows() {
		if row >= 100 {
			t.Fatalf("row %d exists after flipping a bitmap of a hundred rows", row)
		}
	}
}

// TestIntersectingWithAShorterBitmapClearsTheTail is the safe direction to be
// wrong in. Rows the other side never vouched for must not survive the
// permission filter.
func TestIntersectingWithAShorterBitmapClearsTheTail(t *testing.T) {
	long := column.NewBitmap(200)
	long.SetRange(0, 200)
	short := column.NewBitmap(70)
	short.SetRange(0, 70)

	got := long.Clone()
	got.And(short)
	if c := got.Count(); c != 70 {
		t.Errorf("intersecting two hundred rows with seventy left %d", c)
	}

	got = long.Clone()
	got.AndNot(short)
	if c := got.Count(); c != 130 {
		t.Errorf("subtracting seventy rows from two hundred left %d", c)
	}

	got = short.Clone()
	got.Or(long)
	if c := got.Count(); c != 70 {
		t.Errorf("a union into a seventy row bitmap holds %d rows", c)
	}
}

func TestEachStopsWhenItIsToldTo(t *testing.T) {
	b := column.NewBitmap(1000)
	b.SetRange(0, 1000)
	seen := 0
	b.Each(func(int) bool {
		seen++
		return seen < 3
	})
	if seen != 3 {
		t.Errorf("Each visited %d rows after being told to stop at three", seen)
	}
}
