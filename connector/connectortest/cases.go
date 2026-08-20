package connectortest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
)

// The documents the suite works with. They are named so that the order they
// sort in is the order they were written in, because a connector that walks its
// source in name order and one that walks it in modification order then agree,
// and a case about resuming can say what came after what.
const (
	first  = "a.md"
	second = "b.md"
	third  = "c.md"
	fourth = "d.md"
)

// The marker in each body, which is what a case looks for rather than comparing
// the whole text. A connector is allowed to do work on the way past.
const (
	firstText  = "the first document is about gearboxes"
	secondText = "the second document is about bearings"
	thirdText  = "the third document is about hydraulics"
	fourthText = "the fourth document is about castings"
)

// The source name is what a resume point is filed under, so it has to be there
// and it has to stay the same.
func testSourceName(t *testing.T, f Fixture) {
	name := f.Connector.Source()
	if name == "" {
		t.Fatal("the connector has no source name, and the resume point for a source is filed under that name")
	}
	if again := f.Connector.Source(); again != name {
		t.Fatalf("the source name changed from %q to %q between two calls, so a restart would resume from nothing", name, again)
	}
	if strings.TrimSpace(name) != name {
		t.Errorf("the source name %q has whitespace around it, which will not survive a round trip through a query filter", name)
	}
}

// A full sync is the claim a connector makes about what its source holds. If it
// is short by one document, nothing else in the system will ever notice.
func testFullSync(t *testing.T, f Fixture) {
	write(t, f, first, firstText)
	write(t, f, second, secondText)
	write(t, f, third, thirdText)

	changes, next := syncFrom(t, f, connector.Cursor{})
	want := []string{f.ID(first), f.ID(second), f.ID(third)}
	slices.Sort(want)
	if got := sorted(changes); !slices.Equal(got, want) {
		t.Fatalf("a full sync of three documents emitted %v, want %v", got, want)
	}
	if next.IsZero() {
		t.Error("a sync that found three documents came back with no cursor, so the next run starts from the beginning again")
	}
	for _, ch := range changes {
		if ch.Deleted || ch.PermissionsOnly {
			t.Errorf("a full sync of a source nothing has been removed from emitted %s as a deletion or a permission change", ch.Document.ID)
		}
	}
	if ch := find(changes, f.ID(second)); ch != nil && !mentions(ch.Document.Body, "bearings") {
		t.Errorf("%s came back with body %q, which does not contain what was written to it", ch.Document.ID, ch.Document.Body)
	}
}

// A sync from the beginning is a statement about the source rather than about
// what the connector remembers, so two of them say the same thing.
func testFullSyncRepeats(t *testing.T, f Fixture) {
	write(t, f, first, firstText)
	write(t, f, second, secondText)

	once, _ := syncFrom(t, f, connector.Cursor{})
	twice, _ := syncFrom(t, f, connector.Cursor{})
	if got, want := sorted(twice), sorted(once); !slices.Equal(got, want) {
		t.Fatalf("two full syncs of the same source emitted %v and %v", want, got)
	}
	for _, ch := range once {
		other := find(twice, ch.Document.ID)
		if other == nil {
			continue
		}
		if other.Document.Body != ch.Document.Body {
			t.Errorf("%s came back with a different body on the second full sync", ch.Document.ID)
		}
		if !samePermissions(other.Document.Permissions, ch.Document.Permissions) {
			t.Errorf("%s came back with different permissions on the second full sync", ch.Document.ID)
		}
	}
}

// Every document says who may read it. This is the one rule with no way out,
// and it is checked on its own as well as on every change the suite sees so
// that a connector which forgot fails a case that says so by name.
func testPermissions(t *testing.T, f Fixture) {
	write(t, f, first, firstText)
	write(t, f, second, secondText)

	changes, _ := syncFrom(t, f, connector.Cursor{})
	if len(changes) == 0 {
		t.Fatal("a sync of a source with two documents in it emitted nothing")
	}
	for _, ch := range changes {
		if ch.Deleted {
			continue
		}
		if ch.Document.Permissions.Source == "" {
			t.Errorf("%s was indexed without permissions, and a document indexed without the access control list that governs it is readable by everybody", ch.Document.ID)
		}
	}
}

// A document whose access control list could not be worked out is quarantined
// rather than guessed at, and the rest of the sync carries on.
//
// The two halves matter equally. Publishing the document is the failure nobody
// recovers from, and dropping the whole sync because one share would not resolve
// is how an index quietly stops being complete.
func testUnresolved(t *testing.T, f Fixture) {
	if f.Unresolvable == nil {
		t.Skip("the fixture cannot put a document's permissions beyond working out")
	}
	write(t, f, first, firstText)
	write(t, f, second, secondText)
	f.Unresolvable(t, second)

	changes, _ := syncFrom(t, f, connector.Cursor{})
	ch := find(changes, f.ID(second))
	if ch == nil {
		t.Fatalf("%s had permissions that would not resolve and was dropped from the sync, so nothing above the connector knows it exists", f.ID(second))
	}
	if got := ch.Document.Permissions.Mode; got != acl.ModeUnknown {
		t.Errorf("%s had permissions that would not resolve and was indexed with mode %v, and a connector that cannot answer must not guess", ch.Document.ID, got)
	}
	if find(changes, f.ID(first)) == nil {
		t.Errorf("one document whose permissions would not resolve cost the sync %s as well", f.ID(first))
	}
}

// A sync of a real corpus takes long enough that it will be interrupted, so
// resuming from a change's cursor has to pick up everything that came after it.
//
// Losing a document here is invisible: the run reports success, the cursor moves
// on, and the document is missing until somebody searches for it.
func testResume(t *testing.T, f Fixture) {
	write(t, f, first, firstText)
	write(t, f, second, secondText)
	write(t, f, third, thirdText)
	write(t, f, fourth, fourthText)

	changes, _ := syncFrom(t, f, connector.Cursor{})
	if len(changes) < 4 {
		t.Fatalf("a full sync of four documents emitted %v", ids(changes))
	}

	at := -1
	for i := range changes[:len(changes)-1] {
		if !changes[i].Cursor.IsZero() {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatal("no change carried a cursor, so a run interrupted part of the way through has to start again from the beginning")
	}

	rest, _ := syncFrom(t, f, changes[at].Cursor)
	seen := sorted(rest)
	for _, ch := range changes[at+1:] {
		if !slices.Contains(seen, ch.Document.ID) {
			t.Errorf("resuming from the cursor of %s lost %s, which the same sync emitted after it", changes[at].Document.ID, ch.Document.ID)
		}
	}
}

// An error from emit is the index refusing the document, and a connector that
// swallowed it would report a successful sync of a corpus that was not stored.
func testEmitError(t *testing.T, f Fixture) {
	write(t, f, first, firstText)
	write(t, f, second, secondText)
	write(t, f, third, thirdText)

	refused := errors.New("the store would not take it")
	var n int
	_, err := f.Connector.Sync(t.Context(), connector.Cursor{}, func(_ context.Context, _ connector.Change) error {
		n++
		if n == 2 {
			return refused
		}
		return nil
	})
	if !errors.Is(err, refused) {
		t.Fatalf("sync returned %v, want the error emit returned", err)
	}
	if n != 2 {
		t.Errorf("sync emitted %d changes, want it to stop at the one that was refused", n)
	}
}

// A shutdown stops the crawl. A sync that ran to the end after the context was
// cancelled is one that spends a source's quota on documents nobody is going to
// store, and on a large corpus it is the difference between a restart taking a
// second and taking an hour.
func testCancel(t *testing.T, f Fixture) {
	write(t, f, first, firstText)
	write(t, f, second, secondText)
	write(t, f, third, thirdText)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var n int
	_, err := f.Connector.Sync(ctx, connector.Cursor{}, func(_ context.Context, _ connector.Change) error {
		n++
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("a sync cancelled at its first change ran to the end and reported success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled sync returned %v, want context.Canceled", err)
	}
	if n >= 3 {
		t.Errorf("a sync cancelled at its first change went on to emit all %d of them", n)
	}
}

// The second sync of a source nothing changed in reads nothing. This is the
// whole claim of an incremental sync, and the cursor is the only thing holding
// it up.
func testIncremental(t *testing.T, f Fixture) {
	write(t, f, first, firstText)
	write(t, f, second, secondText)
	write(t, f, third, thirdText)

	_, cursor := syncFrom(t, f, connector.Cursor{})
	if cursor.IsZero() {
		t.Fatal("a full sync came back with no cursor, so there is no incremental path to test")
	}

	changes, next := syncFrom(t, f, cursor)
	for _, ch := range changes {
		if !ch.Deleted && !ch.PermissionsOnly {
			t.Errorf("a second sync of a source nothing had changed in emitted %s again", ch.Document.ID)
		}
	}
	if next.IsZero() {
		t.Error("an incremental sync came back with no cursor, so the run after it starts from the beginning")
	}
}

// A document written after the last sync is found by the next one. A connector
// whose cursor has moved too far on is one whose index stops growing, and
// nothing in the sync says so.
func testNewDocument(t *testing.T, f Fixture) {
	write(t, f, first, firstText)
	write(t, f, second, secondText)
	_, cursor := syncFrom(t, f, connector.Cursor{})

	write(t, f, third, thirdText)
	changes, _ := syncFrom(t, f, cursor)
	ch := find(changes, f.ID(third))
	if ch == nil {
		t.Fatalf("a document written after the last sync was not found by the next one, which emitted %v", ids(changes))
	}
	if !mentions(ch.Document.Body, "hydraulics") {
		t.Errorf("%s came back with body %q, which does not contain what was written to it", ch.Document.ID, ch.Document.Body)
	}
}

// A document deleted at the source stops being part of it, and the connector has
// to make that visible one way or the other.
//
// Saying so in the sync is the good answer. A source with no change feed cannot,
// and for one of those the enumeration is what a reconciliation sweep reads, so
// it has to stop listing the document. A connector that does neither is one
// whose index can never lose a document, which is how deleted content stays
// searchable.
func testDeleted(t *testing.T, f Fixture) {
	if f.Remove == nil {
		t.Skip("the fixture cannot delete from the source")
	}
	write(t, f, first, firstText)
	write(t, f, second, secondText)
	_, cursor := syncFrom(t, f, connector.Cursor{})

	gone := f.ID(second)
	f.Remove(t, second)
	changes, _ := syncFrom(t, f, cursor)

	var reported bool
	for _, ch := range changes {
		if ch.Document.ID != gone {
			continue
		}
		if ch.Deleted {
			reported = true
			continue
		}
		t.Errorf("the sync emitted %s as a document after it had been deleted at the source", gone)
	}
	if !reported {
		e, ok := f.Connector.(connector.Enumerator)
		if !ok {
			t.Fatalf("%s was deleted at the source, the sync did not say so, and the connector cannot list its source either, so nothing will ever remove it from the index", gone)
		}
		if listed := enumerate(t, e); slices.Contains(listed, gone) {
			t.Errorf("%s was deleted at the source and the enumeration still lists it", gone)
		}
	}

	if c, ok := f.Connector.(connector.Fetcher); ok {
		if _, err := c.Fetch(t.Context(), gone); !errors.Is(err, connector.ErrGone) {
			t.Errorf("fetching %s after it was deleted returned %v, want connector.ErrGone", gone, err)
		}
	}
}

// A permission change reaches the index without the document being read again.
//
// Access at a real source does not live on the document. It lives on the folder,
// the space or the group above it, and one edit there governs thousands of
// documents whose content nobody touched. A connector that has to recrawl to
// notice is a connector where a revocation costs a full sync, which in practice
// means it waits.
func testPermissionChange(t *testing.T, f Fixture) {
	if f.Share == nil {
		t.Skip("the fixture cannot change who may read a document")
	}
	write(t, f, first, firstText)
	changes, cursor := syncFrom(t, f, connector.Cursor{})
	was := find(changes, f.ID(first))
	if was == nil {
		t.Fatalf("a full sync did not emit %s", f.ID(first))
	}
	before := was.Document.Permissions

	f.Share(t, first)
	changes, _ = syncFrom(t, f, cursor)
	now := find(changes, f.ID(first))
	if now == nil {
		t.Fatalf("who may read %s changed at the source and the next sync emitted %v, so the index is still serving the old answer", f.ID(first), ids(changes))
	}
	if samePermissions(now.Document.Permissions, before) {
		t.Errorf("the sync after a permission change reported the permissions %s already had", f.ID(first))
	}
}

// An enumeration and a sync have to agree on what is in the corpus. A sweep
// built on a listing that says less than the sync does deletes documents that
// are there, and one that says more refetches documents that are not.
func testEnumerate(t *testing.T, f Fixture) {
	e := enumerator(t, f)
	write(t, f, first, firstText)
	write(t, f, second, secondText)
	write(t, f, third, thirdText)

	changes, _ := syncFrom(t, f, connector.Cursor{})
	if got, want := enumerate(t, e), sorted(changes); !slices.Equal(got, want) {
		t.Errorf("the enumeration lists %v and the sync emitted %v", got, want)
	}
}

// A listing stopped on purpose is not a failed listing. Reporting it as one
// would make a reconciliation that used an early exit delete the whole index.
func testEnumerateEarlyStop(t *testing.T, f Fixture) {
	e := enumerator(t, f)
	write(t, f, first, firstText)
	write(t, f, second, secondText)
	write(t, f, third, thirdText)

	var n int
	if err := e.Enumerate(t.Context(), func(connector.Item) bool {
		n++
		return false
	}); err != nil {
		t.Fatalf("an enumeration that was stopped at its first item returned %v, and a sweep reading that as a failure would delete the index", err)
	}
	if n != 1 {
		t.Errorf("the enumeration called back %d times after the first one said stop", n)
	}
}

// A repair has to produce the document the sync would have produced, or a sweep
// that fills a hole fills it with something else.
func testFetch(t *testing.T, f Fixture) {
	c := fetcher(t, f)
	write(t, f, first, firstText)

	changes, _ := syncFrom(t, f, connector.Cursor{})
	synced := find(changes, f.ID(first))
	if synced == nil {
		t.Fatalf("a full sync did not emit %s", f.ID(first))
	}

	got, err := c.Fetch(t.Context(), f.ID(first))
	if err != nil {
		t.Fatalf("fetching %s: %v", f.ID(first), err)
	}
	verify(t, f, connector.Change{Document: got})
	if got.ID != synced.Document.ID {
		t.Errorf("fetch returned id %q for %q", got.ID, synced.Document.ID)
	}
	if got.Body != synced.Document.Body {
		t.Errorf("fetch returned a different body for %s than the sync did", got.ID)
	}
	if got.Title != synced.Document.Title {
		t.Errorf("fetch returned title %q for %s and the sync returned %q", got.Title, got.ID, synced.Document.Title)
	}
	if !samePermissions(got.Permissions, synced.Document.Permissions) {
		t.Errorf("fetch returned different permissions for %s than the sync did", got.ID)
	}
}

// A document the source does not have is an answer rather than a failure. It is
// how a repair learns that what it was about to refetch should be deleted.
func testFetchGone(t *testing.T, f Fixture) {
	c := fetcher(t, f)
	write(t, f, first, firstText)

	if _, err := c.Fetch(t.Context(), f.ID("never-written.md")); !errors.Is(err, connector.ErrGone) {
		t.Errorf("fetching a document the source has never had returned %v, want connector.ErrGone", err)
	}
}

// The counters are the only checkable statement that an incremental sync is
// cheap. A local corpus is fast either way, and the same connector with the same
// bug against a real service costs a hundred thousand requests and a revoked
// key.
func testCounters(t *testing.T, f Fixture) {
	c := counted(t, f)
	write(t, f, first, firstText)
	write(t, f, second, secondText)
	write(t, f, third, thirdText)

	before := c.Counters()
	_, cursor := syncFrom(t, f, connector.Cursor{})
	after := c.Counters()

	spent := after.Since(before)
	if spent.Requests() == 0 {
		t.Error("a sync that read three documents reported spending nothing at the source")
	}
	if spent.Fetches == 0 {
		t.Error("a sync that read three documents counted no fetches")
	}
	if spent.Bytes == 0 {
		t.Error("a sync that read three documents counted no bytes")
	}
	if spent.Lists < 0 || spent.Metadata < 0 || spent.Fetches < 0 || spent.Bytes < 0 {
		t.Errorf("the counters went backwards over one sync: %+v", spent)
	}

	syncFrom(t, f, cursor)
	if idle := c.Counters().Since(after); idle.Fetches != 0 {
		t.Errorf("a second sync of a source nothing had changed in read %d documents, and the whole point of the cursor is that this is zero", idle.Fetches)
	}
}

// Close runs on every path out, including the ones where something else already
// closed. A second call that failed would turn a clean shutdown into an error in
// the log every time.
func testClose(t *testing.T, f Fixture) {
	if err := f.Connector.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := f.Connector.Close(); err != nil {
		t.Fatalf("closing a second time: %v", err)
	}
}
