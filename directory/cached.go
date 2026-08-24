package directory

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/cache"
)

// Defaults for a [Cache].
const (
	// DefaultTTL is how long a resolved expansion is served for, and therefore
	// how long after a membership change somebody can still be acting on the
	// old one.
	//
	// A minute is a compromise between two things that pull in opposite
	// directions. Long is what makes the cache worth having, because a session
	// makes many requests a minute and every one of them would otherwise be a
	// walk over somebody else's directory. Short is what keeps the window in
	// which a person removed from a group still has its documents down to
	// something an operator can say out loud.
	//
	// It is a minute rather than five because this is a permission input. The
	// hit rate barely moves between the two, since the traffic that matters is
	// one person making forty requests while they are looking at something.
	DefaultTTL = time.Minute

	// DefaultCapacity is how many subjects are held. Reaching it evicts the
	// least recently used, which for this cache means the people who have
	// stopped asking.
	DefaultCapacity = 10000
)

// Expander turns a subject into the groups they are in.
//
// It is what the edge of the API needs, and both [Resolver] and [Cache] satisfy
// it, so a deployment adds or removes caching without anything above it
// changing.
type Expander interface {
	// Name is the identity source the groups belong to.
	Name() string

	// Expand resolves one subject.
	Expand(ctx context.Context, id string) (Expansion, error)
}

// Cache is an [Expander] that remembers what the directory said.
//
// Expanding on every request is not affordable. A person in three hundred
// groups costs three hundred lookups against a service shared by everybody
// else, and a search box that feels instant cannot start by doing that.
//
// # There is exactly one cache and this is it
//
// The obvious second layer, remembering what each group is a member of, is
// deliberately absent. It would help, because real directories converge and the
// same dozen groups sit above everybody. It would also mean an answer could be
// built out of edges that were themselves already a minute old, so the worst
// case age of a group set would be the sum of two lifetimes rather than one.
// That is a staleness bound nobody can state without drawing a diagram, and a
// staleness bound nobody can state is one nobody is holding anyone to.
//
// So there is one layer, it is the whole expansion, and the maximum age of any
// group set this hands out is the TTL. That is the number, it is configured in
// one place, and [Cache.Staleness] returns it.
//
// # What the version does and what the TTL does
//
// These are two mechanisms doing two different jobs and they are easy to
// confuse.
//
// The TTL bounds how long it takes to notice a membership change. Nothing else
// can, because a directory does not call to say somebody was removed from a
// group, so noticing means asking again.
//
// The version bounds how long anything built on top of a group set outlives it.
// Every bitmap, every filter and every cached result keyed by [acl.GroupSet]
// carries the version in its key, so the moment a refreshed expansion produces
// a different closure, all of it stops matching at once. Without that, a minute
// of staleness here would be an unbounded amount of staleness in every layer
// above, and the person removed from a group would keep its documents until
// something unrelated happened to expire.
//
// A Cache is safe for concurrent use.
type Cache struct {
	res     Expander
	entries *cache.Cache[Expansion]
	ttl     time.Duration
}

// CacheOption changes a [Cache].
type CacheOption func(*cacheConfig)

type cacheConfig struct {
	capacity int
	ttl      time.Duration
	now      func() time.Time
}

// WithTTL sets how long an expansion is served for, which is the maximum
// staleness of every group set the cache hands out. A duration below one
// selects [DefaultTTL].
func WithTTL(d time.Duration) CacheOption {
	return func(c *cacheConfig) {
		if d < 1 {
			d = DefaultTTL
		}
		c.ttl = d
	}
}

// WithCapacity sets how many subjects are held. A capacity below one selects
// [DefaultCapacity].
func WithCapacity(n int) CacheOption {
	return func(c *cacheConfig) {
		if n < 1 {
			n = DefaultCapacity
		}
		c.capacity = n
	}
}

// WithCacheClock replaces the clock, so that a test about a staleness bound
// measures the bound rather than the machine it ran on.
func WithCacheClock(now func() time.Time) CacheOption {
	return func(c *cacheConfig) {
		if now != nil {
			c.now = now
		}
	}
}

// NewCache wraps an expander.
func NewCache(e Expander, opts ...CacheOption) (*Cache, error) {
	if e == nil {
		return nil, errors.New("directory: an expander is required")
	}
	if e.Name() == "" {
		return nil, errors.New("directory: an expander must have a name")
	}
	cfg := cacheConfig{capacity: DefaultCapacity, ttl: DefaultTTL, now: time.Now}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Cache{
		res:     e,
		ttl:     cfg.ttl,
		entries: cache.New(cfg.capacity, cfg.ttl, cache.WithClock[Expansion](cfg.now)),
	}, nil
}

// Name is the identity source the groups belong to.
func (c *Cache) Name() string { return c.res.Name() }

// Staleness is the most out of date any group set this returns can be.
//
// It is a configured number rather than something to be measured, which is the
// point of there being one layer. An operator can answer "how long after I
// remove somebody does it take effect" by reading it, and the answer does not
// depend on the traffic.
func (c *Cache) Staleness() time.Duration { return c.ttl }

// Stats is what the cache has done, for the stats endpoint and the metrics.
func (c *Cache) Stats() cache.Stats { return c.entries.Stats() }

// Counters returns whatever the wrapped expander has counted, so a resolver
// underneath a cache is still measurable. It is the zero value if the expander
// counts nothing.
func (c *Cache) Counters() Counters {
	if counted, ok := c.res.(interface{ Counters() Counters }); ok {
		return counted.Counters()
	}
	return Counters{}
}

// Expand returns the groups a subject is in, from the cache when it has a
// fresh answer and from the directory otherwise.
//
// Concurrent requests for the same subject do one expansion between them. That
// matters more here than in most caches: everybody arrives at nine o'clock, a
// cold cache and a thousand people is a thousand walks over the same directory,
// and the directory is the one part of this that belongs to somebody else.
//
// An error is never stored. A directory that was unreachable for a moment is
// not a fact about a subject, and remembering it would turn a blip into a
// minute of refusals.
func (c *Cache) Expand(ctx context.Context, id string) (Expansion, error) {
	// The cache is asked for a value it does not have a context for, so the
	// cancellation of the request that happens to be the one doing the work is
	// checked here rather than lost.
	if err := ctx.Err(); err != nil {
		return Expansion{}, err
	}
	got, err := c.entries.Do(id, func() (Expansion, error) {
		return c.res.Expand(ctx, id)
	})
	if err != nil {
		return Expansion{}, err
	}
	// A cached answer handed to two requests would otherwise be one slice
	// handed to two requests. Everything above is free to keep a principal, put
	// it in another structure and sort what it carries, and this is a
	// permission set, so it is copied on the way out rather than trusted not to
	// be touched.
	return got.clone(), nil
}

// Forget drops what is held about these subjects, so that the next request for
// one of them asks the directory.
//
// It exists because there are moments when waiting for the TTL is the wrong
// answer and they are the moments that matter. Somebody is taken out of a group
// during an incident, an account is closed, a provider sends a webhook. Those
// are about one person, and flushing everybody to deal with one person means a
// sign in storm against the directory at exactly the time nobody wants one.
//
// Forgetting a subject that is not held is not an error.
func (c *Cache) Forget(ids ...string) { c.entries.Delete(ids...) }

// Clear drops everything. It is for a configuration change that invalidates the
// lot, such as pointing at a different directory, and not for a membership
// change, which is what [Cache.Forget] is.
func (c *Cache) Clear() { c.entries.Clear() }

// clone deep copies the parts of an expansion that anything above could hold on
// to.
func (e Expansion) clone() Expansion {
	out := e
	out.Subject.Identities = slices.Clone(e.Subject.Identities)
	out.Subject.MemberOf = slices.Clone(e.Subject.MemberOf)
	out.Groups = acl.GroupSet{Version: e.Groups.Version, Members: slices.Clone(e.Groups.Members)}
	out.Unknown = slices.Clone(e.Unknown)
	return out
}
