package segdir_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tamnd/genba/store/segdir"
	"github.com/tamnd/genba/store/segment"
)

func TestAPublishedSegmentSurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir, segdir.Options{})
	for i := range 3 {
		publish(t, d, d.Next(), fmt.Sprintf("document %d", i))
	}
	if err := d.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	again := open(t, dir, segdir.Options{Verify: true})
	live := again.Segments()
	if len(live) != 3 {
		t.Fatalf("the reopened directory holds %d segments, want 3", len(live))
	}
	for i, e := range live {
		if e.Sequence != uint64(i+1) {
			t.Errorf("segment %d has sequence %d", i, e.Sequence)
		}
		b, err := again.Read(e.Sequence)
		if err != nil {
			t.Fatalf("reading segment %d: %v", e.Sequence, err)
		}
		if got := payload(t, b); got != fmt.Sprintf("document %d", i) {
			t.Errorf("segment %d holds %q", e.Sequence, got)
		}
	}
}

// TestASegmentTheManifestDoesNotNameIsInvisible is the property the whole
// package exists for, tested at the layer below the crash test: whatever put a
// segment file there, a reader only ever sees what the manifest names.
func TestASegmentTheManifestDoesNotNameIsInvisible(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir, segdir.Options{})
	publish(t, d, d.Next(), "real")

	// A publish that got as far as the rename and no further.
	orphan := filepath.Join(dir, "0000000000000002.seg")
	if err := os.WriteFile(orphan, build(t, 2, "never published"), 0o644); err != nil {
		t.Fatalf("planting a segment: %v", err)
	}
	// A publish that did not get that far.
	half := filepath.Join(dir, "0000000000000003.seg.tmp")
	if err := os.WriteFile(half, []byte("half a segment"), 0o644); err != nil {
		t.Fatalf("planting a partial write: %v", err)
	}

	again := open(t, dir, segdir.Options{Verify: true})
	if got := again.Segments(); len(got) != 1 || got[0].Sequence != 1 {
		t.Errorf("the reopened directory holds %v, want only segment 1", got)
	}
	if _, err := again.Read(2); !errors.Is(err, segdir.ErrMissing) {
		t.Errorf("reading an unpublished segment returned %v, want ErrMissing", err)
	}
	for _, file := range []string{orphan, half} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("%s survived recovery", filepath.Base(file))
		}
	}
}

// TestRecoveryLeavesNothingBehind is why a directory that has crashed a
// thousand times is the same size as one that never has.
func TestRecoveryLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir, segdir.Options{})
	publish(t, d, d.Next(), "one")
	publish(t, d, d.Next(), "two")
	for i := range 20 {
		file := filepath.Join(dir, fmt.Sprintf("%016d.seg.tmp", i+3))
		if err := os.WriteFile(file, []byte("rubbish"), 0o644); err != nil {
			t.Fatalf("planting rubbish: %v", err)
		}
	}

	open(t, dir, segdir.Options{})
	if got, want := listing(t, dir), []string{"0000000000000001.seg", "0000000000000002.seg", "manifest"}; !slices.Equal(got, want) {
		t.Errorf("after recovery the directory holds %v, want %v", got, want)
	}
}

// TestAManifestThatNamesAMissingSegmentIsRefused is the loud failure. An index
// that quietly came back smaller would be a search that quietly returns fewer
// answers, and that takes a month to notice.
func TestAManifestThatNamesAMissingSegmentIsRefused(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir, segdir.Options{})
	publish(t, d, d.Next(), "one")
	publish(t, d, d.Next(), "two")

	if err := os.Remove(filepath.Join(dir, "0000000000000002.seg")); err != nil {
		t.Fatalf("removing a segment behind the manifest's back: %v", err)
	}
	if _, err := segdir.Open(dir, segdir.Options{}); !errors.Is(err, segdir.ErrMissing) {
		t.Errorf("opening returned %v, want ErrMissing", err)
	}
}

func TestASegmentTruncatedBehindTheManifestIsRefused(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir, segdir.Options{})
	publish(t, d, d.Next(), "one")

	file := filepath.Join(dir, "0000000000000001.seg")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading the segment: %v", err)
	}
	if err := os.WriteFile(file, b[:len(b)-8], 0o644); err != nil {
		t.Fatalf("truncating the segment: %v", err)
	}
	if _, err := segdir.Open(dir, segdir.Options{}); !errors.Is(err, segdir.ErrMissing) {
		t.Errorf("opening returned %v, want ErrMissing", err)
	}
}

// TestVerifyIsWhatCatchesTheRightSizeAndTheWrongBytes says what the option
// buys, which is the failure a crash does not produce and a bad sector does.
func TestVerifyIsWhatCatchesTheRightSizeAndTheWrongBytes(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir, segdir.Options{})
	publish(t, d, d.Next(), "one")

	file := filepath.Join(dir, "0000000000000001.seg")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading the segment: %v", err)
	}
	b[len(b)-1] ^= 0x40
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatalf("damaging the segment: %v", err)
	}

	if _, err := segdir.Open(dir, segdir.Options{}); err != nil {
		t.Errorf("the cheap check refused a file of the right length: %v", err)
	}
	if _, err := segdir.Open(dir, segdir.Options{Verify: true}); !errors.Is(err, segdir.ErrMissing) {
		t.Errorf("verifying returned %v, want ErrMissing", err)
	}
}

func TestASequencePublishedTwiceIsRefused(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir, segdir.Options{})
	publish(t, d, 1, "one")
	if _, err := d.Publish(build(t, 1, "one again")); !errors.Is(err, segdir.ErrSequence) {
		t.Errorf("publishing a sequence twice returned %v, want ErrSequence", err)
	}
}

func TestBytesThatAreNotASegmentAreNeverWritten(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir, segdir.Options{})
	if _, err := d.Publish([]byte("not a segment")); err == nil {
		t.Fatal("publishing rubbish was accepted")
	}
	if got := listing(t, dir); len(got) != 0 {
		t.Errorf("the refused publish left %v behind", got)
	}
}

// TestASequenceIsNeverReused matters because something above this caches
// segments by name. A name that came back meaning a different segment would be
// a cache that serves the wrong bytes and never notices.
func TestASequenceIsNeverReused(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir, segdir.Options{})
	for range 3 {
		publish(t, d, d.Next(), "a segment")
	}
	if err := d.Remove(1, 2, 3); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if got := d.Segments(); len(got) != 0 {
		t.Fatalf("the directory still holds %v", got)
	}

	reopened := open(t, dir, segdir.Options{})
	if got := reopened.Next(); got != 4 {
		t.Errorf("the next sequence after removing everything is %d, want 4", got)
	}
}

func TestRemovingSomethingThatIsNotThereIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir, segdir.Options{})
	publish(t, d, d.Next(), "one")
	if err := d.Remove(99); err != nil {
		t.Errorf("removing an unpublished sequence returned %v", err)
	}
	if err := d.Remove(1); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if err := d.Remove(1); err != nil {
		t.Errorf("removing twice returned %v", err)
	}
	if got := listing(t, dir); !slices.Equal(got, []string{"manifest"}) {
		t.Errorf("after removing everything the directory holds %v", got)
	}
}

// TestTheSyncPolicyDefaultsToSafe is a one line test for a decision that is
// easy to reverse by accident: the zero value of the options has to be the one
// that survives a power cut.
func TestTheSyncPolicyDefaultsToSafe(t *testing.T) {
	if got := (segdir.Options{}).Sync; got != segdir.SyncFull {
		t.Errorf("the default sync policy is %v, want full", got)
	}
	for _, s := range []segdir.Sync{segdir.SyncFull, segdir.SyncManifest, segdir.SyncNone} {
		if got := s.String(); strings.HasPrefix(got, "sync(") {
			t.Errorf("policy %d has no name", uint8(s))
		}
	}
}

func TestEveryPolicyRoundTrips(t *testing.T) {
	for _, s := range []segdir.Sync{segdir.SyncFull, segdir.SyncManifest, segdir.SyncNone} {
		t.Run(s.String(), func(t *testing.T) {
			dir := t.TempDir()
			d := open(t, dir, segdir.Options{Sync: s})
			publish(t, d, d.Next(), "one")
			publish(t, d, d.Next(), "two")

			again := open(t, dir, segdir.Options{Verify: true})
			if got := again.Segments(); len(got) != 2 {
				t.Errorf("the reopened directory holds %d segments, want 2", len(got))
			}
		})
	}
}

func TestUsingAClosedDirectoryIsRefused(t *testing.T) {
	d := open(t, t.TempDir(), segdir.Options{})
	if err := d.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	if _, err := d.Publish(build(t, 1, "one")); !errors.Is(err, segdir.ErrClosed) {
		t.Errorf("publishing to a closed directory returned %v, want ErrClosed", err)
	}
	if err := d.Remove(1); !errors.Is(err, segdir.ErrClosed) {
		t.Errorf("removing from a closed directory returned %v, want ErrClosed", err)
	}
	if _, err := d.Read(1); !errors.Is(err, segdir.ErrClosed) {
		t.Errorf("reading from a closed directory returned %v, want ErrClosed", err)
	}
}

// TestOpenRefusesADamagedManifest is the same hostility the segment format
// applies to its own bytes. A manifest is the one file that says what a reader
// may see, so it is the last place to be forgiving.
func TestOpenRefusesADamagedManifest(t *testing.T) {
	cases := []struct {
		name   string
		damage func(b []byte) []byte
		want   error
	}{
		{"empty", func([]byte) []byte { return nil }, segdir.ErrFormat},
		{"a header and nothing else", func(b []byte) []byte { return b[:16] }, segdir.ErrFormat},
		{"not a manifest", func(b []byte) []byte { b[0] = 'x'; return b }, segdir.ErrFormat},
		{"a version from the future", func(b []byte) []byte { b[8] = 2; return b }, segdir.ErrVersion},
		{"a flag this build does not know", func(b []byte) []byte { b[10] = 1; return b }, segdir.ErrVersion},
		{"reserved bytes that are not zero", func(b []byte) []byte { b[28] = 1; return b }, segdir.ErrFormat},
		{"a count the file cannot hold", func(b []byte) []byte { b[12] = 99; return b }, segdir.ErrFormat},
		{"a flipped byte in an entry", func(b []byte) []byte { b[33] ^= 0x01; return b }, segdir.ErrFormat},
		{"a truncated entry", func(b []byte) []byte { return b[:len(b)-4] }, segdir.ErrFormat},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			d := open(t, dir, segdir.Options{})
			publish(t, d, d.Next(), "one")
			publish(t, d, d.Next(), "two")

			file := filepath.Join(dir, "manifest")
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("reading the manifest: %v", err)
			}
			if err := os.WriteFile(file, c.damage(b), 0o644); err != nil {
				t.Fatalf("damaging the manifest: %v", err)
			}
			if _, err := segdir.Open(dir, segdir.Options{}); !errors.Is(err, c.want) {
				t.Errorf("opening returned %v, want %v", err, c.want)
			}
		})
	}
}

// TestOpenRefusesEveryDamagedManifestByte is the same idea without a list of
// the ways it can go wrong. Every byte is flipped two ways and the only
// requirement is that each one either opens or says why not.
func TestOpenRefusesEveryDamagedManifestByte(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir, segdir.Options{})
	publish(t, d, d.Next(), "one")
	publish(t, d, d.Next(), "two")
	sound, err := os.ReadFile(filepath.Join(dir, "manifest"))
	if err != nil {
		t.Fatalf("reading the manifest: %v", err)
	}

	for i := range sound {
		for _, bit := range []byte{0x01, 0x80} {
			b := slices.Clone(sound)
			b[i] ^= bit
			at := t.TempDir()
			if err := os.WriteFile(filepath.Join(at, "manifest"), b, 0o644); err != nil {
				t.Fatalf("writing the manifest: %v", err)
			}
			// Nothing to assert beyond not panicking and not hanging. A
			// manifest that survives a flipped byte is one whose entries still
			// name segments, and those are not there, so it fails at the check
			// instead.
			if _, err := segdir.Open(at, segdir.Options{}); err == nil {
				t.Errorf("byte %d flipped with %#02x opened and named segments that are not there", i, bit)
			}
		}
	}
}

func FuzzOpen(f *testing.F) {
	dir := f.TempDir()
	d := open(f, dir, segdir.Options{})
	publish(f, d, d.Next(), "one")
	b, err := os.ReadFile(filepath.Join(dir, "manifest"))
	if err != nil {
		f.Fatalf("reading the manifest: %v", err)
	}
	f.Add(b)
	f.Add([]byte("genbaman"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		at := t.TempDir()
		if err := os.WriteFile(filepath.Join(at, "manifest"), b, 0o644); err != nil {
			t.Skip()
		}
		// The only contract is that nothing panics. A manifest that parses
		// names segments that are not there, which is an error, and one that
		// does not parse is an error too.
		if d, err := segdir.Open(at, segdir.Options{}); err == nil {
			d.Segments()
			d.Close()
		}
	})
}

// build returns a segment carrying a payload, so that a test can tell one
// segment from another by reading it back.
func build(tb testing.TB, sequence uint64, payload string) []byte {
	tb.Helper()
	w := segment.NewWriter(sequence)
	if err := w.Add(segment.KindFields, []byte(payload)); err != nil {
		tb.Fatalf("adding a section: %v", err)
	}
	b, err := w.Bytes()
	if err != nil {
		tb.Fatalf("building a segment: %v", err)
	}
	return b
}

// payload reads it back.
func payload(tb testing.TB, b []byte) string {
	tb.Helper()
	s, err := segment.Open(b)
	if err != nil {
		tb.Fatalf("opening a segment: %v", err)
	}
	section, ok := s.Section(segment.KindFields)
	if !ok {
		tb.Fatal("the segment has no fields section")
	}
	return string(section)
}

func open(tb testing.TB, path string, opt segdir.Options) *segdir.Dir {
	tb.Helper()
	d, err := segdir.Open(path, opt)
	if err != nil {
		tb.Fatalf("opening %s: %v", path, err)
	}
	return d
}

func publish(tb testing.TB, d *segdir.Dir, sequence uint64, payload string) segdir.Entry {
	tb.Helper()
	e, err := d.Publish(build(tb, sequence, payload))
	if err != nil {
		tb.Fatalf("publishing segment %d: %v", sequence, err)
	}
	return e
}

// listing is the file names in a directory, sorted, which is what the recovery
// tests assert on.
func listing(tb testing.TB, path string) []string {
	tb.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		tb.Fatalf("listing %s: %v", path, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}
