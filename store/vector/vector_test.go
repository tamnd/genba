package vector_test

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/tamnd/genba/store/vector"
)

// The four boxes on the issue, in order: the flat scan is exact by construction
// and the quantisation does not reorder anything clear, the scan applies
// visibility as it goes, the benchmarks cover a hundred thousand and a million
// vectors in bench_test.go, and the index kind is a byte in the section rather
// than a branch in the code.

func TestASectionRoundTripsTheDirectionOfEveryVector(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 7))
	const dim, rows = 128, 300

	want := make([][]float32, rows)
	b := builder(t, dim, vector.KindAuto)
	for i := range want {
		want[i] = random(rnd, dim)
		if err := b.Append(want[i]); err != nil {
			t.Fatalf("appending row %d: %v", i, err)
		}
	}
	s := open(t, b)

	if got := s.Rows(); got != rows {
		t.Errorf("Rows returned %d, want %d", got, rows)
	}
	if got := s.Dim(); got != dim {
		t.Errorf("Dim returned %d, want %d", got, dim)
	}
	if got := s.Kind(); got != vector.KindFlat {
		t.Errorf("Kind returned %s, want %s", got, vector.KindFlat)
	}
	if got := s.Metric(); got != vector.MetricCosine {
		t.Errorf("Metric returned %s, want %s", got, vector.MetricCosine)
	}

	// A quantised vector is not the vector that went in and is not supposed to
	// be. What has to survive is the direction, because the direction is the
	// only thing cosine reads.
	worst := 1.0
	for i, v := range want {
		got, ok := s.At(i)
		if !ok {
			t.Fatalf("row %d has no vector", i)
		}
		if c := cosine(v, got); c < worst {
			worst = c
		}
	}
	if worst < 0.9999 {
		t.Errorf("the worst row came back at cosine %.6f to what went in, want at least 0.9999", worst)
	}
	t.Logf("%d rows of %d dimensions in %d bytes, the worst at cosine %.6f to the original", rows, dim, s.Size(), worst)
}

func TestARowWithNoVectorIsNeverScored(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 8))
	const dim = 64

	b := builder(t, dim, vector.KindFlat)
	q := random(rnd, dim)
	// Row 0 is the query itself, so if a null row could ever be returned it
	// would have to displace the best possible match to do it.
	if err := b.Append(q); err != nil {
		t.Fatalf("appending: %v", err)
	}
	for range 20 {
		b.AppendNull()
	}
	s := open(t, b)

	if s.Has(3) {
		t.Error("Has says a null row carries a vector")
	}
	if _, ok := s.At(3); ok {
		t.Error("At returned a vector for a null row")
	}

	got := search(t, s, q, 21, nil)
	if len(got) != 1 {
		t.Fatalf("a section with one vector and twenty nulls returned %d matches, want 1", len(got))
	}
	if got[0].Row != 0 {
		t.Errorf("the one match is row %d, want 0", got[0].Row)
	}
}

func TestAZeroVectorIsRefusedRatherThanStored(t *testing.T) {
	b := builder(t, 8, vector.KindAuto)
	if err := b.Append(make([]float32, 8)); !errors.Is(err, vector.ErrZero) {
		t.Errorf("appending a zero vector returned %v, want ErrZero", err)
	}
	if _, err := vector.NewQuery(make([]float32, 8)); !errors.Is(err, vector.ErrZero) {
		t.Errorf("a zero query returned %v, want ErrZero", err)
	}
	if got := b.Rows(); got != 0 {
		t.Errorf("the refused vector still added %d rows", got)
	}
}

func TestAVectorOfTheWrongWidthIsRefused(t *testing.T) {
	b := builder(t, 8, vector.KindAuto)
	if err := b.Append(make([]float32, 9)); !errors.Is(err, vector.ErrDim) {
		t.Errorf("appending nine components to an eight wide section returned %v, want ErrDim", err)
	}
	if _, err := vector.NewBuilder(0, vector.KindAuto); !errors.Is(err, vector.ErrDim) {
		t.Errorf("a section of no dimensions returned %v, want ErrDim", err)
	}
	if _, err := vector.NewBuilder(vector.MaxDim+1, vector.KindAuto); !errors.Is(err, vector.ErrDim) {
		t.Errorf("a section wider than MaxDim returned %v, want ErrDim", err)
	}
}

func TestAQueryFromAnotherSegmentIsRefused(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 9))
	b := builder(t, 16, vector.KindAuto)
	if err := b.Append(random(rnd, 16)); err != nil {
		t.Fatalf("appending: %v", err)
	}
	s := open(t, b)

	q, err := vector.NewQuery(random(rnd, 32))
	if err != nil {
		t.Fatalf("building a query: %v", err)
	}
	if _, err := s.Search(q, 10, nil); !errors.Is(err, vector.ErrDim) {
		t.Errorf("a 32 wide query against a 16 wide section returned %v, want ErrDim", err)
	}
	if _, err := s.Search(nil, 10, nil); !errors.Is(err, vector.ErrQuery) {
		t.Errorf("a search with no query returned %v, want ErrQuery", err)
	}
}

// TestTheIndexKindIsAByteInTheSection is the box that says choosing an index is
// a configuration decision rather than a code change. The kind is written into
// the header, Open dispatches on it, and a caller holds an interface either way.
func TestTheIndexKindIsAByteInTheSection(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 10))
	for _, want := range []vector.Kind{vector.KindAuto, vector.KindFlat} {
		b := builder(t, 8, want)
		if err := b.Append(random(rnd, 8)); err != nil {
			t.Fatalf("appending: %v", err)
		}
		raw, err := b.Build()
		if err != nil {
			t.Fatalf("building with kind %s: %v", want, err)
		}
		// Byte two of the header, which is where a reader looks before it
		// decides which implementation to hand back.
		if got := vector.Kind(raw[2]); got != vector.KindFlat {
			t.Errorf("a section asked for %s carries kind %s, want %s", want, got, vector.KindFlat)
		}
		s, err := vector.Open(raw)
		if err != nil {
			t.Fatalf("opening: %v", err)
		}
		if got := s.Kind(); got != vector.KindFlat {
			t.Errorf("the opened index reports %s, want %s", got, vector.KindFlat)
		}
	}

	if _, err := vector.NewBuilder(8, vector.Kind(9)); !errors.Is(err, vector.ErrKind) {
		t.Errorf("a builder for an unknown kind returned %v, want ErrKind", err)
	}
}

// TestOpenRefusesBytesThatAreNotASection covers the ways a section arrives
// wrong. These bytes come off a disk, and a reader that trusts a length field
// turns a bad sector into a dead process.
func TestOpenRefusesBytesThatAreNotASection(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 11))
	good := func() []byte {
		b := builder(t, 16, vector.KindFlat)
		for range 4 {
			if err := b.Append(random(rnd, 16)); err != nil {
				t.Fatalf("appending: %v", err)
			}
		}
		raw, err := b.Build()
		if err != nil {
			t.Fatalf("building: %v", err)
		}
		return raw
	}

	cases := []struct {
		name   string
		damage func(b []byte) []byte
		want   error
	}{
		{"empty", func([]byte) []byte { return nil }, vector.ErrFormat},
		{"a header and nothing else", func(b []byte) []byte { return b[:8] }, vector.ErrFormat},
		{"a version from the future", func(b []byte) []byte { b[0] = vector.Version + 1; return b }, vector.ErrVersion},
		{"a flag this build does not know", func(b []byte) []byte { b[3] = 1; return b }, vector.ErrVersion},
		{"reserved bytes that are not zero", func(b []byte) []byte { b[12] = 1; return b }, vector.ErrFormat},
		{"a metric this build does not have", func(b []byte) []byte { b[1] = 9; return b }, vector.ErrFormat},
		{"an index kind this build cannot read", func(b []byte) []byte { b[2] = 9; return b }, vector.ErrKind},
		{"no dimensions", func(b []byte) []byte { b[4] = 0; b[5] = 0; return b }, vector.ErrDim},
		{"more dimensions than the limit", func(b []byte) []byte { b[4], b[5], b[6], b[7] = 0xff, 0xff, 0xff, 0xff; return b }, vector.ErrDim},
		{"a row count the bytes cannot hold", func(b []byte) []byte { b[8], b[9], b[10], b[11] = 0xff, 0xff, 0xff, 0x0f; return b }, vector.ErrFormat},
		{"a truncated copy", func(b []byte) []byte { return b[:len(b)-1] }, vector.ErrFormat},
		{"bytes after the end", func(b []byte) []byte { return append(b, 0) }, vector.ErrFormat},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := vector.Open(c.damage(good())); !errors.Is(err, c.want) {
				t.Errorf("Open returned %v, want %v", err, c.want)
			}
		})
	}
}

// FuzzOpen has one contract, which is that nothing it is fed ever panics. A
// reader that panics on a damaged file turns a recoverable segment into an
// outage.
func FuzzOpen(f *testing.F) {
	rnd := rand.New(rand.NewPCG(2121, 12))
	b := builder(f, 8, vector.KindFlat)
	for range 3 {
		if err := b.Append(random(rnd, 8)); err != nil {
			f.Fatalf("appending: %v", err)
		}
	}
	b.AppendNull()
	raw, err := b.Build()
	if err != nil {
		f.Fatalf("building: %v", err)
	}
	f.Add(raw)
	f.Add([]byte{})
	f.Add(make([]byte, 16))

	q, err := vector.NewQuery(random(rnd, 8))
	if err != nil {
		f.Fatalf("building a query: %v", err)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		s, err := vector.Open(in)
		if err != nil {
			return
		}
		for row := range s.Rows() + 2 {
			s.Has(row)
			s.At(row)
		}
		if s.Dim() == q.Dim() {
			if _, err := s.Search(q, 5, nil); err != nil {
				t.Fatalf("searching a section that opened: %v", err)
			}
		}
	})
}

// builder is NewBuilder with the error handled once.
func builder(tb testing.TB, dim int, kind vector.Kind) *vector.Builder {
	tb.Helper()
	b, err := vector.NewBuilder(dim, kind)
	if err != nil {
		tb.Fatalf("a builder for %d dimensions: %v", dim, err)
	}
	return b
}

// open builds and opens in one step, which is what every test that is not about
// the encoding wants.
func open(tb testing.TB, b *vector.Builder) vector.Index {
	tb.Helper()
	raw, err := b.Build()
	if err != nil {
		tb.Fatalf("building: %v", err)
	}
	s, err := vector.Open(raw)
	if err != nil {
		tb.Fatalf("opening: %v", err)
	}
	return s
}

// random returns a vector from a normal distribution, which is the shape a real
// embedding has: no component dominates and the length carries no meaning.
func random(rnd *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(rnd.NormFloat64())
	}
	return v
}

// cosine is the exact answer, in float64, which is what everything here is
// measured against.
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}
