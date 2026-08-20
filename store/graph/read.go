package graph

import (
	"fmt"
	"sort"

	"github.com/tamnd/genba/store/column"
)

// Graph is the entity and relationship section of a segment.
//
// It holds the bytes it was opened over and never copies them. Every part is a
// window onto the section, so a memory mapped segment is traversed without
// reading the parts a query never touches, and however many readers have the
// same segment open there is one copy of it.
type Graph struct {
	b         []byte
	rows      int
	keys      *column.Column
	types     *column.Column
	kinds     *column.Column
	mentions  *postings
	evidence  *postings
	adjacency []byte
	targets   []byte
	entities  int
	edges     int
}

// Open parses a graph section.
//
// These bytes come off a disk, so nothing here is allocated from a length read
// out of them, every part is checked against the bytes actually on hand, and a
// part that does not agree with the header about how many things it holds is a
// refusal rather than something to work around. A graph that pairs one entity
// with another entity's mentions would answer questions wrongly rather than
// fail, and a wrong answer here is a name shown to somebody who should not have
// it.
func Open(b []byte) (*Graph, error) {
	if len(b) < headerSize+dirSize {
		return nil, fmt.Errorf("%w: %d bytes is shorter than a header and a directory", ErrFormat, len(b))
	}
	if v := b[offVersion]; v != Version {
		return nil, fmt.Errorf("%w: version %d, this build reads %d", ErrVersion, v, Version)
	}
	// A flag this build does not know changes the meaning of the bytes below it
	// in a way it cannot guess, so it is the same refusal as an unknown version
	// rather than something to skip politely.
	if f := b[offFlags]; f != 0 {
		return nil, fmt.Errorf("%w: flags %#02x are not known to this build", ErrVersion, f)
	}
	if r := le.Uint16(b[offReserved:]); r != 0 {
		return nil, fmt.Errorf("%w: reserved header bytes are not zero", ErrFormat)
	}

	entities := uint64(le.Uint32(b[offEntities:]))
	if entities > MaxEntities {
		return nil, fmt.Errorf("%w: %d entities, the limit is %d", ErrFormat, entities, MaxEntities)
	}
	edges := uint64(le.Uint32(b[offEdges:]))
	if edges > MaxEdges {
		return nil, fmt.Errorf("%w: %d edges, the limit is %d", ErrFormat, edges, MaxEdges)
	}

	g := &Graph{b: b, rows: int(le.Uint32(b[offRows:])), entities: int(entities), edges: int(edges)}

	parts := make([][]byte, partCount)
	end := uint64(headerSize + dirSize)
	for i := range parts {
		at, length := uint64(le.Uint32(b[headerSize+i*entrySize:])), uint64(le.Uint32(b[headerSize+i*entrySize+4:]))
		// The parts are contiguous and in order, so a directory that says
		// anything else is damaged. Requiring it rather than tolerating it is
		// what makes the last check below, that they end exactly at the end of
		// the section, cover every byte between.
		if at != end {
			return nil, fmt.Errorf("%w: part %d starts at %d and the one before it ended at %d", ErrFormat, i, at, end)
		}
		if at+length > uint64(len(b)) {
			return nil, fmt.Errorf("%w: part %d runs to %d and the section holds %d bytes", ErrFormat, i, at+length, len(b))
		}
		parts[i] = b[at : at+length : at+length]
		end = at + length
	}
	if end != uint64(len(b)) {
		return nil, fmt.Errorf("%w: the parts end at %d and the section holds %d bytes", ErrFormat, end, len(b))
	}

	var err error
	if g.keys, err = g.column(parts[partKeys], g.entities, "entity keys"); err != nil {
		return nil, err
	}
	if g.types, err = g.column(parts[partTypes], g.entities, "entity types"); err != nil {
		return nil, err
	}
	if g.kinds, err = g.column(parts[partEdgeKinds], g.edges, "edge kinds"); err != nil {
		return nil, err
	}
	if g.mentions, err = openPostings(parts[partMentions], g.entities, "mention"); err != nil {
		return nil, err
	}
	if g.evidence, err = openPostings(parts[partEvidence], g.edges, "evidence"); err != nil {
		return nil, err
	}

	// entities and edges have both been bounded above, so these cannot
	// overflow and are safe to compute before comparing them to the file.
	if want := (entities + 1) * 4; uint64(len(parts[partAdjacency])) != want {
		return nil, fmt.Errorf("%w: %d entities need %d bytes of adjacency and the part holds %d", ErrFormat, entities, want, len(parts[partAdjacency]))
	}
	if want := edges * 4; uint64(len(parts[partEdgeTargets])) != want {
		return nil, fmt.Errorf("%w: %d edges need %d bytes of targets and the part holds %d", ErrFormat, edges, want, len(parts[partEdgeTargets]))
	}
	g.adjacency, g.targets = parts[partAdjacency], parts[partEdgeTargets]

	// The adjacency is read on the traversal loop, so it is checked once here
	// rather than on every hop. It has to ascend, start at zero and end at the
	// edge count, which is what makes every entity's range a valid slice.
	last := uint32(0)
	for i := range g.entities + 1 {
		at := le.Uint32(g.adjacency[i*4:])
		if at < last {
			return nil, fmt.Errorf("%w: the adjacency goes backwards at entity %d", ErrFormat, i)
		}
		last = at
	}
	if uint64(last) != edges {
		return nil, fmt.Errorf("%w: the adjacency ends at %d and the section holds %d edges", ErrFormat, last, edges)
	}

	// A target is read once per edge on the traversal loop and is used to index
	// the adjacency, so it is bounds checked here rather than there.
	for i := range g.edges {
		if dst := le.Uint32(g.targets[i*4:]); uint64(dst) >= entities {
			return nil, fmt.Errorf("%w: edge %d points at entity %d and the section holds %d", ErrFormat, i, dst, entities)
		}
	}
	return g, nil
}

// column opens one of the three column parts and checks it covers the rows the
// header claims.
func (g *Graph) column(b []byte, want int, what string) (*column.Column, error) {
	c, err := column.Open(b)
	if err != nil {
		return nil, fmt.Errorf("the %s: %w", what, err)
	}
	if c.Rows() != want {
		return nil, fmt.Errorf("%w: the %s cover %d rows and the section holds %d", ErrFormat, what, c.Rows(), want)
	}
	return c, nil
}

// Rows is how many documents the segment holds, which is what the mention and
// evidence lists are numbered against.
func (g *Graph) Rows() int { return g.rows }

// Entities is how many entities the section names.
func (g *Graph) Entities() int { return g.entities }

// Edges is how many relationships it holds.
func (g *Graph) Edges() int { return g.edges }

// Size is the length of the section in bytes.
func (g *Graph) Size() int { return len(g.b) }

// Key is an entity's stable key.
func (g *Graph) Key(entity int) (string, bool) { return g.keys.StringAt(entity) }

// Type is an entity's type, whatever the connector called it.
func (g *Graph) Type(entity int) (string, bool) { return g.types.StringAt(entity) }

// Kind is an edge's kind.
func (g *Graph) Kind(edge int) (string, bool) { return g.kinds.StringAt(edge) }

// Kinds is every edge kind in the section, sorted. It is the vocabulary the
// segment actually uses, which is what a caller offering somebody a choice of
// relationships to follow needs, and it is the column's dictionary rather than
// a scan.
func (g *Graph) Kinds() []string { return g.kinds.Dict() }

// Types is every entity type in the section, sorted, for the same reason.
func (g *Graph) Types() []string { return g.types.Dict() }

// Find returns the row of the entity with a key, if the section has one.
//
// The entities are stored in key order, so this is a binary search over the
// rows and costs a handful of comparisons on a segment with a hundred thousand
// entities in it. That matters because every traversal starts with one of these
// per seed, and a scan there would cost more than the walk.
func (g *Graph) Find(key string) (int, bool) {
	at := sort.Search(g.entities, func(i int) bool {
		k, _ := g.keys.StringAt(i)
		return k >= key
	})
	if at == g.entities {
		return 0, false
	}
	if k, ok := g.keys.StringAt(at); !ok || k != key {
		return 0, false
	}
	return at, true
}

// Mentions is the document rows that name an entity, unfiltered.
//
// It is the evidence for why an entity was returned rather than part of
// deciding whether it should be, so a caller showing it to somebody has to
// intersect it with what that reader may see. [Graph.Visible] is the question
// the traversal asks, and it is the one to ask when the answer matters.
func (g *Graph) Mentions(entity int) []int { return g.mentions.rows(entity) }

// Evidence is the document rows that say an edge exists, unfiltered, with the
// same caveat as [Graph.Mentions].
func (g *Graph) Evidence(edge int) []int { return g.evidence.rows(edge) }

// Visible reports whether a reader may know an entity exists, which is true
// when they may read a document that mentions it.
//
// A nil allow means every document, which is what a caller with no principal to
// apply passes.
func (g *Graph) Visible(entity int, allow *column.Bitmap) bool {
	return g.mentions.visible(entity, allow)
}

// EdgeVisible reports whether a reader may know a relationship exists, which is
// true when they may read a document that says so.
func (g *Graph) EdgeVisible(edge int, allow *column.Bitmap) bool {
	return g.evidence.visible(edge, allow)
}

// Out is the range of edges leaving an entity, as a half open pair of edge
// indexes. An entity with no edges is an empty range rather than a special
// case.
func (g *Graph) Out(entity int) (lo, hi int) {
	if entity < 0 || entity >= g.entities {
		return 0, 0
	}
	return int(le.Uint32(g.adjacency[entity*4:])), int(le.Uint32(g.adjacency[(entity+1)*4:]))
}

// Target is the entity an edge points at. Open has already checked every one of
// these against the entity count, which is why the traversal can use it as an
// index without checking again.
func (g *Graph) Target(edge int) int { return int(le.Uint32(g.targets[edge*4:])) }
