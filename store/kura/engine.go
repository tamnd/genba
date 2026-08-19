//go:build cgo && kura

package kura

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/kura/include
#cgo LDFLAGS: -L${SRCDIR}/../../third_party/kura/lib -lkura
#cgo linux LDFLAGS: -lm -ldl -lpthread
#cgo windows LDFLAGS: -lws2_32 -luserenv -lntdll -lbcrypt
#include <stdlib.h>
#include "kura.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// err turns a status into an error, and nil into nil.
//
// It is here rather than beside the codes in kura.go because only a build with
// the engine in it can produce a status to turn into anything.
func (s Status) err() error {
	if s == StatusOK {
		return nil
	}
	return s
}

// errABI is what Available reports when the engine linked in is not the one
// this package knows how to call.
func errABI(got uint32) error {
	return fmt.Errorf("kura: the engine speaks ABI version %d and this was built against %d, so one of them is stale", got, ABIVersion)
}

// Available reports whether the engine is linked and speaks an ABI this package
// knows. It returns nil when it does.
//
// The version check is a refusal rather than a warning. Two builds that
// disagree about a struct layout produce wrong answers rather than errors, and
// wrong answers from a storage engine are the worst kind of failure there is.
func Available() error {
	if got := uint32(C.kura_abi_version()); got != ABIVersion {
		return errABI(got)
	}
	return nil
}

// Version is what the engine calls itself.
func Version() string { return C.GoString(C.kura_version()) }

// statusMessage is what the engine calls a status code.
//
// It is here rather than in the test that uses it because a _test.go file may
// not import "C", and the test it exists for is the one that checks the status
// strings mirrored in kura.go have not drifted from the engine's own.
func statusMessage(s Status) string {
	return C.GoString(C.kura_status_message(C.int32_t(s)))
}

// Bitmap is a set of document ids held by the engine.
//
// It is the one thing in this package that outlives a call, so it is the one
// thing that has to be closed. The engine is not thread safe per handle: two
// goroutines may use two bitmaps at once and must not use the same one, and
// there is no lock in here because a lock would quietly turn a contract into a
// performance question.
type Bitmap struct{ ptr *C.KuraBitmap }

// NewBitmap allocates an empty bitmap in the engine.
func NewBitmap() (*Bitmap, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	p := C.kura_bitmap_new()
	if p == nil {
		return nil, StatusNull
	}
	return &Bitmap{ptr: p}, nil
}

// Close frees the bitmap. It is safe to call more than once, which matters
// because the alternative to a second Close being a no op is a second Close
// being a use after free.
func (b *Bitmap) Close() {
	if b.ptr != nil {
		C.kura_bitmap_free(b.ptr)
		b.ptr = nil
	}
}

// Insert adds a document id.
func (b *Bitmap) Insert(id uint32) error {
	if b.ptr == nil {
		return ErrClosed
	}
	return Status(C.kura_bitmap_insert(b.ptr, C.uint32_t(id))).err()
}

// Remove takes a document id out.
func (b *Bitmap) Remove(id uint32) error {
	if b.ptr == nil {
		return ErrClosed
	}
	return Status(C.kura_bitmap_remove(b.ptr, C.uint32_t(id))).err()
}

// Contains reports whether a document id is in the set.
func (b *Bitmap) Contains(id uint32) (bool, error) {
	if b.ptr == nil {
		return false, ErrClosed
	}
	var out C.int32_t
	if err := Status(C.kura_bitmap_contains(b.ptr, C.uint32_t(id), &out)).err(); err != nil {
		return false, err
	}
	return out != 0, nil
}

// Len is how many ids are in the set.
func (b *Bitmap) Len() (int, error) {
	if b.ptr == nil {
		return 0, ErrClosed
	}
	var out C.size_t
	if err := Status(C.kura_bitmap_len(b.ptr, &out)).err(); err != nil {
		return 0, err
	}
	return int(out), nil
}

// Intersect keeps only the ids that are also in the other bitmap.
func (b *Bitmap) Intersect(other *Bitmap) error {
	if b.ptr == nil || other == nil || other.ptr == nil {
		return ErrClosed
	}
	return Status(C.kura_bitmap_intersect(b.ptr, other.ptr)).err()
}

// Union adds every id from the other bitmap.
func (b *Bitmap) Union(other *Bitmap) error {
	if b.ptr == nil || other == nil || other.ptr == nil {
		return ErrClosed
	}
	return Status(C.kura_bitmap_union(b.ptr, other.ptr)).err()
}

// Array copies the ids out into Go memory, ascending.
func (b *Bitmap) Array() ([]uint32, error) {
	n, err := b.Len()
	if err != nil {
		return nil, err
	}
	out := make([]uint32, n)
	if n == 0 {
		return out, nil
	}
	var wrote C.size_t
	// The destination is Go memory and the engine writes into it for the
	// duration of this call and does not keep the pointer, which is what the
	// cgo pointer rules allow.
	st := Status(C.kura_bitmap_to_array(b.ptr, (*C.uint32_t)(unsafe.Pointer(&out[0])), C.size_t(n), &wrote))
	if err := st.err(); err != nil {
		return nil, err
	}
	return out[:wrote], nil
}

// EncodePostings compresses a list of ascending document ids.
func EncodePostings(ids []uint32) ([]byte, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	var buf C.KuraBuffer
	st := Status(C.kura_postings_encode(u32(ids), C.size_t(len(ids)), &buf))
	if err := st.err(); err != nil {
		// The header says freeing a buffer with a null pointer is a no op, so
		// this runs on the failure path too rather than being skipped in the
		// one case where it is hardest to reason about.
		C.kura_buffer_free(buf)
		return nil, err
	}
	out := C.GoBytes(unsafe.Pointer(buf.data), C.int(buf.len))
	C.kura_buffer_free(buf)
	return out, nil
}

// PostingsLen reads the count out of the encoded header, without decoding.
func PostingsLen(data []byte) (int, error) {
	if err := Available(); err != nil {
		return 0, err
	}
	var out C.size_t
	if err := Status(C.kura_postings_len(u8(data), C.size_t(len(data)), &out)).err(); err != nil {
		return 0, err
	}
	return int(out), nil
}

// DecodePostings expands an encoded list back into ids.
func DecodePostings(data []byte) ([]uint32, error) {
	n, err := PostingsLen(data)
	if err != nil {
		return nil, err
	}
	// The count comes out of the header, and the header is bytes that may be
	// anything. A list of n ids spends at least one byte per id in its blocks
	// section and the blocks are part of the input, so a count larger than the
	// input cannot be honest. Without this check a seven byte input can claim
	// four billion ids and this line asks the allocator for seventeen
	// gigabytes, which the fuzzer found in under a minute.
	if n > len(data) {
		return nil, StatusTruncated
	}
	out := make([]uint32, n)
	if n == 0 {
		return out, nil
	}
	var wrote C.size_t
	st := Status(C.kura_postings_decode(u8(data), C.size_t(len(data)),
		(*C.uint32_t)(unsafe.Pointer(&out[0])), C.size_t(n), &wrote))
	if err := st.err(); err != nil {
		return nil, err
	}
	return out[:wrote], nil
}

// PostingsContains answers a membership question by decoding one block, so a
// large list never has to cross the boundary to be asked about.
func PostingsContains(data []byte, id uint32) (bool, error) {
	if err := Available(); err != nil {
		return false, err
	}
	var out C.int32_t
	if err := Status(C.kura_postings_contains(u8(data), C.size_t(len(data)), C.uint32_t(id), &out)).err(); err != nil {
		return false, err
	}
	return out != 0, nil
}

// Cosine is the cosine similarity of two vectors of the same length.
func Cosine(a, b []float32) (float32, error) {
	if err := Available(); err != nil {
		return 0, err
	}
	var out C.float
	st := Status(C.kura_vector_cosine(f32(a), C.size_t(len(a)), f32(b), C.size_t(len(b)), &out))
	if err := st.err(); err != nil {
		return 0, err
	}
	return float32(out), nil
}

// Quantise turns a vector into one signed byte per dimension plus a scale. Both
// parts are needed to score or reconstruct it, so keep them together.
func Quantise(v []float32) (codes []int8, scale float32, err error) {
	if err := Available(); err != nil {
		return nil, 0, err
	}
	out := make([]int8, len(v))
	if len(v) == 0 {
		return out, 0, nil
	}
	var factor C.float
	st := Status(C.kura_vector_quantise(f32(v), C.size_t(len(v)), (*C.int8_t)(unsafe.Pointer(&out[0])), &factor))
	if err := st.err(); err != nil {
		return nil, 0, err
	}
	return out, float32(factor), nil
}

// DotQuantised scores two quantised vectors against each other.
func DotQuantised(a []int8, aScale float32, b []int8, bScale float32) (float32, error) {
	if err := Available(); err != nil {
		return 0, err
	}
	if len(a) != len(b) {
		return 0, StatusDimensionMismatch
	}
	var out C.float
	st := Status(C.kura_vector_dot_quantised(i8(a), C.float(aScale), i8(b), C.float(bScale), C.size_t(len(a)), &out))
	if err := st.err(); err != nil {
		return 0, err
	}
	return float32(out), nil
}

// The four helpers below turn a Go slice into a pointer the engine can read.
//
// An empty slice has no first element to take the address of, and a null
// pointer is an error to the engine rather than an empty input, so an empty
// slice points at a byte of scratch that the engine is told is zero long. That
// keeps "no ids" a legal argument rather than a special case at every call
// site.
var scratch [8]byte

func u32(v []uint32) *C.uint32_t {
	if len(v) == 0 {
		return (*C.uint32_t)(unsafe.Pointer(&scratch))
	}
	return (*C.uint32_t)(unsafe.Pointer(&v[0]))
}

func u8(v []byte) *C.uint8_t {
	if len(v) == 0 {
		return (*C.uint8_t)(unsafe.Pointer(&scratch))
	}
	return (*C.uint8_t)(unsafe.Pointer(&v[0]))
}

func f32(v []float32) *C.float {
	if len(v) == 0 {
		return (*C.float)(unsafe.Pointer(&scratch))
	}
	return (*C.float)(unsafe.Pointer(&v[0]))
}

func i8(v []int8) *C.int8_t {
	if len(v) == 0 {
		return (*C.int8_t)(unsafe.Pointer(&scratch))
	}
	return (*C.int8_t)(unsafe.Pointer(&v[0]))
}
