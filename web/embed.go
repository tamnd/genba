//go:build !noassets

package web

import (
	"embed"
	"io/fs"
	"sync"
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
