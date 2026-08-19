package sqlitestore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tamnd/genba/benchcorpus"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/sqlitestore"
)

// This file is an external test package because package benchcorpus imports
// sqlitestore to build the fixture, and a benchmark in package sqlitestore
// would close that loop into an import cycle.
//
// These measure the driver rather than the search, so what they tell you is
// where a search regression lives: if index gets slower and Rank did not, the
// cost moved into Go.

const pool = 500

func BenchmarkRank(b *testing.B) {
	st, spec := benchcorpus.Fixture(b)
	p := spec.Principal()
	requests := requestsOf(benchcorpus.ClassCommon)
	sel := store.Selection{Limit: pool}

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if _, err := st.Rank(b.Context(), p, requests[i%len(requests)], sel); err != nil {
			b.Fatalf("Rank: %v", err)
		}
	}
}

// BenchmarkRankMultiTerm is the class that costs the most in the driver,
// because every extra term is another set of postings to join against.
func BenchmarkRankMultiTerm(b *testing.B) {
	st, spec := benchcorpus.Fixture(b)
	p := spec.Principal()
	requests := requestsOf(benchcorpus.ClassMulti)
	sel := store.Selection{Limit: pool}

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if _, err := st.Rank(b.Context(), p, requests[i%len(requests)], sel); err != nil {
			b.Fatalf("Rank: %v", err)
		}
	}
}

// BenchmarkRankFilterOnly has no terms at all, so there is no full text index
// to cut the match set down and the filter columns are carrying it alone.
func BenchmarkRankFilterOnly(b *testing.B) {
	st, spec := benchcorpus.Fixture(b)
	p := spec.Principal()
	requests := requestsOf(benchcorpus.ClassFilter)
	sel := store.Selection{Limit: pool}

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if _, err := st.Rank(b.Context(), p, requests[i%len(requests)], sel); err != nil {
			b.Fatalf("Rank: %v", err)
		}
	}
}

// BenchmarkStatistics is the second query of every search. It is separate
// because it is the one that would quietly become a scan over every document
// carrying a common term.
func BenchmarkStatistics(b *testing.B) {
	st, spec := benchcorpus.Fixture(b)
	p := spec.Principal()
	requests := requestsOf(benchcorpus.ClassMulti)

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if _, err := st.Statistics(b.Context(), p, requests[i%len(requests)].Terms); err != nil {
			b.Fatalf("Statistics: %v", err)
		}
	}
}

// BenchmarkGetByIDs fetches one page worth of documents, which is the third and
// last query of a search and the only one that decodes any JSON.
func BenchmarkGetByIDs(b *testing.B) {
	st, spec := benchcorpus.Fixture(b)
	p := spec.Principal()

	ranked, err := st.Rank(context.Background(), p, requestsOf(benchcorpus.ClassCommon)[0], store.Selection{Limit: pool})
	if err != nil {
		b.Fatalf("Rank: %v", err)
	}
	if len(ranked.Candidates) < 20 {
		b.Fatalf("the corpus returned %d candidates, too few to fetch a page from", len(ranked.Candidates))
	}
	ids := make([]string, 0, 20)
	for _, c := range ranked.Candidates[:20] {
		ids = append(ids, c.ID)
	}

	b.ReportAllocs()
	for b.Loop() {
		got, err := st.Fetch(b.Context(), p, ids)
		if err != nil {
			b.Fatalf("Fetch: %v", err)
		}
		if len(got) != len(ids) {
			b.Fatalf("fetched %d of %d documents", len(got), len(ids))
		}
	}
}

// BenchmarkPutBatch is ingestion, in the batch size a connector actually uses.
//
// Every iteration writes the same documents into a database that has just been
// created. Writing them into the same store twice would be measuring a replace,
// which retires the old postings before writing the new ones and is not the
// path a first crawl takes. Creating the database is outside the timer.
func BenchmarkPutBatch(b *testing.B) {
	const batch = 500

	docs := make([]doc.Document, 0, batch)
	benchcorpus.Default(benchcorpus.DefaultSeed, batch).Each(func(d doc.Document) bool {
		docs = append(docs, d)
		return true
	})

	dir := b.TempDir()
	var n int

	b.ReportAllocs()
	b.SetBytes(int64(bodyBytes(docs)))
	for b.Loop() {
		b.StopTimer()
		path := filepath.Join(dir, fmt.Sprintf("put-%d.db", n))
		st, err := sqlitestore.Open(b.Context(), path)
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		n++
		b.StartTimer()

		if err := st.Put(b.Context(), docs...); err != nil {
			b.Fatalf("Put: %v", err)
		}

		b.StopTimer()
		if err := st.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
		// Each iteration leaves behind a database holding five hundred
		// documents, and a long run leaves hundreds of them. That filled a disk
		// while this benchmark was being written, which is a silly way to lose
		// a measurement, so the file goes as soon as it has been measured.
		for _, ext := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(path + ext)
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(batch*n)/b.Elapsed().Seconds(), "docs/s")
}

// requestsOf turns the checked in queries of one class into store requests, so
// the driver benchmarks measure the same text the search benchmarks do.
func requestsOf(class benchcorpus.Class) []store.Request {
	queries := benchcorpus.ByClass(benchcorpus.Queries())[class]
	out := make([]store.Request, 0, len(queries))
	for _, q := range queries {
		out = append(out, index.Parse(q.Text).Request())
	}
	return out
}

func bodyBytes(docs []doc.Document) int {
	var n int
	for _, d := range docs {
		n += len(d.Body) + len(d.Title)
	}
	return n
}
