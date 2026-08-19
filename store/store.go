// Package store defines the storage interface every driver implements and the
// small set of types that cross it.
//
// The interface is deliberately narrow. A driver stores documents, hands them
// back to a principal who may read them, and reports what it holds. Ranking,
// query parsing and the knowledge graph live above it, so a new driver is a
// weekend of work rather than a rewrite of the product.
//
// The one rule a driver may not bend: the principal is applied while the driver
// walks its own data, not by the caller afterwards. A driver that returns
// documents and expects the caller to filter them is not conformant, and the
// conformance suite in store/storetest will say so.
package store

import (
	"context"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
)

// Store is a tenant's document storage.
//
// Implementations must be safe for concurrent use.
type Store interface {
	// Put inserts or replaces documents. A document whose permissions did not
	// resolve is accepted and held, but never returned to a query: see
	// [Stats.Quarantined].
	Put(ctx context.Context, docs ...doc.Document) error

	// Delete removes documents by id. Deleting an id that is not present is not
	// an error, because a deletion sweep should be able to run twice.
	Delete(ctx context.Context, ids ...string) error

	// Get returns one document if the principal may read it.
	//
	// It returns an error matching genba.ErrNotFound both when the document
	// does not exist and when the principal may not see it. The two cases are
	// not distinguishable on purpose: a caller who can tell them apart can use
	// the difference to prove that a document exists.
	Get(ctx context.Context, p *acl.Principal, id string) (doc.Document, error)

	// Scan calls fn for every document the principal may read, and stops early
	// if fn returns false. The order is unspecified.
	Scan(ctx context.Context, p *acl.Principal, fn func(doc.Document) bool) error

	// Stats reports what the store holds.
	Stats(ctx context.Context) (Stats, error)

	// Close releases the store's resources.
	Close() error
}

// ContentStore is a store that can also hold the bytes of a document.
//
// It is an optional capability rather than part of [Store], because a driver
// over a search service that only keeps an inverted index has nowhere to put a
// megabyte of PNG, and refusing to implement it is a better answer than
// implementing it badly. A caller checks for it with a type assertion and
// serves nothing where it is absent.
//
// The rule the rest of the interface follows is not relaxed here. The principal
// is applied while the driver walks its own data, and a caller who may not read
// the document gets the same not found the document itself would produce.
//
// A driver that implements this takes [doc.Content] on Put and stores it beside
// the document. Get and Scan must not return it: a scan that carried image
// bytes would make every query pay for them.
type ContentStore interface {
	Store

	// Content returns the bytes of one document if the principal may read it.
	//
	// It returns an error matching genba.ErrNotFound when the document does not
	// exist, when the principal may not see it, and when it exists but holds no
	// content. All three are the same answer for the same reason: a caller who
	// can tell them apart can use the difference to prove a document exists.
	Content(ctx context.Context, p *acl.Principal, id string) (doc.Content, error)
}

// Stats is a snapshot of a store's contents.
type Stats struct {
	// Documents is the number of documents that can be served to a query.
	Documents int

	// Quarantined is the number of documents held back because their
	// permissions did not resolve. It is a number an operator should watch: it
	// is normal for it to be small and briefly non zero during a crawl, and it
	// is a bug when it grows.
	Quarantined int
}
