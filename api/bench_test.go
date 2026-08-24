package api_test

import (
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/benchcorpus"
	"github.com/tamnd/genba/doc"
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

// BenchmarkAPISearchCurated is a search that somebody has written the answer to.
//
// The miss is already measured, because every search asks whether this question
// has been answered and almost none of them have, so the cost of asking is in
// BenchmarkAPISearch above. This is the other half: the probe finds something,
// and the answer's sources are then read through the reader asking, which is one
// more query and a permission check per citation.
//
// The question is deliberately not one of the checked in queries. Answering one
// of those would put a card on a page the benchmark beside this one measures,
// and the two numbers would stop being comparable.
func BenchmarkAPISearchCurated(b *testing.B) {
	const question = "how does the benchmark corpus answer this particular question"
	h, hdr := handler(b, api.WithLogger(slog.New(slog.DiscardHandler)))

	var page struct {
		Hits []struct {
			ID string `json:"id"`
		} `json:"hits"`
	}
	queries := benchcorpus.ByClass(benchcorpus.Queries())[benchcorpus.ClassCommon]
	w := get(b, h, "/api/v1/search?q="+urlQuery(queries[0].Text), hdr)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		b.Fatalf("decoding the search page: %v", err)
	}
	if len(page.Hits) < api.AnswerSources {
		b.Fatalf("the corpus returned %d documents, and an answer is measured with %d sources under it",
			len(page.Hits), api.AnswerSources)
	}

	sources := make([]string, 0, api.AnswerSources)
	for _, hit := range page.Hits[:api.AnswerSources] {
		sources = append(sources, hit.ID)
	}
	body, err := json.Marshal(map[string]any{
		"question": question,
		"body":     "The corpus is generated from a seed, so every run reads the same documents in the same order.",
		"sources":  sources,
	})
	if err != nil {
		b.Fatalf("encoding the answer: %v", err)
	}

	admin := maps.Clone(hdr)
	admin[api.HeaderRoles] = acl.RoleAdmin
	r := httptest.NewRequestWithContext(b.Context(), http.MethodPut, "/api/v1/admin/answers/bench", strings.NewReader(string(body)))
	for k, v := range admin {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		b.Fatalf("writing the answer: %d %s", rec.Code, rec.Body)
	}

	target := "/api/v1/search?q=" + urlQuery(question)
	got := get(b, h, target, hdr)
	if !strings.Contains(got.Body.String(), `"curated"`) {
		b.Fatal("the question that was just answered came back with no answer on it")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		get(b, h, target, hdr)
	}
}

// BenchmarkAPIMe and BenchmarkAPIStats are the two calls the interface makes
// before it can draw anything, so they are on the path to first paint even
// though neither of them searches for anything.
//
// This measures the second session of the morning rather than the first. The
// filter rail is counted over every document the reader may open and held for a
// minute, so what is on this path almost always is the cache lookup, and the
// count behind it is measured where it happens, in BenchmarkReachable.
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

// BenchmarkAPIReported is the other panel on the home screen: the documents
// this person owns or wrote that somebody has said are out of date. It is
// measured for the same reason the recent lists are, which is that the front
// page reads it on every visit, and a panel that is usually empty still costs a
// query every time.
//
// The corpus draws its owners from its own generated people and the reader
// every other benchmark runs as owns none of it, so the reports are made by
// that reader and read back by one carrying the owners' addresses. That is also
// the shape the endpoint answers in a deployment: the person who complained and
// the person who has to fix it are not the same person.
func BenchmarkAPIReported(b *testing.B) {
	// A screenful, which is what the panel draws and so what it asks for.
	const rows = 6

	st, spec := benchcorpus.Fixture(b)
	s := api.New(st, index.New(st, index.WithClock(func() time.Time { return benchcorpus.Epoch })),
		api.HeaderAuth{Tenant: benchcorpus.Tenant}, api.WithLogger(slog.New(slog.DiscardHandler)))
	h := s.Handler()
	reader := headers(spec.Principal())

	owners := make([]string, 0, rows)
	if err := st.Scan(b.Context(), spec.Principal(), func(d doc.Document) bool {
		if d.Owner.Email == "" {
			return true
		}
		r := httptest.NewRequestWithContext(b.Context(), http.MethodPost,
			"/api/v1/documents/"+url.PathEscape(d.ID)+"/stale",
			strings.NewReader(`{"note":"this names a cluster that was turned off"}`))
		for k, v := range reader {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			b.Fatalf("POST /api/v1/documents/%s/stale = %d, %s", d.ID, rec.Code, rec.Body)
		}
		owners = append(owners, "gdrive:"+d.Owner.Email)
		return len(owners) < rows
	}); err != nil {
		b.Fatalf("reading the corpus: %v", err)
	}

	// The same reader, carrying the addresses those six documents are owned at.
	// The groups do not move, so what is being measured is the ownership match
	// on top of the permission predicate rather than a different corpus.
	owner := headers(spec.Principal())
	owner[api.HeaderIdentities] = strings.Join(owners, ",")

	target := "/api/v1/reported?limit=" + strconv.Itoa(rows)
	var inbox struct {
		Documents []struct {
			ID string `json:"id"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(get(b, h, target, owner).Body.Bytes(), &inbox); err != nil {
		b.Fatalf("decoding the inbox: %v", err)
	}
	// An empty list is a read of nothing, and a benchmark of it would report a
	// number that says the panel is free right up until somebody's readers use
	// the feature.
	if len(inbox.Documents) != rows {
		b.Fatalf("the inbox holds %d documents and %d were reported", len(inbox.Documents), rows)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		get(b, h, target, owner)
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

// BenchmarkAPIAccess is the question an operator asks over and over: can this
// person read this document. It is measured with the full group expansion the
// corpus generates rather than with one group, because the permission predicate
// is built from the membership and a question about somebody in two groups is
// not the question anybody asks about a long serving employee.
//
// This is the one that has to be quick, and it is the reason the counts beside
// it are asked for rather than always computed.
func BenchmarkAPIAccess(b *testing.B) {
	h, hdr := handler(b, api.WithLogger(slog.New(slog.DiscardHandler)))
	hdr[api.HeaderRoles] = acl.RoleAdmin
	target := accessTarget(hdr) + "&id=b0"
	get(b, h, target, hdr)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		get(b, h, target, hdr)
	}
}

// BenchmarkAPIAccessCounts is the aggregate: how much of the corpus that person
// can reach, by connector. It is a pass over every document in the tenant and
// it is measured here because the number is the argument for keeping it behind
// counts=1 rather than on the first paint. See #149.
func BenchmarkAPIAccessCounts(b *testing.B) {
	h, hdr := handler(b, api.WithLogger(slog.New(slog.DiscardHandler)))
	hdr[api.HeaderRoles] = acl.RoleAdmin
	target := accessTarget(hdr) + "&counts=1"
	get(b, h, target, hdr)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		get(b, h, target, hdr)
	}
}

// accessTarget is the access question with the principal from the headers put
// into the query string, which is how an operator asks about somebody else.
func accessTarget(hdr map[string]string) string {
	return "/api/v1/admin/access?subject=u_bench" +
		"&groups=" + url.QueryEscape(hdr[api.HeaderGroups]) +
		"&identities=" + url.QueryEscape(hdr[api.HeaderIdentities])
}

// urlQuery escapes a query for the q parameter. The benchmark queries are
// generated words, spaces and colons, so this is all the escaping they need and
// url.QueryEscape would turn the spaces into plus signs for no reason.
func urlQuery(s string) string {
	return strings.ReplaceAll(s, " ", "%20")
}
