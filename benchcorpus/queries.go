package benchcorpus

import (
	"bufio"
	_ "embed"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Class is a kind of query, because one number for "search" hides the fact that
// queries differ in cost by two orders of magnitude.
//
// A single average over a mixed workload can be met by being fast at the cheap
// queries and hopeless at the expensive ones, which is the shape of every
// search system that feels slow while its dashboard looks fine. Each class gets
// its own budget and is measured separately.
type Class string

// The classes, with the share of traffic each one stands for.
const (
	// ClassCommon is one term that a good fraction of the corpus carries. The
	// most common real query and the one where the candidate cut earns its keep.
	ClassCommon Class = "common"

	// ClassMulti is the two to four term query somebody types when they know
	// roughly what they are looking for.
	ClassMulti Class = "multi"

	// ClassFilter is browsing: no text at all, just facets. It has no full text
	// index to narrow with, so it is the class that exposes a permission
	// predicate that cannot use an index.
	ClassFilter Class = "filter"

	// ClassTermFilter is a term plus a facet, which is what somebody does
	// straight after a search returns too much.
	ClassTermFilter Class = "term-filter"

	// ClassRare is a term almost nothing carries, including terms nothing
	// carries at all. Cheap, and the class that catches work being done before
	// the match set is known to be empty.
	ClassRare Class = "rare"

	// ClassPathological is one character, or a term in most of the corpus. One
	// query in a hundred, its own relaxed budget, and measured rather than
	// ignored, because "we do not care about that query" is how a denial of
	// service works.
	ClassPathological Class = "pathological"
)

// Budget is the p95 each class is allowed.
var Budget = map[Class]time.Duration{
	ClassCommon:       10 * time.Millisecond,
	ClassMulti:        10 * time.Millisecond,
	ClassFilter:       8 * time.Millisecond,
	ClassTermFilter:   10 * time.Millisecond,
	ClassRare:         3 * time.Millisecond,
	ClassPathological: 25 * time.Millisecond,
}

// shares is the traffic mix, in parts per thousand.
var shares = []struct {
	class Class
	parts int
}{
	{ClassCommon, 300},
	{ClassMulti, 350},
	{ClassFilter, 150},
	{ClassTermFilter, 150},
	{ClassRare, 40},
	{ClassPathological, 10},
}

// Query is one benchmark query, in the syntax a person would type.
type Query struct {
	Class Class
	Text  string
}

// queryFile is the checked in query set.
//
// It is a file rather than a generator call, because a benchmark comparing this
// month's number against last month's has to be running the same queries, and a
// generator is one refactor away from quietly producing a different set. The
// corpus is too large to check in and is rebuilt from its seed. A thousand
// lines of text is not.
//
//go:embed queries.txt
var queryFile string

// Queries returns the checked in query set.
func Queries() []Query {
	var out []Query
	s := bufio.NewScanner(strings.NewReader(queryFile))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		class, text, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		out = append(out, Query{Class: Class(class), Text: text})
	}
	return out
}

// ByClass groups a query set, which is how every assertion in the gate is made:
// per class, against that class's budget.
func ByClass(queries []Query) map[Class][]Query {
	out := map[Class][]Query{}
	for _, q := range queries {
		out[q.Class] = append(out[q.Class], q)
	}
	return out
}

// BuildQueries regenerates the query set from the seed.
//
// It is used to write queries.txt and by nothing else. Regenerating the file is
// a deliberate act that invalidates every recorded baseline, so it lives behind
// the generator command rather than being called at test time.
func (s Spec) BuildQueries(n int) []Query {
	r := rand.New(rand.NewPCG(s.Seed^0x51ed270b, s.Seed))

	// The mix is dealt out and then shuffled, rather than sampled, so the set
	// has exactly the stated proportions instead of approximately them. Four
	// pathological queries where there should be ten is a twenty five percent
	// error in the class that dominates the tail.
	classes := make([]Class, 0, n)
	for _, sh := range shares {
		for range sh.parts * n / 1_000 {
			classes = append(classes, sh.class)
		}
	}
	for len(classes) < n {
		classes = append(classes, ClassCommon)
	}
	r.Shuffle(len(classes), func(i, j int) { classes[i], classes[j] = classes[j], classes[i] })

	out := make([]Query, 0, n)
	for _, class := range classes {
		out = append(out, s.query(r, class))
	}
	return out
}

func (s Spec) query(r *rand.Rand, class Class) Query {
	switch class {
	case ClassCommon:
		// Term ranks 20 to 200. Rank one is in almost every document and is the
		// pathological class, and rank ten thousand is in four, so the band in
		// between is where the ordinary query lives.
		return Query{class, word(20 + r.IntN(180))}

	case ClassMulti:
		terms := make([]string, 2+r.IntN(3))
		for j := range terms {
			terms[j] = word(30 + r.IntN(4_000))
		}
		return Query{class, strings.Join(terms, " ")}

	case ClassFilter:
		return Query{class, "source:" + sources[r.IntN(len(sources))].name +
			" kind:" + string(kinds[r.IntN(len(kinds))])}

	case ClassTermFilter:
		return Query{class, word(20+r.IntN(2_000)) +
			" source:" + sources[r.IntN(len(sources))].name}

	case ClassRare:
		if r.IntN(4) == 0 {
			// A term the vocabulary does not contain at all, which is the empty
			// match set and the cheapest query there is.
			return Query{class, "zzqx" + strconv.Itoa(r.IntN(1_000))}
		}
		return Query{class, word(s.Vocabulary - 1 - r.IntN(20_000))}

	default:
		if r.IntN(2) == 0 {
			// The most common term in the corpus, which most documents carry.
			return Query{class, word(0)}
		}
		// One character, which matches by prefix in the suggest path and is the
		// query a person generates by accident on their way to a real one.
		return Query{class, string(rune('a' + r.IntN(26)))}
	}
}
