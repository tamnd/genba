package jirasource_test

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
	"github.com/tamnd/genba/connector/connectortest"
	"github.com/tamnd/genba/connector/jirasource"
	"github.com/tamnd/genba/connector/limit"
	"github.com/tamnd/genba/connector/threadsource"
	"github.com/tamnd/genba/doc"
)

// The account the crawl runs as, which is a service account rather than a
// person. It is deliberately not one of the accounts in the fake, because the
// recording is checked for the crawling credential and a check that matched a
// display email on a ticket would fail for the wrong reason.
const (
	email = "search-crawler@acme.com"
	token = "a-real-looking-api-token"
)

// quick is a rate limit that does not slow a test down while still being the
// real limiter, retries, backoff, circuit breaker and all.
var quick = limit.Limits{Rate: 10000, Burst: 50, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}

func newSource(t *testing.T, s *site, opts ...jirasource.Option) *threadsource.Source {
	t.Helper()
	all := append([]jirasource.Option{
		jirasource.WithLimits(quick),
		jirasource.WithClock(s.now),
		// An hour rather than a day, so that a test that wants to be on the far
		// side of a refresh interval can move the clock rather than wait a day.
		jirasource.WithACLRefresh(time.Hour),
	}, opts...)
	src, err := jirasource.New(s.server(t), email, token, all...)
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
// everything else in this file is about the parts that are specific to Jira.
func TestConformance(t *testing.T) {
	connectortest.Run(t, func(t *testing.T) connectortest.Fixture {
		s := newSite()
		src := newSource(t, s)
		return connectortest.Fixture{
			Connector: src,
			ID:        func(name string) string { return "jira:" + s.keyOf(name) },
			Write:     func(_ *testing.T, name, body string) { s.write(name, body) },
			Remove:    func(_ *testing.T, name string) { s.remove(s.keyOf(name)) },
			Share: func(_ *testing.T, _ string) {
				// A project's rule changes and not one issue in it is touched,
				// which is the case the schedule exists for. Nothing in Jira
				// reports the edit, so the clock is what carries it.
				p := s.project(home)
				p.grants = []grant{{"group", "engineering"}}
				s.advance(time.Hour)
			},
			// A security level nobody can resolve is the concrete case of an
			// access control list beyond working out, and it is the reason this
			// connector can override a container's rule at all.
			Unresolvable: func(_ *testing.T, name string) { s.restrict(s.keyOf(name), "no-such-level") },
		}
	})
}

// An issue and the argument underneath it are one document. Somebody asking
// what was decided about the gearbox wants the ticket, not the fourth comment
// on it with no idea what it is replying to.
func TestAnIssueAndItsCommentsAreOneDocument(t *testing.T) {
	s := newSite()
	key := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	s.comment(key, "acc-sam", "Bearing, I think.")
	s.comment(key, "acc-lee", "Replacement ordered, arrives Thursday.")
	s.file(home, "Coolant pump", "Coolant pump replaced.")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	if len(changes) != 2 {
		t.Fatalf("a project with two issues and two comments produced %d documents: %v", len(changes), ids(changes))
	}

	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	for _, want := range []string{"making a noise", "Bearing, I think", "arrives Thursday"} {
		if !strings.Contains(got.Document.Body, want) {
			t.Errorf("the body does not hold %q:\n%s", want, got.Document.Body)
		}
	}
	// Who said each thing goes in as well, which is the difference between
	// searching for what Lee said about the gearbox and only being able to
	// search for the gearbox.
	for _, want := range []string{"Mei Tanaka", "Sam Okafor", "Lee Berger"} {
		if !strings.Contains(got.Document.Body, want) {
			t.Errorf("the body does not name %q:\n%s", want, got.Document.Body)
		}
	}
	if got.Document.Kind != doc.KindTicket {
		t.Errorf("the document is a %q, want a ticket", got.Document.Kind)
	}
	if !strings.HasPrefix(got.Document.Title, key+" ") {
		t.Errorf("the title is %q, and a ticket without its key in the title is a ticket nobody can quote", got.Document.Title)
	}
}

// The reporter is the author and the assignee is a property, because "tickets
// Sam reported" and "tickets Sam is working on" are different questions.
func TestTheReporterIsTheAuthorAndTheAssigneeIsAProperty(t *testing.T) {
	s := newSite()
	key := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	s.assign(key, "acc-sam")
	s.label(key, "line-two", "mechanical")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if got.Document.Author.Name != "Mei Tanaka" {
		t.Errorf("the author is %q, want the reporter", got.Document.Author.Name)
	}
	for name, want := range map[string]string{
		"assignee": "Sam Okafor",
		"status":   "To Do",
		"type":     "Bug",
		"priority": "Medium",
		"labels":   "line-two, mechanical",
	} {
		if got := got.Document.Properties[name]; got != want {
			t.Errorf("the %s property is %q, want %q", name, got, want)
		}
	}
}

// A description is a document tree rather than a sentence, and what is in it is
// very often the answer: a stack trace, a snippet of configuration, a table of
// what was tried. Flattening it answers the query and fails the person.
func TestADescriptionKeepsItsStructure(t *testing.T) {
	s := newSite()
	key := s.file(home, "Gearbox noise", "placeholder")
	s.describe(key, map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []any{
			map[string]any{
				"type":    "heading",
				"attrs":   map[string]any{"level": 2},
				"content": []any{map[string]any{"type": "text", "text": "What we tried"}},
			},
			map[string]any{
				"type": "bulletList",
				"content": []any{
					map[string]any{"type": "listItem", "content": []any{
						map[string]any{"type": "paragraph", "content": []any{
							map[string]any{"type": "text", "text": "Replaced the bearing"},
						}},
					}},
					map[string]any{"type": "listItem", "content": []any{
						map[string]any{"type": "paragraph", "content": []any{
							map[string]any{"type": "text", "text": "Checked the alignment"},
						}},
					}},
				},
			},
			map[string]any{
				"type":  "codeBlock",
				"attrs": map[string]any{"language": "go"},
				"content": []any{map[string]any{
					"type": "text",
					"text": "panic: runtime error: index out of range [3]",
				}},
			},
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "See "},
					map[string]any{
						"type":  "text",
						"text":  "the runbook",
						"marks": []any{map[string]any{"type": "link", "attrs": map[string]any{"href": "https://wiki.acme.com/runbook"}}},
					},
					map[string]any{"type": "text", "text": " and "},
					map[string]any{
						"type":  "text",
						"text":  "restart --hard",
						"marks": []any{map[string]any{"type": "code"}},
					},
				},
			},
		},
	})
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	for _, want := range []string{
		"## What we tried",
		"- Replaced the bearing",
		"- Checked the alignment",
		"```go",
		"panic: runtime error: index out of range [3]",
		"[the runbook](https://wiki.acme.com/runbook)",
		"`restart --hard`",
	} {
		if !strings.Contains(got.Document.Body, want) {
			t.Errorf("the rendered description does not hold %q:\n%s", want, got.Document.Body)
		}
	}
}

// A table survives as a table, because a ticket whose description is a table of
// readings is a ticket whose point is the table.
func TestATableSurvivesAsATable(t *testing.T) {
	s := newSite()
	key := s.file(home, "Readings", "placeholder")
	s.describe(key, map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []any{map[string]any{
			"type": "table",
			"content": []any{
				row("Shift", "Temperature"),
				row("Morning", "62"),
				row("Night", "71"),
			},
		}},
	})
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	for _, want := range []string{
		"| Shift | Temperature |",
		"| --- | --- |",
		"| Night | 71 |",
	} {
		if !strings.Contains(got.Document.Body, want) {
			t.Errorf("the rendered table does not hold %q:\n%s", want, got.Document.Body)
		}
	}
}

// row is one table row of plain text cells.
func row(cells ...string) map[string]any {
	out := make([]any, 0, len(cells))
	for _, c := range cells {
		out = append(out, map[string]any{
			"type": "tableCell",
			"content": []any{map[string]any{
				"type":    "paragraph",
				"content": []any{map[string]any{"type": "text", "text": c}},
			}},
		})
	}
	return map[string]any{"type": "tableRow", "content": out}
}

// This is the thing chat cannot do. A ticket moved from one column to another
// changed, nobody wrote a word, and Jira says so, so the sync finds it without
// a window to widen or a guess to make.
func TestAStatusChangeWithNoCommentIsFoundBySync(t *testing.T) {
	s := newSite()
	key := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	src := newSource(t, s)

	_, cursor := run(t, src, connector.Cursor{})
	if changes, _ := run(t, src, cursor); len(changes) != 0 {
		t.Fatalf("a second sync of a project nothing happened in emitted %v", ids(changes))
	}

	s.transition(key, "In Progress")

	changes, _ := run(t, src, cursor)
	if len(changes) != 1 {
		t.Fatalf("a status change emitted %d documents: %v", len(changes), ids(changes))
	}
	if got := changes[0].Document.Properties["status"]; got != "In Progress" {
		t.Errorf("the status is %q, want the one it was moved to", got)
	}
}

// A security level replaces the project's rule rather than adding to it. That
// is what a security level is for, and it is the reason a conversation is
// allowed to override the container it is in.
func TestASecurityLevelReplacesTheProjectRule(t *testing.T) {
	s := newSite()
	s.addLevel("10500", "Legal only", "acc-lee")
	open := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	shut := s.file(home, "Injury report", "Somebody was hurt on line two.")
	s.restrict(shut, "10500")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})

	ordinary := find(changes, "jira:"+open)
	if ordinary == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if got := ordinary.Document.Permissions.AllowUsers; len(got) != 1 || got[0].Value != "acc-mei" {
		t.Errorf("an ordinary issue allows %v, want the members of the project role", got)
	}

	restricted := find(changes, "jira:"+shut)
	if restricted == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	perms := restricted.Document.Permissions
	if perms.Mode != acl.ModeACL {
		t.Fatalf("a restricted issue is %v, want an access control list", perms.Mode)
	}
	if got := perms.AllowUsers; len(got) != 1 || got[0].Value != "acc-lee" {
		t.Errorf("a restricted issue allows %v, want only the security level", got)
	}
	if len(perms.AllowGroups) != 0 {
		t.Errorf("a restricted issue still allows the groups the project allows: %v", perms.AllowGroups)
	}

	// Mei may read the project and is not in the level, which is the whole
	// point of putting the level on.
	mei := &acl.Principal{
		Subject:    "acc-mei",
		Identities: []acl.Identity{{Source: "jira", Value: "acc-mei"}},
	}
	if !ordinary.Document.Permissions.Allows(mei) {
		t.Error("the reporter cannot read an ordinary issue in her own project")
	}
	if perms.Allows(mei) {
		t.Error("a security level did not keep out somebody the project lets in")
	}
}

// A level this token cannot resolve is quarantined rather than falling back to
// the project, because the one thing a security level is is somebody deciding
// on purpose to keep other people out.
func TestAnUnresolvableSecurityLevelQuarantinesTheIssue(t *testing.T) {
	s := newSite()
	key := s.file(home, "Injury report", "Somebody was hurt on line two.")
	s.restrict(key, "10500") // never registered, so the site refuses to resolve it

	var skipped []string
	src := newSource(t, s, jirasource.WithSkipped(func(id string, _ error) {
		skipped = append(skipped, id)
	}))

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if !quarantined(got.Document.Permissions) {
		t.Errorf("an issue behind a level nobody could resolve is readable by %v", got.Document.Permissions)
	}
	if len(skipped) == 0 {
		t.Error("the level could not be resolved and nothing said so, which is an index quietly missing what nobody can read")
	}
}

// A level granted to a project role cannot be resolved without knowing which
// project the issue is in, and this deliberately does not know. Guessing in
// either direction is worse than saying so.
func TestASecurityLevelGrantedToARoleIsQuarantined(t *testing.T) {
	s := newSite()
	l := s.addLevel("10600", "Managers")
	l.roles = []int{10002}
	key := s.file(home, "Pay review", "The pay review for line two.")
	s.restrict(key, "10600")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if !quarantined(got.Document.Permissions) {
		t.Errorf("a level granted to a role resolved to %v", got.Document.Permissions)
	}
}

// The same level on a thousand issues is one request, not a thousand. A project
// with a security scheme puts the same handful of levels on everything in it.
func TestASecurityLevelIsResolvedOnce(t *testing.T) {
	s := newSite()
	s.addLevel("10500", "Legal only", "acc-lee")
	for _, name := range []string{"one", "two", "three"} {
		s.restrict(s.file(home, name, "Something happened."), "10500")
	}
	src := newSource(t, s)
	s.resetCounts()

	run(t, src, connector.Cursor{})
	if got := s.counted("/rest/api/3/issuesecurityschemes/level/member"); got != 1 {
		t.Errorf("three issues at one security level cost %d requests to resolve it", got)
	}
}

// A project whose permission scheme this account may not read is quarantined
// rather than indexed on a guess, and it says so.
func TestAProjectWhosePermissionSchemeIsHiddenIsQuarantined(t *testing.T) {
	s := newSite()
	key := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	s.hide(home)

	var skipped []string
	src := newSource(t, s, jirasource.WithSkipped(func(id string, _ error) {
		skipped = append(skipped, id)
	}))

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if !quarantined(got.Document.Permissions) {
		t.Errorf("a project nobody could work out the rule for is readable by %v", got.Document.Permissions)
	}
	if !slices.Contains(skipped, home) {
		t.Errorf("the unreadable scheme was reported as %v, want the project", skipped)
	}
}

// A permission scheme grants browse to a role and the project decides who is in
// it. An adapter that stopped at the role name would produce an access control
// list naming a thing rather than anybody.
func TestAProjectRoleIsResolvedToItsMembers(t *testing.T) {
	s := newSite()
	key := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	perms := got.Document.Permissions
	if len(perms.AllowUsers) != 1 || perms.AllowUsers[0].Value != "acc-mei" {
		t.Errorf("the rule allows the accounts %v, want the one in the role", perms.AllowUsers)
	}
	if len(perms.AllowGroups) != 1 || perms.AllowGroups[0].Value != "engineering" {
		t.Errorf("the rule allows the groups %v, want the one in the role", perms.AllowGroups)
	}
}

// A project everybody with a licence can browse is a real configuration, and
// describing it as an access control list of nobody would hide it instead.
func TestAProjectOpenToTheTenantIsSaidToBeOpen(t *testing.T) {
	s := newSite()
	p := s.addProject("DOCS", "Documentation")
	p.grants = []grant{{"applicationRole", "jira-software"}}
	key := s.file("DOCS", "Style guide", "How we write things down.")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if got.Document.Permissions.Mode != acl.ModePublicToTenant {
		t.Errorf("a project open to the tenant is %v", got.Document.Permissions.Mode)
	}
}

// A scheme that grants browse to nobody is not a project everybody can read, it
// is a project we failed to understand.
func TestAProjectNobodyCanBrowseIsQuarantined(t *testing.T) {
	s := newSite()
	p := s.addProject("VOID", "Nobody home")
	p.grants = nil
	key := s.file("VOID", "Lost", "Nobody can see this.")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if !quarantined(got.Document.Permissions) {
		t.Errorf("a project granting browse to nobody is readable by %v", got.Document.Permissions)
	}
}

// Nothing in Jira reports that somebody was taken out of a project role. A
// revocation that reaches the index when somebody next comments on a ticket is
// not a revocation, so the rule is reapplied on a schedule.
func TestARemovedRoleMemberIsAppliedOnTheSchedule(t *testing.T) {
	s := newSite()
	s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	src := newSource(t, s)

	_, cursor := run(t, src, connector.Cursor{})
	if changes, _ := run(t, src, cursor); len(changes) != 0 {
		t.Fatalf("a second sync in the same interval emitted %v", ids(changes))
	}

	s.project(home).roles["10002"] = &role{groups: []string{"engineering"}}
	// Still inside the same interval, so the site has told us nothing and
	// neither has the schedule.
	if changes, _ := run(t, src, cursor); len(changes) != 0 {
		t.Fatalf("the removal was applied before the schedule said to, which means every sync applies it: %v", ids(changes))
	}

	s.advance(time.Hour)
	before := src.Counters()
	changes, _ := run(t, src, cursor)
	if len(changes) != 1 {
		t.Fatalf("the interval passed and the sync emitted %v", ids(changes))
	}
	if !changes[0].PermissionsOnly {
		t.Error("reapplying a rule read the issue again")
	}
	if spent := src.Counters().Since(before); spent.Fetches != 0 {
		t.Errorf("a permission change read %d issues, and the point of it is that it reads none", spent.Fetches)
	}
	if got := changes[0].Document.Permissions.AllowUsers; len(got) != 0 {
		t.Errorf("the rule still allows %v after the account was taken out of the role", got)
	}
}

// Nothing in JQL reports an issue that was deleted, so the sweep is the only
// thing that ever takes one out of the index.
func TestADeletedIssueIsOnlyFoundByTheSweep(t *testing.T) {
	s := newSite()
	kept := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	gone := s.file(home, "Filed twice", "This one was raised by mistake.")
	src := newSource(t, s)

	_, cursor := run(t, src, connector.Cursor{})
	s.remove(gone)

	if changes, _ := run(t, src, cursor); len(changes) != 0 {
		t.Errorf("a deletion showed up in the change feed as %v, which Jira cannot do", ids(changes))
	}
	if got, want := enumerate(t, src), []string{"jira:" + kept}; !slices.Equal(got, want) {
		t.Errorf("the enumeration lists %v, want %v", got, want)
	}
	if _, err := src.Fetch(t.Context(), "jira:"+gone); !errors.Is(err, connector.ErrGone) {
		t.Errorf("fetching a deleted issue returned %v, want connector.ErrGone", err)
	}
}

// The sweep compares versions, so the version a listing reports and the version
// a read reports have to be the same string for an issue nothing happened to.
// When they disagree the sweep refetches the whole project every time it runs.
func TestTheListingAndTheReadAgreeOnTheVersion(t *testing.T) {
	s := newSite()
	key := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	s.comment(key, "acc-sam", "Bearing, I think.")
	src := newSource(t, s)

	var version string
	if err := src.Enumerate(t.Context(), func(item connector.Item) bool {
		if item.ID == "jira:"+key {
			version = item.Version
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if version == "" {
		t.Fatal("the enumeration reported no version, so the sweep has nothing to compare")
	}

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if got.Document.SourceUpdate != version {
		t.Errorf("the listing says the version is %q and the sync says %q, so every sweep refetches it", version, got.Document.SourceUpdate)
	}
}

// JQL compares to the minute and the cursor does not, which is a boundary an
// adapter has to get right in both directions: nothing lost, nothing indexed
// twice.
func TestAnIssueUpdatedInTheCursorsMinuteIsNeitherLostNorRepeated(t *testing.T) {
	s := newSite()
	first := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	src := newSource(t, s)

	_, cursor := run(t, src, connector.Cursor{})

	// Both of these land inside the minute the cursor stopped in, because the
	// site's clock moves a minute per event and JQL rounds to the minute.
	s.transition(first, "In Progress")
	second := s.file(home, "Coolant pump", "Coolant pump replaced.")

	changes, next := run(t, src, cursor)
	if got, want := ids(changes), []string{"jira:" + first, "jira:" + second}; !slices.Equal(got, want) {
		t.Fatalf("the sync emitted %v, want both changes in the cursor's minute: %v", got, want)
	}
	if again, _ := run(t, src, next); len(again) != 0 {
		t.Errorf("the sync after that emitted %v again", ids(again))
	}
}

// Decoding a page of JSON into the value the last page went in reuses the
// elements and only overwrites the fields the new page mentions, so an issue
// arriving with no assignee where one used to be inherits the previous
// assignee. It is silent, it is wrong, and it is a permission field away from
// being a leak.
func TestAnIssueOnALaterPageInheritsNothingFromTheOneBefore(t *testing.T) {
	s := newSite()
	first := s.file(home, "One", "The first one.")
	s.assign(first, "acc-sam")
	s.label(first, "line-two")
	s.file(home, "Two", "The second one.")
	third := s.file(home, "Three", "The third one, on a later page.")
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	if len(changes) != 3 {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	got := find(changes, "jira:"+third)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if who, ok := got.Document.Properties["assignee"]; ok {
		t.Errorf("an issue with no assignee came back assigned to %q, which is the page before it showing through", who)
	}
	if labels, ok := got.Document.Properties["labels"]; ok {
		t.Errorf("an issue with no labels came back labelled %q", labels)
	}
}

// A site that caps the page size below what was asked for is the normal case,
// and a crawl that read a short page as the end of the listing would index part
// of a project and report success.
func TestAProjectLargerThanAPageIsWalkedToTheEnd(t *testing.T) {
	s := newSite()
	want := make([]string, 0, 7)
	for _, name := range []string{"one", "two", "three", "four", "five", "six", "seven"} {
		want = append(want, "jira:"+s.file(home, name, "Something happened on line two."))
	}
	slices.Sort(want)
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	if got := ids(changes); !slices.Equal(got, want) {
		t.Errorf("the sync emitted %d of %d issues: %v", len(got), len(want), got)
	}
	if got := enumerate(t, src); !slices.Equal(got, want) {
		t.Errorf("the enumeration listed %d of %d issues: %v", len(got), len(want), got)
	}
}

// Comments page too, and a ticket with a long argument on it is the ticket
// somebody is looking for.
func TestAnIssueWithMoreCommentsThanAPageKeepsThemAll(t *testing.T) {
	s := newSite()
	key := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	for _, said := range []string{"Bearing, I think.", "Ordered a replacement.", "Arrives Thursday.", "Fitted, and it is quiet."} {
		s.comment(key, "acc-sam", said)
	}
	src := newSource(t, s)

	changes, _ := run(t, src, connector.Cursor{})
	got := find(changes, "jira:"+key)
	if got == nil {
		t.Fatalf("the sync emitted %v", ids(changes))
	}
	if !strings.Contains(got.Document.Body, "Fitted, and it is quiet.") {
		t.Errorf("the last comment is not in the body, so the argument stops halfway:\n%s", got.Document.Body)
	}
}

// A ticket with no comments on it costs no request for its comments. Asking
// unconditionally would be a request per issue in the site to be told what we
// were already holding.
func TestAnIssueWithFewCommentsCostsNoExtraRequest(t *testing.T) {
	s := newSite()
	key := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	s.comment(key, "acc-sam", "Bearing, I think.")
	src := newSource(t, s)
	s.resetCounts()

	run(t, src, connector.Cursor{})
	if got := s.counted("/rest/api/3/issue/" + key + "/comment"); got != 0 {
		t.Errorf("an issue whose comments arrived with it cost %d further requests for them", got)
	}
}

// Jira asks a crawler to slow down with a 429 and a Retry-After, and a run that
// gave up at the first one would be a run that never finished a large site.
func TestAThrottledRequestIsRetriedRatherThanFailing(t *testing.T) {
	s := newSite()
	key := s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	src := newSource(t, s)
	s.slowDown(3)

	changes, _ := run(t, src, connector.Cursor{})
	if find(changes, "jira:"+key) == nil {
		t.Fatalf("a run that was asked to slow down emitted %v", ids(changes))
	}
}

// A site that has stopped answering is a failed sync rather than an empty one,
// because an empty sync followed by a sweep is an emptied index.
func TestASiteThatRefusesIsAFailedSyncRatherThanAnEmptyOne(t *testing.T) {
	s := newSite()
	s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	src := newSource(t, s)
	s.refuse("/rest/api/3/search", http.StatusInternalServerError)

	_, err := src.Sync(t.Context(), connector.Cursor{}, func(context.Context, connector.Change) error { return nil })
	if err == nil {
		t.Fatal("a site answering every search with an error produced a successful sync of nothing")
	}
}

// An id this source could never have produced is refused rather than being put
// into a URL path to find out what happens.
func TestAnIdThatIsNotAnIssueKeyIsRefused(t *testing.T) {
	s := newSite()
	src := newSource(t, s)

	for _, id := range []string{"jira:../../admin", "jira:not a key", "jira:LINE-", "jira:-1"} {
		if _, err := src.Fetch(t.Context(), id); err == nil {
			t.Errorf("fetching %q was allowed", id)
		}
	}
}

// The credentials go in a header. A token in a query string ends up in a server
// log, in a recording and in a bug report.
func TestTheTokenIsNeverInAURL(t *testing.T) {
	s := newSite()
	s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")

	var seen []string
	src := newSource(t, s, jirasource.WithHTTPClient(&http.Client{
		Transport: watcher{seen: &seen},
	}))

	// The client above answers nothing, so the sync fails. What is being
	// checked is what went out before it did.
	_, _ = src.Sync(t.Context(), connector.Cursor{}, func(context.Context, connector.Change) error { return nil })
	if len(seen) == 0 {
		t.Fatal("nothing was requested")
	}
	for _, u := range seen {
		if strings.Contains(u, token) || strings.Contains(u, email) {
			t.Errorf("the credentials went out in %s", u)
		}
	}
}

// watcher records where requests went and refuses them.
type watcher struct{ seen *[]string }

func (w watcher) RoundTrip(req *http.Request) (*http.Response, error) {
	*w.seen = append(*w.seen, req.URL.String())
	return nil, errors.New("no")
}

// A site is not asked for three hundred custom fields when the document holds
// twelve of them, because a page of everything is a request the site bills
// accordingly.
func TestOnlyTheFieldsThatEndUpInTheDocumentAreAskedFor(t *testing.T) {
	s := newSite()
	s.file(home, "Gearbox noise", "The gearbox on line two is making a noise.")
	src := newSource(t, s)

	if err := src.Enumerate(t.Context(), func(connector.Item) bool { return true }); err != nil {
		t.Fatal(err)
	}
	// The sweep needs one field, and asking for the description of every issue
	// in a site to work out which of them changed would be the expensive way to
	// learn nothing.
	if got := s.lastFields("/rest/api/3/search"); got != "updated" {
		t.Errorf("the listing asked for the fields %q, want only the one the sweep compares", got)
	}
}
