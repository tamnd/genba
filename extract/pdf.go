package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"
)

// PDF reads PDF files, which are the format people actually send each other
// and the format that says least about what it contains.
//
// A PDF has no headings, no paragraphs and no sentences. It has a page, and on
// that page there are instructions to draw a run of glyphs at a coordinate,
// which is why the same document can look identical to two readers and extract
// as two different things. What comes out here is the text in the order the
// page draws it, split into lines where the drawing moves down the page, with
// one blank line between pages.
//
// # What it does not do
//
// There is no optical character recognition, so a scan extracts as nothing.
// That is the honest answer: a scanned page contains no text and inventing
// some would put a guess in the index with no way to tell it from a fact.
//
// A document whose glyphs cannot be mapped back to characters extracts as
// nothing too, rather than as the mojibake that a naive reader produces. See
// [PDF.Extract].
type PDF struct{}

// Media returns the PDF type.
func (PDF) Media() []string { return []string{"application/pdf"} }

// Extract reads the text of every page in order.
//
// Text comes out through the font's ToUnicode mapping where the file has one,
// which is what makes an embedded subset font readable: the glyph codes in the
// content stream are the font's own numbering and mean nothing without it.
// Where a font has no such mapping the codes are read as characters directly,
// which is right for the ordinary encodings and wrong for a subset font, so
// the result is checked before it is returned: text that is mostly not
// characters anybody typed is dropped rather than indexed.
func (PDF) Extract(ctx context.Context, data []byte, o Options) (Doc, error) {
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return Doc{}, fmt.Errorf("%w: no PDF header", ErrCorrupt)
	}
	limit := o.MaxDecompressed
	if limit <= 0 {
		limit = DefaultOptions().MaxDecompressed
	}
	doc := parseDocument(data, &quota{left: limit})

	pages := doc.pages()
	if len(pages) == 0 {
		return Doc{}, fmt.Errorf("%w: no pages", ErrCorrupt)
	}

	b := newBuilder(o.MaxOutput)
	if title := doc.title(); title != "" {
		b.setTitle(title)
	}

	extracted := 0
	for _, page := range pages {
		if err := ctx.Err(); err != nil {
			return Doc{}, err
		}
		text, err := doc.pageText(page)
		if err != nil {
			// The only error a page returns is the budget, and a document that
			// is over it is refused whole. Skipping the page instead would put
			// a fraction of a bomb in the index and call it a document.
			return Doc{}, err
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if !readable(text) {
			// The glyphs came out as codes rather than characters, which is
			// what an embedded subset font with no mapping produces. Indexing
			// it would fill the index with terms nobody can type and snippets
			// nobody can read.
			continue
		}
		extracted++
		for _, para := range strings.Split(text, "\n\n") {
			b.para(para)
		}
	}

	out := b.doc()
	out.Pages = len(pages)
	if extracted == 0 {
		// A document with no extractable text is not an error. A scan is a
		// real document and a connector still has its name, its size and who
		// may read it, all of which are worth indexing.
		out.Text = ""
	}
	return out, nil
}

// pages returns the page dictionaries in reading order.
//
// The order comes from walking the page tree from the catalogue, because the
// order objects appear in the file is the order somebody's writer happened to
// emit them and bears no relation to the order they are read in. A file whose
// tree cannot be walked falls back to object order, which is at least
// deterministic.
func (d *document) pages() []pdict {
	var out []pdict
	if root, ok := d.catalog(); ok {
		if node, ok := d.dict(root["Pages"]); ok {
			d.walkPages(node, pdict{}, &out, 0)
		}
	}
	if len(out) > 0 {
		return out
	}

	// The fallback is object number order, which is not reading order but is at
	// least the order somebody wrote the file in, and is the same on every run.
	for _, num := range d.numbers() {
		if dict, ok := d.objects[num].value.(pdict); ok && dict["Type"] == pname("Page") {
			out = append(out, dict)
		}
	}
	return out
}

// catalog finds the document catalogue, which is the root of the page tree.
//
// It is found by looking for it rather than by following the trailer, because a
// file that has been edited and saved has several trailers and a file that has
// been repaired has one that points at nothing. The search is in object number
// order so that a file with two catalogues, which incremental updates produce,
// extracts the same way every time.
func (d *document) catalog() (pdict, bool) {
	for _, num := range d.numbers() {
		if dict, ok := d.objects[num].value.(pdict); ok && dict["Type"] == pname("Catalog") {
			return dict, true
		}
	}
	return nil, false
}

// numbers returns every object number in the file, in order.
func (d *document) numbers() []int {
	nums := make([]int, 0, len(d.objects))
	for num := range d.objects {
		nums = append(nums, num)
	}
	slices.Sort(nums)
	return nums
}

// inheritable are the entries a page tree node passes down to its children.
//
// Resources is the one that matters. A file that sets it once on the root and
// never again is ordinary, and a reader that only looks at the page finds no
// fonts and produces no text.
var inheritable = []pname{"Resources", "MediaBox", "CropBox", "Rotate"}

// walkPages walks the page tree, carrying down what is inherited.
func (d *document) walkPages(node, inherited pdict, out *[]pdict, depth int) {
	if depth > 64 || len(*out) > 100_000 {
		// A tree deeper than this is a file built to be walked forever rather
		// than a document anybody wrote.
		return
	}

	merged := pdict{}
	for k, v := range inherited {
		merged[k] = v
	}
	for _, k := range inheritable {
		if v, ok := node[k]; ok {
			merged[k] = v
		}
	}

	if node["Type"] == pname("Page") {
		page := pdict{}
		for k, v := range merged {
			page[k] = v
		}
		for k, v := range node {
			page[k] = v
		}
		*out = append(*out, page)
		return
	}

	kids, ok := d.resolve(node["Kids"]).(parray)
	if !ok {
		return
	}
	for _, kid := range kids {
		child, ok := d.dict(kid)
		if !ok {
			continue
		}
		d.walkPages(child, merged, out, depth+1)
	}
}

// title returns the document's title from its information dictionary.
func (d *document) title() string {
	for _, num := range d.numbers() {
		dict, ok := d.objects[num].value.(pdict)
		if !ok {
			continue
		}
		// The information dictionary has no Type, so it is recognised by
		// holding the entries only it holds.
		if _, has := dict["Producer"]; !has {
			if _, has := dict["Creator"]; !has {
				continue
			}
		}
		if s, ok := d.resolve(dict["Title"]).(pstring); ok {
			return decodeText(s)
		}
	}
	return ""
}

// pageText returns the text of one page.
func (d *document) pageText(page pdict) (string, error) {
	content, err := d.contents(page)
	if err != nil {
		return "", err
	}
	if len(content) == 0 {
		return "", nil
	}
	return d.render(content, d.fonts(page)), nil
}

// contents returns a page's content streams, joined.
//
// A page may split its content across several streams, and a token can be
// split across the join, so they are concatenated with a newline between them
// exactly as a viewer does.
func (d *document) contents(page pdict) ([]byte, error) {
	var out []byte
	var err error
	switch t := d.resolve(page["Contents"]).(type) {
	case parray:
		for _, v := range t {
			out, err = appendStream(d, out, v)
			if err != nil {
				return nil, err
			}
		}
	default:
		out, err = appendStream(d, out, page["Contents"])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// appendStream adds one referenced stream to a buffer.
//
// A stream that will not decode is skipped, because one unreadable stream on a
// page of five is a paragraph missing rather than a document lost. A stream
// that runs the file past its budget is not skipped, because that is a
// statement about the whole file.
func appendStream(d *document, out []byte, v any) ([]byte, error) {
	ref, ok := v.(pref)
	if !ok {
		return out, nil
	}
	data, err := d.stream(ref.num)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return nil, err
		}
		// Everything else is one stream this reader could not decode, and the
		// page keeps the streams it could.
		data = nil
	}
	if len(data) == 0 {
		return out, nil
	}
	if len(out) > 0 {
		out = append(out, '\n')
	}
	return append(out, data...), nil
}

// font is what one font tells a reader about the codes it draws with.
type font struct {
	// toUnicode maps a character code to the text it stands for, and is nil
	// for a font that carries no mapping.
	toUnicode map[uint32]string

	// twoByte says codes in a string are two bytes each, which is what an
	// Identity encoded composite font uses and what makes a naive reader
	// produce a page of Chinese for an English document.
	twoByte bool
}

// fonts reads a page's font resources.
func (d *document) fonts(page pdict) map[pname]*font {
	out := map[pname]*font{}
	res, ok := d.dict(page["Resources"])
	if !ok {
		return out
	}
	dict, ok := d.dict(res["Font"])
	if !ok {
		return out
	}
	for name, v := range dict {
		f, ok := d.dict(v)
		if !ok {
			continue
		}
		out[name] = d.font(f)
	}
	return out
}

// font reads one font dictionary.
func (d *document) font(dict pdict) *font {
	f := &font{}
	if enc, ok := d.resolve(dict["Encoding"]).(pname); ok {
		f.twoByte = strings.HasPrefix(string(enc), "Identity")
	}
	if dict["Subtype"] == pname("Type0") {
		// A composite font addresses glyphs by a code that is nearly always
		// two bytes, and the exceptions are rare enough that assuming is
		// better than producing nothing.
		f.twoByte = true
	}
	if ref, ok := dict["ToUnicode"].(pref); ok {
		if data, err := d.stream(ref.num); err == nil {
			f.toUnicode = parseCMap(data)
			if f.toUnicode != nil {
				f.twoByte = f.twoByte || cmapIsWide(data)
			}
		}
	}
	return f
}

// textLine is one line of a page and how far down the page it sat from the
// line before it.
type textLine struct {
	text string
	gap  float64
}

// render walks a content stream and returns the text it draws.
func (d *document) render(content []byte, fonts map[pname]*font) string {
	var (
		lines   []textLine
		operand []any
		current *font
		lastY   = math.NaN()
		gap     float64
		line    strings.Builder
	)

	flush := func() {
		if line.Len() == 0 {
			return
		}
		lines = append(lines, textLine{text: line.String(), gap: gap})
		line.Reset()
	}
	// moved is called when the text position changes, and decides whether that
	// was a new line or a jump along the same one. A page draws a paragraph as
	// a series of positioned runs, so this is the only signal there is about
	// where the lines were.
	moved := func(y float64) {
		if math.IsNaN(lastY) {
			lastY = y
			return
		}
		if math.Abs(y-lastY) > 0.5 {
			flush()
			gap = math.Abs(y - lastY)
		}
		lastY = y
	}
	show := func(v any) {
		s, ok := v.(pstring)
		if !ok {
			return
		}
		line.WriteString(current.text(s))
	}

	p := &parser{data: content}
	for p.i < len(p.data) {
		before := p.i
		v := p.object()
		if p.i == before {
			p.i++
			continue
		}
		op, ok := v.(operator)
		if !ok {
			// Operands accumulate until an operator consumes them, and a
			// stream that pushes without ever operating is capped so that it
			// cannot be a way to allocate without bound.
			if len(operand) < 64 {
				operand = append(operand, v)
			}
			continue
		}

		switch op {
		case "Tf":
			if len(operand) >= 2 {
				if name, ok := operand[len(operand)-2].(pname); ok {
					current = fonts[name]
				}
			}
		case "Tj":
			if len(operand) >= 1 {
				show(operand[len(operand)-1])
			}
		case "'", "\"":
			flush()
			if len(operand) >= 1 {
				show(operand[len(operand)-1])
			}
		case "TJ":
			if len(operand) >= 1 {
				if arr, ok := operand[len(operand)-1].(parray); ok {
					for _, item := range arr {
						switch t := item.(type) {
						case pstring:
							line.WriteString(current.text(t))
						case float64:
							// A large negative adjustment is the space
							// between two words that the font did not draw.
							if t < -200 {
								line.WriteString(" ")
							}
						}
					}
				}
			}
		case "Td", "TD":
			if len(operand) >= 2 {
				if y, ok := operand[len(operand)-1].(float64); ok {
					moved(lastYOr(lastY) + y)
				}
			}
		case "Tm":
			if len(operand) >= 6 {
				if y, ok := operand[len(operand)-1].(float64); ok {
					moved(y)
				}
			}
		case "T*", "ET":
			flush()
		case "BT":
			lastY = math.NaN()
		case "BI":
			// An inline image is binary in the middle of the content stream,
			// and reading it as syntax is how a parser ends up in the weeds.
			skipInlineImage(p)
		}
		operand = operand[:0]
	}
	flush()
	return assemble(lines)
}

// assemble joins a page's lines, putting a paragraph break where the page left
// more room than it usually does.
//
// A PDF says nothing about paragraphs, so the only evidence there is is the
// spacing. Lines of running text sit one leading apart, and the extra space
// before a heading, between paragraphs and around a caption is bigger than
// that. Comparing each gap with the page's own most common gap is what makes
// this work on a document set with a dozen different type sizes in it, where a
// fixed threshold is right for one file and wrong for the rest.
func assemble(lines []textLine) string {
	if len(lines) == 0 {
		return ""
	}
	leading := commonGap(lines)

	var out strings.Builder
	for i, l := range lines {
		switch {
		case i == 0:
		case leading > 0 && l.gap > leading*1.4:
			out.WriteString("\n\n")
		default:
			out.WriteByte('\n')
		}
		out.WriteString(l.text)
	}
	return out.String()
}

// commonGap returns the spacing the page uses most, which is its leading.
func commonGap(lines []textLine) float64 {
	counts := map[float64]int{}
	for _, l := range lines {
		if l.gap <= 0 {
			continue
		}
		// Rounding is what makes this a count rather than a list. Two lines of
		// the same paragraph are set a fraction of a point apart when the text
		// was justified, and counting them separately would leave every gap
		// unique and no gap common.
		counts[math.Round(l.gap*2)/2]++
	}

	var best float64
	most := 0
	for gap, n := range counts {
		// Ties go to the smaller gap, because the leading of a page is the
		// space between two lines rather than between two paragraphs, and a
		// page with as many paragraph breaks as line breaks is a page of one
		// line paragraphs where the answer does not matter.
		if n > most || (n == most && gap < best) {
			best, most = gap, n
		}
	}
	return best
}

// lastYOr treats an unset position as the origin, so that the first relative
// move on a page is measured from somewhere.
func lastYOr(y float64) float64 {
	if math.IsNaN(y) {
		return 0
	}
	return y
}

// text decodes one string through a font.
func (f *font) text(s pstring) string {
	if f == nil {
		return decodeText(s)
	}
	if f.twoByte {
		var out strings.Builder
		for i := 0; i+1 < len(s); i += 2 {
			code := uint32(s[i])<<8 | uint32(s[i+1])
			if f.toUnicode != nil {
				out.WriteString(f.toUnicode[code])
				continue
			}
			out.WriteRune(rune(code))
		}
		return out.String()
	}
	if f.toUnicode == nil {
		return decodeText(s)
	}
	var out strings.Builder
	for _, c := range s {
		if mapped, ok := f.toUnicode[uint32(c)]; ok {
			out.WriteString(mapped)
			continue
		}
		out.WriteString(decodeText(pstring{c}))
	}
	return out.String()
}

// skipInlineImage moves past the binary of an inline image.
func skipInlineImage(p *parser) {
	rest := p.data[p.i:]
	for i := 0; i+1 < len(rest); i++ {
		if rest[i] != 'E' || rest[i+1] != 'I' {
			continue
		}
		// EI ends the image only when it stands alone, because the two bytes
		// occur inside compressed image data all the time.
		if i > 0 && !isSpace(rest[i-1]) {
			continue
		}
		if i+2 < len(rest) && !isSpace(rest[i+2]) {
			continue
		}
		p.i += i + 2
		return
	}
	p.i = len(p.data)
}

// decodeText turns the bytes of a string into characters for a font with no
// mapping of its own.
//
// Two encodings cover almost every such string. A string beginning with a byte
// order mark is UTF-16, which is what a title written in anything but English
// looks like, and everything else is the single byte encoding whose upper half
// is where the typographic quotes and dashes live.
func decodeText(s pstring) string {
	if len(s) >= 2 && s[0] == 0xFE && s[1] == 0xFF {
		var out strings.Builder
		for i := 2; i+1 < len(s); i += 2 {
			out.WriteRune(rune(uint16(s[i])<<8 | uint16(s[i+1])))
		}
		return out.String()
	}
	var out strings.Builder
	for _, c := range s {
		if r, ok := winAnsi[c]; ok {
			out.WriteRune(r)
			continue
		}
		out.WriteRune(rune(c))
	}
	return out.String()
}

// winAnsi is the upper half of the encoding almost every simple font uses,
// where a naive reader turns a quotation mark into a control character.
var winAnsi = map[byte]rune{
	0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„', 0x85: '…', 0x86: '†', 0x87: '‡',
	0x88: 'ˆ', 0x89: '‰', 0x8A: 'Š', 0x8B: '‹', 0x8C: 'Œ', 0x8E: 'Ž', 0x91: '‘',
	0x92: '’', 0x93: '“', 0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—',
	0x98: '˜', 0x99: '™', 0x9A: 'š', 0x9B: '›', 0x9C: 'œ', 0x9E: 'ž', 0x9F: 'Ÿ',
}

// parseCMap reads a ToUnicode mapping.
//
// The mapping is written as a small program in its own syntax, of which two
// statements matter: a list of single code to text pairs, and a list of ranges
// that map a run of codes onto a run of characters.
func parseCMap(data []byte) map[uint32]string {
	out := map[uint32]string{}

	for _, section := range sections(data, "beginbfchar", "endbfchar") {
		p := &parser{data: section}
		for {
			src, ok := p.object().(pstring)
			if !ok {
				break
			}
			dst, ok := p.object().(pstring)
			if !ok {
				break
			}
			out[codeOf(src)] = utf16BE(dst)
		}
	}

	for _, section := range sections(data, "beginbfrange", "endbfrange") {
		p := &parser{data: section}
		for {
			lo, ok := p.object().(pstring)
			if !ok {
				break
			}
			hi, ok := p.object().(pstring)
			if !ok {
				break
			}
			switch dst := p.object().(type) {
			case pstring:
				start, end := codeOf(lo), codeOf(hi)
				// A range wider than this is a file describing a mapping
				// nobody could use, and filling it in would be the memory it
				// was designed to cost.
				if end < start || end-start > 65535 {
					continue
				}
				base := []rune(utf16BE(dst))
				for c := start; c <= end; c++ {
					if len(base) == 0 {
						break
					}
					r := make([]rune, len(base))
					copy(r, base)
					r[len(r)-1] += rune(c - start)
					out[c] = string(r)
				}
			case parray:
				start := codeOf(lo)
				for i, v := range dst {
					if s, ok := v.(pstring); ok {
						out[start+uint32(i)] = utf16BE(s)
					}
				}
			default:
				continue
			}
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// cmapIsWide reports whether a mapping is written in two byte codes, which is
// how a font says its strings are pairs of bytes rather than characters.
func cmapIsWide(data []byte) bool {
	for _, section := range sections(data, "begincodespacerange", "endcodespacerange") {
		p := &parser{data: section}
		if s, ok := p.object().(pstring); ok {
			return len(s) >= 2
		}
	}
	for _, section := range sections(data, "beginbfchar", "endbfchar") {
		p := &parser{data: section}
		if s, ok := p.object().(pstring); ok {
			return len(s) >= 2
		}
	}
	return false
}

// sections returns the bodies between each pair of markers.
func sections(data []byte, begin, end string) [][]byte {
	var out [][]byte
	rest := data
	for {
		i := bytes.Index(rest, []byte(begin))
		if i < 0 {
			return out
		}
		rest = rest[i+len(begin):]
		j := bytes.Index(rest, []byte(end))
		if j < 0 {
			out = append(out, rest)
			return out
		}
		out = append(out, rest[:j])
		rest = rest[j+len(end):]
	}
}

// codeOf reads a character code out of the bytes a mapping wrote it as.
func codeOf(s pstring) uint32 {
	var v uint32
	for _, c := range s {
		v = v<<8 | uint32(c)
	}
	return v
}

// utf16BE turns the target of a mapping into text, which the format writes as
// big endian sixteen bit units.
func utf16BE(s pstring) string {
	var out strings.Builder
	for i := 0; i+1 < len(s); i += 2 {
		r := rune(uint16(s[i])<<8 | uint16(s[i+1]))
		if r == 0 {
			continue
		}
		out.WriteRune(r)
	}
	if out.Len() == 0 && len(s) == 1 {
		out.WriteRune(rune(s[0]))
	}
	return out.String()
}

// readable reports whether extracted text is characters rather than glyph
// codes.
//
// This is the check that keeps mojibake out of the index. A subset font with
// no ToUnicode mapping draws with codes that begin at one and count upwards,
// so reading them as characters produces a page of control characters and
// private use runes that looks like text to every check except this one. A
// document that fails it is treated as having no text, which is the truth: the
// characters are not recoverable from the file.
func readable(text string) bool {
	var good, bad int
	for _, r := range text {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsPunct(r), unicode.IsSpace(r), unicode.IsSymbol(r):
			if unicode.In(r, unicode.Co) {
				bad++
				continue
			}
			good++
		default:
			bad++
		}
	}
	if good+bad == 0 {
		return false
	}
	return float64(good)/float64(good+bad) > 0.8
}
