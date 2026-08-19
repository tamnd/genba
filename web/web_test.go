package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/genba/web"
)

func TestHandlerServesTheInterface(t *testing.T) {
	h := web.Handler()
	if h == nil {
		t.Skip("this build carries no interface")
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<!doctype html>") {
		t.Fatalf("the root did not serve a document: %q", w.Body.String()[:min(80, w.Body.Len())])
	}
}

func TestUnknownPathsFallBackToTheDocument(t *testing.T) {
	h := web.Handler()
	if h == nil {
		t.Skip("this build carries no interface")
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/search/deep/link", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("a client route returned %d, want 200 so that a reload survives", w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("Cache-Control = %q, the document must not be cached", w.Header().Get("Cache-Control"))
	}
}

func TestEnabledMatchesHandler(t *testing.T) {
	if web.Enabled() != (web.Handler() != nil) {
		t.Fatal("Enabled disagrees with Handler")
	}
}
