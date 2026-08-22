package memstore

import (
	"context"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

var _ store.Verifier = (*Store)(nil)

// Verify records that somebody vouches for a document.
func (s *Store) Verify(ctx context.Context, p *acl.Principal, v store.Verification) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	if err := v.Check(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return genba.ErrClosed
	}
	// Same rule as recording an open: a document this principal cannot see is a
	// write that does not happen rather than an error naming the id.
	d, ok := s.docs[v.Doc]
	if !ok || !visible(p, d) {
		return nil
	}
	s.verified[v.Doc] = v
	return nil
}

// Unverify withdraws the claim.
func (s *Store) Unverify(ctx context.Context, p *acl.Principal, id string) error {
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
	delete(s.verified, id)
	return nil
}

// Verifications returns the claims on the documents the principal may read.
func (s *Store) Verifications(ctx context.Context, p *acl.Principal, ids []string) (map[string]store.Verification, error) {
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

	var out map[string]store.Verification
	for _, id := range ids {
		v, ok := s.verified[id]
		if !ok {
			continue
		}
		d, ok := s.docs[id]
		if !ok || !visible(p, d) {
			continue
		}
		if out == nil {
			out = make(map[string]store.Verification, len(ids))
		}
		out[id] = v
	}
	return out, nil
}
