package limit_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/genba/connector/limit"
)

// start is a fixed point for the fake clock, so that every duration in these
// tests is arithmetic rather than a race with the machine.
var start = time.Date(2026, time.April, 1, 9, 0, 0, 0, time.UTC)

// fakeClock is a clock that only moves when something waits on it.
//
// Sleeping jumps straight to the end of the wait and records how long it was
// for, which is what makes an hour of backoff take a microsecond and makes the
// assertions exact. A test that slept for real would either be slow enough that
// nobody runs it or short enough that it fails on a loaded machine, and the
// second kind of test gets deleted rather than fixed.
//
// A held clock records waits without moving. It is for the tests with more than
// one goroutine in them, where a sleep that moved the shared clock would refill
// the bucket for whoever was not sleeping and make the answer depend on how the
// two were scheduled.
type fakeClock struct {
	held bool

	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newClock() *fakeClock {
	return &fakeClock{now: start}
}

func newHeldClock() *fakeClock {
	return &fakeClock{now: start, held: true}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	if !c.held {
		c.now = c.now.Add(d)
	}
	return nil
}

// advance moves the clock without anything having waited, which is how a test
// says time passed while the crawl was doing something else.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// sleeps is every wait the clock was asked for, in order.
func (c *fakeClock) sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.slept)
}

// total is how long the clock was asked to wait for altogether.
func (c *fakeClock) total() time.Duration {
	var sum time.Duration
	for _, d := range c.sleeps() {
		sum += d
	}
	return sum
}

func TestTheDefaultsAreAWorkingConfiguration(t *testing.T) {
	if err := (limit.Limits{}).Validate(); err != nil {
		t.Fatalf("the zero limits are not usable: %v", err)
	}
	// A limiter built from nothing still limits, which is the property that
	// matters: forgetting to configure one is not the same as turning it off.
	clock := newClock()
	l := limit.NewLimiter(limit.Limits{}, clock)
	for range limit.DefaultBurst + 1 {
		if err := l.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if clock.total() == 0 {
		t.Error("a limiter with no configuration let an unlimited burst through")
	}
}

func TestLimitsAreChecked(t *testing.T) {
	tests := []struct {
		name  string
		limit limit.Limits
		ok    bool
	}{
		{"nothing set", limit.Limits{}, true},
		{"a whole configuration", limit.Limits{Rate: 8, Burst: 16, MaxRetries: 3, MinBackoff: time.Second, MaxBackoff: time.Minute, Failures: 4, Cooldown: time.Minute}, true},
		{"a negative rate", limit.Limits{Rate: -1}, false},
		{"a negative burst", limit.Limits{Burst: -1}, false},
		{"a negative backoff", limit.Limits{MinBackoff: -time.Second}, false},
		{"a negative cooldown", limit.Limits{Cooldown: -time.Second}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.limit.Validate()
			if tt.ok && err != nil {
				t.Fatalf("rejected a usable set: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("accepted %+v", tt.limit)
			}
		})
	}
}

// The burst goes out immediately and the rate binds after it, which is the
// whole of what a token bucket is for: real work is lumpy and a quota is not
// one request wide.
func TestABurstGoesOutAtOnceAndTheRateBindsAfterIt(t *testing.T) {
	clock := newClock()
	l := limit.NewLimiter(limit.Limits{Rate: 10, Burst: 3}, clock)

	for range 3 {
		if err := l.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := clock.sleeps(); len(got) != 0 {
		t.Fatalf("the burst waited for %v", got)
	}

	if err := l.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Ten a second is one every hundred milliseconds, and the bucket is empty.
	if got := clock.total(); got != 100*time.Millisecond {
		t.Errorf("the fourth request waited %v, want 100ms", got)
	}
}

// The bucket refills while nothing is asking, so a crawl that paused for its
// own reasons is not then punished for it.
func TestTheBucketRefillsWhileNothingIsAsking(t *testing.T) {
	clock := newClock()
	l := limit.NewLimiter(limit.Limits{Rate: 10, Burst: 5}, clock)

	for range 5 {
		if err := l.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	clock.advance(time.Second)

	for range 5 {
		if err := l.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := clock.total(); got != 0 {
		t.Errorf("a refilled bucket still waited %v", got)
	}
}

// A pause is the source's own answer and outranks the ceiling the operator
// configured, because the operator was guessing and the service knows.
func TestAPauseOutranksTheBucket(t *testing.T) {
	clock := newClock()
	l := limit.NewLimiter(limit.Limits{Rate: 1000, Burst: 1000}, clock)

	l.Pause(clock.Now().Add(30 * time.Second))
	if at, held := l.Paused(); !held || !at.Equal(start.Add(30*time.Second)) {
		t.Fatalf("Paused() = %v, %v", at, held)
	}
	if err := l.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := clock.total(); got != 30*time.Second {
		t.Errorf("waited %v, want the thirty seconds the source asked for", got)
	}
	if got := l.Stats().Pauses; got != 1 {
		t.Errorf("pauses = %d, want 1", got)
	}
}

// A pause that has already run is not extended by a header that arrived late,
// and a shorter one does not cut a longer one short.
func TestAShorterPauseDoesNotShortenALongerOne(t *testing.T) {
	clock := newClock()
	l := limit.NewLimiter(limit.Limits{Rate: 1000, Burst: 1000}, clock)

	l.Pause(clock.Now().Add(time.Minute))
	l.Pause(clock.Now().Add(time.Second))
	if err := l.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := clock.total(); got != time.Minute {
		t.Errorf("waited %v, want the minute that was asked for first", got)
	}
}

// A shutdown does not fire one last request. It would spend quota on a document
// nobody is going to store.
func TestACancelledContextStopsWaiting(t *testing.T) {
	clock := newClock()
	l := limit.NewLimiter(limit.Limits{Rate: 1, Burst: 1}, clock)
	if err := l.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("a cancelled crawl was let through")
	}
}

// The counters are what say whether a ceiling is set sensibly, so they have to
// be right.
func TestTheLimiterCountsWhatItCost(t *testing.T) {
	clock := newClock()
	l := limit.NewLimiter(limit.Limits{Rate: 10, Burst: 1}, clock)

	for range 4 {
		if err := l.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got := l.Stats()
	if got.Waits != 3 {
		t.Errorf("waits = %d, want the three that were held back", got.Waits)
	}
	if got.Waited != 300*time.Millisecond {
		t.Errorf("waited = %v, want 300ms", got.Waited)
	}
}

// Goroutines sharing a limiter queue behind each other rather than each
// deciding on its own that it may go.
//
// The clock is held for this one so that the answer does not depend on the
// order the eight were scheduled in. Every caller takes its token from the same
// instant, so the bucket goes one token further into debt each time and each
// caller waits ten milliseconds longer than the one before it, whoever got there
// first.
func TestConcurrentCallersQueueBehindEachOther(t *testing.T) {
	clock := newHeldClock()
	l := limit.NewLimiter(limit.Limits{Rate: 100, Burst: 1}, clock)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Wait(t.Context()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	// One of the eight had the burst and went straight out. The other seven
	// waited 10ms, 20ms and so on up to 70ms, which is 280ms of holding back.
	if got := l.Stats().Waits; got != 7 {
		t.Errorf("waits = %d, want the seven that were held back", got)
	}
	if got := l.Stats().Waited; got != 280*time.Millisecond {
		t.Errorf("waited = %v, want 280ms", got)
	}

	// Nothing was let through twice on the same token, which is the thing a
	// limiter with a lock in the wrong place gets wrong.
	if got := len(clock.sleeps()); got != 7 {
		t.Errorf("the clock was asked to wait %d times, want 7", got)
	}
}
