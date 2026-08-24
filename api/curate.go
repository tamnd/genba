package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// answerBodyLimit is how large a written answer may be.
//
// Sixty four kilobytes, which is far more prose than anybody should put above a
// list of results and is the point at which what is being written is a document
// rather than an answer. It is a limit on the request rather than a rule about
// good answers, and the reader who has to scroll past a long one is what
// actually enforces the second.
const answerBodyLimit = 64 << 10

// AnswerSources is how many of an answer's sources are drawn under it.
//
// Six, which is a row of citations rather than a second list of results. An
// answer that needs more than six documents to stand up is not an answer, it is
// a reading list, and the reader is better served by the search underneath.
const AnswerSources = 6

// AnswersLimit and AnswersMax bound the maintenance list.
const (
	AnswersLimit = 50
	AnswersMax   = 500
)

// curatedAnswer is the card above the results.
//
// Its sources are whole rows rather than ids, unlike the quotes in an
// extractive answer, and the difference is not an inconsistency. A quote is
// always taken from a document that is already on the page below it, so the
// title is on the wire once. An answer's sources are whatever the person who
// wrote it cited, which has nothing to do with what this query happened to
// rank, so there is nothing on the page for a bare id to point at.
//
// They are the same shape a result row is, so the interface draws a citation
// with the code it already has, and a reader recognises a citation as the same
// kind of thing as a result.
type curatedAnswer struct {
	ID       string `json:"id"`
	Question string `json:"question"`

	// Body is markdown, unrendered, exactly as it was typed. Rendering it is the
	// interface's job for the reason every other body on the wire is: the
	// renderer that draws a document has to be the renderer that draws this, or
	// an answer and the document it cites format the same list two ways.
	Body string `json:"body"`

	// By and Email are who wrote it, and At is when they last did.
	//
	// This is the field the whole card rests on. A reader believes an answer
	// over the four documents underneath it because a person they can go and ask
	// put their name to it, so an unsigned card would be worse than no card.
	By    string    `json:"by"`
	Email string    `json:"email,omitempty"`
	At    time.Time `json:"at"`

	// Until is when it stops counting as current, and State is where that puts
	// it now, in the same three words a verification badge uses.
	//
	// An expired answer still renders and says so. It is a more useful thing for
	// a reader to know about than the silence they would get if it disappeared,
	// and it is the only way the person who wrote it finds out it needs looking
	// at.
	Until time.Time `json:"until"`
	State string    `json:"state"`

	// Sources are the documents it was drawn from, resolved through the reader
	// asking, so an answer written by somebody with broader access does not
	// become a list of the documents this reader may not open.
	Sources []searchHit `json:"sources,omitempty"`
}

// curatedRecord is the same answer as the person maintaining it sees it.
//
// The difference is the sources, which are ids here and rows on the card. This
// is the shape that goes back out to whoever is going to edit it, and an id
// they cannot read has to survive the round trip: resolving the sources for
// this list and then saving what came back would quietly drop every source the
// editor happens not to have access to.
type curatedRecord struct {
	ID       string    `json:"id"`
	Question string    `json:"question"`
	Variants []string  `json:"variants,omitempty"`
	Body     string    `json:"body"`
	Sources  []string  `json:"sources,omitempty"`
	By       string    `json:"by"`
	Email    string    `json:"email,omitempty"`
	At       time.Time `json:"at"`
	Until    time.Time `json:"until"`
	State    string    `json:"state"`
}

// answerRequest is an answer as it arrives from whoever wrote it.
//
// It carries no author and no date, for the same reason a verification does
// not: both are facts the server records rather than claims the caller makes. It
// carries no id either, because the id is in the path.
type answerRequest struct {
	Question string   `json:"question"`
	Variants []string `json:"variants,omitempty"`
	Body     string   `json:"body"`
	Sources  []string `json:"sources,omitempty"`

	// Until is optional and is almost always left out. When it is, the answer
	// gets the standing cadence, which is the right default for prose somebody
	// wrote in a hurry, and an author who knows their answer goes stale sooner
	// says so.
	Until time.Time `json:"until,omitzero"`
}

type answersResponse struct {
	Answers []curatedRecord `json:"answers"`
	At      time.Time       `json:"at"`
}

// identity is the response without the timestamp, so that a list nobody has
// touched revalidates rather than being sent again.
func (a answersResponse) identity() any {
	a.At = time.Time{}
	return a
}

// curator returns the driver's curation capability, and reports whether this
// deployment has one at all.
func (s *Server) curator() (store.Curator, bool) {
	c, ok := s.store.(store.Curator)
	return c, ok
}

// curatedFor is the answer to a query, drawn ready for the wire.
//
// Best effort, like the verifications beside it in a search response. A driver
// that cannot answer, or a query nobody has written an answer to, leaves the
// card off rather than failing the search it sits above, because a page of
// twenty results with no card is the page this feature was added to, and a page
// with no results at all is not.
func (s *Server) curatedFor(r *http.Request, p *acl.Principal, query string) *curatedAnswer {
	c, ok := s.curator()
	if !ok || query == "" {
		return nil
	}
	a, err := c.Curated(r.Context(), p, query)
	switch {
	case errors.Is(err, genba.ErrNotFound):
		return nil
	case err != nil:
		s.log.Error("the curated answer could not be read", "error", err, "tenant", p.Tenant)
		return nil
	}

	out := &curatedAnswer{
		ID:       a.ID,
		Question: a.Question,
		Body:     a.Body,
		By:       a.By.Display(),
		Email:    a.By.Email,
		At:       a.At,
		Until:    a.Until,
		State:    string(a.State(s.now())),
	}
	ids := a.Sources
	if len(ids) > AnswerSources {
		ids = ids[:AnswerSources]
	}
	for _, d := range s.documents(r, p, ids) {
		out.Sources = append(out.Sources, hitOf(d))
	}
	return out
}

// documents resolves a handful of ids through the principal, keeping the order
// they were given in and dropping the ones that come back not found.
//
// Dropping rather than failing is the rule the whole surface runs on: a citation
// to something this reader may not open is not an error, it is a citation that
// is not drawn, and the reader is never told the difference between a document
// they cannot see and one that was deleted.
func (s *Server) documents(r *http.Request, p *acl.Principal, ids []string) []doc.Document {
	if len(ids) == 0 {
		return nil
	}
	out := make([]doc.Document, 0, len(ids))
	if f, ok := s.store.(store.Fetcher); ok {
		found, err := f.Fetch(r.Context(), p, ids)
		if err != nil {
			s.log.Error("the sources of an answer could not be read", "error", err, "tenant", p.Tenant)
			return nil
		}
		// One statement for the page, and then back into the order they were
		// cited in, because the order is the author's and a driver has no reason
		// to preserve it.
		by := make(map[string]doc.Document, len(found))
		for _, d := range found {
			by[d.ID] = d
		}
		for _, id := range ids {
			if d, ok := by[id]; ok {
				out = append(out, d)
			}
		}
		return out
	}
	for _, id := range ids {
		d, err := s.store.Get(r.Context(), p, id)
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	return out
}

// handleAnswers lists the answers this tenant has written down.
//
// It is the maintenance view rather than anything a reader sees, which is why
// it sits behind the administrator guard with the connectors: the reader's view
// of an answer is the card above their results, and it arrives with the search.
func (s *Server) handleAnswers(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	c, ok := s.curator()
	if !ok {
		// An empty list rather than a refusal, for the reason the inbox answers
		// with one: the screen that reads this draws nothing, instead of drawing
		// an error about a capability nobody asked for.
		writeConditional(w, r, http.StatusOK, answersResponse{Answers: []curatedRecord{}}, nil)
		return
	}

	limit, err := intParam(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "limit must be a number")
		return
	}
	if limit <= 0 {
		limit = AnswersLimit
	}
	limit = min(limit, AnswersMax)

	all, err := c.Answers(r.Context(), p, limit)
	if err != nil {
		s.log.Error("the answers could not be listed", "error", err, "tenant", p.Tenant)
		writeError(w, http.StatusInternalServerError, "internal", "the answers could not be read")
		return
	}

	now := s.now()
	out := answersResponse{Answers: make([]curatedRecord, 0, len(all)), At: now}
	for _, a := range all {
		out.Answers = append(out.Answers, curatedRecord{
			ID:       a.ID,
			Question: a.Question,
			Variants: a.Variants,
			Body:     a.Body,
			Sources:  a.Sources,
			By:       a.By.Display(),
			Email:    a.By.Email,
			At:       a.At,
			Until:    a.Until,
			State:    string(a.State(now)),
		})
	}
	writeConditional(w, r, http.StatusOK, out, out.identity())
}

// handleCurate writes an answer, or replaces the one that was there.
//
// Writing one is the same call as editing one, and both are also how an answer
// is re-verified: the date on the card is the date somebody last stood behind
// the words, and there is no separate act that produces it.
func (s *Server) handleCurate(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	c, ok := s.curator()
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment cannot remember a written answer")
		return
	}
	if !store.MayCurate(p) {
		writeError(w, http.StatusForbidden, "forbidden", "only an administrator can write an answer")
		return
	}

	var body answerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, answerBodyLimit)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be a JSON object with a question and an answer in it")
		return
	}

	now := s.now()
	until := body.Until
	if until.IsZero() {
		until = now.Add(store.Cadence)
	}
	a := store.Answer{
		ID:       r.PathValue("id"),
		Question: body.Question,
		Variants: body.Variants,
		Body:     body.Body,
		Sources:  body.Sources,
		By:       whoever(p),
		At:       now,
		Until:    until,
	}
	// Checked here as well as in the driver, because the difference between a
	// request that was wrong and a deployment that broke is the difference
	// between a 400 and a 500, and a driver returning one error for both would
	// have the interface tell somebody to try again with the same words.
	if err := a.Check(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	s.log.Info("answer written", "subject", p.Subject, "tenant", p.Tenant, "answer", a.ID)
	if err := c.Curate(r.Context(), p, a); err != nil {
		s.log.Error("the answer could not be written", "error", err, "answer", a.ID)
		writeError(w, http.StatusInternalServerError, "internal", "the answer could not be written")
		return
	}
	writeJSON(w, http.StatusOK, curatedRecord{
		ID:       a.ID,
		Question: a.Question,
		Variants: a.Variants,
		Body:     a.Body,
		Sources:  a.Sources,
		By:       a.By.Display(),
		Email:    a.By.Email,
		At:       a.At,
		Until:    a.Until,
		State:    string(a.State(now)),
	})
}

// handleRetract takes an answer down.
//
// Taking down one that is not there is not an error, so that a mistake can be
// undone twice and so that two administrators clearing the same bad answer do
// not race each other into a failure.
func (s *Server) handleRetract(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	c, ok := s.curator()
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment cannot remember a written answer")
		return
	}
	if !store.MayCurate(p) {
		writeError(w, http.StatusForbidden, "forbidden", "only an administrator can take an answer down")
		return
	}

	id := r.PathValue("id")
	s.log.Info("answer retracted", "subject", p.Subject, "tenant", p.Tenant, "answer", id)
	if err := c.Retract(r.Context(), p, id); err != nil {
		s.log.Error("the answer could not be retracted", "error", err, "answer", id)
		writeError(w, http.StatusInternalServerError, "internal", "the answer could not be taken down")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
