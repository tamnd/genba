package doc

import (
	"strings"
	"unicode"
)

// Span is one analysed term together with where it sits in the text it came
// from.
//
// The offsets are byte offsets into that text, so a caller can slice the
// original string and get back the exact characters that produced the term,
// with their original case and any accents intact.
type Span struct {
	Term       string
	Start, End int
}

// Tokenize splits text into the terms an index matches on.
//
// It keeps letters and digits, folds case, and breaks on everything else. That
// is enough for English, and it does not mangle CJK because each ideograph
// becomes its own term, which is a coarse but workable unigram index. A real
// language aware analyzer belongs behind an Analyzer interface once there is a
// second language to test against, not before.
//
// It lives in this package rather than next to the ranking because a storage
// driver that keeps its own inverted index has to analyse text exactly the way
// the ranking does. A driver that tokenizes differently returns a different
// match set for the same query, and the difference shows up as a document that
// one deployment finds and another does not.
func Tokenize(text string) []string {
	spans := Analyze(text)
	terms := make([]string, len(spans))
	for i, s := range spans {
		terms[i] = s.Term
	}
	return terms
}

// Analyze is [Tokenize] keeping the offsets.
//
// It is the actual analyzer and Tokenize is the common case of it, rather than
// the two being separate implementations that have to be kept in step. The
// offsets are what highlighting needs: marking the matched words in a snippet
// means knowing which characters of the original text became which term, and a
// substring search after the fact gets that wrong, because it lights up the
// "run" inside "runbook" that the index never matched.
func Analyze(text string) []Span {
	var (
		out   []Span
		cur   strings.Builder
		start = -1
	)
	flush := func(end int) {
		if start >= 0 && cur.Len() > 0 {
			out = append(out, Span{Term: cur.String(), Start: start, End: end})
		}
		cur.Reset()
		start = -1
	}
	for i, r := range text {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			flush(i)
			out = append(out, Span{Term: string(r), Start: i, End: i + len(string(r))})
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if start < 0 {
				start = i
			}
			cur.WriteRune(unicode.ToLower(r))
		default:
			flush(i)
		}
	}
	flush(len(text))
	return out
}

// Analyzed returns the document's searchable text as a single string of terms
// separated by spaces.
//
// A driver with a full text index of its own stores this rather than the raw
// text, so that whatever tokenizer that index uses sees terms that are already
// the terms [Tokenize] produced.
//
// It covers the title and the body and nothing else, because a driver's index
// decides which documents are candidates and the ranking decides the order. A
// field in one and not the other would produce candidates that score zero and
// disappear again, which costs work and makes a result count wrong.
func (d Document) Analyzed() string { return strings.Join(d.Terms(), " ") }

// Terms returns the document's searchable text as terms, title first.
func (d Document) Terms() []string {
	title, body := Tokenize(d.Title), Tokenize(d.Body)
	terms := make([]string, 0, len(title)+len(body))
	terms = append(terms, title...)
	terms = append(terms, body...)
	return terms
}

// TermCount is how often one term occurs in a document, by field.
type TermCount struct{ Title, Body int }

// Analysis is everything a ranker needs to know about one document, computed
// once from its text.
//
// It exists because the alternative is computing it at query time, which means
// running the analyzer over every document in the match set on every search.
// That is the single thing that made search cost a second on a five thousand
// document corpus: the numbers here are small, they never change unless the
// document does, and a store that keeps them turns a scan of the corpus into a
// lookup of a few hundred rows.
type Analysis struct {
	// TitleTokens and BodyTokens are token counts, not distinct terms. BM25
	// normalises by document length and length is a count of tokens.
	TitleTokens int
	BodyTokens  int

	// Terms is the per term occurrence count, by field.
	Terms map[string]TermCount
}

// Analyze returns the document's statistics in one pass over its text.
func (d Document) Analyze() Analysis {
	title, body := Tokenize(d.Title), Tokenize(d.Body)
	a := Analysis{
		TitleTokens: len(title),
		BodyTokens:  len(body),
		Terms:       make(map[string]TermCount, len(title)+len(body)),
	}
	for _, t := range title {
		c := a.Terms[t]
		c.Title++
		a.Terms[t] = c
	}
	for _, t := range body {
		c := a.Terms[t]
		c.Body++
		a.Terms[t] = c
	}
	return a
}

// Display is what a person is labelled with in a facet or a result row, which
// is the most specific thing the connector managed to resolve.
//
// It is here rather than in whichever package happened to need it first because
// a storage driver stores this string in a column and the ranking counts facets
// over it. Two definitions of a person's display name is two facet lists that
// disagree about who wrote what.
func (p Person) Display() string {
	switch {
	case p.Name != "":
		return p.Name
	case p.Email != "":
		return p.Email
	default:
		return p.Identity.Value
	}
}
