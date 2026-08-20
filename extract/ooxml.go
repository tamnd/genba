package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// The Office formats are all zip archives of XML, and they are told apart by
// which part they contain rather than by anything in their first bytes.
const (
	mediaWord   = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mediaSlides = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	mediaSheets = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// zipMedia reports which Office format an archive holds, or that it is an
// ordinary zip.
//
// Every one of these formats has the same first four bytes, so the only way to
// tell a document from a spreadsheet is to open the archive and look. It is
// cheap: a zip's index is at the end and reading it does not decompress
// anything.
func zipMedia(data []byte) string {
	if len(data) < 4 || string(data[:4]) != "PK\x03\x04" {
		return ""
	}
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		// A truncated Office file lands here, and it is the file the name is
		// worth believing about: a half copied .docx is a broken document
		// rather than an unsupported format, and saying so is the difference
		// between an operator recopying the file and filing a bug about a
		// format that is already supported.
		return ""
	}
	for _, f := range r.File {
		switch f.Name {
		case "word/document.xml":
			return mediaWord
		case "ppt/presentation.xml":
			return mediaSlides
		case "xl/workbook.xml":
			return mediaSheets
		}
	}
	return "application/zip"
}

// quota is the decompression budget for one archive.
//
// It is counted across the whole archive rather than per entry, because a zip
// bomb is usually a thousand small entries rather than one enormous one, and a
// per entry limit lets every one of them through.
type quota struct{ left int64 }

// take reserves n bytes, and fails once the archive has expanded past its
// budget.
func (q *quota) take(n int64) error {
	if n > q.left {
		return fmt.Errorf("%w: archive expands past %d bytes", ErrTooLarge, q.left)
	}
	q.left -= n
	return nil
}

// archive is an opened Office file.
type archive struct {
	r *zip.Reader
	q *quota
}

// openArchive opens the container.
func openArchive(data []byte, o Options) (*archive, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	limit := o.MaxDecompressed
	if limit <= 0 {
		limit = DefaultOptions().MaxDecompressed
	}
	return &archive{r: r, q: &quota{left: limit}}, nil
}

// part reads one entry of the archive, decompressing it under the budget.
//
// The declared size is checked first and the read is limited anyway. A zip
// header is written by whoever made the file, so the number in it is a claim
// rather than a fact, and a bomb is exactly the file that lies about it.
func (a *archive) part(name string) ([]byte, error) {
	for _, f := range a.r.File {
		if f.Name != name {
			continue
		}
		return a.read(f)
	}
	return nil, fmt.Errorf("%w: no %s in archive", ErrCorrupt, name)
}

// read decompresses one entry under the budget.
func (a *archive) read(f *zip.File) ([]byte, error) {
	declared := int64(f.UncompressedSize64)
	if declared > a.q.left {
		return nil, fmt.Errorf("%w: %s declares %d bytes", ErrTooLarge, f.Name, declared)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrCorrupt, f.Name, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, a.q.left+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrCorrupt, f.Name, err)
	}
	if err := a.q.take(int64(len(data))); err != nil {
		return nil, err
	}
	return data, nil
}

// names returns the entries under a prefix, in the order their numbers say
// rather than the order the archive lists them.
//
// Slide ten sorts before slide two everywhere else, and a presentation whose
// slides are indexed out of order reads as somebody else's document.
func (a *archive) names(prefix, suffix string) []string {
	var out []string
	for _, f := range a.r.File {
		if strings.HasPrefix(f.Name, prefix) && strings.HasSuffix(f.Name, suffix) {
			out = append(out, f.Name)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		if n := trailingNumber(a) - trailingNumber(b); n != 0 {
			return n
		}
		return strings.Compare(a, b)
	})
	return out
}

// trailingNumber reads the number out of a name such as slide12.xml.
func trailingNumber(name string) int {
	end := strings.LastIndexByte(name, '.')
	if end < 0 {
		end = len(name)
	}
	start := end
	for start > 0 && name[start-1] >= '0' && name[start-1] <= '9' {
		start--
	}
	n, err := strconv.Atoi(name[start:end])
	if err != nil {
		return 0
	}
	return n
}

// decoder returns an XML decoder that reads what it can.
//
// It is deliberately not strict. These files are written by a dozen products
// and half of them get some corner of the specification wrong, and refusing a
// document because an element closed in the wrong order means losing a
// document a person can open perfectly well. Undefined entities resolve to
// nothing rather than failing for the same reason, and nothing here follows an
// external reference, so a document cannot make the extractor fetch a URL.
func decoder(data []byte) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	return d
}

// nextToken reads the next token, and reports false where there are no more.
//
// The end of a part and a document that stops in the middle of an element are
// the same thing to every reader here. Whatever has been read so far is still a
// document, and half a contract is worth more to the person looking for it than
// an error in a log nobody reads.
func nextToken(d *xml.Decoder) (xml.Token, bool) {
	t, err := d.Token()
	if err != nil {
		return nil, false
	}
	return t, true
}

// Word reads .docx files.
//
// The structure worth keeping is the outline. A Word document says which
// paragraphs are headings, in a style name, and that is the only reliable
// statement of structure in any of these formats: a font size is a guess and a
// numbered paragraph is a list rather than a section.
type Word struct{}

// Media returns the Word type.
func (Word) Media() []string { return []string{mediaWord} }

// Extract reads the document part and its properties.
func (Word) Extract(ctx context.Context, data []byte, o Options) (Doc, error) {
	a, err := openArchive(data, o)
	if err != nil {
		return Doc{}, err
	}
	body, err := a.part("word/document.xml")
	if err != nil {
		return Doc{}, err
	}

	b := newBuilder(o.MaxOutput)
	if props, err := a.part("docProps/core.xml"); err == nil {
		b.setTitle(coreTitle(props))
	}

	w := &wordReader{b: b}
	if err := w.run(ctx, body); err != nil {
		return Doc{}, err
	}
	return b.doc(), nil
}

// wordReader walks the document part once.
type wordReader struct {
	b *builder

	para  strings.Builder
	style string
	list  bool

	cell    strings.Builder
	inCell  bool
	row     []string
	inTable int
	first   bool
}

func (w *wordReader) run(ctx context.Context, data []byte) error {
	d := decoder(data)
	for n := 0; ; n++ {
		if n%2048 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		t, ok := nextToken(d)
		if !ok {
			break
		}
		switch t := t.(type) {
		case xml.StartElement:
			w.start(d, t)
		case xml.EndElement:
			w.end(t.Name.Local)
		}
	}
	w.flush()
	return nil
}

func (w *wordReader) start(d *xml.Decoder, t xml.StartElement) {
	switch t.Name.Local {
	case "t":
		var s string
		if err := d.DecodeElement(&s, &t); err == nil {
			w.write(s)
		}
	case "tab":
		w.write(" ")
	case "br", "cr":
		w.write(" ")
	case "pStyle":
		w.style = attr(t, "val")
	case "numPr":
		w.list = true
	case "tbl":
		w.flush()
		w.inTable++
		w.first = true
	case "tr":
		w.row = w.row[:0]
	case "tc":
		w.inCell = true
		w.cell.Reset()
	}
}

func (w *wordReader) end(name string) {
	switch name {
	case "p":
		w.flush()
	case "tc":
		if w.inCell {
			w.row = append(w.row, w.cell.String())
			w.inCell = false
		}
	case "tr":
		if len(w.row) > 0 {
			w.b.row(w.row)
			if w.first {
				// The first row of a Word table is very nearly always its
				// header, and unlike HTML there is no markup that says so.
				w.b.rule(len(w.row))
				w.first = false
			}
			w.row = w.row[:0]
		}
	case "tbl":
		if w.inTable > 0 {
			w.inTable--
		}
	}
}

// write adds text to whatever is being collected.
func (w *wordReader) write(s string) {
	if w.inCell {
		w.cell.WriteString(s)
		return
	}
	w.para.WriteString(s)
}

// flush writes the paragraph that just ended, as the kind of block its style
// says it is.
func (w *wordReader) flush() {
	text := w.para.String()
	style, list := w.style, w.list
	w.para.Reset()
	w.style, w.list = "", false

	if strings.TrimSpace(text) == "" {
		return
	}
	if w.inCell {
		return
	}
	switch {
	case headingStyle(style) > 0:
		w.b.heading(headingStyle(style), text)
	case strings.EqualFold(style, "Title"):
		w.b.setTitle(text)
		w.b.heading(1, text)
	case list:
		w.b.item(text)
	default:
		w.b.para(text)
	}
}

// headingStyle reads the level out of a Word style name.
//
// Word writes "Heading1" in the file and "heading 1" in some producers, and a
// document translated from another product carries whatever that product
// called it. Matching loosely costs nothing and missing a heading costs the
// outline.
func headingStyle(style string) int {
	s := strings.ToLower(strings.ReplaceAll(style, " ", ""))
	if !strings.HasPrefix(s, "heading") {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "heading"))
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// attr returns one attribute of an element, ignoring its namespace.
func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// coreTitle reads the title out of the core properties part.
func coreTitle(data []byte) string {
	d := decoder(data)
	for {
		t, ok := nextToken(d)
		if !ok {
			return ""
		}
		if s, ok := t.(xml.StartElement); ok && s.Name.Local == "title" {
			var v string
			if err := d.DecodeElement(&v, &s); err == nil {
				return v
			}
			return ""
		}
	}
}

// Slides reads .pptx files.
//
// A deck is a sequence of slides and each slide is a handful of text boxes,
// so the structure is the slide boundary and the title on it. Both are worth
// keeping: a citation into a deck that cannot say which slide is a citation
// nobody can follow.
type Slides struct{}

// Media returns the presentation type.
func (Slides) Media() []string { return []string{mediaSlides} }

// Extract reads every slide in order.
func (Slides) Extract(ctx context.Context, data []byte, o Options) (Doc, error) {
	a, err := openArchive(data, o)
	if err != nil {
		return Doc{}, err
	}
	names := a.names("ppt/slides/slide", ".xml")
	if len(names) == 0 {
		return Doc{}, fmt.Errorf("%w: no slides in presentation", ErrCorrupt)
	}

	b := newBuilder(o.MaxOutput)
	if props, err := a.part("docProps/core.xml"); err == nil {
		b.setTitle(coreTitle(props))
	}

	for i, name := range names {
		if err := ctx.Err(); err != nil {
			return Doc{}, err
		}
		part, err := a.part(name)
		if errors.Is(err, ErrTooLarge) {
			// The budget is the one failure that has to be reported. Every
			// slide after this one will fail the same way, so carrying on
			// would hand back a deck that is missing most of itself and say
			// nothing about it.
			return Doc{}, err
		}
		if err != nil {
			// One unreadable slide is not a reason to lose the deck.
			continue
		}
		slide(b, part, i+1)
	}

	doc := b.doc()
	doc.Pages = len(names)
	return doc, nil
}

// slide writes one slide.
//
// Every slide gets a heading whether or not it had a title, because the
// heading is what a chunker splits on and an anchor points at. A deck of forty
// untitled slides that extracted as one block of text would be one chunk, and
// a citation into it would say "somewhere in this deck".
func slide(b *builder, data []byte, number int) {
	d := decoder(data)
	var (
		texts   []string
		para    strings.Builder
		isTitle bool
		title   string
	)
	flush := func() {
		s := strings.TrimSpace(para.String())
		para.Reset()
		if s == "" {
			return
		}
		if isTitle && title == "" {
			title = s
			return
		}
		texts = append(texts, s)
	}

	for {
		t, ok := nextToken(d)
		if !ok {
			break
		}
		switch t := t.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "ph":
				// The placeholder type is how a slide says which box is its
				// title, and it is stated on the shape rather than on the text.
				if v := attr(t, "type"); v == "title" || v == "ctrTitle" {
					isTitle = true
				}
			case "t":
				var s string
				if err := d.DecodeElement(&s, &t); err == nil {
					para.WriteString(s)
				}
			case "br":
				para.WriteString(" ")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				flush()
			case "sp":
				flush()
				isTitle = false
			}
		}
	}
	flush()

	if title == "" {
		title = "Slide " + strconv.Itoa(number)
	}
	b.heading(2, title)
	for _, s := range texts {
		b.para(s)
	}
}

// Sheets reads .xlsx files.
//
// A spreadsheet is a table and it stays one. Flattening the rows into prose
// would make every number in the file meaningless, because a figure without
// its row and column headings is a figure about nothing.
type Sheets struct{}

// Media returns the spreadsheet type.
func (Sheets) Media() []string { return []string{mediaSheets} }

// Extract reads every sheet in the order the workbook lists them.
func (Sheets) Extract(ctx context.Context, data []byte, o Options) (Doc, error) {
	a, err := openArchive(data, o)
	if err != nil {
		return Doc{}, err
	}
	book, err := a.part("xl/workbook.xml")
	if err != nil {
		return Doc{}, err
	}

	var shared []string
	if part, err := a.part("xl/sharedStrings.xml"); err == nil {
		shared = sharedStrings(part)
	}
	rels := relationships(a)

	b := newBuilder(o.MaxOutput)
	if props, err := a.part("docProps/core.xml"); err == nil {
		b.setTitle(coreTitle(props))
	}

	for _, s := range workbookSheets(book) {
		if err := ctx.Err(); err != nil {
			return Doc{}, err
		}
		target := rels[s.id]
		if target == "" {
			continue
		}
		part, err := a.part("xl/" + strings.TrimPrefix(target, "/xl/"))
		if errors.Is(err, ErrTooLarge) {
			return Doc{}, err
		}
		if err != nil {
			// A sheet whose part is missing is one sheet, and the rest of the
			// workbook is still worth reading.
			continue
		}
		// The sheet name is the only label a cell has above its column
		// heading, and a workbook of twelve monthly sheets is unreadable
		// without it.
		b.heading(1, s.name)
		sheet(b, part, shared)
	}
	return b.doc(), nil
}

// sheetRef is one sheet as the workbook lists it.
type sheetRef struct {
	name string
	id   string
}

// workbookSheets reads the sheet order out of the workbook part.
func workbookSheets(data []byte) []sheetRef {
	var out []sheetRef
	d := decoder(data)
	for {
		t, ok := nextToken(d)
		if !ok {
			break
		}
		s, ok := t.(xml.StartElement)
		if !ok || s.Name.Local != "sheet" {
			continue
		}
		out = append(out, sheetRef{name: attr(s, "name"), id: attr(s, "id")})
	}
	return out
}

// relationships maps a relationship id to the part it names.
//
// The indirection is real and worth following: a workbook's third sheet is not
// necessarily sheet3.xml, and a file where somebody deleted a sheet in the
// middle is exactly where they diverge.
func relationships(a *archive) map[string]string {
	out := make(map[string]string)
	data, err := a.part("xl/_rels/workbook.xml.rels")
	if err != nil {
		return out
	}
	d := decoder(data)
	for {
		t, ok := nextToken(d)
		if !ok {
			break
		}
		s, ok := t.(xml.StartElement)
		if !ok || s.Name.Local != "Relationship" {
			continue
		}
		out[attr(s, "Id")] = attr(s, "Target")
	}
	return out
}

// sharedStrings reads the workbook's string table.
//
// A spreadsheet stores every repeated string once and refers to it by index,
// which is why a sheet read on its own is a page of numbers.
func sharedStrings(data []byte) []string {
	var (
		out  []string
		cur  strings.Builder
		open bool
	)
	d := decoder(data)
	for {
		t, ok := nextToken(d)
		if !ok {
			break
		}
		switch t := t.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				open = true
				cur.Reset()
			case "t":
				var s string
				if err := d.DecodeElement(&s, &t); err == nil && open {
					cur.WriteString(s)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "si" {
				out = append(out, cur.String())
				open = false
			}
		}
	}
	return out
}

// sheet writes one worksheet as a table.
func sheet(b *builder, data []byte, shared []string) {
	d := decoder(data)
	var (
		row    []string
		column int
		typ    string
		first  = true
	)
	for {
		t, ok := nextToken(d)
		if !ok {
			break
		}
		switch t := t.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = row[:0]
			case "c":
				typ = attr(t, "t")
				// Cells are only written where they hold something, so the
				// reference is what says which column this is. Without it a
				// row with a gap in it shifts every value after the gap one
				// column to the left, which is worse than losing the row.
				column = columnOf(attr(t, "r"))
				if column < 0 {
					// A cell with no reference on it is the next one along,
					// which is how the compact writers emit a dense row.
					column = len(row)
				}
				for len(row) < column {
					row = append(row, "")
				}
			case "v", "t":
				var s string
				if err := d.DecodeElement(&s, &t); err != nil {
					break
				}
				if typ == "s" {
					if i, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && i >= 0 && i < len(shared) {
						s = shared[i]
					}
				}
				if len(row) == column {
					row = append(row, s)
				} else if column < len(row) {
					row[column] = s
				}
			}
		case xml.EndElement:
			if t.Name.Local != "row" {
				continue
			}
			b.row(row)
			if first {
				b.rule(len(row))
				first = false
			}
			row = row[:0]
		}
	}
}

// columnOf turns a cell reference such as "AB12" into a zero based column, and
// returns minus one for a cell that carries no reference.
func columnOf(ref string) int {
	n := 0
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		switch {
		case c >= 'A' && c <= 'Z':
			n = n*26 + int(c-'A') + 1
		case c >= 'a' && c <= 'z':
			n = n*26 + int(c-'a') + 1
		default:
			// The row number, which says nothing about the column.
			i = len(ref)
		}
	}
	if n == 0 {
		return -1
	}
	return n - 1
}
