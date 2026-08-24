package okta_test

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
	"github.com/tamnd/genba/directory/okta"
)

// The fixture set is a resolution against a small organisation written down
// once and read back by the tests below, so that the adapter is exercised
// against the bytes an Okta organisation actually sends without anybody needing
// a tenant or an API token to run the tests.
//
// The fake next to this file is the other half of the story and neither
// replaces the other. The fake is where behaviour is written: it can be made to
// suspend somebody, to page differently, to refuse a token. The recording is
// where the wire format is pinned: it is the answer to "did we change what we
// send" and it is the thing that would have to be refreshed if Okta changed a
// field name.
const fixtures = "testdata/organisation"

// The organisation the recording is of. It is a real host that nothing reaches,
// because keying the fixtures on the port a test server happened to be given
// would make them unreadable and unstable at once.
const recordedOrg = "https://acme.okta.com"

// The ids the recorded organisation holds. They are written down rather than
// worked out because a fixture set is a fixed corpus, and a test that derived
// them from the fake would pass just as happily against a recording of
// something else entirely.
const (
	recordedPerson  = "00u1mei"
	recordedNobody  = "00u1jo"
	recordedLeaver  = "00u1lee"
	recordedGroup   = "00g1sec"
	recordedMissing = "00g1gone"
)

var update = flag.Bool("update", false, "record the Okta fixtures in testdata again")

// recordingOrg is the organisation the fixtures are a resolution of.
//
// It is small on purpose and every part of it is there for a reason: somebody
// in three groups so that the listing is more than one page at the size below,
// somebody in none so that an empty listing is in the recording, somebody
// suspended so that the refusal is too, and a group looked up on its own so
// that the endpoint the buffer usually saves is in there as well.
func recordingOrg(t *testing.T) string {
	t.Helper()
	o, base := newOrg(t)
	o.putGroup(t, directory.Group{ID: "00g1eng", Name: "engineering"})
	o.putGroup(t, directory.Group{ID: "00g1ops", Name: "operations"})
	o.putGroup(t, directory.Group{ID: recordedGroup, Name: "security"})

	o.put(t, directory.Subject{
		ID:       recordedPerson,
		Name:     "Mei Tanaka",
		Email:    "mei@acme.test",
		MemberOf: []string{"00g1eng", "00g1ops", recordedGroup},
	})
	o.put(t, directory.Subject{ID: recordedNobody, Email: "jo@acme.test"})
	o.put(t, directory.Subject{ID: recordedLeaver, Email: "lee@acme.test", Disabled: true})
	return base
}

// TestRecordTheFixtures writes the fixture set again. It is skipped unless the
// flag is given, because a recording that refreshed itself on every run would
// never fail: the thing it is there to catch is a change to what we send, and a
// fixture regenerated alongside the change agrees with it by construction.
func TestRecordTheFixtures(t *testing.T) {
	if !*update {
		t.Skip("run with -update to record the fixtures again")
	}

	base := recordingOrg(t)
	rec := recorded.Record(http.DefaultTransport, recorded.WithRedactedHeaders("Authorization"))

	resolve(t, func(opts ...okta.Option) *okta.Directory {
		return dial(t, base, append([]okta.Option{okta.WithHTTPClient(rec.Client())}, opts...)...)
	})

	// The same question asked twice is one file. A resolution asks for a group
	// listing once per expansion and the pages repeat across them, and writing
	// the answer down twice is two files to review and no more coverage.
	var (
		seen = map[string]bool{}
		once []recorded.Exchange
	)
	for _, e := range rec.Exchanges() {
		e = settle(e, base)
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
// host is written as an Okta one for the same reason: a reader opening one of
// these files should see the request that would have gone to Okta, and that
// includes the next page link inside the answer.
func settle(e recorded.Exchange, base string) recorded.Exchange {
	e.Request.URL = rehost(e.Request.URL)
	for name, values := range e.Response.Headers {
		if !strings.EqualFold(name, "Link") {
			continue
		}
		for i, v := range values {
			values[i] = strings.ReplaceAll(v, base, recordedOrg)
		}
	}
	delete(e.Response.Headers, "Date")
	delete(e.Response.Headers, "Content-Length")
	return e
}

// rehost puts one URL on the organisation the fixtures are written against.
func rehost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	org, err := url.Parse(recordedOrg)
	if err != nil {
		panic(err)
	}
	u.Scheme, u.Host = org.Scheme, org.Host
	return u.String()
}

// resolve is everything the fixture set has to answer, and it is shared so that
// what is recorded and what is replayed cannot drift apart.
func resolve(t *testing.T, dir func(opts ...okta.Option) *okta.Directory) directory.Expansion {
	t.Helper()

	// Two at a time, so that a listing over pages is in the recording rather
	// than only in the fake.
	r, err := directory.New(dir(okta.WithPageSize(2)))
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
	alone := dir(okta.WithBuffer(0, 0))
	if _, err := alone.Group(t.Context(), recordedGroup); err != nil {
		t.Fatalf("looking up %s: %v", recordedGroup, err)
	}
	if _, err := alone.Group(t.Context(), recordedMissing); !errors.Is(err, directory.ErrNoGroup) {
		t.Fatalf("looking up %s gave %v, want ErrNoGroup", recordedMissing, err)
	}
	return got
}

// TestTheRecordedOrganisationResolvesWithoutATenant is the point of the fixture
// set: the whole adapter, the real paging and the real decoding, against bytes
// that came off an organisation and are now a file.
func TestTheRecordedOrganisationResolvesWithoutATenant(t *testing.T) {
	rt, err := recorded.Replay(fixtures)
	if err != nil {
		t.Fatal(err)
	}

	got := resolve(t, func(opts ...okta.Option) *okta.Directory {
		d, err := okta.New(recordedOrg, token, append([]okta.Option{okta.WithHTTPClient(rt.Client())}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		return d
	})

	want := []string{"okta:00g1eng", "okta:00g1ops", "okta:" + recordedGroup}
	if !slices.Equal(got.Groups.Members, want) {
		t.Fatalf("the recorded organisation resolved to %s, want %v", describe(got.Groups.Members), want)
	}
	if got.Subject.Name != "Mei Tanaka" {
		t.Errorf("the recorded person is called %q", got.Subject.Name)
	}
	if want := (acl.Identity{Source: "email", Value: "mei@acme.test"}); !slices.Contains(got.Subject.Identities, want) {
		t.Errorf("the identities off the recording are %v", got.Subject.Identities)
	}
	if got.Depth != 1 {
		t.Errorf("a provider whose groups cannot contain groups was walked %d levels deep", got.Depth)
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
	resolve(t, func(opts ...okta.Option) *okta.Directory {
		d, err := okta.New(recordedOrg, token, append([]okta.Option{okta.WithHTTPClient(rt.Client())}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		return d
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
		if strings.Contains(string(raw), token) {
			t.Errorf("%s holds the token it was recorded with", e.Name())
		}
		// The scheme as well as the token, because an API token is whatever
		// follows it and a reviewer skimming for "SSWS" is how one gets noticed.
		if strings.Contains(string(raw), "SSWS ") {
			t.Errorf("%s holds something shaped like an Okta API token", e.Name())
		}
	}
}
