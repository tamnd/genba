package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
)

// Answer is a question somebody answered once so that nobody has to answer it
// again.
//
// Everything else in this package improves the corpus by ranking it, marking it
// or complaining about it. This is the one object that adds to it. It exists
// because a search engine is only ever as good as what was written down, and
// the questions people actually ask are the ones nobody wrote down: what the
// deploy freeze dates are, which of the four onboarding documents is current,
// who to ask about billing. The answer is in somebody's head and in a chat
// thread from March, and no amount of ranking finds it.
//
// It is written by a person and quoted verbatim. Nothing here is generated, and
// the body is stored as the markdown somebody typed rather than as anything
// derived from the documents it cites, which is what makes it worth outranking
// them: it is not a better summary of the corpus, it is a claim the corpus does
// not contain.
//
// The whole design rests on one rule taken from every curation system that has
// ever failed: curation has to be cheap to write and immediately visible, or
// the curator does the work, sees nothing change, and never does it again. So
// an answer is a question, a body, and a name, it takes effect on the next
// search, and everything else about it is optional.
type Answer struct {
	// ID is the answer, chosen by whoever wrote it. It is a caller's id for the
	// same reason a document's is: the thing that owns the name is the thing
	// that has to reference it later.
	ID string

	// Question is the canonical phrasing, as written, and is what a reader sees
	// at the top of the card. Variants are the other ways people ask it.
	//
	// Both are matched by [AnswerKey], so the punctuation and the capitals in
	// them are for the reader rather than for the lookup, and "What's the deploy
	// freeze?" is found by somebody who typed it without the question mark.
	Question string
	Variants []string

	// Body is the answer itself, in markdown. It is the only free text here and
	// it is deliberately not bounded by this package: an answer long enough to
	// be a problem is a document, and the person writing it will find that out
	// from the reader who has to scroll past it.
	Body string

	// Sources are the documents the answer is drawn from, by id.
	//
	// They are ids and not documents because the reader they are shown to is not
	// necessarily the person who wrote them down. Whoever renders an answer
	// resolves these through the principal asking, and the ones that come back
	// not found are simply not drawn, so an answer written by somebody with
	// broader access does not become a list of the documents a reader may not
	// open. That is why nothing on this type carries a title.
	Sources []string

	// By is who wrote it, carried whole so that the card has a name on it
	// without a second lookup, and At is when they last wrote it.
	By doc.Person
	At time.Time

	// Until is when it stops counting as current.
	//
	// An answer is a verification of itself. Editing one is the same act as
	// saying it is still true, made by the same person, so there is no separate
	// verifier here and no second date: At is both when it was written and when
	// somebody last stood behind it, and Until is how long that lasts. A
	// deployment that wanted those to be different people would be asking a
	// reviewer to vouch for prose they cannot edit, which is a workflow rather
	// than a fact about an answer.
	//
	// It is stored rather than derived from At plus [Cadence] for the reason a
	// verification's is: the cadence is policy and it will change, and an answer
	// written under the old one keeps the deadline it was given.
	Until time.Time
}

// State reports where the answer is in its life at the given time, using the
// same three words and the same window a verification uses.
//
// An expired answer is still an answer and still renders. It says how long it
// has been since anybody stood behind it, which is a more useful thing for a
// reader to know than the silence they would get if it disappeared, and it is
// the only way the person who wrote it ever finds out it needs looking at.
func (a Answer) State(now time.Time) State { return stateAt(a.Until, now) }

// Zero reports whether there is no answer here at all.
func (a Answer) Zero() bool { return a.ID == "" }

// What [Answer.Check] stands behind.
var (
	ErrNoQuestion = errors.New("answer has no question")
	ErrNoBody     = errors.New("answer has no body")
	ErrNoAuthor   = errors.New("answer has no author")
	ErrNoAnswerID = errors.New("answer has no id")
)

// Check rejects an answer that cannot be stored.
//
// The three required fields are the three a card is unreadable without. An
// answer with no question is unfindable, one with no body is a heading, and one
// with nobody's name on it is the anonymous wiki page this type exists to
// replace: the entire reason a reader believes an answer over the four
// documents under it is that a person they can go and ask put their name to it.
//
// An answer with no expiry is refused for the reason a verification with none
// is. A claim that never runs out is a boolean that nothing ever unsets, and a
// corpus full of those is the state this whole area of the product is trying to
// get out of.
func (a Answer) Check() error {
	switch {
	case a.ID == "":
		return ErrNoAnswerID
	case AnswerKey(a.Question) == "":
		return ErrNoQuestion
	case strings.TrimSpace(a.Body) == "":
		return ErrNoBody
	case a.By.Name == "" && a.By.Subject == "" && a.By.Email == "":
		return ErrNoAuthor
	case a.Until.IsZero():
		return ErrNoExpiry
	}
	return nil
}

// Keys is every phrasing this answer is found by, folded and deduplicated.
//
// It is a method rather than something each driver works out, because the set
// of rows a driver writes has to be the set of keys a lookup asks for, and two
// spellings of that would give an answer that can be written and never found.
func (a Answer) Keys() []string {
	out := make([]string, 0, len(a.Variants)+1)
	for _, phrasing := range append([]string{a.Question}, a.Variants...) {
		k := AnswerKey(phrasing)
		if k == "" {
			continue
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// AnswerKey folds a phrasing into the string a query is matched against.
//
// It is the index's own analyzer with the terms joined back up, so a question
// and a query that produce the same terms produce the same key. Using anything
// else here means an answer that matches a query the search does not, which is
// a card above a list of results that has nothing to do with it.
//
// The match is the whole key and not part of it. That is a deliberately strict
// rule and it is the important decision in this file. A card above the results
// claims authority, and the cost of getting it wrong is not a bad result, it is
// a reader believing a confident answer to a question they did not ask. Until
// there is a model that can score a query against a question and be held to a
// confidence floor, the phrasings an answer is found by are the ones a person
// wrote down, and a near miss gets the ordinary list of results it would have
// got before this existed.
func AnswerKey(s string) string { return strings.Join(doc.Tokenize(s), " ") }

// MayCurate reports whether the principal may write answers.
//
// Administrators, which is narrower than the product wants and is the right
// place to start. An answer outranks the documents it cites for every reader in
// the tenant, so the blast radius of a bad one is the whole deployment, and a
// permission that starts narrow can be widened once there is a role to widen it
// to. The role model this eventually wants is a content manager grant rather
// than an administrator, and that is a decision about roles rather than about
// answers.
//
// It lives here beside [MayVerify] rather than inside a driver for the same
// reason that one does: it is a curation policy, made once, above the storage
// layer, and what a driver enforces is the tenant.
func MayCurate(p *acl.Principal) bool { return p.HasRole(acl.RoleAdmin) }

// Curator is the optional capability of a driver that can remember an answer
// somebody wrote.
//
// It is optional like every capability outside [Store]. A deployment whose
// driver does not implement it searches exactly as it did before this existed,
// with no card above the results and no way to write one, rather than with a
// button that fails.
//
// The permission rule is not the document rule and this is the one place in the
// package where that is true. An answer belongs to the tenant rather than to a
// principal: everybody who can search can read every answer, because an answer
// is the product's own text and the whole point of writing one is that the next
// person finds it. What does not follow is the documents it cites, which stay
// behind the permission they always had, and [Answer.Sources] carries ids
// precisely so that resolving them applies it.
//
// Anything genuinely sensitive belongs in a permissioned document rather than
// in an answer, and that is a rule for whoever writes one rather than something
// a driver can enforce. It is written down here because a curation object with
// an audience field would look like it enforces it, which is how a system ends
// up with salary bands on the front page.
type Curator interface {
	Store

	// Curate writes an answer, replacing any earlier one with the same id.
	//
	// Replacing rather than appending is what makes editing an answer the same
	// call as writing one, and it is why re-writing an answer is also how it is
	// re-verified. The phrasings it is found by are replaced with it: a variant
	// dropped from the answer stops matching, or an answer could never lose a
	// question it turned out not to answer.
	//
	// A phrasing that already belongs to another answer moves to this one. Two
	// answers claiming the same question is a curation conflict rather than a
	// storage problem, and the resolution that does not need a screen is that
	// the most recent writer wins.
	Curate(ctx context.Context, p *acl.Principal, a Answer) error

	// Retract removes an answer. Retracting one that is not there is not an
	// error, so that a mistake can be undone twice.
	Retract(ctx context.Context, p *acl.Principal, id string) error

	// Curated returns the answer to a question, or [genba.ErrNotFound].
	//
	// The question is a phrasing rather than a key: folding it is this method's
	// job, so that a caller cannot fold it differently.
	Curated(ctx context.Context, p *acl.Principal, question string) (Answer, error)

	// Answers lists the answers in the tenant, most recently written first, at
	// most limit of them. A limit of zero or less returns nothing.
	//
	// Most recent first because the list is read by whoever maintains it, and
	// what they are looking for is either the one they just wrote or the ones
	// nobody has touched since the reorganisation.
	Answers(ctx context.Context, p *acl.Principal, limit int) ([]Answer, error)
}
