package api_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// What one person can see, as an operator asks it.
//
// The endpoint answers a question people otherwise answer by borrowing
// somebody's account, so the two things it has to get right are the answer and
// the trail it leaves. Both are here, and so is the line it does not cross: it
// counts, it never lists, and it explains a yes and never a no.

type accessBody struct {
	Subject    string   `json:"subject"`
	Tenant     string   `json:"tenant"`
	Groups     []string `json:"groups"`
	Identities []string `json:"identities"`
	Documents  int      `json:"documents"`
	Countable  bool     `json:"countable"`
	Counted    bool     `json:"counted"`
	Sources    []struct {
		Source    string `json:"source"`
		Documents int    `json:"documents"`
	} `json:"sources"`
	Document *struct {
		ID      string `json:"id"`
		Visible bool   `json:"visible"`
		Rule    string `json:"rule"`
		Matched string `json:"matched"`
	} `json:"document"`
}

// newAccessServer holds a small corpus with three kinds of document in it: one
// everybody in the tenant may read, two an engineering group may read, one only
// sales may read, and one nobody may read because its rules never resolved.
func newAccessServer(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	page := func(id, source string, perm acl.Permissions) doc.Document {
		return doc.Document{
			ID: id, Tenant: "acme", Source: source, Kind: doc.KindPage,
			Title: "runbook " + id, Body: "how to fail over the payments queue",
			Permissions: perm,
		}
	}
	group := func(name string) acl.Permissions {
		return acl.Permissions{
			Mode:        acl.ModeACL,
			Source:      "gdrive",
			AllowGroups: []acl.Ref{{Source: "gdrive", Value: name}},
			Version:     1,
		}
	}
	docs := []doc.Document{
		page("d1", "gdrive", acl.Permissions{Mode: acl.ModePublicToTenant, Source: "gdrive", Version: 1}),
		page("d2", "gdrive", group("eng@acme.com")),
		page("d3", "slack", group("eng@acme.com")),
		page("d4", "gdrive", group("sales@acme.com")),
		page("d5", "gdrive", acl.Permissions{
			Mode: acl.ModeUnknown, Source: "gdrive",
			Reason: "foreign domain: a grant to @contractor.example",
		}),
	}
	if err := st.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, api.WithLogger(log))
	return s.Handler(), &logged
}

// ask puts one access question, as an operator.
func ask(t *testing.T, h http.Handler, params url.Values) accessBody {
	t.Helper()
	w := request(t, h, http.MethodGet, "/api/v1/admin/access?"+params.Encode(), operator())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	return decode[accessBody](t, w)
}

func TestAccessNeedsTheAdministratorRole(t *testing.T) {
	h, logged := newAccessServer(t)

	w := request(t, h, http.MethodGet, "/api/v1/admin/access?subject=u_mei", engineer())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	// The refusal matters more here than on the screen next door. This is the
	// endpoint that answers questions about other people, so somebody reaching
	// it without the role is the thing a deployment most wants recorded.
	if got := logged.String(); !strings.Contains(got, "administration refused") || !strings.Contains(got, "u_mei") {
		t.Errorf("the refusal was not audited: %s", got)
	}
}

func TestAccessAuditsWhoWasAskedAbout(t *testing.T) {
	h, logged := newAccessServer(t)

	ask(t, h, url.Values{"subject": {"u_kenji"}, "groups": {"gdrive:sales@acme.com"}})

	got := logged.String()
	if !strings.Contains(got, "administration access check") {
		t.Fatalf("the check was not audited: %s", got)
	}
	// Both names. That an administrator read something is one fact and which
	// person they asked about is another, and afterwards it is the second one
	// somebody has to explain.
	if !strings.Contains(got, "u_ops") || !strings.Contains(got, "u_kenji") {
		t.Errorf("the audit line names %q, want both the operator and the subject", got)
	}
}

func TestAccessNeedsASubjectToAskAbout(t *testing.T) {
	h, _ := newAccessServer(t)

	w := request(t, h, http.MethodGet, "/api/v1/admin/access", operator())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestAccessCountsWhatThePersonCanReach(t *testing.T) {
	h, _ := newAccessServer(t)

	body := ask(t, h, url.Values{
		"subject": {"u_mei"},
		"groups":  {"gdrive:eng@acme.com"},
		"counts":  {"1"},
	})
	if !body.Countable || !body.Counted {
		t.Fatal("the counts were asked for and the driver can produce them, so the answer should carry them")
	}
	// The public one and the two engineering ones. Not the sales one and not
	// the held one.
	if body.Documents != 3 {
		t.Errorf("documents = %d, want 3", body.Documents)
	}
	if len(body.Sources) != 2 {
		t.Fatalf("sources = %+v, want two", body.Sources)
	}
	// Largest first, so the connector somebody has most of is read first.
	if body.Sources[0].Source != "gdrive" || body.Sources[0].Documents != 2 {
		t.Errorf("the first source is %+v, want two documents in gdrive", body.Sources[0])
	}
	if body.Sources[1].Source != "slack" || body.Sources[1].Documents != 1 {
		t.Errorf("the second source is %+v, want one document in slack", body.Sources[1])
	}
}

// TestAccessLeavesTheCountsAloneUnlessAsked is the shape of the screen in one
// assertion. The check is two indexed reads and the counts are an aggregate
// over the whole tenant, so the answer an operator waits for on every keystroke
// does not pay for the one they ask for occasionally.
func TestAccessLeavesTheCountsAloneUnlessAsked(t *testing.T) {
	h, _ := newAccessServer(t)

	body := ask(t, h, url.Values{"subject": {"u_mei"}, "groups": {"gdrive:eng@acme.com"}})
	if body.Counted {
		t.Error("the counts were not asked for and the answer says it carries them")
	}
	if body.Documents != 0 || len(body.Sources) != 0 {
		t.Errorf("the answer carries %d documents in %v without being asked", body.Documents, body.Sources)
	}
	// The driver can still count, and the screen needs to know that to decide
	// whether to offer the button at all.
	if !body.Countable {
		t.Error("the driver can count and the answer does not say so")
	}
}

// TestAccessCountsNothingForSomebodyInNoGroups is the answer an operator gets
// when they have typed a group name wrongly, which is why the question is
// echoed back with the answer.
func TestAccessCountsNothingForSomebodyInNoGroups(t *testing.T) {
	h, _ := newAccessServer(t)

	body := ask(t, h, url.Values{"subject": {"u_new"}, "groups": {"gdrive:eng@acme.example"}, "counts": {"1"}})
	if body.Documents != 1 {
		t.Errorf("documents = %d, want 1: only what the whole tenant may read", body.Documents)
	}
	if len(body.Groups) != 1 || body.Groups[0] != "gdrive:eng@acme.example" {
		t.Errorf("the question was echoed back as %v, want the group that was asked about", body.Groups)
	}
	if body.Subject != "u_new" || body.Tenant != "acme" {
		t.Errorf("the answer is about %s in %s, want u_new in acme", body.Subject, body.Tenant)
	}
}

// TestAccessNeverCountsAHeldDocument is the property the whole quarantine rests
// on, asserted from the one screen that could plausibly be given an exception.
// An operator holding the role is not a reader, and the count they see of
// somebody else's access is the count that person would see.
func TestAccessNeverCountsAHeldDocument(t *testing.T) {
	h, _ := newAccessServer(t)

	// Somebody in every group in the corpus, which is the most access anybody
	// here has. The held document is still not theirs.
	body := ask(t, h, url.Values{
		"subject": {"u_mei"},
		"groups":  {"gdrive:eng@acme.com,gdrive:sales@acme.com"},
		"counts":  {"1"},
	})
	if body.Documents != 4 {
		t.Errorf("documents = %d, want 4: the held one belongs to nobody", body.Documents)
	}
}

func TestAccessExplainsWhySomebodyCanReadADocument(t *testing.T) {
	h, _ := newAccessServer(t)

	body := ask(t, h, url.Values{
		"subject": {"u_mei"},
		"groups":  {"gdrive:eng@acme.com"},
		"id":      {"d2"},
	})
	if body.Document == nil {
		t.Fatal("the question named a document and the answer did not")
	}
	if !body.Document.Visible {
		t.Fatalf("d2 is %+v, want visible", body.Document)
	}
	// Which group admitted them, which is the answer to why the contractor can
	// find that document, and is the only useful form of the answer.
	if body.Document.Rule != string(acl.RuleListed) {
		t.Errorf("rule = %q, want %q", body.Document.Rule, acl.RuleListed)
	}
	if body.Document.Matched != "gdrive:eng@acme.com" {
		t.Errorf("matched = %q, want the group on the access control list", body.Document.Matched)
	}
}

func TestAccessSaysTheTenantWideRuleByName(t *testing.T) {
	h, _ := newAccessServer(t)

	body := ask(t, h, url.Values{"subject": {"u_new"}, "id": {"d1"}})
	if body.Document == nil || !body.Document.Visible {
		t.Fatalf("d1 is %+v, want visible to anybody in the tenant", body.Document)
	}
	if body.Document.Rule != string(acl.RuleTenant) {
		t.Errorf("rule = %q, want %q", body.Document.Rule, acl.RuleTenant)
	}
	// Nothing matched, because nothing had to. A reference here would be an
	// invented one.
	if body.Document.Matched != "" {
		t.Errorf("matched = %q, want nothing: no reference decided", body.Document.Matched)
	}
}

// TestAccessDoesNotExplainARefusal is the line this endpoint does not cross.
// A document held back, a document in another tenant, a document belonging to a
// group this person is not in and a document that was never indexed are one
// answer, because an operator who can tell them apart can use the difference to
// prove that a document exists.
func TestAccessDoesNotExplainARefusal(t *testing.T) {
	h, _ := newAccessServer(t)

	for _, id := range []string{"d4", "d5", "d-never-existed"} {
		body := ask(t, h, url.Values{
			"subject": {"u_mei"},
			"groups":  {"gdrive:eng@acme.com"},
			"id":      {id},
		})
		if body.Document == nil {
			t.Fatalf("%s: the question named a document and the answer did not", id)
		}
		if body.Document.Visible {
			t.Errorf("%s is reported visible", id)
		}
		if body.Document.Rule != "" || body.Document.Matched != "" {
			t.Errorf("%s says %+v, want a refusal that explains nothing", id, body.Document)
		}
	}
}

// TestAccessCarriesNoTitles is the other line. The role grants nothing over
// documents, and a count that came back with a list of what was counted would
// be a way of reading any corpus through somebody else's eyes.
func TestAccessCarriesNoTitles(t *testing.T) {
	h, _ := newAccessServer(t)

	w := request(t, h, http.MethodGet,
		"/api/v1/admin/access?subject=u_mei&groups=gdrive%3Aeng%40acme.com&id=d2", operator())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	for _, word := range []string{"runbook", "payments queue"} {
		if strings.Contains(w.Body.String(), word) {
			t.Errorf("the answer carries %q, which is the document rather than the count: %s", word, w.Body)
		}
	}
}

func TestAccessRefusesAnAbsurdNumberOfGroups(t *testing.T) {
	h, _ := newAccessServer(t)

	groups := make([]string, api.GroupLimit+1)
	for i := range groups {
		groups[i] = "gdrive:g" + string(rune('a'+i%26))
	}
	w := request(t, h, http.MethodGet,
		"/api/v1/admin/access?subject=u_mei&groups="+url.QueryEscape(strings.Join(groups, ",")), operator())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestAccessAsksAboutTheOperatorsOwnTenant keeps one administrator out of
// another tenant's corpus. The tenant is not a parameter, so a question that
// names one gets an answer about the tenant the operator is on.
func TestAccessAsksAboutTheOperatorsOwnTenant(t *testing.T) {
	h, _ := newAccessServer(t)

	body := ask(t, h, url.Values{"subject": {"u_mei"}, "tenant": {"globex"}})
	if body.Tenant != "acme" {
		t.Errorf("tenant = %q, want acme", body.Tenant)
	}
}
