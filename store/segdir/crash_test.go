package segdir

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/tamnd/genba/store/segment"
)

// The crash test kills a real process at every instant a publish can be
// interrupted at, and then opens the directory and says what has to be true.
//
// It is a subprocess rather than a simulation because the thing being tested is
// what the operating system does with writes that were in flight, and a fake
// that decided which of them survived would be a test of the fake. The child is
// this same test binary with an environment variable telling it which write
// point to die at, and it dies with os.Exit, which runs no deferred function
// and flushes nothing, exactly like a kill or a panic in another goroutine.
//
// What it cannot test is a power cut. The pages the child wrote and did not
// flush are the operating system's by the time it dies, so they arrive on disk
// afterwards, and only pulling the plug tells you whether an fsync was really
// called. That gap is the reason SyncFull is the default rather than something
// to reach for: the test proves the ordering is right, and fsync is what makes
// the ordering mean anything on a machine that loses power.
const (
	crashEnvDir  = "GENBA_SEGDIR_CRASH_DIR"
	crashEnvAt   = "GENBA_SEGDIR_CRASH_AT"
	crashEnvWhat = "GENBA_SEGDIR_CRASH_WHAT"

	// crashed is what the child exits with when it died where it was told to,
	// so that the parent can tell that from a child that failed for some other
	// reason and from one that ran out of write points.
	crashed = 7
)

// TestKillingTheProcessAtEveryWritePointRecovers is the test issue #9 asks for.
//
// For each instant inside a publish, and then each instant inside a remove, a
// child process is killed there and the directory it left behind has to satisfy
// all of this: it opens, everything published before the interrupted operation
// is still there and still readable, the interrupted operation either happened
// or did not, nothing that no reader can see is left on disk, and the next
// publish works.
func TestKillingTheProcessAtEveryWritePointRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("this spawns a process per write point")
	}
	for _, what := range []string{"publish", "remove"} {
		t.Run(what, func(t *testing.T) {
			points := 0
			// The number of write points is not written down anywhere, which is
			// on purpose. The loop stops when the child survives, so adding one
			// to the publish path adds a case here without anybody having to
			// remember to update a constant.
			for at := 1; at < 32; at++ {
				dir := filepath.Join(t.TempDir(), "index")
				if !crash(t, dir, what, at) {
					points = at - 1
					break
				}
				t.Run(fmt.Sprintf("at %d", at), func(t *testing.T) {
					survivors(t, dir, what)
				})
			}
			if points == 0 {
				t.Fatal("the child never died, so nothing was tested")
			}
			t.Logf("%s has %d write points, and a crash at each of them recovers", what, points)
		})
	}
}

// crash runs a child that dies at the nth write point, and reports whether it
// got that far.
func crash(t *testing.T, dir, what string, at int) bool {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestCrashHelper", "-test.v")
	cmd.Env = append(os.Environ(),
		crashEnvDir+"="+dir,
		crashEnvWhat+"="+what,
		crashEnvAt+"="+strconv.Itoa(at),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return false
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != crashed {
		t.Fatalf("the child failed for the wrong reason: %v\n%s", err, out)
	}
	return true
}

// TestCrashHelper is the child. It is skipped when it is nobody's child, which
// is what makes it invisible in an ordinary test run.
func TestCrashHelper(t *testing.T) {
	dir := os.Getenv(crashEnvDir)
	if dir == "" {
		t.Skip("this test is only run as the child of the crash test")
	}
	at, err := strconv.Atoi(os.Getenv(crashEnvAt))
	if err != nil {
		t.Fatalf("the crash point: %v", err)
	}

	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	// The state the parent expects to survive, published normally.
	for i := range 3 {
		if _, err := d.Publish(segmentOf(t, uint64(i+1))); err != nil {
			t.Fatalf("publishing segment %d: %v", i+1, err)
		}
	}

	// Armed only now, so the counting starts at the operation being tested.
	seen := 0
	crashPoint = func(string) {
		seen++
		if seen == at {
			os.Exit(crashed)
		}
	}
	switch what := os.Getenv(crashEnvWhat); what {
	case "publish":
		_, err = d.Publish(segmentOf(t, 4))
	case "remove":
		err = d.Remove(2)
	default:
		t.Fatalf("no such operation: %q", what)
	}
	if err != nil {
		t.Fatalf("the operation failed rather than crashing: %v", err)
	}
	// A clean exit says the operation had fewer write points than the parent
	// asked for, which is how the parent knows to stop.
	os.Exit(0)
}

// survivors is everything that has to be true of a directory a killed process
// left behind.
func survivors(t *testing.T, dir, what string) {
	t.Helper()

	d, err := Open(dir, Options{Verify: true})
	if err != nil {
		t.Fatalf("the directory does not open: %v", err)
	}
	live := d.Segments()

	// The three segments published before the interrupted operation are always
	// there, and segment 2 is the one a remove was interrupted on.
	want := []uint64{1, 2, 3}
	if what == "remove" {
		want = []uint64{1, 3}
	}
	for _, seq := range want {
		if !slices.ContainsFunc(live, func(e Entry) bool { return e.Sequence == seq }) {
			if what == "remove" || seq != 2 {
				t.Errorf("segment %d is gone, and it was published before the crash", seq)
			}
		}
	}
	if what == "remove" && len(live) > 3 {
		t.Errorf("the directory holds %d segments, want 2 or 3", len(live))
	}
	if what == "publish" && (len(live) < 3 || len(live) > 4) {
		t.Errorf("the directory holds %d segments, want 3 or 4", len(live))
	}

	// Everything live reads back, and reads back as itself.
	for _, e := range live {
		b, err := d.Read(e.Sequence)
		if err != nil {
			t.Fatalf("reading segment %d: %v", e.Sequence, err)
		}
		s, err := segment.Open(b)
		if err != nil {
			t.Fatalf("segment %d does not parse: %v", e.Sequence, err)
		}
		if s.Sequence() != e.Sequence {
			t.Errorf("the file for segment %d holds segment %d", e.Sequence, s.Sequence())
		}
	}

	// Nothing a reader cannot see is left on disk, which is what stops a
	// directory that has crashed a thousand times from filling a volume.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if got, want := len(entries), len(live)+1; got != want {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %d files and %d segments are live: %v", got, len(live), names)
	}

	// And the directory is not just readable, it is writable: a crash leaves
	// something that can be carried on from rather than something that has to
	// be rebuilt.
	next := d.Next()
	if _, err := d.Publish(segmentOf(t, next)); err != nil {
		t.Fatalf("publishing after recovery: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	again, err := Open(dir, Options{Verify: true})
	if err != nil {
		t.Fatalf("reopening after recovery: %v", err)
	}
	if got := again.Segments(); len(got) != len(live)+1 {
		t.Errorf("after publishing again the directory holds %d segments, want %d", len(got), len(live)+1)
	}
}

// segmentOf is a segment whose payload names its own sequence, so that a file
// that holds the wrong segment is visible rather than plausible.
func segmentOf(tb testing.TB, sequence uint64) []byte {
	tb.Helper()
	w := segment.NewWriter(sequence)
	if err := w.Add(segment.KindFields, fmt.Appendf(nil, "segment %d", sequence)); err != nil {
		tb.Fatalf("adding a section: %v", err)
	}
	b, err := w.Bytes()
	if err != nil {
		tb.Fatalf("building a segment: %v", err)
	}
	return b
}
