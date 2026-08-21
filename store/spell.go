package store

import (
	"context"
	"sort"
	"unicode/utf8"

	"github.com/tamnd/genba/acl"
)

// Speller is the optional capability of a driver that can name terms its own
// index holds near a term it does not.
//
// It exists so that a correction offered to somebody who mistyped comes from
// the corpus rather than from a dictionary of English. A dictionary knows that
// recieve is receive and does not know that kubectl, genbad or the name of a
// service are words at all, and in a corpus of runbooks and code most of the
// words worth correcting to are of the second kind.
//
// # What it does not do
//
// Near applies the tenant and nothing else. There is no permission filter on a
// vocabulary and there cannot usefully be one: a term is carried by many
// documents, the asker may read some of them, and a per asker term list is an
// aggregate over the corpus on a query that has already found nothing.
//
// So the rule is on the caller, and it is not optional. Nothing derived from
// Near may be shown to anybody until the corrected query has been run as that
// person and returned at least one hit. That confirmation is one search of a
// single row, it is only ever reached on a query that matched nothing, and it
// is what keeps a correction from telling somebody that a word appears in a
// document they cannot read. [index.Searcher] does exactly this, and a caller
// that skips it has built an oracle for the contents of the corpus.
type Speller interface {
	// Near returns terms in the principal's tenant that are close to the given
	// term, nearest first and commonest among equals, and at most limit of
	// them. The term itself is never returned. A driver that finds nothing
	// returns an empty slice rather than an error.
	//
	// What comes back is what the driver can afford to find rather than
	// everything that is within reach. A driver whose vocabulary is a table
	// reads a range of it and leaves the rest, because a correction is offered
	// on a query that already found nothing and is not worth more time than
	// the query it follows. So a term that is close may be missing, and the
	// caller treats an empty answer as no correction rather than as proof that
	// there is none.
	Near(ctx context.Context, p *acl.Principal, term string, limit int) ([]string, error)
}

// Edits is the edit distance between two terms, and is at most one past the
// bound it is given.
//
// It is the Damerau version, which counts a transposition as one edit rather
// than two, because a transposition is what a pair of hands produces and the
// difference decides whether teh reaches the. Anything further away than bound
// is reported as one past it rather than measured, which is what makes the
// bound the point of the function: the rows scanned to find a correction are
// mostly rows that are nothing like the word, and each one costs a prefix of
// the matrix instead of all of it.
func Edits(a, b string, bound int) int {
	if a == b {
		return 0
	}
	x, y := []rune(a), []rune(b)
	// A length difference is a lower bound on the distance, so a candidate that
	// cannot possibly be close enough is answered without any work at all.
	if d := len(x) - len(y); d > bound || -d > bound {
		return bound + 1
	}
	if len(x) > len(y) {
		x, y = y, x
	}

	// Two rows of the matrix rather than all of it, plus the row before them,
	// which is the row a transposition reaches back to.
	prev := make([]int, len(x)+1)
	cur := make([]int, len(x)+1)
	before := make([]int, len(x)+1)
	for i := range prev {
		prev[i] = i
	}

	for j := 1; j <= len(y); j++ {
		cur[0] = j
		best := cur[0]
		for i := 1; i <= len(x); i++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			d := min(prev[i]+1, cur[i-1]+1, prev[i-1]+cost)
			if i > 1 && j > 1 && x[i-1] == y[j-2] && x[i-2] == y[j-1] {
				d = min(d, before[i-2]+1)
			}
			cur[i] = d
			best = min(best, d)
		}
		// Every distance from here on is at least the best on this row, so a row
		// with nothing under the bound on it is the end of the measurement.
		if best > bound {
			return bound + 1
		}
		before, prev, cur = prev, cur, before
	}
	if prev[len(x)] > bound {
		return bound + 1
	}
	return prev[len(x)]
}

// MaxEdits is how far a term may be from what somebody typed and still be
// offered as what they meant.
//
// Two for an ordinary word and one for a short one, because two edits on a four
// letter word is not a correction, it is a different word: from cat to cot is a
// typo and from cat to dog would be a suggestion nobody asked for. It is a
// function rather than a constant so that the drivers and the ranking cannot
// disagree about where the line is.
func MaxEdits(term string) int {
	if utf8.RuneCountInString(term) <= 4 {
		return 1
	}
	return 2
}

// Nearest ranks candidate terms against the one somebody typed, which is the
// half of a correction that is the same in every driver.
//
// candidates maps a term to how many documents carry it. Nearer wins, and among
// equals the term more of the corpus uses wins, because the common spelling of
// a word is what somebody meant far more often than the rare one. The term
// itself is dropped: a query that matched nothing was not spelled that way.
func Nearest(term string, candidates map[string]int, limit int) []string {
	if limit <= 0 || term == "" {
		return nil
	}
	bound := MaxEdits(term)

	type scored struct {
		term string
		docs int
		dist int
	}
	near := make([]scored, 0, 8)
	for cand, docs := range candidates {
		if cand == term || docs <= 0 {
			continue
		}
		d := Edits(term, cand, bound)
		if d > bound {
			continue
		}
		near = append(near, scored{term: cand, docs: docs, dist: d})
	}

	sort.Slice(near, func(i, j int) bool {
		if near[i].dist != near[j].dist {
			return near[i].dist < near[j].dist
		}
		if near[i].docs != near[j].docs {
			return near[i].docs > near[j].docs
		}
		return near[i].term < near[j].term
	})

	out := make([]string, 0, min(limit, len(near)))
	for _, n := range near[:min(limit, len(near))] {
		out = append(out, n.term)
	}
	return out
}
