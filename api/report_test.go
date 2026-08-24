package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// Saying that a document is out of date.
//
// This is the one curation endpoint with no permission of its own. Anybody who
// can read a document can say it is wrong, because the person who just lost an
// hour to a stale runbook is the person who knows, and a corpus that only takes
// corrections from the people who own it takes very few. Clearing what was said
// is the other way round and is held to the same rule as verifying: if any
// reader could clear a report, the first thing that would happen to an
// inconvenient one is that somebody would clear it.

// staleBody is the block the interface reads to draw the mark.
type staleBody struct {
	Count int       `json:"count"`
	By    string    `json:"by"`
	Email string    `json:"email"`
	At    time.Time `json:"at"`
	Note  string    `json:"note"`
	Mine  bool      `json:"mine"`
}

// documentStale is the preview as far as this file is concerned.
type documentStale struct {
	Stale      *staleBody `json:"stale"`
	CanReport  bool       `json:"can_report"`
	CanResolve bool       `json:"can_resolve"`
	Verified   *struct {
		State string `json:"state"`
	} `json:"verified"`
}

// newReportServer holds the same three documents the ownership tests use, which
// is the shape that matters here as well: one the reader wrote, one they own
// without having written it, and one that is somebody else's from end to end.
func newReportServer(t *testing.T) http.Handler {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Put(t.Context(), ownedDocs()...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, api.WithClock(clock))
	return s.Handler()
}

// colleague can read everything here and owns none of it, which is the person
// this feature exists for.
func colleague() map[string]string {
	return map[string]string{
		api.HeaderSubject:    "u_ren",
		api.HeaderGroups:     "gdrive:eng@acme.com",
		api.HeaderIdentities: "gdrive:ren@acme.com",
	}
}

// teammate is a second reader in the same group, for the cases that need two
// people to have complained about the same document.
func teammate() map[string]string {
	return map[string]string{
		api.HeaderSubject:    "u_ade",
		api.HeaderGroups:     "gdrive:eng@acme.com",
		api.HeaderIdentities: "gdrive:ade@acme.com",
	}
}

func panelOf(t *testing.T, h http.Handler, id string, headers map[string]string) documentStale {
	t.Helper()
	w := request(t, h, http.MethodGet, "/api/v1/documents/"+id, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("reading %s: %d %s", id, w.Code, w.Body.String())
	}
	return decode[documentStale](t, w)
}

func TestAnyReaderCanSayADocumentIsOutOfDate(t *testing.T) {
	h := newReportServer(t)

	w := post(t, h, http.MethodPost, "/api/v1/documents/hers/stale",
		`{"note":"the failover step names a cluster we turned off in March"}`, colleague())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	said := decode[documentStale](t, w)
	if said.Stale == nil || said.Stale.Count != 1 {
		t.Fatalf("one person reported it and the answer says %+v", said.Stale)
	}
	// The name and the sentence, because a number on its own leaves the owner
	// with nothing to do and nobody to ask.
	if said.Stale.By != "ren@acme.com" || said.Stale.Note == "" {
		t.Errorf("the report came back as %+v", said.Stale)
	}
	if !said.Stale.At.Equal(clock()) {
		t.Errorf("reported at %v, and the request was made at %v", said.Stale.At, clock())
	}
	// The person who reported it cannot clear it, which is the whole difference
	// between the two halves of this endpoint.
	if said.CanResolve {
		t.Errorf("a reader who does not own the document is offered the control that clears it")
	}

	// And the owner sees it on the panel, with the control this time.
	got := panelOf(t, h, "hers", owner())
	if got.Stale == nil || got.Stale.Count != 1 {
		t.Fatalf("the owner's panel says %+v", got.Stale)
	}
	if !got.CanResolve {
		t.Errorf("the owner is not offered the control that clears the report")
	}
}

// The count is people rather than clicks, which is what makes it worth printing.
func TestReportingTwiceIsStillOnePerson(t *testing.T) {
	h := newReportServer(t)

	first := post(t, h, http.MethodPost, "/api/v1/documents/hers/stale", `{"note":"the diagram is wrong"}`, colleague())
	second := post(t, h, http.MethodPost, "/api/v1/documents/hers/stale", `{"note":"and so is the appendix"}`, colleague())
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("the two reports answered %d and %d", first.Code, second.Code)
	}
	said := decode[documentStale](t, second).Stale
	if said == nil || said.Count != 1 {
		t.Fatalf("the same person reported it twice and the count says %+v", said)
	}
	if said.Note != "and so is the appendix" {
		t.Errorf("the standing report says %q rather than what they said last", said.Note)
	}
}

// Reading a document is enough to report it and is not enough to decide the
// report has been dealt with.
func TestOnlyTheOwnerOrTheAuthorCanResolve(t *testing.T) {
	h := newReportServer(t)
	if w := post(t, h, http.MethodPost, "/api/v1/documents/hers/stale", `{"note":"out of date"}`, colleague()); w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body.String())
	}

	if w := post(t, h, http.MethodDelete, "/api/v1/documents/hers/stale", "", colleague()); w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if w := post(t, h, http.MethodDelete, "/api/v1/documents/hers/stale", "", owner()); w.Code != http.StatusNoContent {
		t.Fatalf("the owner could not clear the report: %d %s", w.Code, w.Body.String())
	}
	if got := panelOf(t, h, "hers", owner()); got.Stale != nil {
		t.Errorf("the report was cleared and the panel still carries %+v", got.Stale)
	}
}

// A person who cannot see the document is told the same thing they would be told
// if it were not there. Anything else is a way to ask whether an id is real.
func TestReportingSomethingYouCannotSeeIsANotFound(t *testing.T) {
	h := newReportServer(t)

	seen := post(t, h, http.MethodPost, "/api/v1/documents/hers/stale", `{"note":"wrong"}`, salesperson())
	missing := post(t, h, http.MethodPost, "/api/v1/documents/nothing/stale", `{"note":"wrong"}`, salesperson())
	if seen.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("a hidden document answered %d and a missing one answered %d, and both should be 404",
			seen.Code, missing.Code)
	}
	if seen.Body.String() != missing.Body.String() {
		t.Errorf("the two answers differ, which is enough to tell that hers exists:\n%s\n%s",
			seen.Body.String(), missing.Body.String())
	}
}

// Putting your name to a document answers what anybody had said was wrong with
// it. A document that is verified and marked out of date at the same time says
// two things at once, and the reader believes neither.
func TestVerifyingClearsTheReports(t *testing.T) {
	h := newReportServer(t)
	if w := post(t, h, http.MethodPost, "/api/v1/documents/hers/stale", `{"note":"out of date"}`, colleague()); w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body.String())
	}

	if w := post(t, h, http.MethodPost, "/api/v1/documents/hers/verify", "", owner()); w.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}
	got := panelOf(t, h, "hers", owner())
	if got.Stale != nil {
		t.Errorf("the document was verified and is still marked out of date: %+v", got.Stale)
	}
	if got.Verified == nil || got.Verified.State != "fresh" {
		t.Errorf("the verification did not stand: %+v", got.Verified)
	}
}

// Reporting the wrong document is a mistake somebody makes in the ten seconds
// this feature is built to cost, and the way out of it is not asking the owner
// to clear a report that never meant anything.
func TestAReaderCanTakeBackTheirOwnReport(t *testing.T) {
	h := newReportServer(t)
	if w := post(t, h, http.MethodPost, "/api/v1/documents/hers/stale", `{"note":"wrong document"}`, colleague()); w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body.String())
	}

	w := post(t, h, http.MethodDelete, "/api/v1/documents/hers/stale/mine", "", colleague())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	// What stands afterwards, because the count has changed and so has what the
	// button should say.
	if said := decode[documentStale](t, w).Stale; said != nil {
		t.Errorf("the only report was withdrawn and the answer still carries %+v", said)
	}
	if got := panelOf(t, h, "hers", owner()); got.Stale != nil {
		t.Errorf("the owner's panel still carries a report that was taken back: %+v", got.Stale)
	}

	// Again, because a second click on a button that has already done its work is
	// something that happens on a slow connection.
	if again := post(t, h, http.MethodDelete, "/api/v1/documents/hers/stale/mine", "", colleague()); again.Code != http.StatusOK {
		t.Errorf("withdrawing a report that is no longer there: %d %s", again.Code, again.Body.String())
	}
}

// Taking back your own sentence is not clearing everybody's, which is the whole
// reason this is a second endpoint rather than a permission somebody relaxed.
func TestWithdrawingLeavesEverybodyElsesReportStanding(t *testing.T) {
	h := newReportServer(t)
	for _, who := range []map[string]string{colleague(), teammate()} {
		if w := post(t, h, http.MethodPost, "/api/v1/documents/hers/stale", `{"note":"out of date"}`, who); w.Code != http.StatusOK {
			t.Fatalf("report: %d %s", w.Code, w.Body.String())
		}
	}

	w := post(t, h, http.MethodDelete, "/api/v1/documents/hers/stale/mine", "", colleague())
	said := decode[documentStale](t, w).Stale
	if said == nil || said.Count != 1 {
		t.Fatalf("one of two reports was withdrawn and what is left says %+v", said)
	}
	if said.Mine {
		t.Errorf("the reader took their report back and the answer still says it is theirs")
	}
	if got := panelOf(t, h, "hers", teammate()); got.Stale == nil || !got.Stale.Mine {
		t.Errorf("one person withdrawing took somebody else's report with it: %+v", got.Stale)
	}
}

// The button cannot read as a way out until the server says whether there is
// anything to get out of, and it cannot be worked out from the name on the mark:
// that is the most recent person to have complained, who is usually not the
// person reading the page.
func TestThePanelSaysWhetherTheReaderIsOneOfThePeopleWhoReportedIt(t *testing.T) {
	h := newReportServer(t)
	if w := post(t, h, http.MethodPost, "/api/v1/documents/hers/stale", `{"note":"out of date"}`, colleague()); w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body.String())
	}
	if w := post(t, h, http.MethodPost, "/api/v1/documents/hers/stale", `{"note":"and the diagram"}`, teammate()); w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body.String())
	}

	if got := panelOf(t, h, "hers", colleague()); got.Stale == nil || !got.Stale.Mine {
		t.Errorf("the reader reported it and their panel says somebody else did: %+v", got.Stale)
	}
	if got := panelOf(t, h, "hers", owner()); got.Stale == nil || got.Stale.Mine {
		t.Errorf("the owner never reported it and their panel says they did: %+v", got.Stale)
	}
}

// Same silence as reporting. Anything else is a way to ask whether an id is
// real by trying to take back a report nobody made.
func TestWithdrawingSomethingYouCannotSeeIsANotFound(t *testing.T) {
	h := newReportServer(t)

	seen := post(t, h, http.MethodDelete, "/api/v1/documents/hers/stale/mine", "", salesperson())
	missing := post(t, h, http.MethodDelete, "/api/v1/documents/nothing/stale/mine", "", salesperson())
	if seen.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("a hidden document answered %d and a missing one answered %d, and both should be 404",
			seen.Code, missing.Code)
	}
	if seen.Body.String() != missing.Body.String() {
		t.Errorf("the two answers differ, which is enough to tell that hers exists:\n%s\n%s",
			seen.Body.String(), missing.Body.String())
	}
}

// The button is offered on every document the driver can remember a report
// about, because reporting needs nothing but being able to read.
func TestTheReportControlIsOfferedToEverybodyWhoCanRead(t *testing.T) {
	h := newReportServer(t)

	for _, id := range []string{"mine", "hers", "theirs"} {
		if got := panelOf(t, h, id, colleague()); !got.CanReport {
			t.Errorf("a reader is not offered the control that reports %s", id)
		}
	}
}
