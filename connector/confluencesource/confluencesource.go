// Package confluencesource indexes a Confluence site.
//
// It is an adapter and nothing more. The crawl, the cursor, the resume, the
// reconciliation sweep and the permission refresh are all in [threadsource],
// and what is here is the four questions that package asks, answered against
// the Confluence Cloud REST API: list the spaces and say who may read each one,
// walk the pages in a space that changed since a time, list the page ids a
// space holds, and read one page by its id.
//
// # A page and its comments are one document
//
// A wiki page is a title, a body and the comments underneath it, and the
// comments are where the correction usually is. The page says the deploy runs
// at nine and the third comment says it moved to eleven eighteen months ago, so
// a page indexed without them is a page that answers with the wrong time.
//
// So a page is fetched with its comments, assembled by
// [thread.Conversation.Document] the same way a ticket and a chat thread are,
// and indexed as a single result that ranks as a whole.
//
// # Permissions
//
// Confluence decides who may read a page in two places, and both of them are
// here.
//
// The space is the first. A space's read permission is granted to accounts and
// to groups, and it may be granted to anonymous users, which is a space that is
// open rather than a space with an empty rule. What comes out of resolving it
// is a list of accounts and a list of groups, which is an access control list. A
// space nobody can resolve a rule for is quarantined rather than guessed at.
//
// A page restriction is the second, and it is the reason [threadsource] lets a
// conversation override its container at all. A page with a read restriction on
// it is readable by the people named in the restriction and by nobody else,
// whatever the space says.
//
// Restrictions inherit, and that is the part worth being careful about. A page
// with no restriction of its own under a parent that has one is restricted, so
// the ancestors are consulted as well, and a chain carrying a restriction at
// more than one level is quarantined rather than resolved. Confluence means the
// intersection of the two there, and an intersection of a list of accounts with
// a list of groups is not something that can be worked out from outside the
// identity provider. Publishing the wider of the two would publish exactly the
// pages somebody went out of their way to restrict.
//
// # What incremental means here
//
// Confluence has a real change feed and it is CQL. A page carries a version, an
// edit moves it, a comment on the page does not, and CQL will both filter and
// order by the time of the last change. So a sync asks for the pages in a space
// modified at or after the cursor, oldest first, and that is the whole of it.
//
// The wrinkle is the one the ticket adapter has: CQL compares times to the
// minute and rejects anything finer, so the query asks for the minute the cursor
// is in and what arrived from the first half of it is dropped after the fact.
// Rounding down rather than up is deliberate, because the direction that costs a
// re-read of a few pages is the one that does not lose them.
//
// A comment moves nothing in the listing, which is a real gap and not one this
// adapter can close from outside: Confluence dates the comment and not the page
// it is on. The sweep is what catches it, because the version a page reports is
// derived from the page and its comments together, so a page that was answered
// and not edited still reports a version the index does not have.
//
// What the sweep is also for is deletion. Nothing in CQL reports a page that was
// deleted, archived or moved to a space this token cannot see, and all three
// leave the index holding a page that is no longer there.
//
// # Which API version
//
// Everything here is version one of the REST API rather than version two, and
// that is a decision rather than an oversight. The two endpoints this connector
// cannot do without, the read permissions on a space and the read restrictions
// on a page, are version one endpoints, and version two neither replaces them
// nor reports the group names that an access control list has to be written in.
// Using both would mean two paging models, two shapes for the same page body and
// two sets of field names for the same page, which is the sort of seam that ends
// up with a permission read one way in one path and another way in the other.
//
// # Rate limits
//
// Confluence Cloud does not publish a request rate. It bills by the cost of what
// was asked for, and the way a client is told it has spent too much is a 429
// with a Retry-After on it. So there is one bucket, it is set to something a
// shared site will tolerate, and the header is what actually governs. All of
// that is [limit] doing the work, and it is the same code every other connector
// in this repository uses.
package confluencesource

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
const DefaultName = "confluence"

// DefaultPageSize is how many items are asked for per page.
//
// Half what the ticket adapter asks for, because what comes back is different.
// A page of search results from a tracker carries a hundred summaries and a page
// of results from a wiki carries fifty documents, since a wiki page's body is
// the document rather than a line describing one.
const DefaultPageSize = 50

// DefaultRate is how many requests a second the adapter allows itself.
//
// There is no published number to match. Confluence Cloud bills by cost and
// answers with a Retry-After when the bill is too high, so this is a floor that
// keeps a crawl from being the reason somebody else's integration starts seeing
// 429s, and the header is what governs when it is not enough.
const DefaultRate = 5

// Service answers [threadsource.Service] against a Confluence site.
type Service struct {
	http       *http.Client
	base       string
	wiki       string
	email      string
	token      string
	name       string
	page       int
	aclRefresh time.Duration
	now        func() time.Time
	restricted *restrictions
	skipped    func(id string, reason error)
}

var _ threadsource.Service = (*Service)(nil)

// Option configures a [Service].
type Option func(*Service)

// WithName sets the source name, which is what every document id from this site
// is prefixed with. A deployment indexing two sites needs two names, because the
// ids from them would otherwise collide.
func WithName(name string) Option {
	return func(s *Service) { s.name = name }
}

// WithHTTPClient replaces the client entirely, rate limiting included.
//
// A caller who passes one is taking on the limits themselves, which is right for
// a test replaying a recording and wrong for anything talking to Confluence.
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

// WithSkipped is told about a space or a page that could not be indexed, with
// the reason.
//
// The two worth having it for are a space whose permissions this token may not
// read and a page whose restrictions could not be worked out. Both are
// quarantined, and an index quietly missing what nobody could read looks exactly
// like an index that is complete.
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
// The site is the address of a Confluence Cloud instance, with or without the
// /wiki on the end, because both are what people have in front of them: /wiki is
// where the product lives and the bare host is what the browser bar says on the
// way in. The credentials are an account's email address and an API token, which
// is what Atlassian's basic authentication expects rather than a password.
func NewService(site, email, token string, opts ...Option) (*Service, error) {
	switch {
	case site == "":
		return nil, errors.New("confluencesource: a site URL is required")
	case email == "":
		return nil, errors.New("confluencesource: an account email is required")
	case token == "":
		return nil, errors.New("confluencesource: an API token is required")
	}

	base := strings.TrimSuffix(strings.TrimSuffix(site, "/"), "/wiki")
	s := &Service{
		base:  base,
		wiki:  base + "/wiki",
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
		return nil, errors.New("confluencesource: the source name cannot be empty")
	}
	if s.page < 1 {
		s.page = DefaultPageSize
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.restricted = newRestrictions(s)
	return s, nil
}

// New builds a connector over a Confluence site, ready to be handed to the
// ingestion pipeline.
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

// Error is what Confluence says when it refuses.
//
// The status is what a caller matches on. The message is kept because a 400 from
// CQL is a syntax error with the offending clause in it, and a crawl that threw
// that away would report "bad request" for a thing the site was willing to
// explain.
type Error struct {
	Path    string
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("confluence: %s: %d", e.Path, e.Status)
	}
	return fmt.Sprintf("confluence: %s: %d: %s", e.Path, e.Status, e.Message)
}

// refused reports whether an error is Confluence saying no with this particular
// status, which is how a caller tells "you may not read that space" from "the
// network went away".
func refused(err error, status int) bool {
	var ce *Error
	return errors.As(err, &ce) && ce.Status == status
}

// missing reports whether an error is Confluence saying the thing is not there.
//
// All three statuses mean it for our purposes. A 404 is a page that was deleted
// or archived, a 403 on a single read is a page that moved somewhere this token
// cannot follow, and a 401 on one page of a site the rest of which is answering
// is the same thing again. From the index's point of view they are one event: it
// is not ours to hold any more.
func missing(err error) bool {
	return refused(err, http.StatusNotFound) ||
		refused(err, http.StatusForbidden) ||
		refused(err, http.StatusUnauthorized)
}
