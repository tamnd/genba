package kura_test

import (
	"errors"
	"math"
	"testing"

	"github.com/tamnd/genba/store/kura"
)

// These tests run in both builds. The ones that need the engine skip without
// it, and the one that checks the refusal skips with it, so a plain
// go test ./... covers the default build and
// CGO_ENABLED=1 go test -tags kura ./... covers the linked one.

func linked(t *testing.T) {
	t.Helper()
	if err := kura.Available(); err != nil {
		t.Skip("the engine is not linked into this build: ", err)
	}
}

// TestTheDefaultBuildSaysWhichBuildToUse is the whole of the pure Go side. A
// caller that asked for the engine and did not get it has to be told, and told
// something it can act on.
func TestTheDefaultBuildSaysWhichBuildToUse(t *testing.T) {
	if kura.Available() == nil {
		t.Skip("this build has the engine linked in")
	}

	calls := map[string]func() error{
		"Available": kura.Available,
		"NewBitmap": func() error { _, err := kura.NewBitmap(); return err },
		"EncodePostings": func() error {
			_, err := kura.EncodePostings([]uint32{1, 2, 3})
			return err
		},
		"PostingsLen":      func() error { _, err := kura.PostingsLen(nil); return err },
		"DecodePostings":   func() error { _, err := kura.DecodePostings(nil); return err },
		"PostingsContains": func() error { _, err := kura.PostingsContains(nil, 1); return err },
		"Cosine":           func() error { _, err := kura.Cosine([]float32{1}, []float32{1}); return err },
		"Quantise":         func() error { _, _, err := kura.Quantise([]float32{1}); return err },
		"DotQuantised": func() error {
			_, err := kura.DotQuantised([]int8{1}, 1, []int8{1}, 1)
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, kura.ErrUnavailable) {
			t.Errorf("%s in a build without the engine gave %v", name, err)
		}
	}
	if v := kura.Version(); v != "" {
		t.Errorf("a build without the engine reports the engine version as %q", v)
	}
}

func TestABitmapHoldsTheIdsPutInIt(t *testing.T) {
	linked(t)
	b, err := kura.NewBitmap()
	if err != nil {
		t.Fatalf("allocating: %v", err)
	}
	defer b.Close()

	for _, id := range []uint32{7, 1, 4, 7} {
		if err := b.Insert(id); err != nil {
			t.Fatalf("inserting %d: %v", id, err)
		}
	}
	if n, err := b.Len(); err != nil || n != 3 {
		t.Errorf("the bitmap holds %d %v and three distinct ids went in", n, err)
	}
	if got, err := b.Contains(4); err != nil || !got {
		t.Errorf("4 is in the bitmap as %v %v", got, err)
	}
	if got, err := b.Contains(5); err != nil || got {
		t.Errorf("5 is in the bitmap as %v %v", got, err)
	}
	if err := b.Remove(4); err != nil {
		t.Fatalf("removing: %v", err)
	}
	got, err := b.Array()
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 7 {
		t.Errorf("the bitmap reads back as %v and should be 1 and 7 ascending", got)
	}
}

func TestAnEmptyBitmapReadsBackEmpty(t *testing.T) {
	linked(t)
	b, err := kura.NewBitmap()
	if err != nil {
		t.Fatalf("allocating: %v", err)
	}
	defer b.Close()
	got, err := b.Array()
	if err != nil || len(got) != 0 {
		t.Errorf("an empty bitmap reads back as %v %v", got, err)
	}
}

func TestBitmapsCombine(t *testing.T) {
	linked(t)
	build := func(ids ...uint32) *kura.Bitmap {
		t.Helper()
		b, err := kura.NewBitmap()
		if err != nil {
			t.Fatalf("allocating: %v", err)
		}
		for _, id := range ids {
			if err := b.Insert(id); err != nil {
				t.Fatalf("inserting: %v", err)
			}
		}
		return b
	}

	left, right := build(1, 2, 3, 4), build(3, 4, 5)
	defer left.Close()
	defer right.Close()

	if err := left.Intersect(right); err != nil {
		t.Fatalf("intersecting: %v", err)
	}
	if got, err := left.Array(); err != nil || len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Errorf("the intersection is %v %v and should be 3 and 4", got, err)
	}
	if err := left.Union(right); err != nil {
		t.Fatalf("uniting: %v", err)
	}
	if got, err := left.Array(); err != nil || len(got) != 3 {
		t.Errorf("the union is %v %v and should hold three ids", got, err)
	}
}

// TestAClosedBitmapIsARefusalRatherThanACrash is the reason Close sets the
// pointer to nil. Handing a freed pointer back to the engine is a use after
// free, and a second Close is the easiest way to do it by accident.
func TestAClosedBitmapIsARefusalRatherThanACrash(t *testing.T) {
	linked(t)
	b, err := kura.NewBitmap()
	if err != nil {
		t.Fatalf("allocating: %v", err)
	}
	b.Close()
	b.Close()
	b.Close()

	other, err := kura.NewBitmap()
	if err != nil {
		t.Fatalf("allocating: %v", err)
	}
	defer other.Close()

	for name, call := range map[string]func() error{
		"Insert":          func() error { return b.Insert(1) },
		"Remove":          func() error { return b.Remove(1) },
		"Contains":        func() error { _, err := b.Contains(1); return err },
		"Len":             func() error { _, err := b.Len(); return err },
		"Array":           func() error { _, err := b.Array(); return err },
		"Intersect":       func() error { return b.Intersect(other) },
		"Union":           func() error { return b.Union(other) },
		"Intersect a nil": func() error { return other.Intersect(nil) },
		"Union a closed":  func() error { return other.Union(b) },
	} {
		if err := call(); !errors.Is(err, kura.ErrClosed) {
			t.Errorf("%s on a closed bitmap gave %v", name, err)
		}
	}
}

func TestPostingsRoundTrip(t *testing.T) {
	linked(t)
	for _, n := range []int{0, 1, 2, 127, 128, 1000, 10_000} {
		ids := make([]uint32, n)
		for i := range ids {
			ids[i] = uint32(i) * 7
		}
		encoded, err := kura.EncodePostings(ids)
		if err != nil {
			t.Fatalf("encoding %d ids: %v", n, err)
		}
		if got, err := kura.PostingsLen(encoded); err != nil || got != n {
			t.Errorf("the header of %d ids says %d %v", n, got, err)
		}
		got, err := kura.DecodePostings(encoded)
		if err != nil {
			t.Fatalf("decoding %d ids: %v", n, err)
		}
		if len(got) != n {
			t.Fatalf("%d ids came back as %d", n, len(got))
		}
		for i := range ids {
			if got[i] != ids[i] {
				t.Fatalf("id %d of %d came back as %d and should be %d", i, n, got[i], ids[i])
			}
		}
		if n > 0 {
			if in, err := kura.PostingsContains(encoded, ids[n/2]); err != nil || !in {
				t.Errorf("an id that is in a list of %d reads back as %v %v", n, in, err)
			}
			if in, err := kura.PostingsContains(encoded, 3); err != nil || in {
				t.Errorf("an id that is not in a list of %d reads back as %v %v", n, in, err)
			}
		}
	}
}

func TestPostingsRefuseIdsThatAreNotAscending(t *testing.T) {
	linked(t)
	_, err := kura.EncodePostings([]uint32{5, 4})
	if !errors.Is(err, kura.StatusNotSorted) {
		t.Errorf("encoding descending ids gave %v", err)
	}
}

// TestDamagedPostingsAreAnErrorRatherThanACrash is the box about an abort in
// the library never taking the process down. Every one of these is bytes the
// engine did not write, and it has to say so.
func TestDamagedPostingsAreAnErrorRatherThanACrash(t *testing.T) {
	linked(t)
	good, err := kura.EncodePostings([]uint32{1, 2, 3, 4, 5})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"nothing at all", nil},
		{"one byte", []byte{0}},
		{"not a kura file", []byte("this is not a posting list at all")},
		{"the header only", good[:min(4, len(good))]},
		{"truncated", good[:len(good)-1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := kura.PostingsLen(tc.data); err == nil {
				t.Error("reading the length succeeded")
			}
			if _, err := kura.DecodePostings(tc.data); err == nil {
				t.Error("decoding succeeded")
			}
			if _, err := kura.PostingsContains(tc.data, 1); err == nil {
				t.Error("a membership question succeeded")
			}
		})
	}
}

// TestAHeaderThatClaimsMoreIdsThanItCouldHoldIsRefused is a regression test for
// what the fuzzer found. The count is a variable length integer at the front of
// the list and nothing in the format bounds it, so seven bytes can claim four
// billion ids. Sizing a destination from that number is a seventeen gigabyte
// allocation from an input a caller was handed.
func TestAHeaderThatClaimsMoreIdsThanItCouldHoldIsRefused(t *testing.T) {
	linked(t)
	// Four billion as a variable length integer, then no skip entries and no
	// blocks, which is a header a reader parses happily.
	data := []byte{0xff, 0xff, 0xff, 0xff, 0x0f, 0x00, 0x00}

	n, err := kura.PostingsLen(data)
	if err != nil {
		t.Fatalf("reading the length of a header that parses: %v", err)
	}
	if n <= len(data) {
		t.Fatalf("the header claims %d ids in %d bytes, which is not the case this test is about", n, len(data))
	}
	if got, err := kura.DecodePostings(data); !errors.Is(err, kura.StatusTruncated) {
		t.Errorf("decoding a header claiming %d ids gave %d ids and %v", n, len(got), err)
	}
}

func TestVectorsScore(t *testing.T) {
	linked(t)
	a := []float32{1, 0, 0}
	if got, err := kura.Cosine(a, a); err != nil || math.Abs(float64(got)-1) > 1e-5 {
		t.Errorf("a vector against itself scores %v %v and should be one", got, err)
	}
	if got, err := kura.Cosine(a, []float32{0, 1, 0}); err != nil || math.Abs(float64(got)) > 1e-5 {
		t.Errorf("two orthogonal vectors score %v %v and should be zero", got, err)
	}
	if _, err := kura.Cosine(a, []float32{1, 0}); !errors.Is(err, kura.StatusDimensionMismatch) {
		t.Errorf("two vectors of different lengths gave %v", err)
	}
}

func TestQuantisingKeepsTheDotProductClose(t *testing.T) {
	linked(t)
	a := make([]float32, 128)
	b := make([]float32, 128)
	var want float64
	for i := range a {
		a[i] = float32(math.Sin(float64(i)))
		b[i] = float32(math.Cos(float64(i) / 3))
		want += float64(a[i]) * float64(b[i])
	}

	qa, sa, err := kura.Quantise(a)
	if err != nil {
		t.Fatalf("quantising: %v", err)
	}
	qb, sb, err := kura.Quantise(b)
	if err != nil {
		t.Fatalf("quantising: %v", err)
	}
	if len(qa) != len(a) || len(qb) != len(b) {
		t.Fatalf("quantising 128 dimensions gave %d and %d", len(qa), len(qb))
	}

	got, err := kura.DotQuantised(qa, sa, qb, sb)
	if err != nil {
		t.Fatalf("scoring: %v", err)
	}
	// One signed byte per dimension is about two decimal digits, so a percent
	// over a hundred and twenty eight dimensions is the shape of answer to
	// expect. This is a check that the scale is applied at all, not a check on
	// the quantiser's accuracy, which is the engine's business.
	if diff := math.Abs(float64(got) - want); diff > 0.05*math.Abs(want)+0.1 {
		t.Errorf("the quantised dot product is %v and the exact one is %v", got, want)
	}
	if _, err := kura.DotQuantised(qa, sa, qb[:64], sb); !errors.Is(err, kura.StatusDimensionMismatch) {
		t.Errorf("scoring vectors of different lengths gave %v", err)
	}
}

func TestEmptyInputsAreLegal(t *testing.T) {
	linked(t)
	if _, err := kura.EncodePostings(nil); err != nil {
		t.Errorf("encoding no ids at all gave %v", err)
	}
	if got, scale, err := kura.Quantise(nil); err != nil || len(got) != 0 || scale != 0 {
		t.Errorf("quantising nothing gave %v %v %v", got, scale, err)
	}
	if got, err := kura.Cosine(nil, nil); err != nil {
		t.Errorf("the cosine of two empty vectors gave %v %v", got, err)
	}
}

func TestTheEngineNamesItself(t *testing.T) {
	linked(t)
	if kura.Version() == "" {
		t.Error("the engine reports no version")
	}
}

// FuzzPostingsSurvivesAnything is the same contract the pure Go formats have,
// across a boundary where getting it wrong is a segmentation fault rather than
// a Go panic. It does nothing at all in a build without the engine, which is
// the honest outcome: there is nothing to fuzz.
func FuzzPostingsSurvivesAnything(f *testing.F) {
	if kura.Available() != nil {
		f.Skip("the engine is not linked into this build")
	}
	if good, err := kura.EncodePostings([]uint32{1, 2, 3, 500, 100_000}); err == nil {
		f.Add(good)
	}
	f.Add([]byte{})
	f.Add(make([]byte, 32))

	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := kura.PostingsLen(data)
		if err == nil && n < 0 {
			t.Fatalf("the header claims %d ids", n)
		}
		got, err := kura.DecodePostings(data)
		// An id costs at least a byte, so no honest list decodes to more ids
		// than the bytes it came in. This is the invariant that keeps a corrupt
		// header from turning into an allocation the size of the header's
		// imagination.
		if err == nil && len(got) > len(data) {
			t.Fatalf("%d bytes decoded to %d ids", len(data), len(got))
		}
		_, _ = kura.PostingsContains(data, 1)
	})
}
