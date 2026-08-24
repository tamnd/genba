package limit

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ErrOpen is returned instead of making a request when the circuit is open.
//
// It is what "stop rather than retry forever" looks like from the connector's
// side: the sync fails, the pipeline reports it, the run ends, and the next
// scheduled refresh tries again. A crawler that kept retrying a source which
// has been refusing everything for a minute is doing nothing except making the
// outage look like load.
var ErrOpen = errors.New("limit: the circuit is open, the source has been refusing requests")

// Transport wraps a round tripper with a rate limit, retries and a circuit
// breaker.
//
// It is safe for concurrent use, and it is meant to be shared by everything
// that talks to one service with one set of credentials, because that is the
// scope a quota has.
type Transport struct {
	base    http.RoundTripper
	limits  Limits
	limiter *Limiter
	clock   Clock
	log     *slog.Logger
	jitter  func(time.Duration) time.Duration

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	probing   bool
	requests  int64
	retries   int64
	trips     int64
}

// Option configures a transport.
type Option func(*Transport)

// WithBase replaces the round tripper the requests are actually made on, which
// is where a deployment puts its own proxy, its own connection pool or its own
// TLS configuration.
func WithBase(rt http.RoundTripper) Option {
	return func(t *Transport) {
		if rt != nil {
			t.base = rt
		}
	}
}

// WithClock replaces the clock, which is how the tests run an hour of backoff
// without waiting for one.
func WithClock(c Clock) Option {
	return func(t *Transport) {
		if c != nil {
			t.clock = c
		}
	}
}

// WithLogger installs the logger the waiting is reported to.
//
// A crawl that is being throttled looks exactly like a crawl that is slow, and
// the difference decides whether somebody goes looking at the network or asks
// for more quota. So a backoff is logged at warning level rather than at debug:
// it is not the normal case, and an operator should not have to turn anything
// on to find out it is happening.
func WithLogger(l *slog.Logger) Option {
	return func(t *Transport) {
		if l != nil {
			t.log = l
		}
	}
}

// WithJitter replaces the function that spreads a backoff out.
//
// The default takes a random duration between half the computed wait and the
// whole of it. The spread is what stops a fleet of crawlers that were all
// refused at the same moment from all coming back at the same moment, which
// turns one overload into a series of them.
//
// It is replaceable so that a test can ask for the wait it computed rather than
// a random fraction of it.
func WithJitter(f func(time.Duration) time.Duration) Option {
	return func(t *Transport) {
		if f != nil {
			t.jitter = f
		}
	}
}

// NewTransport returns a transport enforcing l.
func NewTransport(l Limits, opts ...Option) *Transport {
	t := &Transport{
		base:   http.DefaultTransport,
		limits: l.withDefaults(),
		clock:  realClock{},
		log:    slog.New(slog.DiscardHandler),
		jitter: halfToFull,
	}
	for _, opt := range opts {
		opt(t)
	}
	t.limiter = NewLimiter(t.limits, t.clock)
	return t
}

var _ http.RoundTripper = (*Transport)(nil)

// Limiter returns the limiter this transport is enforcing, for a caller that
// wants its counters.
func (t *Transport) Limiter() *Limiter { return t.limiter }

// RoundTrip waits its turn, makes the request, and tries again if the answer
// was one worth trying again.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if err := t.closed(); err != nil {
		return nil, err
	}

	for attempt := 0; ; attempt++ {
		if err := t.limiter.Wait(ctx); err != nil {
			return nil, err
		}

		t.mu.Lock()
		t.requests++
		t.mu.Unlock()

		resp, err := t.base.RoundTrip(req)

		// The quota is read off every answer rather than only off a refusal. A
		// response saying none is left is the last request before the wall, and
		// holding back there is the difference between a crawl that stays inside
		// its quota and one that finds the edge by hitting it.
		if until := spent(resp, t.clock.Now()); !until.IsZero() {
			t.limiter.Pause(until)
			t.log.Warn("the source says its quota is spent, holding back until the window rolls over",
				"url", req.URL.Redacted(),
				"until", until.UTC().Format(time.RFC3339),
			)
		}

		wait, retry := t.verdict(req, resp, err, attempt)
		if !retry {
			t.record(resp, err)
			return resp, err
		}

		// The body of a response that is about to be thrown away still has to be
		// closed, or the connection is dropped instead of going back to the pool
		// and a crawl that is being throttled quietly opens a socket per refusal.
		reason := err
		if resp != nil {
			_ = resp.Body.Close()
			reason = fmt.Errorf("limit: %s %s: %s", req.Method, req.URL.Redacted(), resp.Status)
		}

		t.log.Warn("waiting before trying a request again",
			"method", req.Method,
			"url", req.URL.Redacted(),
			"attempt", attempt+1,
			"of", t.limits.MaxRetries,
			"wait", wait.Round(time.Millisecond),
			"reason", reason,
		)
		t.mu.Lock()
		t.retries++
		t.mu.Unlock()

		if err := t.clock.Sleep(ctx, wait); err != nil {
			return nil, err
		}
		if err := t.closed(); err != nil {
			return nil, err
		}
	}
}

// verdict decides what to do with one answer, and is the whole of the retry
// policy in one place.
func (t *Transport) verdict(req *http.Request, resp *http.Response, err error, attempt int) (wait time.Duration, retry bool) {
	if attempt >= t.limits.MaxRetries || !repeatable(req) {
		return 0, false
	}
	if err != nil {
		// A request that never got an answer is worth another go: a connection
		// reset is the single most common thing that goes wrong on a long crawl
		// and it means nothing at all about the document being read.
		return t.backoff(attempt, 0), true
	}
	if !worthRetrying(resp.StatusCode) {
		return 0, false
	}
	// What the source said beats what we would have guessed. A service that
	// names a time knows when its window rolls over and we do not, and coming
	// back before then spends an attempt on a refusal that was already certain.
	return t.backoff(attempt, retryAfter(resp, t.clock.Now())), true
}

// record updates the breaker with how one request ended.
//
// The counter is of consecutive failures rather than of failures, because a
// crawl of any size has a few of those and a source that is actually broken has
// nothing else. One success anywhere in the run says the credentials are good,
// the network is up and the service is answering, which is the whole of what
// the breaker is there to decide.
func (t *Transport) record(resp *http.Response, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !broken(resp, err) {
		t.failures, t.probing = 0, false
		return
	}
	t.failures++
	if t.probing || t.failures >= t.limits.Failures {
		t.openUntil = t.clock.Now().Add(t.limits.Cooldown)
		t.trips++
		t.probing = false
		t.log.Error("a source is refusing requests, stopping until it recovers",
			"consecutive_failures", t.failures,
			"cooldown", t.limits.Cooldown,
			"until", t.openUntil.UTC().Format(time.RFC3339),
		)
	}
}

// closed reports whether a request may go out, and lets exactly one through
// once the cooldown is over to find out whether the source has recovered.
func (t *Transport) closed() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.openUntil.IsZero() {
		return nil
	}
	if t.clock.Now().Before(t.openUntil) {
		return fmt.Errorf("%w, trying again at %s", ErrOpen, t.openUntil.UTC().Format(time.RFC3339))
	}
	// The cooldown is over. One request goes, and until it comes back the
	// circuit is neither open nor closed: a failure reopens it immediately
	// rather than after another five, because the five have already happened.
	t.openUntil, t.probing = time.Time{}, true
	return nil
}

// backoff is how long to wait before attempt+1, jittered and bounded.
//
// A source that named its own time is honoured exactly, with the jitter added
// on top rather than taken off it, because coming back one millisecond early is
// coming back before the window rolled over.
func (t *Transport) backoff(attempt int, named time.Duration) time.Duration {
	if named > 0 {
		if named > t.limits.MaxBackoff {
			named = t.limits.MaxBackoff
		}
		return named + t.jitter(t.limits.MinBackoff)
	}
	d := t.limits.MinBackoff << attempt
	// Shifting a duration far enough overflows into a negative one, which would
	// be a backoff of no time at all on the attempt that most needed one.
	if d <= 0 || d > t.limits.MaxBackoff {
		d = t.limits.MaxBackoff
	}
	return t.jitter(d)
}

// TransportStats is what a transport has done.
type TransportStats struct {
	// Requests is every attempt, including the ones that were retried.
	Requests int64

	// Retries is how many of those were second or later attempts. A crawl where
	// this is a large fraction of Requests is one where the ceiling is set too
	// high, and it is costing quota to find that out.
	Retries int64

	// Trips is how many times the circuit opened.
	Trips int64

	// Open says the circuit is open now, which means the source is not being
	// read at all.
	Open bool

	// Limiter is what the waiting cost.
	Limiter LimiterStats
}

// Stats returns what this transport has done.
func (t *Transport) Stats() TransportStats {
	t.mu.Lock()
	s := TransportStats{
		Requests: t.requests,
		Retries:  t.retries,
		Trips:    t.trips,
		Open:     !t.openUntil.IsZero() && t.clock.Now().Before(t.openUntil),
	}
	t.mu.Unlock()
	s.Limiter = t.limiter.Stats()
	return s
}

// worthRetrying reports whether a status is one where the same request might
// work in a moment.
//
// Too many requests is the obvious one. The five hundreds are here because they
// are the source saying something went wrong at its end, and a gateway timeout
// on one document says nothing about the next attempt at the same document. A
// four hundred is not here: a request the service considers malformed is
// malformed on every attempt, and retrying it burns quota to arrive at the same
// answer more slowly.
func worthRetrying(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// broken reports whether an answer counts towards opening the circuit.
//
// Transport errors and the five hundreds count, because both mean the service
// is not answering. Unauthorised counts, because a token that has been revoked
// is exactly the state this is here to notice and it is not going to fix itself
// on the next document.
//
// Forbidden deliberately does not count. At an object store it is the ordinary
// answer for one object out of a million that this account may not read, and a
// breaker that tripped on it would stop a healthy crawl over a handful of
// objects that were never part of the corpus.
//
// Too many requests does not count either. It is the service working exactly as
// designed and saying so, and it is handled by waiting rather than by stopping.
func broken(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return true
	}
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode/100 == 5
}

// repeatable reports whether a request can be sent a second time.
//
// Only a request with no body at all is, and that is not a limitation in
// practice: a crawler reads, so everything that goes through here is a GET or a
// HEAD with nothing attached. The alternative is rewinding the body between
// attempts, which means either consuming the caller's reader and putting it
// back or holding the whole of it in memory, and a round tripper is not allowed
// to modify the request it was handed in the first place.
//
// The method is checked as well, because a body is not the only thing that
// makes a second attempt wrong. A POST that timed out may well have been
// carried out, and sending it again is how one order becomes two.
func repeatable(req *http.Request) bool {
	if req.Body != nil && req.Body != http.NoBody {
		return false
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// The headers a service uses to say how much quota is left and when it comes
// back.
//
// There are four conventions and no way to tell which one a service follows
// except by looking. Retry-After is the standard and is what a refusal carries.
// The unprefixed RateLimit fields are the newer draft and carry a delta in
// seconds. The X-RateLimit fields are what most large services shipped years
// before the draft existed, and their reset is usually a Unix timestamp rather
// than a delta.
//
// The fourth is the same idea with a hyphen in a different place. Okta spells
// it X-Rate-Limit, which canonicalises to a different header name entirely, so
// a transport reading only the other three sees an organisation that sends its
// numbers on every single response as one that sends none at all.
const (
	headerRetryAfter    = "Retry-After"
	headerReset         = "RateLimit-Reset"
	headerRemaining     = "RateLimit-Remaining"
	headerLegacyReset   = "X-RateLimit-Reset"
	headerLegacyRemains = "X-RateLimit-Remaining"
	headerHyphenReset   = "X-Rate-Limit-Reset"
	headerHyphenRemains = "X-Rate-Limit-Remaining"
)

// retryAfter is how long a response says to wait, and zero for one that does
// not say.
func retryAfter(resp *http.Response, now time.Time) time.Duration {
	if resp == nil {
		return 0
	}
	if v := resp.Header.Get(headerRetryAfter); v != "" {
		// The header is either a number of seconds or an HTTP date, and both
		// spellings are in use by services this connector will meet.
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Duration(secs) * time.Second
		}
		if at, err := http.ParseTime(v); err == nil {
			return at.Sub(now)
		}
	}
	for _, h := range [...]string{headerReset, headerLegacyReset, headerHyphenReset} {
		if d := resetIn(resp.Header.Get(h), now); d > 0 {
			return d
		}
	}
	return 0
}

// spent returns when the window rolls over, for a response saying the quota is
// gone, and the zero time for everything else.
//
// It is read off every response rather than only off a refusal, because a
// response saying none is left is the last request before the wall rather than
// the first one after it. Acting there is the difference between a crawl that
// stays inside its quota and one that finds the edge by hitting it.
func spent(resp *http.Response, now time.Time) time.Time {
	if resp == nil {
		return time.Time{}
	}
	for _, pair := range [...][2]string{
		{headerRemaining, headerReset},
		{headerLegacyRemains, headerLegacyReset},
		{headerHyphenRemains, headerHyphenReset},
	} {
		v := resp.Header.Get(pair[0])
		if v == "" {
			continue
		}
		left, err := strconv.ParseInt(v, 10, 64)
		if err != nil || left > 0 {
			continue
		}
		if d := resetIn(resp.Header.Get(pair[1]), now); d > 0 {
			return now.Add(d)
		}
	}
	return time.Time{}
}

// maxReset bounds what a reset header is believed to be saying.
//
// A service that reports a window rolling over in four hours has almost
// certainly sent a timestamp in a format nobody expected, and a crawl that
// believed it would stop for the afternoon. The cap is well above any real
// window and well below any misreading worth acting on.
const maxReset = time.Hour

// resetIn reads a reset header, which is either a delta in seconds or a Unix
// timestamp depending on which convention the service follows.
//
// The two are told apart by size. A delta is a number of seconds in a window,
// so it is small, and a Unix timestamp has been ten figures since 2001. There
// is no ambiguity in the range that matters: a delta large enough to be
// mistaken for a timestamp would be a window of three hundred years.
func resetIn(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	// Some services send a fractional number of seconds, which parses as a
	// float and not as an integer.
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		return 0
	}
	var d time.Duration
	if n > 1e9 {
		d = time.Unix(int64(n), 0).Sub(now)
	} else {
		d = time.Duration(n * float64(time.Second))
	}
	if d <= 0 || d > maxReset {
		return 0
	}
	return d
}

// halfToFull is the default jitter: somewhere between half of the computed wait
// and all of it.
//
// The lower bound is half rather than zero because a jitter that can return
// almost nothing turns the first retry of a fleet into the same stampede the
// jitter was added to prevent.
func halfToFull(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int64N(int64(d)/2+1))
}
