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
	cands     []store.Candidate
	total     int
	truncated bool
	facets    map[string][]Facet
	corpus    store.Corpus
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
		ranked, err := rk.Rank(ctx, p, r, sel)
		if err != nil {
			return pool{}, err
		}
		return pool{
			cands:     ranked.Candidates,
			total:     ranked.Total,
			truncated: ranked.Truncated,
			facets:    facetsFrom(ranked.Facets),
		}, nil
	}

	var (
		out  pool
		seen []store.Candidate
	)
	take := func(d doc.Document) bool {
		if len(seen) >= MaxMatches {
			out.truncated = true
			return false
		}
		seen = append(seen, candidateOf(d, r.Terms))
		return true
	}

	var err error
	if rt, ok := s.store.(store.Retriever); ok {
		// The driver has already applied the terms and the filters, so every
		// document it yields is in the match set.
		err = rt.Retrieve(ctx, p, r, take)
	} else {
		err = s.store.Scan(ctx, p, func(d doc.Document) bool {
			if !r.Matches(d) {
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
	out.total = len(seen)
	out.facets = facetsOf(seen)
	return out, nil
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
		return st.Statistics(ctx, p, terms)
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

// facetsOf counts the facets over a match set the caller holds in full.
func facetsOf(cands []store.Candidate) map[string][]Facet {
	bySource := map[string]int{}
	byKind := map[string]int{}
	byContainer := map[string]int{}
	byAuthor := map[string]int{}
	for _, c := range cands {
		bySource[c.Source]++
		byKind[string(c.Kind)]++
		byContainer[c.Container]++
		byAuthor[c.Author]++
	}
	return map[string][]Facet{
		"source":    sortedFacets(bySource),
		"kind":      sortedFacets(byKind),
		"container": sortedFacets(byContainer),
		"author":    sortedFacets(byAuthor),
	}
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
