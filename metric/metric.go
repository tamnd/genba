// Package metric exposes what the process is doing, in the Prometheus text
// format, with nothing imported to do it.
//
// The whole of this package is a few hundred lines because the exposition
// format is a few hundred lines, and the alternative is a dependency tree an
// order of magnitude larger than the thing being measured. This repository has
// no non SQLite dependencies and it is not spending that budget on a text
// encoder. What is here is the subset a search server needs: histograms it
// records into, and counters it reads out of the structures that already keep
// them.
//
// # What is measured
//
// A latency gate in CI proves a regression did not ship. It does not prove the
// system is fast for the person using it, because the runner is not the
// deployment and the benchmark corpus is not the corpus. These are the numbers
// that answer the second question, and they are deliberately the same
// quantities the gate asserts on, so that a slowdown in production can be
// reproduced by the gate rather than argued about.
//
// Durations are in milliseconds rather than the seconds that Prometheus
// conventionally wants, and the buckets are tight at the bottom. The budget for
// this system is stated in single digit milliseconds, so the interesting
// question is what fraction of requests were under ten. A default bucket set
// answers what fraction were under a hundred, which here is all of them, and a
// histogram whose every observation lands in the first bucket has measured
// nothing.
package metric

import (
	"context"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Duration is the bucket set for anything measured in milliseconds.
//
// It is tighter at the bottom than a default set because the budgets are, and
// it stops at half a second because a search that took longer than that has a
// problem no percentile is going to describe.
var Duration = []float64{1, 2, 5, 10, 25, 50, 100, 250, 500}

// Size is the bucket set for anything counted rather than timed: candidates
// ranked, documents matched. It is logarithmic because the interesting
// difference is between ten and a thousand rather than between ten and twenty.
var Size = []float64{1, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// Registry holds the families a process publishes.
//
// Families are written in the order they were registered, which makes the
// output diffable between two scrapes of the same binary and puts the numbers
// somebody is looking for near the top.
type Registry struct {
	mu       sync.Mutex
	order    []string
	families map[string]family
}

// family is one metric name and everything needed to write it out.
type family struct {
	help string
	kind string
	// write appends the series of this family. It runs at scrape time, which is
	// what lets a counter read a structure that was already counting rather
	// than requiring every counter in the process to be a metric.
	//
	// It takes the scrape's context because some of what is read at scrape time
	// is not a number in memory. A family backed by a database has to be able to
	// give up when the scrape has, and a monitoring system that holds a
	// connection open against a store that has stopped answering makes an
	// incident worse rather than describing it.
	write func(context.Context, *encoder)
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{families: map[string]family{}}
}

// register adds a family, panicking on a duplicate name.
//
// A duplicate is a programming error found at startup rather than a scrape that
// silently reports one of two meanings for the same name, which is the failure
// that wastes an afternoon during an incident.
func (r *Registry) register(name, help, kind string, write func(context.Context, *encoder)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.families[name]; ok {
		panic("metric: " + name + " is registered twice")
	}
	r.order = append(r.order, name)
	r.families[name] = family{help: help, kind: kind, write: write}
}

// Histogram records observations into fixed buckets, optionally split by one
// label.
//
// One label rather than several because the only split this system has needed
// is by endpoint, and every label is a multiplier on the series count. A
// cardinality mistake in a metrics library is not a small bug: it is a
// monitoring system falling over at the moment somebody needs it.
type Histogram struct {
	buckets []float64
	label   string

	mu     sync.Mutex
	series map[string]*bucketSet
}

// bucketSet is the counts for one label value.
type bucketSet struct {
	counts []uint64
	sum    float64
	total  uint64
}

// NewHistogram registers a histogram. Passing an empty label makes it a single
// series with no labels at all.
func (r *Registry) NewHistogram(name, help, label string, buckets []float64) *Histogram {
	h := &Histogram{
		buckets: slices.Clone(buckets),
		label:   label,
		series:  map[string]*bucketSet{},
	}
	slices.Sort(h.buckets)
	r.register(name, help, "histogram", func(_ context.Context, e *encoder) { h.write(e, name) })
	return h
}

// Observe records one value under a label. Pass an empty value for a histogram
// registered without a label.
func (h *Histogram) Observe(value float64, label string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.series[label]
	if set == nil {
		set = &bucketSet{counts: make([]uint64, len(h.buckets))}
		h.series[label] = set
	}
	// A linear walk over nine bounds beats a binary search on any real bucket
	// set, and the common case exits on the first comparison because the common
	// case is fast.
	for i, bound := range h.buckets {
		if value <= bound {
			set.counts[i]++
			break
		}
	}
	set.sum += value
	set.total++
}

// write emits the cumulative form the format asks for.
func (h *Histogram) write(e *encoder, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, value := range slices.Sorted(maps.Keys(h.series)) {
		set := h.series[value]
		var running uint64
		for i, bound := range h.buckets {
			running += set.counts[i]
			e.series(name+"_bucket", h.pairs(value, "le", trim(bound)), float64(running))
		}
		e.series(name+"_bucket", h.pairs(value, "le", "+Inf"), float64(set.total))
		e.series(name+"_sum", h.pairs(value), set.sum)
		e.series(name+"_count", h.pairs(value), float64(set.total))
	}
}

// pairs builds the label list for one series, dropping the histogram's own
// label when it was registered without one.
func (h *Histogram) pairs(value string, extra ...string) []string {
	if h.label == "" {
		return extra
	}
	return append([]string{h.label, value}, extra...)
}

// Counters registers a family read at scrape time from something that was
// already counting.
//
// The read returns a value per label value, and an empty key means a series
// with no labels. This is how the cache layers and the storage driver are
// published: they keep their own counters because their own tests assert on
// them, and turning those into metrics should not mean counting anything twice.
//
// kind is "counter" for anything that only goes up and "gauge" for anything
// that can go down, which the format needs and which a reader needs more.
//
// The read is given the scrape's context, and one that has to go and ask
// something slower than a mutex should honour it. A read that returns nothing
// publishes no series at all, which is how a family says it has no answer
// rather than answering zero.
func (r *Registry) Counters(name, help, label, kind string, read func(context.Context) map[string]float64) {
	r.register(name, help, kind, func(ctx context.Context, e *encoder) {
		values := read(ctx)
		for _, key := range slices.Sorted(maps.Keys(values)) {
			if label == "" {
				e.series(name, nil, values[key])
				continue
			}
			e.series(name, []string{label, key}, values[key])
		}
	})
}

// Collect writes an exposition of every family.
//
// The context belongs to the scrape that asked. It is passed to every family
// that reads something at scrape time, so a scrape the client has given up on
// stops costing the process anything.
func (r *Registry) Collect(ctx context.Context, w io.Writer) (int64, error) {
	r.mu.Lock()
	names := slices.Clone(r.order)
	families := maps.Clone(r.families)
	r.mu.Unlock()

	e := &encoder{}
	for _, name := range names {
		f := families[name]
		e.buf.WriteString("# HELP " + name + " " + strings.NewReplacer("\\", `\\`, "\n", `\n`).Replace(f.help) + "\n")
		e.buf.WriteString("# TYPE " + name + " " + f.kind + "\n")
		f.write(ctx, e)
	}
	n, err := io.WriteString(w, e.buf.String())
	return int64(n), err
}

// Text returns the exposition as a string, which is what the tests read.
//
// It is not called String, because a String that takes an argument is not the
// method everything else in Go means by that name and would be called by a
// print statement that was never meant to scrape anything.
func (r *Registry) Text(ctx context.Context) string {
	var sb strings.Builder
	_, _ = r.Collect(ctx, &sb)
	return sb.String()
}

// encoder accumulates the text of one scrape.
type encoder struct{ buf strings.Builder }

// series writes one line. Labels are name and value alternating.
func (e *encoder) series(name string, labels []string, value float64) {
	e.buf.WriteString(name)
	if len(labels) > 1 {
		e.buf.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				e.buf.WriteByte(',')
			}
			e.buf.WriteString(labels[i])
			e.buf.WriteString(`="`)
			e.buf.WriteString(escape(labels[i+1]))
			e.buf.WriteByte('"')
		}
		e.buf.WriteByte('}')
	}
	e.buf.WriteByte(' ')
	e.buf.WriteString(trim(value))
	e.buf.WriteByte('\n')
}

// escape makes a label value safe to put between quotes. A label value here can
// come from a request path, so this is not a formality.
func escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// trim renders a float the way the format expects, without a trailing .000000
// on the whole numbers that most of these are.
func trim(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Percentile returns the value at q over a sorted copy of samples, by the
// nearest rank method.
//
// It is here rather than in the benchmark that uses it because the gate and the
// metrics have to agree on what p95 means. Two definitions of a percentile in
// one repository is how a gate passes on a number a dashboard calls a
// regression.
func Percentile(samples []float64, q float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	rank := int(math.Ceil(q * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}
