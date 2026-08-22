package index

import (
	"context"
	"math"
	"sort"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// BM25 parameters. k1 controls how fast term frequency saturates and b how hard
// long documents are penalised. These are the usual defaults, and they are named
// constants so that a ranking experiment shows up as a diff rather than as a
// magic number moving.
const (
	bm25K1 = 1.2
	bm25B  = 0.75

	// titleWeight is how many body occurrences one title occurrence is worth. A
	// document titled "payments runbook" should beat one that mentions payments
	// forty times in a changelog.
	//
	// It lives here and only here. A storage driver stores token counts by
	// field and never a weighted length, so that changing this number is a
	// change to the ranking rather than a reindex of the corpus.
	titleWeight = 3.0
)

// MaxMatches caps how many documents one query scores on the fallback path.
//
// A driver that can select candidates for itself is asked for a pool instead,
// which is what [CandidateFloor] sizes. This is the cap for a driver that
// cannot: a query for a common word otherwise pulls the whole corpus into
// memory to rank it, and the twenty results anyone reads would have been the
// same without the last ninety nine percent of that work. When the cap bites,
// [Results.Truncated] says so, because a total that quietly stops counting is
// worse than one that admits it is a lower bound.
const MaxMatches = 100_000

const (
	// CandidateFloor is the smallest candidate pool a driver is asked for.
	//
	// It is not twenty, because the recency prior reorders. A document ranked a
	// hundred and eightieth on lexical match alone can reach the first page
	// once the prior has been applied to everything above it, and cutting at
	// the page size would make the prior invisible. That is the kind of change
	// that gets discovered as "search got worse" six months later. Five hundred
	// candidates cost about a tenth of a millisecond to score, so the floor is
	// very nearly free.
	CandidateFloor = 500

	// CandidateFactor multiplies the requested window, so that deep paging
	// still ranks over more than it returns.
	CandidateFactor = 10
)

// CandidatePool is how many candidates a query asks its driver for.
//
// It is exported because it is the bound the performance assertions are stated
// against: a search may rank this many candidates and not one more, whatever
// the match set turns out to be.
func CandidatePool(offset, limit int) int {
	return min(max(CandidateFloor, (offset+limit)*CandidateFactor), MaxMatches)
}

// FacetPool is how many matching documents the facet counts are counted over.
//
// The sidebar is read as an ordering and as a set of proportions. Which sources
// a query is finding things in, which of them has most of them, and whether
// ticking one is going to leave anything: all three are settled by the first
// thousand documents of a match set and none of them move when the other fifty
// thousand are counted as well. What counting the rest costs is a read of four
// columns of every matching document, which on a term most of the corpus
// carries is a pass over most of the corpus, on every keystroke, for a number
// nobody reads past its first two digits.
//
// So the counts stop here and say so. A count past the bound is a lower bound
// rather than a count, [Results.Approximate] carries that up, and the interface
// writes it as 1,000+ in the same place it already writes a truncated total.
// The total itself is exact, because it is one count over the predicate and
// costs nothing to be right about.
//
// Twice the candidate floor, so that the sidebar always describes more
// documents than the page was ranked from.
const FacetPool = 2 * CandidateFloor

// pool is one query's retrieval: the candidates worth scoring, the counts over
// the whole match set, and the corpus statistics the scorer needs.
//
// A candidate is not a document. It is the twenty or so bytes of an id, four
// display strings, a date and the per term counts, which is everything the
// ranking reads and nothing else. The bodies of the twenty documents on the
// page are fetched afterwards, in one statement, because a match set of a
// hundred thousand bodies is hundreds of megabytes of text that only the page's
// worth is ever read from.
type pool struct {
	cands       []store.Candidate
	total       int
	truncated   bool
	facets      map[string][]Facet
	approximate bool
	corpus      store.Corpus
}

// collect asks the driver for the candidates, using the best capability it has.
//
// A driver implementing [store.Ranker] makes the cut inside the statement that
// applies the permission rule, so the work is proportional to the pool. One
// implementing [store.Retriever] answers the match set out of its own index and
// the cut happens here. One implementing neither is walked in full and filtered
// here. All three produce the same ranking over the same documents, which is
// what store/storetest holds a driver to, and the only thing the capability
// changes is how much data had to be touched to get there.
func (s *Searcher) collect(ctx context.Context, p *acl.Principal, r store.Request, sel store.Selection) (pool, error) {
	if rk, ok := s.store.(store.Ranker); ok {
		// The cache goes around this branch and not around the other two. This is
		// the branch a deployment actually runs, and it is the one whose result is
		// a value the driver produced under the permission rule, which is what
		// makes it something a fingerprint can name. The fallbacks are for a driver
		// that has no index, where the cost is the scan and caching the tail of it
		// would be measuring the wrong thing.
		call := func() (store.Ranked, error) { return rk.Rank(ctx, p, r, sel) }
		var (
			ranked store.Ranked
			err    error
		)
		if s.cache != nil {
			ranked, err = s.cache.ranked(p, r, sel, call)
		} else {
			ranked, err = call()
		}
		if err != nil {
			return pool{}, err
		}
		return pool{
			cands:       ranked.Candidates,
			total:       ranked.Total,
			truncated:   ranked.Truncated,
			facets:      facetsFrom(ranked.Facets),
			approximate: ranked.Approximate,
		}, nil
	}

	// The walk is over the request with the facet constraints lifted, and the
	// constraints are applied here instead. A document that fails exactly one of
	// them is not a result and is still a count: it is what ticking that value
	// instead would have found, which is the number the sidebar exists to show.
	// See [store.Request.Without].
	base := r.WithoutFacets()
	var (
		out     pool
		seen    []store.Candidate
		matched int
		counts  = newDrill(r)
		walked  int
	)
	take := func(d doc.Document) bool {
		// The cap is on the walk rather than on the match set, because the walk
		// is the work. A query that reaches it has stopped counting, and
		// truncated is how that is admitted.
		if walked >= MaxMatches {
			out.truncated = true
			return false
		}
		walked++
		if !counts.add(d) {
			return true
		}
		matched++
		// A selection that asked for no candidates is asking what the counts are,
		// and the counting is done by the line above. What is skipped here is
		// re-analysing the document, which is this path's whole cost and is the
		// same work the drivers skip when they do not run the candidate statement.
		if sel.Limit > 0 {
			seen = append(seen, candidateOf(d, r.Terms))
		}
		return true
	}

	var err error
	if rt, ok := s.store.(store.Retriever); ok {
		// The driver has already applied the terms and everything that is not a
		// facet, so what it yields is the match set plus the documents one
		// ticked box away from it.
		err = rt.Retrieve(ctx, p, base, take)
	} else {
		err = s.store.Scan(ctx, p, func(d doc.Document) bool {
			if !base.Matches(d) {
				return true
			}
			return take(d)
		})
	}
	if err != nil {
		return pool{}, err
	}

	// The whole match set is in hand, so the counts are exact and the cut is
	// this side of the driver. Sorting before the cut would be sorting by a
	// score that has not been computed yet, so the pool is trimmed after
	// scoring instead: see [Searcher.Search].
	out.cands = seen
	out.total = matched
	out.facets = counts.facets()
	// Exact, unless [MaxMatches] stopped the walk, in which case the counts are
	// over what was walked and are a lower bound like a driver's bounded ones.
	out.approximate = out.truncated
	return out, nil
}

// drill counts the facets the way a filter panel has to be counted: each field
// with its own constraint lifted, and every other constraint applied.
//
// This is the Go side of what a driver does in SQL, and the two produce the same
// sidebar for the same query, which is what store/storetest holds a driver to. A
// document is offered to it once and lands in one of three places. In the match
// set, where it counts towards all four fields. One constraint short of it,
// where it counts towards that field and nothing else, because it is a result
// that ticking that value would produce. Two or more short, where it counts
// towards nothing: it is not reachable by changing one answer, and a count that
// included it would be a count of a page nobody can get to.
type drill struct {
	r      store.Request
	counts map[string]map[string]int
}

func newDrill(r store.Request) *drill {
	d := &drill{r: r, counts: make(map[string]map[string]int, len(store.FacetFields))}
	for _, field := range store.FacetFields {
		d.counts[field] = map[string]int{}
	}
	return d
}

// add records one document and reports whether it is in the match set.
func (dr *drill) add(d doc.Document) bool {
	missing := ""
	for _, field := range store.FacetFields {
		if dr.r.Passes(field, d) {
			continue
		}
		if missing != "" {
			return false
		}
		missing = field
	}
	if missing != "" {
		dr.counts[missing][valueOf(missing, d)]++
		return false
	}
	for _, field := range store.FacetFields {
		dr.counts[field][valueOf(field, d)]++
	}
	return true
}

func (dr *drill) facets() map[string][]Facet {
	out := make(map[string][]Facet, len(dr.counts))
	for field, counts := range dr.counts {
		out[field] = sortedFacets(counts)
	}
	return out
}

// valueOf is the display string a facet counts a document under. It is the same
// string a driver reads out of its own column, because both of them are what a
// person sees on the row and clicks to filter by.
func valueOf(field string, d doc.Document) string {
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

// candidateOf is a document reduced to what the ranking reads.
func candidateOf(d doc.Document, terms []string) store.Candidate {
	a := d.Analyze()
	c := store.Candidate{
		ID:          d.ID,
		Source:      d.Source,
		Kind:        d.Kind,
		Container:   d.Container,
		Author:      d.Author.Display(),
		ModifiedAt:  d.ModifiedAt,
		TitleTokens: a.TitleTokens,
		BodyTokens:  a.BodyTokens,
	}
	// Only the query terms, for the same reason a driver returns only those: a
	// four hundred word document has three hundred terms the scorer will never
	// look at.
	for _, t := range terms {
		if n, ok := a.Terms[t]; ok {
			if c.Terms == nil {
				c.Terms = make(map[string]doc.TermCount, len(terms))
			}
			c.Terms[t] = n
		}
	}
	return c
}

// statistics reads the corpus numbers the scorer needs.
//
// A driver that maintains them answers in a couple of key lookups. One that
// does not gets them derived from the candidates in hand, which is the old
// behaviour and is an approximation: the document frequency of a term among the
// documents that matched is not its frequency in the corpus. It is kept as a
// fallback so that a driver is not required to implement anything to work at
// all, and both drivers in the tree implement the capability.
func (s *Searcher) statistics(ctx context.Context, p *acl.Principal, terms []string, from pool) (store.Corpus, error) {
	if st, ok := s.store.(store.Statistician); ok {
		call := func() (store.Corpus, error) { return st.Statistics(ctx, p, terms) }
		if s.cache != nil {
			return s.cache.corpusStats(p, terms, call)
		}
		return call()
	}
	c := store.Corpus{Documents: len(from.cands), DocFreq: make(map[string]int, len(terms))}
	for _, cand := range from.cands {
		c.TitleTokens += int64(cand.TitleTokens)
		c.BodyTokens += int64(cand.BodyTokens)
		for t := range cand.Terms {
			c.DocFreq[t]++
		}
	}
	return c, nil
}

// score is BM25F over one candidate.
//
// It is the only place the ranking function exists. A driver could compute this
// in SQL and it would be faster still, and then the function would exist twice,
// once per driver, and the cross driver test that proves nineteen queries
// produce the same answer on both would have to be weakened to "similar". A
// ranking that depends on which deployment you asked is not something anybody
// finds until it is in production.
func score(c store.Candidate, terms []string, corpus store.Corpus) float64 {
	avg := corpus.AvgLength(titleWeight)
	if avg == 0 || corpus.Documents == 0 {
		return 0
	}
	length := titleWeight*float64(c.TitleTokens) + float64(c.BodyTokens)
	norm := bm25K1 * (1 - bm25B + bm25B*length/avg)
	n := float64(corpus.Documents)

	var total float64
	for _, t := range terms {
		count, ok := c.Terms[t]
		if !ok {
			continue
		}
		tf := titleWeight*float64(count.Title) + float64(count.Body)
		if tf == 0 {
			continue
		}
		df := float64(corpus.DocFreq[t])
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		total += idf * tf * (bm25K1 + 1) / (tf + norm)
	}
	return total
}

// facetsFrom carries a driver's counts across, in the same order the Go side
// produces so that the two are comparable value for value.
func facetsFrom(in map[string][]store.Facet) map[string][]Facet {
	out := make(map[string][]Facet, len(in))
	for field, values := range in {
		counts := make(map[string]int, len(values))
		for _, v := range values {
			counts[v.Value] = v.Count
		}
		out[field] = sortedFacets(counts)
	}
	for _, field := range []string{"source", "kind", "container", "author"} {
		if out[field] == nil {
			out[field] = []Facet{}
		}
	}
	return out
}

func sortedFacets(counts map[string]int) []Facet {
	out := make([]Facet, 0, len(counts))
	for v, n := range counts {
		if v == "" {
			continue
		}
		out = append(out, Facet{Value: v, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	if len(out) > store.MaxFacetValues {
		out = out[:store.MaxFacetValues]
	}
	return out
}
