package store

import (
	"context"
	"time"
)

// Held is one document the index is holding back.
//
// A quarantined document is the one thing in this system that is invisible from
// every angle. It is not in a result, not in a facet count, not in a suggestion
// and not in the statistics a search reports, which is exactly right and is also
// why the only trace of it anywhere is a single number on the stats endpoint.
// Somebody who has been told there are fourteen hundred of them has been told
// nothing they can act on.
//
// So this is the small amount of a held document that an operator needs to act:
// which one it is, where it came from, and why it did not resolve. It is not
// content and it does not become content. The body, the extracted text, the
// author, the access control lists and everything else stay where they are.
type Held struct {
	// ID is the document id, which is what an operator quotes in a bug against
	// whichever connector produced it.
	ID string

	// Title is what the source called it. It is the one piece of the document
	// itself that crosses this interface, and it does so because a list of
	// opaque ids is a list nobody can look at and recognise the pattern in,
	// which is the whole task: forty held documents that turn out to all be in
	// one folder is an answer, and forty ids is not.
	Title string

	// Source is the connector it came from.
	Source string

	// Reason is [acl.Permissions.Reason], which is whatever gave up on it
	// saying so in its own words. It is empty for a document quarantined by
	// something that did not record one, and a screen showing it should say
	// that plainly rather than inventing a reason.
	Reason string

	// At is when the source last changed the document, and is zero where the
	// source never said. It is here so that a list can be read newest first,
	// which is how somebody watching a connector they have just fixed sees
	// whether it is still producing them.
	At time.Time
}

// Quarantine is the capability that lists what is being held back.
//
// It is optional, like every other capability outside [Store], because a driver
// over a service that only keeps an inverted index has no way to walk the
// documents it is not indexing. A caller checks for it and says the driver
// cannot answer, which is a better screen than one that pretends the quarantine
// is empty.
//
// It is not principal scoped and it cannot be. A held document is one whose
// permissions did not resolve, so there is no principal it could be scoped to:
// the question "who may see this" is the question that failed. That is the same
// bargain [Maintenance] makes and it is paid for the same way, by handing back
// the least that answers the question. See [Held].
type Quarantine interface {
	// Quarantined returns at most limit documents that are being held back for
	// one tenant. The order is unspecified and a limit of zero or less returns
	// nothing.
	//
	// The count is deliberately not here. [Stats.Quarantined] already has it,
	// every caller of this already asks for stats, and a second count taken a
	// moment later would let a screen print a list of a hundred under a total
	// of ninety nine.
	Quarantined(ctx context.Context, tenant string, limit int) ([]Held, error)
}
