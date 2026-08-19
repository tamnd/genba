//go:build noassets

package web

import "io/fs"

// assets reports that this build carries no interface. Built with the noassets
// tag, nothing under dist reaches the binary at all.
func assets() (fs.FS, bool) { return nil, false }
