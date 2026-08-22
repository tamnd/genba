package store

import (
	"context"

	"github.com/tamnd/genba/acl"
)

// Reach is how much of one source a principal can read.
//
// It is a count and nothing else on purpose. The question an operator opens
// this for is whether somebody's access is roughly the shape it should be, and
// a list of titles would answer that question by handing the operator the
// documents themselves, which is the one thing the administrator role does not
// grant. Counts answer it without becoming a way to read a corpus through
// somebody else's eyes.
type Reach struct {
	// Source is the connector the documents came from, which is the same name a
	// search filters on.
	Source string

	// Documents is how many of that source's documents this principal may read,
	// counted exactly rather than sampled. A number that says "at least four
	// hundred" cannot be compared with the same number taken last week, and
	// comparing it with last week is the whole use of it.
	Documents int
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
// to spend here. That is why the interface asks for it rather than showing it
// with everything else, and #149 is what would have to change for it not to
// have to.
type Access interface {
	Store

	// Reachable reports, per source, how many documents the principal may read.
	//
	// The permission rule is the same one every other read path applies and it
	// is applied the same way, inside the driver's own query. A driver that
	// counts everything and filters afterwards is not conformant, and the
	// suite in store/storetest will say so.
	//
	// Sources the principal can reach nothing in are left out rather than
	// returned as zero, because a source that has nothing in it for somebody
	// and a source that does not exist are the same answer to the person
	// looking at the screen.
	//
	// The order is unspecified. A caller drawing a list sorts it into whatever
	// order that list is read in.
	Reachable(ctx context.Context, p *acl.Principal) ([]Reach, error)
}
