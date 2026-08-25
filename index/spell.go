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

// CorrectionOffers is how many spellings of one word are considered.
//
// It is exported for the same reason the driver's near window is: the counter
// test spends it, and a budget written as a number in two places is a budget
// that stops agreeing with itself.
//
// Asking the vocabulary for the single nearest word and stopping there makes
// the offer depend on who owns the document that word came from: the nearest
// spelling of a mistyped word can sit in a document the asker may not open, and
// then there is no correction at all, while the same query from a colleague who
// can open it gets one. Silence that moves with somebody else's permissions is
// a channel, so a list is taken and the first spelling this person has a
// document for wins.
//
// A dozen, because the list is sorted by edit distance and the word somebody
// meant is near the top of it or nowhere, and because a dozen costs what one
// costs. The scan that finds them is the same scan whatever the limit is, and
// the words that come back are all asked about in a single read. What the extra
// eleven buy is not a better correction, it is the same correction for everybody
// who can read the document it leads to.
//
// A dozen is also where this stops. A word whose twelve nearest spellings all
// sit in documents the asker cannot open gets no offer, where somebody who can
// open one of them would get one. That, the order the vocabulary comes back in,
// which is by how many documents in the tenant carry each word, and the window
// the driver reads to find it at all, are what is left of a correction drawn
// from a vocabulary with no permission filter on it. See #199.
const CorrectionOffers = 12

// CarriagePool is how many documents the carriage read looks at. It is
// exported, like the rest of the numbers a search spends, because the counter
// test spends it.
//
// It is asking which of a handful of words this person has a document for, and
// the answer is in the candidates it gets back: each one carries the counts for
// the words it holds. A word is carried when a document that holds it comes
// back, so the pool has to be deep enough that one document per word fits in
// it, which the candidate floor is by a wide margin. The cut is ordered by the
// match, and the words at issue here are rare ones, so the document that
// answers for a word is at the top of the pool rather than at the bottom.
const CarriagePool = CandidateFloor

// correct is the "did you mean" on a search that found nothing.
//
// It runs only on the empty result, only on a short query, and only for terms
// the person asking has no document carrying, which is what keeps it from
// second guessing a query that is spelled fine and simply matched nothing this
// week.
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
//
// Whether an offer is made at all is the same question one step earlier, and it
// is the one this used to get wrong. Deciding to stay quiet because the corpus
// already has the word makes the silence itself an answer: type a word nobody
// has and you are offered a correction, type a word that appears only in a
// document you may not open and you are offered nothing, so the two replies
// read the tenant's vocabulary out one word per search. The document
// frequencies are counted over the tenant, which is what makes them stable
// enough to rank with, and that is exactly what makes them the wrong thing to
// branch on here. So a term the corpus has is checked against what this person
// can read before it is passed over, and the spelling that gets offered is the
// nearest one they have a document for rather than the nearest one that exists.
// See [Searcher.carried] and [CorrectionOffers].
func (s *Searcher) correct(ctx context.Context, p *acl.Principal, q Query, terms []string, corpus store.Corpus) (string, error) {
	sp, ok := s.store.(store.Speller)
	if !ok || q.Text == "" || len(terms) == 0 || len(terms) > correctionWords {
		return "", nil
	}

	// The words long enough to be worth correcting, and among those the ones the
	// corpus has at all. A frequency of zero over the tenant is zero over
	// anything inside it, so a word nobody has costs nothing to be sure about
	// and a query of nothing but typos does not read anything here.
	var words, known []string
	for _, t := range terms {
		if utf8.RuneCountInString(t) < correctionShortest {
			continue
		}
		words = append(words, t)
		if corpus.DocFreq[t] > 0 {
			known = append(known, t)
		}
	}
	if len(words) == 0 {
		return "", nil
	}
	has, err := s.carried(ctx, p, known)
	if err != nil {
		return "", err
	}

	offers := make(map[string][]string, len(words))
	var spellings []string
	for _, t := range words {
		if has[t] {
			continue
		}
		near, err := sp.Near(ctx, p, t, CorrectionOffers)
		if err != nil {
			return "", err
		}
		if len(near) > 0 {
			offers[t] = near
			spellings = append(spellings, near...)
		}
	}
	if len(offers) == 0 {
		return "", nil
	}

	// Every spelling of every word in one read, and then the nearest one this
	// person has a document for. Asking about them one at a time would be a
	// statement each, and asking about none of them is what used to leave the
	// choice to whoever owns the nearest document.
	held, err := s.carried(ctx, p, spellings)
	if err != nil {
		return "", err
	}
	swap := make(map[string]string, len(offers))
	for t, near := range offers {
		for _, n := range near {
			if held[n] {
				swap[t] = n
				break
			}
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
	// is the whole question. The words are all carried, and a query made of them
	// can still find nothing: they may be carried by different documents, or by
	// documents outside the filters this search is under.
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

// carried is which of these words the person asking has a document for.
//
// It is the whole list in one read rather than a read per word, which is what
// makes it affordable to be sure about a dozen spellings instead of guessing
// from the nearest one. The candidates come back with the counts for the words
// they hold, so which words are carried is in the answer rather than in a
// second question.
//
// There are no filters on it, because the question is whether a word is spelled
// the way this corpus spells it rather than whether it appears in the four
// sources somebody has ticked. A word that only exists in a source they
// filtered out is still a word, and offering to respell it would be the wrong
// offer.
//
// It goes through the same path a search takes, so it obeys the permission rule
// the driver applies and lands in the same cache a repeat of the query would
// hit, and it happens only on a query that matched nothing.
func (s *Searcher) carried(ctx context.Context, p *acl.Principal, words []string) (map[string]bool, error) {
	if len(words) == 0 {
		return nil, nil
	}
	found, err := s.collect(ctx, p, store.Request{Terms: unique(words)}, store.Selection{Limit: CarriagePool})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(words))
	for _, c := range found.cands {
		for t, n := range c.Terms {
			if n.Title+n.Body > 0 {
				out[t] = true
			}
		}
	}
	return out, nil
}

// unique is the words in the order they were first asked about, with the
// repeats dropped. Two misspelled words in one query can suggest the same
// spelling, and asking the driver about it twice would be a term in the
// statement twice.
func unique(words []string) []string {
	out := make([]string, 0, len(words))
	seen := make(map[string]bool, len(words))
	for _, w := range words {
		if seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
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
