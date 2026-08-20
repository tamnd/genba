package memstore

import (
	"context"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

var _ store.Maintenance = (*Store)(nil)

// Inventory calls fn for every document held for one tenant and source.
//
// The callback runs under the read lock, which is fine here and worth saying
// out loud: it means a callback that writes back into this store deadlocks. The
// pipeline collects ids and acts afterwards, and a driver over a database has
// the same shape for the same reason, so the restriction is the interface's
// rather than this driver's.
func (s *Store) Inventory(ctx context.Context, tenant, source string, fn func(store.Item) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return genba.ErrClosed
	}
	for _, d := range s.docs {
		if d.Tenant != tenant || d.Source != source {
			continue
		}
		if !fn(store.Item{ID: d.ID, Version: d.SourceUpdate}) {
			return nil
		}
	}
	return nil
}

// SetPermissions replaces the access control lists of stored documents.
func (s *Store) SetPermissions(ctx context.Context, tenant string, perms map[string]acl.Permissions) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	changed, written, err := s.setPermissions(tenant, perms)
	if err != nil {
		return 0, err
	}
	// Reported as a write, because it is one. A cache that kept a result page
	// for a document somebody has just been locked out of has to be told, and
	// a revocation is the change it is least acceptable to miss.
	s.Changes(written, false)
	return changed, nil
}

// setPermissions does the work under the lock and reports how many documents it
// rewrote and which ids to invalidate, per tenant.
func (s *Store) setPermissions(tenant string, perms map[string]acl.Permissions) (changed int, written map[string][]string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, nil, genba.ErrClosed
	}
	written = make(map[string][]string)
	for id, p := range perms {
		d, ok := s.docs[id]
		// A document of another tenant is not there as far as this call is
		// concerned. The alternative is a caller able to rewrite the access
		// control lists of a corpus it named by guessing at ids.
		if !ok || d.Tenant != tenant {
			continue
		}
		d.Permissions = p
		s.docs[id] = d
		written[d.Tenant] = append(written[d.Tenant], id)
		changed++
	}
	return changed, written, nil
}
