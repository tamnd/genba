package extract

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"strings"
)

// Plain reads the formats that are already text: prose, source code and comma
// separated values.
//
// Almost all of it is a copy, and that is the point. Text arrives as text, and
// a reader that reformatted it would be losing information for no gain: the
// line breaks in a log file, the indentation in a Python file and the blank
// lines in a plain text README are all content.
type Plain struct{}

// Media returns the text types this reads.
func (Plain) Media() []string {
	text := []string{"text/plain", "text/csv"}
	code := codeMedia("")
	out := make([]string, 0, len(text)+len(code))
	out = append(out, text...)
	return append(out, code...)
}

// Extract copies text through, converting only comma separated values.
func (Plain) Extract(_ context.Context, data []byte, o Options) (Doc, error) {
	b := newBuilder(o.MaxOutput)
	text := string(data)

	if looksSeparated(text) {
		if doc, ok := separated(b, text); ok {
			return doc, nil
		}
		// A file that did not parse as a table is still a text file, and the
		// text is worth more than the failure. Falling through rather than
		// failing is the right way round because the cost of being wrong here
		// is a table indexed as lines, and the cost the other way is a
		// document nobody can find.
		b = newBuilder(o.MaxOutput)
	}

	b.raw(text)
	return b.doc(), nil
}

// separated turns comma separated values into a table, treating the first row
// as the header.
//
// The first row is very nearly always the header in a file anybody exported on
// purpose, and the cost of being wrong is one row of data reading as column
// names, which is a great deal cheaper than a spreadsheet whose columns have
// been shuffled into a single line of prose.
func separated(b *builder, text string) (Doc, bool) {
	r := csv.NewReader(strings.NewReader(text))
	// A real export has ragged rows and quotes inside unquoted fields, and
	// refusing to read one is not a service to anybody trying to find it.
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	first := true
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Doc{}, false
		}
		b.row(row)
		if first {
			b.rule(len(row))
			first = false
		}
	}
	if first {
		return Doc{}, false
	}
	return b.doc(), true
}

// looksSeparated is a cheap check that a file is worth handing to the comma
// separated values reader.
//
// The file that has to be kept out is an ordinary letter, where every line has
// a comma in it and none of them is a column. Two things tell them apart. A
// table has the same number of fields on nearly every line, and a paragraph
// does not. And in prose a comma is followed by a space or ends the line, while
// a separator is followed by the next field.
//
// Only the start of the file is read, because the answer does not improve after
// the first few dozen lines and this runs on every text file that arrives.
func looksSeparated(text string) bool {
	const sample = 64

	fields := map[int]int{}
	var lines, commas, prose int
	for line := range strings.SplitSeq(text, "\n") {
		if lines == sample {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n := strings.Count(line, ",")
		if n == 0 {
			// One line with no comma at all in the first few is enough. A table
			// with a column missing still has its separators.
			return false
		}
		lines++
		fields[n+1]++
		commas += n
		for i := range len(line) {
			if line[i] == ',' && (i+1 == len(line) || line[i+1] == ' ') {
				prose++
			}
		}
	}
	if lines < 2 || prose*2 > commas {
		return false
	}

	var width, most int
	for n, count := range fields {
		if count > most || (count == most && n > width) {
			width, most = n, count
		}
	}
	// A file of one column is a list of lines, and reading it as a table gains
	// nothing and loses the shape of the file. Half the lines agreeing on the
	// width is enough, because a real export has quoted separators inside
	// fields and rows that stop early, and counting commas cannot tell those
	// from a column.
	return width >= 2 && most*2 >= lines
}

// Markdown reads Markdown, which is the format everything else is converted
// into, so the only work is finding where the headings are.
type Markdown struct{}

// Media returns the Markdown type.
func (Markdown) Media() []string { return []string{"text/markdown"} }

// Extract copies the document through and records its headings.
//
// Nothing is rewritten. A Markdown file is already the canonical form, so
// reformatting it would only be a chance to break a table somebody wrote by
// hand.
func (Markdown) Extract(_ context.Context, data []byte, o Options) (Doc, error) {
	b := newBuilder(o.MaxOutput)
	text := string(data)

	body, title := frontMatter(text)
	if title != "" {
		b.setTitle(title)
	}

	lines := strings.Split(body, "\n")
	fenced := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// A heading inside a fenced block is a comment in somebody's shell
		// script, and treating it as a section boundary would split a code
		// sample down the middle.
		if fence := fenceOf(trimmed); fence != "" {
			switch {
			case fenced == "":
				fenced = fence
			case strings.HasPrefix(fence, fenced):
				fenced = ""
			}
			b.line(line)
			continue
		}
		if fenced != "" {
			b.line(line)
			continue
		}

		if level, text, ok := atxHeading(trimmed); ok {
			b.heading(level, text)
			continue
		}
		if level, ok := setextLevel(lines, i); ok {
			b.heading(level, trimmed)
			continue
		}
		if isSetextRule(lines, i) {
			// The line under a heading has already been consumed by it.
			continue
		}
		b.line(line)
	}

	doc := b.doc()
	if doc.Title == "" && title != "" {
		doc.Title = title
	}
	return doc, nil
}

// frontMatter strips a YAML front matter block and returns any title in it.
//
// The block is not indexed. It is metadata for whatever renders the file, and
// the fields in it read as prose in a snippet: a search result whose first
// line is "layout: default" is a result nobody clicks. The title is the one
// field worth keeping, because a file that states its title states it better
// than its first heading does.
func frontMatter(text string) (body, title string) {
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return text, ""
	}
	rest := text[strings.Index(text, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		// An unterminated block is not front matter, it is a document that
		// happens to start with a rule.
		return text, ""
	}
	block := rest[:end]
	rest = rest[end+1:]
	if i := strings.Index(rest, "\n"); i >= 0 {
		rest = rest[i+1:]
	} else {
		rest = ""
	}

	for line := range strings.SplitSeq(block, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "title" {
			continue
		}
		title = strings.Trim(strings.TrimSpace(value), `"'`)
		break
	}
	return rest, title
}

// atxHeading reads a heading written with leading hashes.
func atxHeading(line string) (level int, text string, ok bool) {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, "", false
	}
	rest := line[n:]
	if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
		// "#hashtag" is not a heading, and a document full of them would
		// otherwise be all structure and no content.
		return 0, "", false
	}
	return n, strings.TrimRight(strings.TrimSpace(rest), " #"), true
}

// setextLevel reports whether a line is a heading because the line under it
// underlines it.
func setextLevel(lines []string, i int) (int, bool) {
	if strings.TrimSpace(lines[i]) == "" || i+1 >= len(lines) {
		return 0, false
	}
	switch rule(lines[i+1]) {
	case '=':
		return 1, true
	case '-':
		return 2, true
	}
	return 0, false
}

// isSetextRule reports whether a line is the underline of the heading above it.
func isSetextRule(lines []string, i int) bool {
	if i == 0 || rule(lines[i]) == 0 {
		return false
	}
	return strings.TrimSpace(lines[i-1]) != ""
}

// rule returns the character a line is made of, for a line made entirely of
// equals signs or dashes, and zero otherwise.
func rule(line string) byte {
	s := strings.TrimSpace(line)
	if len(s) < 2 {
		return 0
	}
	c := s[0]
	if c != '=' && c != '-' {
		return 0
	}
	if strings.Trim(s, string(c)) != "" {
		return 0
	}
	return c
}

// fenceOf returns the fence a line opens or closes, or empty.
func fenceOf(line string) string {
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(line, marker) {
			return marker
		}
	}
	return ""
}

// code maps a source file's extension to a media type.
//
// The list is the languages a working repository actually holds, and its job
// is to keep source code out of the unsupported bucket. Every one of them is
// read the same way, verbatim, because there is no structure in a source file
// this package is in a position to extract and reformatting one would be
// destroying the only structure it has.
var code = map[string]string{
	".go":    "text/x-go",
	".py":    "text/x-python",
	".js":    "text/javascript",
	".mjs":   "text/javascript",
	".ts":    "text/x-typescript",
	".tsx":   "text/x-typescript",
	".jsx":   "text/javascript",
	".java":  "text/x-java",
	".c":     "text/x-c",
	".h":     "text/x-c",
	".cc":    "text/x-c++",
	".cpp":   "text/x-c++",
	".hpp":   "text/x-c++",
	".rs":    "text/x-rust",
	".rb":    "text/x-ruby",
	".php":   "text/x-php",
	".cs":    "text/x-csharp",
	".swift": "text/x-swift",
	".kt":    "text/x-kotlin",
	".scala": "text/x-scala",
	".sh":    "text/x-shellscript",
	".bash":  "text/x-shellscript",
	".zsh":   "text/x-shellscript",
	".sql":   "text/x-sql",
	".proto": "text/x-protobuf",
	".tf":    "text/x-terraform",
}

// codeMedia returns the media type for a source file extension, or every type
// in the table when the extension is empty.
func codeMedia(ext string) []string {
	if ext != "" {
		if m, ok := code[ext]; ok {
			return []string{m}
		}
		return nil
	}
	seen := make(map[string]bool, len(code))
	out := make([]string, 0, len(code))
	for _, m := range code {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}
