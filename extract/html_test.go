package extract

import (
	"strings"
	"testing"
)

func TestHTMLKeepsTheStructureAndDropsTheFurniture(t *testing.T) {
	for _, c := range []struct {
		why    string
		in     string
		want   string
		absent string
	}{
		{
			why:  "a paragraph is a block whatever its source looked like",
			in:   "<p>One\n   sentence\tsplit\nacross lines.</p>",
			want: "One sentence split across lines.",
		},
		{
			why:  "inline markup is not a block boundary",
			in:   "<p>Filed by <b>field</b>-<i>ops</i>.</p>",
			want: "Filed by field-ops.",
		},
		{
			why:  "two blocks are two blocks",
			in:   "<div>First.</div><div>Second.</div>",
			want: "First.\n\nSecond.",
		},
		{
			why:  "a break inside a paragraph is a space rather than a new block",
			in:   "<p>Line one<br>line two</p>",
			want: "Line one line two",
		},
		{
			why:  "entities are decoded, including the numeric ones",
			in:   "<p>caf&eacute; &amp; co &#8212; open</p>",
			want: "café & co — open",
		},
		{
			why:    "a script is code rather than content",
			in:     "<p>Real.</p><script>var secret = 'token';</script>",
			absent: "secret",
		},
		{
			why:    "a style sheet is not prose",
			in:     "<style>p { color: red }</style><p>Real.</p>",
			absent: "color",
		},
		{
			why:    "a comment is not content",
			in:     "<p>Real.</p><!-- a note to the next developer -->",
			absent: "next developer",
		},
		{
			why:  "an attribute that looks like text is not text",
			in:   `<p title="a tooltip">Visible.</p>`,
			want: "Visible.",
		},
		{
			why:  "a list is a list",
			in:   "<ul><li>First</li><li>Second</li></ul>",
			want: "- First\n- Second",
		},
		{
			why:  "a table keeps its rows",
			in:   "<table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table>",
			want: "| A | B |\n| --- | --- |\n| 1 | 2 |",
		},
		{
			why:  "preformatted text keeps its line breaks",
			in:   "<pre>one\n  two</pre>",
			want: "```\none\n  two\n```",
		},
		{
			why:  "a pipe in a cell does not eat the rest of the row",
			in:   "<table><tr><td>a|b</td><td>c</td></tr></table>",
			want: `| a\|b | c |`,
		},
		{
			why:  "an unclosed tag does not lose the document",
			in:   "<p>First.<p>Second.",
			want: "First.\n\nSecond.",
		},
		{
			why:  "a tag that never closes at the end of the file is not a crash",
			in:   "<p>Text.</p><div class=\"unfinished",
			want: "Text.",
		},
		{
			why:  "uppercase tags are the same tags",
			in:   "<H1>Title</H1><P>Body.</P>",
			want: "# Title\n\nBody.",
		},
	} {
		doc, err := Extract(t.Context(), strings.NewReader(c.in), "page.html")
		if err != nil {
			t.Errorf("%s: %v", c.why, err)
			continue
		}
		if c.want != "" && !strings.Contains(doc.Text, c.want) {
			t.Errorf("%s: text is %q, want it to contain %q", c.why, doc.Text, c.want)
		}
		if c.absent != "" && strings.Contains(doc.Text, c.absent) {
			t.Errorf("%s: text is %q, which should not contain %q", c.why, doc.Text, c.absent)
		}
	}
}

func TestHTMLHeadingsBecomeTheOutline(t *testing.T) {
	const page = `<html><head><title>Stated title</title></head><body>
<h1>Top</h1><p>a</p><h2>Middle</h2><p>b</p><h3>Inner</h3><p>c</p></body></html>`

	doc, err := Extract(t.Context(), strings.NewReader(page), "page.html")
	if err != nil {
		t.Fatal(err)
	}
	// A stated title beats the first heading, because the page said it on
	// purpose and the heading is where the text happens to start.
	if doc.Title != "Stated title" {
		t.Errorf("title is %q", doc.Title)
	}
	want := []Heading{{1, "Top", 0}, {2, "Middle", 0}, {3, "Inner", 0}}
	if len(doc.Headings) != len(want) {
		t.Fatalf("got %d headings, want %d: %v", len(doc.Headings), len(want), doc.Headings)
	}
	for i, h := range doc.Headings {
		if h.Level != want[i].Level || h.Text != want[i].Text {
			t.Errorf("heading %d is %d %q, want %d %q", i, h.Level, h.Text, want[i].Level, want[i].Text)
		}
	}
}

func TestDeeplyNestedHTMLIsNotAStackOverflow(t *testing.T) {
	// A page can nest as far as it likes, and a recursive reader is one file
	// away from taking the process with it.
	in := strings.Repeat("<div>", 50_000) + "buried" + strings.Repeat("</div>", 50_000)

	doc, err := Extract(t.Context(), strings.NewReader(in), "deep.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Text, "buried") {
		t.Error("the text at the bottom of the nesting did not come out")
	}
}

// FuzzHTML is the tokenizer, which is the piece of this package most exposed
// to whatever somebody puts on a web server.
func FuzzHTML(f *testing.F) {
	for _, seed := range []string{
		"<p>hello</p>",
		"<table><tr><td>a",
		"<!-- unterminated",
		"<script>var x = '</scr' + 'ipt>';</script>",
		"<a href=\"",
		"<pre>x</pre>",
		"&amp;&#x41;&notanentity;",
		"<h1>",
		"<<<>>>",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		doc, err := Extract(t.Context(), strings.NewReader(in), "page.html")
		if err != nil {
			return
		}
		// Whatever comes out has to be usable as text, because everything
		// downstream of here assumes it is.
		if strings.Contains(doc.Text, "\x00") {
			t.Fatalf("text holds a NUL: %q", doc.Text)
		}
		for _, h := range doc.Headings {
			if h.Offset < 0 || h.Offset > len(doc.Text) {
				t.Fatalf("heading %q has offset %d, text is %d bytes", h.Text, h.Offset, len(doc.Text))
			}
		}
	})
}
