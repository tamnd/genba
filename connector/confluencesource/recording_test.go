package confluencesource_test

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

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/confluencesource"
	"github.com/tamnd/genba/connector/recorded"
	"github.com/tamnd/genba/connector/threadsource"
)

// The fixture set is a crawl of a small site written down once and read back by
// the tests below, so that the adapter is exercised against the bytes a
// Confluence site actually sends without anybody needing an account or a token
// to run the tests.
//
// The fake next to this file is the other half of the story and neither replaces
// the other. The fake is where behaviour is written: it can be made to throttle,
// to hide a space's permissions, to lose a page between two syncs. The recording
// is where the wire format is pinned: it is the answer to "did we change what we
// send" and it is the thing that would have to be refreshed if Atlassian changed
// a field name.
const fixtures = "testdata/site"

// where the recording is replayed against. Nothing reaches it. It is the real
// shape of a Confluence Cloud address because a reader opening one of these
// files should see the request that would have gone to a site.
const siteURL = "https://acme.atlassian.net"

// The ids the recorded site produces, written down rather than worked out. A
// test that derived them from the fake would pass just as happily against a
// recording of something else entirely.
const (
	deployID  = "confluence:100001"
	runbookID = "confluence:100002"
	pumpID    = "confluence:100004"
	specID    = "confluence:100005"
)

// pinned is what the clock says while the fixtures are being read back. A full
// sync reads the whole site and asks for nothing that depends on the time, so
// this only has to be steady rather than right.
var pinned = func() time.Time { return start.Add(time.Hour) }

var update = flag.Bool("update", false, "record the Confluence fixtures in testdata again")

// recordingSite is the site the fixtures are a crawl of.
//
// It is small on purpose and every part of it is there for a reason: two spaces
// so that the listing is more than one entry, one of them readable by named
// groups and the other readable by anybody so that both resolutions are in the
// recording, a page with an argument underneath it from two different people, a
// page written in the old editor so that the second request for a storage format
// body is in there, and a page restricted out of its space under a parent so
// that the override and the inheritance walk are in there too.
func recordingSite() *site {
	s := newSite()

	deploy := s.create(home, "Deploy", "The deploy runs at nine every weekday morning.")
	s.label(deploy, "runbook", "deploy")
	s.comment(deploy, "acc-sam", "It moved to eleven in March when the batch job was added.")
	s.comment(deploy, "acc-lee", "Confirmed, eleven. The heading above is out of date.")

	runbook := s.create(home, "Restarting line two", "placeholder")
	s.legacy(runbook, `<h2>Restarting line two</h2>`+
		`<p>Stop the feed, wait for the <strong>green</strong> light, then run:</p>`+
		`<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">bash</ac:parameter>`+
		`<ac:plain-text-body><![CDATA[line2ctl stop --drain
line2ctl start]]></ac:plain-text-body></ac:structured-macro>`+
		`<ac:structured-macro ac:name="warning"><ac:parameter ac:name="title">Do not skip the drain</ac:parameter>`+
		`<ac:rich-text-body><p>The gearbox will bind if the feed is still moving.</p></ac:rich-text-body>`+
		`</ac:structured-macro>`)

	incidents := s.create(home, "Incidents", "Everything that went wrong, one page each.")
	pump := s.create(home, "Pump failure", "The coolant pump on line two failed on Tuesday afternoon.")
	s.nest(pump, incidents)
	s.restrict(pump, []string{"acc-lee"}, []string{"safety-officers"})

	eng := s.addSpace("ENG", "Engineering")
	eng.open = true
	s.create("ENG", "Line two specification", "Line two runs at 1450 rpm under load.")

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
	for _, want := range []struct{ title, id string }{
		{"Deploy", deployID},
		{"Restarting line two", runbookID},
		{"Pump failure", pumpID},
		{"Line two specification", specID},
	} {
		if got := "confluence:" + s.idOf(want.title); got != want.id {
			t.Fatalf("the site filed %q under %s, and the tests below look for %s", want.title, got, want.id)
		}
	}

	rec := recorded.Record(authed{http.DefaultTransport},
		recorded.WithRedactedHeaders("Authorization"),
	)
	src, err := confluencesource.New(s.server(t), email, token,
		confluencesource.WithHTTPClient(rec.Client()),
		confluencesource.WithClock(pinned),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()

	crawl(t, src)

	// A crawl asks the same listing question more than once: the sync, the sweep
	// and a fetch each need to know what the spaces are and who may read each.
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

	if _, err := src.Fetch(t.Context(), deployID); err != nil {
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
	src, err := confluencesource.New(siteURL, email, token,
		confluencesource.WithHTTPClient(rt.Client()),
		confluencesource.WithClock(pinned),
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

	want := []string{deployID, runbookID, "confluence:100003", pumpID, specID}
	slices.Sort(want)
	if got := distinct(ids(changes)); !slices.Equal(got, want) {
		t.Fatalf("the recorded site crawled to %v, want %v", got, want)
	}

	deploy := find(changes, deployID)
	if deploy == nil {
		t.Fatal("the page with the argument on it is not in the crawl")
	}
	for _, phrase := range []string{
		"runs at nine every weekday morning",
		"moved to eleven in March",
		"heading above is out of date",
	} {
		if !strings.Contains(deploy.Document.Body, phrase) {
			t.Errorf("the page is missing %q:\n%s", phrase, deploy.Document.Body)
		}
	}
	if got := deploy.Document.Properties["labels"]; got != "runbook, deploy" && got != "deploy, runbook" {
		t.Errorf("the page carries the labels %q", got)
	}

	// Names came out of the recording too, which is the part a crawl without an
	// account usually cannot show.
	if got := deploy.Document.Author.Name; got != "Mei Tanaka" {
		t.Errorf("the page is attributed to %q, want the name the recording holds", got)
	}
}

// The storage format is in the recording as well, which is most of why it is
// worth committing one for a wiki. A page written before the editor changed
// arrives as XHTML rather than as an Atlassian document, and the structure in it
// is the thing a reader was looking for.
func TestTheRecordedSiteReadsAPageFromTheOldEditor(t *testing.T) {
	changes := crawl(t, replaying(t))

	runbook := find(changes, runbookID)
	if runbook == nil {
		t.Fatalf("the crawl emitted %v", distinct(ids(changes)))
	}
	for _, phrase := range []string{
		"## Restarting line two",
		"**green**",
		"```bash\nline2ctl stop --drain\nline2ctl start\n```",
		"> **Do not skip the drain**",
		"gearbox will bind",
	} {
		if !strings.Contains(runbook.Document.Body, phrase) {
			t.Errorf("the rendered page is missing %q:\n%s", phrase, runbook.Document.Body)
		}
	}
}

// The permission work is in the recording as well. A space granting read to a
// group, a space anybody may read and a page restricted out of the space it is
// in are three different answers, and the third one is the one that would
// publish something if it went wrong.
func TestTheRecordedSiteResolvesItsPermissions(t *testing.T) {
	changes := crawl(t, replaying(t))

	deploy := find(changes, deployID)
	if deploy == nil {
		t.Fatalf("the crawl emitted %v", distinct(ids(changes)))
	}
	if got, want := values(deploy.Document.Permissions.AllowGroups), []string{"engineering"}; !slices.Equal(got, want) {
		t.Errorf("a page in the operations space allows the groups %v, want %v", got, want)
	}

	spec := find(changes, specID)
	if spec == nil {
		t.Fatalf("the crawl emitted %v", distinct(ids(changes)))
	}
	if got := spec.Document.Permissions.Mode; got != acl.ModePublicToTenant {
		t.Errorf("a page in an anonymously readable space was given mode %v", got)
	}

	pump := find(changes, pumpID)
	if pump == nil {
		t.Fatalf("the crawl emitted %v", distinct(ids(changes)))
	}
	if got, want := values(pump.Document.Permissions.AllowUsers), []string{"acc-lee"}; !slices.Equal(got, want) {
		t.Errorf("a restricted page allows the accounts %v, want only the restriction", got)
	}
	if got, want := values(pump.Document.Permissions.AllowGroups), []string{"safety-officers"}; !slices.Equal(got, want) {
		t.Errorf("a restricted page allows the groups %v, want only the restriction", got)
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
	src, err := confluencesource.New(siteURL, email, token,
		confluencesource.WithHTTPClient(rt.Client()),
		confluencesource.WithClock(pinned),
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
		// Confluence site, so it does not belong in a committed file either.
		if strings.Contains(body, email) {
			t.Errorf("%s holds the account the recording was made with", e.Name())
		}
		if strings.Contains(body, base64.StdEncoding.EncodeToString([]byte(email+":"+token))) {
			t.Errorf("%s holds an encoded credential", e.Name())
		}
	}
}
