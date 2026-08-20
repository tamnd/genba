// Package vector stores embeddings in a segment section and searches them.
//
// One section, two arrays and a scan. Every vector is normalised and quantised
// to a byte a component with its own scale, the search vector is quantised the
// same way, and a score is an integer dot product turned back into a cosine at
// the end. A hundred thousand passages at 768 dimensions is seventy five
// megabytes, and searching them is one sequential pass over those bytes.
//
// # Why the flat scan comes first
//
// It is exact. The top k a flat scan returns is the true top k under the
// metric, so recall is not a number that has to be measured, it is one by
// construction. Every approximate index trades that away for speed, and the
// only honest way to decide whether a particular trade is worth making is to
// have the exact answer to compare against. So this is the thing a graph index
// has to beat to earn its complexity, and on a few hundred thousand vectors
// there is not much to beat.
//
// # The permission filter is the scan
//
// [Set.Search] is given the rows the principal may read and walks those rows.
// Not the whole segment with a filter afterwards, and not the whole segment
// with a check inside the loop. The allowed rows are what the loop is over, so
// a document the reader may not see is never scored, never reaches the heap and
// cannot be counted. That also makes a filtered search cheaper than an
// unfiltered one, which is the right way round: the more restricted the reader,
// the less work the machine does.
//
// # Quantisation
//
// Cosine is the metric, so a vector is normalised to unit length before
// anything else happens to it. Its components are then divided by its own
// largest magnitude and rounded to a signed byte, and that largest magnitude
// over 127 is the scale stored beside it.
//
// The scale is per vector rather than per section because how large the largest
// component of a unit vector is depends on how peaked that vector is. A section
// holding a sharply peaked embedding and a flat one would have to size its
// codes for the peaked one, and every code of the flat one would then spend
// most of its bits on leading zeros.
//
// The cost is a rounding error of at most half a scale per component, from each
// of the two vectors in a dot product. On 768 dimensional unit vectors that is
// a dot product error with a standard deviation near four ten thousandths,
// against scores that spread over about 0.036 for unrelated text. So the error
// is around one percent of the distance between two unrelated documents and
// far below the distance between a relevant one and an irrelevant one, and
// there is a test that fails if a clearly separated pair ever comes back in the
// wrong order.
//
// # Rows are the segment's rows
//
// A row here is the same row as in the segment's columns, so row 4000 is the
// same document in both and the intersection of a filter and a vector search is
// an intersection of row numbers. A document with no embedding still occupies
// its row, written as a null, because a section that skipped it would need a
// second numbering and a map between the two.
package vector

import (
	"encoding/binary"
	"errors"
	"strconv"
)

// Version is the encoding this package writes and the only one it reads. A
// reader that meets a version it does not know refuses the bytes rather than
// guessing at them, the same way the segment format and the column encoding do.
const Version uint8 = 1

// Metric is how two vectors are compared.
type Metric uint8

// The supported metrics. Cosine is the one embedding models are trained for,
// and for the unit vectors this package stores it is the dot product.
const (
	MetricCosine Metric = 1
)

func (m Metric) String() string {
	if m == MetricCosine {
		return "cosine"
	}
	return "metric(" + strconv.Itoa(int(m)) + ")"
}

// Kind names the index a section carries.
//
// It is a byte in the header rather than a decision in the code, which is what
// makes the choice of index a property of the data. A segment written with a
// graph index is read by a different implementation of [Index], chosen by
// [Open] from this byte, and nothing that calls [Open] knows the difference.
type Kind uint8

// The index kinds.
const (
	// KindAuto is not a kind a section can carry. It is what a builder is given
	// when the caller would rather the builder decided, and it is resolved to a
	// real kind before a byte is written.
	KindAuto Kind = 0

	// KindFlat is every vector, scanned. Exact, and the only kind this build
	// writes or reads.
	KindFlat Kind = 1
)

func (k Kind) String() string {
	switch k {
	case KindAuto:
		return "auto"
	case KindFlat:
		return "flat"
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// MaxDim is the widest vector this will store.
//
// It is above every embedding model in use and it is checked anyway, for two
// reasons. A dimension read out of a damaged file is a number nothing should be
// sized from, and the scan accumulates a dot product of two signed bytes in an
// int32, which holds 127 times 127 times a hundred and thirty thousand
// dimensions before it wraps. This is three orders of magnitude inside that.
const MaxDim = 4096

// The errors a caller can act on differently. They are separate values because
// they send somebody looking in different places: a version is a section
// written by a newer build, a format error is bytes that were damaged or were
// never a section, a dimension error is a query that does not belong to this
// segment, and a zero vector is an embedder that returned nothing.
var (
	ErrVersion = errors.New("vector: unknown version")
	ErrFormat  = errors.New("vector: malformed")
	ErrKind    = errors.New("vector: unknown index kind")
	ErrDim     = errors.New("vector: wrong number of dimensions")
	ErrZero    = errors.New("vector: a zero vector has no direction")
)

// The header. Fixed size, at the front, and the version is the first byte so a
// reader can refuse an unknown one before it has interpreted anything else.
//
//	0  1  version
//	1  1  metric
//	2  1  index kind
//	3  1  flags, reserved and zero, so a future bit can be a refusal too
//	4  4  dimensions
//	8  4  rows
//	12 4  reserved, zero
//
// Then the scales, one float32 per row, and then the codes, dim signed bytes
// per row. The scales come first so that the four byte values stay aligned
// whatever the dimension is, and so that a scan reading a scale and then a row
// of codes reads two runs that each go forwards.
const headerSize = 16

const (
	offVersion  = 0
	offMetric   = 1
	offKind     = 2
	offFlags    = 3
	offDim      = 4
	offRows     = 8
	offReserved = 12
)

// scaleSize is the width of one stored scale.
const scaleSize = 4

var le = binary.LittleEndian
