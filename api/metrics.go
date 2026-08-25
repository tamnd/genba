package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/tamnd/genba/audit"
	"github.com/tamnd/genba/metric"
	"github.com/tamnd/genba/store"
)

// statsBudget is how long a scrape may spend asking the store what it holds.
//
// It is a second because that is long enough for the count on any store that is
// working and short enough that a store that is not does not hold the scrape
// open. The endpoint that reports the same number to a person is on the request
// path and has a far tighter budget than this, so a store slow enough to reach
// this is one somebody already knows about.
const statsBudget = time.Second

// Metric names. They are constants because the alert in docs/alerts.yml names
// them, and a rename that only edits the code leaves an alert that will never
// fire again and will never say so.
const (
	MetricRequestDuration = "genba_request_duration_milliseconds"
	MetricSearchDuration  = "genba_search_duration_milliseconds"
	MetricCandidates      = "genba_search_candidates"
	MetricMatches         = "genba_search_matches"
	MetricCacheHits       = "genba_cache_hits_total"
	MetricCacheMisses     = "genba_cache_misses_total"
	MetricCacheEvictions  = "genba_cache_evictions_total"
	MetricCacheEntries    = "genba_cache_entries"
	MetricGroupStaleness  = "genba_directory_staleness_seconds"
	MetricQuarantined     = "genba_quarantined_documents"
	MetricAuditRecords    = "genba_audit_records_total"
	MetricAuditFailed     = "genba_audit_failed_total"
	MetricAuditQueued     = "genba_audit_queued_records"
	MetricStoreRows       = "genba_store_rows_total"
	MetricStoreStatements = "genba_store_statements_total"
	MetricStoreDecodes    = "genba_store_decodes_total"
)

// metrics is everything the server publishes.
//
// The histograms are recorded into on the request path. The counters are read
// at scrape time out of the structures that were already keeping them, which is
// why there is no counting done twice anywhere in here: the cache layers and
// the storage driver have their own counters because their own tests assert on
// them, and this publishes those rather than shadowing them.
type metrics struct {
	registry *metric.Registry

	requests   *metric.Histogram
	searches   *metric.Histogram
	candidates *metric.Histogram
	matches    *metric.Histogram
}

// newMetrics registers the families of one server.
func newMetrics(s *Server) *metrics {
	r := metric.New()
	m := &metrics{
		registry: r,
		requests: r.NewHistogram(MetricRequestDuration,
			"How long a request took, from the first line read to the last byte written.",
			"endpoint", metric.Duration),
		searches: r.NewHistogram(MetricSearchDuration,
			"How long the search itself took, excluding request parsing and encoding.",
			"", metric.Duration),
		candidates: r.NewHistogram(MetricCandidates,
			"How many documents were ranked to produce one page of results.",
			"", metric.Size),
		matches: r.NewHistogram(MetricMatches,
			"How many documents matched, before paging.",
			"", metric.Size),
	}

	// The cache layers appear and disappear with the configuration, so the
	// label values come from the scrape rather than from registration. A
	// deployment with result caching turned off publishes two layers instead of
	// three, which is the truth and is more useful than a zero.
	cacheStat := func(pick func(hits, misses, evictions, entries int64) int64) func(context.Context) map[string]float64 {
		return func(context.Context) map[string]float64 {
			out := map[string]float64{}
			for layer, st := range s.cacheStats() {
				out[layer] = float64(pick(st.Hits, st.Misses, st.Evictions, int64(st.Entries)))
			}
			return out
		}
	}
	r.Counters(MetricCacheHits, "Cache reads that found an entry, per layer.", "layer", "counter",
		cacheStat(func(hits, _, _, _ int64) int64 { return hits }))
	r.Counters(MetricCacheMisses, "Cache reads that had to do the work, per layer.", "layer", "counter",
		cacheStat(func(_, misses, _, _ int64) int64 { return misses }))
	r.Counters(MetricCacheEvictions, "Entries dropped to stay within capacity, per layer.", "layer", "counter",
		cacheStat(func(_, _, evictions, _ int64) int64 { return evictions }))
	r.Counters(MetricCacheEntries, "Entries currently held, per layer.", "layer", "gauge",
		cacheStat(func(_, _, _, entries int64) int64 { return entries }))

	// How long a membership change can take to be noticed. It is published
	// because it is a promise the deployment is making about its permissions,
	// and a promise that only exists in a configuration file is one nobody is
	// checking. A deployment that resolves without a cache publishes nothing
	// here, which is the truth: there is no staleness to bound.
	r.Counters(MetricGroupStaleness,
		"The most out of date a resolved group set can be, which is the directory cache lifetime.",
		"", "gauge", func(context.Context) map[string]float64 {
			bounded, ok := s.auth.(interface{ staleness() (float64, bool) })
			if !ok {
				return nil
			}
			seconds, ok := bounded.staleness()
			if !ok {
				return nil
			}
			return map[string]float64{"": seconds}
		})

	// How many documents are being held back because their permissions did not
	// resolve. It is the one number a held document produces anywhere, by
	// design, and until now it was only on an endpoint somebody has to go and
	// look at. A quarantine is normally empty and briefly non zero during a
	// crawl, so what an operator wants is to be told when that stops being
	// true, and being told requires a metric.
	//
	// A store that cannot answer publishes nothing rather than a zero. A zero
	// here is the same shape as a healthy index and would silence the alert at
	// exactly the moment the index stopped being able to say anything about
	// itself, so the series goes missing instead, which is a thing an alert can
	// be written against.
	r.Counters(MetricQuarantined,
		"Documents held back because their permissions did not resolve, which are invisible to every principal.",
		"", "gauge", func(ctx context.Context) map[string]float64 {
			// The scrape's context, bounded again, because a database that has
			// stopped answering should cost the scrape a second rather than
			// whatever the scrape timeout happens to be.
			ctx, cancel := context.WithTimeout(ctx, statsBudget)
			defer cancel()

			st, err := s.store.Stats(ctx)
			if err != nil {
				return nil
			}
			return map[string]float64{"": float64(st.Quarantined)}
		})

	// What the audit trail has done with itself.
	//
	// Failed is the one to alert on and it should be flat at zero: a record that
	// could not be written is an access that happened and cannot be proved, and
	// the deployment finding that out at the end of a quarter is the failure
	// this metric exists to prevent. Queued is the other half of the story,
	// because a queue that is not draining is a sink that is about to start
	// making a request wait.
	audited := func(pick func(audit.Stats) float64) func(context.Context) map[string]float64 {
		return func(context.Context) map[string]float64 {
			return map[string]float64{"": pick(s.audit.Stats())}
		}
	}
	r.Counters(MetricAuditRecords, "Content access records written.", "", "counter",
		audited(func(st audit.Stats) float64 { return float64(st.Written) }))
	r.Counters(MetricAuditFailed, "Content access records that could not be written, which should be zero.", "", "counter",
		audited(func(st audit.Stats) float64 { return float64(st.Failed) }))
	r.Counters(MetricAuditQueued, "Content access records waiting to be written.", "", "gauge",
		audited(func(st audit.Stats) float64 { return float64(st.Queued) }))

	// A driver is not obliged to be measurable. One that is publishes the same
	// numbers the CI gate asserts on, so a slow deployment can be compared with
	// a green pull request instead of argued about.
	if counted, ok := s.store.(store.Counted); ok {
		single := func(pick func(store.Counters) int64) func(context.Context) map[string]float64 {
			return func(context.Context) map[string]float64 {
				return map[string]float64{"": float64(pick(counted.Counters()))}
			}
		}
		r.Counters(MetricStoreRows, "Rows the storage driver has returned on read paths.", "", "counter",
			single(func(c store.Counters) int64 { return c.Rows }))
		r.Counters(MetricStoreStatements, "Statements the storage driver has run on read paths.", "", "counter",
			single(func(c store.Counters) int64 { return c.Statements }))
		r.Counters(MetricStoreDecodes, "Stored documents the driver has decoded.", "", "counter",
			single(func(c store.Counters) int64 { return c.Decodes }))
	}
	return m
}

// Metrics returns a handler serving the Prometheus text format.
//
// It is not mounted on the API router. Metrics say how much traffic there is,
// how large the match sets are and how hard the caches are working, which is
// not secret and is not public either, and the deployment shape that gets this
// right is a second listener on an address the outside cannot reach.
// [Server.Handler] therefore never serves it, and a caller that wants it
// mounted somewhere has to say so.
func (s *Server) Metrics() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = s.metrics.registry.Collect(r.Context(), w)
	})
}

// measure times every request and files it under the route that served it.
//
// The label is the registered pattern rather than the path, because the path
// carries document ids and a metric with one series per document is not a
// metric. Routing runs twice per request to get it, which is a map lookup
// against a fixed set of patterns and is not measurable next to the handler.
func (s *Server) measure(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		mux.ServeHTTP(w, r)

		// An event stream is open for as long as somebody is looking at the
		// page, so timing it would put a number in the tail that describes a
		// person's attention span rather than the server.
		_, pattern := mux.Handler(r)
		if pattern == "" || pattern == "GET /api/v1/events" {
			return
		}
		s.metrics.requests.Observe(float64(s.now().Sub(start).Microseconds())/1000, endpoint(pattern))
	})
}

// endpoint strips the method from a route pattern, since the label is a place
// and the method is already the reason there are separate patterns.
func endpoint(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}

// observeSearch records what one search cost, whatever the response looked
// like.
func (m *metrics) observeSearch(took time.Duration, candidates, total int) {
	m.searches.Observe(float64(took.Microseconds())/1000, "")
	m.candidates.Observe(float64(candidates), "")
	m.matches.Observe(float64(total), "")
}
