package segdir_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tamnd/genba/store/segdir"
)

// The sizes a real index reaches. A segment is a flush of a memtable, so a
// corpus of a few million documents is thousands of them before compaction
// catches up, and ten thousand is the number worth being able to open quickly.
var sizes = []int{100, 1_000, 10_000}

// BenchmarkOpen is recovery, which is what an operator waits for after a crash.
//
// It is the same path a clean start takes, on purpose, because a path only
// taken after a crash is a path only exercised after a crash. What it costs is
// a read of the manifest, a listing of the directory and one stat per segment,
// so it is linear in the number of segments and independent of their size.
func BenchmarkOpen(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("%d segments", n), func(b *testing.B) {
			dir := corpus(b, n)
			b.ResetTimer()
			for b.Loop() {
				open(b, dir, segdir.Options{})
			}
			b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds()/1e3, "ksegment/s")
		})
	}
}

// BenchmarkOpenVerified is what the paranoid option costs, which is a read of
// the whole index rather than a stat of it.
func BenchmarkOpenVerified(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("%d segments", n), func(b *testing.B) {
			dir := corpus(b, n)
			b.ResetTimer()
			for b.Loop() {
				open(b, dir, segdir.Options{Verify: true})
			}
			b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds()/1e3, "ksegment/s")
		})
	}
}

// BenchmarkOpenAfterACrash is the same thing with rubbish to sweep, which is
// what a directory that was interrupted a few hundred times looks like. The
// number to compare it against is BenchmarkOpen at the same size.
func BenchmarkOpenAfterACrash(b *testing.B) {
	const litter = 500
	for _, n := range sizes {
		b.Run(fmt.Sprintf("%d segments", n), func(b *testing.B) {
			dir := corpus(b, n)
			b.ResetTimer()
			for b.Loop() {
				b.StopTimer()
				for i := range litter {
					file := filepath.Join(dir, fmt.Sprintf("%016d.seg.tmp", n+i+1))
					if err := os.WriteFile(file, []byte("a publish that was interrupted"), 0o644); err != nil {
						b.Fatalf("planting rubbish: %v", err)
					}
				}
				b.StartTimer()
				open(b, dir, segdir.Options{})
			}
		})
	}
}

// BenchmarkPublish is the write side, and the number it exposes is the one the
// design trades away: the manifest is rewritten whole every time, so a publish
// costs a write proportional to the number of live segments rather than to the
// one being added.
//
// It is measured with the flushes turned off, because with them on this is a
// benchmark of the disk. The point is the shape of the curve.
func BenchmarkPublish(b *testing.B) {
	for _, n := range []int{10, 100, 1_000, 10_000} {
		b.Run(fmt.Sprintf("%d live", n), func(b *testing.B) {
			dir := corpus(b, n)
			d := open(b, dir, segdir.Options{Sync: segdir.SyncNone})
			b.ResetTimer()
			for b.Loop() {
				publish(b, d, d.Next(), "another")
			}
		})
	}
}

// corpus is a directory with n segments in it, written without flushes because
// what is being measured is the reading.
func corpus(tb testing.TB, n int) string {
	tb.Helper()
	dir := filepath.Join(tb.TempDir(), "index")
	d := open(tb, dir, segdir.Options{Sync: segdir.SyncNone})
	for i := range n {
		publish(tb, d, uint64(i+1), fmt.Sprintf("segment %d", i+1))
	}
	if err := d.Close(); err != nil {
		tb.Fatalf("closing: %v", err)
	}
	return dir
}
