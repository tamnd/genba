package extract

import (
	"context"
	"html"
	"strings"
	"unicode/utf8"
)

// HTML reads web pages, which is the format with the widest gap between what a
// file contains and what a person reading it saw.
//
// A page is mostly not its content. Navigation, cookie banners, script tags,
// style sheets and a footer of legal links are the majority of the bytes on an
// average intranet page, and indexing them means every page in a wiki matches
// a query for the words in its own navigation bar. So the elements that hold
// no prose are dropped whole, and what is left keeps its headings, its lists
// and its tables.
type HTML struct{}

// Media returns the HTML types.
func (HTML) Media() []string { return []string{"text/html"} }

// Extract turns a page into text.
func (HTML) Extract(ctx context.Context, data []byte, o Options) (Doc, error) {
	p := &page{b: newBuilder(o.MaxOutput), z: &tokenizer{data: string(data)}}
	return p.run(ctx)
}

// isHTML reports whether the bytes are a web page.
//
// It looks for the elements that only a page has, rather than for an angle
// bracket, because half the configuration files in a repository begin with an
// angle bracket and none of them are pages.
func isHTML(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	s := strings.ToLower(string(head))
	for _, marker := range []string{"<!doctype html", "<html", "<head>", "<body", "<meta charset"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// dropped are the elements whose contents are never content.
//
// The list is short on purpose. Dropping a navigation element by its name is
// tempting and wrong: plenty of pages put their only prose inside one, and a
// reader that guessed would lose whole documents silently. These are the
// elements that hold code, styling or an alternative rendering, none of which
// is text anybody meant to read.
var dropped = map[string]bool{
	"script":   true,
	"style":    true,
	"noscript": true,
	"template": true,
	"svg":      true,
	"math":     true,
	"iframe":   true,
	"object":   true,
	"embed":    true,
	"canvas":   true,
	"select":   true,
	"textarea": true,
}

// blocks are the elements that end whatever text was being collected.
//
// Anything not listed is inline, so a link or a bold run in the middle of a
// sentence does not break the sentence in two. That distinction is what
// decides whether a snippet reads as prose or as a column of fragments.
var blocks = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true,
	"header": true, "footer": true, "aside": true, "nav": true, "figure": true,
	"figcaption": true, "blockquote": true, "ul": true, "ol": true, "dl": true,
	"dt": true, "dd": true, "hr": true, "address": true, "form": true,
	"fieldset": true, "details": true, "summary": true, "caption": true,
}

// page is one pass over a document.
type page struct {
	b *builder
	z *tokenizer

	buf strings.Builder

	// heading is the level being collected, and zero outside one.
	heading int

	// item says the text being collected is a list item.
	item bool

	// cell, row and header are the table being read. A table is written a row
	// at a time as it is read, so a document that ends mid table still has the
	// rows before the end.
	cell   strings.Builder
	inCell bool
	row    []string
	header bool
	first  bool

	// pre holds preformatted text, which is the one place inside a page where
	// the whitespace is the content.
	pre   strings.Builder
	inPre bool

	// skip is the element whose contents are being thrown away, and empty the
	// rest of the time.
	skip string
}

// run walks the tokens once.
func (p *page) run(ctx context.Context) (Doc, error) {
	for n := 0; ; n++ {
		// The deadline is checked every so often rather than every token,
		// because a page is millions of tokens and a clock read each time is
		// most of the parse.
		if n%2048 == 0 {
			if err := ctx.Err(); err != nil {
				return Doc{}, err
			}
		}
		t, ok := p.z.next()
		if !ok {
			break
		}
		switch t.kind {
		case tokText:
			p.text(t.text)
		case tokStart:
			p.start(t)
		case tokEnd:
			p.end(t.name)
		}
	}
	p.emit()
	return p.b.doc(), nil
}

// text collects a run of characters into whatever is being built.
func (p *page) text(s string) {
	if p.skip != "" {
		return
	}
	if p.inPre {
		p.pre.WriteString(html.UnescapeString(s))
		return
	}
	s = html.UnescapeString(s)
	if p.inCell {
		p.cell.WriteString(s)
		return
	}
	p.buf.WriteString(s)
}

// start handles an opening tag.
func (p *page) start(t token) {
	name := t.name
	if p.skip != "" {
		return
	}
	if dropped[name] {
		p.emit()
		p.skip = name
		return
	}
	switch name {
	case "title":
		p.b.setTitle(html.UnescapeString(t.raw))
		return
	case "br":
		p.text(" ")
		return
	case "img":
		// The alternative text of an image is the only description of it a page
		// carries, and it is what somebody searching for the diagram will type.
		if alt := t.attr("alt"); alt != "" {
			p.text(" " + html.UnescapeString(alt) + " ")
		}
		return
	case "meta":
		if strings.EqualFold(t.attr("name"), "description") {
			p.b.para(html.UnescapeString(t.attr("content")))
		}
		return
	case "pre":
		p.emit()
		p.inPre = true
		p.pre.Reset()
		return
	case "table":
		p.emit()
		p.first = true
		return
	case "tr":
		p.emit()
		p.row = p.row[:0]
		p.header = false
		return
	case "td", "th":
		p.inCell = true
		p.cell.Reset()
		if name == "th" {
			p.header = true
		}
		return
	case "li":
		p.emit()
		p.item = true
		return
	}

	if level, ok := headingLevel(name); ok {
		p.emit()
		p.heading = level
		return
	}
	if blocks[name] {
		p.emit()
	}
}

// end handles a closing tag.
func (p *page) end(name string) {
	if p.skip != "" {
		if name == p.skip {
			p.skip = ""
		}
		return
	}
	switch name {
	case "pre":
		if p.inPre {
			p.inPre = false
			p.b.raw("```\n" + strings.Trim(p.pre.String(), "\n") + "\n```")
		}
		return
	case "td", "th":
		if p.inCell {
			p.row = append(p.row, p.cell.String())
			p.inCell = false
		}
		return
	case "tr":
		if len(p.row) > 0 {
			p.b.row(p.row)
			if p.first && p.header {
				p.b.rule(len(p.row))
			}
			p.first = false
			p.row = p.row[:0]
		}
		return
	case "li":
		p.emit()
		p.item = false
		return
	case "table":
		p.emit()
		p.first = true
		return
	}

	if _, ok := headingLevel(name); ok {
		p.emit()
		p.heading = 0
		return
	}
	if blocks[name] {
		p.emit()
	}
}

// emit writes whatever has been collected as the block it turned out to be.
func (p *page) emit() {
	s := p.buf.String()
	p.buf.Reset()
	if strings.TrimSpace(s) == "" {
		return
	}
	switch {
	case p.heading > 0:
		p.b.heading(p.heading, s)
	case p.item:
		p.b.item(s)
	default:
		p.b.para(s)
	}
}

// headingLevel reads h1 through h6.
func headingLevel(name string) (int, bool) {
	if len(name) != 2 || name[0] != 'h' || name[1] < '1' || name[1] > '6' {
		return 0, false
	}
	return int(name[1] - '0'), true
}

// The token kinds.
const (
	tokText = iota
	tokStart
	tokEnd
)

// token is one piece of a document.
type token struct {
	kind int
	name string

	// text is the characters of a text token.
	text string

	// attrs is the unparsed attribute text of a start tag, and raw is the
	// contents of an element the tokenizer read whole, such as a title.
	attrs string
	raw   string
}

// attr returns one attribute of a start tag.
//
// The attributes are parsed on demand rather than up front because almost no
// tag on a page has an attribute this package wants, and building a map per
// tag is most of the cost of the parse on a document with a hundred thousand
// links in it.
func (t token) attr(name string) string {
	s := t.attrs
	for {
		s = strings.TrimLeft(s, " \t\r\n/")
		if s == "" {
			return ""
		}
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			return ""
		}
		key = strings.TrimSpace(key)
		// A bare attribute before this one, as in <input disabled name=x>,
		// leaves its name attached to the front of the key.
		if i := strings.LastIndexAny(key, " \t\r\n"); i >= 0 {
			key = key[i+1:]
		}
		rest = strings.TrimLeft(rest, " \t\r\n")
		var value string
		switch {
		case rest == "":
			return ""
		case rest[0] == '"' || rest[0] == '\'':
			quote := rest[0]
			end := strings.IndexByte(rest[1:], quote)
			if end < 0 {
				value, s = rest[1:], ""
			} else {
				value, s = rest[1:1+end], rest[end+2:]
			}
		default:
			end := strings.IndexAny(rest, " \t\r\n")
			if end < 0 {
				value, s = rest, ""
			} else {
				value, s = rest[:end], rest[end:]
			}
		}
		if strings.EqualFold(key, name) {
			return value
		}
	}
}

// tokenizer walks a document once, left to right, and never backs up.
//
// It is deliberately not a parser. Nothing here builds a tree, matches an
// unclosed element or works out what a stray closing tag was meant to end,
// because extracting text does not need any of that, and every one of them is
// a place where a hostile document can make a parser do a great deal of work.
// A tokenizer that only ever moves forward cannot be made to.
type tokenizer struct {
	data string
	i    int

	// raw is set while inside an element whose contents are not markup, such
	// as a script. The angle brackets in a script are arithmetic and reading
	// them as tags is how a page's text ends up being its source code.
	raw string
}

// next returns the next token.
func (z *tokenizer) next() (token, bool) {
	if z.i >= len(z.data) {
		return token{}, false
	}

	if z.raw != "" {
		return z.rawText()
	}

	if z.data[z.i] != '<' {
		start := z.i
		if j := strings.IndexByte(z.data[z.i:], '<'); j < 0 {
			z.i = len(z.data)
		} else {
			z.i += j
		}
		return token{kind: tokText, text: z.data[start:z.i]}, true
	}

	rest := z.data[z.i:]
	switch {
	case strings.HasPrefix(rest, "<!--"):
		z.skipTo("-->", 4)
		return z.next()
	case strings.HasPrefix(rest, "<![CDATA["):
		start := z.i + 9
		z.skipTo("]]>", 9)
		end := min(z.i, len(z.data))
		if end > start {
			return token{kind: tokText, text: z.data[start : end-3]}, true
		}
		return z.next()
	case strings.HasPrefix(rest, "<!"), strings.HasPrefix(rest, "<?"):
		z.skipTo(">", 2)
		return z.next()
	case strings.HasPrefix(rest, "</"):
		name, _ := z.tag(2)
		return token{kind: tokEnd, name: name}, true
	case len(rest) > 1 && isTagStart(rest[1]):
		name, attrs := z.tag(1)
		if rawText[name] {
			z.raw = name
			// The contents of a title are wanted and the contents of a script
			// are not, and both are read the same way, so the difference is
			// made here rather than in the tokenizer's loop.
			if name == "title" {
				text, ok := z.rawText()
				return token{kind: tokStart, name: name, raw: text.text}, ok
			}
		}
		return token{kind: tokStart, name: name, attrs: attrs}, true
	default:
		// A lone angle bracket in prose, which is what an unescaped comparison
		// in a paragraph is.
		z.i++
		return token{kind: tokText, text: "<"}, true
	}
}

// rawText returns everything up to the closing tag of a raw text element.
func (z *tokenizer) rawText() (token, bool) {
	name := z.raw
	z.raw = ""
	closer := "</" + name
	rest := z.data[z.i:]
	end := indexFold(rest, closer)
	if end < 0 {
		text := rest
		z.i = len(z.data)
		return token{kind: tokText, text: text}, true
	}
	text := rest[:end]
	z.i += end
	return token{kind: tokText, text: text}, true
}

// tag reads a tag name and its attributes, and leaves the cursor after the
// closing bracket.
func (z *tokenizer) tag(offset int) (name, attrs string) {
	s := z.data[z.i+offset:]
	n := 0
	for n < len(s) && isTagByte(s[n]) {
		n++
	}
	name = strings.ToLower(s[:n])

	// Scan to the end of the tag, respecting quoted attribute values so that a
	// title attribute containing an angle bracket does not end the tag early.
	j := n
	quote := byte(0)
	for j < len(s) {
		c := s[j]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			attrs = s[n:j]
			z.i += offset + j + 1
			return name, attrs
		}
		j++
	}
	attrs = s[n:]
	z.i = len(z.data)
	return name, attrs
}

// skipTo moves past the next occurrence of a marker, or to the end.
func (z *tokenizer) skipTo(marker string, from int) {
	if j := strings.Index(z.data[z.i+from:], marker); j >= 0 {
		z.i += from + j + len(marker)
		return
	}
	z.i = len(z.data)
}

// rawText names the elements whose contents are not markup.
var rawText = map[string]bool{
	"script":   true,
	"style":    true,
	"textarea": true,
	"title":    true,
}

// isTagStart reports whether a byte can begin a tag name.
func isTagStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// isTagByte reports whether a byte can appear in a tag name.
func isTagByte(c byte) bool {
	return isTagStart(c) || c >= '0' && c <= '9' || c == '-' || c == ':' || c == '_'
}

// indexFold is a case insensitive index, which closing tags need because a
// page written in 1999 closes its elements in capitals.
func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(sub))
}
