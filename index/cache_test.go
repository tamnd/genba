package index_test

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
)

// A cache is only ever as good as the assertion that it did not answer the
// wrong person. So most of what is below is about who got what, and the two
// tests about speed are the short ones.

// counting is a driver that ranks and fetches like a real one and says how many
// times it was asked.
//
// It is built on the reference driver rather than on SQLite because the thing
// under test is the searcher's use of the cache, and a test that has to open a
// database to prove that a second search did no work is a test that is measuring
// two things.
type counting struct {
	*memstore.Store

	mu      sync.Mutex
	ranks   int
	stats   int
	fetches int
	fetched []string

	// hold blocks every rank until it is closed, which is how the single flight
	// assertion gets sixteen readers onto one cold key.
	hold chan struct{}
}

func newCounting(t *testing.T, fixtures []fixture) *counting {
	t.Helper()
	st := &counting{Store: memstore.New()}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Put(t.Context(), documents(fixtures)...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return st
}

func (s *counting) Rank(ctx context.Context, p *acl.Principal, r store.Request, sel store.Selection) (store.Ranked, error) {
	s.mu.Lock()
	s.ranks++
	hold := s.hold
	s.mu.Unlock()
	if hold != nil {
		<-hold
	}

	var out store.Ranked
	err := s.Scan(ctx, p, func(d doc.Document) bool {
		if !r.Matches(d) {
			return true
		}
		out.Total++
		a := d.Analyze()
		c := store.Candidate{
			ID:          d.ID,
			ModifiedAt:  d.ModifiedAt,
			TitleTokens: a.TitleTokens,
			BodyTokens:  a.BodyTokens,
			Terms:       make(map[string]doc.TermCount, len(r.Terms)),
		}
		for _, t := range r.Terms {
			if n, ok := a.Terms[t]; ok {
				c.Terms[t] = n
			}
		}
		out.Candidates = append(out.Candidates, c)
		return true
	})
	if err != nil {
		return store.Ranked{}, err
	}
	slices.SortFunc(out.Candidates, func(a, b store.Candidate) int {
		return int(b.ModifiedAt.Sub(a.ModifiedAt))
	})
	if sel.Limit > 0 && len(out.Candidates) > sel.Limit {
		out.Candidates = out.Candidates[:sel.Limit]
		out.Truncated = true
	}
	return out, nil
}

func (s *counting) Statistics(ctx context.Context, p *acl.Principal, terms []string) (store.Corpus, error) {
	s.mu.Lock()
	s.stats++
	s.mu.Unlock()
	return s.Store.Statistics(ctx, p, terms)
}

func (s *counting) Fetch(ctx context.Context, p *acl.Principal, ids []string) ([]doc.Document, error) {
	s.mu.Lock()
	s.fetches++
	s.fetched = append(s.fetched, ids...)
	s.mu.Unlock()

	// Held in a variable rather than reached through the embedded field on
	// every pass, so that this stays a read of the wrapped store even if
	// counting ever grows a Get of its own.
	base := s.Store
	out := make([]doc.Document, 0, len(ids))
	for _, id := range ids {
		d, err := base.Get(ctx, p, id)
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *counting) counts() (ranks, stats, fetches int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ranks, s.stats, s.fetches
}

// documents turns the shared fixtures into documents. It is the body of
// newSearcher, split out so that a test can put the same corpus behind a
// different driver.
func documents(fixtures []fixture) []doc.Document {
	docs := make([]doc.Document, 0, len(fixtures))
	for _, f := range fixtures {
		if f.source == "" {
			f.source = "gdrive"
		}
		if f.kind == "" {
			f.kind = doc.KindPage
		}
		var props map[string]string
		if f.media != "" {
			props = map[string]string{doc.MediaType: f.media}
		}
		docs = append(docs, doc.Document{
			ID:          f.id,
			Tenant:      "acme",
			Source:      f.source,
			Kind:        f.kind,
			Title:       f.title,
			Body:        f.body,
			ModifiedAt:  f.modified,
			Permissions: f.perm,
			Properties:  props,
		})
	}
	return docs
}

func cachedSearcher(t *testing.T, st store.Store, opts ...index.CacheOption) (*index.Searcher, *index.Cache) {
	t.Helper()
	c := index.NewCache(opts...)
	s := index.New(st, index.WithClock(clock), index.WithCache(c))
	t.Cleanup(func() { _ = s.Close() })
	return s, c
}

func search(t *testing.T, s *index.Searcher, p *acl.Principal, q index.Query) index.Results {
	t.Helper()
	res, err := s.Search(t.Context(), p, q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return res
}

func TestRepeatedSearchIsAnsweredFromTheCache(t *testing.T) {
	st := newCounting(t, corpus)
	s, _ := cachedSearcher(t, st)
	p := principal("gdrive:eng@acme.com")

	first := search(t, s, p, index.Query{Text: "payments"})
	second := search(t, s, p, index.Query{Text: "payments"})

	if !slices.Equal(ids(first), ids(second)) {
		t.Fatalf("the cached answer differs from the first: %v then %v", ids(first), ids(second))
	}
	ranks, stats, fetches := st.counts()
	if ranks != 1 {
		t.Errorf("the driver ranked %d times for two identical searches, want 1", ranks)
	}
	if stats != 1 {
		t.Errorf("the driver computed statistics %d times for two identical searches, want 1", stats)
	}
	if fetches != 1 {
		t.Errorf("the driver fetched %d times for two identical searches, want 1", fetches)
	}
}

// TestCacheNeverCrossesVisibility is the assertion the whole design exists for.
// Two people who may read different documents type the same query, and neither
// of them may be handed the other's answer.
func TestCacheNeverCrossesVisibility(t *testing.T) {
	st := newCounting(t, corpus)
	s, _ := cachedSearcher(t, st)

	engineer := principal("gdrive:eng@acme.com")
	seller := &acl.Principal{
		Tenant:  "acme",
		Subject: "u_sam",
		Groups:  acl.GroupSet{Version: 1, Members: []string{"gdrive:sales@acme.com"}},
	}

	mine := ids(search(t, s, engineer, index.Query{Text: "payments"}))
	theirs := ids(search(t, s, seller, index.Query{Text: "payments"}))

	if len(mine) == 0 {
		t.Fatal("the engineer found nothing, so the test proves nothing")
	}
	if slices.Contains(theirs, "d1") || slices.Contains(theirs, "d2") {
		t.Fatalf("the seller was handed the engineer's documents: %v", theirs)
	}
	if ranks, _, _ := st.counts(); ranks != 2 {
		t.Errorf("the driver ranked %d times for two different askers, want 2", ranks)
	}

	// And going back to the first asker still gets the first answer, rather than
	// the entry the second asker left behind.
	if again := ids(search(t, s, engineer, index.Query{Text: "payments"})); !slices.Equal(again, mine) {
		t.Errorf("the engineer got %v on the second ask, want %v", again, mine)
	}
}

// TestTenantsNeverShareAnEntry is the same assertion one level up. Two
// principals with the same subject and the same groups in different tenants are
// two different views of two different corpora.
func TestTenantsNeverShareAnEntry(t *testing.T) {
	st := newCounting(t, corpus)
	s, _ := cachedSearcher(t, st)

	here := principal("gdrive:eng@acme.com")
	elsewhere := &acl.Principal{
		Tenant:  "other",
		Subject: "u_mei",
		Groups:  acl.GroupSet{Version: 1, Members: []string{"gdrive:eng@acme.com"}},
	}

	if got := ids(search(t, s, here, index.Query{Text: "payments"})); len(got) == 0 {
		t.Fatal("the acme principal found nothing, so the test proves nothing")
	}
	if got := ids(search(t, s, elsewhere, index.Query{Text: "payments"})); len(got) != 0 {
		t.Fatalf("a principal of another tenant was handed acme's documents: %v", got)
	}
}

// TestIdenticalViewsShareAnEntry is the other half. A fingerprint that named
// something other than the visibility would be safe and useless, so the cache
// has to hit for two principals the permission rule cannot tell apart.
func TestIdenticalViewsShareAnEntry(t *testing.T) {
	st := newCounting(t, corpus)
	s, _ := cachedSearcher(t, st)

	first := principal("gdrive:eng@acme.com")
	second := principal("gdrive:eng@acme.com")
	// The same view described in a different order is still the same view.
	third := &acl.Principal{
		Tenant:  "acme",
		Subject: "u_mei",
		Groups:  acl.GroupSet{Version: 1, Members: []string{"gdrive:eng@acme.com", "gdrive:eng@acme.com"}},
	}

	for _, p := range []*acl.Principal{first, second, third} {
		search(t, s, p, index.Query{Text: "payments"})
	}
	if ranks, _, _ := st.counts(); ranks != 1 {
		t.Errorf("the driver ranked %d times for three descriptions of one view, want 1", ranks)
	}
}

// TestGroupChangeMakesPreviousEntriesUnreachable is the revocation path. Nobody
// sweeps anything: the entries are still in the cache and the new fingerprint
// cannot name them.
func TestGroupChangeMakesPreviousEntriesUnreachable(t *testing.T) {
	st := newCounting(t, corpus)
	s, _ := cachedSearcher(t, st)

	before := principal("gdrive:eng@acme.com")
	if got := ids(search(t, s, before, index.Query{Text: "payments"})); len(got) == 0 {
		t.Fatal("the engineer found nothing before the change, so the test proves nothing")
	}

	// The same person, expanded again after being removed from the group.
	after := &acl.Principal{
		Tenant:  "acme",
		Subject: "u_mei",
		Groups:  acl.GroupSet{Version: 2},
	}
	if got := ids(search(t, s, after, index.Query{Text: "payments"})); len(got) != 0 {
		t.Fatalf("a principal whose group was taken away still saw %v", got)
	}
	if ranks, _, _ := st.counts(); ranks != 2 {
		t.Errorf("the driver ranked %d times either side of a group change, want 2", ranks)
	}
}

// TestWriteInvalidatesCorpusStatistics covers the layer whose staleness is
// bounded by the write rather than by the clock.
func TestWriteInvalidatesCorpusStatistics(t *testing.T) {
	st := newCounting(t, corpus)
	s, _ := cachedSearcher(t, st)
	p := principal("gdrive:eng@acme.com")

	search(t, s, p, index.Query{Text: "payments"})
	search(t, s, p, index.Query{Text: "payments"})
	if _, stats, _ := st.counts(); stats != 1 {
		t.Fatalf("the driver computed statistics %d times before any write, want 1", stats)
	}

	if err := st.Put(t.Context(), documents([]fixture{
		{id: "d9", title: "Payments postmortem", body: "The payments queue backed up.", perm: openTo("eng@acme.com")},
	})...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	search(t, s, p, index.Query{Text: "payments"})
	if _, stats, _ := st.counts(); stats != 2 {
		t.Errorf("the driver computed statistics %d times after a write, want 2", stats)
	}
}

// TestAnyChangeSweepsTheTenantsOrderings is the decision the cache exists
// around, stated as a test so that anybody tempted by the obvious optimisation
// has to argue with it first.
//
// A write could be allowed to keep the cached orderings, since working out
// which of them a new document would have joined costs more than the cache
// saves. It is the wrong trade for a search box: somebody saves a document,
// searches for it, and is told it does not exist for as long as the expiry
// lasts.
func TestAnyChangeSweepsTheTenantsOrderings(t *testing.T) {
	st := newCounting(t, corpus)
	s, _ := cachedSearcher(t, st)
	p := principal("gdrive:eng@acme.com")

	search(t, s, p, index.Query{Text: "payments"})
	if err := st.Put(t.Context(), documents([]fixture{
		{id: "d9", title: "Payments postmortem", body: "The payments queue backed up.", perm: openTo("eng@acme.com")},
	})...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got := ids(search(t, s, p, index.Query{Text: "payments"}))
	if ranks, _, _ := st.counts(); ranks != 2 {
		t.Errorf("the driver ranked %d times across a write, want 2", ranks)
	}
	if !slices.Contains(got, "d9") {
		t.Errorf("a document written a moment ago is missing from the results: %v", got)
	}

	if err := st.Delete(t.Context(), "d9"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got = ids(search(t, s, p, index.Query{Text: "payments"}))
	if ranks, _, _ := st.counts(); ranks != 3 {
		t.Errorf("the driver ranked %d times across a delete, want 3", ranks)
	}
	if slices.Contains(got, "d9") {
		t.Errorf("a deleted document is still in the results: %v", got)
	}
}

// TestDocumentCacheIsBehindThePermissionCheck is the test the document layer
// owes, because that layer is keyed by id alone and its safety comes from the
// order of two operations rather than from the shape of its key.
//
// The order is: retrieve under the permission predicate, then fetch the ids the
// retrieval produced. So the assertion is that no id ever reaches the fetch
// except one this principal's own retrieval named, and that a document cached
// for one person is not served to another who may not read it.
func TestDocumentCacheIsBehindThePermissionCheck(t *testing.T) {
	st := newCounting(t, corpus)
	s, _ := cachedSearcher(t, st)

	engineer := principal("gdrive:eng@acme.com")
	seller := &acl.Principal{
		Tenant:  "acme",
		Subject: "u_sam",
		Groups:  acl.GroupSet{Version: 1, Members: []string{"gdrive:sales@acme.com"}},
	}

	mine := ids(search(t, s, engineer, index.Query{Text: "payments"}))
	if !slices.Contains(mine, "d1") {
		t.Fatalf("the engineer did not get d1, so it was never cached: %v", mine)
	}

	// The seller asks for the same thing. The engineer's documents are sitting in
	// the document cache under their ids, and the seller must not reach them.
	theirs := ids(search(t, s, seller, index.Query{Text: "payments"}))
	if slices.Contains(theirs, "d1") {
		t.Fatalf("a cached document was served to a principal who may not read it: %v", theirs)
	}

	// Revocation. The document is rewritten so that only sales may read it, which
	// is a write, so the cached copy is dropped and the next retrieval applies the
	// new rule.
	revoked := documents([]fixture{{id: "d1", title: "Payments failover runbook", body: "Failover the payments queue when the primary region is unhealthy.", perm: openTo("sales@acme.com")}})
	if err := st.Put(t.Context(), revoked...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	after := ids(search(t, s, engineer, index.Query{Text: "payments"}))
	if slices.Contains(after, "d1") {
		t.Fatalf("a revoked document was served from the cache: %v", after)
	}
}

func TestResultExpiryZeroTurnsResultCachingOff(t *testing.T) {
	st := newCounting(t, corpus)
	s, _ := cachedSearcher(t, st, index.WithResultExpiry(0))
	p := principal("gdrive:eng@acme.com")

	first := ids(search(t, s, p, index.Query{Text: "payments"}))
	second := ids(search(t, s, p, index.Query{Text: "payments"}))
	if !slices.Equal(first, second) {
		t.Fatalf("turning the cache off changed the answer: %v then %v", first, second)
	}
	ranks, stats, _ := st.counts()
	if ranks != 2 {
		t.Errorf("the driver ranked %d times with result caching off, want 2", ranks)
	}
	// The other two layers are bounded by the write rather than by the clock, so
	// turning the result expiry off does not turn them off with it.
	if stats != 1 {
		t.Errorf("the driver computed statistics %d times with result caching off, want 1", stats)
	}
}

func TestResultCacheExpires(t *testing.T) {
	st := newCounting(t, corpus)
	var now atomic.Int64
	now.Store(epoch.UnixNano())
	c := index.NewCache(index.WithCacheClock(func() time.Time { return time.Unix(0, now.Load()) }))
	s := index.New(st, index.WithClock(clock), index.WithCache(c))
	t.Cleanup(func() { _ = s.Close() })
	p := principal("gdrive:eng@acme.com")

	search(t, s, p, index.Query{Text: "payments"})
	now.Add(int64(index.DefaultResultExpiry) + 1)
	search(t, s, p, index.Query{Text: "payments"})

	if ranks, _, _ := st.counts(); ranks != 2 {
		t.Errorf("the driver ranked %d times either side of the expiry, want 2", ranks)
	}
}

// TestSixteenReadersProduceOneQuery is the case a cache exists for: a popular
// key goes cold and everybody misses it at once.
func TestSixteenReadersProduceOneQuery(t *testing.T) {
	st := newCounting(t, corpus)
	st.hold = make(chan struct{})
	s, _ := cachedSearcher(t, st)
	p := principal("gdrive:eng@acme.com")

	const readers = 16
	var (
		arrived atomic.Int64
		wg      sync.WaitGroup
	)
	results := make([][]string, readers)
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			arrived.Add(1)
			res, err := s.Search(t.Context(), p, index.Query{Text: "payments"})
			if err != nil {
				t.Errorf("Search: %v", err)
				return
			}
			results[i] = ids(res)
		}()
	}
	for arrived.Load() < readers {
		time.Sleep(time.Millisecond)
	}
	// Every reader is inside Search and the one that got there first is blocked
	// in the driver, so the other fifteen are waiting on its call rather than
	// making their own.
	time.Sleep(20 * time.Millisecond)
	close(st.hold)
	wg.Wait()

	if ranks, _, _ := st.counts(); ranks != 1 {
		t.Errorf("sixteen concurrent readers made %d queries, want 1", ranks)
	}
	for i, got := range results {
		if !slices.Equal(got, results[0]) {
			t.Fatalf("reader %d got %v, reader 0 got %v", i, got, results[0])
		}
	}
}

// TestCacheStatsAreReportedPerLayer keeps the numbers an operator needs
// separate. A hit rate averaged over three layers with three different jobs
// says nothing about any of them.
func TestCacheStatsAreReportedPerLayer(t *testing.T) {
	st := newCounting(t, corpus)
	s, _ := cachedSearcher(t, st)
	p := principal("gdrive:eng@acme.com")

	search(t, s, p, index.Query{Text: "payments"})
	search(t, s, p, index.Query{Text: "payments"})

	stats := s.CacheStats()
	for _, layer := range []string{"results", "corpus", "documents"} {
		got, ok := stats[layer]
		if !ok {
			t.Fatalf("the stats do not report the %s layer", layer)
		}
		if got.Hits == 0 {
			t.Errorf("the %s layer reports no hits after a repeated search", layer)
		}
		if got.Entries == 0 {
			t.Errorf("the %s layer reports no entries after a search", layer)
		}
		if got.Ratio() <= 0 {
			t.Errorf("the %s layer reports a hit ratio of %v", layer, got.Ratio())
		}
	}
}

func TestSearcherWithoutACacheStillAnswers(t *testing.T) {
	st := newCounting(t, corpus)
	s := index.New(st, index.WithClock(clock))
	p := principal("gdrive:eng@acme.com")

	first := ids(search(t, s, p, index.Query{Text: "payments"}))
	second := ids(search(t, s, p, index.Query{Text: "payments"}))
	if !slices.Equal(first, second) {
		t.Fatalf("two searches without a cache disagreed: %v then %v", first, second)
	}
	if ranks, _, _ := st.counts(); ranks != 2 {
		t.Errorf("a searcher with no cache ranked %d times, want 2", ranks)
	}
}

// deaf is a driver that ranks but does not report its writes, which is what a
// driver over somebody else's search service looks like.
type deaf struct{ inner *counting }

func (d deaf) Put(ctx context.Context, docs ...doc.Document) error { return d.inner.Put(ctx, docs...) }
func (d deaf) Delete(ctx context.Context, ids ...string) error     { return d.inner.Delete(ctx, ids...) }
func (d deaf) Get(ctx context.Context, p *acl.Principal, id string) (doc.Document, error) {
	return d.inner.Get(ctx, p, id)
}
func (d deaf) Scan(ctx context.Context, p *acl.Principal, fn func(doc.Document) bool) error {
	return d.inner.Scan(ctx, p, fn)
}
func (d deaf) Stats(ctx context.Context) (store.Stats, error) { return d.inner.Stats(ctx) }
func (d deaf) Close() error                                   { return nil }
func (d deaf) Rank(ctx context.Context, p *acl.Principal, r store.Request, sel store.Selection) (store.Ranked, error) {
	return d.inner.Rank(ctx, p, r, sel)
}
func (d deaf) Statistics(ctx context.Context, p *acl.Principal, terms []string) (store.Corpus, error) {
	return d.inner.Statistics(ctx, p, terms)
}

// TestADriverThatDoesNotReportWritesKeepsOnlyTheBoundedLayer covers the
// deployment where the cache cannot be told that it is wrong. The layers whose
// staleness is bounded by a write turn themselves off, and the one bounded by
// the clock stays on.
func TestADriverThatDoesNotReportWritesKeepsOnlyTheBoundedLayer(t *testing.T) {
	inner := newCounting(t, corpus)
	if _, ok := any(deaf{inner}).(store.Notifier); ok {
		t.Fatal("deaf implements store.Notifier, so this test no longer covers the blind path")
	}
	s, _ := cachedSearcher(t, deaf{inner})
	p := principal("gdrive:eng@acme.com")

	search(t, s, p, index.Query{Text: "payments"})
	search(t, s, p, index.Query{Text: "payments"})

	ranks, stats, _ := inner.counts()
	if ranks != 1 {
		t.Errorf("the driver ranked %d times, want 1: the result layer is bounded by the expiry and stays on", ranks)
	}
	if stats != 2 {
		t.Errorf("the driver computed statistics %d times, want 2: a layer that can never be told it is wrong is turned off", stats)
	}
}
