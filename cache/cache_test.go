package cache_test

import (
	"errors"
	"hash/fnv"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tamnd/genba/cache"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// clock is a hand wound clock, so that an expiry test measures the policy
// rather than the machine it ran on.
type clock struct{ nanos atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.nanos.Store(epoch.UnixNano())
	return c
}

func (c *clock) now() time.Time      { return time.Unix(0, c.nanos.Load()) }
func (c *clock) add(d time.Duration) { c.nanos.Add(int64(d)) }

func TestGetReturnsWhatWasPut(t *testing.T) {
	c := cache.New[string](64, time.Minute)
	c.Put("k", "v")

	got, ok := c.Get("k")
	if !ok || got != "v" {
		t.Fatalf("Get returned %q, %v, want \"v\", true", got, ok)
	}
	if _, ok := c.Get("absent"); ok {
		t.Fatal("Get found a key that was never put")
	}
}

func TestEntriesExpire(t *testing.T) {
	tick := newClock()
	c := cache.New(64, time.Minute, cache.WithClock[string](tick.now))
	c.Put("k", "v")

	tick.add(59 * time.Second)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("an entry expired before its time")
	}
	tick.add(2 * time.Second)
	if _, ok := c.Get("k"); ok {
		t.Fatal("an expired entry was served")
	}
	if entries := c.Stats().Entries; entries != 0 {
		t.Errorf("the expired entry is still held: %d entries", entries)
	}
}

func TestForeverDoesNotExpire(t *testing.T) {
	tick := newClock()
	c := cache.New(64, cache.Forever, cache.WithClock[string](tick.now))
	c.Put("k", "v")

	tick.add(365 * 24 * time.Hour)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("an entry with no expiry expired anyway")
	}
}

// TestOffHoldsNothing covers the setting a deployment reaches for when it
// cannot tolerate a stale answer. It has to be a real off rather than a very
// short expiry, because a very short expiry still serves one stale answer.
func TestOffHoldsNothing(t *testing.T) {
	c := cache.New[string](64, cache.Off)
	c.Put("k", "v")
	if _, ok := c.Get("k"); ok {
		t.Fatal("a cache that is off served an entry")
	}

	calls := 0
	for range 3 {
		if _, err := c.Do("k", func() (string, error) { calls++; return "v", nil }); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}
	if calls != 3 {
		t.Errorf("a cache that is off called through %d times for three lookups, want 3", calls)
	}
}

// TestEvictionTakesTheLeastRecentlyUsed works inside one shard, because that is
// where the least recently used list is. The keys are chosen by the same hash
// the cache uses, so the test does not depend on how many shards there are.
func TestEvictionTakesTheLeastRecentlyUsed(t *testing.T) {
	first, second, third := threeKeysInOneShard(t)

	// Two entries per shard.
	c := cache.New[string](2*cache.Shards, time.Minute)
	c.Put(first, "1")
	c.Put(second, "2")

	// Touching the first makes the second the one to go.
	if _, ok := c.Get(first); !ok {
		t.Fatalf("the first key went missing before anything was evicted")
	}
	c.Put(third, "3")

	if _, ok := c.Get(first); !ok {
		t.Error("the recently used entry was evicted")
	}
	if _, ok := c.Get(second); ok {
		t.Error("the least recently used entry survived a full shard")
	}
	if _, ok := c.Get(third); !ok {
		t.Error("the entry that caused the eviction is not there")
	}
	if got := c.Stats().Evictions; got != 1 {
		t.Errorf("the cache reports %d evictions, want 1", got)
	}
}

func threeKeysInOneShard(t *testing.T) (a, b, d string) {
	t.Helper()
	byShard := make(map[uint32][]string)
	for i := range 10_000 {
		k := "key" + strconv.Itoa(i)
		h := fnv.New32a()
		_, _ = h.Write([]byte(k))
		s := h.Sum32() % cache.Shards
		byShard[s] = append(byShard[s], k)
		if len(byShard[s]) == 3 {
			return byShard[s][0], byShard[s][1], byShard[s][2]
		}
	}
	t.Fatal("could not find three keys in one shard")
	return "", "", ""
}

// TestDoRunsOnceForConcurrentMisses is the case the cache exists for: a popular
// key goes cold and every reader misses it at the same moment.
func TestDoRunsOnceForConcurrentMisses(t *testing.T) {
	c := cache.New[int](64, time.Minute)

	const readers = cache.Shards
	var (
		calls   atomic.Int64
		arrived atomic.Int64
		release = make(chan struct{})
		wg      sync.WaitGroup
	)
	got := make([]int, readers)
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			arrived.Add(1)
			v, err := c.Do("hot", func() (int, error) {
				calls.Add(1)
				<-release
				return 42, nil
			})
			if err != nil {
				t.Errorf("Do: %v", err)
				return
			}
			got[i] = v
		}()
	}
	for arrived.Load() < readers {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("%d readers produced %d calls, want 1", readers, n)
	}
	for i, v := range got {
		if v != 42 {
			t.Fatalf("reader %d got %d, want 42", i, v)
		}
	}
}

// TestDoDoesNotRememberFailures keeps a moment of backend trouble from becoming
// a minute of it.
func TestDoDoesNotRememberFailures(t *testing.T) {
	c := cache.New[string](64, time.Minute)
	boom := errors.New("boom")

	calls := 0
	produce := func() (string, error) {
		calls++
		if calls == 1 {
			return "", boom
		}
		return "v", nil
	}

	if _, err := c.Do("k", produce); !errors.Is(err, boom) {
		t.Fatalf("Do returned %v, want the error from the call", err)
	}
	v, err := c.Do("k", produce)
	if err != nil || v != "v" {
		t.Fatalf("the second Do returned %q, %v, want the value", v, err)
	}
	if calls != 2 {
		t.Errorf("the cache called through %d times, want 2: an error must not be stored", calls)
	}
}

func TestDeleteAndClear(t *testing.T) {
	c := cache.New[string](64, time.Minute)
	c.Put("a", "1")
	c.Put("b", "2")

	c.Delete("a", "never-there")
	if _, ok := c.Get("a"); ok {
		t.Error("a deleted entry was served")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("deleting one key removed another")
	}

	c.Clear()
	if entries := c.Stats().Entries; entries != 0 {
		t.Errorf("Clear left %d entries", entries)
	}
	c.Put("c", "3")
	if _, ok := c.Get("c"); !ok {
		t.Error("the cache stopped working after Clear")
	}
}

func TestStatsCountLookups(t *testing.T) {
	c := cache.New[string](64, time.Minute)
	c.Put("k", "v")

	c.Get("k")
	c.Get("k")
	c.Get("absent")

	got := c.Stats()
	if got.Hits != 2 || got.Misses != 1 {
		t.Errorf("Stats reports %d hits and %d misses, want 2 and 1", got.Hits, got.Misses)
	}
	if got.Entries != 1 {
		t.Errorf("Stats reports %d entries, want 1", got.Entries)
	}
	if ratio := got.Ratio(); ratio < 0.66 || ratio > 0.67 {
		t.Errorf("Stats reports a ratio of %v, want two thirds", ratio)
	}
	if empty := (cache.Stats{}).Ratio(); empty != 0 {
		t.Errorf("a cache nobody asked anything reports a ratio of %v, want 0", empty)
	}
}

func TestCapacityIsRespected(t *testing.T) {
	const capacity = 4 * cache.Shards
	c := cache.New[int](capacity, time.Minute)
	for i := range 10_000 {
		c.Put(strconv.Itoa(i), i)
	}
	if got := c.Stats().Entries; got > capacity {
		t.Errorf("the cache holds %d entries, over its capacity of %d", got, capacity)
	}
	if got := c.Stats().Evictions; got == 0 {
		t.Error("ten thousand entries went into a cache of sixty four and nothing was evicted")
	}
}

// TestConcurrentUseIsSafe is here for the race detector rather than for its
// assertions.
func TestConcurrentUseIsSafe(t *testing.T) {
	c := cache.New[int](128, time.Minute)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				k := strconv.Itoa((i*j)%50 + 1)
				switch j % 4 {
				case 0:
					c.Put(k, j)
				case 1:
					c.Get(k)
				case 2:
					_, _ = c.Do(k, func() (int, error) { return j, nil })
				case 3:
					c.Delete(k)
				}
			}
		}()
	}
	wg.Wait()
	c.Stats()
}
