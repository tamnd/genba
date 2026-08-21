package web_test

import (
	"bytes"
	"compress/gzip"
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

	// A document id carries a source prefix and a path, so the route for one
	// arrives here with a colon and slashes already decoded and looks exactly
	// like a file that is not there. It has to reach the shell anyway, or every
	// link anybody pastes is a 404 on the first load.
	for _, path := range []string{
		"/search/deep/link",
		"/d/repo:index/cache.go",
		"/d/notes:Spec/2121/ui/08_kaggle_calibration.md",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s returned %d, want 200 so that a reload survives", path, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s Cache-Control = %q, the document must not be cached", path, w.Header().Get("Cache-Control"))
		}
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

// The interface has a transfer budget and no build step, so the files on disk
// are the files on the wire and this is the only place the budget can be
// checked. The numbers are per file gzipped and summed, because every module is
// fetched separately and a bundler is not going to appear to fix it.
func TestTheInterfaceFitsItsTransferBudget(t *testing.T) {
	const (
		scriptBudget = 180 << 10
		styleBudget  = 40 << 10
	)

	totals := map[string]int{}
	err := fs.WalkDir(os.DirFS("dist"), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := path.Ext(name)
		if ext != ".js" && ext != ".css" {
			return nil
		}
		body, err := os.ReadFile(path.Join("dist", name))
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(body); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		totals[ext] += buf.Len()
		return nil
	})
	if err != nil {
		t.Fatalf("walking the assets: %v", err)
	}

	if totals[".js"] > scriptBudget {
		t.Errorf("the scripts are %d bytes gzipped, over the %d budget", totals[".js"], scriptBudget)
	}
	if totals[".css"] > styleBudget {
		t.Errorf("the styles are %d bytes gzipped, over the %d budget", totals[".css"], styleBudget)
	}
	t.Logf("scripts %d bytes gzipped of %d, styles %d of %d", totals[".js"], scriptBudget, totals[".css"], styleBudget)
}

// Nothing the server said about the corpus is written to disk.
//
// Search results are titles and body excerpts from a permissioned corpus, and
// putting those in localStorage or IndexedDB leaves them on the machine after
// the session that was allowed to see them has ended. Memory is the correct
// lifetime because it is the session's lifetime, and this is a test rather than
// a convention because the tempting change is one line long and would look like
// an improvement.
func TestTheCacheKeepsNothingOnDisk(t *testing.T) {
	// The theme, the density and the development identity are the whole of what
	// may persist, and none of them is anything the corpus said.
	persist := map[string]bool{"api.js": true, "app.js": true}

	err := fs.WalkDir(os.DirFS("dist"), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(name) != ".js" {
			return err
		}
		body, err := os.ReadFile(path.Join("dist", name))
		if err != nil {
			return err
		}
		// The trailing dot is what separates a use from a mention, since these
		// names are worth writing down in the comment that explains why they are
		// not used.
		for _, store := range []string{"indexedDB.", "sessionStorage.", "caches."} {
			if strings.Contains(string(body), store) {
				t.Errorf("%s uses %s, which outlives the session that was allowed to read the corpus", name, store)
			}
		}
		if strings.Contains(string(body), "localStorage.") && !persist[path.Base(name)] {
			t.Errorf("%s writes to localStorage, which is only for the theme, the density and the identity", name)
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
