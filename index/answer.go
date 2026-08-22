package index

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tamnd/genba/doc"
)

// AnswerQuotes is how many documents an answer quotes from.
//
// Three, because an answer above a list of results is competing with the list
// for the same screen and it loses that competition the moment it is longer than
// the first two results. Three quotes is about eight lines, which is a glance.
// It is also the point where a fourth source stops adding evidence and starts
// adding scrolling: on every query we have measured, the fourth ranked document
// repeats what one of the first three already said.
const AnswerQuotes = 3

// MinQuote and MaxQuote bound how long a quoted passage may be, in bytes.
//
// Below the minimum a sentence is a heading, a caption or a list item, and
// quoting "Configuration" under a question is worse than quoting nothing. Above
// the maximum it is a paragraph that happens to contain no full stop, usually
// because it is a table row or a line of generated text, and a paragraph pushes
// the results off the screen.
const (
	MinQuote = 40
	MaxQuote = 400
)

// answerWindow is how much of a body is read looking for a passage worth
// quoting, in characters around the first match.
//
// The document is already in the top three, so the first match is a reasonable
// place to look and the alternative is reading whole documents to draw eight
// lines. Six snippets wide is enough to hold the sentence the match is in and
// the two on either side of it, which is what the scoring below chooses between.
const answerWindow = 6 * SnippetWidth

// Answer is what the corpus already says about a query.
//
// It is quoted rather than written. Every sentence in it is a passage lifted
// verbatim out of a document this principal may read, and each one carries the
// document it came from, so a citation is not a claim about a source, it is the
// source. Nothing here is paraphrased, summarised or generated, which means
// there is no sentence in an answer that is not already in the corpus and no way
// for one to be wrong in a way the document it points at is not also wrong.
//
// That is a smaller thing than the assistant in the specification and it is
// deliberately the honest half of it. The half that needs a model is M5. What
// this settles first is the part a model does not fix: where the answer sits,
// what a citation does when somebody clicks it, and how a reader tells the
// product's words apart from a document's. An assistant that gets those wrong is
// worse with a good model than without one, because a confident paraphrase whose
// citation goes nowhere is how people learn to stop reading citations.
type Answer struct {
	// Quotes are the passages, best first and at most [AnswerQuotes] of them.
	// One document contributes one quote: three quotes out of one file is a
	// document worth opening rather than an answer.
	Quotes []Quote
}

// Quote is one passage and the document it was taken from.
type Quote struct {
	// ID is the document. It is an id rather than the document itself because a
	// quote is only ever produced for a document that is already on the page it
	// sits above, so the caller has the title, the source and everything else
	// beside it already, and sending them twice is two copies to disagree.
	ID string

	// Text is the passage, verbatim, with its whitespace collapsed. It is not
	// escaped, which is the caller's job, and it is not the snippet: see quote.
	Text string

	// Passages is Text split so the runs that matched the query are marked, the
	// same way [Result.Passages] is and for the same reason.
	Passages []Passage
}

// answer picks the passages worth quoting from a page of results.
//
// It reads the documents that are already in hand rather than running a
// retrieval of its own. That is the whole reason the answer is part of a search
// response instead of an endpoint beside it: the ranking has already chosen
// which documents are worth reading and the fetch has already paid for their
// bodies, so what an answer costs on top of a search is three passes over three
// windows, and a results page still answers in one request.
func answer(hits []Result, terms []string) Answer {
	if len(terms) == 0 {
		return Answer{}
	}
	var out Answer
	for _, hit := range hits {
		if len(out.Quotes) == AnswerQuotes {
			break
		}
		text, passages := quote(hit.Document, terms)
		if text == "" {
			continue
		}
		out.Quotes = append(out.Quotes, Quote{ID: hit.Document.ID, Text: text, Passages: passages})
	}
	return out
}

// quote picks the sentence of a document that best answers the query.
//
// A snippet and a quote are different claims and this is why there are two
// functions. A snippet is evidence that a document matched, shown under a title
// somebody is already reading, so it is a window around the first match and it
// is allowed to start mid word and end in an ellipsis. A quote is read on its
// own, above the results, by somebody deciding whether any of this is worth
// opening. It has to start where a sentence starts and end where one ends,
// because a fragment read out of context is how a reader ends up with an
// impression the document does not support, and the fragment is the part they
// will remember.
func quote(d doc.Document, terms []string) (string, []Passage) {
	// A source file is never quoted, and the reason is not that there is no
	// prose in one. There is a great deal of it, and every sentence of it arrives
	// wearing its markers: a paragraph of a package comment reads as "// // # The
	// rule // // A cache key that does not name the asker's visibility is a
	// permission bug." Taking the markers off is a renderer's job, it is done in
	// the browser where the language is known, and guessing at it here would mean
	// the index deciding what a leading hash means in nine languages. The answer
	// has two more documents to quote from and no reason to guess.
	if d.Kind == doc.KindCode {
		return "", nil
	}

	raw := d.Body
	if raw == "" {
		return "", nil
	}

	from := 0
	if at := firstMatch(raw, terms); at > 0 {
		from = back(raw, at, answerWindow/2)
	}
	to := forward(raw, from, answerWindow)

	text := raw[from:to]
	if d.Properties[doc.MediaType] == "text/markdown" {
		text = plainText(text)
	}

	// The window was cut at a character count, so the sentence at each end of it
	// is almost certainly half a sentence. Dropping them costs a candidate at
	// each edge of a window six snippets wide and buys the guarantee the whole
	// function exists for.
	best := bestSentence(sentences(text), terms, from > 0, to < len(raw))
	if best == "" {
		return "", nil
	}
	return best, mark(best, terms)
}

// sentences splits text where a sentence ends.
//
// It is deliberately simple: a full stop, question mark or exclamation mark,
// followed by a space and something that starts a new sentence. A blank line
// ends one too, because a heading and a list item have no punctuation and
// running them into the paragraph below is how a quote ends up being two
// unrelated things.
//
// Abbreviations break it. "e.g. the index" splits after the g, and the cost of
// that is a candidate too short to pass [MinQuote], which is dropped. A
// heuristic that fails by discarding a passage is the right way round; one that
// fails by joining two paragraphs into a quote nobody wrote is not.
func sentences(text string) []string {
	var (
		out  []string
		last int
	)
	for i, r := range text {
		switch {
		case r == '\n' && i+1 < len(text) && text[i+1] == '\n':
		case r == '.' || r == '!' || r == '?':
			// A full stop inside a version number, a file name or a decimal is
			// not the end of anything, and neither is one at the very end of the
			// window, which the tail below picks up.
			next := i + 1
			if next >= len(text) {
				continue
			}
			after, _ := utf8.DecodeRuneInString(text[next:])
			if !isBreak(after) {
				continue
			}
		default:
			continue
		}
		out = append(out, text[last:i+1])
		last = i + 1
	}
	if last < len(text) {
		out = append(out, text[last:])
	}
	return out
}

// isBreak reports whether a character can follow a full stop that ended a
// sentence. Whitespace is the ordinary case, and a closing quote or bracket is
// the one that follows a sentence inside a quotation.
func isBreak(r rune) bool {
	return unicode.IsSpace(r) || r == '"' || r == '\'' || r == ')' || r == ']'
}

// bestSentence is the one of these worth quoting, or nothing.
//
// The score is how many distinct query terms the sentence carries, because a
// sentence holding three of the words somebody asked about says more about their
// question than one holding a single word three times. Length only breaks ties,
// and it breaks them toward the shorter of two sentences that say the same
// amount.
//
// Nothing is returned rather than the least bad candidate. A document can be a
// perfectly good result and still have no sentence in it worth reading on its
// own, which is true of every source file and most spreadsheets, and an answer
// that quotes a line of JSON above the results has made the page worse. The
// caller drops the document and quotes the next one.
func bestSentence(parts, terms []string, cutHead, cutTail bool) string {
	if len(parts) == 0 {
		return ""
	}
	if cutHead {
		parts = parts[1:]
	}
	if cutTail && len(parts) > 0 {
		parts = parts[:len(parts)-1]
	}

	want := make(map[string]bool, len(terms))
	for _, t := range terms {
		want[t] = true
	}

	var (
		best  string
		found int
	)
	for _, part := range parts {
		s := collapse(part)
		if len(s) < MinQuote {
			continue
		}
		if len(s) > MaxQuote {
			s = clip(s, MaxQuote)
		}
		if !prose(s) {
			continue
		}
		seen := make(map[string]bool, len(terms))
		for _, t := range doc.Analyze(s) {
			if want[t.Term] {
				seen[t.Term] = true
			}
		}
		switch {
		case len(seen) > found:
		case len(seen) == found && found > 0 && len(s) < len(best):
		default:
			continue
		}
		best, found = s, len(seen)
	}
	if found == 0 {
		return ""
	}
	return best
}

// notProse are the characters that do not turn up in a sentence somebody wrote.
//
// Braces, brackets, angle brackets, pipes, equals signs, backslashes and
// backticks are the punctuation of code, configuration and markup rather than of
// a language, and one of them in a candidate is enough to say the candidate is a
// line out of a file rather than something written to be read. That is a blunt
// test and it is meant to be. It costs the occasional real sentence that mentions
// an array index or a shell command, and losing a good sentence is cheap because
// there are two more documents to quote from, while quoting a line of JSON above
// the results is the failure this whole region has to avoid.
const notProse = "{}[]<>|=\\`"

// minWords is how many words a candidate needs before it counts as a sentence.
//
// The length bound alone lets through a single long identifier, a URL and a file
// path, all of which reach forty characters without saying anything. Six words is
// the shortest thing in our corpus that reads as a statement.
const minWords = 6

// prose reports whether a candidate is a sentence rather than a line of a file.
func prose(s string) bool {
	if strings.ContainsAny(s, notProse) {
		return false
	}
	// The whitespace is already collapsed, so gaps and words differ by one.
	return strings.Count(s, " ") >= minWords-1
}

// clip cuts a passage at the last word boundary that fits and says it was cut.
//
// The alternative is dropping a sentence for being long, and the sentences that
// run past [MaxQuote] are not all runaway table rows: a specification written in
// long sentences would never be quoted at all. Cutting mid word would undo the
// point of quoting a sentence rather than a window.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndexByte(s[:n], ' ')
	if cut <= 0 {
		cut = n
	}
	return strings.TrimRight(s[:cut], " ,;:") + "..."
}
