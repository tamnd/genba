// Package connector is the contract between a source system and the index.
//
// Every source is different and none of that difference should reach the index.
// A connector's whole job is to turn whatever its system calls a page, a
// message, a ticket or a file into a [doc.Document], and to say who is allowed
// to read it. What it does behind that line is its own business.
//
// # Permissions are not optional
//
// A connector that cannot report permissions does not ship. Indexing content
// without the access control list that governs it is the one mistake this
// project cannot recover from, because the content is then searchable by
// everybody and no later fix makes it unsearchable by the people who already
// found it.
//
// The type system cannot enforce that, so the pipeline does. A document whose
// [acl.Permissions] did not resolve arrives with [acl.ModeUnknown], and
// [doc.Document.Queryable] is false for it, which keeps it out of every query
// path in the system. A connector that does not know the answer says so with
// [Unresolved] and the document is quarantined. A connector that guesses is a
// bug.
//
// # Resuming
//
// A sync of a real corpus takes long enough that it will be interrupted. Every
// [Change] carries the [Cursor] to resume from if the process dies just after
// that change was durably stored, so a run that is killed picks up where it
// stopped rather than starting again. What goes in a cursor is entirely the
// connector's business, and nothing above it may parse it.
//
// # The incremental path is not enough on its own
//
// [Connector] is the whole of the required interface. Everything else here is
// optional, and every one of the optional pieces exists because an index built
// only out of a change feed drifts away from its source and nothing in the feed
// ever says so. Events get dropped, a bulk edit at the source raises none, a
// permission change raises one that carries no content, and a process killed
// between a store and a checkpoint replays or skips.
//
// So a connector that can do better says so by implementing more:
// [Enumerator] to list what the source holds, [Fetcher] to fetch one document
// by id, [Counted] to report what it spent, and [Change.PermissionsOnly] to
// report an access control change without the content that did not change.
// A connector that implements none of them still works, and what it gives up
// is the ability to be checked.
package connector

import (
	"context"
	"errors"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
)

// Connector pulls documents out of one source system.
//
// Implementations are not required to be safe for concurrent use. The pipeline
// drives one connector from one goroutine.
type Connector interface {
	// Source is the stable name of this source, which lands in
	// [doc.Document.Source] and in the cursor the pipeline stores. It has to
	// stay the same across restarts and across versions of the connector,
	// because it is the key a resume point is filed under.
	Source() string

	// Sync walks the source from the given cursor and calls emit for every
	// change it finds, in an order where every change is safe to resume after.
	//
	// A zero cursor means a full sync. Sync returns the cursor to resume from
	// if the walk completed, which is normally the cursor of the last change it
	// emitted.
	//
	// Backpressure is structural: emit does the indexing work and Sync is
	// blocked while it runs, so a source that produces faster than the index
	// can absorb is slowed by the act of handing anything over. There is no
	// queue between them to grow without bound.
	//
	// An error from emit is returned to the caller unchanged and stops the
	// walk. Sync must not swallow it, and must not keep emitting after it.
	Sync(ctx context.Context, from Cursor, emit func(context.Context, Change) error) (Cursor, error)

	// Close releases whatever the connector holds open. It is safe to call more
	// than once.
	Close() error
}

// Change is one document that appeared, changed or went away.
type Change struct {
	// Document is the normalised content. For a deletion only ID and Tenant are
	// required, because there is nothing left to index.
	Document doc.Document

	// Deleted says the document is gone from the source and should be removed
	// from the index rather than stored.
	Deleted bool

	// PermissionsOnly says the only thing that changed is who may read the
	// document, and that the rest of Document is not filled in.
	//
	// This is the difference between a permission change costing a write and
	// costing a recrawl. Access control at a real source does not live on the
	// document: it lives on the folder, the space, the group or the OWNERS file
	// above it, and one edit there governs thousands of documents whose content
	// nobody touched. A connector that noticed such an edit knows the new access
	// control list without fetching anything, and this is how it says so.
	//
	// Only ID and Permissions are read. Everything else in Document is ignored,
	// so a connector must not use this to sneak a content update through: the
	// content will not be stored.
	PermissionsOnly bool

	// Cursor is the point to resume from once this change has been durably
	// stored. It is opaque above the connector that produced it.
	Cursor Cursor
}

// Item is one document as an enumeration sees it: a name and a version, and
// deliberately nothing else.
//
// Listing a source is a different operation from reading it, and at most
// sources it is a different price. This type is what makes that difference
// usable: a walk that returns ids and revisions can compare a whole corpus
// against the index without fetching a single document.
type Item struct {
	// ID is the document id, the same one [Change] would carry.
	ID string

	// Version is the source's own idea of a revision, which lands in
	// [doc.Document.SourceUpdate] when the document is stored. It is opaque
	// above the connector: the only thing done with it is comparing it to the
	// version already stored, and a difference means the index is behind.
	//
	// An empty version means the source does not have one. That is allowed and
	// it costs something: a document with no version can be found to be missing
	// or extra, but never stale.
	Version string
}

// Enumerator is the optional capability of a connector that can list what its
// source holds without reading it.
//
// It is what reconciliation is built on. An incremental sync is a stream of
// changes and it is only ever as good as the change feed underneath it: feeds
// drop events, sources forget to raise them for a bulk edit, and a process
// killed at the wrong moment resumes past something. None of those are visible
// from inside the incremental path, which is exactly why the sweep that catches
// them cannot be built out of it.
//
// A connector that cannot enumerate cheaply should not implement this. A sweep
// that costs a full recrawl is a full recrawl, and an operator is better served
// by being told the source has no enumeration than by a maintenance job that
// quietly reads the corpus every night.
type Enumerator interface {
	Connector

	// Enumerate calls fn for every document the source currently holds, and
	// stops early if fn returns false. The order is unspecified.
	//
	// It must be complete or it must fail. A walk that silently returns part of
	// the corpus reads, to everything above it, as a corpus that lost the rest,
	// and the repair for that is deletion.
	Enumerate(ctx context.Context, fn func(Item) bool) error
}

// Fetcher is the optional capability of a connector that can read one document
// by id, out of the order a sync would have found it in.
//
// Reconciliation needs it to repair what it finds. Without it a sweep can still
// report that the index is missing a document and can still delete one the
// source no longer has, but it cannot fill the hole, and the only way to get
// the document back is a full sync.
type Fetcher interface {
	Connector

	// Fetch returns one document by id, with its permissions resolved the same
	// way a sync would resolve them.
	//
	// It returns [ErrGone] if the source no longer has the document, which is
	// not an error condition: it is the answer, and it is how a repair learns
	// that what it was about to refetch should be deleted instead.
	Fetch(ctx context.Context, id string) (doc.Document, error)
}

// ErrGone is returned by [Fetcher.Fetch] for a document the source no longer
// has.
var ErrGone = errors.New("connector: the source no longer has this document")

// Counters is what a connector spent talking to its source.
//
// They exist because the claim that an incremental sync is cheap is not
// checkable from the outside. A second run over an unchanged corpus takes
// milliseconds either way on a local disk, and the same connector against a
// real API with the same bug costs a hundred thousand requests and a rate
// limit. Time measures the machine. These measure the work.
//
// The split is by price rather than by protocol, because that is what a
// connector's caller is deciding about.
type Counters struct {
	// Lists is enumerations: a directory read, a page of a change feed, a
	// search that returns identifiers.
	Lists int64

	// Metadata is per document lookups that do not return content, which is
	// what an incremental sync spends on documents it decides to skip.
	Metadata int64

	// Fetches is full reads of a document. This is the expensive one and the
	// one a second run over an unchanged corpus should leave at zero.
	Fetches int64

	// Bytes is how much content came back.
	Bytes int64
}

// Requests is the total number of calls made to the source.
func (c Counters) Requests() int64 { return c.Lists + c.Metadata + c.Fetches }

// Since returns what has been spent since an earlier reading.
//
// A connector counts for its own lifetime rather than per run, because it does
// not know where a run begins. A caller that wants one run takes a reading
// before and subtracts it.
func (c Counters) Since(earlier Counters) Counters {
	return Counters{
		Lists:    c.Lists - earlier.Lists,
		Metadata: c.Metadata - earlier.Metadata,
		Fetches:  c.Fetches - earlier.Fetches,
		Bytes:    c.Bytes - earlier.Bytes,
	}
}

// Counted is the optional capability of a connector that reports what it spent.
type Counted interface {
	// Counters returns the running totals for the life of the connector. It is
	// safe to call from another goroutine while a sync is running, which is the
	// point: a long sync is exactly when somebody wants to know.
	Counters() Counters
}

// Cursor is a source's own idea of where a sync got to.
//
// It is deliberately a string and a time rather than a structured type. Every
// source counts differently, some by revision, some by a change feed token and
// some by a modification timestamp, and a shared schema for that would be a
// shared schema for nothing.
type Cursor struct {
	// Value is whatever the connector needs to resume, and is meaningless to
	// everything else. An empty value means the beginning.
	Value string

	// Time is when the cursor was taken, for operators looking at how far
	// behind a source is. Nothing resumes from it.
	Time time.Time
}

// IsZero reports whether the cursor points at the beginning of the source.
func (c Cursor) IsZero() bool { return c.Value == "" }

// Unresolved returns the permission descriptor for a document whose access
// control list a connector could not work out.
//
// Use it rather than leaving the field zero. Both produce [acl.ModeUnknown] and
// both quarantine the document, but this one says at the call site that the
// connector considered the question and failed, which is a different bug from
// having forgotten to set the field at all.
func Unresolved(source string) acl.Permissions {
	return acl.Permissions{Mode: acl.ModeUnknown, Source: source}
}

// Checkpoints is where resume points are kept between runs.
//
// It is separate from [store.Store] because a checkpoint is operational state
// rather than content: it has no tenant visible existence, no permissions and
// no reason to be searchable. A deployment that keeps documents in PostgreSQL
// can keep checkpoints in a file, and often should.
type Checkpoints interface {
	// Load returns the resume point for a source, or the zero cursor if the
	// source has never been synced. A source that has never run is not an
	// error.
	Load(ctx context.Context, tenant, source string) (Cursor, error)

	// Save records a resume point. It must be atomic against a crash: a reader
	// after an interrupted Save sees either the old cursor or the new one, and
	// never a torn value. Replaying a little is always recoverable, and
	// resuming past unindexed documents is not.
	Save(ctx context.Context, tenant, source string, c Cursor) error
}
