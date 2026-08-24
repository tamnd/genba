package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tamnd/genba/api"
)

// Resolving a handful of ids into titles.
//
// The one screen that needs this is the answers editor, which holds source ids
// and has to print something a person recognises. Everything worth pinning here
// is about what it refuses to do: it never answers with a document the caller
// may not read, it never answers with the corpus, and it never answers with
// more than was asked for.

type resolved struct {
	Documents []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Source string `json:"source"`
		Kind   string `json:"kind"`
	} `json:"documents"`
}

func resolve(t *testing.T, h http.Handler, who map[string]string, ids ...string) resolved {
	t.Helper()
	w := request(t, h, http.MethodGet, "/api/v1/documents?"+query(ids), who)
	if w.Code != http.StatusOK {
		t.Fatalf("resolving %v: %d %s", ids, w.Code, w.Body.String())
	}
	return decode[resolved](t, w)
}

func query(ids []string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, "id="+id)
	}
	return strings.Join(parts, "&")
}

func namesOf(got resolved) []string {
	out := make([]string, 0, len(got.Documents))
	for _, d := range got.Documents {
		out = append(out, d.ID)
	}
	return out
}

// The order is the caller's, because on the screen that asks it is the order
// somebody cited them in and a driver has no reason to keep it.
func TestDocumentsComeBackInTheOrderTheyWereAskedFor(t *testing.T) {
	h := newAnswerServer(t)

	got := resolve(t, h, engineer(), "oncall", "freeze")
	if names := namesOf(got); len(names) != 2 || names[0] != "oncall" || names[1] != "freeze" {
		t.Fatalf("resolved %v, want oncall then freeze", names)
	}
	if got.Documents[0].Title != "Holiday oncall" {
		t.Errorf("the first row is titled %q, and the title is the whole point of this endpoint", got.Documents[0].Title)
	}
	if got.Documents[0].Source != "gdrive" || got.Documents[0].Kind != "page" {
		t.Errorf("the row is %+v, want the same fields a result row carries", got.Documents[0])
	}
}

// The rule the whole surface runs on, applied here as well: a document this
// caller may not read is not in the answer, and is not an error either.
func TestDocumentsLeavesOutWhatTheCallerCannotRead(t *testing.T) {
	h := newAnswerServer(t)

	got := resolve(t, h, engineer(), "freeze", "budget", "oncall")
	if names := namesOf(got); len(names) != 2 || names[0] != "freeze" || names[1] != "oncall" {
		t.Fatalf("the engineer resolved %v, want the two they may read in the order asked", names)
	}
	// And the finance reader gets the other one, which is what says the line
	// above is a permission rule rather than a document that is missing.
	finance := map[string]string{
		api.HeaderSubject: "u_fin",
		api.HeaderGroups:  "gdrive:finance@acme.com",
	}
	if names := namesOf(resolve(t, h, finance, "freeze", "budget")); len(names) != 1 || names[0] != "budget" {
		t.Fatalf("finance resolved %v, want the cost model alone", names)
	}
}

// No ids is nothing, rather than everything. An endpoint that answered a bare
// path with the corpus would be a way of listing documents that never had to
// name one.
func TestDocumentsWithNoIdsIsEmpty(t *testing.T) {
	h := newAnswerServer(t)

	w := request(t, h, http.MethodGet, "/api/v1/documents", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want an empty answer rather than a refusal", w.Code)
	}
	if got := decode[resolved](t, w); len(got.Documents) != 0 {
		t.Fatalf("a request naming nothing came back with %v", namesOf(got))
	}
	if got := resolve(t, h, engineer(), ""); len(got.Documents) != 0 {
		t.Fatalf("an empty id came back with %v", namesOf(got))
	}
}

// A screen that cites the same document twice has a bug in it, and answering
// with the document twice would make that bug somebody else's to find.
func TestDocumentsAreNotRepeated(t *testing.T) {
	h := newAnswerServer(t)

	got := resolve(t, h, engineer(), "freeze", "freeze", "oncall", "freeze")
	if names := namesOf(got); len(names) != 2 || names[0] != "freeze" || names[1] != "oncall" {
		t.Fatalf("resolved %v, want each named document once", names)
	}
}

// The bound is on the request rather than on the answer, so somebody who asks
// for too much is told, instead of being given a page of it and left to think
// the rest do not exist.
func TestDocumentsRefusesMoreThanItWillRead(t *testing.T) {
	h := newAnswerServer(t)

	ids := make([]string, 0, api.DocumentsMax+1)
	for i := range api.DocumentsMax + 1 {
		ids = append(ids, fmt.Sprintf("d%d", i))
	}
	w := request(t, h, http.MethodGet, "/api/v1/documents?"+query(ids), engineer())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for more ids than one statement reads", w.Code)
	}

	// And exactly the bound is fine, so the refusal is a ceiling rather than an
	// off by one nobody would find until a screen grew.
	w = request(t, h, http.MethodGet, "/api/v1/documents?"+query(ids[:api.DocumentsMax]), engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d for exactly the bound, want 200", w.Code)
	}
}

// It is revalidated like every other read here, because the editor asks the
// same question every time somebody opens the same answer.
func TestDocumentsRevalidate(t *testing.T) {
	h := newAnswerServer(t)

	w := request(t, h, http.MethodGet, "/api/v1/documents?id=freeze", engineer())
	tag := w.Header().Get("ETag")
	if tag == "" {
		t.Fatal("no entity tag, so the editor refetches every title every time it opens")
	}

	who := engineer()
	who["If-None-Match"] = tag
	if again := request(t, h, http.MethodGet, "/api/v1/documents?id=freeze", who); again.Code != http.StatusNotModified {
		t.Fatalf("status = %d with the tag it was just given, want 304", again.Code)
	}
}
