// Package directory resolves a person into the set of groups they are in.
//
// It sits underneath the permission model rather than beside it. A document's
// access rules name groups, a group contains other groups, and the answer to
// "may this person read this" is only as good as the expansion of that graph.
// [acl] compares two lists of strings and is deliberately incapable of asking
// anybody anything. This package is where the asking happens.
//
// # Why the graph is the hard part
//
// Nobody grants access to a person. They grant it to "engineering", which
// contains "platform", which contains "storage", which contains four people and
// a service account, and the person who joined last week is in the index under
// a group nobody named in a rule anywhere. So a membership is a reachability
// question over a directed graph that somebody else maintains, and every real
// directory has three properties that make walking it interesting.
//
// It is big. A person at a large company is in a few hundred groups and the
// tail is in the thousands, and the expansion happens when a session starts, in
// front of somebody waiting for a search box to work.
//
// It has cycles. Not because anybody meant to: two groups end up in each other
// through a third one that was added during a reorganisation, and the directory
// itself does not mind because it never walks the graph the way we do. A walk
// written without a seen set hangs, and it hangs in production rather than in a
// test, because the test directory is a diagram somebody drew on purpose.
//
// It changes. Somebody is removed from a group at eleven and the thing that
// matters is not how fast the change is noticed but that everything derived
// from the old answer stops being used when it is. That is what
// [acl.GroupSet.Version] is for, and this package is where the number comes
// from.
//
// # What a provider has to answer
//
// [Directory] is two lookups, and it is small on purpose. Okta, Entra ID,
// Google Workspace and LDAP disagree about almost everything else, but every
// one of them will say what one subject is directly a member of and what one
// group is directly a member of. Everything above that, the closure, the cycle
// detection, the bound on how much work one expansion may cost and the version
// the answer is stamped with, is written once here and shared, because those
// are exactly the parts that are easy to get subtly wrong and impossible to
// notice from the outside.
//
// A provider that already expands transitively, and Entra ID does, is not
// penalised for it: it returns the closure it was given, the walk finds nothing
// new above it and stops. A provider that only knows direct edges gets the same
// answer for more requests.
//
// # The version is a fingerprint, not a clock
//
// [Expansion.Groups] carries a version, and it is derived from the answer
// rather than read off a timer. It is a hash of every group in the closure and
// of whatever revision the directory gave for each, so it moves when and only
// when the set of groups this person is in moves.
//
// That has two consequences worth stating. A directory with no revision of its
// own still gets correct invalidation, because the group names are in the hash.
// And a membership change somewhere in the directory that does not change this
// person's closure does not invalidate this person's cached state, which is the
// difference between a cache that works at a company with fifty thousand groups
// and one that does not.
//
// # Which way it fails
//
// A directory that cannot be reached fails the expansion. It does not return an
// empty group set, and no caller here turns an error into one, because an empty
// group set is a valid answer that happens to mean "this person is in no groups
// at all" and the whole system is built to trust it. Refusing the request is
// visible and recoverable. Quietly demoting somebody to no groups is a support
// ticket about missing search results and, on the sources that grant to a
// person directly, a partial answer that looks complete.
//
// A subject the directory has never heard of is [ErrNoSubject], and a subject
// the directory holds but has deactivated is [ErrDisabled]. Both refuse. An
// account that was closed on Friday should stop resolving on Friday rather than
// when the last cache entry expires.
//
// A group named in a membership that the directory does not hold is the one
// case that does not refuse. The directory has said this person is in it, which
// is a statement about them, and the only thing missing is what that group is
// itself a member of. Dropping the group would take away access the directory
// granted, so it is kept, its parents are not walked, and it is reported in
// [Expansion.Unknown] so that an inconsistent directory is something an
// operator can see rather than something a user reports.
package directory

import (
	"context"
	"errors"

	"github.com/tamnd/genba/acl"
)

// Errors a [Directory] returns for the two cases a caller has to tell apart
// from a directory that could not answer at all.
//
// Anything else is an outage: a timeout, a refusal, a rate limit, a certificate
// nobody renewed. Those fail the expansion, and they are meant to.
var (
	// ErrNoSubject means the directory does not hold this subject. It is a
	// complete answer rather than a failure to answer, and it refuses.
	ErrNoSubject = errors.New("directory: no such subject")

	// ErrNoGroup means the directory does not hold this group. It is what makes
	// a group appear in [Expansion.Unknown] rather than failing the expansion.
	ErrNoGroup = errors.New("directory: no such group")

	// ErrDisabled means the subject exists and has been deactivated. It refuses
	// for the same reason ErrNoSubject does.
	ErrDisabled = errors.New("directory: subject is disabled")
)

// Subject is what a directory says about one person, service account or bot.
type Subject struct {
	// ID is how this directory names them, and it is what was asked for.
	ID string

	// Name and Email are for display and for the audit record. Neither is used
	// to decide anything.
	Name  string
	Email string

	// Version is the directory's own revision of this subject, if it keeps one.
	// It is an opaque string that changes when their memberships change, and an
	// empty one is fine: see the note about fingerprints in the package
	// documentation.
	Version string

	// Identities is the same person in the systems being indexed, which is what
	// lets a rule naming a Slack member id apply to somebody who signed in with
	// an email address. A directory that cannot say is not required to.
	Identities []acl.Identity

	// MemberOf is the groups this subject is directly in, named the way this
	// directory names groups. The expansion turns it into the closure.
	MemberOf []string

	// Disabled is an account that exists and has been deactivated. It is
	// separate from the subject being absent because directories rarely delete
	// anybody, and an offboarded account that still resolves is the difference
	// between a leaver and a leaver with a search session.
	Disabled bool
}

// Group is what a directory says about one group.
//
// It deliberately does not carry the members. Listing them is the expensive
// direction, it is the one large directories rate limit hardest, and nothing
// here needs it: the walk goes upwards from a person, not downwards from a
// group.
type Group struct {
	// ID is how this directory names the group, and it is the value that ends
	// up in [acl.GroupSet.Members] with the directory's name in front of it.
	ID string

	// Name is for display. A rule is never matched against it, because a
	// display name is a thing people rename.
	Name string

	// Version is the directory's revision of this group's membership, if it
	// keeps one, and it goes into the fingerprint.
	Version string

	// MemberOf is the groups this group is directly in. This is the edge that
	// makes the whole thing a graph rather than a list.
	MemberOf []string
}

// Directory is one identity provider.
//
// Both methods are lookups of one thing by its id, and that is the whole
// interface. See the package documentation for why it is this small.
//
// An implementation must be safe for concurrent use. The expansion looks up a
// level of the graph in parallel, because the alternative is a person in three
// hundred groups waiting for three hundred round trips one after another.
type Directory interface {
	// Name is the identity source these ids belong to, and it is the same
	// string a connector puts in [acl.Ref.Source] for a group. It is what makes
	// "engineering" from one provider and "engineering" from another two
	// different groups, which they are.
	Name() string

	// Subject returns one subject by id. It returns [ErrNoSubject] if the
	// directory does not hold them.
	Subject(ctx context.Context, id string) (Subject, error)

	// Group returns one group by id. It returns [ErrNoGroup] if the directory
	// does not hold it.
	Group(ctx context.Context, id string) (Group, error)
}
