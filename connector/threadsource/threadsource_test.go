package threadsource_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/thread"
	"github.com/tamnd/genba/connector/threadsource"
	"github.com/tamnd/genba/doc"
)

func newSource(t *testing.T, f *fake, opts ...threadsource.Option) *threadsource.Source {
	t.Helper()
	src, err := threadsource.New(f, source, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

// sync runs one sync and hands back everything it emitted.
func run(t *testing.T, src *threadsource.Source, from connector.Cursor) ([]connector.Change, connector.Cursor) {
	t.Helper()
	var got []connector.Change
	next, err := src.Sync(t.Context(), from, func(_ context.Context, ch connector.Change) error {
		got = append(got, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return got, next
}

// find returns the change for one document id.
func find(changes []connector.Change, id string) *connector.Change {
	for i := range changes {
		if changes[i].Document.ID == id {
			return &changes[i]
		}
	}
	return nil
}

func ids(changes []connector.Change) []string {
	out := make([]string, 0, len(changes))
	for _, ch := range changes {
		out = append(out, ch.Document.ID)
	}
	slices.Sort(out)
	return out
}

// A thread is one result with its replies, which is the whole reason this
// connector assembles rather than emitting a row per message.
func TestAThreadAndItsRepliesAreOneDocument(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.reply("r1", "gearbox", "sam", "Bearing, I think.")
	f.reply("r1", "gearbox", "mei", "Replacement ordered.")
	src := newSource(t, f)

	changes, _ := run(t, src, connector.Cursor{})
	if len(changes) != 1 {
		t.Fatalf("a thread of three messages produced %d documents: %v", len(changes), ids(changes))
	}

	got := changes[0].Document
	for _, want := range []string{"making a noise", "Bearing, I think", "Replacement ordered"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("the body does not hold %q:\n%s", want, got.Body)
		}
	}
	if got.Properties["messages"] != "3" || got.Properties["replies"] != "2" {
		t.Errorf("the counts are messages=%q replies=%q", got.Properties["messages"], got.Properties["replies"])
	}
}

func TestADocumentSaysWhereItCameFromAndNotWhoItIsFor(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	src := newSource(t, f)

	changes, _ := run(t, src, connector.Cursor{})
	got := changes[0].Document
	if got.ID != "chat:gearbox" {
		t.Errorf("the id is %q, want the source name in front of the conversation's own id", got.ID)
	}
	if got.Source != source {
		t.Errorf("the document says it came from %q", got.Source)
	}
	// The tenant is the pipeline's to set, from the run rather than from the
	// source, and a connector that fills it in is one whose documents land in
	// whichever tenant it was first written for.
	if got.Tenant != "" {
		t.Errorf("the document carries tenant %q", got.Tenant)
	}
	// The channel is what a person reading a result row wants to see, and the
	// conversation itself said nothing about it.
	if got.Container != "maintenance" {
		t.Errorf("the container is %q, want the channel's name", got.Container)
	}
}

// A conversation inherits the rule on the container, because that is how every
// one of these products actually works.
func TestAConversationInheritsTheChannelsRule(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.share("r1")
	src := newSource(t, f)

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "chat:gearbox")
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if got.Document.Permissions.Mode != acl.ModeACL {
		t.Fatalf("the document is %v, want the private channel's rule", got.Document.Permissions.Mode)
	}
	if len(got.Document.Permissions.AllowGroups) != 1 {
		t.Errorf("the rule allows %v", got.Document.Permissions.AllowGroups)
	}
}

// A ticket with a security level on it is readable by that level's members and
// by nobody else, whatever the project says.
func TestAConversationWithARuleOfItsOwnKeepsIt(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.write("r1", "payroll", "Payroll is late again.")
	f.restrict("r1", "payroll", acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      source,
		AllowGroups: []acl.Ref{{Source: source, Value: "finance"}},
	})
	src := newSource(t, f)

	changes, _ := run(t, src, connector.Cursor{})
	open := find(changes, "chat:gearbox")
	closed := find(changes, "chat:payroll")
	if open == nil || closed == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if open.Document.Permissions.Mode != acl.ModePublicToTenant {
		t.Errorf("the unrestricted thread is %v", open.Document.Permissions.Mode)
	}
	if got := closed.Document.Permissions.AllowGroups; len(got) != 1 || got[0].Value != "finance" {
		t.Errorf("the restricted thread allows %v, want the group named on it rather than the channel's", got)
	}
}

// There is no permissive default. A channel nobody has said anything about is
// quarantined, which is loud and safe, rather than published to the tenant.
func TestAChannelWithNoRuleQuarantinesWhatIsInIt(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.unruled("r1")
	src := newSource(t, f)

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "chat:gearbox")
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if got.Document.Permissions.Mode != acl.ModeUnknown {
		t.Errorf("a thread in a channel with no rule is %v", got.Document.Permissions.Mode)
	}
	if got.Document.Permissions.Source != source {
		t.Errorf("the descriptor says it came from %q, and a connector that could not answer still says who was asked", got.Document.Permissions.Source)
	}
	if got.Document.Queryable() {
		t.Error("a thread nobody can say who may read is queryable")
	}
}

// Somebody makes a channel private and nothing inside it is touched. A sync
// that only asked what changed would find nothing at all, and the index would
// keep answering with the old rule.
func TestMakingAChannelPrivateReachesTheIndexWithoutReadingIt(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.write("r1", "coolant", "Coolant pump replaced.")
	src := newSource(t, f)

	_, cursor := run(t, src, connector.Cursor{})
	before := src.Counters()

	f.share("r1")
	changes, next := run(t, src, cursor)

	if len(changes) != 2 {
		t.Fatalf("making the channel private emitted %v, want both threads in it", ids(changes))
	}
	for _, ch := range changes {
		if !ch.PermissionsOnly {
			t.Errorf("%s came back as a content change, and nothing in it changed", ch.Document.ID)
		}
		if ch.Document.Body != "" {
			t.Errorf("%s carries content on a permission change, which will not be stored", ch.Document.ID)
		}
		if ch.Document.Permissions.Mode != acl.ModeACL {
			t.Errorf("%s reports %v, which is the rule it already had", ch.Document.ID, ch.Document.Permissions.Mode)
		}
	}
	if spent := src.Counters().Since(before); spent.Fetches != 0 {
		t.Errorf("a permission change read %d threads, and the point of it is that it reads none", spent.Fetches)
	}

	// And the run after it says nothing, because the rule has not changed
	// again. A connector that re-emitted here would rewrite the permissions of
	// every document in the workspace on every sync for ever.
	if again, _ := run(t, src, next); len(again) != 0 {
		t.Errorf("the sync after the permission change emitted %v", ids(again))
	}
}

func TestASecondSyncOfAWorkspaceNothingChangedInEmitsNothing(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.write("r1", "coolant", "Coolant pump replaced.")
	src := newSource(t, f)

	_, cursor := run(t, src, connector.Cursor{})
	before := src.Counters()

	changes, _ := run(t, src, cursor)
	if len(changes) != 0 {
		t.Errorf("a second sync emitted %v", ids(changes))
	}
	if spent := src.Counters().Since(before); spent.Fetches != 0 || spent.Bytes != 0 {
		t.Errorf("a second sync read %d threads and %d bytes", spent.Fetches, spent.Bytes)
	}
}

// A reply is a change to the thread rather than a document of its own, so the
// next sync emits that thread again and the rest of the channel not at all.
func TestAReplyBringsTheWholeThreadBack(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.write("r1", "coolant", "Coolant pump replaced.")
	src := newSource(t, f)

	_, cursor := run(t, src, connector.Cursor{})
	f.reply("r1", "gearbox", "sam", "Bearing, I think.")

	changes, _ := run(t, src, cursor)
	if got := ids(changes); !slices.Equal(got, []string{"chat:gearbox"}) {
		t.Fatalf("the sync after one reply emitted %v", got)
	}
	if !strings.Contains(changes[0].Document.Body, "Bearing, I think") {
		t.Errorf("the reply is not in the body:\n%s", changes[0].Document.Body)
	}
}

// Two conversations changed in the same instant is the case a cursor made of a
// timestamp gets wrong. Asking for what changed strictly after the cursor loses
// the second one for ever, and asking for what changed at or after it emits both
// again on every run until something else happens in the channel.
func TestTwoThreadsInOneInstantAreEmittedOnceEach(t *testing.T) {
	f := newFake()
	at := epoch.Add(time.Hour)
	f.writeAt("r1", "gearbox", "The gearbox on line two is making a noise.", at)
	f.writeAt("r1", "coolant", "Coolant pump replaced.", at)
	src := newSource(t, f)

	changes, cursor := run(t, src, connector.Cursor{})
	if got := ids(changes); !slices.Equal(got, []string{"chat:coolant", "chat:gearbox"}) {
		t.Fatalf("the first sync emitted %v", got)
	}

	if again, next := run(t, src, cursor); len(again) != 0 {
		t.Errorf("the second sync emitted %v again", ids(again))
	} else if next.IsZero() {
		t.Error("the second sync came back with no cursor")
	}

	// And a third conversation in that same instant, written after the sync
	// that recorded it, is still found. This is the half that a connector
	// comparing strictly after the cursor loses without a word.
	f.writeAt("r1", "belt", "Belt tension checked.", at)
	changes, _ = run(t, src, cursor)
	if got := ids(changes); !slices.Equal(got, []string{"chat:belt"}) {
		t.Errorf("a thread written in the same instant as the cursor came back as %v", got)
	}
}

// A crawl of a real workspace takes long enough that it will be interrupted,
// and the cursor a change carries is what makes the next run cheap.
func TestAnInterruptedRunDoesNotWalkTheChannelsItFinished(t *testing.T) {
	f := newFake()
	f.addRoom("r2", "incidents")
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.write("r2", "outage", "The payments queue backed up.")
	src := newSource(t, f)

	changes, _ := run(t, src, connector.Cursor{})
	stopped := find(changes, "chat:outage")
	if stopped == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}

	f.asked = nil
	run(t, src, stopped.Cursor)
	if !slices.Equal(f.asked, []string{"r2"}) {
		t.Errorf("resuming inside r2 walked %v, and r1 was finished before the run died", f.asked)
	}
}

// Resuming has to pick up everything that came after the cursor, in the
// channels the run had reached and in the ones it had not.
func TestResumingLosesNothingInEitherDirection(t *testing.T) {
	f := newFake()
	f.addRoom("r2", "incidents")
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.write("r2", "outage", "The payments queue backed up.")
	src := newSource(t, f)

	changes, _ := run(t, src, connector.Cursor{})
	from := find(changes, "chat:gearbox")
	if from == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}

	f.write("r1", "coolant", "Coolant pump replaced.")
	f.write("r2", "network", "Switch firmware rolled back.")

	got, _ := run(t, src, from.Cursor)
	for _, want := range []string{"chat:coolant", "chat:network", "chat:outage"} {
		if find(got, want) == nil {
			t.Errorf("resuming from a change in r1 did not emit %s, and it emitted %v", want, ids(got))
		}
	}
}

// A cursor this connector cannot read was written by a different version or a
// different connector, and resyncing a workspace on the strength of that is
// hours of somebody else's rate limit.
func TestAnUnreadableCursorIsRefusedRatherThanIgnored(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	src := newSource(t, f)

	_, err := src.Sync(t.Context(), connector.Cursor{Value: "not a cursor this connector wrote"}, func(context.Context, connector.Change) error {
		t.Error("a sync from an unreadable cursor emitted a document")
		return nil
	})
	if err == nil {
		t.Fatal("a sync from an unreadable cursor succeeded")
	}
	if !strings.Contains(err.Error(), "cursor") {
		t.Errorf("the error does not say what was wrong: %v", err)
	}
}

// The sweep is the only thing that ever removes a thread, because none of these
// products reports a deletion in a change feed.
func TestEnumerateListsEveryChannel(t *testing.T) {
	f := newFake()
	f.addRoom("r2", "incidents")
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.write("r2", "outage", "The payments queue backed up.")
	src := newSource(t, f)

	var got []string
	if err := src.Enumerate(t.Context(), func(item connector.Item) bool {
		if item.Version == "" {
			t.Errorf("%s was listed without a version, so it can be found missing but never stale", item.ID)
		}
		got = append(got, item.ID)
		return true
	}); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	slices.Sort(got)
	if want := []string{"chat:gearbox", "chat:outage"}; !slices.Equal(got, want) {
		t.Errorf("the enumeration lists %v, want %v", got, want)
	}
}

// A listing stopped on purpose is not a failed listing. Reporting it as one
// would make a reconciliation that used an early exit delete the whole index.
func TestEnumerateStopsWhenTheCallerSaysSo(t *testing.T) {
	f := newFake()
	f.addRoom("r2", "incidents")
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.write("r2", "outage", "The payments queue backed up.")
	src := newSource(t, f)

	var seen int
	if err := src.Enumerate(t.Context(), func(connector.Item) bool {
		seen++
		return false
	}); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if seen != 1 {
		t.Errorf("the listing carried on for %d items after being told to stop", seen)
	}
}

func TestFetchReturnsWhatASyncWouldHave(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.reply("r1", "gearbox", "sam", "Bearing, I think.")
	src := newSource(t, f)

	changes, _ := run(t, src, connector.Cursor{})
	want := changes[0].Document

	got, err := src.Fetch(t.Context(), "chat:gearbox")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.ID != want.ID || got.Body != want.Body || got.Title != want.Title {
		t.Errorf("fetch returned %+v, and the sync emitted %+v", got, want)
	}
	// A fetch arrives with nothing but an id, so the rule has to be worked out
	// from the channel the conversation says it is in.
	if got.Permissions.Mode != want.Permissions.Mode || got.Permissions.Source != source {
		t.Errorf("fetch resolved %v and the sync resolved %v", got.Permissions, want.Permissions)
	}
}

func TestFetchOfSomethingTheSourceNoLongerHasIsNotAFailure(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	src := newSource(t, f)

	f.remove("r1", "gearbox")
	if _, err := src.Fetch(t.Context(), "chat:gearbox"); !errors.Is(err, connector.ErrGone) {
		t.Errorf("fetching a deleted thread gave %v, want the answer that it is gone", err)
	}
}

func TestFetchOfAnIdFromSomewhereElseSaysSo(t *testing.T) {
	f := newFake()
	src := newSource(t, f)

	_, err := src.Fetch(t.Context(), "wiki:gearbox")
	if err == nil {
		t.Fatal("fetching an id from another source succeeded")
	}
	if errors.Is(err, connector.ErrGone) {
		t.Error("an id from another source came back as a document this source used to have")
	}
	if !strings.Contains(err.Error(), "wiki:gearbox") {
		t.Errorf("the error does not say what was asked for: %v", err)
	}
}

func TestTheCountersSayWhatASyncSpent(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.write("r1", "coolant", "Coolant pump replaced.")
	src := newSource(t, f)

	run(t, src, connector.Cursor{})
	got := src.Counters()
	if got.Fetches != 2 {
		t.Errorf("a sync of two threads counted %d fetches", got.Fetches)
	}
	if got.Lists == 0 {
		t.Error("a sync that listed the channels and their threads counted no listings")
	}
	if got.Bytes == 0 {
		t.Error("a sync that read two threads counted no bytes")
	}
}

// A conversation too long to index whole keeps its beginning and its end, and
// the document says it was cut.
func TestALongThreadIsCutRatherThanDropped(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", strings.Repeat("the gearbox is still making a noise. ", 200))
	f.reply("r1", "gearbox", "sam", "Bearing, I think.")
	src := newSource(t, f, threadsource.WithMaxBody(256))

	changes, _ := run(t, src, connector.Cursor{})
	got := changes[0].Document
	if len(got.Body) > 256 {
		t.Errorf("the body is %d bytes with a limit of 256", len(got.Body))
	}
	if got.Properties["truncated"] != "true" {
		t.Error("the document does not say it was cut, so a reader will report it as missing a sentence that is in it")
	}
}

func TestClosingClosesTheServiceOnceAndIsRepeatable(t *testing.T) {
	f := newFake()
	src, err := threadsource.New(f, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("closing a second time: %v", err)
	}
	if f.closes != 1 {
		t.Errorf("the service was closed %d times", f.closes)
	}
}

func TestASourceWithNothingBehindItIsRefused(t *testing.T) {
	if _, err := threadsource.New(nil, source); err == nil {
		t.Error("a source with no service behind it was built")
	}
	if _, err := threadsource.New(newFake(), ""); err == nil {
		t.Error("a source with no name was built, and the name is what its cursor is filed under")
	}
}

// An error from the service is the caller's to see. A sync that swallowed one
// would report success and move the cursor past documents it never read.
func TestAnErrorFromTheServiceComesBackUnchanged(t *testing.T) {
	failed := errors.New("the workspace is rate limiting us")

	for _, tc := range []struct {
		name string
		set  func(f *fake)
	}{
		{"listing the channels", func(f *fake) { f.failContainers = failed }},
		{"walking a channel", func(f *fake) { f.failThreads = failed }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
			tc.set(f)
			src := newSource(t, f)

			_, err := src.Sync(t.Context(), connector.Cursor{}, func(context.Context, connector.Change) error { return nil })
			if !errors.Is(err, failed) {
				t.Errorf("the sync returned %v, want the error the service gave it", err)
			}
		})
	}
}

// nameless is an adapter with a bug in it: a conversation with no id, which is
// the one thing that leaves nothing to file a document under.
type nameless struct{}

func (nameless) Containers(context.Context) ([]threadsource.Container, error) {
	return []threadsource.Container{{
		ID:     "r1",
		Name:   "maintenance",
		Access: acl.Permissions{Mode: acl.ModePublicToTenant, Source: source},
	}}, nil
}

func (nameless) Threads(ctx context.Context, _ threadsource.Container, _ time.Time, fn func(context.Context, threadsource.Thread) error) error {
	return fn(ctx, threadsource.Thread{
		Conversation: thread.Conversation{Title: "a thread with no identity"},
		Container:    "r1",
		Updated:      epoch,
	})
}

func (nameless) List(context.Context, threadsource.Container, func(connector.Item) bool) error {
	return nil
}

func (nameless) Read(context.Context, string) (threadsource.Thread, error) {
	return threadsource.Thread{}, connector.ErrGone
}

// An index quietly missing what nobody could read looks exactly like an index
// that is complete, and the difference only shows up when somebody cannot find
// a thread they remember.
func TestAConversationThatCannotBeIndexedIsReported(t *testing.T) {
	var skipped []error
	src, err := threadsource.New(nameless{}, source, threadsource.WithSkipped(func(_ string, reason error) {
		skipped = append(skipped, reason)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })

	changes, _ := run(t, src, connector.Cursor{})
	if len(changes) != 0 {
		t.Errorf("a conversation with no id was emitted as %v", ids(changes))
	}
	if len(skipped) != 1 {
		t.Fatalf("the sync passed over a conversation and said %v", skipped)
	}
	if !strings.Contains(skipped[0].Error(), "id") {
		t.Errorf("the reason given was %q", skipped[0])
	}
}

// A document with an author on it is what makes a result row say who to ask,
// and the author of a thread is whoever started it.
func TestTheAuthorIsWhoeverStartedTheThread(t *testing.T) {
	f := newFake()
	f.write("r1", "gearbox", "The gearbox on line two is making a noise.")
	f.reply("r1", "gearbox", "sam", "Bearing, I think.")
	src := newSource(t, f)

	changes, _ := run(t, src, connector.Cursor{})
	if got := changes[0].Document.Author; got.Name != "mei" {
		t.Errorf("the author is %+v, want whoever wrote the first message", got)
	}
	if got := changes[0].Document.Kind; got != doc.KindMessage {
		t.Errorf("the kind is %q", got)
	}
}
