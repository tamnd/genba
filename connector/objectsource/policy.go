package objectsource

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/aclmap"
)

// Policy decides who may read an object.
//
// It is a separate thing from the source for the same reason it is on a
// filesystem: where the answer lives is a property of the deployment. Some
// buckets carry an access control list per object, some carry one on the bucket
// and nothing on the objects, and a great many carry neither because the
// account turned object ownership on and does all of it with policy documents
// that name roles this index has never heard of.
//
// Returning an error quarantines that one document and does not stop the sync.
type Policy interface {
	Permissions(ctx context.Context, key string) (acl.Permissions, error)
}

// PolicyFunc adapts a function to [Policy].
type PolicyFunc func(ctx context.Context, key string) (acl.Permissions, error)

// Permissions calls f.
func (f PolicyFunc) Permissions(ctx context.Context, key string) (acl.Permissions, error) {
	return f(ctx, key)
}

// PublicToTenant is a policy where every object is readable by everybody in the
// tenant.
//
// It is correct for a bucket of published documentation and wrong for almost
// everything else, so it has to be asked for by name.
func PublicToTenant(source string) Policy {
	return PolicyFunc(func(context.Context, string) (acl.Permissions, error) {
		return acl.Permissions{Mode: acl.ModePublicToTenant, Source: source}, nil
	})
}

// Versioned is the optional capability of a [Policy] that can say when its
// answer for a key last changed.
//
// It is what turns a permission change into a write rather than a resync of the
// whole bucket. The answer has to be cheap, because it is asked once per object
// the listing decided was unchanged, which on a real bucket is once per object.
type Versioned interface {
	// ChangedAt returns when the rule governing key last changed, on the
	// store's clock. A zero time means the policy has no idea, and nothing is
	// refreshed on the strength of it.
	ChangedAt(ctx context.Context, key string) (time.Time, error)
}

// Reloader is the optional capability of a [Policy] that caches. It is called
// once at the start of every sync.
type Reloader interface {
	Reload()
}

// BucketPolicy answers for every object with the bucket's own access control
// list.
//
// This is the cheap model and the common one. A bucket where the objects were
// written by one process and are read by one team has the same answer for all
// of them, and reading that answer once per sync rather than once per object is
// the difference between one request and a million.
//
// It is also the model that can notice a revocation. An object's own access
// control list can be rewritten without the object changing, and nothing in a
// listing says so, which means there is no incremental way to find out. A
// bucket's list is one request, so this policy reads it every sync and compares
// it with the last one, and when it differs every object in the bucket gets a
// permission change without a single byte being fetched.
type BucketPolicy struct {
	client *Client
	source string
	acls   *aclmap.Normalizer

	mu      sync.Mutex
	loaded  bool
	perms   acl.Permissions
	err     error
	seen    string
	changed time.Time
}

var (
	_ Policy    = (*BucketPolicy)(nil)
	_ Versioned = (*BucketPolicy)(nil)
	_ Reloader  = (*BucketPolicy)(nil)
)

// NewBucketPolicy returns a policy reading c's bucket access control list.
//
// The source is the connector's name and identity is the identity source the
// names in the list belong to. Any domains given are the ones that count as
// this tenant, which for object storage means the mail domains that appear in
// grants written against an email address.
func NewBucketPolicy(c *Client, source, identity string, domains ...string) (*BucketPolicy, error) {
	n, err := aclmap.New(aclmap.ObjectStore(source, identity, domains...))
	if err != nil {
		return nil, err
	}
	return &BucketPolicy{client: c, source: source, acls: n}, nil
}

// Permissions returns the bucket's answer, which is every object's answer.
func (p *BucketPolicy) Permissions(ctx context.Context, _ string) (acl.Permissions, error) {
	return p.load(ctx)
}

// ChangedAt returns when this process last saw the bucket's list change.
//
// The store keeps no such time, so this is when the difference was noticed
// rather than when it happened, read off the store's clock so that it can be
// compared with the modification times in a listing. That is late rather than
// wrong: the change is applied on the first sync after it was made, which is
// the same latency a content change gets.
func (p *BucketPolicy) ChangedAt(ctx context.Context, _ string) (time.Time, error) {
	if _, err := p.load(ctx); err != nil {
		return time.Time{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed, nil
}

// Reload forgets the list so the next question reads it again.
//
// It does not forget when the list last changed, which is the whole state worth
// keeping across a sync. A process that has been up for a week and never
// reloaded would answer with the permissions the bucket had when it started,
// which is the failure where a revocation looks applied and is not.
func (p *BucketPolicy) Reload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loaded = false
}

// Counts returns what the mapping could and could not represent.
func (p *BucketPolicy) Counts() aclmap.Counts { return p.acls.Counts() }

// load reads the bucket's list at most once per sync.
func (p *BucketPolicy) load(ctx context.Context) (acl.Permissions, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded {
		return p.perms, p.err
	}

	list, at, err := p.client.acl(ctx, "")
	if err != nil {
		// Recorded rather than retried per object. A bucket whose list cannot be
		// read has one problem, and asking it a million more times turns one
		// failure into a denial of service against the store.
		p.loaded, p.perms, p.err = true, connector.Unresolved(p.source, "the access control list of the bucket could not be read: "+err.Error()), err
		return p.perms, p.err
	}

	if mark := fingerprint(list); mark != p.seen {
		if p.seen != "" {
			p.changed = at
		}
		p.seen = mark
	}
	p.perms, p.err = p.acls.Normalize(grantsOf(list))
	p.loaded = true
	return p.perms, p.err
}

// ObjectPolicy reads the access control list of every object.
//
// It is the exact model, and it costs a request per object per sync, which on a
// bucket of any size is the most expensive thing this connector can be asked to
// do. Reach for it when the objects really do have different lists, and reach
// for [BucketPolicy] otherwise.
//
// It deliberately does not implement [Versioned]. The only way to find out
// whether one object's list changed is to read it, so a policy that answered
// that question would be doing the expensive thing on every object the listing
// had already decided was unchanged, which is the work the incremental path
// exists to avoid.
type ObjectPolicy struct {
	client *Client
	source string
	acls   *aclmap.Normalizer
}

var _ Policy = (*ObjectPolicy)(nil)

// NewObjectPolicy returns a policy reading each object's own access control
// list.
func NewObjectPolicy(c *Client, source, identity string, domains ...string) (*ObjectPolicy, error) {
	n, err := aclmap.New(aclmap.ObjectStore(source, identity, domains...))
	if err != nil {
		return nil, err
	}
	return &ObjectPolicy{client: c, source: source, acls: n}, nil
}

// Permissions reads one object's list.
func (p *ObjectPolicy) Permissions(ctx context.Context, key string) (acl.Permissions, error) {
	list, _, err := p.client.acl(ctx, key)
	if err != nil {
		return connector.Unresolved(p.source, "the access control list of this object could not be read: "+err.Error()), err
	}
	return p.acls.Normalize(grantsOf(list))
}

// Counts returns what the mapping could and could not represent.
func (p *ObjectPolicy) Counts() aclmap.Counts { return p.acls.Counts() }

// The grantee URIs S3 defines, which every service copying it also uses.
const (
	uriAllUsers           = "http://acs.amazonaws.com/groups/global/AllUsers"
	uriAuthenticatedUsers = "http://acs.amazonaws.com/groups/global/AuthenticatedUsers"

	// authenticatedUsers is what a grant to every account holder at the
	// provider is called once it is written down as a domain. It is not this
	// tenant and it cannot be enumerated, so it quarantines like any other
	// domain nobody has mapped, which is the point of naming it at all.
	authenticatedUsers = "authenticated-users"
)

// grantsOf turns one access control list into statements, in the store's own
// words, with nothing interpreted.
//
// Which permission confers read, what a grant to a domain means and what to do
// with something unrecognised are all decided in aclmap, once, for every
// connector. What is decided here is only who a statement is about.
func grantsOf(list accessControl) []aclmap.Grant {
	owner := nameOf(list.Owner)
	out := make([]aclmap.Grant, 0, len(list.Grants))
	for _, g := range list.Grants {
		subject, id := subjectOf(g.Grantee)
		out = append(out, aclmap.Grant{
			Subject: subject,
			ID:      id,
			Effect:  aclmap.Allow,
			Role:    g.Permission,
			// Owning an object is not reading it. The owner is marked here only
			// where the list already gives that account read, which is the same
			// reading the file system policy takes: an account that can give
			// itself permission has not been given it yet.
			Owner: subject == aclmap.User && id != "" && id == owner,
		})
	}
	return out
}

// subjectOf says who a grantee is.
//
// A group URI is the only kind that reaches outside the account, and the two
// that do are told apart because they mean very different things: one is the
// open internet and the other is every customer the provider has.
func subjectOf(g grantee) (subject aclmap.Subject, id string) {
	switch {
	case g.URI == uriAllUsers:
		return aclmap.Anyone, ""
	case g.URI == uriAuthenticatedUsers:
		return aclmap.Domain, authenticatedUsers
	case g.URI != "":
		// Every other group the service defines is a set of accounts it
		// maintains, which is what a group is.
		return aclmap.Group, strings.ToLower(g.URI[strings.LastIndex(g.URI, "/")+1:])
	default:
		// An empty name here is a statement about nobody, which aclmap counts as
		// malformed and quarantines. That is the right answer and it is left to
		// aclmap to give, rather than being dropped quietly on the way.
		return aclmap.User, nameOf(g)
	}
}

// nameOf is the best identity a grantee carries.
//
// An email address names a person and is what a company directory can be
// matched against. A display name is what the console shows. A canonical user
// id is neither: it names an account at the provider, it is a sixty four
// character hex string, and it will not match anything a person signs in with.
// It is used anyway when it is all there is, because a grant filed under it is
// still a grant somebody can go and look up, and dropping the statement would
// silently widen the list.
func nameOf(g grantee) string {
	switch {
	case g.Email != "":
		return strings.ToLower(g.Email)
	case g.DisplayName != "":
		return g.DisplayName
	default:
		return g.ID
	}
}

// fingerprint is a stable rendering of a list, for telling whether it changed.
//
// It is built from the parsed statements rather than from the bytes, because
// the bytes carry a request id in some services and the order of grants is not
// promised in any of them, and a fingerprint that moved on its own would
// rewrite the permissions of every object in the bucket on every sync.
func fingerprint(list accessControl) string {
	lines := make([]string, 0, len(list.Grants)+1)
	lines = append(lines, "owner="+nameOf(list.Owner))
	for _, g := range list.Grants {
		subject, id := subjectOf(g.Grantee)
		lines = append(lines, subject.String()+":"+id+"="+strings.ToLower(g.Permission))
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}
