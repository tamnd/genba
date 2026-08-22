package api_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// What an operator can see about the deployment, and what everybody else
// cannot.
//
// The two halves of this file are the two halves of the endpoint. The first is
// the gate, which is a role and not a tenant, and which has to refuse in a way
// that says why. The second is the answer, which is the connectors, the counts
// and the list of what is being held back, all read at the same moment so that
// the number above the list and the list agree.

type adminBody struct {
	Connectors []struct {
		Source  string `json:"source"`
		Kind    string `json:"kind"`
		Target  string `json:"target"`
		Syncing bool   `json:"syncing"`
		Refresh string `json:"refresh"`
		Runs    []struct {
			Started     string `json:"started"`
			Duration    int64  `json:"duration_ms"`
			Error       string `json:"error"`
			Indexed     int    `json:"indexed"`
			Quarantined int    `json:"quarantined"`
		} `json:"runs"`
		Permissions *struct {
			Mapped        int64 `json:"mapped"`
			ForeignDomain int64 `json:"foreign_domain"`
		} `json:"permissions"`
	} `json:"connectors"`
	Documents   int  `json:"documents"`
	Quarantined int  `json:"quarantined"`
	Listable    bool `json:"listable"`
	Held        []struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		Source     string `json:"source"`
		Reason     string `json:"reason"`
		ModifiedAt string `json:"modified_at"`
	} `json:"held"`
}

// operator is a subject the deployment has named as an administrator.
func operator() map[string]string {
	return map[string]string{
		api.HeaderSubject: "u_ops",
		api.HeaderRoles:   acl.RoleAdmin,
	}
}

// newAdminServer is a server holding one readable document and two that were
// held back, with a log to read the audit trail out of.
func newAdminServer(t *testing.T, ops func() api.Operations) (http.Handler, *bytes.Buffer) {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	held := func(id, reason string) doc.Document {
		return doc.Document{
			ID: id, Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title:      "Payroll " + id,
			ModifiedAt: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
			Permissions: acl.Permissions{
				Mode: acl.ModeUnknown, Source: "gdrive", Reason: reason,
			},
		}
	}
	docs := []doc.Document{
		{
			ID: "d1", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments failover runbook",
			Permissions: acl.Permissions{
				Mode: acl.ModePublicToTenant, Source: "gdrive", Version: 1,
			},
		},
		held("d2", "foreign domain: a grant to @contractor.example"),
		held("d3", "malformed grant: the entry named no principal"),
	}
	if err := st.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	opts := []api.Option{api.WithLogger(log)}
	if ops != nil {
		opts = append(opts, api.WithOperations(ops))
	}
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, opts...)
	return s.Handler(), &logged
}

// The gate is the role and nothing else. A person with a perfectly good account
// on the right tenant is still not an operator, and the deployment has to be
// able to show afterwards that they were turned away.
func TestAdministrationRefusesSomebodyWithoutTheRole(t *testing.T) {
	h, logged := newAdminServer(t, nil)

	w := request(t, h, http.MethodGet, "/api/v1/admin/operations", engineer())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	// Forbidden rather than not found, so that somebody handed the wrong account
	// is told which of the two things is wrong.
	if body := w.Body.String(); !strings.Contains(body, "administrator") {
		t.Errorf("the refusal did not say what was missing: %s", body)
	}
	if got := logged.String(); !strings.Contains(got, "administration refused") || !strings.Contains(got, "u_mei") {
		t.Errorf("the refusal was not audited: %s", got)
	}
}

func TestAdministrationRefusesAnAnonymousCaller(t *testing.T) {
	h, _ := newAdminServer(t, nil)

	w := request(t, h, http.MethodGet, "/api/v1/admin/operations", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// A read of how the deployment is running is not a read of anybody's documents,
// and it is still something the deployment should be able to account for.
func TestAdministrationAuditsTheRead(t *testing.T) {
	h, logged := newAdminServer(t, nil)

	w := request(t, h, http.MethodGet, "/api/v1/admin/operations", operator())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := logged.String()
	if !strings.Contains(got, "administration read") || !strings.Contains(got, "u_ops") {
		t.Errorf("the read was not audited: %s", got)
	}
}

// The counts and the list are read together, so the total above the list and
// the list itself cannot disagree.
func TestAdministrationListsTheQuarantineWithReasons(t *testing.T) {
	h, _ := newAdminServer(t, nil)

	w := request(t, h, http.MethodGet, "/api/v1/admin/operations", operator())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := decode[adminBody](t, w)
	// One servable and two held, which is the split the screen draws. Documents
	// is what a query can reach rather than what the store holds, so the two
	// numbers add up to the corpus rather than one containing the other.
	if body.Documents != 1 {
		t.Errorf("documents = %d, want 1", body.Documents)
	}
	if body.Quarantined != 2 {
		t.Errorf("quarantined = %d, want 2", body.Quarantined)
	}
	if !body.Listable {
		t.Fatal("the driver lists its quarantine and the response said it does not")
	}
	if len(body.Held) != 2 {
		t.Fatalf("held = %d entries, want 2", len(body.Held))
	}
	// The reason is the only thing on this screen anybody acts on. A list of two
	// held documents with no reasons is the count again, drawn as a table.
	reasons := map[string]string{}
	for _, h := range body.Held {
		reasons[h.ID] = h.Reason
		if h.Title == "" {
			t.Errorf("%s was listed with no title, which is an id nobody can recognise", h.ID)
		}
		if h.Source != "gdrive" {
			t.Errorf("%s source = %q, want gdrive", h.ID, h.Source)
		}
		if h.ModifiedAt != "2026-03-01T09:00:00Z" {
			t.Errorf("%s modified_at = %q", h.ID, h.ModifiedAt)
		}
	}
	if want := "foreign domain: a grant to @contractor.example"; reasons["d2"] != want {
		t.Errorf("d2 reason = %q, want %q", reasons["d2"], want)
	}
	if want := "malformed grant: the entry named no principal"; reasons["d3"] != want {
		t.Errorf("d3 reason = %q, want %q", reasons["d3"], want)
	}
	// The readable document is not in the quarantine, which is the way this
	// endpoint would leak: it is the one place that reads documents without
	// asking whether the caller may see them, because what failed is exactly
	// that question.
	if strings.Contains(w.Body.String(), "Payments failover runbook") {
		t.Error("a document that resolved was listed as held back")
	}
}

// A process that runs no connectors says so, rather than drawing an empty list
// that looks like every sync has stopped.
func TestAdministrationReportsNoConnectorsWithoutOperations(t *testing.T) {
	h, _ := newAdminServer(t, nil)

	w := request(t, h, http.MethodGet, "/api/v1/admin/operations", operator())
	if body := decode[adminBody](t, w); len(body.Connectors) != 0 {
		t.Fatalf("connectors = %d, want none", len(body.Connectors))
	}
	if strings.Contains(w.Body.String(), `"connectors":null`) {
		t.Error("connectors was null, which a client has to special case")
	}
}

// The failure of a sync is the whole of what box two of the issue asks for, so
// the message travels whole rather than as a code nobody can switch on.
func TestAdministrationReportsSyncFailures(t *testing.T) {
	h, _ := newAdminServer(t, func() api.Operations {
		return api.Operations{Connectors: []api.Connector{{
			Source:  "files",
			Kind:    "corpus",
			Target:  "/srv/notes",
			Tenant:  "acme",
			Refresh: "30s",
			Syncing: true,
			Runs: []api.Run{
				{Started: "2026-03-01T09:00:30Z", Duration: 12, Error: "open /srv/notes: no such file or directory"},
				{Started: "2026-03-01T09:00:00Z", Duration: 940, Indexed: 1204, Quarantined: 2},
			},
			Permissions: &api.Mapping{Mapped: 1204, ForeignDomain: 2},
		}}}
	})

	w := request(t, h, http.MethodGet, "/api/v1/admin/operations", operator())
	body := decode[adminBody](t, w)
	if len(body.Connectors) != 1 {
		t.Fatalf("connectors = %d, want 1", len(body.Connectors))
	}
	c := body.Connectors[0]
	if c.Source != "files" || c.Kind != "corpus" || c.Target != "/srv/notes" || c.Refresh != "30s" {
		t.Errorf("connector = %+v, want the source, kind, target and refresh", c)
	}
	if !c.Syncing {
		t.Error("the connector is syncing and the response said it is not")
	}
	if len(c.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(c.Runs))
	}
	// Newest first, because the question is what is happening now and a screen
	// should not have to sort to answer it.
	if c.Runs[0].Error == "" {
		t.Error("the failed run carried no message, which leaves nothing to act on")
	}
	if !strings.Contains(c.Runs[0].Error, "no such file or directory") {
		t.Errorf("run error = %q, want the whole message", c.Runs[0].Error)
	}
	if c.Runs[1].Indexed != 1204 || c.Runs[1].Quarantined != 2 {
		t.Errorf("run counts = %+v, want 1204 indexed and 2 held", c.Runs[1])
	}
	if c.Permissions == nil || c.Permissions.ForeignDomain != 2 {
		t.Errorf("permissions = %+v, want the mapping counts by reason", c.Permissions)
	}
}

// The role can come from the proxy or from the deployment's own list, and the
// two do not have to agree. The list adds and never takes away, so an operator
// who named themselves at startup is one whatever the proxy sends.
func TestAdministrationTakesTheRoleFromTheConfiguredList(t *testing.T) {
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme", Admins: []string{"u_ops"}})
	h := s.Handler()

	w := request(t, h, http.MethodGet, "/api/v1/admin/operations",
		map[string]string{api.HeaderSubject: "u_ops"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, the deployment named this subject", w.Code)
	}

	// And nobody else, which is the half that matters. A list that let one name
	// in and everybody else as well would be worse than no list.
	w = request(t, h, http.MethodGet, "/api/v1/admin/operations",
		map[string]string{api.HeaderSubject: "u_mei"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// Empty means nobody. A deployment that has not said who operates it has not
// decided, and the answer that cannot be taken back is the wrong guess.
func TestAdministrationAdmitsNobodyByDefault(t *testing.T) {
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })
	h := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}).Handler()

	w := request(t, h, http.MethodGet, "/api/v1/admin/operations",
		map[string]string{api.HeaderSubject: "u_ops"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}
