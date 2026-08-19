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
//
// # Where the work happens
//
// A search runs in two phases. Phase one asks the driver which documents are
// worth ranking, and gets back candidates rather than documents: an id, the
// four strings a result row and a facet are drawn from, a date, and the token
// counts for the query terms. Phase two ranks those in Go and then reads the
// bodies for the page, in one statement, which is the only point at which any
// document text is touched. A query matching a hundred thousand documents
// therefore decodes twenty of them.
//
// A driver that implements [store.Ranker] makes the cut inside the same
// statement that applies the permission rule, so the cost follows the page. One
// that implements [store.Retriever] answers the match set out of an index of
// its own and the cut happens here. One that implements neither is walked with
// [store.Store.Scan] and filtered here. The ranking is the same in all three
// cases, over the same match set, which store/storetest checks. The difference
// is only how much of the corpus had to be touched to produce it.
package index

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/cache"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// Query is one search request.
type Query struct {
	// Text is what the person typed, after any operators have been parsed out
	// of it by [Parse].
	Text string

	// The filters. Values inside one field are alternatives and the fields are
	// combined with and, which is what a facet sidebar implies when somebody
	// ticks two sources and one document type.
	Sources    []string
	Kinds      []doc.Kind
	Containers []string
	Authors    []string
	Owners     []string

	// Since and Until bound the modification time. A zero value is unbounded.
	Since time.Time
	Until time.Time

	// Sort selects the order. The zero value ranks by relevance.
	Sort Sort

	// Limit is the number of hits to return. Zero means DefaultLimit.
	Limit int

	// Offset is how many hits to skip, for paging.
	Offset int
}

// Sort is the order results come back in.
type Sort string

// The orders a query can ask for.
const (
	// ByRelevance ranks on the query. It is the zero value because a search
	// with no opinion about order wants the best match first.
	ByRelevance Sort = ""

	// ByRecent ranks on modification time, which is what somebody asking what
	// changed wants, and what a relevance score answers badly.
	ByRecent Sort = "recency"
)

// DefaultLimit is the page size used when a query does not ask for one.
const DefaultLimit = 20

// MaxLimit caps the page size, so one caller cannot ask a store to
// materialise its whole corpus.
const MaxLimit = 200

// Request is the match set this query describes, which is what a storage driver
// is asked for.
func (q Query) Request() store.Request {
	return store.Request{
		Terms:      queryTerms(q.Text),
		Sources:    q.Sources,
		Kinds:      q.Kinds,
		Containers: q.Containers,
		Authors:    q.Authors,
		Owners:     q.Owners,
		Since:      q.Since,
		Until:      q.Until,
	}
}

// Result is one ranked document.
type Result struct {
	Document doc.Document
	Score    float64

	// Snippet is a short passage from the body around the first match, meant to
	// be shown under the title. It is plain text, and it is the caller's job to
	// escape it.
	Snippet string

	// Passages is the same text as Snippet, split so that the runs that matched
	// the query are marked.
	//
	// The split happens here rather than in the browser because the client
	// would have to reimplement the analyzer to do it, and a client that
	// tokenizes differently from the index highlights the wrong words. It is
	// also what keeps the interface from having to build markup out of offsets
	// into a string whose encoding it is guessing at.
	Passages []Passage
}

// Passage is a run of snippet text, either matched by the query or not.
type Passage struct {
	Text  string
	Match bool
}

// Facet is one value of a facet and how many hits carry it.
type Facet struct {
	Value string
	Count int
}

// Results is what a search returns.
type Results struct {
	Hits []Result

	// Total is the number of documents that matched before paging. When
	// Truncated is set it is a lower bound, because [MaxMatches] stopped the
	// count.
	Total     int
	Truncated bool

	// Facets are counted over the whole match set, not over the page, which is
	// what makes them usable as filters.
	Facets map[string][]Facet

	// Took is how long the search ran for. It is reported rather than logged
	// because the interface shows it, and a number a user can see is a number
	// somebody will keep honest.
	Took time.Duration
}

// Searcher runs queries against a store.
type Searcher struct {
	store store.Store
	now   func() time.Time

	// cache is the derived state a repeated query reuses, and is nil when the
	// searcher was built without one. See cache.go for what is in it and for the
	// rule its keys obey.
	cache     *Cache
	stopWatch func()

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

// WithCache gives the searcher somewhere to reuse the work a repeated query
// would otherwise redo. A searcher without one is correct and slower, which is
// the property that makes the cache safe to have.
func WithCache(c *Cache) Option {
	return func(s *Searcher) { s.cache = c }
}

// New returns a searcher over st.
//
// A searcher given a cache subscribes it to the store's writes here rather than
// leaving that to the caller. Wiring a cache up and forgetting to invalidate it
// is not a mistake that produces an error: it produces a search that quietly
// answers from last week, so it is not left as a step somebody can miss.
func New(st store.Store, opts ...Option) *Searcher {
	s := &Searcher{store: st, now: time.Now, halfLife: 180 * 24 * time.Hour}
	for _, opt := range opts {
		opt(s)
	}
	if s.cache != nil {
		s.stopWatch = s.cache.Watch(st)
	}
	return s
}

// Close releases what the searcher holds, which is at most a subscription to
// the store's writes. It does not close the store: the searcher did not open it
// and something else is still using it.
//
// A process wide searcher never needs this. It is here for a test, and for an
// embedder that builds a searcher per tenant and would otherwise accumulate
// subscriptions for tenants nobody is querying.
func (s *Searcher) Close() error {
	if s.stopWatch != nil {
		s.stopWatch()
		s.stopWatch = nil
	}
	return nil
}

// CacheStats reports what each cache layer has done, and nil when the searcher
// has no cache.
func (s *Searcher) CacheStats() map[string]cache.Stats {
	if s.cache == nil {
		return nil
	}
	return s.cache.Stats()
}

// Retrieving reports whether the store behind this searcher can serve a match
// set out of an index of its own, rather than being walked document by
// document. It is here so that an operator can be told which one they are
// running on instead of having to infer it from a latency graph.
func (s *Searcher) Retrieving() bool {
	if _, ok := s.store.(store.Ranker); ok {
		return true
	}
	_, ok := s.store.(store.Retriever)
	return ok
}

// Ranking reports whether the store can cut to a candidate pool for itself,
// which is the difference between a search whose cost follows the page and one
// whose cost follows the match set.
func (s *Searcher) Ranking() bool {
	_, ok := s.store.(store.Ranker)
	return ok
}

// Search ranks the documents the principal may read.
//
// It returns an error matching genba.ErrNoPrincipal when p is nil. There is no
// anonymous search path, and there is no flag that adds one.
func (s *Searcher) Search(ctx context.Context, p *acl.Principal, q Query) (Results, error) {
	start := s.now()

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)

	req := q.Request()
	sel := store.Selection{Limit: CandidatePool(q.Offset, limit), Recent: q.Sort == ByRecent}

	// Phase one. Which documents are worth ranking, how many matched, and what
	// the facet counts are, all under the permission rule and none of it
	// carrying a body.
	found, err := s.collect(ctx, p, req, sel)
	if err != nil {
		return Results{}, err
	}
	if found.corpus, err = s.statistics(ctx, p, req.Terms, found); err != nil {
		return Results{}, err
	}

	res := Results{
		Total:     found.total,
		Truncated: found.truncated,
		Facets:    found.facets,
	}
	if len(found.cands) == 0 {
		res.Took = s.now().Sub(start)
		return res, nil
	}

	// Phase two. Ranking, in Go, over the candidates and nothing else.
	now := s.now()
	scored := make([]ranked, 0, len(found.cands))
	for _, c := range found.cands {
		scored = append(scored, ranked{
			cand:  c,
			score: score(c, req.Terms, found.corpus) * s.recency(c.ModifiedAt, now),
		})
	}

	// Sort by score, then by id, so equal scores do not shuffle between runs and
	// paging stays stable.
	switch q.Sort {
	case ByRecent:
		sort.SliceStable(scored, func(i, j int) bool {
			if !scored[i].cand.ModifiedAt.Equal(scored[j].cand.ModifiedAt) {
				return scored[i].cand.ModifiedAt.After(scored[j].cand.ModifiedAt)
			}
			return scored[i].cand.ID < scored[j].cand.ID
		})
	default:
		sort.SliceStable(scored, func(i, j int) bool {
			if scored[i].score != scored[j].score {
				return scored[i].score > scored[j].score
			}
			return scored[i].cand.ID < scored[j].cand.ID
		})
	}

	// Bodies and snippets are for the page and not for the match set. On a broad
	// query that is the difference between reading twenty documents and reading
	// a hundred thousand, and it is why phase one never asks for a body.
	window := page(scored, q.Offset, limit)
	bodies, err := s.fetch(ctx, p, window)
	if err != nil {
		return Results{}, err
	}
	// A candidate whose document did not come back is dropped rather than shown
	// from the metadata that was ranked. The fetch is the last thing that applies
	// the permission rule, so an id it declined is an id this person may not read
	// now, whatever was true when the ordering was made. Showing the title, the
	// container and the author of a document somebody just lost access to is a
	// leak, and it is a leak that gets more likely the longer an ordering is
	// allowed to be reused. Losing a row from the page is the cheaper mistake.
	res.Hits = make([]Result, 0, len(window))
	for _, r := range window {
		full, ok := bodies[r.cand.ID]
		if !ok {
			continue
		}
		hit := Result{Document: full, Score: r.score}
		hit.Snippet, hit.Passages = snippet(readable(full), req.Terms)
		res.Hits = append(res.Hits, hit)
	}
	res.Took = s.now().Sub(start)
	return res, nil
}

// ranked is a candidate and what it scored.
type ranked struct {
	cand  store.Candidate
	score float64
}

// fetch reads the documents behind one page.
//
// A driver that can do it in one statement is asked to. One that cannot is
// asked document by document, which is the old behaviour and is correct, just
// twenty round trips instead of one. Either way an id that has stopped being
// readable between the ranking and now is simply missing from what comes back,
// and [Searcher.Search] drops it from the page rather than failing the request.
//
// That makes this the point where a stale ordering is checked against the
// permission rule as it stands now, which is what lets an ordering be reused
// for thirty seconds without a revocation in that window being visible for
// thirty seconds.
func (s *Searcher) fetch(ctx context.Context, p *acl.Principal, window []ranked) (map[string]doc.Document, error) {
	out := make(map[string]doc.Document, len(window))
	if len(window) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(window))
	for _, r := range window {
		ids = append(ids, r.cand.ID)
	}

	// This is the one place the document cache is consulted, and the ids handed
	// to it are the ids the retrieval above just produced under this principal's
	// permission predicate. That ordering is what makes an entry keyed by id
	// alone safe, and it is why the lookup is here rather than behind
	// store.Store.Get where any id at all could arrive. See [Cache.document].
	if s.cache != nil {
		var missing []string
		out, missing = s.cache.document(ids)
		if len(missing) == 0 {
			return out, nil
		}
		ids = missing
	}

	fetched, err := s.read(ctx, p, ids)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		s.cache.putDocuments(fetched)
	}
	for id, d := range fetched {
		out[id] = d
	}
	return out, nil
}

// read is the fetch itself, with no cache in front of it.
func (s *Searcher) read(ctx context.Context, p *acl.Principal, ids []string) (map[string]doc.Document, error) {
	out := make(map[string]doc.Document, len(ids))
	if f, ok := s.store.(store.Fetcher); ok {
		docs, err := f.Fetch(ctx, p, ids)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			out[d.ID] = d
		}
		return out, nil
	}
	for _, id := range ids {
		d, err := s.store.Get(ctx, p, id)
		if err != nil {
			continue
		}
		out[id] = d
	}
	return out, nil
}

// page slices a sorted result set, tolerating an offset past the end.
func page(all []ranked, offset, limit int) []ranked {
	if offset < 0 || offset >= len(all) {
		return nil
	}
	return all[offset:min(offset+limit, len(all))]
}

// recency is a multiplier in (0, 1]. A document modified now scores at full
// weight and one older than several half lives keeps a floor, because an old
// document that is a perfect lexical match is still the right answer.
func (s *Searcher) recency(modified, now time.Time) float64 {
	if s.halfLife <= 0 || modified.IsZero() {
		return 1
	}
	age := now.Sub(modified)
	if age <= 0 {
		return 1
	}
	const floor = 0.5
	decay := math.Exp2(-age.Hours() / s.halfLife.Hours())
	return floor + (1-floor)*decay
}

// SnippetWidth is roughly how many characters of body a snippet shows.
const SnippetWidth = 260

// snippet returns a passage of the body around the first query match, and the
// same passage split at the matches so the interface can mark them.
func snippet(body string, terms []string) (string, []Passage) {
	if body == "" {
		return "", nil
	}
	runes := []rune(body)
	start := 0
	if idx := firstMatch(body, terms); idx > 0 {
		start = max(0, len([]rune(body[:idx]))-SnippetWidth/3)
	}
	end := min(start+SnippetWidth, len(runes))

	text := strings.TrimSpace(collapse(string(runes[start:end])))
	passages := mark(text, terms)
	if start > 0 {
		text = "..." + text
		passages = append([]Passage{{Text: "..."}}, passages...)
	}
	if end < len(runes) {
		text += "..."
		passages = append(passages, Passage{Text: "..."})
	}
	return text, passages
}

// collapse turns runs of whitespace into single spaces. A snippet lifted out of
// a source file otherwise carries its indentation into a result card, where it
// reads as a broken layout rather than as faithful text.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// mark splits text at the query terms. It analyses the text the same way the
// index does, so what is highlighted is what was matched rather than what a
// substring search happened to find: a search for "run" does not light up the
// middle of "runbook", because the index did not match it there either.
func mark(text string, terms []string) []Passage {
	if text == "" || len(terms) == 0 {
		return []Passage{{Text: text}}
	}
	want := make(map[string]bool, len(terms))
	for _, t := range terms {
		want[t] = true
	}

	var (
		out  []Passage
		last int
	)
	for _, s := range doc.Analyze(text) {
		if !want[s.Term] {
			continue
		}
		if s.Start > last {
			out = append(out, Passage{Text: text[last:s.Start]})
		}
		out = append(out, Passage{Text: text[s.Start:s.End], Match: true})
		last = s.End
	}
	if last < len(text) {
		out = append(out, Passage{Text: text[last:]})
	}
	return out
}

// firstMatch returns the byte offset of the first query term in body, or -1.
func firstMatch(body string, terms []string) int {
	want := make(map[string]bool, len(terms))
	for _, t := range terms {
		want[t] = true
	}
	for _, s := range doc.Analyze(body) {
		if want[s.Term] {
			return s.Start
		}
	}
	return -1
}
