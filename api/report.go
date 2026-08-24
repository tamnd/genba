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

// reportBodyLimit is how large a report may be.
//
// Four kilobytes, which is a sentence about what is wrong. Anybody with more to
// say than that has a conversation to have with the owner, and the name and the
// address are on the document so they can have it.
const reportBodyLimit = 4 << 10

// staleness is the wire shape of what has been said about a document.
//
// A count and the most recent thing said, which is what a reader needs to decide
// whether to trust the page in front of them and what an owner needs to decide
// whether to go and fix it. The whole list is not here, because a panel that
// printed nine near identical complaints would bury the one sentence that says
// what is actually wrong.
type staleness struct {
	// Count is how many different people have said so, which is worth printing
	// precisely because it counts people rather than clicks.
	Count int `json:"count"`

	// By and Email are the most recent of them, so that the reader has somebody
	// to ask rather than a number to argue with.
	By    string `json:"by"`
	Email string `json:"email,omitempty"`

	At time.Time `json:"at"`

	// Note is what is wrong in their words, and is the reason this is worth more
	// than a flag. It is usually there and it is not required.
	Note string `json:"note,omitempty"`

	// Mine is whether the caller is one of those people, which is the question
	// the interface has to answer before it can offer to take a report back.
	//
	// It cannot be worked out from By, because By is the most recent of them and
	// the person asking is usually not the most recent person to have complained.
	// Without it the button says the same thing to somebody who has already
	// reported the document and somebody who has not, which is how a reader files
	// a second report meaning to correct their first.
	Mine bool `json:"mine,omitempty"`
}

// reportRequest is a report as it arrives from the interface.
//
// It carries no reporter and no date, for the same reason a verification does
// not: both are facts the server records rather than claims the caller makes.
type reportRequest struct {
	// Note is what is out of date, in the reader's own words.
	Note string `json:"note,omitempty"`
}

// flag is what the two writes answer with.
//
// The two fields are the two the preview carries under the same names, so the
// interface merges them into the document it is already holding and paints. The
// count is the part it could not have worked out for itself: how many other
// people had already said the same thing is on the server.
type flag struct {
	Stale      *staleness `json:"stale"`
	CanResolve bool       `json:"can_resolve"`
}

// staleOf turns what the driver holds into what the wire carries.
func staleOf(s store.Staleness) *staleness {
	if s.Zero() {
		return nil
	}
	return &staleness{
		Count: s.Count,
		By:    s.Last.By.Display(),
		Email: s.Last.By.Email,
		At:    s.Last.At,
		Note:  s.Last.Note,
		Mine:  s.Mine,
	}
}

// reporter returns the driver's reporting capability, and reports whether this
// deployment has one at all.
func (s *Server) reporter() (store.Reporter, bool) {
	r, ok := s.store.(store.Reporter)
	return r, ok
}

// reported reads what has been said about one document.
//
// Best effort, like the verification and the correction beside it: a driver that
// cannot answer leaves the mark off rather than failing the panel it decorates.
func (s *Server) reported(r *http.Request, p *acl.Principal, id string) store.Staleness {
	rep, ok := s.reporter()
	if !ok {
		return store.Staleness{}
	}
	got, err := rep.Reports(r.Context(), p, []string{id})
	if err != nil {
		s.log.Error("reports could not be read", "error", err, "document", id)
		return store.Staleness{}
	}
	return got[id]
}

// handleReport records that the caller says a document is out of date.
//
// This is the one curation endpoint with no permission of its own beyond being
// able to read the document, and that is the whole point of it. A reader who is
// told they are not important enough to say a runbook is wrong is a reader who
// stops telling anybody, and the corpus improves from that end far faster than
// it improves from a review nobody schedules.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	rep, ok := s.reporter()
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment cannot record that a document is out of date")
		return
	}

	var body reportRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, reportBodyLimit)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "the body must be a JSON object with an optional note")
			return
		}
	}

	d, ok := s.readable(w, r, p)
	if !ok {
		return
	}

	said := store.Report{Doc: d.ID, By: who(p, d), At: s.now(), Note: body.Note}
	s.log.Info("document reported as stale", "subject", p.Subject, "tenant", p.Tenant, "document", d.ID)
	if err := rep.Report(r.Context(), p, said); err != nil {
		s.log.Error("report failed", "error", err, "document", d.ID)
		writeError(w, http.StatusInternalServerError, "internal", "the report could not be recorded")
		return
	}
	// Read back rather than answered from what was just written, because the
	// count is how many people have said this and the caller is one of them
	// rather than all of them.
	writeJSON(w, http.StatusOK, flag{
		Stale:      staleOf(s.reported(r, p, d.ID)),
		CanResolve: store.MayResolve(p, d),
	})
}

// handleResolve clears the reports on a document.
//
// Clearing is a claim in its own right, that somebody accountable for the
// document has dealt with what was said about it, so it is held to the same rule
// as verifying. If any reader could clear a report, the first thing that would
// happen to an inconvenient one is that somebody would clear it.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	rep, ok := s.reporter()
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment cannot record that a document is out of date")
		return
	}
	d, ok := s.mayCurate(w, r, p, store.MayResolve,
		"only the owner or the author of a document can say a report has been dealt with, and an administrator can do it for them")
	if !ok {
		return
	}
	s.log.Info("reports resolved", "subject", p.Subject, "tenant", p.Tenant, "document", d.ID)
	if err := rep.Resolve(r.Context(), p, d.ID); err != nil {
		s.log.Error("resolve failed", "error", err, "document", d.ID)
		writeError(w, http.StatusInternalServerError, "internal", "the reports could not be cleared")
		return
	}
	// Nothing is left to describe, unlike clearing a correction, so this is the
	// one write in the pair that says so with no content.
	w.WriteHeader(http.StatusNoContent)
}

// handleWithdraw takes back the report the caller made.
//
// It sits under the same path as clearing with an extra word on it, and it is
// the other endpoint on that path rather than a relaxation of it. Clearing is
// somebody accountable saying the document has been dealt with, so it takes what
// verifying takes. Withdrawing is a reader taking back their own sentence, so it
// takes what reporting takes, which is nothing beyond being able to read the
// page it is on.
//
// Without it, reporting is a one way door. Somebody who reports the wrong
// document has to go and ask its owner to clear a report that never meant
// anything, and until they do the owner's panel keeps the mistake in front of
// them.
func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	rep, ok := s.reporter()
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment cannot record that a document is out of date")
		return
	}
	d, ok := s.readable(w, r, p)
	if !ok {
		return
	}
	s.log.Info("report withdrawn", "subject", p.Subject, "tenant", p.Tenant, "document", d.ID)
	if err := rep.Withdraw(r.Context(), p, d.ID); err != nil {
		s.log.Error("withdraw failed", "error", err, "document", d.ID)
		writeError(w, http.StatusInternalServerError, "internal", "the report could not be withdrawn")
		return
	}
	// What stands afterwards, the same shape reporting answers with, because the
	// count has changed and so has what the button should say. Clearing answers
	// with no content because it leaves nothing behind by definition, and this
	// usually leaves other people's reports where they were.
	writeJSON(w, http.StatusOK, flag{
		Stale:      staleOf(s.reported(r, p, d.ID)),
		CanResolve: store.MayResolve(p, d),
	})
}

// resolveAfterVerify clears the reports on a document somebody has just vouched
// for.
//
// Putting your name to a document is a stronger statement than clearing a report
// on it, so a verifier who then had to clear the complaints by hand would be
// doing the same work twice, and a document that stayed marked stale under a
// fresh verification would say two things at once.
//
// It is done here rather than inside the drivers, because it is a decision about
// what these two features mean to each other and not a property of storage. It
// is best effort for the same reason: the verification is recorded, and a mark
// that outlives it by a few seconds is worth less than a failed request.
func (s *Server) resolveAfterVerify(r *http.Request, p *acl.Principal, id string) {
	rep, ok := s.reporter()
	if !ok {
		return
	}
	if err := rep.Resolve(r.Context(), p, id); err != nil {
		s.log.Error("reports could not be cleared after a verification", "error", err, "document", id)
	}
}

// readable reads the document named in the path, refusing a caller who may not
// see it exactly as the document endpoint would.
//
// It is mayCurate without the policy, for the endpoint whose policy is that
// anybody who can read the document can do this.
func (s *Server) readable(w http.ResponseWriter, r *http.Request, p *acl.Principal) (doc.Document, bool) {
	d, err := s.store.Get(r.Context(), p, r.PathValue("id"))
	switch {
	case errors.Is(err, genba.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such document")
		return doc.Document{}, false
	case err != nil:
		s.log.Error("document lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "the document could not be read")
		return doc.Document{}, false
	}
	return d, true
}
