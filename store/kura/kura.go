// Package kura links the Rust storage engine through its C ABI.
//
// The engine at https://github.com/tamnd/kura holds the compression, the
// posting lists and the vector scan, and exposes them over a C ABI so that any
// host can link it. This package is the Go side of that ABI.
//
// # It is off by default
//
// The whole point of this repository is a single static binary with no
// dependencies, and a cgo build gives that up. So the fast path is opt in and
// the default build does not have it:
//
//	go build ./...                                   the pure Go build
//	CGO_ENABLED=1 go build -tags kura ./...          linked against the engine
//
// Without the tag every call here returns [ErrUnavailable] and says which build
// to use. It is not a panic and it is not a silent fallback, because a caller
// that asked for the engine and quietly got something else has no way to find
// out.
//
// The issue this was built for asked for a plain cgo tag. A plain cgo tag would
// mean that anybody with the default CGO_ENABLED=1 could not build this
// repository at all until they had a copy of the engine on disk, which is a
// worse default than the one it was trying to avoid. The kura tag is the same
// decision made explicit: nothing links the engine unless somebody asked.
//
// # The contract with the library
//
// Every call returns a status code, and every one of them is checked. A status
// that is not [StatusOK] becomes a Go error and no out parameter is read.
//
// Memory the engine allocates is freed by the engine. Every call here that gets
// a buffer back copies it into Go memory and frees it before returning, so
// nothing outside this package ever holds a pointer the engine owns. The one
// handle that outlives a call is [Bitmap], and it has a Close.
//
// Nothing crosses the boundary in either direction. The engine catches its own
// panics and reports [StatusPanic] rather than unwinding into Go, and no Go
// pointer is handed to the engine except for the duration of a single call.
package kura

import "errors"

// ErrUnavailable is what every call returns in a build without the engine.
var ErrUnavailable = errors.New("kura: this binary was not built with the engine, rebuild with CGO_ENABLED=1 go build -tags kura")

// ErrClosed is returned by a method on a bitmap that has already been freed.
// Calling into the engine with a pointer it has freed is a use after free, so
// this is a refusal rather than a crash.
var ErrClosed = errors.New("kura: the bitmap is closed")

// ABIVersion is the version of the C ABI this package was written against. The
// engine reports its own, and a mismatch is a refusal: two builds that disagree
// about a struct layout produce wrong answers rather than errors, which is the
// worst way for this to fail.
const ABIVersion = 1

// Status is a status code from the engine.
type Status int32

// The status codes, from the C header. They are mirrored here rather than read
// through cgo so that an error value means the same thing in both builds, and
// there is a test under the kura tag that checks every one of these against the
// message the engine gives for it.
const (
	StatusOK                 Status = 0
	StatusNull               Status = 1
	StatusTruncated          Status = 2
	StatusOverflow           Status = 3
	StatusBadMagic           Status = 4
	StatusUnsupportedVersion Status = 5
	StatusChecksum           Status = 6
	StatusDimensionMismatch  Status = 7
	StatusNotSorted          Status = 8
	StatusBufferTooSmall     Status = 9
	StatusPanic              Status = 10
)

var statusMessages = map[Status]string{
	StatusOK:                 "ok",
	StatusNull:               "a required pointer was null",
	StatusTruncated:          "input ended early",
	StatusOverflow:           "variable length integer did not terminate",
	StatusBadMagic:           "not a kura file",
	StatusUnsupportedVersion: "unsupported format version",
	StatusChecksum:           "checksum mismatch",
	StatusDimensionMismatch:  "vectors of different lengths",
	StatusNotSorted:          "document ids are not ascending",
	StatusBufferTooSmall:     "the buffer is too small for the result",
	StatusPanic:              "the engine abandoned the call",
}

func (s Status) String() string {
	if m, ok := statusMessages[s]; ok {
		return m
	}
	return "unknown status"
}

// Error makes a status an error, so that a caller can tell an unsorted input
// from a corrupt file with errors.Is rather than by reading a string.
func (s Status) Error() string { return "kura: " + s.String() }
