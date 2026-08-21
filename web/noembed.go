//go:build noassets

package web

import "io/fs"

// assets reports that this build carries no interface. Built with the noassets
// tag, nothing under dist reaches the binary at all.
func assets() (fs.FS, bool) { return nil, false }

// squeeze has nothing to compress in a build with no assets in it, and saying
// so here is what keeps both compressors out of an API only binary.
func squeeze(string, []byte) map[string][]byte { return nil }
