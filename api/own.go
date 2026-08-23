package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// ownerBodyLimit is how large a request to change an owner may be.
//
// Four kilobytes, which is a name and an address. There is nothing else in it,
// and a body larger than this is a client that has misunderstood the endpoint.
const ownerBodyLimit = 4 << 10

// owner is the wire shape of who is accountable for a document.
//
// The name and the address are always the owner as it stands, corrected or not,
// because that is what the interface prints and it should not have to work out
// which of two fields to read. By and At are the provenance and are absent until
// somebody has disagreed with the source, which is what lets the interface say
// where this answer came from rather than presenting a person's judgement as a
// fact from the connector.
type owner struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`

	By string    `json:"by,omitempty"`
	At time.Time `json:"at,omitzero"`
}

// handover is what both writes answer with.
//
// The three fields are the three the preview carries under the same names, so
// the interface merges them into the document it is already holding and paints.
// The owner alone would not be enough: being the owner is what makes somebody
// able to vouch for a document, so a person who has just claimed one has earned
// a button that was not there a moment ago, and a panel that only learned the
// new name would go on hiding it until the next load.
type handover struct {
	Owner       *owner `json:"owner"`
	CanVerify   bool   `json:"can_verify"`
	CanReassign bool   `json:"can_reassign"`
}

// answer is the document as it stands after one of the two writes.
func (s *Server) answer(p *acl.Principal, d doc.Document, c store.Correction) handover {
	out := handover{Owner: ownerOf(d, c), CanReassign: store.MayReassign(p, d)}
	if _, ok := s.verifier(); ok {
		out.CanVerify = store.MayVerify(p, d)
	}
	return out
}

// ownerRequest is a correction as it arrives from the interface.
//
// It carries no corrector and no date, for the same reason a verification does
// not: both are facts the server records rather than claims the caller gets to
// make, and a request that could set them would be a request that could put
// somebody else's name against a change they did not make.
type ownerRequest struct {
	// Email is who owns the document now. It is an address rather than a subject
	// id because the person choosing knows the address, and because a document
	// whose owner is an unresolved identity is exactly what happens when a
	// connector cannot map one either.
	Email string `json:"email"`

	// Name is what to call them, and is the address when it is absent.
	Name string `json:"name,omitempty"`
}

// ownerOf turns the document and any correction on it into what the wire
// carries.
func ownerOf(d doc.Document, c store.Correction) *owner {
	out := owner{Name: d.Owner.Display(), Email: d.Owner.Email}
	if !c.Zero() {
		out.By, out.At = c.By.Display(), c.At
	}
	if out.Name == "" && out.Email == "" {
		// A document nobody owns says so by leaving the block out, rather than by
		// carrying an empty name the interface would have to test for.
		return nil
	}
	return &out
}

// ownership returns the driver's ownership capability, and reports whether this
// deployment has one at all.
func (s *Server) ownership() (store.Ownership, bool) {
	o, ok := s.store.(store.Ownership)
	return o, ok
}

// correction reads the standing correction on one document.
//
// Best effort, like the verification beside it: a driver that cannot answer
// leaves the provenance off rather than failing the panel it decorates. The
// owner shown is right either way, because the correction was written into the
// document when it was made.
func (s *Server) correction(r *http.Request, p *acl.Principal, id string) store.Correction {
	o, ok := s.ownership()
	if !ok {
		return store.Correction{}
	}
	got, err := o.Corrections(r.Context(), p, []string{id})
	if err != nil {
		s.log.Error("corrections could not be read", "error", err, "document", id)
		return store.Correction{}
	}
	return got[id]
}

// handleSetOwner records that the source named the wrong owner.
//
// The three refusals are told apart here exactly as they are for a
// verification, and for the same reasons: a caller who may not see the document
// gets the not found the document itself would give them, and a caller who can
// see it but may not hand it over gets a forbidden, because they already know
// the document is there and the useful answer is why they cannot do this.
func (s *Server) handleSetOwner(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	o, ok := s.ownership()
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment cannot record a change of owner")
		return
	}

	var body ownerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, ownerBodyLimit)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be a JSON object with an email address and an optional name")
		return
	}
	next, ok := personOf(p, body)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "the new owner needs an email address, so that somebody reading the document knows who to ask")
		return
	}

	d, ok := s.reassignable(w, r, p)
	if !ok {
		return
	}

	now := s.now()
	// The corrector is named the same way a verifier is, from the document
	// first and the principal's own identity after it.
	c := store.Correction{Doc: d.ID, Owner: next, By: who(p, d), At: now}
	s.log.Info("document reassigned", "subject", p.Subject, "tenant", p.Tenant, "document", d.ID, "owner", next.Email)
	if err := o.SetOwner(r.Context(), p, c); err != nil {
		s.log.Error("set owner failed", "error", err, "document", d.ID)
		writeError(w, http.StatusInternalServerError, "internal", "the change of owner could not be recorded")
		return
	}
	d.Owner = next
	writeJSON(w, http.StatusOK, s.answer(p, d, c))
}

// handleClearOwner puts back the owner the source reported.
//
// It answers with the owner rather than with no content, which is where it
// parts company with withdrawing a verification. Withdrawing one leaves
// nothing, so the interface can paint the result without being told. Clearing a
// correction leaves whatever the connector says, which only the server knows,
// and the alternative is a second request to find out what just happened.
func (s *Server) handleClearOwner(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	o, ok := s.ownership()
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment cannot record a change of owner")
		return
	}
	d, ok := s.reassignable(w, r, p)
	if !ok {
		return
	}
	// Read before the write, because the source's answer is in the correction
	// that is about to be deleted. Reading the document again afterwards would
	// be a second round trip to learn something this request already had.
	c := s.correction(r, p, d.ID)
	s.log.Info("owner correction cleared", "subject", p.Subject, "tenant", p.Tenant, "document", d.ID)
	if err := o.ClearOwner(r.Context(), p, d.ID); err != nil {
		s.log.Error("clear owner failed", "error", err, "document", d.ID)
		writeError(w, http.StatusInternalServerError, "internal", "the change of owner could not be undone")
		return
	}
	if !c.Zero() {
		d.Owner = c.Was
	}
	writeJSON(w, http.StatusOK, s.answer(p, d, store.Correction{}))
}

// reassignable reads the document named in the path and checks that this caller
// may change who owns it, writing the refusal itself when they may not.
func (s *Server) reassignable(w http.ResponseWriter, r *http.Request, p *acl.Principal) (doc.Document, bool) {
	return s.mayCurate(w, r, p, store.MayReassign,
		"only the owner or the author of a document can hand it over, and an administrator can do it for them")
}

// personOf is the new owner as the request describes them.
//
// The subject is filled in when the address is one of the caller's own, which is
// the common case: somebody claiming a document a connector attributed to the
// account that imported it. Without it the claim would rest on the address
// alone, and the person would keep their new document until the day their
// address changed.
func personOf(p *acl.Principal, body ownerRequest) (doc.Person, bool) {
	next := doc.Person{
		Name:  strings.TrimSpace(body.Name),
		Email: strings.TrimSpace(body.Email),
	}
	if !strings.Contains(next.Email, "@") || strings.ContainsAny(next.Email, " \t\r\n") {
		return doc.Person{}, false
	}
	if next.Name == "" {
		next.Name = next.Email
	}
	if store.IsPerson(p, next) {
		next.Subject = p.Subject
	}
	return next, true
}
