package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
)

// The answer somebody wrote down.
//
// Everything else on this surface improves the corpus by marking it. This is the
// one endpoint that adds to it, and the two things worth pinning are what it
// takes the place of and what it is allowed to show. It takes the place of the
// quoted answer above the results, because that region holds one answer and two
// of them is a reader choosing which of the product's two answers to believe.
// What it may show is bounded by the reader rather than by whoever wrote it: an
// answer written by an administrator who can read everything must not turn into
// a list of the documents the reader in front of it cannot open.

// card is the answer above the results as this file reads it.
type card struct {
	ID       string    `json:"id"`
	Question string    `json:"question"`
	Body     string    `json:"body"`
	By       string    `json:"by"`
	Email    string    `json:"email"`
	At       time.Time `json:"at"`
	Until    time.Time `json:"until"`
	State    string    `json:"state"`
	Sources  []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"sources"`
}

// answered is the search response as far as this file is concerned. The quoted
// answer is here only so that a test can watch it step aside.
type answered struct {
	Curated *card `json:"curated"`
	Answer  *struct {
		Quotes []struct {
			ID string `json:"id"`
		} `json:"quotes"`
	} `json:"answer"`
	Hits []struct {
		ID string `json:"id"`
	} `json:"hits"`
}

type answerList struct {
	Answers []struct {
		ID       string   `json:"id"`
		Question string   `json:"question"`
		Variants []string `json:"variants"`
		Body     string   `json:"body"`
		Sources  []string `json:"sources"`
		By       string   `json:"by"`
		State    string   `json:"state"`
	} `json:"answers"`
}

// newAnswerServer holds two documents the engineer can read and one they cannot,
// which is the shape the citation rule needs: an answer that cites all three.
func newAnswerServer(t *testing.T) http.Handler {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	eng := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
		Version:     1,
	}
	finance := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "finance@acme.com"}},
		Version:     1,
	}
	docs := []doc.Document{
		{
			ID: "freeze", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Release calendar", Body: "The deploy freeze runs over the end of the year.",
			Permissions: eng,
		},
		{
			ID: "oncall", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Holiday oncall", Body: "Who carries the pager over the freeze.",
			Permissions: eng,
		},
		{
			ID: "budget", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Deploy freeze cost model", Body: "What the freeze costs in deferred revenue.",
			Permissions: finance,
		},
	}
	if err := st.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, api.WithClock(clock))
	return s.Handler()
}

// write puts an answer up as the administrator, which is the only person who
// may, and returns the response so a test can look at what came back.
func write(t *testing.T, h http.Handler, id, body string) {
	t.Helper()
	w := post(t, h, http.MethodPut, "/api/v1/admin/answers/"+id, body, curator())
	if w.Code != http.StatusOK {
		t.Fatalf("writing the answer: %d %s", w.Code, w.Body.String())
	}
}

// theFreeze is the answer these tests write, citing one document the reader can
// open, one they cannot, and one they can.
const theFreeze = `{
	"question": "What is the deploy freeze?",
	"variants": ["when is the code freeze"],
	"body": "No production deploys from the 20th of December to the 2nd of January, except for incidents.",
	"sources": ["freeze", "budget", "oncall"]
}`

func searchAs(t *testing.T, h http.Handler, query string, who map[string]string) answered {
	t.Helper()
	w := request(t, h, http.MethodGet, "/api/v1/search?q="+query, who)
	if w.Code != http.StatusOK {
		t.Fatalf("searching for %q: %d %s", query, w.Code, w.Body.String())
	}
	return decode[answered](t, w)
}

func TestAWrittenAnswerStandsInFrontOfTheResults(t *testing.T) {
	h := newAnswerServer(t)

	// The same question before anybody wrote it down, so that the quoted answer
	// stepping aside below is something that happened rather than something that
	// was never there.
	before := searchAs(t, h, "What+is+the+deploy+freeze%3F", engineer())
	if before.Curated != nil {
		t.Fatal("a card came back for a question nobody has answered")
	}
	if before.Answer == nil {
		t.Fatal("the query does not produce a quoted answer, so this test is not watching one step aside")
	}

	write(t, h, "a_freeze", theFreeze)

	got := searchAs(t, h, "What+is+the+deploy+freeze%3F", engineer())
	if got.Curated == nil {
		t.Fatal("the question somebody answered came back with no answer on it")
	}
	if got.Answer != nil {
		t.Fatal("the quoted answer is still there, so the page offers two answers to one question")
	}
	if len(got.Hits) == 0 {
		t.Fatal("the results went away, and the card is meant to stand in front of them rather than instead of them")
	}
	if got.Curated.Question != "What is the deploy freeze?" {
		t.Fatalf("the card is titled %q", got.Curated.Question)
	}
	if got.Curated.Body == "" {
		t.Fatal("the card has no body, which is the only part of it that answers anything")
	}
}

// The name and the date are the reason a reader believes a card over the four
// documents underneath it.
func TestTheCardSaysWhoWroteItAndWhenItWasVerified(t *testing.T) {
	h := newAnswerServer(t)
	write(t, h, "a_freeze", theFreeze)

	got := searchAs(t, h, "when+is+the+code+freeze", engineer()).Curated
	if got == nil {
		t.Fatal("the variant found nothing, so an answer is only ever found by the person who wrote it")
	}
	if got.By != "u_ops" {
		t.Fatalf("the card is signed %q, and an unsigned card is worse than no card", got.By)
	}
	if !got.At.Equal(clock()) {
		t.Fatalf("it was written at %v, want the request clock at %v", got.At, clock())
	}
	if want := clock().Add(store.Cadence); !got.Until.Equal(want) {
		t.Fatalf("it runs out at %v, want the standing cadence at %v", got.Until, want)
	}
	if got.State != string(store.Fresh) {
		t.Fatalf("an answer written a moment ago is %q", got.State)
	}
}

// An answer cites what its author cited, and shows what its reader may open.
func TestACardNeverCitesADocumentTheReaderCannotOpen(t *testing.T) {
	h := newAnswerServer(t)
	write(t, h, "a_freeze", theFreeze)

	got := searchAs(t, h, "What+is+the+deploy+freeze%3F", engineer()).Curated
	if got == nil {
		t.Fatal("no card came back")
	}
	ids := make([]string, 0, len(got.Sources))
	for _, src := range got.Sources {
		ids = append(ids, src.ID)
		if src.ID == "budget" {
			t.Fatal("the card cites a document this reader has no access to, which is an existence oracle with a title on it")
		}
	}
	if len(ids) != 2 || ids[0] != "freeze" || ids[1] != "oncall" {
		t.Fatalf("the card cites %v, want the two readable ones in the order they were cited", ids)
	}
	if got.Sources[0].Title != "Release calendar" {
		t.Fatalf("the citation reads %q, and an id is not something a reader can decide to click", got.Sources[0].Title)
	}
}

// The match is the whole question, because the cost of getting this wrong is
// not a bad result, it is a reader believing a confident answer to a question
// they did not ask.
func TestANearMissGetsTheOrdinaryPage(t *testing.T) {
	h := newAnswerServer(t)
	write(t, h, "a_freeze", theFreeze)

	for _, query := range []string{"deploy+freeze", "what+is+the+deploy+freeze+in+december", "pager"} {
		if got := searchAs(t, h, query, engineer()); got.Curated != nil {
			t.Fatalf("searching for %q drew a card titled %q", query, got.Curated.Question)
		}
	}
}

func TestRetractingAnAnswerTakesTheCardDown(t *testing.T) {
	h := newAnswerServer(t)
	write(t, h, "a_freeze", theFreeze)

	w := request(t, h, http.MethodDelete, "/api/v1/admin/answers/a_freeze", curator())
	if w.Code != http.StatusNoContent {
		t.Fatalf("retracting: %d %s", w.Code, w.Body.String())
	}
	if got := searchAs(t, h, "What+is+the+deploy+freeze%3F", engineer()); got.Curated != nil {
		t.Fatal("the retracted answer is still above the results")
	}
	// Twice, so that a mistake can be undone twice and two administrators
	// clearing the same bad answer do not race each other into a failure.
	if w := request(t, h, http.MethodDelete, "/api/v1/admin/answers/a_freeze", curator()); w.Code != http.StatusNoContent {
		t.Fatalf("retracting again: %d %s", w.Code, w.Body.String())
	}
}

// An answer stands in front of the results for everybody in the tenant, so the
// blast radius of a bad one is the whole deployment.
func TestOnlyAnAdministratorCanWriteAnAnswer(t *testing.T) {
	h := newAnswerServer(t)

	w := post(t, h, http.MethodPut, "/api/v1/admin/answers/a_freeze", theFreeze, engineer())
	if w.Code != http.StatusForbidden {
		t.Fatalf("an ordinary reader writing an answer got %d, want 403", w.Code)
	}
	w = request(t, h, http.MethodDelete, "/api/v1/admin/answers/a_freeze", engineer())
	if w.Code != http.StatusForbidden {
		t.Fatalf("an ordinary reader retracting an answer got %d, want 403", w.Code)
	}
	w = request(t, h, http.MethodGet, "/api/v1/admin/answers", engineer())
	if w.Code != http.StatusForbidden {
		t.Fatalf("an ordinary reader listing the answers got %d, want 403", w.Code)
	}
}

// The three fields a card is unreadable without are refused with a 400 rather
// than a 500, because the difference is whether the interface should tell
// somebody to try again with the same words.
func TestAnIncompleteAnswerIsRefusedAsABadRequest(t *testing.T) {
	h := newAnswerServer(t)
	for name, body := range map[string]string{
		"no question": `{"body":"words"}`,
		"no body":     `{"question":"what is the deploy freeze"}`,
		"not json":    `{`,
	} {
		w := post(t, h, http.MethodPut, "/api/v1/admin/answers/a_freeze", body, curator())
		if w.Code != http.StatusBadRequest {
			t.Fatalf("an answer with %s got %d, want 400", name, w.Code)
		}
	}
}

// The maintenance list carries the sources as ids rather than as rows, because
// resolving them here and saving back what came out would quietly drop every
// source the editor happens not to have access to.
func TestTheAnswerListIsWhatAnEditorNeeds(t *testing.T) {
	h := newAnswerServer(t)
	write(t, h, "a_freeze", theFreeze)

	w := request(t, h, http.MethodGet, "/api/v1/admin/answers", curator())
	if w.Code != http.StatusOK {
		t.Fatalf("listing: %d %s", w.Code, w.Body.String())
	}
	got := decode[answerList](t, w)
	if len(got.Answers) != 1 {
		t.Fatalf("the list holds %d answers, want the one that was written", len(got.Answers))
	}
	a := got.Answers[0]
	switch {
	case a.ID != "a_freeze":
		t.Fatalf("the answer came back as %q", a.ID)
	case len(a.Variants) != 1 || a.Variants[0] != "when is the code freeze":
		t.Fatalf("the variants came back as %v, and they are what an editor edits", a.Variants)
	case len(a.Sources) != 3:
		t.Fatalf("the sources came back as %v, want all three ids including the one this reader cannot open", a.Sources)
	case a.State != string(store.Fresh):
		t.Fatalf("the state came back as %q", a.State)
	}
}

// Editing is the same call as writing, and it is also how an answer is
// re-verified.
func TestWritingAnAnswerAgainEditsItRatherThanAddingOne(t *testing.T) {
	h := newAnswerServer(t)
	write(t, h, "a_freeze", theFreeze)
	write(t, h, "a_freeze", `{
		"question": "What is the deploy freeze?",
		"body": "The freeze now runs to the 6th of January.",
		"sources": ["freeze"]
	}`)

	got := searchAs(t, h, "What+is+the+deploy+freeze%3F", engineer()).Curated
	if got == nil {
		t.Fatal("no card came back")
	}
	if got.Body != "The freeze now runs to the 6th of January." {
		t.Fatalf("the card still reads %q, so the edit did not land", got.Body)
	}
	// The variant went with the edit, because an answer has to be able to lose a
	// question it turned out not to answer.
	if second := searchAs(t, h, "when+is+the+code+freeze", engineer()); second.Curated != nil {
		t.Fatal("the dropped variant still finds the answer")
	}

	w := request(t, h, http.MethodGet, "/api/v1/admin/answers", curator())
	if got := decode[answerList](t, w); len(got.Answers) != 1 {
		t.Fatalf("the tenant holds %d answers, and an edit is not a second answer", len(got.Answers))
	}
}

// The list is read on a screen that polls, so an unchanged one revalidates
// rather than being sent again.
func TestAnUnchangedAnswerListIsNotSentTwice(t *testing.T) {
	h := newAnswerServer(t)
	write(t, h, "a_freeze", theFreeze)

	first := request(t, h, http.MethodGet, "/api/v1/admin/answers", curator())
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("the list carries no entity tag, so there is nothing to revalidate against")
	}
	who := curator()
	who["If-None-Match"] = tag
	second := request(t, h, http.MethodGet, "/api/v1/admin/answers", who)
	if second.Code != http.StatusNotModified {
		t.Fatalf("asking again with the tag returned %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("a 304 carried %d bytes of body", second.Body.Len())
	}
}
