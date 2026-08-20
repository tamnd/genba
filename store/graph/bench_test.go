package graph_test

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/tamnd/genba/store/column"
	"github.com/tamnd/genba/store/graph"
)

// The shape of a real corpus, roughly. A segment of a hundred thousand
// documents names some tens of thousands of distinct people, teams, projects
// and customers, most of them a handful of times and a few of them everywhere,
// and each one is in a few relationships.
var shapes = []struct {
	name           string
	rows, entities int
	degree         int
}{
	{"10k docs", 10_000, 2_000, 4},
	{"100k docs", 100_000, 20_000, 4},
	{"100k docs dense", 100_000, 20_000, 16},
	{"1m docs", 1_000_000, 200_000, 4},
}

// BenchmarkTraverse is the worst case the documentation claims: a traversal to
// the maximum depth over a graph large enough that the whole of it is reachable,
// so the walk visits every entity it can and reads every edge out of them.
//
// The number to watch is entities a second rather than nanoseconds an
// operation, because what a traversal costs is the size of the component it
// walks and not the depth it was asked for.
func BenchmarkTraverse(b *testing.B) {
	for _, s := range shapes {
		b.Run(s.name, func(b *testing.B) {
			g, seeds := corpus(b, s.rows, s.entities, s.degree)
			q := &graph.Traversal{From: seeds[:1], Depth: graph.MaxDepth}
			reached := 0
			b.ResetTimer()
			for b.Loop() {
				r, err := g.Traverse(q, nil)
				if err != nil {
					b.Fatalf("traversing: %v", err)
				}
				reached = len(r.Entities)
			}
			b.ReportMetric(float64(reached)*float64(b.N)/b.Elapsed().Seconds()/1e6, "Mentity/s")
			b.ReportMetric(float64(reached), "reached")
		})
	}
}

// BenchmarkTraverseFiltered is the shape a real question has, where the reader
// may see some fraction of the corpus.
//
// The interesting case is the last one. A reader who may see almost nothing is
// the expensive case rather than the cheap one, because deciding an entity is
// invisible means reading its whole mention list where deciding it is visible
// stops at the first row. That is the trade the design makes on purpose, and it
// is the right way round: the reader who is allowed the least is the one whose
// answer is smallest, so paying more per entity still costs less overall.
func BenchmarkTraverseFiltered(b *testing.B) {
	const rows, entities, degree = 100_000, 20_000, 4
	g, seeds := corpus(b, rows, entities, degree)

	for _, percent := range []int{100, 50, 10, 1} {
		b.Run(fmt.Sprintf("%d%%", percent), func(b *testing.B) {
			rnd := rand.New(rand.NewPCG(2121, 51))
			allow := column.NewBitmap(rows)
			for row := range rows {
				if rnd.IntN(100) < percent {
					allow.Set(row)
				}
			}
			// The seed's own documents are made readable whatever the fraction
			// is, because a traversal from a seed the reader cannot see is over
			// before it starts and measures nothing. What is being measured is
			// the walk under a filter, not the odds that a reader picked at
			// random may see the entity the question is about.
			at, ok := g.Find(seeds[0])
			if !ok {
				b.Fatal("a key the corpus has was not found")
			}
			for _, row := range g.Mentions(at) {
				allow.Set(row)
			}
			q := &graph.Traversal{From: seeds[:1], Depth: graph.MaxDepth}
			reached := 0
			b.ResetTimer()
			for b.Loop() {
				r, err := g.Traverse(q, allow)
				if err != nil {
					b.Fatalf("traversing: %v", err)
				}
				reached = len(r.Entities)
			}
			b.ReportMetric(float64(reached), "reached")
		})
	}
}

// BenchmarkTraverseLimited is what a search result actually asks for, which is
// a page of related entities rather than a component. The limit stops the walk,
// so this should be flat in the size of the graph, and it is the number that
// says whether the graph can be on the request path.
func BenchmarkTraverseLimited(b *testing.B) {
	for _, s := range shapes {
		b.Run(s.name, func(b *testing.B) {
			g, seeds := corpus(b, s.rows, s.entities, s.degree)
			q := &graph.Traversal{From: seeds[:1], Depth: graph.MaxDepth, Limit: 20}
			b.ResetTimer()
			for b.Loop() {
				if _, err := g.Traverse(q, nil); err != nil {
					b.Fatalf("traversing: %v", err)
				}
			}
		})
	}
}

// BenchmarkFind is looking an entity up by key, which every traversal starts
// with and which is a binary search over the column's sorted dictionary.
func BenchmarkFind(b *testing.B) {
	const rows, entities, degree = 100_000, 20_000, 4
	g, seeds := corpus(b, rows, entities, degree)
	i := 0
	for b.Loop() {
		if _, ok := g.Find(seeds[i%len(seeds)]); !ok {
			b.Fatal("a key the corpus has was not found")
		}
		i++
	}
}

// BenchmarkBuild is what an ingestion pipeline pays to encode the section,
// which includes sorting the entities into key order so that a reader does not
// have to.
func BenchmarkBuild(b *testing.B) {
	const rows, entities, degree = 100_000, 20_000, 4
	src := builder(b, rows, entities, degree)
	b.ResetTimer()
	for b.Loop() {
		if _, err := src.Build(); err != nil {
			b.Fatalf("building: %v", err)
		}
	}
	b.ReportMetric(float64(entities)*float64(b.N)/b.Elapsed().Seconds()/1e3, "kentity/s")
}

// corpus builds a section once per case and keeps it, because what is being
// measured is the walk and not the encoder.
func corpus(tb testing.TB, rows, entities, degree int) (g *graph.Graph, seeds []string) {
	tb.Helper()
	raw, err := builder(tb, rows, entities, degree).Build()
	if err != nil {
		tb.Fatalf("building: %v", err)
	}
	g, err = graph.Open(raw)
	if err != nil {
		tb.Fatalf("opening: %v", err)
	}
	seeds = make([]string, 0, 16)
	for i := range 16 {
		seeds = append(seeds, key(i*entities/16))
	}
	tb.Logf("%d entities and %d edges over %d documents in %d bytes", g.Entities(), g.Edges(), g.Rows(), g.Size())
	return g, seeds
}

func builder(tb testing.TB, rows, entities, degree int) *graph.Builder {
	tb.Helper()
	rnd := rand.New(rand.NewPCG(2121, 50))
	b := graph.NewBuilder(rows)
	for i := range entities {
		// A few mentions each, spread over the corpus, which is what makes the
		// visibility check a real one rather than a hit on the first row.
		mentions := make([]int, 0, 4)
		for range 4 {
			mentions = append(mentions, rnd.IntN(rows))
		}
		if _, err := b.Entity(key(i), "person", mentions); err != nil {
			tb.Fatalf("declaring entity %d: %v", i, err)
		}
	}
	kinds := []string{"reports to", "owns", "member of", "worked on"}
	for i := range entities {
		for j := range degree {
			if err := b.Edge(i, rnd.IntN(entities), kinds[j%len(kinds)], []int{rnd.IntN(rows)}); err != nil {
				tb.Fatalf("declaring an edge from %d: %v", i, err)
			}
		}
	}
	return b
}

// key is the stable name of an entity in the generated corpus, padded so that
// the dictionary order and the row order are the same, which keeps the
// benchmark measuring the search rather than an accident of naming.
func key(i int) string { return fmt.Sprintf("e%08d", i) }
