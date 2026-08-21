package index_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/benchcorpus"
	"github.com/tamnd/genba/index"
)

// These run against the fixed corpus, which is generated once and cached under
// testdata. See benchcorpus for what is in it and why. The first run
// builds it and is slow; every run after that is not.
//
// The point of measuring per query class rather than over a mixed workload is
// that the classes differ in cost by two orders of magnitude, and an average
// over the mix can be met by being fast at the cheap ones. A regression in the
// expensive class is exactly the regression somebody notices.

func searcher(b *testing.B) (*index.Searcher, *acl.Principal) {
	b.Helper()
	st, spec := benchcorpus.Fixture(b)
	// A fixed clock, because the recency prior is part of the score and a clock
	// that moves makes today's number incomparable with last month's.
	s := index.New(st, index.WithClock(func() time.Time { return benchcorpus.Epoch }))
	return s, spec.Principal()
}

// run measures one query class, cycling through the checked in queries of that
// class so the measurement is over a distribution rather than over one lucky
// term that happens to be in the page cache.
func run(b *testing.B, class benchcorpus.Class, shape func(index.Query) index.Query) {
	s, p := searcher(b)
	queries := benchcorpus.ByClass(benchcorpus.Queries())[class]
	if len(queries) == 0 {
		b.Fatalf("the query set has no %s queries", class)
	}

	parsed := make([]index.Query, len(queries))
	for i, q := range queries {
		parsed[i] = index.Parse(q.Text)
		if shape != nil {
			parsed[i] = shape(parsed[i])
		}
	}

	// Warm, because the budget is stated warm: the process has been running and
	// the page cache holds the working set. A cold measurement is a measurement
	// of the disk.
	for _, q := range parsed[:min(len(parsed), 20)] {
		if _, err := s.Search(b.Context(), p, q); err != nil {
			b.Fatalf("Search: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if _, err := s.Search(b.Context(), p, parsed[i%len(parsed)]); err != nil {
			b.Fatalf("Search: %v", err)
		}
	}
}

func BenchmarkSearchCommonTerm(b *testing.B) { run(b, benchcorpus.ClassCommon, nil) }
func BenchmarkSearchMultiTerm(b *testing.B)  { run(b, benchcorpus.ClassMulti, nil) }
func BenchmarkSearchFilterOnly(b *testing.B) { run(b, benchcorpus.ClassFilter, nil) }
func BenchmarkSearchTermFilter(b *testing.B) { run(b, benchcorpus.ClassTermFilter, nil) }
func BenchmarkSearchRare(b *testing.B)       { run(b, benchcorpus.ClassRare, nil) }

// BenchmarkSearchPathological is the one character query and the term most of
// the corpus carries. It has its own relaxed budget and it is measured rather
// than ignored, because a query class nobody measures is a denial of service
// waiting to be found.
func BenchmarkSearchPathological(b *testing.B) { run(b, benchcorpus.ClassPathological, nil) }

// BenchmarkSearchDeepPage pages far enough in that the candidate pool has to
// grow, which is where a naive top K falls over: the tenth page cannot be
// served from a pool of twenty.
func BenchmarkSearchDeepPage(b *testing.B) {
	run(b, benchcorpus.ClassCommon, func(q index.Query) index.Query {
		q.Offset = 200
		return q
	})
}

// BenchmarkSearchPage measures the same query at four page sizes, which is the
// measurement that separates the cost of finding the results from the cost of
// assembling them.
//
// Everything before the page is work over the match set and does not move when
// the page grows, so whatever the difference between one result and fifty is,
// that is what a result costs to put on a page. It was 0.7ms each, which on the
// default page of twenty spent more than the whole search budget before
// retrieval had done anything.
func BenchmarkSearchPage(b *testing.B) {
	for _, size := range []int{1, 5, 20, 50} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			run(b, benchcorpus.ClassCommon, func(q index.Query) index.Query {
				q.Limit = size
				return q
			})
		})
	}
}

// BenchmarkSearchByRecency ranks on the date instead of the score, which takes
// a different path through the driver: the cut is on modified_at rather than on
// the full text rank.
func BenchmarkSearchByRecency(b *testing.B) {
	run(b, benchcorpus.ClassCommon, func(q index.Query) index.Query {
		q.Sort = index.ByRecent
		return q
	})
}

// BenchmarkSearchStranger is the reader who is in no group. The match set is
// small and the predicate is doing all the work, so this is the measurement
// that would notice the permission filter moving out of the query and into Go.
func BenchmarkSearchStranger(b *testing.B) {
	st, spec := benchcorpus.Fixture(b)
	s := index.New(st, index.WithClock(func() time.Time { return benchcorpus.Epoch }))
	queries := benchcorpus.ByClass(benchcorpus.Queries())[benchcorpus.ClassCommon]

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if _, err := s.Search(b.Context(), spec.Stranger(), index.Parse(queries[i%len(queries)].Text)); err != nil {
			b.Fatalf("Search: %v", err)
		}
	}
}
