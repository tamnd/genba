package storetest

import (
	"errors"
	"slices"
	"testing"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// RunSpeller checks [store.Speller], the capability behind a correction.
//
// It is optional, so it skips for a driver that does not have it. What it will
// not let a driver do is answer with a word from another tenant, or with a word
// nothing carries any more, because a correction is offered to somebody who is
// stuck and a correction that leads nowhere is worse than none.
//
// What is deliberately not checked here is recall. A driver is allowed to miss
// a word it could have found: the two that exist read a window of an index
// rather than all of it, on purpose, and a suite that pinned the exact set
// would be pinning the size of that window. See [store.Speller].
func RunSpeller(t *testing.T, newStore Factory) {
	t.Helper()
	for _, c := range spellCases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })
			c.run(t, s)
		})
	}
}

type spellCase struct {
	name string
	run  func(t *testing.T, s store.Store)
}

var spellCases = []spellCase{
	{"a word one edit away is offered", testNearOneEdit},
	{"the word itself is never offered back", testNearNotItself},
	{"a word of another tenant is never offered", testNearTenant},
	{"a quarantined document lends no words", testNearQuarantine},
	{"a deleted document takes its words with it", testNearDelete},
	{"the limit is a limit", testNearLimit},
	{"a nil principal is corrected nothing", testNearNilPrincipal},
}

// speller skips a case for a driver that cannot name the words it holds.
func speller(t *testing.T, s store.Store) store.Speller {
	t.Helper()
	sp, ok := s.(store.Speller)
	if !ok {
		t.Skip("driver does not implement store.Speller")
	}
	return sp
}

// worded is a document whose body is the given words, so that a case can say
// what vocabulary the corpus has in one line.
func worded(id, words string) doc.Document {
	d := document(id, readable())
	d.Body = words
	return d
}

func near(t *testing.T, sp store.Speller, p *acl.Principal, term string, limit int) []string {
	t.Helper()
	out, err := sp.Near(t.Context(), p, term, limit)
	if err != nil {
		t.Fatalf("Near(%q): %v", term, err)
	}
	return out
}

func testNearOneEdit(t *testing.T, s store.Store) {
	sp := speller(t, s)
	mustPut(t, s, worded("d1", "the cache invalidation runbook"))

	got := near(t, sp, reader(), "cahce", 5)
	if !slices.Contains(got, "cache") {
		t.Errorf("Near(%q) = %v, which does not offer cache", "cahce", got)
	}
}

func testNearNotItself(t *testing.T, s store.Store) {
	sp := speller(t, s)
	mustPut(t, s, worded("d1", "cache caches cached"))

	// A word the corpus has is not a word to correct, and a driver that hands
	// it back makes the caller show "did you mean cache" under a search for
	// cache.
	if got := near(t, sp, reader(), "cache", 5); slices.Contains(got, "cache") {
		t.Errorf("Near(%q) = %v, which offers the word itself", "cache", got)
	}
}

func testNearTenant(t *testing.T, s store.Store) {
	sp := speller(t, s)
	other := worded("d1", "cache")
	other.Tenant = "globex"
	mustPut(t, s, other, worded("d2", "runbook"))

	// The whole reason [store.Speller] documents what the caller owes: this is
	// the one filter a driver does apply, and a driver that skips it turns a
	// correction into a question about somebody else's corpus.
	if got := near(t, sp, reader(), "cahce", 5); len(got) != 0 {
		t.Errorf("Near(%q) = %v, which are words of another tenant", "cahce", got)
	}
}

func testNearQuarantine(t *testing.T, s store.Store) {
	sp := speller(t, s)
	held := worded("d1", "cache")
	held.Permissions = acl.Permissions{
		Mode:        acl.ModeUnknown,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
	}
	mustPut(t, s, held)

	if got := near(t, sp, reader(), "cahce", 5); len(got) != 0 {
		t.Errorf("Near(%q) = %v, from a document nobody may read", "cahce", got)
	}
}

func testNearDelete(t *testing.T, s store.Store) {
	sp := speller(t, s)
	mustPut(t, s, worded("d1", "cache"))
	if err := s.Delete(t.Context(), "d1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if got := near(t, sp, reader(), "cahce", 5); len(got) != 0 {
		t.Errorf("Near(%q) = %v, from a document that is gone", "cahce", got)
	}
}

func testNearLimit(t *testing.T, s store.Store) {
	sp := speller(t, s)
	mustPut(t, s, worded("d1", "cache caches cached cacher caged"))

	if got := near(t, sp, reader(), "cachs", 2); len(got) > 2 {
		t.Errorf("Near(%q) with a limit of 2 returned %d: %v", "cachs", len(got), got)
	}
}

func testNearNilPrincipal(t *testing.T, s store.Store) {
	sp := speller(t, s)
	mustPut(t, s, worded("d1", "cache"))

	if _, err := sp.Near(t.Context(), nil, "cahce", 5); !errors.Is(err, genba.ErrNoPrincipal) {
		t.Errorf("Near with a nil principal returned %v, want %v", err, genba.ErrNoPrincipal)
	}
}
