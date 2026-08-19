// Package cache is a sharded least recently used cache with an expiry and
// single flight on a miss.
//
// It knows nothing about documents, principals or queries. That is deliberate:
// the rule that a cache key has to name the asker's visibility is a rule about
// what callers put in the key, and a cache that tried to enforce it would need
// to understand the permission model, which would put a second copy of the
// permission model in a package whose job is a map with a lock on it.
//
// # Why it is sharded
//
// The latency budget is stated at sixteen concurrent readers, and a cache
// behind one mutex at sixteen concurrent readers is a queue rather than a
// cache. Sixteen shards keyed on the hash of the key turn that into sixteen
// independent queues of one, which is close enough to none.
//
// It is sixteen rather than a number derived from GOMAXPROCS because the
// contention that matters is the contention the budget was measured at, and a
// cache whose shape changes with the machine is a cache whose measurements do
// not transfer between machines.
//
// # Why single flight is part of it and not a layer on top
//
// A popular key going cold is the case this exists for. Without single flight
// the miss arrives at every reader at once and the backend gets sixteen
// identical queries, which is the moment a cache is least able to help and
// most likely to be blamed. Joining the call that is already running is a few
// lines here and cannot be forgotten at a call site.
package cache

import (
	"container/list"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

// Shards is how many independent maps a cache is split across. See the package
// comment for why it is a constant.
const Shards = 16

// The two expiry values that are not a duration.
const (
	// Off makes the cache hold nothing. Get always misses, Put does nothing and
	// Do calls straight through.
	//
	// It exists because a deployment with a strict staleness requirement has to
	// be able to turn result caching off, and the honest way to offer that is a
	// value of the same setting rather than a second code path. The latency
	// budget is met by the data model, so turning a cache off costs latency and
	// never costs correctness.
	Off time.Duration = 0

	// Forever keeps entries until they are invalidated or evicted. It is for the
	// layers whose staleness is bounded by an explicit drop on write rather than
	// by a clock.
	Forever time.Duration = -1
)

// Cache maps string keys to values of one type.
//
// The zero value is not usable. Use [New]. A cache is safe for concurrent use
// and is meant to be shared.
type Cache[V any] struct {
	shards [Shards]shard[V]
	ttl    time.Duration
	now    func() time.Time

	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64
}

// Option configures a [Cache].
type Option[V any] func(*Cache[V])

// WithClock replaces the clock. It is what lets a test walk an entry past its
// expiry without sleeping through it.
func WithClock[V any](now func() time.Time) Option[V] {
	return func(c *Cache[V]) {
		if now != nil {
			c.now = now
		}
	}
}

// New returns a cache holding at most capacity entries in total, each expiring
// after ttl.
//
// The capacity is divided evenly across the shards, so a long run of keys that
// hash to one shard is evicted rather than being allowed to fill the whole
// cache. That is the price of sharding and it is the right side of the trade:
// the alternative is a global list, which is a global lock, which is the thing
// sharding is for.
func New[V any](capacity int, ttl time.Duration, opts ...Option[V]) *Cache[V] {
	c := &Cache[V]{ttl: ttl, now: time.Now}
	for _, opt := range opts {
		opt(c)
	}
	per := max(1, (capacity+Shards-1)/Shards)
	for i := range c.shards {
		c.shards[i].init(per)
	}
	return c
}

// Get returns the value stored under key.
func (c *Cache[V]) Get(key string) (V, bool) {
	var zero V
	if c.ttl == Off {
		return zero, false
	}
	s := c.shard(key)
	s.mu.Lock()
	v, ok := s.get(key, c.now())
	s.mu.Unlock()
	if ok {
		c.hits.Add(1)
	} else {
		c.misses.Add(1)
	}
	return v, ok
}

// Put stores a value.
func (c *Cache[V]) Put(key string, v V) {
	if c.ttl == Off {
		return
	}
	s := c.shard(key)
	s.mu.Lock()
	if s.put(key, v, c.expiry()) {
		c.evictions.Add(1)
	}
	s.mu.Unlock()
}

// Do returns the value stored under key, calling fn to produce it on a miss.
//
// Concurrent calls for the same key run fn once and all return what it
// produced. An error is returned to every caller waiting on that call and is
// not stored, because a cache that remembers failures turns a moment of backend
// trouble into a minute of it.
func (c *Cache[V]) Do(key string, fn func() (V, error)) (V, error) {
	if c.ttl == Off {
		return fn()
	}
	s := c.shard(key)

	s.mu.Lock()
	if v, ok := s.get(key, c.now()); ok {
		s.mu.Unlock()
		c.hits.Add(1)
		return v, nil
	}
	// The lookup and the decision to become the caller that runs fn happen under
	// the same lock. Checking the map first and taking the lock afterwards would
	// let sixteen readers all miss before any of them registered, which is the
	// herd this is here to prevent.
	if waiting, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		c.misses.Add(1)
		waiting.wg.Wait()
		return waiting.val, waiting.err
	}
	running := &call[V]{}
	running.wg.Add(1)
	s.inflight[key] = running
	s.mu.Unlock()
	c.misses.Add(1)

	running.val, running.err = fn()

	s.mu.Lock()
	delete(s.inflight, key)
	if running.err == nil && s.put(key, running.val, c.expiry()) {
		c.evictions.Add(1)
	}
	s.mu.Unlock()
	running.wg.Done()
	return running.val, running.err
}

// Delete removes keys. Deleting a key that is not there is not an error.
func (c *Cache[V]) Delete(keys ...string) {
	for _, key := range keys {
		s := c.shard(key)
		s.mu.Lock()
		s.remove(key)
		s.mu.Unlock()
	}
}

// Clear empties the cache. It leaves the counters alone, because they describe
// the traffic rather than the contents and a sweep is not a miss.
func (c *Cache[V]) Clear() {
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		s.init(s.cap)
		s.mu.Unlock()
	}
}

// Stats is what one cache layer has done. It is reported on the stats endpoint,
// because a cache nobody can see the hit rate of is a cache nobody can tell is
// broken.
type Stats struct {
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	Evictions int64 `json:"evictions"`
	Entries   int   `json:"entries"`
}

// Ratio is the fraction of lookups that hit, and zero when there were none.
func (s Stats) Ratio() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

// Stats reports the counters and the current size.
func (c *Cache[V]) Stats() Stats {
	s := Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
	}
	for i := range c.shards {
		sh := &c.shards[i]
		sh.mu.Lock()
		s.Entries += sh.ll.Len()
		sh.mu.Unlock()
	}
	return s
}

func (c *Cache[V]) expiry() time.Time {
	if c.ttl == Forever {
		return time.Time{}
	}
	return c.now().Add(c.ttl)
}

func (c *Cache[V]) shard(key string) *shard[V] {
	h := fnv.New32a()
	// Writing to a hash never fails, and the errcheck linter would rather be
	// told that here than at every call site.
	_, _ = h.Write([]byte(key))
	return &c.shards[h.Sum32()%Shards]
}

// call is one in flight production of a value, and everyone waiting on it.
type call[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

// shard is one independent map, list and lock.
type shard[V any] struct {
	mu       sync.Mutex
	cap      int
	items    map[string]*list.Element
	ll       *list.List
	inflight map[string]*call[V]
}

// entry is what the list holds. The key is in it so that evicting the back of
// the list can find the map entry to delete.
type entry[V any] struct {
	key     string
	val     V
	expires time.Time
}

func (s *shard[V]) init(capacity int) {
	s.cap = capacity
	s.items = make(map[string]*list.Element, capacity)
	s.ll = list.New()
	if s.inflight == nil {
		s.inflight = make(map[string]*call[V])
	}
}

// get returns a live value and moves it to the front. An expired entry is
// dropped on the way past rather than left for a sweeper, which is why there is
// no sweeper.
func (s *shard[V]) get(key string, now time.Time) (V, bool) {
	var zero V
	el, ok := s.items[key]
	if !ok {
		return zero, false
	}
	e := el.Value.(*entry[V])
	if !e.expires.IsZero() && !now.Before(e.expires) {
		s.ll.Remove(el)
		delete(s.items, key)
		return zero, false
	}
	s.ll.MoveToFront(el)
	return e.val, true
}

// put stores a value and reports whether storing it evicted another.
func (s *shard[V]) put(key string, v V, expires time.Time) bool {
	if el, ok := s.items[key]; ok {
		e := el.Value.(*entry[V])
		e.val, e.expires = v, expires
		s.ll.MoveToFront(el)
		return false
	}
	s.items[key] = s.ll.PushFront(&entry[V]{key: key, val: v, expires: expires})
	if s.ll.Len() <= s.cap {
		return false
	}
	if back := s.ll.Back(); back != nil {
		s.ll.Remove(back)
		delete(s.items, back.Value.(*entry[V]).key)
	}
	return true
}

func (s *shard[V]) remove(key string) {
	if el, ok := s.items[key]; ok {
		s.ll.Remove(el)
		delete(s.items, key)
	}
}
