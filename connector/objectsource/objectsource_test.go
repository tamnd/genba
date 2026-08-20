package objectsource_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/objectsource"
	"github.com/tamnd/genba/doc"
)

const (
	sourceName = "bucket"
	identity   = "corp"
)

func TestTheBucketIsReadIntoDocuments(t *testing.T) {
	store := newStore(t)
	store.put("notes/release.md", "# Release\n\nThe first one.\n")
	store.put("notes/todo.txt", "buy milk")
	store.settle()

	source := open(t, store, nil)
	changes, _ := syncAll(t, source, connector.Cursor{})

	if len(changes) != 2 {
		t.Fatalf("read %d documents, want 2: %v", len(changes), keysOf(changes))
	}

	first := changes[0].Document
	switch {
	case first.ID != "bucket:notes/release.md":
		t.Errorf("id is %q", first.ID)
	case first.Title != "Release":
		// The heading inside the file beats the file's name, because that is
		// what somebody reading a result row is looking for.
		t.Errorf("title is %q, want the heading", first.Title)
	case !strings.Contains(first.Body, "The first one."):
		t.Errorf("body is %q", first.Body)
	case first.Container != "notes":
		t.Errorf("container is %q", first.Container)
	case first.Kind != doc.KindPage:
		t.Errorf("kind is %v, want a page", first.Kind)
	case first.Properties["bucket"] != theBucket:
		t.Errorf("bucket property is %q", first.Properties["bucket"])
	case first.Properties["key"] != "notes/release.md":
		t.Errorf("key property is %q", first.Properties["key"])
	case first.SourceUpdate == "":
		t.Error("the document carries no version, so nothing can tell whether it was rewritten")
	}
	if !strings.HasSuffix(first.URL, "/"+theBucket+"/notes/release.md") {
		t.Errorf("url is %q, which does not name the object", first.URL)
	}
}

// A source built without a policy has to quarantine, because the alternative to
// an answer about who may read a bucket is not "everybody".
func TestABucketWithNoPolicyIsQuarantinedRatherThanPublished(t *testing.T) {
	store := newStore(t)
	store.put("secret.txt", "the numbers")
	store.settle()

	changes, _ := syncAll(t, open(t, store, nil), connector.Cursor{})
	if len(changes) != 1 {
		t.Fatalf("read %d documents, want 1", len(changes))
	}
	if got := changes[0].Document.Permissions.Mode; got != acl.ModeUnknown {
		t.Errorf("the mode is %v, want it unresolved", got)
	}
}

func TestASecondSyncOverAnUnchangedBucketFetchesNothing(t *testing.T) {
	store := newStore(t)
	store.put("a.md", "one")
	store.put("b.md", "two")
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName))
	_, cursor := syncAll(t, source, connector.Cursor{})

	before := source.Counters()
	changes, next := syncAll(t, source, cursor)

	if len(changes) != 0 {
		t.Fatalf("the second sync emitted %d changes, want none: %v", len(changes), keysOf(changes))
	}
	spent := source.Counters().Since(before)
	switch {
	case spent.Fetches != 0:
		t.Errorf("the second sync fetched %d objects, want none", spent.Fetches)
	case spent.Bytes != 0:
		t.Errorf("the second sync read %d bytes of object, want none", spent.Bytes)
	case spent.Lists == 0:
		t.Error("the second sync made no listing, so it did not look at the bucket at all")
	}
	if next.IsZero() {
		t.Error("the second sync returned no cursor, so the next one would start over")
	}
}

func TestAnObjectWrittenSinceTheCursorIsTheOnlyOneRead(t *testing.T) {
	store := newStore(t)
	store.put("old.md", "unchanged")
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName))
	_, cursor := syncAll(t, source, connector.Cursor{})

	store.put("new.md", "written after the cursor")
	store.settle()

	before := source.Counters()
	changes, _ := syncAll(t, source, cursor)

	if got := keysOf(changes); !slices.Equal(got, []string{"bucket:new.md"}) {
		t.Fatalf("the second sync emitted %v, want only the new object", got)
	}
	if spent := source.Counters().Since(before); spent.Fetches != 1 {
		t.Errorf("the second sync fetched %d objects, want exactly the one that changed", spent.Fetches)
	}
}

func TestAnObjectRewrittenSinceTheCursorIsReadAgain(t *testing.T) {
	store := newStore(t)
	store.put("page.md", "the first version")
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName))
	first, cursor := syncAll(t, source, connector.Cursor{})

	store.put("page.md", "the second version, which is longer")
	store.settle()

	second, _ := syncAll(t, source, cursor)
	if len(second) != 1 {
		t.Fatalf("the second sync emitted %d changes, want the rewritten object", len(second))
	}
	if !strings.Contains(second[0].Document.Body, "the second version") {
		t.Errorf("the body is %q, want the new one", second[0].Document.Body)
	}
	if first[0].Document.SourceUpdate == second[0].Document.SourceUpdate {
		t.Error("the version did not change when the bytes did, so nothing downstream can tell them apart")
	}
}

func TestListingIsPagedRightThrough(t *testing.T) {
	store := newStore(t)
	want := make([]string, 0, 7)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		store.put(name+".md", "the "+name+" file")
		want = append(want, "bucket:"+name+".md")
	}
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName), objectsource.WithPageSize(2))
	store.requests()
	changes, _ := syncAll(t, source, connector.Cursor{})

	if got := keysOf(changes); !slices.Equal(got, want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	// Seven objects two at a time is four listings, and a connector that
	// stopped after the first page would read the first two and report success.
	if got := countListings(store.requests()); got != 4 {
		t.Errorf("made %d listings, want 4", got)
	}
}

func TestOnlyTheKeysUnderThePrefixAreRead(t *testing.T) {
	store := newStore(t)
	store.put("reports/q1.md", "the first quarter")
	store.put("reports/q2.md", "the second quarter")
	store.put("scratch/notes.md", "not part of the corpus")
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName), objectsource.WithPrefix("reports/"))
	store.requests()
	changes, _ := syncAll(t, source, connector.Cursor{})

	want := []string{"bucket:reports/q1.md", "bucket:reports/q2.md"}
	if got := keysOf(changes); !slices.Equal(got, want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	// The prefix has to be in the request rather than applied to the answer. A
	// connector that filtered afterwards would pass the assertion above and
	// would cost a full listing of a bucket it was pointed at one folder of.
	if !slices.ContainsFunc(store.requests(), func(r string) bool {
		return strings.Contains(r, "prefix=reports%2F")
	}) {
		t.Error("no listing carried the prefix, so it was a filter rather than a narrower question")
	}
	// The document's path is relative to the prefix, because where in the
	// bucket the corpus lives is the same on every row and means nothing to
	// anybody reading one.
	if got := changes[0].Document.Properties["path"]; got != "q1.md" {
		t.Errorf("the path is %q, want it relative to the prefix", got)
	}
}

func TestAnInterruptedSyncResumesFromTheKeyItReached(t *testing.T) {
	store := newStore(t)
	for _, name := range []string{"a", "b", "c", "d"} {
		store.put(name+".md", "the "+name+" file")
	}
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName))

	// Stop after two documents, the way a process being shut down does, and
	// keep the cursor the second one carried.
	var (
		got  []string
		stop = errors.New("stopping here")
		last connector.Cursor
	)
	_, err := source.Sync(t.Context(), connector.Cursor{}, func(_ context.Context, c connector.Change) error {
		got = append(got, c.Document.ID)
		last = c.Cursor
		if len(got) == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("the sync returned %v, want the error the callback gave it", err)
	}
	if last.IsZero() {
		t.Fatal("the changes carried no cursor, so an interrupted sync has nowhere to carry on from")
	}

	rest, _ := syncAll(t, source, last)
	if want := []string{"bucket:c.md", "bucket:d.md"}; !slices.Equal(keysOf(rest), want) {
		t.Fatalf("the resumed sync read %v, want %v", keysOf(rest), want)
	}
	// A resume that started over would be correct and would also re-read the
	// whole bucket, which on a real one is the difference between a minute and
	// a day.
	if got := len(rest); got != 2 {
		t.Errorf("the resumed sync emitted %d changes, want the two it had not reached", got)
	}
}

func TestABucketAccessControlListChangeIsAPermissionChangeAndNotAResync(t *testing.T) {
	store := newStore(t)
	store.put("one.md", "the first file")
	store.put("two.md", "the second file")
	store.setACL(listOf("owner@example.com", userGrant("reader@example.com", "READ")))
	store.settle()

	client := store.client(t)
	policy, err := objectsource.NewBucketPolicy(client, sourceName, identity, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	source, err := objectsource.New(client, sourceName, policy)
	if err != nil {
		t.Fatal(err)
	}

	first, cursor := syncAll(t, source, connector.Cursor{})
	if len(first) != 2 {
		t.Fatalf("the first sync emitted %d changes, want 2", len(first))
	}
	if got := refs(first[0].Document.Permissions.AllowUsers); !slices.Equal(got, []string{"reader@example.com"}) {
		t.Fatalf("the first sync allowed %v", got)
	}

	// Somebody takes the reader off the list. Not one object is touched.
	store.setACL(listOf("owner@example.com", userGrant("someone.else@example.com", "READ")))
	store.settle()

	before := source.Counters()
	second, next := syncAll(t, source, cursor)

	if len(second) != 2 {
		t.Fatalf("the revocation produced %d changes, want one per object", len(second))
	}
	for _, c := range second {
		if !c.PermissionsOnly {
			t.Fatalf("%s came back as a whole document, so the body was refetched for a permission change", c.Document.ID)
		}
		if got := refs(c.Document.Permissions.AllowUsers); !slices.Equal(got, []string{"someone.else@example.com"}) {
			t.Errorf("%s allows %v, want the new list", c.Document.ID, got)
		}
	}
	if spent := source.Counters().Since(before); spent.Fetches != 0 {
		t.Errorf("the revocation cost %d object fetches, want none", spent.Fetches)
	}

	// And it settles. A permission change that re-emitted on every later sync
	// would rewrite the whole bucket for ever after one edit.
	store.settle()
	third, _ := syncAll(t, source, next)
	if len(third) != 0 {
		t.Errorf("the sync after the revocation emitted %d changes, want none", len(third))
	}
}

// The bucket's list is read once per sync however many objects there are. That
// is the entire reason this policy exists rather than the per object one.
func TestTheBucketAccessControlListIsReadOncePerSync(t *testing.T) {
	store := newStore(t)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		store.put(name+".md", "the "+name+" file")
	}
	store.setACL(listOf("owner@example.com", groupGrant(uriAllUsers, "READ")))
	store.settle()

	client := store.client(t)
	policy, err := objectsource.NewBucketPolicy(client, sourceName, identity)
	if err != nil {
		t.Fatal(err)
	}
	source, err := objectsource.New(client, sourceName, policy)
	if err != nil {
		t.Fatal(err)
	}

	store.requests()
	changes, _ := syncAll(t, source, connector.Cursor{})
	if len(changes) != 5 {
		t.Fatalf("read %d documents, want 5", len(changes))
	}
	if got := countACLs(store.requests()); got != 1 {
		t.Errorf("read the access control list %d times for five objects, want once", got)
	}
}

func TestEnumerateDescribesEverythingAndFetchesNothing(t *testing.T) {
	store := newStore(t)
	store.put("a.md", "one")
	store.put("b.md", "two")
	store.put("c.md", "three")
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName))
	before := source.Counters()

	var got []connector.Item
	if err := source.Enumerate(t.Context(), func(i connector.Item) bool {
		got = append(got, i)
		return true
	}); err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("described %d objects, want 3", len(got))
	}
	for _, i := range got {
		if i.Version == "" {
			t.Errorf("%s was described with no version, so reconciliation cannot tell it apart from a stale copy", i.ID)
		}
	}
	if spent := source.Counters().Since(before); spent.Fetches != 0 {
		t.Errorf("enumerating fetched %d objects, want none", spent.Fetches)
	}
}

// A caller that stops early has not failed, and reporting that it did would
// make a reconciliation sweep read "nothing is in the source" and delete the
// index.
func TestEnumerateStoppingEarlyIsNotAFailure(t *testing.T) {
	store := newStore(t)
	for _, name := range []string{"a", "b", "c", "d"} {
		store.put(name+".md", "the "+name+" file")
	}
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName), objectsource.WithPageSize(2))

	var seen int
	err := source.Enumerate(t.Context(), func(connector.Item) bool {
		seen++
		return seen < 3
	})
	if err != nil {
		t.Fatalf("stopping early reported %v, want no error", err)
	}
	if seen != 3 {
		t.Errorf("saw %d objects, want the callback to have been stopped at 3", seen)
	}
}

func TestFetchReadsOneObject(t *testing.T) {
	store := newStore(t)
	store.put("notes/plan.md", "# Plan\n\nShip it.\n")
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName))
	document, err := source.Fetch(t.Context(), "bucket:notes/plan.md")
	if err != nil {
		t.Fatal(err)
	}
	if document.Title != "Plan" {
		t.Errorf("title is %q", document.Title)
	}
	if !strings.Contains(document.Body, "Ship it.") {
		t.Errorf("body is %q", document.Body)
	}
	if document.Permissions.Mode != acl.ModePublicToTenant {
		t.Errorf("the mode is %v, want the policy's answer", document.Permissions.Mode)
	}
}

func TestFetchOfSomethingGoneIsErrGone(t *testing.T) {
	store := newStore(t)
	store.put("temporary.md", "here for now")
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName))
	store.remove("temporary.md")

	if _, err := source.Fetch(t.Context(), "bucket:temporary.md"); !errors.Is(err, connector.ErrGone) {
		t.Errorf("fetching a deleted object returned %v, want ErrGone", err)
	}
}

// A store that is refusing, or that has been pointed at a bucket that does not
// exist, must not read as "every document was deleted". The failure that would
// cause is an index emptied by a typo in a setting.
func TestAStoreThatRefusesIsNotADeletion(t *testing.T) {
	store := newStore(t)
	store.put("real.md", "here")
	store.settle()

	client, err := objectsource.NewClient(objectsource.Config{
		Endpoint:        store.server.URL,
		Region:          "eu-west-1",
		Bucket:          "a-bucket-that-is-not-there",
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		PathStyle:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := objectsource.New(client, sourceName, objectsource.PublicToTenant(sourceName))
	if err != nil {
		t.Fatal(err)
	}

	_, err = source.Fetch(t.Context(), "bucket:real.md")
	switch {
	case err == nil:
		t.Fatal("fetching from a bucket that is not there succeeded")
	case errors.Is(err, connector.ErrGone):
		t.Fatal("a missing bucket read as a deleted object, which would empty the index")
	case !strings.Contains(err.Error(), "NoSuchBucket"):
		t.Errorf("the error is %v, want it to say what the store said", err)
	}
}

func TestAnIdFromAnotherSourceIsRefused(t *testing.T) {
	store := newStore(t)
	store.put("shared.md", "in the bucket")
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName))
	for _, id := range []string{"elsewhere:shared.md", "shared.md", "bucket:", ""} {
		if _, err := source.Fetch(t.Context(), id); !errors.Is(err, connector.ErrGone) {
			t.Errorf("fetching %q returned %v, want ErrGone", id, err)
		}
	}
}

func TestAnIdThatTriesToLeaveThePrefixIsRefused(t *testing.T) {
	store := newStore(t)
	store.put("public/open.md", "anybody may read this")
	store.put("private/closed.md", "nobody may")
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName), objectsource.WithPrefix("public/"))
	for _, id := range []string{
		"bucket:private/closed.md",
		"bucket:public/../private/closed.md",
		"bucket:public//closed.md",
	} {
		if _, err := source.Fetch(t.Context(), id); !errors.Is(err, connector.ErrGone) {
			t.Errorf("fetching %q returned %v, want ErrGone", id, err)
		}
	}
	if _, err := source.Fetch(t.Context(), "bucket:public/open.md"); err != nil {
		t.Errorf("fetching a key inside the prefix failed: %v", err)
	}
}

// The store's modification times have a second of resolution and a listing of a
// real bucket takes a great deal longer than that. An object written later in
// the same second as the newest one a run saw would be filed under a time the
// cursor had already passed, and would never be looked at again.
func TestAnObjectWrittenInTheSameSecondIsNotLost(t *testing.T) {
	store := newStore(t)
	store.put("first.md", "written at noon")
	// No settling. The run finishes in the same second as the write, which is
	// what happens on a small bucket and on every developer's laptop.

	source := open(t, store, objectsource.PublicToTenant(sourceName))
	if changes, cursor := syncAll(t, source, connector.Cursor{}); len(changes) != 1 {
		t.Fatalf("the first sync read %d documents, want 1", len(changes))
	} else {
		// A second object lands in the same second, after the listing went past
		// where its key sorts.
		store.putAt("second.md", "written in the same second", store.now())
		store.settle()

		got, _ := syncAll(t, source, cursor)
		if !slices.Contains(keysOf(got), "bucket:second.md") {
			t.Errorf("the second sync read %v, and the object written in the same second is not in it", keysOf(got))
		}
	}
}

func TestAnObjectOverTheSizeLimitIsSkippedAndReported(t *testing.T) {
	store := newStore(t)
	store.put("small.md", "short enough")
	store.put("huge.md", strings.Repeat("x", 4096))
	store.settle()

	var skipped []string
	source := open(t, store,
		objectsource.PublicToTenant(sourceName),
		objectsource.WithMaxObjectSize(1024),
		objectsource.WithSkipped(func(key string, reason error) {
			skipped = append(skipped, key+": "+reason.Error())
		}),
	)
	before := source.Counters()
	changes, _ := syncAll(t, source, connector.Cursor{})

	if got := keysOf(changes); !slices.Equal(got, []string{"bucket:small.md"}) {
		t.Fatalf("read %v, want only the small object", got)
	}
	if len(skipped) != 1 || !strings.HasPrefix(skipped[0], "huge.md:") {
		t.Fatalf("the skipped objects were %v, want the large one reported", skipped)
	}
	// The size came from the listing, so the object was never fetched. An
	// implementation that downloaded it to find out how big it was would pass
	// every other assertion here.
	if spent := source.Counters().Since(before); spent.Fetches != 1 {
		t.Errorf("fetched %d objects, want only the one that was in the corpus", spent.Fetches)
	}
}

func TestTheFormatsNobodyCanSearchAreNotFetchedAtAll(t *testing.T) {
	store := newStore(t)
	store.put("readme.md", "prose")
	store.put("backup.tar.gz", "not prose")
	store.put("disk.img", "not prose either")
	// The empty object a console writes when somebody makes a folder. There is
	// nothing in it and there never was.
	store.put("folder/", "")
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName))
	before := source.Counters()
	changes, _ := syncAll(t, source, connector.Cursor{})

	if got := keysOf(changes); !slices.Equal(got, []string{"bucket:readme.md"}) {
		t.Fatalf("read %v, want only the document", got)
	}
	if spent := source.Counters().Since(before); spent.Fetches != 1 {
		t.Errorf("fetched %d objects, want the one that was worth reading", spent.Fetches)
	}
}

func TestAnImageBecomesContentRatherThanBody(t *testing.T) {
	store := newStore(t)
	store.put("shots/architecture.png", string(onePixelPNG()))
	store.settle()

	source := open(t, store, objectsource.PublicToTenant(sourceName))
	changes, _ := syncAll(t, source, connector.Cursor{})

	if len(changes) != 1 {
		t.Fatalf("read %d documents, want the image", len(changes))
	}
	got := changes[0].Document
	switch {
	case got.Kind != doc.KindImage:
		t.Errorf("kind is %v, want an image", got.Kind)
	case got.Content == nil:
		t.Fatal("the image carries no content, so a preview has nothing to show")
	case got.Content.Width != 1 || got.Content.Height != 1:
		t.Errorf("the image is %dx%d, want 1x1", got.Content.Width, got.Content.Height)
	case got.Body != "":
		t.Errorf("the image has a body of %q, want none", got.Body)
	case got.Title != "architecture.png":
		// The name is the only thing a query can match on an image, which is how
		// somebody finds this by typing architecture.
		t.Errorf("title is %q, want the file name", got.Title)
	}
}

// open builds a source over the fake with the default settings.
func open(t *testing.T, store *fakeStore, policy objectsource.Policy, opts ...objectsource.Option) *objectsource.Source {
	t.Helper()
	source, err := objectsource.New(store.client(t), sourceName, policy, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Errorf("closing the source: %v", err)
		}
	})
	return source
}

// syncAll runs one sync to the end and collects what it emitted.
func syncAll(t *testing.T, s *objectsource.Source, from connector.Cursor) ([]connector.Change, connector.Cursor) {
	t.Helper()
	var got []connector.Change
	next, err := s.Sync(t.Context(), from, func(_ context.Context, c connector.Change) error {
		got = append(got, c)
		return nil
	})
	if err != nil {
		t.Fatalf("syncing: %v", err)
	}
	return got, next
}

func keysOf(changes []connector.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Document.ID)
	}
	return out
}

func refs(list []acl.Ref) []string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.Value)
	}
	slices.Sort(out)
	return out
}

func countListings(requests []string) int {
	return countMatching(requests, "list-type=2")
}

func countACLs(requests []string) int {
	return countMatching(requests, "acl")
}

func countMatching(requests []string, part string) int {
	var n int
	for _, r := range requests {
		if strings.Contains(r, part) {
			n++
		}
	}
	return n
}

// onePixelPNG is the smallest image the standard library will read the size of,
// which is all this needs to be.
func onePixelPNG() []byte {
	return []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
		0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05,
		0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00,
		0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
	}
}

// settle moves the store's clock on, the way real time passes between somebody
// writing an object and a sync noticing it.
//
// It matters because the connector holds its cursor a second behind the store's
// own clock on purpose, so a test that wrote and synced in the same instant
// would be testing that safety margin rather than whatever it meant to test.
func (f *fakeStore) settle() { f.tick(time.Minute) }
