package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/audit"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/recheck"
	"github.com/tamnd/genba/store/memstore"
)

// This file is inside the package for the reason surface_test.go is: it walks
// the route table. What it enforces is the other half of the same promise. The
// audit walk says every surface that can put a document in front of somebody
// writes a record, and this one says every one of them asks the source first.

// probes is how each content surface is asked for something it will actually
// answer with.
//
// It is a second table beside the one the audit walk uses, and the difference
// is the point. That walk needs a request each surface answers; this one needs
// a request each surface answers with a document in it, because a page that
// came back empty proves nothing about whether the rows in it were checked. A
// content route missing from here fails the walk rather than being skipped.
var probes = map[string]string{
	"GET /api/v1/search":                   "/api/v1/search?q=payments",
	"GET /api/v1/suggest":                  "/api/v1/suggest?q=payments",
	"GET /api/v1/documents":                "/api/v1/documents?id=d1&id=img",
	"GET /api/v1/documents/{id}":           "/api/v1/documents/d1",
	"GET /api/v1/documents/{id}/content":   "/api/v1/documents/img/content",
	"GET /api/v1/documents/{id}/thumbnail": "/api/v1/documents/img/thumbnail?size=96",
	"GET /api/v1/reported":                 "/api/v1/reported",
	"GET /api/v1/recent":                   "/api/v1/recent",
}

// TestEverySurfaceThatServesContentRunsTheRecheck asks every content route with
// a source that says no to everything, and requires that nothing of the corpus
// comes back, whatever shape that route's answer has.
//
// A rule that holds on the search page and not on the preview panel is not a
// rule, and the way somebody finds the surface that was missed is by opening
// the one screen nobody thought about.
func TestEverySurfaceThatServesContentRunsTheRecheck(t *testing.T) {
	for _, rt := range served(t) {
		pattern := rt.Method + " " + rt.Pattern
		t.Run(pattern, func(t *testing.T) {
			target, ok := probes[pattern]
			if !ok {
				t.Fatalf("%s serves content and this walk does not know how to make it serve some, add it to probes in recheck_test.go", pattern)
			}

			set := recheck.New()
			set.Add("gdrive", denying())
			_, body := ask(t, checking(t, set), rt.Method, target)

			if named := corpusIn(body); len(named) != 0 {
				t.Errorf("%s served %v after the source withdrew them:\n%s", pattern, named, body)
			}
			if st := set.Stats()["gdrive"]; st.Checked == 0 {
				t.Errorf("%s asked the source about nothing at all", pattern)
			}
		})
	}
}

// TestTheSameSurfacesServeTheCorpusWhenTheSourceSaysYes walks the same routes
// with a source that allows everything, and requires the corpus back.
//
// Without this the walk above would pass on a server that answered every
// request with an empty page, which is a thing a bad refactor can produce and a
// walk looking only for the absence of an id would call a success. It is also
// what stops a probe above from quietly rotting into a request that finds
// nothing.
func TestTheSameSurfacesServeTheCorpusWhenTheSourceSaysYes(t *testing.T) {
	for _, rt := range served(t) {
		pattern := rt.Method + " " + rt.Pattern
		t.Run(pattern, func(t *testing.T) {
			target := probes[pattern]

			set := recheck.New()
			set.Add("gdrive", allowing())
			code, body := ask(t, checking(t, set), rt.Method, target)

			if code != http.StatusOK {
				t.Fatalf("%s answered %d with the source allowing everything:\n%s", pattern, code, body)
			}
			if len(corpusIn(body)) == 0 && !strings.HasPrefix(body, "\x89PNG") {
				t.Errorf("%s served nothing of the corpus, so the walk above says nothing about it:\n%s", pattern, body)
			}
			if st := set.Stats()["gdrive"]; st.Denied != 0 || st.Failed != 0 {
				t.Errorf("%s removed rows the source allowed: %+v", pattern, st)
			}
		})
	}
}

// corpusIn is which of the corpus a response names.
//
// The image surfaces answer with bytes rather than JSON, and their id is in the
// request instead, which is why the walk above reads the count of checks as
// well as the body.
func corpusIn(body string) []string {
	var named []string
	for _, id := range []string{`"d1"`, `"img"`} {
		if strings.Contains(body, id) {
			named = append(named, id)
		}
	}
	return named
}

// TestASourceWithNoCheckerIsServedFromTheIndex, which is what off by default
// means from outside: a deployment that has registered nothing gets exactly the
// behaviour it had before this existed, and so does a source its neighbour is
// checking.
func TestASourceWithNoCheckerIsServedFromTheIndex(t *testing.T) {
	set := recheck.New()
	set.Add("slack", denying())

	code, body := ask(t, checking(t, set), http.MethodGet, "/api/v1/documents/d1")
	if code != http.StatusOK {
		t.Fatalf("a document of an unchecked source answered %d:\n%s", code, body)
	}
	if st := set.Stats()["slack"]; st.Checked != 0 {
		t.Errorf("the slack checker was asked about a drive document: %+v", st)
	}
}

// TestNoRecheckIsNoQuestion. The server built without the option is the one
// almost every deployment runs, and it must not have grown a lookup, a lock or
// a clock on the page path.
func TestNoRecheckIsNoQuestion(t *testing.T) {
	st := corpus(t)
	s := New(st, index.New(st), HeaderAuth{Tenant: "acme"})
	t.Cleanup(func() { _ = s.Close() })

	if s.recheck != nil {
		t.Fatal("a server built with no options is rechecking")
	}
	if _, ok := s.cacheStats()["recheck"]; ok {
		t.Error("a server that checks nothing publishes a cache layer for it")
	}
	code, body := ask(t, s.Handler(), http.MethodGet, "/api/v1/documents/d1")
	if code != http.StatusOK {
		t.Fatalf("the document answered %d:\n%s", code, body)
	}
}

// TestADocumentTheSourceWithdrewLooksLikeOneThatIsNotThere holds the two
// answers to the same status code and the same sentence.
//
// Telling somebody their access was withdrawn tells them the document exists
// and roughly what it is about, which is most of what the withdrawal was for.
func TestADocumentTheSourceWithdrewLooksLikeOneThatIsNotThere(t *testing.T) {
	set := recheck.New()
	set.Add("gdrive", denying())
	h := checking(t, set)

	withdrawn, body := ask(t, h, http.MethodGet, "/api/v1/documents/d1")
	missing, other := ask(t, h, http.MethodGet, "/api/v1/documents/nothing-like-this")
	if withdrawn != http.StatusNotFound {
		t.Fatalf("a withdrawn document answered %d:\n%s", withdrawn, body)
	}
	if withdrawn != missing || body != other {
		t.Errorf("a withdrawn document answers %d %q and a missing one answers %d %q",
			withdrawn, body, missing, other)
	}
}

// TestTheTotalComesDownWithThePage, because a count is an answer.
//
// A query specific enough to match one document says everything a title would
// have said: there is a document about this, and you may not read it. So the
// rows this page dropped come off the total, which leaves a floor rather than a
// recount and is the direction to be wrong in.
func TestTheTotalComesDownWithThePage(t *testing.T) {
	set := recheck.New()
	set.Add("gdrive", denying())

	st := corpus(t)
	s := New(st, index.New(st), HeaderAuth{Tenant: "acme"}, WithRecheck(set))
	t.Cleanup(func() { _ = s.Close() })

	code, body := ask(t, s.Handler(), http.MethodGet, "/api/v1/search?q=payments")
	if code != http.StatusOK {
		t.Fatalf("the search answered %d:\n%s", code, body)
	}
	var out struct {
		Total int                   `json:"total"`
		Hits  []struct{ ID string } `json:"hits"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Hits) != 0 {
		t.Fatalf("the source withdrew everything and the page has %d rows:\n%s", len(out.Hits), body)
	}
	if out.Total != 0 {
		t.Errorf("the page is empty and the total says %d, which says a document exists:\n%s", out.Total, body)
	}
}

// TestACheckThatFailsRemovesTheRowAndSaysSoInTheMetrics, which is the promise
// that makes failing closed operable. A deployment cannot see the documents
// nobody was shown, so the number of them has to be somewhere an operator
// already looks.
func TestACheckThatFailsRemovesTheRowAndSaysSoInTheMetrics(t *testing.T) {
	set := recheck.New()
	set.Add("gdrive", recheck.Func(func(context.Context, *acl.Principal, []string) (map[string]bool, error) {
		return nil, errors.New("the drive is not answering")
	}))

	st := corpus(t)
	s := New(st, index.New(st), HeaderAuth{Tenant: "acme"}, WithRecheck(set))
	t.Cleanup(func() { _ = s.Close() })

	code, body := ask(t, s.Handler(), http.MethodGet, "/api/v1/search?q=payments")
	if code != http.StatusOK {
		t.Fatalf("the search answered %d:\n%s", code, body)
	}
	if strings.Contains(body, `"d1"`) {
		t.Errorf("a document whose check failed was served anyway:\n%s", body)
	}

	scraped := scrape(t, s)
	for _, want := range []string{
		MetricRecheckChecked + `{source="gdrive"} 1`,
		MetricRecheckFailed + `{source="gdrive"} 1`,
		MetricRecheckDenied + `{source="gdrive"} 0`,
	} {
		if !strings.Contains(scraped, want) {
			t.Errorf("the metrics do not carry %q:\n%s", want, scraped)
		}
	}
}

// TestADeniedDocumentIsCountedApartFromAFailedOne. They call for different
// actions: one is the feature working and the other is the feature costing
// somebody a result, and an operator who cannot tell them apart has one number
// that means either.
func TestADeniedDocumentIsCountedApartFromAFailedOne(t *testing.T) {
	set := recheck.New()
	set.Add("gdrive", denying())

	st := corpus(t)
	s := New(st, index.New(st), HeaderAuth{Tenant: "acme"}, WithRecheck(set))
	t.Cleanup(func() { _ = s.Close() })

	if code, body := ask(t, s.Handler(), http.MethodGet, "/api/v1/search?q=payments"); code != http.StatusOK {
		t.Fatalf("the search answered %d:\n%s", code, body)
	}

	scraped := scrape(t, s)
	for _, want := range []string{
		MetricRecheckDenied + `{source="gdrive"} 1`,
		MetricRecheckFailed + `{source="gdrive"} 0`,
		MetricCacheHits + `{layer="recheck"}`,
	} {
		if !strings.Contains(scraped, want) {
			t.Errorf("the metrics do not carry %q:\n%s", want, scraped)
		}
	}
}

// TestAWithdrawnDocumentIsARefusalOnTheTrail. The record is the only place an
// investigation can see that somebody asked for a document at the moment their
// access to it had just gone, and a surface that silently answered 404 without
// one would lose that.
func TestAWithdrawnDocumentIsARefusalOnTheTrail(t *testing.T) {
	set := recheck.New()
	set.Add("gdrive", denying())

	sink := &recorder{}
	log := audit.New(sink)
	t.Cleanup(func() { _ = log.Close() })

	st := corpus(t)
	s := New(st, index.New(st), HeaderAuth{Tenant: "acme"}, WithAudit(log), WithRecheck(set))
	t.Cleanup(func() { _ = s.Close() })

	if code, _ := ask(t, s.Handler(), http.MethodGet, "/api/v1/documents/d1"); code != http.StatusNotFound {
		t.Fatalf("a withdrawn document answered %d", code)
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got := sink.records()
	if len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
	if got[0].Outcome != audit.Refused {
		t.Errorf("the record says %q", got[0].Outcome)
	}
	if got[0].Rule != "" {
		t.Errorf("a refusal carries the rule %q", got[0].Rule)
	}
}

// checking is a server over the walk's corpus with one set of checkers on it.
func checking(t *testing.T, set *recheck.Set) http.Handler {
	t.Helper()
	st := lived(t)
	s := New(st, index.New(st), HeaderAuth{Tenant: "acme"}, WithRecheck(set))
	t.Cleanup(func() { _ = s.Close() })
	return s.Handler()
}

// lived is the walk's corpus with a little history on it.
//
// Two of the screens this walks are about what happened rather than about what
// matched, and on a corpus nobody has ever opened or complained about they are
// both empty. So the history is made here, through the API rather than into the
// driver, which is the only way to be sure it is the history a person would
// have left behind.
func lived(t *testing.T) *memstore.Store {
	t.Helper()
	st := corpus(t)

	s := New(st, index.New(st), HeaderAuth{Tenant: "acme"})
	t.Cleanup(func() { _ = s.Close() })
	h := s.Handler()

	if code, body := send(t, h, http.MethodPost, "/api/v1/documents/d1/stale",
		`{"note":"the failover section names a queue that was retired"}`); code != http.StatusOK {
		t.Fatalf("reporting a document answered %d:\n%s", code, body)
	}
	if code, body := send(t, h, http.MethodPost, "/api/v1/recent", `{"id":"img"}`); code != http.StatusNoContent {
		t.Fatalf("recording an open answered %d:\n%s", code, body)
	}
	return st
}

// ask makes one request as the engineer the walk authenticates as, and returns
// what came back rather than failing on it, because half of what this file
// asserts is that a surface answered 404.
func ask(t *testing.T, h http.Handler, method, target string) (code int, body string) {
	t.Helper()
	return send(t, h, method, target, "")
}

// send is ask with a body.
func send(t *testing.T, h http.Handler, method, target, body string) (code int, out string) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(t.Context(), method, target, nil)
	} else {
		r = httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	}
	r.Header.Set(HeaderSubject, "u_mei")
	r.Header.Set(HeaderGroups, "gdrive:eng@acme.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// scrape reads the metrics the way a collector does.
func scrape(t *testing.T, s *Server) string {
	t.Helper()
	w := httptest.NewRecorder()
	s.Metrics().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return w.Body.String()
}

// denying is a source that has withdrawn everything, which is the state this
// whole package exists to notice between two syncs.
func denying() recheck.Checker {
	return recheck.Func(func(_ context.Context, _ *acl.Principal, ids []string) (map[string]bool, error) {
		out := make(map[string]bool, len(ids))
		for _, id := range ids {
			out[id] = false
		}
		return out, nil
	})
}

// allowing is a source that agrees with the index.
func allowing() recheck.Checker {
	return recheck.Func(func(_ context.Context, _ *acl.Principal, ids []string) (map[string]bool, error) {
		out := make(map[string]bool, len(ids))
		for _, id := range ids {
			out[id] = true
		}
		return out, nil
	})
}
