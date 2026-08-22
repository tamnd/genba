package acl

import (
	"slices"
	"strings"
)

// The permission rule is one rule, and this file is where its inputs get their
// canonical form.
//
// [Permissions.Allows] evaluates it in Go, and a storage driver evaluates the
// same rule in whatever query language it speaks, because the filter has to run
// while the driver walks its own data rather than afterwards. Two
// implementations of a rule drift, and the way this one drifts is that somebody
// sees a document. So both sides compare the same strings: a driver stores the
// keys below next to the document, binds the keys of the asking principal into
// its query, and the comparison is set membership on both sides.

// UserKey is how a user reference is written when it is compared.
//
// The source is folded to lower case because the systems we index disagree
// about the case of their own name, and the identifier is left exactly as the
// source gave it because it is theirs to define.
func (r Ref) UserKey() string {
	if r.Source == "" {
		return r.Value
	}
	return strings.ToLower(r.Source) + ":" + r.Value
}

// GroupKey is how a group reference is written when it is compared, which is
// the same form a directory expansion puts in [GroupSet.Members].
//
// Unlike [Ref.UserKey] it does not fold case, because a group name is compared
// against whatever the identity provider called it and folding one half of that
// comparison and not the other is worse than folding neither.
func (r Ref) GroupKey() string {
	if r.Source == "" {
		return r.Value
	}
	return r.Source + ":" + r.Value
}

// UserKeys returns every user reference that names this principal: one per
// identity, plus the internal subject on its own.
//
// The order is stable so that a driver can use the result as part of a cache
// key without sorting it first.
func (p *Principal) UserKeys() []string {
	if p == nil {
		return nil
	}
	keys := make([]string, 0, len(p.Identities)+1)
	if p.Subject != "" {
		keys = append(keys, p.Subject)
	}
	for _, id := range p.Identities {
		if k := Ref(id).UserKey(); k != "" && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// GroupKeys returns the principal's fully expanded group membership, in the
// form group references are compared in.
func (p *Principal) GroupKeys() []string {
	if p == nil {
		return nil
	}
	return p.Groups.Members
}

// matchesKey returns the first reference that names the principal, using the
// key form for the side of the rule it is on, and reports whether one did.
func matchesKey(keys []string, refs []Ref, key func(Ref) string) (Ref, bool) {
	for _, r := range refs {
		if slices.Contains(keys, key(r)) {
			return r, true
		}
	}
	return Ref{}, false
}
