package index_test

import (
	"testing"

	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
)

// The corpus a correction is drawn from. The words that matter are the ones in
// the bodies: what is offered has to come from the index rather than from a
// dictionary, and northwind is not a word a dictionary of English has.
var misspelt = []fixture{
	{id: "d1", title: "Payments failover runbook", body: "Invalidate the payments cache before the failover.", perm: openTo("eng@acme.com")},
	{id: "d2", title: "Weekly engineering notes", body: "The cache was cold for an hour.", perm: openTo("eng@acme.com")},
	{id: "d3", title: "Board deck", body: "Acquisition of northwind, unannounced.", perm: openTo("board@acme.com")},
}

// plainStore is a driver with none of its optional capabilities showing. Only
// the methods of [store.Store] are promoted through the embedded interface, so
// what the searcher sees is a driver that cannot name a word.
type plainStore struct{ store.Store }

func TestSearchOffersASpellingThatWouldHaveWorked(t *testing.T) {
	s := newSearcher(t, misspelt)
	p := principal("gdrive:eng@acme.com")

	res := search(t, s, p, index.Query{Text: "cahce"})
	if len(res.Hits) != 0 {
		t.Fatalf("cahce matched %d documents, so there is nothing to correct", len(res.Hits))
	}
	if res.Correction != "cache" {
		t.Fatalf("Correction = %q, want cache", res.Correction)
	}

	// And the correction is a query rather than a word, so what the interface
	// links to is what was confirmed.
	if got := search(t, s, p, index.Query{Text: res.Correction}); len(got.Hits) == 0 {
		t.Fatal("the correction that was offered returns nothing when it is run")
	}
}

func TestSearchCorrectsTheWordAndLeavesTheRestOfTheQueryAlone(t *testing.T) {
	s := newSearcher(t, misspelt)

	// Everything that was not the misspelled word comes back exactly as it was
	// typed, capitals and punctuation included, because what is offered is the
	// query somebody meant to type rather than a rewrite of it.
	res := search(t, s, principal("gdrive:eng@acme.com"), index.Query{Text: "The cahce."})
	if res.Correction != "The cache." {
		t.Fatalf("Correction = %q, want The cache.", res.Correction)
	}
}

func TestSearchOffersNothingWhenTheQueryWorked(t *testing.T) {
	s := newSearcher(t, misspelt)

	res := search(t, s, principal("gdrive:eng@acme.com"), index.Query{Text: "cache"})
	if len(res.Hits) == 0 {
		t.Fatal("cache matched nothing, so this proves nothing")
	}
	if res.Correction != "" {
		t.Fatalf("Correction = %q on a search that found results", res.Correction)
	}
}

// The one that is not about spelling.
//
// A vocabulary is a fact about the tenant and results are not, so a correction
// taken out of the index and shown would tell somebody that a word appears in a
// document they may not read. Northwind appears once, in the board deck, and
// nobody outside the board gets to learn that by mistyping it.
func TestSearchNeverCorrectsTowardsADocumentTheAskerCannotRead(t *testing.T) {
	s := newSearcher(t, misspelt)

	res := search(t, s, principal("gdrive:eng@acme.com"), index.Query{Text: "northwnd"})
	if res.Correction != "" {
		t.Fatalf("Correction = %q, drawn from a document this reader may not open", res.Correction)
	}

	// The same query from somebody who may read it is corrected, which is what
	// makes the case above a permission decision rather than a driver that
	// found nothing.
	res = search(t, s, principal("gdrive:board@acme.com"), index.Query{Text: "northwnd"})
	if res.Correction != "northwind" {
		t.Fatalf("Correction = %q for a reader of the board deck, want northwind", res.Correction)
	}
}

func TestSearchCorrectsUnderTheFiltersThatAreOn(t *testing.T) {
	s := newSearcher(t, misspelt)

	// A correction that leads to a page with nothing on it is a second dead
	// end, so the confirmation carries the filters that are on. The cache is
	// only in gdrive pages, and this asks for it in salesforce.
	res := search(t, s, principal("gdrive:eng@acme.com"), index.Query{Text: "cahce", Sources: []string{"salesforce"}})
	if res.Correction != "" {
		t.Fatalf("Correction = %q, which finds nothing under the filters that are on", res.Correction)
	}
}

func TestSearchOffersNothingForALongQuery(t *testing.T) {
	s := newSearcher(t, misspelt)

	// Six words that found nothing is a sentence somebody pasted rather than a
	// typo, and there is no spelling of it that would have worked.
	res := search(t, s, principal("gdrive:eng@acme.com"), index.Query{Text: "invalidate payments cahce before failover please"})
	if res.Correction != "" {
		t.Fatalf("Correction = %q for a query of six words", res.Correction)
	}
}

func TestSearchOffersNothingWhenTheStoreCannotSpell(t *testing.T) {
	// A driver without the capability is not an error, it is a search with no
	// correction on it. This is the reference driver with the capability hidden,
	// which is what a driver that never had one looks like from here.
	mem := memstore.New()
	t.Cleanup(func() { _ = mem.Close() })
	if err := mem.Put(t.Context(), documents(misspelt)...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s := index.New(plainStore{Store: mem}, index.WithClock(clock))

	res := search(t, s, principal("gdrive:eng@acme.com"), index.Query{Text: "cahce"})
	if res.Correction != "" {
		t.Fatalf("Correction = %q from a driver that cannot name a word", res.Correction)
	}
}
