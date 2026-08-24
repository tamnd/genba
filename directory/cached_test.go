package directory_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tamnd/genba/directory"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// tick is a hand wound clock, so that a test about a staleness bound measures
// the bound rather than the machine it ran on.
type tick struct{ nanos atomic.Int64 }

func newTick() *tick {
	c := &tick{}
	c.nanos.Store(epoch.UnixNano())
	return c
}

func (c *tick) now() time.Time      { return time.Unix(0, c.nanos.Load()) }
func (c *tick) add(d time.Duration) { c.nanos.Add(int64(d)) }

// counted is a directory that says how often it was asked anything.
type counted struct {
	*directory.Static
	asked atomic.Int64
}

func (c *counted) Subject(ctx context.Context, id string) (directory.Subject, error) {
	c.asked.Add(1)
	return c.Static.Subject(ctx, id)
}

func (c *counted) Group(ctx context.Context, id string) (directory.Group, error) {
	c.asked.Add(1)
	return c.Static.Group(ctx, id)
}

// company is the directory the cases below resolve against, wrapped so that the
// requests it answers can be counted.
func company(t *testing.T) *counted {
	t.Helper()
	d := &counted{Static: directory.NewStatic("acme")}
	d.PutGroup(directory.Group{ID: "everyone"})
	d.PutGroup(directory.Group{ID: "engineering", MemberOf: []string{"everyone"}})
	d.PutGroup(directory.Group{ID: "finance", MemberOf: []string{"everyone"}})
	d.Put(directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
	d.Put(directory.Subject{ID: "sam", MemberOf: []string{"finance"}})
	return d
}

// cached builds a cache over a directory, with whatever options the case cares
// about.
func cached(t *testing.T, d directory.Directory, opts ...directory.CacheOption) *directory.Cache {
	t.Helper()
	r, err := directory.New(d)
	if err != nil {
		t.Fatal(err)
	}
	c, err := directory.NewCache(r, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func resolved(t *testing.T, c *directory.Cache, id string) directory.Expansion {
	t.Helper()
	got, err := c.Expand(t.Context(), id)
	if err != nil {
		t.Fatalf("expanding %s: %v", id, err)
	}
	return got
}

func TestASecondRequestForTheSamePersonDoesNotAskTheDirectoryAgain(t *testing.T) {
	d := company(t)
	c := cached(t, d)

	first := resolved(t, c, "mei")
	after := d.asked.Load()
	if after == 0 {
		t.Fatal("the first expansion asked the directory nothing")
	}

	second := resolved(t, c, "mei")
	if d.asked.Load() != after {
		t.Errorf("the second expansion asked the directory %d more times", d.asked.Load()-after)
	}
	if second.Groups.Version != first.Groups.Version {
		t.Error("the same answer came back with a different version")
	}
	if st := c.Stats(); st.Hits != 1 || st.Misses != 1 {
		t.Errorf("the cache reports %d hits and %d misses, want one of each", st.Hits, st.Misses)
	}
}

// The box this is here for. A membership change is noticed within the bound,
// and until then it is not, and both halves are the promise.
func TestAMembershipChangeIsSeenWithinTheBound(t *testing.T) {
	clock := newTick()
	d := company(t)
	c := cached(t, d, directory.WithTTL(time.Minute), directory.WithCacheClock(clock.now))

	before := resolved(t, c, "mei")

	d.Put(directory.Subject{ID: "mei", MemberOf: []string{"engineering", "finance"}})

	clock.add(59 * time.Second)
	still := resolved(t, c, "mei")
	if still.Groups.Version != before.Groups.Version {
		t.Fatal("the change was seen before the bound, which means the bound is not what is being tested")
	}

	clock.add(2 * time.Second)
	now := resolved(t, c, "mei")
	if now.Groups.Version == before.Groups.Version {
		t.Fatal("the change was still not seen after the bound had passed")
	}
	if want := "acme:finance"; !slices.Contains(now.Groups.Members, want) {
		t.Errorf("the refreshed answer is %v, and %s is not in it", now.Groups.Members, want)
	}
}

// The version is the other half. Noticing a change within a minute is worth
// nothing if what was built on the old answer outlives it, and everything above
// keys on this number.
func TestTheVersionMovesWithTheAnswerRatherThanWithTheClock(t *testing.T) {
	clock := newTick()
	d := company(t)
	c := cached(t, d, directory.WithTTL(time.Minute), directory.WithCacheClock(clock.now))

	before := resolved(t, c, "mei")

	// A refresh that finds nothing new keeps the version, so nothing derived
	// from it is thrown away for no reason.
	clock.add(2 * time.Minute)
	same := resolved(t, c, "mei")
	if same.Groups.Version != before.Groups.Version {
		t.Error("an unchanged membership came back with a new version, which invalidates every bitmap keyed on it for nothing")
	}

	// Somebody else changing does not move this person's version either.
	d.Put(directory.Subject{ID: "sam", MemberOf: []string{"finance", "engineering"}})
	clock.add(2 * time.Minute)
	if again := resolved(t, c, "mei"); again.Groups.Version != before.Groups.Version {
		t.Error("a change to somebody else moved this person's version")
	}
}

func TestTheStalenessBoundIsTheNumberItWasGiven(t *testing.T) {
	if got := cached(t, company(t)).Staleness(); got != directory.DefaultTTL {
		t.Errorf("the default bound is %v, want %v", got, directory.DefaultTTL)
	}
	if got := cached(t, company(t), directory.WithTTL(30*time.Second)).Staleness(); got != 30*time.Second {
		t.Errorf("the bound is %v, want 30s", got)
	}
	// A bound below one is a configuration mistake rather than a request for a
	// cache that never hits, and it takes the default.
	if got := cached(t, company(t), directory.WithTTL(0)).Staleness(); got != directory.DefaultTTL {
		t.Errorf("a zero bound gave %v, want the default", got)
	}
}

func TestForgettingOnePersonDoesNotFlushEverybody(t *testing.T) {
	d := company(t)
	c := cached(t, d)

	resolved(t, c, "mei")
	resolved(t, c, "sam")
	after := d.asked.Load()

	c.Forget("mei")

	resolved(t, c, "sam")
	if d.asked.Load() != after {
		t.Error("forgetting one person sent somebody else back to the directory")
	}
	resolved(t, c, "mei")
	if d.asked.Load() == after {
		t.Error("the forgotten person was still served from the cache")
	}
}

func TestForgettingSomebodyWhoIsNotHeldIsFine(t *testing.T) {
	c := cached(t, company(t))
	c.Forget("nobody", "")
	c.Forget()
}

func TestClearDropsEverything(t *testing.T) {
	d := company(t)
	c := cached(t, d)
	resolved(t, c, "mei")
	resolved(t, c, "sam")
	after := d.asked.Load()

	c.Clear()

	resolved(t, c, "mei")
	resolved(t, c, "sam")
	if d.asked.Load() == after {
		t.Error("a cleared cache still answered from what it held")
	}
	if entries := c.Stats().Entries; entries != 2 {
		t.Errorf("the cache holds %d entries after being refilled, want 2", entries)
	}
}

// Everybody arrives at nine o'clock. A cold cache and a thousand people has to
// be one walk over the directory per person, not one per request.
func TestPeopleArrivingTogetherShareOneExpansion(t *testing.T) {
	d := company(t)
	c := cached(t, d)

	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			if _, err := c.Expand(context.Background(), "mei"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	// The subject, the group below and the one above it, and the empty level
	// that ends the walk costs nothing.
	if got := d.asked.Load(); got != 3 {
		t.Errorf("sixty four requests for one person asked the directory %d times, want 3", got)
	}
}

// broken answers once it has been told to.
type broken struct {
	*directory.Static
	down atomic.Bool
}

var errDirectoryDown = errors.New("connection refused")

func (b *broken) Subject(ctx context.Context, id string) (directory.Subject, error) {
	if b.down.Load() {
		return directory.Subject{}, errDirectoryDown
	}
	return b.Static.Subject(ctx, id)
}

// A directory that was unreachable for a moment is not a fact about a subject,
// and remembering it would turn a blip into a minute of refusals.
func TestAFailureIsNotRemembered(t *testing.T) {
	d := &broken{Static: directory.NewStatic("acme")}
	d.PutGroup(directory.Group{ID: "engineering"})
	d.Put(directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
	d.down.Store(true)

	c := cached(t, d)
	if _, err := c.Expand(t.Context(), "mei"); !errors.Is(err, errDirectoryDown) {
		t.Fatalf("a directory that was down gave %v", err)
	}

	d.down.Store(false)
	if got := resolved(t, c, "mei"); len(got.Groups.Members) != 1 {
		t.Errorf("the recovered directory resolved to %v", got.Groups.Members)
	}
}

// A refusal the directory meant is a different thing, and it still refuses.
func TestASubjectTheDirectoryDoesNotHoldStillRefuses(t *testing.T) {
	c := cached(t, company(t))
	if _, err := c.Expand(t.Context(), "nobody"); !errors.Is(err, directory.ErrNoSubject) {
		t.Errorf("an unknown subject gave %v, want ErrNoSubject", err)
	}
}

// Everything above is free to keep a principal and sort what it carries, so two
// requests must not be handed the same slice.
func TestTwoRequestsAreNotHandedTheSameSlice(t *testing.T) {
	c := cached(t, company(t))

	first := resolved(t, c, "mei")
	first.Groups.Members[0] = "acme:administrators"
	first.Unknown = append(first.Unknown, "tampered")

	second := resolved(t, c, "mei")
	if slices.Contains(second.Groups.Members, "acme:administrators") {
		t.Fatalf("a write to one caller's group set reached another: %v", second.Groups.Members)
	}
	if want := []string{"acme:engineering", "acme:everyone"}; !slices.Equal(second.Groups.Members, want) {
		t.Errorf("the second caller got %v, want %v", second.Groups.Members, want)
	}
}

func TestACancelledRequestDoesNotGetAnAnswer(t *testing.T) {
	c := cached(t, company(t))
	resolved(t, c, "mei")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Expand(ctx, "mei"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled request was served %v", err)
	}
}

func TestTheCacheCarriesTheCountersOfWhatItWraps(t *testing.T) {
	c := cached(t, company(t))
	resolved(t, c, "mei")
	resolved(t, c, "mei")

	// One expansion, because the second request never reached the resolver.
	if got := c.Counters().Expansions; got != 1 {
		t.Errorf("the resolver under the cache counted %d expansions, want 1", got)
	}
}

func TestACacheNeedsSomethingWithANameToWrap(t *testing.T) {
	if _, err := directory.NewCache(nil); err == nil {
		t.Error("a cache over nothing was accepted")
	}
	if _, err := directory.NewCache(nameless{}); err == nil {
		t.Error("a cache over an expander with no name was accepted")
	}
}

type nameless struct{}

func (nameless) Name() string { return "" }
func (nameless) Expand(context.Context, string) (directory.Expansion, error) {
	return directory.Expansion{}, nil
}

// The least recently used entry goes, which for this cache means the people who
// have stopped asking.
func TestAFullCacheDropsThePeopleWhoStoppedAsking(t *testing.T) {
	d := directory.NewStatic("acme")
	d.PutGroup(directory.Group{ID: "everyone"})
	for i := range 200 {
		d.Put(directory.Subject{ID: "u" + strconv.Itoa(i), MemberOf: []string{"everyone"}})
	}
	c := cached(t, d, directory.WithCapacity(16))

	for i := range 200 {
		resolved(t, c, "u"+strconv.Itoa(i))
	}
	if entries := c.Stats().Entries; entries > 32 {
		t.Errorf("a cache asked for sixteen entries holds %d", entries)
	}
	if c.Stats().Evictions == 0 {
		t.Error("two hundred people went through a cache of sixteen and nothing was evicted")
	}
}

func TestReadingAndWritingTheCacheAtTheSameTimeIsSafe(t *testing.T) {
	d := company(t)
	c := cached(t, d)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			id := "mei"
			if i%2 == 0 {
				id = "sam"
			}
			if _, err := c.Expand(context.Background(), id); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			c.Forget("mei")
			c.Stats()
		})
	}
	wg.Wait()
}
