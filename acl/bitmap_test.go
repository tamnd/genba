package acl_test

import (
	"slices"
	"testing"

	"github.com/tamnd/genba/acl"
)

func bitmapOf(ordinals ...acl.Ordinal) *acl.Bitmap {
	b := acl.NewBitmap(0)
	for _, o := range ordinals {
		b.Add(o)
	}
	return b
}

func TestBitmapAddContainsRemove(t *testing.T) {
	b := acl.NewBitmap(8)
	if b.Contains(3) {
		t.Fatal("a fresh bitmap contained an ordinal")
	}
	b.Add(3)
	b.Add(200) // past the initial allocation, so this also covers growth
	if !b.Contains(3) || !b.Contains(200) {
		t.Fatal("an added ordinal was not found")
	}
	b.Remove(3)
	if b.Contains(3) {
		t.Fatal("a removed ordinal was still found")
	}
	if got, want := b.Count(), 1; got != want {
		t.Fatalf("Count() = %d, want %d", got, want)
	}
}

func TestBitmapSetOperations(t *testing.T) {
	tests := []struct {
		name string
		op   func(a, b *acl.Bitmap)
		a    []acl.Ordinal
		b    []acl.Ordinal
		want []acl.Ordinal
	}{
		{
			name: "union",
			op:   func(a, b *acl.Bitmap) { a.Union(b) },
			a:    []acl.Ordinal{1, 5},
			b:    []acl.Ordinal{5, 70},
			want: []acl.Ordinal{1, 5, 70},
		},
		{
			name: "intersect",
			op:   func(a, b *acl.Bitmap) { a.Intersect(b) },
			a:    []acl.Ordinal{1, 5, 70},
			b:    []acl.Ordinal{5, 70, 99},
			want: []acl.Ordinal{5, 70},
		},
		{
			name: "intersect with a shorter bitmap clears the tail",
			op:   func(a, b *acl.Bitmap) { a.Intersect(b) },
			a:    []acl.Ordinal{1, 500},
			b:    []acl.Ordinal{1},
			want: []acl.Ordinal{1},
		},
		{
			name: "and not",
			op:   func(a, b *acl.Bitmap) { a.AndNot(b) },
			a:    []acl.Ordinal{1, 5, 70},
			b:    []acl.Ordinal{5},
			want: []acl.Ordinal{1, 70},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := bitmapOf(tt.a...), bitmapOf(tt.b...)
			tt.op(a, b)
			if got := a.Slice(); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBitmapCloneIsIndependent(t *testing.T) {
	a := bitmapOf(1, 2, 3)
	c := a.Clone()
	c.Remove(2)
	if !a.Contains(2) {
		t.Fatal("mutating the clone changed the original")
	}
}

func TestBitmapAllStopsEarly(t *testing.T) {
	b := bitmapOf(1, 2, 3, 4)
	seen := 0
	b.All(func(acl.Ordinal) bool {
		seen++
		return seen < 2
	})
	if seen != 2 {
		t.Fatalf("iteration visited %d ordinals, want 2", seen)
	}
}

func BenchmarkBitmapIntersect(b *testing.B) {
	const n = 1 << 20
	visible, candidates := acl.NewBitmap(n), acl.NewBitmap(n)
	for i := range acl.Ordinal(n) {
		if i%3 == 0 {
			visible.Add(i)
		}
		if i%2 == 0 {
			candidates.Add(i)
		}
	}
	for b.Loop() {
		c := candidates.Clone()
		c.Intersect(visible)
	}
}
