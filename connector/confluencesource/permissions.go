package confluencesource

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/threadsource"
)

// DefaultACLRefresh is how often a space's rule is reapplied to the pages in it
// even though nothing said it had changed.
//
// Confluence reports when a page changed and reports nothing at all about when
// a space's permissions changed. Somebody taken off a space, a group emptied
// out, a space made private: none of those touch a single page, and all of them
// are revocations. A revocation that reaches the index when somebody next edits
// a page is not a revocation.
//
// So the rule is reapplied on a schedule as well. A day is the default: it
// bounds how long somebody who lost access keeps seeing a space's pages in their
// results, and it costs one write per page in the space once a day rather than a
// read of any of them.
const DefaultACLRefresh = 24 * time.Hour

// WithACLRefresh sets how often a space's rule is reapplied. Zero selects
// [DefaultACLRefresh].
func WithACLRefresh(d time.Duration) Option {
	return func(s *Service) { s.aclRefresh = d }
}

// read is the operation that decides whether somebody may see something at all.
// Everything else Confluence grants on a space is about changing things.
const read = "read"

// Containers lists the spaces this account can see, with who may read each.
//
// Archived spaces are left out. An archived space is one somebody put away, its
// pages are not what the wiki offers anybody, and a search that answered with
// them would be answering with the wiki as it was rather than as it is.
func (s *Service) Containers(ctx context.Context) ([]threadsource.Container, error) {
	// Every crawl starts here, which makes this the place the restriction cache
	// is emptied. The cache is there so that a restricted subtree costs one
	// request per ancestor rather than one per page, and that is a saving worth
	// having within a crawl and a bug across them: a restriction put on a parent
	// page is a revocation, and one the index only heard about when the process
	// next restarted would not be one.
	s.restricted.forget()

	var found []space
	err := pages(ctx, s, "/rest/api/space", url.Values{
		"status": {"current"},
		"expand": {"permissions"},
	}, func(results []space) bool {
		found = append(found, results...)
		return true
	})
	if err != nil {
		return nil, err
	}

	at := s.tick()
	containers := make([]threadsource.Container, 0, len(found))
	for _, sp := range found {
		if sp.Key == "" {
			continue
		}
		// The key is the id and the name is the label, because the key is what
		// CQL calls the space and the name is what a person calls it.
		containers = append(containers, threadsource.Container{
			ID:       sp.Key,
			Name:     sp.Name,
			Access:   s.access(sp),
			AccessAt: at,
		})
	}
	return containers, nil
}

// access works out who may read a space.
//
// The answer comes out of the space's own permissions, which grant read to
// accounts and to groups and may grant it to anonymous or unlicensed users. What
// comes back for the ordinary case is what an access control list is: a set of
// accounts and a set of groups.
//
// Nothing here asks the site a second question. The permissions arrive expanded
// on the listing, which is one request for the whole site rather than one per
// space, and a site with four hundred spaces is a site where that difference is
// the crawl.
func (s *Service) access(sp space) acl.Permissions {
	if len(sp.Permissions) == 0 {
		// Not a space nobody may read. A space whose permissions this token was
		// not shown, which on many sites is every space it does not administer,
		// and the two look identical from here.
		s.skip(sp.Key, fmt.Errorf("who may read space %s: the listing carried no permissions, which needs an administrator on most sites", sp.Key))
		return connector.Unresolved(s.name, fmt.Sprintf("the permissions on space %s were not readable by this account", sp.Key))
	}

	var (
		users  []acl.Ref
		groups []acl.Ref
		anyone bool
	)
	for _, p := range sp.Permissions {
		if p.Operation.Operation != read {
			continue
		}
		if t := p.Operation.TargetType; t != "" && t != "space" {
			// A grant to read one kind of thing in the space rather than the
			// space, which is not the question being asked.
			continue
		}
		if p.AnonymousAccess || p.UnlicensedAccess {
			// A space readable without signing in, or readable by everybody a
			// service desk lets in, is a space that is open to the tenant. It is
			// a real configuration, and pretending it is an access control list
			// of nobody would hide the space instead of describing it.
			anyone = true
			continue
		}
		users = append(users, refs(s.name, p.Subjects.User.Results, func(u person) string { return u.AccountID })...)
		groups = append(groups, refs(s.name, p.Subjects.Group.Results, groupName)...)
	}

	switch {
	case anyone:
		return acl.Permissions{Mode: acl.ModePublicToTenant, Source: s.name}
	case len(users) == 0 && len(groups) == 0:
		// Permissions that grant read to nothing is not a space everybody can
		// read, it is a space we failed to understand. Say so.
		s.skip(sp.Key, fmt.Errorf("the permissions on space %s grant read to nobody", sp.Key))
		return connector.Unresolved(s.name, fmt.Sprintf("the permissions on space %s grant read to nobody, which is a space we failed to understand rather than one everybody may read", sp.Key))
	}

	return acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      s.name,
		AllowUsers:  tidy(users),
		AllowGroups: tidy(groups),
	}
}

// tick is the time a space's rule is reported to have changed at.
//
// Confluence reports nothing about that, so what is reported instead is the
// start of the current refresh interval, which is what makes the schedule in
// [DefaultACLRefresh] happen. It is quantised rather than measured from the last
// sync so that the answer does not depend on when the syncs ran: two servers
// reading the same site an hour apart agree about whether the rule has moved,
// and a sync every five minutes still only reapplies it once.
func (s *Service) tick() time.Time {
	every := s.aclRefresh
	if every <= 0 {
		every = DefaultACLRefresh
	}
	return s.now().UTC().Truncate(every)
}

// restrictions resolves the read restriction on a page, walking up the pages
// above it.
//
// It is a cache because of what the walk costs. A restriction on a page near the
// top of a tree restricts everything under it, so the same handful of ancestors
// is asked about once per page in the space, and a space with a restricted
// section in it is a space where that is most of the crawl. A failure is cached
// too, because a token that could not read a restriction this minute will not
// read it next minute either, and asking again per page turns one refusal into a
// thousand.
type restrictions struct {
	svc *Service

	mu sync.Mutex
	by map[string]own
}

// own is one page's own read restriction, without anything above it.
type own struct {
	// has is whether there is a restriction on this page at all, which is the
	// question the inheritance rule is decided on rather than what the rule
	// says. A page with no restriction of its own is not a page readable by
	// nobody.
	has bool

	// rule is who the restriction names, or a quarantine when the site would
	// not say.
	rule acl.Permissions
}

func newRestrictions(s *Service) *restrictions {
	return &restrictions{svc: s, by: make(map[string]own)}
}

// rule is who may read a page, and whether the page has a rule of its own at
// all.
//
// A false second return is a page that inherits its space, which is almost every
// page on almost every site, and it is what lets the crawl above leave the
// container's rule in place.
//
// Restrictions inherit, so the pages above this one are consulted as well: a
// page with no restriction of its own under a parent that has one is restricted.
// A chain carrying a restriction at more than one level is quarantined rather
// than resolved. Confluence means the intersection of the two there, and an
// intersection of a list of accounts with a list of groups is not something that
// can be worked out from outside the identity provider, so publishing the wider
// of the two would publish exactly the pages somebody went out of their way to
// restrict.
func (r *restrictions) rule(ctx context.Context, p content) (acl.Permissions, bool) {
	chain := make([]own, 0, len(p.Ancestors)+1)

	// The page's own restriction arrived with it when the crawl asked for it,
	// which is the ordinary path and costs nothing. A page that came from
	// somewhere that did not ask is looked up.
	if p.Restrictions != nil {
		mine := readRule(r.svc, p.ID, p.Restrictions.Read)
		r.remember(p.ID, mine)
		chain = append(chain, mine)
	} else {
		chain = append(chain, r.of(ctx, p.ID))
	}
	for _, a := range p.Ancestors {
		if a.ID == "" || a.ID == p.ID {
			continue
		}
		chain = append(chain, r.of(ctx, a.ID))
	}

	var found []own
	for _, o := range chain {
		if o.has {
			found = append(found, o)
		}
	}

	switch len(found) {
	case 0:
		return acl.Permissions{}, false
	case 1:
		return found[0].rule, true
	default:
		r.svc.skip(p.ID, fmt.Errorf("page %s is restricted at %d levels of the tree above it and on it, and the intersection of those is not ours to work out", p.ID, len(found)))
		return connector.Unresolved(r.svc.name, fmt.Sprintf("page %s carries a read restriction at more than one level, and Confluence means the intersection of them, which cannot be worked out from outside the identity provider", p.ID)), true
	}
}

// of is one page's own restriction, from the cache or from the site.
func (r *restrictions) of(ctx context.Context, id string) own {
	r.mu.Lock()
	got, ok := r.by[id]
	r.mu.Unlock()
	if ok {
		return got
	}

	found := r.fetch(ctx, id)
	r.remember(id, found)
	return found
}

// forget empties the cache, which is what makes it last one crawl.
func (r *restrictions) forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.by)
}

// remember puts an answer in the cache.
func (r *restrictions) remember(id string, o own) {
	if id == "" {
		return
	}
	r.mu.Lock()
	r.by[id] = o
	r.mu.Unlock()
}

// fetch asks the site who may read one page, which is one request that names
// only that page and inherits nothing.
func (r *restrictions) fetch(ctx context.Context, id string) own {
	s := r.svc
	if !looksLikeID(id) {
		return own{}
	}

	var got restriction
	err := s.call(ctx, "/rest/api/content/"+url.PathEscape(id)+"/restriction/byOperation/read", url.Values{
		"expand": {"restrictions.user,restrictions.group"},
	}, &got)
	switch {
	case err == nil:
	case missing(err):
		// The page went away while the tree above another one was being walked,
		// which is a race every crawl has. A page that is not there restricts
		// nothing, and the sweep is what takes it out of the index.
		return own{}
	default:
		// Anything else is the site declining to say, and a restriction nobody
		// can read is the one thing that must not fall back to the space.
		s.skip(id, fmt.Errorf("who may read page %s: %w", id, err))
		return own{has: true, rule: connector.Unresolved(s.name, fmt.Sprintf("the read restriction on page %s could not be read", id))}
	}
	return readRule(s, id, &got)
}

// readRule turns a read restriction into who it names.
func readRule(s *Service, id string, got *restriction) own {
	if got == nil {
		return own{}
	}

	users := refs(s.name, got.Restrictions.User.Results, func(u person) string { return u.AccountID })
	groups := refs(s.name, got.Restrictions.Group.Results, groupName)
	if len(users) == 0 && len(groups) == 0 {
		// Confluence sends the read restriction on every page whether there is
		// one or not, and an empty one is the ordinary case rather than a page
		// nobody may read.
		return own{}
	}

	// A restriction the site sent only part of is a restriction we do not know.
	// Indexing a page against the half of it that arrived is how somebody who
	// was named on the second page of the list stops being able to find their
	// own document, and it is how somebody who was not named starts.
	if u, g := got.Restrictions.User, got.Restrictions.Group; u.Size > len(u.Results) || g.Size > len(g.Results) {
		s.skip(id, fmt.Errorf("the read restriction on page %s names %d accounts and %d groups and the site sent %d and %d of them",
			id, u.Size, g.Size, len(u.Results), len(g.Results)))
		return own{has: true, rule: connector.Unresolved(s.name, fmt.Sprintf("the read restriction on page %s names more people than the site sent in one answer", id))}
	}

	return own{has: true, rule: acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      s.name,
		AllowUsers:  tidy(users),
		AllowGroups: tidy(groups),
	}}
}

// refs turns whoever a permission or a restriction names into references, and
// drops the entries that name nobody.
func refs[T any](source string, in []T, value func(T) string) []acl.Ref {
	out := make([]acl.Ref, 0, len(in))
	for _, v := range in {
		if got := value(v); got != "" {
			out = append(out, acl.Ref{Source: source, Value: got})
		}
	}
	return out
}

// groupName is what to write a group into an access control list as.
//
// The name rather than the id, because a name is what an identity provider hands
// back when it is asked what somebody is in, and matching on the id would mean
// resolving every group on the site to find out which one that was.
func groupName(g group) string { return g.Name }

// tidy puts a set of references in order and drops the repeats.
//
// The order Confluence lists anything in is not something anybody promised, and
// a list that reorders itself between syncs is a permission change that never
// happened, written to every page in the space.
func tidy(refs []acl.Ref) []acl.Ref {
	if len(refs) == 0 {
		return nil
	}
	slices.SortFunc(refs, func(a, b acl.Ref) int {
		if d := strings.Compare(a.Source, b.Source); d != 0 {
			return d
		}
		return strings.Compare(a.Value, b.Value)
	})
	return slices.Compact(refs)
}
