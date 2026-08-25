package store

import (
	"context"

	"github.com/tamnd/genba/acl"
)

// Item is one stored document as the ingestion pipeline sees it: a name and a
// version, and deliberately nothing else.
//
// No title, no body, no permissions. A reconciliation compares two lists of
// names and dates, and anything else in this struct would be content leaving
// the store without a principal having asked for it.
type Item struct {
	// ID is the document id, the same one [Store.Delete] takes.
	ID string

	// Version is [doc.Document.SourceUpdate] as it was last stored, which is
	// the source's own idea of a revision. It is opaque here: the pipeline only
	// ever compares it to the version the connector reports now, and a
	// difference means the stored copy is behind.
	Version string

	// Held is whether the document is being held back because its permissions
	// did not resolve.
	//
	// It is here because a held document is the one kind of drift the version
	// comparison cannot see. The source and the index agree about the revision,
	// so nothing above would think to refetch it, and yet the reason it is held
	// is at the source: a directory that was down, a token that could not list
	// a channel, a group that has since been created. Without this the retry
	// would have to be a second pass over the corpus asking each document
	// whether it resolved, which is the read of the whole corpus this interface
	// exists to avoid.
	//
	// It says nothing about why. That is [Held.Reason], and it is on the
	// capability that is allowed to say a little about a document rather than
	// on the one that is only allowed to compare two lists.
	Held bool
}

// Maintenance is the capability the ingestion pipeline needs and no query path
// has.
//
// It is optional and separate from [Store] because it is the one part of the
// interface that is not principal scoped, and that deserves to be looked at
// squarely rather than folded in beside Get and Scan.
//
// The reason it cannot be principal scoped is that reconciliation asks "what
// does this source hold that the index does not, and the other way round", and
// a principal scoped answer would be one person's view of that. Deleting
// everything a crawler's principal cannot see would empty the index on the
// first run. The alternative is a principal that may see everything, which is a
// superuser, and a superuser is a far worse thing to have in a permission model
// than an interface that returns no content at all.
//
// So the bargain is written into the signatures. [Maintenance.Inventory] hands
// back ids and versions, which is what a comparison needs and nothing a reader
// could learn anything from. [Maintenance.SetPermissions] only writes.
type Maintenance interface {
	Store

	// Inventory calls fn for every document held for one tenant and source, and
	// stops early if fn returns false. The order is unspecified.
	//
	// Quarantined documents are included, with [Item.Held] set. A document
	// whose permissions did not resolve is still a document the source may
	// since have deleted, and leaving it out would make reconciliation the one
	// path that cannot clean up the very documents an operator most wants gone.
	Inventory(ctx context.Context, tenant, source string, fn func(Item) bool) error

	// SetPermissions replaces the access control list of documents that are
	// already stored, without touching anything else about them.
	//
	// This is what makes a permission change at the source cost a write per
	// affected document rather than a recrawl of their content. An id the store
	// does not hold is skipped rather than being an error, because the source
	// and the index are allowed to disagree about what exists and that is what
	// reconciliation is for. The return is how many were actually changed, so a
	// caller can tell a run that did nothing from a run that did.
	//
	// A document whose new permissions do not resolve becomes quarantined by
	// the same rule [Store.Put] applies, and one that resolves after being
	// quarantined becomes queryable again. Revocation has to be as fast as
	// granting, and it is the direction that matters.
	SetPermissions(ctx context.Context, tenant string, perms map[string]acl.Permissions) (int, error)
}
