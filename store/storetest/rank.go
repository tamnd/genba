package storetest

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// RunRanker checks the three capabilities a driver can implement to answer a
// search without materialising the corpus: [store.Ranker], [store.Statistician]
// and [store.Fetcher].
//
// All three are optional and a case skips for a driver that lacks the one it
// covers, so a driver is free to implement none of them and still pass. What a
// driver is not free to do is implement one of them differently. Everything
// here is checked against the same slow, obviously correct answer computed from
// Scan, because the numbers these capabilities return feed the ranking, and a
// ranking computed from statistics that quietly drifted is not something anyone
// notices from the outside. It looks like search getting worse.
func RunRanker(t *testing.T, newStore Factory) {
	t.Helper()
	for _, c := range rankCases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })
			c.run(t, s)
		})
	}
}

type rankCase struct {
	name string
	run  func(t *testing.T, s store.Store)
}

var rankCases = []rankCase{
	{"a pool larger than the match set returns all of it", testRankWholeMatchSet},
	{"the total counts the match set and not the pool", testRankTotal},
	{"truncated says whether the ranking saw everything", testRankTruncated},
	{"facets are counted over the match set and not over the pool", testRankFacets},
	{"a facet count lifts its own filter and applies the others", testRankDrillDown},
	{"a facet bound counts a sample and says the counts are a lower bound", testRankFacetBound},
	{"a selection with no counts ranks the same documents", testRankWithoutCounts},
	{"the principal is applied inside rank", testRankPermission},
	{"a nil principal ranks nothing", testRankNilPrincipal},
	{"a candidate carries the token counts the analyzer produces", testRankTokens},
	{"a candidate carries the query terms and only those", testRankTerms},
	{"a recency selection takes the most recently modified", testRankRecent},
	{"rank reflects a delete", testRankDelete},
	{"statistics agree with the analyzer over the corpus", testStatisticsCorpus},
	{"statistics are over the tenant and not over the reader", testStatisticsTenant},
	{"statistics follow a replace", testStatisticsReplace},
	{"statistics follow a delete", testStatisticsDelete},
	{"a quarantined document is not in the statistics", testStatisticsQuarantine},
	{"a nil principal has no statistics", testStatisticsNilPrincipal},
	{"fetch returns the documents behind a page", testFetchPage},
	{"fetch omits what the principal may not read", testFetchPermission},
	{"a nil principal fetches nothing", testFetchNilPrincipal},
}

func ranker(t *testing.T, s store.Store) store.Ranker {
	t.Helper()
	rk, ok := s.(store.Ranker)
	if !ok {
		t.Skip("driver does not implement store.Ranker")
	}
	return rk
}

func statistician(t *testing.T, s store.Store) store.Statistician {
	t.Helper()
	st, ok := s.(store.Statistician)
	if !ok {
		t.Skip("driver does not implement store.Statistician")
	}
	return st
}

func fetcher(t *testing.T, s store.Store) store.Fetcher {
	t.Helper()
	f, ok := s.(store.Fetcher)
	if !ok {
		t.Skip("driver does not implement store.Fetcher")
	}
	return f
}

// mustRank is Rank with the error handling every case would otherwise repeat.
func mustRank(t *testing.T, rk store.Ranker, p *acl.Principal, r store.Request, sel store.Selection) store.Ranked {
	t.Helper()
	got, err := rk.Rank(t.Context(), p, r, sel)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	return got
}

// candidateIDs is the pool, sorted, so a case can compare it against the Go
// rule without depending on the order a driver happened to cut in.
func candidateIDs(ranked store.Ranked) []string {
	ids := make([]string, 0, len(ranked.Candidates))
	for _, c := range ranked.Candidates {
		ids = append(ids, c.ID)
	}
	slices.Sort(ids)
	return ids
}

// pool is a selection large enough that nothing in the fixture is cut, with the
// counts asked for, which is what a search does.
func pool() store.Selection { return store.Selection{Limit: 100, Counts: true} }

// wantFacets counts the facets the slow way, from the documents the principal
// can actually read.
//
// Each field is counted with its own constraint lifted and every other
// constraint applied, which is the contract in [store.Ranked.Facets]. A
// document in the match set counts towards all four fields. One that fails
// exactly one constraint counts towards that field alone, because it is a
// result that ticking that value instead would have found. One that fails two
// or more counts towards nothing: no single change to the query reaches it.
func wantFacets(t *testing.T, s store.Store, p *acl.Principal, r store.Request) map[string]map[string]int {
	t.Helper()
	out := map[string]map[string]int{
		"source": {}, "kind": {}, "container": {}, "author": {},
	}
	count := func(field string, d doc.Document) {
		if v := facetValue(field, d); v != "" {
			out[field][v]++
		}
	}
	base := r.WithoutFacets()
	if err := s.Scan(t.Context(), p, func(d doc.Document) bool {
		if !base.Matches(d) {
			return true
		}
		missing := ""
		for _, field := range store.FacetFields {
			if r.Passes(field, d) {
				continue
			}
			if missing != "" {
				return true
			}
			missing = field
		}
		if missing != "" {
			count(missing, d)
			return true
		}
		for _, field := range store.FacetFields {
			count(field, d)
		}
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return out
}

// facetValue is the display string a facet counts a document under, which is
// the string on the row somebody clicks to filter by.
func facetValue(field string, d doc.Document) string {
	switch field {
	case "source":
		return d.Source
	case "kind":
		return string(d.Kind)
	case "container":
		return d.Container
	case "author":
		return d.Author.Display()
	}
	return ""
}

// gotFacets turns a driver's answer into the same shape, which is also where a
// duplicated facet value would show up as a lost count.
func gotFacets(t *testing.T, ranked store.Ranked) map[string]map[string]int {
	t.Helper()
	out := map[string]map[string]int{
		"source": {}, "kind": {}, "container": {}, "author": {},
	}
	for field, values := range ranked.Facets {
		if _, ok := out[field]; !ok {
			t.Fatalf("Rank reported a facet nobody asked for: %q", field)
		}
		for _, v := range values {
			if v.Value == "" {
				t.Fatalf("%s facet has an empty value, which is not a filter anyone can tick", field)
			}
			if _, seen := out[field][v.Value]; seen {
				t.Fatalf("%s facet reports %q twice", field, v.Value)
			}
			out[field][v.Value] = v.Count
		}
	}
	return out
}

func testRankWholeMatchSet(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	mustPut(t, s, corpus()...)

	for _, r := range []store.Request{
		{},
		{Terms: []string{"payments"}},
		{Sources: []string{"gdrive"}},
		{Terms: []string{"queue"}, Kinds: []doc.Kind{doc.KindCode}},
	} {
		got := candidateIDs(mustRank(t, rk, reader(), r, pool()))
		want := wantIDs(t, s, reader(), r)
		if !slices.Equal(got, want) {
			t.Fatalf("Rank and Scan disagree for %+v:\n     ranked: %v\n   expected: %v", r, got, want)
		}
	}
}

func testRankTotal(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	mustPut(t, s, corpus()...)

	// The pool cuts to one candidate, and the total still counts everything that
	// matched. A total that reports the pool would tell the interface there is
	// one result when there are four, which is the number a person reads before
	// deciding the search is broken.
	got := mustRank(t, rk, reader(), store.Request{}, store.Selection{Limit: 1, Counts: true})
	if len(got.Candidates) != 1 {
		t.Fatalf("asked for one candidate and got %d", len(got.Candidates))
	}
	if want := len(wantIDs(t, s, reader(), store.Request{})); got.Total != want {
		t.Fatalf("Total = %d, the match set has %d documents in it", got.Total, want)
	}
}

func testRankTruncated(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	mustPut(t, s, corpus()...)

	if got := mustRank(t, rk, reader(), store.Request{}, pool()); got.Truncated {
		t.Fatal("Truncated is set for a pool that held the whole match set")
	}
	if got := mustRank(t, rk, reader(), store.Request{}, store.Selection{Limit: 2, Counts: true}); !got.Truncated {
		t.Fatal("Truncated is not set for a pool that cut the match set in half")
	}
}

func testRankFacets(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	mustPut(t, s, corpus()...)

	want := wantFacets(t, s, reader(), store.Request{})
	// Both selections, because the facets belong to the match set. Counting them
	// over the pool would make the sidebar change as somebody pages, and the
	// counts it shows would be a description of the current page rather than of
	// what ticking the filter is about to do.
	for _, sel := range []store.Selection{pool(), {Limit: 1, Counts: true}} {
		got := gotFacets(t, mustRank(t, rk, reader(), store.Request{}, sel))
		if !maps.EqualFunc(got, want, maps.Equal) {
			t.Fatalf("facets for a pool of %d are %v, expected %v", sel.Limit, got, want)
		}
	}
}

// A sidebar is only worth reading if the values nobody has ticked still carry
// counts. Counted over the request as it stands, a ticked source reports its own
// count and zero for every other source, and the one question somebody has at
// that point, whether unticking it is going to find anything, has no answer on
// the screen.
//
// So each field is counted with its own filter lifted and the rest applied, and
// the three things checked here are that the driver's arithmetic agrees with the
// same rule computed in Go, that the ticked value still reports the fully
// constrained count, and that Total was left alone. Total is the one number that
// does describe the current state, and a driver that lifted a filter out of it
// as well would report more results than the page it is sitting above.
func testRankDrillDown(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	mustPut(t, s, corpus()...)

	for _, r := range []store.Request{
		{Sources: []string{"gdrive"}},
		{Kinds: []doc.Kind{doc.KindPage, doc.KindFile}},
		{Containers: []string{"Platform"}},
		{Authors: []string{"mei@acme.com"}},
		{Sources: []string{"gdrive"}, Kinds: []doc.Kind{doc.KindPage}},
		{Terms: []string{"payments"}, Sources: []string{"gdrive", "slack"}},
		{Sources: []string{"gdrive"}, Authors: []string{"kenji"}},
	} {
		ranked := mustRank(t, rk, reader(), r, pool())
		got, want := gotFacets(t, ranked), wantFacets(t, s, reader(), r)
		if !maps.EqualFunc(got, want, maps.Equal) {
			t.Fatalf("facets for %+v are %v, expected %v", r, got, want)
		}
		if total := len(wantIDs(t, s, reader(), r)); ranked.Total != total {
			t.Fatalf("Total = %d for %+v, the match set has %d documents in it", ranked.Total, r, total)
		}
		if ranked.Approximate {
			t.Fatalf("Approximate is set for %+v, which was counted with no bound", r)
		}
	}

	// The counts a lifted filter produces have to be the ones somebody would get
	// by making that choice, so the sidebar is checked against the searches it is
	// promising. Ticking a value that is already ticked changes nothing, which is
	// how the ticked value's own count is pinned to the fully constrained one.
	r := store.Request{Sources: []string{"gdrive"}}
	for _, v := range mustRank(t, rk, reader(), r, pool()).Facets["source"] {
		next := r
		next.Sources = []string{v.Value}
		if want := len(wantIDs(t, s, reader(), next)); v.Count != want {
			t.Fatalf("the source facet promises %d results for %q and searching for it finds %d", v.Count, v.Value, want)
		}
	}
}

// A facet bound is what a search sets, so this is the shape a driver actually
// runs. The counts stop at the bound, which is what keeps a query for a common
// word from reading four columns of every document that matched, and what comes
// back has to admit that it stopped: a sidebar showing a sample as though it
// were a count is a number somebody will subtract two others from.
//
// The total is checked in the same case because it is the number that must not
// have been bounded along with them. It is one count over the predicate and it
// is cheap to be exact about, and a driver that answers it out of the same
// bounded pool would report four hundred results for a query that found forty
// thousand.
func testRankFacetBound(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	mustPut(t, s, corpus()...)

	matched := len(wantIDs(t, s, reader(), store.Request{}))
	const bound = 2
	got := mustRank(t, rk, reader(), store.Request{}, store.Selection{Limit: 100, Counts: true, Facets: bound})
	if !got.Approximate {
		t.Fatal("Approximate is not set for facets counted over a bound smaller than the match set")
	}
	if got.Total != matched {
		t.Fatalf("Total = %d under a facet bound, the match set has %d documents in it", got.Total, matched)
	}
	if n := len(got.Candidates); n != matched {
		t.Fatalf("the facet bound cut the candidate pool to %d of %d", n, matched)
	}
	for field, values := range got.Facets {
		var counted int
		for _, v := range values {
			counted += v.Count
		}
		if counted > bound {
			t.Fatalf("the %s facet counts %d documents under a bound of %d: %v", field, counted, bound, values)
		}
	}

	// And a bound the match set fits inside is the exact answer, unflagged,
	// because a search that says its counts are a sample when they are not
	// teaches everybody to ignore the flag.
	full := mustRank(t, rk, reader(), store.Request{}, store.Selection{Limit: 100, Counts: true, Facets: 100})
	if full.Approximate {
		t.Fatal("Approximate is set for facets counted over a bound larger than the match set")
	}
	if want := wantFacets(t, s, reader(), store.Request{}); !maps.EqualFunc(gotFacets(t, full), want, maps.Equal) {
		t.Fatalf("facets under a bound larger than the match set are %v, expected %v", gotFacets(t, full), want)
	}
}

// A selection that asks for no counts gets the same candidates and none of the
// counting. It is what the home screen runs, and the point of it is the work
// that does not happen, so what is asserted here is that skipping the counting
// did not quietly change which documents came back.
func testRankWithoutCounts(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	mustPut(t, s, corpus()...)

	for _, r := range []store.Request{{}, {Terms: []string{"payments"}}, {Sources: []string{"gdrive"}}} {
		counted := mustRank(t, rk, reader(), r, pool())
		plain := mustRank(t, rk, reader(), r, store.Selection{Limit: 100})
		if got, want := candidateIDs(plain), candidateIDs(counted); !slices.Equal(got, want) {
			t.Fatalf("a selection with no counts ranked %v for %+v, and one with them ranked %v", got, r, want)
		}
		if plain.Total != 0 {
			t.Fatalf("Total = %d for a selection that asked for no counts", plain.Total)
		}
		if plain.Truncated {
			t.Fatal("Truncated is set for a selection with no total to compare the pool against")
		}
		for field, values := range plain.Facets {
			if len(values) != 0 {
				t.Fatalf("the %s facet was counted for a selection that asked for no counts: %v", field, values)
			}
		}
	}
}

func testRankPermission(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	mustPut(t, s, corpus()...)

	got := mustRank(t, rk, stranger(), store.Request{}, pool())
	if len(got.Candidates) != 0 || got.Total != 0 {
		t.Fatalf("a stranger ranked %d of %d documents", len(got.Candidates), got.Total)
	}
	for field, values := range got.Facets {
		if len(values) != 0 {
			t.Fatalf("a stranger sees the %s facet: %v", field, values)
		}
	}
}

func testRankNilPrincipal(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	mustPut(t, s, corpus()...)

	if _, err := rk.Rank(t.Context(), nil, store.Request{}, pool()); !errors.Is(err, genba.ErrNoPrincipal) {
		t.Fatalf("Rank with no principal returned %v, expected ErrNoPrincipal", err)
	}
}

func testRankTokens(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	docs := corpus()
	mustPut(t, s, docs...)

	want := make(map[string]doc.Analysis, len(docs))
	for _, d := range docs {
		want[d.ID] = d.Analyze()
	}

	// The lengths a driver reports were computed when the document was written,
	// and the ranking divides by their mean. A driver that stores the character
	// count, or the count before folding, produces a plausible number that
	// penalises exactly the wrong documents.
	for _, c := range mustRank(t, rk, reader(), store.Request{}, pool()).Candidates {
		a := want[c.ID]
		if c.TitleTokens != a.TitleTokens || c.BodyTokens != a.BodyTokens {
			t.Fatalf("%s has %d title and %d body tokens, the analyzer produces %d and %d",
				c.ID, c.TitleTokens, c.BodyTokens, a.TitleTokens, a.BodyTokens)
		}
	}
}

func testRankTerms(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	docs := corpus()
	mustPut(t, s, docs...)

	want := make(map[string]doc.Analysis, len(docs))
	for _, d := range docs {
		want[d.ID] = d.Analyze()
	}

	const term = "payments"
	got := mustRank(t, rk, reader(), store.Request{Terms: []string{term}}, pool())
	if len(got.Candidates) == 0 {
		t.Fatal("nothing matched a term the fixture carries")
	}
	for _, c := range got.Candidates {
		if len(c.Terms) > 1 {
			t.Fatalf("%s carries %v, and the query asked about one term", c.ID, slices.Sorted(maps.Keys(c.Terms)))
		}
		if got, expected := c.Terms[term], want[c.ID].Terms[term]; got != expected {
			t.Fatalf("%s counts %+v occurrences of %q, the analyzer counts %+v", c.ID, got, term, expected)
		}
	}
}

func testRankRecent(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	docs := corpus()
	mustPut(t, s, docs...)

	newest := slices.Clone(docs)
	slices.SortFunc(newest, func(a, b doc.Document) int { return b.ModifiedAt.Compare(a.ModifiedAt) })

	// A recency selection has to cut on the date rather than cut on relevance
	// and sort what survives, or a search for what changed this week returns the
	// most recent of whatever happened to match best.
	got := mustRank(t, rk, reader(), store.Request{}, store.Selection{Limit: 2, Recent: true})
	want := []string{newest[0].ID, newest[1].ID}
	slices.Sort(want)
	if ids := candidateIDs(got); !slices.Equal(ids, want) {
		t.Fatalf("the two most recent documents are %v, the pool held %v", want, ids)
	}
}

func testRankDelete(t *testing.T, s store.Store) {
	rk := ranker(t, s)
	mustPut(t, s, corpus()...)

	if err := s.Delete(t.Context(), "r1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got := mustRank(t, rk, reader(), store.Request{}, pool())
	if slices.Contains(candidateIDs(got), "r1") {
		t.Fatal("a deleted document is still a candidate")
	}
	if want := len(wantIDs(t, s, reader(), store.Request{})); got.Total != want {
		t.Fatalf("Total = %d after a delete, expected %d", got.Total, want)
	}
}

// wantCorpus computes the statistics the slow way, over every queryable
// document in the tenant.
func wantCorpus(docs []doc.Document, terms []string) store.Corpus {
	want := make(map[string]bool, len(terms))
	for _, t := range terms {
		want[t] = true
	}
	c := store.Corpus{DocFreq: map[string]int{}}
	for _, d := range docs {
		if !d.Queryable() {
			continue
		}
		a := d.Analyze()
		c.Documents++
		c.TitleTokens += int64(a.TitleTokens)
		c.BodyTokens += int64(a.BodyTokens)
		for term := range a.Terms {
			if want[term] {
				c.DocFreq[term]++
			}
		}
	}
	return c
}

func checkCorpus(t *testing.T, st store.Statistician, p *acl.Principal, docs []doc.Document, terms []string) {
	t.Helper()
	got, err := st.Statistics(t.Context(), p, terms)
	if err != nil {
		t.Fatalf("Statistics: %v", err)
	}
	want := wantCorpus(docs, terms)
	if got.Documents != want.Documents {
		t.Fatalf("Documents = %d, expected %d", got.Documents, want.Documents)
	}
	if got.TitleTokens != want.TitleTokens || got.BodyTokens != want.BodyTokens {
		t.Fatalf("token sums are %d title and %d body, expected %d and %d",
			got.TitleTokens, got.BodyTokens, want.TitleTokens, want.BodyTokens)
	}
	for term, n := range want.DocFreq {
		if got.DocFreq[term] != n {
			t.Fatalf("%d documents carry %q, expected %d", got.DocFreq[term], term, n)
		}
	}
	for term, n := range got.DocFreq {
		if want.DocFreq[term] == 0 {
			t.Fatalf("%q is reported in %d documents and nothing carries it", term, n)
		}
	}
}

// statTerms covers a term in several documents, a term in one, and a term
// nothing carries, which is the case a driver returning zero instead of nothing
// gets wrong quietly.
var statTerms = []string{"payments", "onboarding", "zeppelin"}

func testStatisticsCorpus(t *testing.T, s store.Store) {
	st := statistician(t, s)
	docs := corpus()
	mustPut(t, s, docs...)
	checkCorpus(t, st, reader(), docs, statTerms)
}

func testStatisticsTenant(t *testing.T, s store.Store) {
	st := statistician(t, s)
	docs := corpus()
	mustPut(t, s, docs...)

	// The counts are a property of the corpus and not of the asker. See
	// [store.Corpus] for why: a per asker document frequency costs a permission
	// filtered aggregate over every document carrying the term, on every query,
	// to move the ranking by a fraction of a percent. This case pins the choice
	// so that a driver cannot make it differently.
	checkCorpus(t, st, stranger(), docs, statTerms)
}

func testStatisticsReplace(t *testing.T, s store.Store) {
	st := statistician(t, s)
	docs := corpus()
	mustPut(t, s, docs...)

	// The replacement is shorter and drops a term the original carried, so a
	// driver that adds the new numbers without taking the old ones back out is
	// off in both the lengths and the frequency.
	docs[0].Title = "Runbook"
	docs[0].Body = "moved"
	mustPut(t, s, docs[0])
	checkCorpus(t, st, reader(), docs, statTerms)

	// Twice, because writing the same document again is the ordinary case for a
	// connector that re-syncs, and counting it twice is the ordinary bug.
	mustPut(t, s, docs[0])
	checkCorpus(t, st, reader(), docs, statTerms)
}

func testStatisticsDelete(t *testing.T, s store.Store) {
	st := statistician(t, s)
	docs := corpus()
	mustPut(t, s, docs...)

	if err := s.Delete(t.Context(), docs[0].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	checkCorpus(t, st, reader(), docs[1:], statTerms)

	// Deleting what is already gone must not take it out a second time.
	if err := s.Delete(t.Context(), docs[0].ID); err != nil {
		t.Fatalf("Delete again: %v", err)
	}
	checkCorpus(t, st, reader(), docs[1:], statTerms)
}

func testStatisticsQuarantine(t *testing.T, s store.Store) {
	st := statistician(t, s)
	docs := corpus()
	// A document whose permissions did not resolve is served to nobody, so
	// counting it would mean the ranking is normalised against documents that
	// can never appear in a result.
	docs[0].Permissions = acl.Permissions{}
	mustPut(t, s, docs...)
	checkCorpus(t, st, reader(), docs, statTerms)
}

func testStatisticsNilPrincipal(t *testing.T, s store.Store) {
	st := statistician(t, s)
	mustPut(t, s, corpus()...)

	if _, err := st.Statistics(t.Context(), nil, statTerms); !errors.Is(err, genba.ErrNoPrincipal) {
		t.Fatalf("Statistics with no principal returned %v, expected ErrNoPrincipal", err)
	}
}

func testFetchPage(t *testing.T, s store.Store) {
	f := fetcher(t, s)
	docs := corpus()
	mustPut(t, s, docs...)

	got, err := f.Fetch(t.Context(), reader(), []string{"r3", "r1", "nothing"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	by := make(map[string]doc.Document, len(got))
	for _, d := range got {
		by[d.ID] = d
	}
	if len(by) != 2 {
		t.Fatalf("fetched %d documents for two readable ids and one that does not exist", len(by))
	}
	// The body is the reason this call exists, so a fetch that returns the same
	// metadata the ranking already had is a fetch that did nothing.
	if by["r1"].Body != docs[0].Body || by["r1"].Title != docs[0].Title {
		t.Fatalf("r1 came back as %q / %q", by["r1"].Title, by["r1"].Body)
	}
}

func testFetchPermission(t *testing.T, s store.Store) {
	f := fetcher(t, s)
	docs := corpus()
	mustPut(t, s, docs...)

	got, err := f.Fetch(t.Context(), stranger(), []string{"r1", "r2", "r3", "r4"})
	if err != nil {
		// Not an error, on purpose. An id the asker may not read is absent,
		// because the alternative is a page of twenty results failing because one
		// document was revoked while it was being ranked.
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a stranger fetched %d documents", len(got))
	}
}

func testFetchNilPrincipal(t *testing.T, s store.Store) {
	f := fetcher(t, s)
	mustPut(t, s, corpus()...)

	if _, err := f.Fetch(t.Context(), nil, []string{"r1"}); !errors.Is(err, genba.ErrNoPrincipal) {
		t.Fatalf("Fetch with no principal returned %v, expected ErrNoPrincipal", err)
	}
}
