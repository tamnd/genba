package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// Saying that a document is current.
//
// The endpoint is small and the two rules it enforces are not. Only somebody
// who owns or wrote the document may put their name to it, because a badge
// anybody can apply is a badge that means somebody read the title. And a person
// who cannot see the document is told nothing at all, because a refusal that
// distinguishes forbidden from missing is a way to ask whether a document
// exists.

// clock is a fixed time, so that an expiry computed from it is the expiry the
// test asked for rather than whatever the machine managed between two calls.
func clock() time.Time { return time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC) }

// newVerifyServer holds one document owned by the engineer and one owned by
// somebody else, both readable by the same group.
//
// The second one is the interesting case: being able to read a document is not
// the same as being able to vouch for it, and this is the pair that tells the
// two apart.
func newVerifyServer(t *testing.T) http.Handler {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	perm := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
		Version:     1,
	}
	docs := []doc.Document{
		{
			ID: "mine", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments failover runbook", Body: "Fail the payments queue over to the replica.",
			Owner:       doc.Person{Name: "Mei Tanaka", Email: "mei@acme.com"},
			Permissions: perm,
		},
		{
			ID: "theirs", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments retention policy", Body: "Payments records are kept for seven years.",
			Owner:       doc.Person{Name: "Kenji Sato", Email: "kenji@acme.com"},
			Permissions: perm,
		},
	}
	if err := st.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, api.WithClock(clock))
	return s.Handler()
}

// owner is the engineer, with the identity that makes them the owner of the
// first document. It is the same person as engineer(), told apart by the
// identity header rather than by the subject, because that is how a connector
// names an owner it could not resolve to a subject.
func owner() map[string]string {
	return map[string]string{
		api.HeaderSubject:    "u_mei",
		api.HeaderGroups:     "gdrive:eng@acme.com",
		api.HeaderIdentities: "gdrive:mei@acme.com",
	}
}

func TestVerifyingPutsANameAndADateOnTheDocument(t *testing.T) {
	h := newVerifyServer(t)

	w := post(t, h, http.MethodPost, "/api/v1/documents/mine/verify", `{"note":"checked after the failover"}`, owner())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var made struct {
		State string    `json:"state"`
		By    string    `json:"by"`
		Email string    `json:"email"`
		At    time.Time `json:"at"`
		Until time.Time `json:"until"`
		Note  string    `json:"note"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &made); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The name comes off the document rather than out of the principal, because
	// a subject is not a name anybody recognises and the owner field already
	// carries the one the source knows them by.
	if made.By != "Mei Tanaka" {
		t.Errorf("the badge names %q, and the document says the owner is Mei Tanaka", made.By)
	}
	if made.Email != "mei@acme.com" {
		t.Errorf("the badge has no way to reach the verifier: %q", made.Email)
	}
	if made.State != "fresh" {
		t.Errorf("a claim made a moment ago is %q", made.State)
	}
	if !made.At.Equal(clock()) {
		t.Errorf("verified at %v, and the request was made at %v", made.At, clock())
	}
	if want := clock().Add(180 * 24 * time.Hour); !made.Until.Equal(want) {
		t.Errorf("expires at %v, and the default cadence puts it at %v", made.Until, want)
	}
	if made.Note != "checked after the failover" {
		t.Errorf("the note came back as %q", made.Note)
	}
}

// Reading a document is not the same as being able to vouch for it, and the
// difference has to be a refusal rather than a quietly ignored write.
func TestOnlyTheOwnerOrTheAuthorCanVerify(t *testing.T) {
	h := newVerifyServer(t)

	w := post(t, h, http.MethodPost, "/api/v1/documents/theirs/verify", "", owner())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	// The refusal says which rule was hit, because the caller can already see
	// the document and the useful half of the answer is what to do about it.
	if body := w.Body.String(); !strings.Contains(body, "owner") {
		t.Errorf("the refusal does not say why: %s", body)
	}
}

// A person who cannot see the document is told the same thing they would be
// told if it were not there. Anything else is a way to ask whether an id is
// real.
func TestVerifyingSomethingYouCannotSeeIsANotFound(t *testing.T) {
	h := newVerifyServer(t)

	seen := post(t, h, http.MethodPost, "/api/v1/documents/mine/verify", "", salesperson())
	missing := post(t, h, http.MethodPost, "/api/v1/documents/nothing/verify", "", salesperson())
	if seen.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("a hidden document answered %d and a missing one answered %d, and both should be 404",
			seen.Code, missing.Code)
	}
	if seen.Body.String() != missing.Body.String() {
		t.Errorf("the two answers differ, which is enough to tell that mine exists:\n%s\n%s",
			seen.Body.String(), missing.Body.String())
	}
}

// The badge is on the result row, not only in the preview, because the whole
// value of the signal is that it is visible while somebody is choosing which of
// ten results to open.
func TestAVerifiedDocumentIsMarkedInTheResults(t *testing.T) {
	h := newVerifyServer(t)
	if w := post(t, h, http.MethodPost, "/api/v1/documents/mine/verify", "", owner()); w.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}

	w := request(t, h, http.MethodGet, "/api/v1/search?q=payments", owner())
	if w.Code != http.StatusOK {
		t.Fatalf("search: %d", w.Code)
	}
	var res struct {
		Hits []struct {
			ID       string `json:"id"`
			Verified *struct {
				State string `json:"state"`
				By    string `json:"by"`
			} `json:"verified"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("the query matched %d documents, and two of them say payments", len(res.Hits))
	}
	for _, hit := range res.Hits {
		switch {
		case hit.ID == "mine" && hit.Verified == nil:
			t.Errorf("the verified document came back with no badge on it")
		case hit.ID == "mine" && hit.Verified.By != "Mei Tanaka":
			t.Errorf("the badge names %q", hit.Verified.By)
		case hit.ID == "theirs" && hit.Verified != nil:
			t.Errorf("a document nobody verified came back with a badge on it")
		}
	}
}

// A claim is a fact about a document, so knowing that anyone made one is
// knowing the document is there.
func TestAVerificationIsNotVisibleAcrossAPermission(t *testing.T) {
	h := newVerifyServer(t)
	if w := post(t, h, http.MethodPost, "/api/v1/documents/mine/verify", "", owner()); w.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}

	w := request(t, h, http.MethodGet, "/api/v1/documents/mine", salesperson())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// The preview is where somebody decides whether to trust what they are reading,
// so it carries the claim and whether this reader could make one, in the same
// response. A second request to find out whether to draw a button is a second
// round trip before the panel can be painted.
func TestThePreviewSaysWhoVouchedAndWhetherYouCan(t *testing.T) {
	h := newVerifyServer(t)
	if w := post(t, h, http.MethodPost, "/api/v1/documents/mine/verify", "", owner()); w.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}

	var body struct {
		Verified *struct {
			State string `json:"state"`
		} `json:"verified"`
		CanVerify bool `json:"can_verify"`
	}
	w := request(t, h, http.MethodGet, "/api/v1/documents/mine", owner())
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Verified == nil || body.Verified.State != "fresh" {
		t.Errorf("the preview of a verified document says %+v", body.Verified)
	}
	if !body.CanVerify {
		t.Errorf("the owner is not offered the button on their own document")
	}

	// A fresh value, because the field is left out when it is false and
	// unmarshalling into the previous one would keep the answer to the last
	// question.
	body.CanVerify, body.Verified = false, nil
	w = request(t, h, http.MethodGet, "/api/v1/documents/theirs", owner())
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.CanVerify {
		t.Errorf("a reader who is not the owner is offered a button that would be refused")
	}
}

func TestWithdrawingAClaimTakesTheBadgeOff(t *testing.T) {
	h := newVerifyServer(t)
	if w := post(t, h, http.MethodPost, "/api/v1/documents/mine/verify", "", owner()); w.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}

	if w := post(t, h, http.MethodDelete, "/api/v1/documents/mine/verify", "", owner()); w.Code != http.StatusNoContent {
		t.Fatalf("unverify: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Verified *struct{} `json:"verified"`
	}
	w := request(t, h, http.MethodGet, "/api/v1/documents/mine", owner())
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Verified != nil {
		t.Errorf("the claim was withdrawn and the preview still has a badge on it")
	}
}

// A verifier who knows their document changes weekly says so, and everybody
// else gets the default.
func TestAVerifierCanSayHowLongTheClaimLasts(t *testing.T) {
	h := newVerifyServer(t)

	w := post(t, h, http.MethodPost, "/api/v1/documents/mine/verify", `{"days":7}`, owner())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var made struct {
		Until time.Time `json:"until"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &made); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := clock().AddDate(0, 0, 7); !made.Until.Equal(want) {
		t.Errorf("expires at %v, and seven days from the request is %v", made.Until, want)
	}
}
