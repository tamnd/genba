package web_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
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

// The interface builds every node it shows, and never hands the browser a
// string to parse as markup. That is what makes a document title containing a
// script tag a document title, and it is the only defence there is, because
// there is no sanitiser in the stack to fall back on. It is a test rather than a
// convention because a single innerHTML added later would undo it silently.
func TestAssetsBuildNodesRatherThanMarkup(t *testing.T) {
	banned := []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("}

	err := fs.WalkDir(os.DirFS("dist"), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(name) != ".js" {
			return err
		}
		body, err := os.ReadFile(path.Join("dist", name))
		if err != nil {
			return err
		}
		for _, bad := range banned {
			if strings.Contains(string(body), bad) {
				t.Errorf("%s uses %s, which turns a string into markup", name, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the assets: %v", err)
	}
}

func TestEnabledMatchesHandler(t *testing.T) {
	if web.Enabled() != (web.Handler() != nil) {
		t.Fatal("Enabled disagrees with Handler")
	}
}
