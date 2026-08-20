// Package jirasource indexes a Jira site.
//
// It is an adapter and nothing more. The crawl, the cursor, the resume, the
// reconciliation sweep and the permission refresh are all in [threadsource],
// and what is here is the four questions that package asks, answered against
// the Jira Cloud REST API: list the projects and say who may read each one,
// walk the issues in a project that changed since a time, list the issue keys a
// project holds, and read one issue by its key.
//
// # An issue is the document
//
// A ticket is a summary, a description and the argument underneath it, and the
// argument is where the answer usually is. A comment is not a document any more
// than a reply is: it is a sentence in one. So an issue is fetched with its
// comments, assembled by [thread.Conversation.Document] the same way a chat
// thread is, and indexed as a single result that ranks as a whole.
//
// The difference from chat is who the document is by. A Slack thread is by the
// person who started it. A ticket is by its reporter, who is very often not the
// person who typed the description, and the assignee is a different person
// again. The reporter is the author, and both of them are on the document as
// properties, because "tickets Sam reported" and "tickets Sam is working on"
// are different questions and an index that conflated them answers neither.
//
// # Permissions
//
// Jira decides who may read an issue in two places, and both of them are here.
//
// The project is the first. A project's browse permission is granted to roles,
// to groups and to individual accounts through a permission scheme, and what
// comes out of resolving it is a list of accounts and a list of groups, which is
// an access control list. A project nobody can resolve a rule for is
// quarantined rather than guessed at.
//
// The issue security level is the second, and it is the reason [threadsource]
// lets a conversation override its container at all. An issue with a security
// level on it is readable by that level's members and by nobody else, whatever
// the project says, and a crawl that applied the project's rule to it would
// publish exactly the tickets somebody went out of their way to restrict. Where
// the level's members can be resolved they become the rule. Where they cannot,
// because resolving them needs an administrator and this token is not one, the
// issue is quarantined and reported. Quarantining a restricted ticket is the
// only safe way to be wrong about it.
//
// # What incremental means here
//
// Unlike chat, Jira has a real change feed, and it is JQL. Every issue carries
// an updated time, a comment moves it, an edit moves it, a transition moves it,
// and JQL will both filter and order by it. So a sync asks for the issues in a
// project updated at or after the cursor, in ascending order of update, and
// that is the whole of it: no window, no guessing, and nothing found by the
// sweep that the sync should have caught.
//
// What the sweep is still for is deletion. Nothing in JQL reports an issue that
// was deleted or moved to another project, so the listing is what removes it,
// and the version it reports is the issue's updated time.
//
// # Rate limits
//
// Jira Cloud does not publish a request rate. It bills by cost, the cost of a
// request depends on what was asked for, and the way a client learns it has
// spent too much is a 429 with a Retry-After header. So there is one bucket
// rather than the per method tiers a source with published rates gets, it is
// set to something a shared site will tolerate, and the header is what actually
// governs. All of that is [limit] doing the work: the retries, the backoff, the
// Retry-After header and the circuit breaker are the same ones every connector
// in this repository uses.
package jirasource

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
const DefaultName = "jira"

// DefaultPageSize is how many items are asked for per page.
//
// Jira caps a search at a hundred and will silently return fewer, which is not
// something to argue with: asking for more than the cap and getting less is how
// a crawl convinces itself it has reached the end of a project.
const DefaultPageSize = 100

// DefaultRate is how many requests a second the adapter allows itself.
//
// There is no published number to match. Jira Cloud bills by cost and answers
// with a Retry-After when the bill is too high, so this is a floor that keeps a
// crawl from being the reason somebody else's integration starts seeing 429s,
// and the header is what governs when it is not enough.
const DefaultRate = 5

// Service answers [threadsource.Service] against a Jira site.
type Service struct {
	http       *http.Client
	base       string
	email      string
	token      string
	name       string
	page       int
	aclRefresh time.Duration
	now        func() time.Time
	levels     *security
	skipped    func(id string, reason error)
}

var _ threadsource.Service = (*Service)(nil)

// Option configures a [Service].
type Option func(*Service)

// WithName sets the source name, which is what every document id from this site
// is prefixed with. A deployment indexing two sites needs two names, because
// the ids from them would otherwise collide.
func WithName(name string) Option {
	return func(s *Service) { s.name = name }
}

// WithHTTPClient replaces the client entirely, rate limiting included.
//
// A caller who passes one is taking on the limits themselves, which is right
// for a test replaying a recording and wrong for anything talking to Jira.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Service) { s.http = c }
}

// WithLimits changes what the site is allowed to cost. Zero rate selects
// [DefaultRate].
func WithLimits(l limit.Limits) Option {
	return func(s *Service) {
		if l.Rate <= 0 {
			l.Rate = DefaultRate
		}
		s.http = limited(s.email, s.token, l)
	}
}

// WithPageSize sets how many items are asked for per page. Zero selects
// [DefaultPageSize].
func WithPageSize(n int) Option {
	return func(s *Service) { s.page = n }
}

// WithSkipped is told about a project or an issue that could not be indexed,
// with the reason.
//
// The two worth having it for are a project whose permission scheme this token
// may not read and an issue behind a security level whose members it may not
// resolve. Both are quarantined, and an index quietly missing what nobody could
// read looks exactly like an index that is complete.
func WithSkipped(f func(id string, reason error)) Option {
	return func(s *Service) { s.skipped = f }
}

// WithClock replaces the clock the permission refresh schedule is measured
// against, which is what a test needs to make a day pass without waiting one.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService builds the adapter on its own, which is what a caller wanting to
// wrap it or test it against a recording needs.
//
// The site is the base URL of a Jira Cloud instance, and the credentials are an
// account's email address and an API token, which is what Jira's basic
// authentication expects rather than a password.
func NewService(site, email, token string, opts ...Option) (*Service, error) {
	switch {
	case site == "":
		return nil, errors.New("jirasource: a site URL is required")
	case email == "":
		return nil, errors.New("jirasource: an account email is required")
	case token == "":
		return nil, errors.New("jirasource: an API token is required")
	}

	s := &Service{
		base:  strings.TrimSuffix(site, "/"),
		email: email,
		token: token,
		name:  DefaultName,
		page:  DefaultPageSize,
		now:   time.Now,
	}
	s.http = limited(email, token, limit.Limits{Rate: DefaultRate})
	for _, opt := range opts {
		opt(s)
	}
	if s.name == "" {
		return nil, errors.New("jirasource: the source name cannot be empty")
	}
	if s.page < 1 {
		s.page = DefaultPageSize
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.levels = newSecurity(s)
	return s, nil
}

// New builds a connector over a Jira site, ready to be handed to the ingestion
// pipeline.
func New(site, email, token string, opts ...Option) (*threadsource.Source, error) {
	svc, err := NewService(site, email, token, opts...)
	if err != nil {
		return nil, err
	}
	var wrapped []threadsource.Option
	if svc.skipped != nil {
		wrapped = append(wrapped, threadsource.WithSkipped(svc.skipped))
	}
	return threadsource.New(svc, svc.name, wrapped...)
}

// Name is the source name documents from this site are filed under.
func (s *Service) Name() string { return s.name }

// skip reports something that could not be indexed, if anybody asked to hear
// about it.
func (s *Service) skip(id string, reason error) {
	if s.skipped != nil {
		s.skipped(id, reason)
	}
}

// Error is what Jira says when it refuses.
//
// Unlike some APIs Jira does use status codes, so the status is what a caller
// matches on. The messages are kept because a 400 from JQL is a syntax error
// with the offending clause in it, and a crawl that threw that away would
// report "bad request" for a thing the site was willing to explain.
type Error struct {
	Path     string
	Status   int
	Messages []string
}

func (e *Error) Error() string {
	if len(e.Messages) == 0 {
		return fmt.Sprintf("jira: %s: %d", e.Path, e.Status)
	}
	return fmt.Sprintf("jira: %s: %d: %s", e.Path, e.Status, strings.Join(e.Messages, "; "))
}

// refused reports whether an error is Jira saying no with this particular
// status, which is how a caller tells "you may not read that project" from "the
// network went away".
func refused(err error, status int) bool {
	var je *Error
	return errors.As(err, &je) && je.Status == status
}

// missing reports whether an error is Jira saying the thing is not there.
//
// Both statuses mean it for our purposes. A 404 is an issue that was deleted,
// and a 403 on a single issue read is an issue that was moved somewhere this
// token cannot follow, which from the index's point of view is the same event:
// it is not ours to hold any more.
func missing(err error) bool {
	return refused(err, http.StatusNotFound) || refused(err, http.StatusForbidden)
}
