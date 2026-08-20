// Package aclmap turns what a source said about who may read a document into
// the model the index runs on.
//
// Every system models permissions differently. Some grant to people, some to
// groups, some to a whole email domain, some to anybody holding a link, and
// several of them let a refusal override a grant. None of them agree on what to
// call any of it: the same idea is a "reader" in one, "READ" in another, "VIEW"
// in a third and "BROWSE_PROJECTS" in a fourth.
//
// A connector could map its own system straight onto [acl.Permissions], and the
// first two would look fine. The trouble starts at the edges, and the edges are
// where a search engine leaks. Every connector would decide on its own what a
// grant to a domain means, whether a link share is a grant, and what to do with
// a statement it does not understand, and the decision that costs a company its
// private documents is the last one, made once, quietly, by whoever was writing
// a connector on a Friday.
//
// So the decisions are made here, once, and a connector's job shrinks to
// translating its own vocabulary into [Grant] values.
//
// # The rules
//
// A refusal beats a grant. This package puts denies in the deny lists and
// [acl.Permissions.Allows] applies them first, in every source that has the
// concept.
//
// Anything that cannot be represented faithfully is quarantined and counted. A
// grant to a domain that is not the tenant's, or a refusal aimed at something
// the model has no deny list for, produces a descriptor in [acl.ModeUnknown],
// which is held out of every query path, and an error saying which document and
// why. Approximating instead would mean guessing, and half of the guesses are a
// document shown to somebody it was refused to.
//
// Link sharing and public documents are recorded rather than inferred from the
// absence of a restriction. See [acl.Sharing].
//
// # What a connector writes
//
//	n, err := aclmap.New(aclmap.Drive("drive", "google", "acme.com"))
//	...
//	perms, err := n.Normalize([]aclmap.Grant{
//		{Subject: aclmap.User, ID: "alice@acme.com", Role: "owner", Owner: true},
//		{Subject: aclmap.Group, ID: "eng@acme.com", Role: "writer"},
//	})
//
// The returned descriptor is safe whether or not the error is nil. A connector
// that ignores the error stores a quarantined document, which is the failure
// mode this package is built to have.
package aclmap

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/tamnd/genba/acl"
)

// Subject is who a statement is about.
type Subject uint8

// The subjects a source can name.
const (
	// User is one person, named however the source names people.
	User Subject = iota

	// Group is a set of people the source maintains.
	Group

	// Domain is everybody whose account belongs to an email domain.
	Domain

	// Anyone is no subject at all: the statement is about everybody, which is
	// what a public document and a link share both look like at the source.
	Anyone
)

// String returns the name of the subject.
func (s Subject) String() string {
	switch s {
	case User:
		return "user"
	case Group:
		return "group"
	case Domain:
		return "domain"
	case Anyone:
		return "anyone"
	default:
		return "unknown"
	}
}

// Effect is whether a statement grants or refuses.
type Effect uint8

// The effects.
const (
	// Allow grants.
	Allow Effect = iota

	// Deny refuses, and beats every grant.
	Deny
)

// Grant is one statement a source makes about one document, in the source's own
// vocabulary.
//
// A connector fills these in from whatever its API returned and does not
// interpret them. Interpreting is what [Normalizer.Normalize] is for, and the
// whole value of the split is that the interpretation is in one place.
type Grant struct {
	// Subject is who the statement is about.
	Subject Subject

	// ID is the person, group or domain the source named. It is empty for
	// [Anyone] and required for everything else.
	ID string

	// Effect is whether this grants or refuses.
	Effect Effect

	// Role is the source's own word for what was granted, such as "reader",
	// "READ" or "BROWSE_PROJECTS". It is matched case insensitively against the
	// read roles of the source, and a role that does not confer read is
	// ignored and counted.
	//
	// An empty role means the statement is about reading. Several systems have
	// no role at all on a share, and a connector should not have to invent one.
	Role string

	// Link says the statement is only in force for somebody holding a link.
	Link bool

	// Owner says this subject owns the document. It is only meaningful on a
	// [User] grant.
	Owner bool
}

// LinkPolicy is what a deployment wants a link share to mean in search.
type LinkPolicy uint8

// The link policies.
const (
	// LinkGrantsNothing is the default and the safe reading. A link shared
	// document is readable by whoever its lists name, the share is recorded as
	// [acl.SharedByLink], and somebody who never had the link does not find it
	// by searching.
	LinkGrantsNothing LinkPolicy = iota

	// LinkGrantsTenant treats a link share as readable by everybody in the
	// deployment.
	//
	// It is the right setting for a company where turning on a link is how
	// people publish to their colleagues, and it has to be asked for by name,
	// because in a company where it is not, this is the setting that hands
	// several years of link shared documents to everybody at once.
	LinkGrantsTenant
)

// Rules is how one source's vocabulary maps onto the model.
//
// The presets in sources.go carry the vocabularies of the systems this project
// indexes, and each of them is the source system's own list of names rather
// than one invented here.
type Rules struct {
	// Source is the connector name, which every produced descriptor carries.
	Source string

	// Identity is the identity source that the ids in a [Grant] belong to, for
	// example "google" or "slack". Getting it right is what lets somebody who
	// authenticated through one system match a list written in terms of
	// another.
	Identity string

	// ReadRoles are the source's names for the roles that confer reading. A
	// role outside the list is ignored and counted, which is how a grant of a
	// permission to change a label stops being a grant to read the document.
	//
	// An empty list means every role confers read, which is correct for a
	// source whose shares carry no role at all.
	ReadRoles []string

	// Domains are the email domains that are this tenant. A grant to one of
	// them is everybody in the deployment. A grant to any other domain is a
	// set of people the index cannot name, so it quarantines.
	Domains []string

	// Link is what a link share means here.
	Link LinkPolicy
}

// Reason is why a document could not be mapped.
type Reason uint8

// The reasons, and the counters they increment.
const (
	// ReasonNone is a document that mapped.
	ReasonNone Reason = iota

	// ReasonForeignDomain is a grant to an email domain that is not the
	// tenant's. The people it names are real and the index cannot enumerate
	// them, so it cannot honour the grant and will not approximate it.
	ReasonForeignDomain

	// ReasonUnmappableDeny is a refusal the model has no deny list for, such as
	// one aimed at a domain or at everybody. Dropping it would turn a refusal
	// into silence, which is the one direction that must never happen.
	ReasonUnmappableDeny

	// ReasonMalformed is a statement naming nobody, such as a user grant with
	// an empty id.
	ReasonMalformed
)

// String returns the name of the reason.
func (r Reason) String() string {
	switch r {
	case ReasonNone:
		return "none"
	case ReasonForeignDomain:
		return "foreign domain"
	case ReasonUnmappableDeny:
		return "unmappable deny"
	case ReasonMalformed:
		return "malformed grant"
	default:
		return "unknown"
	}
}

// Error is what a document that could not be mapped failed on.
type Error struct {
	Reason Reason

	// Detail names the statement that could not be mapped, so that somebody
	// reading a log has something to go and look at rather than a count.
	Detail string
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("aclmap: %s: %s", e.Reason, e.Detail)
}

// Counts is what a normaliser has seen.
//
// Quarantined documents are counted by reason because the three reasons want
// three different actions. A foreign domain is a decision somebody has to make
// about the tenant, an unmappable deny is usually a source feature nobody has
// written the mapping for yet, and a malformed grant is a bug in a connector.
// One number for all three is a number nobody can act on.
type Counts struct {
	// Mapped is documents that produced a usable descriptor.
	Mapped int64

	// ForeignDomain, UnmappableDeny and Malformed are documents quarantined for
	// each reason.
	ForeignDomain  int64
	UnmappableDeny int64
	Malformed      int64

	// Ignored is statements whose role does not confer read. It is not a
	// failure and it is worth watching: a sudden climb usually means a source
	// renamed a role and the mapping has not caught up, which shows up to
	// somebody as documents they used to be able to find.
	Ignored int64
}

// Quarantined is how many documents were held back.
func (c Counts) Quarantined() int64 {
	return c.ForeignDomain + c.UnmappableDeny + c.Malformed
}

// Normalizer maps one source's statements onto descriptors and counts what it
// did.
//
// It is safe for concurrent use, because a connector that fetches access
// control lists in parallel is the normal shape of one and having to lock
// around this would be a reason not to.
type Normalizer struct {
	rules Rules
	read  map[string]bool
	domas map[string]bool

	mapped  atomic.Int64
	foreign atomic.Int64
	denied  atomic.Int64
	bad     atomic.Int64
	ignored atomic.Int64
}

// New returns a normaliser for one source.
func New(r Rules) (*Normalizer, error) {
	if r.Source == "" {
		return nil, errors.New("aclmap: empty source name")
	}
	if r.Identity == "" {
		// Without it every user reference would be compared against the bare
		// value, so a person called "alice" in one system would match a person
		// called "alice" in another. That is a silent grant across two
		// companies' worth of accounts.
		return nil, errors.New("aclmap: empty identity source")
	}
	n := &Normalizer{rules: r, read: make(map[string]bool, len(r.ReadRoles)), domas: make(map[string]bool, len(r.Domains))}
	for _, role := range r.ReadRoles {
		n.read[strings.ToLower(role)] = true
	}
	for _, d := range r.Domains {
		n.domas[strings.ToLower(strings.TrimPrefix(d, "@"))] = true
	}
	return n, nil
}

// Counts returns what this normaliser has seen so far.
func (n *Normalizer) Counts() Counts {
	return Counts{
		Mapped:         n.mapped.Load(),
		ForeignDomain:  n.foreign.Load(),
		UnmappableDeny: n.denied.Load(),
		Malformed:      n.bad.Load(),
		Ignored:        n.ignored.Load(),
	}
}

// Normalize turns one document's statements into a descriptor.
//
// The descriptor it returns is safe to store whatever the error is. On failure
// it is [acl.ModeUnknown], which every query path holds back, so a connector
// that logs the error and carries on has quarantined the document rather than
// published it.
//
// An empty list of statements is not a failure. A document nobody has been
// given is a document nobody may read, and that is represented exactly: an
// empty access control list, which allows nobody.
func (n *Normalizer) Normalize(grants []Grant) (acl.Permissions, error) {
	perm := acl.Permissions{Mode: acl.ModeACL, Source: n.rules.Source}
	var owners int

	for _, g := range grants {
		if err := n.apply(&perm, g, &owners); err != nil {
			n.count(err)
			return n.unresolved(), err
		}
	}

	// A document whose only statement is its owner is owner only rather than an
	// access control list with one name in it. The two behave the same today
	// and they do not mean the same thing, and the mode is what a later feature
	// such as "everything of mine" reads.
	if owners == 1 && len(perm.AllowUsers) == 0 && len(perm.AllowGroups) == 0 && perm.Mode == acl.ModeACL {
		perm.Mode = acl.ModeOwnerOnly
	}

	n.mapped.Add(1)
	return perm, nil
}

// apply folds one statement into the descriptor being built.
func (n *Normalizer) apply(perm *acl.Permissions, g Grant, owners *int) error {
	if g.Subject != Anyone && g.ID == "" {
		return &Error{Reason: ReasonMalformed, Detail: g.Subject.String() + " grant naming nobody"}
	}
	if !n.confers(g.Role) {
		// A role outside the read set is not a failure and not a grant. Somebody
		// who may change a label may not read the document, and a source that
		// says so is being precise rather than obstructive.
		n.ignored.Add(1)
		return nil
	}

	if g.Effect == Deny {
		return n.deny(perm, g)
	}
	return n.allow(perm, g, owners)
}

// deny puts a refusal in the right list, or refuses to map the document.
func (n *Normalizer) deny(perm *acl.Permissions, g Grant) error {
	switch g.Subject {
	case User:
		perm.DenyUsers = appendRef(perm.DenyUsers, acl.Ref{Source: n.rules.Identity, Value: g.ID})
		return nil
	case Group:
		perm.DenyGroups = appendRef(perm.DenyGroups, acl.Ref{Source: n.rules.Identity, Value: g.ID})
		return nil
	case Domain, Anyone:
		// There is no deny list for these, and there is no safe approximation
		// either. Treating a refusal aimed at a domain as one aimed at nobody
		// would keep the document readable by exactly the people it was taken
		// away from.
		return &Error{Reason: ReasonUnmappableDeny, Detail: "a deny aimed at " + n.subjectName(g)}
	default:
		return &Error{Reason: ReasonMalformed, Detail: "unknown subject"}
	}
}

// allow puts a grant in the right list, or widens the mode.
func (n *Normalizer) allow(perm *acl.Permissions, g Grant, owners *int) error {
	switch g.Subject {
	case User:
		ref := acl.Ref{Source: n.rules.Identity, Value: g.ID}
		// The first owner is the owner. A source that reports several, which
		// several do, keeps the rest as ordinary readers, because one field
		// cannot hold two of them and dropping the others would take away
		// access somebody has.
		if g.Owner {
			*owners++
			if perm.Owner.Value == "" {
				perm.Owner = ref
				return nil
			}
		}
		perm.AllowUsers = appendRef(perm.AllowUsers, ref)
		return nil

	case Group:
		perm.AllowGroups = appendRef(perm.AllowGroups, acl.Ref{Source: n.rules.Identity, Value: g.ID})
		return nil

	case Domain:
		if !n.domas[strings.ToLower(strings.TrimPrefix(g.ID, "@"))] {
			return &Error{Reason: ReasonForeignDomain, Detail: "a grant to " + g.ID}
		}
		n.widen(perm, acl.ModePublicToTenant)
		return nil

	case Anyone:
		return n.anyone(perm, g)

	default:
		return &Error{Reason: ReasonMalformed, Detail: "unknown subject"}
	}
}

// anyone handles the two statements that carry no subject: a public document
// and a link share.
func (n *Normalizer) anyone(perm *acl.Permissions, g Grant) error {
	if g.Link {
		perm.Sharing = maxSharing(perm.Sharing, acl.SharedByLink)
		if n.rules.Link == LinkGrantsTenant {
			n.widen(perm, acl.ModePublicToTenant)
		}
		return nil
	}
	// Not a link, so the source is saying the document is out in the open. It
	// is readable by everybody in the deployment, and the fact that it reaches
	// further than that is recorded rather than lost.
	perm.Sharing = maxSharing(perm.Sharing, acl.SharedPublic)
	n.widen(perm, acl.ModePublicToTenant)
	return nil
}

// widen moves the mode outwards and never inwards, so that the order the
// statements arrived in cannot change the answer.
func (n *Normalizer) widen(perm *acl.Permissions, m acl.Mode) {
	if perm.Mode != acl.ModePublicToTenant {
		perm.Mode = m
	}
}

// unresolved is the descriptor a quarantined document gets. It names its source
// so that a report can say which connector the document came from, and says
// nothing else, because nothing else was established.
func (n *Normalizer) unresolved() acl.Permissions {
	return acl.Permissions{Mode: acl.ModeUnknown, Source: n.rules.Source}
}

// confers reports whether a role grants reading.
func (n *Normalizer) confers(role string) bool {
	// No role at all is a statement about reading. Several systems attach no
	// role to a share, and a connector should not have to invent one.
	if role == "" || len(n.read) == 0 {
		return true
	}
	return n.read[strings.ToLower(role)]
}

// subjectName is how a statement's subject reads in an error.
func (n *Normalizer) subjectName(g Grant) string {
	if g.Subject == Anyone {
		return "everybody"
	}
	return g.Subject.String() + " " + g.ID
}

// count increments the counter for a failure.
func (n *Normalizer) count(err error) {
	var e *Error
	if !errors.As(err, &e) {
		return
	}
	switch e.Reason {
	case ReasonForeignDomain:
		n.foreign.Add(1)
	case ReasonUnmappableDeny:
		n.denied.Add(1)
	case ReasonMalformed, ReasonNone:
		n.bad.Add(1)
	}
}

// appendRef adds a reference unless it is already there.
//
// Duplicates are common, because a source that reports a person twice under two
// roles is reporting the truth, and they are worth removing here rather than in
// every driver: the lists end up in a query, and a list with a name in it four
// times is four terms in that query.
func appendRef(refs []acl.Ref, r acl.Ref) []acl.Ref {
	for _, have := range refs {
		if have == r {
			return refs
		}
	}
	return append(refs, r)
}

// maxSharing keeps the widest sharing seen, so that a document both link shared
// and public is recorded as public.
func maxSharing(a, b acl.Sharing) acl.Sharing {
	if b > a {
		return b
	}
	return a
}
