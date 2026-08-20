package graph

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tamnd/genba/store/column"
)

// Builder collects the entities and edges of a segment and encodes the section.
//
// Nothing here extracts anything. There is no name recogniser, no regular
// expression and no model, and there is no interface for one either, because an
// interface the store defines is still the store having an opinion about
// extraction. A connector knows what a person is in the system it reads, and it
// says so by calling [Builder.Entity] and [Builder.Edge]. This package's whole
// contribution is that whatever it is told survives a round trip and is never
// shown to somebody who may not see it.
type Builder struct {
	rows     int
	keys     map[string]int
	entities []entity
	edges    []edge
}

type entity struct {
	key      string
	typ      string
	mentions []int
}

type edge struct {
	src, dst int
	kind     string
	evidence []int
}

// NewBuilder returns a builder for a segment with a given number of documents.
//
// The row count is here rather than inferred from the mentions because it is
// what makes an out of range row an error at the call that made it. A builder
// that grew to fit whatever it was told would encode a graph pointing at
// documents the segment does not have, and that only becomes visible much
// later, in a traversal that silently returns nothing.
func NewBuilder(rows int) *Builder {
	return &Builder{rows: rows, keys: make(map[string]int)}
}

// Rows is how many documents the segment holds.
func (b *Builder) Rows() int { return b.rows }

// Entities is how many entities have been declared.
func (b *Builder) Entities() int { return len(b.entities) }

// Edges is how many edges have been declared.
func (b *Builder) Edges() int { return len(b.edges) }

// Entity declares an entity and returns the handle to use for it.
//
// The handle is what [Builder.Edge] takes, and it is a number in this builder
// rather than a row in the finished section, because [Builder.Build] stores the
// entities in key order and only knows that order once the last one is in. A
// caller that wants the row a section gave an entity asks the section, with
// [Graph.Find].
//
// The key is what the entity is called from outside, and it has to be stable
// across segments and unique within one, because it is what a traversal starts
// from and what two segments agree on when the same person is in both. What
// makes a good key is the connector's problem: an email address, an account id,
// a URL. This package only requires that it is not empty and not repeated.
//
// mentions is the document rows that name this entity. An entity with none is
// refused, because visibility here is derived from the documents and an entity
// with nothing to be found through is invisible to every reader, including the
// one who may see the whole segment. That is not a useful row, it is a bug in
// whatever produced it, and it should say so here rather than turn into a
// traversal that mysteriously stops.
//
// It is not a restriction in practice. A document that evidences an edge names
// both of its ends, so an entity reachable at all is an entity with a mention.
//
// The rows are copied, sorted and deduplicated. A caller that found them by
// scanning documents has them in the order it happened to scan, and sorting
// them is a kindness that costs a builder nothing and a caller a mistake.
func (b *Builder) Entity(key, typ string, mentions []int) (int, error) {
	if key == "" {
		return 0, fmt.Errorf("%w: an entity with no key", ErrEntity)
	}
	if at, ok := b.keys[key]; ok {
		return 0, fmt.Errorf("%w: %q is already entity %d", ErrEntity, key, at)
	}
	rows, err := b.postings(mentions)
	if err != nil {
		return 0, fmt.Errorf("the mentions of %q: %w", key, err)
	}
	if len(rows) == 0 {
		return 0, fmt.Errorf("%w: %q", ErrMention, key)
	}

	at := len(b.entities)
	if at >= MaxEntities {
		return 0, fmt.Errorf("%w: more than %d entities", ErrEntity, MaxEntities)
	}
	b.keys[key] = at
	b.entities = append(b.entities, entity{key: key, typ: typ, mentions: rows})
	return at, nil
}

// Lookup returns the handle of an entity already declared, which is what a
// caller building edges out of a stream of documents needs when the same name
// comes round again.
func (b *Builder) Lookup(key string) (int, bool) {
	at, ok := b.keys[key]
	return at, ok
}

// Edge declares a typed relationship between two entities.
//
// evidence is the document rows that say the relationship exists, and it plays
// the same part for an edge that mentions play for an entity: an edge nobody
// can see evidence for is an edge nobody is told about. It is required for the
// same reason, and refusing an edge with none is what makes the visibility rule
// total rather than a rule with a hole in it that a caller can fall through by
// omitting an argument.
//
// The direction is the caller's to define. This package stores an edge as
// given and follows it from source to destination, so a relationship worth
// walking both ways is two edges. Making that explicit is cheaper than a flag
// that every traversal has to reason about, and it lets the two directions
// carry different kinds, which "manages" and "reports to" usually should.
func (b *Builder) Edge(src, dst int, kind string, evidence []int) error {
	if src < 0 || src >= len(b.entities) {
		return fmt.Errorf("%w: source %d", ErrEntity, src)
	}
	if dst < 0 || dst >= len(b.entities) {
		return fmt.Errorf("%w: destination %d", ErrEntity, dst)
	}
	rows, err := b.postings(evidence)
	if err != nil {
		return fmt.Errorf("the evidence for %q: %w", kind, err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("%w: the %q edge from %q", ErrMention, kind, b.entities[src].key)
	}
	if len(b.edges) >= MaxEdges {
		return fmt.Errorf("%w: more than %d edges", ErrEntity, MaxEdges)
	}
	b.edges = append(b.edges, edge{src: src, dst: dst, kind: kind, evidence: rows})
	return nil
}

// postings validates and normalises a list of document rows.
func (b *Builder) postings(rows []int) ([]int, error) {
	out := slices.Clone(rows)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, row := range out {
		if row < 0 || row >= b.rows {
			return nil, fmt.Errorf("%w: row %d of %d", ErrRow, row, b.rows)
		}
	}
	return out, nil
}

// Build returns the encoded section.
func (b *Builder) Build() ([]byte, error) {
	// The entities are stored in key order rather than in the order they were
	// declared, which buys two things. Looking one up by key becomes a binary
	// search over the rows instead of a scan of them, and every traversal starts
	// with one of those. And the section stops depending on the order a
	// connector happened to walk the documents in, so two ingests of the same
	// corpus produce the same bytes.
	order := make([]int, len(b.entities))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(x, y int) int {
		return strings.Compare(b.entities[x].key, b.entities[y].key)
	})
	// row translates a declaration handle, which is what Entity returned and
	// what the edges were declared against, into the row the section stores.
	row := make([]int, len(b.entities))
	for at, i := range order {
		row[i] = at
	}

	// Sorted by source so that an entity's edges are one contiguous run, then
	// by kind and destination so that the order a traversal returns them in is
	// a property of the data rather than of the order they were declared in.
	//
	// Sorting by the kind string sorts by the kind code as well, because the
	// column dictionary is sorted and a code is a rank in it.
	edges := make([]edge, len(b.edges))
	for i, e := range b.edges {
		edges[i] = edge{src: row[e.src], dst: row[e.dst], kind: e.kind, evidence: e.evidence}
	}
	slices.SortStableFunc(edges, func(x, y edge) int {
		if x.src != y.src {
			return x.src - y.src
		}
		if c := strings.Compare(x.kind, y.kind); c != 0 {
			return c
		}
		return x.dst - y.dst
	})

	keys := column.NewBuilder(column.TypeString)
	types := column.NewBuilder(column.TypeString)
	mentions := make([][]int, len(b.entities))
	for at, i := range order {
		e := b.entities[i]
		keys.AppendString(e.key)
		types.AppendString(e.typ)
		mentions[at] = e.mentions
	}

	kinds := column.NewBuilder(column.TypeString)
	targets := make([]byte, 4*len(edges))
	evidence := make([][]int, len(edges))
	adjacency := make([]byte, 4*(len(b.entities)+1))
	at := 0
	for i, e := range edges {
		kinds.AppendString(e.kind)
		le.PutUint32(targets[i*4:], uint32(e.dst))
		evidence[i] = e.evidence
		// Every entity between the last source and this one has no edges, and
		// its slot is where this one's run begins, which is what makes an
		// entity with no edges a zero length range rather than a special case.
		for ; at <= e.src; at++ {
			le.PutUint32(adjacency[at*4:], uint32(i))
		}
	}
	for ; at <= len(b.entities); at++ {
		le.PutUint32(adjacency[at*4:], uint32(len(edges)))
	}

	parts := make([][]byte, partCount)
	var err error
	if parts[partKeys], err = keys.Build(); err != nil {
		return nil, fmt.Errorf("the entity keys: %w", err)
	}
	if parts[partTypes], err = types.Build(); err != nil {
		return nil, fmt.Errorf("the entity types: %w", err)
	}
	if parts[partEdgeKinds], err = kinds.Build(); err != nil {
		return nil, fmt.Errorf("the edge kinds: %w", err)
	}
	parts[partMentions] = encodePostings(mentions)
	parts[partAdjacency] = adjacency
	parts[partEdgeTargets] = targets
	parts[partEvidence] = encodePostings(evidence)

	size := headerSize + dirSize
	for _, p := range parts {
		size += len(p)
	}
	out := make([]byte, headerSize+dirSize, size)
	out[offVersion] = Version
	out[offFlags] = 0
	le.PutUint16(out[offReserved:], 0)
	le.PutUint32(out[offEntities:], uint32(len(b.entities)))
	le.PutUint32(out[offEdges:], uint32(len(edges)))
	le.PutUint32(out[offRows:], uint32(b.rows))
	for i, p := range parts {
		le.PutUint32(out[headerSize+i*entrySize:], uint32(len(out)))
		le.PutUint32(out[headerSize+i*entrySize+4:], uint32(len(p)))
		out = append(out, p...)
	}
	return out, nil
}
