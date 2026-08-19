package storetest

import (
	"slices"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// RunRetriever checks a driver's [store.Retriever] against the definition of
// the match set in [store.Request.Matches].
//
// The suite is separate from [Run] because the capability is optional, and it
// is worth having because the failure it catches is invisible in normal use. A
// driver whose index disagrees slightly with the Go rule still returns
// plausible results, and what changes is that one deployment finds a document
// another does not, on a query nobody thought to test.
//
// Every case builds a corpus, runs a request through Retrieve, runs the same
// request through Scan plus the Go rule, and requires the two to agree. That
// means these cases keep working as the rule grows: a new filter is tested
// here the moment it is implemented in both places.
func RunRetriever(t *testing.T, newStore Factory) {
	t.Helper()
	for _, c := range retrieveCases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })

			rt, ok := s.(store.Retriever)
			if !ok {
				t.Skip("this driver does not implement store.Retriever")
			}
			c.run(t, s, rt)
		})
	}
}

type retrieveCase struct {
	name string
	run  func(t *testing.T, s store.Store, rt store.Retriever)
}

var retrieveCases = []retrieveCase{
	{"an empty request retrieves everything readable", testRetrieveAll},
	{"terms select the same documents the go rule does", testRetrieveTerms},
	{"filters select the same documents the go rule does", testRetrieveFilters},
	{"the principal is applied inside retrieve", testRetrievePermission},
	{"unresolved permissions are never retrieved", testRetrieveQuarantine},
	{"another tenant is never retrieved", testRetrieveTenant},
	{"a nil principal retrieves nothing", testRetrieveNilPrincipal},
	{"retrieve stops when the callback says so", testRetrieveEarlyStop},
	{"retrieve reflects a delete", testRetrieveDelete},
	{"retrieve reflects a revocation", testRetrieveRevocation},
}

// corpus is the fixture the retrieval cases share. The documents differ in
// every field a request can narrow on, so one comparison against the Go rule
// exercises all of them at once.
func corpus() []doc.Document {
	day := func(n int) time.Time {
		return time.Date(2026, 1, n, 12, 0, 0, 0, time.UTC)
	}
	docs := []doc.Document{
		{
			ID: "r1", Source: "gdrive", Kind: doc.KindPage,
			Title:     "Payments failover runbook",
			Body:      "how to fail over the payments queue when the primary region is down",
			Author:    doc.Person{Subject: "u_mei", Name: "Mei Tanaka", Email: "mei@acme.com"},
			Container: "Platform", ModifiedAt: day(10),
		},
		{
			ID: "r2", Source: "slack", Kind: doc.KindMessage,
			Title:     "payments incident",
			Body:      "the queue backed up again this morning",
			Author:    doc.Person{Subject: "u_kenji", Name: "Kenji Ito", Email: "kenji@acme.com"},
			Container: "incidents", ModifiedAt: day(20),
		},
		{
			ID: "r3", Source: "gdrive", Kind: doc.KindFile,
			Title:     "Onboarding checklist",
			Body:      "laptop, badge, and the reading list for new engineers",
			Author:    doc.Person{Subject: "u_mei", Name: "Mei Tanaka", Email: "mei@acme.com"},
			Container: "People", ModifiedAt: day(2),
		},
		{
			ID: "r4", Source: "github", Kind: doc.KindCode,
			Title:     "queue.go",
			Body:      "package queue implements the payments queue",
			Author:    doc.Person{Subject: "u_ada", Name: "Ada Okafor", Email: "ada@acme.com"},
			Container: "acme/platform", ModifiedAt: day(25),
		},
	}
	for i := range docs {
		docs[i].Tenant = "acme"
		docs[i].Permissions = readable()
		docs[i].Owner = docs[i].Author
	}
	return docs
}

// retrieveIDs runs a request through the driver's index.
func retrieveIDs(t *testing.T, rt store.Retriever, p *acl.Principal, r store.Request) []string {
	t.Helper()
	var ids []string
	if err := rt.Retrieve(t.Context(), p, r, func(d doc.Document) bool {
		ids = append(ids, d.ID)
		return true
	}); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	slices.Sort(ids)
	return ids
}

// wantIDs runs the same request the slow, obviously correct way.
func wantIDs(t *testing.T, s store.Store, p *acl.Principal, r store.Request) []string {
	t.Helper()
	var ids []string
	if err := s.Scan(t.Context(), p, func(d doc.Document) bool {
		if r.Matches(d) {
			ids = append(ids, d.ID)
		}
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	slices.Sort(ids)
	return ids
}

// agree is the assertion every case is built out of.
func agree(t *testing.T, s store.Store, rt store.Retriever, p *acl.Principal, r store.Request) []string {
	t.Helper()
	got, want := retrieveIDs(t, rt, p, r), wantIDs(t, s, p, r)
	if !slices.Equal(got, want) {
		t.Fatalf("Retrieve and Scan disagree for %+v:\n retrieved: %v\n  expected: %v", r, got, want)
	}
	return got
}

func testRetrieveAll(t *testing.T, s store.Store, rt store.Retriever) {
	mustPut(t, s, corpus()...)
	if got := agree(t, s, rt, reader(), store.Request{}); len(got) != 4 {
		t.Fatalf("an empty request retrieved %v, want all four documents", got)
	}
}

func testRetrieveTerms(t *testing.T, s store.Store, rt store.Retriever) {
	mustPut(t, s, corpus()...)
	for _, terms := range [][]string{
		{"payments"},
		{"queue"},
		{"onboarding"},
		{"payments", "onboarding"}, // any term matches, not all of them
		{"nothingmatchesthis"},
		{"runbook"}, // title only
	} {
		agree(t, s, rt, reader(), store.Request{Terms: terms})
	}
}

func testRetrieveFilters(t *testing.T, s store.Store, rt store.Retriever) {
	mustPut(t, s, corpus()...)
	day := func(n int) time.Time { return time.Date(2026, 1, n, 0, 0, 0, 0, time.UTC) }
	for _, r := range []store.Request{
		{Sources: []string{"gdrive"}},
		{Sources: []string{"gdrive", "slack"}},
		{Kinds: []doc.Kind{doc.KindPage}},
		{Containers: []string{"Platform"}},
		{Containers: []string{"platform"}},                        // containers compare without case
		{Authors: []string{"mei"}},                                // the local part of the address
		{Authors: []string{"mei@acme.com"}},                       // the address
		{Authors: []string{"Mei Tanaka"}},                         // the display name
		{Owners: []string{"u_ada"}},                               // the subject
		{Since: day(15)},                                          // changed since
		{Until: day(15)},                                          // changed before
		{Since: day(5), Until: day(21)},                           // a window
		{Terms: []string{"payments"}, Sources: []string{"slack"}}, // terms and filters together
		{Terms: []string{"payments"}, Kinds: []doc.Kind{doc.KindCode}},
	} {
		agree(t, s, rt, reader(), r)
	}
}

// testRetrievePermission is the case that matters most. It is not enough for
// the driver to return the right documents to the reader: it has to return
// nothing to somebody who may not read them, from inside its own query, which
// is what makes every count and facet built on top of it safe.
func testRetrievePermission(t *testing.T, s store.Store, rt store.Retriever) {
	docs := corpus()
	docs[1].Permissions.AllowGroups = []acl.Ref{{Source: "gdrive", Value: "sales@acme.com"}}
	docs[2].Permissions = acl.Permissions{
		Mode:       acl.ModeACL,
		Source:     "gdrive",
		AllowUsers: []acl.Ref{{Source: "gdrive", Value: "mei@acme.com"}},
		DenyGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
		Version:    1,
	}
	mustPut(t, s, docs...)

	got := agree(t, s, rt, reader(), store.Request{})
	if slices.Contains(got, "r2") {
		t.Fatal("retrieved a document allowed only to another group")
	}
	if slices.Contains(got, "r3") {
		t.Fatal("retrieved a document the reader is explicitly denied, deny has to beat allow")
	}
	agree(t, s, rt, stranger(), store.Request{})
	agree(t, s, rt, stranger(), store.Request{Terms: []string{"payments"}})
}

func testRetrieveQuarantine(t *testing.T, s store.Store, rt store.Retriever) {
	docs := corpus()
	docs[0].Permissions.Mode = acl.ModeUnknown
	mustPut(t, s, docs...)

	got := agree(t, s, rt, reader(), store.Request{Terms: []string{"payments"}})
	if slices.Contains(got, "r1") {
		t.Fatal("retrieved a document whose permissions never resolved")
	}
}

func testRetrieveTenant(t *testing.T, s store.Store, rt store.Retriever) {
	docs := corpus()
	docs[0].Tenant = "other"
	mustPut(t, s, docs...)

	got := agree(t, s, rt, reader(), store.Request{})
	if slices.Contains(got, "r1") {
		t.Fatal("retrieved a document belonging to another tenant")
	}
}

func testRetrieveNilPrincipal(t *testing.T, s store.Store, rt store.Retriever) {
	mustPut(t, s, corpus()...)

	var seen int
	err := rt.Retrieve(t.Context(), nil, store.Request{}, func(doc.Document) bool {
		seen++
		return true
	})
	if seen != 0 {
		t.Fatalf("a nil principal retrieved %d documents", seen)
	}
	// Returning an error is the better behaviour and returning nothing is
	// acceptable. Returning documents is not, which is what the count above
	// already checked.
	_ = err
}

func testRetrieveEarlyStop(t *testing.T, s store.Store, rt store.Retriever) {
	mustPut(t, s, corpus()...)

	var seen int
	if err := rt.Retrieve(t.Context(), reader(), store.Request{}, func(doc.Document) bool {
		seen++
		return false
	}); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if seen != 1 {
		t.Fatalf("the callback returned false after one document and was called %d times", seen)
	}
}

func testRetrieveDelete(t *testing.T, s store.Store, rt store.Retriever) {
	mustPut(t, s, corpus()...)
	if err := s.Delete(t.Context(), "r1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got := agree(t, s, rt, reader(), store.Request{Terms: []string{"runbook"}})
	if slices.Contains(got, "r1") {
		t.Fatal("retrieved a deleted document, the index still holds its terms")
	}
}

func testRetrieveRevocation(t *testing.T, s store.Store, rt store.Retriever) {
	docs := corpus()
	mustPut(t, s, docs...)

	revoked := docs[0]
	revoked.Permissions.Version = 2
	revoked.Permissions.DenyGroups = []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}}
	mustPut(t, s, revoked)

	got := agree(t, s, rt, reader(), store.Request{Terms: []string{"payments"}})
	if slices.Contains(got, "r1") {
		t.Fatal("retrieved a document after access to it was revoked")
	}
}
