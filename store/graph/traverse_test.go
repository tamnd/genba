package graph_test

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tamnd/genba/store/column"
	"github.com/tamnd/genba/store/graph"
)

// org is a small organisation, written out so that every assertion below can be
// read against something concrete.
//
// The document rows are the point of the fixture. Every entity and every edge
// names the documents it is evidenced by, and those are what a reader is or is
// not allowed to see, so hiding a document is how a test hides a fact.
//
//	row  what it says
//	  0  alice reports to bob
//	  1  bob reports to carol
//	  2  carol reports to dave
//	  3  alice owns payments
//	  4  bob owns billing
//	  5  a page that names alice and nobody else
//	  6  a page that names dave and nobody else
//	  7  a page that names payments and nobody else
//	  8  a page that names billing and nobody else
//	  9  a page that names carol and nobody else
//	 10  a page that names bob and nobody else
func org(tb testing.TB) *testGraph {
	tb.Helper()
	b := graph.NewBuilder(11)
	alice := mustEntity(tb, b, "alice", "person", []int{0, 3, 5})
	bob := mustEntity(tb, b, "bob", "person", []int{0, 1, 4, 10})
	carol := mustEntity(tb, b, "carol", "person", []int{1, 2, 9})
	dave := mustEntity(tb, b, "dave", "person", []int{2, 6})
	payments := mustEntity(tb, b, "payments", "service", []int{3, 7})
	billing := mustEntity(tb, b, "billing", "service", []int{4, 8})

	mustEdge(tb, b, alice, bob, "reports to", []int{0})
	mustEdge(tb, b, bob, carol, "reports to", []int{1})
	mustEdge(tb, b, carol, dave, "reports to", []int{2})
	mustEdge(tb, b, alice, payments, "owns", []int{3})
	mustEdge(tb, b, bob, billing, "owns", []int{4})
	return open(tb, b)
}

// all is the bitmap of a reader who may see every document, which is not the
// same thing as a nil bitmap: nil is "no principal to apply" and this is "a
// principal who happens to be allowed everything". Both have to give the same
// answer, and a few tests check that they do.
func all(rows int) *column.Bitmap {
	b := column.NewBitmap(rows)
	b.SetRange(0, rows)
	return b
}

// hiding is every document except the ones named, which is how a test says
// "this reader may not read that".
func hiding(rows int, hidden ...int) *column.Bitmap {
	b := all(rows)
	for _, row := range hidden {
		b.Clear(row)
	}
	return b
}

func TestATraversalFollowsEdgesToTheDepthAsked(t *testing.T) {
	g := org(t)
	for depth, want := range map[int][]string{
		0: {"alice"},
		1: {"alice", "payments", "bob"},
		2: {"alice", "payments", "bob", "billing", "carol"},
		3: {"alice", "payments", "bob", "billing", "carol", "dave"},
		4: {"alice", "payments", "bob", "billing", "carol", "dave"},
	} {
		got := traverse(t, g, &graph.Traversal{From: []string{"alice"}, Depth: depth}, nil)
		if !slices.Equal(got, want) {
			t.Errorf("depth %d returned %v, want %v", depth, got, want)
		}
	}
}

// TestTraversalNeverReturnsAnEntityTheReaderCannotSee is the box on the issue.
//
// It is checked three ways, because there are three ways to get it wrong: the
// seed, the destination and the edge in between.
func TestTraversalNeverReturnsAnEntityTheReaderCannotSee(t *testing.T) {
	g := org(t)
	const rows = 11

	t.Run("a seed the reader cannot see is not a seed", func(t *testing.T) {
		// Alice is named in rows 0, 3 and 5. Hide all three and she is not
		// somebody this reader has ever heard of, so nothing is reachable
		// through her even though every other document is readable.
		got := traverse(t, g, &graph.Traversal{From: []string{"alice"}, Depth: 3}, hiding(rows, 0, 3, 5))
		if len(got) != 0 {
			t.Errorf("a reader who cannot see alice started from her anyway and got %v", got)
		}
	})

	t.Run("an entity the reader cannot see is not returned", func(t *testing.T) {
		// Billing is named in rows 4 and 8, and row 4 is also the document that
		// says bob owns it. Hiding row 8 alone leaves it visible through row 4,
		// so this first pins that the fixture works the way it is supposed to.
		allow := hiding(rows, 8)
		got := traverse(t, g, &graph.Traversal{From: []string{"alice"}, Depth: 3}, allow)
		if !slices.Contains(got, "billing") {
			t.Fatalf("billing is still visible through row 4 and did not come back: %v", got)
		}

		// Then hide row 4 as well. There is now no document this reader may
		// read that names billing at all, so it is not somebody they have heard
		// of, and the edge that led to it is gone with the same document.
		got = traverse(t, g, &graph.Traversal{From: []string{"alice"}, Depth: 3}, hiding(rows, 4, 8))
		if slices.Contains(got, "billing") {
			t.Errorf("a reader who can see no document naming billing got it anyway: %v", got)
		}
		// The rest of the graph is untouched, so this is not passing by
		// returning nothing.
		if !slices.Contains(got, "dave") {
			t.Errorf("hiding billing also lost dave, which is not what was asked: %v", got)
		}
	})

	t.Run("an edge the reader cannot see is not crossed", func(t *testing.T) {
		// Row 1 is the only document that says bob reports to carol. Hide it
		// and the reader can still see bob and can still see carol, through
		// rows 10 and 9 respectively, but must not learn that one reports to
		// the other, and must not reach dave through her.
		allow := hiding(rows, 1)
		if !g.Visible(g.MustFind(t, "carol"), allow) {
			t.Fatal("the fixture is wrong, carol should still be visible through row 9")
		}
		got := traverse(t, g, &graph.Traversal{From: []string{"alice"}, Depth: 4}, allow)
		want := []string{"alice", "payments", "bob", "billing"}
		if !slices.Equal(got, want) {
			t.Errorf("a reader who cannot read row 1 got %v, want %v", got, want)
		}
	})
}

// TestVisibilityIsCheckedAtEveryHopRatherThanAtTheEnd is the same box from the
// other side. A single hidden document in the middle of a chain has to stop
// everything behind it, however deep, which is what says the check is in the
// walk rather than a filter over the answer.
func TestVisibilityIsCheckedAtEveryHopRatherThanAtTheEnd(t *testing.T) {
	// A chain of forty entities, each linked to the next by an edge evidenced
	// by one document and each mentioned by one other.
	const n = 40
	b := graph.NewBuilder(2 * n)
	at := make([]int, n)
	for i := range n {
		at[i] = mustEntity(t, b, fmt.Sprintf("e%02d", i), "person", []int{n + i})
	}
	for i := range n - 1 {
		mustEdge(t, b, at[i], at[i+1], "next", []int{i})
	}
	g := open(t, b)

	for _, cut := range []int{0, 1, 17, n - 2} {
		got := traverse(t, g, &graph.Traversal{From: []string{"e00"}, Depth: graph.MaxDepth}, hiding(2*n, cut))
		want := min(cut+1, graph.MaxDepth+1)
		if len(got) != want {
			t.Errorf("hiding the document behind hop %d returned %d entities, want %d: %v", cut, len(got), want, got)
		}
	}
}

// TestAReaderWhoMaySeeNothingGetsNothing is the degenerate case, and it is
// worth its own test because it is the one a permission bug most often turns
// into everything.
func TestAReaderWhoMaySeeNothingGetsNothing(t *testing.T) {
	g := org(t)
	got := traverse(t, g, &graph.Traversal{From: []string{"alice", "bob", "carol"}, Depth: graph.MaxDepth}, column.NewBitmap(11))
	if len(got) != 0 {
		t.Errorf("a reader who may see no document got %v", got)
	}
}

// TestNoPrincipalAndAPrincipalWhoMaySeeEverythingAgree keeps the nil bitmap
// from drifting into a different code path with a different answer.
func TestNoPrincipalAndAPrincipalWhoMaySeeEverythingAgree(t *testing.T) {
	g := org(t)
	q := &graph.Traversal{From: []string{"alice"}, Depth: graph.MaxDepth}
	if a, b := traverse(t, g, q, nil), traverse(t, g, q, all(11)); !slices.Equal(a, b) {
		t.Errorf("no principal returned %v and a principal who may see everything returned %v", a, b)
	}
}

func TestOnlyTheKindsAskedForAreFollowed(t *testing.T) {
	g := org(t)
	for _, c := range []struct {
		kinds []string
		want  []string
	}{
		{nil, []string{"alice", "payments", "bob", "billing", "carol", "dave"}},
		{[]string{"reports to"}, []string{"alice", "bob", "carol", "dave"}},
		{[]string{"owns"}, []string{"alice", "payments"}},
		{[]string{"owns", "reports to"}, []string{"alice", "payments", "bob", "billing", "carol", "dave"}},
		{[]string{"married to"}, []string{"alice"}},
		{[]string{"married to", "owns"}, []string{"alice", "payments"}},
	} {
		got := traverse(t, g, &graph.Traversal{From: []string{"alice"}, Kinds: c.kinds, Depth: graph.MaxDepth}, nil)
		if !slices.Equal(got, c.want) {
			t.Errorf("following %v returned %v, want %v", c.kinds, got, c.want)
		}
	}
}

// TestTheLimitStopsTheWalkRatherThanTrimmingTheAnswer is what makes the limit a
// bound on work as well as on the result. It also pins what is kept, which is
// the entities nearest the seeds, because a limit that returned an arbitrary
// subset would be a limit nobody could use.
func TestTheLimitStopsTheWalkRatherThanTrimmingTheAnswer(t *testing.T) {
	g := org(t)
	full := traverse(t, g, &graph.Traversal{From: []string{"alice"}, Depth: graph.MaxDepth}, nil)
	for limit := 1; limit <= len(full)+1; limit++ {
		r, err := g.Traverse(&graph.Traversal{From: []string{"alice"}, Depth: graph.MaxDepth, Limit: limit}, nil)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		want := full[:min(limit, len(full))]
		if got := g.keys(r); !slices.Equal(got, want) {
			t.Errorf("limit %d returned %v, want %v", limit, got, want)
		}
		if truncated := limit < len(full); r.Truncated != truncated {
			t.Errorf("limit %d reported Truncated as %v, want %v", limit, r.Truncated, truncated)
		}
	}
}

// TestACycleIsNotASpecialCase is what the seen bitmap is for. Without it this
// test does not fail, it hangs.
func TestACycleIsNotASpecialCase(t *testing.T) {
	b := graph.NewBuilder(6)
	a := mustEntity(t, b, "a", "person", []int{0})
	c := mustEntity(t, b, "c", "person", []int{1})
	d := mustEntity(t, b, "d", "person", []int{2})
	mustEdge(t, b, a, c, "next", []int{3})
	mustEdge(t, b, c, d, "next", []int{4})
	mustEdge(t, b, d, a, "next", []int{5})
	g := open(t, b)

	got := traverse(t, g, &graph.Traversal{From: []string{"a"}, Depth: graph.MaxDepth}, nil)
	if want := []string{"a", "c", "d"}; !slices.Equal(got, want) {
		t.Errorf("a cycle returned %v, want %v", got, want)
	}
}

// TestAnEntityIsReturnedAtItsShortestDepth matters because a caller usually
// ranks by it. Breadth first is what makes it true, and a depth first walk
// would pass every other test in this file and fail this one.
func TestAnEntityIsReturnedAtItsShortestDepth(t *testing.T) {
	b := graph.NewBuilder(8)
	a := mustEntity(t, b, "a", "person", []int{0})
	c := mustEntity(t, b, "c", "person", []int{1})
	d := mustEntity(t, b, "d", "person", []int{2})
	e := mustEntity(t, b, "e", "person", []int{3})
	// a reaches e directly and also the long way round through c and d, and the
	// long way is declared first so that a walk that took edges in the order
	// they were given would find e at depth three.
	mustEdge(t, b, a, c, "z long", []int{4})
	mustEdge(t, b, c, d, "z long", []int{5})
	mustEdge(t, b, d, e, "z long", []int{6})
	mustEdge(t, b, a, e, "z long", []int{7})
	g := open(t, b)

	r, err := g.Traverse(&graph.Traversal{From: []string{"a"}, Depth: graph.MaxDepth}, nil)
	if err != nil {
		t.Fatalf("traversing: %v", err)
	}
	want := map[string]int{"a": 0, "c": 1, "e": 1, "d": 2}
	for _, reached := range r.Entities {
		key, _ := g.Key(reached.Entity)
		if got := reached.Depth; got != want[key] {
			t.Errorf("%s came back at depth %d, want %d", key, got, want[key])
		}
	}
	if len(r.Entities) != len(want) {
		t.Errorf("got %d entities, want %d", len(r.Entities), len(want))
	}
}

// TestTheHopsSayWhyAnEntityIsInTheResult is what makes a result explainable. A
// list of names with no edges behind them cannot be shown to anybody with a
// reason attached.
func TestTheHopsSayWhyAnEntityIsInTheResult(t *testing.T) {
	g := org(t)
	r, err := g.Traverse(&graph.Traversal{From: []string{"alice"}, Depth: 2}, nil)
	if err != nil {
		t.Fatalf("traversing: %v", err)
	}
	type stated struct{ from, kind, to string }
	var got []stated
	for _, h := range r.Edges {
		from, _ := g.Key(h.From)
		kind, _ := g.Kind(h.Edge)
		to, _ := g.Key(h.To)
		got = append(got, stated{from, kind, to})
		if len(g.Evidence(h.Edge)) == 0 {
			t.Errorf("the %s edge from %s has no evidence behind it", kind, from)
		}
	}
	want := []stated{
		{"alice", "owns", "payments"},
		{"alice", "reports to", "bob"},
		{"bob", "owns", "billing"},
		{"bob", "reports to", "carol"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("the hops came back as %v, want %v", got, want)
	}
}

// TestTheAnswerDoesNotDependOnTheOrderOfTheSeeds keeps two spellings of the
// same question the same question.
func TestTheAnswerDoesNotDependOnTheOrderOfTheSeeds(t *testing.T) {
	g := org(t)
	first := traverse(t, g, &graph.Traversal{From: []string{"alice", "carol", "payments"}, Depth: 2}, nil)
	second := traverse(t, g, &graph.Traversal{From: []string{"payments", "alice", "carol", "alice"}, Depth: 2}, nil)
	if !slices.Equal(first, second) {
		t.Errorf("two orderings of the same seeds returned %v and %v", first, second)
	}
}

func TestASeedTheSectionDoesNotHaveIsSkipped(t *testing.T) {
	g := org(t)
	got := traverse(t, g, &graph.Traversal{From: []string{"nobody", "alice", "also nobody"}, Depth: 1}, nil)
	if want := []string{"alice", "payments", "bob"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestATraversalOutOfRangeIsRefused(t *testing.T) {
	g := org(t)
	if _, err := g.Traverse(&graph.Traversal{From: []string{"alice"}, Depth: graph.MaxDepth + 1}, nil); !errors.Is(err, graph.ErrDepth) {
		t.Errorf("a traversal past MaxDepth returned %v, want ErrDepth", err)
	}
	if _, err := g.Traverse(&graph.Traversal{From: []string{"alice"}, Depth: -1}, nil); !errors.Is(err, graph.ErrDepth) {
		t.Errorf("a negative depth returned %v, want ErrDepth", err)
	}
	if _, err := g.Traverse(&graph.Traversal{From: []string{"alice"}, Limit: -1}, nil); !errors.Is(err, graph.ErrLimit) {
		t.Errorf("a negative limit returned %v, want ErrLimit", err)
	}
	if _, err := g.Traverse(nil, nil); err == nil {
		t.Error("a nil traversal was accepted")
	}
}

// TestAnEntityIsExpandedAtMostOnce is the documented worst case, checked
// against a graph with enough overlap that a walk which expanded an entity per
// path rather than per traversal would do far more work than one pass.
//
// It counts by hiding nothing and comparing the edges crossed against the edges
// that exist: every hop in the result is a distinct edge, and there can never
// be more of them than the section holds.
func TestAnEntityIsExpandedAtMostOnce(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 30))
	const entities, rows = 400, 2000
	b := graph.NewBuilder(rows)
	at := make([]int, entities)
	for i := range entities {
		at[i] = mustEntity(t, b, fmt.Sprintf("e%03d", i), "person", []int{i % rows})
	}
	edges := 0
	for i := range entities {
		for range 6 {
			mustEdge(t, b, at[i], at[rnd.IntN(entities)], "next", []int{rnd.IntN(rows)})
			edges++
		}
	}
	g := open(t, b)

	r, err := g.Traverse(&graph.Traversal{From: []string{"e000"}, Depth: graph.MaxDepth}, nil)
	if err != nil {
		t.Fatalf("traversing: %v", err)
	}
	if len(r.Entities) > g.Entities() {
		t.Fatalf("a traversal returned %d entities and the section holds %d", len(r.Entities), g.Entities())
	}
	seen := make(map[int]bool, len(r.Edges))
	for _, h := range r.Edges {
		if seen[h.Edge] {
			t.Fatalf("edge %d was crossed twice", h.Edge)
		}
		seen[h.Edge] = true
	}
	if len(r.Edges) > edges {
		t.Fatalf("a traversal crossed %d edges and the section holds %d", len(r.Edges), edges)
	}
	t.Logf("%d entities and %d hops out of %d entities and %d edges", len(r.Entities), len(r.Edges), g.Entities(), edges)
}

// traverse is Traverse with the error handled and the rows turned into keys.
func traverse(tb testing.TB, g *testGraph, q *graph.Traversal, allow *column.Bitmap) []string {
	tb.Helper()
	r, err := g.Traverse(q, allow)
	if err != nil {
		tb.Fatalf("traversing: %v", err)
	}
	return g.keys(r)
}
