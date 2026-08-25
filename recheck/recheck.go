// Package recheck asks the source whether somebody may still read a document,
// at the moment the answer is about to be put in front of them.
//
// Indexed permissions are a snapshot, and a snapshot is stale from the moment
// it is taken. Between the sync that recorded who could read a document and the
// query that returns it, somebody left the company, a folder was moved, a share
// was withdrawn. The index cannot know until it looks again, and looking again
// at everything is a crawl.
//
// For the sources that can answer one cheap question, the window closes here
// instead. A page of results is checked against the source before it is
// returned, under a timeout that binds the whole page rather than each row, and
// a check that does not come back removes the row. That last part is the whole
// design: the failure mode of this package is showing somebody less than they
// are entitled to, which is a support ticket, rather than showing them more,
// which is the thing the product cannot do once.
//
// It is per source and off by default. A source with no checker registered is
// left exactly as the index answered, because a deployment that has not said
// its source can answer this quickly should not have its search latency decided
// by somebody else's API.
package recheck

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/cache"
)

// Defaults for a set built with no options.
const (
	// DefaultTimeout is how long the whole page may spend being checked.
	//
	// It is a budget for the request rather than for one source: a page that
	// spans four sources asks all four at once and they share this. Twenty
	// milliseconds is chosen against what it costs to be wrong. A check that
	// misses its deadline removes the row, so a generous timeout hides a slow
	// source and a mean one silently empties a page, and this is the point
	// where an operator would rather see the latency and be told about it.
	DefaultTimeout = 20 * time.Millisecond

	// DefaultTTL is how long an answer from a source is reused.
	//
	// Everything this package is for happens inside this window: a revocation
	// takes at most this long to be seen, rather than as long as the interval
	// between syncs. Ten seconds is short enough that nobody reasons about it
	// and long enough that paging through results is one question per document
	// rather than one per page.
	DefaultTTL = 10 * time.Second

	// DefaultCapacity is how many answers are held. An answer is one boolean
	// under a short key, so this is a few megabytes at the top end and it is
	// sized for the working set of a busy afternoon rather than for the corpus.
	DefaultCapacity = 50_000
)

// Item is one document as far as this package is concerned.
//
// An id and where it came from, and nothing else. A checker is being asked a
// question about the source's own access control list, so the title, the body
// and the permissions this index recorded are all beside the point, and passing
// them would invite a checker to answer out of them.
type Item struct {
	ID     string
	Source string
}

// Checker answers whether a principal may still read documents of one source.
//
// The ids are the source's own ids for the documents, which are what the index
// stores. An implementation is asked about a page at a time and is expected to
// answer for all of them in one call, because the whole point of running this
// on the request path is that it costs one round trip rather than ten.
//
// An id left out of the returned map is not an allow. It is a failure for that
// id, and the row goes. An implementation that cannot answer about one document
// says so by leaving it out, and does not have to decide on its own what the
// safe answer is.
type Checker interface {
	Allowed(ctx context.Context, p *acl.Principal, ids []string) (map[string]bool, error)
}

// Func is a Checker written as a function, for a source whose check is one
// call and a map.
type Func func(ctx context.Context, p *acl.Principal, ids []string) (map[string]bool, error)

// Allowed implements [Checker].
func (f Func) Allowed(ctx context.Context, p *acl.Principal, ids []string) (map[string]bool, error) {
	return f(ctx, p, ids)
}

// Stats is what the checks for one source have done.
//
// Denied and Failed are counted apart because they call for different actions.
// A denial is the feature working: the index was out of date and somebody was
// not shown a document they can no longer read. A failure is the feature
// costing somebody a result they are entitled to, and a deployment where that
// number is not near zero is one where the source is too slow to be asked on
// the request path.
type Stats struct {
	Checked int64 `json:"checked"`
	Denied  int64 `json:"denied"`
	Failed  int64 `json:"failed"`
}

// Set is the checkers a deployment has registered, and the cache and the
// budget they share.
type Set struct {
	mu       sync.RWMutex
	checkers map[string]Checker
	counts   map[string]*counters

	timeout time.Duration
	answers *cache.Cache[bool]
}

// counters are the atomics behind [Stats], one set per source.
type counters struct {
	checked atomic.Int64
	denied  atomic.Int64
	failed  atomic.Int64
}

// Option configures a set.
type Option func(*config)

// config is the settable part of a set, kept apart so that the cache is built
// once the lifetime and the capacity are both known.
type config struct {
	timeout  time.Duration
	ttl      time.Duration
	capacity int
	now      func() time.Time
}

// WithTimeout sets how long a whole page may spend being checked.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// WithCache sets how many answers are held and for how long.
//
// A ttl of zero turns the cache off, which asks the source about every row of
// every page. It is not the setting to reach for on a source anybody uses, and
// it is the honest one for a source whose permissions have to be exact to the
// second.
func WithCache(capacity int, ttl time.Duration) Option {
	return func(c *config) { c.capacity, c.ttl = capacity, ttl }
}

// WithClock replaces the clock the cache ages entries against, for tests.
func WithClock(now func() time.Time) Option {
	return func(c *config) { c.now = now }
}

// New returns a set with no checkers in it, which checks nothing.
//
// That is the state a deployment starts in and stays in until it names a source
// that can answer. See [Set.Add].
func New(opts ...Option) *Set {
	c := config{timeout: DefaultTimeout, ttl: DefaultTTL, capacity: DefaultCapacity, now: time.Now}
	for _, opt := range opts {
		opt(&c)
	}
	var copts []cache.Option[bool]
	if c.now != nil {
		copts = append(copts, cache.WithClock[bool](c.now))
	}
	return &Set{
		checkers: map[string]Checker{},
		counts:   map[string]*counters{},
		timeout:  c.timeout,
		answers:  cache.New[bool](c.capacity, c.ttl, copts...),
	}
}

// Add registers the checker for one source, replacing any it had.
//
// It can be called after the server is serving, because a connector configured
// from the administration screen arrives that way, and a source that gains a
// checker mid afternoon starts being checked on the next query rather than on
// the next deployment.
func (s *Set) Add(source string, c Checker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c == nil {
		delete(s.checkers, source)
		return
	}
	s.checkers[source] = c
	if _, ok := s.counts[source]; !ok {
		s.counts[source] = &counters{}
	}
}

// Sources are the sources that are checked, which is what an operator wants to
// see next to the answer that a deployment has this turned on.
func (s *Set) Sources() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.checkers))
	for source := range s.checkers {
		out = append(out, source)
	}
	return out
}

// Allowed returns which of these items may still be shown.
//
// The map holds an entry for every distinct id it was given, so a caller filters
// with a plain lookup and cannot accidentally read an allow out of a missing
// key. An item whose source has no checker is allowed, because the index has
// already answered for it and this package was not asked to have an opinion.
//
// Every source is asked at once and they share one deadline. What has not come
// back when it expires is denied and counted as a failure, and the request goes
// on: the caller is holding a page somebody is waiting for, and the answer to a
// source that has stopped responding is to show the rows that were checked
// rather than to hold the response open.
func (s *Set) Allowed(ctx context.Context, p *acl.Principal, items []Item) map[string]bool {
	out := make(map[string]bool, len(items))
	if len(items) == 0 {
		return out
	}

	// Grouped by source, deduplicated, and with everything nobody can answer
	// about already allowed. A page that spans no checked source leaves here
	// without touching the clock.
	ask := map[string][]string{}
	for _, it := range items {
		if _, done := out[it.ID]; done {
			continue
		}
		if _, ok := s.checker(it.Source); !ok {
			out[it.ID] = true
			continue
		}
		if allowed, hit := s.answers.Get(key(p, it.Source, it.ID)); hit {
			out[it.ID] = allowed
			s.record(it.Source, allowed)
			continue
		}
		// Denied until something says otherwise, which is what makes every
		// path out of here below fail closed, including the one that returns
		// early because the budget is gone.
		out[it.ID] = false
		ask[it.Source] = append(ask[it.Source], it.ID)
	}
	if len(ask) == 0 {
		return out
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	type answer struct {
		source  string
		allowed map[string]bool
	}
	results := make(chan answer, len(ask))
	for source, ids := range ask {
		c, ok := s.checker(source)
		if !ok {
			// The checker was removed between grouping and asking, which means
			// the deployment has just said it does not want this source
			// checked. The rows stand.
			results <- answer{source: source, allowed: allow(ids)}
			continue
		}
		go func() {
			got, err := c.Allowed(ctx, p, ids)
			if err != nil {
				results <- answer{source: source}
				return
			}
			results <- answer{source: source, allowed: got}
		}()
	}

	for range ask {
		select {
		case got := <-results:
			for _, id := range ask[got.source] {
				allowed, said := got.allowed[id]
				out[id] = said && allowed
				if said {
					s.answers.Put(key(p, got.source, id), allowed)
					s.record(got.source, allowed)
					continue
				}
				// Not cached. A source that could not answer once is a source
				// that will probably answer in a second, and remembering the
				// failure would turn a moment of trouble into a minute of a
				// document nobody can find.
				s.fail(got.source)
			}
		case <-ctx.Done():
			// Whatever is still outstanding is already denied in out, because
			// that is what it was initialised to. Counting it here is what
			// makes the metric say the deployment is over its budget rather
			// than that a source is refusing people.
			s.expire(ask, out)
			return out
		}
	}
	return out
}

// Stats is what has been checked, per source.
func (s *Set) Stats() map[string]Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Stats, len(s.counts))
	for source, c := range s.counts {
		out[source] = Stats{
			Checked: c.checked.Load(),
			Denied:  c.denied.Load(),
			Failed:  c.failed.Load(),
		}
	}
	return out
}

// CacheStats is the answer cache, published as a layer beside the others.
func (s *Set) CacheStats() cache.Stats { return s.answers.Stats() }

// checker returns the checker for one source.
func (s *Set) checker(source string) (Checker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.checkers[source]
	return c, ok
}

// record counts one answered check.
func (s *Set) record(source string, allowed bool) {
	c := s.counter(source)
	c.checked.Add(1)
	if !allowed {
		c.denied.Add(1)
	}
}

// fail counts one check that did not produce an answer.
func (s *Set) fail(source string) {
	c := s.counter(source)
	c.checked.Add(1)
	c.failed.Add(1)
}

// expire counts everything a deadline took, which is every id of every source
// that has not been decided yet.
func (s *Set) expire(ask map[string][]string, out map[string]bool) {
	for source, ids := range ask {
		for _, id := range ids {
			if !out[id] {
				s.fail(source)
			}
		}
	}
}

// counter is the counters of one source, made on first use.
//
// The read lock is taken first because this is on the request path once per
// checked document, and the write lock is only ever reached for a source whose
// first check is happening right now.
func (s *Set) counter(source string) *counters {
	s.mu.RLock()
	c, ok := s.counts[source]
	s.mu.RUnlock()
	if ok {
		return c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.counts[source]; ok {
		return c
	}
	c = &counters{}
	s.counts[source] = c
	return c
}

// key is one cached answer.
//
// The subject and the tenant are in it because the answer is about a person,
// and the source is in it because two sources can name a document the same
// thing. The groups this index resolved are deliberately not in it: the source
// is being asked about its own access control list, and what our directory
// thinks is not part of that question.
func key(p *acl.Principal, source, id string) string {
	if p == nil {
		return "\x00" + source + "\x00" + id
	}
	return p.Tenant + "\x00" + p.Subject + "\x00" + source + "\x00" + id
}

// allow is every id, allowed.
func allow(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}
