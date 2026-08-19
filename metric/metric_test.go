package metric_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/tamnd/genba/metric"
)

// TestAHistogramIsCumulative covers the one part of the format that is easy to
// get wrong and impossible to notice: a bucket count includes every bucket
// below it. A Prometheus server reading non cumulative buckets does not error,
// it draws a wrong graph.
func TestAHistogramIsCumulative(t *testing.T) {
	r := metric.New()
	h := r.NewHistogram("genba_test_ms", "how long a thing took", "endpoint", []float64{1, 10, 100})
	for _, v := range []float64{0.5, 3, 3, 40, 4000} {
		h.Observe(v, "/search")
	}

	got := r.String()
	for _, want := range []string{
		`genba_test_ms_bucket{endpoint="/search",le="1"} 1`,
		`genba_test_ms_bucket{endpoint="/search",le="10"} 3`,
		`genba_test_ms_bucket{endpoint="/search",le="100"} 4`,
		`genba_test_ms_bucket{endpoint="/search",le="+Inf"} 5`,
		`genba_test_ms_count{endpoint="/search"} 5`,
		`genba_test_ms_sum{endpoint="/search"} 4046.5`,
		"# TYPE genba_test_ms histogram",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the exposition is missing %q:\n%s", want, got)
		}
	}
}

// TestAHistogramWithoutALabelHasNoBraces is the case a single series family
// hits, and an empty label set written as {} is a parse error rather than a
// cosmetic problem.
func TestAHistogramWithoutALabelHasNoBraces(t *testing.T) {
	r := metric.New()
	h := r.NewHistogram("genba_test_size", "how big a thing was", "", []float64{1, 10})
	h.Observe(5, "")

	got := r.String()
	if strings.Contains(got, "{}") {
		t.Errorf("a series carries an empty label set:\n%s", got)
	}
	if !strings.Contains(got, `genba_test_size_bucket{le="10"} 1`) {
		t.Errorf("the le label is missing:\n%s", got)
	}
	if !strings.Contains(got, "genba_test_size_count 1") {
		t.Errorf("the count is missing or labelled:\n%s", got)
	}
}

// TestALabelValueCannotBreakTheFormat matters because one of these values is a
// request path, and a path is somebody else's input.
func TestALabelValueCannotBreakTheFormat(t *testing.T) {
	r := metric.New()
	h := r.NewHistogram("genba_test_ms", "how long a thing took", "endpoint", []float64{1})
	h.Observe(1, "/a\"b\\c\nd")

	got := r.String()
	if !strings.Contains(got, `endpoint="/a\"b\\c\nd"`) {
		t.Errorf("the label value was not escaped:\n%s", got)
	}
	// Two header lines and four series. A raw newline would split a series in
	// half and give a scraper two lines it cannot parse.
	if lines := strings.Count(strings.TrimSuffix(got, "\n"), "\n") + 1; lines != 6 {
		t.Errorf("the exposition is %d lines, want 6, so something broke a series in half:\n%s", lines, got)
	}
}

// TestCountersAreReadAtScrapeTime is the property that lets the cache and the
// storage driver publish the counters their own tests already assert on,
// instead of the process counting everything twice.
func TestCountersAreReadAtScrapeTime(t *testing.T) {
	r := metric.New()
	live := map[string]float64{"results": 1}
	r.Counters("genba_test_hits_total", "cache hits", "layer", "counter", func() map[string]float64 {
		return live
	})

	if got := r.String(); !strings.Contains(got, `genba_test_hits_total{layer="results"} 1`) {
		t.Fatalf("the first scrape is wrong:\n%s", got)
	}
	live["results"] = 9
	live["documents"] = 4
	got := r.String()
	if !strings.Contains(got, `genba_test_hits_total{layer="results"} 9`) {
		t.Errorf("the second scrape did not re read the source:\n%s", got)
	}
	if !strings.Contains(got, `genba_test_hits_total{layer="documents"} 4`) {
		t.Errorf("a layer that appeared between scrapes is missing:\n%s", got)
	}
}

// TestAFamilyRegisteredTwiceIsFoundAtStartup, rather than at three in the
// morning when two meanings of one name are being averaged together.
func TestAFamilyRegisteredTwiceIsFoundAtStartup(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering the same name twice was accepted")
		}
	}()
	r := metric.New()
	r.NewHistogram("genba_test_ms", "first", "", []float64{1})
	r.NewHistogram("genba_test_ms", "second", "", []float64{1})
}

// TestObservingFromEveryGoroutineIsSafe, because that is the only way this is
// ever used: one handler per request, all writing the same family.
func TestObservingFromEveryGoroutineIsSafe(t *testing.T) {
	r := metric.New()
	h := r.NewHistogram("genba_test_ms", "how long a thing took", "endpoint", metric.Duration)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				h.Observe(float64(i), "/search")
			}
			_ = r.String()
		}()
	}
	wg.Wait()

	if !strings.Contains(r.String(), `genba_test_ms_count{endpoint="/search"} 1600`) {
		t.Errorf("observations were lost:\n%s", r.String())
	}
}

func TestPercentileIsTheNearestRank(t *testing.T) {
	samples := []float64{5, 1, 4, 2, 3, 10, 9, 8, 7, 6}
	for _, c := range []struct {
		q    float64
		want float64
	}{
		{0.5, 5},
		{0.95, 10},
		{0.99, 10},
		{1, 10},
	} {
		if got := metric.Percentile(samples, c.q); got != c.want {
			t.Errorf("Percentile(%v) = %v, want %v", c.q, got, c.want)
		}
	}
	if got := metric.Percentile(nil, 0.95); got != 0 {
		t.Errorf("Percentile of nothing = %v, want 0", got)
	}
}
