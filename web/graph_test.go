package web_test

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/tamnd/genba/web"
)

// The piece of a bundler this repository actually needs.
//
// There is no build step, which is a promise rather than an accident, and the
// price of it is that nothing walks the module graph on the way to the browser.
// So the graph is walked here instead. What that catches is a module added
// without a preload, a preload left behind by a module that was deleted, an
// importer that reached for a path instead of going through the map, and a
// cycle. All four look perfectly fine in review and none of them fails anything
// else in the tree.

const entry = "app.js"

var (
	importing = regexp.MustCompile(`(?m)(?:from|import)\s*\(?\s*"([^"]+)"`)
	preloaded = regexp.MustCompile(`<link rel="modulepreload" href="([^"]+)"`)
	inMap     = regexp.MustCompile(`(?s)<script type="importmap">(.*?)</script>`)
	addressed = regexp.MustCompile(`/assets/([a-z]+)\.([0-9a-f]{8})\.(js|css)`)
)

func TestTheDocumentPreloadsTheWholeGraph(t *testing.T) {
	graph := imports(t)

	// Everything reachable from the entry point, which is everything the first
	// paint waits on however deep it sits.
	want := map[string]bool{}
	var walk func(string)
	walk = func(module string) {
		if want[module] {
			return
		}
		want[module] = true
		for _, next := range graph[module] {
			walk(next)
		}
	}
	walk(entry)

	got := map[string]bool{}
	for _, m := range preloaded.FindAllStringSubmatch(document(t), 1000) {
		got[strings.TrimPrefix(m[1], "/assets/")] = true
	}

	for module := range want {
		if !got[module] {
			t.Errorf("%s is in the graph and is not preloaded, so the browser finds it a round trip late", module)
		}
	}
	for module := range got {
		if !want[module] {
			t.Errorf("%s is preloaded and nothing imports it, so every visitor downloads it for nothing", module)
		}
	}
}

func TestEveryModuleImportsThroughTheMap(t *testing.T) {
	body := document(t)
	found := inMap.FindStringSubmatch(body)
	if found == nil {
		t.Fatal("the document has no import map, so a bare specifier resolves to nothing")
	}

	var parsed struct {
		Imports map[string]string `json:"imports"`
	}
	if err := json.Unmarshal([]byte(found[1]), &parsed); err != nil {
		t.Fatalf("the import map is not valid JSON, which fails silently in a browser: %v", err)
	}
	if parsed.Imports["genba/"] != "/assets/" {
		t.Errorf("the prefix entry is %q, want /assets/", parsed.Imports["genba/"])
	}

	for module, specifiers := range imports(t) {
		for _, target := range specifiers {
			if _, ok := parsed.Imports["genba/"+target]; !ok {
				t.Errorf("%s imports %s and the map has no entry for it, so it is not addressed by content", module, target)
			}
		}
	}

	// A module that says where another module lives is a module that has to be
	// edited when one moves, and it is a module that would carry a hash in its
	// source if it were addressed by content.
	for name, body := range modules(t) {
		for _, m := range importing.FindAllStringSubmatch(body, -1) {
			if !strings.HasPrefix(m[1], "genba/") {
				t.Errorf("%s imports %q rather than a genba/ specifier", name, m[1])
			}
		}
	}
}

func TestTheModuleGraphHasNoCycles(t *testing.T) {
	graph := imports(t)

	// Modules do load in a cycle, which is exactly the problem: it works until
	// one of the two reads something from the other while it is still being
	// evaluated, and then it is undefined at a line that has not changed.
	const (
		open = 1
		done = 2
	)
	seen := map[string]int{}
	var walk func(string, []string)
	walk = func(module string, path []string) {
		if seen[module] == done {
			return
		}
		if seen[module] == open {
			t.Errorf("import cycle: %s", strings.Join(append(path, module), " -> "))
			return
		}
		seen[module] = open
		for _, next := range graph[module] {
			walk(next, append(path, module))
		}
		seen[module] = done
	}
	for module := range graph {
		walk(module, nil)
	}
}

func TestAnAssetIsServedByContentAndKeptForAYear(t *testing.T) {
	h := web.Handler()
	if h == nil {
		t.Skip("this build carries no interface")
	}

	// The served document names the addressed URLs, and those are the ones a
	// browser asks for, so this reads them out of it rather than computing a
	// hash of its own and checking the server against itself.
	shell := httptest.NewRecorder()
	h.ServeHTTP(shell, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	names := addressed.FindAllString(shell.Body.String(), -1)
	if len(names) == 0 {
		t.Fatal("the document names no addressed asset, so nothing is cached for longer than a minute")
	}

	for _, name := range names {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, name, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s returned %d, and it is what the document asks for", name, w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("%s Cache-Control = %q, want a year and immutable", name, got)
		}
	}

	// The plain name still answers, because a page that is open when the binary
	// is upgraded goes on asking for what it already knows about, and because
	// the checks in scripts/ import modules by path.
	plain := addressed.ReplaceAllString(names[0], "/assets/$1.$3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, plain, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%s returned %d, want the same file under its own name", plain, w.Code)
	}
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "must-revalidate") {
		t.Errorf("%s Cache-Control = %q, a name that does not say what is in it must be revalidated", plain, got)
	}

	// The address of a file from a build that is no longer running. It is
	// missing, and it says so, because the alternative is handing a browser the
	// document where it asked for a module and failing on the MIME type three
	// steps later.
	stale := httptest.NewRecorder()
	h.ServeHTTP(stale, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/assets/app.deadbeef.js", nil))
	if stale.Code != http.StatusNotFound {
		t.Errorf("an asset from an older build returned %d, want 404", stale.Code)
	}
}

func TestTheDocumentIsNeverCachedAndNamesNothingPlain(t *testing.T) {
	h := web.Handler()
	if h == nil {
		t.Skip("this build carries no interface")
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("the document Cache-Control = %q, and it is the one file that must not be held", got)
	}

	// Every asset the served document names carries a hash. Missing one means a
	// file that a browser revalidates on every load for the life of a build.
	body := w.Body.String()
	for _, m := range regexp.MustCompile(`/assets/[a-z]+\.(js|css)`).FindAllString(body, -1) {
		t.Errorf("the served document still names %s without a hash", m)
	}
	if !strings.Contains(body, `"genba/": "/assets/"`) {
		t.Error("the prefix entry did not survive the rewrite, so a module with no entry of its own stops resolving")
	}
}

func TestBodiesArePrecompressedAndNegotiated(t *testing.T) {
	h := web.Handler()
	if h == nil {
		t.Skip("this build carries no interface")
	}

	ask := func(accept string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/assets/app.js", nil)
		if accept != "" {
			r.Header.Set("Accept-Encoding", accept)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	plain := ask("")
	if plain.Header().Get("Content-Encoding") != "" {
		t.Errorf("a request that asked for no encoding got %q", plain.Header().Get("Content-Encoding"))
	}
	if plain.Header().Get("Vary") != "Accept-Encoding" {
		t.Error("a negotiated response that does not vary on Accept-Encoding poisons every cache in front of it")
	}

	for accept, want := range map[string]string{
		"gzip":                    "gzip",
		"br, gzip":                "br",
		"gzip, deflate, br, zstd": "br",
		"gzip;q=0, br":            "br",
		"br;q=0, gzip":            "gzip",
		"br;q=0, gzip;q=0":        "",
	} {
		got := ask(accept).Header().Get("Content-Encoding")
		if got != want {
			t.Errorf("Accept-Encoding %q was answered with %q, want %q", accept, got, want)
		}
	}

	if compressed, uncompressed := ask("br").Body.Len(), plain.Body.Len(); compressed >= uncompressed {
		t.Errorf("the brotli body is %d bytes and the file is %d", compressed, uncompressed)
	}

	// A validator names a representation rather than a file, so the gzip and
	// the file itself cannot share one. A cache that has both and one ETag
	// hands a browser a body it cannot read.
	if ask("gzip").Header().Get("ETag") == plain.Header().Get("ETag") {
		t.Error("the compressed body and the file are served under the same ETag")
	}

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/assets/app.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("If-None-Match", ask("gzip").Header().Get("ETag"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified {
		t.Errorf("a browser that holds the current gzip was answered %d rather than 304", w.Code)
	}
}

// document is the committed shell, which is what the graph is asserted against.
// The served one has been rewritten and is read directly where that is the
// point.
func document(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("dist/index.html")
	if err != nil {
		t.Fatalf("reading the document: %v", err)
	}
	return string(body)
}

// modules is every ES module in the tree, by name.
func modules(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(os.DirFS("dist/assets"), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(name) != ".js" {
			return err
		}
		body, err := os.ReadFile(path.Join("dist/assets", name))
		if err != nil {
			return err
		}
		out[name] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the modules: %v", err)
	}
	return out
}

// imports is the graph: every module, and the modules it names.
func imports(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for name, body := range modules(t) {
		var named []string
		for _, m := range importing.FindAllStringSubmatch(body, -1) {
			target := strings.TrimPrefix(m[1], "genba/")
			if target == m[1] || slices.Contains(named, target) {
				continue
			}
			named = append(named, target)
		}
		sort.Strings(named)
		out[name] = named
	}
	if len(out[entry]) == 0 {
		t.Fatalf("%s imports nothing, so the graph walked from it is not the interface", entry)
	}
	return out
}
