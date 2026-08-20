package graph_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/tamnd/genba/store/graph"
)

// The four boxes on the issue, in order: entities and edges survive a round
// trip, a traversal never returns an entity the reader cannot see at any depth,
// a bounded traversal on a large graph has a documented worst case, and
// extraction is a connector concern.
//
// The last one is the absence of anything: there is no extractor in this
// package, no interface for one, and no import of doc or connector, which
// arch_test.go asserts.

func TestEntitiesAndEdgesSurviveARoundTrip(t *testing.T) {
	b := graph.NewBuilder(12)
	alice := mustEntity(t, b, "person:alice", "person", []int{3, 0, 7, 3})
	bob := mustEntity(t, b, "person:bob", "person", []int{0, 1})
	pay := mustEntity(t, b, "service:payments", "service", []int{7, 9})

	mustEdge(t, b, alice, bob, "reports to", []int{0})
	mustEdge(t, b, alice, pay, "owns", []int{7})
	mustEdge(t, b, bob, pay, "owns", []int{9})

	g := open(t, b)

	if got := g.Entities(); got != 3 {
		t.Errorf("Entities returned %d, want 3", got)
	}
	if got := g.Edges(); got != 3 {
		t.Errorf("Edges returned %d, want 3", got)
	}
	if got := g.Rows(); got != 12 {
		t.Errorf("Rows returned %d, want 12", got)
	}

	for _, want := range []struct{ key, typ string }{
		{"person:alice", "person"},
		{"person:bob", "person"},
		{"service:payments", "service"},
	} {
		at, ok := g.Find(want.key)
		if !ok {
			t.Fatalf("Find(%q) found nothing", want.key)
		}
		if got, _ := g.Key(at); got != want.key {
			t.Errorf("entity %d has key %q, want %q", at, got, want.key)
		}
		if got, _ := g.Type(at); got != want.typ {
			t.Errorf("entity %q has type %q, want %q", want.key, got, want.typ)
		}
	}

	// Sorted and deduplicated, which is what the builder promises.
	if got := g.Mentions(g.MustFind(t, "person:alice")); !slices.Equal(got, []int{0, 3, 7}) {
		t.Errorf("the mentions of alice came back as %v, want [0 3 7]", got)
	}

	if got := g.Kinds(); !slices.Equal(got, []string{"owns", "reports to"}) {
		t.Errorf("Kinds returned %v, want [owns, reports to]", got)
	}
	if got := g.Types(); !slices.Equal(got, []string{"person", "service"}) {
		t.Errorf("Types returned %v, want [person, service]", got)
	}

	// Every edge, read back through the adjacency rather than by index, which
	// is the path a traversal takes.
	type stated struct{ from, kind, to string }
	var got []stated
	for e := range g.Entities() {
		lo, hi := g.Out(e)
		for i := lo; i < hi; i++ {
			from, _ := g.Key(e)
			kind, _ := g.Kind(i)
			to, _ := g.Key(g.Target(i))
			got = append(got, stated{from, kind, to})
		}
	}
	want := []stated{
		{"person:alice", "owns", "service:payments"},
		{"person:alice", "reports to", "person:bob"},
		{"person:bob", "owns", "service:payments"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("the edges came back as %v, want %v", got, want)
	}

	if got := g.Evidence(1); !slices.Equal(got, []int{0}) {
		t.Errorf("the evidence for edge 1 is %v, want [0]", got)
	}
	t.Logf("%d entities and %d edges over %d documents in %d bytes", g.Entities(), g.Edges(), g.Rows(), g.Size())
}

// TestTheOrderOfTheSectionDoesNotDependOnTheOrderOfTheIngest is what makes two
// ingests of the same corpus comparable, and it is why the entities are stored
// in key order and the edges are sorted rather than kept as they arrived.
func TestTheOrderOfTheSectionDoesNotDependOnTheOrderOfTheIngest(t *testing.T) {
	keys := []string{"a", "c", "d"}
	edges := []struct{ src, dst, kind string }{
		{"a", "c", "knows"},
		{"a", "d", "knows"},
		{"c", "d", "manages"},
		{"a", "d", "manages"},
	}
	build := func(people, order []int) []byte {
		b := graph.NewBuilder(10)
		at := make(map[string]int, len(keys))
		for _, i := range people {
			at[keys[i]] = mustEntity(t, b, keys[i], "person", []int{i})
		}
		for _, i := range order {
			mustEdge(t, b, at[edges[i].src], at[edges[i].dst], edges[i].kind, []int{0})
		}
		raw, err := b.Build()
		if err != nil {
			t.Fatalf("building: %v", err)
		}
		return raw
	}
	first := build([]int{0, 1, 2}, []int{0, 1, 2, 3})
	second := build([]int{2, 0, 1}, []int{3, 2, 1, 0})
	if !slices.Equal(first, second) {
		t.Error("the same corpus declared in a different order produced different bytes")
	}
}

// TestTheEntitiesAreStoredInKeyOrder is the property [graph.Graph.Find] relies
// on, and a section that stopped holding it would still answer most lookups,
// which is exactly the kind of thing worth a test of its own.
func TestTheEntitiesAreStoredInKeyOrder(t *testing.T) {
	b := graph.NewBuilder(8)
	for i, key := range []string{"zoe", "alice", "mallory", "bob"} {
		mustEntity(t, b, key, "person", []int{i})
	}
	g := open(t, b)
	want := []string{"alice", "bob", "mallory", "zoe"}
	got := make([]string, g.Entities())
	for i := range got {
		got[i], _ = g.Key(i)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the section holds %v, want %v", got, want)
	}
	for at, key := range want {
		if got, ok := g.Find(key); !ok || got != at {
			t.Errorf("Find(%q) = %d, %v, want %d, true", key, got, ok, at)
		}
	}
	if _, ok := g.Find("nobody"); ok {
		t.Error("a key the section does not have was found")
	}
}

func TestAnEntityWithNoMentionIsRefused(t *testing.T) {
	b := graph.NewBuilder(4)
	if _, err := b.Entity("ghost", "person", nil); !errors.Is(err, graph.ErrMention) {
		t.Errorf("an entity with no mention returned %v, want ErrMention", err)
	}
	if got := b.Entities(); got != 0 {
		t.Errorf("the refused entity still added %d of them", got)
	}

	at := mustEntity(t, b, "real", "person", []int{0})
	if err := b.Edge(at, at, "knows", nil); !errors.Is(err, graph.ErrMention) {
		t.Errorf("an edge with no evidence returned %v, want ErrMention", err)
	}
	if got := b.Edges(); got != 0 {
		t.Errorf("the refused edge still added %d of them", got)
	}
}

func TestAKeyDeclaredTwiceIsRefused(t *testing.T) {
	b := graph.NewBuilder(4)
	mustEntity(t, b, "alice", "person", []int{0})
	if _, err := b.Entity("alice", "person", []int{1}); !errors.Is(err, graph.ErrEntity) {
		t.Errorf("a repeated key returned %v, want ErrEntity", err)
	}
	if _, err := b.Entity("", "person", []int{1}); !errors.Is(err, graph.ErrEntity) {
		t.Errorf("an empty key returned %v, want ErrEntity", err)
	}
}

// TestARowOutsideTheSegmentIsRefusedAtTheCallThatMadeIt is why the builder
// takes a row count. A mention of a document the segment does not have is a
// connector and a segment disagreeing about what was written, and the useful
// place to say so is the call, not a traversal months later that quietly
// returns nothing.
func TestARowOutsideTheSegmentIsRefusedAtTheCallThatMadeIt(t *testing.T) {
	b := graph.NewBuilder(5)
	if _, err := b.Entity("alice", "person", []int{2, 5}); !errors.Is(err, graph.ErrRow) {
		t.Errorf("a mention of row 5 in a segment of 5 rows returned %v, want ErrRow", err)
	}
	if _, err := b.Entity("bob", "person", []int{-1}); !errors.Is(err, graph.ErrRow) {
		t.Errorf("a mention of row -1 returned %v, want ErrRow", err)
	}

	at := mustEntity(t, b, "carol", "person", []int{0})
	if err := b.Edge(at, at, "knows", []int{9}); !errors.Is(err, graph.ErrRow) {
		t.Errorf("evidence from row 9 returned %v, want ErrRow", err)
	}
	if err := b.Edge(at, 7, "knows", []int{0}); !errors.Is(err, graph.ErrEntity) {
		t.Errorf("an edge to entity 7 of 1 returned %v, want ErrEntity", err)
	}
	if err := b.Edge(-1, at, "knows", []int{0}); !errors.Is(err, graph.ErrEntity) {
		t.Errorf("an edge from entity -1 returned %v, want ErrEntity", err)
	}
}

func TestAnEmptyGraphRoundTrips(t *testing.T) {
	g := open(t, graph.NewBuilder(0))
	if g.Entities() != 0 || g.Edges() != 0 {
		t.Fatalf("an empty section holds %d entities and %d edges", g.Entities(), g.Edges())
	}
	if _, ok := g.Find("anything"); ok {
		t.Error("Find found something in an empty section")
	}
	got, err := g.Traverse(&graph.Traversal{From: []string{"anything"}, Depth: 3}, nil)
	if err != nil {
		t.Fatalf("traversing an empty section: %v", err)
	}
	if len(got.Entities) != 0 {
		t.Errorf("an empty section returned %d entities", len(got.Entities))
	}
}

// TestOpenRefusesBytesThatAreNotASection covers the ways a section arrives
// wrong. These bytes come off a disk, and a reader that trusts a length field
// turns a bad sector into a dead process.
func TestOpenRefusesBytesThatAreNotASection(t *testing.T) {
	good := func() []byte {
		b := graph.NewBuilder(8)
		a := mustEntity(t, b, "a", "person", []int{0, 1})
		c := mustEntity(t, b, "c", "team", []int{2})
		mustEdge(t, b, a, c, "member of", []int{0})
		raw, err := b.Build()
		if err != nil {
			t.Fatalf("building: %v", err)
		}
		return raw
	}

	cases := []struct {
		name   string
		damage func(b []byte) []byte
		want   error
	}{
		{"empty", func([]byte) []byte { return nil }, graph.ErrFormat},
		{"a header and nothing else", func(b []byte) []byte { return b[:16] }, graph.ErrFormat},
		{"a version from the future", func(b []byte) []byte { b[0] = graph.Version + 1; return b }, graph.ErrVersion},
		{"a flag this build does not know", func(b []byte) []byte { b[1] = 1; return b }, graph.ErrVersion},
		{"reserved bytes that are not zero", func(b []byte) []byte { b[2] = 1; return b }, graph.ErrFormat},
		{"more entities than the limit", func(b []byte) []byte { put32(b, 4, 1<<30); return b }, graph.ErrFormat},
		{"more edges than the limit", func(b []byte) []byte { put32(b, 8, 1<<29); return b }, graph.ErrFormat},
		{"an entity count the parts do not agree with", func(b []byte) []byte { put32(b, 4, 1); return b }, graph.ErrFormat},
		{"an edge count the parts do not agree with", func(b []byte) []byte { put32(b, 8, 2); return b }, graph.ErrFormat},
		{"a part that starts somewhere else", func(b []byte) []byte { put32(b, 16, 99); return b }, graph.ErrFormat},
		{"a part that runs past the end", func(b []byte) []byte { put32(b, 20, 1<<20); return b }, graph.ErrFormat},
		{"a truncated copy", func(b []byte) []byte { return b[:len(b)-1] }, graph.ErrFormat},
		{"bytes after the end", func(b []byte) []byte { return append(b, 0) }, graph.ErrFormat},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := graph.Open(c.damage(good())); !errors.Is(err, c.want) {
				t.Errorf("Open returned %v, want %v", err, c.want)
			}
		})
	}
}

// TestOpenRefusesEveryDamagedByte is the blunt version of the table above. Every
// byte of a section is flipped in turn, and whatever comes back has to be an
// error or a section that answers questions without panicking. It is here
// because a table of cases only covers the damage somebody thought of.
func TestOpenRefusesEveryDamagedByte(t *testing.T) {
	b := graph.NewBuilder(8)
	a := mustEntity(t, b, "a", "person", []int{0, 1})
	c := mustEntity(t, b, "c", "team", []int{2, 5})
	mustEdge(t, b, a, c, "member of", []int{0})
	mustEdge(t, b, c, a, "has member", []int{1})
	raw, err := b.Build()
	if err != nil {
		t.Fatalf("building: %v", err)
	}

	opened := 0
	for i := range raw {
		for _, bit := range []byte{0x01, 0x80} {
			damaged := slices.Clone(raw)
			damaged[i] ^= bit
			g, err := graph.Open(damaged)
			if err != nil {
				continue
			}
			opened++
			// It parsed, so nothing it is asked may take the process down.
			for e := range g.Entities() + 2 {
				g.Key(e)
				g.Type(e)
				g.Mentions(e)
				g.Visible(e, nil)
				lo, hi := g.Out(e)
				for x := lo; x < hi && x < g.Edges(); x++ {
					g.Kind(x)
					g.Target(x)
					g.Evidence(x)
				}
			}
			if _, err := g.Traverse(&graph.Traversal{From: []string{"a", "c"}, Depth: graph.MaxDepth}, nil); err != nil {
				t.Fatalf("byte %d bit %#02x: traversing a section that opened: %v", i, bit, err)
			}
		}
	}
	t.Logf("%d of %d single bit flips still parsed, and none of them panicked", opened, len(raw)*2)
}

// FuzzOpen has one contract, which is that nothing it is fed ever panics.
func FuzzOpen(f *testing.F) {
	b := graph.NewBuilder(8)
	a := mustEntity(f, b, "a", "person", []int{0, 1})
	c := mustEntity(f, b, "c", "team", []int{2})
	mustEdge(f, b, a, c, "member of", []int{0})
	mustEdge(f, b, c, a, "has member", []int{1})
	raw, err := b.Build()
	if err != nil {
		f.Fatalf("building: %v", err)
	}
	f.Add(raw)
	f.Add([]byte{})
	f.Add(make([]byte, 16+7*8))

	f.Fuzz(func(t *testing.T, in []byte) {
		g, err := graph.Open(in)
		if err != nil {
			return
		}
		for e := range g.Entities() + 2 {
			g.Key(e)
			g.Type(e)
			g.Mentions(e)
			g.Visible(e, nil)
			g.Out(e)
		}
		for e := range g.Edges() + 2 {
			g.Kind(e)
			g.Evidence(e)
			if e < g.Edges() {
				g.Target(e)
			}
		}
		if _, err := g.Traverse(&graph.Traversal{From: []string{"a", "c"}, Kinds: []string{"member of"}, Depth: graph.MaxDepth}, nil); err != nil {
			t.Fatalf("traversing a section that opened: %v", err)
		}
	})
}

func put32(b []byte, at int, v uint32) {
	b[at] = byte(v)
	b[at+1] = byte(v >> 8)
	b[at+2] = byte(v >> 16)
	b[at+3] = byte(v >> 24)
}

func mustEntity(tb testing.TB, b *graph.Builder, key, typ string, mentions []int) int {
	tb.Helper()
	at, err := b.Entity(key, typ, mentions)
	if err != nil {
		tb.Fatalf("declaring %q: %v", key, err)
	}
	return at
}

func mustEdge(tb testing.TB, b *graph.Builder, src, dst int, kind string, evidence []int) {
	tb.Helper()
	if err := b.Edge(src, dst, kind, evidence); err != nil {
		tb.Fatalf("declaring a %q edge: %v", kind, err)
	}
}

// open builds and opens in one step, which is what every test that is not about
// the encoding wants.
func open(tb testing.TB, b *graph.Builder) *testGraph {
	tb.Helper()
	raw, err := b.Build()
	if err != nil {
		tb.Fatalf("building: %v", err)
	}
	g, err := graph.Open(raw)
	if err != nil {
		tb.Fatalf("opening: %v", err)
	}
	return &testGraph{Graph: g}
}

// testGraph is the section with the two lookups every test does spelled once.
type testGraph struct{ *graph.Graph }

func (g *testGraph) MustFind(tb testing.TB, key string) int {
	tb.Helper()
	at, ok := g.Find(key)
	if !ok {
		tb.Fatalf("the section has no entity %q", key)
	}
	return at
}

// keys turns a result into the entity keys it names, which is what the
// traversal tests assert on: a row number says nothing about whether the answer
// is right.
func (g *testGraph) keys(r *graph.Result) []string {
	out := make([]string, 0, len(r.Entities))
	for _, e := range r.Entities {
		key, ok := g.Key(e.Entity)
		if !ok {
			key = fmt.Sprintf("entity(%d)", e.Entity)
		}
		out = append(out, key)
	}
	return out
}
