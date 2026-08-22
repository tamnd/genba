package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
)

// Changing the connectors from the interface.
//
// The endpoint itself is thin on purpose: it checks the role, reads the shape
// of the request, records who asked for what, and hands the work to whatever
// runs the crawlers. So what is tested here is the gate, the shape and the way
// a refusal from below turns into a status somebody can act on, and none of it
// needs a real crawler.

// fakeSupervisor records what it was asked to do and answers with whatever it
// was told to.
type fakeSupervisor struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeSupervisor) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.err
}

func (f *fakeSupervisor) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSupervisor) Add(_ context.Context, fd store.Feed) error {
	return f.record(fmt.Sprintf("add %s/%s kind=%s enabled=%t by=%s config=%s",
		fd.Tenant, fd.Source, fd.Kind, fd.Enabled, fd.By, fd.Config))
}

func (f *fakeSupervisor) Remove(_ context.Context, tenant, source string) error {
	return f.record("remove " + tenant + "/" + source)
}

func (f *fakeSupervisor) Start(_ context.Context, tenant, source string) error {
	return f.record("start " + tenant + "/" + source)
}

func (f *fakeSupervisor) Stop(_ context.Context, tenant, source string) error {
	return f.record("stop " + tenant + "/" + source)
}

func (f *fakeSupervisor) Sync(_ context.Context, tenant, source string) error {
	return f.record("sync " + tenant + "/" + source)
}

// newConnectorServer is a server with one connector already running and a
// supervisor that can be asked to change it.
func newConnectorServer(t *testing.T, sup api.Supervisor) http.Handler {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	ops := func() api.Operations {
		return api.Operations{Connectors: []api.Connector{
			{Source: "handbook", Kind: "corpus", Target: "/srv/handbook", Tenant: "acme", Enabled: true, Managed: true},
		}}
	}
	opts := []api.Option{api.WithOperations(ops)}
	if sup != nil {
		opts = append(opts, api.WithSupervisor(sup))
	}
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, opts...)
	return s.Handler()
}

// post is request with a body, which the shared helper has no room for.
func post(t *testing.T, h http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The gate is the same one the rest of the administration is behind, and it is
// worth checking on the write half rather than assuming, because this is the
// half where being wrong means somebody else's search index gets a new crawler.
func TestChangingAConnectorNeedsTheRole(t *testing.T) {
	sup := &fakeSupervisor{}
	h := newConnectorServer(t, sup)

	for _, c := range []struct {
		method, target, body string
	}{
		{http.MethodPost, "/api/v1/admin/connectors", `{"source":"x","kind":"corpus"}`},
		{http.MethodDelete, "/api/v1/admin/connectors/handbook", ""},
		{http.MethodPost, "/api/v1/admin/connectors/handbook/start", ""},
		{http.MethodPost, "/api/v1/admin/connectors/handbook/stop", ""},
		{http.MethodPost, "/api/v1/admin/connectors/handbook/sync", ""},
	} {
		w := post(t, h, c.method, c.target, c.body, engineer())
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", c.method, c.target, w.Code)
		}
	}
	if calls := sup.seen(); len(calls) != 0 {
		t.Fatalf("a request that was refused still reached the supervisor: %v", calls)
	}
}

// The tenant is the operator's own and is never read out of the request. An
// administrator of one deployment naming another tenant would otherwise be able
// to point a crawler at a corpus that is not theirs.
func TestAddingAConnectorFilesItUnderTheOperatorsOwnTenant(t *testing.T) {
	sup := &fakeSupervisor{}
	h := newConnectorServer(t, sup)

	body := `{"source":"archive","kind":"bucket","enabled":true,"config":{"bucket":"docs","endpoint":"https://s3.example"},"tenant":"other"}`
	w := post(t, h, http.MethodPost, "/api/v1/admin/connectors", body, operator())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}

	calls := sup.seen()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want one", calls)
	}
	if !strings.HasPrefix(calls[0], "add acme/archive kind=bucket enabled=true by=u_ops") {
		t.Errorf("the supervisor was asked for %q", calls[0])
	}
	// The settings go through untouched, because the API does not know what a
	// bucket needs and a field it reformatted is a field a connector cannot
	// read.
	if !strings.Contains(calls[0], `"endpoint":"https://s3.example"`) {
		t.Errorf("the configuration did not arrive intact: %q", calls[0])
	}
}

// A mutation answers with the list the screen draws, so that adding a connector
// is one request rather than one to change it and one to find out what changed.
func TestChangingAConnectorAnswersWithTheConnectors(t *testing.T) {
	h := newConnectorServer(t, &fakeSupervisor{})

	w := post(t, h, http.MethodPost, "/api/v1/admin/connectors/handbook/sync", "", operator())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var body struct {
		Connectors []struct {
			Source  string `json:"source"`
			Enabled bool   `json:"enabled"`
			Managed bool   `json:"managed"`
		} `json:"connectors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v\nbody: %s", err, w.Body)
	}
	if len(body.Connectors) != 1 || body.Connectors[0].Source != "handbook" {
		t.Fatalf("connectors = %+v", body.Connectors)
	}
	if !body.Connectors[0].Enabled || !body.Connectors[0].Managed {
		t.Errorf("the connector came back as %+v, want it enabled and managed", body.Connectors[0])
	}
	// Never cached. This is the state of a deployment somebody is changing, and
	// an answer a proxy kept would be an answer that outlives the change.
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-cache") && !strings.Contains(got, "max-age=0") {
		t.Errorf("Cache-Control = %q", got)
	}
}

// The three refusals that are not a server fault each want a different thing
// from whoever is reading, so each one gets its own status.
func TestAConnectorRefusalSaysWhichKindItIs(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"a connector nobody configured", fmt.Errorf("%w: nope", genba.ErrNotFound), http.StatusNotFound},
		{"a connector from the command line", api.ErrUnmanaged, http.StatusConflict},
		{"settings that cannot be run", fmt.Errorf("%w: refresh is not a duration", api.ErrBadConnector), http.StatusBadRequest},
		{"anything else", errString("the disk is full"), http.StatusInternalServerError},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newConnectorServer(t, &fakeSupervisor{err: c.err})
			w := post(t, h, http.MethodPost, "/api/v1/admin/connectors/handbook/start", "", operator())
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.want, w.Body)
			}
		})
	}
}

// Settings that cannot be run come back with the supervisor's own message,
// because the useful half of it is the part the API cannot write: which field
// is wrong and why.
func TestBadSettingsSayWhichFieldIsWrong(t *testing.T) {
	sup := &fakeSupervisor{err: fmt.Errorf("%w: refresh is not a duration, try 30s or 5m", api.ErrBadConnector)}
	h := newConnectorServer(t, sup)

	w := post(t, h, http.MethodPost, "/api/v1/admin/connectors/handbook/start", "", operator())
	if !strings.Contains(w.Body.String(), "try 30s or 5m") {
		t.Fatalf("the answer does not say what to do: %s", w.Body)
	}
}

// A deployment that wires its own crawlers says so rather than accepting a
// connector and dropping it.
func TestAServerWithNoSupervisorSaysConnectorsAreConfiguredElsewhere(t *testing.T) {
	h := newConnectorServer(t, nil)

	w := post(t, h, http.MethodPost, "/api/v1/admin/connectors", `{"source":"x","kind":"corpus"}`, operator())
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", w.Code, w.Body)
	}
	// And the screen is told the same thing, so it does not draw a form.
	w = request(t, h, http.MethodGet, "/api/v1/admin/operations", operator())
	var body struct {
		Manageable bool `json:"manageable"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Manageable {
		t.Error("the screen was told it can add a connector to a deployment that has nothing to add it to")
	}
}

func TestAConnectorNeedsASourceAndAKind(t *testing.T) {
	sup := &fakeSupervisor{}
	h := newConnectorServer(t, sup)

	for _, body := range []string{
		`{"kind":"corpus"}`,
		`{"source":"x"}`,
		`{"source":"   ","kind":"corpus"}`,
		`not json at all`,
	} {
		w := post(t, h, http.MethodPost, "/api/v1/admin/connectors", body, operator())
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, w.Code)
		}
	}
	if calls := sup.seen(); len(calls) != 0 {
		t.Fatalf("a request that could not be read still reached the supervisor: %v", calls)
	}
}

// A configuration larger than any connector has is refused before it is read,
// rather than decoded into memory first.
func TestAnEnormousConfigurationIsRefused(t *testing.T) {
	sup := &fakeSupervisor{}
	h := newConnectorServer(t, sup)

	huge := `{"source":"x","kind":"corpus","config":{"dir":"` + strings.Repeat("a", 64<<10) + `"}}`
	w := post(t, h, http.MethodPost, "/api/v1/admin/connectors", huge, operator())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if calls := sup.seen(); len(calls) != 0 {
		t.Fatalf("an oversized body still reached the supervisor: %v", calls)
	}
}

// Who changed what is in the log, because the wrapper's line only records that
// an administrator changed something and those are two different things to have
// to explain afterwards.
func TestChangingAConnectorIsAudited(t *testing.T) {
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	var logged bytes.Buffer
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"},
		api.WithLogger(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))),
		api.WithSupervisor(&fakeSupervisor{}),
	)
	h := s.Handler()

	post(t, h, http.MethodPost, "/api/v1/admin/connectors",
		`{"source":"archive","kind":"bucket","enabled":true}`, operator())

	out := logged.String()
	for _, want := range []string{"connector saved", "u_ops", "archive", "bucket"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not say %q:\n%s", want, out)
		}
	}
}

// errString is an error that is nothing in particular, for the case where the
// deployment's own problem must not be shown to the caller.
type errString string

func (e errString) Error() string { return string(e) }
