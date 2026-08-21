package index

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// correctionWords is the longest query a correction is offered for.
//
// A query of two or three words that found nothing is usually one word typed
// wrong. A query of nine words that found nothing is usually a sentence
// somebody pasted, and there is no spelling of it that would have worked, so
// the offer would be noise on top of a screen that already says nothing
// matched.
const correctionWords = 4

// correctionShortest is the shortest word worth correcting. Two letters are
// within one edit of most of the alphabet, and a suggestion drawn from that is
// a guess rather than a correction.
const correctionShortest = 3

// correct is the "did you mean" on a search that found nothing.
//
// It runs only on the empty result, only on a short query, and only for terms
// the corpus does not have at all, which is what keeps it from second guessing a
// query that is spelled fine and simply matched nothing this week. The document
// frequencies it reads for that were already fetched to score the query, so
// deciding whether to try costs no reads.
//
// The security rule is the interesting part, and it belongs to this function
// rather than to the driver. A vocabulary is a fact about the tenant, and the
// person asking may not read every document in the tenant, so a term taken
// straight out of the index and shown would answer "does the word incident
// appear anywhere in this company" for somebody with access to nothing. The
// corrected query is therefore run as the asker before it is offered, asking
// for a single row, and it is dropped unless it found one. What is shown is
// then a query that person can run and get results from, which is the only
// thing a correction was ever supposed to be.
func (s *Searcher) correct(ctx context.Context, p *acl.Principal, q Query, terms []string, corpus store.Corpus) (string, error) {
	sp, ok := s.store.(store.Speller)
	if !ok || q.Text == "" || len(terms) == 0 || len(terms) > correctionWords {
		return "", nil
	}

	swap := make(map[string]string, len(terms))
	for _, t := range terms {
		if corpus.DocFreq[t] > 0 || utf8.RuneCountInString(t) < correctionShortest {
			continue
		}
		near, err := sp.Near(ctx, p, t, 1)
		if err != nil {
			return "", err
		}
		if len(near) > 0 {
			swap[t] = near[0]
		}
	}
	if len(swap) == 0 {
		return "", nil
	}

	text := replaceTerms(q.Text, swap)
	if text == q.Text {
		return "", nil
	}

	// The confirmation. Same filters, because the offer has to lead to the page
	// the person is looking at, and one row, because whether there is a result
	// is the whole question.
	confirm := q
	confirm.Text = text
	found, err := s.collect(ctx, p, confirm.Request(), store.Selection{Limit: 1})
	if err != nil {
		return "", err
	}
	if len(found.cands) == 0 {
		return "", nil
	}
	return text, nil
}

// replaceTerms rewrites the words of a query that were corrected and leaves
// every other character where it was.
//
// It works on the analyzer's own spans rather than on a string replacement,
// which is what keeps it from touching the cache inside caching, and what keeps
// the quotes, the filters and the capitalisation of the words that were spelled
// right exactly as they were typed.
func replaceTerms(text string, swap map[string]string) string {
	var (
		out  strings.Builder
		last int
	)
	for _, span := range doc.Analyze(text) {
		to, ok := swap[span.Term]
		if !ok {
			continue
		}
		out.WriteString(text[last:span.Start])
		out.WriteString(to)
		last = span.End
	}
	if last == 0 {
		return text
	}
	out.WriteString(text[last:])
	return out.String()
}
