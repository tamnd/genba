package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	Query string `json:"query"`
	Total int    `json:"total"`
	Hits  []struct {
		ID      string  `json:"id"`
		Title   string  `json:"title"`
		Snippet string  `json:"snippet"`
		Score   float64 `json:"score"`
	} `json:"hits"`
	Facets map[string][]struct {
		Value string `json:"value"`
		Count int    `json:"count"`
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
