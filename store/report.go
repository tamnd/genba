package store

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
)

// Report is a reader saying that a document is out of date.
//
// It is the other half of a verification and the cheaper half. Verifying costs
// the person who is accountable half an hour of reading; reporting costs the
// person who just lost an hour to a stale runbook about ten seconds, and it is
// the moment they are most willing to spend them. A corpus improves from that
// end far faster than it improves from a quarterly review nobody schedules.
//
// It is a named claim with a date on it for the same reason a verification is.
// An anonymous flag is a number that goes up, and the owner who reads it has
// nothing to act on and nobody to ask what was wrong. A report with a name and
// a sentence is a piece of work somebody can do.
//
// One row per person per document. Somebody reporting the same document twice
// is somebody who found it stale again rather than a second complaint, so the
// second report replaces the first, and the count under the document stays the
// count of people who said so rather than the count of times anybody clicked.
type Report struct {
	// Doc is the document this is about.
	Doc string

	// By is who said so, carried whole rather than as a subject id so that the
	// owner reading it recognises a name without a second lookup.
	By doc.Person

	// At is when they said it.
	At time.Time

	// Note is what is wrong, in their words, and is what makes the report worth
	// more than a counter. It is optional because a report nobody could be
	// bothered to write a sentence for is still a report, and refusing it would
	// trade the signal for the sentence.
	Note string
}

// Zero reports whether there is no report here at all.
func (r Report) Zero() bool { return r.At.IsZero() }

// ErrNoReporter is what [Report.Check] stands behind.
var ErrNoReporter = errors.New("report has no reporter")

// Check rejects a report that cannot be stored.
//
// A report with nobody's name on it is the anonymous flag this type exists to
// avoid, so it is refused here rather than in each driver and two drivers
// cannot disagree about what a report is.
func (r Report) Check() error {
	if r.By.Name == "" && r.By.Subject == "" && r.By.Email == "" {
		return ErrNoReporter
	}
	return nil
}

// Staleness is what has been said about one document, gathered.
//
// It is a count and the most recent report rather than the whole list, because
// what a reader needs is whether anybody has objected and what the last of them
// said, and what an owner needs is the same thing plus a person to reply to.
// The full list is the audit log's job.
type Staleness struct {
	// Doc is the document, and Count is how many different people have reported
	// it. The count is people rather than reports, which is what makes it worth
	// putting a number on.
	Doc   string
	Count int

	// Last is the most recent of them, with the name and the sentence on it.
	Last Report

	// Mine is whether one of those people is the principal who asked.
	//
	// It is here rather than worked out from Last, because Last is the most
	// recent report and the person asking is usually not the most recent person
	// to have complained. Without it an interface has to say the same thing to
	// somebody who has already reported the document and somebody who has not,
	// which is how a reader files a second report meaning to correct their first.
	Mine bool
}

// Zero reports whether nobody has said anything about this document.
func (s Staleness) Zero() bool { return s.Count == 0 }

// Flagged is one document somebody has reported, with what was said about it.
//
// It carries the document rather than its id because every caller is drawing a
// list that needs a title and a source, and a list of ids the caller then
// resolves one at a time is twenty round trips where the driver could do one.
// That is the same bargain [Open] makes.
type Flagged struct {
	Document doc.Document
	Stale    Staleness
}

// MayResolve reports whether the principal may clear the reports on a document.
//
// It is the same rule as [MayVerify], and it delegates rather than restating it
// so the two cannot drift apart. Deciding that a report has been dealt with is
// the same claim as saying the document is current, made by the same person: if
// any reader could clear a report, the first thing that would happen to an
// inconvenient one is that somebody would clear it.
//
// Reporting itself has no rule at all beyond being able to read the document.
// That is the point of the feature. A reader who is told they are not important
// enough to say a document is wrong is a reader who stops telling anybody.
func MayResolve(p *acl.Principal, d doc.Document) bool { return MayVerify(p, d) }

// PrincipalKeys is the folded set of names a principal answers to.
//
// It is the other side of [PersonKeys]: that one folds the person a document
// names, and this one folds the person making the request, so a driver can ask
// which documents somebody owns with the same comparison an owner: filter uses.
// Two spellings of that comparison would eventually give somebody an inbox of
// documents that are not theirs.
func PrincipalKeys(p *acl.Principal) []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Identities)+1)
	add := func(s string) {
		if s == "" {
			return
		}
		if k := Fold(s); !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	add(p.Subject)
	for _, id := range p.Identities {
		add(id.Value)
	}
	return out
}

// Reporter is the optional capability of a driver that can remember which
// documents have been reported as out of date.
//
// It is optional like every capability outside [Store], and a deployment whose
// driver does not implement it is not broken: it shows documents with nothing
// said about them, exactly as it did before this existed, and the interface
// offers no report button rather than one that fails.
//
// The permission rule does not move. A report is written and read through the
// principal, so a caller who may not see a document cannot report it, cannot
// learn that somebody else did, and gets the same silence the document itself
// would give them. Reporting a document that is not there, or that the
// principal may not read, writes nothing and is not an error, for the same
// reason recording an open is not: an error that says the id was wrong is a way
// to find out that an id is right.
type Reporter interface {
	Store

	// Report records that somebody says the document is out of date, replacing
	// anything that person said about it before.
	Report(ctx context.Context, p *acl.Principal, r Report) error

	// Resolve clears every report on one document, because whoever is
	// accountable for it has dealt with them. Resolving a document nobody
	// reported is not an error, so that the same call can be made after a
	// verification without asking first.
	Resolve(ctx context.Context, p *acl.Principal, id string) error

	// Withdraw removes the report this principal wrote about one document, and
	// only that one. Withdrawing where there is nothing to withdraw is not an
	// error, for the same reason resolving an unreported document is not.
	//
	// It is a different operation from Resolve rather than a relaxation of it.
	// Resolve is somebody accountable saying the document has been dealt with, so
	// it clears the lot and is held to [MayResolve]. Withdraw is a reader taking
	// back their own sentence, and it needs no rule beyond [ReportKey] matching
	// the key the row was written under, because that comparison can only ever
	// match the row they wrote themselves.
	//
	// Without it, reporting is a one way door. Somebody who reports the wrong
	// document has to go and ask its owner to clear a report that never meant
	// anything, and until they do the owner's panel keeps the mistake in front of
	// them.
	Withdraw(ctx context.Context, p *acl.Principal, id string) error

	// Reports returns what has been said about each of the given documents that
	// has anything said about it and that the principal may read. A document
	// nobody reported is absent from the map rather than present with a zero
	// value.
	//
	// It takes a page of ids rather than one for the same reason
	// [Verifier.Verifications] does: the caller is drawing a screen, and twenty
	// round trips to draw twenty marks is a screen that arrives late.
	Reports(ctx context.Context, p *acl.Principal, ids []string) (map[string]Staleness, error)

	// Reported returns the documents the principal owns or wrote that somebody
	// has reported, most recently reported first, at most limit entries. A limit
	// of zero or less returns nothing.
	//
	// This is the half of the feature that makes reporting worth doing. A report
	// that only marks the document is a message left where the person who has to
	// act on it does not look, so the driver answers the question the other way
	// round as well: not what has been said about this document, but what has
	// been said about mine.
	//
	// Owns or wrote is by name rather than by role. An administrator has the
	// right to clear any of these and no reason to be handed everybody else's
	// work, and an inbox that filled up with the whole corpus for one person is
	// an inbox that person turns off.
	Reported(ctx context.Context, p *acl.Principal, limit int) ([]Flagged, error)
}

// ReportKey is how a driver keys one person's report of one document.
//
// It is the principal's own subject where there is one and their first identity
// otherwise, folded, so that the same person reporting from two sessions
// replaces their own report rather than adding a second. A driver uses this
// rather than inventing its own key, because two drivers that disagree about
// who counts as the same person disagree about the count under the document.
func ReportKey(p *acl.Principal) string {
	keys := PrincipalKeys(p)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}
