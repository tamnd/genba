package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/store"
)

var _ store.Feeds = (*Store)(nil)

// Feeds returns every configured feed for one tenant.
//
// The reference driver keeps them in memory like everything else it holds, so
// they last exactly as long as the process. That is the wrong behaviour for a
// deployment and the right behaviour for this driver, which is documented as
// holding nothing across a restart, and a connector configuration that survived
// one here would be the only thing in the store that did.
func (s *Store) Feeds(ctx context.Context, tenant string) ([]store.Feed, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, genba.ErrClosed
	}
	out := make([]store.Feed, 0, len(s.feeds))
	for key, f := range s.feeds {
		if key[0] != tenant {
			continue
		}
		out = append(out, clone(f))
	}
	return out, nil
}

// SaveFeed inserts or replaces one feed.
func (s *Store) SaveFeed(ctx context.Context, f store.Feed) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Check(); err != nil {
		return fmt.Errorf("memstore: save feed: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return genba.ErrClosed
	}

	now := time.Now().UTC()
	f.Updated = now
	f.Created = now
	// A replace keeps the row's own age. When it was first configured and when
	// it was last touched are two different answers and an operator reading a
	// connector that has been failing for a year wants both.
	if old, ok := s.feeds[feedKey(f)]; ok {
		f.Created = old.Created
	}
	s.feeds[feedKey(f)] = clone(f)
	return nil
}

// DropFeed forgets one feed, and forgets nothing else.
func (s *Store) DropFeed(ctx context.Context, tenant, source string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return genba.ErrClosed
	}
	delete(s.feeds, [2]string{tenant, source})
	return nil
}

func feedKey(f store.Feed) [2]string { return [2]string{f.Tenant, f.Source} }

// clone copies the JSON out with the row, because the caller owns the slice it
// passed in and a stored feed that shared it would change when they reused it.
func clone(f store.Feed) store.Feed {
	f.Config = json.RawMessage(slices.Clone([]byte(f.Config)))
	return f
}
