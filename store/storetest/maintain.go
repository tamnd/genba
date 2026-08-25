package storetest

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// RunMaintenance checks a driver's [store.Maintenance] against what the
// ingestion pipeline relies on.
//
// The suite is separate from [Run] because the capability is optional, and it
// is worth having on its own because both halves of it are easy to implement in
// a way that looks right and is not. An inventory that quietly leaves out the
// quarantined documents makes reconciliation unable to clean up exactly the
// documents an operator most wants gone, and a permission change that forgets
// the index leaves a revoked document matching queries while Get says it is not
// there.
func RunMaintenance(t *testing.T, newStore Factory) {
	t.Helper()
	for _, c := range maintainCases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })

			m, ok := s.(store.Maintenance)
			if !ok {
				t.Skip("this driver does not implement store.Maintenance")
			}
			c.run(t, s, m)
		})
	}
}

type maintainCase struct {
	name string
	run  func(t *testing.T, s store.Store, m store.Maintenance)
}

var maintainCases = []maintainCase{
	{"inventory lists one source of one tenant", testInventoryScope},
	{"inventory reports the stored version", testInventoryVersion},
	{"inventory includes quarantined documents", testInventoryQuarantined},
	{"inventory says which documents are held", testInventoryReportsHeld},
	{"inventory stops when the callback says so", testInventoryEarlyStop},
	{"a permission change does not rewrite the document", testSetPermissionsKeepsContent},
	{"a permission change ignores ids of another tenant", testSetPermissionsTenant},
	{"a revocation into quarantine takes a document out of the index", testSetPermissionsQuarantine},
	{"a permission change is reported as a write", testSetPermissionsNotifies},
}

// versioned is a document carrying the source's own revision, which is the
// field an incremental sync compares against.
func versioned(id, source, version string) doc.Document {
	d := document(id, readable())
	d.Source = source
	d.SourceUpdate = version
	return d
}

// inventory collects a whole source, sorted, so a driver is free to return its
// documents in whatever order its storage puts them in.
func inventory(t *testing.T, m store.Maintenance, tenant, source string) []store.Item {
	t.Helper()
	var items []store.Item
	if err := m.Inventory(t.Context(), tenant, source, func(it store.Item) bool {
		items = append(items, it)
		return true
	}); err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	slices.SortFunc(items, func(a, b store.Item) int { return strings.Compare(a.ID, b.ID) })
	return items
}

func inventoryIDs(t *testing.T, m store.Maintenance, tenant, source string) []string {
	t.Helper()
	items := inventory(t, m, tenant, source)
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

func testInventoryScope(t *testing.T, s store.Store, m store.Maintenance) {
	other := versioned("d4", "gdrive", "v1")
	other.Tenant = "globex"
	mustPut(t, s,
		versioned("d1", "gdrive", "v1"),
		versioned("d2", "gdrive", "v1"),
		versioned("d3", "slack", "v1"),
		other,
	)

	if ids := inventoryIDs(t, m, "acme", "gdrive"); !slices.Equal(ids, []string{"d1", "d2"}) {
		t.Fatalf("Inventory returned %v, want only the gdrive documents of acme", ids)
	}
	if ids := inventoryIDs(t, m, "acme", "slack"); !slices.Equal(ids, []string{"d3"}) {
		t.Fatalf("Inventory of slack returned %v, want [d3]", ids)
	}
	if ids := inventoryIDs(t, m, "acme", "never-crawled"); len(ids) != 0 {
		t.Fatalf("Inventory of a source with nothing in it returned %v", ids)
	}
}

// testInventoryVersion is what makes a second sync cheap. A store that returns
// an empty version for every document turns every incremental run into a full
// recrawl, and nothing else in the suite would notice.
func testInventoryVersion(t *testing.T, s store.Store, m store.Maintenance) {
	mustPut(t, s, versioned("d1", "gdrive", "etag-1"))

	items := inventory(t, m, "acme", "gdrive")
	if len(items) != 1 || items[0].Version != "etag-1" {
		t.Fatalf("Inventory returned %+v, want one item at version etag-1", items)
	}

	// A rewrite at a new revision has to be visible here, or the comparison
	// would say the stored copy is current forever.
	d := versioned("d1", "gdrive", "etag-2")
	d.Title = "runbook d1, second edition"
	mustPut(t, s, d)

	items = inventory(t, m, "acme", "gdrive")
	if len(items) != 1 || items[0].Version != "etag-2" {
		t.Fatalf("after a rewrite Inventory returned %+v, want one item at version etag-2", items)
	}
}

func testInventoryQuarantined(t *testing.T, s store.Store, m store.Maintenance) {
	bad := versioned("d2", "gdrive", "v1")
	bad.Permissions = acl.Permissions{Mode: acl.ModeUnknown, Source: "gdrive"}
	mustPut(t, s, versioned("d1", "gdrive", "v1"), bad)

	if ids := inventoryIDs(t, m, "acme", "gdrive"); !slices.Equal(ids, []string{"d1", "d2"}) {
		t.Fatalf("Inventory returned %v, want the quarantined document too", ids)
	}
}

// testInventoryReportsHeld is what the automatic retry stands on. A held
// document is the one kind of drift the version comparison cannot see, because
// the source and the index agree about the revision and the reason it is held is
// at the source, so a driver that reported nothing here would leave the
// quarantine to be emptied by hand.
func testInventoryReportsHeld(t *testing.T, s store.Store, m store.Maintenance) {
	bad := versioned("d2", "gdrive", "v1")
	bad.Permissions = acl.Permissions{Mode: acl.ModeUnknown, Source: "gdrive", Reason: "the directory was unreachable"}
	mustPut(t, s, versioned("d1", "gdrive", "v1"), bad)

	held := map[string]bool{}
	for _, it := range inventory(t, m, "acme", "gdrive") {
		held[it.ID] = it.Held
	}
	if held["d1"] {
		t.Error("a queryable document was reported as held, so every sweep would refetch the whole corpus")
	}
	if !held["d2"] {
		t.Error("a quarantined document was not reported as held, so nothing would ever retry it")
	}

	// And it moves. A document that resolves stops being held, which is the
	// answer the sweep after a fixed directory has to get.
	good := versioned("d2", "gdrive", "v1")
	mustPut(t, s, good)

	for _, it := range inventory(t, m, "acme", "gdrive") {
		if it.ID == "d2" && it.Held {
			t.Error("a document whose permissions resolved is still reported as held")
		}
	}
}

func testInventoryEarlyStop(t *testing.T, s store.Store, m store.Maintenance) {
	mustPut(t, s,
		versioned("d1", "gdrive", "v1"),
		versioned("d2", "gdrive", "v1"),
		versioned("d3", "gdrive", "v1"),
	)

	seen := 0
	if err := m.Inventory(t.Context(), "acme", "gdrive", func(store.Item) bool {
		seen++
		return false
	}); err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if seen != 1 {
		t.Fatalf("Inventory called the callback %d times after it returned false, want 1", seen)
	}
}

// testSetPermissionsKeepsContent is the whole point of the call: a company wide
// access control change costs a write per document and not a recrawl, so the
// document that comes back afterwards has to be the one that went in.
func testSetPermissionsKeepsContent(t *testing.T, s store.Store, m store.Maintenance) {
	d := versioned("d1", "gdrive", "etag-1")
	d.Title = "the payments failover runbook"
	mustPut(t, s, d)

	sales := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "sales@acme.com"}},
		Version:     2,
	}
	n, err := m.SetPermissions(t.Context(), "acme", map[string]acl.Permissions{"d1": sales, "missing": sales})
	if err != nil {
		t.Fatalf("SetPermissions: %v", err)
	}
	// An id the store does not hold is skipped rather than counted, so a caller
	// can tell a run that did something from one that did not.
	if n != 1 {
		t.Fatalf("SetPermissions changed %d documents, want 1", n)
	}

	if _, err := s.Get(t.Context(), reader(), "d1"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("a reader whose access was taken away could still read the document: %v", err)
	}
	got, err := s.Get(t.Context(), stranger(), "d1")
	if err != nil {
		t.Fatalf("the reader who was granted access could not read the document: %v", err)
	}
	if got.Title != "the payments failover runbook" || got.Body != d.Body {
		t.Fatalf("a permission change rewrote the document: title %q, body %q", got.Title, got.Body)
	}
	if got.SourceUpdate != "etag-1" {
		t.Fatalf("a permission change moved the stored version to %q, so the next sync would recrawl", got.SourceUpdate)
	}
	if items := inventory(t, m, "acme", "gdrive"); len(items) != 1 || items[0].Version != "etag-1" {
		t.Fatalf("after a permission change Inventory returned %+v, want the version unchanged", items)
	}
}

func testSetPermissionsTenant(t *testing.T, s store.Store, m store.Maintenance) {
	other := document("d1", acl.Permissions{Mode: acl.ModePublicToTenant, Version: 1})
	other.Tenant = "globex"
	mustPut(t, s, other)

	n, err := m.SetPermissions(t.Context(), "acme", map[string]acl.Permissions{"d1": readable()})
	if err != nil {
		t.Fatalf("SetPermissions: %v", err)
	}
	if n != 0 {
		t.Fatalf("SetPermissions changed %d documents of another tenant, want 0", n)
	}

	// The document is still what globex put there. A caller able to rewrite the
	// access control lists of a corpus it named by guessing at ids would be a
	// hole no amount of filtering on the read path closes.
	globex := &acl.Principal{Tenant: "globex", Subject: "u_pat"}
	got, err := s.Get(t.Context(), globex, "d1")
	if err != nil {
		t.Fatalf("Get as the owning tenant: %v", err)
	}
	if got.Permissions.Mode != acl.ModePublicToTenant {
		t.Fatalf("another tenant's permissions were rewritten to mode %v", got.Permissions.Mode)
	}
}

// testSetPermissionsQuarantine is the case a driver gets wrong. A quarantined
// document is absent from the full text index and the corpus statistics, so a
// permission change that crosses that line has to take it out or put it back,
// and a driver that only updates the columns the visibility predicate reads
// leaves a document matching queries after it has been revoked.
func testSetPermissionsQuarantine(t *testing.T, s store.Store, m store.Maintenance) {
	mustPut(t, s, versioned("d1", "gdrive", "v1"), versioned("d2", "gdrive", "v1"))

	unresolved := acl.Permissions{Mode: acl.ModeUnknown, Source: "gdrive"}
	if _, err := m.SetPermissions(t.Context(), "acme", map[string]acl.Permissions{"d1": unresolved}); err != nil {
		t.Fatalf("SetPermissions: %v", err)
	}

	if _, err := s.Get(t.Context(), reader(), "d1"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("a quarantined document was still readable: %v", err)
	}
	st, err := s.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Documents != 1 || st.Quarantined != 1 {
		t.Fatalf("Stats = %+v, want 1 served and 1 quarantined", st)
	}
	matches(t, s, m, []string{"d2"})

	// And back, which is the direction that has to pay for the analyzer again.
	if _, err := m.SetPermissions(t.Context(), "acme", map[string]acl.Permissions{"d1": readable()}); err != nil {
		t.Fatalf("SetPermissions: %v", err)
	}
	if _, err := s.Get(t.Context(), reader(), "d1"); err != nil {
		t.Fatalf("a document whose permissions resolved again was not readable: %v", err)
	}
	st, err = s.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Documents != 2 || st.Quarantined != 0 {
		t.Fatalf("Stats = %+v, want 2 served and none quarantined", st)
	}
	matches(t, s, m, []string{"d1", "d2"})
}

// matches asserts what a term query finds, for a driver that has an index. The
// comparison is against the Go rule rather than against a literal, so it is the
// same assertion the retrieval suite makes and it cannot drift from it.
func matches(t *testing.T, s store.Store, m store.Maintenance, want []string) {
	t.Helper()
	rt, ok := m.(store.Retriever)
	if !ok {
		return
	}
	got := agree(t, s, rt, reader(), store.Request{Terms: []string{"runbook"}})
	if !slices.Equal(got, want) {
		t.Fatalf("a term query retrieved %v, want %v", got, want)
	}
}

func testSetPermissionsNotifies(t *testing.T, s store.Store, m store.Maintenance) {
	n, ok := s.(store.Notifier)
	if !ok {
		t.Skip("this driver does not report its writes")
	}
	mustPut(t, s, versioned("d1", "gdrive", "v1"))

	var got notified
	stop := n.OnChange(got.record)
	defer stop()

	// A cache holding a result page for a document somebody has just been locked
	// out of has to be told, and a revocation is the change it is least
	// acceptable to miss.
	if _, err := m.SetPermissions(t.Context(), "acme", map[string]acl.Permissions{
		"d1":      {Mode: acl.ModeUnknown, Source: "gdrive"},
		"missing": readable(),
	}); err != nil {
		t.Fatalf("SetPermissions: %v", err)
	}

	changes := got.all()
	if len(changes) != 1 {
		t.Fatalf("a permission change reported %d changes, want 1", len(changes))
	}
	if changes[0].Tenant != "acme" || !slices.Equal(changes[0].IDs, []string{"d1"}) {
		t.Fatalf("the change reported tenant %q and ids %v, want acme and [d1]", changes[0].Tenant, changes[0].IDs)
	}
	if changes[0].Deleted {
		t.Error("a permission change reported itself as a delete, so a subscriber would drop the document entirely")
	}
}
