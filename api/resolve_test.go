package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/cache"
	"github.com/tamnd/genba/directory"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// site is the directory the cases below resolve against: one person in one
// group that is inside another, one person who was deactivated, and nobody
// else.
func site(t *testing.T) *directory.Resolver {
	t.Helper()
	d := directory.NewStatic("acme")
	d.PutGroup(directory.Group{ID: "everyone"})
	d.PutGroup(directory.Group{ID: "engineering", MemberOf: []string{"everyone"}})
	d.Put(directory.Subject{
		ID:         "mei",
		Identities: []acl.Identity{{Source: "jira", Value: "acc-mei"}},
		MemberOf:   []string{"engineering"},
	})
	d.Put(directory.Subject{ID: "lee", MemberOf: []string{"engineering"}, Disabled: true})

	r, err := directory.New(d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func asking(subject string, header ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=gearbox", http.NoBody)
	r.Header.Set(api.HeaderSubject, subject)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	return r
}

func TestTheDirectoryDecidesTheGroups(t *testing.T) {
	auth := api.Resolving{
		Auth:     api.HeaderAuth{Tenant: "acme"},
		Resolver: site(t),
	}

	p, err := auth.Authenticate(asking("mei"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acme:engineering", "acme:everyone"}
	if !slices.Equal(p.Groups.Members, want) {
		t.Errorf("mei resolved to %v, want %v", p.Groups.Members, want)
	}
	if p.Groups.Version == 0 {
		t.Error("the principal came back with version zero")
	}
	if !slices.Contains(p.Identities, (acl.Identity{Source: "jira", Value: "acc-mei"})) {
		t.Errorf("the directory's identity did not reach the principal: %v", p.Identities)
	}
}

// The whole reason this type exists. A header saying which groups somebody is
// in is a copy of an answer, and a copy somebody can edit is a way in.
func TestAGroupsHeaderIsThrownAwayRatherThanBelieved(t *testing.T) {
	auth := api.Resolving{
		Auth:     api.HeaderAuth{Tenant: "acme"},
		Resolver: site(t),
	}

	r := asking("mei", api.HeaderGroups, "acme:administrators,acme:finance")
	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(p.Groups.Members, "acme:administrators") {
		t.Fatalf("a group from the request survived into the principal: %v", p.Groups.Members)
	}
	if want := []string{"acme:engineering", "acme:everyone"}; !slices.Equal(p.Groups.Members, want) {
		t.Errorf("the principal carries %v, want %v", p.Groups.Members, want)
	}
}

func TestASubjectTheDirectoryDoesNotHoldIsRefused(t *testing.T) {
	auth := api.Resolving{
		Auth:     api.HeaderAuth{Tenant: "acme"},
		Resolver: site(t),
	}
	if _, err := auth.Authenticate(asking("nobody")); !errors.Is(err, api.ErrUnauthenticated) {
		t.Errorf("a subject the directory does not hold gave %v, want ErrUnauthenticated", err)
	}
}

// An account closed on Friday stops resolving on Friday, whatever the token in
// somebody's browser still says.
func TestADeactivatedSubjectIsRefused(t *testing.T) {
	auth := api.Resolving{
		Auth:     api.HeaderAuth{Tenant: "acme"},
		Resolver: site(t),
	}
	if _, err := auth.Authenticate(asking("lee")); !errors.Is(err, api.ErrUnauthenticated) {
		t.Errorf("a deactivated subject gave %v, want ErrUnauthenticated", err)
	}
}

// down is a directory that cannot answer.
type down struct{ *directory.Static }

var errDown = errors.New("connection refused")

func (down) Subject(context.Context, string) (directory.Subject, error) {
	return directory.Subject{}, errDown
}

func TestADirectoryThatCannotBeReachedIsNotACredentialProblem(t *testing.T) {
	r, err := directory.New(down{directory.NewStatic("acme")})
	if err != nil {
		t.Fatal(err)
	}
	auth := api.Resolving{Auth: api.HeaderAuth{Tenant: "acme"}, Resolver: r}

	p, err := auth.Authenticate(asking("mei"))
	if !errors.Is(err, api.ErrDirectoryUnavailable) {
		t.Fatalf("a directory that could not answer gave %v", err)
	}
	if errors.Is(err, api.ErrUnauthenticated) {
		t.Error("a directory outage was reported as a bad credential")
	}
	if p != nil {
		t.Errorf("a refused request still produced a principal: %+v", p)
	}
}

// And the server says so, because a wall of 401s during a directory outage
// sends whoever is on call to the wrong system.
func TestTheServerAnswersUnavailableWhenTheDirectoryIs(t *testing.T) {
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	r, err := directory.New(down{directory.NewStatic("acme")})
	if err != nil {
		t.Fatal(err)
	}
	s := api.New(st, index.New(st), api.Resolving{
		Auth:     api.HeaderAuth{Tenant: "acme"},
		Resolver: r,
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, asking("mei"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a request during a directory outage answered %d, want 503", rec.Code)
	}
}

// The identity the directory knows people by is not always the one they sign in
// with, and a deployment that federates two providers has to be able to say so.
func TestTheLookupSaysWhoToAskAbout(t *testing.T) {
	d := directory.NewStatic("acme")
	d.PutGroup(directory.Group{ID: "engineering"})
	d.Put(directory.Subject{ID: "mei@acme.com", MemberOf: []string{"engineering"}})
	r, err := directory.New(d)
	if err != nil {
		t.Fatal(err)
	}

	auth := api.Resolving{
		Auth:     api.HeaderAuth{Tenant: "acme"},
		Resolver: r,
		Lookup:   func(p *acl.Principal) string { return p.Subject + "@acme.com" },
	}
	p, err := auth.Authenticate(asking("mei"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme:engineering"}; !slices.Equal(p.Groups.Members, want) {
		t.Errorf("the principal carries %v, want %v", p.Groups.Members, want)
	}
}

// Without a lookup, an identity for the directory's own source wins over the
// authenticated subject, which is what a proxy in front of an identity provider
// hands down.
func TestAnIdentityForTheDirectoryIsPreferredOverTheSubject(t *testing.T) {
	auth := api.Resolving{
		Auth:     api.HeaderAuth{Tenant: "acme"},
		Resolver: site(t),
	}
	r := asking("u-12345", api.HeaderIdentities, "acme:mei")
	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme:engineering", "acme:everyone"}; !slices.Equal(p.Groups.Members, want) {
		t.Errorf("the principal carries %v, want %v", p.Groups.Members, want)
	}
}

func TestAWrapperWithNothingToWrapRefuses(t *testing.T) {
	if _, err := (api.Resolving{}).Authenticate(asking("mei")); !errors.Is(err, api.ErrUnauthenticated) {
		t.Errorf("an unconfigured wrapper gave %v", err)
	}
	if _, err := (api.Resolving{Auth: api.HeaderAuth{Tenant: "acme"}}).Authenticate(asking("mei")); !errors.Is(err, api.ErrUnauthenticated) {
		t.Errorf("a wrapper with no directory gave %v", err)
	}
}

// caching wraps the site directory in a cache, which is how a deployment
// resolves once the traffic is real.
func caching(t *testing.T, opts ...directory.CacheOption) *directory.Cache {
	t.Helper()
	c, err := directory.NewCache(site(t), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Nothing at the edge of the API can tell a cache from a resolver, which is the
// point of the interface being two methods.
func TestACachedDirectoryAnswersTheSame(t *testing.T) {
	auth := api.Resolving{Auth: api.HeaderAuth{Tenant: "acme"}, Resolver: caching(t)}

	first, err := auth.Authenticate(asking("mei"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := auth.Authenticate(asking("mei"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acme:engineering", "acme:everyone"}
	if !slices.Equal(second.Groups.Members, want) {
		t.Errorf("the cached answer is %v, want %v", second.Groups.Members, want)
	}
	if first.Groups.Version != second.Groups.Version {
		t.Error("two requests for the same person got different versions")
	}
}

// A cache layer nobody can see the hit rate of is a cache layer nobody can tell
// is broken, and this one decides how hard the directory is being hit.
func TestTheDirectoryLayerIsOnTheStatsEndpoint(t *testing.T) {
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, index.New(st), api.Resolving{
		Auth:     api.HeaderAuth{Tenant: "acme", Admins: []string{"mei"}},
		Resolver: caching(t),
	})
	h := s.Handler()

	for range 2 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asking("mei"))
		if rec.Code != http.StatusOK {
			t.Fatalf("a search answered %d: %s", rec.Code, rec.Body.String())
		}
	}

	stats := asking("mei")
	stats.URL.Path = "/api/v1/stats"
	stats.URL.RawQuery = ""
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, stats)
	if rec.Code != http.StatusOK {
		t.Fatalf("the stats endpoint answered %d", rec.Code)
	}
	var body struct {
		Cache map[string]cache.Stats `json:"cache"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	got, ok := body.Cache["directory"]
	if !ok {
		t.Fatalf("no directory layer in %v", body.Cache)
	}
	if got.Hits == 0 {
		t.Errorf("the second request did not hit the directory cache: %+v", got)
	}
}

// The bound is a promise the deployment is making about its permissions, and a
// promise that only exists in a configuration file is one nobody is checking.
func TestTheStalenessBoundIsPublished(t *testing.T) {
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, index.New(st), api.Resolving{
		Auth:     api.HeaderAuth{Tenant: "acme"},
		Resolver: caching(t, directory.WithTTL(30*time.Second)),
	})

	rec := httptest.NewRecorder()
	s.Metrics().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	if !strings.Contains(rec.Body.String(), "\n"+api.MetricGroupStaleness+" 30\n") {
		t.Errorf("the staleness bound is not published:\n%s", rec.Body.String())
	}
}

// A hit rate of zero and no cache at all look the same on a dashboard and mean
// opposite things, so a deployment that resolves without one publishes neither.
func TestADeploymentWithoutADirectoryCachePublishesNoLayer(t *testing.T) {
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, index.New(st), api.Resolving{
		Auth:     api.HeaderAuth{Tenant: "acme"},
		Resolver: site(t),
	})

	rec := httptest.NewRecorder()
	s.Metrics().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	body := rec.Body.String()
	if strings.Contains(body, `genba_cache_hits_total{layer="directory"}`) {
		t.Error("a deployment with no directory cache published a directory layer")
	}
	if strings.Contains(body, "\n"+api.MetricGroupStaleness+" ") {
		t.Error("a deployment with no directory cache published a staleness bound")
	}
}
