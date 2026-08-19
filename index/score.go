package index

import (
	"context"
	"math"
	"slices"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
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

// candidates is the match set of one query along with the statistics needed to
// score it.
//
// The whole match set is held in memory. That is honest for a corpus a single
// node serves and it is the wrong shape for a large one, where the scoring has
// to move into the driver's scan so that only a heap of the top hits ever
// crosses the interface. The interface already allows that: a driver is free to
// score during Scan, and this package becomes the fallback for drivers that do
// not.
type candidates struct {
	docs   []doc.Document
	tf     []map[string]float64
	length []float64
	df     map[string]int
	avgLen float64
}

func (s *Searcher) collect(ctx context.Context, p *acl.Principal, q Query, terms []string) (*candidates, error) {
	c := &candidates{df: make(map[string]int, len(terms))}
	want := make(map[string]bool, len(terms))
	for _, t := range terms {
		want[t] = true
	}

	var totalLen float64
	err := s.store.Scan(ctx, p, func(d doc.Document) bool {
		if !matches(d, q) {
			return true
		}
		title, body := Tokenize(d.Title), Tokenize(d.Body)
		length := titleWeight*float64(len(title)) + float64(len(body))

		tf := make(map[string]float64)
		for _, t := range title {
			if want[t] {
				tf[t] += titleWeight
			}
		}
		for _, t := range body {
			if want[t] {
				tf[t]++
			}
		}
		for t := range tf {
			c.df[t]++
		}

		c.docs = append(c.docs, d)
		c.tf = append(c.tf, tf)
		c.length = append(c.length, length)
		totalLen += length
		return true
	})
	if err != nil {
		return nil, err
	}
	if n := len(c.docs); n > 0 {
		c.avgLen = totalLen / float64(n)
	}
	return c, nil
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

// matches applies the structured filters. They are conjunctive across fields
// and disjunctive within one field, which is what a facet sidebar implies when
// somebody ticks two sources.
func matches(d doc.Document, q Query) bool {
	if len(q.Sources) > 0 && !slices.Contains(q.Sources, d.Source) {
		return false
	}
	if len(q.Kinds) > 0 && !slices.Contains(q.Kinds, d.Kind) {
		return false
	}
	if q.Container != "" && d.Container != q.Container {
		return false
	}
	return true
}
