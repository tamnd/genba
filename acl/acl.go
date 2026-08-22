// Package acl holds the permission model: who is asking, which groups they
// belong to, what a document's source said about who may read it, and how those
// two sides meet.
//
// Two rules run through everything here.
//
// A deny always beats an allow. Several of the systems we index model
// permissions that way, and inverting the precedence turns a document nobody
// was supposed to see into a search result.
//
// A permission we could not resolve is not a permission. A document whose
// access rules failed to parse, or that references a group we never saw, is
// held back from every query path rather than indexed with a guess.
package acl

import (
	"slices"
	"strings"
)

// Kind distinguishes the sort of subject making a request. Agents and services
// take different paths through the guardrails and are left out of the signals
// that personalise ranking.
type Kind uint8

// The kinds of principal.
const (
	KindUser Kind = iota
	KindService
	KindAgent
)

// Identity is how one source system names a subject. The same person is an
// email address in one tool, an opaque member id in another and a numeric id in
// a third, so a principal carries the whole set and each source's rules are
// evaluated against the identity that source understands.
type Identity struct {
	Source string // the source that issued the identifier, for example "slack"
	Value  string // the identifier itself, for example "U04AB"
}

// GroupSet is a subject's fully expanded group membership together with the
// version it was expanded at.
//
// Expansion is a transitive closure over the directory and can touch thousands
// of edges, so it happens when a session starts rather than on every query. The
// version is the safety catch: it is part of the cache key of every bitmap
// derived from the set, so a membership change invalidates the derived state
// immediately instead of waiting for a timeout.
type GroupSet struct {
	Version uint64
	Members []string
}

// Has reports whether the set contains the named group.
func (g GroupSet) Has(group string) bool {
	return slices.Contains(g.Members, group)
}

// Principal is the authenticated subject of a request.
//
// Every call that can return document content takes one. Passing nil is a
// programming error, not a way to search anonymously.
type Principal struct {
	Tenant     string
	Subject    string
	Kind       Kind
	Identities []Identity
	Groups     GroupSet
	Roles      []string

	// OnBehalfOf is set when a service or an agent acts for a person. The
	// rights of the run are the intersection of that person's rights and the
	// caller's declared scopes, never the union.
	OnBehalfOf string
}

// IdentityIn returns the subject's identifier in the named source, and reports
// whether one is known.
func (p *Principal) IdentityIn(source string) (string, bool) {
	if p == nil {
		return "", false
	}
	for _, id := range p.Identities {
		if strings.EqualFold(id.Source, source) {
			return id.Value, true
		}
	}
	return "", false
}

// RoleAdmin is the role that may see how the deployment is running.
//
// It grants nothing over documents and it must not start to. Everything it
// opens up is about the machinery: which connectors are syncing, which runs
// failed, what is being held back and why. A person holding it sees the same
// search results as they did without it, because an operator being able to read
// the corpus is a decision about that person and not about that job, and the
// day the two are the same role is the day nobody can be given the second
// without the first.
const RoleAdmin = "admin"

// HasRole reports whether the principal holds the named role.
func (p *Principal) HasRole(role string) bool {
	return p != nil && slices.Contains(p.Roles, role)
}

// Mode says how a document's access rules should be read.
type Mode uint8

// The permission modes.
const (
	// ModeUnknown means the source's rules were not resolved. A document in
	// this mode is never visible to anyone, whatever else its descriptor says.
	ModeUnknown Mode = iota

	// ModeACL means the allow and deny lists below decide.
	ModeACL

	// ModePublicToTenant means everyone in the deployment may read the
	// document. A source has to say so explicitly for a document to land here.
	ModePublicToTenant

	// ModeOwnerOnly means only the owner may read the document.
	ModeOwnerOnly
)

// Valid reports whether m is one of the modes above.
//
// A mode is a small integer and it arrives from places the compiler does not
// check: a connector written against an older version of this package, a row
// read back out of a store, a value decoded from a cursor. An unrecognised one
// is not a mode this system knows how to apply, and the only safe reading of it
// is that the descriptor did not resolve.
//
// The switch has no default clause on purpose. That is what makes the linter
// fail here when a mode is added, which is a great deal better than the
// alternative, where a new mode is quietly reported as not existing by every
// check that was written before it.
func (m Mode) Valid() bool {
	switch m {
	case ModeUnknown, ModeACL, ModePublicToTenant, ModeOwnerOnly:
		return true
	}
	return false
}

// Sharing records how far a source said a document travels, beyond the people
// its lists name.
//
// It grants nothing. [Permissions.Allows] never reads it and no storage driver
// filters on it, because by the time a descriptor is stored the effective grant
// is already in the mode and the lists. What it is for is saying so out loud.
//
// A document that anybody can read because somebody turned on a link is a
// different fact from a document that anybody can read because somebody named
// them, and an index that records only the second cannot answer the question an
// administrator actually asks, which is what did we share and with whom. The
// absence of a restriction is not a record of a decision, so the decision is
// recorded.
type Sharing uint8

// The sharing states.
const (
	// SharedNone is a document readable only through its lists. It is the
	// default and it is what almost everything is.
	SharedNone Sharing = iota

	// SharedByLink is a document its source says anybody holding a link may
	// read.
	//
	// Whether that counts as a grant in the index is a deployment's decision
	// and not this package's, because the two readings are both defensible and
	// they are not the same. Somebody who has the link was given it. Somebody
	// searching was not, and turning a link share into a search result hands
	// the document to everybody who never had one.
	SharedByLink

	// SharedPublic is a document its source says is readable outside the
	// tenant entirely, such as one published on the internet.
	SharedPublic
)

// String returns the name of the sharing state.
func (s Sharing) String() string {
	switch s {
	case SharedNone:
		return "none"
	case SharedByLink:
		return "link"
	case SharedPublic:
		return "public"
	default:
		return "unknown"
	}
}

// Ref names a user or a group inside one source.
type Ref struct {
	Source string
	Value  string
}

// Permissions is the normalised form of what one source said about who may read
// one document.
type Permissions struct {
	Mode        Mode
	Owner       Ref
	AllowUsers  []Ref
	AllowGroups []Ref
	DenyUsers   []Ref
	DenyGroups  []Ref

	// Sharing is how far the source said the document travels. It is a record
	// rather than a rule, and the comment on [Sharing] says why.
	Sharing Sharing

	// Source is the connector the document came from.
	Source string

	// Version increases every time the source reports a change. It is what a
	// revocation bumps, and what derived bitmaps are keyed on.
	Version uint64

	// Reason says why the mode is unknown, in the words of whatever failed to
	// resolve it. It is empty for every descriptor that resolved, and nothing
	// reads it to decide anything.
	//
	// It is here because a quarantined document is otherwise a number. An
	// operator looking at a count of eleven hundred held back documents has no
	// way to tell one connector's broken role mapping from a tenant boundary
	// working exactly as intended, and the two want opposite actions. The
	// mapping already knows which it was at the moment it gave up, so the
	// sentence it would have logged and thrown away travels with the document
	// instead, and the screen that lists the quarantine can say why for each
	// entry rather than in aggregate.
	//
	// It is the one field here with a JSON tag, because every document in the
	// store is written as JSON with these names and almost none of them have a
	// reason. An always present empty string on every row of a corpus is a cost
	// paid by the many for the few.
	Reason string `json:",omitempty"`
}

// Rule names the clause of the permission rule that settled a question.
type Rule string

// The clauses, in the order [Permissions.Decide] applies them.
const (
	// RuleNoPrincipal is nobody asking, which is a programming error rather
	// than an anonymous reader.
	RuleNoPrincipal Rule = "no-principal"

	// RuleUnresolved is a descriptor the source's rules never resolved into.
	// The document is held back from everybody.
	RuleUnresolved Rule = "unresolved"

	// RuleDenied is an explicit deny, which beats every allow.
	RuleDenied Rule = "denied"

	// RuleOwner is the subject owning the document.
	RuleOwner Rule = "owner"

	// RuleOwnerOnly is a document only its owner may read, read by somebody
	// else.
	RuleOwnerOnly Rule = "owner-only"

	// RuleTenant is a document the source said everybody in the deployment may
	// read.
	RuleTenant Rule = "tenant"

	// RuleListed and RuleNotListed are the access control list admitting the
	// subject, and failing to.
	RuleListed    Rule = "listed"
	RuleNotListed Rule = "not-listed"
)

// Decision is the answer [Permissions.Allows] gives, with the reason for it.
//
// It exists for the administration screen that answers what a given person can
// see, where "no" on its own is not an answer somebody can act on. What they
// need is which clause settled it and which reference it matched, because the
// action is different for each: a deny is a decision somebody made, an empty
// access control list is usually a connector that has not finished, and an
// unresolved descriptor is a bug in whichever mapping gave up.
//
// It is a separate type rather than three return values because it is served on
// the wire, and it does not carry a sentence because the words belong on the
// screen that shows them rather than in the package that decides.
type Decision struct {
	// Allowed is the answer, and it is exactly what Allows returns.
	Allowed bool

	// Rule is the clause that settled it.
	Rule Rule

	// Ref is the reference that matched, where one did. It is the deny that
	// won, the allow that admitted them, or the owner, and it is zero for the
	// clauses that match nothing.
	Ref Ref
}

// Allows reports whether the principal may read the document.
func (perm Permissions) Allows(p *Principal) bool { return perm.Decide(p).Allowed }

// Decide answers whether the principal may read the document, and why.
//
// The order is fixed and is the whole point of the function: an unresolved
// descriptor denies, then an explicit deny denies, then an allow allows.
//
// It is the one implementation of that order. Allows is written in terms of it
// rather than beside it, because two copies of a rule drift and the way this
// one drifts is that somebody sees a document.
func (perm Permissions) Decide(p *Principal) Decision {
	if p == nil {
		return Decision{Rule: RuleNoPrincipal}
	}
	if perm.Mode == ModeUnknown {
		return Decision{Rule: RuleUnresolved}
	}
	if r, ok := perm.matchesUser(p, perm.DenyUsers); ok {
		return Decision{Rule: RuleDenied, Ref: r}
	}
	if r, ok := perm.matchesGroup(p, perm.DenyGroups); ok {
		return Decision{Rule: RuleDenied, Ref: r}
	}
	if perm.isOwner(p) {
		return Decision{Allowed: true, Rule: RuleOwner, Ref: perm.Owner}
	}
	switch perm.Mode {
	case ModeOwnerOnly:
		return Decision{Rule: RuleOwnerOnly}
	case ModePublicToTenant:
		return Decision{Allowed: true, Rule: RuleTenant}
	case ModeACL:
		if r, ok := perm.matchesUser(p, perm.AllowUsers); ok {
			return Decision{Allowed: true, Rule: RuleListed, Ref: r}
		}
		if r, ok := perm.matchesGroup(p, perm.AllowGroups); ok {
			return Decision{Allowed: true, Rule: RuleListed, Ref: r}
		}
		return Decision{Rule: RuleNotListed}
	default:
		return Decision{Rule: RuleNotListed}
	}
}

func (perm Permissions) isOwner(p *Principal) bool {
	if perm.Owner.Value == "" {
		return false
	}
	_, ok := perm.matchesUser(p, []Ref{perm.Owner})
	return ok
}

// matchesUser and matchesGroup are the same set membership a storage driver
// runs inside its own query, expressed over the key forms in ref.go so that
// there is one definition of the rule rather than one per driver.
//
// They return which reference matched as well as whether one did. The decision
// does not need it and the screen that explains the decision does, and working
// it out a second time afterwards would be a second implementation of the
// comparison.
func (perm Permissions) matchesUser(p *Principal, refs []Ref) (Ref, bool) {
	return matchesKey(p.UserKeys(), refs, Ref.UserKey)
}

func (perm Permissions) matchesGroup(p *Principal, refs []Ref) (Ref, bool) {
	return matchesKey(p.GroupKeys(), refs, Ref.GroupKey)
}
