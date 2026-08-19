package index_test

import (
	"slices"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
	"github.com/tamnd/genba/store/sqlitestore"
)

// The searcher has two ways to collect candidates. A driver that implements
// store.Retriever is asked for the match set and does the filtering in its own
// query, and a driver that does not is scanned with the same rules applied in
// Go. There is one definition of the match set and both paths are held to it,
// so the only honest test is to run the same searches through both drivers and
// require the same answer.
//
// This is also the test that would catch a driver drifting from the analyzer:
// the ranking is computed from what the driver returned, so a candidate set
// that differs by one document changes the order of everything after it.

func TestDriversAgree(t *testing.T) {
	mem := memstore.New()
	t.Cleanup(func() { _ = mem.Close() })

	sq, err := sqlitestore.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })

	if _, ok := any(mem).(store.Retriever); ok {
		t.Fatal("memstore now implements store.Retriever, so this test no longer covers the scan path")
	}
	if _, ok := any(sq).(store.Retriever); !ok {
		t.Fatal("sqlitestore does not implement store.Retriever, so this test no longer covers the retrieve path")
	}

	docs := driverCorpus()
	for _, st := range []store.Store{mem, sq} {
		if err := st.Put(t.Context(), docs...); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	scanning := index.New(mem, index.WithClock(clock))
	retrieving := index.New(sq, index.WithClock(clock))
	if scanning.Retrieving() {
		t.Fatal("the memstore searcher reports that it retrieves")
	}
	if !retrieving.Retrieving() {
		t.Fatal("the sqlite searcher reports that it scans")
	}

	p := &acl.Principal{
		Tenant:  "acme",
		Subject: "u_mei",
		Groups:  acl.GroupSet{Version: 1, Members: []string{"gdrive:eng@acme.com"}},
	}
	stranger := &acl.Principal{
		Tenant:  "acme",
		Subject: "u_nobody",
		Groups:  acl.GroupSet{Version: 1},
	}

	queries := []struct {
		name  string
		who   *acl.Principal
		query index.Query
	}{
		{"everything", p, index.Query{}},
		{"one term", p, index.Query{Text: "payments"}},
		{"two terms", p, index.Query{Text: "payments failover"}},
		{"a term nothing carries", p, index.Query{Text: "zeppelin"}},
		{"a source filter", p, index.Query{Sources: []string{"slack"}}},
		{"a kind filter", p, index.Query{Kinds: []doc.Kind{doc.KindTicket}}},
		{"a container filter", p, index.Query{Containers: []string{"Incidents"}}},
		{"an author filter", p, index.Query{Authors: []string{"mei@acme.com"}}},
		{"an owner filter", p, index.Query{Owners: []string{"mei@acme.com"}}},
		{"a term and a filter", p, index.Query{Text: "payments", Sources: []string{"gdrive"}}},
		{"a lower time bound", p, index.Query{Since: epoch.AddDate(0, 0, -30)}},
		{"an upper time bound", p, index.Query{Until: epoch.AddDate(0, 0, -30)}},
		{"a window", p, index.Query{Since: epoch.AddDate(0, 0, -60), Until: epoch.AddDate(0, 0, -10)}},
		{"sorted by recency", p, index.Query{Sort: index.ByRecent}},
		{"a second page", p, index.Query{Limit: 2, Offset: 2}},
		{"an accented term", p, index.Query{Text: "café"}},
		{"a CJK term", p, index.Query{Text: "決済"}},
		{"a stranger sees nothing", stranger, index.Query{}},
		{"a stranger searching", stranger, index.Query{Text: "payments"}},
	}

	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			want, err := scanning.Search(t.Context(), tc.who, tc.query)
			if err != nil {
				t.Fatalf("memstore Search: %v", err)
			}
			got, err := retrieving.Search(t.Context(), tc.who, tc.query)
			if err != nil {
				t.Fatalf("sqlite Search: %v", err)
			}

			if !slices.Equal(ids(got), ids(want)) {
				t.Fatalf("sqlite returned %v, memstore returned %v", ids(got), ids(want))
			}
			if got.Total != want.Total {
				t.Fatalf("Total = %d, memstore said %d", got.Total, want.Total)
			}
			for field, values := range want.Facets {
				if !slices.Equal(got.Facets[field], values) {
					t.Fatalf("%s facets = %v, memstore said %v", field, got.Facets[field], values)
				}
			}
			// The snippet comes from the body, and the body is refetched for the
			// page rather than carried through the candidate set, so agreeing on
			// it is what says both drivers stored the same document.
			for i := range got.Hits {
				if got.Hits[i].Snippet != want.Hits[i].Snippet {
					t.Fatalf("snippet for %s = %q, memstore said %q", got.Hits[i].Document.ID, got.Hits[i].Snippet, want.Hits[i].Snippet)
				}
			}
		})
	}
}

func driverCorpus() []doc.Document {
	mei := doc.Person{Subject: "u_mei", Email: "mei@acme.com", Name: "Mei Tanaka"}
	sam := doc.Person{Subject: "u_sam", Email: "sam@acme.com", Name: "Sam Ortiz"}
	open := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		Version:     1,
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
	}
	shut := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		Version:     1,
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "finance@acme.com"}},
	}

	return []doc.Document{
		{
			ID: "d1", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments failover runbook", Body: "Failover the payments queue when the primary region is unhealthy.",
			Container: "Platform", Author: mei, Owner: mei,
			ModifiedAt: epoch.AddDate(0, 0, -5), Permissions: open,
		},
		{
			ID: "d2", Tenant: "acme", Source: "slack", Kind: doc.KindMessage,
			Title: "Payments are slow again", Body: "The payments dashboard is red, opening an incident.",
			Container: "Incidents", Author: sam, Owner: mei,
			ModifiedAt: epoch.AddDate(0, 0, -40), Permissions: open,
		},
		{
			ID: "d3", Tenant: "acme", Source: "jira", Kind: doc.KindTicket,
			Title: "Café latency", Body: "The café service is slow after the migration.",
			Container: "Platform", Author: sam, Owner: sam,
			ModifiedAt: epoch.AddDate(0, 0, -90), Permissions: open,
		},
		{
			ID: "d4", Tenant: "acme", Source: "gdrive", Kind: doc.KindFile,
			Title: "決済の設計", Body: "決済サービスの設計メモ。",
			Container: "Platform", Author: mei, Owner: mei,
			ModifiedAt: epoch.AddDate(0, 0, -1), Permissions: open,
		},
		{
			ID: "d5", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments revenue forecast", Body: "Payments revenue by quarter.",
			Container: "Finance", Author: sam, Owner: sam,
			ModifiedAt: time.Time{}, Permissions: shut,
		},
	}
}
