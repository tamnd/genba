package directory_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/directory"
)

// The two companies. Mei was given an account on both sides during the
// integration, Ravi only exists at the acquirer, and Jo only exists at the
// company that was bought.
func acquirer(t *testing.T) *directory.Static {
	t.Helper()
	d := directory.NewStatic("acme")
	d.PutGroup(directory.Group{ID: "everyone"})
	d.PutGroup(directory.Group{ID: "engineering", MemberOf: []string{"everyone"}})
	d.Put(directory.Subject{
		ID:         "mei",
		Name:       "Mei",
		Email:      "mei@acme.com",
		Identities: []acl.Identity{{Source: "slack", Value: "U04AB"}},
		MemberOf:   []string{"engineering"},
	})
	d.Put(directory.Subject{ID: "ravi", MemberOf: []string{"everyone"}})
	return d
}

func acquired(t *testing.T) *directory.Static {
	t.Helper()
	d := directory.NewStatic("beta")
	d.PutGroup(directory.Group{ID: "payroll"})
	d.Put(directory.Subject{
		ID:         "mei",
		Name:       "Mei Tan",
		Email:      "mei@beta.example",
		Identities: []acl.Identity{{Source: "jira", Value: "5f1"}},
		MemberOf:   []string{"payroll"},
	})
	d.Put(directory.Subject{ID: "jo", MemberOf: []string{"payroll"}})
	return d
}

// union builds the thing under test over whatever directories a case wants.
func union(t *testing.T, dirs ...directory.Directory) *directory.Multi {
	t.Helper()
	parts := make([]directory.Expander, len(dirs))
	for i, d := range dirs {
		parts[i] = mustResolve(t, d)
	}
	m, err := directory.NewMulti(parts...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// both is the union the ordinary cases resolve against.
func both(t *testing.T) *directory.Multi {
	t.Helper()
	return union(t, acquirer(t), acquired(t))
}

func expandOne(t *testing.T, e directory.Expander, id string) directory.Expansion {
	t.Helper()
	got, err := e.Expand(t.Context(), id)
	if err != nil {
		t.Fatalf("expanding %s: %v", id, err)
	}
	return got
}

func TestSomebodyInBothDirectoriesCarriesTheGroupsOfBoth(t *testing.T) {
	got := expandOne(t, both(t), "mei")

	want := []string{"acme:engineering", "acme:everyone", "beta:payroll"}
	if !slices.Equal(got.Groups.Members, want) {
		t.Errorf("mei resolved to %v, want %v", got.Groups.Members, want)
	}
}

// Most people are in one directory of several, so a directory that has never
// heard of somebody is the ordinary case rather than something to report.
func TestSomebodyInOnlyOneDirectoryResolvesFine(t *testing.T) {
	m := both(t)

	for _, c := range []struct {
		id   string
		want []string
	}{
		{"ravi", []string{"acme:everyone"}},
		{"jo", []string{"beta:payroll"}},
	} {
		if got := expandOne(t, m, c.id).Groups.Members; !slices.Equal(got, c.want) {
			t.Errorf("%s resolved to %v, want %v", c.id, got, c.want)
		}
	}
}

func TestSomebodyInNoneOfThemIsStillNoSuchSubject(t *testing.T) {
	_, err := both(t).Expand(t.Context(), "nobody")
	if !errors.Is(err, directory.ErrNoSubject) {
		t.Errorf("the error is %v, want it to be ErrNoSubject", err)
	}
}

// The reason this type is written the way it is. A person whose second
// directory timed out looks exactly like a person who is only in the first one,
// and serving them half their groups takes away what they can read with nothing
// anywhere to say why.
func TestADirectoryThatCannotAnswerRefusesRatherThanReturningHalfTheGroups(t *testing.T) {
	down := &broken{Static: acquired(t)}
	down.down.Store(true)
	m := union(t, acquirer(t), down)

	_, err := m.Expand(t.Context(), "mei")
	if err == nil {
		t.Fatal("a union with one directory down answered anyway")
	}
	if !errors.Is(err, errDirectoryDown) {
		t.Errorf("the error is %v, want the directory's own", err)
	}
	// And the message names the directory that failed rather than the union,
	// because that is the one somebody has to go and look at.
	if !strings.Contains(err.Error(), "beta") {
		t.Errorf("the error is %q and does not name the directory that failed", err)
	}
}

// A directory that has been asked to stop speaking for somebody is a statement
// somebody made on purpose, and it holds even where another directory has not
// caught up yet.
func TestADeactivatedAccountInEitherDirectoryRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		in   func(*testing.T) *directory.Multi
	}{
		{"the first", func(t *testing.T) *directory.Multi {
			d := acquirer(t)
			d.Put(directory.Subject{ID: "mei", Disabled: true})
			return union(t, d, acquired(t))
		}},
		{"the second", func(t *testing.T) *directory.Multi {
			d := acquired(t)
			d.Put(directory.Subject{ID: "mei", Disabled: true})
			return union(t, acquirer(t), d)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.in(t).Expand(t.Context(), "mei")
			if !errors.Is(err, directory.ErrDisabled) {
				t.Errorf("the error is %v, want it to be ErrDisabled", err)
			}
		})
	}
}

// The version has to move when either side does, because the thing it
// invalidates is the whole session and not one directory's part of it.
func TestTheVersionMovesWhenEitherDirectoryDoes(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(a, b *directory.Static)
	}{
		{"the first", func(a, _ *directory.Static) {
			a.Put(directory.Subject{ID: "mei", MemberOf: []string{"everyone"}})
		}},
		{"the second", func(_, b *directory.Static) {
			b.Put(directory.Subject{ID: "mei"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, b := acquirer(t), acquired(t)
			m := union(t, a, b)

			was := expandOne(t, m, "mei").Groups.Version
			c.change(a, b)
			now := expandOne(t, m, "mei").Groups.Version
			if was == now {
				t.Errorf("the version is still %d after a membership changed", now)
			}
		})
	}
}

// And it does not move when nothing did, or every session would be thrown away
// on every request.
func TestTheVersionIsTheSameTwiceRunning(t *testing.T) {
	m := both(t)
	if was, now := expandOne(t, m, "mei").Groups.Version, expandOne(t, m, "mei").Groups.Version; was != now {
		t.Errorf("two expansions of an unchanged directory gave %d and %d", was, now)
	}
}

// A rule may name a Slack member id that only one of the two directories knows
// about, so taking the identities from the first one to answer would drop it.
func TestIdentitiesAreUnionedAcrossTheDirectories(t *testing.T) {
	got := expandOne(t, both(t), "mei")

	for _, want := range []acl.Identity{
		{Source: "slack", Value: "U04AB"},
		{Source: "jira", Value: "5f1"},
	} {
		if !slices.Contains(got.Subject.Identities, want) {
			t.Errorf("the identities are %v, want %v in them", got.Subject.Identities, want)
		}
	}
}

// Somebody has to win, and the first directory given is the one an operator can
// reorder.
func TestTheDisplayNameComesFromTheFirstDirectoryThatHasThem(t *testing.T) {
	if got := expandOne(t, both(t), "mei").Subject.Email; got != "mei@acme.com" {
		t.Errorf("the email is %q, want the first directory's", got)
	}
	if got := expandOne(t, union(t, acquired(t), acquirer(t)), "mei").Subject.Email; got != "mei@beta.example" {
		t.Errorf("the email is %q, want the first directory's", got)
	}
}

// MemberOf is a list of ids in one directory's own naming and there is no
// naming here that both of them share, so it is left empty rather than
// concatenated into something that reads like a single directory's answer.
func TestTheRawMembershipListIsNotConcatenated(t *testing.T) {
	if got := expandOne(t, both(t), "mei").Subject.MemberOf; len(got) != 0 {
		t.Errorf("the subject carries MemberOf %v, which belongs to no single directory", got)
	}
}

// Two providers can each be missing a group called the same thing, and an
// operator reading the list has to know which one to go and look at.
func TestAnUnknownGroupSaysWhichDirectoryItIsMissingFrom(t *testing.T) {
	a, b := acquirer(t), acquired(t)
	a.Put(directory.Subject{ID: "mei", MemberOf: []string{"engineering", "ghosts"}})
	b.Put(directory.Subject{ID: "mei", MemberOf: []string{"payroll", "ghosts"}})

	got := expandOne(t, union(t, a, b), "mei")
	if want := []string{"acme:ghosts", "beta:ghosts"}; !slices.Equal(got.Unknown, want) {
		t.Errorf("the unknown groups are %v, want %v", got.Unknown, want)
	}
	// They are still in the group set, because the directory saying somebody is
	// a member is a statement about them whatever else it has lost.
	for _, want := range []string{"acme:ghosts", "beta:ghosts"} {
		if !slices.Contains(got.Groups.Members, want) {
			t.Errorf("the groups are %v, want %v in them", got.Groups.Members, want)
		}
	}
}

// Two separate services over two separate networks, so asking them in turn
// makes a sign in cost the sum of two round trips for no reason.
func TestBothDirectoriesAreAskedAtTheSameTime(t *testing.T) {
	var (
		arrived = make(chan struct{}, 2)
		release = make(chan struct{})
	)
	slow := func(d *directory.Static) directory.Directory {
		return &gated{Static: d, arrived: arrived, release: release}
	}
	m, err := directory.NewMulti(
		mustResolve(t, slow(acquirer(t))),
		mustResolve(t, slow(acquired(t))),
	)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := m.Expand(t.Context(), "mei"); err != nil {
			t.Errorf("expanding mei: %v", err)
		}
	}()

	for i := range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of the two directories had been asked, so they are being asked in turn", i)
		}
	}
	close(release)
	<-done
}

// gated is a directory that says when it has been asked and then waits.
type gated struct {
	*directory.Static
	arrived chan<- struct{}
	release <-chan struct{}
}

func (g *gated) Subject(ctx context.Context, id string) (directory.Subject, error) {
	g.arrived <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return directory.Subject{}, ctx.Err()
	}
	return g.Static.Subject(ctx, id)
}

func TestTheCountersAreTheDirectoriesOwn(t *testing.T) {
	m := both(t)
	expandOne(t, m, "mei")

	got := m.Counters()
	if got.Expansions != 2 {
		t.Errorf("one sign in against two directories counted %d expansions, want 2", got.Expansions)
	}
	// Mei is looked up twice, engineering and everyone once each on one side and
	// payroll once on the other.
	if got.Lookups != 5 {
		t.Errorf("the union made %d lookups, want 5", got.Lookups)
	}
}

func TestACancelledRequestIsNotSentToAnyDirectory(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := both(t).Expand(ctx, "mei"); !errors.Is(err, context.Canceled) {
		t.Errorf("the error is %v, want it to be a cancellation", err)
	}
}

func TestAUnionNeedsDirectoriesThatCanBeToldApart(t *testing.T) {
	one := mustResolve(t, acquirer(t))
	for _, c := range []struct {
		name string
		in   []directory.Expander
		says string
	}{
		{"nothing at all", nil, "at least one"},
		{"a nil directory", []directory.Expander{one, nil}, "is nil"},
		{"a directory with no name", []directory.Expander{one, nameless{}}, "no name"},
		{"the same name twice", []directory.Expander{one, mustResolve(t, acquirer(t))}, `"acme"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := directory.NewMulti(c.in...)
			if err == nil {
				t.Fatal("the union was built")
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("the error is %q, and %q is not in it", err, c.says)
			}
		})
	}
}

// A union of one is a supported shape, because it is what a deployment that was
// given a list of directories with one entry in it ends up with.
func TestAUnionOfOneAnswersLikeTheDirectory(t *testing.T) {
	m := union(t, acquirer(t))
	if got := m.Name(); got != "acme" {
		t.Errorf("the union is named %q", got)
	}
	got := expandOne(t, m, "mei").Groups.Members
	if want := []string{"acme:engineering", "acme:everyone"}; !slices.Equal(got, want) {
		t.Errorf("mei resolved to %v, want %v", got, want)
	}
}

func TestTheNameSaysWhichDirectoriesAreInIt(t *testing.T) {
	m := both(t)
	if got := m.Name(); got != "acme+beta" {
		t.Errorf("the union is named %q, want acme+beta", got)
	}
	if want := []string{"acme", "beta"}; !slices.Equal(m.Directories(), want) {
		t.Errorf("the union holds %v, want %v", m.Directories(), want)
	}
}

// One cache over the union rather than one over each directory, which is the
// arrangement the documentation asks for and the one the daemon builds.
func TestOneCacheOverTheUnionHoldsOnePersonPerEntry(t *testing.T) {
	c, err := directory.NewCache(both(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Expand(t.Context(), "mei"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Expand(t.Context(), "mei"); err != nil {
		t.Fatal(err)
	}
	if got := c.Stats(); got.Entries != 1 || got.Hits != 1 {
		t.Errorf("the cache holds %d entries with %d hits, want one of each", got.Entries, got.Hits)
	}
	if got := c.Name(); got != "acme+beta" {
		t.Errorf("the cache is named %q", got)
	}
}

func TestTwoAnswersFromAUnionAreNotTheSameSlice(t *testing.T) {
	m := both(t)
	first := expandOne(t, m, "mei")
	first.Groups.Members[0] = "acme:administrators"

	if got := expandOne(t, m, "mei").Groups.Members[0]; got != "acme:engineering" {
		t.Errorf("an edit to one answer reached the next one, which now starts with %q", got)
	}
}

func TestResolvingFromSeveralDirectoriesAtOnceIsSafe(t *testing.T) {
	m := both(t)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			id := []string{"mei", "ravi", "jo"}[i%3]
			if _, err := m.Expand(t.Context(), id); err != nil {
				t.Errorf("expanding %s: %v", id, err)
			}
		})
	}
	wg.Wait()
}
