//go:build !cgo || !kura

package kura

// This is the default build. Everything here returns [ErrUnavailable] and says
// which build to use.
//
// It is deliberately not a fallback to the pure Go implementations elsewhere in
// the repository. A caller that asked for the engine and quietly got something
// else has no way to find out, and "the fast path was not linked" is a fact an
// operator should be told once at startup rather than discover from a latency
// graph.

// Bitmap is the same shape as the linked one so that code compiles either way.
type Bitmap struct{}

// Available reports why the engine cannot be used, which in this build is that
// it was not linked in.
func Available() error { return ErrUnavailable }

// Version is empty, because there is no engine to ask.
func Version() string { return "" }

// NewBitmap returns [ErrUnavailable].
func NewBitmap() (*Bitmap, error) { return nil, ErrUnavailable }

// Close does nothing.
func (b *Bitmap) Close() {}

// Insert returns [ErrUnavailable].
func (b *Bitmap) Insert(uint32) error { return ErrUnavailable }

// Remove returns [ErrUnavailable].
func (b *Bitmap) Remove(uint32) error { return ErrUnavailable }

// Contains returns [ErrUnavailable].
func (b *Bitmap) Contains(uint32) (bool, error) { return false, ErrUnavailable }

// Len returns [ErrUnavailable].
func (b *Bitmap) Len() (int, error) { return 0, ErrUnavailable }

// Intersect returns [ErrUnavailable].
func (b *Bitmap) Intersect(*Bitmap) error { return ErrUnavailable }

// Union returns [ErrUnavailable].
func (b *Bitmap) Union(*Bitmap) error { return ErrUnavailable }

// Array returns [ErrUnavailable].
func (b *Bitmap) Array() ([]uint32, error) { return nil, ErrUnavailable }

// EncodePostings returns [ErrUnavailable].
func EncodePostings([]uint32) ([]byte, error) { return nil, ErrUnavailable }

// PostingsLen returns [ErrUnavailable].
func PostingsLen([]byte) (int, error) { return 0, ErrUnavailable }

// DecodePostings returns [ErrUnavailable].
func DecodePostings([]byte) ([]uint32, error) { return nil, ErrUnavailable }

// PostingsContains returns [ErrUnavailable].
func PostingsContains([]byte, uint32) (bool, error) { return false, ErrUnavailable }

// Cosine returns [ErrUnavailable].
func Cosine(_, _ []float32) (float32, error) { return 0, ErrUnavailable }

// Quantise returns [ErrUnavailable].
func Quantise([]float32) (codes []int8, scale float32, err error) { return nil, 0, ErrUnavailable }

// DotQuantised returns [ErrUnavailable].
func DotQuantised(_ []int8, _ float32, _ []int8, _ float32) (float32, error) {
	return 0, ErrUnavailable
}
