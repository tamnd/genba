package limit

import (
	"context"
	"sync"
	"time"
)

// Clock is where a limiter reads the time and where it waits.
//
// It is an interface so that the tests can run an hour of backoff in a
// microsecond. A limiter is almost entirely about durations, and a test suite
// that checked those by sleeping would either be slow enough that nobody runs
// it or short enough that it fails on a loaded machine, which is the failure
// mode that gets a test deleted rather than fixed.
type Clock interface {
	// Now is the current time.
	Now() time.Time

	// Sleep waits for d, or returns the context's error if that happens first.
	// A d of zero or less returns immediately.
	Sleep(ctx context.Context, d time.Duration) error
}

// realClock is the wall clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Limiter spaces requests out so that a source is asked for no more than it
// said it would give.
//
// It is a token bucket, which is the shape that matches what services actually
// enforce: a rate over a window, with a little room for a burst inside it. The
// bucket is refilled by arithmetic on the clock rather than by a goroutine, so
// a limiter that is never used costs nothing and a process holding a hundred of
// them holds no timers.
//
// It is safe for concurrent use.
type Limiter struct {
	rate  float64
	burst float64

	mu     sync.Mutex
	tokens float64
	last   time.Time

	// until is a wall the source itself asked for, out of a Retry-After or a
	// quota reset. It outranks the bucket in both directions: nothing goes out
	// before it whatever the bucket says, and it is not shortened by tokens
	// having piled up while the crawl was waiting.
	until time.Time

	waits   int64
	waited  time.Duration
	paused  int64
	clock   Clock
	started bool
}

// NewLimiter returns a limiter for the given limits, with a full bucket.
//
// It starts full because the alternative punishes a process for having just
// started. The first thing a crawl does is a listing followed by the documents
// on it, and making that wait for a bucket to fill would add a delay that
// protects nobody: no requests have been made yet, so no quota has been spent.
func NewLimiter(l Limits, clock Clock) *Limiter {
	l = l.withDefaults()
	if clock == nil {
		clock = realClock{}
	}
	return &Limiter{
		rate:   l.Rate,
		burst:  float64(l.Burst),
		tokens: float64(l.Burst),
		clock:  clock,
	}
}

// Wait blocks until a request may go out, or until the context is done.
//
// It returns the context's error rather than going ahead when a crawl is being
// shut down, which matters more here than it looks: a shutdown that fired one
// last request would spend quota on a document nobody is going to store.
func (l *Limiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d := l.reserve(l.clock.Now())
	if d <= 0 {
		return nil
	}
	l.mu.Lock()
	l.waits++
	l.waited += d
	l.mu.Unlock()
	return l.clock.Sleep(ctx, d)
}

// reserve takes a token and returns how long to wait before using it.
//
// The token is taken now and the waiting is done afterwards, without the lock
// held, which is what makes two callers queue behind each other instead of both
// deciding they may go at once. A bucket that has gone into debt is exactly the
// record of the requests that are already on their way.
func (l *Limiter) reserve(now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.started {
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
	}
	l.started, l.last = true, now
	l.tokens--

	var wait time.Duration
	if l.tokens < 0 {
		wait = time.Duration(-l.tokens / l.rate * float64(time.Second))
	}
	if held := l.until.Sub(now); held > wait {
		wait = held
	}
	return wait
}

// Pause holds every request back until the given time.
//
// This is what a source's own answer turns into. A Retry-After header or a
// quota that reports nothing left is the service saying when it will talk
// again, and it outranks whatever ceiling the operator configured, because the
// operator was guessing and the service knows.
//
// A time already in the past does nothing, so a header that arrived late or a
// clock that disagrees cannot shorten a pause that is already running.
func (l *Limiter) Pause(until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until.After(l.until) {
		l.until = until
		l.paused++
	}
}

// Paused reports when the limiter will next let a request out, and whether it
// is being held back at all.
func (l *Limiter) Paused() (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.until.After(l.clock.Now()) {
		return l.until, true
	}
	return time.Time{}, false
}

// LimiterStats is what the limiter has cost so far.
type LimiterStats struct {
	// Waits is how many requests were held back at all, and Waited is how long
	// they were held back for in total.
	//
	// These are the two numbers that say whether a ceiling is set sensibly. A
	// crawl where almost nothing waited is running below its limit and could go
	// faster, and one where every request waited an hour is configured for a
	// quota somebody has since been given more of.
	Waits  int64
	Waited time.Duration

	// Pauses is how many times the source itself asked for a delay. This is the
	// one to watch: it means the ceiling is set above what the service is
	// actually willing to give, and the crawl is finding that out by being told
	// off rather than by staying under it.
	Pauses int64
}

// Stats returns what the limiter has cost.
func (l *Limiter) Stats() LimiterStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return LimiterStats{Waits: l.waits, Waited: l.waited, Pauses: l.paused}
}
