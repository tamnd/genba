package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/cache"
	"github.com/tamnd/genba/directory"
)

// Resolving wraps an authenticator so that a request's group membership comes
// from a directory rather than from whoever authenticated it.
//
// The two are different questions and it is worth keeping them apart. Who this
// is comes from a credential and is the authenticator's job. What they are a
// member of is a fact about the company, it changes without anybody signing in
// again, and no token is a good place to keep it. A proxy that passes a group
// list down is passing on a copy of an answer somebody else cached, and the day
// somebody is taken out of a group is the day the copy is wrong.
//
// So this replaces the group set entirely rather than adding to it. A wrapped
// authenticator that puts groups on the principal has them thrown away, which
// is the point: if the directory is the answer then a header saying otherwise
// is not a second opinion, it is a way in.
//
// Identities are merged rather than replaced, because those come from two
// places legitimately. A token can say which Slack account signed in and the
// directory can say which Jira account is the same person.
type Resolving struct {
	// Auth says who is asking. It is usually [HeaderAuth] behind a proxy, and
	// it is whatever a deployment authenticates with.
	Auth Authenticator

	// Resolver is the directory the groups come from. It is a
	// [directory.Resolver] on its own, or one wrapped in a
	// [directory.Cache], and nothing here can tell the difference.
	Resolver directory.Expander

	// Lookup says how the principal is named in the directory, and it is
	// optional. The default is the identity the principal carries for the
	// directory's own source if there is one, and the authenticated subject
	// otherwise, which is right whenever people sign in with the identity
	// provider the directory is.
	Lookup func(p *acl.Principal) string
}

// ErrDirectoryUnavailable is what [Resolving] returns when the directory could
// not answer.
//
// It is deliberately not [ErrUnauthenticated]. The credential was fine and the
// request is refused anyway, and an operator reading a wall of 401s during a
// directory outage would go looking at the wrong system. The request still
// fails, because the alternative is serving somebody with no groups and calling
// it a search result.
var ErrDirectoryUnavailable = errors.New("api: the directory could not be reached")

// Authenticate resolves the request and then resolves its groups.
func (rs Resolving) Authenticate(r *http.Request) (*acl.Principal, error) {
	if rs.Auth == nil || rs.Resolver == nil {
		return nil, ErrUnauthenticated
	}
	p, err := rs.Auth.Authenticate(r)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrUnauthenticated
	}

	id := rs.name(p)
	if id == "" {
		return nil, ErrUnauthenticated
	}

	got, err := rs.Resolver.Expand(r.Context(), id)
	switch {
	case errors.Is(err, directory.ErrNoSubject), errors.Is(err, directory.ErrDisabled):
		// Somebody the directory does not hold, or holds and has closed. Both
		// are the directory answering rather than failing, and both are a no.
		return nil, ErrUnauthenticated
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrDirectoryUnavailable, err)
	}

	// The group set is replaced rather than merged. Anything the wrapped
	// authenticator claimed about membership is gone by here.
	p.Groups = acl.GroupSet{}
	got.Apply(p)
	return p, nil
}

// CacheStats reports the directory layer under the name the stats endpoint and
// the metrics file it under, and nothing at all when the deployment resolves
// without a cache.
//
// Publishing a layer that is not there as a row of zeros would be worse than
// leaving it out, because a hit rate of zero and no cache look the same on a
// dashboard and mean opposite things.
func (rs Resolving) CacheStats() map[string]cache.Stats {
	reporter, ok := rs.Resolver.(interface{ Stats() cache.Stats })
	if !ok {
		return nil
	}
	return map[string]cache.Stats{"directory": reporter.Stats()}
}

// staleness is the directory cache's bound in seconds, and false when there is
// no cache to have one.
func (rs Resolving) staleness() (float64, bool) {
	bounded, ok := rs.Resolver.(interface{ Staleness() time.Duration })
	if !ok {
		return 0, false
	}
	return bounded.Staleness().Seconds(), true
}

// name is who to look up.
func (rs Resolving) name(p *acl.Principal) string {
	if rs.Lookup != nil {
		return rs.Lookup(p)
	}
	if id, ok := p.IdentityIn(rs.Resolver.Name()); ok {
		return id
	}
	return p.Subject
}
