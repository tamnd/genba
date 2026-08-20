// Package limit keeps a connector inside the limits of the service it is
// reading.
//
// A crawler that ignores an API's limits gets the company's integration token
// revoked, and that is a worse outcome than a slow crawl by a wide margin. A
// slow crawl finishes late. A revoked token is an index that stops updating,
// a conversation with whoever owns the integration, and in most companies a
// week before anybody is allowed to try again.
//
// # What it is
//
// [Transport] is an [net/http.RoundTripper] that wraps another one and does
// four things around every request: it waits for a token so that the request
// rate stays under a ceiling, it reads what the response says about the quota
// and holds the next request back when the quota is gone, it retries what is
// worth retrying with bounded jittered backoff, and it stops the source
// altogether when it has been refusing everything.
//
// It is a round tripper rather than a wrapper around each connector because
// that is the layer where the requests actually are. A connector built on
// [net/http.Client] gets all of this by being handed a different client, which
// is one line at the call site and nothing at all inside the connector.
//
//	client := &http.Client{Transport: limit.NewTransport(limit.Limits{Rate: 8, Burst: 16})}
//	src, err := objectsource.New(objectsource.NewClient(cfg, objectsource.WithHTTPClient(client)), ...)
//
// # One limiter per source
//
// A limiter belongs to one source because a quota does. Two connectors reading
// two different services have nothing to do with each other and sharing a
// limiter between them would mean a slow wiki holding up a fast bucket. Two
// connectors reading the same service with the same credentials share a quota
// whether they like it or not, and those two should share one [Transport].
//
// # What is not here
//
// There is no queue and no worker pool. A connector's Sync is one goroutine
// making one request at a time, blocked while the pipeline indexes what it
// handed over, and that backpressure is the thing that keeps memory bounded.
// Adding concurrency here would remove it.
package limit

import (
	"errors"
	"fmt"
	"time"
)

// The defaults, which are deliberately cautious.
//
// A default that is too slow costs a longer first sync and nothing else. A
// default that is too fast costs the token. Anybody who knows their quota can
// say so, and anybody who does not is better served by the crawl that finishes
// than by the one that gets cut off.
const (
	DefaultRate       = 5.0
	DefaultBurst      = 10
	DefaultMaxRetries = 4
	DefaultMinBackoff = 500 * time.Millisecond
	DefaultMaxBackoff = 30 * time.Second
	DefaultFailures   = 5
	DefaultCooldown   = time.Minute
)

// Limits is what a source is allowed to cost.
//
// Every field has a default and the zero value is a working configuration, so
// a caller who only knows one of these numbers sets that one.
type Limits struct {
	// Rate is the sustained ceiling in requests per second. A negative rate is
	// rejected and zero selects [DefaultRate].
	//
	// There is no value meaning unlimited, on purpose. A crawler with no ceiling
	// is the thing this package exists to prevent, and an operator who wants one
	// can ask for a rate high enough that it never binds, which is a number in
	// the log rather than a special case in the code.
	Rate float64

	// Burst is how many requests may go out back to back before the rate binds.
	//
	// It exists because real work is lumpy. A page of a listing followed
	// immediately by the four documents on it is a burst of five, and a limiter
	// with no burst would space them out for no reason: the quota is a rate over
	// a window and the window is not one request wide.
	Burst int

	// MaxRetries is how many times one request is tried again after a refusal
	// worth retrying. Zero selects [DefaultMaxRetries] and a negative value
	// turns retrying off.
	MaxRetries int

	// MinBackoff and MaxBackoff bound the wait between attempts. The wait
	// doubles from the first towards the second and is jittered, and a source
	// that named its own time in a header overrides both.
	MinBackoff time.Duration
	MaxBackoff time.Duration

	// Failures is how many consecutive failures open the circuit. Zero selects
	// [DefaultFailures].
	Failures int

	// Cooldown is how long the circuit stays open before one request is let
	// through to find out whether the source has recovered.
	Cooldown time.Duration
}

// withDefaults fills in what the caller left alone.
func (l Limits) withDefaults() Limits {
	if l.Rate == 0 {
		l.Rate = DefaultRate
	}
	if l.Burst <= 0 {
		l.Burst = DefaultBurst
	}
	if l.MaxRetries == 0 {
		l.MaxRetries = DefaultMaxRetries
	}
	if l.MaxRetries < 0 {
		l.MaxRetries = 0
	}
	if l.MinBackoff <= 0 {
		l.MinBackoff = DefaultMinBackoff
	}
	if l.MaxBackoff <= 0 {
		l.MaxBackoff = DefaultMaxBackoff
	}
	if l.MaxBackoff < l.MinBackoff {
		l.MaxBackoff = l.MinBackoff
	}
	if l.Failures <= 0 {
		l.Failures = DefaultFailures
	}
	if l.Cooldown <= 0 {
		l.Cooldown = DefaultCooldown
	}
	return l
}

// Validate reports the first thing about the limits that would make a crawl
// misbehave.
//
// It is separate from the defaults because the two answer different questions.
// A field left at zero is somebody who did not have an opinion, and a field set
// to minus one is somebody who had one and got it wrong.
func (l Limits) Validate() error {
	errs := make([]error, 0, 3)
	if l.Rate < 0 {
		errs = append(errs, fmt.Errorf("limit: rate %v is negative", l.Rate))
	}
	if l.Burst < 0 {
		errs = append(errs, fmt.Errorf("limit: burst %d is negative", l.Burst))
	}
	if l.MinBackoff < 0 || l.MaxBackoff < 0 {
		errs = append(errs, errors.New("limit: a backoff is negative"))
	}
	if l.Cooldown < 0 {
		errs = append(errs, errors.New("limit: cooldown is negative"))
	}
	return errors.Join(errs...)
}
