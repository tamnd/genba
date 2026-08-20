// Package graph is the entity and relationship section of a segment, and the
// traversal over it.
//
// Half of what people search for is a person, a team, a project or a customer,
// and the useful answer is usually one hop away from the document that matched.
// Who owns this service. Who else worked on that incident. Which customer the
// contract belongs to. A search index that can only return documents makes
// somebody answer those by reading the documents.
//
// # Visibility is derived, not stored
//
// An entity has no access control list of its own. It is visible to a reader
// when the reader can see a document that mentions it, and an edge is visible
// when the reader can see a document that says so.
//
// That is the whole reason the graph lives in the segment rather than beside
// it. A second permission model is a second thing to keep in step with the
// first, and the failure mode when they drift is a reader being shown a name
// they were never supposed to learn. Here there is nothing to drift: the
// permission bitmap that filters documents is the same bitmap that filters
// entities, and an entity nobody may read has no visible mention to be found
// through.
//
// It is deliberately conservative. A reader who can see that Alice reports to
// Bob, and that Bob reports to Carol, can work out something about Carol
// whether or not this package returns her. What the rule guarantees is the
// direction that matters: nothing is returned that is not already stated in a
// document the reader may read.
//
// # Columns and posting lists
//
// Entity keys and types are [column] sections, which is what makes looking an
// entity up by name a binary search over a sorted dictionary and looking one up
// by prefix a scan over codes rather than over text.
//
// Mentions and edge evidence are posting lists, ascending document rows as
// variable length deltas, because they are read to answer one question, whether
// any row in this list is visible, and that question is usually answered by the
// first entry.
//
// Edges are a compressed adjacency: sorted by source, with an index that gives
// each entity's run of them. Following an entity's edges is then a slice, which
// is what bounds the traversal.
//
// # The shape
//
//	header      16 bytes, fixed
//	directory   8 bytes per part, offset and length
//	parts       the part bytes, in the same order
//
// Row numbers in the mention and evidence lists are the segment's document
// rows, the same ones the columns and the vector section use, so a permission
// bitmap built once serves all of them.
package graph

import (
	"encoding/binary"
	"errors"
)

// Version is the format this build writes and the only one it reads. Like the
// segment around it, this is a refusal rather than a negotiation: a reader that
// parses an unknown version hopefully is a reader that will one day return an
// entity to somebody who should not have it.
const Version uint8 = 1

// The limits, which exist so that a header out of a damaged file is refused by
// a comparison rather than by an allocator.
const (
	// MaxEntities is more entities than a segment of a few hundred thousand
	// documents can plausibly name, by two orders of magnitude.
	MaxEntities = 1 << 26

	// MaxEdges is the same headroom on the relationships between them.
	MaxEdges = 1 << 28

	// MaxDepth bounds a traversal. Six hops is past the point where a result is
	// about the thing that was asked for, and a caller that wants the whole
	// component wants a different operation than this one.
	MaxDepth = 6
)

// The errors a caller can act on differently.
var (
	// ErrVersion is a section written by something newer, so go and find that
	// thing. It is separate from ErrFormat because an operator told
	// "malformed" about a file that is merely from a newer build goes looking
	// in the wrong place.
	ErrVersion = errors.New("graph: unknown version")

	// ErrFormat is bytes that were damaged or were never a graph section.
	ErrFormat = errors.New("graph: malformed")

	// ErrEntity is an entity row that does not exist, or a key that was
	// declared twice.
	ErrEntity = errors.New("graph: no such entity")

	// ErrRow is a document row outside the segment, which means the caller and
	// the segment disagree about how many documents there are.
	ErrRow = errors.New("graph: document row is outside the segment")

	// ErrMention is an entity with nothing to be found through. See
	// [Builder.Entity].
	ErrMention = errors.New("graph: an entity with no mention cannot be seen by anybody")

	// ErrDepth is a traversal asking for more hops than [MaxDepth].
	ErrDepth = errors.New("graph: depth is out of range")

	// ErrLimit is a limit that is not a positive number of entities.
	ErrLimit = errors.New("graph: limit is out of range")
)

// The header, byte for byte. Little endian throughout, for the same reason the
// segment format is.
//
//	 0  1  version
//	 1  1  flags, reserved and zero, so a future bit can be a refusal too
//	 2  2  reserved, zero
//	 4  4  entities
//	 8  4  edges
//	12  4  document rows the mention lists are numbered against
const headerSize = 16

const (
	offVersion  = 0
	offFlags    = 1
	offReserved = 2
	offEntities = 4
	offEdges    = 8
	offRows     = 12
)

// The parts, in the order they appear. The directory is a fixed length because
// the set of parts is fixed: a part that is not needed is present and empty,
// which costs eight bytes and removes the question of whether a missing part
// means absent or means damaged.
const (
	// partKeys is a string column of one stable key per entity, in entity row
	// order. The keys are what a caller names an entity by, and the column's
	// sorted dictionary is what makes that a binary search.
	partKeys = iota

	// partTypes is a string column of one type per entity, person or team or
	// project or whatever the extractor decided. This package does not know
	// the set and does not want to.
	partTypes

	// partMentions is one posting list per entity, the document rows that
	// mention it.
	partMentions

	// partAdjacency is entities+1 uint32 offsets into the edge arrays, so that
	// the edges out of entity n are the half open range [n, n+1).
	partAdjacency

	// partEdgeKinds is a string column of one kind per edge, in edge order.
	// The dictionary is what turns "follow only reports_to and owns" into two
	// integer comparisons.
	partEdgeKinds

	// partEdgeTargets is one uint32 per edge, the destination entity row. It is
	// a plain array rather than a column because it is read once per edge on
	// the one loop in this package that has to be fast, and a packed code is a
	// shift and a mask that buys nothing at four bytes.
	partEdgeTargets

	// partEvidence is one posting list per edge, the document rows that say the
	// edge exists.
	partEvidence

	partCount
)

const entrySize = 8

// dirSize is the whole directory, which is fixed because partCount is.
const dirSize = partCount * entrySize

var le = binary.LittleEndian
