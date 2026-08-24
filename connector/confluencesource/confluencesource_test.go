package confluencesource_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/confluencesource"
	"github.com/tamnd/genba/connector/connectortest"
	"github.com/tamnd/genba/connector/limit"
	"github.com/tamnd/genba/connector/threadsource"
	"github.com/tamnd/genba/doc"
)

// The account the crawl runs as, which is a service account rather than a
// person. It is deliberately not one of the accounts in the fake, because the
// recording is checked for the crawling credential and a check that matched a
// display email on a page would pass for the wrong reason.
const (
	email = "search-crawler@acme.com"
	token = "a-real-looking-api-token"
)

// quick is a rate limit that does not slow a test down while still being the
// real limiter, retries, backoff, circuit breaker and all.
var quick = limit.Limits{Rate: 10000, Burst: 50, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}

func newSource(t *testing.T, s *site, opts ...confluencesource.Option) *threadsource.Source {
	t.Helper()
	all := append([]confluencesource.Option{
		confluencesource.WithLimits(quick),
		confluencesource.WithClock(s.now),
		// An hour rather than a day, so that a test that wants to be on the far
		// side of a refresh interval can move the clock rather than wait a day.
		confluencesource.WithACLRefresh(time.Hour),
	}, opts...)
	src, err := confluencesource.New(s.server(t)+"/wiki", email, token, all...)
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

// quarantined reports whether a document was indexed with nobody able to read
// it, which is what a connector that could not work out an access control list
// is required to produce rather than a guess.
func quarantined(p acl.Permissions) bool { return p.Mode == acl.ModeUnknown }

func values(refs []acl.Ref) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Value)
	}
	return out
}

func enumerate(t *testing.T, src *threadsource.Source) []string {
	t.Helper()
	var out []string
	if err := src.Enumerate(t.Context(), func(item connector.Item) bool {
		out = append(out, item.ID)
		return true
	}); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	slices.Sort(out)
	return out
}

// The conformance suite is what decides whether this is a connector, and
// everything else in this file is about the parts that are specific to
// Confluence.
func TestConformance(t *testing.T) {
	connectortest.Run(t, func(t *testing.T) connectortest.Fixture {
		s := newSite()
		src := newSource(t, s)
		return connectortest.Fixture{
			Connector: src,
			ID:        func(name string) string { return "confluence:" + s.idOf(name) },
			Write:     func(_ *testing.T, name, body string) { s.write(name, body) },
			Remove:    func(_ *testing.T, name string) { s.remove(s.idOf(name)) },
			Share: func(_ *testing.T, _ string) {
				// A space's rule changes and not one page in it is touched, which
				// is the case the schedule exists for. Nothing in Confluence
				// reports the edit, so the clock is what carries it.
				a := s.space(home)
				a.groups = []string{"operations"}
				a.users = nil
				s.advance(time.Hour)
			},
			// A read restriction the site sends only part of is the concrete case
			// of an access control list beyond working out, and a page
			// restriction is the reason this connector can override a container's
			// rule at all.
			Unresolvable: func(_ *testing.T, name string) { s.truncate(s.idOf(name)) },
		}
	})
}

// A page and the argument underneath it are one document. The page says the
// deploy runs at nine and the third comment says it moved to eleven, and a page
// indexed without them answers with the wrong time.
func TestAPageAndItsCommentsAreOneDocument(t *testing.T) {
	s := newSite()
	id := s.create(home, "Deploy", "The deploy runs at nine.")
	s.comment(id, "acc-sam", "It moved to eleven in March.")
	s.comment(id, "acc-lee", "Confirmed, eleven.")
	s.create(home, "Rollback", "Roll back with the previous tag.")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	// Two documents and not four. A comment is a sentence in a page rather than
	// a document, and the page is what gets indexed. The page arrives more than
	// once because each comment on it is a change to be walked past, which costs
	// the index an overwrite with the same bytes and is what moves the cursor.
	if got := distinct(ids(changes)); len(got) != 2 {
		t.Fatalf("a space with two pages and two comments produced %d documents: %v", len(got), got)
	}

	ch := find(changes, "confluence:"+id)
	if ch == nil {
		t.Fatalf("the page with the comments on it was not emitted: %v", ids(changes))
	}
	for _, want := range []string{"runs at nine", "moved to eleven", "Confirmed, eleven"} {
		if !strings.Contains(ch.Document.Body, want) {
			t.Errorf("the document does not say %q:\n%s", want, ch.Document.Body)
		}
	}
	if ch.Document.Kind != doc.KindPage {
		t.Errorf("a wiki page was indexed as %v", ch.Document.Kind)
	}
	if ch.Document.Title != "Deploy" {
		t.Errorf("the document is titled %q rather than after its page", ch.Document.Title)
	}
}

// Confluence dates a comment and leaves the page it is on alone, so a page
// answered and never edited again is a page a query about pages reports as
// unchanged for ever. Asking about comments as well is the whole of the fix, and
// what comes back is still the page.
func TestACommentBringsBackThePageItIsOn(t *testing.T) {
	s := newSite()
	id := s.create(home, "Deploy", "The deploy runs at nine.")
	src := newSource(t, s)

	_, cursor := run(t, src, connector.Cursor{})
	s.comment(id, "acc-sam", "It moved to eleven in March.")

	changes, _ := run(t, src, cursor)
	if got := distinct(ids(changes)); len(got) != 1 {
		t.Fatalf("a comment on an unedited page produced %d documents: %v", len(got), got)
	}
	ch := changes[0]
	if ch.Document.ID != "confluence:"+id {
		t.Errorf("a comment came back as %s rather than as the page it is on", ch.Document.ID)
	}
	if !strings.Contains(ch.Document.Body, "moved to eleven") {
		t.Errorf("the page came back without the comment that brought it back:\n%s", ch.Document.Body)
	}
}

// A page edited and then commented on inside one sync window is one page, and
// reading it twice is a request nobody needed.
func TestAPageEditedAndCommentedOnIsReadOnce(t *testing.T) {
	s := newSite()
	id := s.create(home, "Deploy", "The deploy runs at nine.")
	src := newSource(t, s)

	_, cursor := run(t, src, connector.Cursor{})
	s.write("Deploy", "The deploy runs at eleven.")
	s.comment(id, "acc-sam", "Fixed the time above.")
	s.resetCounts()

	changes, _ := run(t, src, cursor)
	if len(changes) < 1 {
		t.Fatal("a page that was edited and commented on came back as nothing")
	}
	for _, ch := range changes {
		if ch.Document.ID != "confluence:"+id {
			t.Errorf("something other than the page came back: %s", ch.Document.ID)
		}
	}
	if got := s.counted("/rest/api/content/" + id); got != 0 {
		t.Errorf("the page was read %d times by id, and the search that found it already carried it", got)
	}
}

// The body of a page written before the editor changed is the storage format,
// and there are a great many of those on any site old enough to be worth
// searching. A connector that could not read them would index a wiki as the
// pages somebody has touched since the migration.
func TestAPageWrittenInTheOldEditorIsStillAPage(t *testing.T) {
	s := newSite()
	id := s.create(home, "Runbook", "placeholder")
	s.legacy(id, `<h2>Restart</h2><p>Run <code>make deploy</code> and wait.</p>`)
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	ch := find(changes, "confluence:"+id)
	if ch == nil {
		t.Fatalf("a page with a storage format body was not emitted: %v", ids(changes))
	}
	for _, want := range []string{"## Restart", "`make deploy`"} {
		if !strings.Contains(ch.Document.Body, want) {
			t.Errorf("the rendered body does not have %q in it:\n%s", want, ch.Document.Body)
		}
	}
}

// The storage format is not asked for on the crawl, because asking means the
// body of every page on a migrated site arriving twice. It is asked for one page
// at a time, only for the pages that came back with nothing.
func TestTheOldBodyIsAskedForOnlyForThePagesThatNeedIt(t *testing.T) {
	s := newSite()
	old := s.create(home, "Runbook", "placeholder")
	s.legacy(old, `<p>Run make deploy.</p>`)
	s.create(home, "Deploy", "The deploy runs at nine.")
	s.create(home, "Rollback", "Roll back with the previous tag.")
	src := newSource(t, s)

	run(t, src, connector.Cursor{})

	if got := s.counted("/rest/api/content/" + old); got != 1 {
		t.Errorf("the page with no Atlassian document was read %d extra times, want one", got)
	}
	if strings.Contains(s.lastExpand("/rest/api/content/search"), "body.storage") {
		t.Errorf("the crawl asked for both body formats: %s", s.lastExpand("/rest/api/content/search"))
	}
	for _, name := range []string{"Deploy", "Rollback"} {
		if got := s.counted("/rest/api/content/" + s.idOf(name)); got != 0 {
			t.Errorf("the page %s came back whole and was read %d more times", name, got)
		}
	}
}

// A space's read permission is a list of accounts and a list of groups, and that
// is an access control list. Everything in the space inherits it.
func TestASpacesPermissionsBecomeTheRuleOnItsPages(t *testing.T) {
	s := newSite()
	s.create(home, "Deploy", "The deploy runs at nine.")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	if len(changes) != 1 {
		t.Fatalf("a space with one page produced %v", ids(changes))
	}

	p := changes[0].Document.Permissions
	if p.Mode != acl.ModeACL {
		t.Fatalf("a page in a space with a read permission on it was given mode %v", p.Mode)
	}
	if got, want := values(p.AllowUsers), []string{"acc-mei"}; !slices.Equal(got, want) {
		t.Errorf("the rule allows the accounts %v, want %v", got, want)
	}
	if got, want := values(p.AllowGroups), []string{"engineering"}; !slices.Equal(got, want) {
		t.Errorf("the rule allows the groups %v, want %v", got, want)
	}
}

// A space readable without signing in is a space that is open, not a space with
// an empty rule. An adapter that ignored the flag would hide the space instead
// of describing it.
func TestASpaceAnybodyCanReadIsOpenRatherThanEmpty(t *testing.T) {
	s := newSite()
	a := s.space(home)
	a.open = true
	s.create(home, "Deploy", "The deploy runs at nine.")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	if len(changes) != 1 {
		t.Fatalf("a space with one page produced %v", ids(changes))
	}
	if got := changes[0].Document.Permissions.Mode; got != acl.ModePublicToTenant {
		t.Errorf("a page in an anonymously readable space was given mode %v", got)
	}
}

// A space whose permissions this token was not shown looks exactly like a space
// with no permissions on it, and the two are the same answer here: we do not
// know, so say so rather than publishing it.
func TestASpaceWhosePermissionsAreNotReadableIsQuarantined(t *testing.T) {
	s := newSite()
	s.space(home).dark = true
	s.create(home, "Deploy", "The deploy runs at nine.")

	var skipped []string
	src := newSource(t, s, confluencesource.WithSkipped(func(id string, _ error) {
		skipped = append(skipped, id)
	}))

	changes, _ := run(t, src, connector.Cursor{})
	if len(changes) != 1 {
		t.Fatalf("a space nobody could read the permissions of produced %v", ids(changes))
	}
	if !quarantined(changes[0].Document.Permissions) {
		t.Errorf("a page in a space with unreadable permissions was given mode %v", changes[0].Document.Permissions.Mode)
	}
	if !slices.Contains(skipped, home) {
		t.Errorf("nothing was reported about the space nobody could resolve, and an index quietly missing what nobody could read looks like an index that is complete, got %v", skipped)
	}
}

// A read restriction on a page replaces the space's rule rather than adding to
// it, which is what a restriction is and is the reason this connector overrides
// its container at all.
func TestAPageRestrictionReplacesTheSpacesRule(t *testing.T) {
	s := newSite()
	open := s.create(home, "Deploy", "The deploy runs at nine.")
	shut := s.create(home, "Incident", "The pump failed on Tuesday.")
	s.restrict(shut, []string{"acc-lee"}, nil)
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})

	was := find(changes, "confluence:"+open)
	if was == nil {
		t.Fatalf("the unrestricted page was not emitted: %v", ids(changes))
	}
	if got, want := values(was.Document.Permissions.AllowGroups), []string{"engineering"}; !slices.Equal(got, want) {
		t.Errorf("the unrestricted page allows the groups %v, want the space's %v", got, want)
	}

	now := find(changes, "confluence:"+shut)
	if now == nil {
		t.Fatalf("the restricted page was not emitted: %v", ids(changes))
	}
	if got, want := values(now.Document.Permissions.AllowUsers), []string{"acc-lee"}; !slices.Equal(got, want) {
		t.Errorf("the restricted page allows the accounts %v, want %v", got, want)
	}
	if got := values(now.Document.Permissions.AllowGroups); len(got) != 0 {
		t.Errorf("the restricted page kept the space's groups %v, and a restriction replaces the space rather than adding to it", got)
	}
}

// Restrictions inherit. A page with no restriction of its own under a parent
// that has one is restricted, and an adapter that only looked at the page would
// publish every page in a restricted section.
func TestARestrictionOnAParentReachesThePagesUnderIt(t *testing.T) {
	s := newSite()
	top := s.create(home, "Incidents", "Everything that went wrong.")
	mid := s.create(home, "March", "What went wrong in March.")
	low := s.create(home, "Pump failure", "The pump failed on Tuesday.")
	s.nest(mid, top)
	s.nest(low, mid)
	s.restrict(top, []string{"acc-lee"}, nil)
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	for _, id := range []string{top, mid, low} {
		ch := find(changes, "confluence:"+id)
		if ch == nil {
			t.Fatalf("a page in the restricted tree was not emitted: %v", ids(changes))
		}
		if got, want := values(ch.Document.Permissions.AllowUsers), []string{"acc-lee"}; !slices.Equal(got, want) {
			t.Errorf("%s allows the accounts %v, want the restriction above it, %v", ch.Document.ID, got, want)
		}
	}
}

// A chain carrying a restriction at more than one level is quarantined rather
// than resolved. Confluence means the intersection of the two, and an
// intersection of a list of accounts with a list of groups is not something that
// can be worked out from outside the identity provider, so publishing the wider
// of the two would publish exactly the page somebody restricted.
func TestAPageRestrictedAtTwoLevelsIsQuarantined(t *testing.T) {
	s := newSite()
	top := s.create(home, "Incidents", "Everything that went wrong.")
	low := s.create(home, "Pump failure", "The pump failed on Tuesday.")
	s.nest(low, top)
	s.restrict(top, nil, []string{"operations"})
	s.restrict(low, []string{"acc-lee"}, nil)

	var skipped []string
	src := newSource(t, s, confluencesource.WithSkipped(func(id string, _ error) {
		skipped = append(skipped, id)
	}))

	changes, _ := run(t, src, connector.Cursor{})
	ch := find(changes, "confluence:"+low)
	if ch == nil {
		t.Fatalf("the doubly restricted page was not emitted: %v", ids(changes))
	}
	if !quarantined(ch.Document.Permissions) {
		t.Errorf("a page restricted at two levels was given mode %v and the accounts %v, which is one of the two rules rather than the intersection Confluence means",
			ch.Document.Permissions.Mode, values(ch.Document.Permissions.AllowUsers))
	}
	if !slices.Contains(skipped, low) {
		t.Errorf("nothing was reported about the page nobody could resolve, got %v", skipped)
	}
}

// The tree above a page is walked once per crawl and not once per page. A
// restricted section is a section where the same handful of ancestors is asked
// about for every page in it, and on a real space that is most of the crawl.
func TestTheTreeAboveAPageIsAskedAboutOnce(t *testing.T) {
	s := newSite()
	top := s.create(home, "Incidents", "Everything that went wrong.")
	s.restrict(top, []string{"acc-lee"}, nil)
	for _, title := range []string{"March", "April", "May", "June"} {
		s.nest(s.create(home, title, "What went wrong in "+title+"."), top)
	}
	src := newSource(t, s)
	s.resetCounts()

	run(t, src, connector.Cursor{})

	// The pages carry their own restriction on the listing, so the only thing
	// that has to be asked about is the page above them.
	if got := s.counted("/rest/api/content/" + top + "/restriction/byOperation/read"); got > 1 {
		t.Errorf("the page above four restricted pages was asked about %d times, want at most one", got)
	}
}

// A restriction the site sent half of is a restriction we do not know. Indexing
// a page against the half that arrived is how somebody named on the second page
// of the list stops being able to find their own document.
func TestARestrictionSentInPartIsQuarantined(t *testing.T) {
	s := newSite()
	id := s.create(home, "Incident", "The pump failed on Tuesday.")
	s.truncate(id)
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	ch := find(changes, "confluence:"+id)
	if ch == nil {
		t.Fatalf("the page with the half sent restriction was not emitted: %v", ids(changes))
	}
	if !quarantined(ch.Document.Permissions) {
		t.Errorf("a page whose restriction arrived in part was indexed against the part that arrived, mode %v accounts %v",
			ch.Document.Permissions.Mode, values(ch.Document.Permissions.AllowUsers))
	}
}

// A restriction the site declines to answer for is the one thing that must not
// fall back to the space, because the space is the wider rule and falling back
// to it publishes the page.
//
// The page above is the one that has to be asked about here, and an incremental
// sync is where that happens: the parent did not change, so it is not in the
// sync, and the only way to know whether it restricts the child that did change
// is to ask.
func TestARestrictionThatCannotBeReadDoesNotFallBackToTheSpace(t *testing.T) {
	s := newSite()
	top := s.create(home, "Incidents", "Everything that went wrong.")
	low := s.create(home, "Pump failure", "The pump failed on Tuesday.")
	s.nest(low, top)
	src := newSource(t, s)

	_, cursor := run(t, src, connector.Cursor{})
	s.write("Pump failure", "The pump failed on Tuesday afternoon.")
	s.refuse("/rest/api/content/"+top+"/restriction/byOperation/read", http.StatusInternalServerError)

	changes, _ := run(t, src, cursor)
	ch := find(changes, "confluence:"+low)
	if ch == nil {
		t.Fatalf("the page under the unreadable restriction was not emitted: %v", ids(changes))
	}
	if !quarantined(ch.Document.Permissions) {
		t.Errorf("a page whose parent's restriction could not be read was given the space's rule, mode %v groups %v",
			ch.Document.Permissions.Mode, values(ch.Document.Permissions.AllowGroups))
	}
}

// A restriction put on a page above is a revocation, and nothing in Confluence
// reports it: the parent's version does not move and neither does the child's.
// What reaches the index is the next time the child is crawled for any reason,
// and it has to reach it then rather than when the process next restarts.
func TestARestrictionAddedAboveReachesTheNextCrawl(t *testing.T) {
	s := newSite()
	top := s.create(home, "Incidents", "Everything that went wrong.")
	low := s.create(home, "Pump failure", "The pump failed on Tuesday.")
	s.nest(low, top)
	src := newSource(t, s)

	changes, cursor := run(t, src, connector.Cursor{})
	if ch := find(changes, "confluence:"+low); ch == nil || ch.Document.Permissions.Mode != acl.ModeACL {
		t.Fatalf("the unrestricted page did not start out with the space's rule: %v", ids(changes))
	}

	s.restrict(top, []string{"acc-lee"}, nil)
	s.write("Pump failure", "The pump failed on Tuesday afternoon.")

	changes, _ = run(t, src, cursor)
	ch := find(changes, "confluence:"+low)
	if ch == nil {
		t.Fatalf("the edited page was not emitted: %v", ids(changes))
	}
	if got, want := values(ch.Document.Permissions.AllowUsers), []string{"acc-lee"}; !slices.Equal(got, want) {
		t.Errorf("the page allows the accounts %v after its parent was restricted, want %v", got, want)
	}
}

// The listing is what the sweep compares against, and it carries a page's own
// rule so that a scheduled permission refresh does not write the space's rule
// over a page that was restricted out of the space. Getting that without reading
// the pages is the whole point: a refresh that had to read a space would be a
// recrawl, which is what the refresh exists instead of.
func TestTheListingCarriesARuleAndNoBodies(t *testing.T) {
	s := newSite()
	shut := s.create(home, "Incident", "The pump failed on Tuesday.")
	s.restrict(shut, []string{"acc-lee"}, nil)
	s.create(home, "Deploy", "The deploy runs at nine.")
	src := newSource(t, s)

	run(t, src, connector.Cursor{})
	s.resetCounts()

	if got, want := enumerate(t, src), []string{"confluence:" + s.idOf("Deploy"), "confluence:" + shut}; !slices.Equal(got, sorted(want)) {
		t.Errorf("the enumeration lists %v, want %v", got, sorted(want))
	}
	if got := s.lastExpand("/rest/api/content/search"); strings.Contains(got, "body") {
		t.Errorf("the listing asked for page bodies: %s", got)
	}
	if got := s.counted("/rest/api/content/" + shut); got != 0 {
		t.Errorf("the listing read %d page bodies, and a sweep that reads the space is a recrawl", got)
	}
}

// The schedule reapplies a space's rule to the pages in it because nothing in
// Confluence reports that a space's permissions changed. What it must not do is
// reapply it over a page that has a rule of its own.
func TestTheScheduleDoesNotWriteASpacesRuleOverARestriction(t *testing.T) {
	s := newSite()
	open := s.create(home, "Deploy", "The deploy runs at nine.")
	shut := s.create(home, "Incident", "The pump failed on Tuesday.")
	s.restrict(shut, []string{"acc-lee"}, nil)
	src := newSource(t, s)

	_, cursor := run(t, src, connector.Cursor{})

	a := s.space(home)
	a.groups = []string{"operations"}
	a.users = nil
	s.advance(time.Hour)

	before := src.Counters()
	changes, _ := run(t, src, cursor)
	spent := src.Counters().Since(before)

	was := find(changes, "confluence:"+open)
	if was == nil {
		t.Fatalf("the page that inherits the space did not get the new rule: %v", ids(changes))
	}
	if got, want := values(was.Document.Permissions.AllowGroups), []string{"operations"}; !slices.Equal(got, want) {
		t.Errorf("the unrestricted page allows the groups %v after the space changed, want %v", got, want)
	}

	now := find(changes, "confluence:"+shut)
	if now == nil {
		t.Fatal("the restricted page was not touched by the refresh, which is a correct answer, so there is nothing more to check here")
	}
	if got, want := values(now.Document.Permissions.AllowUsers), []string{"acc-lee"}; !slices.Equal(got, want) {
		t.Errorf("the restricted page allows the accounts %v after the space changed, want its own rule, %v", got, want)
	}
	if spent.Fetches != 0 {
		t.Errorf("reapplying a space's rule read %d pages, and a refresh that reads the pages is a recrawl", spent.Fetches)
	}
}

// A page put away is a page the wiki is not offering anybody. Nothing in CQL
// reports that it happened, which is what the sweep is for.
func TestAnArchivedPageStopsBeingPartOfTheSpace(t *testing.T) {
	s := newSite()
	id := s.create(home, "Deploy", "The deploy runs at nine.")
	s.create(home, "Rollback", "Roll back with the previous tag.")
	src := newSource(t, s)

	run(t, src, connector.Cursor{})
	s.archive(id)

	if got := enumerate(t, src); slices.Contains(got, "confluence:"+id) {
		t.Errorf("an archived page is still listed as part of the space: %v", got)
	}
}

// An archived page is still readable by its id, and a fetch that handed one back
// would put it in the index by the back door.
func TestFetchingAnArchivedPageReportsItGone(t *testing.T) {
	s := newSite()
	id := s.create(home, "Deploy", "The deploy runs at nine.")
	src := newSource(t, s)

	run(t, src, connector.Cursor{})
	s.archive(id)

	if _, err := src.Fetch(t.Context(), "confluence:"+id); !errors.Is(err, connector.ErrGone) {
		t.Errorf("fetching an archived page returned %v, want it reported as gone", err)
	}
}

// The last editor is a property rather than a second author, because "pages Sam
// wrote" and "pages Sam last touched" are different questions and an index that
// conflated them would answer neither.
func TestAPageKeepsItsAuthorAndItsLastEditorApart(t *testing.T) {
	s := newSite()
	id := s.create(home, "Deploy", "The deploy runs at nine.")
	s.find(id).author = "acc-mei"
	s.label(id, "runbook", "deploy")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	ch := find(changes, "confluence:"+id)
	if ch == nil {
		t.Fatalf("the page was not emitted: %v", ids(changes))
	}
	if got := ch.Document.Author.Subject; got != "acc-mei" {
		t.Errorf("the document is attributed to %q rather than to the account that wrote the page", got)
	}
	if got := ch.Document.Properties["editor"]; got != "Mei Tanaka" {
		t.Errorf("the last editor is recorded as %q", got)
	}
	if got := ch.Document.Properties["labels"]; got != "deploy, runbook" && got != "runbook, deploy" {
		t.Errorf("the labels are recorded as %q", got)
	}
}

// CQL compares times to the minute and the cursor does not, so a query asked for
// the minute the cursor is in comes back with what changed in the first half of
// it as well. Rounding down is deliberate, because the direction that costs a
// re-read is the one that does not lose pages.
func TestASecondSyncOfAnUnchangedSpaceEmitsNothing(t *testing.T) {
	s := newSite()
	s.create(home, "Deploy", "The deploy runs at nine.")
	s.create(home, "Rollback", "Roll back with the previous tag.")
	src := newSource(t, s)

	_, cursor := run(t, src, connector.Cursor{})
	changes, _ := run(t, src, cursor)
	if len(changes) != 0 {
		t.Errorf("a second sync of an unchanged space emitted %v", ids(changes))
	}
}

// The address of a page has its title in it and a title is a thing people
// change, so where a person goes to read it is what the site says rather than
// something built here.
func TestThePageAddressIsTheOneTheSiteGave(t *testing.T) {
	s := newSite()
	id := s.create(home, "Deploy", "The deploy runs at nine.")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	ch := find(changes, "confluence:"+id)
	if ch == nil {
		t.Fatalf("the page was not emitted: %v", ids(changes))
	}
	if !strings.HasSuffix(ch.Document.URL, "/wiki/spaces/"+home+"/pages/"+id+"/Deploy") {
		t.Errorf("the page address is %q, which is not what the site said it was", ch.Document.URL)
	}
}

// A site address is what somebody has in front of them, and that is either the
// bare host or the host with /wiki on it. Both have to work, because getting it
// wrong means every request 404s and the error says nothing about why.
func TestEitherFormOfTheSiteAddressWorks(t *testing.T) {
	for _, suffix := range []string{"", "/wiki", "/wiki/", "/"} {
		t.Run("site"+suffix, func(t *testing.T) {
			s := newSite()
			id := s.create(home, "Deploy", "The deploy runs at nine.")

			src, err := confluencesource.New(s.server(t)+suffix, email, token,
				confluencesource.WithLimits(quick), confluencesource.WithClock(s.now))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = src.Close() })

			changes, _ := run(t, src, connector.Cursor{})
			if find(changes, "confluence:"+id) == nil {
				t.Errorf("a site address ending %q found %v", suffix, ids(changes))
			}
		})
	}
}

// The credentials go in a header and never in a query string, because a token in
// a query string ends up in a server log, in a recording and in a bug report.
func TestTheTokenNeverGoesInAQueryString(t *testing.T) {
	s := newSite()
	s.create(home, "Deploy", "The deploy runs at nine.")

	var asked []string
	seen := &recorder{next: http.DefaultTransport, saw: func(u string) { asked = append(asked, u) }}
	src, err := confluencesource.New(s.server(t), email, token,
		confluencesource.WithHTTPClient(&http.Client{Transport: seen}),
		confluencesource.WithClock(s.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })

	// The client here has no credentials on it, so the site refuses, which is
	// itself the check that the header is the only place they were ever going.
	_, _ = src.Sync(t.Context(), connector.Cursor{}, func(context.Context, connector.Change) error { return nil })

	if len(asked) == 0 {
		t.Fatal("no requests were made")
	}
	for _, u := range asked {
		if strings.Contains(u, token) || strings.Contains(u, email) {
			t.Errorf("a request carried the credentials in its address: %s", u)
		}
	}
}

type recorder struct {
	next http.RoundTripper
	saw  func(string)
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.saw(req.URL.String())
	return r.next.RoundTrip(req)
}

// Confluence asks a crawler to wait with a 429 and a Retry-After, and obeying
// that is the limiter's job rather than this adapter's. What is checked here is
// that the adapter is on the limiter at all.
func TestASiteAskingForAPauseIsWaitedFor(t *testing.T) {
	s := newSite()
	id := s.create(home, "Deploy", "The deploy runs at nine.")
	src := newSource(t, s)
	s.slowDown(3)

	changes, _ := run(t, src, connector.Cursor{})
	if find(changes, "confluence:"+id) == nil {
		t.Errorf("a site that asked for a pause three times lost the sync: %v", ids(changes))
	}
}

// An id this source could never have produced is a caller error rather than a
// document that went away, and the two have to be told apart: one is a bug and
// the other is Tuesday.
func TestAnIdThisSourceNeverMintedIsNotAMissingPage(t *testing.T) {
	s := newSite()
	src := newSource(t, s)

	_, err := src.Fetch(t.Context(), "confluence:not-a-number")
	switch {
	case err == nil:
		t.Fatal("fetching a malformed id succeeded")
	case errors.Is(err, connector.ErrGone):
		t.Fatal("a malformed id was reported as a page that went away, which a sweep would act on")
	}
}

// sorted is a copy of a list in order, for comparing against something the
// source chose the order of.
func sorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// distinct is how many different documents a sync was about, which is not the
// same as how many changes it emitted. A page arrives once per change to be
// walked past, and a comment is a change to the page.
func distinct(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
