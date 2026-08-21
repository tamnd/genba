package index

import (
	"strings"
	"testing"

	"github.com/tamnd/genba/doc"
)

// A snippet is cut out of a window of the source rather than out of the whole
// document, and for markdown the syntax is removed from the window rather than
// from the document. These are the properties that has to keep.

func markdown(body string) doc.Document {
	return doc.Document{Body: body, Properties: map[string]string{doc.MediaType: "text/markdown"}}
}

func TestSnippetFindsAMatchPastTheWindow(t *testing.T) {
	body := strings.Repeat("Nothing relevant here. ", 400) +
		"The payments queue drains through the replica during a failover."
	got, _ := snippet(doc.Document{Body: body}, []string{"replica"})

	if !strings.Contains(got, "replica") {
		t.Fatalf("the matched term is not in the snippet:\n%s", got)
	}
	if !strings.HasPrefix(got, "...") {
		t.Errorf("a snippet from the middle of a body does not say so:\n%s", got)
	}
	if len(got) > 2*SnippetWidth {
		t.Errorf("the snippet is %d bytes, which is not a snippet:\n%s", len(got), got)
	}
}

func TestSnippetOfMarkdownReadsAsProse(t *testing.T) {
	body := strings.Repeat("# Heading\n\nSome filler prose here.\n\n", 60) +
		"The **payments** queue drains through the `replica` during a failover.\n"
	got, _ := snippet(markdown(body), []string{"replica"})

	if !strings.Contains(got, "replica") {
		t.Fatalf("the matched term is not in the snippet:\n%s", got)
	}
	for _, syntax := range []string{"**", "`", "# "} {
		if strings.Contains(got, syntax) {
			t.Errorf("the snippet carries %q markup:\n%s", syntax, got)
		}
	}
}

// The window is taken from the source and the snippet is cut from the window
// after the markup has gone, so a document that is more markup than words still
// produces a full snippet rather than a few words and an ellipsis.
func TestSnippetIsFullEvenWhenTheSourceIsMostlyMarkup(t *testing.T) {
	body := strings.Repeat("| a | b | c |\n|---|---|---|\n", 40) +
		"payments settle overnight and the ledger is closed the following morning by the on call engineer\n"
	got, _ := snippet(markdown(body), []string{"payments"})

	words := len(strings.Fields(strings.ReplaceAll(got, "...", " ")))
	if words < 10 {
		t.Errorf("the snippet is %d words, so the window was too narrow:\n%s", words, got)
	}
}

func TestSnippetWithoutAMatchStartsAtTheBeginning(t *testing.T) {
	body := "The ledger closes overnight. " + strings.Repeat("More prose. ", 200)
	got, _ := snippet(doc.Document{Body: body}, []string{"absent"})

	if !strings.HasPrefix(got, "The ledger closes overnight.") {
		t.Errorf("a body with no match does not start at the top:\n%s", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("a snippet that stops short of the end does not say so:\n%s", got)
	}
}

func TestSnippetOfAShortBodyHasNoEllipsis(t *testing.T) {
	got, _ := snippet(doc.Document{Body: "The payments queue drains overnight."}, []string{"payments"})
	if strings.Contains(got, "...") {
		t.Errorf("a body shorter than a snippet was reported as cut:\n%s", got)
	}
}

func TestPassagesJoinToTheSnippet(t *testing.T) {
	body := strings.Repeat("Nothing relevant here. ", 100) +
		"The payments queue drains through the replica.\n"
	got, passages := snippet(markdown(body), []string{"payments", "replica"})

	var rebuilt strings.Builder
	var marked int
	for _, p := range passages {
		rebuilt.WriteString(p.Text)
		if p.Match {
			marked++
		}
	}
	if rebuilt.String() != got {
		t.Fatalf("the passages join to %q, want the snippet %q", rebuilt.String(), got)
	}
	if marked != 2 {
		t.Errorf("marked %d passages, want the two terms that matched:\n%s", marked, got)
	}
}

// The cost of a result on a page. It used to be the cost of the document,
// because finding where to cut meant analysing the whole body and taking the
// markup out meant rewriting the whole body. See #112.
func BenchmarkSnippet(b *testing.B) {
	prose := strings.Repeat("The ledger closes overnight and the queue drains. ", 400)
	late := prose + "The payments queue drains through the replica during a failover."
	terms := []string{"replica"}

	b.Run("plain", func(b *testing.B) {
		d := doc.Document{Body: late}
		b.ReportAllocs()
		for b.Loop() {
			_, _ = snippet(d, terms)
		}
	})
	b.Run("markdown", func(b *testing.B) {
		d := markdown(strings.Repeat("# Heading\n\n"+prose+"\n\n", 4) + "\nThe `replica` drains.\n")
		b.ReportAllocs()
		for b.Loop() {
			_, _ = snippet(d, terms)
		}
	})
	b.Run("early", func(b *testing.B) {
		d := doc.Document{Body: "The replica drains overnight. " + prose}
		b.ReportAllocs()
		for b.Loop() {
			_, _ = snippet(d, terms)
		}
	})
}
