// Package web serves the browser interface out of the binary.
//
// The assets are compiled in, so a deployment is one file with no sidecar
// directory to forget. Building with the noassets tag drops them, which is what
// an API only deployment and most container images want, and it takes the whole
// frontend out of the binary rather than shipping it unused.
//
// [Handler] returns something that can be mounted anywhere. It serves index.html
// for any path it does not recognise, because the interface routes on the client
// and a deep link has to survive a page reload.
//
// # Why there is no build step
//
// The interface is hand written HTML, CSS and ES modules, committed as it is
// served. A clone builds a working interface with the Go toolchain and nothing
// else, and the assets in the repository are the assets in the binary, which
// makes a diff of the interface readable in a review.
//
// The cost is that nothing renames a file when its contents change, so the
// usual trick of caching hashed filenames forever is not available. Every file
// is served with an ETag of its contents instead. A browser revalidates and
// gets a 304 for anything it already has, which costs one conditional request
// per asset and is always correct, including the case that matters: an operator
// upgrading the binary under a browser that has the old one cached.
package web

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// file is one asset with the validator it is served under.
type file struct {
	name string
	etag string
}

// index is built once, lazily, over the embedded tree. It is small, it never
// changes for the life of the process, and computing it up front would make an
// API only build pay for a frontend it does not serve.
var index = sync.OnceValue(func() map[string]file {
	root, ok := assets()
	if !ok {
		return nil
	}
	out := map[string]file{}
	_ = fs.WalkDir(root, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[name] = file{name: name, etag: `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`}
		return nil
	})
	return out
})

// Handler serves the interface, or nil when the binary was built without it.
// Callers should check for nil rather than assume: an API only build is a
// supported build, not a broken one.
func Handler() http.Handler {
	root, ok := assets()
	if !ok {
		return nil
	}
	files := index()
	server := http.FileServerFS(root)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		f, ok := files[name]
		if !ok {
			// An unknown path is a client route, not a missing file. The shell
			// loads and the client decides what the path meant.
			serveIndex(w, r, root, files)
			return
		}

		w.Header().Set("ETag", f.etag)
		w.Header().Set("Cache-Control", cacheControl(name))
		if match(r, f.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		server.ServeHTTP(w, r)
	})
}

// cacheControl lets a browser hold an asset briefly without asking, and never
// lets it hold the document that names the assets.
func cacheControl(name string) string {
	if strings.HasPrefix(name, "assets/") {
		return "public, max-age=60, must-revalidate"
	}
	return "no-cache"
}

func match(r *http.Request, etag string) bool {
	for candidate := range strings.SplitSeq(r.Header.Get("If-None-Match"), ",") {
		if strings.TrimSpace(candidate) == etag {
			return true
		}
	}
	return false
}

// Enabled reports whether the binary carries the interface.
func Enabled() bool {
	_, ok := assets()
	return ok
}

func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS, files map[string]file) {
	b, err := fs.ReadFile(root, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if f, ok := files["index.html"]; ok {
		w.Header().Set("ETag", f.etag)
		if match(r, f.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(string(b)))
}
