package store

import (
	"context"
	"errors"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
)

// Correction is a person saying that the owner a source reported is wrong.
//
// Ownership is derived, and derived ownership is wrong often enough that a
// corpus which cannot be corrected slowly stops being trusted. A connector
// reports whoever the source calls the owner, which is the account that ran the
// import, the person who created the folder in 2019, or the service that syncs
// the drive. None of them can answer a question about the document and none of
// them will re-verify it, so the badge, the nudge and the report all go
// nowhere.
//
// A correction is one row per document and it replaces the derived answer
// rather than sitting beside it. That is the whole design. An overlay applied
// while reading would be invisible to the owner: filter, to the facet counts
// and to every ranking signal that reads the owner, which means the interface
// would show one owner and the search would sort by another. So the driver
// writes the corrected owner into the document itself, and keeps applying it
// every time the crawl reports the source's answer again.
//
// Correcting who owns a document does not change who may read it. Ownership
// here is the person accountable for the content and permissions are
// [acl.Permissions], a different field with a different meaning that no part of
// this touches. It is worth saying plainly because the two words are the same
// word, and a curation feature that quietly widened an ACL would be the worst
// bug this repository could ship.
type Correction struct {
	// Doc is the document this is about.
	Doc string

	// Owner is who owns it, and Was is what the source said before anybody
	// corrected it.
	//
	// Was is kept because a correction has to be undoable, and undoing one means
	// putting back the answer the connector gave rather than leaving the
	// document owned by nobody until the next crawl comes round. It is refreshed
	// on every write, so a source that fixes its own metadata is what a reader
	// gets back when the correction is cleared.
	Owner doc.Person
	Was   doc.Person

	// By is who said so and At is when, carried whole for the same reason a
	// verification carries its verifier whole: the interface says a name, and a
	// change of ownership with no name against it is a change nobody can query.
	By doc.Person
	At time.Time
}

// Zero reports whether there is no correction here at all.
func (c Correction) Zero() bool { return c.At.IsZero() }

// ErrNoOwner and ErrNoCorrector are what [Correction.Check] stands behind.
var (
	ErrNoOwner     = errors.New("correction has no owner")
	ErrNoCorrector = errors.New("correction has nobody making it")
)

// Check rejects a correction that cannot be stored.
//
// Both refusals are here rather than in each driver, so that three drivers
// cannot disagree about what a correction is. A correction to nobody is a
// deletion written the wrong way and there is a call for that, and one with
// nobody making it is an audit trail with a hole in it exactly where the
// question gets asked.
func (c Correction) Check() error {
	switch {
	case c.Owner.Name == "" && c.Owner.Email == "" && c.Owner.Subject == "":
		return ErrNoOwner
	case c.By.Name == "" && c.By.Subject == "":
		return ErrNoCorrector
	default:
		return nil
	}
}

// MayReassign reports whether the principal is allowed to correct who owns the
// document.
//
// It is deliberately the same rule as [MayVerify], and it delegates rather than
// restating it so that the two cannot drift apart. They have to be the same
// rule, because being the owner is what makes somebody allowed to verify: if
// any reader could name themselves the owner, then any reader could vouch for
// any document they can read, and the accountability that makes a verification
// worth reading would be gone. Reassignment is a handover between people who
// were already accountable, or an administrator fixing what a connector got
// wrong.
//
// The cost of that is real and worth naming. A document whose derived owner is
// a service account, with no author, cannot be claimed by the person who
// actually owns it: they have to ask an administrator. That is the right way
// round. The alternative hands the strongest signal in the corpus to whoever
// clicks first.
func MayReassign(p *acl.Principal, d doc.Document) bool { return MayVerify(p, d) }

// Ownership is the optional capability of a driver that can remember a
// corrected owner.
//
// It is optional like every capability outside [Store], and a deployment whose
// driver does not implement it is not broken: it shows the owner the connector
// reported, exactly as it did before this existed, and the interface offers no
// way to change it rather than one that fails.
//
// A driver that implements this applies corrections on [Store.Put]. That is the
// contract and it is the only part of this that is subtle: a crawl which reports
// the source's answer again must not undo a correction, and the place that can
// promise it is the write, because the write is the one path every document
// takes into a driver. Doing it there is also what keeps reading free. The
// stored document already names the corrected owner, so a search result, an
// owner: filter and a facet count all agree with the interface without joining
// to anything, and the whole cost of the feature sits on a write that happens
// once a year per document.
//
// The permission rule does not move. Corrections are written and read through
// the principal, so a caller who may not see a document cannot correct it,
// cannot learn that somebody else did, and gets the same silence the document
// itself would give them. Correcting a document that is not there, or that the
// principal may not read, writes nothing and is not an error, for the same
// reason recording an open is not: an error that says the id was wrong is a way
// to find out that an id is right.
type Ownership interface {
	Store

	// SetOwner records the correction, replacing any earlier one for the same
	// document, and writes the new owner into the document itself.
	//
	// Replacing rather than appending is what makes correcting a correction the
	// same call as making one. The source's own answer survives the replacement:
	// a document corrected twice still remembers what the connector said, so
	// clearing it once puts the derived owner back rather than the previous
	// person's guess.
	SetOwner(ctx context.Context, p *acl.Principal, c Correction) error

	// ClearOwner drops the correction and puts back the owner the source
	// reported. Clearing one that is not there is not an error, so that a
	// mistake can be undone twice.
	ClearOwner(ctx context.Context, p *acl.Principal, id string) error

	// Corrections returns the correction for each of the given documents that
	// has one and that the principal may read. A document with no correction is
	// absent from the map rather than present with a zero value.
	//
	// It takes a page of ids rather than one for the same reason [Verifier] does,
	// though the caller today is a single document: the owner shown in a result
	// is already the corrected one, and this is what is asked for when somebody
	// opens the document and wants to know who changed it and when.
	Corrections(ctx context.Context, p *acl.Principal, ids []string) (map[string]Correction, error)
}
