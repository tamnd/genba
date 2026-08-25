package google_test

import (
	"errors"
	"flag"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector/recorded"
	"github.com/tamnd/genba/directory"
	"github.com/tamnd/genba/directory/google"
)

// The fixture set is a resolution against a small domain written down once and
// read back by the tests below, so that the adapter is exercised against the
// bytes the Admin SDK actually sends without anybody needing a Workspace domain
// and a service account to run the tests.
//
// The fake next to this file is the other half of the story and neither
// replaces the other. The fake is where behaviour is written: it can be made to
// suspend somebody, to page differently, to refuse with a quota. The recording
// is where the wire format is pinned: it is the answer to "did we change what we
// send" and it is the thing that would have to be refreshed if the Admin SDK
// changed a field name.
//
// Signing in is not in here on purpose. The fixtures are a description of the
// directory endpoints and a JWT bearer grant is a description of the token
// endpoint, and putting the second in would mean writing an assertion signed
// with a private key down in a file. What the grant does is covered by the fake.
const fixtures = "testdata/domain"

// The domain the recording is of. It is the real endpoint, which nothing
// reaches, because keying the fixtures on the port a test server happened to be
// given would make them unreadable and unstable at once.
const recordedEndpoint = "https://admin.googleapis.com"

// The ids the recorded domain holds, in the two shapes this API uses: a person
// is a long number and a group is a short string of letters and digits. They are
// written down rather than worked out because a fixture set is a fixed corpus,
// and a test that derived them from the fake would pass just as happily against
// a recording of something else entirely.
const (
	recordedPerson  = "104325091920138142876"
	recordedNobody  = "117234598712345098761"
	recordedLeaver  = "112938475610293847561"
	recordedTop     = "03dy6vkm2gh4kso"
	recordedMiddle  = "01opuj5n3f8k9ac"
	recordedGroup   = "02lwamvv4c1pzs7"
	recordedOncall  = "04hjm7q8w2r5vbz"
	recordedPanel   = "05t9zc3x6n1kfdy"
	recordedMissing = "00meukdy0v9x1nq"
)

var update = flag.Bool("update", false, "record the Google Workspace fixtures in testdata again")

// recordingDomain is the domain the fixtures are a resolution of.
//
// It is small on purpose and every part of it is there for a reason: somebody
// three levels down a nest so that the walk is in the recording rather than only
// in the fake, and in enough groups directly for their listing to be more than
// one page at the size below, somebody in none so that an empty collection is in
// there, somebody suspended so that the refusal is too, and a group looked up on
// its own so that the endpoint the buffer usually saves is there as well.
func recordingDomain(t *testing.T) string {
	t.Helper()
	o, endpoint := newDomain(t)

	for _, g := range []struct {
		id, name, email string
		in              []string
	}{
		{recordedTop, "Everyone", "everyone@acme.test", nil},
		{recordedMiddle, "Engineering", "engineering@acme.test", []string{recordedTop}},
		{recordedGroup, "Storage", "storage@acme.test", []string{recordedMiddle}},
		{recordedOncall, "Storage Oncall", "storage-oncall@acme.test", nil},
		{recordedPanel, "Interviewers", "interviewers@acme.test", nil},
	} {
		o.putGroup(t, directory.Group{ID: g.id, Name: g.name, MemberOf: g.in})
		o.editGroup(t, g.id, func(team *team) { team.email = g.email })
	}

	o.put(t, directory.Subject{
		ID:       recordedPerson,
		Name:     "Mei Tanaka",
		Email:    "mei@acme.test",
		MemberOf: []string{recordedGroup, recordedOncall, recordedPanel},
	})
	o.edit(t, recordedPerson, func(p *person) { p.aliases = []string{"m.tanaka@acme.test"} })

	o.put(t, directory.Subject{ID: recordedNobody, Name: "Jo Okafor", Email: "jo@acme.test"})
	o.put(t, directory.Subject{ID: recordedLeaver, Name: "Lee Byrne", Email: "lee@acme.test", Disabled: true})
	return endpoint
}

// TestRecordTheFixtures writes the fixture set again. It is skipped unless the
// flag is given, because a recording that refreshed itself on every run would
// never fail: the thing it is there to catch is a change to what we send, and a
// fixture regenerated alongside the change agrees with it by construction.
func TestRecordTheFixtures(t *testing.T) {
	if !*update {
		t.Skip("run with -update to record the fixtures again")
	}

	endpoint := recordingDomain(t)
	rec := recorded.Record(http.DefaultTransport, recorded.WithRedactedHeaders("Authorization"))

	resolve(t, func(opts ...google.Option) *google.Directory {
		return dial(t, endpoint, append([]google.Option{google.WithHTTPClient(rec.Client())}, opts...)...)
	})

	// The same question asked twice is one file. A resolution asks for the
	// groups above one key once per expansion and the pages repeat across them,
	// and writing the answer down twice is two files to review and no more
	// coverage.
	var (
		seen = map[string]bool{}
		once []recorded.Exchange
	)
	for _, e := range rec.Exchanges() {
		e = settle(e)
		k := e.Request.Method + " " + e.Request.URL
		if seen[k] {
			continue
		}
		seen[k] = true
		once = append(once, e)
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
// the fixtures a diff on every file with the real change somewhere in it. The
// host is written as the Admin SDK for the same reason: a reader opening one of
// these files should see the request that would have gone to Google.
func settle(e recorded.Exchange) recorded.Exchange {
	e.Request.URL = rehost(e.Request.URL)
	delete(e.Response.Headers, "Date")
	delete(e.Response.Headers, "Content-Length")
	return e
}

// rehost puts one URL on the endpoint the fixtures are written against.
func rehost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	admin, err := url.Parse(recordedEndpoint)
	if err != nil {
		panic(err)
	}
	u.Scheme, u.Host = admin.Scheme, admin.Host
	return u.String()
}

// resolve is everything the fixture set has to answer, and it is shared so that
// what is recorded and what is replayed cannot drift apart.
func resolve(t *testing.T, dir func(opts ...google.Option) *google.Directory) directory.Expansion {
	t.Helper()

	// Two at a time, so that a listing over pages is in the recording rather
	// than only in the fake.
	r, err := directory.New(dir(google.WithPageSize(2)))
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.Expand(t.Context(), recordedPerson)
	if err != nil {
		t.Fatalf("expanding %s: %v", recordedPerson, err)
	}
	if _, err := r.Expand(t.Context(), recordedNobody); err != nil {
		t.Fatalf("expanding %s: %v", recordedNobody, err)
	}
	if _, err := r.Expand(t.Context(), recordedLeaver); !errors.Is(err, directory.ErrDisabled) {
		t.Fatalf("expanding %s gave %v, want ErrDisabled", recordedLeaver, err)
	}

	// A group asked for on its own, which the buffer would otherwise answer out
	// of the listing that already went past.
	alone := dir(google.WithBuffer(0, 0))
	if _, err := alone.Group(t.Context(), recordedGroup); err != nil {
		t.Fatalf("looking up %s: %v", recordedGroup, err)
	}
	if _, err := alone.Group(t.Context(), recordedMissing); !errors.Is(err, directory.ErrNoGroup) {
		t.Fatalf("looking up %s gave %v, want ErrNoGroup", recordedMissing, err)
	}
	return got
}

// replayed is the adapter over the fixture set.
func replayed(t *testing.T, rt *recorded.Transport, opts ...google.Option) *google.Directory {
	t.Helper()
	d, err := google.New(google.Token(bearer), append([]google.Option{
		google.WithEndpoint(recordedEndpoint),
		google.WithHTTPClient(rt.Client()),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestTheRecordedDomainResolvesWithoutADomain is the point of the fixture set:
// the whole adapter, the real paging and the real decoding, against bytes that
// came off the Admin SDK and are now a file.
func TestTheRecordedDomainResolvesWithoutADomain(t *testing.T) {
	rt, err := recorded.Replay(fixtures)
	if err != nil {
		t.Fatal(err)
	}

	got := resolve(t, func(opts ...google.Option) *google.Directory {
		return replayed(t, rt, opts...)
	})

	want := []string{
		"google:" + recordedTop,
		"google:" + recordedMiddle,
		"google:" + recordedGroup,
		"google:" + recordedOncall,
		"google:" + recordedPanel,
	}
	slices.Sort(want)
	if !slices.Equal(got.Groups.Members, want) {
		t.Fatalf("the recorded domain resolved to %v, want %v", got.Groups.Members, want)
	}
	if got.Subject.Name != "Mei Tanaka" {
		t.Errorf("the recorded person is called %q", got.Subject.Name)
	}
	for _, want := range []acl.Identity{
		{Source: "email", Value: "mei@acme.test"},
		{Source: "email", Value: "m.tanaka@acme.test"},
	} {
		if !slices.Contains(got.Subject.Identities, want) {
			t.Errorf("the identities off the recording are %v, and %v is not among them", got.Subject.Identities, want)
		}
	}
	// Three levels of nesting in the domain and three levels of walking here,
	// because this provider answers with direct memberships and nothing else.
	if got.Depth != 3 {
		t.Errorf("a domain three levels deep was walked %d levels", got.Depth)
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
	resolve(t, func(opts ...google.Option) *google.Directory {
		return replayed(t, rt, opts...)
	})

	if left := rt.Unused(); len(left) != 0 {
		t.Errorf("the fixture set answers %d requests the resolution never makes:\n%s", len(left), strings.Join(left, "\n"))
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
		for _, bad := range []struct{ what, needle string }{
			{"the token it was recorded with", bearer},
			// The scheme as well as the token, because a bearer is whatever
			// follows it and a reviewer skimming for "Bearer" is how one gets
			// noticed.
			{"something shaped like an access token", "Bearer "},
			// And the key, which never goes near these endpoints and would be
			// the most serious thing on this list to find in a file.
			{"something shaped like a private key", "PRIVATE KEY"},
			// A signed assertion is not a credential for long, but it is one,
			// and the only way one reaches this directory is by somebody
			// recording the token endpoint into the wrong fixture set.
			{"a token grant", "grant_type"},
		} {
			if strings.Contains(string(raw), bad.needle) {
				t.Errorf("%s holds %s", e.Name(), bad.what)
			}
		}
	}
}
