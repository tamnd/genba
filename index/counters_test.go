package index_test

import (
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/benchcorpus"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/sqlitestore"
)

// A latency number is a property of the machine that produced it. A CI runner
// shares its cores with whatever else is on the box, so a gate written in
// milliseconds fails on a bad afternoon and passes on a good one, and after the
// third false alarm somebody raises the threshold until it never fails at all.
//
// These assert on work instead: rows read, statements executed, documents
// decoded, candidates ranked. Those numbers are the same on a laptop and on a
// loaded runner, they are what latency is made of, and the regressions worth
// catching are all visible in them. A search that starts decoding the whole
// match set to return twenty results shows up here as a decode count in the
// thousands long before anybody notices the page got slower.
//
// The wall clock budgets live in the benchmarks and in the CI gate. This is the
// part that runs on every pull request.

// counterCorpus is smaller than the benchmark fixture on purpose. It only has
// to be large enough that the match set of a common term is several times the
// candidate pool, which is what makes the bounds below mean anything, and small
// enough that generating it the first time is a few seconds rather than a
// minute.
const counterCorpus = 4_000

const (
	// maxStatements is the statement budget for one search. Four queries do the
	// work: the candidate cut, the counts and facets, the corpus statistics and
	// the fetch of the page. The budget is a little above that so an extra
	// lookup is allowed, and far below one statement per result, which is the
	// shape of every N plus one regression.
	maxStatements = 8

	// facetRows is the rows the facet counts return: four fields, each capped
	// at the number of values a facet reports, and the total.
	facetRows = 4*store.MaxFacetValues + 1
)

// rowBudget is every row one search is allowed to read, by where it is read.
//
// Writing it out rather than picking a round number is the point. Each term is
// a bound somebody chose, and a search that goes over one of them has broken
// the thing that bound was protecting, which is a more useful failure than
// "slower than it was".
func rowBudget(pool, terms, hits int) int64 {
	// Phase one hands back at most the pool.
	rows := pool
	// The term frequencies for those candidates, which is the one part that
	// grows with the query: one row per candidate per term it carries.
	rows += pool * terms
	// The total and the facet counts, which are aggregates, so the rows are the
	// values reported and not the documents behind them.
	rows += facetRows
	// The corpus row and one row per term of statistics.
	rows += 1 + terms
	// And the page that is actually returned.
	return int64(rows + hits)
}

func counters(t *testing.T) (*index.Searcher, *acl.Principal, *sqlitestore.Store) {
	t.Helper()
	spec := benchcorpus.Default(benchcorpus.DefaultSeed, counterCorpus)
	st := benchcorpus.FixtureOf(t, spec)
	// A fixed clock, so the recency prior is the same on every run and the
	// counts are not a function of what day it is.
	s := index.New(st, index.WithClock(func() time.Time { return benchcorpus.Epoch }))
	return s, spec.Principal(), st
}

func TestSearchCounters(t *testing.T) {
	if testing.Short() {
		t.Skip("generating the corpus takes a few seconds the first time")
	}
	s, p, st := counters(t)

	for _, tc := range []struct {
		name  string
		class benchcorpus.Class
	}{
		{"a common term", benchcorpus.ClassCommon},
		{"several terms", benchcorpus.ClassMulti},
		{"filters and no terms", benchcorpus.ClassFilter},
		{"a term and a filter", benchcorpus.ClassTermFilter},
		{"a term almost nothing carries", benchcorpus.ClassRare},
		{"the worst query somebody can type", benchcorpus.ClassPathological},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queries := benchcorpus.ByClass(benchcorpus.Queries())[tc.class]
			if len(queries) == 0 {
				t.Fatalf("the query set has no %s queries", tc.class)
			}

			// Several queries of the class rather than one, because a single
			// query that happened to match nothing would pass every bound here
			// without proving anything.
			for _, q := range queries[:min(len(queries), 12)] {
				query := index.Parse(q.Text)
				st.ResetCounters()

				res, err := s.Search(t.Context(), p, query)
				if err != nil {
					t.Fatalf("Search(%q): %v", q.Text, err)
				}
				c := st.Counters()
				pool := int64(index.CandidatePool(query.Offset, index.DefaultLimit))

				// Decoding is the expensive part of returning a result, because
				// it is JSON, so a search decodes the page it is about to
				// return and nothing else. This is the counter that catches two
				// phase retrieval quietly turning back into one phase.
				if want := int64(len(res.Hits)); c.Decodes > want {
					t.Errorf("Search(%q) decoded %d documents to return %d", q.Text, c.Decodes, want)
				}
				if c.Statements > maxStatements {
					t.Errorf("Search(%q) ran %d statements, at most %d", q.Text, c.Statements, maxStatements)
				}

				// The ranker sees the pool, never the match set, so a query
				// that matches every document in the tenant costs what a query
				// matching a thousand costs.
				if c.Candidates > pool {
					t.Errorf("Search(%q) ranked %d candidates, the pool is %d", q.Text, c.Candidates, pool)
				}

				// And the rows, which is the counter that catches a count being
				// done by reading the rows and adding them up in Go.
				if most := rowBudget(int(pool), len(query.Request().Terms), len(res.Hits)); c.Rows > most {
					t.Errorf("Search(%q) read %d rows, at most %d", q.Text, c.Rows, most)
				}
			}
		})
	}

	// A reader in another tenant may see nothing at all, and that has to cost
	// nothing at all. If this ever reads rows, the permission predicate has
	// moved out of the SQL and into Go, where it filters after the database has
	// already done the work.
	t.Run("a principal who may read nothing costs nothing", func(t *testing.T) {
		outsider := &acl.Principal{Tenant: "other", Subject: "u_outsider", Groups: acl.GroupSet{Version: 1}}
		for _, q := range benchcorpus.ByClass(benchcorpus.Queries())[benchcorpus.ClassCommon][:12] {
			st.ResetCounters()
			res, err := s.Search(t.Context(), outsider, index.Parse(q.Text))
			if err != nil {
				t.Fatalf("Search(%q): %v", q.Text, err)
			}
			if len(res.Hits) != 0 {
				t.Fatalf("Search(%q) returned %d hits to a reader in another tenant", q.Text, len(res.Hits))
			}
			// Not zero rows, because a count returns a row to say the answer is
			// zero, but a handful and never a walk. Nothing is ranked and
			// nothing is decoded.
			c := st.Counters()
			if c.Candidates != 0 || c.Decodes != 0 {
				t.Errorf("Search(%q) ranked %d candidates and decoded %d documents for a reader who may see nothing", q.Text, c.Candidates, c.Decodes)
			}
			if c.Rows > facetRows {
				t.Errorf("Search(%q) read %d rows for a reader who may see nothing", q.Text, c.Rows)
			}
		}
	})
}

// TestDeepPageCounters holds the tenth page to the same bounds as the first.
// Paging is where a searcher that keeps the whole match set in memory stops
// being a rounding error, because the pool has to grow with the offset and the
// naive implementation grows the decode count along with it.
func TestDeepPageCounters(t *testing.T) {
	if testing.Short() {
		t.Skip("generating the corpus takes a few seconds the first time")
	}
	s, p, st := counters(t)

	text := benchcorpus.ByClass(benchcorpus.Queries())[benchcorpus.ClassCommon][0].Text
	for _, offset := range []int{0, 20, 100, 200, 500} {
		query := index.Parse(text)
		query.Offset = offset

		st.ResetCounters()
		res, err := s.Search(t.Context(), p, query)
		if err != nil {
			t.Fatalf("Search at offset %d: %v", offset, err)
		}
		c := st.Counters()
		pool := int64(index.CandidatePool(offset, index.DefaultLimit))

		if want := int64(len(res.Hits)); c.Decodes > want {
			t.Errorf("offset %d decoded %d documents to return %d", offset, c.Decodes, want)
		}
		if c.Statements > maxStatements {
			t.Errorf("offset %d ran %d statements, at most %d", offset, c.Statements, maxStatements)
		}
		// The pool grows with the offset, because the two hundredth result
		// cannot be picked out of a pool of twenty, and it grows by a factor so
		// that the growth stays bounded and stated in one place.
		if c.Candidates > pool {
			t.Errorf("offset %d ranked %d candidates, the pool is %d", offset, c.Candidates, pool)
		}
	}
}
