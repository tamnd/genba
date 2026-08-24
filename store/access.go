package store

import (
	"context"

	"github.com/tamnd/genba/acl"
)

// Reach is how much of the corpus a principal can read, by source and by kind.
//
// It is counts and nothing else on purpose. One of the questions it answers is
// whether somebody's access is roughly the shape it should be, and a list of
// titles would answer that question by handing an operator the documents
// themselves, which is the one thing the administrator role does not grant.
// Counts answer it without becoming a way to read a corpus through somebody
// else's eyes.
//
// The other question it answers is the filter rail, which is the same question
// asked by a different screen: how many documents are there of each source and
// of each kind, for the person looking. That used to be a search with no query
// in it, whose facet counts stop at a bound, so a rail over a corpus of twenty
// thousand documents added up to a thousand. See #142.
//
// Both fields are counted exactly rather than sampled. A number that says "at
// least four hundred" cannot be compared with the same number taken last week,
// and comparing it with last week is the whole use of it on one screen, while
// on the other a rail whose every row says the same bound has no proportions in
// it and proportions are what a rail is for.
type Reach struct {
	// Sources is how many documents of each connector this principal may read.
	// The value is the same name a search filters on.
	Sources []Facet

	// Kinds is the same count over the document type.
	Kinds []Facet
}

// Access is the capability that answers what one principal can reach.
//
// It is optional like every capability outside [Store], because a driver over a
// service that ranks for itself may have no way to count a match set it did not
// produce. A caller checks for it and says the driver cannot answer, which is a
// better screen than one that walks the corpus in the request and calls it a
// count.
//
// It is a capability rather than a use of [Store.Scan] because Scan hands back
// every document the principal may read, and counting them means decoding a
// whole corpus to reach a handful of numbers that a database can produce as one
// aggregate.
//
// Be plain about what it still costs. The aggregate is over every document in
// the tenant, and on the twenty thousand document benchmark corpus it takes
// tens of milliseconds, which is outside what an interactive request is allowed
// to spend. Two things follow from that and both are deliberate. The screen an
// operator opens asks for it rather than getting it with everything else, and
// the filter rail reads it through a cache with an expiry of its own rather
// than through the one a search uses. See [index.Cache] for why the rail may be
// a minute out of date and a search may not. #149 is what would have to change
// for neither to be necessary.
type Access interface {
	Store

	// Reachable reports how many documents the principal may read, of each
	// source and of each kind.
	//
	// The permission rule is the same one every other read path applies and it
	// is applied the same way, inside the driver's own query. A driver that
	// counts everything and filters afterwards is not conformant, and the
	// suite in store/storetest will say so.
	//
	// Values the principal can reach nothing under are left out rather than
	// returned as zero, because a source that has nothing in it for somebody
	// and a source that does not exist are the same answer to the person
	// looking at the screen.
	//
	// The order of each list is unspecified. A caller drawing a list sorts it
	// into whatever order that list is read in.
	Reachable(ctx context.Context, p *acl.Principal) (Reach, error)
}
