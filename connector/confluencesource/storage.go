package confluencesource

import (
	"encoding/xml"
	"strconv"
	"strings"
)

// The storage format is the other thing a Confluence page can be.
//
// A page written in the current editor arrives as an Atlassian document and
// [adf] renders it. A page written before the editor changed, and there are a
// great many of those on any site old enough to be worth searching, has no
// Atlassian document to give. What it has is the storage format, which is XHTML
// with a handful of Confluence's own elements mixed into it, and a connector
// that could not read it would index a wiki as the pages somebody has touched
// since the migration.
//
// It is rendered to Markdown for the same reason the Atlassian document is. A
// runbook is a heading, a numbered list and three code blocks, and a search
// result that hands the reader a flattened version of the thing they were
// looking for has answered the query and failed the person.
//
// Two things about the input are worth knowing before reading any of this.
//
// It is not a document. It is a fragment, with no root element and no namespace
// declarations, so `ac:structured-macro` is a well formed name to Confluence and
// an undeclared prefix to a parser. It is read with the strict mode off, which
// leaves the prefix where it was written and is exactly what is wanted here.
//
// It is also somebody else's markup, which is the reason for the depth limit and
// the reason nothing in here returns an error. A page this cannot read is a page
// with a title, an author and a comment thread, all of which are worth indexing,
// and refusing the whole page over the shape of one element would lose more than
// it saves.

// maxStorageDepth is how far into a page this will walk.
//
// The same number the Atlassian document renderer uses, for the same reason. A
// page is nested a handful of levels deep and nothing legitimate is near this.
const maxStorageDepth = 32

// autoClose is the tags that never have an end tag, so that a page written the
// way people write HTML parses rather than swallowing the rest of itself.
//
// It is [xml.HTMLAutoClose] with link taken out, and that one omission is the
// whole reason this exists. A prefix is not a name to a parser reading a
// fragment that declares no namespaces, so ac:link arrives with the local name
// link, and closing it on sight would end a Confluence link before its label,
// leave a stray end tag, and take the rest of the sentence with it.
var autoClose = []string{
	"area", "base", "basefont", "br", "col", "frame",
	"hr", "img", "input", "isindex", "meta", "param",
}

// element is one node of a parsed page: a tag with children, or a run of text.
type element struct {
	// name is the local name, lowercased, and it is empty on a text node.
	name string

	// attr is the attributes, keyed by prefix and local name where there was a
	// prefix and by local name where there was not.
	attr map[string]string

	text string
	kids []*element
}

// storage renders the Confluence storage format as Markdown.
func storage(in string) string {
	if strings.TrimSpace(in) == "" {
		return ""
	}
	var b strings.Builder
	elemBlocks(&b, parseStorage(in), 0, "")
	return strings.TrimSpace(collapse(b.String()))
}

// parseStorage reads a page into a tree.
//
// The fragment is wrapped in a root element because a fragment with two
// paragraphs in it is two documents to an XML parser and one page to everybody
// else. What comes back is that root's children.
//
// Anything the parser will not read ends the walk and keeps what was read. A
// page that stops making sense two thirds of the way down is two thirds of a
// page in the index, which is better than none of it and much better than a
// failed sync of the space it is in.
func parseStorage(in string) []*element {
	d := xml.NewDecoder(strings.NewReader("<genba>" + in + "</genba>"))
	// Strict off is what makes an undeclared prefix readable, and it is what
	// leaves ac and ri in place rather than resolving them to nothing.
	d.Strict = false
	d.AutoClose = autoClose
	d.Entity = xml.HTMLEntity

	root := &element{}
	stack := []*element{root}
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		top := stack[len(stack)-1]
		switch t := tok.(type) {
		case xml.StartElement:
			e := &element{name: strings.ToLower(t.Name.Local)}
			for _, a := range t.Attr {
				if e.attr == nil {
					e.attr = make(map[string]string, len(t.Attr))
				}
				key := strings.ToLower(a.Name.Local)
				if a.Name.Space != "" {
					key = strings.ToLower(a.Name.Space) + ":" + key
				}
				e.attr[key] = a.Value
			}
			top.kids = append(top.kids, e)
			if len(stack) < maxStorageDepth {
				stack = append(stack, e)
			}
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			top.kids = append(top.kids, &element{text: string(t)})
		}
	}

	if len(root.kids) == 1 && root.kids[0].name == "genba" {
		return root.kids[0].kids
	}
	return root.kids
}

// at is the first of these attributes the element carries.
//
// More than one name is asked for because a prefix survives only while it is
// undeclared. A site that sends the fragment inside something that declares the
// Confluence namespaces gets the prefix resolved away, and the same attribute
// then arrives under its bare local name.
func (e *element) at(names ...string) string {
	for _, n := range names {
		if v := e.attr[n]; v != "" {
			return v
		}
	}
	return ""
}

// child is the first child with one of these names, or nil.
func (e *element) child(names ...string) *element {
	if e == nil {
		return nil
	}
	for _, k := range e.kids {
		for _, n := range names {
			if k.name == n {
				return k
			}
		}
	}
	return nil
}

// textOf is every piece of text under an element, joined and nothing else.
// It is what a code block and a macro parameter are.
func textOf(e *element) string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*element, int)
	walk = func(n *element, depth int) {
		if depth > maxStorageDepth {
			return
		}
		if n.name == "" {
			b.WriteString(n.text)
		}
		for _, k := range n.kids {
			walk(k, depth+1)
		}
	}
	walk(e, 0)
	return b.String()
}

// block reports whether an element stands on its own rather than sitting inside
// a line.
//
// A macro is not on the list even though most of them are blocks, because the
// ones that are not are inline in a sentence, and an element inside a paragraph
// is rendered inline whatever this says.
func (e *element) block() bool {
	switch e.name {
	case "h1", "h2", "h3", "h4", "h5", "h6",
		"p", "hr", "blockquote", "ul", "ol", "pre", "table",
		"div", "section", "body", "article",
		"structured-macro", "rich-text-body", "task-list",
		"layout", "layout-section", "layout-cell", "adf-extension":
		return true
	}
	return false
}

// elemBlocks renders a run of elements with a blank line between them.
//
// The elements that are not blocks are gathered up as they go and become one
// paragraph, which is what turns a cell holding a sentence and a bold word into
// a sentence with a bold word in it rather than two paragraphs.
func elemBlocks(b *strings.Builder, kids []*element, depth int, prefix string) {
	if depth > maxStorageDepth {
		return
	}

	first := true
	var run []*element
	gap := func() {
		if !first {
			line(b, strings.TrimRight(prefix, " "), "")
		}
		first = false
	}
	flush := func() {
		if len(run) == 0 {
			return
		}
		said := strings.TrimSpace(elemInline(run, depth+1))
		run = nil
		if said == "" {
			return
		}
		gap()
		line(b, prefix, said)
	}

	for _, k := range kids {
		if !k.block() {
			run = append(run, k)
			continue
		}
		flush()

		// A block is rendered aside first so that one which came to nothing can
		// be left out rather than separated from its neighbours by a blank line.
		// A table of contents is the ordinary way that happens, and a page whose
		// first element is one should not begin with an empty line.
		var one strings.Builder
		elemBlock(&one, k, depth, prefix)
		if strings.Trim(one.String(), ">| \t\n") == "" {
			continue
		}
		gap()
		b.WriteString(one.String())
	}
	flush()
}

// elemBlock renders one element that stands on its own.
func elemBlock(b *strings.Builder, e *element, depth int, prefix string) {
	if depth > maxStorageDepth {
		return
	}

	switch e.name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level, _ := strconv.Atoi(e.name[1:])
		line(b, prefix, strings.Repeat("#", level)+" "+elemInline(e.kids, depth+1))

	case "p":
		line(b, prefix, elemInline(e.kids, depth+1))

	case "hr":
		line(b, prefix, "---")

	case "blockquote":
		elemBlocks(b, e.kids, depth+1, prefix+"> ")

	case "ul", "ol":
		listOf(b, e, depth+1, prefix)

	case "pre":
		fenced(b, "", textOf(e), prefix)

	case "table":
		tableOf(b, e, depth+1, prefix)

	case "task-list":
		tasksOf(b, e, depth+1, prefix)

	case "structured-macro":
		macroOf(b, e, depth+1, prefix)

	default:
		// A container of some sort, which is most of what is left: a div, a
		// section, a page layout column, a macro's rich text body. What is
		// interesting is inside it.
		elemBlocks(b, e.kids, depth+1, prefix)
	}
}

// listOf renders a bulleted or numbered list, including the lists inside it.
func listOf(b *strings.Builder, e *element, depth int, prefix string) {
	ordered := e.name == "ol"
	start := 1
	if got, err := strconv.Atoi(e.at("start")); err == nil && got > 0 {
		start = got
	}

	n := 0
	for _, item := range e.kids {
		if item.name != "li" {
			continue
		}
		bullet := "- "
		if ordered {
			bullet = strconv.Itoa(start+n) + ". "
		}
		n++

		// Everything under the first line of an item is indented to sit under
		// it, which is what keeps a paragraph belonging to its bullet instead of
		// ending the list.
		inner := prefix + strings.Repeat(" ", len(bullet))
		var body strings.Builder
		elemBlocks(&body, item.kids, depth+1, "")

		lines := strings.Split(strings.TrimRight(body.String(), "\n"), "\n")
		for i, l := range lines {
			switch {
			case i == 0:
				line(b, prefix, bullet+l)
			case strings.TrimSpace(l) == "":
				line(b, "", "")
			default:
				line(b, inner, l)
			}
		}
	}
}

// tableOf renders a table as a Markdown one.
//
// The first row is treated as the header whether or not its cells say they are,
// because Markdown has no way to write a table without one and a table whose
// first row is data reads better as a header than as nothing.
func tableOf(b *strings.Builder, e *element, depth int, prefix string) {
	var rows []*element
	var walk func(*element, int)
	walk = func(n *element, d int) {
		if d > maxStorageDepth {
			return
		}
		for _, k := range n.kids {
			switch k.name {
			case "tr":
				rows = append(rows, k)
			case "thead", "tbody", "tfoot", "colgroup":
				walk(k, d+1)
			}
		}
	}
	walk(e, depth)
	if len(rows) == 0 {
		return
	}

	for i, row := range rows {
		var cells []string
		for _, c := range row.kids {
			if c.name != "td" && c.name != "th" {
				continue
			}
			var cell strings.Builder
			elemBlocks(&cell, c.kids, depth+1, "")
			// A newline inside a cell would end the table, so what was several
			// paragraphs becomes one line.
			cells = append(cells, strings.Join(strings.Fields(cell.String()), " "))
		}
		if len(cells) == 0 {
			continue
		}
		line(b, prefix, "| "+strings.Join(cells, " | ")+" |")
		if i == 0 {
			line(b, prefix, "|"+strings.Repeat(" --- |", len(cells)))
		}
	}
}

// tasksOf renders a task list, which is the wiki's version of a checklist and is
// very often the actual content of a page.
func tasksOf(b *strings.Builder, e *element, depth int, prefix string) {
	for _, t := range e.kids {
		if t.name != "task" {
			continue
		}
		box := "[ ] "
		if strings.EqualFold(strings.TrimSpace(textOf(t.child("task-status"))), "complete") {
			box = "[x] "
		}
		body := t.child("task-body")
		if body == nil {
			continue
		}
		line(b, prefix, "- "+box+elemInline(body.kids, depth+1))
	}
}

// macroOf renders one of Confluence's own elements.
//
// A macro is where the page keeps the things a wiki has and a document does not,
// and the three groups are worth telling apart. A code macro holds text that has
// to survive exactly. A panel holds prose that has to be read. A table of
// contents holds nothing at all: it is navigation, it is generated from the
// headings that are already in the page, and indexing it would be indexing the
// same words twice.
func macroOf(b *strings.Builder, e *element, depth int, prefix string) {
	switch strings.ToLower(e.at("ac:name", "name")) {
	case "code":
		fenced(b, param(e, "language"), textOf(e.child("plain-text-body")), prefix)

	case "noformat":
		fenced(b, "", textOf(e.child("plain-text-body")), prefix)

	case "toc", "children", "pagetree", "recently-updated", "livesearch",
		"contentbylabel", "include", "excerpt-include", "anchor", "gallery":
		// Navigation and transclusion. Neither says anything the page says, and
		// what they point at is indexed where it lives.

	case "info", "note", "warning", "tip", "panel", "expand", "excerpt":
		// A coloured box with a note in it, or an expander with the note hidden.
		// The colour is the whole of what it adds and a quote is the closest
		// thing Markdown has.
		if title := param(e, "title"); title != "" {
			line(b, prefix+"> ", "**"+title+"**")
			// With no blank line after it the title and the first line under it
			// are one paragraph, which is the opposite of what a panel is: a
			// label and the thing under the label.
			line(b, strings.TrimRight(prefix, " "), ">")
		}
		elemBlocks(b, bodyOf(e), depth+1, prefix+"> ")

	default:
		// An unknown macro is still rendered rather than dropped. Sites install
		// their own, and a page written with one should be a page with its text
		// in the index rather than a page that mysteriously has none.
		if body := bodyOf(e); len(body) > 0 {
			elemBlocks(b, body, depth+1, prefix)
			return
		}
		if said := strings.TrimSpace(textOf(e.child("plain-text-body"))); said != "" {
			line(b, prefix, said)
		}
	}
}

// bodyOf is what is inside a macro that holds prose.
func bodyOf(e *element) []*element {
	if body := e.child("rich-text-body"); body != nil {
		return body.kids
	}
	return nil
}

// param is one of a macro's settings, which are elements rather than attributes.
func param(e *element, name string) string {
	for _, k := range e.kids {
		if k.name == "parameter" && strings.EqualFold(k.at("ac:name", "name"), name) {
			return strings.TrimSpace(textOf(k))
		}
	}
	return ""
}

// fenced writes a code block.
func fenced(b *strings.Builder, lang, body, prefix string) {
	line(b, prefix, "```"+lang)
	for _, l := range strings.Split(strings.Trim(body, "\n"), "\n") {
		line(b, prefix, l)
	}
	line(b, prefix, "```")
}

// elemInline renders a run of elements that sit inside a line.
func elemInline(kids []*element, depth int) string {
	if depth > maxStorageDepth {
		return ""
	}
	var b strings.Builder
	for _, k := range kids {
		said := inlineOne(k, depth)
		// An element that comes to nothing sits between the space before it and
		// the space after it and would otherwise leave both behind. A mention of
		// somebody the page names only by account id is the one that happens.
		if strings.HasPrefix(said, " ") && strings.HasSuffix(b.String(), " ") {
			said = strings.TrimLeft(said, " ")
		}
		b.WriteString(said)
	}
	return b.String()
}

// inlineOne renders one element that sits inside a line.
func inlineOne(e *element, depth int) string {
	if e.name == "" {
		// Whitespace in XHTML is layout rather than content, and a page written
		// with one word per line is a paragraph rather than a column.
		return spaced(e.text)
	}

	switch e.name {
	case "br":
		return "\n"

	case "strong", "b":
		return around("**", elemInline(e.kids, depth+1))

	case "em", "i", "cite", "var":
		return around("*", elemInline(e.kids, depth+1))

	case "code", "kbd", "tt", "samp":
		return around("`", elemInline(e.kids, depth+1))

	case "del", "s", "strike":
		return around("~~", elemInline(e.kids, depth+1))

	case "a":
		// The text of a link and where it goes are different things and both are
		// worth having: somebody searching for a runbook by name should find the
		// page that linked to it, and somebody reading the page should be able
		// to follow it.
		said := strings.TrimSpace(elemInline(e.kids, depth+1))
		href := e.at("href")
		switch {
		case href == "" || href == said:
			return said
		case said == "":
			return href
		default:
			return "[" + said + "](" + href + ")"
		}

	case "link":
		return linkOf(e, depth)

	case "image":
		return imageOf(e)

	case "emoticon":
		// The fallback is the emoji itself, which is what the page says.
		return e.at("ac:emoji-fallback", "emoji-fallback")

	case "time":
		if at := e.at("datetime"); at != "" {
			return at
		}
		return elemInline(e.kids, depth+1)

	case "script", "style":
		// Not text anybody wrote and not text anybody reads.
		return ""

	default:
		return elemInline(e.kids, depth+1)
	}
}

// linkOf renders a link to something inside the wiki.
//
// It comes out as its label and no address, which is deliberate. The element
// names what it points at, a page by title or an attachment by filename, and it
// does not say where that is. Building an address out of a title would be
// building one that stops working the day somebody renames the page, and the
// page it points at is indexed under its own id anyway. The label is the part
// worth keeping, because the label is the sentence.
func linkOf(e *element, depth int) string {
	if body := e.child("plain-text-link-body", "link-body"); body != nil {
		if said := strings.TrimSpace(elemInline(body.kids, depth+1)); said != "" {
			return said
		}
	}
	for _, k := range e.kids {
		switch k.name {
		case "page", "blog-post":
			return k.at("ri:content-title", "content-title")
		case "attachment":
			return k.at("ri:filename", "filename")
		case "space":
			return k.at("ri:space-key", "space-key")
		}
	}
	// A mention is a link with an account id in it and no name anywhere, and an
	// account id is not something to show anybody.
	return ""
}

// imageOf renders an image the same way the Atlassian document renderer does.
//
// What is worth saying about an attachment is that it is there and what it is
// called. The index does not hold the file, and a link to it would be a link
// that stops working the day the token does.
func imageOf(e *element) string {
	name := e.at("ac:alt", "alt", "ac:title", "title")
	if name == "" {
		for _, k := range e.kids {
			switch k.name {
			case "attachment":
				name = k.at("ri:filename", "filename")
			case "url":
				name = k.at("ri:value", "value")
			}
			if name != "" {
				break
			}
		}
	}
	if name == "" {
		return ""
	}
	return "(attachment: " + name + ")"
}

// around puts a mark on both sides of some text, and leaves empty text alone so
// that an empty bold tag does not come out as four asterisks.
func around(mark, said string) string {
	if strings.TrimSpace(said) == "" {
		return said
	}
	return mark + said + mark
}

// spaced turns a run of layout whitespace into one space.
func spaced(s string) string {
	if s == "" {
		return ""
	}
	var (
		b     strings.Builder
		blank bool
	)
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', ' ':
			if !blank {
				b.WriteByte(' ')
			}
			blank = true
		default:
			blank = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// line writes one line with its prefix, leaving no trailing space on a line that
// has nothing else on it.
func line(b *strings.Builder, prefix, s string) {
	b.WriteString(strings.TrimRight(prefix+s, " "))
	b.WriteByte('\n')
}

// collapse turns any run of blank lines into one.
//
// A page that nests quotes inside lists inside macros produces a blank line per
// level on the way out, and six of them in a row is not a paragraph break, it is
// the renderer showing through.
func collapse(s string) string {
	var (
		b     strings.Builder
		blank bool
	)
	for _, l := range strings.Split(s, "\n") {
		empty := strings.TrimSpace(strings.TrimLeft(l, ">| ")) == ""
		if empty && blank {
			continue
		}
		blank = empty
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}
