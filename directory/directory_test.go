package directory_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/directory"
	"github.com/tamnd/genba/directory/directorytest"
)

func TestConformance(t *testing.T) {
	directorytest.Run(t, func(t *testing.T) directorytest.Fixture {
		d := directory.NewStatic("acme")
		return directorytest.Fixture{
			Directory: d,
			Put:       func(_ *testing.T, s directory.Subject) { d.Put(s) },
			PutGroup:  func(_ *testing.T, g directory.Group) { d.PutGroup(g) },
			Drop:      func(_ *testing.T, group string) { d.RemoveGroup(group) },
		}
	})
}

// flaky is a directory that answers out of a static one and can be told to
// break, which is the half of the contract a conformance suite over a working
// directory cannot reach.
type flaky struct {
	*directory.Static

	mu     sync.Mutex
	broken map[string]error

	asked atomic.Int64
}

func newFlaky(name string) *flaky {
	return &flaky{Static: directory.NewStatic(name), broken: make(map[string]error)}
}

func (f *flaky) breakGroup(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broken[id] = err
}

func (f *flaky) Group(ctx context.Context, id string) (directory.Group, error) {
	f.asked.Add(1)

	f.mu.Lock()
	err, broken := f.broken[id]
	f.mu.Unlock()

	if broken {
		return directory.Group{}, err
	}
	return f.Static.Group(ctx, id)
}

var errDown = errors.New("the directory is having a bad afternoon")

// The whole reason this package refuses rather than returning what it has. An
// empty group set is a valid answer that means somebody is in no groups, and
// every layer above is built to trust it.
func TestADirectoryThatCannotAnswerFailsRatherThanResolvingToNothing(t *testing.T) {
	d := newFlaky("acme")
	d.PutGroup(directory.Group{ID: "engineering", MemberOf: []string{"everyone"}})
	d.PutGroup(directory.Group{ID: "everyone"})
	d.Put(directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
	d.breakGroup("everyone", errDown)

	r, err := directory.New(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Expand(t.Context(), "mei")
	if !errors.Is(err, errDown) {
		t.Fatalf("a directory that could not answer gave %v", err)
	}
	if len(got.Groups.Members) != 0 {
		t.Errorf("a failed expansion handed back %v", got.Groups.Members)
	}
	if got.Groups.Version != 0 {
		t.Errorf("a failed expansion was stamped with version %d", got.Groups.Version)
	}
}

// A failure in one lookup is the whole level thrown away, so the requests that
// have not started yet should not be made. On a directory that has just started
// refusing, the alternative is a sign in storm turning into a few hundred
// requests each against a service that is already unhappy.
func TestOneFailedLookupStopsTheRestOfItsLevel(t *testing.T) {
	d := newFlaky("acme")
	var in []string
	for i := range 200 {
		id := "g" + strconv.Itoa(i)
		d.PutGroup(directory.Group{ID: id})
		in = append(in, id)
	}
	d.Put(directory.Subject{ID: "mei", MemberOf: in})
	for _, id := range in {
		d.breakGroup(id, errDown)
	}

	// Four at a time, so at most four are past the point of no return when the
	// first one fails and the rest are never started.
	r, err := directory.New(d, directory.WithWidth(4))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Expand(t.Context(), "mei"); !errors.Is(err, errDown) {
		t.Fatalf("a broken lookup gave %v, want the directory's error", err)
	}

	if asked := d.asked.Load(); asked > 20 {
		t.Errorf("a level of %d that fails on the first entry still made %d requests", len(in), asked)
	}
}

// The width is a promise to the far side rather than a tuning knob. A directory
// is shared by every session that starts, so an expansion that goes as wide as
// it likes makes every other expansion slower.
func TestNoMoreLookupsRunAtOnceThanTheWidthAllows(t *testing.T) {
	d := newFlaky("acme")
	var in []string
	for i := range 64 {
		id := "g" + strconv.Itoa(i)
		d.PutGroup(directory.Group{ID: id})
		in = append(in, id)
	}
	d.Put(directory.Subject{ID: "mei", MemberOf: in})

	var (
		mu   sync.Mutex
		now  int
		peak int
	)
	counted := &counting{Directory: d, enter: func() {
		mu.Lock()
		now++
		if now > peak {
			peak = now
		}
		mu.Unlock()
	}, leave: func() {
		mu.Lock()
		now--
		mu.Unlock()
	}}

	r, err := directory.New(counted, directory.WithWidth(4))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Expand(t.Context(), "mei"); err != nil {
		t.Fatal(err)
	}
	if peak > 4 {
		t.Errorf("a width of 4 ran %d lookups at once", peak)
	}
}

type counting struct {
	directory.Directory
	enter func()
	leave func()
}

func (c *counting) Group(ctx context.Context, id string) (directory.Group, error) {
	c.enter()
	defer c.leave()
	return c.Directory.Group(ctx, id)
}

func TestTheCountersAddUp(t *testing.T) {
	d := newFlaky("acme")
	d.PutGroup(directory.Group{ID: "everyone"})
	d.PutGroup(directory.Group{ID: "engineering", MemberOf: []string{"everyone", "gone"}})
	d.Put(directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
	d.Put(directory.Subject{ID: "lee", Disabled: true})

	r, err := directory.New(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Expand(t.Context(), "mei"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Expand(t.Context(), "nobody"); !errors.Is(err, directory.ErrNoSubject) {
		t.Fatalf("expanding a subject that is not there gave %v", err)
	}
	if _, err := r.Expand(t.Context(), "lee"); !errors.Is(err, directory.ErrDisabled) {
		t.Fatalf("expanding a deactivated subject gave %v", err)
	}

	want := directory.Counters{
		Expansions: 1,
		Failures:   2,
		// mei, engineering, then everyone and gone together, plus the two
		// subject lookups that failed.
		Lookups:    6,
		Missing:    1,
		Disabled:   1,
		Unresolved: 1,
	}
	if got := r.Counters(); got != want {
		t.Errorf("the counters read %+v, want %+v", got, want)
	}
}

func TestApplyLeavesEverythingItIsNotAbout(t *testing.T) {
	d := directory.NewStatic("acme")
	d.PutGroup(directory.Group{ID: "engineering"})
	d.Put(directory.Subject{
		ID:         "mei",
		Identities: []acl.Identity{{Source: "slack", Value: "U04AB"}, {Source: "jira", Value: "acc-mei"}},
		MemberOf:   []string{"engineering"},
	})

	r, err := directory.New(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Expand(t.Context(), "mei")
	if err != nil {
		t.Fatal(err)
	}

	p := &acl.Principal{
		Tenant:     "acme",
		Subject:    "mei",
		Kind:       acl.KindUser,
		Roles:      []string{acl.RoleAdmin},
		Identities: []acl.Identity{{Source: "slack", Value: "U04AB"}},
	}
	got.Apply(p)

	if !slices.Equal(p.Roles, []string{acl.RoleAdmin}) {
		t.Errorf("applying an expansion changed the roles to %v", p.Roles)
	}
	if p.Tenant != "acme" || p.Subject != "mei" {
		t.Errorf("applying an expansion changed the principal to %s/%s", p.Tenant, p.Subject)
	}
	// The identity the principal already had is not added a second time, and
	// the one only the directory knew is.
	want := []acl.Identity{{Source: "slack", Value: "U04AB"}, {Source: "jira", Value: "acc-mei"}}
	if !slices.Equal(p.Identities, want) {
		t.Errorf("the principal carries the identities %v, want %v", p.Identities, want)
	}
	if !slices.Equal(p.Groups.Members, []string{"acme:engineering"}) {
		t.Errorf("the principal carries the groups %v", p.Groups.Members)
	}
}

// Two directories under different names are two sets of groups, because they
// are. A rule granting read to "engineering" at one company and the same name
// at an acquisition that has not been merged yet are different rules, and
// folding them together is the kind of mistake nobody finds by looking.
func TestGroupsFromTwoDirectoriesDoNotCollide(t *testing.T) {
	one := directory.NewStatic("acme")
	one.PutGroup(directory.Group{ID: "engineering"})
	one.Put(directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	other := directory.NewStatic("northwind")
	other.PutGroup(directory.Group{ID: "engineering"})
	other.Put(directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	first := expandWith(t, one, "mei")
	second := expandWith(t, other, "mei")

	if slices.Equal(first.Groups.Members, second.Groups.Members) {
		t.Fatalf("both directories resolved to %v", first.Groups.Members)
	}
	if first.Groups.Version == second.Groups.Version {
		t.Error("the same group name in two directories produced the same version")
	}

	p := &acl.Principal{Tenant: "acme", Subject: "mei", Kind: acl.KindUser}
	first.Apply(p)
	perm := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "northwind",
		AllowGroups: []acl.Ref{{Source: "northwind", Value: "engineering"}},
	}
	if perm.Allows(p) {
		t.Error("a rule about the other directory's engineering group allowed somebody from this one")
	}
}

// A directory's revision moving is enough on its own, without the membership
// changing, because a provider that keeps one is saying something we should
// believe.
func TestADirectoryRevisionMovesTheVersion(t *testing.T) {
	d := directory.NewStatic("acme")
	d.PutGroup(directory.Group{ID: "engineering", Version: "1"})
	d.Put(directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
	before := expandWith(t, d, "mei").Groups.Version

	d.PutGroup(directory.Group{ID: "engineering", Version: "2"})
	after := expandWith(t, d, "mei").Groups.Version

	if before == after {
		t.Fatalf("the group's revision moved and the version stayed at %d", before)
	}
}

func TestAResolverNeedsADirectoryWithAName(t *testing.T) {
	if _, err := directory.New(nil); err == nil {
		t.Error("a resolver over no directory was built without complaint")
	}
	if _, err := directory.New(directory.NewStatic("")); err == nil {
		t.Error("a resolver over an unnamed directory was built without complaint")
	}
}

func TestLoadIsOneStep(t *testing.T) {
	d := directory.NewStatic("acme")
	d.Put(directory.Subject{ID: "old"})
	d.PutGroup(directory.Group{ID: "retired"})

	d.Load(
		[]directory.Subject{{ID: "mei", MemberOf: []string{"engineering"}}, {ID: ""}},
		[]directory.Group{{ID: "engineering"}, {ID: ""}},
	)

	if _, err := d.Subject(t.Context(), "old"); !errors.Is(err, directory.ErrNoSubject) {
		t.Errorf("a subject from before the reload gave %v", err)
	}
	subs, groups := d.Snapshot()
	if len(subs) != 1 || subs[0].ID != "mei" {
		t.Errorf("the directory holds the subjects %v", subs)
	}
	if len(groups) != 1 || groups[0].ID != "engineering" {
		t.Errorf("the directory holds the groups %v", groups)
	}
}

// What a caller does with an answer must not reach back into what the next
// caller is given, which is easy to get wrong with a map of structs holding
// slices and impossible to notice until two sessions share a group.
func TestAnAnswerCannotBeWrittenBackIntoTheDirectory(t *testing.T) {
	d := directory.NewStatic("acme")
	d.Put(directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	got, err := d.Subject(t.Context(), "mei")
	if err != nil {
		t.Fatal(err)
	}
	got.MemberOf[0] = "administrators"

	again, err := d.Subject(t.Context(), "mei")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(again.MemberOf, []string{"engineering"}) {
		t.Fatalf("writing to one answer changed the directory to %v", again.MemberOf)
	}
}

func TestReadingAndWritingAtTheSameTimeIsSafe(t *testing.T) {
	d := directory.NewStatic("acme")
	d.PutGroup(directory.Group{ID: "engineering"})
	d.Put(directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	r, err := directory.New(d)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for n := range 50 {
				d.PutGroup(directory.Group{ID: "engineering", Version: fmt.Sprint(i, n)})
			}
		})
	}
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if _, err := r.Expand(context.Background(), "mei"); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
}

func expandWith(t *testing.T, d directory.Directory, id string) directory.Expansion {
	t.Helper()
	r, err := directory.New(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Expand(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
