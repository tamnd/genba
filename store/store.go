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

// Counters is the work one query cost, counted exactly.
//
// A driver that keeps these is a driver whose performance can be asserted on,
// because a latency assertion on a shared CI runner is a coin flip and these
// are not. Rows read, statements issued, documents decoded and candidates
// scored do not vary with how busy the machine is, they are where a regression
// shows up first, and an assertion on them names the mistake precisely: a per
// hit refetch reappearing in a year moves Decodes from twenty to twenty plus
// the match set, on any runner, at any speed.
//
// They are cumulative for the life of the store rather than per query, so a
// caller measuring one query resets them first and a caller publishing them as
// metrics does not reset them at all.
type Counters struct {
	// Rows is the rows the database handed back. It is what the test that
	// proves the permission filter is in the query itself asserts on: a reader
	// who may see nothing has to cost zero rows, not a full walk that the
	// driver then discards.
	Rows int64

	// Statements is the statements executed, read paths only. A search that
	// issues one per result rather than one per page is the regression this
	// counts.
	Statements int64

	// Decodes is stored documents decoded into a [doc.Document]. A search
	// should decode the page and nothing else.
	Decodes int64

	// Candidates is the documents handed to the ranker. It is bounded by the
	// candidate pool rather than by the match set, which is the whole point of
	// two phase retrieval.
	Candidates int64

	// Faceted is the documents the facet counts were counted over. It is
	// bounded by [Selection.Facets] rather than by the match set.
	//
	// It counts rows the database read on the driver's behalf rather than rows
	// it handed back, which is the one place the two have to be told apart. An
	// aggregate returns the same handful of rows whether it counted fifty
	// documents or fifty thousand, so Rows cannot see the difference and the
	// regression this exists to catch is invisible in it: a facet count that
	// goes back to walking the whole match set costs a second on a common term
	// and looks free in every other counter here.
	Faceted int64
}

// Counted is a store that reports the work it has done.
//
// It is optional because a driver is not obliged to be measurable, and the
// layers above check for it rather than requiring it. The interface is here
// rather than in the driver so that anything above the storage layer can read
// the numbers without importing a driver, which the layering forbids for good
// reasons.
type Counted interface {
	// Counters returns the totals since the store was opened or last reset.
	Counters() Counters

	// ResetCounters zeroes them, so that a measurement can be scoped to one
	// query.
	ResetCounters()
}
