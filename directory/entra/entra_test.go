package entra_test

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

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/directory"
	"github.com/tamnd/genba/directory/directorytest"
	"github.com/tamnd/genba/directory/entra"
)

func TestConformance(t *testing.T) {
	directorytest.Run(t, func(t *testing.T) directorytest.Fixture {
		o, endpoint := newTenant(t)
		return directorytest.Fixture{
			Directory: dial(t, endpoint),
			Put:       o.put,
			PutGroup:  o.putGroup,

			// Every group a person is in arrives at once, so a nest of any
			// depth is one level. See the package documentation.
			Transitive: true,

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

// dial is a directory over the fake tenant.
//
// Without the rate limiter, because these cases are about the adapter and the
// limiter is tested where it lives. Leaving it on would make a case about a
// person in three hundred groups a case about how long five requests a second
// takes.
func dial(t *testing.T, endpoint string, opts ...entra.Option) *entra.Directory {
	t.Helper()
	d, err := entra.New(entra.Token(bearer), append([]entra.Option{
		entra.WithEndpoint(endpoint),
		entra.WithHTTPClient(http.DefaultClient),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// expand is one resolution, which is what every case below is about.
func expand(t *testing.T, d *entra.Directory, id string, opts ...directory.Option) directory.Expansion {
	t.Helper()
	got, err := attempt(t, d, id, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func attempt(t *testing.T, d *entra.Directory, id string, opts ...directory.Option) (directory.Expansion, error) {
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
func members(t *testing.T, d *entra.Directory, got directory.Expansion) []string {
	t.Helper()
	out := make([]string, 0, len(got.Groups.Members))
	for _, key := range got.Groups.Members {
		out = append(out, strings.TrimPrefix(key, d.Name()+":"))
	}
	return out
}

func TestTheGroupsAListingAlreadyCarriedAreNotAskedForAgain(t *testing.T) {
	o, endpoint := newTenant(t)
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
	// not three hundred and one requests.
	if n := o.spent("group"); n != 0 {
		t.Errorf("a person in three hundred groups cost %d group lookups, want none", n)
	}
}

func TestWithTheBufferOffEveryGroupIsARequest(t *testing.T) {
	o, endpoint := newTenant(t)
	in := make([]string, 0, 20)
	for i := range 20 {
		id := "g" + strconv.Itoa(i)
		o.putGroup(t, directory.Group{ID: id})
		in = append(in, id)
	}
	o.put(t, directory.Subject{ID: "mei", MemberOf: in})

	// Saying what the buffer is worth by taking it away. Without this the test
	// above would pass against an adapter that never looks a group up at all.
	expand(t, dial(t, endpoint, entra.WithBuffer(0, 0)), "mei")
	if n := o.spent("group"); n != 20 {
		t.Errorf("twenty groups cost %d lookups with the buffer off, want 20", n)
	}
}

func TestAListingIsFollowedAcrossItsPages(t *testing.T) {
	o, endpoint := newTenant(t)
	in := make([]string, 0, 25)
	for i := range 25 {
		id := fmt.Sprintf("g%02d", i)
		o.putGroup(t, directory.Group{ID: id})
		in = append(in, id)
	}
	o.put(t, directory.Subject{ID: "mei", MemberOf: in})

	got := expand(t, dial(t, endpoint, entra.WithPageSize(4)), "mei")
	if n := len(got.Groups.Members); n != 25 {
		t.Fatalf("mei resolved to %d groups, want 25", n)
	}
	if n := o.spent("memberships"); n != 7 {
		t.Errorf("twenty five groups at four a page cost %d requests, want 7", n)
	}
}

func TestNoGroupIsServedTwiceOrMissedAcrossAPageBoundary(t *testing.T) {
	o, endpoint := newTenant(t)
	want := make([]string, 0, 13)
	for i := range 13 {
		id := fmt.Sprintf("g%02d", i)
		o.putGroup(t, directory.Group{ID: id})
		want = append(want, id)
	}
	o.put(t, directory.Subject{ID: "mei", MemberOf: want})

	d := dial(t, endpoint, entra.WithPageSize(5))
	got := members(t, d, expand(t, d, "mei"))
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("thirteen groups at five a page resolved to [%s], want [%s]", describe(got), describe(want))
	}
}

func TestADirectoryRoleIsNotAGroup(t *testing.T) {
	o, endpoint := newTenant(t)
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	d := dial(t, endpoint)
	got := members(t, d, expand(t, d, "mei"))
	// The cast on the end of the path is what keeps this out. Without it the
	// role is a directory object mei belongs to, its id goes in the group set
	// beside the real groups, and a rule naming it allows everybody who holds
	// it.
	if slices.Contains(got, role) {
		t.Errorf("mei resolved to [%s], which includes a directory role", describe(got))
	}
	if n := o.spent("uncast"); n != 0 {
		t.Errorf("%d membership requests went without the cast that asks for groups only", n)
	}
}

func TestTheClosureCostsOneLevelHoweverDeepTheNestIs(t *testing.T) {
	o, endpoint := newTenant(t)
	// Six levels, which against a provider the resolver has to walk is six
	// rounds of requests one after another.
	for i := range 6 {
		g := directory.Group{ID: "level" + strconv.Itoa(i)}
		if i > 0 {
			g.MemberOf = []string{"level" + strconv.Itoa(i-1)}
		}
		o.putGroup(t, g)
	}
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"level5"}})

	got := expand(t, dial(t, endpoint), "mei")
	if n := len(got.Groups.Members); n != 6 {
		t.Fatalf("a six level nest resolved to %d groups, want 6", n)
	}
	if got.Depth != 1 {
		t.Errorf("a six level nest was walked %d levels deep, want 1", got.Depth)
	}
	if n := o.spent("memberships"); n != 1 {
		t.Errorf("a six level nest cost %d membership requests, want 1", n)
	}
}

func TestAnAnswerWithoutTheAccountStateIsNotTakenAsADeactivation(t *testing.T) {
	o, endpoint := newTenant(t)
	o.silent()
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	// A field that is absent and a field that is false mean opposite things.
	// Reading the second out of the first refuses everybody in the tenant, and
	// the symptom is a search engine that has stopped finding anything.
	got, err := attempt(t, dial(t, endpoint), "mei")
	if err != nil {
		t.Fatalf("an answer with no accountEnabled in it gave %v, want mei's groups", err)
	}
	if len(got.Groups.Members) != 1 {
		t.Errorf("mei resolved to %v, want one group", got.Groups.Members)
	}
}

func TestAGuestPrincipalNameIsNotAnAddress(t *testing.T) {
	o, endpoint := newTenant(t)
	o.put(t, directory.Subject{ID: "mei"})
	o.edit(t, "mei", func(p *person) {
		p.mail = "mei@partner.test"
		p.upn = "mei_partner.test#EXT#@acme.onmicrosoft.com"
	})

	got := expand(t, dial(t, endpoint), "mei")
	want := acl.Identity{Source: "email", Value: "mei@partner.test"}
	if !slices.Contains(got.Subject.Identities, want) {
		t.Errorf("a guest resolved to %v, which does not include their address", got.Subject.Identities)
	}
	// It has an at sign in it and it is nobody's mailbox. A rule granting to
	// that string is not a rule anybody wrote.
	for _, id := range got.Subject.Identities {
		if strings.Contains(id.Value, "#EXT#") {
			t.Errorf("a guest resolved to %v, which includes their principal name as an address", got.Subject.Identities)
		}
	}
}

func TestEveryAddressThePersonHasBecomesAnIdentity(t *testing.T) {
	o, endpoint := newTenant(t)
	o.put(t, directory.Subject{ID: "mei", Email: "mei@acme.test"})
	o.edit(t, "mei", func(p *person) {
		p.other = []string{"mei.tanaka@acme.test", "m.tanaka@acme.test"}
	})

	got := expand(t, dial(t, endpoint), "mei")
	for _, want := range []acl.Identity{
		{Source: "entra", Value: "mei"},
		{Source: "email", Value: "mei@acme.test"},
		{Source: "email", Value: "mei.tanaka@acme.test"},
		{Source: "email", Value: "m.tanaka@acme.test"},
	} {
		if !slices.Contains(got.Subject.Identities, want) {
			t.Errorf("mei resolved to %v, which does not include %v", got.Subject.Identities, want)
		}
	}
	// The principal name is an address here and the mail is the same one, so
	// the answer holds it once.
	if n := len(got.Subject.Identities); n != 4 {
		t.Errorf("mei resolved to %d identities, want 4 without repeats", n)
	}
}

func TestTheVersionMovesWhenTheirAddressDoes(t *testing.T) {
	o, endpoint := newTenant(t)
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "mei", Email: "mei@acme.test", MemberOf: []string{"engineering"}})

	d := dial(t, endpoint, entra.WithBuffer(0, 0))
	before := expand(t, d, "mei").Groups.Version

	// The group ids have not moved, so nothing about the closure catches this.
	// A version that ignored it would leave a cached principal answering with
	// an address the person no longer has.
	o.edit(t, "mei", func(p *person) { p.other = []string{"mei.tanaka@acme.test"} })
	if after := expand(t, d, "mei").Groups.Version; after == before {
		t.Errorf("a new address left the version at %d", before)
	}
}

func TestAnIdThatCouldNotBelongToAnybodyIsNobodyRatherThanAFailure(t *testing.T) {
	_, endpoint := newTenant(t)

	// Graph refuses a lookup for something that is neither an id nor a
	// principal name with a bad request rather than with a not found. Both mean
	// the tenant does not hold this person, and treating the first as a failure
	// turns one bad row in a store into an expansion that never succeeds.
	_, err := attempt(t, dial(t, endpoint), "not an id")
	if !errors.Is(err, directory.ErrNoSubject) {
		t.Errorf("an id the service refused as malformed gave %v, want ErrNoSubject", err)
	}
}

func TestABadTokenIsAFailureRatherThanAnEmptyDirectory(t *testing.T) {
	_, endpoint := newTenant(t)

	d, err := entra.New(entra.Token("not-the-token"), entra.WithEndpoint(endpoint), entra.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	// The important half is that it is not ErrNoSubject. A tenant that refuses
	// the credential and a tenant that has never heard of somebody have to be
	// told apart, because the first is an outage and the second is a fact.
	_, err = attempt(t, d, "mei")
	switch {
	case err == nil:
		t.Fatal("a refused token resolved somebody anyway")
	case errors.Is(err, directory.ErrNoSubject):
		t.Fatalf("a refused token gave %v, which reads as a person who is not in the tenant", err)
	case !strings.Contains(err.Error(), "InvalidAuthenticationToken"):
		t.Errorf("a refused token gave %v, which does not say why", err)
	}
}

func TestAGroupTheTenantDoesNotHoldIsRefusedAsSuch(t *testing.T) {
	_, endpoint := newTenant(t)
	_, err := dial(t, endpoint, entra.WithBuffer(0, 0)).Group(t.Context(), "00g1gone")
	if !errors.Is(err, directory.ErrNoGroup) {
		t.Errorf("a group nobody holds gave %v, want ErrNoGroup", err)
	}
}

func TestTwoTenantsKeepTheirGroupsApart(t *testing.T) {
	one, first := newTenant(t)
	two, second := newTenant(t)
	for _, o := range []*tenant{one, two} {
		o.putGroup(t, directory.Group{ID: "engineering"})
		o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
	}

	a := expand(t, dial(t, first, entra.WithName("acme")), "mei")
	b := expand(t, dial(t, second, entra.WithName("globex")), "mei")

	// The same id in two tenants is two groups, and a deployment that indexed
	// both would otherwise hand one tenant's documents to the other's people.
	if slices.Equal(a.Groups.Members, b.Groups.Members) {
		t.Fatalf("two tenants both resolved to %v", a.Groups.Members)
	}
	if !slices.Contains(a.Groups.Members, "acme:engineering") {
		t.Errorf("the first tenant resolved to %v, want acme:engineering", a.Groups.Members)
	}
	if !slices.Contains(b.Groups.Members, "globex:engineering") {
		t.Errorf("the second tenant resolved to %v, want globex:engineering", b.Groups.Members)
	}
}

func TestNewRefusesATenantItCannotUse(t *testing.T) {
	for _, c := range []struct {
		name   string
		tokens entra.Tokens
		opts   []entra.Option
	}{
		{"no tokens at all", nil, nil},
		{"an endpoint that is not a URL", entra.Token(bearer), []entra.Option{entra.WithEndpoint("graph.microsoft.com")}},
		{"an endpoint with no host", entra.Token(bearer), []entra.Option{entra.WithEndpoint("https://")}},
		{"no source name", entra.Token(bearer), []entra.Option{entra.WithName("")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := entra.New(c.tokens, c.opts...); err == nil {
				t.Error("that was accepted, and the failure would arrive at the first lookup instead")
			}
		})
	}
}

func TestResolvingFromOneTenantAtOnceIsSafe(t *testing.T) {
	o, endpoint := newTenant(t)
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
	o, endpoint := newTenant(t)
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := dial(t, endpoint).Subject(ctx, "mei"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled lookup gave %v, want a cancellation", err)
	}
}
