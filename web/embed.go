//go:build !noassets

package web

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io/fs"
	"path"
	"sync"

	"github.com/andybalholm/brotli"
)

//go:embed all:dist
var dist embed.FS

// assets returns the built interface rooted at dist, so that a request for
// /index.html does not have to know the directory it was built into.
var assets = sync.OnceValues(func() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// The directory is embedded at compile time, so this cannot happen
		// without the embed directive above having changed.
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
})

// The size under which compressing is not worth the trouble. A body this small
// arrives in the same packet either way, and a browser still has to run a
// decompressor over it.
const worthCompressing = 512

// How much smaller a compressed form has to be to be kept at all. A file that
// barely compresses is one somebody already compressed.
const worthKeeping = 0.9

// squeeze is every encoding worth sending a file in instead of sending the file.
//
// It runs once per asset, at startup, because the alternative is compressing
// the same twenty five modules again for every visitor. Both encoders are asked
// for their best: this is paid once for the life of the process, and what it
// buys is paid for on every cold load anybody ever has.
func squeeze(name string, body []byte) map[string][]byte {
	if len(body) < worthCompressing || !compressible(name) {
		return nil
	}

	out := map[string][]byte{}
	var wg sync.WaitGroup
	var gz, br []byte
	wg.Add(2)
	go func() {
		defer wg.Done()
		gz = gzipped(body)
	}()
	go func() {
		defer wg.Done()
		br = brotlied(body)
	}()
	wg.Wait()

	for encoding, encoded := range map[string][]byte{"gzip": gz, "br": br} {
		if len(encoded) > 0 && float64(len(encoded)) < worthKeeping*float64(len(body)) {
			out[encoding] = encoded
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func gzipped(body []byte) []byte {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := w.Write(body); err != nil {
		return nil
	}
	if err := w.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

func brotlied(body []byte) []byte {
	var buf bytes.Buffer
	w := brotli.NewWriterLevel(&buf, brotli.BestCompression)
	if _, err := w.Write(body); err != nil {
		return nil
	}
	if err := w.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// compressible is the text in the tree. An image or a font is already
// compressed and running a second pass over one costs time to make it larger.
func compressible(name string) bool {
	switch path.Ext(name) {
	case ".js", ".css", ".html", ".json", ".svg", ".txt", ".map":
		return true
	}
	return false
}
