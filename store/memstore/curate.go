package memstore

import (
	"cmp"
	"context"
	"slices"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

// An answer is the one thing this driver keeps that is not about a document, so
// it is the one thing here keyed by tenant rather than reached through one. The
// tenant comes off the principal on every path below, and it is the whole of
// the permission rule: everybody who can search a tenant can read its answers,
// and the documents an answer cites are resolved by whoever draws it.

// Curate writes an answer, replacing any earlier one with the same id.
func (s *Store) Curate(ctx context.Context, p *acl.Principal, a store.Answer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	if err := a.Check(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return genba.ErrClosed
	}

	// The phrasings this answer used to claim go before the ones it claims now
	// are written, or an edit that drops a variant leaves the old variant
	// pointing at an answer that no longer says it.
	s.forget(p.Tenant, a.ID)
	s.answers[[2]string{p.Tenant, a.ID}] = a
	for _, key := range a.Keys() {
		s.phrasings[[2]string{p.Tenant, key}] = a.ID
	}
	return nil
}

// Retract removes an answer.
func (s *Store) Retract(ctx context.Context, p *acl.Principal, id string) error {
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
	s.forget(p.Tenant, id)
	delete(s.answers, [2]string{p.Tenant, id})
	return nil
}

// forget drops every phrasing pointing at an answer. The caller holds the lock.
//
// It walks the phrasings rather than reading the answer's own keys, because the
// keys that have to go are the ones that were written, and an answer edited by
// a build that folded a question differently would leave one behind.
func (s *Store) forget(tenant, id string) {
	for key, at := range s.phrasings {
		if key[0] == tenant && at == id {
			delete(s.phrasings, key)
		}
	}
}

// Curated returns the answer to a question.
func (s *Store) Curated(ctx context.Context, p *acl.Principal, question string) (store.Answer, error) {
	if err := ctx.Err(); err != nil {
		return store.Answer{}, err
	}
	if p == nil {
		return store.Answer{}, genba.ErrNoPrincipal
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return store.Answer{}, genba.ErrClosed
	}

	id, ok := s.phrasings[[2]string{p.Tenant, store.AnswerKey(question)}]
	if !ok {
		return store.Answer{}, genba.ErrNotFound
	}
	a, ok := s.answers[[2]string{p.Tenant, id}]
	if !ok {
		return store.Answer{}, genba.ErrNotFound
	}
	return a, nil
}

// Answers lists the answers in the tenant, most recently written first.
func (s *Store) Answers(ctx context.Context, p *acl.Principal, limit int) ([]store.Answer, error) {
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

	var out []store.Answer
	for key, a := range s.answers {
		if key[0] == p.Tenant {
			out = append(out, a)
		}
	}
	// The id breaks the tie, because two answers written in the same nanosecond
	// are possible on a test clock and a list that puts them in a different
	// order on every call is a screen that changes when nothing did.
	slices.SortFunc(out, func(x, y store.Answer) int {
		if !x.At.Equal(y.At) {
			return y.At.Compare(x.At)
		}
		return cmp.Compare(x.ID, y.ID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
