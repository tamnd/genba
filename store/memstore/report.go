package memstore

import (
	"context"
	"slices"
	"sort"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

var _ store.Reporter = (*Store)(nil)

// Report records that somebody says the document is out of date.
func (s *Store) Report(ctx context.Context, p *acl.Principal, r store.Report) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	if err := r.Check(); err != nil {
		return err
	}
	key := store.ReportKey(p)
	if key == "" {
		return store.ErrNoReporter
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return genba.ErrClosed
	}
	// Same rule as recording an open: a document this principal cannot see is a
	// write that does not happen rather than an error naming the id.
	d, ok := s.docs[r.Doc]
	if !ok || !visible(p, d) {
		return nil
	}
	said, ok := s.reports[r.Doc]
	if !ok {
		said = make(map[string]store.Report)
		s.reports[r.Doc] = said
	}
	said[key] = r
	return nil
}

// Resolve clears every report on one document.
func (s *Store) Resolve(ctx context.Context, p *acl.Principal, id string) error {
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
	d, ok := s.docs[id]
	if !ok || !visible(p, d) {
		return nil
	}
	delete(s.reports, id)
	return nil
}

// Withdraw removes the report this principal wrote, and only that one.
func (s *Store) Withdraw(ctx context.Context, p *acl.Principal, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	key := store.ReportKey(p)
	if key == "" {
		return store.ErrNoReporter
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return genba.ErrClosed
	}
	d, ok := s.docs[id]
	if !ok || !visible(p, d) {
		return nil
	}
	said := s.reports[id]
	delete(said, key)
	// The map goes when the last row in it does, because the read paths take an
	// empty one for a document nobody reported and an owner's panel that kept a
	// row with a count of zero on it would be a panel with nothing to act on.
	if len(said) == 0 {
		delete(s.reports, id)
	}
	return nil
}

// Reports returns what has been said about the documents the principal may read.
func (s *Store) Reports(ctx context.Context, p *acl.Principal, ids []string) (map[string]store.Staleness, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, genba.ErrNoPrincipal
	}
	if len(ids) == 0 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, genba.ErrClosed
	}

	key := store.ReportKey(p)
	var out map[string]store.Staleness
	for _, id := range ids {
		said := s.reports[id]
		if len(said) == 0 {
			continue
		}
		d, ok := s.docs[id]
		if !ok || !visible(p, d) {
			continue
		}
		if out == nil {
			out = make(map[string]store.Staleness, len(ids))
		}
		out[id] = gather(id, said, key)
	}
	return out, nil
}

// Reported returns what has been said about the principal's own documents.
func (s *Store) Reported(ctx context.Context, p *acl.Principal, limit int) ([]store.Flagged, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, genba.ErrNoPrincipal
	}
	if limit <= 0 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, genba.ErrClosed
	}

	keys := store.PrincipalKeys(p)
	key := store.ReportKey(p)
	out := make([]store.Flagged, 0, limit)
	for id, said := range s.reports {
		if len(said) == 0 {
			continue
		}
		d, ok := s.docs[id]
		if !ok || !visible(p, d) || !mine(keys, d) {
			continue
		}
		out = append(out, store.Flagged{Document: d, Stale: gather(id, said, key)})
	}
	// Most recently reported first, and the id after it so that two reports made
	// in the same instant come back in the same order twice. A list that shuffles
	// under a reader is a list nobody trusts they have read.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Stale.Last, out[j].Stale.Last
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		return out[i].Document.ID < out[j].Document.ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// gather turns what several people said into the one summary a screen draws.
//
// The key is the asking principal's, so that the summary can say whether one of
// those people is them. That is a lookup here and a comparison the caller could
// not make: what it carries is the most recent report, and the person asking is
// usually not the most recent person to have complained.
func gather(id string, said map[string]store.Report, key string) store.Staleness {
	out := store.Staleness{Doc: id, Count: len(said)}
	for _, r := range said {
		if r.At.After(out.Last.At) {
			out.Last = r
		}
	}
	if key != "" {
		_, out.Mine = said[key]
	}
	return out
}

// mine reports whether this document is one the principal owns or wrote.
//
// By name rather than by role, which is what [store.Reporter] says: an
// administrator may clear any of these and has no reason to be handed the whole
// corpus as their own work.
func mine(keys []string, d doc.Document) bool {
	for _, who := range []doc.Person{d.Owner, d.Author} {
		for _, k := range store.PersonKeys(who) {
			if slices.Contains(keys, k) {
				return true
			}
		}
	}
	return false
}
