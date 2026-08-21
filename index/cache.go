package index

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/cache"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// Caching comes after the data model, not instead of it.
//
// Every budget in this milestone is met by a query that does the right amount
// of work, and the caches here take the repeated queries out of the path on top
// of that. Turning all of them off is a supported way to run: it costs latency
// and it does not cost correctness, which is the property that makes a cache
// safe to have.
//
// # The rule
//
// A cache key that does not name the asker's visibility is a permission bug.
// Two people typing the same query get different answers, and an entry that
// crossed between them has leaked a document without looking like a leak.
//
// So the result cache is keyed by [acl.Fingerprint] and nothing here is keyed
// by the query alone. The one layer that is not keyed by the fingerprint is the
// document cache, and its justification is in [Cache.document].

// The default sizes and expiries.
const (
	// DefaultResultExpiry is the backstop on how stale an ordering can be.
	//
	// A write drops the orderings of the tenant it touched, so this is not the
	// mechanism that keeps results current. It is what bounds an ordering held
	// by a driver that cannot report its writes, and what keeps a cache from
	// holding a query nobody has asked in an hour.
	//
	// Thirty seconds, and the value cached is the ranked ids, the total and the
	// facets rather than the documents. That is what makes even the backstop
	// honest: a hit still fetches the bodies, so an edited document shows its new
	// text on the very next request. The cache can serve a stale ordering and it
	// cannot serve stale content.
	DefaultResultExpiry = 30 * time.Second

	// DefaultResultEntries is how many orderings are held. An entry is a few
	// hundred candidates, so this is tens of megabytes at the limit and the
	// limit is only reached by a deployment with thousands of distinct live
	// queries, which is a deployment getting value from it.
	DefaultResultEntries = 4_000

	// DefaultCorpusEntries is how many corpus statistics are held. Terms follow
	// a Zipf distribution, so a small cache covers most of the traffic and the
	// tail that misses is the tail that was going to be cheap anyway.
	DefaultCorpusEntries = 50_000

	// DefaultDocumentEntries is how many documents are held. It is the layer
	// that turns paging back and forth through a result set into no work at all.
	DefaultDocumentEntries = 20_000
)

// Cache is the derived state a searcher reuses between requests.
//
// It is safe for concurrent use and is meant to be shared by every request in a
// process. A nil *Cache is not usable: a searcher without one simply does not
// take the option.
type Cache struct {
	results *cache.Cache[store.Ranked]
	corpus  *cache.Cache[store.Corpus]
	docs    *cache.Cache[doc.Document]

	// blind is set when the driver does not report its writes, which turns off
	// the two layers that have no other way to learn that they are wrong.
	blind atomic.Bool

	// gen is the per tenant generation counter that stands in for a sweep.
	//
	// Finding and deleting every key of a tenant in a sharded cache means walking
	// every shard, under every lock, on a write. Bumping a counter that is part
	// of the key makes the old entries unreachable instead, and the least
	// recently used list retires them without anybody having to go looking. It
	// is the same idea the fingerprint uses for a permission change, applied to
	// a corpus change.
	//
	// One counter rather than one per layer, and any committed change bumps it.
	// The tempting optimisation is to let a write keep the cached orderings,
	// since working out which cached queries a new document would have joined
	// costs more than the cache saves, and to bound the staleness with the
	// expiry instead. It is the wrong trade for a search box. It means somebody
	// saves a document, searches for it, and is told it does not exist, for as
	// long as the expiry lasts. That is the complaint that makes people stop
	// trusting a search box, and no hit rate is worth it. What the cache is
	// actually for is the repeated reads between writes, which on any corpus
	// anybody reads is nearly all of the time.
	mu  sync.RWMutex
	gen map[string]uint64
}

// CacheOption configures a [Cache].
type CacheOption func(*cacheConfig)

// cacheConfig is the settings a cache is built from. It exists because the
// expiry of a layer is fixed when the layer is made, so that no entry can
// outlive the policy it was stored under, and options therefore have to be
// collected before anything is built rather than applied to a built cache.
type cacheConfig struct {
	resultExpiry          time.Duration
	results, corpus, docs int
	clock                 func() time.Time
}

// WithResultExpiry sets how long a ranked ordering is reused.
//
// Passing zero turns result caching off. A deployment that cannot tolerate a
// thirty second old ordering is meant to be able to say so and to still meet
// the latency budget afterwards, because the budget is met by the data model
// rather than by the cache.
func WithResultExpiry(d time.Duration) CacheOption {
	return func(c *cacheConfig) { c.resultExpiry = d }
}

// WithCacheCapacity sets how many entries each layer holds. A non positive
// value leaves that layer at its default.
func WithCacheCapacity(results, corpus, documents int) CacheOption {
	return func(c *cacheConfig) {
		if results > 0 {
			c.results = results
		}
		if corpus > 0 {
			c.corpus = corpus
		}
		if documents > 0 {
			c.docs = documents
		}
	}
}

// WithCacheClock replaces the clock the expiries are measured against, which is
// what lets a test walk an entry past its expiry without sleeping through it.
func WithCacheClock(now func() time.Time) CacheOption {
	return func(c *cacheConfig) { c.clock = now }
}

// NewCache returns the three layers a searcher uses.
func NewCache(opts ...CacheOption) *Cache {
	cfg := cacheConfig{
		resultExpiry: DefaultResultExpiry,
		results:      DefaultResultEntries,
		corpus:       DefaultCorpusEntries,
		docs:         DefaultDocumentEntries,
		clock:        time.Now,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Cache{
		results: cache.New(cfg.results, cfg.resultExpiry, cache.WithClock[store.Ranked](cfg.clock)),

		// The corpus statistics and the documents never expire on a clock. They
		// are exact until a write makes them wrong, and a write says so, so an
		// expiry on top would only throw away answers that were still right.
		corpus: cache.New(cfg.corpus, cache.Forever, cache.WithClock[store.Corpus](cfg.clock)),
		docs:   cache.New(cfg.docs, cache.Forever, cache.WithClock[doc.Document](cfg.clock)),

		gen: make(map[string]uint64),
	}
}

// Watch subscribes to a store's writes and returns a function that
// unsubscribes.
//
// A store that does not report its writes is not an error, and it is not
// treated as one either. The two layers whose staleness is bounded by the write
// rather than by a clock, the corpus statistics and the documents, are turned
// off, because a layer that would only ever be invalidated by a notification it
// will never receive is a layer that serves the corpus as it was when the
// process started. Result caching stays on: its bound is the expiry, and the
// expiry works whatever the driver can do.
func (c *Cache) Watch(st store.Store) (stop func()) {
	n, ok := st.(store.Notifier)
	if !ok {
		c.blind.Store(true)
		return func() {}
	}
	c.blind.Store(false)
	return n.OnChange(c.Invalidate)
}

// Invalidate applies one committed write.
//
// It is called from the goroutine that did the write, so it does no work beyond
// bumping two integers and deleting the named documents.
func (c *Cache) Invalidate(ch store.Change) {
	c.docs.Delete(ch.IDs...)

	c.mu.Lock()
	c.gen[ch.Tenant]++
	c.mu.Unlock()
}

// Clear empties every layer. It is what an operator reaches for and what a test
// uses to make a second measurement cold.
func (c *Cache) Clear() {
	c.results.Clear()
	c.corpus.Clear()
	c.docs.Clear()
}

// Stats reports each layer separately, because a hit rate averaged over three
// layers with three different jobs says nothing about any of them.
func (c *Cache) Stats() map[string]cache.Stats {
	return map[string]cache.Stats{
		"results":   c.results.Stats(),
		"corpus":    c.corpus.Stats(),
		"documents": c.docs.Stats(),
	}
}

func (c *Cache) generation(tenant string) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gen[tenant]
}

// ranked returns a cached ordering, or produces one and stores it.
//
// The key is the tenant's generation, the asker's visibility fingerprint and the
// canonical form of the request. Nothing about the query alone reaches an
// entry.
func (c *Cache) ranked(p *acl.Principal, r store.Request, sel store.Selection, fn func() (store.Ranked, error)) (store.Ranked, error) {
	fp := acl.Fingerprint(p)
	if fp == "" {
		return fn()
	}
	var key strings.Builder
	key.WriteString(strconv.FormatUint(c.generation(p.Tenant), 10))
	key.WriteByte('|')
	key.WriteString(fp)
	key.WriteByte('|')
	writeRequest(&key, r, sel)
	return c.results.Do(key.String(), fn)
}

// corpusStats returns the cached corpus numbers for a set of terms.
//
// The measurement spec asked for two layers here, corpus statistics per tenant
// and document frequency per term. This driver surface answers both in one
// call, [store.Statistician.Statistics], so there is one layer with the tighter
// of the two policies: keyed by tenant and terms, dropped on any write to that
// tenant.
//
// The key does not carry the fingerprint, because the value does not depend on
// the asker. Corpus statistics are counted over the tenant rather than over what
// one person may read, which is a decision made when the statistics were first
// written down: a document frequency counted over one asker's view would make a
// term's weight depend on who was asking, so the same query would rank
// differently for two colleagues. Keying a tenant wide value by fingerprint
// would spend an entry per asker to store the same numbers.
func (c *Cache) corpusStats(p *acl.Principal, terms []string, fn func() (store.Corpus, error)) (store.Corpus, error) {
	if p == nil || c.blind.Load() {
		return fn()
	}
	sorted := slices.Clone(terms)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	var key strings.Builder
	key.WriteString(strconv.FormatUint(c.generation(p.Tenant), 10))
	key.WriteByte('|')
	key.WriteString(p.Tenant)
	for _, t := range sorted {
		key.WriteByte('|')
		key.WriteString(t)
	}
	return c.corpus.Do(key.String(), fn)
}

// document returns cached documents for ids and the ids that were not cached.
//
// This layer is keyed by document id and not by the asker's visibility, which
// is the one exception to the rule at the top of this file, and it needs its
// justification stated rather than assumed.
//
// The cache sits behind the permission check rather than in front of it. The
// only caller is [Searcher.fetch], and the only ids it is ever given are ids
// that came out of the retrieval that applied the permission predicate to this
// principal moments earlier. An entry is therefore only ever reached with an id
// the caller has already proven they may read. Correctness comes from the order
// of the two operations rather than from the shape of the key, so that ordering
// has a test of its own: see TestDocumentCacheIsBehindThePermissionCheck.
//
// The two ways that ordering could go wrong are both closed. A revocation is a
// write, so the document entry is dropped and the fetch behind the next request
// goes back to the driver, which applies the predicate and returns nothing. A
// group change changes the fingerprint, so the ordering that named the id is
// unreachable and never reaches this layer at all.
func (c *Cache) document(ids []string) (have map[string]doc.Document, missing []string) {
	have = make(map[string]doc.Document, len(ids))
	if c.blind.Load() {
		return have, ids
	}
	for _, id := range ids {
		if d, ok := c.docs.Get(id); ok {
			have[id] = d
			continue
		}
		missing = append(missing, id)
	}
	return have, missing
}

func (c *Cache) putDocuments(docs map[string]doc.Document) {
	if c.blind.Load() {
		return
	}
	for id, d := range docs {
		c.docs.Put(id, d)
	}
}

// writeRequest writes the canonical form of a request.
//
// Canonical matters twice. Two requests that describe the same match set have
// to produce the same key or the cache never hits, and two that describe
// different match sets must not, or it hits when it should not. Every list is
// sorted and every field is written with a separator that cannot appear inside
// it, so no rearrangement of one field's values can be read as another field's.
func writeRequest(key *strings.Builder, r store.Request, sel store.Selection) {
	lists := [][]string{
		r.Terms,
		r.Sources,
		kindStrings(r.Kinds),
		r.Containers,
		r.Authors,
		r.Owners,
	}
	for _, list := range lists {
		sorted := slices.Clone(list)
		slices.Sort(sorted)
		sorted = slices.Compact(sorted)
		for _, v := range sorted {
			key.WriteString(strconv.Itoa(len(v)))
			key.WriteByte(':')
			key.WriteString(v)
			key.WriteByte(',')
		}
		key.WriteByte('|')
	}
	key.WriteString(strconv.FormatInt(r.Since.UnixNano(), 10))
	key.WriteByte('|')
	key.WriteString(strconv.FormatInt(r.Until.UnixNano(), 10))
	key.WriteByte('|')
	key.WriteString(strconv.Itoa(sel.Limit))
	key.WriteByte('|')
	key.WriteString(strconv.FormatBool(sel.Recent))
	key.WriteByte('|')
	// The counting is part of the key because it is part of the value. An entry
	// produced for a screen that wanted no counts holds a zero total and no
	// facets, and serving it to a search would draw a page of results above the
	// words "0 results".
	key.WriteString(strconv.FormatBool(sel.Counts))
	key.WriteByte('|')
	key.WriteString(strconv.Itoa(sel.Facets))
}

func kindStrings(kinds []doc.Kind) []string {
	if len(kinds) == 0 {
		return nil
	}
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}
