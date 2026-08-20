package slacksource_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/connectortest"
	"github.com/tamnd/genba/connector/limit"
	"github.com/tamnd/genba/connector/slacksource"
	"github.com/tamnd/genba/connector/threadsource"
	"github.com/tamnd/genba/doc"
)

const token = "xoxb-a-real-looking-token"

// quick is a rate limit that does not slow a test down while still being the
// real limiter, retries, backoff, circuit breaker and all.
var quick = limit.Limits{Rate: 10000, Burst: 50, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}

func newSource(t *testing.T, w *workspace, opts ...slacksource.Option) *threadsource.Source {
	t.Helper()
	all := append([]slacksource.Option{
		slacksource.WithBaseURL(w.server(t)),
		slacksource.WithLimits(quick),
		slacksource.WithClock(w.now),
	}, opts...)
	src, err := slacksource.New(token, all...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

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

func ids(changes []connector.Change) []string {
	out := make([]string, 0, len(changes))
	for _, ch := range changes {
		out = append(out, ch.Document.ID)
	}
	slices.Sort(out)
	return out
}

func find(changes []connector.Change, id string) *connector.Change {
	for i := range changes {
		if changes[i].Document.ID == id {
			return &changes[i]
		}
	}
	return nil
}

// The conformance suite is what decides whether this is a connector, and
// everything else in this file is about the parts that are specific to Slack.
//
// Unresolvable is left out because Slack has no per message rule to make
// unresolvable. A thread is readable exactly when its channel is, and the case
// where that cannot be worked out is a whole channel at a time, which is its own
// test below.
func TestConformance(t *testing.T) {
	connectortest.Run(t, func(t *testing.T) connectortest.Fixture {
		w := newWorkspace()
		src, err := slacksource.New(token,
			slacksource.WithBaseURL(w.server(t)),
			slacksource.WithLimits(quick),
			slacksource.WithClock(w.now),
		)
		if err != nil {
			t.Fatal(err)
		}
		return connectortest.Fixture{
			Connector: src,
			ID:        func(name string) string { return "slack:C_GENERAL:" + w.tsOf("C_GENERAL", name) },
			Write:     func(_ *testing.T, name, body string) { w.post("C_GENERAL", name, body) },
			Remove:    func(_ *testing.T, name string) { w.remove("C_GENERAL", name) },
			Share:     func(_ *testing.T, _ string) { w.makePrivate("C_GENERAL") },
		}
	})
}

// A reply is a sentence in a document rather than a document, which is the one
// thing this connector exists to get right.
func TestAThreadAndItsRepliesAreOneDocument(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	w.reply("C_GENERAL", "gearbox", "U_SAM", "Bearing, I think.")
	w.reply("C_GENERAL", "gearbox", "U_LEE", "Replacement ordered, arrives Thursday.")
	w.post("C_GENERAL", "coolant", "Coolant pump replaced.")
	src := newSource(t, w)

	changes, _ := run(t, src, connector.Cursor{})
	if len(changes) != 2 {
		t.Fatalf("a channel with two threads and three replies produced %d documents: %v", len(changes), ids(changes))
	}

	got := find(changes, "slack:C_GENERAL:"+w.tsOf("C_GENERAL", "gearbox"))
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	for _, want := range []string{"making a noise", "Bearing, I think", "arrives Thursday"} {
		if !strings.Contains(got.Document.Body, want) {
			t.Errorf("the body does not hold %q:\n%s", want, got.Document.Body)
		}
	}
	// The name of whoever said each thing goes into the body, which is the
	// difference between searching for what Mei said about the gearbox and only
	// being able to search for the gearbox.
	for _, want := range []string{"Mei Tanaka", "Sam Okafor", "Lee Berger"} {
		if !strings.Contains(got.Document.Body, want) {
			t.Errorf("the body does not name %q:\n%s", want, got.Document.Body)
		}
	}
	if got.Document.Properties["replies"] != "2" {
		t.Errorf("the document reports %q replies", got.Document.Properties["replies"])
	}
	if got.Document.Kind != doc.KindMessage {
		t.Errorf("the kind is %q", got.Document.Kind)
	}
	if !strings.Contains(got.Document.URL, "/archives/C_GENERAL/p") {
		t.Errorf("the link is %q, and it has to be one a person can open", got.Document.URL)
	}
}

// The noise Slack posts about the channel itself is not a document. Indexing
// "Mei has joined the channel" is how a search for somebody's name returns four
// hundred results they did not write.
func TestJoinsAndLeavesAreNotDocuments(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	w.system("C_GENERAL", "channel_join", "<@U_SAM> has joined the channel")
	w.system("C_GENERAL", "channel_leave", "<@U_LEE> has left the channel")
	w.system("C_GENERAL", "channel_topic", "<@U_MEI> set the channel topic: line two")
	src := newSource(t, w)

	changes, _ := run(t, src, connector.Cursor{})
	if got := ids(changes); len(got) != 1 {
		t.Fatalf("a channel with one thread and three pieces of housekeeping emitted %v", got)
	}
	if !strings.Contains(changes[0].Document.Body, "making a noise") {
		t.Errorf("the one document is not the thread:\n%s", changes[0].Document.Body)
	}
}

// A message whose text is empty has nothing to index. A file share with no
// comment is the common one, and it arrives as a message with an attachment and
// no words in it.
func TestAMessageWithNothingInItIsNotADocument(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	w.system("C_GENERAL", "file_share", "   ")
	src := newSource(t, w)

	changes, _ := run(t, src, connector.Cursor{})
	if len(changes) != 1 {
		t.Errorf("a message with no text in it was indexed: %v", ids(changes))
	}
}

// A public channel is readable by the whole workspace, which is what makes it
// public, and a private one is readable by its members and nobody else.
func TestAPrivateChannelIsInvisibleToAnybodyNotInIt(t *testing.T) {
	w := newWorkspace()
	w.addChannel("C_LEAD", "leadership", true)
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	w.post("C_LEAD", "restructure", "The reorganisation goes out on Monday.")
	src := newSource(t, w)

	changes, _ := run(t, src, connector.Cursor{})
	open := find(changes, "slack:C_GENERAL:"+w.tsOf("C_GENERAL", "gearbox"))
	closed := find(changes, "slack:C_LEAD:"+w.tsOf("C_LEAD", "restructure"))
	if open == nil || closed == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}

	if open.Document.Permissions.Mode != acl.ModePublicToTenant {
		t.Errorf("a thread in a public channel is %v", open.Document.Permissions.Mode)
	}
	if closed.Document.Permissions.Mode != acl.ModeACL {
		t.Fatalf("a thread in a private channel is %v", closed.Document.Permissions.Mode)
	}
	if got := closed.Document.Permissions.AllowUsers; len(got) != 2 {
		t.Fatalf("the private thread allows %v, want the two members of the channel", got)
	}

	// A person who is not in the channel is not on the list, and the list is
	// the whole of the rule: there is no group and no fallback that would let
	// them through.
	lee := acl.Principal{Tenant: "acme", Subject: "U_LEE", Identities: []acl.Identity{{Source: "slack", Value: "U_LEE"}}}
	if closed.Document.Permissions.Allows(&lee) {
		t.Error("somebody who is not in the private channel may read a thread from it")
	}
	mei := acl.Principal{Tenant: "acme", Subject: "U_MEI", Identities: []acl.Identity{{Source: "slack", Value: "U_MEI"}}}
	if !closed.Document.Permissions.Allows(&mei) {
		t.Error("a member of the private channel may not read a thread from it")
	}
}

// The member list is not ordered by anything, and a list that reorders itself
// between syncs would be a revocation that never happened, written to every
// document in the channel.
func TestTheMemberListIsPutInOrder(t *testing.T) {
	w := newWorkspace()
	w.addChannel("C_LEAD", "leadership", true)
	w.post("C_LEAD", "restructure", "The reorganisation goes out on Monday.")
	src := newSource(t, w)

	changes, _ := run(t, src, connector.Cursor{})
	got := changes[0].Document.Permissions.AllowUsers
	if !slices.IsSortedFunc(got, func(a, b acl.Ref) int { return strings.Compare(a.Value, b.Value) }) {
		t.Errorf("the rule lists %v, and the fake hands them back in the least helpful order it can", got)
	}
}

// Direct messages have no rule that makes them safe to put in a shared index,
// so they are not asked for at all.
func TestDirectMessagesAreNotIndexed(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	src := newSource(t, w)

	if _, err := src.Sync(t.Context(), connector.Cursor{}, func(context.Context, connector.Change) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// The fake refuses anything it was not asked for by returning nothing, so
	// the assertion is on the request: nothing ever asked for im or mpim.
	if w.askedFor("im") || w.askedFor("mpim") {
		t.Error("the crawl asked Slack for direct messages")
	}
}

// A bot that is in the workspace but not in a channel is the case that quietly
// produces an incomplete index, so it is quarantined and reported rather than
// guessed at.
func TestAChannelTheTokenCannotReadIsQuarantinedAndReported(t *testing.T) {
	w := newWorkspace()
	w.addChannel("C_LEAD", "leadership", true)
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	w.post("C_LEAD", "restructure", "The reorganisation goes out on Monday.")
	w.notIn["C_LEAD"] = true

	var skipped []string
	src := newSource(t, w, slacksource.WithSkipped(func(id string, _ error) {
		skipped = append(skipped, id)
	}))

	changes, _ := run(t, src, connector.Cursor{})
	if !slices.Contains(skipped, "C_LEAD") {
		t.Errorf("the run said nothing about a channel it could not read, it said %v", skipped)
	}
	for _, ch := range changes {
		if strings.HasPrefix(ch.Document.ID, "slack:C_LEAD:") {
			t.Errorf("%s was emitted from a channel the token cannot read", ch.Document.ID)
		}
	}
	if find(changes, "slack:C_GENERAL:"+w.tsOf("C_GENERAL", "gearbox")) == nil {
		t.Errorf("one unreadable channel emptied the whole sync, which emitted %v", ids(changes))
	}
}

// A listing that reported an unreadable channel as empty would be a claim that
// its documents should all be deleted, so it is an error instead.
func TestListingAChannelTheTokenCannotReadIsAnError(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	src := newSource(t, w)
	w.notIn["C_GENERAL"] = true

	err := src.Enumerate(t.Context(), func(connector.Item) bool { return true })
	if err == nil {
		t.Fatal("listing a channel the token cannot read succeeded, and the sweep would then delete everything in it")
	}
	if !strings.Contains(err.Error(), "general") {
		t.Errorf("the error does not say which channel: %v", err)
	}
}

// Making a channel private changes who may read everything in it without
// touching a single message.
func TestMakingAChannelPrivateReachesTheIndexWithoutReadingIt(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	w.post("C_GENERAL", "coolant", "Coolant pump replaced.")
	src := newSource(t, w)

	_, cursor := run(t, src, connector.Cursor{})
	before := src.Counters()

	w.makePrivate("C_GENERAL")
	changes, next := run(t, src, cursor)

	if len(changes) != 2 {
		t.Fatalf("making the channel private emitted %v, want both threads in it", ids(changes))
	}
	for _, ch := range changes {
		if !ch.PermissionsOnly {
			t.Errorf("%s came back as a content change and nothing in it changed", ch.Document.ID)
		}
		if ch.Document.Permissions.Mode != acl.ModeACL {
			t.Errorf("%s reports %v", ch.Document.ID, ch.Document.Permissions.Mode)
		}
	}
	if spent := src.Counters().Since(before); spent.Fetches != 0 {
		t.Errorf("a permission change read %d threads, and the point of it is that it reads none", spent.Fetches)
	}
	if again, _ := run(t, src, next); len(again) != 0 {
		t.Errorf("the sync after the permission change emitted %v", ids(again))
	}
}

// Slack does not report that somebody was removed from a private channel
// anywhere, and a revocation that reaches the index whenever somebody happens
// to post is not a revocation. So the rule is reapplied on a schedule.
func TestARemovedMemberIsAppliedOnTheSchedule(t *testing.T) {
	w := newWorkspace()
	w.addChannel("C_LEAD", "leadership", true)
	w.post("C_LEAD", "restructure", "The reorganisation goes out on Monday.")

	// The clock is the workspace's, so the test moves it rather than waiting.
	now := w.now()
	src := newSource(t, w,
		slacksource.WithACLRefresh(time.Hour),
		slacksource.WithClock(func() time.Time { return now }),
	)

	_, cursor := run(t, src, connector.Cursor{})
	if changes, _ := run(t, src, cursor); len(changes) != 0 {
		t.Fatalf("a second sync in the same interval emitted %v", ids(changes))
	}

	w.removeMember("C_LEAD", "U_SAM")
	// Still inside the same interval, so Slack has told us nothing and neither
	// has the schedule.
	if changes, _ := run(t, src, cursor); len(changes) != 0 {
		t.Fatalf("the removal was applied before the schedule said to, which means every sync applies it: %v", ids(changes))
	}

	now = now.Add(time.Hour)
	changes, _ := run(t, src, cursor)
	if len(changes) != 1 {
		t.Fatalf("the interval passed and the sync emitted %v", ids(changes))
	}
	if !changes[0].PermissionsOnly {
		t.Error("reapplying a rule read the thread again")
	}
	if got := changes[0].Document.Permissions.AllowUsers; len(got) != 1 || got[0].Value != "U_MEI" {
		t.Errorf("the rule now allows %v, want only the member who is still in the channel", got)
	}
}

// The reply window is the honest part of a source with no change feed. A reply
// inside it is in the sync, and a reply outside it is in the sweep.
func TestAReplyInsideTheWindowIsFoundByTheSync(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	w.post("C_GENERAL", "coolant", "Coolant pump replaced.")
	src := newSource(t, w, slacksource.WithReplyWindow(time.Hour))

	_, cursor := run(t, src, connector.Cursor{})
	w.reply("C_GENERAL", "gearbox", "U_SAM", "Bearing, I think.")

	changes, _ := run(t, src, cursor)
	want := "slack:C_GENERAL:" + w.tsOf("C_GENERAL", "gearbox")
	if got := ids(changes); !slices.Equal(got, []string{want}) {
		t.Fatalf("the sync after one reply emitted %v, want just %s", got, want)
	}
	if !strings.Contains(changes[0].Document.Body, "Bearing, I think") {
		t.Errorf("the reply is not in the body:\n%s", changes[0].Document.Body)
	}
}

// A reply to a thread older than the window does not move that thread anywhere
// Slack will report, so the change feed cannot see it. The version in the
// listing is what makes the sweep find it instead.
func TestAReplyOutsideTheWindowIsFoundByTheListing(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	// Something newer, so that the cursor lands past the gearbox thread and a
	// window of nothing leaves it out of reach of the next sync.
	w.post("C_GENERAL", "coolant", "Coolant pump replaced.")
	src := newSource(t, w, slacksource.WithReplyWindow(time.Nanosecond))

	_, cursor := run(t, src, connector.Cursor{})
	id := "slack:C_GENERAL:" + w.tsOf("C_GENERAL", "gearbox")
	was := versionOf(t, src, id)

	w.reply("C_GENERAL", "gearbox", "U_SAM", "Bearing, I think.")

	// The sync cannot see it, and that is the documented cost rather than a bug.
	if again, _ := run(t, src, cursor); len(again) != 0 {
		t.Errorf("the sync saw a reply outside the window after all, which would be better: %v", ids(again))
	}
	// The listing can, because the version is derived from the newest message.
	if now := versionOf(t, src, id); now == was {
		t.Fatalf("the listing reports version %q both before and after a reply, so the sweep can never repair it", now)
	}
	// And the repair is a real fetch of the thread with the reply in it.
	got, err := src.Fetch(t.Context(), id)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(got.Body, "Bearing, I think") {
		t.Errorf("the repaired document does not hold the reply:\n%s", got.Body)
	}
}

// versionOf reads what the listing says about one document.
func versionOf(t *testing.T, src *threadsource.Source, id string) string {
	t.Helper()
	var got string
	if err := src.Enumerate(t.Context(), func(item connector.Item) bool {
		if item.ID == id {
			got = item.Version
			return false
		}
		return true
	}); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if got == "" {
		t.Fatalf("the listing does not hold %s", id)
	}
	return got
}

// An edit changes what a document says without any reply being added, and a
// version derived only from reply times would leave the index serving the old
// text until somebody happened to answer.
func TestAnEditMovesTheVersion(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	src := newSource(t, w)

	changes, cursor := run(t, src, connector.Cursor{})
	id := changes[0].Document.ID
	was := versionOf(t, src, id)

	w.post("C_GENERAL", "gearbox", "The gearbox on line two has stopped.")

	if now := versionOf(t, src, id); now == was {
		t.Errorf("an edit left the version at %q", now)
	}
	got, _ := run(t, src, cursor)
	if len(got) != 1 || !strings.Contains(got[0].Document.Body, "has stopped") {
		t.Errorf("the sync after an edit emitted %v", ids(got))
	}
}

// A paged reply listing repeats the parent message on every page, so an adapter
// that concatenated the pages would say the first thing three times and treble
// its weight in the ranking.
func TestTheParentIsNotCountedOncePerPage(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	for _, text := range []string{"Bearing, I think.", "Replacement ordered.", "It arrived.", "Fitted and quiet."} {
		w.reply("C_GENERAL", "gearbox", "U_SAM", text)
	}
	src := newSource(t, w)

	changes, _ := run(t, src, connector.Cursor{})
	if got := changes[0].Document.Properties["replies"]; got != "4" {
		t.Errorf("the document reports %q replies over three pages, want 4", got)
	}
	if n := strings.Count(changes[0].Document.Body, "making a noise"); n != 1 {
		t.Errorf("the first message is in the body %d times", n)
	}
}

// A source that gave up on the first refusal would fail a run over a rate limit
// that Slack expects every client to hit.
func TestBeingThrottledDoesNotFailTheRun(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	src := newSource(t, w)

	w.throttle = 3
	changes, _ := run(t, src, connector.Cursor{})
	if len(changes) != 1 {
		t.Fatalf("a run that was throttled three times emitted %v", ids(changes))
	}
	if w.throttle != 0 {
		t.Errorf("%d refusals were never retried", w.throttle)
	}
}

// Slack limits per method in tiers, and the crawl uses methods from three of
// them. Sharing one bucket would either be refused at the speed of the fastest
// or crawl at the speed of the slowest.
func TestEachMethodDrawsOnItsOwnLimit(t *testing.T) {
	w := newWorkspace()
	w.addChannel("C_LEAD", "leadership", true)
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	w.post("C_LEAD", "restructure", "The reorganisation goes out on Monday.")

	svc, err := slacksource.NewService(token,
		slacksource.WithBaseURL(w.server(t)),
		slacksource.WithLimits(quick),
		slacksource.WithClock(w.now),
	)
	if err != nil {
		t.Fatal(err)
	}
	src, err := threadsource.New(svc, svc.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })

	run(t, src, connector.Cursor{})
	if got := svc.Stats().Requests; got == 0 {
		t.Fatal("the run went through no limiter at all")
	}
	for _, method := range []string{"conversations.list", "conversations.members", "conversations.history", "users.info"} {
		if w.counted(method) == 0 {
			t.Errorf("the crawl never called %s, so this test is not measuring what it says it is", method)
		}
	}
}

// A name looked up once is a name looked up once, however many messages the
// person wrote.
func TestAPersonIsLookedUpOnce(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	w.post("C_GENERAL", "coolant", "Coolant pump replaced.")
	w.reply("C_GENERAL", "gearbox", "U_MEI", "Still noisy.")
	src := newSource(t, w)

	w.resetCounts()
	run(t, src, connector.Cursor{})
	if got := w.counted("users.info"); got != 1 {
		t.Errorf("one person who wrote three messages was looked up %d times", got)
	}
}

// A lookup that fails is a display name, and failing a whole channel over one
// is worse than indexing the thread with the id in place of the name.
func TestAFailedNameLookupDoesNotFailTheChannel(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	w.fail["users.info"] = "user_not_found"

	var skipped []string
	src := newSource(t, w, slacksource.WithSkipped(func(id string, _ error) { skipped = append(skipped, id) }))

	changes, _ := run(t, src, connector.Cursor{})
	if len(changes) != 1 {
		t.Fatalf("a failed name lookup emitted %v", ids(changes))
	}
	if got := changes[0].Document.Author.Identity.Value; got != "U_MEI" {
		t.Errorf("the author's identity is %q, and that much was known without asking", got)
	}
	if !slices.Contains(skipped, "U_MEI") {
		t.Errorf("nothing was said about the failed lookup, only %v", skipped)
	}
}

// An error that is not a refusal is the caller's to see. A sync that swallowed
// one would report success and move the cursor past threads it never read.
func TestARealErrorComesBack(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	w.fail["conversations.list"] = "invalid_auth"
	src := newSource(t, w)

	_, err := src.Sync(t.Context(), connector.Cursor{}, func(context.Context, connector.Change) error { return nil })
	if err == nil {
		t.Fatal("a sync with an invalid token succeeded")
	}
	if !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("the error does not say what Slack said: %v", err)
	}
}

func TestFetchOfSomethingNoLongerThere(t *testing.T) {
	w := newWorkspace()
	w.post("C_GENERAL", "gearbox", "The gearbox on line two is making a noise.")
	src := newSource(t, w)

	changes, _ := run(t, src, connector.Cursor{})
	id := changes[0].Document.ID
	w.remove("C_GENERAL", "gearbox")

	if _, err := src.Fetch(t.Context(), id); !errors.Is(err, connector.ErrGone) {
		t.Errorf("fetching a deleted thread gave %v, want the answer that it is gone", err)
	}
}

func TestFetchOfAnIdThisWorkspaceCouldNotHaveMade(t *testing.T) {
	w := newWorkspace()
	src := newSource(t, w)

	_, err := src.Fetch(t.Context(), "slack:nonsense")
	if err == nil {
		t.Fatal("fetching a malformed id succeeded")
	}
	if errors.Is(err, connector.ErrGone) {
		t.Error("a malformed id came back as a thread this workspace used to have")
	}
}

func TestBuildingTheAdapterRejectsWhatCannotWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []slacksource.Option
		tok  string
	}{
		{name: "no token"},
		{name: "no source name", tok: token, opts: []slacksource.Option{slacksource.WithName("")}},
		{name: "no base URL", tok: token, opts: []slacksource.Option{slacksource.WithBaseURL("")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := slacksource.New(tc.tok, tc.opts...); err == nil {
				t.Error("it was built anyway")
			}
		})
	}
}
