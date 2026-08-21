// Package storetest is the conformance suite every storage driver has to pass.
//
// A driver that passes it can be swapped in without reading the rest of the
// codebase, and a driver that fails it is not a driver. Most of the cases are
// about permissions rather than about storage, which is the right proportion:
// the interesting way for a storage layer to be wrong here is not losing a
// document, it is returning one to the wrong person.
//
// Usage from a driver's own test file:
//
//	func TestConformance(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) store.Store { return memstore.New() })
//	}
package storetest

import (
	"errors"
	"testing"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// Factory returns a fresh, empty store for one test case.
type Factory func(t *testing.T) store.Store

// Run executes the suite against a driver.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })
			c.run(t, s)
		})
	}
}

type testCase struct {
	name string
	run  func(t *testing.T, s store.Store)
}

var cases = []testCase{
	{"put then get", testPutGet},
	{"get is not found for another reader", testGetHidden},
	{"missing and forbidden are indistinguishable", testNotFoundIsUniform},
	{"scan only yields readable documents", testScanFiltering},
	{"scan stops when the callback says so", testScanEarlyStop},
	{"unresolved permissions are never served", testQuarantine},
	{"documents of another tenant are never served", testTenantIsolation},
	{"a nil principal reads nothing", testNilPrincipal},
	{"delete removes and is repeatable", testDelete},
	{"put replaces an existing document", testReplace},
	{"a revoked document disappears", testRevocation},
	{"stats count served and quarantined documents", testStats},
	{"content is served only to a reader who may see the document", testContent},
	{"content never rides along on a query path", testContentIsNotInTheDocument},
	{"deleting a document deletes its content", testContentDelete},
	{"writes are reported after they are visible", testNotifyWrites},
	{"opens come back most recent first", testOpenLogOrder},
	{"opens honour the limit", testOpenLogLimit},
	{"an open is not a licence to keep reading", testOpenLogPermissions},
	{"a deleted document leaves the history", testOpenLogDelete},
	{"a history belongs to one tenant", testOpenLogTenants},
}

// contentStore skips a case for a driver that does not hold bytes.
//
// The capability is optional, so a driver without it is not failing the suite,
// and a driver with it does not get to opt out of the permission rule.
func contentStore(t *testing.T, s store.Store) store.ContentStore {
	t.Helper()
	cs, ok := s.(store.ContentStore)
	if !ok {
		t.Skip("driver does not implement store.ContentStore")
	}
	return cs
}

func withContent(id string, perm acl.Permissions) doc.Document {
	d := document(id, perm)
	d.Kind = doc.KindImage
	d.Body = ""
	d.Properties = map[string]string{doc.MediaType: "image/png"}
	d.Content = &doc.Content{Bytes: []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a}, Width: 12, Height: 8}
	return d
}

func reader() *acl.Principal {
	return &acl.Principal{
		Tenant:     "acme",
		Subject:    "u_mei",
		Identities: []acl.Identity{{Source: "gdrive", Value: "mei@acme.com"}},
		Groups:     acl.GroupSet{Version: 1, Members: []string{"gdrive:eng@acme.com"}},
	}
}

func stranger() *acl.Principal {
	return &acl.Principal{
		Tenant:     "acme",
		Subject:    "u_kenji",
		Identities: []acl.Identity{{Source: "gdrive", Value: "kenji@acme.com"}},
		Groups:     acl.GroupSet{Version: 1, Members: []string{"gdrive:sales@acme.com"}},
	}
}

func document(id string, perm acl.Permissions) doc.Document {
	return doc.Document{
		ID:          id,
		Tenant:      "acme",
		Source:      "gdrive",
		Kind:        doc.KindPage,
		Title:       "runbook " + id,
		Body:        "how to fail over the payments queue",
		Permissions: perm,
	}
}

func readable() acl.Permissions {
	return acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
		Version:     1,
	}
}

func mustPut(t *testing.T, s store.Store, docs ...doc.Document) {
	t.Helper()
	if err := s.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

func scanIDs(t *testing.T, s store.Store, p *acl.Principal) []string {
	t.Helper()
	var ids []string
	if err := s.Scan(t.Context(), p, func(d doc.Document) bool {
		ids = append(ids, d.ID)
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return ids
}

func testPutGet(t *testing.T, s store.Store) {
	mustPut(t, s, document("d1", readable()))
	got, err := s.Get(t.Context(), reader(), "d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "d1" || got.Title != "runbook d1" {
		t.Fatalf("Get returned %+v", got)
	}
}

func testGetHidden(t *testing.T, s store.Store) {
	mustPut(t, s, document("d1", readable()))
	if _, err := s.Get(t.Context(), stranger(), "d1"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("Get by a reader without access returned %v, want ErrNotFound", err)
	}
}

func testNotFoundIsUniform(t *testing.T, s store.Store) {
	mustPut(t, s, document("d1", readable()))

	_, hidden := s.Get(t.Context(), stranger(), "d1")
	_, missing := s.Get(t.Context(), stranger(), "does-not-exist")

	if !errors.Is(hidden, genba.ErrNotFound) || !errors.Is(missing, genba.ErrNotFound) {
		t.Fatalf("hidden = %v, missing = %v, both should match ErrNotFound", hidden, missing)
	}
	if hidden.Error() != missing.Error() {
		t.Fatalf("a hidden document and a missing one produced different errors:\n hidden: %v\nmissing: %v", hidden, missing)
	}
}

func testScanFiltering(t *testing.T, s store.Store) {
	mine := readable()
	theirs := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "sales@acme.com"}},
		Version:     1,
	}
	mustPut(t, s, document("d1", mine), document("d2", theirs), document("d3", mine))

	ids := scanIDs(t, s, reader())
	if len(ids) != 2 {
		t.Fatalf("scan yielded %v, want the two readable documents", ids)
	}
	for _, id := range ids {
		if id == "d2" {
			t.Fatal("scan yielded a document the reader has no access to")
		}
	}
}

func testScanEarlyStop(t *testing.T, s store.Store) {
	mustPut(t, s, document("d1", readable()), document("d2", readable()), document("d3", readable()))

	seen := 0
	if err := s.Scan(t.Context(), reader(), func(doc.Document) bool {
		seen++
		return false
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if seen != 1 {
		t.Fatalf("the callback ran %d times after asking to stop, want 1", seen)
	}
}

func testQuarantine(t *testing.T, s store.Store) {
	unresolved := acl.Permissions{
		Mode:        acl.ModeUnknown,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
	}
	mustPut(t, s, document("d1", readable()), document("d2", unresolved))

	if _, err := s.Get(t.Context(), reader(), "d2"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("a document with unresolved permissions was returned by Get: %v", err)
	}
	for _, id := range scanIDs(t, s, reader()) {
		if id == "d2" {
			t.Fatal("a document with unresolved permissions was returned by Scan")
		}
	}
}

func testTenantIsolation(t *testing.T, s store.Store) {
	other := document("d1", acl.Permissions{Mode: acl.ModePublicToTenant, Version: 1})
	other.Tenant = "globex"
	mustPut(t, s, other)

	if _, err := s.Get(t.Context(), reader(), "d1"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("a document from another tenant was returned by Get: %v", err)
	}
	if ids := scanIDs(t, s, reader()); len(ids) != 0 {
		t.Fatalf("a document from another tenant was returned by Scan: %v", ids)
	}
}

func testNilPrincipal(t *testing.T, s store.Store) {
	mustPut(t, s, document("d1", acl.Permissions{Mode: acl.ModePublicToTenant, Version: 1}))

	if _, err := s.Get(t.Context(), nil, "d1"); err == nil {
		t.Fatal("Get with a nil principal returned a document")
	}
	err := s.Scan(t.Context(), nil, func(doc.Document) bool {
		t.Fatal("Scan with a nil principal yielded a document")
		return false
	})
	if err == nil {
		t.Fatal("Scan with a nil principal returned no error")
	}
}

func testDelete(t *testing.T, s store.Store) {
	mustPut(t, s, document("d1", readable()))
	if err := s.Delete(t.Context(), "d1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(t.Context(), reader(), "d1"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("Get after Delete returned %v, want ErrNotFound", err)
	}
	if err := s.Delete(t.Context(), "d1", "never-existed"); err != nil {
		t.Fatalf("deleting twice should be harmless, got %v", err)
	}
}

func testReplace(t *testing.T, s store.Store) {
	d := document("d1", readable())
	mustPut(t, s, d)

	d.Title = "runbook d1, second edition"
	mustPut(t, s, d)

	got, err := s.Get(t.Context(), reader(), "d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "runbook d1, second edition" {
		t.Fatalf("Get returned the stale title %q", got.Title)
	}
	if ids := scanIDs(t, s, reader()); len(ids) != 1 {
		t.Fatalf("replacing a document left %d copies behind", len(ids))
	}
}

func testRevocation(t *testing.T, s store.Store) {
	d := document("d1", readable())
	mustPut(t, s, d)
	if _, err := s.Get(t.Context(), reader(), "d1"); err != nil {
		t.Fatalf("Get before the revocation: %v", err)
	}

	d.Permissions.DenyGroups = []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}}
	d.Permissions.Version++
	mustPut(t, s, d)

	if _, err := s.Get(t.Context(), reader(), "d1"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("a revoked document was still readable: %v", err)
	}
}

func testStats(t *testing.T, s store.Store) {
	mustPut(t, s,
		document("d1", readable()),
		document("d2", readable()),
		document("d3", acl.Permissions{Mode: acl.ModeUnknown}),
	)
	st, err := s.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Documents != 2 || st.Quarantined != 1 {
		t.Fatalf("Stats = %+v, want 2 served and 1 quarantined", st)
	}
}

func testContent(t *testing.T, s store.Store) {
	cs := contentStore(t, s)
	mustPut(t, s, withContent("d1", readable()))

	got, err := cs.Content(t.Context(), reader(), "d1")
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	if len(got.Bytes) != 6 || got.Width != 12 || got.Height != 8 {
		t.Fatalf("Content returned %d bytes at %dx%d, want 6 bytes at 12x8", len(got.Bytes), got.Width, got.Height)
	}

	if _, err := cs.Content(t.Context(), stranger(), "d1"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("Content for a reader without access returned %v, want ErrNotFound", err)
	}
	// A document that exists and holds no bytes answers the same way a missing
	// one does, so the response cannot be used to ask whether a document is
	// there.
	mustPut(t, s, document("d2", readable()))
	if _, err := cs.Content(t.Context(), reader(), "d2"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("Content for a document with no bytes returned %v, want ErrNotFound", err)
	}
	if _, err := cs.Content(t.Context(), reader(), "missing"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("Content for a missing document returned %v, want ErrNotFound", err)
	}
	if _, err := cs.Content(t.Context(), nil, "d1"); err == nil {
		t.Fatal("Content with a nil principal returned bytes")
	}
}

func testContentIsNotInTheDocument(t *testing.T, s store.Store) {
	contentStore(t, s)
	mustPut(t, s, withContent("d1", readable()))

	got, err := s.Get(t.Context(), reader(), "d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Content != nil {
		t.Fatal("Get returned a document carrying content bytes")
	}
	if err := s.Scan(t.Context(), reader(), func(d doc.Document) bool {
		if d.Content != nil {
			t.Error("Scan yielded a document carrying content bytes")
		}
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
}

func testContentDelete(t *testing.T, s store.Store) {
	cs := contentStore(t, s)
	mustPut(t, s, withContent("d1", readable()))
	if err := s.Delete(t.Context(), "d1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := cs.Content(t.Context(), reader(), "d1"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("Content after Delete returned %v, want ErrNotFound", err)
	}

	// Replacing an image with a text document has to leave nothing behind.
	mustPut(t, s, withContent("d2", readable()))
	mustPut(t, s, document("d2", readable()))
	if _, err := cs.Content(t.Context(), reader(), "d2"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("Content after a put with no bytes returned %v, want ErrNotFound", err)
	}
}
