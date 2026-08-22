package index_test

import (
	"strings"
	"testing"

	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
)

// The answer's whole claim is that it is quoted rather than written, so every
// test in here is a way of asking whether that is still true.
var quotable = []fixture{
	{
		id:    "run",
		title: "Payments failover runbook",
		body: "This runbook covers the payments queue only.\n\n" +
			"When the primary region stops acknowledging writes, failover the payments queue to the standby region before draining anything. " +
			"The drain is safe once the queue has failed over and not before.\n\n" +
			"Escalate to the on call engineer if the failover takes longer than ten minutes.",
		perm: openTo("eng@acme.com"),
	},
	{
		id:    "notes",
		title: "Weekly engineering notes",
		body: "Attendees: Mei, Sam, Alex.\n\n" +
			"We agreed that the payments queue needs a second consumer before the end of the quarter, and that the failover drill happens monthly from now on.\n\n" +
			"Nobody had anything on hiring.",
		perm: openTo("eng@acme.com"),
	},
	{
		id:    "conf",
		title: "queue.json",
		body:  `{"payments": {"queue": "primary", "failover": true}}`,
		kind:  doc.KindCode,
		perm:  openTo("eng@acme.com"),
	},
	{
		id:    "fenced",
		title: "Draining the standby",
		body: "# Draining the standby\n\n" +
			"```\ngenba drain --queue=payments --failover=standby --wait=10m\n```\n\n" +
			"Run it from the bastion.",
		media: "text/markdown",
		perm:  openTo("eng@acme.com"),
	},
	{
		id:    "secret",
		title: "Payments incident postmortem",
		body:  "The payments queue failover was triggered by a bad deploy on the ninth of March, and it took forty minutes to recover.",
		perm:  openTo("sales@acme.com"),
	},
}

func answerFor(t *testing.T, q index.Query, groups ...string) index.Answer {
	t.Helper()
	s := newSearcher(t, quotable)
	res, err := s.Search(t.Context(), principal(groups...), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return res.Answer
}

// This is the test the rest of the feature exists to make passable. A quote that
// is not a substring of the document it cites is a sentence the product wrote
// and attributed to somebody else, which is the exact failure a citation is
// supposed to make impossible.
func TestEveryQuoteIsInTheDocumentItCites(t *testing.T) {
	s := newSearcher(t, quotable)
	p := principal("gdrive:eng@acme.com")
	res, err := s.Search(t.Context(), p, index.Query{Text: "payments failover queue"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Answer.Quotes) == 0 {
		t.Fatal("a query matching three documents produced no quotes")
	}

	bodies := make(map[string]string, len(res.Hits))
	for _, hit := range res.Hits {
		bodies[hit.Document.ID] = collapseSpace(hit.Document.Body)
	}
	for _, q := range res.Answer.Quotes {
		body, ok := bodies[q.ID]
		if !ok {
			t.Fatalf("quote cites %q, which is not on the page it sits above", q.ID)
		}
		text := strings.TrimSuffix(q.Text, "...")
		if !strings.Contains(body, text) {
			t.Errorf("quote from %s is not in that document:\n  quoted: %q", q.ID, text)
		}
	}
}

// The passages are what the interface marks, so the text it draws has to be the
// text it was given. A split that dropped or duplicated a byte would highlight
// the right words in a sentence nobody wrote.
func TestPassagesRebuildTheQuote(t *testing.T) {
	a := answerFor(t, index.Query{Text: "payments failover"}, "gdrive:eng@acme.com")
	if len(a.Quotes) == 0 {
		t.Fatal("no quotes")
	}
	for _, q := range a.Quotes {
		var b strings.Builder
		matched := false
		for _, p := range q.Passages {
			b.WriteString(p.Text)
			matched = matched || p.Match
		}
		if b.String() != q.Text {
			t.Errorf("passages for %s rebuild %q, want %q", q.ID, b.String(), q.Text)
		}
		if !matched {
			t.Errorf("quote from %s marks none of the query terms", q.ID)
		}
	}
}

// A quote is read on its own rather than under a title, so it has to be a
// sentence. A window that starts mid word is a snippet and belongs on a result
// row instead.
func TestAQuoteIsAWholeSentence(t *testing.T) {
	a := answerFor(t, index.Query{Text: "payments failover queue"}, "gdrive:eng@acme.com")
	if len(a.Quotes) == 0 {
		t.Fatal("no quotes")
	}
	for _, q := range a.Quotes {
		if strings.HasPrefix(q.Text, "...") {
			t.Errorf("quote from %s opens with an ellipsis: %q", q.ID, q.Text)
		}
		first := rune(q.Text[0])
		if first >= 'a' && first <= 'z' {
			t.Errorf("quote from %s starts mid sentence: %q", q.ID, q.Text)
		}
		if len(q.Text) < index.MinQuote {
			t.Errorf("quote from %s is %d bytes, under the %d byte floor: %q", q.ID, len(q.Text), index.MinQuote, q.Text)
		}
	}
}

// A perfectly good result can have nothing in it worth reading on its own. The
// JSON fixture matches every term in the query and is one line with no sentence
// in it, and quoting it above the results would make the page worse than an
// answer region that is not there.
func TestADocumentWithNothingQuotableIsSkipped(t *testing.T) {
	a := answerFor(t, index.Query{Text: "payments failover queue"}, "gdrive:eng@acme.com")
	for _, q := range a.Quotes {
		if q.ID == "conf" {
			t.Fatalf("a line of JSON was quoted above the results: %q", q.Text)
		}
	}
}

// A command line is not a sentence, and it stays out of the answer even when it
// is the only place in the document the words appear. Markdown keeps the inside
// of a fence exactly as it was written, which is right for the index and for a
// snippet, and it means the quote has to be the thing that says no.
func TestAQuoteIsNeverALineOfCode(t *testing.T) {
	a := answerFor(t, index.Query{Text: "payments failover queue"}, "gdrive:eng@acme.com")
	for _, q := range a.Quotes {
		if q.ID == "fenced" {
			t.Fatalf("a command line was quoted above the results: %q", q.Text)
		}
	}
}

// One quote per document, at most three documents. Three quotes out of one file
// is a document worth opening rather than an answer.
func TestAnAnswerIsBoundedAndCitesEachDocumentOnce(t *testing.T) {
	a := answerFor(t, index.Query{Text: "payments failover queue region"}, "gdrive:eng@acme.com")
	if len(a.Quotes) > index.AnswerQuotes {
		t.Fatalf("got %d quotes, want at most %d", len(a.Quotes), index.AnswerQuotes)
	}
	seen := map[string]bool{}
	for _, q := range a.Quotes {
		if seen[q.ID] {
			t.Fatalf("%s was quoted twice", q.ID)
		}
		seen[q.ID] = true
	}
}

// The answer runs on the hits, so it inherits the permission rule rather than
// applying one of its own. This asserts the inheritance rather than trusting it:
// the postmortem is the best match in the corpus for these words and this reader
// may not read it.
func TestAnAnswerNeverQuotesADocumentTheReaderCannotOpen(t *testing.T) {
	a := answerFor(t, index.Query{Text: "payments failover postmortem deploy"}, "gdrive:eng@acme.com")
	for _, q := range a.Quotes {
		if q.ID == "secret" {
			t.Fatalf("quoted a document this reader may not open: %q", q.Text)
		}
	}
}

// Three states where the region is absent, and the interface is built around its
// absence: nothing below it moves when there is no answer.
func TestThereIsNoAnswerWithoutAQuestion(t *testing.T) {
	for _, tc := range []struct {
		what  string
		query index.Query
	}{
		{"a browse with no terms", index.Query{}},
		{"the second page", index.Query{Text: "payments failover", Offset: 20}},
		{"a list sorted by date", index.Query{Text: "payments failover", Sort: index.ByRecent}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if a := answerFor(t, tc.query, "gdrive:eng@acme.com"); len(a.Quotes) != 0 {
				t.Fatalf("got %d quotes, want none", len(a.Quotes))
			}
		})
	}
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }
