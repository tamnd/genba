// Package okta resolves group membership against an Okta organisation.
//
// It answers the two lookups [directory.Directory] asks for and nothing else.
// The closure, the cycle detection, the bound on how much one expansion may
// cost and the version an answer is stamped with are all in [directory], and
// they are shared by every provider, because those are exactly the parts that
// are easy to get subtly wrong and impossible to notice from the outside.
//
// # Okta groups do not contain groups
//
// This is the one fact about the provider that shapes everything here. Okta's
// model is flat: a user is a member of groups, and a group is a member of
// nothing. There is no nesting to walk, so [Directory.Group] always answers
// with an empty membership and every expansion is one level deep.
//
// That has a consequence for cost. The walk asks about each group the user is
// in, and a person in three hundred groups would be three hundred requests
// against an organisation that everybody else is signing in to at the same
// time. So the group listing that answers the subject lookup, which returns the
// whole group object rather than an id, fills a small buffer that the group
// lookups then read.
//
// It is worth being precise about why that is not a second permission cache,
// because [directory.Cache] documents at some length why there is only one of
// those. The fact being held here is "this group exists and is a member of no
// groups", and for a provider with no nesting that fact cannot become false in
// a way that changes an answer. A group that was deleted since it was buffered
// would move into [directory.Expansion] Unknown if it were looked up again, and
// a group in Unknown is still in the group set, because the directory saying
// somebody is a member is a statement about them. So the buffer changes how
// many requests an expansion costs and cannot change what it returns.
//
// # What counts as deactivated
//
// Okta has eight account states and only some of them are a person who can
// still be signed in. SUSPENDED and DEPROVISIONED are the two an administrator
// reaches for when somebody leaves, and STAGED is an account that was created
// and never activated. All three refuse. LOCKED_OUT and PASSWORD_EXPIRED do
// not, because both are a live account having a bad morning and neither is a
// decision anybody made about their access.
//
// # Rate limits
//
// Okta publishes its own numbers on every response and refuses with a 429 and
// a reset when they run out, which is what [limit] is for. The retries, the
// backoff and the circuit breaker are the same ones every adapter in this
// repository uses.
//
// The one thing worth knowing is that Okta spells the headers
// X-Rate-Limit-Remaining and X-Rate-Limit-Reset, with the hyphen in a different
// place from everybody else, and that canonicalises to a different header name
// entirely rather than to a variation something reads by accident. [limit]
// reads that spelling too, which is what makes holding back before a refusal
// possible here rather than only after one.
package okta

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/cache"
	"github.com/tamnd/genba/connector/limit"
	"github.com/tamnd/genba/directory"
)

// DefaultName is the identity source the groups belong to, which is what every
// group key from this organisation is prefixed with.
const DefaultName = "okta"

// DefaultPageSize is how many groups are asked for per page. Okta accepts up to
// a thousand on this endpoint and two hundred is what it suggests.
const DefaultPageSize = 200

// DefaultBufferTTL is how long the group listing that answered a subject lookup
// is kept for the group lookups that follow it.
//
// It is short because it only has to outlive one expansion, which is one round
// trip and a handful of lookups. See the package documentation for why holding
// it for longer would not change an answer either.
const DefaultBufferTTL = 30 * time.Second

// DefaultBufferSize is how many groups the buffer holds before it starts
// dropping the ones nobody has asked about recently.
const DefaultBufferSize = 20000

// Directory resolves subjects and groups against one Okta organisation.
//
// It is safe for concurrent use, which it has to be: an expansion looks up a
// level of the graph in parallel.
type Directory struct {
	http  *http.Client
	base  string
	token string
	name  string
	page  int

	// held is the group listing from a subject lookup, waiting for the group
	// lookups that are about to ask for the same objects.
	held *cache.Cache[directory.Group]
}

var _ directory.Directory = (*Directory)(nil)

// Option configures a [Directory].
type Option func(*Directory)

// WithName sets the identity source the group ids belong to.
//
// A deployment resolving against two organisations needs two names, because
// "00g1abcd" from one and "00g1abcd" from the other would otherwise be the same
// group, and they are not.
func WithName(name string) Option {
	return func(d *Directory) { d.name = name }
}

// WithHTTPClient replaces the client entirely, rate limiting included.
//
// A caller who passes one is taking on the limits themselves, which is right
// for a test replaying a recording and wrong for anything talking to Okta.
func WithHTTPClient(c *http.Client) Option {
	return func(d *Directory) {
		if c != nil {
			d.http = c
		}
	}
}

// WithLimits changes what the organisation is allowed to cost.
func WithLimits(l limit.Limits) Option {
	return func(d *Directory) { d.http = throttled(l) }
}

// WithPageSize sets how many groups are asked for per page. A size below one
// selects [DefaultPageSize].
func WithPageSize(n int) Option {
	return func(d *Directory) { d.page = n }
}

// WithBuffer sets how long and how much of the group listing is kept for the
// group lookups that follow it. A size below one selects [DefaultBufferSize].
//
// A lifetime below one turns the buffer off, which is a request per group in
// the set rather than one listing. That is correct and slow, so it is for a
// test that wants to count the requests rather than for a deployment.
func WithBuffer(ttl time.Duration, size int) Option {
	return func(d *Directory) {
		if ttl < 1 {
			d.held = nil
			return
		}
		if size < 1 {
			size = DefaultBufferSize
		}
		d.held = cache.New[directory.Group](size, ttl)
	}
}

// New returns a directory over an Okta organisation.
//
// The org is the URL of the tenant, "https://acme.okta.com", and the token is
// an API token, which Okta sends as SSWS rather than as a bearer.
func New(org, token string, opts ...Option) (*Directory, error) {
	if org == "" {
		return nil, errors.New("okta: an organisation URL is required")
	}
	if token == "" {
		return nil, errors.New("okta: an API token is required")
	}
	u, err := url.Parse(org)
	if err != nil {
		return nil, fmt.Errorf("okta: the organisation URL %q: %w", org, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("okta: the organisation URL %q needs a scheme and a host", org)
	}

	d := &Directory{
		http:  throttled(limit.Limits{}),
		base:  strings.TrimSuffix(org, "/"),
		token: token,
		name:  DefaultName,
		page:  DefaultPageSize,
		held:  cache.New[directory.Group](DefaultBufferSize, DefaultBufferTTL),
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.name == "" {
		return nil, errors.New("okta: the source name cannot be empty")
	}
	if d.page < 1 {
		d.page = DefaultPageSize
	}
	return d, nil
}

// throttled is the client used when the caller has not supplied one.
func throttled(l limit.Limits) *http.Client {
	return &http.Client{Transport: limit.NewTransport(l)}
}

// Name is the identity source these ids belong to.
func (d *Directory) Name() string { return d.name }

// Subject returns one person, with the groups they are directly in.
//
// It is two requests plus a page of groups: Okta answers a user and their group
// memberships from separate endpoints and there is no expand parameter that
// merges them.
func (d *Directory) Subject(ctx context.Context, id string) (directory.Subject, error) {
	if id == "" {
		return directory.Subject{}, fmt.Errorf("okta: %w", directory.ErrNoSubject)
	}

	var u user
	if err := d.get(ctx, "/api/v1/users/"+url.PathEscape(id), nil, &u); err != nil {
		if errors.Is(err, errNotFound) {
			return directory.Subject{}, fmt.Errorf("okta: %q: %w", id, directory.ErrNoSubject)
		}
		return directory.Subject{}, err
	}

	sub := directory.Subject{
		ID:         cmp.Or(u.ID, id),
		Name:       u.Profile.displayName(),
		Email:      u.Profile.Email,
		Version:    u.LastUpdated,
		Identities: d.identities(u),
		Disabled:   deactivated(u.Status),
	}
	// A deactivated account is refused by the resolver, so the group listing it
	// would have needed is a request nobody is going to read the answer to.
	if sub.Disabled {
		return sub, nil
	}

	groups, err := d.memberships(ctx, u.ID)
	if err != nil {
		return directory.Subject{}, err
	}
	sub.MemberOf = make([]string, 0, len(groups))
	for _, g := range groups {
		sub.MemberOf = append(sub.MemberOf, g.ID)
		d.buffer(g)
	}
	return sub, nil
}

// buffer keeps a group object for the lookup that is about to ask for it.
func (d *Directory) buffer(g directory.Group) {
	if d.held != nil {
		d.held.Put(g.ID, g)
	}
}

// buffered is the group object a listing already went past, if there is one.
func (d *Directory) buffered(id string) (directory.Group, bool) {
	if d.held == nil {
		return directory.Group{}, false
	}
	return d.held.Get(id)
}

// Group returns one group.
//
// The membership is always empty, because Okta groups do not contain groups,
// and the lookup is usually free, because the listing that answered the subject
// put the object here on its way past. See the package documentation for why
// that cannot change an answer.
func (d *Directory) Group(ctx context.Context, id string) (directory.Group, error) {
	if id == "" {
		return directory.Group{}, fmt.Errorf("okta: %w", directory.ErrNoGroup)
	}
	if g, ok := d.buffered(id); ok {
		return g, nil
	}

	var raw group
	if err := d.get(ctx, "/api/v1/groups/"+url.PathEscape(id), nil, &raw); err != nil {
		if errors.Is(err, errNotFound) {
			return directory.Group{}, fmt.Errorf("okta: %q: %w", id, directory.ErrNoGroup)
		}
		return directory.Group{}, err
	}
	g := raw.directory(id)
	d.buffer(g)
	return g, nil
}

// memberships reads every group a user is in, following the pages.
func (d *Directory) memberships(ctx context.Context, id string) ([]directory.Group, error) {
	var (
		out  []directory.Group
		path = "/api/v1/users/" + url.PathEscape(id) + "/groups"
		q    = url.Values{"limit": {strconv.Itoa(d.page)}}
	)
	// Bounded because the page after the last one is a link the service sends,
	// and a service that sent the same link twice would otherwise be an
	// expansion that never returned. The resolver's own bound is on groups
	// reached rather than on requests made, so it would not catch this.
	for range maxPages {
		var page []group
		next, err := d.page1(ctx, path, q, &page)
		if err != nil {
			return nil, err
		}
		for _, raw := range page {
			if raw.ID == "" {
				continue
			}
			out = append(out, raw.directory(raw.ID))
		}
		if next == "" {
			return out, nil
		}
		u, err := url.Parse(next)
		if err != nil {
			return nil, fmt.Errorf("okta: the next page link %q: %w", next, err)
		}
		path, q = u.Path, u.Query()
	}
	return nil, fmt.Errorf("okta: the groups of %q did not stop after %d pages", id, maxPages)
}

// maxPages bounds a listing. At the default page size it is two hundred
// thousand groups, which is far past anybody's organisation and well short of
// forever.
const maxPages = 1000

// user is what Okta says about one person.
type user struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	LastUpdated string  `json:"lastUpdated"`
	Profile     profile `json:"profile"`
}

type profile struct {
	Login       string `json:"login"`
	Email       string `json:"email"`
	SecondEmail string `json:"secondEmail"`
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	DisplayName string `json:"displayName"`
}

// displayName is what to call somebody, in the order Okta itself falls back.
func (p profile) displayName() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	if name := strings.TrimSpace(p.FirstName + " " + p.LastName); name != "" {
		return name
	}
	return p.Login
}

// group is what Okta says about one group.
type group struct {
	ID          string       `json:"id"`
	LastUpdated string       `json:"lastUpdated"`
	Profile     groupProfile `json:"profile"`
}

type groupProfile struct {
	Name string `json:"name"`
}

// directory turns one into what the resolver walks.
//
// The membership is empty and that is not an omission: Okta groups do not
// contain groups.
//
// The version is the group object's own revision, and the interesting part is
// which revision it is not. Okta also sends lastMembershipUpdated, which is the
// obvious choice and the wrong one. It moves when anybody at all joins the
// group, and what this version invalidates is one person's group set, which
// somebody else joining does not change. At a company of any size something
// moves every second, so a version derived from it would be correct and
// useless: nothing cached above it would ever survive to be reused.
//
// This person joining or leaving is caught without it, because the group set is
// fingerprinted over the group ids as well as their versions, and joining or
// leaving is what changes the ids. For a provider that nests, the group version
// is also what catches a group being moved under another one. Okta does not
// nest, so there is nothing else for it to catch.
func (g group) directory(id string) directory.Group {
	return directory.Group{
		ID:      cmp.Or(g.ID, id),
		Name:    g.Profile.Name,
		Version: g.LastUpdated,
	}
}

// identities is the same person in the systems being indexed.
//
// The Okta id is one of them, because a connector that was told about groups by
// Okta names people the same way. The addresses are the others, because a rule
// granting to somebody by email is the common shape and an email address is
// what a person has in every product they use.
func (d *Directory) identities(u user) []acl.Identity {
	out := make([]acl.Identity, 0, 4)
	add := func(source, value string) {
		if value == "" {
			return
		}
		id := acl.Identity{Source: source, Value: value}
		if slices.Contains(out, id) {
			return
		}
		out = append(out, id)
	}
	add(d.name, u.ID)
	add("email", u.Profile.Email)
	// A login is an email address at almost every organisation and is something
	// else at the few that use a directory of their own, so it is only an email
	// identity when it looks like one.
	if strings.Contains(u.Profile.Login, "@") {
		add("email", u.Profile.Login)
	}
	add("email", u.Profile.SecondEmail)
	if len(out) == 0 {
		return nil
	}
	return out
}

// deactivated says whether an account state is somebody who should stop
// resolving. See the package documentation for why these three and not the
// others.
func deactivated(status string) bool {
	switch strings.ToUpper(status) {
	case "SUSPENDED", "DEPROVISIONED", "STAGED", "DELETED":
		return true
	default:
		return false
	}
}

// errNotFound is Okta answering rather than failing to answer, and it is what
// separates a subject the organisation does not hold from an organisation that
// could not be reached.
var errNotFound = errors.New("not found")

// get reads one JSON document.
func (d *Directory) get(ctx context.Context, path string, q url.Values, into any) error {
	_, err := d.page1(ctx, path, q, into)
	return err
}

// page1 reads one response into a value and returns the link to the page after
// it, if the service sent one.
func (d *Directory) page1(ctx context.Context, path string, q url.Values, into any) (string, error) {
	u := d.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("okta: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// SSWS rather than Bearer. An API token is not an OAuth token and Okta
	// refuses it under the other scheme.
	req.Header.Set("Authorization", "SSWS "+d.token)

	resp, err := d.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("okta: %s: %w", path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return "", d.refusal(path, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return "", fmt.Errorf("okta: %s: reading the answer: %w", path, err)
	}
	return nextLink(resp.Header.Values("Link")), nil
}

// refusal turns a response Okta refused with into an error worth reading.
//
// The body carries a code and a summary, and both go in the message: the code
// is what the operator searches for, and the summary is what tells them whether
// it is about a token or about a user.
func (d *Directory) refusal(path string, resp *http.Response) error {
	var body struct {
		ErrorCode    string `json:"errorCode"`
		ErrorSummary string `json:"errorSummary"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = json.Unmarshal(raw, &body)

	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	switch {
	case body.ErrorCode != "" && body.ErrorSummary != "":
		return fmt.Errorf("okta: %s: %s: %s (%s)", path, resp.Status, body.ErrorSummary, body.ErrorCode)
	case body.ErrorSummary != "":
		return fmt.Errorf("okta: %s: %s: %s", path, resp.Status, body.ErrorSummary)
	default:
		return fmt.Errorf("okta: %s: %s", path, resp.Status)
	}
}

// nextLink reads the page after this one out of the Link headers.
//
// Okta sends several of them, one per relation, and the one that matters is
// rel="next". A listing that read the first link it saw would follow rel="self"
// and ask the same question until something stopped it.
func nextLink(headers []string) string {
	for _, h := range headers {
		for _, part := range links(h) {
			link, params, ok := strings.Cut(part, ";")
			if !ok {
				continue
			}
			if !strings.Contains(strings.ToLower(params), `rel="next"`) {
				continue
			}
			return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(link), "<"), ">")
		}
	}
	return ""
}

// links splits one Link header into its entries.
//
// Not by comma, because the URL is inside angle brackets and a filter or a
// cursor in it may contain one. Splitting on every comma would hand back half a
// URL, and half a URL that happens to parse is a request to the wrong place.
func links(h string) []string {
	var (
		out   []string
		depth int
		start int
	)
	for i, r := range h {
		switch r {
		case '<':
			depth++
		case '>':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(h[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(h[start:]))
}
