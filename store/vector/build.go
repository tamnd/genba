package vector

import (
	"fmt"
	"math"
)

// Builder collects the vectors of a segment and encodes the section.
//
// Rows are appended in segment row order and every row gets one call, including
// the documents that have no embedding, which get [Builder.AppendNull]. The
// builder does not know what a document is and cannot tell a missing row from a
// caller that forgot one, so the rule is one call per row and the caller keeps
// it.
type Builder struct {
	dim    int
	kind   Kind
	scales []float32
	codes  []byte
}

// NewBuilder returns a builder for vectors of a given width.
//
// The kind is the index to write, and [KindAuto] leaves the choice to the
// builder, which is what a caller that has no opinion should pass. A caller
// with an opinion is usually an operator who has one, so the value comes from
// configuration rather than from a decision made here.
func NewBuilder(dim int, kind Kind) (*Builder, error) {
	if dim <= 0 || dim > MaxDim {
		return nil, fmt.Errorf("%w: %d dimensions, the limit is %d", ErrDim, dim, MaxDim)
	}
	switch kind {
	case KindAuto, KindFlat:
	default:
		return nil, fmt.Errorf("%w: %s", ErrKind, kind)
	}
	return &Builder{dim: dim, kind: kind}, nil
}

// Dim is the width of the vectors this builder takes.
func (b *Builder) Dim() int { return b.dim }

// Rows is how many rows have been appended.
func (b *Builder) Rows() int { return len(b.scales) }

// Append quantises a vector and adds it as the next row.
//
// The vector is normalised first, so the caller does not have to and two
// callers cannot disagree about whether it was done. A vector of all zeros is
// refused rather than stored as a null: it means the embedder returned nothing,
// and a pipeline that silently indexes that is a pipeline where a broken model
// looks like a corpus with no matches.
func (b *Builder) Append(v []float32) error {
	if len(v) != b.dim {
		return fmt.Errorf("%w: the vector has %d and the section has %d", ErrDim, len(v), b.dim)
	}
	scale, unit := quantiser(v)
	if scale == 0 {
		return ErrZero
	}
	b.scales = append(b.scales, float32(scale))

	// The row is grown in one step and then written by index. Appending a byte
	// at a time is a length check and a capacity check per component, and on a
	// corpus that is a few hundred million of each.
	at := len(b.codes)
	b.codes = append(b.codes, make([]byte, b.dim)...)
	row := b.codes[at:]
	for i, x := range v {
		row[i] = byte(round(float64(x) * unit))
	}
	return nil
}

// AppendNull adds a row with no vector.
//
// The row still exists and still costs its bytes, because row numbers are the
// segment's and a section that skipped a document would need a second numbering
// and a map between the two. A null row is never scored and never appears in a
// result, whatever the query.
func (b *Builder) AppendNull() {
	b.scales = append(b.scales, 0)
	b.codes = append(b.codes, make([]byte, b.dim)...)
}

// Build returns the encoded section.
func (b *Builder) Build() ([]byte, error) {
	kind, err := b.resolve()
	if err != nil {
		return nil, err
	}
	rows := b.Rows()
	out := make([]byte, headerSize+rows*scaleSize+rows*b.dim)
	out[offVersion] = Version
	out[offMetric] = byte(MetricCosine)
	out[offKind] = byte(kind)
	out[offFlags] = 0
	le.PutUint32(out[offDim:], uint32(b.dim))
	le.PutUint32(out[offRows:], uint32(rows))
	le.PutUint32(out[offReserved:], 0)

	at := headerSize
	for _, s := range b.scales {
		le.PutUint32(out[at:], math.Float32bits(s))
		at += scaleSize
	}
	copy(out[at:], b.codes)
	return out, nil
}

// resolve turns the requested kind into the one that gets written.
//
// It is a method rather than a function because the size threshold for a graph
// index belongs here, and the row count it would read is the builder's. That is
// the whole of what changes when there is a second index: a section built by a
// build that picks a different kind is read by a different implementation of
// [Index], and no caller of [NewBuilder] or [Open] is touched. Today every size
// resolves to flat, because a flat scan is the only index this build has and an
// exact answer at any size beats an approximate one that does not exist.
func (b *Builder) resolve() (Kind, error) {
	switch b.kind {
	case KindAuto, KindFlat:
		return KindFlat, nil
	}
	return 0, fmt.Errorf("%w: %s", ErrKind, b.kind)
}

// quantiser works out how a vector is encoded.
//
// It returns the scale to store beside the vector and the factor a raw
// component is multiplied by to become a code, which is one over the length
// times one over the scale. Both are returned rather than the caller doing the
// second division itself, because a division per component of every vector in a
// corpus is a real cost and because the query and the stored vectors have to be
// quantised by the same rule or the dot product between them means nothing.
//
// A zero scale means a vector of all zeros, which has no direction.
//
// The sums are in float64. A vector of a thousand small components loses
// precision in float32 before the square root ever sees it.
func quantiser(v []float32) (scale, unit float64) {
	sum, largest := float64(0), float64(0)
	for _, f := range v {
		x := float64(f)
		sum += x * x
		if a := math.Abs(x); a > largest {
			largest = a
		}
	}
	if sum == 0 {
		return 0, 0
	}
	// The scale is stated in terms of the normalised vector, since that is what
	// is being encoded, and it is the largest component of it over the 127
	// codes a signed byte has either side of zero.
	inv := 1 / math.Sqrt(sum)
	scale = largest * inv / 127
	return scale, inv / scale
}

// round puts one scaled component on the byte range.
//
// The clamp is not reachable from the arithmetic in [quantiser], where the
// scale is defined as the largest component over 127. It is here because that
// arithmetic is floating point and the cost of being wrong about it is a
// wrapped byte, which turns the largest component of a vector into the most
// negative one and the best match in a segment into the worst.
//
// The rounding is math.Round rather than the obvious add a half and let the
// conversion truncate, which needs the sign of the value and is therefore a
// branch on data that is half negative. That branch mispredicts every other
// component and costs three times what the library function does, which is
// branchless bit manipulation. BenchmarkAppend is three times faster this way,
// which is the opposite of what writing the arithmetic out by hand usually
// does and is why it was measured rather than assumed.
func round(c float64) int8 {
	switch {
	case c >= 127:
		return 127
	case c <= -127:
		return -127
	}
	return int8(math.Round(c))
}
