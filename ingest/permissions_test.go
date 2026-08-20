package ingest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/ingest"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
)

// engineering is an access control list nobody in these tests belongs to, which
// is how a revocation is written.
func engineering() acl.Permissions {
	return acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "test",
		AllowGroups: []acl.Ref{{Source: "test", Value: "engineering"}},
		Version:     2,
	}
}

// TestAPermissionChangeCostsAWriteNotARecrawl is the box. The connector reports
// who may read a document and nothing else, and the document that comes back
// afterwards is the one that went in.
func TestAPermissionChangeCostsAWriteNotARecrawl(t *testing.T) {
	st := memstore.New()
	p, err := ingest.New(st, connector.NewMemoryCheckpoints())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	run(t, p, &fakeSource{name: "test", changes: changes(4)})

	// A second pipeline with no checkpoints, because these changes carry no
	// cursor and a resumed run would filter them out before the pipeline ever
	// saw one.
	p, _ = ingest.New(st, nil)

	// The body is deliberately empty. A pipeline that stored this change as a
	// document would leave an indexed document with nothing in it, which is
	// exactly the failure worth catching.
	src := &fakeSource{name: "test", changes: []connector.Change{{
		Document:        doc.Document{ID: "doc-0002", Permissions: engineering()},
		PermissionsOnly: true,
	}}}
	stats := run(t, p, src)

	if stats.Repermissioned != 1 {
		t.Fatalf("the run repermissioned %d documents, want 1", stats.Repermissioned)
	}
	if stats.Indexed != 0 {
		t.Fatalf("a permission change indexed %d documents, so it was a recrawl", stats.Indexed)
	}

	if _, err := st.Get(t.Context(), principal(), "doc-0002"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("a reader whose access was taken away can still read the document: %v", err)
	}
	member := &acl.Principal{
		Tenant: tenant, Subject: "member", Kind: acl.KindUser,
		Groups: acl.GroupSet{Version: 1, Members: []string{"test:engineering"}},
	}
	got, err := st.Get(t.Context(), member, "doc-0002")
	if err != nil {
		t.Fatalf("the reader who was granted access cannot read the document: %v", err)
	}
	if got.Title != "document number 2" || got.Body == "" {
		t.Fatalf("a permission change rewrote the document: title %q, body of %d bytes", got.Title, len(got.Body))
	}
}

// TestAPermissionChangeForAnUnknownDocumentIsNotAnError covers the disagreement
// between a source and an index that reconciliation exists to settle. It must
// not stop a sync.
func TestAPermissionChangeForAnUnknownDocumentIsNotAnError(t *testing.T) {
	st := memstore.New()
	p, _ := ingest.New(st, connector.NewMemoryCheckpoints())

	stats := run(t, p, &fakeSource{name: "test", changes: []connector.Change{
		{Document: doc.Document{ID: "never-indexed", Permissions: engineering()}, PermissionsOnly: true},
	}})

	if stats.Repermissioned != 0 {
		t.Fatalf("repermissioned %d documents that are not in the index", stats.Repermissioned)
	}
}

// TestManyPermissionChangesAreOneWrite is why the change carries no content. An
// edit to one group at the source governs a subtree, and the pipeline has to
// turn that into a batch rather than a write per document.
func TestManyPermissionChangesAreOneWrite(t *testing.T) {
	st := &countingMaintenance{Store: memstore.New()}
	p, err := ingest.New(st, connector.NewMemoryCheckpoints(), ingest.WithBatchSize(500))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	run(t, p, &fakeSource{name: "test", changes: changes(200)})
	p, _ = ingest.New(st, nil, ingest.WithBatchSize(500))

	var perms []connector.Change
	for _, ch := range changes(200) {
		perms = append(perms, connector.Change{
			Document:        doc.Document{ID: ch.Document.ID, Permissions: engineering()},
			PermissionsOnly: true,
		})
	}
	stats := run(t, p, &fakeSource{name: "test", changes: perms})

	if stats.Repermissioned != 200 {
		t.Fatalf("repermissioned %d documents, want 200", stats.Repermissioned)
	}
	if st.calls != 1 {
		t.Fatalf("200 permission changes cost %d calls to the store, want 1", st.calls)
	}
}

// TestAStoreThatCannotRepermissionSaysSo refuses rather than doing nothing. A
// sync that reported success while a revocation went nowhere is worse than one
// that failed.
func TestAStoreThatCannotRepermissionSaysSo(t *testing.T) {
	p, err := ingest.New(plainStore{memstore.New()}, connector.NewMemoryCheckpoints())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = p.Run(t.Context(), tenant, &fakeSource{name: "test", changes: []connector.Change{
		{Document: doc.Document{ID: "doc-0000", Permissions: engineering()}, PermissionsOnly: true},
	}})
	if err == nil {
		t.Fatal("a permission change against a store that cannot apply one was reported as a success")
	}
}

// countingMaintenance counts the calls that rewrite access control lists.
type countingMaintenance struct {
	*memstore.Store
	calls int
}

func (c *countingMaintenance) SetPermissions(ctx context.Context, tenant string, perms map[string]acl.Permissions) (int, error) {
	c.calls++
	return c.Store.SetPermissions(ctx, tenant, perms)
}

// plainStore hides the optional capabilities of the driver underneath, so that
// a test can see what the pipeline does with a store that only stores.
type plainStore struct{ inner *memstore.Store }

func (p plainStore) Put(ctx context.Context, docs ...doc.Document) error {
	return p.inner.Put(ctx, docs...)
}
func (p plainStore) Delete(ctx context.Context, ids ...string) error {
	return p.inner.Delete(ctx, ids...)
}
func (p plainStore) Get(ctx context.Context, pr *acl.Principal, id string) (doc.Document, error) {
	return p.inner.Get(ctx, pr, id)
}
func (p plainStore) Scan(ctx context.Context, pr *acl.Principal, fn func(doc.Document) bool) error {
	return p.inner.Scan(ctx, pr, fn)
}
func (p plainStore) Stats(ctx context.Context) (store.Stats, error) { return p.inner.Stats(ctx) }
func (p plainStore) Close() error                                   { return p.inner.Close() }
