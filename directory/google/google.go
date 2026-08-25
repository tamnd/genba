// Package google resolves group membership against a Google Workspace domain.
//
// It answers the two lookups [directory.Directory] asks for and nothing else.
// The closure, the cycle detection, the bound on how much one expansion may
// cost and the version an answer is stamped with are all in [directory], and
// they are shared by every provider, because those are exactly the parts that
// are easy to get subtly wrong and impossible to notice from the outside.
//
// # One endpoint answers both lookups
//
// Google Workspace groups nest and the Admin SDK will not walk the nesting for
// you. The groups collection takes a userKey and returns the groups that key is
// directly a member of, which is one level and no more.
//
// The useful part is what a userKey may be. It is a person, by id or by any of
// their addresses, and it is also a group, by id or by the group's address. So
// the same request that answers "which groups is this person in" answers "which
// groups is this group in", and [Directory.Group] returns a real membership
// rather than an empty one. The resolver walks from there, which is the shape it
// was built for.
//
// That is worth preferring to the alternative even where the alternative exists.
// The members collection will report inherited membership if it is asked to, but
// it answers the question the other way round, from the group towards the
// people in it, and a company with one group that everybody is in would have to
// read every employee to find out that one of them is a member. Walking up costs
// a request per group above somebody and never depends on how many colleagues
// they have.
//
// # What the buffer holds, and what it does not
//
// The walk asks about every group, and a person in three hundred groups would be
// three hundred requests against a domain everybody else is signing in to at the
// same time. The listing that answers the subject lookup returns whole group
// objects rather than ids, so those objects fill a small buffer that the group
// lookups then read, and the three hundred lookups of the object become none.
//
// What the buffer cannot hold is what those groups are members of, because the
// listing does not say. So a group lookup here is one request rather than two:
// the object comes out of the buffer and the level above it is still asked for.
// That is the honest limit of what one collection can be made to do, and it is
// still half of what the walk would otherwise cost.
//
// It is worth being precise about why this is not a second permission cache,
// because [directory.Cache] documents at some length why there is only one of
// those. The fact being held is "this group exists and is called this", which is
// not an input to any decision: the membership, which is the part that decides
// anything, is asked for on every lookup and never held.
//
// # Suspended is not the only way to leave
//
// An account somebody has suspended is the obvious one and it is not the only
// one. An archived account is a person who has left, whose mailbox was kept and
// whose licence was released, and it is a separate field with a separate value.
// An adapter that reads suspended alone keeps resolving the groups of everybody
// who left the company and was archived rather than suspended, which is a common
// way to run a Workspace domain and not a corner case.
//
// Neither is a deletion. A deleted user is not returned by this endpoint at all,
// so they arrive as a subject the domain does not hold, which is what they are.
//
// # A forbidden that means slow down
//
// The Admin SDK refuses a caller who is over the rate with a 403 rather than
// with a 429, and the only thing separating that from an account which may not
// read the directory at all is a reason string in the body. Retrying every
// forbidden would hammer a service over permissions that are not going to
// change, and retrying none of them would turn a throttle that clears in two
// seconds into a sync that failed. So [quota] reads the reason, and [limit] is
// told about it with [limit.WithThrottled].
//
// The daily limit is deliberately not in that set. It is the same shape of
// answer and it clears at midnight, so waiting for it with a backoff measured in
// seconds spends the rest of the quota finding out that there is none left.
package google

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
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
// group key from this domain is prefixed with.
const DefaultName = "google"

// DefaultEndpoint is the Admin SDK.
const DefaultEndpoint = "https://admin.googleapis.com"

// apiPath is the version of the Directory API this speaks, and it is part of
// the path rather than a header.
const apiPath = "/admin/directory/v1"

// DefaultPageSize is how many groups are asked for per page, which is the most
// this collection accepts.
const DefaultPageSize = 200

// MaxPageSize is the ceiling the endpoint enforces. A larger page is refused
// with a bad request rather than quietly trimmed, and a bad request on a
// membership listing is an expansion that fails, so a size above this is capped
// here instead of being passed on to find out.
const MaxPageSize = 200

// DefaultBufferTTL is how long the group listing that answered a subject lookup
// is kept for the group lookups that follow it.
//
// It is short because it only has to outlive one expansion. See the package
// documentation for what is in it and why holding it for longer would not change
// an answer either.
const DefaultBufferTTL = 30 * time.Second

// DefaultBufferSize is how many groups the buffer holds before it starts
// dropping the ones nobody has asked about recently.
const DefaultBufferSize = 20000

// maxPages bounds a listing. At the default page size it is two hundred
// thousand groups, which is far past anybody's domain and well short of forever.
const maxPages = 1000

// Directory resolves subjects and groups against one Google Workspace domain.
//
// It is safe for concurrent use, which it has to be: an expansion looks up a
// level of the graph in parallel.
type Directory struct {
	http   *http.Client
	base   string
	tokens Tokens
	name   string
	page   int

	// held is the group listing from a subject lookup, waiting for the group
	// lookups that are about to ask for the same objects.
	held *cache.Cache[group]
}

var _ directory.Directory = (*Directory)(nil)

// Option configures a [Directory].
type Option func(*Directory)

// WithName sets the identity source the group ids belong to.
//
// A deployment resolving against two domains needs two names, because a group id
// from one and the same id from the other would otherwise be the same group, and
// they are not.
func WithName(name string) Option {
	return func(d *Directory) { d.name = name }
}

// WithEndpoint replaces the Admin SDK, which a test needs and nothing else does.
func WithEndpoint(base string) Option {
	return func(d *Directory) { d.base = strings.TrimSuffix(base, "/") }
}

// WithHTTPClient replaces the client entirely, rate limiting included.
//
// A caller who passes one is taking on the limits themselves, which is right for
// a test replaying a recording and wrong for anything talking to Google.
func WithHTTPClient(c *http.Client) Option {
	return func(d *Directory) {
		if c != nil {
			d.http = c
		}
	}
}

// WithLimits changes what the domain is allowed to cost.
func WithLimits(l limit.Limits) Option {
	return func(d *Directory) { d.http = throttled(l) }
}

// WithPageSize sets how many groups are asked for per page. A size below one
// selects [DefaultPageSize] and a size above [MaxPageSize] is that.
func WithPageSize(n int) Option {
	return func(d *Directory) { d.page = n }
}

// WithBuffer sets how long and how much of the group listing is kept for the
// group lookups that follow it. A size below one selects [DefaultBufferSize].
//
// A lifetime below one turns the buffer off, which is an extra request per group
// in the set. That is correct and slow, so it is for a test that wants to count
// the requests rather than for a deployment.
func WithBuffer(ttl time.Duration, size int) Option {
	return func(d *Directory) {
		if ttl < 1 {
			d.held = nil
			return
		}
		if size < 1 {
			size = DefaultBufferSize
		}
		d.held = cache.New[group](size, ttl)
	}
}

// New returns a directory over a Google Workspace domain.
//
// The tokens are where the bearer comes from. [NewServiceAccount] is the one to
// reach for in a deployment and [Token] is the one for a test.
func New(tokens Tokens, opts ...Option) (*Directory, error) {
	if tokens == nil {
		return nil, errors.New("google: a source of tokens is required")
	}

	d := &Directory{
		http:   throttled(limit.Limits{}),
		base:   DefaultEndpoint,
		tokens: tokens,
		name:   DefaultName,
		page:   DefaultPageSize,
		held:   cache.New[group](DefaultBufferSize, DefaultBufferTTL),
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.name == "" {
		return nil, errors.New("google: the source name cannot be empty")
	}
	if d.page < 1 {
		d.page = DefaultPageSize
	}
	if d.page > MaxPageSize {
		d.page = MaxPageSize
	}
	u, err := url.Parse(d.base)
	if err != nil {
		return nil, fmt.Errorf("google: the endpoint %q: %w", d.base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("google: the endpoint %q needs a scheme and a host", d.base)
	}
	return d, nil
}

// throttled is the client used when the caller has not supplied one.
func throttled(l limit.Limits) *http.Client {
	return &http.Client{Transport: limit.NewTransport(l, limit.WithThrottled(quota))}
}

// Name is the identity source these ids belong to.
func (d *Directory) Name() string { return d.name }

// Subject returns one person, with the groups they are directly in.
func (d *Directory) Subject(ctx context.Context, id string) (directory.Subject, error) {
	if id == "" {
		return directory.Subject{}, fmt.Errorf("google: %w", directory.ErrNoSubject)
	}

	var u user
	if err := d.get(ctx, apiPath+"/users/"+url.PathEscape(id), nil, &u); err != nil {
		if errors.Is(err, errNotFound) {
			return directory.Subject{}, fmt.Errorf("google: %q: %w", id, directory.ErrNoSubject)
		}
		return directory.Subject{}, err
	}

	sub := directory.Subject{
		ID:         cmp.Or(u.ID, id),
		Name:       u.display(),
		Email:      u.PrimaryEmail,
		Version:    u.version(),
		Identities: d.identities(u),
		Disabled:   u.Suspended || u.Archived,
	}
	// A deactivated account is refused by the resolver, so the group listing it
	// would have needed is a request nobody is going to read the answer to.
	if sub.Disabled {
		return sub, nil
	}

	groups, err := d.memberships(ctx, sub.ID)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return directory.Subject{}, fmt.Errorf("google: %q: %w", id, directory.ErrNoSubject)
		}
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
func (d *Directory) buffer(g group) {
	if d.held != nil && g.ID != "" {
		d.held.Put(g.ID, g)
	}
}

// buffered is the group object a listing already went past, if there is one.
func (d *Directory) buffered(id string) (group, bool) {
	if d.held == nil {
		return group{}, false
	}
	return d.held.Get(id)
}

// Group returns one group, with the groups it is directly inside.
//
// The membership is asked for every time and the object usually is not, because
// the listing that answered the subject put the object here on its way past and
// said nothing about what it is a member of. See the package documentation.
func (d *Directory) Group(ctx context.Context, id string) (directory.Group, error) {
	if id == "" {
		return directory.Group{}, fmt.Errorf("google: %w", directory.ErrNoGroup)
	}

	raw, ok := d.buffered(id)
	if !ok {
		if err := d.get(ctx, apiPath+"/groups/"+url.PathEscape(id), nil, &raw); err != nil {
			if errors.Is(err, errNotFound) {
				return directory.Group{}, fmt.Errorf("google: %q: %w", id, directory.ErrNoGroup)
			}
			return directory.Group{}, err
		}
		d.buffer(raw)
	}

	above, err := d.memberships(ctx, cmp.Or(raw.ID, id))
	if err != nil {
		if errors.Is(err, errNotFound) {
			return directory.Group{}, fmt.Errorf("google: %q: %w", id, directory.ErrNoGroup)
		}
		return directory.Group{}, err
	}
	parents := make([]string, 0, len(above))
	for _, p := range above {
		parents = append(parents, p.ID)
		d.buffer(p)
	}
	return raw.directory(id, parents), nil
}

// memberships reads every group a key is directly in, following the pages.
//
// The key is a person or a group, and the request is the same one either way,
// which is the whole reason this provider is walkable at all.
func (d *Directory) memberships(ctx context.Context, key string) ([]group, error) {
	var (
		out  []group
		q    = url.Values{"userKey": {key}, "maxResults": {strconv.Itoa(d.page)}}
		seen = make(map[string]bool)
	)
	// Bounded because the page after the last one is a token the service sends,
	// and a service that sent the same token twice would otherwise be an
	// expansion that never returned. The resolver's own bound is on groups
	// reached rather than on requests made, so it would not catch this.
	for range maxPages {
		var page listing
		if err := d.get(ctx, apiPath+"/groups", q, &page); err != nil {
			return nil, err
		}
		for _, raw := range page.Groups {
			// A key can reach the same group by two paths, and the group set is
			// a set.
			if raw.ID == "" || seen[raw.ID] {
				continue
			}
			seen[raw.ID] = true
			out = append(out, raw)
		}
		if page.Next == "" {
			return out, nil
		}
		q.Set("pageToken", page.Next)
	}
	return nil, fmt.Errorf("google: the groups of %q did not stop after %d pages", key, maxPages)
}

// listing is a page of groups, and the token for the page after it.
type listing struct {
	Groups []group `json:"groups"`
	Next   string  `json:"nextPageToken"`
}

// user is what the Admin SDK says about one person.
type user struct {
	ID           string   `json:"id"`
	PrimaryEmail string   `json:"primaryEmail"`
	Name         fullName `json:"name"`
	Aliases      []string `json:"aliases"`
	Emails       []email  `json:"emails"`
	ETag         string   `json:"etag"`

	// Suspended and Archived are two different ways to stop working here and
	// only the first is the obvious one. See the package documentation.
	Suspended bool `json:"suspended"`
	Archived  bool `json:"archived"`
}

type fullName struct {
	Given  string `json:"givenName"`
	Family string `json:"familyName"`
	Full   string `json:"fullName"`
}

type email struct {
	Address string `json:"address"`
	Primary bool   `json:"primary"`
}

// display is what to call somebody, in the order the provider itself falls back.
func (u user) display() string {
	if u.Name.Full != "" {
		return u.Name.Full
	}
	if name := strings.TrimSpace(u.Name.Given + " " + u.Name.Family); name != "" {
		return name
	}
	return u.PrimaryEmail
}

// version is a fingerprint of what this adapter reports about the person.
//
// The etag is the provider's own revision and it is the first thing in here, so
// for a domain that sends one this is that revision under a different name. The
// rest is not belt and braces: the Admin SDK has been taking etags off responses
// across its APIs for years, and an adapter that reads one and finds nothing
// would stamp every person in the domain with the same version and never
// invalidate anything built on top of a group set. Hashing the fields as well
// means a domain that stops sending etags loses nothing.
//
// The group ids are not in here because [directory] fingerprints those
// separately, so somebody joining or leaving a group is caught without them.
func (u user) version() string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", u.ETag, u.ID, u.display(), u.PrimaryEmail)
	for _, a := range u.Aliases {
		fmt.Fprintf(h, "%s\x00", a)
	}
	for _, e := range u.Emails {
		fmt.Fprintf(h, "%s\x00", e.Address)
	}
	if u.Suspended {
		fmt.Fprint(h, "suspended\x00")
	}
	if u.Archived {
		fmt.Fprint(h, "archived\x00")
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// group is what the Admin SDK says about one group.
type group struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	ETag  string `json:"etag"`
}

// directory turns one into what the resolver walks.
func (g group) directory(id string, above []string) directory.Group {
	return directory.Group{
		ID:       cmp.Or(g.ID, id),
		Name:     cmp.Or(g.Name, g.Email),
		Version:  g.version(above),
		MemberOf: above,
	}
}

// version is a fingerprint of the group and of where it sits.
//
// The groups above it are in here and that is the part worth explaining. A
// version on a group exists to catch a change that alters somebody's group set
// without altering the ids already in it, and for a provider that nests, that
// change is a group being put inside another one. The etag does not move when
// that happens, because being a member of something is a fact the provider keeps
// on the other end of the edge. So a version that was the etag alone would leave
// every member of that group with a cached set that is missing whatever the
// group was just put underneath, until an unrelated change moved something else.
func (g group) version(above []string) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", g.ETag, g.Name, g.Email)
	for _, id := range above {
		fmt.Fprintf(h, "%s\x00", id)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// identities is the same person in the systems being indexed.
//
// The Google id is one of them, because a connector that was told about groups
// by Workspace names people the same way. The addresses are the others, because
// a rule granting to somebody by email is the common shape and an email address
// is what a person has in every product they use.
//
// The non editable aliases are not in here. Those are generated for the domain
// rather than chosen for the person: a company with three domain aliases gets
// three more addresses for every employee, none of which anybody has ever
// written down, and putting them all in the principal makes every expansion
// carry a list nothing will ever match.
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
	add("email", u.PrimaryEmail)
	for _, a := range u.Aliases {
		add("email", a)
	}
	for _, e := range u.Emails {
		add("email", e.Address)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// errNotFound is the service answering rather than failing to answer, and it is
// what separates a subject the domain does not hold from a domain that could not
// be reached.
var errNotFound = errors.New("not found")

// get reads one JSON document.
func (d *Directory) get(ctx context.Context, path string, q url.Values, into any) error {
	u := d.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	token, err := d.tokens.Token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return fmt.Errorf("google: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("google: %s: %w", path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return d.refusal(path, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("google: %s: reading the answer: %w", path, err)
	}
	return nil
}

// failure is the error document every Google API answers a refusal with.
type failure struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Domain  string `json:"domain"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
}

// reason is what the service says this refusal was about, which is the field
// worth branching on and the one worth putting in a message.
func (f failure) reason() string {
	if len(f.Error.Errors) == 0 {
		return ""
	}
	return f.Error.Errors[0].Reason
}

// refusal turns a response the service refused with into an error worth reading.
//
// A bad request is one of these and not a failure, which is worth saying out
// loud. The endpoint refuses a userKey that could not belong to anybody with an
// invalid rather than with a not found, so a store holding a username where an
// address belongs would otherwise fail the expansion instead of reporting that
// nobody by that name is in the domain. The two mean the same thing to a caller:
// the domain does not hold this person.
//
// That mapping is only safe because nothing else this adapter sends can be
// invalid. The one parameter with a range on it is the page size, and it is
// capped at [MaxPageSize] on the way in rather than sent to find out.
func (d *Directory) refusal(path string, resp *http.Response) error {
	var body failure
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(raw, &body)
	reason := body.reason()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode == http.StatusBadRequest && (reason == "invalid" || reason == "invalidInput"):
		return errNotFound
	}

	message := firstLine(body.Error.Message)
	switch {
	case reason != "" && message != "":
		return fmt.Errorf("google: %s: %s: %s (%s)", path, resp.Status, message, reason)
	case message != "":
		return fmt.Errorf("google: %s: %s: %s", path, resp.Status, message)
	default:
		return fmt.Errorf("google: %s: %s", path, resp.Status)
	}
}

// quota reports whether a refusal is the service saying the requests are
// arriving too fast, rather than saying this account may not read the directory.
//
// Both are a 403 and the reason is the only thing that tells them apart. See the
// package documentation for why the daily limit is not in here.
func quota(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	var body failure
	if err := json.Unmarshal(limit.Peek(resp, 1<<16), &body); err != nil {
		return false
	}
	for _, e := range body.Error.Errors {
		switch e.Reason {
		case "quotaExceeded", "rateLimitExceeded", "userRateLimitExceeded":
			return true
		}
	}
	return false
}

// firstLine trims a message down to the sentence worth reading. The rest of it
// is usually a link to a console page, which belongs in a support ticket rather
// than in a log line.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
