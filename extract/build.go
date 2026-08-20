package extract

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// builder is the one place text is written, whatever it was extracted from.
//
// Having a single writer is what makes the output of six readers comparable. A
// heading is the same three bytes whether it came from a Word style or an HTML
// tag, blocks are separated the same way, and the offsets in [Doc.Headings]
// cannot drift out of step with the text because the same call writes both.
//
// It is also where the output budget is enforced, for the same reason: a limit
// applied in six places is a limit six readers can each forget.
type builder struct {
	sb    strings.Builder
	limit int64

	headings []Heading
	title    string

	// full is set once the limit is reached, after which every write is
	// dropped. Stopping rather than failing is deliberate, and the reason is
	// on [Options.MaxOutput].
	full bool
}

func newBuilder(limit int64) *builder {
	if limit <= 0 {
		limit = DefaultOptions().MaxOutput
	}
	return &builder{limit: limit}
}

// heading writes a heading and remembers where it went.
//
// A level outside one to six is clamped rather than refused. Sources produce
// them: a Word document with a Heading 9 style is a real document, and the
// alternative to clamping is losing a section boundary over a number.
func (b *builder) heading(level int, text string) {
	text = clean(text)
	if text == "" {
		return
	}
	level = max(1, min(6, level))
	b.gap()
	if b.full {
		return
	}
	b.headings = append(b.headings, Heading{Level: level, Text: text, Offset: b.sb.Len()})
	b.write(strings.Repeat("#", level) + " " + text + "\n")
	if b.title == "" && level == 1 {
		b.title = text
	}
}

// para writes a paragraph.
func (b *builder) para(text string) {
	text = clean(text)
	if text == "" {
		return
	}
	b.gap()
	b.write(text + "\n")
}

// item writes a list item.
func (b *builder) item(text string) {
	text = clean(text)
	if text == "" {
		return
	}
	if b.sb.Len() > 0 && !b.endsWith("\n") {
		b.write("\n")
	}
	b.write("- " + text + "\n")
}

// row writes one row of a table.
//
// Tables are written as they are read, row by row, because none of these
// formats tells a reader how many rows there are before the last one. The
// header separator is the caller's business, since only the caller knows
// whether the first row was a header, and guessing turns an ordinary first row
// of data into column names.
func (b *builder) row(cells []string) {
	out := make([]string, 0, len(cells))
	empty := true
	for _, c := range cells {
		c = clean(c)
		// A pipe inside a cell would end the cell, so it is escaped. A cell
		// that ate the rest of its row is worse than one with a backslash in
		// it, and this is the only character in the output with that property.
		c = strings.ReplaceAll(c, "|", `\|`)
		if c != "" {
			empty = false
		}
		out = append(out, c)
	}
	if empty {
		return
	}
	if b.sb.Len() > 0 && !b.endsWith("\n") {
		b.write("\n")
	}
	b.write("| " + strings.Join(out, " | ") + " |\n")
}

// rule writes the separator under a table's header row.
func (b *builder) rule(columns int) {
	if columns <= 0 {
		return
	}
	b.write("|" + strings.Repeat(" --- |", columns) + "\n")
}

// raw writes text that is already in the output form, such as the body of a
// Markdown file, which is the one input that needs no conversion.
func (b *builder) raw(text string) {
	if text == "" {
		return
	}
	b.gap()
	b.write(text)
	if !b.endsWith("\n") {
		b.write("\n")
	}
}

// line writes one line of a document that is already in the output form,
// blank lines included.
//
// This is how a Markdown file is copied through, and it is separate from [raw]
// because in Markdown a blank line is the paragraph break rather than
// whitespace to be tidied away. A reader that dropped them would join every
// paragraph in the file into one, and a reader that put a blank line between
// every line would break every list.
func (b *builder) line(s string) {
	s = strings.TrimRight(s, " \t\r")
	if b.sb.Len() == 0 && s == "" {
		// Leading blank lines are what is left where front matter was, and a
		// document that starts with one has a snippet that starts with
		// nothing.
		return
	}
	b.write(s + "\n")
}

// gap opens a blank line between blocks, and does nothing at the start of the
// document or where there is already one.
func (b *builder) gap() {
	if b.sb.Len() == 0 || b.endsWith("\n\n") {
		return
	}
	if !b.endsWith("\n") {
		b.write("\n")
	}
	b.write("\n")
}

// write appends, up to the limit, and only what the rest of the pipeline can
// carry.
func (b *builder) write(s string) {
	if b.full || s == "" {
		return
	}
	s = printable(s)
	if s == "" {
		return
	}
	room := b.limit - int64(b.sb.Len())
	if room <= 0 {
		b.full = true
		return
	}
	if int64(len(s)) > room {
		// Cut on a rune boundary, because half a rune in the index is a term
		// nothing matches and a snippet that renders as a replacement
		// character.
		s = truncateRunes(s, int(room))
		b.full = true
	}
	b.sb.WriteString(s)
}

// printable removes what cannot travel any further.
//
// Two things get dropped. Control characters, because a document is a page of
// text and a NUL in the middle of one is a byte that a storage driver will
// either refuse or silently cut the document at, and neither failure shows up
// until somebody searches for the missing half. And bytes that are not valid
// UTF-8, which arrive whenever a file's extension was believed over its
// contents, because half a rune is a term nothing matches.
//
// Tabs and newlines stay. They are the only two control characters that mean
// anything in the output, and a code block loses its shape without them.
func printable(s string) string {
	if !strings.ContainsFunc(s, unprintable) {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	for _, r := range s {
		if unprintable(r) {
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// unprintable reports whether a rune has to be dropped. A replacement
// character is included because it is what decoding an invalid byte produces,
// and it is a mark on the page that was never in the document.
func unprintable(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return r == utf8.RuneError || unicode.IsControl(r)
}

// endsWith reports whether the output so far ends with a suffix, without
// copying the whole thing to find out.
func (b *builder) endsWith(suffix string) bool {
	n := b.sb.Len()
	if n < len(suffix) {
		return false
	}
	// strings.Builder has no way to look at its tail, so this is the price of
	// not keeping a second copy of every byte. The suffixes asked about are one
	// and two bytes long, so the slice is cheap.
	s := b.sb.String()
	return strings.HasSuffix(s, suffix)
}

// doc finishes the document.
func (b *builder) doc() Doc {
	text := strings.TrimRight(b.sb.String(), "\n")
	if text != "" {
		// The closing newline counts against the budget like every other byte.
		// A limit that the output goes one over is a limit that a caller
		// sizing a column or a buffer cannot rely on.
		if int64(len(text)) >= b.limit {
			text = truncateRunes(text, int(b.limit)-1)
		}
		text += "\n"
	}
	return Doc{
		Title:     b.title,
		Text:      text,
		Headings:  b.headings,
		Truncated: b.full,
	}
}

// setTitle records a title the format stated, such as an HTML title element or
// a Word document's metadata, which beats the first heading.
func (b *builder) setTitle(s string) {
	if s = clean(s); s != "" {
		b.title = s
	}
}

// clean collapses the whitespace inside one block and trims the ends.
//
// Every one of these formats carries whitespace that is layout rather than
// content: a Word paragraph split across runs, an HTML block indented by its
// source, a PDF line broken by the typesetter. Collapsing it here means the
// tokenizer sees words and a snippet reads as a sentence.
//
// Line breaks inside a block go the same way. A block is a block, and a reader
// that wanted two of them writes two.
func clean(s string) string {
	var (
		out   strings.Builder
		space bool
	)
	out.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\ufeff' || r == '\u200b' || r == '\u00ad':
			// A byte order mark, a zero width space and a soft hyphen are
			// invisible and they all end up inside words, where they split one
			// term into two that nothing will ever match.
			continue
		case unicode.IsSpace(r), r == ' ':
			space = out.Len() > 0
		default:
			if space {
				out.WriteByte(' ')
				space = false
			}
			out.WriteRune(r)
		}
	}
	return out.String()
}

// truncateRunes cuts a string to at most n bytes without splitting a rune.
func truncateRunes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

// utf8Start reports whether a byte can begin a rune, which is every byte that
// is not a continuation byte.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
