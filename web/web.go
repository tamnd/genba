// Package web serves the browser interface out of the binary.
//
// The built assets are compiled in, so a deployment is one file with no
// sidecar directory to forget. Building with the noassets tag drops them, which
// is what an API only deployment and most container images want, and it takes
// the whole frontend out of the binary rather than shipping it unused.
//
// [Handler] returns something that can be mounted anywhere. It serves index.html
// for any path it does not recognise, because the interface routes on the client
// and a deep link has to survive a page reload.
package web

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// Handler serves the interface, or nil when the binary was built without it.
// Callers should check for nil rather than assume: an API only build is a
// supported build, not a broken one.
func Handler() http.Handler {
	root, ok := assets()
	if !ok {
		return nil
	}
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(root, name); err != nil {
			serveIndex(w, r, root)
			return
		}
		// Everything under assets/ is content addressed by the bundler, so it
		// can be cached hard. Everything else has a stable name and must not be.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// Enabled reports whether the binary carries the interface.
func Enabled() bool {
	_, ok := assets()
	return ok
}

func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS) {
	b, err := fs.ReadFile(root, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(string(b)))
}
