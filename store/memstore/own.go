package memstore

import (
	"context"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

var _ store.Ownership = (*Store)(nil)

// SetOwner records a correction and writes the new owner into the document.
func (s *Store) SetOwner(ctx context.Context, p *acl.Principal, c store.Correction) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	if err := c.Check(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return genba.ErrClosed
	}
	// Same rule as recording an open: a document this principal cannot see is a
	// write that does not happen rather than an error naming the id.
	d, ok := s.docs[c.Doc]
	if !ok || !visible(p, d) {
		return nil
	}
	// The source's answer is whatever the document said before the first
	// correction, not before this one. Correcting a correction must still leave
	// the connector's answer to go back to.
	if old, ok := s.owners[c.Doc]; ok {
		c.Was = old.Was
	} else {
		c.Was = d.Owner
	}
	s.owners[c.Doc] = c
	d.Owner = c.Owner
	s.docs[c.Doc] = d
	return nil
}

// ClearOwner drops the correction and puts the source's answer back.
func (s *Store) ClearOwner(ctx context.Context, p *acl.Principal, id string) error {
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
	c, ok := s.owners[id]
	if !ok {
		return nil
	}
	delete(s.owners, id)
	d.Owner = c.Was
	s.docs[id] = d
	return nil
}

// Corrections returns the corrections on the documents the principal may read.
func (s *Store) Corrections(ctx context.Context, p *acl.Principal, ids []string) (map[string]store.Correction, error) {
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

	var out map[string]store.Correction
	for _, id := range ids {
		c, ok := s.owners[id]
		if !ok {
			continue
		}
		d, ok := s.docs[id]
		if !ok || !visible(p, d) {
			continue
		}
		if out == nil {
			out = make(map[string]store.Correction, len(ids))
		}
		out[id] = c
	}
	return out, nil
}

// correct applies a standing correction to a document on its way in, and
// refreshes the source's answer from what the crawl just reported.
//
// It runs under the write lock, from put, which is the contract [store.Ownership]
// states: the correction outlives the crawl that would otherwise undo it.
func (s *Store) correct(d doc.Document) doc.Document {
	c, ok := s.owners[d.ID]
	if !ok {
		return d
	}
	c.Was = d.Owner
	s.owners[d.ID] = c
	d.Owner = c.Owner
	return d
}
