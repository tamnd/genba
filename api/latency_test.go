package api_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/benchcorpus"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/metric"
)

// The latency half of the performance gate.
//
// The counter assertions in the index package catch a query that starts doing
// more work. They cannot catch a change that does the same amount of work more
// slowly, and that is most of them: a lock held across a decode, an allocation
// in a loop, a comparison that used to be a byte compare and is now a regexp.
// Those show up in the wall clock and nowhere else.
//
// It measures the endpoint rather than the searcher, because the budget is
// stated against what a browser waits for. Between the two there is header
// parsing, query parsing, snippet building and JSON encoding, and every one of
// them has been the surprise in somebody's latency budget at some point.
//
// A wall clock gate on a shared runner is the thing everybody warns about, so
// this is built to be believed rather than to be strict, and what it compares
// was chosen by measuring the same commit twice rather than by reasoning about
// it. Two runs of an unchanged tree on a contended laptop disagreed by a factor
// of two on the p95 of the most expensive class. The median of the quietest
// round of those same runs held to within a quarter, and to within a tenth for
// half the classes. So the number that fails a build is the quietest median,
// the tolerance is set above what an unchanged tree was measured to do rather
// than at a number that sounds strict, and the percentiles are recorded and
// printed for a human to read and never compared.
//
// That is a real limit and it is worth being plain about: a regression that
// only moves the tail will not fail this gate. Nothing that runs on a shared
// machine can catch that, and a check that pretends otherwise is a check that
// goes red on a Tuesday for no reason and gets deleted on the Wednesday.
//
// The caches are off. A gate that measured them would mostly be measuring the
// hit rate of a synthetic query set, which is not a number anybody should be
// making decisions about. What is measured here is what a cache miss costs,
// which is what the caches are hiding and what regresses underneath them.

const (
	// gateWarmup is the queries run per class before any timer starts. The
	// budgets are stated warm: a process that has been up for a while, with the
	// working set in the page cache. A cold measurement measures the disk.
	gateWarmup = 100

	// gateSpend is roughly how long one class is allowed to take being
	// measured, which is what sets the sample count. It is spent rather than
	// timed: the number of samples is derived from the budget so that it is the
	// same on every machine, because a sample count that depended on the clock
	// would make two runs incomparable.
	gateSpend = 15 * time.Second

	// The sample count is held between these. Below two hundred a round holds
	// too few samples for its median to settle, and above a thousand the gate
	// stops being something anybody is willing to wait for.
	gateMinSamples = 200
	gateMaxSamples = 1000

	// gateRounds is how many times the whole class list is measured.
	//
	// The classes are interleaved rather than each measured to completion, so a
	// burst of load lands across all of them rather than in whichever one was
	// unlucky, and every round runs the same queries in the same order so that
	// two rounds differ only in when they ran.
	gateRounds = 8

	// gateTolerance is how far the quietest median may move before it is a
	// regression. It is where it is because an unchanged tree measured twice on
	// a contended laptop moved by a quarter, and a threshold below what the
	// code does when nothing changed is a threshold that fails honest work.
	gateTolerance = 1.35

	// gateBackstop is the multiple of the stated budget that fails on its own,
	// with no baseline involved. Twice the budget is not a busy runner.
	gateBackstop = 2

	// gateReportMove is the move worth mentioning in the report, in either
	// direction. An improvement that nobody notices is an improvement that gets
	// undone.
	gateReportMove = 0.10

	// gateCalibrationSpread is how far apart the calibration figures may be
	// before two runs stop being comparable at all. A runner half the speed of
	// the one the baseline came from is not a runner whose numbers can be
	// scaled into agreement.
	gateCalibrationSpread = 2.0

	// gateCalibrationBand is how far apart they have to be before the
	// correction is applied at all. Inside it the two machines are the same
	// machine and the difference is the calibration's own noise.
	gateCalibrationBand = 1.10
)

// baselineFile is where the recorded numbers live.
//
// It sits next to the corpus generator rather than under testdata, because
// testdata is a cache directory that CI restores over the checkout, and a
// baseline that a stale cache can overwrite is a baseline that quietly stops
// meaning anything.
//
// GENBA_GATE_BASELINE points at another one. A number is a property of the
// machine that produced it, so the runner keeps its own file and a laptop keeps
// the one checked in here, rather than the two being averaged into something
// that describes neither. It is opened as given, and a test runs in its own
// package directory, so what belongs in that variable is an absolute path.
const baselineFile = "../benchcorpus/baseline.json"

func gatePath() string {
	if p := os.Getenv("GENBA_GATE_BASELINE"); p != "" {
		return p
	}
	return baselineFile
}

// gateBaseline is one recorded run.
type gateBaseline struct {
	// Seed and Documents identify the corpus. Numbers taken against a different
	// corpus are not comparable and the gate says so rather than dividing them.
	Seed      uint64 `json:"seed"`
	Documents int    `json:"documents"`

	// Recorded and Runner are for the person reading a failure. Neither is used
	// in a comparison.
	Recorded string `json:"recorded"`
	Runner   string `json:"runner"`

	// Calibration is how long the fixed workload in gateCalibrate took, in
	// milliseconds. It is how a slower machine is told apart from a slower
	// query.
	Calibration float64 `json:"calibration_ms"`

	Classes map[string]gateReading `json:"classes"`
}

// gateReading is one class of one run.
type gateReading struct {
	Samples int `json:"samples"`
	Rounds  int `json:"rounds"`

	// Best is the median of the quietest round, in milliseconds, and it is the
	// only figure the gate compares. Ambient load can only ever make a round
	// slower, so the lowest of them is the closest this machine got to
	// measuring the code on its own.
	Best float64 `json:"best_ms"`

	// P50, P95 and P99 are over every sample of every round. They are the
	// report. They are not compared, because on a shared machine they are not
	// reproducible enough to be compared honestly.
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`

	Budget float64 `json:"budget_ms"`
}

// TestSearchLatencyGate measures every query class and compares it with the
// recorded baseline.
//
// Recording a new baseline is GENBA_GATE_RECORD=1, and it belongs in a commit
// of its own that changes nothing else, because a commit that moves the
// baseline and the code together is a commit that cannot be reviewed.
func TestSearchLatencyGate(t *testing.T) {
	if testing.Short() {
		t.Skip("the latency gate needs the benchmark corpus")
	}

	st, spec := benchcorpus.Fixture(t)
	// A fixed clock, because the recency prior is part of the score, and a
	// score that moves with the wall clock changes which documents are ranked
	// and therefore how long ranking them takes.
	searcher := index.New(st, index.WithClock(func() time.Time { return benchcorpus.Epoch }))
	t.Cleanup(func() { _ = searcher.Close() })
	h := api.New(st, searcher, api.HeaderAuth{Tenant: benchcorpus.Tenant}).Handler()
	hdr := headers(spec.Principal())

	byClass := benchcorpus.ByClass(benchcorpus.Queries())
	classes := gateClasses(byClass)

	// Every class is warmed before any of them is timed, because the rounds
	// interleave them and a class warmed in the first round would be measured
	// cold in it.
	targets := make(map[benchcorpus.Class][]string, len(classes))
	perRound := make(map[benchcorpus.Class]int, len(classes))
	for _, class := range classes {
		queries := byClass[class]
		list := make([]string, len(queries))
		for i, q := range queries {
			list[i] = "/api/v1/search?q=" + urlQuery(q.Text)
		}
		targets[class] = list
		perRound[class] = max(gateSampleCount(benchcorpus.Budget[class])/gateRounds, 1)
		for i := range gateWarmup {
			gateGet(t, h, list[i%len(list)], hdr)
		}
	}

	// The calibration is taken between the rounds rather than once at the
	// start. A single reading describes the first instant of a run that lasts
	// minutes, and the way a shared machine actually behaves is that the load
	// arrives halfway through. That is a run whose numbers are slow and whose
	// calibration says the machine was idle, which is the one case this must
	// not get wrong.
	cal := []float64{gateCalibrate()}
	took := make(map[benchcorpus.Class][]float64, len(classes))
	best := make(map[benchcorpus.Class]float64, len(classes))
	round := make([]float64, 0, gateMaxSamples)

	for range gateRounds {
		for _, class := range classes {
			list := targets[class]
			round = round[:0]
			for i := range perRound[class] {
				// The request is built before the timer starts and the response
				// is dropped after it stops, so what is timed is the endpoint
				// and nothing around it. The query is picked by position within
				// the round rather than by a running count, so every round
				// measures the same queries.
				target := list[i%len(list)]
				start := time.Now()
				code := gateServe(h, target, hdr)
				ms := float64(time.Since(start).Microseconds()) / 1000
				if code != http.StatusOK {
					t.Fatalf("GET %s = %d", target, code)
				}
				round = append(round, ms)
				took[class] = append(took[class], ms)
			}
			if median := metric.Percentile(round, 0.50); best[class] == 0 || median < best[class] {
				best[class] = median
			}
		}
		cal = append(cal, gateCalibrate())
	}

	now := gateBaseline{
		Seed:        spec.Seed,
		Documents:   spec.Documents,
		Runner:      os.Getenv("GENBA_GATE_RUNNER"),
		Recorded:    os.Getenv("GENBA_GATE_DATE"),
		Calibration: metric.Percentile(cal, 0.50),
		Classes:     make(map[string]gateReading, len(classes)),
	}
	for _, class := range classes {
		samples := took[class]
		now.Classes[string(class)] = gateReading{
			Samples: len(samples),
			Rounds:  gateRounds,
			Best:    best[class],
			P50:     metric.Percentile(samples, 0.50),
			P95:     metric.Percentile(samples, 0.95),
			P99:     metric.Percentile(samples, 0.99),
			Budget:  float64(benchcorpus.Budget[class].Microseconds()) / 1000,
		}
	}
	t.Logf("calibration %.2fms, from %d readings spread through the run, lowest %.2fms and highest %.2fms",
		now.Calibration, len(cal), slices.Min(cal), slices.Max(cal))

	if os.Getenv("GENBA_GATE_RECORD") != "" {
		gateWrite(t, gatePath(), now)
		t.Logf("recorded a new baseline in %s, commit it on its own", gatePath())
		return
	}
	if path := os.Getenv("GENBA_GATE_REPORT"); path != "" {
		gateWrite(t, path, now)
	}

	base, ok := gateRead(t, gatePath())
	failures, notes := gateCompare(now, base, ok, gateToleranceOf())
	t.Log("\n" + gateTable(now, base, ok))
	for _, n := range notes {
		t.Log(n)
	}
	for _, f := range failures {
		t.Error(f)
	}
}

// gateServe issues one request and returns the status, doing as little as it
// can either side of the handler because everything it does lands inside the
// measurement.
func gateServe(h http.Handler, target string, hdr map[string]string) int {
	r := httptest.NewRequest(http.MethodGet, target, http.NoBody)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// gateGet is gateServe for the warmup, where a failure is worth reporting
// before four thousand more requests are sent at it.
func gateGet(t *testing.T, h http.Handler, target string, hdr map[string]string) {
	t.Helper()
	if code := gateServe(h, target, hdr); code != http.StatusOK {
		t.Fatalf("GET %s = %d", target, code)
	}
}

// gateClasses is the classes present in the query set, in a fixed order, so
// that two runs measure the same thing in the same sequence.
func gateClasses(byClass map[benchcorpus.Class][]benchcorpus.Query) []benchcorpus.Class {
	out := make([]benchcorpus.Class, 0, len(byClass))
	for class, queries := range byClass {
		if len(queries) > 0 {
			out = append(out, class)
		}
	}
	slices.Sort(out)
	return out
}

// gateSampleCount derives how many queries a class is measured over from its
// budget, so the expensive classes do not run for ten minutes and the cheap
// ones still produce a median worth reading. It is the total across the rounds.
//
// GENBA_GATE_SAMPLES overrides it and is taken at face value, bounds and all.
// That is for the nightly run, which is on a machine of its own with time to
// spend.
func gateSampleCount(budget time.Duration) int {
	if n := gateEnvInt("GENBA_GATE_SAMPLES"); n > 0 {
		return n
	}
	if budget <= 0 {
		return gateMaxSamples
	}
	return min(max(int(gateSpend/(gateBackstop*budget)), gateMinSamples), gateMaxSamples)
}

// gateToleranceOf is how far a class may move before it is a regression.
//
// GENBA_GATE_TOLERANCE overrides it, and the nightly run tightens it. That run
// is on a dedicated machine, so it can afford to notice a move that would be
// noise on a shared runner, and it files an issue rather than failing anything.
func gateToleranceOf() float64 {
	if v := gateEnvFloat("GENBA_GATE_TOLERANCE"); v > 0 {
		return v
	}
	return gateTolerance
}

func gateEnvInt(name string) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return 0
	}
	return n
}

func gateEnvFloat(name string) float64 {
	f, err := strconv.ParseFloat(os.Getenv(name), 64)
	if err != nil {
		return 0
	}
	return f
}

// gateCalibrate is how long a fixed unit of work takes on this machine right
// now, in milliseconds. It is called between the rounds, and the median of
// those readings is what the run is scaled by.
//
// It is deliberately not a search. Its job is to say how fast the machine is
// today, and a calibration that ran the code under test would move with the
// regression it is supposed to be correcting for, which is how a gate ends up
// approving anything as long as everything got slower together.
//
// It does have to be shaped like a search, though, and the first version was
// not. It hashed a buffer that fit in cache, which is pure arithmetic, and on a
// contended laptop it reported the machine getting slower across two runs where
// every search class got faster. What a search spends its time on is
// allocating, sorting and hashing its way through a few megabytes, so that is
// what this does, and it is the memory system rather than the arithmetic units
// that it asks about.
func gateCalibrate() float64 {
	const n = 1 << 17
	runs := make([]float64, 5)
	for i := range runs {
		start := time.Now()
		xs := make([]int64, n)
		// A linear congruential sequence, because the values need to be
		// scattered and the run needs to be identical on every machine, and the
		// standard generator is neither seeded the same way twice nor free.
		x := int64(1)
		for j := range xs {
			x = x*6364136223846793005 + 1442695040888963407
			xs[j] = x >> 33
		}
		slices.Sort(xs)
		seen := make(map[int64]int, n/4)
		for j := 0; j < n; j += 4 {
			seen[xs[j]] = j
		}
		for j := 1; j < n; j += 4 {
			if _, ok := seen[xs[j]]; ok {
				gateSink++
			}
		}
		gateSink += len(seen)
		runs[i] = float64(time.Since(start).Microseconds()) / 1000
	}
	// The median of five, because the calibration runs on the same busy machine
	// as everything else and one interrupted run should not decide what the
	// whole comparison is scaled by.
	return metric.Percentile(runs, 0.50)
}

// gateSink keeps the calibration from being optimised away.
var gateSink int

// gateCompare is the whole judgement, kept apart from the measurement so that
// it can be tested with numbers instead of with a corpus.
func gateCompare(now, base gateBaseline, haveBase bool, tol float64) (failures, notes []string) {
	// The backstop, which is the one absolute number in here. A class the
	// recorded baseline already misses is exempt from it and is held to the
	// baseline instead: the budgets are what the query path is aiming at rather
	// than what it has hit, three of them are missed today and that is written
	// down in benchcorpus/BASELINE.md, and a gate that failed every pull request
	// until they are met is a gate that gets deleted in a week. The exemption
	// expires on its own the moment a baseline inside the budget is recorded.
	//
	// A machine with no baseline at all gets no backstop either, because the
	// exemption is read out of the baseline and without one every class that is
	// merely aiming at its budget looks like a regression. The budgets were
	// stated against a laptop and a two core runner is several times slower than
	// one, so an absolute millisecond applied to a machine nothing has
	// characterised is the flake the rest of this design exists to avoid. Record
	// a baseline on the machine and everything below turns on.
	if !haveBase {
		return nil, []string{"there is no baseline for this machine, so the numbers above were recorded and nothing was enforced, and " +
			gatePath() + " is where one produced by make bench-gate-record belongs"}
	}

	for _, class := range slices.Sorted(maps.Keys(now.Classes)) {
		got := now.Classes[class]
		was, recorded := base.Classes[class]
		switch {
		case got.Budget <= 0 || got.Best <= got.Budget*gateBackstop:
		case recorded && was.Budget > 0 && was.Best > was.Budget:
			notes = append(notes, fmt.Sprintf(
				"%s is over its budget of %.1fms at %.1fms, and the baseline records %.1fms, so it is held to the baseline until the budget is met",
				class, got.Budget, got.Best, was.Best))
		default:
			failures = append(failures, fmt.Sprintf(
				"%s is %.1fms against a budget of %.1fms, which is past the backstop of %dx and is not a busy runner",
				class, got.Best, got.Budget, gateBackstop))
		}
	}

	scale, ok, why := gateScale(now, base, haveBase)
	if !ok {
		return failures, append(notes, why+", so only the absolute backstop was applied")
	}
	if why != "" {
		notes = append(notes, why)
	}

	for _, class := range slices.Sorted(maps.Keys(now.Classes)) {
		got, was := now.Classes[class], base.Classes[class]
		if was.Best == 0 {
			notes = append(notes, class+" is not in the baseline, so it was only held to the backstop")
			continue
		}
		best := got.Best * scale
		if best > was.Best*tol {
			failures = append(failures, fmt.Sprintf(
				"%s is %.1fms normalised against a baseline of %.1fms, a move of %+.0f%% and the tolerance is %+.0f%%",
				class, best, was.Best, 100*(best/was.Best-1), 100*(tol-1)))
		}
		if move := best/was.Best - 1; move >= gateReportMove || move <= -gateReportMove {
			notes = append(notes, fmt.Sprintf("%s moved %+.0f%%, from %.1fms to %.1fms normalised", class, 100*move, was.Best, best))
		}
	}
	return failures, notes
}

// gateScale is the factor that turns this machine's milliseconds into the
// baseline machine's, along with why it cannot be trusted when it cannot.
func gateScale(now, base gateBaseline, haveBase bool) (scale float64, ok bool, why string) {
	switch {
	case !haveBase || len(base.Classes) == 0:
		return 0, false, "there is no recorded baseline"
	case base.Seed != now.Seed || base.Documents != now.Documents:
		return 0, false, fmt.Sprintf("the baseline was recorded against seed %d at %d documents and this run is seed %d at %d",
			base.Seed, base.Documents, now.Seed, now.Documents)
	case base.Calibration <= 0 || now.Calibration <= 0:
		return 1, true, "there is no calibration figure, so the numbers are compared unscaled"
	}
	scale = base.Calibration / now.Calibration
	switch {
	case scale > gateCalibrationSpread || scale < 1/gateCalibrationSpread:
		return 0, false, fmt.Sprintf("this machine calibrates at %.2fms against the baseline's %.2fms, which is too far apart to scale into agreement",
			now.Calibration, base.Calibration)
	// The deadband. The calibration is a proxy and a proxy that is a tenth
	// apart from where it was is saying nothing, so correcting by it would be
	// putting its noise into every class rather than taking the machine's speed
	// out of them.
	case scale > 1/gateCalibrationBand && scale < gateCalibrationBand:
		return 1, true, ""
	}
	return scale, true, fmt.Sprintf("this machine calibrates at %.2fms against the baseline's %.2fms, so the measurements are scaled by %.2f",
		now.Calibration, base.Calibration, scale)
}

// gateTable is the report, as markdown, because it ends up in a CI log that
// somebody has to read at the point they are least inclined to.
func gateTable(now, base gateBaseline, haveBase bool) string {
	var b strings.Builder
	b.WriteString("| class | samples | best | p50 | p95 | p99 | budget | baseline best |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, class := range slices.Sorted(maps.Keys(now.Classes)) {
		got := now.Classes[class]
		was := "not recorded"
		if haveBase {
			if r, ok := base.Classes[class]; ok {
				was = fmt.Sprintf("%.1fms", r.Best)
			}
		}
		fmt.Fprintf(&b, "| %s | %d | %.1fms | %.1fms | %.1fms | %.1fms | %.1fms | %s |\n",
			class, got.Samples, got.Best, got.P50, got.P95, got.P99, got.Budget, was)
	}
	b.WriteString("\nbest is the median of the quietest of ")
	fmt.Fprintf(&b, "%d rounds and is the only column compared, and the percentiles are over every sample of every round\n", gateRounds)
	return b.String()
}

func gateRead(t *testing.T, path string) (gateBaseline, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return gateBaseline{}, false
	}
	if err != nil {
		t.Fatalf("reading the baseline: %v", err)
	}
	var b gateBaseline
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("the baseline in %s does not parse: %v", path, err)
	}
	return b, true
}

func gateWrite(t *testing.T, path string, b gateBaseline) {
	t.Helper()
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatalf("encoding the baseline: %v", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// TestGateComparisonIsHonest covers the judgement with numbers, so that the
// gate's own logic is not something only a slow corpus run can exercise.
func TestGateComparisonIsHonest(t *testing.T) {
	reading := func(best float64) map[string]gateReading {
		return map[string]gateReading{"common": {
			Samples: 1000, Rounds: gateRounds, Best: best,
			P50: best, P95: best * 3, P99: best * 4, Budget: 10,
		}}
	}
	base := gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 10, Classes: reading(8)}

	tests := []struct {
		name string
		now  gateBaseline
		want string
	}{
		{"a run that matches the baseline",
			gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 10, Classes: reading(8)}, ""},
		{"a move inside the tolerance",
			gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 10, Classes: reading(9.5)}, ""},
		{"a move past the tolerance",
			gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 10, Classes: reading(11)}, "tolerance"},
		{"a slow machine measured the same query path",
			// Twice the calibration and twice the latency is the same code on
			// half the machine, and the gate has to stay quiet.
			gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 20, Classes: reading(16)}, ""},
		{"a slow machine and a real regression",
			gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 20, Classes: reading(24)}, "tolerance"},
		{"past the backstop on a class the baseline meets",
			gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 10, Classes: reading(25)}, "backstop"},
		{"a tail that moved and a median that did not",
			// The one the percentiles would have failed and this deliberately
			// does not, because two runs of an unchanged tree move the tail by
			// more than this.
			gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 10, Classes: map[string]gateReading{
				"common": {Samples: 1000, Rounds: gateRounds, Best: 8, P50: 9, P95: 60, P99: 90, Budget: 10},
			}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failures, _ := gateCompare(tt.now, base, true, gateTolerance)
			switch {
			case tt.want == "" && len(failures) > 0:
				t.Fatalf("the gate failed a run it should have passed: %v", failures)
			case tt.want == "":
				return
			case len(failures) == 0:
				t.Fatalf("the gate passed a run it should have failed")
			}
			if !strings.Contains(strings.Join(failures, "\n"), tt.want) {
				t.Fatalf("the failure does not mention %q: %v", tt.want, failures)
			}
		})
	}

	// A class the baseline already misses is held to the baseline rather than to
	// the budget, and is still held to something.
	overBudget := gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 10, Classes: reading(22)}
	if failures, notes := gateCompare(overBudget, overBudget, true, gateTolerance); len(failures) > 0 {
		t.Errorf("a class the baseline itself misses failed the backstop: %v", failures)
	} else if !strings.Contains(strings.Join(notes, "\n"), "over its budget") {
		t.Errorf("the report does not say the class is over its budget: %v", notes)
	}
	if failures, _ := gateCompare(gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 10, Classes: reading(30)},
		overBudget, true, gateTolerance); len(failures) == 0 {
		t.Error("a class exempt from the backstop stopped being held to its baseline as well")
	}

	// A machine with no baseline is a machine nothing is known about. It reports
	// and it does not fail, because the budgets describe a laptop and a runner
	// several times slower than one is not a regression.
	slow := gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 40, Classes: reading(80)}
	if failures, notes := gateCompare(slow, gateBaseline{}, false, gateTolerance); len(failures) > 0 {
		t.Errorf("a run with no baseline failed anyway: %v", failures)
	} else if !strings.Contains(strings.Join(notes, "\n"), "no baseline") {
		t.Errorf("the report does not say there was no baseline: %v", notes)
	}

	// A corpus mismatch is not a regression and must not be reported as one.
	other := gateBaseline{Seed: 7, Documents: 20_000, Calibration: 10, Classes: reading(11)}
	failures, notes := gateCompare(other, base, true, gateTolerance)
	if len(failures) > 0 {
		t.Errorf("a baseline from a different corpus was compared anyway: %v", failures)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "seed") {
		t.Errorf("the report does not say why the comparison was skipped: %v", notes)
	}

	// A baseline recorded before the gate measured rounds has no figure to
	// compare against, and saying so beats dividing by zero.
	old := gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 10, Classes: map[string]gateReading{
		"common": {Samples: 1000, P50: 9, P95: 20, P99: 30, Budget: 10},
	}}
	if failures, notes := gateCompare(reading9(), old, true, gateTolerance); len(failures) > 0 {
		t.Errorf("a baseline with no recorded figure was compared anyway: %v", failures)
	} else if !strings.Contains(strings.Join(notes, "\n"), "not in the baseline") {
		t.Errorf("the report does not say the class is missing from the baseline: %v", notes)
	}
}

// reading9 is a run of one class at nine milliseconds, for the cases that need
// a run rather than a class.
func reading9() gateBaseline {
	return gateBaseline{Seed: 2121, Documents: 20_000, Calibration: 10, Classes: map[string]gateReading{
		"common": {Samples: 1000, Rounds: gateRounds, Best: 9, P50: 9, P95: 20, P99: 30, Budget: 10},
	}}
}

// TestGateSampleCountFitsTheBudget, because a class that takes ten minutes to
// measure is a class that gets dropped from the gate.
func TestGateSampleCountFitsTheBudget(t *testing.T) {
	for class, budget := range benchcorpus.Budget {
		n := gateSampleCount(budget)
		if n < gateMinSamples || n > gateMaxSamples {
			t.Errorf("%s takes %d samples, which is outside the bounds", class, n)
		}
		if spend := time.Duration(n) * budget * gateBackstop; spend > gateSpend+time.Second {
			t.Errorf("%s is measured for %v, and the class is allowed %v", class, spend, gateSpend)
		}
		if per := n / gateRounds; per < 20 {
			t.Errorf("%s puts %d samples in a round, which is too few for a median", class, per)
		}
	}
	if got := gateSampleCount(0); got != gateMaxSamples {
		t.Errorf("a class with no budget takes %d samples, want %d", got, gateMaxSamples)
	}
}
