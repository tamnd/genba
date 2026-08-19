// Command gen writes the benchmark corpus and the benchmark query set.
//
//	go run ./benchcorpus/gen -seed 2121 -n 100000 -out testdata/bench.db
//	go run ./benchcorpus/gen -queries benchcorpus/queries.txt
//
// The database is not checked in. It is a few hundred megabytes, it is derived
// entirely from the seed, and CI restores it from a cache keyed on the hash of
// the generator rather than from the repository. The query set is checked in,
// because it is a thousand short lines and because a baseline is only
// comparable against the queries it was recorded with.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tamnd/genba/benchcorpus"
	"github.com/tamnd/genba/store/sqlitestore"
)

func main() {
	var (
		seed    = flag.Uint64("seed", 2121, "the seed the corpus is derived from")
		n       = flag.Int("n", 100_000, "how many documents to generate")
		out     = flag.String("out", "", "where to write the database")
		queries = flag.String("queries", "", "write the query set to this file instead of generating a corpus")
		count   = flag.Int("query-count", 1_000, "how many queries to write")
	)
	flag.Parse()

	spec := benchcorpus.Default(*seed, *n)
	if *queries != "" {
		if err := writeQueries(spec, *queries, *count); err != nil {
			fail(err)
		}
		return
	}
	if *out == "" {
		fail(fmt.Errorf("no -out and no -queries, so there is nothing to write"))
	}
	if err := writeCorpus(spec, *out); err != nil {
		fail(err)
	}
}

func writeCorpus(spec benchcorpus.Spec, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// A partial corpus is worse than none: it looks like a corpus and every
	// number measured against it is wrong. Writing to a temporary name and
	// renaming at the end means an interrupted run leaves nothing behind.
	tmp := path + ".partial"
	_ = os.Remove(tmp)

	ctx := context.Background()
	st, err := sqlitestore.Open(ctx, tmp)
	if err != nil {
		return err
	}

	start := time.Now()
	err = spec.Generate(ctx, st, func(done int) {
		if done%10_000 != 0 && done != spec.Documents {
			return
		}
		elapsed := time.Since(start)
		fmt.Fprintf(os.Stderr, "\r%d/%d documents, %.0f/s", done, spec.Documents, float64(done)/elapsed.Seconds())
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		_ = st.Close()
		return err
	}
	if err := st.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// The write ahead log and its index belong to the database, so a corpus
	// moved without them is a corpus missing whatever had not been checkpointed.
	for _, ext := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(tmp + ext); err == nil {
			if err := os.Rename(tmp+ext, path+ext); err != nil {
				return err
			}
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s, %d documents, %.1f MB, seed %d\n",
		path, spec.Documents, float64(info.Size())/(1<<20), spec.Seed)
	return nil
}

func writeQueries(spec benchcorpus.Spec, path string, count int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# %d benchmark queries, seed %d, class distribution from the budgets.\n", count, spec.Seed)
	fmt.Fprintf(&b, "# Regenerated with: go run ./benchcorpus/gen -queries %s\n", path)
	fmt.Fprintf(&b, "# Regenerating this file invalidates every recorded baseline.\n")
	for _, q := range spec.BuildQueries(count) {
		fmt.Fprintf(&b, "%s\t%s\n", q.Class, q.Text)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s, %d queries, seed %d\n", path, count, spec.Seed)
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "benchcorpus:", err)
	os.Exit(1)
}
