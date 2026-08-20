package vector_test

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tamnd/genba/store/column"
	"github.com/tamnd/genba/store/vector"
)

// TestTheScanConsidersEveryRow is what "recall is exact by definition" means in
// practice. A flat scan has no candidate list to get wrong, so asking for as
// many results as there are rows returns every row that carries a vector, in
// descending order, with nothing missing and nothing counted twice.
func TestTheScanConsidersEveryRow(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 20))
	const dim, rows = 96, 2000

	b := builder(t, dim, vector.KindFlat)
	for range rows {
		if err := b.Append(random(rnd, dim)); err != nil {
			t.Fatalf("appending: %v", err)
		}
	}
	s := open(t, b)

	got := search(t, s, random(rnd, dim), rows, nil)
	if len(got) != rows {
		t.Fatalf("asking for %d results over %d rows returned %d", rows, rows, len(got))
	}
	seen := make(map[int]bool, rows)
	for i, m := range got {
		if seen[m.Row] {
			t.Fatalf("row %d came back twice", m.Row)
		}
		seen[m.Row] = true
		if i > 0 && got[i-1].Score < m.Score {
			t.Fatalf("result %d scored %f and the one before it scored %f, which is out of order", i, m.Score, got[i-1].Score)
		}
	}
	for row := range rows {
		if !seen[row] {
			t.Fatalf("row %d was never scored", row)
		}
	}
}

// TestQuantisationDoesNotReorderClearResults is the box on the issue.
//
// Quantisation is lossy, so two documents that score within the noise of each
// other can swap places, and that is fine: nothing downstream can tell them
// apart either. What must not happen is a pair with a real gap between them
// coming back the wrong way round, because that is a relevant document losing
// to an irrelevant one.
//
// So this compares the whole returned order against the exact cosines and
// fails on any inversion wider than the margin.
func TestQuantisationDoesNotReorderClearResults(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 21))
	const dim, rows = 768, 1500

	// A tenth of a percent of the score range, which is a hundred times the
	// error the quantisation actually produces at this width and still far
	// finer than any difference a person could see in a result list.
	const margin = 0.01

	vs := make([][]float32, rows)
	b := builder(t, dim, vector.KindFlat)
	for i := range vs {
		vs[i] = random(rnd, dim)
		if err := b.Append(vs[i]); err != nil {
			t.Fatalf("appending: %v", err)
		}
	}
	s := open(t, b)

	q := random(rnd, dim)
	exact := make([]float64, rows)
	for i, v := range vs {
		exact[i] = cosine(q, v)
	}

	got := search(t, s, q, rows, nil)
	if len(got) != rows {
		t.Fatalf("got %d results over %d rows", len(got), rows)
	}
	inversions := 0
	for i := range got {
		for j := i + 1; j < len(got); j++ {
			if exact[got[j].Row]-exact[got[i].Row] > margin {
				inversions++
				if inversions == 1 {
					t.Errorf("row %d came back at position %d with an exact cosine of %.6f, ahead of row %d at position %d with %.6f",
						got[i].Row, i, exact[got[i].Row], got[j].Row, j, exact[got[j].Row])
				}
			}
		}
	}
	if inversions > 0 {
		t.Errorf("%d pairs separated by more than %.3f came back in the wrong order", inversions, margin)
	}
}

// TestScoresTrackTheCosine measures the thing the ordering above depends on. It
// is a separate test because a number worth relying on is a number worth
// printing: the log line is what says how much room the margin has.
func TestScoresTrackTheCosine(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 22))
	for _, dim := range []int{64, 384, 768} {
		const rows = 500
		vs := make([][]float32, rows)
		b := builder(t, dim, vector.KindFlat)
		for i := range vs {
			vs[i] = random(rnd, dim)
			if err := b.Append(vs[i]); err != nil {
				t.Fatalf("appending: %v", err)
			}
		}
		s := open(t, b)

		q := random(rnd, dim)
		got := search(t, s, q, rows, nil)

		worst := 0.0
		for _, m := range got {
			if e := math.Abs(float64(m.Score) - cosine(q, vs[m.Row])); e > worst {
				worst = e
			}
		}
		t.Logf("%d dimensions: the worst score is off the exact cosine by %.6f", dim, worst)
		if worst > 0.01 {
			t.Errorf("%d dimensions: the worst score is off by %.6f, want under 0.01", dim, worst)
		}
	}
}

// TestTheScanNeverScoresARowTheReaderMayNotSee is the second box on the issue.
//
// The bitmap is deliberately the complement of the best matches, so a scan that
// filtered afterwards rather than as it went would still pass a test that only
// checked the rows it returned. This one checks the whole answer against the
// top of the allowed rows computed separately, so a result that came from
// anywhere else fails.
func TestTheScanNeverScoresARowTheReaderMayNotSee(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 23))
	const dim, rows, k = 128, 1200, 20

	vs := make([][]float32, rows)
	b := builder(t, dim, vector.KindFlat)
	for i := range vs {
		vs[i] = random(rnd, dim)
		if err := b.Append(vs[i]); err != nil {
			t.Fatalf("appending: %v", err)
		}
	}
	s := open(t, b)

	q := random(rnd, dim)
	exact := make([]float64, rows)
	order := make([]int, rows)
	for i, v := range vs {
		exact[i] = cosine(q, v)
		order[i] = i
	}
	slices.SortFunc(order, func(a, c int) int {
		switch {
		case exact[a] > exact[c]:
			return -1
		case exact[a] < exact[c]:
			return 1
		}
		return a - c
	})

	// Hide the fifty nearest documents, which is the case a filter applied
	// after the scan gets wrong in the most visible way.
	hidden := make(map[int]bool, 50)
	for _, row := range order[:50] {
		hidden[row] = true
	}
	allow := column.NewBitmap(rows)
	var want []int
	for _, row := range order {
		if hidden[row] {
			continue
		}
		allow.Set(row)
		if len(want) < k {
			want = append(want, row)
		}
	}

	got := search(t, s, q, k, allow)
	if len(got) != k {
		t.Fatalf("got %d results, want %d", len(got), k)
	}
	for _, m := range got {
		if hidden[m.Row] {
			t.Fatalf("row %d was hidden from the reader and came back anyway", m.Row)
		}
	}

	// The cut is where the twentieth allowed document sits, and the margin is
	// the quantisation noise around it. Two documents either side of the cut
	// and within the noise of each other can trade places, which is a swap
	// nothing downstream can see. A document from outside that band cannot be
	// there at all, and one from well inside it cannot be missing.
	const margin = 0.005
	cut := exact[want[k-1]]
	in := make(map[int]bool, k)
	for _, m := range got {
		in[m.Row] = true
		if exact[m.Row] < cut-margin {
			t.Errorf("row %d came back at cosine %.6f, below the twentieth allowed document at %.6f", m.Row, exact[m.Row], cut)
		}
	}
	for _, row := range want {
		if !in[row] && exact[row] > cut+margin {
			t.Errorf("row %d at cosine %.6f is clear of the cut at %.6f and did not come back", row, exact[row], cut)
		}
	}
}

func TestAReaderWhoMaySeeNothingGetsNothing(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 24))
	const dim, rows = 32, 200

	b := builder(t, dim, vector.KindFlat)
	for range rows {
		if err := b.Append(random(rnd, dim)); err != nil {
			t.Fatalf("appending: %v", err)
		}
	}
	s := open(t, b)

	if got := search(t, s, random(rnd, dim), 10, column.NewBitmap(rows)); len(got) != 0 {
		t.Errorf("an empty permission bitmap returned %d matches, want none", len(got))
	}
}

// TestABitmapFromALongerSegmentStopsAtTheEnd covers the bitmap that was built
// against a segment with more rows than this section has, which happens the
// moment a section is written for a subset of a segment's documents.
func TestABitmapFromALongerSegmentStopsAtTheEnd(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 25))
	const dim, rows = 32, 50

	b := builder(t, dim, vector.KindFlat)
	for range rows {
		if err := b.Append(random(rnd, dim)); err != nil {
			t.Fatalf("appending: %v", err)
		}
	}
	s := open(t, b)

	allow := column.NewBitmap(rows * 4)
	for row := range rows * 4 {
		allow.Set(row)
	}
	got := search(t, s, random(rnd, dim), rows*4, allow)
	if len(got) != rows {
		t.Fatalf("got %d matches over %d rows, want %d", len(got), rows, rows)
	}
	for _, m := range got {
		if m.Row >= rows {
			t.Fatalf("row %d is past the end of a section with %d rows", m.Row, rows)
		}
	}
}

// TestATieIsBrokenByTheRow keeps the output stable. Two documents that are
// genuinely the same distance from a query are common, and a result list that
// reorders them between two identical requests is a result list that looks
// broken to anybody paging through it.
func TestATieIsBrokenByTheRow(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 26))
	const dim = 64

	same := random(rnd, dim)
	b := builder(t, dim, vector.KindFlat)
	for row := range 12 {
		v := random(rnd, dim)
		if row == 5 || row == 9 {
			v = same
		}
		if err := b.Append(v); err != nil {
			t.Fatalf("appending: %v", err)
		}
	}
	s := open(t, b)

	got := search(t, s, same, 2, nil)
	if len(got) != 2 {
		t.Fatalf("got %d matches, want 2", len(got))
	}
	if got[0].Score != got[1].Score {
		t.Fatalf("the two copies scored %f and %f, which is not a tie", got[0].Score, got[1].Score)
	}
	if got[0].Row != 5 || got[1].Row != 9 {
		t.Errorf("a tie came back as rows %d and %d, want 5 then 9", got[0].Row, got[1].Row)
	}
}

func TestKBoundsTheResult(t *testing.T) {
	rnd := rand.New(rand.NewPCG(2121, 27))
	const dim, rows = 32, 100

	b := builder(t, dim, vector.KindFlat)
	for range rows {
		if err := b.Append(random(rnd, dim)); err != nil {
			t.Fatalf("appending: %v", err)
		}
	}
	s := open(t, b)
	q := random(rnd, dim)

	for _, k := range []int{-1, 0, 1, 7, rows, rows * 2} {
		got := search(t, s, q, k, nil)
		want := min(max(k, 0), rows)
		if len(got) != want {
			t.Errorf("k of %d returned %d matches, want %d", k, len(got), want)
		}
	}
}

// search is Search with the query built and the errors handled once.
func search(tb testing.TB, s vector.Index, v []float32, k int, allow *column.Bitmap) []vector.Match {
	tb.Helper()
	q, err := vector.NewQuery(v)
	if err != nil {
		tb.Fatalf("building a query: %v", err)
	}
	got, err := s.Search(q, k, allow)
	if err != nil {
		tb.Fatalf("searching: %v", err)
	}
	return got
}
