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
package connector

import (
	"context"
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

	// Cursor is the point to resume from once this change has been durably
	// stored. It is opaque above the connector that produced it.
	Cursor Cursor
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
