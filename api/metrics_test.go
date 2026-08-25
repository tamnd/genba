package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/sqlitestore"
)

// measuredServer is cachingServer with the server itself handed back, because
// the metrics handler is deliberately not on the router.
func measuredServer(t *testing.T) (*api.Server, http.Handler) {
	t.Helper()
	st, err := sqlitestore.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Put(t.Context(), cacheCorpus()...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	searcher := index.New(st, index.WithCache(index.NewCache()))
	t.Cleanup(func() { _ = searcher.Close() })

	s := api.New(st, searcher, api.HeaderAuth{Tenant: "acme"})
	h := s.Handler()
	return s, h
}

// scrape reads the metrics handler.
func scrape(t *testing.T, s *api.Server) string {
	t.Helper()
	w := httptest.NewRecorder()
	s.Metrics().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("the metrics handler returned %d", w.Code)
	}
	return w.Body.String()
}

// value reads one series out of an exposition.
func value(t *testing.T, body, series string) float64 {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no series %q in:\n%s", series, body)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("series %q has value %q: %v", series, m[1], err)
	}
	return v
}

// TestTheMetricsAreNotOnTheApiRouter is the deployment shape, asserted rather
// than described in a comment. Metrics say how many documents a tenant holds
// and how hard the caches are working, and the address that serves them is not
// the address the world can reach.
func TestTheMetricsAreNotOnTheApiRouter(t *testing.T) {
	_, h := measuredServer(t)
	for _, path := range []string{"/metrics", "/api/v1/metrics"} {
		w := request(t, h, http.MethodGet, path, engineer())
		if w.Code == http.StatusOK {
			t.Errorf("%s is served by the API router", path)
		}
	}
}

// TestARequestIsFiledUnderItsRoute covers the label, which is the part that
// turns a metric into a bill. A path carries document ids, and one series per
// document is not a metric, it is an outage in the monitoring system.
func TestARequestIsFiledUnderItsRoute(t *testing.T) {
	s, h := measuredServer(t)

	request(t, h, http.MethodGet, "/api/v1/documents/d1", engineer())
	request(t, h, http.MethodGet, "/api/v1/documents/d2", engineer())
	request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())

	body := scrape(t, s)
	if got := value(t, body, `genba_request_duration_milliseconds_count{endpoint="/api/v1/documents/{id}"}`); got != 2 {
		t.Errorf("the document route recorded %v requests, want 2", got)
	}
	if got := value(t, body, `genba_request_duration_milliseconds_count{endpoint="/api/v1/search"}`); got != 1 {
		t.Errorf("the search route recorded %v requests, want 1", got)
	}
	if strings.Contains(body, `endpoint="/api/v1/documents/d1"`) {
		t.Error("a document id reached a label, which is one series per document")
	}
}

// TestASearchReportsTheWorkItDid is the pair of numbers that says whether the
// two phase retrieval is still cutting. Candidates is what was ranked, matches
// is what was found, and the day they start moving together the first phase has
// stopped doing its job.
func TestASearchReportsTheWorkItDid(t *testing.T) {
	s, h := measuredServer(t)
	request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())

	body := scrape(t, s)
	if got := value(t, body, "genba_search_candidates_count"); got != 1 {
		t.Errorf("candidates recorded %v searches, want 1", got)
	}
	if got := value(t, body, "genba_search_matches_count"); got != 1 {
		t.Errorf("matches recorded %v searches, want 1", got)
	}
	if got := value(t, body, "genba_search_matches_sum"); got == 0 {
		t.Error("the search matched nothing, so this test is measuring an empty query")
	}
	if got := value(t, body, "genba_search_candidates_sum"); got == 0 {
		t.Error("the search ranked nothing, so the candidate count is not being reported")
	}
}

// TestTheCacheCountersComeFromTheCache, rather than from a second set of
// counters kept alongside them that can disagree.
func TestTheCacheCountersComeFromTheCache(t *testing.T) {
	s, h := measuredServer(t)

	request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	first := value(t, scrape(t, s), `genba_cache_misses_total{layer="results"}`)
	if first == 0 {
		t.Fatal("the first search was not a miss, so the results layer is not being read")
	}

	request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	body := scrape(t, s)
	if got := value(t, body, `genba_cache_hits_total{layer="results"}`); got == 0 {
		t.Error("the repeated search was not a hit")
	}
	if got := value(t, body, `genba_cache_misses_total{layer="results"}`); got != first {
		t.Errorf("the miss count moved to %v on a hit, want %v", got, first)
	}
	if !strings.Contains(body, `genba_cache_entries{layer="results"}`) {
		t.Error("the entry gauge is missing, which is how a layer at capacity is spotted")
	}
}

// TestTheStoreCountersAreThere because they are the numbers the CI gate asserts
// on, and a production slowdown that cannot be compared with a green pull
// request is a slowdown that gets argued about instead of found.
func TestTheStoreCountersAreThere(t *testing.T) {
	s, h := measuredServer(t)
	request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())

	body := scrape(t, s)
	for _, series := range []string{"genba_store_rows_total", "genba_store_statements_total", "genba_store_decodes_total"} {
		if got := value(t, body, series); got == 0 {
			t.Errorf("%s is zero after a search that read the store", series)
		}
	}
}

// serving builds a server over a store the caller has filled, which is what the
// quarantine tests need and the shared helper above does not give them.
func serving(t *testing.T, st store.Store) *api.Server {
	t.Helper()
	searcher := index.New(st, index.WithCache(index.NewCache()))
	t.Cleanup(func() { _ = searcher.Close() })
	return api.New(st, searcher, api.HeaderAuth{Tenant: "acme"})
}

// withHeld returns a store holding the ordinary corpus plus n documents whose
// permissions did not resolve.
func withHeld(t *testing.T, n int) *sqlitestore.Store {
	t.Helper()
	st, err := sqlitestore.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	docs := cacheCorpus()
	for i := range n {
		docs = append(docs, doc.Document{
			ID: fmt.Sprintf("held-%d", i), Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Board pack", Body: "The payments line for the quarter.",
			Permissions: acl.Permissions{
				Mode: acl.ModeUnknown, Source: "gdrive",
				Reason: "the directory was unreachable",
			},
		})
	}
	if err := st.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return st
}

// TestTheQuarantineIsAGaugeAnOperatorCanAlertOn. A held document is invisible to
// every principal by design, so this number is the only trace it leaves
// anywhere, and until it was a metric the only way to see it was to go and look
// at an endpoint.
func TestTheQuarantineIsAGaugeAnOperatorCanAlertOn(t *testing.T) {
	s := serving(t, withHeld(t, 2))

	body := scrape(t, s)
	if got := value(t, body, api.MetricQuarantined); got != 2 {
		t.Errorf("the quarantine gauge reads %v, want 2", got)
	}
	if !strings.Contains(body, "# TYPE "+api.MetricQuarantined+" gauge") {
		t.Errorf("the quarantine is not published as a gauge:\n%s", body)
	}
}

// TestAnEmptyQuarantineStillReportsZero, because a series that only appears once
// something has gone wrong is one nobody can write a rule against until the
// night it matters.
func TestAnEmptyQuarantineStillReportsZero(t *testing.T) {
	s := serving(t, withHeld(t, 0))

	if got := value(t, scrape(t, s), api.MetricQuarantined); got != 0 {
		t.Errorf("an index holding nothing back reads %v, want 0", got)
	}
}

// TestAStoreThatCannotSayWhatItHoldsPublishesNothing is the difference between
// no answer and an answer of zero. A zero here is the same shape as a healthy
// index, so publishing one would silence the alert at exactly the moment the
// index stopped being able to describe itself.
func TestAStoreThatCannotSayWhatItHoldsPublishesNothing(t *testing.T) {
	s := serving(t, silentStore{withHeld(t, 2)})

	body := scrape(t, s)
	if strings.Contains(body, "\n"+api.MetricQuarantined+" ") {
		t.Errorf("a store that cannot answer published a count anyway:\n%s", body)
	}
	if got := value(t, body, "genba_store_rows_total"); got < 0 {
		t.Errorf("the rest of the exposition went missing with it: %v", got)
	}
}

// silentStore is a store that has stopped being able to say what it holds,
// which is a database under load rather than an exotic failure.
type silentStore struct{ *sqlitestore.Store }

func (silentStore) Stats(context.Context) (store.Stats, error) {
	return store.Stats{}, errors.New("the database is not answering")
}

// TestAnOpenEventStreamIsNotTimed. It is open for as long as somebody is
// looking at the page, so its duration describes an attention span rather than
// the server, and one of them in the tail moves every percentile above it.
func TestAnOpenEventStreamIsNotTimed(t *testing.T) {
	s, h := measuredServer(t)
	request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())

	if body := scrape(t, s); strings.Contains(body, `endpoint="/api/v1/events"`) {
		t.Errorf("the event stream is being timed:\n%s", body)
	}
}
