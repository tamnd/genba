package jirasource_test

import (
	"encoding/base64"
	"flag"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/jirasource"
	"github.com/tamnd/genba/connector/recorded"
	"github.com/tamnd/genba/connector/threadsource"
)

// The fixture set is a crawl of a small site written down once and read back by
// the tests below, so that the adapter is exercised against the bytes a Jira
// site actually sends without anybody needing an account or a token to run the
// tests.
//
// The fake next to this file is the other half of the story and neither
// replaces the other. The fake is where behaviour is written: it can be made to
// throttle, to hide a permission scheme, to lose an issue between two syncs. The
// recording is where the wire format is pinned: it is the answer to "did we
// change what we send" and it is the thing that would have to be refreshed if
// Atlassian changed a field name.
const fixtures = "testdata/site"

// where the recording is replayed against. Nothing reaches it. It is the real
// shape of a Jira Cloud address because a reader opening one of these files
// should see the request that would have gone to a site.
const siteURL = "https://acme.atlassian.net"

// The ids the recorded site produces, written down rather than worked out. A
// test that derived them from the fake would pass just as happily against a
// recording of something else entirely.
const (
	gearboxID = "jira:LINE-1"
	coolantID = "jira:LINE-2"
	guardID   = "jira:SAFETY-1"
)

// pinned is what the clock says while the fixtures are being read back. A full
// sync reads the whole site and asks for nothing that depends on the time, so
// this only has to be steady rather than right.
var pinned = func() time.Time { return start.Add(time.Hour) }

var update = flag.Bool("update", false, "record the Jira fixtures in testdata again")

// recordingSite is the site the fixtures are a crawl of.
//
// It is small on purpose and every part of it is there for a reason: two
// projects so that the listing is more than one entry, a scheme granting browse
// through a role and another granting it to a group so that both resolutions are
// in the recording, a ticket with a structured description and an argument
// underneath it from two different people, and an issue behind a security level
// so that the override is in there too.
func recordingSite() *site {
	s := newSite()

	gearbox := s.file(home, "Gearbox noise on line two", "placeholder")
	s.describe(gearbox, map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []any{
			map[string]any{"type": "paragraph", "content": []any{
				map[string]any{"type": "text", "text": "The gearbox on line two is making a noise under load."},
			}},
			map[string]any{
				"type":  "codeBlock",
				"attrs": map[string]any{"language": "text"},
				"content": []any{map[string]any{
					"type": "text",
					"text": "vibration 4.2 mm/s at 1450 rpm",
				}},
			},
		},
	})
	s.assign(gearbox, "acc-sam")
	s.label(gearbox, "line-two", "mechanical")
	s.comment(gearbox, "acc-sam", "Sounds like the outboard bearing to me.")
	s.comment(gearbox, "acc-lee", "I changed that one on Tuesday, so it should not be.")
	s.transition(gearbox, "In Progress")

	s.file(home, "Coolant pump replaced", "The coolant pump on line two was replaced this morning.")

	safety := s.addProject("SAFETY", "Safety")
	safety.grants = []grant{{"group", "safety-officers"}}
	s.addLevel("10500", "Incident reports", "acc-lee")
	s.restrict(s.file("SAFETY", "Guard on the press", "The guard on the press is being replaced on Friday morning."), "10500")

	return s
}

// authed puts the credentials on the way the adapter does, so that what is
// recorded is a real authenticated crawl rather than one made against a service
// that was not asking.
type authed struct{ base http.RoundTripper }

func (a authed) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(email+":"+token)))
	return a.base.RoundTrip(req)
}

// TestRecordTheFixtures writes the fixture set again. It is skipped unless the
// flag is given, because a recording that refreshed itself on every run would
// never fail: the thing it is there to catch is a change to what we send, and a
// fixture regenerated alongside the change agrees with it by construction.
func TestRecordTheFixtures(t *testing.T) {
	if !*update {
		t.Skip("run with -update to record the fixtures again")
	}

	s := recordingSite()
	if got := "jira:" + s.keyOf("Gearbox noise on line two"); got != gearboxID {
		t.Fatalf("the site filed the gearbox ticket under %s, and the tests below look for %s", got, gearboxID)
	}

	rec := recorded.Record(authed{http.DefaultTransport},
		recorded.WithRedactedHeaders("Authorization"),
	)
	src, err := jirasource.New(s.server(t), email, token,
		jirasource.WithHTTPClient(rec.Client()),
		jirasource.WithClock(pinned),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()

	crawl(t, src)

	// A crawl asks the same listing question more than once: the sync, the sweep
	// and a fetch each need to know what the projects are and who may read each.
	// Writing the answer down three times is three files to review and no more
	// coverage, so the repeats are dropped and what is left is one file per
	// distinct question.
	var (
		seen = map[string]bool{}
		once []recorded.Exchange
	)
	for _, e := range rec.Exchanges() {
		k := e.Request.Method + " " + e.Request.URL
		if seen[k] {
			continue
		}
		seen[k] = true
		once = append(once, settle(e))
	}

	if err := recorded.From(once).Save(fixtures); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d of %d exchanges to %s", len(once), len(rec.Exchanges()), fixtures)
}

// settle takes the run out of a recorded exchange.
//
// Two things in one differ every time it is made: the address the test server
// was given, and the date the answer came back on. Neither is matched against
// and neither means anything, and leaving them in would make every refresh of
// the fixtures a diff on every file with the real change somewhere in it.
func settle(e recorded.Exchange) recorded.Exchange {
	if u, err := url.Parse(e.Request.URL); err == nil {
		site, err := url.Parse(siteURL)
		if err != nil {
			panic(err)
		}
		u.Scheme, u.Host = site.Scheme, site.Host
		e.Request.URL = u.String()
	}
	delete(e.Response.Headers, "Date")
	delete(e.Response.Headers, "Content-Length")
	return e
}

// crawl is everything the fixture set has to answer, and it is shared so that
// what is recorded and what is replayed cannot drift apart.
func crawl(t *testing.T, src *threadsource.Source) []connector.Change {
	t.Helper()

	changes, _ := run(t, src, connector.Cursor{})

	var listed []connector.Item
	if err := src.Enumerate(t.Context(), func(item connector.Item) bool {
		listed = append(listed, item)
		return true
	}); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(listed) == 0 {
		t.Fatal("the listing found nothing")
	}

	if _, err := src.Fetch(t.Context(), gearboxID); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	return changes
}

// replaying is the adapter over the committed fixture set.
func replaying(t *testing.T) *threadsource.Source {
	t.Helper()
	rt, err := recorded.Replay(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	src, err := jirasource.New(siteURL, email, token,
		jirasource.WithHTTPClient(rt.Client()),
		jirasource.WithClock(pinned),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

// TestTheRecordedSiteCrawlsWithoutAnAccount is the point of the fixture set: the
// whole adapter, the real paging and the real decoding, against bytes that came
// off a site and are now a file.
func TestTheRecordedSiteCrawlsWithoutAnAccount(t *testing.T) {
	changes := crawl(t, replaying(t))

	want := []string{gearboxID, coolantID, guardID}
	slices.Sort(want)
	if got := ids(changes); !slices.Equal(got, want) {
		t.Fatalf("the recorded site crawled to %v, want %v", got, want)
	}

	gearbox := find(changes, gearboxID)
	if gearbox == nil {
		t.Fatal("the ticket with the argument on it is not in the crawl")
	}
	for _, phrase := range []string{
		"making a noise under load",
		"vibration 4.2 mm/s at 1450 rpm",
		"outboard bearing",
		"changed that one on Tuesday",
	} {
		if !strings.Contains(gearbox.Document.Body, phrase) {
			t.Errorf("the ticket is missing %q:\n%s", phrase, gearbox.Document.Body)
		}
	}
	if got := gearbox.Document.Properties["status"]; got != "In Progress" {
		t.Errorf("the ticket is %q, want the status it was moved to", got)
	}

	// Names came out of the recording too, which is the part a crawl without an
	// account usually cannot show.
	if got := gearbox.Document.Author.Name; got != "Mei Tanaka" {
		t.Errorf("the ticket is attributed to %q, want the name the recording holds", got)
	}
}

// The permission work is in the recording as well, which is what makes it worth
// committing. A role resolved to its members, a group named directly and a
// security level overriding the project it is in are three different requests
// and three different answers.
func TestTheRecordedSiteResolvesItsPermissions(t *testing.T) {
	changes := crawl(t, replaying(t))

	gearbox := find(changes, gearboxID)
	if gearbox == nil {
		t.Fatalf("the crawl emitted %v", ids(changes))
	}
	if got := gearbox.Document.Permissions.AllowUsers; len(got) != 1 || got[0].Value != "acc-mei" {
		t.Errorf("the project role resolved to the accounts %v", got)
	}
	if got := gearbox.Document.Permissions.AllowGroups; len(got) != 1 || got[0].Value != "engineering" {
		t.Errorf("the project role resolved to the groups %v", got)
	}

	guard := find(changes, guardID)
	if guard == nil {
		t.Fatalf("the crawl emitted %v", ids(changes))
	}
	if got := guard.Document.Permissions.AllowUsers; len(got) != 1 || got[0].Value != "acc-lee" {
		t.Errorf("an issue behind a security level allows %v, want only the level", got)
	}
	if got := guard.Document.Permissions.AllowGroups; len(got) != 0 {
		t.Errorf("an issue behind a security level still allows the project's groups: %v", got)
	}
}

// TestTheRecordingHoldsNothingItIsNotAskedFor keeps the fixture set from growing
// a tail of responses nothing reads.
//
// A recording is a thing people add to and rarely take away from, and one that
// has drifted is worse than none: a reviewer reading a file next to the test
// reasonably assumes the test depends on it.
func TestTheRecordingHoldsNothingItIsNotAskedFor(t *testing.T) {
	rt, err := recorded.Replay(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	src, err := jirasource.New(siteURL, email, token,
		jirasource.WithHTTPClient(rt.Client()),
		jirasource.WithClock(pinned),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()

	crawl(t, src)

	if left := rt.Unused(); len(left) != 0 {
		t.Errorf("the fixture set answers %d requests the crawl never makes:\n%s", len(left), strings.Join(left, "\n"))
	}
}

// TestTheRecordingCarriesNoCredentials is the reason a fixture set is safe to
// commit at all. It is a check on the recording rather than on the code, and it
// is here because the file is here.
func TestTheRecordingCarriesNoCredentials(t *testing.T) {
	entries, err := os.ReadDir(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(fixtures, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		if strings.Contains(body, token) {
			t.Errorf("%s holds the token it was recorded with", e.Name())
		}
		// The email address is half of a basic authentication credential on a
		// Jira site, so it does not belong in a committed file either.
		if strings.Contains(body, email) {
			t.Errorf("%s holds the account the recording was made with", e.Name())
		}
		if strings.Contains(body, base64.StdEncoding.EncodeToString([]byte(email+":"+token))) {
			t.Errorf("%s holds an encoded credential", e.Name())
		}
	}
}
