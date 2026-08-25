package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

func newServer(t *testing.T) http.Handler {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	perm := func(group string) acl.Permissions {
		return acl.Permissions{
			Mode:        acl.ModeACL,
			Source:      "gdrive",
			AllowGroups: []acl.Ref{{Source: "gdrive", Value: group}},
			Version:     1,
		}
	}
	docs := []doc.Document{
		{
			ID: "d1", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments failover runbook", Body: "Fail the payments queue over to the replica.",
			URL: "https://drive.example.com/d1", Permissions: perm("eng@acme.com"),
		},
		{
			ID: "d2", Tenant: "acme", Source: "salesforce", Kind: doc.KindTicket,
			Title: "Renewal for Globex", Body: "The payments discount expires in March.",
			Permissions: perm("sales@acme.com"),
		},
	}
	if err := st.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"})
	return s.Handler()
}

func request(t *testing.T, h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func engineer() map[string]string {
	return map[string]string{
		api.HeaderSubject: "u_mei",
		api.HeaderGroups:  "gdrive:eng@acme.com",
	}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding the response: %v\nbody: %s", err, w.Body.String())
	}
	return v
}

func TestHealthNeedsNoCredential(t *testing.T) {
	w := request(t, newServer(t), http.MethodGet, "/healthz", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := decode[map[string]string](t, w)
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestReadyChecksTheStore(t *testing.T) {
	w := request(t, newServer(t), http.MethodGet, "/readyz", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestSearchWithoutACredentialIsRejected(t *testing.T) {
	for _, path := range []string{"/api/v1/search?q=payments", "/api/v1/documents/d1", "/api/v1/stats"} {
		w := request(t, newServer(t), http.MethodGet, path, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s returned %d, want 401", path, w.Code)
		}
	}
}

type searchBody struct {
	Query      string `json:"query"`
	Total      int    `json:"total"`
	Correction string `json:"correction"`
	Hits       []struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Snippet string `json:"snippet"`
	} `json:"hits"`
	Facets map[string][]struct {
		Value    string `json:"value"`
		Count    int    `json:"count"`
		Selected bool   `json:"selected"`
	} `json:"facets"`
}

func TestSearchReturnsOnlyWhatTheCallerMayRead(t *testing.T) {
	w := request(t, newServer(t), http.MethodGet, "/api/v1/search?q=payments", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	body := decode[searchBody](t, w)
	if body.Total != 1 || len(body.Hits) != 1 {
		t.Fatalf("got %d hits, want only the document the engineer may read", len(body.Hits))
	}
	if body.Hits[0].ID != "d1" {
		t.Fatalf("hit = %q, want d1", body.Hits[0].ID)
	}
	if strings.Contains(w.Body.String(), "Globex") {
		t.Fatal("the response leaked a document the caller may not read")
	}
}

// A correction rides on the search that found nothing, so the interface has it
// at the moment it has to draw an empty screen rather than a request later.
func TestSearchOffersACorrectionOnTheAnswerThatFoundNothing(t *testing.T) {
	h := newServer(t)

	w := request(t, h, http.MethodGet, "/api/v1/search?q=paymnets", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	body := decode[searchBody](t, w)
	if body.Total != 0 {
		t.Fatalf("paymnets matched %d documents, so there is nothing to correct", body.Total)
	}
	if body.Correction != "payments" {
		t.Fatalf("correction = %q, want payments", body.Correction)
	}

	// And a query that worked carries no correction at all, rather than an
	// empty one, because a client that reads a field that is always there ends
	// up drawing an empty offer.
	w = request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	if strings.Contains(w.Body.String(), "correction") {
		t.Fatalf("a search that found results carried a correction: %s", w.Body)
	}
}

func TestSearchRejectsBadPaging(t *testing.T) {
	w := request(t, newServer(t), http.MethodGet, "/api/v1/search?q=payments&limit=lots", engineer())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestDocumentHidesWhatTheCallerMayNotRead(t *testing.T) {
	h := newServer(t)

	w := request(t, h, http.MethodGet, "/api/v1/documents/d1", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "eng@acme.com") {
		t.Fatal("the document response echoed an access control list back to the client")
	}

	forbidden := request(t, h, http.MethodGet, "/api/v1/documents/d2", engineer())
	missing := request(t, h, http.MethodGet, "/api/v1/documents/nope", engineer())
	if forbidden.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("forbidden = %d, missing = %d, both should be 404", forbidden.Code, missing.Code)
	}
	if forbidden.Body.String() != missing.Body.String() {
		t.Fatalf("a forbidden document and a missing one produced different bodies:\n%s\n%s", forbidden.Body, missing.Body)
	}
}

func TestHeaderAuthNeedsASubject(t *testing.T) {
	w := request(t, newServer(t), http.MethodGet, "/api/v1/stats", map[string]string{
		api.HeaderGroups: "gdrive:eng@acme.com",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestResponsesAreJSONAndNotSniffable(t *testing.T) {
	w := request(t, newServer(t), http.MethodGet, "/healthz", nil)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the nosniff header is missing")
	}
}

func TestAssetsAreServedWhenConfigured(t *testing.T) {
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	assets := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<!doctype html>"))
	})
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, api.WithAssets(assets))

	w := request(t, s.Handler(), http.MethodGet, "/", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "doctype") {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body)
	}
}

// The interface asks who it is talking to before it draws anything, and the
// answer has to be scoped to that caller. A source list built from the whole
// index would tell a stranger which connectors exist and how much is in them.
func TestMeIsScopedToTheCaller(t *testing.T) {
	type meBody struct {
		Subject string `json:"subject"`
		Tenant  string `json:"tenant"`
		Sources []struct {
			Value string `json:"value"`
			Count int    `json:"count"`
		} `json:"sources"`
	}

	h := newServer(t)

	w := request(t, h, http.MethodGet, "/api/v1/me", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	me := decode[meBody](t, w)
	if me.Subject != "u_mei" {
		t.Errorf("subject = %q, want u_mei", me.Subject)
	}
	if me.Tenant != "acme" {
		t.Errorf("tenant = %q, want acme", me.Tenant)
	}
	if len(me.Sources) != 1 || me.Sources[0].Value != "gdrive" {
		t.Errorf("sources = %v, want only gdrive", me.Sources)
	}

	w = request(t, h, http.MethodGet, "/api/v1/me", map[string]string{api.HeaderSubject: "u_nobody"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if stranger := decode[meBody](t, w); len(stranger.Sources) != 0 {
		t.Errorf("a caller who may read nothing was told about %v", stranger.Sources)
	}
}

func TestMeNeedsACredential(t *testing.T) {
	w := request(t, newServer(t), http.MethodGet, "/api/v1/me", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

type suggestBody struct {
	Suggestions []struct {
		Kind  string `json:"kind"`
		Text  string `json:"text"`
		ID    string `json:"id"`
		Value string `json:"value"`
	} `json:"suggestions"`
}

func TestSuggestOffersDocumentsTheCallerMayRead(t *testing.T) {
	h := newServer(t)

	w := request(t, h, http.MethodGet, "/api/v1/suggest?q=payments", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decode[suggestBody](t, w)
	if len(got.Suggestions) == 0 {
		t.Fatal("no suggestions for a term that matches a readable document")
	}
	for _, s := range got.Suggestions {
		if s.ID == "d2" {
			t.Fatalf("suggested %q, which the caller may not read", s.ID)
		}
	}

	w = request(t, h, http.MethodGet, "/api/v1/suggest?q=payments", map[string]string{api.HeaderSubject: "u_nobody"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	for _, s := range decode[suggestBody](t, w).Suggestions {
		if s.Kind != "operator" {
			t.Fatalf("a caller who may read nothing was offered %q", s.Text)
		}
	}
}

// A half typed operator completes to the operator rather than searching for it,
// which is the whole reason the box does not need a separate filter menu.
func TestSuggestCompletesAnOperator(t *testing.T) {
	w := request(t, newServer(t), http.MethodGet, "/api/v1/suggest?q=ap", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	for _, s := range decode[suggestBody](t, w).Suggestions {
		if s.Kind == "operator" && strings.HasPrefix(s.Value, "app:") {
			return
		}
	}
	t.Fatal("typing the start of an operator offered no way to complete it")
}

// The operators in the box and the parameters in the URL are the same filters,
// so a query typed as text has to narrow exactly like a ticked facet does.
func TestSearchReadsOperatorsFromTheQuery(t *testing.T) {
	h := engineerWithBothGroups()
	srv := newServer(t)

	typed := decode[searchBody](t, request(t, srv, http.MethodGet, "/api/v1/search?q="+url.QueryEscape("payments app:gdrive"), h))
	ticked := decode[searchBody](t, request(t, srv, http.MethodGet, "/api/v1/search?q=payments&source=gdrive", h))

	if typed.Total != ticked.Total {
		t.Fatalf("the operator returned %d results and the parameter returned %d", typed.Total, ticked.Total)
	}
	if len(typed.Hits) != 1 || typed.Hits[0].ID != "d1" {
		t.Fatalf("app:gdrive returned %v, want only the drive document", typed.Hits)
	}
}

// A filter panel is a set of questions about what would happen if you chose
// something else, and the answer to all of them used to be zero. Counting a
// field with its own filter applied leaves the value that was ticked reporting
// its own count and every sibling reporting nothing, so the one moment somebody
// needs to know whether unticking is worth it is the moment the panel goes
// blank.
func TestSearchCountsAFacetWithItsOwnFilterLifted(t *testing.T) {
	h := engineerWithBothGroups()
	body := decode[searchBody](t, request(t, newServer(t), http.MethodGet,
		"/api/v1/search?q=payments&kind=page", h))

	if body.Total != 1 {
		t.Fatalf("total = %d, the filtered match set is one page", body.Total)
	}
	kinds := map[string]int{}
	for _, v := range body.Facets["kind"] {
		kinds[v.Value] = v.Count
	}
	if kinds["page"] != 1 || kinds["ticket"] != 1 {
		t.Fatalf("the kind facet is %v, and there is a page and a ticket to be found", kinds)
	}

	// The other fields keep the filter, because they are a different question.
	// How many results there are in Drive given that you are looking at pages is
	// useful. How many there are with the query's filters ignored is a fact about
	// the corpus rather than about the search.
	sources := map[string]int{}
	for _, v := range body.Facets["source"] {
		sources[v.Value] = v.Count
	}
	if len(sources) != 1 || sources["gdrive"] != 1 {
		t.Fatalf("the source facet is %v, and only the drive document is a page", sources)
	}
}

// Which values are ticked is the server's answer rather than the client's,
// because a count that was computed with a filter lifted only makes sense next
// to the flag that says the filter is there.
func TestSearchSaysWhichFacetValuesAreChosen(t *testing.T) {
	h := engineerWithBothGroups()
	srv := newServer(t)

	body := decode[searchBody](t, request(t, srv, http.MethodGet, "/api/v1/search?q=payments&kind=page", h))
	for _, v := range body.Facets["kind"] {
		if want := v.Value == "page"; v.Selected != want {
			t.Errorf("the kind facet reports %q as selected=%v", v.Value, v.Selected)
		}
	}
	// A different field, untouched by the query, has nothing chosen in it.
	for _, v := range body.Facets["source"] {
		if v.Selected {
			t.Errorf("the source facet reports %q as chosen and the query does not narrow by source", v.Value)
		}
	}

	// A typed operator is the same filter as a ticked box, so it ticks the box.
	typed := decode[searchBody](t, request(t, srv, http.MethodGet,
		"/api/v1/search?q="+url.QueryEscape("payments app:gdrive"), h))
	var chosen int
	for _, v := range typed.Facets["source"] {
		if v.Selected {
			chosen++
			if v.Value != "gdrive" {
				t.Errorf("app:gdrive ticked %q", v.Value)
			}
		}
	}
	if chosen != 1 {
		t.Errorf("app:gdrive ticked %d values in the source facet", chosen)
	}
}

func engineerWithBothGroups() map[string]string {
	return map[string]string{
		api.HeaderSubject: "u_mei",
		api.HeaderGroups:  "gdrive:eng@acme.com,gdrive:sales@acme.com",
	}
}

// contentServer holds an image, a page with no bytes, and an image of a type
// the interface will not render inline.
func contentServer(t *testing.T) http.Handler {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	perm := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
		Version:     1,
	}
	docs := []doc.Document{
		{
			ID: "img", Tenant: "acme", Source: "gdrive", Kind: doc.KindImage,
			Title: "architecture.png", Permissions: perm,
			Properties: map[string]string{doc.MediaType: "image/png"},
			Content:    &doc.Content{Bytes: []byte("\x89PNG\r\n\x1a\nbytes"), Width: 24, Height: 16},
		},
		{
			ID: "page", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "A page", Body: "text", Permissions: perm,
			Properties: map[string]string{doc.MediaType: "text/markdown"},
		},
		{
			ID: "odd", Tenant: "acme", Source: "gdrive", Kind: doc.KindFile,
			Title: "report.pdf", Permissions: perm,
			Properties: map[string]string{doc.MediaType: "application/pdf"},
			Content:    &doc.Content{Bytes: []byte("%PDF-1.7")},
		},
	}
	if err := st.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"})
	return s.Handler()
}

func TestContentServesTheBytesToAReaderWhoMaySeeTheDocument(t *testing.T) {
	h := contentServer(t)
	w := request(t, h, http.MethodGet, "/api/v1/documents/img/content", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "\x89PNG\r\n\x1a\nbytes" {
		t.Errorf("body is %q", got)
	}
	for header, want := range map[string]string{
		"Content-Type":           "image/png",
		"Content-Disposition":    "inline",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "private, max-age=600",
		"X-Content-Dimensions":   "24x16",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s is %q, want %q", header, got, want)
		}
	}
	if w.Header().Get("ETag") == "" {
		t.Error("no ETag, so every open pays for the bytes again")
	}
}

func TestContentAnswersAConditionalRequestWithoutTheBytes(t *testing.T) {
	h := contentServer(t)
	first := request(t, h, http.MethodGet, "/api/v1/documents/img/content", engineer())
	headers := engineer()
	headers["If-None-Match"] = first.Header().Get("ETag")

	w := request(t, h, http.MethodGet, "/api/v1/documents/img/content", headers)
	if w.Code != http.StatusNotModified {
		t.Fatalf("status %d, want 304", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("a 304 carried %d bytes", w.Body.Len())
	}
}

// The endpoint may not become a way to ask whether a document exists, so a
// document somebody may not read, one that is not there and one that holds no
// bytes all answer the same way the document endpoint does.
func TestContentIsNotFoundForEverythingTheCallerMayNotSee(t *testing.T) {
	h := contentServer(t)
	stranger := map[string]string{
		api.HeaderSubject: "u_kenji",
		api.HeaderGroups:  "gdrive:sales@acme.com",
	}
	cases := []struct {
		name    string
		target  string
		headers map[string]string
	}{
		{"a document the caller may not read", "/api/v1/documents/img/content", stranger},
		{"a document that is not there", "/api/v1/documents/nope/content", engineer()},
		{"a document that holds no bytes", "/api/v1/documents/page/content", engineer()},
	}
	var bodies []string
	for _, c := range cases {
		w := request(t, h, http.MethodGet, c.target, c.headers)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", c.name, w.Code)
		}
		bodies = append(bodies, w.Body.String())
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Errorf("the responses differ, which tells a caller which case they hit:\n%s\n%s", bodies[0], b)
		}
	}
}

func TestContentOffTheAllowListIsADownloadRatherThanAPage(t *testing.T) {
	w := request(t, contentServer(t), http.MethodGet, "/api/v1/documents/odd/content", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type is %q, want application/octet-stream", got)
	}
	if got := w.Header().Get("Content-Disposition"); got != "attachment" {
		t.Errorf("Content-Disposition is %q, want attachment", got)
	}
}

func TestDocumentReportsItsMediaType(t *testing.T) {
	w := request(t, contentServer(t), http.MethodGet, "/api/v1/documents/page", engineer())
	got := decode[map[string]any](t, w)
	if got["media_type"] != "text/markdown" {
		t.Errorf("media_type is %v, want text/markdown", got["media_type"])
	}
}
