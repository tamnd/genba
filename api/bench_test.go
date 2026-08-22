package api_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/benchcorpus"
	"github.com/tamnd/genba/index"
)

// These measure the endpoint rather than the searcher, because the budget is
// stated against what a browser waits for. Between the two there is header
// parsing, query parsing, snippet building and JSON encoding, and every one of
// them has been the surprise in somebody's latency budget at some point.
//
// The corpus is the same fixed one the search benchmarks use, so a number here
// and a number there are comparable and the difference between them is the cost
// of the HTTP layer.

func handler(b *testing.B, opts ...api.Option) (h http.Handler, ids map[string]string) {
	b.Helper()
	st, spec := benchcorpus.Fixture(b)
	s := api.New(st, index.New(st, index.WithClock(func() time.Time { return benchcorpus.Epoch })),
		api.HeaderAuth{Tenant: benchcorpus.Tenant}, opts...)
	return s.Handler(), headers(spec.Principal())
}

// headers is the principal as a trusted proxy would pass it down.
func headers(p *acl.Principal) map[string]string {
	identities := make([]string, 0, len(p.Identities))
	for _, id := range p.Identities {
		identities = append(identities, id.Source+":"+id.Value)
	}
	return map[string]string{
		api.HeaderSubject:      p.Subject,
		api.HeaderTenant:       p.Tenant,
		api.HeaderGroups:       strings.Join(p.Groups.Members, ","),
		api.HeaderIdentities:   strings.Join(identities, ","),
		api.HeaderGroupVersion: "1",
	}
}

// get issues one request and fails the benchmark on anything that is not a
// success, because a benchmark of an error path measures nothing.
func get(b *testing.B, h http.Handler, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(b.Context(), http.MethodGet, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		b.Fatalf("GET %s = %d, %s", target, w.Code, w.Body)
	}
	return w
}

// endpoint measures one path, cycling through the checked in queries of a class
// so the measurement is over a distribution rather than over one lucky term.
func endpoint(b *testing.B, path string, class benchcorpus.Class) {
	h, hdr := handler(b)
	queries := benchcorpus.ByClass(benchcorpus.Queries())[class]
	if len(queries) == 0 {
		b.Fatalf("the query set has no %s queries", class)
	}

	targets := make([]string, len(queries))
	for i, q := range queries {
		targets[i] = path + "?q=" + urlQuery(q.Text)
	}
	for _, t := range targets[:min(len(targets), 20)] {
		get(b, h, t, hdr)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		get(b, h, targets[i%len(targets)], hdr)
	}
}

func BenchmarkAPISearch(b *testing.B) { endpoint(b, "/api/v1/search", benchcorpus.ClassCommon) }

// BenchmarkAPISearchFilter is the endpoint with the facets doing the work and
// no terms at all, which is what the interface asks for when somebody clicks a
// source in the sidebar with an empty box.
func BenchmarkAPISearchFilter(b *testing.B) {
	endpoint(b, "/api/v1/search", benchcorpus.ClassFilter)
}

// BenchmarkAPISuggest is the typeahead, and it has the tightest budget of
// anything here: it runs on a keystroke, so a person will issue several of them
// in the time it takes to read one result page.
func BenchmarkAPISuggest(b *testing.B) { endpoint(b, "/api/v1/suggest", benchcorpus.ClassCommon) }

// BenchmarkAPIDocument is opening a result, over ids taken from a real search so
// that every one of them is a document this reader may actually read.
func BenchmarkAPIDocument(b *testing.B) {
	h, hdr := handler(b)
	queries := benchcorpus.ByClass(benchcorpus.Queries())[benchcorpus.ClassCommon]

	var page struct {
		Hits []struct {
			ID string `json:"id"`
		} `json:"hits"`
	}
	w := get(b, h, "/api/v1/search?q="+urlQuery(queries[0].Text), hdr)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		b.Fatalf("decoding the search page: %v", err)
	}
	if len(page.Hits) == 0 {
		b.Fatal("the first benchmark query returned nothing to open")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		get(b, h, "/api/v1/documents/"+page.Hits[i%len(page.Hits)].ID, hdr)
	}
}

// BenchmarkAPIMe and BenchmarkAPIStats are the two calls the interface makes
// before it can draw anything, so they are on the path to first paint even
// though neither of them searches for anything.
func BenchmarkAPIMe(b *testing.B) {
	h, hdr := handler(b)
	get(b, h, "/api/v1/me", hdr)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		get(b, h, "/api/v1/me", hdr)
	}
}

// BenchmarkAPIRecent is the home screen: what this person opened and what has
// changed in the corpus they can see. The history is filled first, because an
// empty one is a read of nothing and the number that matters is the one with a
// screen of rows in it.
func BenchmarkAPIRecent(b *testing.B) {
	h, hdr := handler(b)

	w := get(b, h, "/api/v1/search?q="+urlQuery(benchcorpus.ByClass(benchcorpus.Queries())[benchcorpus.ClassCommon][0].Text), hdr)
	var page struct {
		Hits []struct {
			ID string `json:"id"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		b.Fatalf("decoding the search page: %v", err)
	}
	for _, hit := range page.Hits {
		r := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "/api/v1/recent",
			strings.NewReader(`{"id":"`+hit.ID+`"}`))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusNoContent {
			b.Fatalf("POST /api/v1/recent = %d, %s", rec.Code, rec.Body)
		}
	}

	get(b, h, "/api/v1/recent", hdr)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		get(b, h, "/api/v1/recent", hdr)
	}
}

func BenchmarkAPIStats(b *testing.B) {
	h, hdr := handler(b)
	get(b, h, "/api/v1/stats", hdr)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		get(b, h, "/api/v1/stats", hdr)
	}
}

// BenchmarkAPIAdmin is the administration screen, which repaints itself every
// five seconds for as long as somebody leaves it open. That is the reason it is
// measured: an endpoint nobody calls twice can afford to be slow and one that
// answers on a timer cannot. It reads memory for the connectors and then asks
// the store for its counts and for a bounded page of what is being held back,
// so the number here is the cost of those two queries over the whole corpus.
// Every read of it is audited, and the audit goes to the process log, so the
// logger is thrown away here. Measuring an endpoint with its log line going to
// a terminal measures the terminal.
func BenchmarkAPIAdmin(b *testing.B) {
	h, hdr := handler(b, api.WithLogger(slog.New(slog.DiscardHandler)))
	hdr[api.HeaderRoles] = acl.RoleAdmin
	get(b, h, "/api/v1/admin/operations", hdr)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		get(b, h, "/api/v1/admin/operations", hdr)
	}
}

// urlQuery escapes a query for the q parameter. The benchmark queries are
// generated words, spaces and colons, so this is all the escaping they need and
// url.QueryEscape would turn the spaces into plus signs for no reason.
func urlQuery(s string) string {
	return strings.ReplaceAll(s, " ", "%20")
}
