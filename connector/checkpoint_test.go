package connector_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
)

func TestACursorThatWasNeverSavedIsTheBeginning(t *testing.T) {
	for name, cp := range checkpointStores(t) {
		t.Run(name, func(t *testing.T) {
			got, err := cp.Load(t.Context(), "acme", "never-run")
			if err != nil {
				t.Fatalf("loading a source that never ran is not an error: %v", err)
			}
			if !got.IsZero() {
				t.Errorf("got cursor %+v, want the zero cursor", got)
			}
		})
	}
}

func TestACursorRoundTrips(t *testing.T) {
	for name, cp := range checkpointStores(t) {
		t.Run(name, func(t *testing.T) {
			want := connector.Cursor{Value: "page-42", Time: time.Now().UTC().Truncate(time.Second)}
			if err := cp.Save(t.Context(), "acme", "wiki", want); err != nil {
				t.Fatalf("save: %v", err)
			}
			got, err := cp.Load(t.Context(), "acme", "wiki")
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got.Value != want.Value {
				t.Errorf("value is %q, want %q", got.Value, want.Value)
			}
			if !got.Time.Equal(want.Time) {
				t.Errorf("time is %v, want %v", got.Time, want.Time)
			}
		})
	}
}

func TestASavedCursorReplacesTheOneBeforeIt(t *testing.T) {
	for name, cp := range checkpointStores(t) {
		t.Run(name, func(t *testing.T) {
			for _, v := range []string{"one", "two", "three"} {
				if err := cp.Save(t.Context(), "acme", "wiki", connector.Cursor{Value: v}); err != nil {
					t.Fatalf("save %s: %v", v, err)
				}
			}
			got, _ := cp.Load(t.Context(), "acme", "wiki")
			if got.Value != "three" {
				t.Errorf("value is %q, want the last one saved", got.Value)
			}
		})
	}
}

func TestTenantsAndSourcesDoNotShareACursor(t *testing.T) {
	for name, cp := range checkpointStores(t) {
		t.Run(name, func(t *testing.T) {
			if err := cp.Save(t.Context(), "acme", "wiki", connector.Cursor{Value: "a"}); err != nil {
				t.Fatal(err)
			}
			if err := cp.Save(t.Context(), "acme", "chat", connector.Cursor{Value: "b"}); err != nil {
				t.Fatal(err)
			}
			if err := cp.Save(t.Context(), "other", "wiki", connector.Cursor{Value: "c"}); err != nil {
				t.Fatal(err)
			}

			for _, want := range []struct{ tenant, source, value string }{
				{"acme", "wiki", "a"},
				{"acme", "chat", "b"},
				{"other", "wiki", "c"},
			} {
				got, _ := cp.Load(t.Context(), want.tenant, want.source)
				if got.Value != want.value {
					t.Errorf("%s/%s is %q, want %q", want.tenant, want.source, got.Value, want.value)
				}
			}
		})
	}
}

// A source name is not a path. A connector called ../../etc must not be able to
// write a checkpoint outside the directory it was given.
func TestASourceNameCannotEscapeTheCheckpointDirectory(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "keep")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	cp, err := connector.NewFileCheckpoints(inside)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"../escaped", "../../escaped", "a/b/escaped", `..\escaped`} {
		if err := cp.Save(t.Context(), "acme", name, connector.Cursor{Value: "x"}); err != nil {
			t.Fatalf("save %q: %v", name, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "keep" {
			t.Errorf("a checkpoint was written outside the directory: %s", e.Name())
		}
	}
}

// The file version has to survive a crash mid write, which is what the rename
// is for. A half written file is a cursor pointing somewhere real that contains
// nothing, and resuming from it skips documents silently.
func TestAHalfWrittenCheckpointIsNeverVisible(t *testing.T) {
	dir := t.TempDir()
	cp, err := connector.NewFileCheckpoints(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Save(t.Context(), "acme", "wiki", connector.Cursor{Value: "good"}); err != nil {
		t.Fatal(err)
	}

	// Litter the directory with the temporary files an interrupted save would
	// have left behind, holding bytes that are not valid JSON.
	for i := range 5 {
		name := filepath.Join(dir, ".checkpoint-torn")
		if err := os.WriteFile(name+string(rune('a'+i)), []byte(`{"value":"trunc`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := cp.Load(t.Context(), "acme", "wiki")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Value != "good" {
		t.Errorf("value is %q, want the committed one", got.Value)
	}
}

func TestACheckpointDirectoryIsCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	if _, err := connector.NewFileCheckpoints(dir); err != nil {
		t.Fatalf("the directory should have been created: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory was not created: %v", err)
	}
	if _, err := connector.NewFileCheckpoints(""); err == nil {
		t.Error("an empty directory was accepted")
	}
}

func TestUnresolvedNeverAllowsAnybody(t *testing.T) {
	perm := connector.Unresolved("wiki")
	if perm.Mode != acl.ModeUnknown {
		t.Errorf("mode is %v, want ModeUnknown", perm.Mode)
	}
	// Including somebody the descriptor would otherwise name as the owner.
	p := &acl.Principal{Tenant: "acme", Subject: "alice", Kind: acl.KindUser}
	if perm.Allows(p) {
		t.Error("an unresolved descriptor allowed a read")
	}
}

func TestTheZeroCursorIsTheBeginning(t *testing.T) {
	if !(connector.Cursor{}).IsZero() {
		t.Error("the zero cursor does not report itself as zero")
	}
	if (connector.Cursor{Value: "x"}).IsZero() {
		t.Error("a cursor with a value reports itself as zero")
	}
	// A time alone is not a resume point, since nothing resumes from it.
	if !(connector.Cursor{Time: time.Now()}).IsZero() {
		t.Error("a cursor with only a time reports itself as usable")
	}
}

func checkpointStores(t *testing.T) map[string]connector.Checkpoints {
	t.Helper()
	files, err := connector.NewFileCheckpoints(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]connector.Checkpoints{
		"memory": connector.NewMemoryCheckpoints(),
		"file":   files,
	}
}
