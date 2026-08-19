package benchcorpus_test

import (
	"path/filepath"
	"testing"

	"github.com/tamnd/genba/benchcorpus"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store/memstore"
	"github.com/tamnd/genba/store/sqlitestore"
)

// The corpus is the thing every performance number is measured against, so the
// properties asserted here are the ones that would silently invalidate a
// comparison: that the same seed produces the same documents, and that the
// benchmark principal can read roughly the share the budgets assume. A corpus
// that quietly became readable in full would make every measurement look good.

const sample = 2_000

func TestSameSeedSameCorpus(t *testing.T) {
	first := collect(t, benchcorpus.Default(2121, sample))
	second := collect(t, benchcorpus.Default(2121, sample))

	if len(first) != sample {
		t.Fatalf("generated %d documents, asked for %d", len(first), sample)
	}
	for i := range first {
		if first[i].ID != second[i].ID || first[i].Title != second[i].Title || first[i].Body != second[i].Body {
			t.Fatalf("document %d differs between two runs of the same seed", i)
		}
	}

	// And a different seed has to produce a different corpus, or the seed is
	// not doing anything and the reproducibility above is trivially true.
	other := collect(t, benchcorpus.Default(7, sample))
	if other[0].Body == first[0].Body {
		t.Fatal("two seeds produced the same first document")
	}
}

func TestPrincipalReadsTheStatedShare(t *testing.T) {
	spec := benchcorpus.Default(2121, sample)
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })
	if err := spec.Generate(t.Context(), st, nil); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var readable int
	if err := st.Scan(t.Context(), spec.Principal(), func(doc.Document) bool {
		readable++
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	share := float64(readable) / float64(spec.Documents)
	if share < spec.Readable-0.10 || share > spec.Readable+0.10 {
		t.Fatalf("the principal reads %.0f%% of the corpus and the spec says %.0f%%", share*100, spec.Readable*100)
	}

	// The stranger is in no group, so the only thing they can read is the
	// quarter that is public to the tenant. If this ever returns everything, the
	// permission predicate has stopped being part of what the benchmark
	// measures.
	var strangerReads int
	if err := st.Scan(t.Context(), spec.Stranger(), func(doc.Document) bool {
		strangerReads++
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := float64(strangerReads) / float64(spec.Documents); got < 0.20 || got > 0.30 {
		t.Fatalf("a stranger reads %.0f%% of the corpus, expected the quarter that is public to the tenant", got*100)
	}
}

// TestTermsAreZipf checks the shape of the vocabulary rather than its values. A
// uniform corpus is the failure mode that makes a search benchmark meaningless:
// every query costs the same and none of them cost what a real one does.
func TestTermsAreZipf(t *testing.T) {
	spec := benchcorpus.Default(2121, sample)
	docs := collect(t, spec)

	counts := map[string]int{}
	for _, d := range docs {
		for term := range d.Analyze().Terms {
			counts[term]++
		}
	}

	// The most common term is in most documents and the thousandth is in a few
	// percent of them, which is the spread the query classes are drawn against.
	if got := float64(counts[benchcorpus.Word(0)]) / float64(len(docs)); got < 0.80 {
		t.Fatalf("the most common term is in %.0f%% of documents, expected most of them", got*100)
	}
	if got := float64(counts[benchcorpus.Word(1_000)]) / float64(len(docs)); got > 0.25 {
		t.Fatalf("the thousandth term is in %.0f%% of documents, which is too flat a distribution", got*100)
	}
	if len(counts) < 5_000 {
		t.Fatalf("the corpus carries %d distinct terms, which is too uniform to measure anything", len(counts))
	}
}

func TestQueriesCoverEveryClass(t *testing.T) {
	byClass := benchcorpus.ByClass(benchcorpus.Queries())
	if len(byClass) == 0 {
		t.Fatal("the checked in query set is empty")
	}
	for class := range benchcorpus.Budget {
		if len(byClass[class]) == 0 {
			t.Fatalf("the query set has no %s queries, so that budget is never measured", class)
		}
	}
}

// TestGeneratesIntoSQLite is the smoke test for the driver the fixture actually
// uses, since memstore accepts documents a schema might not.
func TestGeneratesIntoSQLite(t *testing.T) {
	spec := benchcorpus.Default(2121, 500)
	st, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "bench.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := spec.Generate(t.Context(), st, nil); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	stats, err := st.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Documents != spec.Documents {
		t.Fatalf("stored %d documents, generated %d", stats.Documents, spec.Documents)
	}
	if stats.Quarantined != 0 {
		t.Fatalf("%d generated documents have permissions that did not resolve", stats.Quarantined)
	}
}

func collect(t *testing.T, spec benchcorpus.Spec) []doc.Document {
	t.Helper()
	docs := make([]doc.Document, 0, spec.Documents)
	spec.Each(func(d doc.Document) bool {
		docs = append(docs, d)
		return true
	})
	return docs
}
