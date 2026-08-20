// Package slacksource indexes a Slack workspace.
//
// It is an adapter and nothing more. The crawl, the cursor, the resume, the
// reconciliation sweep and the permission refresh are all in
// [threadsource], and what is here is the four questions that package asks,
// answered against the Slack Web API: list the channels and say who may read
// each one, walk the threads in a channel that changed since a time, list the
// thread ids a channel holds, and read one thread by its id.
//
// # A thread is the document
//
// Slack stores a conversation as a parent message and a list of replies, and a
// reply is not a document. It is a sentence in one. So a thread is fetched
// whole, assembled by [thread.Conversation.Document], and indexed as a single
// result that ranks as a whole.
//
// # Permissions
//
// A public channel is readable by everybody in the workspace, so it maps to
// [acl.ModePublicToTenant]. A private channel is readable by its members and by
// nobody else, so it maps to an access control list naming them, which is one
// extra request per private channel per sync rather than per thread.
//
// A direct message conversation is not indexed at all. There is no rule that
// makes one safe to put in a shared index: the members are two people, the
// content is theirs, and a product that indexed them would be one nobody could
// deploy. They are skipped by not asking for them.
//
// A channel this token cannot see is a channel that is not listed, which is the
// end of it. The interesting case is the channel that is listed and then refuses
// to be read, which happens when a bot is in the workspace but not in the
// channel. That is reported rather than guessed at.
//
// # What incremental means here
//
// Slack has no "what changed since" endpoint. `conversations.history` is ordered
// by the time a message was posted, and a reply to a two year old thread does
// not move that thread anywhere: the parent keeps its original timestamp and
// only its latest_reply moves.
//
// So a sync reads the history of each channel back to the older of the cursor
// and the reply window, and emits a thread whose newest change is at or after
// the cursor. A late reply to a thread older than the window is not in the
// change feed at all, and is found by the reconciliation sweep instead: the
// listing reports each thread with a version derived from its newest message,
// and a version that differs from the stored one is a document the pipeline
// refetches. That is the honest arrangement. Widening the window costs history
// pages on every sync, and narrowing it moves work onto a sweep that runs less
// often, and neither of them loses anything.
//
// # Rate limits
//
// Slack limits per method rather than per token, in tiers, and a crawl that
// treated them as one number would either be throttled at the speed of its
// slowest method or be refused at the speed of its fastest. Each tier gets its
// own bucket, requests are routed to one by the method they name, and a 429 is
// obeyed by the tier that earned it. All of that is [limit] doing the work: the
// retries, the backoff, the Retry-After header and the circuit breaker are the
// same ones every connector in this repository uses.
package slacksource

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tamnd/genba/connector/limit"
	"github.com/tamnd/genba/connector/threadsource"
)

// DefaultName is the source name a document's id is prefixed with when the
// caller does not choose one.
const DefaultName = "slack"

// DefaultBaseURL is the Slack Web API. It is an option so that a test can point
// the adapter at a recording or at a server of its own.
const DefaultBaseURL = "https://slack.com/api"

// DefaultReplyWindow is how far back a sync reads history looking for threads
// that were replied to since the cursor.
//
// A week is chosen because it is roughly how long a Slack thread stays alive.
// A reply after it is not lost, it is found by the sweep instead.
const DefaultReplyWindow = 7 * 24 * time.Hour

// DefaultPageSize is how many items are asked for per page. Slack accepts up to
// a thousand and recommends far less, and two hundred is what its own
// documentation suggests for the listing endpoints.
const DefaultPageSize = 200

// Service answers [threadsource.Service] against a Slack workspace.
type Service struct {
	http       *http.Client
	base       string
	token      string
	name       string
	page       int
	window     time.Duration
	aclRefresh time.Duration
	now        func() time.Time
	users      *people
	skipped    func(id string, reason error)
}

var _ threadsource.Service = (*Service)(nil)

// Option configures a [Service].
type Option func(*Service)

// WithName sets the source name, which is what every document id from this
// workspace is prefixed with. A deployment indexing two workspaces needs two
// names, because the ids from them would otherwise collide.
func WithName(name string) Option {
	return func(s *Service) { s.name = name }
}

// WithBaseURL points the adapter somewhere other than Slack, which is what a
// test against a recording does.
func WithBaseURL(u string) Option {
	return func(s *Service) { s.base = strings.TrimSuffix(u, "/") }
}

// WithHTTPClient replaces the client entirely, rate limiting included.
//
// A caller who passes one is taking on the limits themselves, which is right
// for a test replaying a recording and wrong for anything talking to Slack.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Service) { s.http = c }
}

// WithLimits changes what the workspace is allowed to cost.
//
// The rates are per tier and this scales all of them, so a workspace that has
// asked us to go slower is one number rather than four.
func WithLimits(l limit.Limits) Option {
	return func(s *Service) { s.http = tiered(s.token, l) }
}

// WithReplyWindow sets how far back each sync reads history looking for threads
// that were replied to. Zero selects [DefaultReplyWindow].
func WithReplyWindow(d time.Duration) Option {
	return func(s *Service) { s.window = d }
}

// WithPageSize sets how many items are asked for per page. Zero selects
// [DefaultPageSize].
func WithPageSize(n int) Option {
	return func(s *Service) { s.page = n }
}

// WithSkipped is told about a channel or a thread that could not be indexed,
// with the reason.
//
// The case worth having it for is a bot that is in the workspace but not in a
// channel: the channel is listed and then refuses to be read, and an index
// quietly missing it looks exactly like an index that is complete.
func WithSkipped(f func(id string, reason error)) Option {
	return func(s *Service) { s.skipped = f }
}

// WithClock replaces the clock the reply window and the permission refresh
// schedule are measured against.
//
// It is here for tests, which need those two to be measured against a time they
// chose rather than against a real one, and it is exported rather than smuggled
// in through a build tag because a test in another package needs it too.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService builds the adapter on its own, which is what a caller wanting to
// wrap it or test it against a recording needs.
func NewService(token string, opts ...Option) (*Service, error) {
	if token == "" {
		return nil, errors.New("slacksource: a token is required")
	}
	s := &Service{
		base:   DefaultBaseURL,
		token:  token,
		name:   DefaultName,
		page:   DefaultPageSize,
		window: DefaultReplyWindow,
		now:    time.Now,
	}
	s.http = tiered(token, limit.Limits{})
	for _, opt := range opts {
		opt(s)
	}
	if s.name == "" {
		return nil, errors.New("slacksource: the source name cannot be empty")
	}
	if s.base == "" {
		return nil, errors.New("slacksource: the base URL cannot be empty")
	}
	if s.page < 1 {
		s.page = DefaultPageSize
	}
	if s.window <= 0 {
		s.window = DefaultReplyWindow
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.users = newPeople(s)
	return s, nil
}

// New builds a connector over a Slack workspace, ready to be handed to the
// ingestion pipeline.
func New(token string, opts ...Option) (*threadsource.Source, error) {
	svc, err := NewService(token, opts...)
	if err != nil {
		return nil, err
	}
	var wrapped []threadsource.Option
	if svc.skipped != nil {
		wrapped = append(wrapped, threadsource.WithSkipped(svc.skipped))
	}
	return threadsource.New(svc, svc.name, wrapped...)
}

// Name is the source name documents from this workspace are filed under.
func (s *Service) Name() string { return s.name }

// skip reports something that could not be indexed, if anybody asked to hear
// about it.
func (s *Service) skip(id string, reason error) {
	if s.skipped != nil {
		s.skipped(id, reason)
	}
}

// Error is what Slack says when it refuses. The string is the machine readable
// one from the body rather than the status, because a Slack refusal is a 200
// with ok false far more often than it is a status code.
type Error struct {
	Method string
	Code   string
}

func (e *Error) Error() string { return fmt.Sprintf("slack: %s: %s", e.Method, e.Code) }

// refused reports whether an error is Slack saying no with this particular
// code, which is how a caller tells "not in that channel" from "the network
// went away".
func refused(err error, code string) bool {
	var se *Error
	return errors.As(err, &se) && se.Code == code
}
