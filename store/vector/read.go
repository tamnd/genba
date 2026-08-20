package vector

import (
	"fmt"
	"math"

	"github.com/tamnd/genba/store/column"
)

// Index is what a vector search runs against.
//
// It exists so that the choice of index is a property of the segment rather
// than a branch in the query path. [Open] reads the kind byte and returns the
// implementation that matches it, and a caller holding one of these has no way
// to tell an exact scan from an approximate index apart from asking it. Today
// there is one implementation, [Set], and the interface is here now because
// adding the second one later should not be a change to everything that calls
// it.
type Index interface {
	// Search returns the best k rows for a query, restricted to the rows a
	// principal may read.
	Search(q *Query, k int, allow *column.Bitmap) ([]Match, error)

	// Kind is the index this segment carries.
	Kind() Kind

	// Metric is how scores were computed.
	Metric() Metric

	// Dim is the width of the vectors.
	Dim() int

	// Rows is how many rows the section covers, including the ones with no
	// vector.
	Rows() int

	// Size is the length of the section in bytes.
	Size() int

	// Has reports whether a row carries a vector.
	Has(row int) bool

	// At returns a row's vector, dequantised.
	At(row int) ([]float32, bool)
}

var _ Index = (*Set)(nil)

// Set is the flat index: every vector of a segment, scanned.
//
// It holds the bytes it was opened over and never copies them. The scales and
// the codes are windows onto the section, so a segment of seventy five
// megabytes costs seventy five megabytes once however many readers have it
// open, and a memory mapped segment is searched without reading the parts of it
// the query never touches.
type Set struct {
	b      []byte
	kind   Kind
	metric Metric
	dim    int
	rows   int
	scales []byte
	codes  []byte
}

// Open parses a vector section and returns the index it holds.
//
// These bytes come off a disk, so nothing here is allocated from a length read
// out of them and every window is checked against the bytes actually on hand. A
// header claiming four billion rows fails a bounds check rather than reserving
// memory for them.
//
// The order of the checks is the same one the segment format uses. Version
// first, because a newer encoding could put anything at all in the bytes below
// and nothing after that line is worth doing until the layout is known. Then
// the fields that size the two arrays, then the arrays themselves.
func Open(b []byte) (Index, error) {
	if len(b) < headerSize {
		return nil, fmt.Errorf("%w: %d bytes is shorter than a header", ErrFormat, len(b))
	}
	if v := b[offVersion]; v != Version {
		return nil, fmt.Errorf("%w: version %d, this build reads %d", ErrVersion, v, Version)
	}
	// A flag this build does not know changes the meaning of the bytes below in
	// a way it cannot guess, so it is the same refusal as an unknown version
	// rather than something to skip politely.
	if f := b[offFlags]; f != 0 {
		return nil, fmt.Errorf("%w: flags %#02x are not known to this build", ErrVersion, f)
	}
	if r := le.Uint32(b[offReserved:]); r != 0 {
		return nil, fmt.Errorf("%w: reserved header bytes are not zero", ErrFormat)
	}

	metric := Metric(b[offMetric])
	if metric != MetricCosine {
		return nil, fmt.Errorf("%w: metric %s", ErrFormat, metric)
	}
	kind := Kind(b[offKind])
	if kind != KindFlat {
		return nil, fmt.Errorf("%w: %s", ErrKind, kind)
	}

	dim := uint64(le.Uint32(b[offDim:]))
	if dim == 0 || dim > MaxDim {
		return nil, fmt.Errorf("%w: %d dimensions, the limit is %d", ErrDim, dim, MaxDim)
	}
	rows := uint64(le.Uint32(b[offRows:]))

	// rows is a uint32 and dim has been bounded by MaxDim, so neither product
	// can overflow a uint64, which is why they are safe to compute before they
	// are compared against the file.
	want := uint64(headerSize) + rows*scaleSize + rows*dim
	if want != uint64(len(b)) {
		return nil, fmt.Errorf("%w: %d rows of %d dimensions need %d bytes and the section holds %d", ErrFormat, rows, dim, want, len(b))
	}

	end := headerSize + rows*scaleSize
	return &Set{
		b:      b,
		kind:   kind,
		metric: metric,
		dim:    int(dim),
		rows:   int(rows),
		scales: b[headerSize:end:end],
		codes:  b[end:],
	}, nil
}

// Kind is the index this section carries, which is always flat here and is on
// the interface so that a caller can report which one it got.
func (s *Set) Kind() Kind { return s.kind }

// Metric is how scores were computed.
func (s *Set) Metric() Metric { return s.metric }

// Dim is the width of the vectors.
func (s *Set) Dim() int { return s.dim }

// Rows is how many rows the section covers, including the ones with no vector.
func (s *Set) Rows() int { return s.rows }

// Size is the length of the section in bytes.
func (s *Set) Size() int { return len(s.b) }

// Has reports whether a row carries a vector. A row that does not has a scale
// of zero, which is a value a real vector cannot have, since a vector with no
// largest component is a vector of all zeros and those are refused at write.
func (s *Set) Has(row int) bool { return s.scaleAt(row) != 0 }

// At returns a row's vector, dequantised, and whether there was one.
//
// It allocates, and it is here for a caller that wants to look at a stored
// vector rather than search with it: a test, a diagnostic, or a re rank of a
// page of results against the full precision query. A scan never calls it.
func (s *Set) At(row int) ([]float32, bool) {
	scale := s.scaleAt(row)
	if scale == 0 {
		return nil, false
	}
	codes := s.codesAt(row)
	out := make([]float32, s.dim)
	for i, c := range codes {
		out[i] = scale * float32(int8(c))
	}
	return out, true
}

// scaleAt is a row's scale, and zero for a row outside the section. Out of
// range reads back as absent rather than as a panic, because the row numbers
// reaching here come from a bitmap the caller built and a search that is asked
// about a row that does not exist should return nothing rather than take the
// process down.
func (s *Set) scaleAt(row int) float32 {
	if row < 0 || row >= s.rows {
		return 0
	}
	return math.Float32frombits(le.Uint32(s.scales[row*scaleSize:]))
}

// codesAt is a row's codes, as the bytes they are stored in. The caller reads
// them as signed, which is a conversion rather than work.
func (s *Set) codesAt(row int) []byte {
	at := row * s.dim
	return s.codes[at : at+s.dim : at+s.dim]
}
