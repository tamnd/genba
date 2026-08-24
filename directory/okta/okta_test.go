package okta_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/directory"
	"github.com/tamnd/genba/directory/directorytest"
	"github.com/tamnd/genba/directory/okta"
)

// TestConformance is the suite every adapter has to pass, over a fake
// organisation.
//
// Flat, because Okta groups do not contain groups, so the cases about walking a
// graph are about a provider this is not. No Drop, because a membership in Okta
// is a fact stored on the group and the organisation cannot get into the state
// that case describes on its own.
func TestConformance(t *testing.T) {
	directorytest.Run(t, func(t *testing.T) directorytest.Fixture {
		o, base := newOrg(t)
		return directorytest.Fixture{
			Directory: dial(t, base),
			Put:       o.put,
			PutGroup:  o.putGroup,
			Flat:      true,
			Identity: func(t *testing.T, s *directory.Subject) acl.Identity {
				t.Helper()
				s.Email = "mei@acme.test"
				return acl.Identity{Source: "email", Value: "mei@acme.test"}
			},
		}
	})
}

// dial is the adapter under test, pointed at a fake.
func dial(t *testing.T, base string, opts ...okta.Option) *okta.Directory {
	t.Helper()
	d, err := okta.New(base, token, append([]okta.Option{
		okta.WithHTTPClient(http.DefaultClient),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// expand resolves one person through the resolver, which is the only way an
// adapter is ever used.
func expand(t *testing.T, d directory.Directory, id string, opts ...directory.Option) directory.Expansion {
	t.Helper()
	r, err := directory.New(d, opts...)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Expand(t.Context(), id)
	if err != nil {
		t.Fatalf("expanding %q: %v", id, err)
	}
	return got
}

// joins puts somebody in n groups and hands back the fake and the adapter.
func joins(t *testing.T, n int, opts ...okta.Option) (*org, *okta.Directory) {
	t.Helper()
	o, base := newOrg(t)
	in := make([]string, 0, n)
	for i := range n {
		id := "g" + strconv.Itoa(i)
		o.putGroup(t, directory.Group{ID: id})
		in = append(in, id)
	}
	o.put(t, directory.Subject{ID: "mei", MemberOf: in})
	return o, dial(t, base, opts...)
}

// The reason the buffer exists. A person in three hundred groups would be three
// hundred group lookups against an organisation everybody else is signing in
// to, and the listing that answered the subject already carried every one of
// those objects past on its way here.
func TestTheGroupsAListingAlreadyCarriedAreNotAskedForAgain(t *testing.T) {
	o, d := joins(t, 300)
	o.forget()

	got := expand(t, d, "mei")
	if len(got.Groups.Members) != 300 {
		t.Fatalf("a person in 300 groups resolved to %d", len(got.Groups.Members))
	}
	if n := o.count("group"); n != 0 {
		t.Errorf("resolving somebody cost %d group lookups on top of the listing that already held them", n)
	}
	if n := o.count("user"); n != 1 {
		t.Errorf("resolving somebody read the user %d times", n)
	}
}

// And with it off the same expansion is a request each, which is what says the
// number above is the buffer working rather than the fake being asked nothing.
func TestWithTheBufferOffEveryGroupIsARequest(t *testing.T) {
	o, d := joins(t, 20, okta.WithBuffer(0, 0))
	o.forget()

	got := expand(t, d, "mei")
	if len(got.Groups.Members) != 20 {
		t.Fatalf("a person in 20 groups resolved to %d", len(got.Groups.Members))
	}
	if n := o.count("group"); n != 20 {
		t.Errorf("an unbuffered expansion cost %d group lookups, want 20", n)
	}
}

// A listing arrives a page at a time and the pages are followed. An adapter
// that read the first one and stopped would give somebody a fraction of their
// groups, which is somebody quietly getting fewer search results.
//
// The fake sends a rel="self" link as well, with a comma inside the URL, which
// is legal and is what a Link header read by splitting on every comma gets
// wrong. An adapter that followed self would ask the same question until
// something stopped it.
func TestAListingIsFollowedAcrossItsPages(t *testing.T) {
	o, d := joins(t, 25, okta.WithPageSize(4))
	o.forget()

	got := expand(t, d, "mei")
	if len(got.Groups.Members) != 25 {
		t.Fatalf("a person in 25 groups over pages of 4 resolved to %d: %s", len(got.Groups.Members), describe(got.Groups.Members))
	}
	// Seven pages, because the last full one is followed by an empty one only
	// if the service says there is one, and this one does not.
	if n := o.count("memberships"); n != 7 {
		t.Errorf("25 groups in pages of 4 took %d requests, want 7", n)
	}
}

// A page boundary that moves would serve a group twice or not at all, so the
// listing is ordered and the cursor is a position in that order.
func TestNoGroupIsServedTwiceOrMissedAcrossAPageBoundary(t *testing.T) {
	_, d := joins(t, 9, okta.WithPageSize(2))

	got := expand(t, d, "mei")
	want := make([]string, 0, 9)
	for i := range 9 {
		want = append(want, "okta:g"+strconv.Itoa(i))
	}
	slices.Sort(want)
	if !slices.Equal(got.Groups.Members, want) {
		t.Errorf("paging gave %s", describe(got.Groups.Members))
	}
}

// Okta has eight account states and only some of them are a decision somebody
// made about access. Refusing on the others would lock people out of search for
// forgetting a password.
func TestOnlyTheStatesThatMeanSomebodyLeftAreRefused(t *testing.T) {
	tests := []struct {
		status  string
		refused bool
	}{
		{"ACTIVE", false},
		{"PROVISIONED", false},
		{"RECOVERY", false},
		{"LOCKED_OUT", false},
		{"PASSWORD_EXPIRED", false},
		{"STAGED", true},
		{"SUSPENDED", true},
		{"DEPROVISIONED", true},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			o, base := newOrg(t)
			o.putGroup(t, directory.Group{ID: "engineering"})
			o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
			o.edit(t, "mei", func(u *person) { u.status = tt.status })

			r, err := directory.New(dial(t, base))
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.Expand(t.Context(), "mei")
			switch {
			case tt.refused && !errors.Is(err, directory.ErrDisabled):
				t.Fatalf("%s resolved with %v, want ErrDisabled", tt.status, err)
			case tt.refused && len(got.Groups.Members) != 0:
				t.Errorf("a refused expansion handed back %s", describe(got.Groups.Members))
			case !tt.refused && err != nil:
				t.Fatalf("%s was refused with %v", tt.status, err)
			case !tt.refused && len(got.Groups.Members) != 1:
				t.Errorf("%s resolved to %s", tt.status, describe(got.Groups.Members))
			}
		})
	}
}

// A refused account is refused before the group listing, because nobody is
// going to read the answer to it.
func TestARefusedAccountDoesNotCostAListing(t *testing.T) {
	o, base := newOrg(t)
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "lee", MemberOf: []string{"engineering"}, Disabled: true})
	o.forget()

	r, err := directory.New(dial(t, base))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Expand(t.Context(), "lee"); !errors.Is(err, directory.ErrDisabled) {
		t.Fatalf("a suspended account gave %v", err)
	}
	if n := o.count("memberships"); n != 0 {
		t.Errorf("a refused account cost %d group listings", n)
	}
}

// The whole point of an identity is a rule written where the document lives
// applying to somebody who signed in through Okta, and the address is what
// those rules are usually written in.
func TestBothAddressesAndTheOktaIdBecomeIdentities(t *testing.T) {
	o, base := newOrg(t)
	o.put(t, directory.Subject{ID: "00u1mei", Email: "mei@acme.test"})
	o.edit(t, "00u1mei", func(u *person) { u.secondEmail = "mei@personal.test" })

	got := expand(t, dial(t, base), "00u1mei")
	for _, want := range []acl.Identity{
		{Source: "okta", Value: "00u1mei"},
		{Source: "email", Value: "mei@acme.test"},
		{Source: "email", Value: "mei@personal.test"},
	} {
		if !slices.Contains(got.Subject.Identities, want) {
			t.Errorf("%v is not among the identities %v", want, got.Subject.Identities)
		}
	}
}

// A login is an email address at almost every organisation and is a username at
// the few that use a directory of their own. Turning one of those into an email
// identity would be a rule granting to an address matching somebody who does
// not have it.
func TestALoginThatIsNotAnAddressIsNotAnEmailIdentity(t *testing.T) {
	o, base := newOrg(t)
	o.put(t, directory.Subject{ID: "00u1mei", Email: "mei@acme.test"})
	o.edit(t, "00u1mei", func(u *person) { u.login = "mei.nakamura" })

	got := expand(t, dial(t, base), "00u1mei")
	if slices.Contains(got.Subject.Identities, acl.Identity{Source: "email", Value: "mei.nakamura"}) {
		t.Errorf("a username became an email address: %v", got.Subject.Identities)
	}
}

func TestTheDisplayNameFallsBackTheWayOktaDoes(t *testing.T) {
	tests := []struct {
		name string
		edit func(*person)
		want string
	}{
		{"the display name when there is one", func(u *person) {
			u.display, u.first, u.last = "Mei N", "Mei", "Nakamura"
		}, "Mei N"},
		{"the two halves of the name otherwise", func(u *person) {
			u.first, u.last = "Mei", "Nakamura"
		}, "Mei Nakamura"},
		{"the login when there is nothing else", func(u *person) {
			u.login = "mei.nakamura"
		}, "mei.nakamura"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, base := newOrg(t)
			o.put(t, directory.Subject{ID: "mei"})
			o.edit(t, "mei", func(u *person) {
				u.display, u.first, u.last = "", "", ""
				tt.edit(u)
			})

			if got := expand(t, dial(t, base), "mei").Subject.Name; got != tt.want {
				t.Errorf("the name is %q, want %q", got, tt.want)
			}
		})
	}
}

// The version invalidates one person's group set, and somebody else joining a
// group they are both in does not change it. Okta sends lastMembershipUpdated,
// which moves every time anybody joins anything, and a version built on it
// would be correct and useless at a company of any size.
func TestTheVersionIgnoresWhoElseJoinedTheGroup(t *testing.T) {
	o, base := newOrg(t)
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
	d := dial(t, base, okta.WithBuffer(0, 0))

	before := expand(t, d, "mei").Groups.Version
	for i := range 50 {
		o.put(t, directory.Subject{ID: "other" + strconv.Itoa(i), MemberOf: []string{"engineering"}})
	}
	after := expand(t, d, "mei").Groups.Version

	if before != after {
		t.Errorf("fifty other people joining moved mei's version from %d to %d, so nothing cached above it survives a working day", before, after)
	}
}

// And it does move when this person's own membership does, which is the half
// that is not allowed to be sacrificed for the half above.
func TestTheVersionMovesWhenThisPersonsMembershipDoes(t *testing.T) {
	o, base := newOrg(t)
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.putGroup(t, directory.Group{ID: "on-call"})
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering", "on-call"}})
	d := dial(t, base, okta.WithBuffer(0, 0))

	before := expand(t, d, "mei").Groups.Version
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
	after := expand(t, d, "mei").Groups.Version

	if before == after {
		t.Errorf("coming off the on-call rotation left the version at %d", before)
	}
}

// An organisation that refuses the token is not an organisation with nobody in
// it, and the difference matters: one is an operator with a job to do and the
// other is everybody silently losing every group they have.
func TestABadTokenIsAFailureRatherThanAnEmptyDirectory(t *testing.T) {
	o, base := newOrg(t)
	o.put(t, directory.Subject{ID: "mei"})

	d, err := okta.New(base, "wrong", okta.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	r, err := directory.New(d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Expand(t.Context(), "mei")
	if err == nil {
		t.Fatal("a refused token resolved")
	}
	if errors.Is(err, directory.ErrNoSubject) {
		t.Fatalf("a refused token looks like a person who is not there: %v", err)
	}
	// The operator has to be able to tell which of the two it is from the log.
	if !strings.Contains(err.Error(), "E0000011") {
		t.Errorf("the error is %q and does not carry the code to search for", err)
	}
}

func TestAGroupTheOrganisationDoesNotHoldIsRefusedAsSuch(t *testing.T) {
	_, base := newOrg(t)

	_, err := dial(t, base).Group(t.Context(), "00gnope")
	if !errors.Is(err, directory.ErrNoGroup) {
		t.Fatalf("an absent group gave %v, want ErrNoGroup", err)
	}
}

// Two organisations under one deployment is the acquisition shape, and the ids
// are opaque strings that do not carry which tenant they came from.
func TestTwoOrganisationsKeepTheirGroupsApart(t *testing.T) {
	left, leftBase := newOrg(t)
	right, rightBase := newOrg(t)
	for _, o := range []*org{left, right} {
		o.putGroup(t, directory.Group{ID: "00g1abcd"})
		o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"00g1abcd"}})
	}

	a := expand(t, dial(t, leftBase, okta.WithName("acme")), "mei")
	b := expand(t, dial(t, rightBase, okta.WithName("beta")), "mei")

	if slices.Equal(a.Groups.Members, b.Groups.Members) {
		t.Fatalf("the same id in two organisations is the same group: %s", describe(a.Groups.Members))
	}
	if want := []string{"acme:00g1abcd"}; !slices.Equal(a.Groups.Members, want) {
		t.Errorf("the first organisation gave %s, want %v", describe(a.Groups.Members), want)
	}
}

func TestNewRefusesAnOrganisationItCannotUse(t *testing.T) {
	tests := []struct {
		name       string
		org, token string
		opts       []okta.Option
		want       string
	}{
		{"no organisation", "", token, nil, "organisation URL is required"},
		{"no token", "https://acme.okta.com", "", nil, "API token is required"},
		{"a host and no scheme", "acme.okta.com", token, nil, "scheme and a host"},
		{"an empty name", "https://acme.okta.com", token, []okta.Option{okta.WithName("")}, "cannot be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := okta.New(tt.org, tt.token, tt.opts...)
			if err == nil {
				t.Fatal("New accepted it")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the error is %q and does not mention %q", err, tt.want)
			}
		})
	}
}

// An expansion looks a level up in parallel, so the adapter is used from
// several goroutines at once whether it wants to be or not.
func TestResolvingFromOneOrganisationAtOnceIsSafe(t *testing.T) {
	_, d := joins(t, 40)

	r, err := directory.New(d)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	done := make(chan error, 16)
	for range 16 {
		go func() {
			got, err := r.Expand(ctx, "mei")
			if err == nil && len(got.Groups.Members) != 40 {
				err = errors.New("resolved to " + strconv.Itoa(len(got.Groups.Members)) + " groups")
			}
			done <- err
		}()
	}
	for range 16 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}
