package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/ingest"
	"github.com/tamnd/genba/store/memstore"
)

// listedSource is a source that can be listed and fetched, and counts what it
// was asked for.
//
// It stands in for the thing the interfaces are really about: a system where
// listing is cheap and fetching is not. Nothing here would be visible on a
// local filesystem, where both are fast, and everything here is the difference
// between a sync that finishes and one that gets rate limited.
type listedSource struct {
	name string

	// held is the corpus, keyed by id, in the order a walk would find it.
	held  map[string]doc.Document
	order []string

	// versions overrides what the enumeration reports, so that a test can make
	// the source claim a revision the index does not have without changing the
	// document behind it.
	versions map[string]string

	// enumErr fails the walk partway through, after emitting this many items.
	enumErr   error
	enumAfter int

	counters connector.Counters
	fetched  []string
}

func (s *listedSource) Source() string { return s.name }
func (s *listedSource) Close() error   { return nil }

// Sync emits nothing. These tests are about the sweep that runs beside the
// incremental path, so the incremental path is left empty on purpose.
func (s *listedSource) Sync(context.Context, connector.Cursor, func(context.Context, connector.Change) error) (connector.Cursor, error) {
	return connector.Cursor{}, nil
}

func (s *listedSource) Enumerate(_ context.Context, fn func(connector.Item) bool) error {
	s.counters.Lists++
	for i, id := range s.order {
		if s.enumErr != nil && i == s.enumAfter {
			return s.enumErr
		}
		version := s.held[id].SourceUpdate
		if v, ok := s.versions[id]; ok {
			version = v
		}
		if !fn(connector.Item{ID: id, Version: version}) {
			return nil
		}
	}
	return nil
}

func (s *listedSource) Fetch(_ context.Context, id string) (doc.Document, error) {
	s.counters.Fetches++
	s.fetched = append(s.fetched, id)
	d, ok := s.held[id]
	if !ok {
		return doc.Document{}, connector.ErrGone
	}
	s.counters.Bytes += int64(len(d.Body))
	return d, nil
}

func (s *listedSource) Counters() connector.Counters { return s.counters }

func (s *listedSource) put(id, version, body string) {
	if s.held == nil {
		s.held = make(map[string]doc.Document)
	}
	if _, ok := s.held[id]; !ok {
		s.order = append(s.order, id)
	}
	s.held[id] = doc.Document{
		ID:           id,
		Title:        "document " + id,
		Body:         body,
		SourceUpdate: version,
		Permissions:  acl.Permissions{Mode: acl.ModePublicToTenant, Source: s.name},
	}
}

func (s *listedSource) remove(id string) {
	delete(s.held, id)
	s.order = slices.DeleteFunc(s.order, func(x string) bool { return x == id })
}

// unresolved makes the source hand back a document whose permissions did not
// resolve, which is the document the index stores and holds back. It leaves the
// revision alone, because that is the whole point: nothing about the document
// changed, only what the source could say about who may read it.
func (s *listedSource) unresolved(id, reason string) {
	d := s.held[id]
	d.Permissions = connector.Unresolved(s.name, reason)
	s.held[id] = d
}

// resolved is the fix, whatever it was: the directory came back up, the token
// was given the scope it was missing, the group now exists.
func (s *listedSource) resolved(id string) {
	d := s.held[id]
	d.Permissions = acl.Permissions{Mode: acl.ModePublicToTenant, Source: s.name}
	s.held[id] = d
}

// corpus builds a source holding n documents at revision one.
func corpus(name string, n int) *listedSource {
	s := &listedSource{name: name}
	for i := range n {
		s.put(fmt.Sprintf("doc-%04d", i), "v1", "the body of document "+strconv.Itoa(i))
	}
	return s
}

func reconcile(t *testing.T, p *ingest.Pipeline, src connector.Connector) ingest.Reconciliation {
	t.Helper()
	rec, err := p.Reconcile(t.Context(), tenant, src)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return rec
}

// pipelineOver returns a pipeline over a fresh store, and the store.
func pipelineOver(t *testing.T) (*ingest.Pipeline, *memstore.Store) {
	t.Helper()
	st := memstore.New()
	p, err := ingest.New(st, connector.NewMemoryCheckpoints())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return p, st
}

// TestReconcileFillsAnEmptyIndex is the simplest reading of the comparison: an
// index that has never seen the source is entirely missing, and the sweep is
// what a first sync would have been.
func TestReconcileFillsAnEmptyIndex(t *testing.T) {
	p, st := pipelineOver(t)
	src := corpus("test", 12)

	rec := reconcile(t, p, src)

	if rec.SourceItems != 12 || rec.IndexItems != 0 {
		t.Fatalf("compared %d source items against %d index items, want 12 against 0", rec.SourceItems, rec.IndexItems)
	}
	if rec.Missing.Count != 12 || rec.Repaired != 12 {
		t.Fatalf("found %d missing and repaired %d, want 12 and 12", rec.Missing.Count, rec.Repaired)
	}
	if rec.Extra.Count != 0 || rec.Stale.Count != 0 {
		t.Fatalf("an empty index reported %d extra and %d stale", rec.Extra.Count, rec.Stale.Count)
	}
	if _, err := st.Get(t.Context(), principal(), "doc-0003"); err != nil {
		t.Fatalf("a repaired document is not readable: %v", err)
	}
}

// TestReconcileIsQuietWhenNothingDrifted is the case that has to be cheap,
// because it is the case that happens every night for the life of a deployment.
func TestReconcileIsQuietWhenNothingDrifted(t *testing.T) {
	p, _ := pipelineOver(t)
	src := corpus("test", 20)
	reconcile(t, p, src)

	before := src.Counters()
	rec := reconcile(t, p, src)

	if rec.Drift() != 0 {
		t.Fatalf("a second sweep over an unchanged corpus found %d discrepancies", rec.Drift())
	}
	if rec.Repaired != 0 || rec.Deleted != 0 {
		t.Fatalf("a sweep over an unchanged corpus repaired %d and deleted %d", rec.Repaired, rec.Deleted)
	}
	if fetches := src.Counters().Fetches - before.Fetches; fetches != 0 {
		t.Fatalf("a sweep over an unchanged corpus fetched %d documents, want none", fetches)
	}
	if rec.Requests.Lists != 1 {
		t.Fatalf("the sweep made %d enumerations, want 1", rec.Requests.Lists)
	}
	if rec.Held.Count != 0 || rec.Released != 0 {
		t.Fatalf("a sweep over an index holding nothing back reported %d held and %d released", rec.Held.Count, rec.Released)
	}
}

// TestReconcileReleasesWhatTheSourceCanNowResolve is the automatic retry. A
// document held back because its permissions did not resolve is refetched
// whatever its revision says, because the reason it is held is at the source and
// the fix does not touch the document.
func TestReconcileReleasesWhatTheSourceCanNowResolve(t *testing.T) {
	p, st := pipelineOver(t)
	src := corpus("test", 4)
	src.unresolved("doc-0001", "the directory was unreachable")
	reconcile(t, p, src)

	if _, err := st.Get(t.Context(), principal(), "doc-0001"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("a document whose permissions did not resolve is readable: %v", err)
	}

	src.resolved("doc-0001")
	src.fetched = nil
	rec := reconcile(t, p, src)

	if rec.Held.Count != 1 || !slices.Equal(rec.Held.IDs, []string{"doc-0001"}) {
		t.Fatalf("the sweep reported %d held documents, ids %v, want doc-0001", rec.Held.Count, rec.Held.IDs)
	}
	if rec.Drift() != 0 {
		t.Fatalf("a held document was counted as drift: %d discrepancies", rec.Drift())
	}
	if rec.Released != 1 || rec.Repaired != 1 {
		t.Fatalf("the sweep released %d and repaired %d, want 1 and 1", rec.Released, rec.Repaired)
	}
	if !slices.Equal(src.fetched, []string{"doc-0001"}) {
		t.Fatalf("the sweep fetched %v, want only the held document", src.fetched)
	}
	if _, err := st.Get(t.Context(), principal(), "doc-0001"); err != nil {
		t.Fatalf("a released document is still not readable: %v", err)
	}
}

// TestReconcileKeepsHoldingWhatStillDoesNotResolve is the other half, and it is
// the half that says the retry is safe to run every night. A refetch that comes
// back unresolved again leaves the document exactly where it was, and the
// released count says so.
func TestReconcileKeepsHoldingWhatStillDoesNotResolve(t *testing.T) {
	p, st := pipelineOver(t)
	src := corpus("test", 3)
	src.unresolved("doc-0002", "the group could not be listed")
	reconcile(t, p, src)

	src.fetched = nil
	rec := reconcile(t, p, src)

	if rec.Held.Count != 1 {
		t.Fatalf("the sweep reported %d held documents, want 1", rec.Held.Count)
	}
	if rec.Released != 0 {
		t.Fatalf("the sweep released %d documents whose permissions still do not resolve", rec.Released)
	}
	if !slices.Equal(src.fetched, []string{"doc-0002"}) {
		t.Fatalf("the sweep fetched %v, want the held document retried", src.fetched)
	}
	if _, err := st.Get(t.Context(), principal(), "doc-0002"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("a retry that failed made the document readable: %v", err)
	}
}

// TestReconcileDeletesAHeldDocumentTheSourceRemoved. A document that is held and
// has also gone from the source is not a retry, it is a deletion, and fetching
// it would be a request that can only fail.
func TestReconcileDeletesAHeldDocumentTheSourceRemoved(t *testing.T) {
	p, st := pipelineOver(t)
	src := corpus("test", 3)
	src.unresolved("doc-0001", "the directory was unreachable")
	reconcile(t, p, src)

	src.remove("doc-0001")
	src.fetched = nil
	rec := reconcile(t, p, src)

	if rec.Held.Count != 0 {
		t.Fatalf("a document the source no longer holds was reported as held")
	}
	if rec.Extra.Count != 1 || rec.Deleted != 1 {
		t.Fatalf("the sweep reported %d extra and deleted %d, want 1 and 1", rec.Extra.Count, rec.Deleted)
	}
	if len(src.fetched) != 0 {
		t.Fatalf("the sweep fetched %v, want nothing fetched for a deleted document", src.fetched)
	}
	if _, err := st.Get(t.Context(), principal(), "doc-0001"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("a held document deleted at the source is still in the index: %v", err)
	}
}

// TestReconcileFetchesAHeldStaleDocumentOnce. Both reasons to refetch apply to
// the same document, and a sweep that acted on both would spend two requests to
// store the same bytes twice.
func TestReconcileFetchesAHeldStaleDocumentOnce(t *testing.T) {
	p, _ := pipelineOver(t)
	src := corpus("test", 3)
	src.unresolved("doc-0001", "the directory was unreachable")
	reconcile(t, p, src)

	// A new revision, which also carries permissions that resolve.
	src.put("doc-0001", "v2", "the body, rewritten")
	src.fetched = nil
	rec := reconcile(t, p, src)

	if rec.Stale.Count != 1 || rec.Held.Count != 0 {
		t.Fatalf("the sweep reported %d stale and %d held, want the document counted once as stale", rec.Stale.Count, rec.Held.Count)
	}
	if !slices.Equal(src.fetched, []string{"doc-0001"}) {
		t.Fatalf("the sweep fetched %v, want one fetch of doc-0001", src.fetched)
	}
}

// TestReconcileRemovesWhatTheSourceDeleted is the box this was written for. A
// document deleted at the source is still being served until something notices,
// and nothing in a change feed has to say so.
func TestReconcileRemovesWhatTheSourceDeleted(t *testing.T) {
	p, st := pipelineOver(t)
	src := corpus("test", 6)
	reconcile(t, p, src)

	src.remove("doc-0002")
	rec := reconcile(t, p, src)

	if rec.Extra.Count != 1 || !slices.Equal(rec.Extra.IDs, []string{"doc-0002"}) {
		t.Fatalf("the sweep reported %d extra documents, ids %v, want doc-0002", rec.Extra.Count, rec.Extra.IDs)
	}
	if rec.Deleted != 1 {
		t.Fatalf("the sweep deleted %d documents, want 1", rec.Deleted)
	}
	if _, err := st.Get(t.Context(), principal(), "doc-0002"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("a document deleted at the source is still readable: %v", err)
	}

	// And the sweep after that finds nothing, which is what says the repair was
	// a repair and not a loop that deletes and refetches the same document
	// every night.
	if again := reconcile(t, p, src); again.Drift() != 0 {
		t.Fatalf("the sweep after the repair found %d discrepancies", again.Drift())
	}
}

// TestReconcileRefetchesWhatMovedOn covers the revision comparison, which is
// the part that catches a source that changed a document without saying so.
func TestReconcileRefetchesWhatMovedOn(t *testing.T) {
	p, st := pipelineOver(t)
	src := corpus("test", 5)
	reconcile(t, p, src)

	src.fetched = nil
	src.put("doc-0001", "v2", "the body, rewritten")
	rec := reconcile(t, p, src)

	if rec.Stale.Count != 1 || !slices.Equal(rec.Stale.IDs, []string{"doc-0001"}) {
		t.Fatalf("the sweep reported %d stale documents, ids %v, want doc-0001", rec.Stale.Count, rec.Stale.IDs)
	}
	if rec.Repaired != 1 {
		t.Fatalf("the sweep repaired %d documents, want 1", rec.Repaired)
	}
	if !slices.Equal(src.fetched, []string{"doc-0001"}) {
		t.Fatalf("the sweep fetched %v, want only the stale document", src.fetched)
	}
	got, err := st.Get(t.Context(), principal(), "doc-0001")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Body != "the body, rewritten" {
		t.Fatalf("the repaired document still has the old body %q", got.Body)
	}
}

// TestReconcileDeletesNothingOnAPartialEnumeration is the rule that matters
// most in this file. A walk that failed halfway is not a statement about what
// the source holds, and acting on it as though it were empties the index over a
// timeout.
func TestReconcileDeletesNothingOnAPartialEnumeration(t *testing.T) {
	p, st := pipelineOver(t)
	src := corpus("test", 10)
	reconcile(t, p, src)

	src.enumErr = errors.New("the source hung up")
	src.enumAfter = 3

	rec, err := p.Reconcile(t.Context(), tenant, src)
	if err == nil {
		t.Fatal("a failed enumeration was reported as a successful sweep")
	}
	if rec.Deleted != 0 {
		t.Fatalf("a failed enumeration deleted %d documents", rec.Deleted)
	}

	var count int
	if err := st.Scan(t.Context(), principal(), func(doc.Document) bool {
		count++
		return true
	}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if count != 10 {
		t.Fatalf("the index holds %d documents after a failed sweep, want all 10", count)
	}
}

// TestReconcileTreatsAVanishedFetchAsADeletion covers the race a corpus people
// are working in produces every day: the document was there when the source was
// listed and is gone by the time it is fetched.
func TestReconcileTreatsAVanishedFetchAsADeletion(t *testing.T) {
	p, st := pipelineOver(t)
	src := corpus("test", 3)

	// Listed but not held, which is exactly what a delete between the two
	// calls looks like from here.
	src.order = append(src.order, "doc-ghost")

	rec := reconcile(t, p, src)

	if rec.Missing.Count != 4 {
		t.Fatalf("the sweep reported %d missing documents, want 4", rec.Missing.Count)
	}
	if rec.Repaired != 3 || rec.Deleted != 1 {
		t.Fatalf("the sweep repaired %d and deleted %d, want 3 and 1", rec.Repaired, rec.Deleted)
	}
	if _, err := st.Get(t.Context(), principal(), "doc-ghost"); !errors.Is(err, genba.ErrNotFound) {
		t.Fatalf("a document that vanished between the list and the fetch is readable: %v", err)
	}
}

// TestReconcileSamplesRatherThanListsEverything keeps the report a report. A
// first sweep over a large corpus finds all of it missing, and a structure that
// named every id would be a second copy of the corpus.
func TestReconcileSamplesRatherThanListsEverything(t *testing.T) {
	p, _ := pipelineOver(t)
	src := corpus("test", ingest.SampleSize*3)

	rec := reconcile(t, p, src)

	if rec.Missing.Count != ingest.SampleSize*3 {
		t.Fatalf("the count is %d, want the exact number missing", rec.Missing.Count)
	}
	if len(rec.Missing.IDs) != ingest.SampleSize {
		t.Fatalf("the sample holds %d ids, want %d", len(rec.Missing.IDs), ingest.SampleSize)
	}
	if rec.Missing.IDs[0] != "doc-0000" {
		t.Fatalf("the sample starts at %q, want the first id in sorted order", rec.Missing.IDs[0])
	}
}

// TestReconcileNeedsAnEnumerator says no rather than doing something expensive
// and calling it a sweep.
func TestReconcileNeedsAnEnumerator(t *testing.T) {
	p, _ := pipelineOver(t)
	if _, err := p.Reconcile(t.Context(), tenant, &fakeSource{name: "test"}); err == nil {
		t.Fatal("reconciling a connector that cannot enumerate was allowed")
	}
}

func TestReconcileRefusesAnEmptyTenant(t *testing.T) {
	p, _ := pipelineOver(t)
	if _, err := p.Reconcile(t.Context(), "", corpus("test", 1)); err == nil {
		t.Fatal("reconciling with no tenant was allowed")
	}
}
