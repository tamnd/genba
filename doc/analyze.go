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
