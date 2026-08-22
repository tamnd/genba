package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// What the server says while a source is still being read for the first time.
//
// The split is the same one the settings screen made and it is made for the
// same reason. How much of a corpus there is and what the sources are called
// goes to a caller who authenticated. Whether a crawl is running goes to
// anybody, because the thing that needs to know is a readiness probe and it
// does not carry a credential.

func newIndexingServer(t *testing.T, state func() (api.Indexing, bool)) http.Handler {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, api.WithIndexing(state))
	return s.Handler()
}

type indexingBody struct {
	Indexing *struct {
		Source string `json:"source"`
		Done   int    `json:"done"`
		Total  int    `json:"total"`
	} `json:"indexing"`
}

func TestStatsReportsTheSourceStillBeingRead(t *testing.T) {
	h := newIndexingServer(t, func() (api.Indexing, bool) {
		return api.Indexing{Source: "notes", Done: 4182, Total: 22235}, true
	})

	w := request(t, h, http.MethodGet, "/api/v1/stats", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decode[indexingBody](t, w).Indexing
	if got == nil {
		t.Fatal("stats said nothing about the sync that is running")
	}
	if got.Source != "notes" || got.Done != 4182 || got.Total != 22235 {
		t.Errorf("indexing = %+v, want the source and both numbers", *got)
	}
}

// Absent rather than a zero object, so the interface renders its banner on the
// key being there and never has to compare two numbers to work out whether a
// sync is running.
func TestStatsLeavesIndexingOutWhenNothingIsIndexing(t *testing.T) {
	for _, state := range []func() (api.Indexing, bool){
		nil,
		func() (api.Indexing, bool) { return api.Indexing{}, false },
	} {
		h := newIndexingServer(t, state)
		w := request(t, h, http.MethodGet, "/api/v1/stats", engineer())
		if got := decode[indexingBody](t, w).Indexing; got != nil {
			t.Errorf("stats reported %+v with nothing being indexed", *got)
		}
	}
}

// The readiness check says whether to wait and refuses to say what for. It is
// unauthenticated, and how large somebody's corpus is and what their sources are
// called are not facts to hand to whoever can reach the port.
func TestReadinessSaysThatASyncIsRunningAndNotWhatItIs(t *testing.T) {
	h := newIndexingServer(t, func() (api.Indexing, bool) {
		return api.Indexing{Source: "payroll", Done: 12, Total: 900}, true
	})

	w := request(t, h, http.MethodGet, "/readyz", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, a process reading a source still answers", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"indexing":true`) {
		t.Errorf("readyz did not say a sync is running: %s", body)
	}
	for _, leak := range []string{"payroll", "900"} {
		if strings.Contains(body, leak) {
			t.Errorf("readyz told an anonymous caller %q: %s", leak, body)
		}
	}
}

func TestReadinessIsSilentWhenNothingIsIndexing(t *testing.T) {
	h := newIndexingServer(t, func() (api.Indexing, bool) { return api.Indexing{}, false })

	w := request(t, h, http.MethodGet, "/readyz", nil)
	if body := w.Body.String(); strings.Contains(body, "indexing") {
		t.Errorf("readyz mentioned indexing with nothing being indexed: %s", body)
	}
}
