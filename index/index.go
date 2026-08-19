// Package index turns a query and a principal into ranked results.
//
// It sits directly on top of a [store.Store] and never sees a document the
// principal may not read, because the driver applies the principal while it
// walks its own data. That is the whole reason the permission check lives down
// there and not here: this package can be rewritten, replaced or benchmarked
// without anyone having to re check whether it leaks.
//
// The ranking here is lexical BM25F with a mild recency prior. It is the
// baseline every later ranking change is measured against, so it is worth
// having it be a real implementation rather than a placeholder. Semantic
// retrieval and learned ranking arrive as additional retrievers behind the same
// [Searcher] surface, not as a rewrite of it.
package index

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// Query is one search request.
type Query struct {
	// Text is what the person typed.
	Text string

	// Sources, Kinds and Container narrow the candidate set. An empty filter
	// matches everything.
	Sources   []string
	Kinds     []doc.Kind
	Container string

	// Limit is the number of hits to return. Zero means DefaultLimit.
	Limit int

	// Offset is how many hits to skip, for paging.
	Offset int
}

// DefaultLimit is the page size used when a query does not ask for one.
const DefaultLimit = 20

// MaxLimit caps the page size, so one caller cannot ask a store to
// materialise its whole corpus.
const MaxLimit = 200

// Result is one ranked document.
type Result struct {
	Document doc.Document
	Score    float64

	// Snippet is a short passage from the body around the first match, meant to
	// be shown under the title. It is plain text, and it is the caller's job to
	// escape it.
	Snippet string
}

// Facet is one value of a facet and how many hits carry it.
type Facet struct {
	Value string
	Count int
}

// Results is what a search returns.
type Results struct {
	Hits []Result

	// Total is the number of documents that matched before paging.
	Total int

	// Facets are counted over the whole match set, not over the page, which is
	// what makes them usable as filters.
	Facets map[string][]Facet
}

// Searcher runs queries against a store.
type Searcher struct {
	store store.Store
	now   func() time.Time

	// halfLife is how long it takes for the recency prior to decay by half. A
	// document that has not changed in a year should not outrank this week's
	// runbook on a tie, and it should not be buried either.
	halfLife time.Duration
}

// Option configures a [Searcher].
type Option func(*Searcher)

// WithClock replaces the clock, which lets tests pin the recency prior.
func WithClock(now func() time.Time) Option {
	return func(s *Searcher) { s.now = now }
}

// WithRecencyHalfLife sets how fast the recency prior decays. A zero or
// negative value turns the prior off.
func WithRecencyHalfLife(d time.Duration) Option {
	return func(s *Searcher) { s.halfLife = d }
}

// New returns a searcher over st.
func New(st store.Store, opts ...Option) *Searcher {
	s := &Searcher{store: st, now: time.Now, halfLife: 180 * 24 * time.Hour}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Search ranks the documents the principal may read.
//
// It returns an error matching genba.ErrNoPrincipal when p is nil. There is no
// anonymous search path, and there is no flag that adds one.
func (s *Searcher) Search(ctx context.Context, p *acl.Principal, q Query) (Results, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)

	terms := queryTerms(q.Text)

	cands, err := s.collect(ctx, p, q, terms)
	if err != nil {
		return Results{}, err
	}

	res := Results{
		Total:  len(cands.docs),
		Facets: facetsOf(cands.docs),
	}
	if len(cands.docs) == 0 {
		return res, nil
	}

	now := s.now()
	scored := make([]Result, 0, len(cands.docs))
	for i, d := range cands.docs {
		score := cands.bm25(i, terms)
		if score == 0 && len(terms) > 0 {
			continue
		}
		score *= s.recency(d, now)
		scored = append(scored, Result{Document: d, Score: score, Snippet: snippet(d.Body, terms)})
	}

	// Sort by score, then by id, so equal scores do not shuffle between runs and
	// paging stays stable.
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Document.ID < scored[j].Document.ID
	})

	res.Total = len(scored)
	res.Hits = page(scored, q.Offset, limit)
	return res, nil
}

// page slices a sorted result set, tolerating an offset past the end.
func page(all []Result, offset, limit int) []Result {
	if offset < 0 || offset >= len(all) {
		return nil
	}
	return all[offset:min(offset+limit, len(all))]
}

// recency is a multiplier in (0, 1]. A document modified now scores at full
// weight and one older than several half lives keeps a floor, because an old
// document that is a perfect lexical match is still the right answer.
func (s *Searcher) recency(d doc.Document, now time.Time) float64 {
	if s.halfLife <= 0 || d.ModifiedAt.IsZero() {
		return 1
	}
	age := now.Sub(d.ModifiedAt)
	if age <= 0 {
		return 1
	}
	const floor = 0.5
	decay := math.Exp2(-age.Hours() / s.halfLife.Hours())
	return floor + (1-floor)*decay
}

func snippet(body string, terms []string) string {
	const width = 240
	if body == "" {
		return ""
	}
	runes := []rune(body)
	start := 0
	if idx := firstMatch(body, terms); idx > 0 {
		start = max(0, len([]rune(body[:idx]))-width/3)
	}
	end := min(start+width, len(runes))
	out := strings.TrimSpace(string(runes[start:end]))
	if start > 0 {
		out = "..." + out
	}
	if end < len(runes) {
		out += "..."
	}
	return out
}

// firstMatch returns the byte offset of the earliest query term in body, or -1.
func firstMatch(body string, terms []string) int {
	lower := strings.ToLower(body)
	best := -1
	for _, t := range terms {
		if i := strings.Index(lower, t); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

func facetsOf(docs []doc.Document) map[string][]Facet {
	bySource := map[string]int{}
	byKind := map[string]int{}
	for _, d := range docs {
		bySource[d.Source]++
		byKind[string(d.Kind)]++
	}
	return map[string][]Facet{
		"source": sortedFacets(bySource),
		"kind":   sortedFacets(byKind),
	}
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
	return out
}
