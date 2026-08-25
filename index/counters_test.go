package index_test

import (
	"slices"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/benchcorpus"
	"github.com/tamnd/genba/doc"
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

// maxStatements is the statement budget for one search. The candidate cut, the
// term frequencies for the candidates, the total, the facet counts, two of
// corpus statistics and the fetch of the page. The budget is a little above that
// so an extra lookup is allowed, and far below one statement per result, which
// is the shape of every N plus one regression.
//
// It does not move with the filters. Every facet, lifted or not, is counted in
// the same statement, and a filtered search running one statement per ticked box
// is the regression this number is here to catch.
const maxStatements = 9

// correctionStatements is what a search that found nothing may spend on top of
// that, looking for a spelling that would have found something.
//
// One window read of the term table per word the corpus does not have, one read
// asking which of the words that were typed this person has a document for, one
// asking the same of every spelling that came back, and the confirmation of the
// corrected query. The two carriage reads are one statement each however many
// words they are about, which is what makes it affordable to be sure rather
// than to guess from the nearest spelling.
//
// It is written per term rather than as one number because that is the shape of
// the cost, and because a correction that started running a statement per
// spelling would otherwise hide inside a round number.
func correctionStatements(terms int) int64 {
	return int64(terms + 3)
}

// countRows is the rows the counting returns for a request that constrains the
// given number of facet fields: the total, then one pool per expression the
// statement declares and one capped group of values per field it counts.
//
// A field somebody has ticked is counted twice, once with its own filter lifted
// and once over the match set, and the lifted count needs an expression of its
// own. So a search with two boxes ticked returns rather more rows than one with
// none, and writing that out is how the growth stays something somebody chose
// rather than something that happened.
func countRows(constrained int) int {
	pools := 1 + constrained
	groups := len(store.FacetFields) + constrained
	return 1 + pools + groups*store.MaxFacetValues
}

// constrained is how many facet fields the request narrows, which is what the
// counting statement grows with.
func constrained(r store.Request) int {
	var n int
	for _, field := range store.FacetFields {
		if r.Constrains(field) {
			n++
		}
	}
	return n
}

// rowBudget is every row one search is allowed to read, by where it is read.
//
// Writing it out rather than picking a round number is the point. Each term is
// a bound somebody chose, and a search that goes over one of them has broken
// the thing that bound was protecting, which is a more useful failure than
// "slower than it was".
func rowBudget(pool, terms, hits, constrained int) int64 {
	// Phase one hands back at most the pool.
	rows := pool
	// The term frequencies for those candidates, which is the one part that
	// grows with the query: one row per candidate per term it carries.
	rows += pool * terms
	// The total and the facet counts, which are aggregates, so the rows are the
	// values reported and not the documents behind them. What those aggregates
	// read is counted separately: see [store.Counters.Faceted].
	rows += countRows(constrained)
	// The corpus row and one row per term of statistics.
	rows += 1 + terms
	// A search that found nothing looks for a spelling that would have found
	// something, and that is four reads on top of the search.
	//
	// Four windows of the term table per word it does not recognise. Two
	// carriage reads, one for the words that were typed and one for the
	// spellings offered back, each a pool of documents and the counts for the
	// words they hold. And the confirmation of the corrected query, which is a
	// cut for a single row.
	if hits == 0 {
		spellings := terms * index.CorrectionOffers
		rows += terms * 4 * sqlitestore.NearWindow
		rows += index.CarriagePool * (1 + terms)
		rows += index.CarriagePool * (1 + spellings)
		rows += 1 + terms
	}
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
				filters := constrained(query.Request())

				// Decoding is the expensive part of returning a result, because
				// it is JSON, so a search decodes the page it is about to
				// return and nothing else. This is the counter that catches two
				// phase retrieval quietly turning back into one phase.
				if want := int64(len(res.Hits)); c.Decodes > want {
					t.Errorf("Search(%q) decoded %d documents to return %d", q.Text, c.Decodes, want)
				}
				statements := int64(maxStatements)
				if len(res.Hits) == 0 {
					statements += correctionStatements(len(query.Request().Terms))
				}
				if c.Statements > statements {
					t.Errorf("Search(%q) ran %d statements, at most %d", q.Text, c.Statements, statements)
				}

				// The ranker sees the pool, never the match set, so a query
				// that matches every document in the tenant costs what a query
				// matching a thousand costs.
				if c.Candidates > pool {
					t.Errorf("Search(%q) ranked %d candidates, the pool is %d", q.Text, c.Candidates, pool)
				}

				// And the rows, which is the counter that catches a count being
				// done by reading the rows and adding them up in Go.
				if most := rowBudget(int(pool), len(query.Request().Terms), len(res.Hits), filters); c.Rows > most {
					t.Errorf("Search(%q) read %d rows, at most %d", q.Text, c.Rows, most)
				}

				// The facet counts, which the rows counter cannot see because an
				// aggregate returns the same handful of rows whatever it read.
				//
				// The bound is per expression and there is one of those for the
				// match set plus one per ticked box, because a field counted with
				// its own filter lifted is counted over a different set of
				// documents and each of them stops at the same bound. Four boxes
				// is five bounded walks and never a walk of the match set.
				if most := int64(index.FacetPool) * int64(1+filters); c.Faceted > most {
					t.Errorf("Search(%q) counted facets over %d documents, the bound is %d", q.Text, c.Faceted, most)
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
			if c.Rows > int64(countRows(0)) {
				t.Errorf("Search(%q) read %d rows for a reader who may see nothing", q.Text, c.Rows)
			}
		}
	})
}

// TestFacetCounters holds the sidebar to the facet pool, and holds the total to
// the truth.
//
// The two are separate on purpose. A total is one count over the predicate and
// there is no reason for it to be anything other than exact. The facets are four
// grouped counts over four display strings of every matching document, which is
// the one part of a search with no bound on it at all, so they get one and say
// they got one. A search that reports approximate counts as though they were
// counts is worse than either.
func TestFacetCounters(t *testing.T) {
	if testing.Short() {
		t.Skip("generating the corpus takes a few seconds the first time")
	}
	s, p, st := counters(t)

	byClass := benchcorpus.ByClass(benchcorpus.Queries())
	queries := append(slices.Clone(byClass[benchcorpus.ClassCommon][:12]), byClass[benchcorpus.ClassPathological][:4]...)

	var bounded int
	for _, q := range queries {
		st.ResetCounters()
		res, err := s.Search(t.Context(), p, index.Parse(q.Text))
		if err != nil {
			t.Fatalf("Search(%q): %v", q.Text, err)
		}
		c := st.Counters()

		if res.Total <= index.FacetPool {
			// The match set fits, so the counts are the real ones and saying
			// otherwise would teach everybody to ignore the flag.
			if res.Approximate {
				t.Errorf("Search(%q) matched %d documents and called its facets approximate", q.Text, res.Total)
			}
			if c.Faceted != int64(res.Total) {
				t.Errorf("Search(%q) counted facets over %d of %d matching documents", q.Text, c.Faceted, res.Total)
			}
			continue
		}
		bounded++
		if !res.Approximate {
			t.Errorf("Search(%q) counted %d of %d matching documents and did not say the facets are a lower bound",
				q.Text, c.Faceted, res.Total)
		}
		if c.Faceted != int64(index.FacetPool) {
			t.Errorf("Search(%q) counted facets over %d documents, the pool is %d", q.Text, c.Faceted, index.FacetPool)
		}
	}

	// Without this the assertions above are a bound nothing in the query set has
	// ever reached, which is a test that passes on the implementation it was
	// written to catch.
	if bounded == 0 {
		t.Fatalf("no query matched more than the facet pool of %d, so the bound was never exercised", index.FacetPool)
	}
}

// The filter rail counts the corpus and a search samples it, and the difference
// is the whole of #142.
//
// A sidebar is drawn beside a match set, where the bound is the right trade: a
// query that matched fifty thousand documents is described well enough by the
// first thousand of them, and reading four columns of the other forty nine to
// improve the second digit of a number nobody reads that far is not worth a
// second of anybody's time. A rail is drawn beside no match set at all. It says
// how much of each source and each kind there is, it is read as proportions,
// and a rail whose every row reports the same bound has no proportions in it.
//
// So this asserts the two behaviours side by side on one corpus, because either
// one alone reads like an accident.
func TestTheRailCountsPastTheBoundASearchStopsAt(t *testing.T) {
	if testing.Short() {
		t.Skip("generating the corpus takes a few seconds the first time")
	}
	s, p, st := counters(t)

	// A search with nothing in it, which is the most documents any query here
	// can match and therefore the case the bound bites hardest.
	res, err := s.Search(t.Context(), p, index.Query{Limit: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total <= index.FacetPool {
		t.Fatalf("the corpus holds %d readable documents and the facet pool is %d, so the bound is never reached",
			res.Total, index.FacetPool)
	}
	if !res.Approximate {
		t.Errorf("a search over %d documents counted its facets over %d and did not say so",
			res.Total, index.FacetPool)
	}
	if got := sum(res.Facets["source"]); got != index.FacetPool {
		t.Errorf("the search sidebar adds up to %d, want the bound of %d", got, index.FacetPool)
	}

	st.ResetCounters()
	rail, err := s.Filters(t.Context(), p)
	if err != nil {
		t.Fatalf("Filters: %v", err)
	}
	for _, field := range []string{"source", "kind"} {
		if got := sum(rail[field]); got != res.Total {
			t.Errorf("the rail adds up to %d by %s, want the %d documents this reader can open",
				got, field, res.Total)
		}
	}

	// One statement for both groupings. Counting the corpus twice to describe it
	// two ways would double the one part of this that is proportional to the
	// corpus, and it is the part that put the rail behind a cache.
	if c := st.Counters(); c.Statements != 1 {
		t.Errorf("the rail cost %d statements, want one", c.Statements)
	}
}

func sum(facets []index.Facet) int {
	var n int
	for _, f := range facets {
		n += f.Count
	}
	return n
}

// A filtered search counts more documents than it matched, on purpose. A field
// somebody has ticked is counted a second time with its own filter lifted, so
// that the values nobody ticked still carry the number of results choosing them
// would find, which is the one thing worth knowing at the moment somebody is
// looking at a filter they have already applied.
//
// What that must not become is unbounded. The bound applies to each of those
// walks separately, so the work grows by a factor of the boxes somebody ticked,
// which is at most five walks, and never with the size of the match set.
func TestFilteredFacetCounters(t *testing.T) {
	if testing.Short() {
		t.Skip("generating the corpus takes a few seconds the first time")
	}
	s, p, st := counters(t)

	byClass := benchcorpus.ByClass(benchcorpus.Queries())
	queries := append(slices.Clone(byClass[benchcorpus.ClassFilter][:8]), byClass[benchcorpus.ClassTermFilter][:8]...)

	var siblings, exact int
	for _, q := range queries {
		query := index.Parse(q.Text)
		r := query.Request()
		filters := constrained(r)
		if filters == 0 {
			t.Fatalf("%q is meant to be a filtered query and narrows no facet", q.Text)
		}

		st.ResetCounters()
		res, err := s.Search(t.Context(), p, query)
		if err != nil {
			t.Fatalf("Search(%q): %v", q.Text, err)
		}
		if most := int64(index.FacetPool) * int64(1+filters); st.Counters().Faceted > most {
			t.Errorf("Search(%q) counted facets over %d documents, the bound is %d",
				q.Text, st.Counters().Faceted, most)
		}

		for _, field := range store.FacetFields {
			if !r.Constrains(field) {
				continue
			}
			var ticked, other int
			for _, v := range res.Facets[field] {
				if chosen(r, field, v.Value) {
					ticked = v.Count
					continue
				}
				other++
			}
			siblings += other
			// The value somebody ticked reports the results they are looking at,
			// which is the total. Under the bound it is a lower bound like every
			// other count, so the check is on the queries small enough to be exact.
			if res.Approximate || res.Total > index.FacetPool {
				continue
			}
			exact++
			if ticked != res.Total {
				t.Errorf("Search(%q) ticked %s and its own count is %d of %d results",
					q.Text, field, ticked, res.Total)
			}
		}
	}

	// The point of all of it. If every ticked field reported nothing but itself
	// the assertions above would still pass, and the sidebar would be a list of
	// one thing somebody already knows.
	if siblings == 0 {
		t.Fatal("no filtered query offered a single alternative to the value it had ticked")
	}
	if exact == 0 {
		t.Fatal("every filtered query was counted under the bound, so nothing checked a ticked count against the total")
	}
}

// chosen reports whether the request already narrows the field to this value.
func chosen(r store.Request, field, value string) bool {
	switch field {
	case "source":
		return slices.Contains(r.Sources, value)
	case "kind":
		return slices.Contains(r.Kinds, doc.Kind(value))
	case "container":
		return slices.Contains(r.Containers, value)
	}
	return false
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
