package jirasource

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/threadsource"
)

// DefaultACLRefresh is how often a project's rule is reapplied to the issues in
// it even though nothing said it had changed.
//
// Jira reports when an issue changed and reports nothing at all about when a
// permission scheme changed. Somebody removed from a project role, a group
// emptied out, a scheme swapped for a stricter one: none of those touch a single
// issue, and all of them are revocations. A revocation that reaches the index
// when somebody next comments on a ticket is not a revocation.
//
// So the rule is reapplied on a schedule as well. A day is the default: it
// bounds how long somebody who lost access keeps seeing a project's tickets in
// their results, and it costs one write per issue in the project once a day
// rather than a read of any of them.
const DefaultACLRefresh = 24 * time.Hour

// WithACLRefresh sets how often a project's rule is reapplied. Zero selects
// [DefaultACLRefresh].
func WithACLRefresh(d time.Duration) Option {
	return func(s *Service) { s.aclRefresh = d }
}

// browse is the Jira permission that decides whether somebody may see an issue
// at all. Everything else a permission scheme grants is about changing things.
const browse = "BROWSE_PROJECTS"

// Containers lists the projects this account can see, with who may read each.
func (s *Service) Containers(ctx context.Context) ([]threadsource.Container, error) {
	type listing struct {
		window
		Values []project `json:"values"`
	}

	var found []project
	err := pages(ctx, s, "/rest/api/3/project/search", url.Values{
		"orderBy": {"key"},
	}, func(page listing) (int, bool) {
		found = append(found, page.Values...)
		return len(page.Values), true
	})
	if err != nil {
		return nil, err
	}

	containers := make([]threadsource.Container, 0, len(found))
	for _, p := range found {
		access, err := s.access(ctx, p)
		if err != nil {
			return nil, err
		}
		// The key is the id and the name is the label, because the key is what
		// JQL calls the project and the name is what a person calls it.
		containers = append(containers, threadsource.Container{
			ID:       p.Key,
			Name:     p.Name,
			Access:   access,
			AccessAt: s.tick(),
		})
	}
	return containers, nil
}

// access works out who may browse a project.
//
// The answer comes from the permission scheme, which grants browse to some
// mixture of roles, groups and named accounts, and Jira will resolve all of
// that in one request if asked the right way. What comes back is what an access
// control list is: a set of accounts and a set of groups.
func (s *Service) access(ctx context.Context, p project) (acl.Permissions, error) {
	type holder struct {
		Type      string `json:"type"`
		Parameter string `json:"parameter"`
	}
	type grant struct {
		Holder holder `json:"holder"`
		Permit string `json:"permission"`
	}
	type scheme struct {
		Permissions []grant `json:"permissions"`
	}

	var out scheme
	err := s.call(ctx, "/rest/api/3/project/"+url.PathEscape(p.Key)+"/permissionscheme", url.Values{
		"expand": {"permissions"},
	}, &out)
	switch {
	case err == nil:
	case refused(err, http.StatusUnauthorized), refused(err, http.StatusForbidden), refused(err, http.StatusNotFound):
		// Reading a permission scheme is an administrator's request on some
		// sites, and this account may only be a user. Quarantining is the only
		// safe answer and staying quiet about it is the only unsafe one.
		s.skip(p.Key, fmt.Errorf("who may browse %s: %w", p.Key, err))
		return connector.Unresolved(s.name, fmt.Sprintf("reading the permission scheme for %s needs an administrator and this account is not one", p.Key)), nil
	default:
		return acl.Permissions{}, err
	}

	var (
		users  []acl.Ref
		groups []acl.Ref
		anyone bool
	)
	for _, g := range out.Permissions {
		if g.Permit != browse {
			continue
		}
		switch g.Holder.Type {
		case "group", "groupCustomField":
			if g.Holder.Parameter != "" {
				groups = append(groups, acl.Ref{Source: s.name, Value: g.Holder.Parameter})
			}
		case "user", "userCustomField":
			if g.Holder.Parameter != "" {
				users = append(users, acl.Ref{Source: s.name, Value: g.Holder.Parameter})
			}
		case "projectRole":
			members, err := s.role(ctx, p.Key, g.Holder.Parameter)
			if err != nil {
				return acl.Permissions{}, err
			}
			users = append(users, members.users...)
			groups = append(groups, members.groups...)
		case "applicationRole", "anyone", "sd.customer.portal.only":
			// Granting browse to everybody with a licence, or to anonymous
			// users, is a project that is open to the tenant. It is a real
			// configuration and pretending it is an access control list of
			// nobody would hide the project instead of describing it.
			anyone = true
		}
	}

	if anyone {
		return acl.Permissions{Mode: acl.ModePublicToTenant, Source: s.name}, nil
	}
	if len(users) == 0 && len(groups) == 0 {
		// A scheme that grants browse to nothing is not a project everybody can
		// read, it is a project we failed to understand. Say so.
		s.skip(p.Key, fmt.Errorf("the permission scheme for %s grants %s to nobody", p.Key, browse))
		return connector.Unresolved(s.name, fmt.Sprintf("the permission scheme for %s grants %s to nobody, which is a scheme we failed to understand rather than a project everybody may read", p.Key, browse)), nil
	}

	return acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      s.name,
		AllowUsers:  tidy(users),
		AllowGroups: tidy(groups),
	}, nil
}

// members is what a project role resolves to.
type members struct {
	users  []acl.Ref
	groups []acl.Ref
}

// role resolves a project role to the accounts and groups in it.
//
// A role is Jira's indirection between a person and a permission, and it is the
// one every real site uses: the scheme grants browse to Developers and the
// project decides who they are. An adapter that stopped at the role name would
// produce an access control list naming a thing rather than anybody.
func (s *Service) role(ctx context.Context, key, id string) (members, error) {
	if id == "" {
		return members{}, nil
	}

	type actor struct {
		Type      string `json:"type"`
		Name      string `json:"displayName"`
		ActorUser *struct {
			AccountID string `json:"accountId"`
		} `json:"actorUser"`
		ActorGroup *struct {
			Name string `json:"name"`
		} `json:"actorGroup"`
	}
	var out struct {
		Actors []actor `json:"actors"`
	}

	path := "/rest/api/3/project/" + url.PathEscape(key) + "/role/" + url.PathEscape(id)
	if err := s.call(ctx, path, nil, &out); err != nil {
		if refused(err, http.StatusUnauthorized) || missing(err) {
			// The same story as the scheme itself. The caller turns an empty
			// result plus nothing else into a quarantine rather than into a
			// project readable by nobody.
			s.skip(key, fmt.Errorf("who is in role %s of %s: %w", id, key, err))
			return members{}, nil
		}
		return members{}, err
	}

	var got members
	for _, a := range out.Actors {
		switch {
		case a.ActorUser != nil && a.ActorUser.AccountID != "":
			got.users = append(got.users, acl.Ref{Source: s.name, Value: a.ActorUser.AccountID})
		case a.ActorGroup != nil && a.ActorGroup.Name != "":
			got.groups = append(got.groups, acl.Ref{Source: s.name, Value: a.ActorGroup.Name})
		}
	}
	return got, nil
}

// tick is the time a project's rule is reported to have changed at.
//
// Jira reports nothing about that, so what is reported instead is the start of
// the current refresh interval, which is what makes the schedule in
// [DefaultACLRefresh] happen. It is quantised rather than measured from the
// last sync so that the answer does not depend on when the syncs ran: two
// servers reading the same site an hour apart agree about whether the rule has
// moved, and a sync every five minutes still only reapplies it once.
func (s *Service) tick() time.Time {
	every := s.aclRefresh
	if every <= 0 {
		every = DefaultACLRefresh
	}
	return s.now().UTC().Truncate(every)
}

// security resolves issue security levels to the people who may read what is
// behind them.
//
// It is a cache because a project with a security scheme puts the same handful
// of levels on thousands of issues, and resolving one per issue would spend the
// crawl on the same four answers. A failure is cached too, because a token that
// may not resolve a level this minute may not resolve it next minute either,
// and asking again per issue turns one refusal into a thousand.
type security struct {
	svc *Service

	mu sync.Mutex
	by map[string]acl.Permissions
}

func newSecurity(s *Service) *security {
	return &security{svc: s, by: make(map[string]acl.Permissions)}
}

// level returns the rule for an issue security level.
func (l *security) level(ctx context.Context, id, name string) acl.Permissions {
	l.mu.Lock()
	got, ok := l.by[id]
	l.mu.Unlock()
	if ok {
		return got
	}

	found := l.resolve(ctx, id, name)

	l.mu.Lock()
	l.by[id] = found
	l.mu.Unlock()
	return found
}

// resolve asks the site who is in a security level.
//
// Everything about this is deliberately pessimistic. An issue security level is
// the one thing in Jira somebody set on purpose to keep other people out, so a
// level this token cannot resolve is a quarantine rather than a fall back to the
// project's rule, and an empty level is a quarantine too rather than a level
// that grants nothing and is therefore ignored.
func (l *security) resolve(ctx context.Context, id, name string) acl.Permissions {
	s := l.svc

	type member struct {
		Holder struct {
			Type      string `json:"type"`
			Parameter string `json:"parameter"`
			User      *struct {
				AccountID string `json:"accountId"`
			} `json:"user"`
			Group *struct {
				Name string `json:"name"`
			} `json:"group"`
			ProjectRole *struct {
				ID int `json:"id"`
			} `json:"projectRole"`
		} `json:"holder"`
		IssueSecurityLevelID string `json:"issueSecurityLevelId"`
	}
	type listing struct {
		window
		Values []member `json:"values"`
	}

	var found []member
	err := pages(ctx, s, "/rest/api/3/issuesecurityschemes/level/member", url.Values{
		"levelId": {id},
	}, func(page listing) (int, bool) {
		found = append(found, page.Values...)
		return len(page.Values), true
	})
	if err != nil {
		s.skip("security level "+id, fmt.Errorf("who may read issues at security level %q: %w", name, err))
		return connector.Unresolved(s.name, fmt.Sprintf("the members of security level %q could not be listed", name))
	}

	var users, groups []acl.Ref
	for _, m := range found {
		switch {
		case m.Holder.User != nil && m.Holder.User.AccountID != "":
			users = append(users, acl.Ref{Source: s.name, Value: m.Holder.User.AccountID})
		case m.Holder.Group != nil && m.Holder.Group.Name != "":
			groups = append(groups, acl.Ref{Source: s.name, Value: m.Holder.Group.Name})
		case m.Holder.ProjectRole != nil:
			// A level granted to a role is granted to whoever is in that role
			// in the issue's own project, and the project is not something this
			// endpoint says. Resolving it correctly means knowing which issue is
			// being asked about, which this cache deliberately does not, so it
			// is a quarantine rather than a guess in either direction.
			s.skip("security level "+id, fmt.Errorf(
				"security level %q is granted to project role %d, which cannot be resolved without the project",
				name, m.Holder.ProjectRole.ID))
			return connector.Unresolved(s.name, fmt.Sprintf("security level %q is granted to a project role, and a role cannot be resolved without knowing which project the issue is in", name))
		}
	}

	if len(users) == 0 && len(groups) == 0 {
		s.skip("security level "+id, fmt.Errorf("security level %q resolves to nobody", name))
		return connector.Unresolved(s.name, fmt.Sprintf("security level %q resolves to nobody", name))
	}

	return acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      s.name,
		AllowUsers:  tidy(users),
		AllowGroups: tidy(groups),
	}
}

// tidy puts a set of references in order and drops the repeats.
//
// The order Jira lists anything in is not something anybody promised, and a
// list that reorders itself between syncs is a permission change that never
// happened, written to every document in the project.
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
