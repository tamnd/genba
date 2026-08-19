//go:build cgo && kura && unix

package kura

// The leak tests, which are Unix only.
//
// What they measure is the resident set high water mark, because the memory at
// risk is the engine\'s and the Go allocator has never heard of it. The only
// portable way to read that is getrusage, which Windows does not have, so these
// run on Linux and macOS and Windows gets the rest of the suite. A leak in a
// cross platform Rust library is not going to be one that only happens on
// Windows.

import (
	"runtime"
	"syscall"
	"testing"
)

// TestABitmapDoesNotLeak is the box about ten thousand cycles not growing
// memory. It is a resident set measurement rather than an allocator counter,
// because the memory at risk here is the engine's and the Go allocator has
// never heard of it.
func TestABitmapDoesNotLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("this one runs twenty thousand allocation cycles")
	}
	if raceDetector {
		t.Skip("the race detector grows the resident set on its own, which is the measurement this makes")
	}
	const (
		warmup = 2_000
		cycles = 20_000
		ids    = 64
	)

	cycle := func(n int) {
		for range n {
			b, err := NewBitmap()
			if err != nil {
				t.Fatalf("allocating: %v", err)
			}
			for id := range uint32(ids) {
				if err := b.Insert(id * 3); err != nil {
					t.Fatalf("inserting: %v", err)
				}
			}
			if _, err := b.Array(); err != nil {
				t.Fatalf("reading back: %v", err)
			}
			b.Close()
		}
	}

	// The warmup is not politeness. The first few thousand cycles grow the
	// allocator's arenas, and measuring across that growth measures the arena
	// rather than the leak.
	cycle(warmup)
	runtime.GC()
	before := maxrss(t)

	cycle(cycles)
	runtime.GC()
	after := maxrss(t)

	// Maxrss is a high water mark, so it can only go up, and a leak of one
	// bitmap per cycle over twenty thousand cycles is megabytes rather than
	// kilobytes. Eight megabytes of slack is wide enough that ordinary
	// fragmentation does not fail this and narrow enough that a real leak
	// cannot hide under it.
	const slack = 8 << 20
	if grew := after - before; grew > slack {
		t.Errorf("twenty thousand bitmaps grew the resident set by %d bytes", grew)
	}
}

// TestPostingsDoNotLeak is the same measurement for the path that allocates in
// the engine and frees from Go, which is the one where a missing free is
// easiest to write.
func TestPostingsDoNotLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("this one runs twenty thousand encode and decode cycles")
	}
	if raceDetector {
		t.Skip("the race detector grows the resident set on its own, which is the measurement this makes")
	}
	ids := make([]uint32, 512)
	for i := range ids {
		ids[i] = uint32(i) * 11
	}

	cycle := func(n int) {
		for range n {
			encoded, err := EncodePostings(ids)
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}
			if _, err := DecodePostings(encoded); err != nil {
				t.Fatalf("decoding: %v", err)
			}
		}
	}

	cycle(2_000)
	runtime.GC()
	before := maxrss(t)

	cycle(20_000)
	runtime.GC()
	after := maxrss(t)

	const slack = 8 << 20
	if grew := after - before; grew > slack {
		t.Errorf("twenty thousand encode and decode cycles grew the resident set by %d bytes", grew)
	}
}

// maxrss is the high water mark of this process's resident set, in bytes.
func maxrss(t *testing.T) int64 {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Skipf("this platform does not report resident set size: %v", err)
	}
	// Linux reports kilobytes and the BSDs report bytes, so the units differ by
	// a factor of a thousand and a fixed slack means different things on each.
	// Normalising here keeps the tests above talking about bytes. The
	// conversion is redundant on a 64 bit platform and is not on a 32 bit one,
	// where Maxrss is an int32.
	if runtime.GOOS == "linux" {
		return int64(usage.Maxrss) * 1024
	}
	return int64(usage.Maxrss)
}
