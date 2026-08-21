package index

import "strings"

// plainText removes markdown syntax and keeps the words.
//
// A snippet is cut out of what this returns, because two lines of a result row
// is a small budget and one spent on hashes, asterisks and table pipes has been
// spent on nothing. The person reading wants the sentence somebody wrote.
//
// Only markdown is treated this way, and see [snippet] for where that is
// decided. A source file is not markdown, and stripping what looks like syntax
// out of code would be lying about the file.
//
// It is a line pass and then an inline pass, which is enough for a snippet and
// deliberately less than a renderer. It never reorders text: a term at the start
// of the body is still near the start afterwards, so the passage this produces is
// the passage a reader would have picked.
func plainText(md string) string {
	if md == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(md))

	fence := ""
	for line := range strings.SplitSeq(md, "\n") {
		trimmed := strings.TrimSpace(line)

		// Inside a fence the text is code and it is kept exactly, because that
		// is the one place where an asterisk means an asterisk.
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
				continue
			}
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		if marker := fenceOf(trimmed); marker != "" {
			fence = marker
			continue
		}
		if isRule(trimmed) || isTableDivider(trimmed) {
			continue
		}

		trimmed = trimMarkers(trimmed)
		if strings.HasPrefix(trimmed, "|") {
			trimmed = tableRow(trimmed)
		}
		b.WriteString(inlineText(trimmed))
		b.WriteByte('\n')
	}
	return b.String()
}

// fenceOf returns the fence marker a line opens, or an empty string.
func fenceOf(line string) string {
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(line, marker) {
			return marker
		}
	}
	return ""
}

// isRule reports whether a line is a horizontal rule, which carries no words.
func isRule(line string) bool {
	if len(line) < 3 {
		return false
	}
	c := line[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	return strings.Trim(line, string(c)) == ""
}

// isTableDivider reports whether a line is the alignment row under a table
// head. It is the one table line that is pure syntax.
func isTableDivider(line string) bool {
	if !strings.HasPrefix(line, "|") {
		return false
	}
	return strings.Trim(line, "|-: \t") == "" && strings.ContainsRune(line, '-')
}

// trimMarkers removes the block level markers from the front of a line: the
// hashes of a heading, the arrows of a quote, a list bullet or number, and a
// task box.
func trimMarkers(line string) string {
	for {
		before := line
		switch {
		case strings.HasPrefix(line, "#"):
			line = strings.TrimLeft(line, "#")
		case strings.HasPrefix(line, ">"):
			line = strings.TrimLeft(line, ">")
		case len(line) > 1 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && line[1] == ' ':
			line = line[1:]
		default:
			line = trimOrderedMarker(line)
		}
		line = strings.TrimLeft(line, " \t")
		if line == before {
			break
		}
	}
	// A task box is a marker too, and an empty one in a snippet reads as a typo.
	for _, box := range []string{"[ ] ", "[x] ", "[X] "} {
		if rest, ok := strings.CutPrefix(line, box); ok {
			return rest
		}
	}
	return line
}

// trimOrderedMarker removes a leading "12. " or "12) ".
func trimOrderedMarker(line string) string {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i+1 >= len(line) {
		return line
	}
	if (line[i] == '.' || line[i] == ')') && line[i+1] == ' ' {
		return line[i+1:]
	}
	return line
}

// tableRow turns a row of cells into the words in it, in order.
func tableRow(line string) string {
	cells := strings.Split(strings.Trim(line, "|"), "|")
	for i, c := range cells {
		cells[i] = strings.TrimSpace(c)
	}
	return strings.Join(cells, " ")
}

// inlineText removes the inline syntax from one line.
//
// Emphasis markers are only dropped at a word boundary, so snake_case survives
// and a multiplication sign in prose is not mistaken for the start of an
// emphasis run. A link keeps its text and loses its target, because the target
// is not something anybody reads in a result row.
func inlineText(line string) string {
	var b strings.Builder
	b.Grow(len(line))

	for i := 0; i < len(line); {
		switch c := line[i]; {
		case c == '`':
			i++
		case c == '\\' && i+1 < len(line) && isPunct(line[i+1]):
			// An escaped marker is the character itself.
			b.WriteByte(line[i+1])
			i += 2
		case (c == '*' || c == '_') && boundary(line, i):
			for i < len(line) && line[i] == c {
				i++
			}
		case c == '!' && i+1 < len(line) && line[i+1] == '[':
			text, next, ok := linkAt(line, i+1)
			if !ok {
				b.WriteByte(c)
				i++
				continue
			}
			b.WriteString(text)
			i = next
		case c == '[':
			text, next, ok := linkAt(line, i)
			if !ok {
				b.WriteByte(c)
				i++
				continue
			}
			b.WriteString(text)
			i = next
		case c == '<':
			if end := strings.IndexByte(line[i:], '>'); end > 0 && isAutolink(line[i+1:i+end]) {
				b.WriteString(line[i+1 : i+end])
				i += end + 1
				continue
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// linkAt reads a [text](target) starting at an open bracket, returning the text
// and the offset just past the target.
func linkAt(line string, i int) (text string, past int, ok bool) {
	bracket := strings.IndexByte(line[i:], ']')
	if bracket < 0 || i+bracket+1 >= len(line) || line[i+bracket+1] != '(' {
		return "", 0, false
	}
	paren := strings.IndexByte(line[i+bracket+1:], ')')
	if paren < 0 {
		return "", 0, false
	}
	return line[i+1 : i+bracket], i + bracket + paren + 2, true
}

// boundary reports whether the marker at i starts or ends a run of emphasis
// rather than sitting inside a word.
func boundary(line string, i int) bool {
	c := line[i]
	before := byte(' ')
	if i > 0 {
		before = line[i-1]
	}
	j := i
	for j < len(line) && line[j] == c {
		j++
	}
	after := byte(' ')
	if j < len(line) {
		after = line[j]
	}
	return before == ' ' || before == '\t' || after == ' ' || after == '\t' || j == len(line)
}

func isAutolink(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "mailto:")
}

func isPunct(c byte) bool {
	return strings.IndexByte("\\`*_{}[]()#+-.!|>~", c) >= 0
}
