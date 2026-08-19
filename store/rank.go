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
	// many candidates and still counts the whole match set.
	Limit int

	// Recent asks for the most recently modified rather than the best matching.
	//
	// It is here rather than being left to the caller because a caller sorting
	// by date over a pool that was selected by relevance would be showing the
	// most recent of the most relevant, which is neither of the two things
	// anybody asked for.
	Recent bool
}

// Ranked is one query's retrieval: the candidates worth scoring and the counts
// over the whole match set.
type Ranked struct {
	Candidates []Candidate

	// Total is the size of the match set, not of the candidate pool.
	Total int

	// Truncated reports that the match set was larger than the pool, so the
	// ranking is over candidates rather than over everything. A caller is
	// entitled to know which of the two it got.
	Truncated bool

	// Facets are counted over the whole match set, which is what makes them
	// usable as filters rather than as a description of the current page.
	Facets map[string][]Facet
}

// Candidate is what a ranker needs to score a document without reading it.
//
// The four strings are the display forms a facet is counted over and a result
// row is drawn from. They are here rather than decoded out of the document
// because decoding the document is the cost being removed.
type Candidate struct {
	ID        string
	Source    string
	Kind      doc.Kind
	Container string
	Author    string

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
