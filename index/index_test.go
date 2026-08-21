package index_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func clock() time.Time { return epoch }

func principal(groups ...string) *acl.Principal {
	return &acl.Principal{
		Tenant:  "acme",
		Subject: "u_mei",
		Groups:  acl.GroupSet{Version: 1, Members: groups},
	}
}

func openTo(groups ...string) acl.Permissions {
	perm := acl.Permissions{Mode: acl.ModeACL, Source: "gdrive", Version: 1}
	for _, g := range groups {
		perm.AllowGroups = append(perm.AllowGroups, acl.Ref{Source: "gdrive", Value: g})
	}
	return perm
}

type fixture struct {
	id       string
	title    string
	body     string
	source   string
	kind     doc.Kind
	media    string
	modified time.Time
	perm     acl.Permissions
}

func newSearcher(t *testing.T, fixtures []fixture, opts ...index.Option) *index.Searcher {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Put(t.Context(), documents(fixtures)...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	opts = append([]index.Option{index.WithClock(clock)}, opts...)
	return index.New(st, opts...)
}

func ids(res index.Results) []string {
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.Document.ID)
	}
	return out
}

var corpus = []fixture{
	{id: "d1", title: "Payments failover runbook", body: "Failover the payments queue when the primary region is unhealthy.", perm: openTo("eng@acme.com")},
	{id: "d2", title: "Weekly engineering notes", body: "We talked about payments, hiring and the office move.", perm: openTo("eng@acme.com")},
	{id: "d3", title: "Sales pipeline review", body: "Deals closing this quarter.", source: "salesforce", kind: doc.KindTicket, perm: openTo("sales@acme.com")},
}

func TestSearchRanksTitleMatchesFirst(t *testing.T) {
	s := newSearcher(t, corpus)
	res, err := s.Search(t.Context(), principal("gdrive:eng@acme.com"), index.Query{Text: "payments runbook"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := ids(res); !slices.Equal(got, []string{"d1", "d2"}) {
		t.Fatalf("got %v, want the runbook first and the notes second", got)
	}
}

func TestSearchNeverReturnsUnreadableDocuments(t *testing.T) {
	s := newSearcher(t, corpus)
	res, err := s.Search(t.Context(), principal("gdrive:eng@acme.com"), index.Query{Text: "deals quarter"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("a reader without access to the sales document got %v", ids(res))
	}
}

func TestSearchRequiresAPrincipal(t *testing.T) {
	s := newSearcher(t, corpus)
	if _, err := s.Search(t.Context(), nil, index.Query{Text: "payments"}); !errors.Is(err, genba.ErrNoPrincipal) {
		t.Fatalf("Search with a nil principal returned %v, want ErrNoPrincipal", err)
	}
}

func TestFiltersNarrowTheCandidateSet(t *testing.T) {
	s := newSearcher(t, corpus)
	p := principal("gdrive:eng@acme.com", "gdrive:sales@acme.com")

	res, err := s.Search(t.Context(), p, index.Query{Text: "payments deals", Sources: []string{"salesforce"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := ids(res); !slices.Equal(got, []string{"d3"}) {
		t.Fatalf("got %v, want only the salesforce document", got)
	}

	res, err = s.Search(t.Context(), p, index.Query{Text: "payments deals", Kinds: []doc.Kind{doc.KindTicket}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := ids(res); !slices.Equal(got, []string{"d3"}) {
		t.Fatalf("got %v, want only the ticket", got)
	}
}

func TestFacetsCountTheWholeMatchSet(t *testing.T) {
	s := newSearcher(t, corpus)
	p := principal("gdrive:eng@acme.com", "gdrive:sales@acme.com")

	res, err := s.Search(t.Context(), p, index.Query{Limit: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("asked for one hit, got %d", len(res.Hits))
	}
	if res.Total != 3 {
		t.Fatalf("Total = %d, want 3", res.Total)
	}
	want := []index.Facet{{Value: "gdrive", Count: 2}, {Value: "salesforce", Count: 1}}
	if !slices.Equal(res.Facets["source"], want) {
		t.Fatalf("source facets = %v, want %v", res.Facets["source"], want)
	}
}

func TestPagingIsStable(t *testing.T) {
	s := newSearcher(t, corpus)
	p := principal("gdrive:eng@acme.com", "gdrive:sales@acme.com")

	first, err := s.Search(t.Context(), p, index.Query{Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	second, err := s.Search(t.Context(), p, index.Query{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := append(ids(first), ids(second)...)
	if !slices.Equal(got, []string{"d1", "d2", "d3"}) {
		t.Fatalf("paging produced %v, want every document exactly once", got)
	}

	past, err := s.Search(t.Context(), p, index.Query{Offset: 500})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(past.Hits) != 0 {
		t.Fatalf("an offset past the end returned %v", ids(past))
	}
}

func TestRecencyBreaksATie(t *testing.T) {
	fixtures := []fixture{
		{id: "old", title: "Deploy checklist", body: "Same body.", modified: epoch.AddDate(-3, 0, 0), perm: openTo("eng@acme.com")},
		{id: "new", title: "Deploy checklist", body: "Same body.", modified: epoch.AddDate(0, 0, -1), perm: openTo("eng@acme.com")},
	}
	s := newSearcher(t, fixtures)
	res, err := s.Search(t.Context(), principal("gdrive:eng@acme.com"), index.Query{Text: "deploy checklist"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := ids(res); !slices.Equal(got, []string{"new", "old"}) {
		t.Fatalf("got %v, want the newer document first", got)
	}
}

func TestSnippetIsDrawnFromTheBody(t *testing.T) {
	body := strings.Repeat("Nothing relevant here. ", 40) +
		"The payments queue drains through the eu-west-1 replica during a failover."
	s := newSearcher(t, []fixture{{id: "d1", title: "Runbook", body: body, perm: openTo("eng@acme.com")}})

	res, err := s.Search(t.Context(), principal("gdrive:eng@acme.com"), index.Query{Text: "replica"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	snip := res.Hits[0].Snippet
	if !strings.Contains(snip, "replica") {
		t.Fatalf("snippet %q does not contain the matched term", snip)
	}
	if len(snip) >= len(body) {
		t.Fatalf("snippet is not shorter than the body it came from")
	}
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"Hello, World!", []string{"hello", "world"}},
		{"eu-west-1", []string{"eu", "west", "1"}},
		{"  ", nil},
		{"現場の記録", []string{"現", "場", "の", "記", "録"}},
	}
	for _, tt := range tests {
		if got := index.Tokenize(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("Tokenize(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// The browser marks a snippet from the passages the server returned rather than
// searching the snippet text for the query, because the analyzer is what
// decided the match and a substring search would highlight words the index
// never matched. These are the properties the browser relies on.
func TestPassagesCoverTheSnippetAndMarkOnlyMatches(t *testing.T) {
	s := newSearcher(t, []fixture{{
		id:    "d1",
		title: "Runbook",
		body:  "Failover the payments queue. Payment volume is unaffected by the failover.",
		perm:  openTo("eng@acme.com"),
	}})

	res, err := s.Search(t.Context(), principal("gdrive:eng@acme.com"), index.Query{Text: "payments"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	hit := res.Hits[0]

	var rebuilt strings.Builder
	marked := 0
	for _, p := range hit.Passages {
		rebuilt.WriteString(p.Text)
		if !p.Match {
			continue
		}
		marked++
		if !strings.EqualFold(p.Text, "payments") {
			t.Errorf("marked %q, want only the term that matched", p.Text)
		}
	}
	if rebuilt.String() != hit.Snippet {
		t.Fatalf("the passages join to %q, want the snippet %q", rebuilt.String(), hit.Snippet)
	}
	if marked != 1 {
		t.Fatalf("marked %d passages, want 1", marked)
	}
}

// A term the analyzer would fold to nothing, or one that appears only in the
// title, must not produce a mark that is not there in the text.
func TestPassagesAreEmptyWithoutAMatchInTheBody(t *testing.T) {
	s := newSearcher(t, []fixture{{
		id:    "d1",
		title: "Failover runbook",
		body:  "Drain the queue, then bring the replica up.",
		perm:  openTo("eng@acme.com"),
	}})

	res, err := s.Search(t.Context(), principal("gdrive:eng@acme.com"), index.Query{Text: "failover"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	for _, p := range res.Hits[0].Passages {
		if p.Match {
			t.Fatalf("marked %q, which is not in the body", p.Text)
		}
	}
}

// Two lines of a result row is a small budget, and a snippet full of hashes,
// asterisks and table pipes has spent it on nothing.
func TestSnippetsFromMarkdownReadAsProse(t *testing.T) {
	s := newSearcher(t, []fixture{{
		id:    "spec",
		title: "Permissions",
		media: "text/markdown",
		body: "# Permissions\n\n" +
			"The rule is that a **principal** is applied by the `driver`, not by the caller.\n" +
			"\n| Mode | Meaning |\n| --- | --- |\n| acl | a list |\n",
		perm: openTo("eng@acme.com"),
	}})
	res, err := s.Search(t.Context(), principal("gdrive:eng@acme.com"), index.Query{Text: "principal"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	snippet := res.Hits[0].Snippet
	for _, syntax := range []string{"#", "**", "`", "|"} {
		if strings.Contains(snippet, syntax) {
			t.Errorf("the snippet still carries %q:\n%s", syntax, snippet)
		}
	}
	if !strings.Contains(snippet, "principal") {
		t.Errorf("the matched term is gone from the snippet:\n%s", snippet)
	}
	var marked []string
	for _, p := range res.Hits[0].Passages {
		if p.Match {
			marked = append(marked, p.Text)
		}
	}
	if !slices.Equal(marked, []string{"principal"}) {
		t.Errorf("marked %v, want the term marked at its offset in the stripped text", marked)
	}
}

// A source file is not markdown, and stripping what looks like syntax out of
// code would be lying about the file.
func TestSnippetsFromCodeAreLeftAlone(t *testing.T) {
	s := newSearcher(t, []fixture{{
		id:    "src",
		title: "store/store.go",
		kind:  doc.KindCode,
		media: "text/x-go",
		body:  "// principal is applied by the driver\nfunc visible(p *acl.Principal) bool { return *p.ok }\n",
		perm:  openTo("eng@acme.com"),
	}})
	res, err := s.Search(t.Context(), principal("gdrive:eng@acme.com"), index.Query{Text: "principal"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.Hits))
	}
	if !strings.Contains(res.Hits[0].Snippet, "*p.ok") {
		t.Errorf("the code was rewritten:\n%s", res.Hits[0].Snippet)
	}
}
