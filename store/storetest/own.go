package storetest

import (
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// A correction is the one write in this interface that changes the document
// itself rather than something stored beside it, and that is what these cases
// are about. The owner a reader sees, the owner an owner: filter matches and the
// owner the next crawl reports all have to be the same person, and a driver that
// keeps the correction in its own table and forgets to apply it to the document
// passes a naive round trip and fails every one of those.
//
// The other half is what survives what, and it is the opposite of the rule for a
// verification. A crawl rewrites a document and the correction is applied again
// rather than left alone, because the source will keep reporting the answer
// somebody has already said is wrong, and a correction that lasts until the next
// nightly crawl is not a correction. A deletion takes it with it, for the same
// reason a deletion takes the badge: whatever gets written under that id next is
// a different document.

// ownership skips a case for a driver that cannot remember a correction.
func ownership(t *testing.T, s store.Store) store.Ownership {
	t.Helper()
	o, ok := s.(store.Ownership)
	if !ok {
		t.Skip("driver does not implement store.Ownership")
	}
	return o
}

// derived is who the connector says owns these documents, which is the account
// that ran the import rather than a person. It is the case the whole feature is
// for.
var derived = doc.Person{Name: "Drive Sync", Email: "drive-sync@acme.com"}

// owned is a document with a derived owner on it.
func owned(id string) doc.Document {
	d := document(id, readable())
	d.Owner = derived
	return d
}

// reassign is a correction made at a fixed time, so that a test asserts on the
// date it asked for rather than on whatever the machine managed between two
// calls.
func reassign(id string, owner doc.Person) store.Correction {
	return store.Correction{
		Doc:   id,
		Owner: owner,
		By:    doc.Person{Subject: "u_mei", Name: "Mei Tanaka", Email: "mei@acme.com"},
		At:    at(0),
	}
}

// ada is who the documents keep being handed to.
var ada = doc.Person{Subject: "u_ada", Name: "Ada Okafor", Email: "ada@acme.com"}

func corrections(t *testing.T, o store.Ownership, p *acl.Principal, ids ...string) map[string]store.Correction {
	t.Helper()
	got, err := o.Corrections(t.Context(), p, ids)
	if err != nil {
		t.Fatalf("Corrections: %v", err)
	}
	return got
}

// ownerOf reads back who the store says owns a document.
func ownerOf(t *testing.T, s store.Store, id string) doc.Person {
	t.Helper()
	d, err := s.Get(t.Context(), reader(), id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return d.Owner
}

func testOwnerRoundTrip(t *testing.T, s store.Store) {
	o := ownership(t, s)
	mustPut(t, s, owned("d1"))

	if err := o.SetOwner(t.Context(), reader(), reassign("d1", ada)); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}

	// The document itself, because that is the contract. A correction that only
	// a second call can see is an overlay, and an overlay is invisible to every
	// query path.
	if got := ownerOf(t, s, "d1"); got.Email != ada.Email {
		t.Errorf("the document is owned by %+v, and it was handed to %+v", got, ada)
	}

	c, ok := corrections(t, o, reader(), "d1")["d1"]
	if !ok {
		t.Fatalf("the owner was corrected and the document came back with no correction on it")
	}
	if c.Owner != ada {
		t.Errorf("the correction names %+v as the owner, and %+v went in", c.Owner, ada)
	}
	if c.Was != derived {
		t.Errorf("the source said %+v and the correction remembers %+v", derived, c.Was)
	}
	if c.By.Subject != "u_mei" || !c.At.Equal(at(0)) {
		t.Errorf("the correction was made by %+v at %v", c.By, c.At)
	}
}

func testOwnerReplaces(t *testing.T, s store.Store) {
	o := ownership(t, s)
	mustPut(t, s, owned("d1"))

	if err := o.SetOwner(t.Context(), reader(), reassign("d1", ada)); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	kenji := doc.Person{Subject: "u_kenji", Name: "Kenji Ito", Email: "kenji@acme.com"}
	if err := o.SetOwner(t.Context(), reader(), reassign("d1", kenji)); err != nil {
		t.Fatalf("correcting the correction: %v", err)
	}

	got := corrections(t, o, reader(), "d1")
	if len(got) != 1 {
		t.Fatalf("correcting twice produced %d corrections, and a document has one", len(got))
	}
	if got["d1"].Owner.Subject != "u_kenji" {
		t.Errorf("the document still belongs to %q after somebody handed it on", got["d1"].Owner.Subject)
	}
	// The connector's answer, not the previous person's. Clearing has to put back
	// what the source said rather than an earlier guess.
	if got["d1"].Was != derived {
		t.Errorf("the second correction remembers %+v as the source's answer, and the source said %+v", got["d1"].Was, derived)
	}
}

func testOwnerPermissions(t *testing.T, s store.Store) {
	o := ownership(t, s)
	mustPut(t, s, owned("d1"))

	// A stranger cannot make the correction, and the refusal is silence rather
	// than an error, for the same reason a missing document and a forbidden one
	// are the same answer.
	if err := o.SetOwner(t.Context(), stranger(), reassign("d1", ada)); err != nil {
		t.Fatalf("correcting something the stranger cannot see should be quiet, and it said: %v", err)
	}
	if got := corrections(t, o, reader(), "d1"); len(got) != 0 {
		t.Fatalf("a reader who cannot see the document reassigned it anyway")
	}
	if got := ownerOf(t, s, "d1"); got != derived {
		t.Fatalf("the document is owned by %+v after a stranger tried to hand it over", got)
	}

	// Nor can a stranger read a correction somebody else made, which is the half
	// that would otherwise be an existence oracle.
	if err := o.SetOwner(t.Context(), reader(), reassign("d1", ada)); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	if got := corrections(t, o, stranger(), "d1"); len(got) != 0 {
		t.Fatalf("the stranger learned that d1 exists from a correction on it")
	}
}

func testOwnerSurvivesRewrite(t *testing.T, s store.Store) {
	o := ownership(t, s)
	mustPut(t, s, owned("d1"))
	if err := o.SetOwner(t.Context(), reader(), reassign("d1", ada)); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}

	// The crawl comes round again and reports the account that ran the import,
	// exactly as it did last night and will again tomorrow.
	again := owned("d1")
	again.Body = "how to fail over the payments queue, second edition"
	mustPut(t, s, again)

	if got := ownerOf(t, s, "d1"); got.Email != ada.Email {
		t.Fatalf("a crawl handed the document back to %+v", got)
	}
	if got := corrections(t, o, reader(), "d1"); len(got) != 1 {
		t.Fatalf("rewriting the document erased the correction on it")
	}

	// The source's answer is refreshed by the crawl, so a source that fixes its
	// own metadata is what clearing the correction puts back.
	fixed := owned("d1")
	fixed.Owner = doc.Person{Subject: "u_kenji", Name: "Kenji Ito", Email: "kenji@acme.com"}
	mustPut(t, s, fixed)
	if got := corrections(t, o, reader(), "d1")["d1"].Was.Subject; got != "u_kenji" {
		t.Errorf("the correction remembers %q as the source's answer, and the source now says u_kenji", got)
	}
}

func testClearOwner(t *testing.T, s store.Store) {
	o := ownership(t, s)
	mustPut(t, s, owned("d1"))
	if err := o.SetOwner(t.Context(), reader(), reassign("d1", ada)); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}

	if err := o.ClearOwner(t.Context(), reader(), "d1"); err != nil {
		t.Fatalf("ClearOwner: %v", err)
	}
	if got := corrections(t, o, reader(), "d1"); len(got) != 0 {
		t.Fatalf("the correction was cleared and is still there")
	}
	if got := ownerOf(t, s, "d1"); got != derived {
		t.Fatalf("clearing the correction left the document owned by %+v, and the source says %+v", got, derived)
	}
	// Twice, because undoing a mistake should not itself be a mistake.
	if err := o.ClearOwner(t.Context(), reader(), "d1"); err != nil {
		t.Fatalf("clearing a correction that is not there: %v", err)
	}
}

func testOwnerDelete(t *testing.T, s store.Store) {
	o := ownership(t, s)
	mustPut(t, s, owned("d1"))
	if err := o.SetOwner(t.Context(), reader(), reassign("d1", ada)); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	if err := s.Delete(t.Context(), "d1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The correction goes with the document. Putting the id back is a new
	// document that nobody has corrected yet.
	mustPut(t, s, owned("d1"))
	if got := corrections(t, o, reader(), "d1"); len(got) != 0 {
		t.Fatalf("a document that was deleted and re-crawled came back carrying a correction about the old one")
	}
	if got := ownerOf(t, s, "d1"); got != derived {
		t.Fatalf("a document that was deleted and re-crawled came back owned by %+v", got)
	}
}

func testOwnerRejectsIncomplete(t *testing.T, s store.Store) {
	o := ownership(t, s)
	mustPut(t, s, owned("d1"))

	nobody := reassign("d1", doc.Person{})
	if err := o.SetOwner(t.Context(), reader(), nobody); err == nil {
		t.Errorf("a document was handed to nobody, which is a deletion written the wrong way")
	}

	anonymous := reassign("d1", ada)
	anonymous.By = doc.Person{}
	if err := o.SetOwner(t.Context(), reader(), anonymous); err == nil {
		t.Errorf("a correction with nobody making it was accepted")
	}
}

func testOwnerBatch(t *testing.T, s store.Store) {
	o := ownership(t, s)
	mustPut(t, s, owned("d1"), owned("d2"), owned("d3"))
	for _, id := range []string{"d1", "d3"} {
		if err := o.SetOwner(t.Context(), reader(), reassign(id, ada)); err != nil {
			t.Fatalf("SetOwner %s: %v", id, err)
		}
	}

	got := corrections(t, o, reader(), "d1", "d2", "d3")
	if len(got) != 2 {
		t.Fatalf("asked about three documents and got %d corrections, and two of them were corrected", len(got))
	}
	// A document nobody corrected is absent rather than present and empty, so
	// that a caller can range over what came back.
	if _, ok := got["d2"]; ok {
		t.Errorf("a document nobody corrected came back with a correction on it")
	}
}
