package directory

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/tamnd/genba/acl"
)

// Defaults for a [Resolver].
const (
	// DefaultWidth is how many lookups one expansion has in flight at once.
	//
	// It is the number that decides whether a person in three hundred groups
	// waits for one round trip or for three hundred, and it is not larger
	// because the far side is somebody else's rate limit. A directory is shared
	// by every session that starts, so an expansion that goes as wide as it can
	// is an expansion that makes every other expansion slower.
	DefaultWidth = 16

	// DefaultMaxGroups is the most groups one expansion may reach before it is
	// refused.
	//
	// It is a bound on a walk over a graph somebody else maintains, which is
	// the reason it exists at all: without one, a directory that has grown a
	// pathological shape turns one sign in into an unbounded amount of work
	// against a service everybody else is also using.
	//
	// Ten thousand is far past what anybody is legitimately in. The tail of a
	// large company is in the low thousands, so this is a limit that catches a
	// mistake rather than a limit somebody has to think about.
	DefaultMaxGroups = 10000
)

// ErrTooManyGroups is returned when an expansion reaches [Option] WithMaxGroups
// entries.
//
// It refuses rather than returning what it had. A truncated group set is not a
// smaller answer, it is a wrong one, and the way it is wrong is that somebody
// stops seeing documents they can read with no error anywhere to explain it.
var ErrTooManyGroups = errors.New("directory: too many groups")

// Expansion is one resolved subject.
type Expansion struct {
	// Subject is what the directory said about them, unchanged.
	Subject Subject

	// Groups is the transitive closure, in the form [acl.Permissions] compares
	// against, with a version derived from the closure itself.
	Groups acl.GroupSet

	// Unknown is the groups the directory said this subject is in, directly or
	// through another group, and then did not hold when asked about. They are
	// in Groups, because the directory's statement that somebody is a member is
	// a statement about them, and whatever those groups are themselves members
	// of is not in Groups, because nothing could be asked.
	//
	// A non empty Unknown is an inconsistent directory and worth an alert. It
	// is not worth failing a sign in over, which is why it is a field rather
	// than an error.
	Unknown []string

	// Lookups is how many requests the directory answered, the subject
	// included. It is what the latency of an expansion is a function of.
	Lookups int

	// Depth is how many levels of nesting were walked. One means the subject's
	// groups are in no groups themselves.
	Depth int
}

// Apply puts the expansion onto a principal.
//
// It sets the group set and merges in any identities the directory knew that
// the principal did not already carry. It does not touch the tenant, the roles
// or the subject, because none of those is a directory's business.
func (e Expansion) Apply(p *acl.Principal) {
	if p == nil {
		return
	}
	p.Groups = e.Groups
	for _, id := range e.Subject.Identities {
		if !slices.Contains(p.Identities, id) {
			p.Identities = append(p.Identities, id)
		}
	}
}

// Counters are the numbers one resolver has produced since it was made.
type Counters struct {
	// Expansions is how many completed, and Failures how many did not. A
	// failure is a directory that could not answer, not a subject it does not
	// hold.
	Expansions uint64
	Failures   uint64

	// Lookups is every request made of the directory, which is the number that
	// tells an operator what the directory is being asked to carry.
	Lookups uint64

	// Missing is subjects the directory does not hold, Disabled is subjects it
	// holds and has deactivated, and Unresolved is groups named in a membership
	// that the directory did not hold when asked. The last one is the number to
	// alert on: the other two are ordinary.
	Missing    uint64
	Disabled   uint64
	Unresolved uint64
}

// Resolver expands subjects against one directory.
//
// It holds no cache of its own. That is deliberate and it is not an oversight:
// caching a group membership correctly is a harder problem than expanding one,
// it needs the version this produces in order to be invalidated, and putting
// the two in the same type is how the cache ends up with a timeout instead.
// [Cache] is the layer, it wraps this, and everything above takes either.
//
// A Resolver is safe for concurrent use.
type Resolver struct {
	dir   Directory
	width int
	max   int

	expansions atomic.Uint64
	failures   atomic.Uint64
	lookups    atomic.Uint64
	missing    atomic.Uint64
	disabled   atomic.Uint64
	unresolved atomic.Uint64
}

// Option changes a [Resolver].
type Option func(*Resolver)

// WithWidth sets how many lookups one expansion has in flight at once. A width
// below one selects [DefaultWidth].
func WithWidth(n int) Option {
	return func(r *Resolver) {
		if n < 1 {
			n = DefaultWidth
		}
		r.width = n
	}
}

// WithMaxGroups sets the most groups one expansion may reach before it is
// refused with [ErrTooManyGroups]. A bound below one selects
// [DefaultMaxGroups].
func WithMaxGroups(n int) Option {
	return func(r *Resolver) {
		if n < 1 {
			n = DefaultMaxGroups
		}
		r.max = n
	}
}

// New returns a resolver over one directory.
func New(d Directory, opts ...Option) (*Resolver, error) {
	if d == nil {
		return nil, errors.New("directory: a directory is required")
	}
	if d.Name() == "" {
		return nil, errors.New("directory: a directory must have a name")
	}
	r := &Resolver{dir: d, width: DefaultWidth, max: DefaultMaxGroups}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Name is the identity source this resolver's groups belong to.
func (r *Resolver) Name() string { return r.dir.Name() }

// Counters returns the numbers so far.
func (r *Resolver) Counters() Counters {
	return Counters{
		Expansions: r.expansions.Load(),
		Failures:   r.failures.Load(),
		Lookups:    r.lookups.Load(),
		Missing:    r.missing.Load(),
		Disabled:   r.disabled.Load(),
		Unresolved: r.unresolved.Load(),
	}
}

// Expand resolves one subject into the groups they are in.
//
// The walk is breadth first, one level at a time, with the lookups in a level
// running together. Breadth first rather than depth first is what makes the
// concurrency worth having: the groups at one level are independent of each
// other, and the ones at the level above cannot be named until this level has
// answered.
//
// A group already seen is never looked up again, which is both the cycle
// detection and most of the speed. Real directories converge hard: a few
// hundred groups at the bottom reach the same dozen at the top.
func (r *Resolver) Expand(ctx context.Context, id string) (Expansion, error) {
	// Checked here rather than left to the directory, because whether an
	// adapter looks at the context is up to the adapter and whether an
	// expansion runs after the request that wanted it went away is not.
	if err := ctx.Err(); err != nil {
		return Expansion{}, fmt.Errorf("directory %s: %q: %w", r.dir.Name(), id, err)
	}

	sub, err := r.dir.Subject(ctx, id)
	r.lookups.Add(1)
	switch {
	case errors.Is(err, ErrNoSubject):
		r.missing.Add(1)
		r.failures.Add(1)
		return Expansion{}, fmt.Errorf("directory %s: %q: %w", r.dir.Name(), id, err)
	case err != nil:
		r.failures.Add(1)
		return Expansion{}, fmt.Errorf("directory %s: %q: %w", r.dir.Name(), id, err)
	}
	if sub.Disabled {
		r.disabled.Add(1)
		r.failures.Add(1)
		return Expansion{}, fmt.Errorf("directory %s: %q: %w", r.dir.Name(), id, ErrDisabled)
	}
	if sub.ID == "" {
		sub.ID = id
	}

	var (
		// seen is the closure and the cycle detection in one. A group is put in
		// it when it is scheduled rather than when it is answered, so a group
		// two of its children both point at is looked up once.
		seen     = make(map[string]string)
		unknown  []string
		frontier = dedupe(sub.MemberOf)
		lookups  = 1
		depth    int
	)
	for _, g := range frontier {
		seen[g] = ""
	}
	if len(seen) > r.max {
		r.failures.Add(1)
		return Expansion{}, fmt.Errorf("directory %s: %q is directly in %d groups: %w", r.dir.Name(), id, len(seen), ErrTooManyGroups)
	}

	for len(frontier) > 0 {
		if err := ctx.Err(); err != nil {
			r.failures.Add(1)
			return Expansion{}, fmt.Errorf("directory %s: expanding %q: %w", r.dir.Name(), id, err)
		}
		depth++
		found, err := r.level(ctx, frontier)
		lookups += len(frontier)
		r.lookups.Add(uint64(len(frontier)))
		if err != nil {
			r.failures.Add(1)
			return Expansion{}, fmt.Errorf("directory %s: expanding %q: %w", r.dir.Name(), id, err)
		}

		var next []string
		for i, got := range found {
			if got.absent {
				unknown = append(unknown, frontier[i])
				continue
			}
			seen[frontier[i]] = got.group.Version
			for _, up := range got.group.MemberOf {
				if up == "" {
					continue
				}
				if _, ok := seen[up]; ok {
					continue
				}
				seen[up] = ""
				next = append(next, up)
			}
		}
		if len(seen) > r.max {
			r.failures.Add(1)
			return Expansion{}, fmt.Errorf("directory %s: %q reaches at least %d groups: %w", r.dir.Name(), id, len(seen), ErrTooManyGroups)
		}
		frontier = next
	}

	ids := make([]string, 0, len(seen))
	for g := range seen {
		ids = append(ids, g)
	}
	sort.Strings(ids)
	sort.Strings(unknown)

	members := make([]string, len(ids))
	for i, g := range ids {
		members[i] = acl.Ref{Source: r.dir.Name(), Value: g}.GroupKey()
	}

	r.expansions.Add(1)
	r.unresolved.Add(uint64(len(unknown)))

	return Expansion{
		Subject: sub,
		Groups: acl.GroupSet{
			Version: fingerprint(r.dir.Name(), sub.Version, ids, seen),
			Members: members,
		},
		Unknown: unknown,
		Lookups: lookups,
		Depth:   depth,
	}, nil
}

// answer is one group lookup, with absent kept apart from an error because the
// two mean opposite things: absent is the directory answering, and an error is
// the directory not answering.
type answer struct {
	group  Group
	absent bool
}

// level looks up one breadth first level, together.
//
// The first failure cancels the rest, because they are all about to be thrown
// away: an expansion that cannot see the whole graph does not return part of
// it. The others are still waited for, so that nothing is left running against
// the directory after this returns.
func (r *Resolver) level(ctx context.Context, ids []string) ([]answer, error) {
	out := make([]answer, len(ids))
	errs := make([]error, len(ids))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg   sync.WaitGroup
		sem  = make(chan struct{}, min(r.width, len(ids)))
		fail atomic.Bool
	)
	for i, id := range ids {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if fail.Load() {
				// Somebody else has already failed the level, so this lookup
				// would be discarded. Not making it is one less request against
				// a directory that may be the thing having the bad day, and it
				// records nothing because the failure is already recorded.
				return
			}
			g, err := r.dir.Group(ctx, id)
			switch {
			case errors.Is(err, ErrNoGroup):
				out[i] = answer{absent: true}
			case err != nil:
				errs[i] = err
				fail.Store(true)
				cancel()
			default:
				if g.ID == "" {
					g.ID = id
				}
				out[i] = answer{group: g}
			}
		})
	}
	wg.Wait()

	// In order rather than whichever failed first, so that the same directory
	// in the same state produces the same message twice running. A real failure
	// is preferred over a cancellation because the cancellation is very often
	// this level reacting to that failure, and the message an operator reads
	// should name the thing that went wrong rather than the consequence.
	for i, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("group %q: %w", ids[i], err)
		}
	}
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("group %q: %w", ids[i], err)
		}
	}
	return out, nil
}

// fingerprint is the version stamped on a group set.
//
// It is a hash of the answer: the source, the subject's own revision, and every
// group in the closure with whatever revision the directory gave for it. See
// the package documentation for why it is derived rather than counted.
//
// Zero is avoided because a version of zero is what an unset field looks like,
// and a cache key that cannot tell "not expanded yet" from "expanded, and the
// hash happened to be zero" is a cache key with one very rare bug in it.
func fingerprint(source, subject string, ids []string, versions map[string]string) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%s\x00", source, subject)
	for _, id := range ids {
		fmt.Fprintf(h, "%s\x00%s\x00", id, versions[id])
	}
	if v := h.Sum64(); v != 0 {
		return v
	}
	return 1
}

// dedupe returns the ids once each, in the order they first appeared, with the
// empty ones dropped.
func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || slices.Contains(out, v) {
			continue
		}
		out = append(out, v)
	}
	return out
}
