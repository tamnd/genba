package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// The recent screen is the one place where the server keeps something about a
// person rather than about the corpus, so most of what is worth testing here is
// that it keeps it for exactly one person and stops keeping it the moment they
// lose access to what it names.

type recentBody struct {
	Opened []struct {
		ID    string    `json:"id"`
		Title string    `json:"title"`
		At    time.Time `json:"at"`
	} `json:"opened"`
	Changed []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"changed"`
	At time.Time `json:"at"`
}

func recentServer(t *testing.T) (http.Handler, *memstore.Store) {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	perm := func(group string) acl.Permissions {
		return acl.Permissions{
			Mode:        acl.ModeACL,
			Source:      "gdrive",
			AllowGroups: []acl.Ref{{Source: "gdrive", Value: group}},
			Version:     1,
		}
	}
	day := func(n int) time.Time { return time.Date(2026, 8, n, 9, 0, 0, 0, time.UTC) }
	docs := []doc.Document{
		{
			ID: "d1", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments failover runbook", Body: "Fail the payments queue over to the replica.",
			ModifiedAt: day(1), Permissions: perm("eng@acme.com"),
		},
		{
			ID: "d2", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "On call handover", Body: "Who is holding the pager this week.",
			ModifiedAt: day(2), Permissions: perm("eng@acme.com"),
		},
		{
			ID: "d3", Tenant: "acme", Source: "salesforce", Kind: doc.KindTicket,
			Title: "Renewal for Globex", Body: "The discount expires in March.",
			ModifiedAt: day(3), Permissions: perm("sales@acme.com"),
		},
	}
	if err := st.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}).Handler(), st
}

// open posts a record of an open the way the interface does, and reports the
// status so a test can say what the endpoint answered.
func open(t *testing.T, h http.Handler, id string, headers map[string]string) int {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/recent",
		strings.NewReader(`{"id":`+quote(id)+`}`))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func quote(s string) string { return `"` + s + `"` }

func salesperson() map[string]string {
	return map[string]string{
		api.HeaderSubject: "u_sam",
		api.HeaderGroups:  "gdrive:sales@acme.com",
	}
}

func TestRecentAnswersBothHalves(t *testing.T) {
	h, _ := recentServer(t)

	if code := open(t, h, "d1", engineer()); code != http.StatusNoContent {
		t.Fatalf("POST /api/v1/recent = %d, want 204", code)
	}
	if code := open(t, h, "d2", engineer()); code != http.StatusNoContent {
		t.Fatalf("POST /api/v1/recent = %d, want 204", code)
	}

	w := request(t, h, http.MethodGet, "/api/v1/recent", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	body := decode[recentBody](t, w)

	if len(body.Opened) != 2 || body.Opened[0].ID != "d2" || body.Opened[1].ID != "d1" {
		t.Fatalf("opened = %+v, want d2 then d1", body.Opened)
	}
	if body.Opened[0].Title != "On call handover" {
		t.Fatalf("opened carries %q, want the title", body.Opened[0].Title)
	}
	if body.Opened[0].At.IsZero() {
		t.Fatal("an opened entry carries no time, so the interface has nothing to show next to it")
	}

	// Changed is the corpus this person can see, newest first, and d3 belongs to
	// somebody else.
	if len(body.Changed) != 2 || body.Changed[0].ID != "d2" || body.Changed[1].ID != "d1" {
		t.Fatalf("changed = %+v, want d2 then d1", body.Changed)
	}
	if strings.Contains(w.Body.String(), "Globex") {
		t.Fatal("the response leaked a document the caller may not read")
	}
	if body.At.IsZero() {
		t.Fatal("the response does not say when it was answered")
	}
}

func TestRecentIsOnePersonsHistory(t *testing.T) {
	h, _ := recentServer(t)
	if code := open(t, h, "d1", engineer()); code != http.StatusNoContent {
		t.Fatalf("POST /api/v1/recent = %d, want 204", code)
	}

	body := decode[recentBody](t, request(t, h, http.MethodGet, "/api/v1/recent", salesperson()))
	if len(body.Opened) != 0 {
		t.Fatalf("opened = %+v for somebody who has opened nothing", body.Opened)
	}
	if len(body.Changed) != 1 || body.Changed[0].ID != "d3" {
		t.Fatalf("changed = %+v, want only the document this reader may see", body.Changed)
	}
}

// Recording an open of something the caller may not read is a 204 and records
// nothing. Anything else would answer which ids exist.
func TestRecordOpenSaysNothingAboutWhatExists(t *testing.T) {
	h, _ := recentServer(t)
	for _, id := range []string{"d3", "no-such-document"} {
		if code := open(t, h, id, engineer()); code != http.StatusNoContent {
			t.Errorf("POST /api/v1/recent with %q = %d, want 204", id, code)
		}
	}
	if body := decode[recentBody](t, request(t, h, http.MethodGet, "/api/v1/recent", engineer())); len(body.Opened) != 0 {
		t.Fatalf("opened = %+v, want nothing: neither id was readable", body.Opened)
	}
}

func TestRecordOpenRejectsABodyItCannotRead(t *testing.T) {
	h, _ := recentServer(t)
	for _, body := range []string{"", "{}", "not json"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/recent", strings.NewReader(body))
		for k, v := range engineer() {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("POST /api/v1/recent with %q = %d, want 400", body, w.Code)
		}
	}
}

// An open is not a licence to keep reading. Access revoked after the fact takes
// the entry out of the list, and it does so without the handler filtering
// anything.
func TestRecentDropsWhatTheReaderLost(t *testing.T) {
	h, st := recentServer(t)
	if code := open(t, h, "d1", engineer()); code != http.StatusNoContent {
		t.Fatalf("POST /api/v1/recent = %d, want 204", code)
	}

	moved := doc.Document{
		ID: "d1", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
		Title: "Payments failover runbook", ModifiedAt: time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC),
		Permissions: acl.Permissions{
			Mode:        acl.ModeACL,
			Source:      "gdrive",
			AllowGroups: []acl.Ref{{Source: "gdrive", Value: "sales@acme.com"}},
			Version:     1,
		},
	}
	if err := st.Put(t.Context(), moved); err != nil {
		t.Fatalf("Put: %v", err)
	}

	body := decode[recentBody](t, request(t, h, http.MethodGet, "/api/v1/recent", engineer()))
	if len(body.Opened) != 0 {
		t.Fatalf("opened = %+v after the reader lost access to it", body.Opened)
	}
	if strings.Contains(body.Changed[0].ID, "d1") {
		t.Fatal("the changed list still carries a document this reader may not read")
	}
}

func TestRecentHonoursTheLimit(t *testing.T) {
	h, _ := recentServer(t)
	body := decode[recentBody](t, request(t, h, http.MethodGet, "/api/v1/recent?limit=1", engineer()))
	if len(body.Changed) != 1 {
		t.Fatalf("changed = %+v with a limit of one", body.Changed)
	}
	if w := request(t, h, http.MethodGet, "/api/v1/recent?limit=lots", engineer()); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d for a limit that is not a number, want 400", w.Code)
	}
}

// The endpoint is revalidated rather than reused, like every other read, and
// the tag has to survive the response saying when it was answered.
func TestRecentRevalidates(t *testing.T) {
	h, _ := recentServer(t)
	w := request(t, h, http.MethodGet, "/api/v1/recent", engineer())
	tag := w.Header().Get("ETag")
	if tag == "" {
		t.Fatal("no ETag, so the interface has to download the list every time")
	}
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "private") {
		t.Fatalf("Cache-Control = %q, want a private response", got)
	}

	headers := engineer()
	headers["If-None-Match"] = tag
	again := request(t, h, http.MethodGet, "/api/v1/recent", headers)
	if again.Code != http.StatusNotModified {
		t.Fatalf("status = %d for a request carrying the tag it was given, want 304", again.Code)
	}

	// And it stops matching when the list changes.
	if code := open(t, h, "d1", engineer()); code != http.StatusNoContent {
		t.Fatalf("POST /api/v1/recent = %d, want 204", code)
	}
	changed := request(t, h, http.MethodGet, "/api/v1/recent", headers)
	if changed.Code != http.StatusOK {
		t.Fatalf("status = %d after the history changed, want 200", changed.Code)
	}
}

func TestRecentNeedsACredential(t *testing.T) {
	h, _ := recentServer(t)
	if w := request(t, h, http.MethodGet, "/api/v1/recent", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/recent = %d, want 401", w.Code)
	}
	if code := open(t, h, "d1", nil); code != http.StatusUnauthorized {
		t.Fatalf("POST /api/v1/recent = %d, want 401", code)
	}
}
