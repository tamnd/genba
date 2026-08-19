package index

import (
	"strings"
	"unicode"
)

// Tokenize splits text into lowercased terms.
//
// It keeps letters and digits, folds case, and breaks on everything else. That
// is enough for English, and it does not mangle CJK because each ideograph
// becomes its own term, which is a coarse but workable unigram index. A real
// language aware analyzer belongs behind the Analyzer interface once we have a
// second language to test against, not before.
func Tokenize(text string) []string {
	var (
		terms []string
		cur   strings.Builder
	)
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		terms = append(terms, cur.String())
		cur.Reset()
	}
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			flush()
			terms = append(terms, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur.WriteRune(unicode.ToLower(r))
		default:
			flush()
		}
	}
	flush()
	return terms
}

// stopwords are dropped from a query but not from a document, so a search for
// "the deploy runbook" is not diluted while a phrase in the body stays intact.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "but": true, "by": true, "for": true, "from": true, "how": true,
	"in": true, "is": true, "it": true, "of": true, "on": true, "or": true,
	"that": true, "the": true, "this": true, "to": true, "was": true,
	"what": true, "when": true, "where": true, "which": true, "with": true,
}

// queryTerms tokenizes a query and drops stopwords, unless the query is nothing
// but stopwords, in which case they are all we have to work with.
func queryTerms(q string) []string {
	all := Tokenize(q)
	kept := make([]string, 0, len(all))
	for _, t := range all {
		if !stopwords[t] {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return all
	}
	return kept
}
