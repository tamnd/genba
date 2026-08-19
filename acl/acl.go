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

	// Source is the connector the document came from.
	Source string

	// Version increases every time the source reports a change. It is what a
	// revocation bumps, and what derived bitmaps are keyed on.
	Version uint64
}

// Allows reports whether the principal may read the document.
//
// The order is fixed and is the whole point of the function: an unresolved
// descriptor denies, then an explicit deny denies, then an allow allows.
func (perm Permissions) Allows(p *Principal) bool {
	if p == nil || perm.Mode == ModeUnknown {
		return false
	}
	if perm.matchesUser(p, perm.DenyUsers) || perm.matchesGroup(p, perm.DenyGroups) {
		return false
	}
	if perm.isOwner(p) {
		return true
	}
	switch perm.Mode {
	case ModeOwnerOnly:
		return false
	case ModePublicToTenant:
		return true
	case ModeACL:
		return perm.matchesUser(p, perm.AllowUsers) || perm.matchesGroup(p, perm.AllowGroups)
	default:
		return false
	}
}

func (perm Permissions) isOwner(p *Principal) bool {
	if perm.Owner.Value == "" {
		return false
	}
	return perm.matchesUser(p, []Ref{perm.Owner})
}

// matchesUser and matchesGroup are the same set membership a storage driver
// runs inside its own query, expressed over the key forms in ref.go so that
// there is one definition of the rule rather than one per driver.
func (perm Permissions) matchesUser(p *Principal, refs []Ref) bool {
	return matchesKey(p.UserKeys(), refs, Ref.UserKey)
}

func (perm Permissions) matchesGroup(p *Principal, refs []Ref) bool {
	return matchesKey(p.GroupKeys(), refs, Ref.GroupKey)
}
