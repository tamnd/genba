package extract

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestAFileThatIsNotAPDFIsRefused(t *testing.T) {
	_, err := Extract(t.Context(), strings.NewReader("%PDF is what it claims"), "claim.pdf")
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("error is %v, want %v", err, ErrCorrupt)
	}
}

func TestTextIsKeptAndGlyphCodesAreNot(t *testing.T) {
	// This is the check that keeps mojibake out of the index. A subset font
	// with no mapping draws with codes that count up from one, and reading them
	// as characters produces something that looks like text to every other
	// check.
	for _, c := range []struct {
		why  string
		in   string
		want bool
	}{
		{"ordinary prose", "Visits were up on the quarter.", true},
		{"prose in another script", "Besuche stiegen im Quartal um ein Fünftel.", true},
		{"a line of figures", "| 2026 | 14 | 9 |", true},
		{"glyph codes read as characters", "\x01\x02\x03\x04\x05", false},
		{"private use runes from a subset font", "\ue000\ue001\ue002", false},
		{"nothing at all", "", false},
	} {
		if got := readable(c.in); got != c.want {
			t.Errorf("%s: readable is %v, want %v", c.why, got, c.want)
		}
	}
}

func TestASimpleFontsUpperHalfIsTypographyRatherThanControlCharacters(t *testing.T) {
	// A dash and a pair of curly quotes are one byte each in the file, and a
	// reader that skipped the encoding turns each of them into a control
	// character that the rest of the pipeline then throws away.
	for _, c := range []struct {
		why  string
		in   pstring
		want string
	}{
		{"plain bytes are plain characters", pstring("Berlin"), "Berlin"},
		{"the upper half is punctuation", pstring{'a', 0x96, 'b', 0x93, 'c', 0x94}, "a–b“c”"},
		{"a byte order mark means sixteen bit units", pstring{0xFE, 0xFF, 0x00, 'H', 0x00, 'i'}, "Hi"},
	} {
		if got := decodeText(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.why, got, c.want)
		}
	}
}

func TestAToUnicodeMapIsReadInBothOfItsForms(t *testing.T) {
	const cmap = `/CIDInit /ProcSet findresource begin
begincmap
1 begincodespacerange
<0000> <FFFF>
endcodespacerange
2 beginbfchar
<0003> <0041>
<0004> <0042>
endbfchar
2 beginbfrange
<0010> <0012> <0061>
<0020> <0021> [<0058> <0059>]
endbfrange
endcmap
`

	got := parseCMap([]byte(cmap))
	for code, want := range map[uint32]string{
		0x03: "A",
		0x04: "B",
		0x10: "a", 0x11: "b", 0x12: "c",
		0x20: "X", 0x21: "Y",
	} {
		if got[code] != want {
			t.Errorf("code %#04x maps to %q, want %q", code, got[code], want)
		}
	}
	// The code space is what says a string is pairs of bytes rather than
	// characters, and getting it wrong turns an English document into a page of
	// Chinese.
	if !cmapIsWide([]byte(cmap)) {
		t.Error("a two byte code space was read as one byte")
	}
	if cmapIsWide([]byte("1 beginbfchar\n<41> <0041>\nendbfchar\n")) {
		t.Error("a one byte code space was read as two bytes")
	}
}

func TestAFontDecodesTheCodesItWasGiven(t *testing.T) {
	wide := &font{twoByte: true, toUnicode: map[uint32]string{1: "H", 2: "i"}}
	if got := wide.text(pstring{0x00, 0x01, 0x00, 0x02}); got != "Hi" {
		t.Errorf("two byte codes came out as %q", got)
	}
	// A code the map does not cover is left out rather than guessed at.
	if got := wide.text(pstring{0x00, 0x01, 0x0A, 0x0B}); got != "H" {
		t.Errorf("an unmapped code came out as %q", got)
	}

	simple := &font{toUnicode: map[uint32]string{'q': "quarter"}}
	if got := simple.text(pstring("q1")); got != "quarter1" {
		t.Errorf("single byte codes came out as %q", got)
	}

	// A page can draw before it names a font, and a reader that assumed one
	// would panic on the file rather than read it.
	var none *font
	if got := none.text(pstring("Text")); got != "Text" {
		t.Errorf("text drawn with no font came out as %q", got)
	}
}

func TestSpacingIsTheOnlyEvidenceOfWhereTheParagraphsAre(t *testing.T) {
	// Lines of running text sit one leading apart, and the space around a
	// paragraph break is bigger than that. The comparison is against the page's
	// own most common gap, because a fixed threshold is right for one type size
	// and wrong for every other.
	lines := []textLine{
		{text: "First line.", gap: 0},
		{text: "Still the first paragraph.", gap: 14},
		{text: "And still.", gap: 14.2},
		{text: "A new paragraph.", gap: 28},
		{text: "Its second line.", gap: 14},
	}

	want := "First line.\nStill the first paragraph.\nAnd still.\n\nA new paragraph.\nIts second line."
	if got := assemble(lines); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := commonGap(lines); got != 14 {
		t.Errorf("the page's leading came out as %v, want 14", got)
	}
	if got := assemble(nil); got != "" {
		t.Errorf("an empty page came out as %q", got)
	}
}

func TestPagesComeOutInTheOrderTheTreeSaysRatherThanFileOrder(t *testing.T) {
	// The order objects sit in the file is the order somebody's writer happened
	// to emit them, and on a file saved by an editor it is nothing like reading
	// order.
	data := buildPDF([]pdfObject{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [4 0 R 3 0 R] /Count 2 /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 7 0 R >> >> >>"},
		{body: "<< /Type /Page /Parent 2 0 R /Contents 5 0 R >>"},
		{body: "<< /Type /Page /Parent 2 0 R /Contents 6 0 R >>"},
		{body: "<< /Length %LEN% >>", stream: []byte("BT /F1 11 Tf 72 720 Td " + pdfString("Written second.") + " Tj ET")},
		{body: "<< /Length %LEN% >>", stream: []byte("BT /F1 11 Tf 72 720 Td " + pdfString("Read first.") + " Tj ET")},
		{body: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"},
	})

	doc, err := Extract(t.Context(), bytes.NewReader(data), "edited.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Pages != 2 {
		t.Errorf("document has %d pages, want 2", doc.Pages)
	}
	first := strings.Index(doc.Text, "Read first.")
	second := strings.Index(doc.Text, "Written second.")
	if first < 0 || second < 0 || first > second {
		t.Errorf("the pages came out in file order:\n%s", doc.Text)
	}
}

func TestResourcesAreInheritedFromThePageTree(t *testing.T) {
	// A file that names its fonts once on the root of the tree and never again
	// is ordinary, and a reader that only looked at the page would find no font
	// and read the codes as characters.
	cmap := "1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n" +
		"5 beginbfchar\n<0001> <0053>\n<0002> <0068>\n<0003> <0061>\n<0004> <0072>\n<0005> <0065>\nendbfchar\n"

	data := buildPDF([]pdfObject{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [3 0 R] /Count 1 /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 5 0 R >> >> >>"},
		{body: "<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>"},
		{body: "<< /Length %LEN% >>", stream: []byte("BT /F1 14 Tf 72 720 Td <00010002000300040005> Tj ET")},
		{body: "<< /Type /Font /Subtype /Type0 /BaseFont /AAAAAA+Inter /Encoding /Identity-H /ToUnicode 6 0 R >>"},
		{body: "<< /Length %LEN% >>", stream: []byte(cmap)},
	})

	doc, err := Extract(t.Context(), bytes.NewReader(data), "inherited.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Text, "Share") {
		t.Errorf("the font on the tree root was not used:\n%q", doc.Text)
	}
}

func TestASubsetFontWithNoMapIsNotIndexedAsMojibake(t *testing.T) {
	// The codes in a subset font are the font's own numbering. Without a map
	// there is no way back to characters, and the honest answer is that the
	// page has no text rather than five terms nobody can type.
	data := buildPDF([]pdfObject{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [3 0 R] /Count 1 /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 5 0 R >> >> >>"},
		{body: "<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>"},
		{body: "<< /Length %LEN% >>", stream: []byte("BT /F1 14 Tf 72 720 Td <00010002000300040005> Tj ET")},
		{body: "<< /Type /Font /Subtype /Type0 /BaseFont /AAAAAA+Inter /Encoding /Identity-H >>"},
	})

	doc, err := Extract(t.Context(), bytes.NewReader(data), "unmapped.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Text != "" {
		t.Errorf("glyph codes were indexed as text: %q", doc.Text)
	}
	// The document is still a document, and a connector still has its name, its
	// size and who may read it.
	if doc.Pages != 1 {
		t.Errorf("document has %d pages, want 1", doc.Pages)
	}
}

func TestAScanIsADocumentWithNoTextRatherThanAFailure(t *testing.T) {
	doc, err := Extract(t.Context(), bytes.NewReader(scanPDF()), "scan.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Text != "" {
		t.Errorf("a page with no text on it extracted as %q", doc.Text)
	}
	if doc.Pages != 1 {
		t.Errorf("document has %d pages, want 1", doc.Pages)
	}
}

func TestAnInlineImageDoesNotSwallowTheRestOfThePage(t *testing.T) {
	// The bytes of an inline image sit in the middle of the content stream, and
	// the two that end it turn up inside compressed image data all the time.
	content := "BT /F1 11 Tf 72 720 Td " + pdfString("Before the image.") + " Tj ET\n" +
		"BI /W 4 /H 4 /BPC 8 /CS /G ID xEIx binary EI\n" +
		"BT /F1 11 Tf 72 700 Td " + pdfString("After the image.") + " Tj ET\n"

	data := buildPDF([]pdfObject{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [3 0 R] /Count 1 /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 5 0 R >> >> >>"},
		{body: "<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>"},
		{body: "<< /Length %LEN% >>", stream: []byte(content)},
		{body: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"},
	})

	doc, err := Extract(t.Context(), bytes.NewReader(data), "inline.pdf")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Before the image.", "After the image."} {
		if !strings.Contains(doc.Text, want) {
			t.Errorf("text does not contain %q:\n%s", want, doc.Text)
		}
	}
	if strings.Contains(doc.Text, "binary") {
		t.Errorf("the image data was read as text:\n%s", doc.Text)
	}
}

func TestAPageMaySplitItsContentAcrossStreams(t *testing.T) {
	// A token can be split across the join, so the streams are concatenated the
	// way a viewer concatenates them rather than read one at a time.
	data := buildPDF([]pdfObject{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [3 0 R] /Count 1 /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 6 0 R >> >> >>"},
		{body: "<< /Type /Page /Parent 2 0 R /Contents [4 0 R 5 0 R] >>"},
		{body: "<< /Length %LEN% >>", stream: []byte("BT /F1 11 Tf 72 720 Td " + pdfString("One half of a sentence") + " Tj")},
		{body: "<< /Length %LEN% >>", stream: []byte(pdfString(" and the other half.") + " Tj ET")},
		{body: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"},
	})

	doc, err := Extract(t.Context(), bytes.NewReader(data), "split.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Text, "One half of a sentence and the other half.") {
		t.Errorf("the streams were not joined:\n%q", doc.Text)
	}
}

func TestAPDFWithNoPagesIsCorrupt(t *testing.T) {
	data := buildPDF([]pdfObject{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [] /Count 0 >>"},
	})

	_, err := Extract(t.Context(), bytes.NewReader(data), "blank.pdf")
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("error is %v, want %v", err, ErrCorrupt)
	}
}
