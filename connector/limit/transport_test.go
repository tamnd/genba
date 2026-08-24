package limit_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tamnd/genba/connector/limit"
)

// reply is one scripted answer from the service the transport is talking to.
type reply struct {
	status int
	header http.Header
	err    error
}

// service is a round tripper that answers from a script.
//
// The last reply in the script repeats, so a test that wants a source which
// refuses everything writes one refusal rather than guessing how many attempts
// the transport is going to make. Bodies count their own closes, because a
// discarded response whose body is left open is a leaked connection and that is
// exactly the sort of thing a retry loop gets wrong.
type service struct {
	mu      sync.Mutex
	replies []reply
	seen    []string

	bodies atomic.Int64
}

func newService(replies ...reply) *service {
	if len(replies) == 0 {
		replies = []reply{{status: http.StatusOK}}
	}
	return &service{replies: replies}
}

func (s *service) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.seen = append(s.seen, req.Method+" "+req.URL.Path)
	r := s.replies[0]
	if len(s.replies) > 1 {
		s.replies = s.replies[1:]
	}
	s.mu.Unlock()

	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{
		StatusCode: r.status,
		Status:     fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)),
		Header:     r.header.Clone(),
		Body:       &countingBody{closes: &s.bodies},
		Request:    req,
	}, nil
}

// calls is how many requests actually reached the service, which is how a test
// tells a request that was refused from one that was never made.
func (s *service) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

type countingBody struct{ closes *atomic.Int64 }

func (countingBody) Read([]byte) (int, error) { return 0, io.EOF }

func (b *countingBody) Close() error {
	b.closes.Add(1)
	return nil
}

// header builds a response header from alternating names and values.
func header(kv ...string) http.Header {
	h := make(http.Header, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// noJitter and fullJitter pin the spread so that a test can assert the wait the
// transport computed rather than a random fraction of it.
func noJitter(time.Duration) time.Duration     { return 0 }
func fullJitter(d time.Duration) time.Duration { return d }

// get builds a request the transport is willing to retry.
func get(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.test"+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// status makes one request and returns what came back, failing the test if the
// transport returned an error.
//
// A refusal is a response and not an error, which is the shape of almost every
// case here, and the few that really are errors go through refused instead. The
// status is all a caller gets because the body is closed on the way out, which
// keeps every response the service handed over accounted for.
func status(t *testing.T, rt http.RoundTripper, req *http.Request) int {
	t.Helper()
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// refused makes one request that is expected not to happen, and returns why.
func refused(t *testing.T, rt http.RoundTripper, req *http.Request) error {
	t.Helper()
	resp, err := rt.RoundTrip(req)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		t.Fatalf("%s %s went out and came back %s", req.Method, req.URL, resp.Status)
	}
	return err
}

// The ordinary case costs nothing: one request, no waiting, no retrying.
func TestAnAnswerThatWorksIsAskedForOnce(t *testing.T) {
	svc := newService()
	clock := newClock()
	tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000},
		limit.WithBase(svc), limit.WithClock(clock))

	if got := status(t, tr, get(t, "/one")); got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	if got := svc.calls(); got != 1 {
		t.Errorf("the service was asked %d times, want 1", got)
	}
	stats := tr.Stats()
	if stats.Requests != 1 || stats.Retries != 0 || stats.Trips != 0 || stats.Open {
		t.Errorf("stats = %+v", stats)
	}
	if got := clock.total(); got != 0 {
		t.Errorf("an unthrottled request waited %v", got)
	}
}

// Too many requests is the case this whole package exists for, and the answer
// to it is to come back later rather than to fail the sync.
func TestARefusalForTooManyRequestsIsTriedAgain(t *testing.T) {
	svc := newService(
		reply{status: http.StatusTooManyRequests},
		reply{status: http.StatusOK},
	)
	clock := newClock()
	tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000, MinBackoff: 100 * time.Millisecond},
		limit.WithBase(svc), limit.WithClock(clock), limit.WithJitter(fullJitter))

	if got := status(t, tr, get(t, "/two")); got != http.StatusOK {
		t.Fatalf("status = %d, want the answer from the second attempt", got)
	}
	if got := svc.calls(); got != 2 {
		t.Errorf("the service was asked %d times, want 2", got)
	}
	if got := tr.Stats().Retries; got != 1 {
		t.Errorf("retries = %d, want 1", got)
	}
	if got := clock.sleeps(); len(got) != 1 || got[0] != 100*time.Millisecond {
		t.Errorf("waited %v, want one wait of 100ms", got)
	}
	// Both bodies were closed, the refusal the transport threw away and the
	// answer the caller read. A retry loop that dropped the first one opens a
	// socket per refusal, which is the last thing a throttled crawl needs.
	if got, want := svc.bodies.Load(), int64(svc.calls()); got != want {
		t.Errorf("%d of %d bodies were closed", got, want)
	}
}

// A source that names its own time is honoured, in both spellings of the header
// that are actually in use.
func TestTheSourcesOwnRetryTimeIsHonoured(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"seconds", header("Retry-After", "20"), 20 * time.Second},
		{"an http date", header("Retry-After", start.Add(30*time.Second).Format(http.TimeFormat)), 30 * time.Second},
		{"a reset as a delta", header("RateLimit-Reset", "12"), 12 * time.Second},
		{"a fractional reset", header("RateLimit-Reset", "1.5"), 1500 * time.Millisecond},
		{"a legacy reset as a timestamp", header("X-RateLimit-Reset", strconv.FormatInt(start.Add(45*time.Second).Unix(), 10)), 45 * time.Second},
		// Okta's spelling, which is a different header name once it has been
		// canonicalised and not a variation anything reads by accident.
		{"a reset with the hyphen in the other place", header("X-Rate-Limit-Reset", strconv.FormatInt(start.Add(60*time.Second).Unix(), 10)), 60 * time.Second},
		{"a date already gone by", header("Retry-After", start.Add(-time.Hour).Format(http.TimeFormat)), 0},
		{"a reset too far off to believe", header("RateLimit-Reset", "7200"), 0},
		{"nonsense", header("Retry-After", "soon"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newService(
				reply{status: http.StatusTooManyRequests, header: tt.header},
				reply{status: http.StatusOK},
			)
			clock := newClock()
			tr := limit.NewTransport(
				limit.Limits{Rate: 1000, Burst: 1000, MinBackoff: time.Second, MaxBackoff: 5 * time.Minute},
				limit.WithBase(svc), limit.WithClock(clock), limit.WithJitter(noJitter))

			status(t, tr, get(t, "/three"))

			// Nothing named means the computed backoff, which the jitter here
			// takes down to nothing.
			want := []time.Duration{tt.want}
			if tt.want == 0 {
				want = nil
			}
			if got := clock.sleeps(); len(got) != len(want) || (len(got) == 1 && got[0] != want[0]) {
				t.Errorf("waited %v, want %v", got, want)
			}
		})
	}
}

// The wait doubles and then stops doubling, so a source that is down for an
// hour is asked about occasionally rather than exponentially less often until
// the crawl gives up on its own.
func TestTheWaitDoublesAndIsBounded(t *testing.T) {
	svc := newService(reply{status: http.StatusServiceUnavailable})
	clock := newClock()
	tr := limit.NewTransport(limit.Limits{
		Rate:       1000,
		Burst:      1000,
		MaxRetries: 4,
		MinBackoff: 100 * time.Millisecond,
		MaxBackoff: 300 * time.Millisecond,
	}, limit.WithBase(svc), limit.WithClock(clock), limit.WithJitter(fullJitter))

	if got := status(t, tr, get(t, "/four")); got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the refusal to be returned once the attempts ran out", got)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond}
	if got := clock.sleeps(); !slices.Equal(got, want) {
		t.Errorf("waited %v, want %v", got, want)
	}
	stats := tr.Stats()
	if stats.Requests != 5 || stats.Retries != 4 {
		t.Errorf("stats = %+v, want five attempts of which four were retries", stats)
	}
}

// A four hundred is not retried. The service considers the request malformed
// and it will be just as malformed the second time.
func TestARefusalThatWillNotChangeIsNotTriedAgain(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			svc := newService(reply{status: code})
			tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000},
				limit.WithBase(svc), limit.WithClock(newClock()))

			if got := status(t, tr, get(t, "/five")); got != code {
				t.Fatalf("status = %d, want the refusal to come straight back", got)
			}
			if got := svc.calls(); got != 1 {
				t.Errorf("the service was asked %d times, want 1", got)
			}
		})
	}
}

// A request that never got an answer is worth another go. A connection reset is
// the most common thing that goes wrong on a long crawl and it says nothing at
// all about the document being read.
func TestARequestThatGotNoAnswerIsTriedAgain(t *testing.T) {
	svc := newService(
		reply{err: errors.New("connection reset by peer")},
		reply{status: http.StatusOK},
	)
	tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000},
		limit.WithBase(svc), limit.WithClock(newClock()), limit.WithJitter(noJitter))

	if got := status(t, tr, get(t, "/six")); got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	if got := svc.calls(); got != 2 {
		t.Errorf("the service was asked %d times, want 2", got)
	}
}

// Only a request that can be sent twice is sent twice.
func TestARequestThatCannotBeRepeatedIsNotRetried(t *testing.T) {
	tests := []struct {
		name   string
		method string
		body   io.Reader
	}{
		{"a get with a body", http.MethodGet, strings.NewReader("query")},
		{"a post", http.MethodPost, nil},
		{"a delete", http.MethodDelete, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newService(reply{status: http.StatusServiceUnavailable})
			tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000},
				limit.WithBase(svc), limit.WithClock(newClock()))

			req, err := http.NewRequestWithContext(t.Context(), tt.method, "https://example.test/seven", tt.body)
			if err != nil {
				t.Fatal(err)
			}
			status(t, tr, req)
			if got := svc.calls(); got != 1 {
				t.Errorf("the service was asked %d times, want 1", got)
			}
		})
	}
}

// A response saying the quota is gone is the last request before the wall, and
// the next one waits for the window to roll over rather than finding the edge
// by hitting it.
func TestTheQuotaBeingSpentHoldsTheNextRequestBack(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"the draft headers", header("RateLimit-Remaining", "0", "RateLimit-Reset", "30"), 30 * time.Second},
		{
			"the older headers",
			header("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(start.Add(90*time.Second).Unix(), 10)),
			90 * time.Second,
		},
		{
			"the headers with the hyphen in the other place",
			header("X-Rate-Limit-Remaining", "0", "X-Rate-Limit-Reset", strconv.FormatInt(start.Add(120*time.Second).Unix(), 10)),
			120 * time.Second,
		},
		{"some quota left", header("RateLimit-Remaining", "4", "RateLimit-Reset", "30"), 0},
		{"no reset to go with it", header("RateLimit-Remaining", "0"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newService(reply{status: http.StatusOK, header: tt.header}, reply{status: http.StatusOK})
			clock := newClock()
			tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000},
				limit.WithBase(svc), limit.WithClock(clock))

			status(t, tr, get(t, "/eight"))
			status(t, tr, get(t, "/nine"))

			if got := clock.total(); got != tt.want {
				t.Errorf("the second request waited %v, want %v", got, tt.want)
			}
			want := int64(0)
			if tt.want > 0 {
				want = 1
			}
			if got := tr.Stats().Limiter.Pauses; got != want {
				t.Errorf("pauses = %d, want %d", got, want)
			}
		})
	}
}

// A source that has been refusing everything is stopped rather than retried
// forever. The sync fails, the next scheduled refresh tries again, and the
// outage stops looking like load.
func TestTheCircuitOpensAfterEnoughFailures(t *testing.T) {
	svc := newService(reply{status: http.StatusInternalServerError})
	clock := newClock()
	tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000, MaxRetries: -1, Failures: 3, Cooldown: time.Minute},
		limit.WithBase(svc), limit.WithClock(clock))

	for range 3 {
		status(t, tr, get(t, "/ten"))
	}
	if got := svc.calls(); got != 3 {
		t.Fatalf("the service was asked %d times, want 3", got)
	}

	err := refused(t, tr, get(t, "/ten"))
	if !errors.Is(err, limit.ErrOpen) {
		t.Fatalf("err = %v, want the circuit to be open", err)
	}
	// The request never went out, which is the point of the breaker.
	if got := svc.calls(); got != 3 {
		t.Errorf("the service was asked %d times after the circuit opened, want 3", got)
	}
	stats := tr.Stats()
	if stats.Trips != 1 || !stats.Open {
		t.Errorf("stats = %+v, want one trip and an open circuit", stats)
	}
}

// The breaker counts consecutive failures, because a crawl of any size has a
// few failures and a source that is actually broken has nothing else.
func TestOneGoodAnswerClearsTheCount(t *testing.T) {
	svc := newService(
		reply{status: http.StatusInternalServerError},
		reply{status: http.StatusOK},
		reply{status: http.StatusInternalServerError},
		reply{status: http.StatusOK},
	)
	tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000, MaxRetries: -1, Failures: 2},
		limit.WithBase(svc), limit.WithClock(newClock()))

	for range 4 {
		status(t, tr, get(t, "/eleven"))
	}
	if got := tr.Stats().Trips; got != 0 {
		t.Errorf("trips = %d, want the circuit to have stayed closed", got)
	}
}

// Forbidden is the ordinary answer for one object out of a million that this
// account may not read, and a breaker that tripped on it would stop a healthy
// crawl over a handful of objects that were never part of the corpus.
func TestForbiddenDoesNotStopACrawl(t *testing.T) {
	svc := newService(reply{status: http.StatusForbidden})
	tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000, MaxRetries: -1, Failures: 2},
		limit.WithBase(svc), limit.WithClock(newClock()))

	for range 10 {
		status(t, tr, get(t, "/twelve"))
	}
	if got := tr.Stats().Trips; got != 0 {
		t.Errorf("trips = %d, want forbidden to be an ordinary answer", got)
	}
}

// A revoked token is exactly the state the breaker is there to notice, and it
// is not going to fix itself on the next document.
func TestARevokedTokenStopsTheCrawl(t *testing.T) {
	svc := newService(reply{status: http.StatusUnauthorized})
	tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000, MaxRetries: -1, Failures: 2},
		limit.WithBase(svc), limit.WithClock(newClock()))

	for range 2 {
		status(t, tr, get(t, "/thirteen"))
	}
	if err := refused(t, tr, get(t, "/thirteen")); !errors.Is(err, limit.ErrOpen) {
		t.Fatalf("err = %v, want the circuit to be open", err)
	}
}

// Once the cooldown is over one request goes through to find out whether the
// source has recovered, and a good answer puts the crawl back to normal.
func TestTheCircuitClosesAgainWhenTheSourceRecovers(t *testing.T) {
	svc := newService(
		reply{status: http.StatusInternalServerError},
		reply{status: http.StatusInternalServerError},
		reply{status: http.StatusOK},
	)
	clock := newClock()
	tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000, MaxRetries: -1, Failures: 2, Cooldown: time.Minute},
		limit.WithBase(svc), limit.WithClock(clock))

	for range 2 {
		status(t, tr, get(t, "/fourteen"))
	}
	if err := refused(t, tr, get(t, "/fourteen")); !errors.Is(err, limit.ErrOpen) {
		t.Fatalf("err = %v, want the circuit to be open", err)
	}

	clock.advance(2 * time.Minute)
	if got := status(t, tr, get(t, "/fourteen")); got != http.StatusOK {
		t.Fatalf("the probe got %d", got)
	}
	if got := status(t, tr, get(t, "/fourteen")); got != http.StatusOK {
		t.Fatalf("the request after the probe got %d", got)
	}
	stats := tr.Stats()
	if stats.Open || stats.Trips != 1 {
		t.Errorf("stats = %+v, want a closed circuit and the one trip it had", stats)
	}
}

// A probe that fails reopens the circuit straight away rather than after
// another full count of failures, because those failures have already happened.
func TestAProbeThatFailsReopensTheCircuitAtOnce(t *testing.T) {
	svc := newService(reply{status: http.StatusInternalServerError})
	clock := newClock()
	tr := limit.NewTransport(limit.Limits{Rate: 1000, Burst: 1000, MaxRetries: -1, Failures: 5, Cooldown: time.Minute},
		limit.WithBase(svc), limit.WithClock(clock))

	for range 5 {
		status(t, tr, get(t, "/fifteen"))
	}
	clock.advance(2 * time.Minute)

	status(t, tr, get(t, "/fifteen"))
	if err := refused(t, tr, get(t, "/fifteen")); !errors.Is(err, limit.ErrOpen) {
		t.Fatalf("err = %v, want the circuit open again after one failed probe", err)
	}
	if got := tr.Stats().Trips; got != 2 {
		t.Errorf("trips = %d, want 2", got)
	}
}

// The rate applies to requests made through the transport, which is the whole
// reason it is a round tripper.
func TestTheTransportSpacesRequestsOut(t *testing.T) {
	svc := newService()
	clock := newClock()
	tr := limit.NewTransport(limit.Limits{Rate: 10, Burst: 1, MaxRetries: -1},
		limit.WithBase(svc), limit.WithClock(clock))

	for range 3 {
		status(t, tr, get(t, "/sixteen"))
	}
	if got := clock.total(); got != 200*time.Millisecond {
		t.Errorf("three requests out of a bucket of one waited %v, want 200ms", got)
	}
	if got := tr.Limiter().Stats().Waits; got != 2 {
		t.Errorf("waits = %d, want 2", got)
	}
}

// A shutdown does not fire one last request.
func TestACancelledRequestNeverGoesOut(t *testing.T) {
	svc := newService()
	tr := limit.NewTransport(limit.Limits{Rate: 1, Burst: 1},
		limit.WithBase(svc), limit.WithClock(newClock()))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test/seventeen", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	// The refusal comes from the limiter rather than from the service, which is
	// the whole point: the request never went out at all.
	if err := refused(t, tr, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if got := svc.calls(); got != 0 {
		t.Errorf("the service was asked %d times, want 0", got)
	}
}
