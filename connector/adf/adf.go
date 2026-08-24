// Package adf renders an Atlassian document as Markdown.
//
// A Jira description is not text. Neither is a Confluence page, a comment on
// either of them, or anything else written in the editor those products share.
// It is a document tree, and turning it back into something a person and an
// index can both read is the whole of what this package does.
//
// Two ways of getting it wrong are worth naming, because both are common.
//
// Concatenating every text node is the first. It produces a wall with the
// heading run into the paragraph and the three bullet points run into each
// other, and an index built on it cannot tell a phrase somebody wrote from a
// phrase made by two unrelated lines meeting. Structure is not decoration.
//
// Throwing the structure away is the second, and it is worse. A ticket's
// description is very often a stack trace, a snippet of configuration or a
// table of what was tried, and a search result that shows the reader a
// flattened version of the thing they were looking for has answered the query
// and failed the person. So this renders Markdown: headings stay headings,
// code blocks keep their fences and their language, lists stay lists and tables
// stay tables. The index reads it as text and the interface renders it as what
// it is.
//
// It is a package rather than a file inside one connector because the format
// belongs to the editor rather than to the product. Every Atlassian connector
// meets the same tree, and the second one to want it should get this and not a
// second renderer that agrees with this one on the cases somebody thought to
// test.
package adf

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// node is one element of an Atlassian document.
type node struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Attrs   map[string]any  `json:"attrs"`
	Marks   []mark          `json:"marks"`
	Content json.RawMessage `json:"content"`
}

// mark is the formatting on a text node, which arrives beside the text rather
// than around it.
type mark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs"`
}

// maxDepth is how far into a document this will walk.
//
// A description is nested a handful of levels deep and nothing legitimate is
// near this. It is here because the input is somebody else's JSON and the walk
// is recursive, and a document that nests itself a hundred thousand deep should
// be a truncated description rather than a crashed indexer.
const maxDepth = 32

// Render turns an Atlassian document into Markdown.
//
// Anything that will not parse comes back empty rather than as an error. A
// description this cannot read is a ticket with a summary, a reporter, a status
// and a comment thread, all of which are worth indexing, and refusing the whole
// issue over the shape of one field would be losing more than it saves.
//
// A plain string is accepted as well as a tree, because a site on the older API
// and a field somebody set through a script both send one.
func Render(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// A site on the older API, or a field somebody set through a script, holds
	// a plain string here rather than a tree.
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return strings.TrimSpace(plain)
	}

	var doc node
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}

	var b strings.Builder
	block(&b, doc, 0, "")
	return strings.TrimSpace(collapse(b.String()))
}

// children decodes the content of a node, which is held raw so that a document
// is only walked as deep as it is read.
func children(n node) []node {
	if len(n.Content) == 0 {
		return nil
	}
	var out []node
	if json.Unmarshal(n.Content, &out) != nil {
		return nil
	}
	return out
}

// block renders a node that stands on its own, at the given nesting depth and
// with prefix in front of every line it produces.
//
// The prefix is what makes a list inside a quote inside a list come out right:
// each level adds to it rather than tracking where it is.
func block(b *strings.Builder, n node, depth int, prefix string) {
	if depth > maxDepth {
		return
	}

	switch n.Type {
	case "doc":
		blocks(b, children(n), depth+1, prefix)

	case "paragraph":
		line(b, prefix, inline(children(n), depth+1))

	case "heading":
		level := 1
		if got, ok := number(n.Attrs, "level"); ok && got >= 1 && got <= 6 {
			level = got
		}
		line(b, prefix, strings.Repeat("#", level)+" "+inline(children(n), depth+1))

	case "blockquote":
		blocks(b, children(n), depth+1, prefix+"> ")

	case "bulletList", "orderedList":
		list(b, n, depth+1, prefix)

	case "codeBlock":
		lang, _ := str(n.Attrs, "language")
		line(b, prefix, "```"+lang)
		for _, l := range strings.Split(inline(children(n), depth+1), "\n") {
			line(b, prefix, l)
		}
		line(b, prefix, "```")

	case "rule":
		line(b, prefix, "---")

	case "panel":
		// A panel is a coloured box with a note in it. The colour is the whole
		// of what it adds and a quote is the closest thing Markdown has.
		blocks(b, children(n), depth+1, prefix+"> ")

	case "table":
		table(b, n, depth+1, prefix)

	case "taskList":
		for _, item := range children(n) {
			state, _ := str(item.Attrs, "state")
			box := "[ ] "
			if strings.EqualFold(state, "DONE") {
				box = "[x] "
			}
			line(b, prefix, "- "+box+inline(children(item), depth+2))
		}

	case "mediaGroup", "mediaSingle":
		for _, m := range children(n) {
			block(b, m, depth+1, prefix)
		}

	case "media":
		// An attachment is a file the index does not hold, and what is worth
		// saying about it is that it is there and what it is called. A link to
		// it would be a link that stops working the day the token does.
		name, _ := str(n.Attrs, "alt")
		if name == "" {
			name, _ = str(n.Attrs, "id")
		}
		if name != "" {
			line(b, prefix, "(attachment: "+name+")")
		}

	case "expand", "nestedExpand":
		if title, ok := str(n.Attrs, "title"); ok && title != "" {
			line(b, prefix, "**"+title+"**")
			// With no blank line after it the title and the first line under it
			// are one paragraph in Markdown, which is the opposite of what an
			// expander is: a label and the thing it was hiding.
			line(b, strings.TrimRight(prefix, " "), "")
		}
		blocks(b, children(n), depth+1, prefix)

	default:
		// An unknown block is still rendered rather than dropped. The format
		// gains node types and a description written with next year's editor
		// should be a ticket with its text in the index rather than a ticket
		// that mysteriously has none.
		kids := children(n)
		if len(kids) == 0 {
			line(b, prefix, inlineOne(n, depth+1))
			return
		}
		blocks(b, kids, depth+1, prefix)
	}
}

// blocks renders a run of nodes with a blank line between them.
func blocks(b *strings.Builder, nodes []node, depth int, prefix string) {
	for i, n := range nodes {
		if i > 0 {
			line(b, strings.TrimRight(prefix, " "), "")
		}
		block(b, n, depth, prefix)
	}
}

// list renders a bulleted or numbered list, including the lists inside it.
func list(b *strings.Builder, n node, depth int, prefix string) {
	ordered := n.Type == "orderedList"
	start := 1
	if got, ok := number(n.Attrs, "order"); ok && got > 0 {
		start = got
	}

	for i, item := range children(n) {
		bullet := "- "
		if ordered {
			bullet = strconv.Itoa(start+i) + ". "
		}
		// Everything under the first line of an item is indented to sit under
		// it, which is what keeps a paragraph belonging to its bullet instead
		// of ending the list.
		inner := prefix + strings.Repeat(" ", len(bullet))
		var body strings.Builder
		blocks(&body, children(item), depth+1, "")

		lines := strings.Split(strings.TrimRight(body.String(), "\n"), "\n")
		for j, l := range lines {
			switch {
			case j == 0:
				line(b, prefix, bullet+l)
			case strings.TrimSpace(l) == "":
				line(b, "", "")
			default:
				line(b, inner, l)
			}
		}
	}
}

// table renders a table as a Markdown one.
//
// The first row is treated as the header whether or not its cells say they are,
// because Markdown has no way to write a table without one and a table whose
// first row is data reads better as a header than as nothing.
func table(b *strings.Builder, n node, depth int, prefix string) {
	rows := children(n)
	if len(rows) == 0 {
		return
	}
	for i, row := range rows {
		cells := children(row)
		out := make([]string, 0, len(cells))
		for _, c := range cells {
			var cell strings.Builder
			blocks(&cell, children(c), depth+1, "")
			// A newline inside a cell would end the table, so what was several
			// paragraphs becomes one line.
			out = append(out, strings.Join(strings.Fields(cell.String()), " "))
		}
		line(b, prefix, "| "+strings.Join(out, " | ")+" |")
		if i == 0 {
			line(b, prefix, "|"+strings.Repeat(" --- |", len(out)))
		}
	}
}

// inline renders a run of nodes that sit inside a line.
func inline(nodes []node, depth int) string {
	if depth > maxDepth {
		return ""
	}
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString(inlineOne(n, depth))
	}
	return b.String()
}

// inlineOne renders one node that sits inside a line.
func inlineOne(n node, depth int) string {
	switch n.Type {
	case "text":
		return marked(n)

	case "hardBreak":
		return "\n"

	case "mention":
		// The text a mention carries already has the at sign on it. Where it
		// does not, the id is better than nothing, because a ticket that says
		// "assigning this to" and then stops is a ticket that lost its point.
		if t, ok := str(n.Attrs, "text"); ok && t != "" {
			return t
		}
		if id, ok := str(n.Attrs, "id"); ok {
			return "@" + id
		}
		return ""

	case "emoji":
		if t, ok := str(n.Attrs, "text"); ok && t != "" {
			return t
		}
		s, _ := str(n.Attrs, "shortName")
		return s

	case "date":
		// A date node holds milliseconds since the epoch, which is not
		// something to show anybody, and there is nothing else in the node.
		if ts, ok := str(n.Attrs, "timestamp"); ok {
			if ms, err := strconv.ParseInt(ts, 10, 64); err == nil {
				return stampMillis(ms)
			}
		}
		return ""

	case "status":
		if t, ok := str(n.Attrs, "text"); ok {
			return t
		}
		return ""

	case "inlineCard", "blockCard", "embedCard":
		u, _ := str(n.Attrs, "url")
		return u

	default:
		return inline(children(n), depth+1)
	}
}

// marked applies a text node's formatting.
//
// A link is the one that earns its place. The text of a link and where it goes
// are different things and both are worth having: somebody searching for a
// runbook by name should find the ticket that linked to it, and somebody
// reading the ticket should be able to follow it.
func marked(n node) string {
	out := n.Text
	if out == "" {
		return ""
	}

	var href string
	for _, m := range n.Marks {
		switch m.Type {
		case "code":
			out = "`" + out + "`"
		case "strong":
			out = "**" + out + "**"
		case "em":
			out = "*" + out + "*"
		case "strike":
			out = "~~" + out + "~~"
		case "link":
			href, _ = str(m.Attrs, "href")
		}
	}
	if href != "" && href != out {
		out = "[" + out + "](" + href + ")"
	}
	return out
}

// line writes one line with its prefix, leaving no trailing space on a line
// that has nothing else on it.
func line(b *strings.Builder, prefix, s string) {
	b.WriteString(strings.TrimRight(prefix+s, " "))
	b.WriteByte('\n')
}

// collapse turns any run of blank lines into one.
//
// A document that nests quotes inside lists inside panels produces a blank line
// per level on the way out, and six of them in a row is not a paragraph break,
// it is the renderer showing through.
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

// str reads a string attribute, which arrives as any because the attribute set
// of a node is not a fixed shape.
func str(attrs map[string]any, name string) (string, bool) {
	v, ok := attrs[name].(string)
	return v, ok
}

// stampMillis renders a date node's timestamp, which arrives as milliseconds
// since the epoch and is a date rather than an instant.
func stampMillis(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}

// number reads a numeric attribute. JSON numbers decode as float64, and a
// heading level that arrived as 2.0 is a heading level of two.
func number(attrs map[string]any, name string) (int, bool) {
	switch v := attrs[name].(type) {
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil
	default:
		return 0, false
	}
}
