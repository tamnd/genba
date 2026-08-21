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
// The usual thing a build step buys is a name that changes when the contents
// change, so a browser can be told to keep the file for a year. That does not
// need a bundler. This package hashes each file as it reads it and serves it
// under both names: the plain one, revalidated, and assets/app.7f3a9c2e.js,
// immutable for a year. The document is rewritten on the way out so that its
// stylesheet links, its module preloads and its import map all point at the
// addressed names, which is why no hash appears anywhere in the source. The
// document itself is never cached, so an upgraded binary is picked up on the
// next load and every asset it names is either already held or fetched once.
//
// Compression is the other thing a build step usually does. Each asset is
// compressed once when it is first read, with brotli and with gzip, and the
// request picks between them, so a body is compressed once for the life of the
// process rather than once per request. A form that did not come out
// meaningfully smaller is dropped rather than kept, since sending it would cost
// a browser a decompression for nothing.
//
// # What the interface holds
//
// assets/cache.js keeps answers in memory for the life of the tab, so that going
// back to a search paints before the network is asked anything. The same rule
// the server cache is built on holds here: a key that does not name the asker's
// visibility is a permission bug. The client cannot work out its own view, so
// [github.com/tamnd/genba/api] hands it one in the me response and every key is
// written under it. Switching identity throws the whole store away rather than
// filtering it, because a tab holds results for more than one person over its
// life and an entry that survived the switch has leaked a document.
//
// Nothing is written to disk. The theme, the density and the development
// identity live in localStorage and are the whole of what persists, because
// they are the only things there that the corpus did not say. Results and
// documents are memory only, since memory is the session's lifetime and the
// session is exactly how long anybody was allowed to read them.
package web

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// The encodings that may be sent instead of the file itself, best first. There
// are two and there will not be a third: everything else a browser advertises
// is either older than gzip or not for text.
var encodings = []string{"br", "gzip"}

// asset is one file as it is served: its bytes, the compressed forms worth
// sending instead of them, and the validator that names this version of it.
type asset struct {
	ctype   string
	etag    string
	body    []byte
	encoded map[string][]byte
}

// servable is an asset under one of the names it answers to. The same bytes are
// reachable at a plain name and at a content addressed one, and which of the
// two was asked for is the whole difference between a file a browser checks
// every minute and a file it keeps for a year.
type servable struct {
	*asset
	name      string
	immutable bool
}

// index is built once, lazily, over the embedded tree. It is small, it never
// changes for the life of the process, and computing it up front would make an
// API only build pay for a frontend it does not serve. Reading, hashing and
// compressing all happen here, so a request does none of them.
var index = sync.OnceValue(func() map[string]servable {
	root, ok := assets()
	if !ok {
		return nil
	}

	type entry struct {
		name string
		body []byte
	}
	var files []entry
	_ = fs.WalkDir(root, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || name == "index.html" {
			return err
		}
		body, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		files = append(files, entry{name: name, body: body})
		return nil
	})

	// One goroutine per file, because compressing twenty five modules one after
	// another is most of the time a process spends starting up and they have
	// nothing to say to each other.
	built := make([]*asset, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Go(func() { built[i] = read(f.name, f.body) })
	}
	wg.Wait()

	out := map[string]servable{}
	addressed := map[string]string{}
	for i, f := range files {
		addr := address(f.name, f.body)
		out[f.name] = servable{asset: built[i], name: f.name}
		out[addr] = servable{asset: built[i], name: addr, immutable: true}
		addressed["/"+f.name] = "/" + addr
	}

	// The document is built last, because what it says depends on the names
	// every other file ended up with.
	body, err := fs.ReadFile(root, "index.html")
	if err != nil {
		return out
	}
	out["index.html"] = servable{asset: read("index.html", point(body, addressed)), name: "index.html"}
	return out
})

// read turns bytes into the asset they are served as.
func read(name string, body []byte) *asset {
	sum := sha256.Sum256(body)
	return &asset{
		ctype:   contentType(name),
		etag:    `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`,
		body:    body,
		encoded: squeeze(name, body),
	}
}

// address is the name a file is cached under for a year. Eight hex characters
// of the hash of the contents is four thousand million buckets, which is more
// than enough to tell two versions of a twenty kilobyte module apart, and short
// enough to read in a network panel.
func address(name string, body []byte) string {
	sum := sha256.Sum256(body)
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:4]) + ext
}

// point rewrites the document so that every asset it names is named by its
// content addressed URL instead. The committed document is written with plain
// URLs and works as it stands, which is what keeps it reviewable, and this is a
// substitution rather than a parse because the thing being replaced is a
// filename and there is exactly one spelling of each.
func point(body []byte, addressed map[string]string) []byte {
	if len(addressed) == 0 {
		return body
	}
	names := make([]string, 0, len(addressed))
	for from := range addressed {
		names = append(names, from)
	}
	// Longest first, so that a name which is a prefix of another cannot claim
	// the match. Nothing in the tree is named that way today and nothing should
	// have to remember that when adding a file.
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})
	pairs := make([]string, 0, 2*len(names))
	for _, from := range names {
		pairs = append(pairs, from, addressed[from])
	}
	return []byte(strings.NewReplacer(pairs...).Replace(string(body)))
}

// Handler serves the interface, or nil when the binary was built without it.
// Callers should check for nil rather than assume: an API only build is a
// supported build, not a broken one.
func Handler() http.Handler {
	if _, ok := assets(); !ok {
		return nil
	}
	files := index()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		f, ok := files[name]
		if !ok {
			// An asset that is not there is missing, and saying so is the
			// useful answer. This is the address of a file from a build that is
			// no longer running, and answering it with the document would hand
			// a browser a page where it asked for a module.
			if strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r)
				return
			}
			// Anything else is a client route rather than a missing file. The
			// shell loads and the client decides what the path meant.
			f, ok = files["index.html"]
			if !ok {
				http.NotFound(w, r)
				return
			}
		}
		serve(w, r, f)
	})
}

// serve writes one asset in whichever encoding the request allows.
func serve(w http.ResponseWriter, r *http.Request, f servable) {
	encoding, body := f.pick(r.Header.Get("Accept-Encoding"))
	etag := f.etag
	if encoding != "" {
		// A compressed body is a different representation of the same file, so
		// it gets a validator of its own. A browser that held the gzip and then
		// asked without it must not be told that what it holds is current.
		etag = strings.TrimSuffix(f.etag, `"`) + "+" + encoding + `"`
	}

	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", f.cacheControl())
	h.Set("Vary", "Accept-Encoding")
	h.Set("Content-Type", f.ctype)
	if encoding != "" {
		h.Set("Content-Encoding", encoding)
	}
	if match(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

// pick is the encoding to send and the bytes to send in it.
func (f servable) pick(accept string) (encoding string, body []byte) {
	for _, candidate := range encodings {
		encoded, ok := f.encoded[candidate]
		if ok && accepts(accept, candidate) {
			return candidate, encoded
		}
	}
	return "", f.body
}

// cacheControl lets a browser keep a file forever when the name it asked for
// says what is in it, keep it briefly when the name does not, and never keep
// the document that names the rest.
func (f servable) cacheControl() string {
	switch {
	case f.immutable:
		return "public, max-age=31536000, immutable"
	case strings.HasPrefix(f.name, "assets/"):
		return "public, max-age=60, must-revalidate"
	default:
		return "no-cache"
	}
}

// accepts reports whether a request allows an encoding. A quality of zero is a
// refusal, which is how a client asks for a file as it is without saying so.
func accepts(header, encoding string) bool {
	for part := range strings.SplitSeq(header, ",") {
		token, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(token), encoding) {
			continue
		}
		return quality(params) > 0
	}
	return false
}

func quality(params string) float64 {
	for param := range strings.SplitSeq(params, ";") {
		key, value, ok := strings.Cut(param, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return 0
		}
		return q
	}
	return 1
}

func match(r *http.Request, etag string) bool {
	for candidate := range strings.SplitSeq(r.Header.Get("If-None-Match"), ",") {
		if strings.TrimSpace(candidate) == etag {
			return true
		}
	}
	return false
}

// contentType is stated rather than sniffed. The tree holds text, the extension
// says which kind, and a module served as anything but JavaScript is a page
// that does not load at all.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	}
	if ctype := mime.TypeByExtension(path.Ext(name)); ctype != "" {
		return ctype
	}
	return "application/octet-stream"
}

// Enabled reports whether the binary carries the interface.
func Enabled() bool {
	_, ok := assets()
	return ok
}
