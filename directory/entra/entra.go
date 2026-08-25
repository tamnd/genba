// Package entra resolves group membership against a Microsoft Entra ID tenant.
//
// It answers the two lookups [directory.Directory] asks for and nothing else.
// The closure, the cycle detection, the bound on how much one expansion may
// cost and the version an answer is stamped with are all in [directory], and
// they are shared by every provider, because those are exactly the parts that
// are easy to get subtly wrong and impossible to notice from the outside.
//
// # The graph arrives already walked
//
// Entra groups do nest, and unlike most providers it will do the nesting for
// you. The transitiveMemberOf collection is every group a person is in however
// deeply that membership is inherited, so a person eight levels down a nest is
// one request rather than eight rounds of them.
//
// That is why [Directory.Group] answers with an empty membership. It is not
// that a group here is a member of nothing, it is that whatever it is a member
// of already came back with the subject. Returning the parents as well would
// have the resolver walk a graph it has already been handed, which costs the
// round trips this endpoint exists to avoid and changes no answer.
//
// The cost is the same shape as any other provider once the closure is in
// hand: the walk asks about each group, and a person in three hundred groups
// would be three hundred requests. So the listing, which returns whole group
// objects rather than ids, fills a small buffer that the group lookups read.
// The fact being held there is "this group exists and is a member of nothing
// this adapter will report", which cannot become false in a way that changes an
// answer, so the buffer changes what an expansion costs and not what it says.
//
// # The cast in the middle of the path
//
// The path is transitiveMemberOf/microsoft.graph.group and the cast on the end
// of it is load bearing. Without it the collection also returns directory roles
// and administrative units, which are directory objects a person belongs to and
// are not groups. Their ids would go into the group set beside the real ones,
// and a rule naming one would then allow everybody who holds that role. The
// cast asks the service to return only groups rather than filtering after the
// fact, so the ids that arrive are the ids that belong there.
//
// # Two properties that have to be asked for
//
// Graph answers a user lookup with a default set of properties, and
// accountEnabled is not in it. An adapter that reads the user and looks for
// that field without selecting it gets the zero value, which is false, or gets
// nothing at all and calls it true, depending on how it is written. Both are
// wrong in a way nobody notices: the first refuses everybody and the second
// keeps resolving the groups of people who were deactivated months ago. Every
// property this adapter reads is named in a $select for that reason.
//
// # There is no revision to read
//
// The v1.0 Graph exposes no last modified time on a user or on a group, so
// there is nothing to hand [directory] as a version and copy from the provider.
//
// The group version is empty, and for this provider that is complete rather
// than a gap. A version on a group is there to catch a change that alters
// somebody's group set without altering the ids in it, which for a provider
// that nests means a group being moved under another one. Here that move
// arrives as a different closure, because the closure is what the provider
// returns. Nothing else a group carries can change a decision, and hashing the
// display name in would invalidate every member's cached group set for a rename
// that alters no answer.
//
// The subject version is a hash of exactly the fields this adapter reports
// about the person, because those are not covered by the closure. A new alias
// or an account being deactivated has to reach the principal, and the group ids
// do not move when either happens.
//
// # Rate limits
//
// Graph says nothing about what is left until it refuses, and then answers a
// 429 or a 503 with a Retry-After. That is the opposite of a provider that
// publishes its numbers on every response, and it means [limit] can only hold
// back after a refusal rather than before one. The breaker matters more here
// for the same reason: a tenant that has started refusing is one that wants to
// be left alone for a while, and the only way to learn that is to have been
// told once and remember it.
package entra

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
// group key from this tenant is prefixed with.
const DefaultName = "entra"

// DefaultEndpoint is the Graph. A national cloud is a different one.
const DefaultEndpoint = "https://graph.microsoft.com"

// DefaultPageSize is how many groups are asked for per page, which is the most
// this collection accepts.
const DefaultPageSize = 999

// DefaultBufferTTL is how long the group listing that answered a subject lookup
// is kept for the group lookups that follow it.
//
// It is short because it only has to outlive one expansion. See the package
// documentation for why holding it for longer would not change an answer
// either.
const DefaultBufferTTL = 30 * time.Second

// DefaultBufferSize is how many groups the buffer holds before it starts
// dropping the ones nobody has asked about recently.
const DefaultBufferSize = 20000

// maxPages bounds a listing. At the default page size it is nearly a million
// groups, which is past anybody's tenant and well short of forever.
const maxPages = 1000

// version is the Graph version this speaks. The beta endpoint carries fields
// that would be useful here and no promise that they will still be there next
// month, which is the wrong trade for the thing that decides what people can
// read.
const version = "/v1.0"

// Directory resolves subjects and groups against one Entra ID tenant.
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
	held *cache.Cache[directory.Group]
}

var _ directory.Directory = (*Directory)(nil)

// Option configures a [Directory].
type Option func(*Directory)

// WithName sets the identity source the group ids belong to.
//
// A deployment resolving against two tenants needs two names, because a group
// id from one and the same id from the other would otherwise be the same group,
// and they are not.
func WithName(name string) Option {
	return func(d *Directory) { d.name = name }
}

// WithEndpoint replaces the Graph, which a national cloud and a test both need.
func WithEndpoint(base string) Option {
	return func(d *Directory) { d.base = strings.TrimSuffix(base, "/") }
}

// WithHTTPClient replaces the client entirely, rate limiting included.
//
// A caller who passes one is taking on the limits themselves, which is right
// for a test replaying a recording and wrong for anything talking to Graph.
func WithHTTPClient(c *http.Client) Option {
	return func(d *Directory) {
		if c != nil {
			d.http = c
		}
	}
}

// WithLimits changes what the tenant is allowed to cost.
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

// New returns a directory over an Entra ID tenant.
//
// The tokens are where the bearer comes from. [NewApplication] is the one to
// reach for in a deployment and [Token] is the one for a test.
func New(tokens Tokens, opts ...Option) (*Directory, error) {
	if tokens == nil {
		return nil, errors.New("entra: a source of tokens is required")
	}

	d := &Directory{
		http:   throttled(limit.Limits{}),
		base:   DefaultEndpoint,
		tokens: tokens,
		name:   DefaultName,
		page:   DefaultPageSize,
		held:   cache.New[directory.Group](DefaultBufferSize, DefaultBufferTTL),
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.name == "" {
		return nil, errors.New("entra: the source name cannot be empty")
	}
	if d.page < 1 {
		d.page = DefaultPageSize
	}
	u, err := url.Parse(d.base)
	if err != nil {
		return nil, fmt.Errorf("entra: the endpoint %q: %w", d.base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("entra: the endpoint %q needs a scheme and a host", d.base)
	}
	return d, nil
}

// throttled is the client used when the caller has not supplied one.
func throttled(l limit.Limits) *http.Client {
	return &http.Client{Transport: limit.NewTransport(l)}
}

// Name is the identity source these ids belong to.
func (d *Directory) Name() string { return d.name }

// Subject returns one person, with every group they are in.
//
// Every group rather than the ones they are directly in, because that is what
// the provider answers with and there is nothing to gain by throwing the rest
// away and asking again. See the package documentation.
func (d *Directory) Subject(ctx context.Context, id string) (directory.Subject, error) {
	if id == "" {
		return directory.Subject{}, fmt.Errorf("entra: %w", directory.ErrNoSubject)
	}

	q := url.Values{"$select": {"id,displayName,mail,userPrincipalName,otherMails,accountEnabled"}}
	var u user
	if err := d.get(ctx, version+"/users/"+url.PathEscape(id), q, &u); err != nil {
		if errors.Is(err, errNotFound) {
			return directory.Subject{}, fmt.Errorf("entra: %q: %w", id, directory.ErrNoSubject)
		}
		return directory.Subject{}, err
	}

	sub := directory.Subject{
		ID:         cmp.Or(u.ID, id),
		Name:       u.DisplayName,
		Email:      u.address(),
		Identities: d.identities(u),
		Disabled:   u.Enabled != nil && !*u.Enabled,
	}
	sub.Version = u.version()
	// A deactivated account is refused by the resolver, so the group listing it
	// would have needed is a request nobody is going to read the answer to.
	if sub.Disabled {
		return sub, nil
	}

	groups, err := d.memberships(ctx, sub.ID)
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
// The membership is always empty, because whatever this group is a member of
// arrived with the subject, and the lookup is usually free, because the listing
// that answered the subject put the object here on its way past. See the
// package documentation for both.
func (d *Directory) Group(ctx context.Context, id string) (directory.Group, error) {
	if id == "" {
		return directory.Group{}, fmt.Errorf("entra: %w", directory.ErrNoGroup)
	}
	if g, ok := d.buffered(id); ok {
		return g, nil
	}

	q := url.Values{"$select": {"id,displayName"}}
	var raw group
	if err := d.get(ctx, version+"/groups/"+url.PathEscape(id), q, &raw); err != nil {
		if errors.Is(err, errNotFound) {
			return directory.Group{}, fmt.Errorf("entra: %q: %w", id, directory.ErrNoGroup)
		}
		return directory.Group{}, err
	}
	g := raw.directory(id)
	d.buffer(g)
	return g, nil
}

// memberships reads the whole closure, following the pages.
func (d *Directory) memberships(ctx context.Context, id string) ([]directory.Group, error) {
	var (
		out  []directory.Group
		ref  = version + "/users/" + url.PathEscape(id) + "/transitiveMemberOf/microsoft.graph.group"
		q    = url.Values{"$select": {"id,displayName"}, "$top": {strconv.Itoa(d.page)}}
		seen = make(map[string]bool)
	)
	// Bounded because the page after the last one is a link the service sends,
	// and a service that sent the same link twice would otherwise be an
	// expansion that never returned. The resolver's own bound is on groups
	// reached rather than on requests made, so it would not catch this.
	for range maxPages {
		var page listing[group]
		if err := d.get(ctx, ref, q, &page); err != nil {
			return nil, err
		}
		for _, raw := range page.Value {
			// A closure can name the same group twice when two paths reach it,
			// and the group set is a set.
			if raw.ID == "" || seen[raw.ID] {
				continue
			}
			seen[raw.ID] = true
			out = append(out, raw.directory(raw.ID))
		}
		if page.Next == "" {
			return out, nil
		}
		// The link is absolute and already carries the skip token, the select
		// and the page size, so it is followed as it was sent rather than taken
		// apart and rebuilt.
		ref, q = page.Next, nil
	}
	return nil, fmt.Errorf("entra: the groups of %q did not stop after %d pages", id, maxPages)
}

// listing is a collection of one kind of thing, and the link to the rest of it.
type listing[T any] struct {
	Value []T    `json:"value"`
	Next  string `json:"@odata.nextLink"`
}

// user is what Graph says about one person.
type user struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"displayName"`
	Mail        string   `json:"mail"`
	UPN         string   `json:"userPrincipalName"`
	OtherMails  []string `json:"otherMails"`

	// Enabled is a pointer so that the field being absent and the field being
	// false are two different things. They mean opposite things and the reason
	// for the first is nearly always a $select somebody forgot.
	Enabled *bool `json:"accountEnabled"`
}

// address is the person's mailbox, which is what a rule written by email names.
//
// The user principal name is the fallback and not the first choice. It is an
// address at most tenants and a login that merely looks like one at the rest.
func (u user) address() string {
	if u.Mail != "" {
		return u.Mail
	}
	if guest(u.UPN) {
		return ""
	}
	return u.UPN
}

// version is a fingerprint of what this adapter reports about the person.
//
// Graph has no revision to copy, so the alternative to computing one is an
// empty version, and an empty version means a display name or a new alias never
// reaches a principal that is already cached. The group set is not in here
// because [directory] fingerprints the group ids separately and this provider
// answers with the whole closure, so a membership moving moves those.
func (u user) version() string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", u.ID, u.DisplayName, u.Mail, u.UPN)
	for _, m := range u.OtherMails {
		fmt.Fprintf(h, "%s\x00", m)
	}
	if u.Enabled != nil && !*u.Enabled {
		fmt.Fprint(h, "disabled\x00")
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// group is what Graph says about one group.
type group struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// directory turns one into what the resolver walks.
//
// The membership is empty and the version is empty, and neither is an omission.
// See the package documentation for both.
func (g group) directory(id string) directory.Group {
	return directory.Group{ID: cmp.Or(g.ID, id), Name: g.DisplayName}
}

// identities is the same person in the systems being indexed.
//
// The tenant id is one of them, because a connector that was told about groups
// by Entra names people the same way. The addresses are the others, because a
// rule granting to somebody by email is the common shape and an email address
// is what a person has in every product they use.
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
	add("email", u.Mail)
	// A guest's principal name is their address with the at sign replaced and
	// the host tenant stuck on the end, which is a string that looks enough like
	// an address to be mistaken for one and is nobody's mailbox. Their real
	// address is in mail, which is already above.
	if strings.Contains(u.UPN, "@") && !guest(u.UPN) {
		add("email", u.UPN)
	}
	for _, m := range u.OtherMails {
		add("email", m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// guest says whether a principal name belongs to somebody from another tenant.
func guest(upn string) bool {
	return strings.Contains(strings.ToUpper(upn), "#EXT#")
}

// errNotFound is Graph answering rather than failing to answer, and it is what
// separates a subject the tenant does not hold from a tenant that could not be
// reached.
var errNotFound = errors.New("not found")

// get reads one JSON document.
//
// The reference is either a path under the endpoint or the absolute link a
// listing sent for the page after it.
func (d *Directory) get(ctx context.Context, ref string, q url.Values, into any) error {
	u := ref
	if strings.HasPrefix(ref, "/") {
		u = d.base + ref
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	token, err := d.tokens.Token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return fmt.Errorf("entra: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("entra: %s: %w", ref, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return d.refusal(ref, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("entra: %s: reading the answer: %w", ref, err)
	}
	return nil
}

// refusal turns a response Graph refused with into an error worth reading.
//
// A 400 is one of these and not a failure, which is worth saying out loud.
// Graph answers a lookup for an id that could not belong to anybody with a bad
// request rather than with a not found, so a store holding a username where a
// principal name belongs would otherwise fail the expansion instead of
// reporting that nobody by that name is in the tenant. The two mean the same
// thing to a caller: the tenant does not hold this person.
func (d *Directory) refusal(ref string, resp *http.Response) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(raw, &body)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode == http.StatusBadRequest && body.Error.Code == "Request_BadRequest":
		return errNotFound
	}
	switch {
	case body.Error.Code != "" && body.Error.Message != "":
		return fmt.Errorf("entra: %s: %s: %s (%s)", ref, resp.Status, firstLine(body.Error.Message), body.Error.Code)
	case body.Error.Message != "":
		return fmt.Errorf("entra: %s: %s: %s", ref, resp.Status, firstLine(body.Error.Message))
	default:
		return fmt.Errorf("entra: %s: %s", ref, resp.Status)
	}
}
