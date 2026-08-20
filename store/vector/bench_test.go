package vector_test

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/tamnd/genba/store/column"
	"github.com/tamnd/genba/store/vector"
)

// The sizes the issue asks for, at the widths embedding models actually
// produce. A million vectors at 384 dimensions is 384 MB of codes, which is the
// largest thing in this repository's benchmarks and is the reason the wider
// models are measured at a hundred thousand instead.
var sizes = []struct{ rows, dim int }{
	{100_000, 128},
	{100_000, 768},
	{1_000_000, 128},
	{1_000_000, 384},
}

// k is a page of results, which is what a search actually asks for. The heap
// only ever holds this many, so the size of the answer is not what the scan
// costs.
const k = 20

func BenchmarkSearch(b *testing.B) {
	for _, s := range sizes {
		b.Run(fmt.Sprintf("%dk/%d", s.rows/1000, s.dim), func(b *testing.B) {
			idx, q := corpus(b, s.rows, s.dim)
			b.ResetTimer()
			for b.Loop() {
				if _, err := idx.Search(q, k, nil); err != nil {
					b.Fatalf("searching: %v", err)
				}
			}
			b.ReportMetric(float64(s.rows)*float64(b.N)/b.Elapsed().Seconds()/1e6, "Mrow/s")
		})
	}
}

// BenchmarkSearchFiltered is the shape a real query has, where the reader may
// see some fraction of the corpus. The permission bitmap is the loop rather
// than a check inside it, so these numbers should fall roughly with the
// fraction, and a scan that filtered afterwards would show the same cost at
// every one of them.
func BenchmarkSearchFiltered(b *testing.B) {
	const rows, dim = 100_000, 768
	idx, q := corpus(b, rows, dim)

	for _, percent := range []int{100, 50, 10, 1} {
		b.Run(fmt.Sprintf("%d%%", percent), func(b *testing.B) {
			rnd := rand.New(rand.NewPCG(2121, 41))
			allow := column.NewBitmap(rows)
			for row := range rows {
				if rnd.IntN(100) < percent {
					allow.Set(row)
				}
			}
			visible := allow.Count()
			b.ResetTimer()
			for b.Loop() {
				if _, err := idx.Search(q, k, allow); err != nil {
					b.Fatalf("searching: %v", err)
				}
			}
			b.ReportMetric(float64(visible)*float64(b.N)/b.Elapsed().Seconds()/1e6, "Mrow/s")
		})
	}
}

func BenchmarkNewQuery(b *testing.B) {
	rnd := rand.New(rand.NewPCG(2121, 42))
	v := random(rnd, 768)
	for b.Loop() {
		if _, err := vector.NewQuery(v); err != nil {
			b.Fatalf("building a query: %v", err)
		}
	}
}

// BenchmarkAppend is what an ingestion pipeline pays per document.
//
// It builds a whole small section per iteration rather than appending forever
// into one builder. A builder that grows to a hundred megabytes inside the
// timing loop measures the garbage collector reading a hundred megabyte slice,
// which is around forty percent of the number and is not the encoder.
func BenchmarkAppend(b *testing.B) {
	rnd := rand.New(rand.NewPCG(2121, 43))
	const dim, rows = 768, 4096
	vs := make([][]float32, rows)
	for i := range vs {
		vs[i] = random(rnd, dim)
	}

	b.ResetTimer()
	for b.Loop() {
		bl := builder(b, dim, vector.KindFlat)
		for _, v := range vs {
			if err := bl.Append(v); err != nil {
				b.Fatalf("appending: %v", err)
			}
		}
	}
	b.ReportMetric(float64(rows)*float64(b.N)/b.Elapsed().Seconds()/1e3, "kvec/s")
}

// corpus builds a section once per benchmark case and keeps it, because the
// thing being measured is the scan and not the encoder. The vectors are drawn
// from a normal distribution, which is the shape a real embedding has: the cost
// of a scan does not depend on the values, but the shape of the heap traffic
// does, and a corpus of identical vectors would make every candidate beat the
// root and every candidate lose to it in turn.
func corpus(tb testing.TB, rows, dim int) (vector.Index, *vector.Query) {
	tb.Helper()
	rnd := rand.New(rand.NewPCG(2121, 40))
	b := builder(tb, dim, vector.KindFlat)
	for range rows {
		if err := b.Append(random(rnd, dim)); err != nil {
			tb.Fatalf("appending: %v", err)
		}
	}
	idx := open(tb, b)
	q, err := vector.NewQuery(random(rnd, dim))
	if err != nil {
		tb.Fatalf("building a query: %v", err)
	}
	return idx, q
}
