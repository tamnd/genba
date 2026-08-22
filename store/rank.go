package store

import (
	"context"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
)

// Ranker is the optional capability of a driver that can select the candidates
// worth ranking without materialising the documents behind them.
//
// The distinction from [Retriever] is where the cut happens. Retrieve hands the
// caller the whole match set and the caller throws most of it away after
// scoring. Rank makes the cut inside the same statement that applies the
// permission rule, so the work is proportional to what comes back rather than
// to what matched.
//
// The permission rule does not move, for the same reason it did not move for
// Retrieve: the predicate is in the statement, so no count, facet or candidate
// derived from it can name a document the asker may not read.
type Ranker interface {
	Rank(ctx context.Context, p *acl.Principal, r Request, sel Selection) (Ranked, error)
}

// Selection is how many candidates to cut to, and in what order.
type Selection struct {
	// Limit is the size of the candidate pool. A driver returns at most this
	// many candidates, and when it is also asked for the counts it counts the
	// whole match set.
	//
	// Zero asks for no candidates at all, which is only legal alongside Counts.
	// That is the filter rail on its own: how many documents there are of each
	// source and each kind, with no page under it and nothing to rank. It has to
	// be sayable because the pool has a floor, so a caller that wants the counts
	// and no results cannot ask for them by requesting a small page. It ends up
	// requesting five hundred documents, scoring all of them and reading none,
	// which is what the first request of every session used to do.
	Limit int

	// Recent asks for the most recently modified rather than the best matching.
	//
	// It is here rather than being left to the caller because a caller sorting
	// by date over a pool that was selected by relevance would be showing the
	// most recent of the most relevant, which is neither of the two things
	// anybody asked for.
	Recent bool

	// Counts asks for [Ranked.Total] and [Ranked.Facets].
	//
	// They are the one part of a retrieval whose cost follows the match set
	// rather than the page: a facet count has to count something, and on a query
	// with no terms that something is every document the asker may read. A
	// screen that shows neither a result count nor a sidebar should not pay for
	// them, so it says so, and a driver that is not asked leaves both empty.
	//
	// It is opt in rather than opt out because leaving it out is the cheap
	// answer and a caller that forgets to ask gets a screen with no facets on
	// it, which is visible. Defaulting the other way would mean a caller who
	// forgets gets the slow query, which is not.
	Counts bool

	// Facets bounds how many matching documents the facet counts are counted
	// over. Zero counts every one of them.
	//
	// The total and the facets are asked for together and they are not the same
	// question. A total is one number and a driver answers it with a count over
	// the predicate. The facets are four grouped counts over four display
	// strings, so answering them exactly means reading four columns of every
	// matching document, and on a term that most of the corpus carries that is
	// a pass over most of the corpus to draw a sidebar.
	//
	// Past the bound the counts are a lower bound rather than a count, and
	// [Ranked.Approximate] says which of the two came back. Zero is kept as the
	// exact answer because a driver has to be able to produce it: the
	// conformance suite checks a driver's arithmetic against the same counts
	// computed in Go, and it can only do that against an exact one.
	Facets int
}

// Ranked is one query's retrieval: the candidates worth scoring and the counts
// over the whole match set.
type Ranked struct {
	Candidates []Candidate

	// Total is the size of the match set, not of the candidate pool. It is zero
	// when the selection did not ask for the counts.
	Total int

	// Truncated reports that the match set was larger than the pool, so the
	// ranking is over candidates rather than over everything. A caller is
	// entitled to know which of the two it got. Without the counts there is
	// nothing to compare the pool against, so it is false, and so is it for a
	// selection that asked for no pool: there is no ranking for it to be a claim
	// about, and a total next to an empty page would otherwise always report one.
	Truncated bool

	// Facets are counted over the match set rather than over the page, which is
	// what makes them usable as filters rather than as a description of what is
	// on screen. They are empty when the selection did not ask for the counts,
	// and bounded by [Selection.Facets] when it set a bound.
	//
	// Each field is counted with its own constraint lifted and every other
	// constraint applied: see [Request.Without]. A value somebody has already
	// ticked is counted over the match set itself, because that is the same
	// number arrived at from a smaller set and therefore the more accurate of
	// the two under a bound.
	Facets map[string][]Facet

	// Approximate reports that the facets were counted over the first
	// [Selection.Facets] documents of the match set rather than over all of
	// them, so each count is a lower bound on the true one. Total is exact
	// either way, and a caller showing the counts is expected to say so.
	//
	// A field counted with its own constraint lifted is counted over a larger
	// set than the match set, so it reaches the bound sooner. When a driver
	// cannot tell whether such a count stopped early it says approximate, which
	// overstates the doubt on an exact count rather than understating it on an
	// inexact one.
	Approximate bool
}

// Candidate is what a ranker needs to score a document without reading it, and
// nothing else.
//
// It used to carry the source, the kind, the container and the author as well,
// on the reasoning that a facet is counted over them and a result row is drawn
// from them. Neither turned out to be true. The facets are counted by the driver
// in its counting statement, over its own columns, and the fallback counts them
// in Go from the document. A result row is drawn from the document the page
// fetch returns, because a row needs a snippet and a snippet needs a body. So
// the four were selected, scanned and allocated for five hundred rows on every
// search, read by nothing and asserted by nothing, which meant a driver could
// have returned the wrong author for every candidate and passed conformance.
type Candidate struct {
	ID string

	ModifiedAt time.Time

	// TitleTokens and BodyTokens are the token counts recorded when the
	// document was written.
	TitleTokens int
	BodyTokens  int

	// Terms holds the occurrence counts for the query terms only. A driver does
	// not return the whole term vector: the scorer looks up the query terms and
	// nothing else, and a four hundred word document has three hundred terms it
	// would never read.
	Terms map[string]doc.TermCount
}

// Facet is one value of a facet and how many documents in the match set carry
// it.
type Facet struct {
	Value string
	Count int
}

// MaxFacetValues caps how many values one facet reports.
//
// A facet with ten thousand values is not a filter, it is a scroll bar, and
// counting them all into a response costs more than the sidebar it feeds is
// worth. It lives here rather than next to the ranking because a driver
// counting facets in SQL and a caller counting them in Go have to cut at the
// same place, or the two produce different sidebars for the same query.
const MaxFacetValues = 50

// Statistician is the optional capability of a driver that maintains the corpus
// statistics a scorer needs, instead of leaving them to be derived from
// whatever one query happened to walk past.
//
// BM25 needs three numbers that are properties of the corpus rather than of a
// document: how many documents there are, how long they are on average, and how
// many of them carry each query term. Deriving those from the match set makes
// them depend on the query, which is wrong, and deriving them from a scan makes
// every search read the corpus, which is slow. A driver that keeps them updated
// as it writes answers all three with a handful of key lookups.
type Statistician interface {
	// Statistics returns the corpus numbers for the terms, in the principal's
	// tenant.
	Statistics(ctx context.Context, p *acl.Principal, terms []string) (Corpus, error)
}

// Corpus is what a scorer needs to know about the corpus rather than about one
// document.
//
// The counts are over the tenant, not over what the asker may read. That is a
// deliberate trade and it is worth being explicit about it. A per asker
// document frequency is the more principled definition, and it costs a
// permission filtered aggregate over every document carrying the term on every
// query, which is the entire latency budget spent on a number that moves the
// ranking by a fraction of a percent. What is exposed by the tenant wide form
// is the shape of the ranking, not a document, a title or an id, and the
// document count it derives from is already reported by the stats endpoint.
type Corpus struct {
	// Documents is how many documents in the tenant a query could match.
	Documents int

	// TitleTokens and BodyTokens are the sums over those documents, kept apart
	// so that the average length is a division rather than a scan and so that
	// the weight a ranker puts on a title stays in the ranker. A storage driver
	// that knew the title weight would be a second place the ranking function
	// lives, which is exactly what this milestone is trying not to create.
	TitleTokens int64
	BodyTokens  int64

	// DocFreq is how many of those documents carry each requested term. A term
	// nobody has is absent rather than zero.
	DocFreq map[string]int
}

// AvgLength is the mean document length under the given title weight, and zero
// for an empty corpus.
func (c Corpus) AvgLength(titleWeight float64) float64 {
	if c.Documents == 0 {
		return 0
	}
	return (titleWeight*float64(c.TitleTokens) + float64(c.BodyTokens)) / float64(c.Documents)
}

// Fetcher is the optional capability of a driver that can return a page of
// documents in one round trip.
//
// It exists because the alternative is a call to [Store.Get] per result, which
// is twenty statements, twenty round trips and twenty decodes for a page a
// single statement can answer. The permission rule is unchanged: a driver
// applies the principal inside the same statement, and an id the principal may
// not read is simply absent from the answer rather than being an error, because
// a page that fails because one document was revoked mid query is worse than a
// page with nineteen results on it.
type Fetcher interface {
	Fetch(ctx context.Context, p *acl.Principal, ids []string) ([]doc.Document, error)
}
