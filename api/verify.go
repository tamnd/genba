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

// verifyBodyLimit is how large a verification request may be.
//
// Four kilobytes, which is a note, an expiry and nothing else. The note exists
// for the sentence a badge cannot express, not for a second copy of the
// document.
const verifyBodyLimit = 4 << 10

// verification is the wire shape of a claim somebody made about a document.
//
// The state is computed here rather than in the browser. It is derived from a
// date, a cadence and a window, and a client that worked any of the three out
// for itself would be a second place the policy lives, drawing an amber badge a
// fortnight after the server thinks it should be red.
type verification struct {
	// State is fresh, expiring or expired. It is the only field the interface
	// needs to decide how the badge looks.
	State string `json:"state"`

	// By is who put their name to it, and Email is how to reach them, which is
	// the whole point of a signal that is a person rather than a flag.
	By    string `json:"by"`
	Email string `json:"email,omitempty"`

	At    time.Time `json:"at"`
	Until time.Time `json:"until"`

	// Note is why, in the verifier's own words, and is usually empty.
	Note string `json:"note,omitempty"`
}

// verifyRequest is a claim as it arrives from the interface.
//
// It carries no verifier and no date. Both are facts the server records rather
// than claims the caller gets to make, and a request that could set them would
// be a request that could put somebody else's name on a badge.
type verifyRequest struct {
	// Note is the sentence that goes with the claim.
	Note string `json:"note,omitempty"`

	// Days is how long the claim should last, and is [store.Cadence] when it is
	// absent or not positive. A verifier who knows their document changes weekly
	// says so, and everybody else gets the default.
	Days int `json:"days,omitempty"`
}

// verifiedOf turns what the driver holds into what the wire carries.
func verifiedOf(v store.Verification, now time.Time) *verification {
	if v.Zero() {
		return nil
	}
	return &verification{
		State: string(v.State(now)),
		By:    v.By.Name,
		Email: v.By.Email,
		At:    v.At,
		Until: v.Until,
		Note:  v.Note,
	}
}

// verifier returns the driver's verification capability, and reports whether
// this deployment has one at all.
func (s *Server) verifier() (store.Verifier, bool) {
	v, ok := s.store.(store.Verifier)
	return v, ok
}

// verifications reads the claims on a page of documents.
//
// It is one call for the whole page and it is best effort: a driver that cannot
// answer leaves the badges off rather than failing the search behind them. A
// page of twenty results is not worth a five hundred because the table that
// decorates it is unavailable, and the reader loses a badge rather than the
// answer they asked for.
func (s *Server) verifications(r *http.Request, p *acl.Principal, ids []string) map[string]store.Verification {
	v, ok := s.verifier()
	if !ok || len(ids) == 0 {
		return nil
	}
	got, err := v.Verifications(r.Context(), p, ids)
	if err != nil {
		s.log.Error("verifications could not be read", "error", err, "tenant", p.Tenant)
		return nil
	}
	return got
}

// handleVerify records that the caller vouches for a document.
//
// The three refusals are told apart on purpose here, unlike the ones around
// reading a document. A caller who may not see the document gets the same not
// found the document itself would give them, which keeps the endpoint from
// being a way to ask whether an id exists. A caller who can see it but is not
// its owner gets a forbidden, because at that point they already know the
// document is there and the useful answer is why they cannot do this.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	v, ok := s.verifier()
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment cannot record who verified a document")
		return
	}

	var body verifyRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, verifyBodyLimit)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "the body must be a JSON object with an optional note and number of days")
			return
		}
	}

	d, ok := s.verifiable(w, r, p)
	if !ok {
		return
	}

	now := s.now()
	until := now.Add(store.Cadence)
	if body.Days > 0 {
		until = now.AddDate(0, 0, body.Days)
	}
	claim := store.Verification{
		Doc:   d.ID,
		By:    who(p, d),
		At:    now,
		Until: until,
		Note:  body.Note,
	}
	s.log.Info("document verified", "subject", p.Subject, "tenant", p.Tenant, "document", d.ID, "until", until)
	if err := v.Verify(r.Context(), p, claim); err != nil {
		s.log.Error("verify failed", "error", err, "document", d.ID)
		writeError(w, http.StatusInternalServerError, "internal", "the verification could not be recorded")
		return
	}
	// Putting your name to a document answers whatever anybody had said was
	// wrong with it, so the reports go with it.
	s.resolveAfterVerify(r, p, d.ID)
	writeJSON(w, http.StatusOK, verifiedOf(claim, now))
}

// handleUnverify withdraws the claim.
func (s *Server) handleUnverify(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	v, ok := s.verifier()
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment cannot record who verified a document")
		return
	}
	d, ok := s.verifiable(w, r, p)
	if !ok {
		return
	}
	s.log.Info("verification withdrawn", "subject", p.Subject, "tenant", p.Tenant, "document", d.ID)
	if err := v.Unverify(r.Context(), p, d.ID); err != nil {
		s.log.Error("unverify failed", "error", err, "document", d.ID)
		writeError(w, http.StatusInternalServerError, "internal", "the verification could not be withdrawn")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// verifiable reads the document named in the path and checks that this caller is
// allowed to make a claim about it, writing the refusal itself when they are
// not.
func (s *Server) verifiable(w http.ResponseWriter, r *http.Request, p *acl.Principal) (doc.Document, bool) {
	return s.mayCurate(w, r, p, store.MayVerify,
		"only the owner or the author of a document can say it is current, and an administrator can do it for them")
}

// mayCurate is the shape every curation endpoint has: read the document named in
// the path, refuse the caller who may not see it the way the document itself
// would, and refuse the caller who may see it but not do this with a reason.
//
// The policy is passed in rather than named here so that the two endpoints
// cannot answer the permission question in two places and eventually differ
// about which of them is stricter.
func (s *Server) mayCurate(
	w http.ResponseWriter,
	r *http.Request,
	p *acl.Principal,
	allow func(*acl.Principal, doc.Document) bool,
	refusal string,
) (doc.Document, bool) {
	d, err := s.store.Get(r.Context(), p, r.PathValue("id"))
	switch {
	case errors.Is(err, genba.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such document")
		return doc.Document{}, false
	case err != nil:
		s.log.Error("document lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "the document could not be read")
		return doc.Document{}, false
	case !allow(p, d):
		writeError(w, http.StatusForbidden, "forbidden", refusal)
		return doc.Document{}, false
	}
	return d, true
}

// who is the principal as the person a badge names.
//
// The document is consulted first, because a principal is a subject and a list
// of identities and none of that is a name anybody recognises, while the owner
// and the author fields on the document carry the name the source knows the
// person by. Somebody verifying a document they own therefore gets their real
// name on the badge without this package having to learn about a directory.
//
// Failing that it is the first identity, and failing that the subject. A badge
// that says u-4181 vouched for this is a poor badge and it is still one
// somebody can chase down, which is more than an empty one gives them.
func who(p *acl.Principal, d doc.Document) doc.Person {
	for _, known := range []doc.Person{d.Owner, d.Author} {
		if known.Name != "" && store.IsPerson(p, known) {
			known.Subject = p.Subject
			return known
		}
	}
	person := doc.Person{Subject: p.Subject, Name: p.Subject}
	for _, id := range p.Identities {
		if id.Value == "" {
			continue
		}
		person.Email, person.Name = id.Value, id.Value
		break
	}
	return person
}
