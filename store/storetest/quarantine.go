package storetest

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// RunQuarantine checks a driver's [store.Quarantine] against what the
// administration screen relies on.
//
// The suite is small and every case in it is a way of returning the wrong
// documents while looking correct. Listing the queryable ones is the obvious
// one and is caught by the first case. The two that are not obvious are the
// tenant filter, which a driver that reaches straight for the queryable column
// forgets because the quarantine of a shared deployment is not one list, and
// the reason, which is the only field on the screen anybody acts on and is the
// one a driver drops when it builds the row out of columns instead of out of
// the document.
func RunQuarantine(t *testing.T, newStore Factory) {
	t.Helper()
	for _, c := range quarantineCases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })

			q, ok := s.(store.Quarantine)
			if !ok {
				t.Skip("this driver does not implement store.Quarantine")
			}
			c.run(t, s, q)
		})
	}
}

type quarantineCase struct {
	name string
	run  func(t *testing.T, s store.Store, q store.Quarantine)
}

var quarantineCases = []quarantineCase{
	{"only held documents are listed", testQuarantinedOnlyHeld},
	{"the reason survives the round trip", testQuarantinedReason},
	{"another tenant's held documents are not listed", testQuarantinedTenant},
	{"the limit is honoured and a limit of zero returns nothing", testQuarantinedLimit},
	{"a document that resolves later leaves the list", testQuarantinedReleased},
}

// held is a document whose permissions did not resolve, with the reason the
// connector gave.
func held(id, reason string) doc.Document {
	d := document(id, acl.Permissions{Mode: acl.ModeUnknown, Source: "gdrive", Reason: reason})
	d.ModifiedAt = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	return d
}

// quarantined collects the list, sorted by id, so a driver is free to return
// its documents in whatever order its storage puts them in.
func quarantined(t *testing.T, q store.Quarantine, tenant string, limit int) []store.Held {
	t.Helper()
	out, err := q.Quarantined(t.Context(), tenant, limit)
	if err != nil {
		t.Fatalf("Quarantined: %v", err)
	}
	slices.SortFunc(out, func(a, b store.Held) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func ids(list []store.Held) []string {
	out := make([]string, len(list))
	for i, h := range list {
		out[i] = h.ID
	}
	return out
}

func testQuarantinedOnlyHeld(t *testing.T, s store.Store, q store.Quarantine) {
	mustPut(t, s, document("d1", readable()), held("d2", "a deny aimed at everybody"), document("d3", readable()))

	got := ids(quarantined(t, q, "acme", 10))
	if !slices.Equal(got, []string{"d2"}) {
		t.Fatalf("held documents = %v, want [d2]", got)
	}
}

func testQuarantinedReason(t *testing.T, s store.Store, q store.Quarantine) {
	const reason = "foreign domain: a grant to @contractor.example"
	mustPut(t, s, held("d1", reason))

	got := quarantined(t, q, "acme", 10)
	if len(got) != 1 {
		t.Fatalf("held documents = %d, want 1", len(got))
	}
	if got[0].Reason != reason {
		t.Fatalf("reason = %q, want %q", got[0].Reason, reason)
	}
	// The title is the other half of what makes the list readable. A screen of
	// opaque ids cannot be looked at and recognised.
	if got[0].Title != "runbook d1" {
		t.Fatalf("title = %q, want %q", got[0].Title, "runbook d1")
	}
	if got[0].Source != "gdrive" {
		t.Fatalf("source = %q, want gdrive", got[0].Source)
	}
}

func testQuarantinedTenant(t *testing.T, s store.Store, q store.Quarantine) {
	other := held("d2", "a deny aimed at everybody")
	other.Tenant = "other"
	mustPut(t, s, held("d1", "a deny aimed at everybody"), other)

	got := ids(quarantined(t, q, "acme", 10))
	if !slices.Equal(got, []string{"d1"}) {
		t.Fatalf("held documents = %v, want [d1]", got)
	}
}

func testQuarantinedLimit(t *testing.T, s store.Store, q store.Quarantine) {
	mustPut(t, s, held("d1", "malformed grant"), held("d2", "malformed grant"), held("d3", "malformed grant"))

	if got := quarantined(t, q, "acme", 2); len(got) != 2 {
		t.Fatalf("held documents at limit 2 = %d, want 2", len(got))
	}
	// Zero is not "no limit". A screen that asks for none must not be handed
	// the whole quarantine of a corpus.
	if got := quarantined(t, q, "acme", 0); len(got) != 0 {
		t.Fatalf("held documents at limit 0 = %d, want 0", len(got))
	}
}

func testQuarantinedReleased(t *testing.T, s store.Store, q store.Quarantine) {
	mustPut(t, s, held("d1", "malformed grant"))
	if got := quarantined(t, q, "acme", 10); len(got) != 1 {
		t.Fatalf("held documents before the fix = %d, want 1", len(got))
	}

	// The same document, indexed again by a connector that now understands its
	// access control list. This is what an operator does after fixing the thing
	// the reason named, and a driver that keeps the row in the list would have
	// them chasing a document that is already being served.
	mustPut(t, s, document("d1", readable()))
	if got := quarantined(t, q, "acme", 10); len(got) != 0 {
		t.Fatalf("held documents after the fix = %d, want 0", len(got))
	}
}
