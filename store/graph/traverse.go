package graph

import (
	"fmt"
	"slices"

	"github.com/tamnd/genba/store/column"
)

// Traversal is a question about the graph.
type Traversal struct {
	// From is the entity keys to start from. A key the section does not have is
	// skipped rather than refused, because a caller asking the same question of
	// every segment in a corpus is asking about entities that mostly are not in
	// any one of them.
	From []string

	// Kinds is the edge kinds to follow, and empty means every kind. It is
	// resolved against the section's dictionary once, before the walk, so
	// filtering by kind costs an integer comparison per edge rather than a
	// string one.
	Kinds []string

	// Depth is how many hops to take. Zero returns the seeds and nothing else,
	// which is the cheap way to ask which of these entities this reader may
	// know about at all. The limit is [MaxDepth].
	Depth int

	// Limit bounds the entities in the result, and zero means no bound. It
	// stops the walk rather than trimming the answer, so it bounds the work as
	// well, and what it keeps is the entities nearest the seeds because the
	// walk is breadth first.
	Limit int
}

// Reached is one entity a traversal found.
type Reached struct {
	// Entity is the entity row, which is what [Graph.Key] and [Graph.Type] take.
	Entity int

	// Depth is how many hops from the nearest seed it was found at, so a seed
	// is zero.
	Depth int
}

// Hop is one edge a traversal crossed. Together these are why the entities in a
// result are there, which is what a caller needs to say "Alice, because she
// owns the service the document is about" rather than just "Alice".
type Hop struct {
	// From and To are entity rows, and Edge is the edge index, which is what
	// [Graph.Kind] and [Graph.Evidence] take.
	From, Edge, To int
}

// Result is what a traversal found.
type Result struct {
	// Entities is what was reached, nearest first: by depth, then by the order
	// the edges that led to them were crossed, which is the order the section
	// stores those edges in. The seeds come first, sorted by entity row.
	//
	// Every part of that is a property of the section rather than of the ingest
	// that wrote it or the order the caller listed the seeds in, so the same
	// question over the same segment gives the same answer in the same order.
	// It is what makes a limit mean something: what it keeps is the nearest
	// entities, not an arbitrary subset.
	Entities []Reached

	// Edges is the hops that were crossed, in the order they were crossed.
	Edges []Hop

	// Truncated says the limit stopped the walk, so there was more to find.
	Truncated bool
}

// Traverse walks the graph from a set of entities, following edges the reader
// may see to entities the reader may see.
//
// allow is the same permission bitmap that filters documents, over the same
// document rows, and it is applied at every hop rather than at the end. An edge
// whose evidence the reader cannot read is not crossed, so nothing behind it is
// reached through it, and an entity with no mention the reader can read is not
// returned even when an edge led to it. A traversal is therefore a walk over
// the subgraph the reader could have built by reading their own documents,
// which is the strongest thing that can be said about it and the only thing
// worth saying.
//
// A nil allow means every document, which is what a caller with no principal to
// apply passes. It is the only way to reach entities nobody vouched for, and it
// is deliberately the shape that has to be written out rather than the default
// that happens by omission.
//
// # What it costs
//
// An entity is expanded at most once, because a traversal keeps a bitmap of
// which ones it has reached, and expanding an entity walks its run of edges
// once. So however deep the traversal goes, it reads each edge at most once and
// each entity at most once, and the worst case is one pass over the section
// rather than anything that grows with depth. A cycle is not a special case,
// it is the same bitmap doing its job.
//
// The visibility checks are the part that is not constant. Deciding an edge or
// an entity is visible stops at the first document the reader may see, so the
// usual cost is one variable length integer. Deciding one is not visible reads
// its whole list, because that is what it takes to be sure. A traversal by a
// reader who may see nothing is therefore the expensive case rather than the
// cheap one, and its worst case is the size of the mention and evidence lists
// it touched.
func (g *Graph) Traverse(t *Traversal, allow *column.Bitmap) (*Result, error) {
	if t == nil {
		return nil, fmt.Errorf("%w: no traversal", ErrFormat)
	}
	if t.Depth < 0 || t.Depth > MaxDepth {
		return nil, fmt.Errorf("%w: %d hops, the limit is %d", ErrDepth, t.Depth, MaxDepth)
	}
	if t.Limit < 0 {
		return nil, fmt.Errorf("%w: %d", ErrLimit, t.Limit)
	}
	kinds, err := g.follow(t.Kinds)
	if err != nil {
		return nil, err
	}

	limit := t.Limit
	if limit == 0 || limit > g.entities {
		limit = g.entities
	}
	out := &Result{}
	if limit == 0 {
		return out, nil
	}

	seen := column.NewBitmap(g.entities)
	frontier := make([]int, 0, len(t.From))
	for _, key := range t.From {
		at, ok := g.Find(key)
		if !ok || seen.Get(at) || !g.Visible(at, allow) {
			continue
		}
		seen.Set(at)
		frontier = append(frontier, at)
	}
	// The seeds are sorted so that the result does not depend on the order the
	// caller happened to list them, which is what makes two spellings of the
	// same question the same question.
	slices.Sort(frontier)
	for _, at := range frontier {
		// Checked before the entity is added rather than after, so that a limit
		// reached exactly by the last thing there was to find is not reported as
		// having cut anything off. Truncated means there was more.
		if len(out.Entities) >= limit {
			out.Truncated = true
			return out, nil
		}
		out.Entities = append(out.Entities, Reached{Entity: at, Depth: 0})
	}

	next := make([]int, 0, len(frontier))
	for depth := 1; depth <= t.Depth && len(frontier) > 0; depth++ {
		next = next[:0]
		for _, from := range frontier {
			lo, hi := g.Out(from)
			for e := lo; e < hi; e++ {
				if !kinds.wanted(g, e) {
					continue
				}
				to := g.Target(e)
				if seen.Get(to) {
					continue
				}
				// The edge first, because it is the cheaper of the two
				// questions on a graph where entities are mentioned far more
				// often than any one relationship is stated.
				if !g.EdgeVisible(e, allow) || !g.Visible(to, allow) {
					continue
				}
				if len(out.Entities) >= limit {
					out.Truncated = true
					return out, nil
				}
				seen.Set(to)
				out.Edges = append(out.Edges, Hop{From: from, Edge: e, To: to})
				out.Entities = append(out.Entities, Reached{Entity: to, Depth: depth})
				next = append(next, to)
			}
		}
		// Ascending, so the next level is expanded in section order whatever
		// order this one discovered it in.
		slices.Sort(next)
		frontier = append(frontier[:0], next...)
	}
	return out, nil
}

// kindFilter is the set of edge kinds a traversal follows, resolved to codes.
type kindFilter struct {
	codes []uint64
	all   bool
}

// wanted reports whether an edge is one of the kinds being followed.
func (f kindFilter) wanted(g *Graph, edge int) bool {
	if f.all {
		return true
	}
	code, ok := g.kinds.CodeAt(edge)
	if !ok {
		return false
	}
	// The set is small enough that a scan beats a map: following one or two
	// kinds is the ordinary case, and a map lookup on the inner loop of a
	// traversal costs more than two comparisons.
	return slices.Contains(f.codes, code)
}

// follow resolves the kinds a traversal asked for against the section's
// dictionary.
//
// A kind the section does not use resolves to nothing and is dropped, and if
// none of them resolve the filter matches no edge, which is what makes asking
// for a relationship this segment has never seen return the seeds rather than
// everything.
func (g *Graph) follow(kinds []string) (kindFilter, error) {
	if len(kinds) == 0 {
		return kindFilter{all: true}, nil
	}
	m, err := g.kinds.MatchStrings(kinds...)
	if err != nil {
		return kindFilter{}, fmt.Errorf("the edge kinds: %w", err)
	}
	// MatchStrings answers in edge rows, and what is wanted is the codes. One
	// matching edge per kind is enough to learn the code, and the set is at most
	// the number of kinds asked for.
	out := kindFilter{codes: make([]uint64, 0, len(kinds))}
	m.Each(func(edge int) bool {
		if code, ok := g.kinds.CodeAt(edge); ok && !slices.Contains(out.codes, code) {
			out.codes = append(out.codes, code)
		}
		return len(out.codes) < len(kinds)
	})
	return out, nil
}
