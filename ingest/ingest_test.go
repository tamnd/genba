package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/ingest"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
)

const tenant = "acme"

// fakeSource emits a fixed list of changes, and records where it was asked to
// resume from so that a test can assert the resume actually happened.
type fakeSource struct {
	name      string
	changes   []connector.Change
	resumedAt connector.Cursor
	emitted   int
	final     connector.Cursor
}

func (f *fakeSource) Source() string { return f.name }
func (f *fakeSource) Close() error   { return nil }

func (f *fakeSource) Sync(ctx context.Context, from connector.Cursor, emit func(context.Context, connector.Change) error) (connector.Cursor, error) {
	f.resumedAt = from
	f.emitted = 0
	for _, ch := range f.changes {
		// A cursor is a resume point, so a resumed run must not re emit what
		// the cursor already covers. A real connector does this by seeking.
		if !from.IsZero() && ch.Cursor.Value <= from.Value {
			continue
		}
		f.emitted++
		if err := emit(ctx, ch); err != nil {
			return connector.Cursor{}, err
		}
	}
	return f.final, nil
}

// changes builds n documents that everybody in the tenant may read.
func changes(n int) []connector.Change {
	out := make([]connector.Change, 0, n)
	for i := range n {
		out = append(out, connector.Change{
			Document: doc.Document{
				ID:    fmt.Sprintf("doc-%04d", i),
				Title: fmt.Sprintf("document number %d", i),
				Body:  strings.Repeat("content ", 10),
				Permissions: acl.Permissions{
					Mode:   acl.ModePublicToTenant,
					Source: "test",
				},
			},
			// Cursors are zero padded so that the string comparison a resume
			// does matches the numeric order they were produced in.
			Cursor: connector.Cursor{Value: fmt.Sprintf("%04d", i)},
		})
	}
	return out
}

func principal() *acl.Principal {
	return &acl.Principal{Tenant: tenant, Subject: "reader", Kind: acl.KindUser}
}

func run(t *testing.T, p *ingest.Pipeline, src connector.Connector) ingest.Stats {
	t.Helper()
	st, err := p.Run(t.Context(), tenant, src)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return st
}

func TestDocumentsBecomeReadable(t *testing.T) {
	st := memstore.New()
	p, err := ingest.New(st, connector.NewMemoryCheckpoints())
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	stats := run(t, p, &fakeSource{name: "test", changes: changes(25)})

	if stats.Indexed != 25 {
		t.Errorf("indexed %d documents, want 25", stats.Indexed)
	}
	if stats.Quarantined != 0 {
		t.Errorf("quarantined %d documents, want none", stats.Quarantined)
	}

	got, err := st.Get(t.Context(), principal(), "doc-0007")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "document number 7" {
		t.Errorf("title is %q", got.Title)
	}
	if got.Tenant != tenant {
		t.Errorf("tenant is %q, want %q", got.Tenant, tenant)
	}
	if got.Source != "test" {
		t.Errorf("source is %q, want the connector name", got.Source)
	}
	if got.IndexedAt.IsZero() {
		t.Error("IndexedAt was not stamped")
	}
}

// A connector that lies about its tenant must not be able to write into
// somebody else's corpus.
func TestTheTenantComesFromTheCallerNotTheConnector(t *testing.T) {
	st := memstore.New()
	p, _ := ingest.New(st, nil)

	ch := changes(1)
	ch[0].Document.Tenant = "someone-else"
	run(t, p, &fakeSource{name: "test", changes: ch})

	if _, err := st.Get(t.Context(), principal(), "doc-0000"); err != nil {
		t.Fatalf("the document should belong to the caller's tenant: %v", err)
	}
	other := &acl.Principal{Tenant: "someone-else", Subject: "reader", Kind: acl.KindUser}
	if _, err := st.Get(t.Context(), other, "doc-0000"); !errors.Is(err, genba.ErrNotFound) {
		t.Errorf("the other tenant can see the document, error was %v", err)
	}
}

func TestUnresolvedPermissionsAreQuarantinedNotIndexed(t *testing.T) {
	st := memstore.New()
	p, _ := ingest.New(st, nil)

	ch := changes(10)
	for i := range ch {
		if i%2 == 0 {
			ch[i].Document.Permissions = connector.Unresolved("test")
		}
	}
	stats := run(t, p, &fakeSource{name: "test", changes: ch})

	if stats.Indexed != 5 || stats.Quarantined != 5 {
		t.Errorf("indexed %d and quarantined %d, want 5 and 5", stats.Indexed, stats.Quarantined)
	}

	// Counted by the store as quarantined, which is how an operator finds them.
	got, err := st.Stats(t.Context())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.Documents != 5 || got.Quarantined != 5 {
		t.Errorf("store reports %+v, want 5 documents and 5 quarantined", got)
	}

	// And not readable by anybody, which is the part that matters.
	if _, err := st.Get(t.Context(), principal(), "doc-0000"); !errors.Is(err, genba.ErrNotFound) {
		t.Errorf("a quarantined document was served, error was %v", err)
	}
	seen := 0
	err = st.Scan(t.Context(), principal(), func(doc.Document) bool { seen++; return true })
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if seen != 5 {
		t.Errorf("scan yielded %d documents, want only the 5 that resolved", seen)
	}
}

func TestDeletesRemoveDocuments(t *testing.T) {
	st := memstore.New()
	p, _ := ingest.New(st, nil)
	run(t, p, &fakeSource{name: "test", changes: changes(5)})

	gone := []connector.Change{{
		Document: doc.Document{ID: "doc-0002"},
		Deleted:  true,
		Cursor:   connector.Cursor{Value: "9999"},
	}}
	stats := run(t, p, &fakeSource{name: "test", changes: gone})
	if stats.Deleted != 1 {
		t.Errorf("deleted %d, want 1", stats.Deleted)
	}
	if _, err := st.Get(t.Context(), principal(), "doc-0002"); !errors.Is(err, genba.ErrNotFound) {
		t.Errorf("the deleted document is still readable, error was %v", err)
	}
	if _, err := st.Get(t.Context(), principal(), "doc-0003"); err != nil {
		t.Errorf("a document that was not deleted went missing: %v", err)
	}
}

func TestAChangeWithNoIdIsSkippedRatherThanStored(t *testing.T) {
	st := memstore.New()
	p, _ := ingest.New(st, nil)

	ch := changes(3)
	ch[1].Document.ID = ""
	stats := run(t, p, &fakeSource{name: "test", changes: ch})

	if stats.Skipped != 1 {
		t.Errorf("skipped %d, want 1", stats.Skipped)
	}
	if stats.Indexed != 2 {
		t.Errorf("indexed %d, want 2", stats.Indexed)
	}
}

func TestAnEmptyTenantIsRefused(t *testing.T) {
	p, _ := ingest.New(memstore.New(), nil)
	if _, err := p.Run(t.Context(), "", &fakeSource{name: "test"}); err == nil {
		t.Fatal("a run with no tenant was accepted")
	}
}

func TestBatchingWritesFewerTimesThanThereAreDocuments(t *testing.T) {
	counter := &countingStore{Store: memstore.New()}
	p, _ := ingest.New(counter, nil, ingest.WithBatchSize(10))
	stats := run(t, p, &fakeSource{name: "test", changes: changes(95)})

	// 95 documents in batches of 10 is nine full batches plus the final flush.
	if stats.Batches != 10 {
		t.Errorf("made %d batches, want 10", stats.Batches)
	}
	if counter.puts != 10 {
		t.Errorf("called Put %d times, want 10", counter.puts)
	}
}

// This is the one that matters. A sync of a real corpus will be interrupted, so
// the pipeline is killed after every possible number of store writes and the
// corpus is checked for completeness after the resume.
//
// It also pins down the ordering rule. Documents are stored and only then is the
// checkpoint saved, so a crash between the two replays a batch. Replaying is
// harmless because a put of the same document twice is the same as once. The
// opposite order would move the cursor past documents that were never stored,
// and nothing downstream would ever discover they were missing.
func TestKillingTheRunAtAnyPointLosesNothing(t *testing.T) {
	const total = 137

	for kill := range 20 {
		t.Run(fmt.Sprintf("after_%d_writes", kill), func(t *testing.T) {
			backing := memstore.New()
			checkpoints := connector.NewMemoryCheckpoints()

			// First run, killed after `kill` store writes.
			dying := &countingStore{Store: backing, failAfter: kill}
			p, _ := ingest.New(dying, checkpoints, ingest.WithBatchSize(7))
			_, err := p.Run(t.Context(), tenant, &fakeSource{name: "test", changes: changes(total)})
			if kill > 0 && err == nil {
				t.Fatal("the store was supposed to fail and the run reported success")
			}

			// Second run against the same store and the same checkpoints, which
			// is what a restarted process does.
			healthy := &countingStore{Store: backing}
			p2, _ := ingest.New(healthy, checkpoints, ingest.WithBatchSize(7))
			src := &fakeSource{name: "test", changes: changes(total)}
			if _, err := p2.Run(t.Context(), tenant, src); err != nil {
				t.Fatalf("resume: %v", err)
			}

			// Every document is present exactly once, whatever happened.
			seen := map[string]int{}
			err = backing.Scan(t.Context(), principal(), func(d doc.Document) bool {
				seen[d.ID]++
				return true
			})
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(seen) != total {
				t.Fatalf("corpus holds %d documents after the resume, want %d", len(seen), total)
			}
			for id, n := range seen {
				if n != 1 {
					t.Errorf("%s appears %d times", id, n)
				}
			}

			// And the resume was a resume rather than a restart, once the first
			// run got far enough to checkpoint anything.
			if kill >= 2 && src.resumedAt.IsZero() {
				t.Error("the second run started from the beginning instead of the checkpoint")
			}
			if kill >= 2 && src.emitted >= total {
				t.Errorf("the second run re emitted all %d documents, so nothing was saved", total)
			}
		})
	}
}

// The checkpoint must never be ahead of what is durably stored, because a
// cursor past an unstored document is a document nobody will ever look for
// again.
func TestTheCheckpointIsNeverAheadOfTheStore(t *testing.T) {
	backing := memstore.New()
	checkpoints := connector.NewMemoryCheckpoints()
	watcher := &checkpointWatcher{Checkpoints: checkpoints, store: backing, t: t}

	p, _ := ingest.New(backing, watcher, ingest.WithBatchSize(9))
	run(t, p, &fakeSource{name: "test", changes: changes(100)})

	if watcher.saves == 0 {
		t.Fatal("no checkpoint was ever saved")
	}
}

func TestAConnectorCursorSurvivesToTheNextRun(t *testing.T) {
	checkpoints := connector.NewMemoryCheckpoints()
	p, _ := ingest.New(memstore.New(), checkpoints)

	first := &fakeSource{name: "test", changes: changes(4), final: connector.Cursor{Value: "0003"}}
	stats := run(t, p, first)
	if stats.Cursor.Value != "0003" {
		t.Errorf("run ended at cursor %q, want the connector's own end of walk cursor", stats.Cursor.Value)
	}

	second := &fakeSource{name: "test", changes: changes(4)}
	run(t, p, second)
	if second.resumedAt.Value != "0003" {
		t.Errorf("second run resumed at %q, want 0003", second.resumedAt.Value)
	}
	if second.emitted != 0 {
		t.Errorf("second run re emitted %d documents that the cursor already covered", second.emitted)
	}
}

func TestRateIsZeroForARunTooShortToMeasure(t *testing.T) {
	if got := (ingest.Stats{Indexed: 10}).Rate(); got != 0 {
		t.Errorf("rate of a zero duration run is %v, want 0", got)
	}
	s := ingest.Stats{Indexed: 100, Quarantined: 100, Duration: 2 * time.Second}
	if got := s.Rate(); got != 100 {
		t.Errorf("rate is %v, want 100", got)
	}
}

func TestANilStoreIsRefused(t *testing.T) {
	if _, err := ingest.New(nil, nil); err == nil {
		t.Fatal("a pipeline was built with no store")
	}
}

// countingStore counts writes and can be told to start failing, which is how a
// process being killed is simulated without killing the test process.
type countingStore struct {
	store.Store
	puts      int
	failAfter int
	failing   bool
}

var errKilled = errors.New("store went away")

func (c *countingStore) Put(ctx context.Context, docs ...doc.Document) error {
	if c.failAfter > 0 && c.puts >= c.failAfter {
		c.failing = true
		return errKilled
	}
	c.puts++
	return c.Store.Put(ctx, docs...)
}

func (c *countingStore) Delete(ctx context.Context, ids ...string) error {
	if c.failing {
		return errKilled
	}
	return c.Store.Delete(ctx, ids...)
}

// checkpointWatcher asserts, on every save, that everything the cursor claims
// is covered is actually in the store.
type checkpointWatcher struct {
	connector.Checkpoints
	store *memstore.Store
	t     *testing.T
	saves int
}

func (w *checkpointWatcher) Save(ctx context.Context, tenant, source string, c connector.Cursor) error {
	w.t.Helper()
	w.saves++

	// The cursors in this test are the document ordinals, so everything up to
	// and including the cursor must already be readable.
	var want int
	if _, err := fmt.Sscanf(c.Value, "%04d", &want); err == nil {
		for i := 0; i <= want; i++ {
			id := fmt.Sprintf("doc-%04d", i)
			if _, err := w.store.Get(ctx, principal(), id); err != nil {
				w.t.Errorf("checkpoint moved to %q but %s is not in the store yet", c.Value, id)
			}
		}
	}
	return w.Checkpoints.Save(ctx, tenant, source, c)
}
