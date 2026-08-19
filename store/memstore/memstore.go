// Package memstore is an in memory implementation of store.Store.
//
// It is the reference driver. It is what the conformance suite is written
// against, what the tests of every package above storage run on, and what
// genbad uses when it is started without a data directory. It is not meant to
// hold a company's corpus.
package memstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// Store keeps documents in a map guarded by a read write mutex.
type Store struct {
	mu   sync.RWMutex
	docs map[string]doc.Document

	// content is kept out of the document for the same reason the SQLite driver
	// keeps it in its own table: a scan that carried image bytes would make
	// every query pay for them.
	content map[string]doc.Content
	closed  bool
}

// New returns an empty store.
func New() *Store {
	return &Store{
		docs:    make(map[string]doc.Document),
		content: make(map[string]doc.Content),
	}
}

var (
	_ store.ContentStore = (*Store)(nil)
	_ store.Statistician = (*Store)(nil)
)

// Put inserts or replaces documents.
func (s *Store) Put(ctx context.Context, docs ...doc.Document) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return genba.ErrClosed
	}
	for _, d := range docs {
		if d.ID == "" {
			return fmt.Errorf("memstore: put: %w", errNoID)
		}
		if d.Content != nil {
			s.content[d.ID] = *d.Content
		} else {
			delete(s.content, d.ID)
		}
		d.Content = nil
		s.docs[d.ID] = d
	}
	return nil
}

// Delete removes documents by id.
func (s *Store) Delete(ctx context.Context, ids ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return genba.ErrClosed
	}
	for _, id := range ids {
		delete(s.docs, id)
		delete(s.content, id)
	}
	return nil
}

// Get returns one document if the principal may read it.
func (s *Store) Get(ctx context.Context, p *acl.Principal, id string) (doc.Document, error) {
	if err := ctx.Err(); err != nil {
		return doc.Document{}, err
	}
	if p == nil {
		return doc.Document{}, genba.ErrNoPrincipal
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return doc.Document{}, genba.ErrClosed
	}
	d, ok := s.docs[id]
	if !ok || !visible(p, d) {
		// The id is deliberately left out of the message. A caller who can see
		// which of the two happened can use it to prove that a document exists.
		return doc.Document{}, genba.ErrNotFound
	}
	return d, nil
}

// Scan calls fn for every document the principal may read.
//
// Documents are visited in id order. The interface does not promise an order,
// but a deterministic one makes failures reproducible, and at this size it is
// free.
func (s *Store) Scan(ctx context.Context, p *acl.Principal, fn func(doc.Document) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return genba.ErrNoPrincipal
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return genba.ErrClosed
	}
	for _, id := range slices.Sorted(maps.Keys(s.docs)) {
		d := s.docs[id]
		if !visible(p, d) {
			continue
		}
		if !fn(d) {
			return nil
		}
	}
	return nil
}

// Content returns the bytes of one document if the principal may read it.
func (s *Store) Content(ctx context.Context, p *acl.Principal, id string) (doc.Content, error) {
	if err := ctx.Err(); err != nil {
		return doc.Content{}, err
	}
	if p == nil {
		return doc.Content{}, genba.ErrNoPrincipal
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return doc.Content{}, genba.ErrClosed
	}
	d, ok := s.docs[id]
	if !ok || !visible(p, d) {
		return doc.Content{}, genba.ErrNotFound
	}
	c, ok := s.content[id]
	if !ok {
		// A document with no content is not distinguishable from one that is not
		// there, because telling the two apart is a way of asking whether a
		// document exists.
		return doc.Content{}, genba.ErrNotFound
	}
	return c, nil
}

// Statistics counts the corpus by walking it.
//
// This driver keeps no derived numbers, so it recomputes them, which is O(n) on
// every call and is the correct thing for a reference implementation to do. It
// is what the driver that maintains them incrementally is checked against, and
// the whole value of a reference is that it is obviously right rather than
// fast.
func (s *Store) Statistics(ctx context.Context, p *acl.Principal, terms []string) (store.Corpus, error) {
	if err := ctx.Err(); err != nil {
		return store.Corpus{}, err
	}
	if p == nil {
		return store.Corpus{}, genba.ErrNoPrincipal
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return store.Corpus{}, genba.ErrClosed
	}

	want := make(map[string]bool, len(terms))
	for _, t := range terms {
		want[t] = true
	}

	c := store.Corpus{DocFreq: make(map[string]int, len(terms))}
	for _, d := range s.docs {
		// The tenant, and not the principal's visibility. See [store.Corpus] for
		// why the counts are over what the tenant holds.
		if !d.Queryable() || d.Tenant != p.Tenant {
			continue
		}
		a := d.Analyze()
		c.Documents++
		c.TitleTokens += int64(a.TitleTokens)
		c.BodyTokens += int64(a.BodyTokens)
		for t := range a.Terms {
			if want[t] {
				c.DocFreq[t]++
			}
		}
	}
	return c, nil
}

// Stats reports what the store holds.
func (s *Store) Stats(ctx context.Context) (store.Stats, error) {
	if err := ctx.Err(); err != nil {
		return store.Stats{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return store.Stats{}, genba.ErrClosed
	}
	var st store.Stats
	for _, d := range s.docs {
		if d.Queryable() {
			st.Documents++
		} else {
			st.Quarantined++
		}
	}
	return st, nil
}

// Close releases the store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.docs = nil
	s.content = nil
	return nil
}

// visible is the whole permission decision for this driver. It runs inside the
// read lock, on the driver's own data, before the caller ever sees a document.
func visible(p *acl.Principal, d doc.Document) bool {
	if !d.Queryable() || d.Tenant != p.Tenant {
		return false
	}
	return d.Permissions.Allows(p)
}

var errNoID = errors.New("document has no id")
