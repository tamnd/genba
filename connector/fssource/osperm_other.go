//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !windows

package fssource

import (
	"errors"
	"io/fs"
	"time"
)

// accessRules has nothing to read on a platform whose permissions this package
// has not been taught.
//
// It refuses rather than returning an empty list. An empty list is a document
// nobody may read, which would look like a working policy producing a very
// strict answer, and a strict answer nobody asked for is how a whole corpus
// quietly disappears from search.
func accessRules(string, fs.FileInfo) ([]rule, error) {
	return nil, errors.New("fssource: this platform's file permissions are not read by this policy")
}

// changeTime has no answer here either.
func changeTime(fs.FileInfo) time.Time { return time.Time{} }
