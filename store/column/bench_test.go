package column_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/tamnd/genba/store/column"
)

// The scan throughput per column type, which is the fourth box on the issue.
//
// Every one of them reports rows a second, because that is the figure a query
// planner needs. A filter over a segment costs rows divided by this, and the
// interesting comparison is not between two runs of the same benchmark but
// between the two encodings of the same data: a sorted column is read a run at
// a time and a shuffled one a row at a time, and the gap between them is the
// whole argument for measuring both and keeping the smaller.
//
// They are also what would catch a change that makes the packed reader do more
// per row. Nothing in the counters gate can see that, because the work stays
// the same and only the time moves.

const benchRows = 1_000_000

func BenchmarkScanString(b *testing.B) {
	for _, sorted := range []bool{false, true} {
		c := stringCorpus(b, sorted)
		b.Run(c.Encoding().String(), func(b *testing.B) {
			for b.Loop() {
				if _, err := c.MatchStrings("source-0007"); err != nil {
					b.Fatal(err)
				}
			}
			rowsPerSecond(b)
		})
		b.Run(c.Encoding().String()+"-oneof", func(b *testing.B) {
			set := []string{"source-0007", "source-0031", "source-0099", "source-0500"}
			for b.Loop() {
				if _, err := c.MatchStrings(set...); err != nil {
					b.Fatal(err)
				}
			}
			rowsPerSecond(b)
		})
		b.Run(c.Encoding().String()+"-prefix", func(b *testing.B) {
			for b.Loop() {
				if _, err := c.MatchPrefix("source-00"); err != nil {
					b.Fatal(err)
				}
			}
			rowsPerSecond(b)
		})
	}
}

func BenchmarkScanInt(b *testing.B) {
	for _, sorted := range []bool{false, true} {
		c := intCorpus(b, sorted)
		b.Run(c.Encoding().String(), func(b *testing.B) {
			for b.Loop() {
				if _, err := c.MatchInts(1000, 2000); err != nil {
					b.Fatal(err)
				}
			}
			rowsPerSecond(b)
		})
	}
}

func BenchmarkScanTime(b *testing.B) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, sorted := range []bool{false, true} {
		r := rand.New(rand.NewPCG(2121, 23))
		offsets := make([]int, benchRows)
		for i := range offsets {
			offsets[i] = r.IntN(365 * 24)
		}
		if sorted {
			slices.Sort(offsets)
		}
		bu := column.NewBuilder(column.TypeTime)
		for _, o := range offsets {
			bu.AppendTime(start.Add(time.Duration(o) * time.Hour))
		}
		c := open(b, bu)
		b.Run(c.Encoding().String(), func(b *testing.B) {
			lo, hi := start.AddDate(0, 3, 0), start.AddDate(0, 6, 0)
			for b.Loop() {
				if _, err := c.MatchTimes(lo, hi); err != nil {
					b.Fatal(err)
				}
			}
			rowsPerSecond(b)
		})
	}
}

func BenchmarkScanBool(b *testing.B) {
	for _, sorted := range []bool{false, true} {
		r := rand.New(rand.NewPCG(2121, 29))
		values := make([]int, benchRows)
		for i := range values {
			values[i] = r.IntN(4)
		}
		if sorted {
			slices.Sort(values)
		}
		bu := column.NewBuilder(column.TypeBool)
		for _, v := range values {
			bu.AppendBool(v == 0)
		}
		c := open(b, bu)
		b.Run(c.Encoding().String(), func(b *testing.B) {
			for b.Loop() {
				if _, err := c.MatchBool(true); err != nil {
					b.Fatal(err)
				}
			}
			rowsPerSecond(b)
		})
	}
}

// BenchmarkIntersect is the permission filter on its own, without a scan in
// front of it. It is the operation every query does at least once and the
// reason the bitmap is dense.
func BenchmarkIntersect(b *testing.B) {
	left, right := column.NewBitmap(benchRows), column.NewBitmap(benchRows)
	r := rand.New(rand.NewPCG(2121, 31))
	for i := range benchRows {
		if r.IntN(2) == 0 {
			left.Set(i)
		}
		if r.IntN(4) != 0 {
			right.Set(i)
		}
	}
	for b.Loop() {
		left.And(right)
	}
	rowsPerSecond(b)
}

// BenchmarkValueAt is the other side of a scan: once the bitmap has been
// intersected down to the rows that will be shown, something has to turn them
// back into values.
func BenchmarkValueAt(b *testing.B) {
	for _, sorted := range []bool{false, true} {
		c := stringCorpus(b, sorted)
		b.Run(c.Encoding().String(), func(b *testing.B) {
			i := 0
			for b.Loop() {
				if _, ok := c.StringAt(i % benchRows); !ok {
					b.Fatal("a row with no value")
				}
				i += 7919 // A prime, so the reads are spread rather than sequential.
			}
		})
	}
}

func BenchmarkBuild(b *testing.B) {
	values := benchStrings(false)
	b.Run("string", func(b *testing.B) {
		for b.Loop() {
			bu := column.NewBuilder(column.TypeString)
			for _, v := range values {
				bu.AppendString(v)
			}
			if _, err := bu.Build(); err != nil {
				b.Fatal(err)
			}
		}
		rowsPerSecond(b)
	})
}

func stringCorpus(b *testing.B, sorted bool) *column.Column {
	b.Helper()
	values := benchStrings(sorted)
	bu := column.NewBuilder(column.TypeString)
	for _, v := range values {
		bu.AppendString(v)
	}
	return open(b, bu)
}

// benchStrings is a thousand distinct values over a million rows, which is the
// shape of a source or an owner column on a real corpus.
func benchStrings(sorted bool) []string {
	terms := make([]string, 1000)
	for i := range terms {
		terms[i] = fmt.Sprintf("source-%04d", i)
	}
	r := rand.New(rand.NewPCG(2121, 19))
	out := make([]string, benchRows)
	for i := range out {
		out[i] = terms[r.IntN(len(terms))]
	}
	if sorted {
		slices.Sort(out)
	}
	return out
}

func intCorpus(b *testing.B, sorted bool) *column.Column {
	b.Helper()
	r := rand.New(rand.NewPCG(2121, 37))
	values := make([]int64, benchRows)
	for i := range values {
		values[i] = int64(r.IntN(10_000))
	}
	if sorted {
		slices.Sort(values)
	}
	bu := column.NewBuilder(column.TypeInt)
	for _, v := range values {
		bu.AppendInt(v)
	}
	return open(b, bu)
}

// rowsPerSecond is the figure these benchmarks exist to report. Nanoseconds per
// operation is a number that moves with the corpus size and rows a second is
// not, so it is the one that can be compared between two of these.
func rowsPerSecond(b *testing.B) {
	b.Helper()
	b.ReportMetric(float64(benchRows)*float64(b.N)/b.Elapsed().Seconds(), "rows/s")
}
