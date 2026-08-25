package google_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector/limit"
	"github.com/tamnd/genba/directory"
	"github.com/tamnd/genba/directory/directorytest"
	"github.com/tamnd/genba/directory/google"
)

func TestConformance(t *testing.T) {
	directorytest.Run(t, func(t *testing.T) directorytest.Fixture {
		o, endpoint := newDomain(t)
		return directorytest.Fixture{
			Directory: dial(t, endpoint),
			Put:       o.put,
			PutGroup:  o.putGroup,

			// Neither Flat nor Transitive. Workspace groups nest and the
			// provider will not walk the nesting, so this is the ordinary case
			// the resolver was built for and every case in the suite applies.

			// Drop is deliberately absent. A membership naming a group that is
			// not there is not a state this provider can be in, because the
			// collection answers with the group objects rather than with their
			// ids, and an object that is not there is not in the answer.

			Identity: func(t *testing.T, s *directory.Subject) acl.Identity {
				t.Helper()
				s.Email = "mei@acme.test"
				return acl.Identity{Source: "email", Value: "mei@acme.test"}
			},
		}
	})
}

// dial is a directory over the fake domain.
//
// Without the rate limiter, because these cases are about the adapter and the
// limiter is tested where it lives. Leaving it on would make a case about a
// person in three hundred groups a case about how long five requests a second
// takes.
func dial(t *testing.T, endpoint string, opts ...google.Option) *google.Directory {
	t.Helper()
	d, err := google.New(google.Token(bearer), append([]google.Option{
		google.WithEndpoint(endpoint),
		google.WithHTTPClient(http.DefaultClient),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// expand is one resolution, which is what every case below is about.
func expand(t *testing.T, d *google.Directory, id string, opts ...directory.Option) directory.Expansion {
	t.Helper()
	got, err := attempt(t, d, id, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func attempt(t *testing.T, d *google.Directory, id string, opts ...directory.Option) (directory.Expansion, error) {
	t.Helper()
	r, err := directory.New(d, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return r.Expand(t.Context(), id)
}

// members is the group set with the source prefix taken back off, because what
// these cases are about is which groups came back rather than how a key is
// spelled.
func members(t *testing.T, d *google.Directory, got directory.Expansion) []string {
	t.Helper()
	out := make([]string, 0, len(got.Groups.Members))
	for _, key := range got.Groups.Members {
		out = append(out, strings.TrimPrefix(key, d.Name()+":"))
	}
	return out
}

func TestTheGroupObjectsAListingCarriedAreNotAskedForAgain(t *testing.T) {
	o, endpoint := newDomain(t)
	in := make([]string, 0, 300)
	for i := range 300 {
		id := "g" + strconv.Itoa(i)
		o.putGroup(t, directory.Group{ID: id, Name: "Group " + strconv.Itoa(i)})
		in = append(in, id)
	}
	o.put(t, directory.Subject{ID: "mei", MemberOf: in})

	got := expand(t, dial(t, endpoint), "mei")
	if len(got.Groups.Members) != 300 {
		t.Fatalf("mei resolved to %d groups, want 300", len(got.Groups.Members))
	}
	// The whole point of the buffer. Three hundred groups is one listing and
	// not three hundred requests for objects the listing already carried.
	if n := o.spent("group"); n != 0 {
		t.Errorf("a person in three hundred groups cost %d object lookups, want none", n)
	}
	// What the buffer cannot save is the membership of each of those groups,
	// because the listing says nothing about it. Two pages for mei at the
	// default size and one listing for each group above her.
	if n := o.spent("list"); n != 302 {
		t.Errorf("a person in three hundred groups cost %d listings, want 302", n)
	}
}

func TestWithTheBufferOffEveryGroupIsAnExtraRequest(t *testing.T) {
	o, endpoint := newDomain(t)
	in := make([]string, 0, 20)
	for i := range 20 {
		id := "g" + strconv.Itoa(i)
		o.putGroup(t, directory.Group{ID: id})
		in = append(in, id)
	}
	o.put(t, directory.Subject{ID: "mei", MemberOf: in})

	// Saying what the buffer is worth by taking it away. Without this the case
	// above would pass against an adapter that never looks a group up at all.
	expand(t, dial(t, endpoint, google.WithBuffer(0, 0)), "mei")
	if n := o.spent("group"); n != 20 {
		t.Errorf("twenty groups cost %d object lookups with the buffer off, want 20", n)
	}
}

func TestAListingIsFollowedAcrossItsPages(t *testing.T) {
	o, endpoint := newDomain(t)
	in := make([]string, 0, 25)
	for i := range 25 {
		id := fmt.Sprintf("g%02d", i)
		o.putGroup(t, directory.Group{ID: id})
		in = append(in, id)
	}
	o.put(t, directory.Subject{ID: "mei", MemberOf: in})

	got := expand(t, dial(t, endpoint, google.WithPageSize(4)), "mei")
	if n := len(got.Groups.Members); n != 25 {
		t.Fatalf("mei resolved to %d groups, want 25", n)
	}
	// Seven pages for mei, and one empty listing for each of the twenty five
	// groups nothing is above.
	if n := o.spent("list"); n != 32 {
		t.Errorf("twenty five groups at four a page cost %d requests, want 32", n)
	}
}

func TestNoGroupIsServedTwiceOrMissedAcrossAPageBoundary(t *testing.T) {
	o, endpoint := newDomain(t)
	want := make([]string, 0, 13)
	for i := range 13 {
		id := fmt.Sprintf("g%02d", i)
		o.putGroup(t, directory.Group{ID: id})
		want = append(want, id)
	}
	o.put(t, directory.Subject{ID: "mei", MemberOf: want})

	d := dial(t, endpoint, google.WithPageSize(5))
	got := members(t, d, expand(t, d, "mei"))
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("thirteen groups at five a page resolved to [%s], want [%s]", describe(got), describe(want))
	}
}

// TestAPageSizeTheEndpointWouldRefuseIsCapped is about a mistake in a
// configuration file rather than in the code, and about what it costs.
func TestAPageSizeTheEndpointWouldRefuseIsCapped(t *testing.T) {
	o, endpoint := newDomain(t)
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	// The endpoint refuses a page above two hundred with a bad request, and this
	// adapter reads a bad request on a lookup as a person the domain does not
	// hold. So passing the number straight through would not fail loudly, it
	// would quietly report that mei is in no groups.
	got := expand(t, dial(t, endpoint, google.WithPageSize(5000)), "mei")
	if n := len(got.Groups.Members); n != 1 {
		t.Fatalf("with an oversized page mei resolved to %d groups, want 1", n)
	}
	if n := o.spent("refused"); n != 0 {
		t.Errorf("%d requests were refused, so the page size went out as it was given", n)
	}
}

// TestAGroupInsideAnotherIsWalkedFromTheSameCollection is the fact the whole
// adapter is shaped around: one endpoint answers both lookups.
func TestAGroupInsideAnotherIsWalkedFromTheSameCollection(t *testing.T) {
	o, endpoint := newDomain(t)
	for i := range 4 {
		g := directory.Group{ID: "level" + strconv.Itoa(i)}
		if i > 0 {
			g.MemberOf = []string{"level" + strconv.Itoa(i-1)}
		}
		o.putGroup(t, g)
	}
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"level3"}})

	d := dial(t, endpoint)
	got := expand(t, d, "mei")
	want := []string{"level0", "level1", "level2", "level3"}
	in := members(t, d, got)
	slices.Sort(in)
	if !slices.Equal(in, want) {
		t.Fatalf("a four level nest resolved to [%s], want [%s]", describe(in), describe(want))
	}
	// One level per level, which is what a provider that does not expand
	// transitively costs and what the resolver is for.
	if got.Depth != 4 {
		t.Errorf("a four level nest was walked %d levels deep, want 4", got.Depth)
	}
}

// TestAnArchivedAccountIsRefusedTheSameWayASuspendedOneIs is the one fact about
// this provider that an adapter can get wrong without anything noticing.
func TestAnArchivedAccountIsRefusedTheSameWayASuspendedOneIs(t *testing.T) {
	o, endpoint := newDomain(t)
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "lee", MemberOf: []string{"engineering"}})
	o.edit(t, "lee", func(p *person) { p.archived = true })

	// Archiving is how a lot of companies handle somebody leaving: the mailbox
	// is kept and the licence is released, and suspended stays false. An adapter
	// reading that field alone keeps resolving the groups of everybody who has
	// left.
	got, err := attempt(t, dial(t, endpoint), "lee")
	if !errors.Is(err, directory.ErrDisabled) {
		t.Fatalf("an archived account gave %v, want ErrDisabled", err)
	}
	if len(got.Groups.Members) != 0 {
		t.Errorf("a refused expansion still handed back the groups %v", got.Groups.Members)
	}
	// And it costs one request. There is nobody to resolve, so the listing is a
	// question nobody is going to read the answer to.
	if n := o.spent("list"); n != 0 {
		t.Errorf("a refused account still cost %d membership listings", n)
	}
}

func TestEveryAddressThePersonHasBecomesAnIdentity(t *testing.T) {
	o, endpoint := newDomain(t)
	o.put(t, directory.Subject{ID: "mei", Email: "mei@acme.test"})
	o.edit(t, "mei", func(p *person) {
		p.aliases = []string{"mei.tanaka@acme.test"}
		p.others = []string{"m.tanaka@acme.test"}
	})

	got := expand(t, dial(t, endpoint), "mei")
	for _, want := range []acl.Identity{
		{Source: "google", Value: "mei"},
		{Source: "email", Value: "mei@acme.test"},
		{Source: "email", Value: "mei.tanaka@acme.test"},
		{Source: "email", Value: "m.tanaka@acme.test"},
	} {
		if !slices.Contains(got.Subject.Identities, want) {
			t.Errorf("mei resolved to %v, which does not include %v", got.Subject.Identities, want)
		}
	}
	// The primary address arrives twice, once on its own and once inside the
	// list of addresses, and the answer holds it once.
	if n := len(got.Subject.Identities); n != 4 {
		t.Errorf("mei resolved to %d identities, want 4 without repeats", n)
	}
}

// TestTheVersionMovesWhenAGroupIsPutInsideAnother is what the group version is
// for on a provider that nests, and it is the case a version taken straight off
// the etag would fail.
func TestTheVersionMovesWhenAGroupIsPutInsideAnother(t *testing.T) {
	o, endpoint := newDomain(t)
	o.putGroup(t, directory.Group{ID: "everyone"})
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	d := dial(t, endpoint, google.WithBuffer(0, 0))
	before := expand(t, d, "mei").Groups.Version

	// Nothing about the engineering group object changed, so its etag has not
	// moved. What changed is where it sits, and mei can now read everything
	// granted to everyone.
	o.editGroup(t, "engineering", func(g *team) { g.memberOf = []string{"everyone"} })

	got := expand(t, d, "mei")
	if len(got.Groups.Members) != 2 {
		t.Fatalf("after the move mei resolved to %v, want two groups", got.Groups.Members)
	}
	if got.Groups.Version == before {
		t.Errorf("a group being put inside another left the version at %d", before)
	}
}

// TestADomainThatSendsNoEtagsStillHasVersionsThatMove is about a provider that
// takes something away rather than about one that never had it. Google has been
// dropping etags across its APIs for years.
func TestADomainThatSendsNoEtagsStillHasVersionsThatMove(t *testing.T) {
	o, endpoint := newDomain(t)
	o.forgetEtags()
	o.putGroup(t, directory.Group{ID: "engineering", Name: "Engineering"})
	o.put(t, directory.Subject{ID: "mei", Email: "mei@acme.test", MemberOf: []string{"engineering"}})

	d := dial(t, endpoint, google.WithBuffer(0, 0))
	before := expand(t, d, "mei").Groups.Version

	// The group ids have not moved, so nothing about the membership catches
	// this. A version that was the etag alone would be empty here and this
	// change would never reach anything holding a cached group set.
	o.edit(t, "mei", func(p *person) { p.aliases = []string{"mei.tanaka@acme.test"} })
	if after := expand(t, d, "mei").Groups.Version; after == before {
		t.Errorf("with no etags to copy, a new address left the version at %d", before)
	}
}

// TestAQuotaRefusalIsWaitedOutRatherThanReported is the reason this adapter
// tells the transport what a throttle looks like here.
func TestAQuotaRefusalIsWaitedOutRatherThanReported(t *testing.T) {
	o, endpoint := newDomain(t)
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	// Two refusals and then the answer. The service says this with a forbidden
	// rather than with a 429, and a transport that read the status alone would
	// give up on a wait of a few milliseconds.
	o.throttle(2, "quotaExceeded")

	d := dial(t, endpoint, google.WithLimits(limit.Limits{
		Rate:       1000,
		Burst:      1000,
		MinBackoff: time.Millisecond,
		MaxBackoff: 5 * time.Millisecond,
	}))
	got := expand(t, d, "mei")
	if len(got.Groups.Members) != 1 {
		t.Fatalf("mei resolved to %v after two throttles, want one group", got.Groups.Members)
	}
	if n := o.spent("refused"); n != 2 {
		t.Errorf("the domain refused %d times, want 2", n)
	}
}

// TestAForbiddenThatIsNotAQuotaIsReportedAtOnce is the other half. Retrying a
// permission that is not going to change spends the rest of the quota arriving
// at the same answer more slowly.
func TestAForbiddenThatIsNotAQuotaIsReportedAtOnce(t *testing.T) {
	o, endpoint := newDomain(t)
	o.put(t, directory.Subject{ID: "mei"})
	o.throttle(20, "forbidden")

	d := dial(t, endpoint, google.WithLimits(limit.Limits{
		Rate:       1000,
		Burst:      1000,
		MinBackoff: time.Millisecond,
		MaxBackoff: 5 * time.Millisecond,
	}))
	_, err := attempt(t, d, "mei")
	switch {
	case err == nil:
		t.Fatal("a forbidden resolved somebody anyway")
	case errors.Is(err, directory.ErrNoSubject):
		t.Fatalf("a forbidden gave %v, which reads as a person who is not in the domain", err)
	case !strings.Contains(err.Error(), "forbidden"):
		t.Errorf("a forbidden gave %v, which does not say why", err)
	}
	if n := o.spent("refused"); n != 1 {
		t.Errorf("a refusal that was never going to clear was tried %d times, want once", n)
	}
}

func TestAnIdThatCouldNotBelongToAnybodyIsNobodyRatherThanAFailure(t *testing.T) {
	_, endpoint := newDomain(t)

	// The endpoint refuses a userKey that is neither an id nor an address with a
	// bad request rather than with a not found. Both mean the domain does not
	// hold this person, and treating the first as a failure turns one bad row in
	// a store into an expansion that never succeeds.
	_, err := attempt(t, dial(t, endpoint), "not an id")
	if !errors.Is(err, directory.ErrNoSubject) {
		t.Errorf("an id the service refused as malformed gave %v, want ErrNoSubject", err)
	}
}

func TestABadTokenIsAFailureRatherThanAnEmptyDirectory(t *testing.T) {
	_, endpoint := newDomain(t)

	d, err := google.New(google.Token("not-the-token"), google.WithEndpoint(endpoint), google.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	// The important half is that it is not ErrNoSubject. A domain that refuses
	// the credential and a domain that has never heard of somebody have to be
	// told apart, because the first is an outage and the second is a fact.
	_, err = attempt(t, d, "mei")
	switch {
	case err == nil:
		t.Fatal("a refused token resolved somebody anyway")
	case errors.Is(err, directory.ErrNoSubject):
		t.Fatalf("a refused token gave %v, which reads as a person who is not in the domain", err)
	case !strings.Contains(err.Error(), "authError"):
		t.Errorf("a refused token gave %v, which does not say why", err)
	}
}

func TestAGroupTheDomainDoesNotHoldIsRefusedAsSuch(t *testing.T) {
	_, endpoint := newDomain(t)
	_, err := dial(t, endpoint, google.WithBuffer(0, 0)).Group(t.Context(), "01abcdefgone")
	if !errors.Is(err, directory.ErrNoGroup) {
		t.Errorf("a group nobody holds gave %v, want ErrNoGroup", err)
	}
}

func TestTwoDomainsKeepTheirGroupsApart(t *testing.T) {
	one, first := newDomain(t)
	two, second := newDomain(t)
	for _, o := range []*domain{one, two} {
		o.putGroup(t, directory.Group{ID: "engineering"})
		o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
	}

	a := expand(t, dial(t, first, google.WithName("acme")), "mei")
	b := expand(t, dial(t, second, google.WithName("globex")), "mei")

	// The same id in two domains is two groups, and a deployment that indexed
	// both would otherwise hand one company's documents to the other's people.
	if slices.Equal(a.Groups.Members, b.Groups.Members) {
		t.Fatalf("two domains both resolved to %v", a.Groups.Members)
	}
	if !slices.Contains(a.Groups.Members, "acme:engineering") {
		t.Errorf("the first domain resolved to %v, want acme:engineering", a.Groups.Members)
	}
	if !slices.Contains(b.Groups.Members, "globex:engineering") {
		t.Errorf("the second domain resolved to %v, want globex:engineering", b.Groups.Members)
	}
}

func TestNewRefusesADomainItCannotUse(t *testing.T) {
	for _, c := range []struct {
		name   string
		tokens google.Tokens
		opts   []google.Option
	}{
		{"no tokens at all", nil, nil},
		{"an endpoint that is not a URL", google.Token(bearer), []google.Option{google.WithEndpoint("admin.googleapis.com")}},
		{"an endpoint with no host", google.Token(bearer), []google.Option{google.WithEndpoint("https://")}},
		{"no source name", google.Token(bearer), []google.Option{google.WithName("")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := google.New(c.tokens, c.opts...); err == nil {
				t.Error("that was accepted, and the failure would arrive at the first lookup instead")
			}
		})
	}
}

func TestResolvingFromOneDomainAtOnceIsSafe(t *testing.T) {
	o, endpoint := newDomain(t)
	for i := range 40 {
		o.putGroup(t, directory.Group{ID: "g" + strconv.Itoa(i)})
	}
	for i := range 8 {
		id := "u" + strconv.Itoa(i)
		in := make([]string, 0, 40)
		for g := range 40 {
			in = append(in, "g"+strconv.Itoa(g))
		}
		o.put(t, directory.Subject{ID: id, MemberOf: in})
	}

	d := dial(t, endpoint)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				if _, err := attempt(t, d, "u"+strconv.Itoa(i)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestALookupThatWasCancelledStops(t *testing.T) {
	o, endpoint := newDomain(t)
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := dial(t, endpoint).Subject(ctx, "mei"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled lookup gave %v, want a cancellation", err)
	}
}
