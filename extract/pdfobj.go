package extract

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

// The object model of a PDF file.
//
// A PDF is not a document format so much as a small object database with a
// page description language stored in it. Everything in the file is one of
// these seven things, objects refer to each other by number, and the text a
// person reads is a stream of drawing commands several layers down. That is
// why extracting text from one is a parser rather than a decoder.
type (
	// pname is a name, written /Type in the file.
	pname string

	// pref is a reference to another object.
	pref struct{ num int }

	// pdict is a dictionary, written << /Key value >>.
	pdict map[pname]any

	// parray is an array, written [ a b c ].
	parray []any

	// pstring is a string, written (text) or <68657820>.
	pstring []byte
)

// object is one indirect object of the file.
type object struct {
	value any

	// stream is the raw, still encoded bytes of the object's stream, and nil
	// for an object that has none.
	stream []byte
}

// document is a parsed PDF, held as its objects.
//
// It is built by scanning the file for objects rather than by reading the
// cross reference table at the end. That is a deliberate choice and not a
// shortcut: cross reference tables are the first thing to be wrong in a file
// that was truncated, repaired, incrementally updated by two different
// products or assembled by a script, and a reader that trusts one gives up on
// files that every viewer opens. Scanning costs one pass over bytes that are
// already in memory.
type document struct {
	objects map[int]*object
	q       *quota

	// decoded caches stream contents, because a page's resources are shared
	// and a font used on four hundred pages would otherwise be inflated four
	// hundred times.
	decoded map[int][]byte
}

// objectHeader matches the "12 0 obj" that begins every indirect object.
var objectHeader = regexp.MustCompile(`(?s)(\d+)[\r\n\t ]+(\d+)[\r\n\t ]+obj\b`)

// parseDocument reads every object in the file.
func parseDocument(data []byte, q *quota) *document {
	doc := &document{objects: make(map[int]*object), q: q, decoded: make(map[int][]byte)}

	matches := objectHeader.FindAllSubmatchIndex(data, -1)
	for i, m := range matches {
		num, err := strconv.Atoi(string(data[m[2]:m[3]]))
		if err != nil {
			continue
		}
		end := len(data)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		body := data[m[1]:end]
		if j := bytes.Index(body, []byte("endobj")); j >= 0 {
			body = body[:j]
		}
		// A later definition of the same object number wins, which is what an
		// incremental update is: the file appends a new version of an object
		// and leaves the old one where it was.
		doc.objects[num] = parseObject(body)
	}

	doc.expandStreams()
	return doc
}

// parseObject reads one object body, with its stream if it has one.
func parseObject(body []byte) *object {
	p := &parser{data: body}
	value := p.object()

	p.space()
	if !p.keyword("stream") {
		return &object{value: value}
	}
	// The stream data begins after the end of line that follows the keyword.
	if p.i < len(p.data) && p.data[p.i] == '\r' {
		p.i++
	}
	if p.i < len(p.data) && p.data[p.i] == '\n' {
		p.i++
	}
	rest := p.data[p.i:]
	if j := bytes.Index(rest, []byte("endstream")); j >= 0 {
		rest = rest[:j]
	}
	return &object{value: value, stream: bytes.TrimRight(rest, "\r\n")}
}

// expandStreams pulls the objects out of the object streams.
//
// Since PDF 1.5 most of the objects in a file, including nearly every page
// dictionary, live compressed inside other objects. A reader that skips this
// step finds no pages at all in a modern file and concludes it is empty, which
// is the failure that looks like a working extractor with a strange gap in it.
func (d *document) expandStreams() {
	for num, obj := range d.objects {
		dict, ok := obj.value.(pdict)
		if !ok || dict["Type"] != pname("ObjStm") {
			continue
		}
		data, err := d.stream(num)
		if err != nil {
			continue
		}
		count, _ := d.int(dict["N"])
		first, _ := d.int(dict["First"])
		if count <= 0 || first <= 0 || first > len(data) {
			continue
		}

		header := &parser{data: data[:first]}
		for range count {
			n, ok1 := header.integer()
			off, ok2 := header.integer()
			if !ok1 || !ok2 || first+off >= len(data) {
				break
			}
			if _, taken := d.objects[n]; taken {
				// An object defined outside the stream is a later revision of
				// the one inside it.
				continue
			}
			p := &parser{data: data[first+off:]}
			d.objects[n] = &object{value: p.object()}
		}
	}
}

// resolve follows a reference to the value it names.
func (d *document) resolve(v any) any {
	for range 32 {
		ref, ok := v.(pref)
		if !ok {
			return v
		}
		obj, ok := d.objects[ref.num]
		if !ok {
			return nil
		}
		v = obj.value
	}
	// A reference that leads back to itself, which a corrupt file can hold and
	// which would otherwise be an infinite loop in a crawler.
	return nil
}

// dict resolves a value to a dictionary.
func (d *document) dict(v any) (pdict, bool) {
	switch t := d.resolve(v).(type) {
	case pdict:
		return t, true
	default:
		return nil, false
	}
}

// int resolves a value to an integer.
func (d *document) int(v any) (int, bool) {
	if f, ok := d.resolve(v).(float64); ok {
		return int(f), true
	}
	return 0, false
}

// stream returns the decoded contents of an object's stream.
func (d *document) stream(num int) ([]byte, error) {
	if data, ok := d.decoded[num]; ok {
		return data, nil
	}
	obj, ok := d.objects[num]
	if !ok || obj.stream == nil {
		return nil, fmt.Errorf("%w: object %d has no stream", ErrCorrupt, num)
	}
	dict, _ := obj.value.(pdict)

	data, err := d.decode(dict, obj.stream)
	if err != nil {
		return nil, err
	}
	if err := d.q.take(int64(len(data))); err != nil {
		return nil, err
	}
	d.decoded[num] = data
	return data, nil
}

// decode applies a stream's filters.
//
// Only the filters that carry text are implemented. An image filter is not a
// gap: the bytes behind it are a picture, and this package does not read
// pictures.
func (d *document) decode(dict pdict, raw []byte) ([]byte, error) {
	filters := d.filters(dict)
	data := raw
	for _, f := range filters {
		switch f {
		case "FlateDecode", "Fl":
			out, err := inflate(data, d.q.left)
			if err != nil {
				return nil, err
			}
			data = out
		case "ASCIIHexDecode", "AHx":
			data = unhex(data)
		case "ASCII85Decode", "A85":
			out, err := unascii85(data)
			if err != nil {
				return nil, err
			}
			data = out
		default:
			return nil, fmt.Errorf("%w: %s stream", ErrUnsupported, f)
		}
	}
	return data, nil
}

// filters returns a stream's filter names in the order they were applied.
func (d *document) filters(dict pdict) []pname {
	switch t := d.resolve(dict["Filter"]).(type) {
	case pname:
		return []pname{t}
	case parray:
		out := make([]pname, 0, len(t))
		for _, v := range t {
			if n, ok := d.resolve(v).(pname); ok {
				out = append(out, n)
			}
		}
		return out
	default:
		return nil
	}
}

// inflate decompresses a deflate stream, bounded.
//
// Both framings are tried because both occur. The specification says zlib and
// a noticeable share of real files, generally the ones written by somebody's
// own library, leave the two byte header off.
func inflate(data []byte, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultOptions().MaxDecompressed
	}
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return inflateRaw(data, limit)
	}
	defer r.Close()

	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil && len(out) == 0 {
		return inflateRaw(data, limit)
	}
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("%w: stream expands past %d bytes", ErrTooLarge, limit)
	}
	// A truncated stream still decompressed to something, and that something
	// is the first pages of the document. Returning it is better than losing
	// the file over its last object. Running out of budget is the other case
	// and is not the same: that one is refused above, because a prefix of a
	// bomb is not a prefix of a document.
	return out, nil
}

// inflateRaw decompresses a headerless deflate stream.
func inflateRaw(data []byte, limit int64) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(data))
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("%w: stream expands past %d bytes", ErrTooLarge, limit)
	}
	return out, nil
}

// unhex decodes an ASCIIHexDecode stream.
func unhex(data []byte) []byte {
	out := make([]byte, 0, len(data)/2)
	var b byte
	half := false
	for _, c := range data {
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		case c == '>':
			if half {
				out = append(out, b<<4)
			}
			return out
		default:
			continue
		}
		if half {
			out = append(out, b<<4|v)
			half = false
		} else {
			b = v
			half = true
		}
	}
	if half {
		out = append(out, b<<4)
	}
	return out
}

// unascii85 decodes an ASCII85Decode stream.
func unascii85(data []byte) ([]byte, error) {
	if i := bytes.Index(data, []byte("~>")); i >= 0 {
		data = data[:i]
	}
	var (
		out   []byte
		group [5]byte
		n     int
	)
	for _, c := range data {
		switch {
		case c == 'z' && n == 0:
			out = append(out, 0, 0, 0, 0)
			continue
		case c < '!' || c > 'u':
			continue
		}
		group[n] = c - '!'
		n++
		if n < 5 {
			continue
		}
		out = append(out, base85(group, 5)...)
		n = 0
	}
	if n > 1 {
		for i := n; i < 5; i++ {
			group[i] = 84
		}
		out = append(out, base85(group, n)...)
	}
	return out, nil
}

// base85 turns one group of five digits into four bytes, keeping n minus one
// of them for a short final group.
func base85(group [5]byte, n int) []byte {
	var v uint32
	for _, d := range group {
		v = v*85 + uint32(d)
	}
	b := []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	return b[:n-1]
}

// parser reads PDF syntax out of a byte slice.
//
// It never backs up beyond the current token and it never recurses on data it
// has not bounded, because both are how a parser over hostile input turns into
// a stack overflow or a loop that never ends.
type parser struct {
	data  []byte
	i     int
	depth int
}

// space skips whitespace and comments.
func (p *parser) space() {
	for p.i < len(p.data) {
		switch p.data[p.i] {
		case ' ', '\t', '\r', '\n', '\f', 0:
			p.i++
		case '%':
			for p.i < len(p.data) && p.data[p.i] != '\n' && p.data[p.i] != '\r' {
				p.i++
			}
		default:
			return
		}
	}
}

// keyword consumes a bare keyword if it is next.
func (p *parser) keyword(word string) bool {
	p.space()
	if p.i+len(word) > len(p.data) || string(p.data[p.i:p.i+len(word)]) != word {
		return false
	}
	p.i += len(word)
	return true
}

// object parses one object.
func (p *parser) object() any {
	p.space()
	if p.i >= len(p.data) {
		return nil
	}
	// A dictionary inside a dictionary inside a dictionary is ordinary, and a
	// file with ten thousand of them is a file built to run a parser out of
	// stack.
	if p.depth > 64 {
		return nil
	}

	switch c := p.data[p.i]; {
	case c == '<' && p.i+1 < len(p.data) && p.data[p.i+1] == '<':
		return p.dictionary()
	case c == '<':
		return p.hexString()
	case c == '(':
		return p.literalString()
	case c == '[':
		return p.array()
	case c == '/':
		return p.name()
	case c == ']' || c == '>' || c == '}' || c == ')':
		p.i++
		return nil
	case c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.':
		return p.numberOrRef()
	default:
		return p.bareword()
	}
}

// dictionary parses << ... >>.
func (p *parser) dictionary() any {
	p.i += 2
	p.depth++
	defer func() { p.depth-- }()

	out := pdict{}
	for p.i < len(p.data) {
		p.space()
		if p.i+1 < len(p.data) && p.data[p.i] == '>' && p.data[p.i+1] == '>' {
			p.i += 2
			return out
		}
		if p.i >= len(p.data) || p.data[p.i] != '/' {
			// A key that is not a name means the dictionary is malformed. What
			// has been read so far is still worth returning, because a page
			// dictionary missing its last entry still names its contents.
			if v := p.object(); v == nil && p.i >= len(p.data) {
				return out
			}
			continue
		}
		key, _ := p.name().(pname)
		out[key] = p.object()
	}
	return out
}

// array parses [ ... ].
func (p *parser) array() any {
	p.i++
	p.depth++
	defer func() { p.depth-- }()

	out := parray{}
	for p.i < len(p.data) {
		p.space()
		if p.i < len(p.data) && p.data[p.i] == ']' {
			p.i++
			return out
		}
		before := p.i
		out = append(out, p.object())
		if p.i == before {
			p.i++
		}
	}
	return out
}

// name parses /Name, including the #xx escapes a name can carry.
func (p *parser) name() any {
	p.i++
	var out []byte
	for p.i < len(p.data) {
		c := p.data[p.i]
		if isDelim(c) || isSpace(c) {
			break
		}
		if c == '#' && p.i+2 < len(p.data) {
			if v, err := strconv.ParseUint(string(p.data[p.i+1:p.i+3]), 16, 8); err == nil {
				out = append(out, byte(v))
				p.i += 3
				continue
			}
		}
		out = append(out, c)
		p.i++
	}
	return pname(out)
}

// literalString parses (text), with the escapes and the balanced parentheses
// the format allows.
func (p *parser) literalString() any {
	p.i++
	var (
		out   []byte
		depth = 1
	)
	for p.i < len(p.data) {
		c := p.data[p.i]
		p.i++
		switch c {
		case '\\':
			if p.i >= len(p.data) {
				return pstring(out)
			}
			e := p.data[p.i]
			p.i++
			switch e {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case '\n':
				// A backslash at the end of a line is a continuation, and the
				// break is not part of the string.
			case '\r':
				if p.i < len(p.data) && p.data[p.i] == '\n' {
					p.i++
				}
			default:
				if e >= '0' && e <= '7' {
					v := int(e - '0')
					for range 2 {
						if p.i < len(p.data) && p.data[p.i] >= '0' && p.data[p.i] <= '7' {
							v = v*8 + int(p.data[p.i]-'0')
							p.i++
						}
					}
					out = append(out, byte(v))
					continue
				}
				out = append(out, e)
			}
		case '(':
			depth++
			out = append(out, c)
		case ')':
			depth--
			if depth == 0 {
				return pstring(out)
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return pstring(out)
}

// hexString parses <68657861>.
func (p *parser) hexString() any {
	p.i++
	start := p.i
	for p.i < len(p.data) && p.data[p.i] != '>' {
		p.i++
	}
	out := unhex(p.data[start:p.i])
	if p.i < len(p.data) {
		p.i++
	}
	return pstring(out)
}

// numberOrRef parses a number, or the "12 0 R" that is a reference.
func (p *parser) numberOrRef() any {
	start := p.i
	for p.i < len(p.data) && !isDelim(p.data[p.i]) && !isSpace(p.data[p.i]) {
		p.i++
	}
	text := string(p.data[start:p.i])
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil
	}
	if value != float64(int(value)) || value < 0 {
		return value
	}

	// A reference is three tokens and looks like two numbers until the R
	// arrives, so the position is kept and restored when it does not.
	save := p.i
	p.space()
	genStart := p.i
	for p.i < len(p.data) && p.data[p.i] >= '0' && p.data[p.i] <= '9' {
		p.i++
	}
	if p.i > genStart {
		p.space()
		if p.i < len(p.data) && p.data[p.i] == 'R' && (p.i+1 >= len(p.data) || isDelim(p.data[p.i+1]) || isSpace(p.data[p.i+1])) {
			p.i++
			return pref{num: int(value)}
		}
	}
	p.i = save
	return value
}

// integer reads a plain integer, which is what an object stream's header is
// made of.
func (p *parser) integer() (int, bool) {
	p.space()
	start := p.i
	for p.i < len(p.data) && p.data[p.i] >= '0' && p.data[p.i] <= '9' {
		p.i++
	}
	if p.i == start {
		return 0, false
	}
	n, err := strconv.Atoi(string(p.data[start:p.i]))
	return n, err == nil
}

// bareword reads a keyword such as true, null or a content stream operator.
func (p *parser) bareword() any {
	start := p.i
	for p.i < len(p.data) && !isDelim(p.data[p.i]) && !isSpace(p.data[p.i]) {
		p.i++
	}
	if p.i == start {
		p.i++
		return nil
	}
	switch word := string(p.data[start:p.i]); word {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	default:
		return operator(word)
	}
}

// operator is a content stream keyword, which is the one thing in the syntax
// that is not a value.
type operator string

// isSpace reports whether a byte is PDF whitespace.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == 0
}

// isDelim reports whether a byte ends a token.
func isDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}
