package extract

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestZipMediaTellsTheOfficeFormatsApart(t *testing.T) {
	// Every one of these files has the same first four bytes, so a reader that
	// went by the signature would hand a spreadsheet to the Word extractor.
	plain := archiveOf([]part{{"notes.txt", []byte("hello")}})

	for _, c := range []struct {
		why  string
		in   []byte
		want string
	}{
		{"a document is a document", handbookDocx(), mediaWord},
		{"a deck is a deck", reviewPptx(), mediaSlides},
		{"a workbook is a workbook", budgetXlsx(), mediaSheets},
		{"an ordinary archive is an ordinary archive", plain, "application/zip"},
		{"a file that is not a zip is not one", []byte("hello"), ""},
		{
			// A half copied .docx is a broken document rather than an
			// unsupported format, and the difference is whether an operator
			// recopies the file or files a bug about a format that already
			// works.
			"a truncated archive says nothing rather than the wrong thing",
			truncatedDocx(),
			"",
		},
	} {
		if got := zipMedia(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.why, got, c.want)
		}
	}
}

func TestAWordHeadingIsFoundHoweverItsStyleIsSpelled(t *testing.T) {
	// The style name is the only reliable statement of structure in the format,
	// and it arrives spelled whichever way the product that wrote the file
	// spells it.
	for _, c := range []struct {
		style string
		want  int
	}{
		{"Heading1", 1},
		{"heading 1", 1},
		{"HEADING 3", 3},
		{"Heading9", 9},
		{"Title", 0},
		{"ListParagraph", 0},
		{"Heading", 0},
		{"Heading0", 0},
		{"", 0},
	} {
		if got := headingStyle(c.style); got != c.want {
			t.Errorf("style %q is level %d, want %d", c.style, got, c.want)
		}
	}
}

func TestAWordParagraphSplitAcrossRunsIsOneParagraph(t *testing.T) {
	// Word splits a sentence at every change of formatting and at the point
	// somebody stopped typing, and a reader that took each run as a block would
	// index a page of fragments.
	doc, err := Extract(t.Context(), bytes.NewReader(wordOf(
		`<w:p><w:r><w:t xml:space="preserve">Confirm the site contact </w:t></w:r>`+
			`<w:r><w:t xml:space="preserve">the day </w:t></w:r><w:r><w:t>before.</w:t></w:r></w:p>`,
	)), "one.docx")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Text != "Confirm the site contact the day before.\n" {
		t.Errorf("text is %q", doc.Text)
	}
}

func TestAWordDocumentWithNoPropertiesStillHasATitle(t *testing.T) {
	// Plenty of documents carry no core properties at all, and the Title style
	// is what the author actually typed at the top of the page.
	doc, err := Extract(t.Context(), bytes.NewReader(wordOf(
		`<w:p><w:pPr><w:pStyle w:val="Title"/></w:pPr><w:r><w:t>Site visit report</w:t></w:r></w:p>`+
			`<w:p><w:r><w:t>Body.</w:t></w:r></w:p>`,
	)), "report.docx")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "Site visit report" {
		t.Errorf("title is %q", doc.Title)
	}
	if !strings.HasPrefix(doc.Text, "# Site visit report\n") {
		t.Errorf("the title is not a heading in the text: %q", doc.Text)
	}
}

func TestAWordTableGetsAHeaderRow(t *testing.T) {
	// There is no markup in the format that says a row is a header, and the
	// first row is one in very nearly every table anybody made on purpose.
	doc, err := Extract(t.Context(), bytes.NewReader(wordOf(
		`<w:tbl>`+
			`<w:tr><w:tc><w:p><w:r><w:t>Part</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Quantity</w:t></w:r></w:p></w:tc></w:tr>`+
			`<w:tr><w:tc><w:p><w:r><w:t>Battery</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>2</w:t></w:r></w:p></w:tc></w:tr>`+
			`</w:tbl>`,
	)), "parts.docx")
	if err != nil {
		t.Fatal(err)
	}
	want := "| Part | Quantity |\n| --- | --- |\n| Battery | 2 |\n"
	if doc.Text != want {
		t.Errorf("text is %q, want %q", doc.Text, want)
	}
}

func TestSlidesAreReadInTheOrderTheirNumbersSay(t *testing.T) {
	// Slide ten sorts before slide two as text, and a deck whose slides come
	// out in that order is somebody else's document.
	for _, c := range []struct {
		name string
		want int
	}{
		{"ppt/slides/slide1.xml", 1},
		{"ppt/slides/slide10.xml", 10},
		{"ppt/slides/slide2.xml", 2},
		{"ppt/slides/notes.xml", 0},
	} {
		if got := trailingNumber(c.name); got != c.want {
			t.Errorf("%s is number %d, want %d", c.name, got, c.want)
		}
	}

	doc, err := Extract(t.Context(), bytes.NewReader(reviewPptx()), "review.pptx")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Pages != 3 {
		t.Errorf("deck has %d slides, want 3", doc.Pages)
	}
	first := strings.Index(doc.Text, "What went well")
	last := strings.Index(doc.Text, "An untitled closing slide")
	if first < 0 || last < 0 || first > last {
		t.Errorf("slide two did not come out before slide ten:\n%s", doc.Text)
	}
}

func TestAnUntitledSlideStillGetsAHeading(t *testing.T) {
	// The heading is what a chunker splits on and what a citation points at, so
	// a deck of untitled slides that came out as one block would cite as
	// "somewhere in this deck".
	doc, err := Extract(t.Context(), bytes.NewReader(reviewPptx()), "review.pptx")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Headings) != 3 {
		t.Fatalf("got %d headings, want one per slide: %v", len(doc.Headings), doc.Headings)
	}
	if doc.Headings[2].Text != "Slide 3" {
		t.Errorf("the untitled slide is headed %q", doc.Headings[2].Text)
	}
}

func TestAPresentationWithNoSlidesIsCorrupt(t *testing.T) {
	deck := archiveOf([]part{
		{"[Content_Types].xml", contentTypes("")},
		{"ppt/presentation.xml", []byte(`<p:presentation/>`)},
	})

	_, err := Extract(t.Context(), bytes.NewReader(deck), "empty.pptx")
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("error is %v, want %v", err, ErrCorrupt)
	}
}

func TestACellReferenceSaysWhichColumnItIs(t *testing.T) {
	for _, c := range []struct {
		ref  string
		want int
	}{
		{"A1", 0},
		{"B2", 1},
		{"Z9", 25},
		{"AA1", 26},
		{"AB12", 27},
		{"a1", 0},
		{"12", -1},
		{"", -1},
	} {
		if got := columnOf(c.ref); got != c.want {
			t.Errorf("%q is column %d, want %d", c.ref, got, c.want)
		}
	}
}

func TestASheetWithAGapKeepsItsColumnsLinedUp(t *testing.T) {
	// Cells are only written where they hold something. A reader that ignored
	// the reference would shift every value after the gap one column left,
	// which puts a hotel bill in the flights column and is worse than losing
	// the row.
	doc, err := Extract(t.Context(), bytes.NewReader(budgetXlsx()), "budget.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Text, "| Berlin |  | 280 |") {
		t.Errorf("the gap did not survive:\n%s", doc.Text)
	}
}

func TestASheetIsFoundThroughItsRelationshipRatherThanItsPosition(t *testing.T) {
	// A workbook's second sheet is not necessarily sheet2.xml, and a file where
	// somebody deleted a sheet in the middle is exactly where the two diverge.
	doc, err := Extract(t.Context(), bytes.NewReader(budgetXlsx()), "budget.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Travel", "# Equipment", "| Spare battery | 89 |"} {
		if !strings.Contains(doc.Text, want) {
			t.Errorf("text does not contain %q:\n%s", want, doc.Text)
		}
	}
}

func TestASheetWhosePartIsMissingDoesNotLoseTheWorkbook(t *testing.T) {
	book := `<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets><sheet name="Gone" r:id="rId9"/><sheet name="Here" r:id="rId1"/></sheets></workbook>`
	rels := `<Relationships>
<Relationship Id="rId1" Target="worksheets/sheet1.xml"/>
<Relationship Id="rId9" Target="worksheets/deleted.xml"/>
</Relationships>`
	data := archiveOf([]part{
		{"[Content_Types].xml", contentTypes("")},
		{"xl/workbook.xml", []byte(book)},
		{"xl/_rels/workbook.xml.rels", []byte(rels)},
		{"xl/worksheets/sheet1.xml", []byte(`<worksheet><sheetData>
<row r="1"><c r="A1"><v>7</v></c></row></sheetData></worksheet>`)},
	})

	doc, err := Extract(t.Context(), bytes.NewReader(data), "half.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Text, "| 7 |") {
		t.Errorf("the sheet that is there did not come out:\n%s", doc.Text)
	}
}

func TestAFormattedCellIsOneString(t *testing.T) {
	// A cell where one word is bold is stored as several runs, and joining them
	// is the difference between "Field ops" and two terms that match nothing.
	const in = `<sst><si><t>Field </t></si><si><r><t>Field</t></r><r><t> ops</t></r></si></sst>`

	got := sharedStrings([]byte(in))
	want := []string{"Field ", "Field ops"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("string %d is %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTheDecompressionBudgetIsSharedAcrossTheWholeArchive(t *testing.T) {
	// A zip bomb is usually a thousand small entries rather than one enormous
	// one, and a limit applied per entry lets every one of them through.
	const (
		slides = 8
		each   = 200 << 10
	)
	parts := []part{
		{"[Content_Types].xml", contentTypes("")},
		{"ppt/presentation.xml", []byte(`<p:presentation/>`)},
	}
	for i := 1; i <= slides; i++ {
		body := `<p:sld><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>` +
			strings.Repeat("a", each) + `</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`
		parts = append(parts, part{name: "ppt/slides/slide" + string(rune('0'+i)) + ".xml", data: []byte(body)})
	}

	_, err := Extract(t.Context(), bytes.NewReader(archiveOf(parts)), "bomb.pptx",
		WithMaxDecompressed(512<<10), WithMaxOutput(1<<20))
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("error is %v, want %v", err, ErrTooLarge)
	}
}

// wordOf wraps body in the smallest .docx that opens.
func wordOf(body string) []byte {
	document := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>` + body + `</w:body></w:document>`

	return archiveOf([]part{
		{"[Content_Types].xml", contentTypes("")},
		{"word/document.xml", []byte(document)},
	})
}
