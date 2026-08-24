package storetest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// An answer is the only object in this interface that is not about a document,
// and that is what these cases are about. Everything else here reaches its
// tenant through a document and inherits a permission rule from it. An answer
// has neither, so a driver has to apply the tenant itself, and the way to get
// that wrong is to key an answer by its id alone and hand one deployment's
// handbook to another.
//
// The other half is what a lookup finds. An answer is found by the phrasings
// somebody wrote down, folded, and a driver that writes a different set of keys
// from the set it later probes gives an answer that can be written and never
// read. So these cases write one, edit it, take a phrasing away, and ask for it
// by each of the ways it was said.

// curator skips a case for a driver that cannot remember an answer.
func curator(t *testing.T, s store.Store) store.Curator {
	t.Helper()
	c, ok := s.(store.Curator)
	if !ok {
		t.Skip("driver does not implement store.Curator")
	}
	return c
}

// written is an answer made at a fixed time, so that a test asserts on the
// dates it asked for rather than on whatever the machine managed between two
// calls.
func written(id string) store.Answer {
	return store.Answer{
		ID:       id,
		Question: "What is the deploy freeze?",
		Variants: []string{"when is the code freeze"},
		Body:     "No production deploys from the 20th of December to the 2nd of January, except for incidents.",
		Sources:  []string{"d1"},
		By:       doc.Person{Subject: "u_mei", Name: "Mei Tanaka", Email: "mei@acme.com"},
		At:       at(0),
		Until:    at(0).Add(store.Cadence),
	}
}

func curate(t *testing.T, c store.Curator, p *acl.Principal, a store.Answer) {
	t.Helper()
	if err := c.Curate(t.Context(), p, a); err != nil {
		t.Fatalf("Curate: %v", err)
	}
}

func curated(t *testing.T, c store.Curator, p *acl.Principal, question string) (store.Answer, error) {
	t.Helper()
	return c.Curated(t.Context(), p, question)
}

func answers(t *testing.T, c store.Curator, p *acl.Principal, limit int) []string {
	t.Helper()
	got, err := c.Answers(t.Context(), p, limit)
	if err != nil {
		t.Fatalf("Answers: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, a := range got {
		ids = append(ids, a.ID)
	}
	return ids
}

func testCurateRoundTrip(t *testing.T, s store.Store) {
	c := curator(t, s)
	in := written("a1")
	curate(t, c, reader(), in)

	got, err := curated(t, c, reader(), in.Question)
	if err != nil {
		t.Fatalf("Curated: %v", err)
	}
	switch {
	case got.ID != in.ID:
		t.Fatalf("the answer came back as %q", got.ID)
	case got.Question != in.Question:
		t.Fatalf("the question came back as %q, and it is what the card is titled with", got.Question)
	case got.Body != in.Body:
		t.Fatalf("the body came back as %q", got.Body)
	case !slices.Equal(got.Variants, in.Variants):
		t.Fatalf("the variants came back as %v, so a phrasing was lost on the way in", got.Variants)
	case !slices.Equal(got.Sources, in.Sources):
		t.Fatalf("the sources came back as %v, so the card would cite nothing", got.Sources)
	case got.By.Name != in.By.Name || got.By.Email != in.By.Email:
		t.Fatalf("the author came back as %+v, and the name is why a reader believes the answer", got.By)
	case !got.At.Equal(in.At):
		t.Fatalf("it was written at %v, want %v", got.At, in.At)
	case !got.Until.Equal(in.Until):
		t.Fatalf("it runs out at %v, want %v", got.Until, in.Until)
	}
}

// A question is matched by what it means rather than by how it was typed, or
// the answer is only ever found by the person who wrote it.
func testCuratedIgnoresPunctuationAndCase(t *testing.T, s store.Store) {
	c := curator(t, s)
	curate(t, c, reader(), written("a1"))

	for _, asked := range []string{
		"What is the deploy freeze?",
		"what is the deploy freeze",
		"  WHAT IS THE DEPLOY FREEZE!!  ",
		"when is the code freeze",
	} {
		if _, err := curated(t, c, reader(), asked); err != nil {
			t.Fatalf("asking %q found nothing: %v", asked, err)
		}
	}
}

// A near miss gets the ordinary list of results rather than a confident card
// above them, which is the whole reason the match is the whole key.
func testCuratedIsNotFuzzy(t *testing.T, s store.Store) {
	c := curator(t, s)
	curate(t, c, reader(), written("a1"))

	for _, asked := range []string{"deploy freeze", "what is the deploy freeze in december", ""} {
		if _, err := curated(t, c, reader(), asked); !errors.Is(err, genba.ErrNotFound) {
			t.Fatalf("asking %q returned %v, want ErrNotFound", asked, err)
		}
	}
}

// Editing an answer is the same call as writing one, and it is also how an
// answer is re-verified.
func testCurateReplaces(t *testing.T, s store.Store) {
	c := curator(t, s)
	curate(t, c, reader(), written("a1"))

	again := written("a1")
	again.Body = "No production deploys for the last two weeks of December."
	again.At = at(5)
	again.Until = at(5).Add(store.Cadence)
	curate(t, c, reader(), again)

	got, err := curated(t, c, reader(), again.Question)
	if err != nil {
		t.Fatalf("Curated: %v", err)
	}
	if got.Body != again.Body {
		t.Fatalf("the body is still %q, so the edit did not land", got.Body)
	}
	if !got.Until.Equal(again.Until) {
		t.Fatalf("it runs out at %v, want %v, because writing an answer is standing behind it", got.Until, again.Until)
	}
	if ids := answers(t, c, reader(), 10); len(ids) != 1 {
		t.Fatalf("the tenant holds %v, and an edit is not a second answer", ids)
	}
}

// A variant taken out of an answer stops finding it, or an answer could never
// lose a question it turned out not to answer.
func testCurateDropsAPhrasing(t *testing.T, s store.Store) {
	c := curator(t, s)
	curate(t, c, reader(), written("a1"))
	if _, err := curated(t, c, reader(), "when is the code freeze"); err != nil {
		t.Fatalf("the variant did not find the answer to begin with: %v", err)
	}

	fewer := written("a1")
	fewer.Variants = nil
	curate(t, c, reader(), fewer)

	if _, err := curated(t, c, reader(), "when is the code freeze"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("the dropped variant returned %v, want ErrNotFound", err)
	}
	if _, err := curated(t, c, reader(), fewer.Question); err != nil {
		t.Fatalf("the question it still claims found nothing: %v", err)
	}
}

// Two answers claiming the same question is a curation conflict, and the
// resolution that needs no screen is that the most recent writer wins.
func testCurateMovesAPhrasing(t *testing.T, s store.Store) {
	c := curator(t, s)
	curate(t, c, reader(), written("a1"))

	second := written("a2")
	second.Body = "The freeze now runs to the 6th of January."
	second.At = at(5)
	curate(t, c, reader(), second)

	got, err := curated(t, c, reader(), second.Question)
	if err != nil {
		t.Fatalf("Curated: %v", err)
	}
	if got.ID != "a2" {
		t.Fatalf("the question still answers to %q, so the later writer did not win", got.ID)
	}
	if ids := answers(t, c, reader(), 10); len(ids) != 2 {
		t.Fatalf("the tenant holds %v, and taking a question away is not deleting an answer", ids)
	}
}

func testRetract(t *testing.T, s store.Store) {
	c := curator(t, s)
	curate(t, c, reader(), written("a1"))

	if err := c.Retract(t.Context(), reader(), "a1"); err != nil {
		t.Fatalf("Retract: %v", err)
	}
	if _, err := curated(t, c, reader(), written("a1").Question); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("the retracted answer returned %v, want ErrNotFound", err)
	}
	// Twice, so that a mistake can be undone twice.
	if err := c.Retract(t.Context(), reader(), "a1"); err != nil {
		t.Fatalf("retracting again: %v", err)
	}
	if ids := answers(t, c, reader(), 10); len(ids) != 0 {
		t.Fatalf("the tenant still holds %v", ids)
	}
}

// An answer belongs to a tenant and to nothing else, which is the one rule a
// driver has to apply here by itself.
func testCurateTenants(t *testing.T, s store.Store) {
	c := curator(t, s)
	globex := &acl.Principal{Tenant: "globex", Subject: "u_pat"}
	curate(t, c, reader(), written("a1"))

	if _, err := curated(t, c, globex, written("a1").Question); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("another tenant asking the same question got %v, want ErrNotFound", err)
	}
	if ids := answers(t, c, globex, 10); len(ids) != 0 {
		t.Fatalf("another tenant listed %v", ids)
	}

	// The same id in two tenants is two answers, and neither of them is the
	// other, which is what a driver keying on the id alone gets wrong.
	theirs := written("a1")
	theirs.Question = "What is the deploy freeze?"
	theirs.Body = "Globex does not freeze deploys."
	curate(t, c, globex, theirs)

	ours, err := curated(t, c, reader(), theirs.Question)
	if err != nil {
		t.Fatalf("Curated: %v", err)
	}
	if ours.Body == theirs.Body {
		t.Fatal("one tenant's answer replaced another's, which is the whole of the isolation rule here")
	}
}

// The list is what whoever maintains the answers reads, so it is most recently
// written first and it stops where it was told to.
func testAnswersOrder(t *testing.T, s store.Store) {
	c := curator(t, s)
	for i, id := range []string{"a1", "a2", "a3"} {
		a := written(id)
		a.Question = "question " + id
		a.Variants = nil
		a.At = at(i)
		curate(t, c, reader(), a)
	}

	if ids := answers(t, c, reader(), 10); !slices.Equal(ids, []string{"a3", "a2", "a1"}) {
		t.Fatalf("the list came back as %v, want the most recently written first", ids)
	}
	if ids := answers(t, c, reader(), 2); !slices.Equal(ids, []string{"a3", "a2"}) {
		t.Fatalf("a limit of two gave %v", ids)
	}
	if ids := answers(t, c, reader(), 0); len(ids) != 0 {
		t.Fatalf("a limit of zero gave %v", ids)
	}
}

// The three fields a card is unreadable without, refused above the drivers so
// that two of them cannot disagree about what an answer is.
func testCurateRejectsIncomplete(t *testing.T, s store.Store) {
	c := curator(t, s)
	for name, a := range map[string]func(store.Answer) store.Answer{
		"no id":       func(a store.Answer) store.Answer { a.ID = ""; return a },
		"no question": func(a store.Answer) store.Answer { a.Question = "  ?  "; return a },
		"no body":     func(a store.Answer) store.Answer { a.Body = "   "; return a },
		"no author":   func(a store.Answer) store.Answer { a.By = doc.Person{}; return a },
		"no expiry":   func(a store.Answer) store.Answer { a.Until = time.Time{}; return a },
	} {
		if err := c.Curate(t.Context(), reader(), a(written("a1"))); err == nil {
			t.Fatalf("an answer with %s was accepted", name)
		}
	}
}

// A nil principal has no tenant, so it has no answers, and it says so rather
// than reading somebody else's.
func testCurateNilPrincipal(t *testing.T, s store.Store) {
	c := curator(t, s)
	curate(t, c, reader(), written("a1"))

	if err := c.Curate(t.Context(), nil, written("a2")); !errors.Is(err, genba.ErrNoPrincipal) {
		t.Fatalf("Curate with no principal returned %v, want ErrNoPrincipal", err)
	}
	if _, err := c.Curated(t.Context(), nil, "What is the deploy freeze?"); !errors.Is(err, genba.ErrNoPrincipal) {
		t.Fatalf("Curated with no principal returned %v, want ErrNoPrincipal", err)
	}
	if _, err := c.Answers(t.Context(), nil, 10); !errors.Is(err, genba.ErrNoPrincipal) {
		t.Fatalf("Answers with no principal returned %v, want ErrNoPrincipal", err)
	}
	if err := c.Retract(t.Context(), nil, "a1"); !errors.Is(err, genba.ErrNoPrincipal) {
		t.Fatalf("Retract with no principal returned %v, want ErrNoPrincipal", err)
	}
}
