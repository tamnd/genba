package doc

import (
	"iter"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
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
	var terms []string
	for s := range Spans(text) {
		terms = append(terms, s.Term)
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
func Analyze(text string) []Span { return slices.Collect(Spans(text)) }

// Spans is [Analyze] one term at a time, for a caller that stops before the end.
//
// Finding where to cut a snippet means finding the first query term in a body,
// and the body is the whole document. Analysing all of it to use the first
// answer is the difference between reading a sentence and reading a file, and on
// a page of twenty results it was most of what the page cost.
func Spans(text string) iter.Seq[Span] {
	return func(yield func(Span) bool) {
		scan(text, func(term []byte, start, end int) bool {
			return yield(Span{Term: string(term), Start: start, End: end})
		})
	}
}

// Find returns the offset of the first term in text that is one of want, or -1.
//
// It is here rather than written out at the caller because it is the analyzer
// answering a question about its own output, and a caller that searched the text
// for the words instead would find the ones the index never matched.
//
// It allocates nothing. That matters because the caller is snippet cutting on a
// page of results, where the answer is one number and the input is the whole
// document: building a string for every word of every result to throw all but
// one of them away was most of what a page cost.
func Find(text string, want map[string]bool) int {
	at := -1
	scan(text, func(term []byte, start, _ int) bool {
		if !want[string(term)] {
			return true
		}
		at = start
		return false
	})
	return at
}

// scan is the analyzer. Everything else in this file is a way of asking it
// something.
//
// The term it yields is the folded bytes and it is only valid until the next
// one, because the buffer is reused. That is the reason it is unexported: a
// caller that keeps the slice gets the next word. [Find] looks it up in a map,
// which the compiler does without copying, and [Spans] makes a string of it.
func scan(text string, yield func(term []byte, start, end int) bool) {
	var (
		cur   []byte
		start = -1
	)
	// flush emits the term being built, if there is one, and reports whether the
	// walk should carry on.
	flush := func(end int) bool {
		at := start
		term := cur
		cur, start = cur[:0], -1
		if at < 0 || len(term) == 0 {
			return true
		}
		return yield(term, at, end)
	}
	for i, r := range text {
		// The ASCII half of the same rules, written out. Every branch below asks a
		// range table whether a rune is a letter, and a table lookup per character
		// of a document is what a snippet was spending its time on. This is the
		// same answer for the characters most text is made of.
		if r < utf8.RuneSelf {
			switch {
			case 'a' <= r && r <= 'z' || '0' <= r && r <= '9':
				if start < 0 {
					start = i
				}
				cur = append(cur, byte(r))
			case 'A' <= r && r <= 'Z':
				if start < 0 {
					start = i
				}
				cur = append(cur, byte(r)+'a'-'A')
			default:
				if !flush(i) {
					return
				}
			}
			continue
		}
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			if !flush(i) {
				return
			}
			cur = utf8.AppendRune(cur, r)
			if !yield(cur, i, i+len(cur)) {
				return
			}
			cur = cur[:0]
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if start < 0 {
				start = i
			}
			cur = utf8.AppendRune(cur, unicode.ToLower(r))
		default:
			if !flush(i) {
				return
			}
		}
	}
	flush(len(text))
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
	// Straight into the map rather than through two slices of terms, because this
	// runs on every document that is written and the slices are the size of the
	// corpus while the map is the size of its vocabulary.
	a := Analysis{Terms: make(map[string]TermCount)}
	for s := range Spans(d.Title) {
		a.TitleTokens++
		c := a.Terms[s.Term]
		c.Title++
		a.Terms[s.Term] = c
	}
	for s := range Spans(d.Body) {
		a.BodyTokens++
		c := a.Terms[s.Term]
		c.Body++
		a.Terms[s.Term] = c
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
