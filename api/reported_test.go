package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// The owner's side of a report.
//
// A report that lands in a table nobody reads is a report that was never made.
// What is being checked here is that it reaches the person who can act on it and
// reaches nobody else: this endpoint is the one place in the product that hands
// somebody a list of documents on the grounds that they are accountable for
// them, and the rule for that is the same owns or wrote it rule the rest of
// curation uses.

// inbox is the reported list as this file reads it.
type inbox struct {
	Documents []struct {
		ID    string     `json:"id"`
		Title string     `json:"title"`
		Stale *staleBody `json:"stale"`
	} `json:"documents"`
	At time.Time `json:"at"`
}

// stepping is a clock that moves a minute every time it is read, so that three
// reports made in one test are three different instants and the order the
// endpoint promises is an order this test can see.
func stepping(from time.Time) func() time.Time {
	at := from
	return func() time.Time {
		at = at.Add(time.Minute)
		return at
	}
}

func newInboxServer(t *testing.T, now func() time.Time) http.Handler {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Put(t.Context(), ownedDocs()...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, api.WithClock(now))
	return s.Handler()
}

// reportOne says a document is out of date and fails the test if it could not.
func reportOne(t *testing.T, h http.Handler, id, note string, headers map[string]string) {
	t.Helper()
	w := post(t, h, http.MethodPost, "/api/v1/documents/"+id+"/stale", `{"note":"`+note+`"}`, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("reporting %s: %d %s", id, w.Code, w.Body.String())
	}
}

func inboxOf(t *testing.T, h http.Handler, query string, headers map[string]string) inbox {
	t.Helper()
	w := request(t, h, http.MethodGet, "/api/v1/reported"+query, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("reading the inbox: %d %s", w.Code, w.Body.String())
	}
	return decode[inbox](t, w)
}

func ids(got inbox) []string {
	out := make([]string, 0, len(got.Documents))
	for _, d := range got.Documents {
		out = append(out, d.ID)
	}
	return out
}

// Owning it or having written it is what puts a document in this list, and
// neither of those is being able to read it.
func TestTheInboxHoldsOnlyYourOwnDocuments(t *testing.T) {
	h := newInboxServer(t, clock)

	for _, id := range []string{"mine", "hers", "theirs"} {
		reportOne(t, h, id, "out of date", colleague())
	}

	// Mei wrote mine and owns hers. Kenji's document is reported and is not her
	// problem.
	got := ids(inboxOf(t, h, "", owner()))
	if len(got) != 2 || got[0] != "hers" || got[1] != "mine" {
		t.Fatalf("the owner's inbox holds %v, and should hold hers and mine", got)
	}

	// The person who reported all three owns none of them, so they are told
	// about none of them. A report is not a subscription.
	if said := ids(inboxOf(t, h, "", colleague())); len(said) != 0 {
		t.Errorf("the reader who made the reports was handed %v", said)
	}
}

// The list is worth reading because of what is on each row, not because of how
// long it is.
func TestTheInboxCarriesTheCountAndTheSentence(t *testing.T) {
	// A moving clock, because the row carries the most recent of the two reports
	// and two reports made in the same instant leave which one that is up to the
	// driver.
	h := newInboxServer(t, stepping(clock()))
	reportOne(t, h, "hers", "the pager numbers are last year's", curator())
	reportOne(t, h, "hers", "and it still names a rota we stopped running", colleague())

	got := inboxOf(t, h, "", owner())
	if len(got.Documents) != 1 {
		t.Fatalf("the inbox holds %d rows, and two people reported one document", len(got.Documents))
	}
	row := got.Documents[0]
	if row.Title != "Payments oncall rota" {
		t.Errorf("the row says %q rather than naming the document", row.Title)
	}
	if row.Stale == nil || row.Stale.Count != 2 {
		t.Fatalf("two people reported it and the row says %+v", row.Stale)
	}
	// The last thing said and the person who said it, so the owner has a sentence
	// to act on and somebody to go back to rather than a number to argue with.
	if row.Stale.Note != "and it still names a rota we stopped running" {
		t.Errorf("the row carries the note %q", row.Stale.Note)
	}
	if row.Stale.By != "ren@acme.com" {
		t.Errorf("the row says %q reported it", row.Stale.By)
	}
}

// An empty inbox is the common case and has to be cheap to draw.
func TestAnEmptyInboxIsAListRatherThanANull(t *testing.T) {
	h := newInboxServer(t, clock)

	w := request(t, h, http.MethodGet, "/api/v1/reported", owner())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := decode[inbox](t, w); got.Documents == nil || len(got.Documents) != 0 {
		t.Errorf("an empty inbox came back as %+v", got.Documents)
	}
}

// Most recently reported first, because the report somebody made this morning is
// the one that is still being lived with.
func TestTheInboxIsMostRecentFirstAndHonoursALimit(t *testing.T) {
	h := newInboxServer(t, stepping(clock()))

	reportOne(t, h, "mine", "the first thing anybody said", colleague())
	reportOne(t, h, "hers", "and the second", colleague())

	if got := ids(inboxOf(t, h, "", owner())); len(got) != 2 || got[0] != "hers" {
		t.Fatalf("the inbox reads %v, and hers was reported last", got)
	}
	if got := ids(inboxOf(t, h, "?limit=1", owner())); len(got) != 1 || got[0] != "hers" {
		t.Errorf("asking for one gave %v", got)
	}
	if w := request(t, h, http.MethodGet, "/api/v1/reported?limit=nine", owner()); w.Code != http.StatusBadRequest {
		t.Errorf("a limit that is not a number answered %d", w.Code)
	}
}

// Dealing with a report takes the document off the list, which is what makes the
// list a thing somebody can get to the bottom of.
func TestClearingAReportEmptiesTheInbox(t *testing.T) {
	h := newInboxServer(t, clock)
	reportOne(t, h, "hers", "out of date", colleague())

	if w := post(t, h, http.MethodDelete, "/api/v1/documents/hers/stale", "", owner()); w.Code != http.StatusNoContent {
		t.Fatalf("clearing the report: %d %s", w.Code, w.Body.String())
	}
	if got := ids(inboxOf(t, h, "", owner())); len(got) != 0 {
		t.Errorf("the report was dealt with and the inbox still holds %v", got)
	}
}

// The home screen reads this on every visit, so an answer that has not changed
// has to cost a tag rather than a body.
func TestAnUnchangedInboxIsNotSentTwice(t *testing.T) {
	h := newInboxServer(t, clock)
	reportOne(t, h, "hers", "out of date", colleague())

	first := request(t, h, http.MethodGet, "/api/v1/reported", owner())
	tag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || tag == "" {
		t.Fatalf("the first read answered %d with tag %q", first.Code, tag)
	}

	headers := owner()
	headers["If-None-Match"] = tag
	second := request(t, h, http.MethodGet, "/api/v1/reported", headers)
	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304: %s", second.Code, second.Body.String())
	}
	if second.Body.Len() != 0 {
		t.Errorf("a not modified answer carried %d bytes", second.Body.Len())
	}
}
