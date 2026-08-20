package slacksource_test

import (
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
	"github.com/tamnd/genba/connector/recorded"
	"github.com/tamnd/genba/connector/slacksource"
	"github.com/tamnd/genba/connector/threadsource"
)

// The fixture set is a crawl of a small workspace written down once and read
// back by the test below, so that the adapter is exercised against the bytes a
// Slack workspace actually sends without anybody needing an account or a token
// to run the tests.
//
// The fake next to this file is the other half of the story and neither
// replaces the other. The fake is where behaviour is written: it can be made to
// throttle, to refuse a channel, to lose a message between two syncs. The
// recording is where the wire format is pinned: it is the answer to "did we
// change what we send" and it is the thing that would have to be refreshed if
// Slack changed a field name.
const fixtures = "testdata/workspace"

// The ids the recorded workspace produces. They are written down rather than
// worked out because a fixture set is a fixed corpus, and a test that derived
// them from the fake would pass just as happily against a recording of
// something else entirely.
const (
	welcomeID = "slack:C_GENERAL:1780000001.000000"
	gearboxID = "slack:C_LINE_TWO:1780000003.000000"
	guardID   = "slack:C_SAFETY:1780000008.000000"
)

// pinned is what the clock says while the fixtures are being read back. A full
// sync reads all of history and asks for nothing that depends on the time, so
// this only has to be steady rather than right.
var pinned = func() time.Time { return time.Unix(1780000009, 0).UTC() }

var update = flag.Bool("update", false, "record the Slack fixtures in testdata again")

// recordingWorkspace is the workspace the fixtures are a crawl of.
//
// It is small on purpose and every part of it is there for a reason: two public
// channels so that the listing is more than one entry, a thread with two
// replies from two different people so that assembly and the name lookups are
// in the recording, a join message so that the thing we refuse to index is in
// there too, and a private channel so that a membership listing is as well.
func recordingWorkspace() *workspace {
	w := newWorkspace()
	w.post("C_GENERAL", "welcome", "Morning all. Line two is down until the gearbox is looked at.")

	w.addChannel("C_LINE_TWO", "line-two", false)
	w.post("C_LINE_TWO", "gearbox", "The gearbox on line two is making a noise under load.")
	w.reply("C_LINE_TWO", "gearbox", "U_SAM", "Sounds like the outboard bearing to me.")
	w.reply("C_LINE_TWO", "gearbox", "U_LEE", "I changed that one on Tuesday, so it should not be.")
	w.system("C_LINE_TWO", "channel_join", "<@U_LEE> has joined the channel")

	w.addChannel("C_SAFETY", "safety", true)
	w.post("C_SAFETY", "guard", "The guard on the press is being replaced on Friday morning.")
	return w
}

// authed puts the token on the way the adapter does, so that what is recorded
// is a real authenticated crawl rather than one made against a service that was
// not asking.
type authed struct{ base http.RoundTripper }

func (a authed) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+token)
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

	w := recordingWorkspace()
	if got := "slack:C_LINE_TWO:" + w.tsOf("C_LINE_TWO", "gearbox"); got != gearboxID {
		t.Fatalf("the workspace filed the gearbox thread under %s, and the tests below look for %s", got, gearboxID)
	}

	rec := recorded.Record(authed{http.DefaultTransport},
		recorded.WithRedactedHeaders("Authorization"),
		recorded.WithRedactedParams("token"),
	)
	src, err := slacksource.New(token,
		slacksource.WithBaseURL(w.server(t)),
		slacksource.WithHTTPClient(rec.Client()),
		slacksource.WithClock(pinned),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()

	crawl(t, src)

	// A crawl asks the same listing question more than once: the sync, the
	// sweep and a fetch each need to know what the channels are and who may
	// read them. Writing the answer down three times is three files to review
	// and no more coverage, so the repeats are dropped and what is left is one
	// file per distinct question.
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
// the fixtures a diff on all eleven files with the real change somewhere in it.
// The host is written as Slack's for the same reason: a reader opening one of
// these files should see the request that would have gone to Slack.
func settle(e recorded.Exchange) recorded.Exchange {
	if u, err := url.Parse(e.Request.URL); err == nil {
		slack, err := url.Parse(slacksource.DefaultBaseURL)
		if err != nil {
			panic(err)
		}
		u.Scheme, u.Host = slack.Scheme, slack.Host
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

// TestTheRecordedWorkspaceCrawlsWithoutAnAccount is the point of the fixture
// set: the whole adapter, the real paging and the real decoding, against bytes
// that came off a workspace and are now a file.
func TestTheRecordedWorkspaceCrawlsWithoutAnAccount(t *testing.T) {
	rt, err := recorded.Replay(fixtures)
	if err != nil {
		t.Fatal(err)
	}

	src, err := slacksource.New(token,
		// The base URL is the real one. Nothing reaches it, and using it is what
		// keeps the recording keyed on the path Slack serves rather than on the
		// port a test server happened to be given.
		slacksource.WithBaseURL(slacksource.DefaultBaseURL),
		slacksource.WithHTTPClient(rt.Client()),
		slacksource.WithClock(pinned),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()

	changes := crawl(t, src)

	want := []string{welcomeID, gearboxID, guardID}
	if got := ids(changes); !slices.Equal(got, want) {
		t.Fatalf("the recorded workspace crawled to %v, want %v", got, want)
	}

	gearbox := find(changes, gearboxID)
	if gearbox == nil {
		t.Fatal("the thread with the replies on it is not in the crawl")
	}
	for _, phrase := range []string{
		"making a noise under load",
		"outboard bearing",
		"changed that one on Tuesday",
	} {
		if !strings.Contains(gearbox.Document.Body, phrase) {
			t.Errorf("the thread is missing %q:\n%s", phrase, gearbox.Document.Body)
		}
	}
	if got := gearbox.Document.Title; !strings.Contains(got, "gearbox") {
		t.Errorf("the thread is titled %q", got)
	}

	// Names came out of the recording too, which is the part a crawl without an
	// account usually cannot show.
	if got := gearbox.Document.Author.Name; got != "Mei Tanaka" {
		t.Errorf("the thread is attributed to %q, want the name the recording holds", got)
	}
}

// TestTheRecordingHoldsNothingItIsNotAskedFor keeps the fixture set from
// growing a tail of responses nothing reads.
//
// A recording is a thing people add to and rarely take away from, and one that
// has drifted is worse than none: a reviewer reading a file next to the test
// reasonably assumes the test depends on it.
func TestTheRecordingHoldsNothingItIsNotAskedFor(t *testing.T) {
	rt, err := recorded.Replay(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	src, err := slacksource.New(token,
		slacksource.WithBaseURL(slacksource.DefaultBaseURL),
		slacksource.WithHTTPClient(rt.Client()),
		slacksource.WithClock(pinned),
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
		if strings.Contains(string(raw), token) {
			t.Errorf("%s holds the token it was recorded with", e.Name())
		}
		if strings.Contains(string(raw), "xoxb-") || strings.Contains(string(raw), "xoxp-") {
			t.Errorf("%s holds something shaped like a Slack token", e.Name())
		}
	}
}
