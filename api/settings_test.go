package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
	"github.com/tamnd/genba/store/sqlitestore"
)

// The facts the settings screen prints about the deployment. They are asserted
// here rather than left to the browser gate because the split between them is a
// decision rather than a layout: what a deployment runs on is on the endpoint
// that asks for a credential, and what this build is is on the one that does
// not.

func newNamedServer(t *testing.T, driver string, now func() time.Time) (http.Handler, *memstore.Store) {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	opts := []api.Option{api.WithDriver(driver)}
	if now != nil {
		opts = append(opts, api.WithClock(now))
	}
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, opts...)
	return s.Handler(), st
}

type statsBody struct {
	Driver    string `json:"driver"`
	IndexedAt string `json:"indexed_at"`
	Ranking   bool   `json:"ranking"`
}

// Which of the two query paths this deployment is on.
//
// The two answer the same queries with the same results and differ only in the
// clock, by a factor of a hundred on a few hundred documents, so nothing except
// this says which one somebody got. The performance gate was pointed at the
// scanning driver for months and printed a real number for a deployment nobody
// has, which is the failure this exists to make impossible.
func TestStatsSayWhetherTheDriverRanksForItself(t *testing.T) {
	h, _ := newNamedServer(t, "memory", nil)
	w := request(t, h, http.MethodGet, "/api/v1/stats", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if decode[statsBody](t, w).Ranking {
		t.Error("the reference driver reports that it ranks in the driver")
	}

	sq, err := sqlitestore.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })

	s := api.New(sq, index.New(sq), api.HeaderAuth{Tenant: "acme"}, api.WithDriver("sqlite"))
	w = request(t, s.Handler(), http.MethodGet, "/api/v1/stats", engineer())
	if !decode[statsBody](t, w).Ranking {
		t.Error("the sqlite driver reports that it is scanned")
	}
}

func TestStatsNameTheStoreThisProcessWasStartedWith(t *testing.T) {
	h, _ := newNamedServer(t, "sqlite", nil)

	w := request(t, h, http.MethodGet, "/api/v1/stats", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := decode[statsBody](t, w).Driver; got != "sqlite" {
		t.Errorf("driver = %q, want sqlite", got)
	}

	// An embedder that named no driver says nothing rather than guessing at one
	// from a Go type name that nobody typed on a command line.
	h, _ = newNamedServer(t, "", nil)
	w = request(t, h, http.MethodGet, "/api/v1/stats", engineer())
	if got := decode[statsBody](t, w).Driver; got != "" {
		t.Errorf("a server that was given no driver name reported %q", got)
	}
}

// A process that has just come up has not seen a sync, and saying so is more
// honest than reporting one it was not there for.
func TestStatsReportTheLastSyncOnlySinceThisProcessCameUp(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	h, st := newNamedServer(t, "memory", func() time.Time { return at })

	w := request(t, h, http.MethodGet, "/api/v1/stats", engineer())
	if got := decode[statsBody](t, w).IndexedAt; got != "" {
		t.Fatalf("indexed_at = %q before anything was written, want empty", got)
	}

	perm := acl.Permissions{Mode: acl.ModePublicToTenant, Source: "gdrive", Version: 1}
	d := doc.Document{
		ID: "d1", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
		Title: "Payments failover runbook", Permissions: perm,
	}
	if err := st.Put(t.Context(), d); err != nil {
		t.Fatalf("Put: %v", err)
	}

	w = request(t, h, http.MethodGet, "/api/v1/stats", engineer())
	if got := decode[statsBody](t, w).IndexedAt; got != at.Format(time.RFC3339) {
		t.Errorf("indexed_at = %q, want %q", got, at.Format(time.RFC3339))
	}
}

// The unauthenticated endpoint says what this build is and nothing about what
// it was pointed at. Anybody who can reach the port can read it.
func TestHealthSaysWhatThisBuildIsAndNotWhatItRunsOn(t *testing.T) {
	h, _ := newNamedServer(t, "postgres", nil)

	w := request(t, h, http.MethodGet, "/healthz", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := decode[map[string]string](t, w)
	if _, ok := body["built"]; !ok {
		t.Errorf("healthz said nothing about when this was built: %v", body)
	}
	for _, key := range []string{"driver", "indexed_at"} {
		if v, ok := body[key]; ok {
			t.Errorf("healthz told an anonymous caller %s = %q", key, v)
		}
	}
}

// The first question somebody asks when a document says they may not read it is
// which groups they are in, and the answer has to be the server's rather than
// whatever the browser last sent.
func TestMeReportsTheGroupsTheAuthenticatorResolved(t *testing.T) {
	type meBody struct {
		Groups []string `json:"groups"`
	}
	h, _ := newNamedServer(t, "memory", nil)

	w := request(t, h, http.MethodGet, "/api/v1/me", map[string]string{
		api.HeaderSubject: "u_mei",
		api.HeaderGroups:  "gdrive:eng@acme.com,gdrive:oncall@acme.com",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decode[meBody](t, w).Groups
	if len(got) != 2 || got[0] != "gdrive:eng@acme.com" || got[1] != "gdrive:oncall@acme.com" {
		t.Errorf("groups = %v, want both groups the header carried", got)
	}

	w = request(t, h, http.MethodGet, "/api/v1/me", map[string]string{api.HeaderSubject: "u_nobody"})
	if got := decode[meBody](t, w).Groups; len(got) != 0 {
		t.Errorf("a principal in no groups was told they are in %v", got)
	}
}
