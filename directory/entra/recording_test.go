package entra_test

import (
	"bytes"
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
	"github.com/tamnd/genba/directory/entra"
)

// The fixture set is a resolution against a small tenant written down once and
// read back by the tests below, so that the adapter is exercised against the
// bytes Graph actually sends without anybody needing a tenant or an application
// registration to run the tests.
//
// The fake next to this file is the other half of the story and neither
// replaces the other. The fake is where behaviour is written: it can be made to
// deactivate somebody, to page differently, to refuse a secret. The recording is
// where the wire format is pinned: it is the answer to "did we change what we
// send" and it is the thing that would have to be refreshed if Graph changed a
// field name.
//
// Signing in is not in here on purpose. The fixtures are a description of the
// Graph and a client credentials grant is a description of the token endpoint,
// and putting the second in would mean writing a request with a client secret
// in its body down in a file. What the grant does is covered by the fake.
const fixtures = "testdata/tenant"

// The tenant the recording is of. It is the real endpoint, which nothing
// reaches, because keying the fixtures on the port a test server happened to be
// given would make them unreadable and unstable at once.
const recordedEndpoint = "https://graph.microsoft.com"

// The ids the recorded tenant holds. They are written down rather than worked
// out because a fixture set is a fixed corpus, and a test that derived them
// from the fake would pass just as happily against a recording of something
// else entirely.
const (
	recordedPerson  = "8f7c1a2b-3d4e-4f50-9a6b-1c2d3e4f5a60"
	recordedNobody  = "2b1c4d5e-6f70-4812-93a4-5b6c7d8e9f01"
	recordedLeaver  = "c3d4e5f6-0718-4923-a4b5-6c7d8e9f0a12"
	recordedTop     = "11111111-2222-4333-8444-555555555551"
	recordedMiddle  = "11111111-2222-4333-8444-555555555552"
	recordedGroup   = "11111111-2222-4333-8444-555555555553"
	recordedMissing = "99999999-9999-4999-8999-999999999999"
)

var update = flag.Bool("update", false, "record the Entra fixtures in testdata again")

// recordingTenant is the tenant the fixtures are a resolution of.
//
// It is small on purpose and every part of it is there for a reason: somebody
// three levels down a nest so that the flattening is in the recording rather
// than only in the fake, and in enough groups for the listing to be more than
// one page at the size below, somebody in none so that an empty collection is
// in there, somebody deactivated so that the refusal is too, and a group looked
// up on its own so that the endpoint the buffer usually saves is there as well.
func recordingTenant(t *testing.T) string {
	t.Helper()
	o, endpoint := newTenant(t)
	o.putGroup(t, directory.Group{ID: recordedTop, Name: "Everyone"})
	o.putGroup(t, directory.Group{ID: recordedMiddle, Name: "Engineering", MemberOf: []string{recordedTop}})
	o.putGroup(t, directory.Group{ID: recordedGroup, Name: "Storage", MemberOf: []string{recordedMiddle}})

	o.put(t, directory.Subject{
		ID:       recordedPerson,
		Name:     "Mei Tanaka",
		Email:    "mei@acme.test",
		MemberOf: []string{recordedGroup},
	})
	o.edit(t, recordedPerson, func(p *person) { p.upn = "mei@acme.test" })

	o.put(t, directory.Subject{ID: recordedNobody, Name: "Jo Okafor", Email: "jo@acme.test"})
	o.edit(t, recordedNobody, func(p *person) { p.upn = "jo@acme.test" })

	o.put(t, directory.Subject{ID: recordedLeaver, Name: "Lee Byrne", Email: "lee@acme.test", Disabled: true})
	o.edit(t, recordedLeaver, func(p *person) { p.upn = "lee@acme.test" })
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

	endpoint := recordingTenant(t)
	rec := recorded.Record(http.DefaultTransport, recorded.WithRedactedHeaders("Authorization"))

	resolve(t, func(opts ...entra.Option) *entra.Directory {
		return dial(t, endpoint, append([]entra.Option{entra.WithHTTPClient(rec.Client())}, opts...)...)
	})

	// The same question asked twice is one file. A resolution asks for a
	// membership collection once per expansion and the pages repeat across
	// them, and writing the answer down twice is two files to review and no
	// more coverage.
	var (
		seen = map[string]bool{}
		once []recorded.Exchange
	)
	for _, e := range rec.Exchanges() {
		e = settle(e, endpoint)
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
// host is written as the Graph for the same reason: a reader opening one of
// these files should see the request that would have gone to Microsoft, and
// that includes the next page link, which for this API is inside the answer
// rather than in a header.
func settle(e recorded.Exchange, endpoint string) recorded.Exchange {
	e.Request.URL = rehost(e.Request.URL)
	if len(e.Response.Body.JSON) > 0 {
		e.Response.Body.JSON = bytes.ReplaceAll(e.Response.Body.JSON, []byte(endpoint), []byte(recordedEndpoint))
	}
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
	graph, err := url.Parse(recordedEndpoint)
	if err != nil {
		panic(err)
	}
	u.Scheme, u.Host = graph.Scheme, graph.Host
	return u.String()
}

// resolve is everything the fixture set has to answer, and it is shared so that
// what is recorded and what is replayed cannot drift apart.
func resolve(t *testing.T, dir func(opts ...entra.Option) *entra.Directory) directory.Expansion {
	t.Helper()

	// Two at a time, so that a collection over pages is in the recording rather
	// than only in the fake.
	r, err := directory.New(dir(entra.WithPageSize(2)))
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
	// of the collection that already went past.
	alone := dir(entra.WithBuffer(0, 0))
	if _, err := alone.Group(t.Context(), recordedGroup); err != nil {
		t.Fatalf("looking up %s: %v", recordedGroup, err)
	}
	if _, err := alone.Group(t.Context(), recordedMissing); !errors.Is(err, directory.ErrNoGroup) {
		t.Fatalf("looking up %s gave %v, want ErrNoGroup", recordedMissing, err)
	}
	return got
}

// replayed is the adapter over the fixture set.
func replayed(t *testing.T, rt *recorded.Transport, opts ...entra.Option) *entra.Directory {
	t.Helper()
	d, err := entra.New(entra.Token(bearer), append([]entra.Option{
		entra.WithEndpoint(recordedEndpoint),
		entra.WithHTTPClient(rt.Client()),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestTheRecordedTenantResolvesWithoutATenant is the point of the fixture set:
// the whole adapter, the real paging and the real decoding, against bytes that
// came off the Graph and are now a file.
func TestTheRecordedTenantResolvesWithoutATenant(t *testing.T) {
	rt, err := recorded.Replay(fixtures)
	if err != nil {
		t.Fatal(err)
	}

	got := resolve(t, func(opts ...entra.Option) *entra.Directory {
		return replayed(t, rt, opts...)
	})

	want := []string{"entra:" + recordedTop, "entra:" + recordedMiddle, "entra:" + recordedGroup}
	slices.Sort(want)
	if !slices.Equal(got.Groups.Members, want) {
		t.Fatalf("the recorded tenant resolved to %v, want %v", got.Groups.Members, want)
	}
	if got.Subject.Name != "Mei Tanaka" {
		t.Errorf("the recorded person is called %q", got.Subject.Name)
	}
	if want := (acl.Identity{Source: "email", Value: "mei@acme.test"}); !slices.Contains(got.Subject.Identities, want) {
		t.Errorf("the identities off the recording are %v", got.Subject.Identities)
	}
	// Three levels of nesting in the tenant and one level of walking here,
	// which is the whole reason this provider is worth having.
	if got.Depth != 1 {
		t.Errorf("a provider that answers with the closure was walked %d levels deep", got.Depth)
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
	resolve(t, func(opts ...entra.Option) *entra.Directory {
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
		if strings.Contains(string(raw), bearer) {
			t.Errorf("%s holds the token it was recorded with", e.Name())
		}
		// The scheme as well as the token, because a bearer is whatever follows
		// it and a reviewer skimming for "Bearer" is how one gets noticed.
		if strings.Contains(string(raw), "Bearer ") {
			t.Errorf("%s holds something shaped like an access token", e.Name())
		}
		// And the secret, which never goes near the Graph and would be a
		// serious thing to find in a file if it did.
		if strings.Contains(string(raw), "a-client-secret") {
			t.Errorf("%s holds a client secret", e.Name())
		}
	}
}
