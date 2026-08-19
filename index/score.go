package index

import (
	"context"
	"math"

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
	titleWeight = 3.0
)

// MaxMatches caps how many documents one query scores.
//
// A query for a common word against a large corpus otherwise pulls the whole
// corpus into memory to rank it, and the twenty results anyone reads would have
// been the same without the last ninety nine percent of that work. When the cap
// bites, [Results.Truncated] says so, because a total that quietly stops
// counting is worse than one that admits it is a lower bound.
const MaxMatches = 100_000

// candidates is the match set of one query along with the statistics needed to
// score it.
//
// The documents held here have had their bodies dropped. Everything the ranking
// and the facets need is either a small metadata field or already folded into
// the term frequencies, and a match set of a hundred thousand bodies is hundreds
// of megabytes of text that only twenty documents' worth ever gets read. The
// page's bodies are fetched back afterwards, which is twenty point reads instead
// of one very large allocation.
type candidates struct {
	docs   []doc.Document
	tf     []map[string]float64
	length []float64
	df     map[string]int
	avgLen float64

	// truncated records that [MaxMatches] stopped the walk, so the count above
	// can be reported as the lower bound it is.
	truncated bool
}

// collect builds the match set.
//
// A driver that implements [store.Retriever] is asked for the set directly and
// answers it out of an index of its own, with the principal, the terms and the
// filters all applied inside its own query. A driver that does not is walked in
// full and filtered here. Both paths produce the same documents, which is what
// store/storetest holds a driver to, so the only thing the capability changes is
// how much data had to be touched to get them.
func (s *Searcher) collect(ctx context.Context, p *acl.Principal, r store.Request) (*candidates, error) {
	c := &candidates{df: make(map[string]int, len(r.Terms))}
	want := make(map[string]bool, len(r.Terms))
	for _, t := range r.Terms {
		want[t] = true
	}

	var totalLen float64
	take := func(d doc.Document, terms []string) bool {
		if len(c.docs) >= MaxMatches {
			c.truncated = true
			return false
		}
		title := doc.Tokenize(d.Title)
		length := titleWeight*float64(len(title)) + float64(len(terms)-len(title))

		tf := make(map[string]float64)
		for i, t := range terms {
			if !want[t] {
				continue
			}
			if i < len(title) {
				tf[t] += titleWeight
			} else {
				tf[t]++
			}
		}
		for t := range tf {
			c.df[t]++
		}

		d.Body = "" // fetched back for the page only, see [Searcher.Search]
		c.docs = append(c.docs, d)
		c.tf = append(c.tf, tf)
		c.length = append(c.length, length)
		totalLen += length
		return true
	}

	var err error
	if rt, ok := s.store.(store.Retriever); ok {
		// The driver has already applied the terms, so every document it yields
		// is in the match set and only its statistics are still needed.
		err = rt.Retrieve(ctx, p, r, func(d doc.Document) bool {
			return take(d, d.Terms())
		})
	} else {
		err = s.store.Scan(ctx, p, func(d doc.Document) bool {
			if !r.Filters(d) {
				return true
			}
			// Analyse once and use the result for both the term test and the
			// statistics, rather than tokenizing every document in the corpus
			// twice to answer one query.
			terms := d.Terms()
			if len(r.Terms) > 0 && !anyWanted(terms, want) {
				return true
			}
			return take(d, terms)
		})
	}
	if err != nil {
		return nil, err
	}
	if n := len(c.docs); n > 0 {
		c.avgLen = totalLen / float64(n)
	}
	return c, nil
}

func anyWanted(terms []string, want map[string]bool) bool {
	for _, t := range terms {
		if want[t] {
			return true
		}
	}
	return false
}

// bm25 scores document i against the query terms.
func (c *candidates) bm25(i int, terms []string) float64 {
	if c.avgLen == 0 {
		return 0
	}
	n := float64(len(c.docs))
	norm := bm25K1 * (1 - bm25B + bm25B*c.length[i]/c.avgLen)

	var score float64
	for _, t := range terms {
		tf := c.tf[i][t]
		if tf == 0 {
			continue
		}
		df := float64(c.df[t])
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		score += idf * tf * (bm25K1 + 1) / (tf + norm)
	}
	return score
}
