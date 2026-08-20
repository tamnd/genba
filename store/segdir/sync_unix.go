//go:build !windows

package segdir

import "os"

// syncDir flushes a directory, which is what makes a rename durable.
//
// A rename that has not been flushed is a name that exists in the page cache
// and not on the platter, so a power cut can leave the file with its old name
// or with no name at all. Everything in this package that renames a file
// therefore follows it with one of these, and the reason it is a separate
// function is that Windows cannot do it at all: see the other half of this
// pair.
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
