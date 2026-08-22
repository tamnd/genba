package storetest

import (
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// A verification is a claim about a document made by somebody other than the
// crawler, which makes it the first thing in this interface that a driver could
// plausibly decide is safe to hand out freely. It is not. Who vouched for a
// document is a fact about the document, so knowing that anyone did is knowing
// the document exists, and a badge that renders for a reader who cannot open the
// file is the same existence oracle the not found rule exists to close.
//
// The other half of what these cases pin down is what survives what. A crawl
// rewrites a document and must leave the claim alone, because a source that
// touches every file every night would otherwise erase the entire signal by
// morning. A deletion takes the claim with it, because the alternative is a
// claim about a document that no longer exists, waiting to be reattached to
// whatever gets written under that id next.

// verifier skips a case for a driver that cannot remember a verification.
func verifier(t *testing.T, s store.Store) store.Verifier {
	t.Helper()
	v, ok := s.(store.Verifier)
	if !ok {
		t.Skip("driver does not implement store.Verifier")
	}
	return v
}

// vouch is a claim made at a fixed time, so that a test asserts on the dates it
// asked for rather than on whatever the machine managed between two calls.
func vouch(id string) store.Verification {
	return store.Verification{
		Doc:   id,
		By:    doc.Person{Subject: "u_mei", Name: "Mei Tanaka", Email: "mei@acme.com"},
		At:    at(0),
		Until: at(0).Add(store.Cadence),
		Note:  "checked against the failover we ran last week",
	}
}

func claims(t *testing.T, v store.Verifier, p *acl.Principal, ids ...string) map[string]store.Verification {
	t.Helper()
	got, err := v.Verifications(t.Context(), p, ids)
	if err != nil {
		t.Fatalf("Verifications: %v", err)
	}
	return got
}

func testVerifyRoundTrip(t *testing.T, s store.Store) {
	v := verifier(t, s)
	mustPut(t, s, document("d1", readable()))

	want := vouch("d1")
	if err := v.Verify(t.Context(), reader(), want); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	got := claims(t, v, reader(), "d1")
	back, ok := got["d1"]
	if !ok {
		t.Fatalf("the document was verified and came back with no claim on it")
	}
	if back.By != want.By {
		t.Errorf("the badge names %+v, and the claim was made by %+v", back.By, want.By)
	}
	if !back.At.Equal(want.At) || !back.Until.Equal(want.Until) {
		t.Errorf("verified at %v until %v, and %v until %v went in", back.At, back.Until, want.At, want.Until)
	}
	if back.Note != want.Note {
		t.Errorf("the note came back as %q and went in as %q", back.Note, want.Note)
	}
}

func testVerifyReplaces(t *testing.T, s store.Store) {
	v := verifier(t, s)
	mustPut(t, s, document("d1", readable()))

	first := vouch("d1")
	if err := v.Verify(t.Context(), reader(), first); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	second := vouch("d1")
	second.By = doc.Person{Subject: "u_kenji", Name: "Kenji Sato"}
	second.At = at(30)
	second.Until = at(30).Add(store.Cadence)
	if err := v.Verify(t.Context(), reader(), second); err != nil {
		t.Fatalf("re-verify: %v", err)
	}

	got := claims(t, v, reader(), "d1")
	if len(got) != 1 {
		t.Fatalf("re-verifying produced %d claims, and a document has one", len(got))
	}
	if got["d1"].By.Subject != "u_kenji" {
		t.Errorf("the badge still names %q after somebody else verified it", got["d1"].By.Subject)
	}
}

func testVerifyPermissions(t *testing.T, s store.Store) {
	v := verifier(t, s)
	mustPut(t, s, document("d1", readable()))

	// A stranger cannot make the claim, and the refusal is silence rather than
	// an error, for the same reason a missing document and a forbidden one are
	// the same answer.
	if err := v.Verify(t.Context(), stranger(), vouch("d1")); err != nil {
		t.Fatalf("verifying something the stranger cannot see should be quiet, and it said: %v", err)
	}
	if got := claims(t, v, reader(), "d1"); len(got) != 0 {
		t.Fatalf("a reader who cannot see the document verified it anyway")
	}

	// Nor can a stranger read a claim somebody else made, which is the half that
	// would otherwise be an existence oracle.
	if err := v.Verify(t.Context(), reader(), vouch("d1")); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := claims(t, v, stranger(), "d1"); len(got) != 0 {
		t.Fatalf("the stranger learned that d1 exists from a badge on it")
	}
}

func testVerifyRevocation(t *testing.T, s store.Store) {
	v := verifier(t, s)
	mustPut(t, s, document("d1", readable()))
	if err := v.Verify(t.Context(), reader(), vouch("d1")); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// The same document, no longer readable. The claim is still stored and is no
	// longer anybody's to see, which is the same rule the open history follows.
	cut := document("d1", readable())
	cut.Permissions.AllowGroups = []acl.Ref{{Source: "gdrive", Value: "legal@acme.com"}}
	mustPut(t, s, cut)

	if got := claims(t, v, reader(), "d1"); len(got) != 0 {
		t.Fatalf("a reader who was cut out of the document can still see who vouched for it")
	}
}

func testVerifySurvivesRewrite(t *testing.T, s store.Store) {
	v := verifier(t, s)
	mustPut(t, s, document("d1", readable()))
	if err := v.Verify(t.Context(), reader(), vouch("d1")); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// A crawl that touches every file every night is the normal case, and a
	// claim that does not survive one is a signal that is erased by morning.
	again := document("d1", readable())
	again.Body = "how to fail over the payments queue, second edition"
	mustPut(t, s, again)

	if got := claims(t, v, reader(), "d1"); len(got) != 1 {
		t.Fatalf("rewriting the document erased the claim on it")
	}
}

func testVerifyDelete(t *testing.T, s store.Store) {
	v := verifier(t, s)
	mustPut(t, s, document("d1", readable()))
	if err := v.Verify(t.Context(), reader(), vouch("d1")); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := s.Delete(t.Context(), "d1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The claim goes with the document. Putting the id back is a new document
	// that nobody has vouched for yet.
	mustPut(t, s, document("d1", readable()))
	if got := claims(t, v, reader(), "d1"); len(got) != 0 {
		t.Fatalf("a document that was deleted and re-crawled came back carrying a claim about the old one")
	}
}

func testUnverify(t *testing.T, s store.Store) {
	v := verifier(t, s)
	mustPut(t, s, document("d1", readable()))
	if err := v.Verify(t.Context(), reader(), vouch("d1")); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if err := v.Unverify(t.Context(), reader(), "d1"); err != nil {
		t.Fatalf("Unverify: %v", err)
	}
	if got := claims(t, v, reader(), "d1"); len(got) != 0 {
		t.Fatalf("the claim was withdrawn and is still there")
	}
	// Twice, because undoing a mistake should not itself be a mistake.
	if err := v.Unverify(t.Context(), reader(), "d1"); err != nil {
		t.Fatalf("withdrawing a claim that is not there: %v", err)
	}
}

func testVerifyRejectsIncomplete(t *testing.T, s store.Store) {
	v := verifier(t, s)
	mustPut(t, s, document("d1", readable()))

	nameless := vouch("d1")
	nameless.By = doc.Person{}
	if err := v.Verify(t.Context(), reader(), nameless); err == nil {
		t.Errorf("a claim with nobody's name on it was accepted, which is the flag this type exists to avoid")
	}

	forever := vouch("d1")
	forever.Until = time.Time{}
	if err := v.Verify(t.Context(), reader(), forever); err == nil {
		t.Errorf("a claim that never expires was accepted")
	}
}

func testVerifyBatch(t *testing.T, s store.Store) {
	v := verifier(t, s)
	mustPut(t, s, document("d1", readable()), document("d2", readable()), document("d3", readable()))
	for _, id := range []string{"d1", "d3"} {
		if err := v.Verify(t.Context(), reader(), vouch(id)); err != nil {
			t.Fatalf("Verify %s: %v", id, err)
		}
	}

	got := claims(t, v, reader(), "d1", "d2", "d3")
	if len(got) != 2 {
		t.Fatalf("asked about three documents and got %d claims, and two of them were verified", len(got))
	}
	// A document with no claim is absent rather than present and empty, so that
	// a caller drawing badges can range over what came back.
	if _, ok := got["d2"]; ok {
		t.Errorf("a document nobody verified came back with a claim on it")
	}
}
