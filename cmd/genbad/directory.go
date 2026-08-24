package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/config"
	"github.com/tamnd/genba/directory"
)

// authenticator is what the server authenticates with, and which of the two it
// is depends on whether the deployment was given a directory.
//
// Without one, the groups on a request are believed. That is the right shape
// behind a proxy that has already done the resolution and on a laptop somebody
// is trying the binary on, and it is not a shape to leave a company in.
//
// With one, the groups on a request are thrown away and the directory is asked
// instead. Who somebody is comes from a credential. What they are a member of
// is a fact about the company that changes without anybody signing in again,
// and a header carrying it is a copy of an answer somebody else cached.
//
// With more than one, the group sets are unioned. That is a company that
// acquired another company: two sets of people, two files, one search box, and
// no collisions because every group key carries the name of the directory it
// came from.
//
// There is one cache and it sits above the union, so the staleness bound is the
// number in the configuration rather than something that emerges from a stack.
//
// The reload loops, if there are any, run until the context is done.
func authenticator(ctx context.Context, cfg config.Config, log *slog.Logger) (api.Authenticator, error) {
	header := api.HeaderAuth{Tenant: cfg.Tenant, Admins: cfg.Admins}
	if len(cfg.Directories) == 0 {
		return header, nil
	}

	var (
		files = make([]*swap, 0, len(cfg.Directories))
		parts = make([]directory.Expander, 0, len(cfg.Directories))
	)
	for _, path := range cfg.Directories {
		held := &swap{path: path}
		if err := held.read(); err != nil {
			return nil, err
		}
		res, err := directory.New(held)
		if err != nil {
			return nil, err
		}
		files = append(files, held)
		parts = append(parts, res)
	}
	union, err := directory.NewMulti(parts...)
	if err != nil {
		return nil, err
	}
	cached, err := directory.NewCache(union, directory.WithTTL(cfg.DirectoryTTL))
	if err != nil {
		return nil, err
	}

	log.Info("resolving groups from a directory",
		"paths", cfg.Directories,
		"sources", union.Directories(),
		"staleness", cached.Staleness(),
		"refresh", cfg.DirectoryRefresh,
	)
	if cfg.DirectoryRefresh > 0 {
		for _, held := range files {
			go held.reload(ctx, cfg.DirectoryRefresh, cached, log)
		}
	}
	return api.Resolving{Auth: header, Resolver: cached}, nil
}

// swap is a directory that can be replaced under the readers.
//
// The pointer is read on every lookup and written by the reload, which is a
// whole file at a time. Reading a directory that is half of the old file and
// half of the new one would be a person resolving to a group set that never
// existed, so nothing is ever edited in place.
type swap struct {
	path string

	held atomic.Pointer[directory.Static]

	// sum is the file as it was last read, so that a reload that changes
	// nothing costs a hash rather than a parse and a cache flush.
	sum atomic.Pointer[[32]byte]
}

// read parses the file and installs it, and reports whether the contents
// differed from what was already held.
func (s *swap) read() error {
	_, err := s.reread()
	return err
}

func (s *swap) reread() (bool, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return false, fmt.Errorf("directory: %w", err)
	}
	sum := sha256.Sum256(raw)
	if held := s.sum.Load(); held != nil && *held == sum {
		return false, nil
	}

	// Parsed from the bytes already read rather than from the file again,
	// because a file that changes between the hash and the parse would leave
	// the two describing different things.
	d, err := directory.ReadStatic(bytes.NewReader(raw))
	if err != nil {
		return false, fmt.Errorf("directory %s: %w", s.path, err)
	}
	if held := s.held.Load(); held != nil && held.Name() != d.Name() {
		// Every group key carries the directory's name, so renaming it renames
		// every group in every rule at once. That is a different directory
		// rather than an edit to this one, and it is a restart.
		return false, fmt.Errorf("directory %s: the name changed from %q to %q, which renames every group", s.path, held.Name(), d.Name())
	}
	s.held.Store(d)
	s.sum.Store(&sum)
	return true, nil
}

// reload rereads the file on a ticker.
//
// A file that has been deleted or has stopped parsing leaves the last good one
// in place and logs. An operator halfway through an edit should not take
// everybody's groups away, and the alternative, refusing every request until
// the file is valid again, turns a typo into an outage.
func (s *swap) reload(ctx context.Context, every time.Duration, cached *directory.Cache, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			changed, err := s.reread()
			switch {
			case err != nil:
				log.Error("rereading the directory, keeping the one already loaded",
					"path", s.path, "error", err)
			case changed:
				// Everybody, rather than one person, because the file changed
				// and nothing in it says who was affected. This is the
				// deployment with forty people in it, so a flush is forty
				// expansions against a file already in the page cache.
				cached.Clear()
				log.Info("the directory changed and was reloaded", "path", s.path)
			}
		}
	}
}

// Name is the identity source the groups belong to.
func (s *swap) Name() string {
	if d := s.held.Load(); d != nil {
		return d.Name()
	}
	return ""
}

// Subject looks one up in whatever is currently held.
func (s *swap) Subject(ctx context.Context, id string) (directory.Subject, error) {
	d := s.held.Load()
	if d == nil {
		return directory.Subject{}, directory.ErrNoSubject
	}
	return d.Subject(ctx, id)
}

// Group looks one up in whatever is currently held.
func (s *swap) Group(ctx context.Context, id string) (directory.Group, error) {
	d := s.held.Load()
	if d == nil {
		return directory.Group{}, directory.ErrNoGroup
	}
	return d.Group(ctx, id)
}
