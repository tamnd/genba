package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Feed is one connector's configuration, written down so it survives a restart.
//
// It is deliberately not a connector and it is not a running thing. It is what
// somebody typed: which source to read, under whose name, how often, and
// whether it should be running at all. Something else turns that into a
// connector and keeps it going, and that something else is in the process that
// owns the crawlers rather than in here.
//
// It lives in the store because the store is the only thing in this system that
// survives a restart and is shared by every copy of the process. A file next to
// the database would work on a laptop and would quietly give three servers
// behind a load balancer three different sets of connectors.
//
// What is not in it is credentials. A bucket needs a key and a secret and
// neither of them goes in here, because a database is backed up, replicated and
// read by more people than a process environment is, and a secret that reaches
// one of those places cannot be recalled from it. They are read from the
// environment exactly as they were before this type existed, and a connector
// added from the interface uses the credentials the process already holds.
type Feed struct {
	// Tenant is whose corpus this feeds, and Source is the name its documents
	// carry. Together they are the key: two tenants may both have a source
	// called files, and one tenant may not have two.
	Tenant string
	Source string

	// Kind says what sort of source it is, and is what decides how Config is
	// read. It is a string rather than a constant of this package because the
	// set of connectors is not the store's business.
	Kind string

	// Enabled says whether it should be running. A feed that is switched off is
	// still configured, which is the difference between stopping a connector
	// that is producing errors and losing how it was set up.
	Enabled bool

	// Config is the kind specific settings, as JSON. It is opaque here on
	// purpose: a directory has a path and an access control policy, a bucket
	// has an endpoint and a region, and a store that knew the difference would
	// need a migration every time a connector grew a field.
	Config json.RawMessage

	// By is the subject that last wrote this row, which is the question that
	// gets asked when two operators are changing the same deployment. It is
	// recorded rather than derived, because the audit log has the action and
	// this has the current state, and reconstructing one from the other means
	// reading every line since the process came up.
	By string

	// Created and Updated are set by the driver, not by the caller. A row's
	// creation time does not move when it is edited, which is what makes it
	// possible to tell a connector that was added this morning from one that
	// has been failing since it was set up last year.
	Created time.Time
	Updated time.Time
}

// ErrNoFeedSource and ErrNoFeedKind are what [Feed.Check] returns.
var (
	ErrNoFeedSource = errors.New("feed has no source")
	ErrNoFeedKind   = errors.New("feed has no kind")
)

// Check rejects a feed that cannot be stored.
//
// It lives here rather than in each driver because every driver keys this on
// the same two fields, and a source with no name would be a row that can be
// written and never addressed again. A driver calls it and wraps the result
// with its own name.
func (f Feed) Check() error {
	switch {
	case f.Source == "":
		return ErrNoFeedSource
	case f.Kind == "":
		return ErrNoFeedKind
	default:
		return nil
	}
}

// Feeds is the capability that remembers how the connectors were configured.
//
// It is optional like every capability outside [Store], and a deployment whose
// driver does not implement it is not broken: it configures its connectors on
// the command line the way every deployment did before this existed, and the
// interface says so rather than offering a form whose answers go nowhere.
//
// It is not principal scoped. A connector is a property of the deployment
// rather than of a document, nothing in it is readable by an ordinary caller,
// and the one role that reaches it is checked in front of the store. That is
// the same bargain [Maintenance] and [Quarantine] make.
type Feeds interface {
	Store

	// Feeds returns every configured feed for one tenant, in an unspecified
	// order. A caller drawing a list sorts it into whatever order that list is
	// read in.
	Feeds(ctx context.Context, tenant string) ([]Feed, error)

	// SaveFeed inserts or replaces one feed, keyed by tenant and source.
	//
	// Replacing rather than failing on a duplicate is what makes editing one
	// field the same call as adding it, and it is what lets a deployment
	// describe its connectors declaratively later without a read first.
	// Created is preserved across a replace and Updated is set to now.
	SaveFeed(ctx context.Context, f Feed) error

	// DropFeed forgets one feed. Dropping one that is not there is not an
	// error, so that removing a connector twice is the same as removing it
	// once.
	//
	// It removes the configuration and nothing else. The documents that feed
	// indexed stay exactly where they are, because forgetting how a corpus was
	// read is not a decision to delete the corpus, and a call that did both
	// would make an operator's undo cost a full crawl.
	DropFeed(ctx context.Context, tenant, source string) error
}
