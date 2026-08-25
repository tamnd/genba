package fssource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/doc"
)

var (
	_ connector.Enumerator = (*Source)(nil)
	_ connector.Fetcher    = (*Source)(nil)
	_ connector.Counted    = (*Source)(nil)
)

// Enumerate calls fn for every file the source would index, with its
// modification time as the version.
//
// It is the same walk [Source.Sync] does and it reads nothing. That is the
// whole difference in price on a filesystem: a hundred thousand stats against a
// hundred thousand reads, which on a real corpus is a second against a minute.
func (s *Source) Enumerate(ctx context.Context, fn func(connector.Item) bool) error {
	err := s.walk(ctx, func(rel string, info fs.FileInfo) error {
		if !fn(connector.Item{ID: s.id(rel), Version: version(info.ModTime())}) {
			return fs.SkipAll
		}
		return nil
	})
	// A walk this one stopped on purpose is not a failed walk, and reporting it
	// as one would make a reconciliation that used an early exit delete the
	// entire index.
	if errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

// Fetch reads one file by document id.
//
// It returns [connector.ErrGone] for a file that is no longer there, which is
// the normal answer on a tree people are working in rather than a failure.
func (s *Source) Fetch(ctx context.Context, id string) (doc.Document, error) {
	if err := ctx.Err(); err != nil {
		return doc.Document{}, err
	}
	rel, ok := s.rel(id)
	if !ok {
		// An id this source did not mint names a file it does not have. Saying
		// so is the same answer as a deleted file, and it is the safe one: the
		// caller deletes it from the index rather than storing something read
		// from a path an id was allowed to steer.
		return doc.Document{}, connector.ErrGone
	}

	full := filepath.Join(s.root, filepath.FromSlash(rel))
	s.counters.metadata.Add(1)
	info, err := os.Stat(full)
	switch {
	case os.IsNotExist(err):
		return doc.Document{}, connector.ErrGone
	case err != nil:
		return doc.Document{}, err
	}
	if !info.Mode().IsRegular() {
		return doc.Document{}, connector.ErrGone
	}
	if limit := s.limitFor(rel); info.Size() > limit {
		return doc.Document{}, connector.ErrGone
	}

	d, err := s.read(ctx, full, rel, info)
	if err != nil {
		return doc.Document{}, err
	}
	return d, nil
}

// Counters returns what this source has spent on the filesystem.
func (s *Source) Counters() connector.Counters {
	return connector.Counters{
		Lists:    s.counters.lists.Load(),
		Metadata: s.counters.metadata.Load(),
		Fetches:  s.counters.fetches.Load(),
		Bytes:    s.counters.bytes.Load(),
	}
}

// id is the document id for a path relative to the root.
func (s *Source) id(rel string) string { return s.name + ":" + rel }

// rel is the inverse of id, and is where a path that tried to leave the tree is
// stopped.
//
// The cleaning is not tidiness. An id arrives from the index, the index was
// written by a connector, and a connector is a lot of code to trust with a
// string that turns into a file path. Making the path absolute first and then
// stripping the leading separator means a "../../etc/passwd" collapses to
// "etc/passwd" inside the root rather than escaping it, and the stat that
// follows simply finds nothing.
func (s *Source) rel(id string) (string, bool) { return relOf(s.name, id) }

// relOf is [Source.rel] without a source, so that a [Checker] built over the
// same tree reads an id exactly the way the connector that minted it does.
//
// Two readings of the same string is how a path that one of them stops gets
// through the other.
func relOf(name, id string) (rel string, ok bool) {
	rel, ok = strings.CutPrefix(id, name+":")
	if !ok || rel == "" {
		return "", false
	}
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if rel == "" || rel == "." {
		return "", false
	}
	return rel, true
}

// version is the form a modification time takes in a cursor and in an item, so
// that the two are comparable without either side parsing the other's.
func version(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// walk is the tree traversal both Sync and Enumerate are built on.
//
// It applies the skip rules, the size limits and the include rule, counts what
// it spent, and calls fn with the path relative to the root. What it does not
// do is decide anything about documents, which is what keeps the two callers
// from drifting apart on which files are in the corpus.
func (s *Source) walk(ctx context.Context, fn func(rel string, info fs.FileInfo) error) error {
	return filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that disappeared under the walk, or one this process
			// may not read, is a fact about the tree rather than a reason to
			// abandon the sync.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if p != s.root && s.skipDir(d.Name()) {
				return fs.SkipDir
			}
			s.counters.lists.Add(1)
			return nil
		}
		if !d.Type().IsRegular() || !s.includeIf(d.Name()) {
			return nil
		}

		s.counters.metadata.Add(1)
		info, err := d.Info()
		if err != nil {
			s.skipped(p, err)
			return nil
		}
		if limit := s.limitFor(d.Name()); info.Size() > limit {
			s.skipped(p, errTooBig(info.Size(), limit))
			return nil
		}

		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			s.skipped(p, err)
			return nil
		}
		return fn(filepath.ToSlash(rel), info)
	})
}

// errTooBig is the reason a file over the size limit was passed over.
func errTooBig(size, limit int64) error {
	return fmt.Errorf("%d bytes is over the limit of %d", size, limit)
}
