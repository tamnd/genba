// Package directorytest is the conformance suite every directory adapter has
// to pass.
//
// A directory adapter is a small amount of code in front of somebody else's
// API, and the amount of damage it can do is out of all proportion to its size.
// Every one of them is written by whoever needed that provider, usually against
// one tenant with a directory that is tidy, and the failure modes only appear
// against a directory that is not: a group that is a member of itself, a
// membership naming a group that was deleted last year, an account somebody
// deactivated on Friday, a person in four hundred groups.
//
// None of those is visible from above. A person who was quietly given nobody
// else's groups gets fewer search results and blames the search engine. A
// person who was quietly given somebody else's groups does not notice at all.
//
// So this suite is the definition of a directory rather than the interface
// being it. The interface says what compiles. This says what the answers have
// to mean.
//
// # What it tests
//
// Two things at once, and that is deliberate. The cases exercise [directory.Directory]
// directly for the answers only the adapter can give, and they exercise
// [directory.Resolver] over the same adapter for the properties that only
// appear once the graph is walked. An adapter is never used without the
// resolver, so testing it without one would leave the interesting half untested.
//
// # Usage
//
//	func TestConformance(t *testing.T) {
//		directorytest.Run(t, func(t *testing.T) directorytest.Fixture {
//			d := directory.NewStatic("acme")
//			return directorytest.Fixture{
//				Directory: d,
//				Put:       func(t *testing.T, s directory.Subject) { d.Put(s) },
//				PutGroup:  func(t *testing.T, g directory.Group) { d.PutGroup(g) },
//				Drop:      func(t *testing.T, group string) { d.RemoveGroup(group) },
//			}
//		})
//	}
package directorytest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/directory"
)

// Fixture is one directory under test, plus the handful of things the suite
// needs to be able to do to whatever is behind it.
//
// The suite never assumes it can write to a live provider. What it assumes is
// that the fixture can put a subject and a group into whatever the adapter
// reads, which for a real provider means a fake of it and for [directory.Static]
// means the type itself.
type Fixture struct {
	// Directory is the thing being tested.
	Directory directory.Directory

	// Put adds or replaces one subject.
	Put func(t *testing.T, s directory.Subject)

	// PutGroup adds or replaces one group.
	PutGroup func(t *testing.T, g directory.Group)

	// Drop removes a group while leaving the memberships that name it alone.
	// It is optional, and a fixture that cannot do it skips the case about an
	// inconsistent directory rather than failing it.
	Drop func(t *testing.T, group string)
}

// Factory builds a fresh fixture for one case. Every case gets its own, because
// a suite where one case can be affected by another is a suite that reports the
// wrong thing when it goes red.
type Factory func(t *testing.T) Fixture

// Run runs the whole suite against one adapter.
func Run(t *testing.T, newFixture Factory) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			switch {
			case f.Directory == nil:
				t.Fatal("the fixture has no directory in it")
			case f.Put == nil:
				t.Fatal("the fixture cannot put a subject in the directory, and every case starts by doing that")
			case f.PutGroup == nil:
				t.Fatal("the fixture cannot put a group in the directory")
			}
			c.run(t, f)
		})
	}
}

type testCase struct {
	name string
	run  func(t *testing.T, f Fixture)
}

var cases = []testCase{
	{"the directory names itself", testName},
	{"a subject resolves to the groups they are directly in", testDirect},
	{"a nested group brings the groups above it", testNested},
	{"a diamond in the graph is one answer and one lookup each", testDiamond},
	{"a cycle in the graph terminates", testCycle},
	{"a group that is a member of itself terminates", testSelfCycle},
	{"a subject in no groups resolves to no groups", testNoGroups},
	{"a subject the directory does not hold is refused", testMissingSubject},
	{"a deactivated subject is refused", testDisabled},
	{"a group named in a membership and not held is reported rather than fatal", testUnknownGroup},
	{"the version moves when a membership moves", testVersionMoves},
	{"the version is the same for the same answer", testVersionStable},
	{"the version does not move for a change that misses this subject", testVersionScoped},
	{"identities the directory knows reach the principal", testIdentities},
	{"a bound on the closure refuses rather than truncating", testBounded},
	{"a thousand groups resolve inside the budget", testWide},
	{"a cancelled expansion stops", testCancel},
}

func testName(t *testing.T, f Fixture) {
	if f.Directory.Name() == "" {
		t.Error("the directory has no name, so nothing can tell its groups from another directory's")
	}
}

func testDirect(t *testing.T, f Fixture) {
	f.PutGroup(t, directory.Group{ID: "engineering"})
	f.PutGroup(t, directory.Group{ID: "on-call"})
	f.Put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering", "on-call"}})

	got := expand(t, f, "mei")
	want := keys(f, "engineering", "on-call")
	if !slices.Equal(got.Groups.Members, want) {
		t.Errorf("mei resolved to %v, want %v", got.Groups.Members, want)
	}
}

func testNested(t *testing.T, f Fixture) {
	f.PutGroup(t, directory.Group{ID: "everyone"})
	f.PutGroup(t, directory.Group{ID: "engineering", MemberOf: []string{"everyone"}})
	f.PutGroup(t, directory.Group{ID: "storage", MemberOf: []string{"engineering"}})
	f.Put(t, directory.Subject{ID: "mei", MemberOf: []string{"storage"}})

	got := expand(t, f, "mei")
	want := keys(f, "engineering", "everyone", "storage")
	if !slices.Equal(got.Groups.Members, want) {
		t.Fatalf("a three level nest resolved to %v, want %v", got.Groups.Members, want)
	}
	if got.Depth != 3 {
		t.Errorf("a three level nest was walked %d levels deep", got.Depth)
	}
}

func testDiamond(t *testing.T, f Fixture) {
	// Two paths to the same group is the ordinary shape of a real directory
	// rather than a corner case, and looking the shared group up twice is how
	// an expansion over a few hundred groups turns into a few thousand
	// requests.
	f.PutGroup(t, directory.Group{ID: "everyone"})
	f.PutGroup(t, directory.Group{ID: "engineering", MemberOf: []string{"everyone"}})
	f.PutGroup(t, directory.Group{ID: "operations", MemberOf: []string{"everyone"}})
	f.Put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering", "operations"}})

	got := expand(t, f, "mei")
	want := keys(f, "engineering", "everyone", "operations")
	if !slices.Equal(got.Groups.Members, want) {
		t.Fatalf("a diamond resolved to %v, want %v", got.Groups.Members, want)
	}
	// The subject, the two groups below and the one they share.
	if got.Lookups != 4 {
		t.Errorf("a diamond cost %d lookups, want 4", got.Lookups)
	}
}

func testCycle(t *testing.T, f Fixture) {
	f.PutGroup(t, directory.Group{ID: "a", MemberOf: []string{"b"}})
	f.PutGroup(t, directory.Group{ID: "b", MemberOf: []string{"c"}})
	f.PutGroup(t, directory.Group{ID: "c", MemberOf: []string{"a"}})
	f.Put(t, directory.Subject{ID: "mei", MemberOf: []string{"a"}})

	got, err := within(t, f, "mei")
	if err != nil {
		t.Fatalf("a cycle in the group graph did not terminate: %v", err)
	}
	if want := keys(f, "a", "b", "c"); !slices.Equal(got.Groups.Members, want) {
		t.Errorf("a cycle resolved to %v, want %v", got.Groups.Members, want)
	}
}

func testSelfCycle(t *testing.T, f Fixture) {
	f.PutGroup(t, directory.Group{ID: "recursive", MemberOf: []string{"recursive"}})
	f.Put(t, directory.Subject{ID: "mei", MemberOf: []string{"recursive"}})

	got, err := within(t, f, "mei")
	if err != nil {
		t.Fatalf("a group that is a member of itself did not terminate: %v", err)
	}
	if want := keys(f, "recursive"); !slices.Equal(got.Groups.Members, want) {
		t.Errorf("a group inside itself resolved to %v, want %v", got.Groups.Members, want)
	}
}

func testNoGroups(t *testing.T, f Fixture) {
	f.Put(t, directory.Subject{ID: "mei"})

	got := expand(t, f, "mei")
	if len(got.Groups.Members) != 0 {
		t.Errorf("a subject in no groups resolved to %v", got.Groups.Members)
	}
	if got.Groups.Version == 0 {
		t.Error("a resolved subject was stamped with version zero, which is what an unresolved one looks like")
	}
}

func testMissingSubject(t *testing.T, f Fixture) {
	// Nothing is put in the directory at all, which is the point.
	r := resolver(t, f)
	_, err := r.Expand(t.Context(), "nobody")
	if !errors.Is(err, directory.ErrNoSubject) {
		t.Fatalf("a subject the directory does not hold gave %v, want ErrNoSubject", err)
	}
}

func testDisabled(t *testing.T, f Fixture) {
	f.PutGroup(t, directory.Group{ID: "engineering"})
	f.Put(t, directory.Subject{ID: "lee", MemberOf: []string{"engineering"}, Disabled: true})

	r := resolver(t, f)
	got, err := r.Expand(t.Context(), "lee")
	if !errors.Is(err, directory.ErrDisabled) {
		t.Fatalf("a deactivated subject gave %v, want ErrDisabled", err)
	}
	if len(got.Groups.Members) != 0 {
		t.Errorf("a refused expansion still handed back the groups %v", got.Groups.Members)
	}
}

func testUnknownGroup(t *testing.T, f Fixture) {
	if f.Drop == nil {
		t.Skip("the fixture cannot remove a group while leaving the memberships that name it")
	}
	f.PutGroup(t, directory.Group{ID: "engineering"})
	f.PutGroup(t, directory.Group{ID: "reorganised"})
	f.Put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering", "reorganised"}})
	f.Drop(t, "reorganised")

	got := expand(t, f, "mei")
	// The group stays. The directory said this person is in it, and taking it
	// away would take away access on the strength of a lookup that failed.
	if want := keys(f, "engineering", "reorganised"); !slices.Equal(got.Groups.Members, want) {
		t.Errorf("a membership naming a group the directory does not hold resolved to %v, want %v", got.Groups.Members, want)
	}
	if !slices.Equal(got.Unknown, []string{"reorganised"}) {
		t.Errorf("the unresolved group was reported as %v, want [reorganised]", got.Unknown)
	}
}

func testVersionMoves(t *testing.T, f Fixture) {
	f.PutGroup(t, directory.Group{ID: "engineering"})
	f.PutGroup(t, directory.Group{ID: "on-call"})
	f.Put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering", "on-call"}})
	before := expand(t, f, "mei").Groups.Version

	f.Put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
	after := expand(t, f, "mei").Groups.Version

	if before == after {
		t.Fatalf("taking somebody out of a group left the version at %d, so nothing derived from it is invalidated", before)
	}
}

func testVersionStable(t *testing.T, f Fixture) {
	f.PutGroup(t, directory.Group{ID: "everyone"})
	f.PutGroup(t, directory.Group{ID: "engineering", MemberOf: []string{"everyone"}})
	f.Put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	first := expand(t, f, "mei").Groups.Version
	second := expand(t, f, "mei").Groups.Version
	if first != second {
		t.Fatalf("the same membership resolved to versions %d and %d, so nothing derived from it is ever reused", first, second)
	}
}

func testVersionScoped(t *testing.T, f Fixture) {
	// A version that moved whenever anything in the directory moved would be
	// correct and useless: at a company with fifty thousand groups something
	// changes every second and nobody's cached state would survive.
	f.PutGroup(t, directory.Group{ID: "engineering"})
	f.PutGroup(t, directory.Group{ID: "facilities"})
	f.Put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})
	f.Put(t, directory.Subject{ID: "sam", MemberOf: []string{"facilities"}})

	before := expand(t, f, "mei").Groups.Version
	f.Put(t, directory.Subject{ID: "sam", MemberOf: []string{"facilities", "engineering"}})
	after := expand(t, f, "mei").Groups.Version

	if before != after {
		t.Errorf("somebody else joining a group moved mei's version from %d to %d", before, after)
	}
}

func testIdentities(t *testing.T, f Fixture) {
	f.PutGroup(t, directory.Group{ID: "engineering"})
	f.Put(t, directory.Subject{
		ID:         "mei",
		Identities: []acl.Identity{{Source: "slack", Value: "U04AB"}},
		MemberOf:   []string{"engineering"},
	})

	got := expand(t, f, "mei")
	if !slices.Contains(got.Subject.Identities, acl.Identity{Source: "slack", Value: "U04AB"}) {
		t.Fatalf("the identity the directory holds did not survive the expansion: %v", got.Subject.Identities)
	}

	// The whole point of an identity is that a rule written in one source's
	// vocabulary applies to somebody who signed in through another.
	p := &acl.Principal{Tenant: "acme", Subject: "mei", Kind: acl.KindUser}
	got.Apply(p)
	perm := acl.Permissions{
		Mode:       acl.ModeACL,
		Source:     "slack",
		AllowUsers: []acl.Ref{{Source: "slack", Value: "U04AB"}},
	}
	if !perm.Allows(p) {
		t.Error("a rule naming the subject by their Slack identity did not allow them")
	}
}

func testBounded(t *testing.T, f Fixture) {
	for i := range 40 {
		f.PutGroup(t, directory.Group{ID: "g" + strconv.Itoa(i)})
	}
	in := make([]string, 0, 40)
	for i := range 40 {
		in = append(in, "g"+strconv.Itoa(i))
	}
	f.Put(t, directory.Subject{ID: "mei", MemberOf: in})

	r, err := directory.New(f.Directory, directory.WithMaxGroups(10))
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Expand(t.Context(), "mei")
	if !errors.Is(err, directory.ErrTooManyGroups) {
		t.Fatalf("a closure past the bound gave %v, want ErrTooManyGroups", err)
	}
	// Refusing rather than truncating, because a truncated group set is not a
	// smaller answer, it is a wrong one, and nothing above here could tell.
	if len(got.Groups.Members) != 0 {
		t.Errorf("a refused expansion handed back %d groups anyway", len(got.Groups.Members))
	}
}

func testWide(t *testing.T, f Fixture) {
	if testing.Short() {
		t.Skip("the wide case is about latency and there is nothing to learn from it in a short run")
	}
	// The budget in the issue this was written for is a person in a thousand
	// groups. The number below is not a benchmark, it is a bound: an adapter
	// that walks the level serially against a fake that answers instantly still
	// passes, and one that is accidentally quadratic does not.
	const n = 1000
	in := make([]string, 0, n)
	for i := range n {
		id := "g" + strconv.Itoa(i)
		f.PutGroup(t, directory.Group{ID: id, MemberOf: []string{"everyone"}})
		in = append(in, id)
	}
	f.PutGroup(t, directory.Group{ID: "everyone"})
	f.Put(t, directory.Subject{ID: "mei", MemberOf: in})

	start := time.Now()
	got := expand(t, f, "mei")
	took := time.Since(start)

	if len(got.Groups.Members) != n+1 {
		t.Fatalf("a subject in %d groups resolved to %d", n, len(got.Groups.Members))
	}
	// The subject, the thousand groups, and the one they all point at.
	if got.Lookups != n+2 {
		t.Errorf("a subject in %d groups cost %d lookups, want %d", n, got.Lookups, n+2)
	}
	if took > 10*time.Second {
		t.Errorf("a subject in %d groups took %s", n, took.Round(time.Millisecond))
	}
	t.Logf("%d groups in %s over %d lookups", len(got.Groups.Members), took.Round(time.Millisecond), got.Lookups)
}

func testCancel(t *testing.T, f Fixture) {
	f.PutGroup(t, directory.Group{ID: "engineering"})
	f.Put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	r := resolver(t, f)
	if _, err := r.Expand(ctx, "mei"); err == nil {
		t.Error("an expansion under a cancelled context came back without an error")
	}
}

// resolver is the resolver every case runs through, with the defaults, because
// the defaults are what a deployment gets.
func resolver(t *testing.T, f Fixture) *directory.Resolver {
	t.Helper()
	r, err := directory.New(f.Directory)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// expand resolves one subject and fails the test if it could not.
func expand(t *testing.T, f Fixture, id string) directory.Expansion {
	t.Helper()
	got, err := resolver(t, f).Expand(t.Context(), id)
	if err != nil {
		t.Fatalf("expanding %q: %v", id, err)
	}
	return got
}

// within resolves one subject under a deadline, so that a walk which never
// terminates fails the case rather than hanging the whole run until the test
// binary is killed and nobody knows which case it was.
//
// The expansion itself runs on another goroutine, so nothing here may call
// [testing.T.Fatal] from inside it.
func within(t *testing.T, f Fixture, id string) (directory.Expansion, error) {
	t.Helper()

	type result struct {
		got directory.Expansion
		err error
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	done := make(chan result, 1)
	r := resolver(t, f)
	go func() {
		got, err := r.Expand(ctx, id)
		done <- result{got, err}
	}()
	select {
	case out := <-done:
		return out.got, out.err
	case <-ctx.Done():
		return directory.Expansion{}, ctx.Err()
	}
}

// keys is the group ids written the way a rule is compared against them, sorted
// the way an expansion returns them.
func keys(f Fixture, ids ...string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = fmt.Sprintf("%s:%s", f.Directory.Name(), id)
	}
	slices.Sort(out)
	return out
}
