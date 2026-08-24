package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
)

// Verification is somebody putting their name to a document.
//
// It is the cheapest curation signal there is, because it applies to content
// that already exists. Writing an answer means writing an answer. Verifying a
// runbook means reading one somebody already wrote and saying it is still true,
// which is thirty seconds of work by the person who would have been asked
// anyway, and it turns a corpus where everything looks equally plausible into
// one where the current things are marked.
//
// It is a claim by a named person with a date on it, not a flag. That is the
// whole design. A boolean "verified" column decays into noise within a year
// because nothing ever unsets it, so this carries who said so and when it stops
// counting, and a reader who disagrees with the claim has somebody to ask.
//
// One row per document rather than a history. What a reader needs is the
// current claim, and reconstructing it from a log means reading every line ever
// written about the document to draw one badge. The history of who verified
// what and when is the audit log's job.
type Verification struct {
	// Doc is the document this is about.
	Doc string

	// By is who made the claim, carried whole rather than as a subject id so
	// that a badge has a name on it without a second lookup. A badge that says
	// u-4181 vouched for this is worse than no badge, because the point of the
	// signal is that a reader recognises the name.
	By doc.Person

	// At is when it was made, and Until is when it stops counting as current.
	//
	// Until is stored rather than derived from At plus a cadence, because the
	// cadence is a policy that will change and a document verified under the
	// old one should keep the expiry it was given. A verifier who wants a
	// different one says so at the time.
	At    time.Time
	Until time.Time

	// Note is why, in the verifier's own words, and is usually empty. It is
	// here for the case the badge cannot express: verified except for the
	// section about the old cluster, which is the sentence that saves the next
	// reader an hour.
	Note string
}

// State is where a verification is in its life.
type State string

// The verification states.
const (
	// Fresh means the claim is current and not close to running out.
	Fresh State = "fresh"

	// Expiring means it is still current and is within [ExpiringWindow] of not
	// being. It exists so that the interface can ask for a re-verification
	// before the badge goes bad rather than after, which is the difference
	// between a nudge and a correction.
	Expiring State = "expiring"

	// Expired means nobody has vouched for the document since Until.
	//
	// The document keeps the badge and the badge says so, rather than the badge
	// disappearing. A document that was verified in 2023 and not since is a more
	// useful thing to know about than a document nobody ever looked at, and
	// silently dropping the mark tells a reader the second story.
	Expired State = "expired"
)

// Cadence is how long a verification lasts when the verifier does not say.
//
// Six months, which is the number every system like this converges on for the
// same reason: a year is long enough that a policy document can be wrong for
// two quarters, and a quarter is short enough that the reminders become noise
// and people start clicking verify without reading. It is a default rather than
// a rule, and a verifier who knows their document changes weekly sets their own.
const Cadence = 180 * 24 * time.Hour

// ExpiringWindow is how long before the expiry a verification starts saying so.
//
// Two weeks, which is roughly the time it takes for a nudge to survive one
// holiday, one on call rotation and one sprint boundary and still be acted on
// before the badge goes bad.
const ExpiringWindow = 14 * 24 * time.Hour

// State reports where the verification is at the given time. The zero
// [Verification] is expired, which is the safe reading of no claim at all.
func (v Verification) State(now time.Time) State { return stateAt(v.Until, now) }

// stateAt is the rule itself, shared with [Answer.State].
//
// A verification and an answer are the same claim about two different things,
// and a reader who learns what the amber badge on a document means has learned
// what it means on a card. Two copies of these three comparisons would let one
// of them start expiring a day earlier than the other.
func stateAt(until, now time.Time) State {
	switch {
	case !now.Before(until):
		return Expired
	case now.Add(ExpiringWindow).After(until):
		return Expiring
	default:
		return Fresh
	}
}

// Zero reports whether there is no claim here at all.
func (v Verification) Zero() bool { return v.At.IsZero() }

// ErrNoVerifier and ErrNotOwner are what [Verification.Check] and [MayVerify]
// stand behind.
var (
	ErrNoVerifier = errors.New("verification has no verifier")
	ErrNoExpiry   = errors.New("verification has no expiry")
)

// Check rejects a verification that cannot be stored.
//
// A claim with nobody's name on it is the boolean column this type exists to
// avoid, and one with no expiry is the same column with an extra field. Both
// are refused here rather than in each driver, so that two drivers cannot
// disagree about what a verification is.
func (v Verification) Check() error {
	switch {
	case v.By.Name == "" && v.By.Subject == "":
		return ErrNoVerifier
	case v.Until.IsZero():
		return ErrNoExpiry
	default:
		return nil
	}
}

// MayVerify reports whether the principal is allowed to vouch for the document.
//
// The rule is the document's owner, its author, or an administrator. Not
// everyone who can read it, because verification means accountability and a
// badge anybody can apply is a badge that means somebody read the title. Not
// only the owner, because the owner a connector reports is often a service
// account or whoever created the folder in 2019, and a rule that strict means
// half the corpus can never be verified by anyone.
//
// It lives here rather than inside a driver because it is a curation policy
// rather than a permission rule, and the difference matters: a driver enforces
// that the principal can read the document, which is what stops a verification
// from being used to prove a document exists. Who among the readers is allowed
// to make the claim is one decision, made once, above the storage layer.
func MayVerify(p *acl.Principal, d doc.Document) bool {
	if p == nil {
		return false
	}
	if p.HasRole(acl.RoleAdmin) {
		return true
	}
	return IsPerson(p, d.Owner) || IsPerson(p, d.Author)
}

// IsPerson reports whether the principal is the person named, by subject id
// first and by email second.
//
// Email is a fallback rather than the rule because it is the one identifier a
// connector reports for a person it could not resolve, and without it the owner
// of every document from a source with no identity mapping is nobody.
//
// It is exported because the answer is needed in two places that must not
// disagree: deciding whether somebody may verify a document, and deciding whose
// name goes on the badge when they do. Two spellings of the same comparison
// would eventually let a person verify a document under somebody else's name.
func IsPerson(p *acl.Principal, who doc.Person) bool {
	if p == nil {
		return false
	}
	if who.Subject != "" && who.Subject == p.Subject {
		return true
	}
	if who.Email == "" {
		return false
	}
	for _, id := range p.Identities {
		if strings.EqualFold(id.Value, who.Email) {
			return true
		}
	}
	return false
}

// Verifier is the optional capability of a driver that can remember who vouched
// for a document.
//
// It is optional like every capability outside [Store], and a deployment whose
// driver does not implement it is not broken: it shows results with no badges
// on them, exactly as it did before this existed, and the interface offers no
// verify button rather than one that fails.
//
// The permission rule does not move. A verification is written and read through
// the principal, so a caller who may not see a document cannot verify it, cannot
// learn that somebody else did, and gets the same silence the document itself
// would give them. Verifying a document that is not there, or that the principal
// may not read, writes nothing and is not an error, for the same reason
// recording an open is not: an error that says the id was wrong is a way to find
// out that an id is right.
type Verifier interface {
	Store

	// Verify records the claim, replacing any earlier one for the same
	// document. Replacing rather than appending is what makes re-verifying the
	// same call as verifying, and it is why this type is one row per document.
	Verify(ctx context.Context, p *acl.Principal, v Verification) error

	// Unverify withdraws the claim. Withdrawing one that is not there is not an
	// error, so that a mistake can be undone twice.
	Unverify(ctx context.Context, p *acl.Principal, id string) error

	// Verifications returns the claim for each of the given documents that has
	// one and that the principal may read. A document with no claim is absent
	// from the map rather than present with a zero value.
	//
	// It takes a page of ids rather than one, because the caller is a search
	// result page and the alternative is twenty round trips to draw twenty
	// badges.
	Verifications(ctx context.Context, p *acl.Principal, ids []string) (map[string]Verification, error)
}
