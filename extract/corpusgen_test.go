package extract

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The corpus is generated rather than collected, and the generator lives here
// so that the files under testdata can be rebuilt by anybody.
//
// Collecting real documents would mean checking somebody else's copyrighted
// file into the repository and hoping it keeps saying what the test says it
// says. Generating them means the expected text is written down next to the
// bytes that are supposed to produce it, and a reviewer can see both. What is
// generated is what real producers emit, down to the parts that make these
// formats awkward: a Word paragraph split across runs, a spreadsheet whose
// strings live in a shared table and whose second sheet is not sheet2.xml, a
// slide whose title is marked by a placeholder rather than by position, and a
// PDF whose glyph codes mean nothing without the font's own mapping.
//
// Regenerate with:
//
//	go test ./extract -run TestGenerateCorpus -update
//
// The files are committed, so the test set is bytes on disk rather than code
// that runs at test time. A change to the generator that changes the corpus
// shows up as a diff in the pull request, which is the point.

// corpusTime is the timestamp every generated archive carries, because a zip
// records modification times and a corpus that changes every time it is
// rebuilt is not a corpus.
var corpusTime = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

// TestGenerateCorpus writes the corpus files. It does nothing without -update.
func TestGenerateCorpus(t *testing.T) {
	if !*update {
		t.Skip("run with -update to rebuild the corpus")
	}
	for dir, files := range map[string]map[string][]byte{
		corpusDir:  corpusFiles(),
		hostileDir: hostileFiles(),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, data := range files {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s (%d bytes)", path, len(data))
		}
	}
}

// corpusFiles are the well formed documents.
func corpusFiles() map[string][]byte {
	return map[string][]byte{
		"handbook.md":   []byte(handbookMarkdown),
		"notes.txt":     []byte(notesText),
		"people.csv":    []byte(peopleCSV),
		"report.html":   []byte(reportHTML),
		"handbook.docx": handbookDocx(),
		"review.pptx":   reviewPptx(),
		"budget.xlsx":   budgetXlsx(),
		"memo.pdf":      memoPDF(),
		"subset.pdf":    subsetPDF(),
		"scan.pdf":      scanPDF(),
	}
}

// hostileFiles are the files built to break a reader.
func hostileFiles() map[string][]byte {
	return map[string][]byte{
		"bomb.docx":      bombDocx(),
		"truncated.docx": truncatedDocx(),
		"malformed.docx": malformedDocx(),
		"empty.docx":     emptyDocx(),
		"bomb.pdf":       bombPDF(),
		"garbage.pdf":    garbagePDF(),
		"cycle.pdf":      cyclePDF(),
	}
}

const handbookMarkdown = `---
title: Field engineering handbook
owner: field-ops
---

# Field engineering handbook

Everything a visiting engineer needs, in the order they will need it.

## Before the visit

Confirm the site contact the day before.
Sites move people around and the name on the ticket is often three months old.

- Charge both batteries.
- Print the site pass.

## On site

Sign in at reception even when the door is open.

    ssh field@edge-07

Report the visit before you leave.
`

const notesText = `Standup notes, 12 March

Ingest is behind by about four hours on the largest tenant.
The cause is the permission refresh, which walks every folder even when nothing changed.

Actions:
Look at the folder cursor before Thursday.
Ask the platform team whether the rate limit can go up for one night.
`

const peopleCSV = `name,team,site
Amara Nwosu,Field ops,Manchester
Jonas Weber,Platform,Berlin
Priya Raman,Field ops,Bengaluru
`

const reportHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Quarterly field report</title>
<script>var tracking = {id: "should not be indexed"};</script>
<style>body { font-family: sans-serif }</style>
</head>
<body>
<nav><a href="/">Home</a></nav>
<h1>Quarterly field report</h1>
<p>Visits were up on the quarter, and the &amp; in this sentence is an entity.</p>
<h2>Sites visited</h2>
<table>
<tr><th>Site</th><th>Visits</th></tr>
<tr><td>Manchester</td><td>14</td></tr>
<tr><td>Berlin</td><td>9</td></tr>
</table>
<h2>Open issues</h2>
<ul>
<li>Edge 07 still reports a failed disk.</li>
<li>The Bengaluru pass system needs a new contact.</li>
</ul>
<pre><code>journalctl -u genbad --since yesterday</code></pre>
<p>Filed by <b>field</b>-<i>ops</i>.</p>
</body>
</html>
`

// part is one entry of a generated archive.
type part struct {
	name string
	data []byte
}

// archiveOf builds a zip with a fixed timestamp, so that regenerating the
// corpus twice produces the same bytes.
func archiveOf(parts []part) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, p := range parts {
		h := &zip.FileHeader{Name: p.name, Method: zip.Deflate}
		h.Modified = corpusTime
		f, err := w.CreateHeader(h)
		if err != nil {
			panic(err)
		}
		if _, err := f.Write(p.data); err != nil {
			panic(err)
		}
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// contentTypes is the part every Office file carries, included so the corpus
// files open in a real application as well as in this package.
func contentTypes(extras string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
` + extras + `
</Types>`)
}

func coreProps(title string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties
 xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties"
 xmlns:dc="http://purl.org/dc/elements/1.1/">
<dc:title>` + title + `</dc:title>
<dc:creator>field-ops</dc:creator>
</cp:coreProperties>`)
}

// handbookDocx is the same handbook as the Markdown file, written the way Word
// writes it.
func handbookDocx() []byte {
	body := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>
<w:p><w:pPr><w:pStyle w:val="Title"/></w:pPr><w:r><w:t>Field engineering handbook</w:t></w:r></w:p>
<w:p><w:r><w:t>Everything a visiting engineer needs, in the order they will need it.</w:t></w:r></w:p>
<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Before the visit</w:t></w:r></w:p>
<w:p><w:r><w:t xml:space="preserve">Confirm the site contact </w:t></w:r><w:r><w:t xml:space="preserve">the day </w:t></w:r><w:r><w:t>before.</w:t></w:r></w:p>
<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:t>Charge both batteries.</w:t></w:r></w:p>
<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:t>Print the site pass.</w:t></w:r></w:p>
<w:p><w:pPr><w:pStyle w:val="Heading2"/></w:pPr><w:r><w:t>Parts to carry</w:t></w:r></w:p>
<w:tbl>
<w:tr><w:tc><w:p><w:r><w:t>Part</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Quantity</w:t></w:r></w:p></w:tc></w:tr>
<w:tr><w:tc><w:p><w:r><w:t>Battery</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>2</w:t></w:r></w:p></w:tc></w:tr>
<w:tr><w:tc><w:p><w:r><w:t>Site pass</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>1</w:t></w:r></w:p></w:tc></w:tr>
</w:tbl>
<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>On site</w:t></w:r></w:p>
<w:p><w:r><w:t>Sign in at reception even when the door is open.</w:t></w:r><w:r><w:br/></w:r><w:r><w:t>Report the visit before you leave.</w:t></w:r></w:p>
</w:body>
</w:document>`

	return archiveOf([]part{
		{"[Content_Types].xml", contentTypes(`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>`)},
		{"_rels/.rels", packageRels("word/document.xml", "officeDocument")},
		{"docProps/core.xml", coreProps("Field engineering handbook")},
		{"word/document.xml", []byte(body)},
	})
}

func packageRels(target, kind string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/` + kind + `" Target="` + target + `"/>
</Relationships>`)
}

// reviewPptx is a three slide deck, with one slide left untitled on purpose.
func reviewPptx() []byte {
	slide := func(title string, body ...string) []byte {
		var sb strings.Builder
		sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"
 xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
<p:cSld><p:spTree>`)
		if title != "" {
			sb.WriteString(`<p:sp><p:nvSpPr><p:nvPr><p:ph type="title"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:r><a:t>` +
				title + `</a:t></a:r></a:p></p:txBody></p:sp>`)
		}
		sb.WriteString(`<p:sp><p:nvSpPr><p:nvPr><p:ph type="body" idx="1"/></p:nvPr></p:nvSpPr><p:txBody>`)
		for _, line := range body {
			sb.WriteString(`<a:p><a:r><a:t>` + line + `</a:t></a:r></a:p>`)
		}
		sb.WriteString(`</p:txBody></p:sp></p:spTree></p:cSld></p:sld>`)
		return []byte(sb.String())
	}

	presentation := []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
<p:sldIdLst><p:sldId id="256"/><p:sldId id="257"/><p:sldId id="258"/></p:sldIdLst>
</p:presentation>`)

	return archiveOf([]part{
		{"[Content_Types].xml", contentTypes(`<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>`)},
		{"_rels/.rels", packageRels("ppt/presentation.xml", "officeDocument")},
		{"docProps/core.xml", coreProps("Quarter in review")},
		{"ppt/presentation.xml", presentation},
		{"ppt/slides/slide1.xml", slide("Quarter in review", "Field operations", "March")},
		{"ppt/slides/slide2.xml", slide("What went well", "Visits up by a fifth", "Two new sites onboarded")},
		// Slide ten is here to catch a reader that sorts slide names as text,
		// which puts it before slide two and rewrites the deck.
		{"ppt/slides/slide10.xml", slide("", "An untitled closing slide")},
	})
}

// budgetXlsx has two sheets, strings in the shared table, a gap in a row and a
// second sheet whose part is not the one its position suggests.
func budgetXlsx() []byte {
	book := []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"
 xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets>
<sheet name="Travel" sheetId="1" r:id="rId1"/>
<sheet name="Equipment" sheetId="2" r:id="rId3"/>
</sheets>
</workbook>`)

	rels := []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet4.xml"/>
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/>
</Relationships>`)

	table := []string{"Site", "Flights", "Hotel", "Manchester", "Berlin", "Item", "Cost", "Spare battery", "Cable kit"}
	var shared bytes.Buffer
	shared.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="` +
		strconv.Itoa(len(table)) + `" uniqueCount="` + strconv.Itoa(len(table)) + `">`)
	for _, s := range table {
		shared.WriteString("<si><t>" + s + "</t></si>")
	}
	shared.WriteString("</sst>")

	travel := []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>
<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c></row>
<row r="2"><c r="A2" t="s"><v>3</v></c><c r="B2"><v>420</v></c><c r="C2"><v>310</v></c></row>
<row r="3"><c r="A3" t="s"><v>4</v></c><c r="C3"><v>280</v></c></row>
</sheetData></worksheet>`)

	equipment := []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>
<row r="1"><c r="A1" t="s"><v>5</v></c><c r="B1" t="s"><v>6</v></c></row>
<row r="2"><c r="A2" t="s"><v>7</v></c><c r="B2"><v>89</v></c></row>
<row r="3"><c r="A3" t="s"><v>8</v></c><c r="B3"><v>24</v></c></row>
</sheetData></worksheet>`)

	return archiveOf([]part{
		{"[Content_Types].xml", contentTypes(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)},
		{"_rels/.rels", packageRels("xl/workbook.xml", "officeDocument")},
		{"docProps/core.xml", coreProps("Field budget")},
		{"xl/workbook.xml", book},
		{"xl/_rels/workbook.xml.rels", rels},
		{"xl/sharedStrings.xml", shared.Bytes()},
		{"xl/worksheets/sheet1.xml", travel},
		{"xl/worksheets/sheet4.xml", equipment},
	})
}

// pdfObject is one object of a generated PDF. A %LEN% in the body is replaced
// by the length of the stream, which is what a real writer fills in once it
// knows how long the compressed content turned out to be.
type pdfObject struct {
	body   string
	stream []byte
}

// buildPDF assembles a file with a correct cross reference table.
//
// The table is written even though the reader here ignores it, because a
// corpus file that only this package can open is not a test of anything.
func buildPDF(objects []pdfObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")

	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = buf.Len()
		body := strings.ReplaceAll(obj.body, "%LEN%", strconv.Itoa(len(obj.stream)))
		fmt.Fprintf(&buf, "%d 0 obj\n%s\n", i+1, body)
		if obj.stream != nil {
			buf.WriteString("stream\n")
			buf.Write(obj.stream)
			buf.WriteString("\nendstream\n")
		}
		buf.WriteString("endobj\n")
	}

	start := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets[1:] {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, len(objects), start)
	return buf.Bytes()
}

// pdfString writes a literal string the way a simple font expects it: one byte
// per character in the encoding the font declares, with the three characters
// that mean something to the parser escaped.
//
// This is the detail that makes the corpus worth having. A dash and a pair of
// curly quotes are one byte each in the file and three bytes each in the
// source of this test, and a reader that skips the encoding step turns them
// into three control characters apiece.
func pdfString(s string) string {
	var b strings.Builder
	b.WriteByte('(')
	for _, r := range s {
		c, ok := winAnsiByte(r)
		if !ok {
			continue
		}
		switch c {
		case '\\', '(', ')':
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte(')')
	return b.String()
}

// winAnsiByte encodes one character, using the inverse of the table the reader
// decodes with.
func winAnsiByte(r rune) (byte, bool) {
	if r < 0x80 {
		return byte(r), true
	}
	for c, mapped := range winAnsi {
		if mapped == r {
			return c, true
		}
	}
	if r < 0x100 {
		return byte(r), true
	}
	return 0, false
}

// memoPDF is an ordinary text PDF: one simple font, two pages, and a
// compressed content stream on the second page because real writers compress.
func memoPDF() []byte {
	page1 := "BT\n/F1 18 Tf\n72 720 Td\n" + pdfString("Quarterly memo") + " Tj\n" +
		"/F1 11 Tf\n0 -28 Td\n" + pdfString("Visits were up on the quarter across every site.") + " Tj\n" +
		"0 -16 Td\n" + pdfString("Manchester ran fourteen visits and Berlin ran nine.") + " Tj\n" +
		"0 -16 Td\n" + pdfString("The typographic dash and quotes read as – and “this”.") + " Tj\nET\n"

	page2 := "BT\n/F1 11 Tf\n72 720 Td\n" + pdfString("Open issues carried into next quarter:") + " Tj\n" +
		"0 -16 Td\n[" + pdfString("Edge 07") + " -400 " + pdfString("still reports a failed disk.") + "] TJ\n" +
		"0 -16 Td\n" + pdfString("The Bengaluru pass system needs a new contact.") + " Tj\nET\n"

	return buildPDF([]pdfObject{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 7 0 R >> >> >>"},
		{body: "<< /Type /Page /Parent 2 0 R /Contents 5 0 R >>"},
		{body: "<< /Type /Page /Parent 2 0 R /Contents 6 0 R >>"},
		{body: "<< /Length %LEN% >>", stream: []byte(page1)},
		{body: "<< /Length %LEN% /Filter /FlateDecode >>", stream: deflated([]byte(page2))},
		{body: "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"},
		{body: "<< /Producer " + pdfString("genba corpus generator") + " /Title " + pdfString("Quarterly memo") + " >>"},
	})
}

// subsetPDF is the case that separates a real reader from a hopeful one: an
// embedded subset font whose glyph codes are its own numbering, with a
// ToUnicode map that says what they stand for.
func subsetPDF() []byte {
	lines := []string{
		"Embedded subset fonts",
		"The codes in this file are not characters.",
		"Only the ToUnicode map says what they are.",
	}
	codes, cmap := subsetFont(lines)

	var content strings.Builder
	content.WriteString("BT\n/F1 14 Tf\n72 720 Td\n")
	for i, line := range lines {
		if i > 0 {
			content.WriteString("0 -20 Td\n")
		}
		content.WriteString("<" + codes[line] + "> Tj\n")
	}
	content.WriteString("ET\n")

	return buildPDF([]pdfObject{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [3 0 R] /Count 1 /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 5 0 R >> >> >>"},
		{body: "<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>"},
		{body: "<< /Length %LEN% >>", stream: []byte(content.String())},
		{body: "<< /Type /Font /Subtype /Type0 /BaseFont /AAAAAA+Inter /Encoding /Identity-H " +
			"/DescendantFonts [7 0 R] /ToUnicode 6 0 R >>"},
		{body: "<< /Length %LEN% >>", stream: []byte(cmap)},
		{body: "<< /Type /Font /Subtype /CIDFontType2 /BaseFont /AAAAAA+Inter >>"},
		{body: "<< /Producer " + pdfString("genba corpus generator") + " /Title " + pdfString("Embedded subset fonts") + " >>"},
	})
}

// subsetFont assigns a glyph code to every character used and writes the
// mapping back, which is what a real subsetter does.
func subsetFont(lines []string) (codes map[string]string, cmap string) {
	assigned := map[rune]int{}
	var order []rune
	for _, line := range lines {
		for _, r := range line {
			if _, ok := assigned[r]; ok {
				continue
			}
			// Codes start at one, and they are assigned in the order the
			// characters were first used, which is why they cannot be guessed
			// from the character.
			assigned[r] = len(assigned) + 1
			order = append(order, r)
		}
	}

	codes = make(map[string]string, len(lines))
	for _, line := range lines {
		var hex strings.Builder
		for _, r := range line {
			fmt.Fprintf(&hex, "%04X", assigned[r])
		}
		codes[line] = hex.String()
	}

	var b strings.Builder
	b.WriteString("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n")
	b.WriteString("/CMapName /Adobe-Identity-UCS def\n/CMapType 2 def\n")
	b.WriteString("1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")
	fmt.Fprintf(&b, "%d beginbfchar\n", len(order))
	for _, r := range order {
		fmt.Fprintf(&b, "<%04X> <%04X>\n", assigned[r], r)
	}
	b.WriteString("endbfchar\nendcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	return codes, b.String()
}

// scanPDF is a page with an image on it and no text, which is what a scanned
// document is. There is no OCR here, so it extracts as nothing, and that is
// the behaviour worth pinning down.
func scanPDF() []byte {
	content := "q\n612 0 0 792 0 0 cm\n/Im0 Do\nQ\n"
	// A one pixel image is enough. What matters is that the page draws
	// something and draws no text.
	pixel := []byte{0x00, 0x00, 0x00}

	return buildPDF([]pdfObject{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [3 0 R] /Count 1 /MediaBox [0 0 612 792] " +
			"/Resources << /XObject << /Im0 5 0 R >> >> >>"},
		{body: "<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>"},
		{body: "<< /Length %LEN% >>", stream: []byte(content)},
		{body: "<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceRGB " +
			"/BitsPerComponent 8 /Length %LEN% >>", stream: pixel},
		{body: "<< /Producer " + pdfString("genba corpus generator") + " >>"},
	})
}

// deflated compresses with the framing PDF calls FlateDecode.
func deflated(data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// bombSize is how far the hostile files expand. It is well over the budget the
// tests run under and well under anything that would trouble the machine
// generating them.
const bombSize = 64 << 20

// bombDocx is a valid document whose one part expands to far more than the
// budget allows.
func bombDocx() []byte {
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	line := `<w:p><w:r><w:t>aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa</w:t></w:r></w:p>`
	for body.Len() < bombSize {
		body.WriteString(line)
	}
	body.WriteString(`</w:body></w:document>`)

	return archiveOf([]part{
		{"[Content_Types].xml", contentTypes("")},
		{"word/document.xml", body.Bytes()},
	})
}

// truncatedDocx is a document that stops half way through, which is what a
// file copied off a share during a write looks like.
func truncatedDocx() []byte {
	full := handbookDocx()
	return full[:len(full)*2/3]
}

// malformedDocx is a document whose XML does not close.
func malformedDocx() []byte {
	body := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>
<w:p><w:r><w:t>The first paragraph survives.</w:t></w:r></w:p>
<w:p><w:r><w:t>The second one does not &notanentity; <w:tbl><w:tr>`

	return archiveOf([]part{
		{"[Content_Types].xml", contentTypes("")},
		{"word/document.xml", []byte(body)},
	})
}

// emptyDocx is an archive that says it is a document and holds no document.
func emptyDocx() []byte {
	return archiveOf([]part{
		{"[Content_Types].xml", contentTypes("")},
		{"word/document.xml", nil},
		{"docProps/core.xml", coreProps("Nothing here")},
	})
}

// bombPDF is a content stream that decompresses to far more than the budget.
func bombPDF() []byte {
	content := bytes.Repeat([]byte("BT /F1 11 Tf 72 720 Td (a) Tj ET\n"), bombSize/33)
	return buildPDF([]pdfObject{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [3 0 R] /Count 1 /MediaBox [0 0 612 792] >>"},
		{body: "<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>"},
		{body: "<< /Length %LEN% /Filter /FlateDecode >>", stream: deflated(content)},
		{body: "<< /Producer " + pdfString("genba corpus generator") + " >>"},
	})
}

// garbagePDF has the header and nothing else that holds up, which is what a
// download that failed half way through leaves behind.
func garbagePDF() []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	// A fixed sequence rather than a random one, because a corpus file that
	// differs between two machines is not a corpus file.
	state := uint32(12345)
	for i := 0; i < 8192; i++ {
		state = state*1664525 + 1013904223
		buf.WriteByte(byte(state >> 24))
	}
	buf.WriteString("\ntrailer\n<< /Root 9 0 R >>\n%%EOF\n")
	return buf.Bytes()
}

// cyclePDF is a page tree that contains itself, which no writer produces on
// purpose and which a walk with no depth limit follows forever.
func cyclePDF() []byte {
	return buildPDF([]pdfObject{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"},
		{body: "<< /Type /Pages /Kids [2 0 R 4 0 R] /Count 1 >>"},
		{body: "<< /Type /Page /Parent 3 0 R /Contents 5 0 R >>"},
		{body: "<< /Length %LEN% >>", stream: []byte("BT /F1 11 Tf 72 720 Td (Round and round.) Tj ET\n")},
		{body: "<< /Producer " + pdfString("genba corpus generator") + " >>"},
	})
}
