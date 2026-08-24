package directory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tamnd/genba/acl"
)

// Multi expands one subject against several directories and unions the answers.
//
// The shape it exists for is a company that acquired another company. There are
// two identity providers, nobody is going to merge them this quarter, and there
// is one search box. Half the people are in one directory, half are in the
// other, and a few are in both because they were given an account on the other
// side during the integration.
//
// Nothing collides, because a group key already carries the name of the
// directory it came from. "engineering" at one company and "engineering" at the
// other are two different groups here for the same reason they are two
// different groups in real life.
//
// # A partial answer is the one thing it will not return
//
// If any directory fails, the expansion fails. This is the same rule the walk
// inside one directory follows and it matters more here, not less: with several
// providers there are several things that can be having a bad day, and a person
// whose second directory timed out looks exactly like a person who is only in
// the first one. Serving them the groups that did answer would silently take
// away half of what they can read, with nothing anywhere to say why.
//
// A directory that does not hold the subject at all is not a failure. That is
// the ordinary case, since most people are in one of them, and it contributes
// nothing to the union. A subject every directory refuses is [ErrNoSubject].
//
// A subject one directory holds and has deactivated refuses the whole
// expansion, even if another directory still has them active. Deactivating an
// account is a statement somebody made on purpose, and the alternative is that
// closing an account on Friday does nothing because a directory nobody has
// tidied up still says yes. During a migration, take the old directory out of
// the list rather than leaving deactivated accounts in it.
//
// # Where the cache goes
//
// Above this, not below it. Wrapping a [Multi] in one [Cache] gives one entry
// per person and one lifetime to reason about. Wrapping each directory
// separately gives the same staleness bound and several times the entries, and
// it makes an expansion a hit only when every part of it is.
//
// A Multi is safe for concurrent use.
type Multi struct {
	parts []Expander
	name  string
}

// NewMulti returns a directory that unions the ones it is given.
//
// The order matters for one thing only: where the display name and the email
// come from when more than one directory holds the same person. It is the first
// one that answers.
func NewMulti(parts ...Expander) (*Multi, error) {
	if len(parts) == 0 {
		return nil, errors.New("directory: a union needs at least one directory")
	}
	names := make([]string, 0, len(parts))
	for i, p := range parts {
		if p == nil {
			return nil, fmt.Errorf("directory: directory %d of %d is nil", i+1, len(parts))
		}
		name := p.Name()
		if name == "" {
			return nil, fmt.Errorf("directory: directory %d of %d has no name", i+1, len(parts))
		}
		// Two directories under one name would put both companies' groups under
		// the same key, so a rule naming one of them would match the other. That
		// is the whole thing this type is built not to do.
		if slices.Contains(names, name) {
			return nil, fmt.Errorf("directory: two of these directories are named %q, so their groups would be the same groups", name)
		}
		names = append(names, name)
	}
	return &Multi{parts: slices.Clone(parts), name: strings.Join(names, "+")}, nil
}

// Name is every directory in the union, joined, which is what ends up in a log
// line and in the startup message. It is not an identity source: the sources
// are the individual directories, and they are what the group keys carry.
func (m *Multi) Name() string { return m.name }

// Directories names the directories in the union, in order.
func (m *Multi) Directories() []string {
	out := make([]string, len(m.parts))
	for i, p := range m.parts {
		out[i] = p.Name()
	}
	return out
}

// Counters adds up the numbers of whichever directories keep them.
//
// They are the directories' own counts rather than this one's, so a person in
// two of them is two expansions here and one sign in. That is the number worth
// having, because what these bound is the load on the providers.
func (m *Multi) Counters() Counters {
	var out Counters
	for _, p := range m.parts {
		c, ok := p.(interface{ Counters() Counters })
		if !ok {
			continue
		}
		n := c.Counters()
		out.Expansions += n.Expansions
		out.Failures += n.Failures
		out.Lookups += n.Lookups
		out.Missing += n.Missing
		out.Disabled += n.Disabled
		out.Unresolved += n.Unresolved
	}
	return out
}

// Expand resolves one subject against every directory at once.
//
// Together rather than one after another, because these are separate services
// over separate networks and doing them in turn makes a sign in cost the sum of
// two round trips for no reason. The first real failure cancels the rest, since
// their answers are about to be thrown away.
func (m *Multi) Expand(ctx context.Context, id string) (Expansion, error) {
	if err := ctx.Err(); err != nil {
		return Expansion{}, fmt.Errorf("directory %s: %q: %w", m.name, id, err)
	}

	type answer struct {
		got  Expansion
		err  error
		held bool
	}
	out := make([]answer, len(m.parts))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for i, p := range m.parts {
		wg.Go(func() {
			got, err := p.Expand(ctx, id)
			switch {
			case errors.Is(err, ErrNoSubject):
				// Not a failure. Most people are in one directory of several, so
				// the ones who are not in this one are the ordinary case rather
				// than something to report.
			case err != nil:
				out[i].err = err
				cancel()
			default:
				out[i].got, out[i].held = got, true
			}
		})
	}
	wg.Wait()

	// In order, and a real failure ahead of a cancellation, so that the same two
	// directories in the same state produce the same message twice running and
	// the message names what went wrong rather than what it caused. The error is
	// returned as the directory wrote it, because that one already says which
	// provider and which subject.
	for _, a := range out {
		if a.err != nil && !errors.Is(a.err, context.Canceled) {
			return Expansion{}, a.err
		}
	}
	for _, a := range out {
		if a.err != nil {
			return Expansion{}, a.err
		}
	}

	var (
		merged   Expansion
		members  []string
		unknown  []string
		sources  []string
		versions = make(map[string]string, len(m.parts))
		answered bool
	)
	for i, a := range out {
		if !a.held {
			continue
		}
		source := m.parts[i].Name()
		sources = append(sources, source)

		// The part's version is already a fingerprint of its own closure, so
		// hashing the versions is hashing the answer. A directory that stops
		// holding this person drops out of the list, which moves the number too.
		versions[source] = strconv.FormatUint(a.got.Groups.Version, 10)

		members = append(members, a.got.Groups.Members...)
		for _, g := range a.got.Unknown {
			// Qualified, unlike the single directory case, because two providers
			// can each be missing a group called "engineering" and an operator
			// reading the list has to know which one to go and look at.
			unknown = append(unknown, acl.Ref{Source: source, Value: g}.GroupKey())
		}
		merged.Lookups += a.got.Lookups
		merged.Depth = max(merged.Depth, a.got.Depth)

		if !answered {
			answered = true
			merged.Subject.ID = cmp.Or(a.got.Subject.ID, id)
			merged.Subject.Name = a.got.Subject.Name
			merged.Subject.Email = a.got.Subject.Email
		}
		// Identities are unioned across all of them rather than taken from the
		// first, because the rule that matters may name a Slack member id that
		// only the other directory knows about.
		for _, ident := range a.got.Subject.Identities {
			if !slices.Contains(merged.Subject.Identities, ident) {
				merged.Subject.Identities = append(merged.Subject.Identities, ident)
			}
		}
	}
	if !answered {
		return Expansion{}, fmt.Errorf("directory %s: %q: %w", m.name, id, ErrNoSubject)
	}

	// Subject.MemberOf is deliberately left empty. It is a list of ids in one
	// directory's own naming, and there is no naming here that several of them
	// share. Everything it would have said is in Groups, with the source on it.

	sort.Strings(members)
	sort.Strings(unknown)
	merged.Groups = acl.GroupSet{
		Version: fingerprint(m.name, "", sources, versions),
		Members: slices.Compact(members),
	}
	merged.Unknown = slices.Compact(unknown)
	return merged, nil
}
