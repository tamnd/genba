package sqlitestore

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

var _ store.Speller = (*Store)(nil)

// NearWindow is how far a correction reads in each direction from the word it
// is correcting, in rows of the term table.
//
// It is exported because the counter test spends it: a search that found
// nothing is allowed this many rows for its correction and not one more, and a
// budget written as a number in two places is a budget that stops agreeing with
// itself. There are four windows in a correction, two ranges read outwards from
// the middle, so this is a quarter of what one misspelled word costs.
const NearWindow = 100

// Near returns terms in the tenant that are close to the one given.
//
// The vocabulary is term_stat, which is maintained on every write for BM25 and
// happens to be exactly the table this needs: a term, the tenant it belongs to,
// and how many documents carry it. Nothing is stored for spelling that ranking
// was not already storing.
//
// What it reads is a window of the primary key around the word itself rather
// than the tenant's whole vocabulary, because a correction is offered after a
// query that found nothing and must not cost more than the query did. The
// window works because a word one edit away is a word that sorts next to it:
// change a letter near the end and the two are neighbours, change one in the
// middle and there are a few hundred words in between. So the scan starts at
// the typed word and walks outwards, which spends its rows on the candidates
// instead of on everything that starts with the same letter.
//
// Two things follow from that, and both are the price of not keeping a second
// index. A word whose first two letters are wrong is not corrected, because the
// window is inside the range those two letters name. The second range is those
// two letters the other way round, which is the transposition, and that is the
// common half of the cases the first range misses. And a word with a very
// crowded neighbourhood, a corpus of identifiers with a shared prefix say, can
// have its correction sitting just outside the window.
//
// The tenant is applied and the reader's permissions are not, which is the
// contract of [store.Speller]. See it for what the caller still owes.
func (s *Store) Near(ctx context.Context, p *acl.Principal, term string, limit int) ([]string, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, genba.ErrNoPrincipal
	}
	if term == "" || limit <= 0 {
		return nil, nil
	}

	bound := store.MaxEdits(term)
	typed := utf8.RuneCountInString(term)
	docs := make(map[string]int)
	if err := s.near(ctx, p.Tenant, windows(term), typed-bound, typed+bound, docs); err != nil {
		return nil, err
	}
	return store.Nearest(term, docs, limit), nil
}

// window is one range of the term table and where in it to start reading.
type window struct {
	lo, hi string
	pivot  string
}

func (s *Store) near(ctx context.Context, tenant string, windows []window, shortest, longest int, docs map[string]int) error {
	if len(windows) == 0 {
		return nil
	}

	// One statement rather than one per window. The windows are small and the
	// statement count is a budget of its own, and a correction that ran four
	// queries would be the largest part of the search that produced it.
	//
	// length() counts characters on a text value, which is the unit the
	// distance is measured in, so a word that cannot be reached in the allowed
	// number of edits is dropped by SQLite rather than carried into Go.
	const forward = `SELECT term, documents FROM (
		SELECT term, documents FROM term_stat
		WHERE tenant = ? AND term >= ? AND term < ? AND documents > 0
			AND length(term) BETWEEN ? AND ?
		ORDER BY term LIMIT ?)`
	const backward = `SELECT term, documents FROM (
		SELECT term, documents FROM term_stat
		WHERE tenant = ? AND term >= ? AND term < ? AND documents > 0
			AND length(term) BETWEEN ? AND ?
		ORDER BY term DESC LIMIT ?)`

	var (
		parts = make([]string, 0, 2*len(windows))
		args  = make([]any, 0, 12*len(windows))
	)
	for _, w := range windows {
		parts = append(parts, forward, backward)
		args = append(args, tenant, w.pivot, w.hi, shortest, longest, NearWindow)
		args = append(args, tenant, w.lo, w.pivot, shortest, longest, NearWindow)
	}

	rows, err := s.query(ctx, strings.Join(parts, "\nUNION ALL\n"), args...)
	if err != nil {
		return fmt.Errorf("sqlitestore: near: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			term string
			n    int
		)
		if err := rows.Scan(&term, &n); err != nil {
			return fmt.Errorf("sqlitestore: near: %w", err)
		}
		s.counters.rows.Add(1)
		docs[term] = n
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlitestore: near: %w", err)
	}
	return nil
}

// windows is where the scan reads: around the word itself, and around the word
// with its first two letters swapped.
func windows(term string) []window {
	first, size := utf8.DecodeRuneInString(term)
	if first == utf8.RuneError {
		return nil
	}
	second, next := utf8.DecodeRuneInString(term[size:])
	if second == utf8.RuneError || next == 0 {
		lo, hi := prefixRange(string(first))
		return []window{{lo: lo, hi: hi, pivot: term}}
	}

	lo, hi := prefixRange(term[:size+next])
	out := make([]window, 0, 2)
	out = append(out, window{lo: lo, hi: hi, pivot: term})
	if second == first {
		return out
	}
	swapped := string(second) + string(first) + term[size+next:]
	lo, hi = prefixRange(swapped[:size+next])
	return append(out, window{lo: lo, hi: hi, pivot: swapped})
}

// prefixRange turns a prefix into the half open range of terms that start with
// it. The upper bound is the next code point, which is the next byte string
// too, because UTF-8 sorts the same way the code points do and the column is
// compared as bytes.
func prefixRange(prefix string) (lo, hi string) {
	last, size := utf8.DecodeLastRuneInString(prefix)
	return prefix, prefix[:len(prefix)-size] + string(last+1)
}
