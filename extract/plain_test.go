package extract

import (
	"strings"
	"testing"
)

func TestPlainTextIsCopiedThroughUnchanged(t *testing.T) {
	// The line breaks in a log file and the indentation in a source file are
	// content, and a reader that tidied them would be losing information for
	// nothing.
	const in = "2026-03-12 10:04:01 ERROR ingest: refresh took 4h12m\n\tsource=share-01\n\n2026-03-12 10:04:02 INFO  ingest: retrying\n"

	doc, err := Extract(t.Context(), strings.NewReader(in), "genbad.log")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Text != in {
		t.Errorf("text is %q, want %q", doc.Text, in)
	}
}

func TestATextFileWithControlBytesComesOutClean(t *testing.T) {
	// A NUL reaches a text column in Postgres and is refused there, which
	// fails the write of a document that was otherwise perfectly readable.
	doc, err := Extract(t.Context(), strings.NewReader("before\x00after\x07\n"), "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(doc.Text, "\x00\x07") {
		t.Errorf("text still holds a control byte: %q", doc.Text)
	}
	if !strings.Contains(doc.Text, "beforeafter") {
		t.Errorf("the text around it did not survive: %q", doc.Text)
	}
}

func TestSeparatedValuesBecomeATable(t *testing.T) {
	const in = "name,team\nAmara,Field ops\nJonas,Platform\n"

	doc, err := Extract(t.Context(), strings.NewReader(in), "people.csv")
	if err != nil {
		t.Fatal(err)
	}
	want := "| name | team |\n| --- | --- |\n| Amara | Field ops |\n| Jonas | Platform |\n"
	if doc.Text != want {
		t.Errorf("text is %q, want %q", doc.Text, want)
	}
}

func TestARaggedExportIsStillATable(t *testing.T) {
	// A real export has rows of different lengths and quotes inside unquoted
	// fields, and refusing to read one is no service to whoever is looking for
	// it.
	const in = "id,note,owner\n1,\"a quoted, comma\",amara\n2,,jonas\n3,unquoted \"quote\",priya\n4,short\n"

	doc, err := Extract(t.Context(), strings.NewReader(in), "export.csv")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| a quoted, comma |", "| 3 | unquoted \"quote\" | priya |", "| 4 | short |"} {
		if !strings.Contains(doc.Text, want) {
			t.Errorf("text does not contain %q:\n%s", want, doc.Text)
		}
	}
}

func TestProseWithCommasIsNotATable(t *testing.T) {
	// One comma per line is a paragraph, not a spreadsheet, and turning it
	// into one would put pipes through somebody's writing.
	const in = "Dear team,\n\nThe visit went well, and the site is happy.\n\nThanks,\nAmara\n"

	doc, err := Extract(t.Context(), strings.NewReader(in), "letter.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc.Text, "|") {
		t.Errorf("prose was read as a table:\n%s", doc.Text)
	}
}

func TestMarkdownKeepsWhatTheAuthorWrote(t *testing.T) {
	const in = `# Title

First line of a paragraph.
Second line of the same paragraph.

- one
- two

## Section

` + "```go\n// # not a heading\nfunc main() {}\n```\n"

	doc, err := Extract(t.Context(), strings.NewReader(in), "notes.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Text != in {
		t.Errorf("the file was rewritten\n got: %q\nwant: %q", doc.Text, in)
	}
	// A heading inside a fence is a comment in somebody's shell script.
	if len(doc.Headings) != 2 {
		t.Fatalf("got %d headings, want 2: %v", len(doc.Headings), doc.Headings)
	}
	if doc.Title != "Title" {
		t.Errorf("title is %q", doc.Title)
	}
}

func TestMarkdownFrontMatterIsMetadataRatherThanContent(t *testing.T) {
	const in = "---\ntitle: Stated in the metadata\nlayout: default\n---\n\n# Written as a heading\n\nBody.\n"

	doc, err := Extract(t.Context(), strings.NewReader(in), "page.md")
	if err != nil {
		t.Fatal(err)
	}
	// A search result whose first line is "layout: default" is a result
	// nobody clicks.
	if strings.Contains(doc.Text, "layout") {
		t.Errorf("front matter was indexed:\n%s", doc.Text)
	}
	if doc.Title != "Stated in the metadata" {
		t.Errorf("title is %q", doc.Title)
	}
	if !strings.HasPrefix(doc.Text, "# Written as a heading") {
		t.Errorf("the body does not start where it should: %q", doc.Text)
	}
}

func TestSetextHeadingsAreHeadings(t *testing.T) {
	const in = "Title\n=====\n\nSection\n-------\n\nBody.\n"

	doc, err := Extract(t.Context(), strings.NewReader(in), "old.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Headings) != 2 || doc.Headings[0].Level != 1 || doc.Headings[1].Level != 2 {
		t.Fatalf("headings are %v", doc.Headings)
	}
	if strings.Contains(doc.Text, "=====") {
		t.Errorf("the rule under the heading was left in:\n%s", doc.Text)
	}
}

func TestSourceCodeIsIndexedAsItself(t *testing.T) {
	const in = "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"

	doc, err := Extract(t.Context(), strings.NewReader(in), "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Media != "text/x-go" {
		t.Errorf("media is %q", doc.Media)
	}
	if doc.Text != in {
		t.Errorf("the file was rewritten: %q", doc.Text)
	}
}
