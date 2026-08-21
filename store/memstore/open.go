package memstore

import (
	"context"
	"slices"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

var _ store.OpenLog = (*Store)(nil)

// opened is one person's reading history, newest last.
//
// A slice rather than a map because it is short, bounded by store.OpenHistory,
// and every read of it is in order. Moving an entry to the end is a linear
// search over at most two hundred strings, which is faster than the map plus
// sort that would replace it.
type opened struct {
	ids []string
	at  map[string]time.Time
}

// RecordOpen notes that the principal opened a document.
func (s *Store) RecordOpen(ctx context.Context, p *acl.Principal, id string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return genba.ErrClosed
	}
	// The visibility check is here rather than in the caller so that this driver
	// obeys the same rule as the rest of the interface: the principal is applied
	// while the driver walks its own data.
	d, ok := s.docs[id]
	if !ok || !visible(p, d) {
		return nil
	}

	key := reader(p)
	log, ok := s.opens[key]
	if !ok {
		log = &opened{at: make(map[string]time.Time)}
		s.opens[key] = log
	}
	if i := slices.Index(log.ids, id); i >= 0 {
		log.ids = slices.Delete(log.ids, i, i+1)
	}
	log.ids = append(log.ids, id)
	log.at[id] = at
	if over := len(log.ids) - store.OpenHistory; over > 0 {
		for _, dropped := range log.ids[:over] {
			delete(log.at, dropped)
		}
		log.ids = slices.Delete(log.ids, 0, over)
	}
	return nil
}

// Opens returns what the principal opened, most recent first.
func (s *Store) Opens(ctx context.Context, p *acl.Principal, limit int) ([]store.Open, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, genba.ErrNoPrincipal
	}
	if limit <= 0 {
		return nil, nil
	}
	// Nobody can be given more history than a driver keeps, so a caller asking
	// for a thousand is asking for all of it. Clamping here rather than trusting
	// the number makes the allocation below a fact about this driver instead of
	// a fact about whatever arrived in a query string.
	if limit > store.OpenHistory {
		limit = store.OpenHistory
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, genba.ErrClosed
	}

	log, ok := s.opens[reader(p)]
	if !ok {
		return nil, nil
	}
	out := make([]store.Open, 0, min(limit, len(log.ids)))
	for i := len(log.ids) - 1; i >= 0 && len(out) < limit; i-- {
		id := log.ids[i]
		d, ok := s.docs[id]
		// A document that was deleted, or that this person may no longer read,
		// is not in the list. The entry is left where it is rather than removed,
		// because a read is not the place to write and because a permission that
		// comes back should bring the document back with it.
		if !ok || !visible(p, d) {
			continue
		}
		out = append(out, store.Open{Document: d, At: log.at[id]})
	}
	return out, nil
}

// reader is the key a reading history is kept under.
//
// The tenant is in it because two tenants can use the same subject string, and
// the whole point of the tenant field is that they do not see each other.
func reader(p *acl.Principal) [2]string { return [2]string{p.Tenant, p.Subject} }
