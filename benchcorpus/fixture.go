package benchcorpus

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/tamnd/genba/store/sqlitestore"
)

// DefaultSeed is the seed every recorded number was measured at.
const DefaultSeed = 2121

// DefaultDocuments is how large the corpus is when nothing says otherwise.
//
// The budgets are stated against a hundred thousand documents and the gate
// measures that, which takes a minute or two to build. Twenty thousand is the
// default here because the common case is somebody running a benchmark while
// they change something, and a fixture that takes two minutes to appear is a
// fixture people stop using. Set GENBA_BENCH_DOCS to measure the real thing.
const DefaultDocuments = 20_000

// Fixture opens the benchmark corpus, building it first if it is not there.
//
// The database is cached under testdata and reused across runs, because
// generating it is the slowest thing in this repository by a wide margin and it
// is derived entirely from the seed, so the same seed and the same size is
// always the same corpus. Deleting the file is how it is rebuilt.
func Fixture(tb testing.TB) (*sqlitestore.Store, Spec) {
	tb.Helper()

	spec := Default(DefaultSeed, envInt("GENBA_BENCH_DOCS", DefaultDocuments))
	return FixtureOf(tb, spec), spec
}

// FixtureOf is [Fixture] for a corpus of a stated shape, which is what a test
// that needs a corpus rather than a measurement wants: the counter assertions
// need a few thousand documents and would rather not wait for a hundred
// thousand. Each shape is cached under its own name.
func FixtureOf(tb testing.TB, spec Spec) *sqlitestore.Store {
	tb.Helper()

	path, err := FixturePath(spec)
	if err != nil {
		tb.Fatalf("benchcorpus: %v", err)
	}

	ctx := context.Background()
	if _, err := os.Stat(path); err != nil {
		tb.Logf("generating %d documents into %s, this happens once", spec.Documents, path)
		if err := build(ctx, spec, path); err != nil {
			tb.Fatalf("benchcorpus: %v", err)
		}
	}

	st, err := sqlitestore.Open(ctx, path)
	if err != nil {
		tb.Fatalf("benchcorpus: %v", err)
	}
	tb.Cleanup(func() { _ = st.Close() })

	// A fixture that is there but half written measures nothing and says
	// nothing, so the count is checked rather than assumed.
	stats, err := st.Stats(ctx)
	if err != nil {
		tb.Fatalf("benchcorpus: %v", err)
	}
	if got := stats.Documents + stats.Quarantined; got != spec.Documents {
		tb.Fatalf("benchcorpus: %s holds %d documents and the spec says %d, delete it and rerun", path, got, spec.Documents)
	}
	return st
}

// FixturePath is where a corpus of this shape is cached.
func FixturePath(spec Spec) (string, error) {
	if p := os.Getenv("GENBA_BENCH_DB"); p != "" {
		return p, nil
	}
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	name := "bench-" + strconv.FormatUint(spec.Seed, 10) + "-" + strconv.Itoa(spec.Documents) + ".db"
	return filepath.Join(root, "testdata", name), nil
}

func build(ctx context.Context, spec Spec, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// The same temporary name and rename the generator command uses, for the
	// same reason: an interrupted build must not leave something that looks
	// like a corpus.
	tmp := path + ".partial"
	_ = os.Remove(tmp)

	st, err := sqlitestore.Open(ctx, tmp)
	if err != nil {
		return err
	}
	if err := spec.Generate(ctx, st, nil); err != nil {
		_ = st.Close()
		return err
	}
	if err := st.Close(); err != nil {
		return err
	}
	for _, ext := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(tmp + ext); err != nil {
			continue
		}
		if err := os.Rename(tmp+ext, path+ext); err != nil {
			return err
		}
	}
	return nil
}

// moduleRoot walks up for the go.mod, so the fixture lands in one place
// whichever package's benchmarks asked for it.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func envInt(name string, fallback int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}
