package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// Correcting who owns a document.
//
// Ownership is derived, and what a connector derives is often the account that
// ran the import. The rules the endpoint enforces are the same two the
// verification endpoint enforces, and deliberately so: only somebody who owns or
// wrote the document may hand it over, because being the owner is what makes
// somebody able to vouch for it and a corpus where any reader can name
// themselves the owner is a corpus where any reader can vouch for anything. And
// a person who cannot see the document is told nothing at all.

// ownerBody is the block the interface reads to print who is accountable.
type ownerBody struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	By    string    `json:"by"`
	At    time.Time `json:"at"`
}

// documentOwner is the preview as far as this file is concerned.
type documentOwner struct {
	Owner       *ownerBody `json:"owner"`
	CanReassign bool       `json:"can_reassign"`
	CanVerify   bool       `json:"can_verify"`
}

// newOwnerServer holds one document the engineer wrote and a service account
// owns, and one that is somebody else's from end to end.
//
// The first is the case the whole feature is for: a real document, with a real
// author, owned by whatever ran the sync. The second is what tells reading a
// document apart from being able to hand it over.
func newOwnerServer(t *testing.T) (http.Handler, *memstore.Store) {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Put(t.Context(), ownedDocs()...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, api.WithClock(clock))
	return s.Handler(), st
}

func ownedDocs() []doc.Document {
	perm := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
		Version:     1,
	}
	return []doc.Document{
		{
			ID: "mine", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments failover runbook", Body: "Fail the payments queue over to the replica.",
			Author:      doc.Person{Name: "Mei Tanaka", Email: "mei@acme.com"},
			Owner:       doc.Person{Name: "Drive Sync", Email: "drive-sync@acme.com"},
			Permissions: perm,
		},
		{
			// Somebody else wrote this one and Mei ended up owning it, which is
			// what a document handed over once looks like from then on.
			ID: "hers", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments oncall rota", Body: "Who carries the payments pager this week.",
			Author:      doc.Person{Name: "Kenji Sato", Email: "kenji@acme.com"},
			Owner:       doc.Person{Name: "Mei Tanaka", Email: "mei@acme.com"},
			Permissions: perm,
		},
		{
			ID: "theirs", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments retention policy", Body: "Payments records are kept for seven years.",
			Author:      doc.Person{Name: "Kenji Sato", Email: "kenji@acme.com"},
			Owner:       doc.Person{Name: "Kenji Sato", Email: "kenji@acme.com"},
			Permissions: perm,
		},
	}
}

// curator is an administrator who can also read the documents, which is what an
// operator fixing a connector's mistakes on somebody else's behalf looks like.
func curator() map[string]string {
	return map[string]string{
		api.HeaderSubject: "u_ops",
		api.HeaderRoles:   acl.RoleAdmin,
		api.HeaderGroups:  "gdrive:eng@acme.com",
	}
}

func previewOf(t *testing.T, h http.Handler, id string, headers map[string]string) documentOwner {
	t.Helper()
	w := request(t, h, http.MethodGet, "/api/v1/documents/"+id, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("reading %s: %d %s", id, w.Code, w.Body.String())
	}
	return decode[documentOwner](t, w)
}

func TestCorrectingAnOwnerPutsTheirNameOnTheDocument(t *testing.T) {
	h, _ := newOwnerServer(t)

	w := post(t, h, http.MethodPut, "/api/v1/documents/mine/owner", `{"email":"mei@acme.com"}`, owner())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	made := decode[documentOwner](t, w).Owner
	if made == nil || made.Email != "mei@acme.com" {
		t.Errorf("the document was handed to %q", made.Email)
	}
	// The name of the person who said so, and when. A change of ownership with
	// nobody against it is a change nobody can query.
	if made.By != "Mei Tanaka" || !made.At.Equal(clock()) {
		t.Errorf("the correction is recorded against %q at %v", made.By, made.At)
	}

	// And the document itself says so, which is the part that matters: the owner
	// on the preview is the corrected one, not a second field beside the
	// connector's answer.
	got := previewOf(t, h, "mine", owner())
	if got.Owner == nil || got.Owner.Email != "mei@acme.com" {
		t.Fatalf("the preview says the owner is %+v", got.Owner)
	}
	if got.Owner.By != "Mei Tanaka" {
		t.Errorf("the preview does not say who corrected it: %+v", got.Owner)
	}
}

// Reading a document is not the same as being able to hand it over, and the
// difference has to be a refusal rather than a quietly ignored write.
func TestOnlyTheOwnerOrTheAuthorCanReassign(t *testing.T) {
	h, _ := newOwnerServer(t)

	w := post(t, h, http.MethodPut, "/api/v1/documents/theirs/owner", `{"email":"mei@acme.com"}`, owner())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if got := previewOf(t, h, "theirs", owner()); got.CanReassign {
		t.Errorf("a reader who is not the owner is offered a control that would be refused")
	}
}

// A person who cannot see the document is told the same thing they would be told
// if it were not there. Anything else is a way to ask whether an id is real.
func TestReassigningSomethingYouCannotSeeIsANotFound(t *testing.T) {
	h, _ := newOwnerServer(t)

	seen := post(t, h, http.MethodPut, "/api/v1/documents/mine/owner", `{"email":"sam@acme.com"}`, salesperson())
	missing := post(t, h, http.MethodPut, "/api/v1/documents/nothing/owner", `{"email":"sam@acme.com"}`, salesperson())
	if seen.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("a hidden document answered %d and a missing one answered %d, and both should be 404",
			seen.Code, missing.Code)
	}
	if seen.Body.String() != missing.Body.String() {
		t.Errorf("the two answers differ, which is enough to tell that mine exists:\n%s\n%s",
			seen.Body.String(), missing.Body.String())
	}
}

func TestClearingACorrectionPutsTheSourcesAnswerBack(t *testing.T) {
	h, _ := newOwnerServer(t)
	if w := post(t, h, http.MethodPut, "/api/v1/documents/mine/owner", `{"email":"mei@acme.com"}`, owner()); w.Code != http.StatusOK {
		t.Fatalf("set owner: %d %s", w.Code, w.Body.String())
	}

	w := post(t, h, http.MethodDelete, "/api/v1/documents/mine/owner", "", owner())
	if w.Code != http.StatusOK {
		t.Fatalf("clear owner: %d %s", w.Code, w.Body.String())
	}
	// The answer carries the owner that was put back, because it is the
	// connector's and the interface has no way to know it otherwise.
	if back := decode[documentOwner](t, w).Owner; back == nil || back.Email != "drive-sync@acme.com" || back.By != "" {
		t.Errorf("clearing answered with %+v", back)
	}

	got := previewOf(t, h, "mine", owner())
	if got.Owner == nil || got.Owner.Email != "drive-sync@acme.com" {
		t.Fatalf("clearing the correction left the owner as %+v", got.Owner)
	}
	if got.Owner.By != "" {
		t.Errorf("the document is back to what the source says and still claims %q corrected it", got.Owner.By)
	}
}

// The crawl comes round every night and reports the account that ran the import
// every time. A correction that does not survive one is not a correction.
func TestACrawlDoesNotUndoACorrection(t *testing.T) {
	h, st := newOwnerServer(t)
	if w := post(t, h, http.MethodPut, "/api/v1/documents/mine/owner", `{"email":"mei@acme.com"}`, owner()); w.Code != http.StatusOK {
		t.Fatalf("set owner: %d %s", w.Code, w.Body.String())
	}

	if err := st.Put(t.Context(), ownedDocs()...); err != nil {
		t.Fatalf("re-crawl: %v", err)
	}
	got := previewOf(t, h, "mine", owner())
	if got.Owner == nil || got.Owner.Email != "mei@acme.com" {
		t.Fatalf("a crawl handed the document back to %+v", got.Owner)
	}
}

// The two policies are the same policy on purpose. Handing a document to
// somebody is what makes them able to vouch for it, which is why handing one
// over is not something any reader may do.
func TestBeingGivenADocumentIsWhatMakesYouAbleToVouchForIt(t *testing.T) {
	h, _ := newOwnerServer(t)
	if got := previewOf(t, h, "theirs", owner()); got.CanVerify {
		t.Fatalf("a reader who neither owns nor wrote the document is offered the verify button")
	}

	if w := post(t, h, http.MethodPut, "/api/v1/documents/theirs/owner", `{"email":"mei@acme.com","name":"Mei Tanaka"}`, curator()); w.Code != http.StatusOK {
		t.Fatalf("an administrator could not correct the owner: %d %s", w.Code, w.Body.String())
	}
	if got := previewOf(t, h, "theirs", owner()); !got.CanVerify {
		t.Errorf("the new owner cannot vouch for the document they were just given")
	}
}

// Handing over a document you did not write hands over the controls with it.
//
// This is the answer earning its keep. The write says what the person who made
// it may do afterwards, and after this one they may do nothing, so the panel
// takes both buttons away without asking the server a second question.
func TestGivingAwayADocumentYouDidNotWriteTakesTheControlsWithIt(t *testing.T) {
	h, _ := newOwnerServer(t)

	w := post(t, h, http.MethodPut, "/api/v1/documents/hers/owner", `{"email":"kenji@acme.com","name":"Kenji Sato"}`, owner())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	got := decode[documentOwner](t, w)
	if got.Owner == nil || got.Owner.Email != "kenji@acme.com" {
		t.Fatalf("the document went to %+v", got.Owner)
	}
	if got.CanReassign || got.CanVerify {
		t.Errorf("the person who gave it away is still offered the controls: %+v", got)
	}
}

// The address is the whole point of the correction. A name with nobody behind it
// leaves the next reader with somebody to blame and no way to ask.
func TestANewOwnerNeedsAnAddress(t *testing.T) {
	h, _ := newOwnerServer(t)

	for _, body := range []string{`{"name":"Mei Tanaka"}`, `{"email":"mei at acme"}`, `{"email":""}`} {
		w := post(t, h, http.MethodPut, "/api/v1/documents/mine/owner", body, owner())
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400: %s", body, w.Code, w.Body.String())
		}
	}
}
