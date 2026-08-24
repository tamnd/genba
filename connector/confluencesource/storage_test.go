package confluencesource

import (
	"strings"
	"testing"
)

// The storage format is the half of a wiki that the Atlassian document renderer
// never sees, and it is the older half, so on a site worth searching it is most
// of the pages. Each case below names the thing that would be lost by the
// obvious wrong implementation, which is stripping the tags and keeping the
// text between them.
func TestTheShapeOfAPageSurvives(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a heading is a heading at the level it says",
			in:   `<h2>Deploy</h2><p>It runs at nine.</p>`,
			want: "## Deploy\n\nIt runs at nine.",
		},
		{
			name: "a bulleted list keeps one line per item",
			in:   `<ul><li>first</li><li>second</li></ul>`,
			want: "- first\n- second",
		},
		{
			name: "a numbered list counts from where it says it does",
			in:   `<ol start="4"><li>Drain it</li><li>Refill it</li></ol>`,
			want: "4. Drain it\n5. Refill it",
		},
		{
			name: "a list inside a list is indented under its bullet",
			in:   `<ul><li>first</li><li>second<ul><li>nested</li></ul></li></ul>`,
			want: "- first\n- second\n\n  - nested",
		},
		{
			name: "a table is a table with a header row",
			in:   `<table><tbody><tr><th>Step</th><th>Who</th></tr><tr><td>Build</td><td>CI</td></tr></tbody></table>`,
			want: "| Step | Who |\n| --- | --- |\n| Build | CI |",
		},
		{
			name: "a table whose first row is data still gets a header, since Markdown has no other kind",
			in:   `<table><tr><td>Build</td><td>CI</td></tr><tr><td>Ship</td><td>ops</td></tr></table>`,
			want: "| Build | CI |\n| --- | --- |\n| Ship | ops |",
		},
		{
			name: "a quote is quoted on every line it covers",
			in:   `<blockquote><p>It has always done that.</p><p>Since the rebuild.</p></blockquote>`,
			want: "> It has always done that.\n>\n> Since the rebuild.",
		},
		{
			name: "a rule is a rule",
			in:   `<p>before</p><hr/><p>after</p>`,
			want: "before\n\n---\n\nafter",
		},
		{
			name: "a preformatted block keeps its fence",
			in:   "<pre>make deploy\nmake check</pre>",
			want: "```\nmake deploy\nmake check\n```",
		},
		{
			name: "a layout column is a container and not content",
			in:   `<ac:layout><ac:layout-section><ac:layout-cell><p>In the left column.</p></ac:layout-cell></ac:layout-section></ac:layout>`,
			want: "In the left column.",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := storage(tt.in); got != tt.want {
				t.Errorf("rendered\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// The marks are the other half, and the entities are the part nobody thinks
// about until a page full of non breaking spaces arrives looking like one word.
func TestFormattingSurvives(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bold and italic and struck through and code",
			in:   `<p><strong>very</strong> <em>quietly</em> <del>wrong</del> <code>restart --hard</code></p>`,
			want: "**very** *quietly* ~~wrong~~ `restart --hard`",
		},
		{
			name: "a link keeps its words and its address",
			in:   `<p>See <a href="https://ops.example.com/runbook">the runbook</a>.</p>`,
			want: "See [the runbook](https://ops.example.com/runbook).",
		},
		{
			name: "a link whose words are already the address is written once",
			in:   `<p><a href="https://ops.example.com/runbook">https://ops.example.com/runbook</a></p>`,
			want: "https://ops.example.com/runbook",
		},
		{
			name: "an empty tag does not come out as its own marks",
			in:   `<p>plain <strong></strong>text</p>`,
			want: "plain text",
		},
		{
			name: "an entity is the character it stands for",
			in:   `<p>Coolant&nbsp;pump &amp; filter</p>`,
			want: "Coolant pump & filter",
		},
		{
			name: "a line break is a break and not a space",
			in:   `<p>Line one<br/>Line two</p>`,
			want: "Line one\nLine two",
		},
		{
			name: "the layout somebody wrote the markup with is not content",
			in:   "<p>one\n    word\n    per\n    line</p>",
			want: "one word per line",
		},
		{
			name: "a script is neither written by anybody nor read by anybody",
			in:   `<p>before<script>alert(1)</script>after</p>`,
			want: "beforeafter",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := storage(tt.in); got != tt.want {
				t.Errorf("rendered %q, want %q", got, tt.want)
			}
		})
	}
}

// The Confluence elements are what makes this not an HTML renderer. A macro is
// where a wiki keeps the things a document does not have, and the three kinds of
// them are worth telling apart: one holds text that has to survive exactly, one
// holds prose that has to be read, and one holds nothing at all.
func TestTheWikisOwnElements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a code macro keeps its fence and its language",
			in: `<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">bash</ac:parameter>` +
				"<ac:plain-text-body><![CDATA[make deploy\nmake check]]></ac:plain-text-body></ac:structured-macro>",
			want: "```bash\nmake deploy\nmake check\n```",
		},
		{
			name: "a code macro with no language still has a fence",
			in: `<ac:structured-macro ac:name="code">` +
				"<ac:plain-text-body><![CDATA[make build]]></ac:plain-text-body></ac:structured-macro>",
			want: "```\nmake build\n```",
		},
		{
			name: "a panel is a quote with its title above what it says",
			in: `<ac:structured-macro ac:name="info"><ac:parameter ac:name="title">Careful</ac:parameter>` +
				`<ac:rich-text-body><p>It moved to eleven.</p></ac:rich-text-body></ac:structured-macro>`,
			want: "> **Careful**\n>\n> It moved to eleven.",
		},
		{
			name: "a panel with no title is just the quote",
			in: `<ac:structured-macro ac:name="warning">` +
				`<ac:rich-text-body><p>Not on a Friday.</p></ac:rich-text-body></ac:structured-macro>`,
			want: "> Not on a Friday.",
		},
		{
			name: "a table of contents is navigation and says nothing the page does not",
			in:   `<ac:structured-macro ac:name="toc"/><p>after</p>`,
			want: "after",
		},
		{
			name: "a macro nobody has heard of still gives up its text",
			in: `<ac:structured-macro ac:name="acme-status-board">` +
				`<ac:rich-text-body><p>Everything is green.</p></ac:rich-text-body></ac:structured-macro>`,
			want: "Everything is green.",
		},
		{
			name: "a task list keeps which ones are done",
			in: `<ac:task-list>` +
				`<ac:task><ac:task-id>1</ac:task-id><ac:task-status>complete</ac:task-status><ac:task-body>Ship it</ac:task-body></ac:task>` +
				`<ac:task><ac:task-id>2</ac:task-id><ac:task-status>incomplete</ac:task-status><ac:task-body>Tell people</ac:task-body></ac:task>` +
				`</ac:task-list>`,
			want: "- [x] Ship it\n- [ ] Tell people",
		},
		{
			name: "an attachment is named, since the bytes are not here",
			in:   `<p><ac:image ac:alt="Line two"><ri:attachment ri:filename="line.png"/></ac:image></p>`,
			want: "(attachment: Line two)",
		},
		{
			name: "an attachment with nothing said about it is named by its file",
			in:   `<p><ac:image><ri:attachment ri:filename="gearbox.png"/></ac:image></p>`,
			want: "(attachment: gearbox.png)",
		},
		{
			name: "an emoticon is the character the page shows",
			in:   `<p>Shipped <ac:emoticon ac:name="tick" ac:emoji-fallback="✅"/></p>`,
			want: "Shipped ✅",
		},
		{
			name: "a link to a page is its label, since the page is indexed under its own id",
			in: `<p>See <ac:link><ri:page ri:content-title="Runbook"/>` +
				"<ac:plain-text-link-body><![CDATA[the runbook]]></ac:plain-text-link-body></ac:link> first.</p>",
			want: "See the runbook first.",
		},
		{
			name: "a link with no label of its own reads as what it points at",
			in:   `<p>See <ac:link><ri:page ri:content-title="Runbook"/></ac:link> first.</p>`,
			want: "See Runbook first.",
		},
		{
			name: "a mention with no name is nothing, because an account id is not a name",
			in:   `<p>Ask <ac:link><ri:user ri:account-id="557058:9c1"/></ac:link> about it.</p>`,
			want: "Ask about it.",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := storage(tt.in); got != tt.want {
				t.Errorf("rendered %q, want %q", got, tt.want)
			}
		})
	}
}

// A Confluence link arrives with the local name link, because a prefix is not a
// name to a parser reading a fragment that declares no namespaces. Closing it on
// sight, which is what the standard list of void elements says to do with a
// link, ends it before its label and takes the rest of the sentence with it.
// This is that bug, and it cost a sentence off the end of every paragraph that
// mentioned another page.
func TestALinkDoesNotSwallowTheRestOfItsSentence(t *testing.T) {
	got := storage(`<p>The deploy is <ac:link><ri:page ri:content-title="Runbook"/>` +
		"<ac:plain-text-link-body><![CDATA[here]]></ac:plain-text-link-body></ac:link>" +
		` and it runs at nine.</p><p>The rollback is separate.</p>`)

	want := "The deploy is here and it runs at nine.\n\nThe rollback is separate."
	if got != want {
		t.Errorf("rendered\n%s\nwant\n%s", got, want)
	}
}

// What arrives is somebody else's markup, written by hand over a decade by
// people who were not thinking about a parser. The rule throughout is that a
// page this cannot read costs its body and not the page: a page with a title, an
// author and a comment thread is worth indexing either way.
func TestWhatCannotBeReadCostsOnlyItself(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"nothing at all", "", ""},
		{"whitespace", "   \n  ", ""},
		{"a tag that was never closed", `<p>unclosed <b>bold</p>`, "unclosed **bold**"},
		{"an end tag with no start, which keeps the page up to where it stopped making sense", `<p>one</p></div><p>two</p>`, "one"},
		{"a page that is only text", `Just a sentence.`, "Just a sentence."},
		{"an empty paragraph", `<p></p>`, ""},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := storage(tt.in); got != tt.want {
				t.Errorf("rendered %q, want %q", got, tt.want)
			}
		})
	}
}

// The walk is recursive and the input is a stranger's, so a page that nests
// itself further than anything legitimate is flattened rather than followed all
// the way down. The text at the bottom of it is still text somebody wrote and it
// is kept. What is dropped is the nesting, which past thirty levels is not
// telling a reader anything.
func TestAPageThatNestsForeverIsFlattenedRatherThanFatal(t *testing.T) {
	in := "<p>the bottom</p>"
	for range 5000 {
		in = "<blockquote>" + in + "</blockquote>"
	}

	done := make(chan string, 1)
	go func() { done <- storage(in) }()
	got := <-done

	if !strings.Contains(got, "the bottom") {
		t.Errorf("a deeply nested page lost the text at the bottom of it:\n%q", got)
	}
	if lines := strings.Count(got, "\n") + 1; lines > 4 {
		t.Errorf("a deeply nested page came out as %d lines:\n%q", lines, got)
	}
	if depth := strings.Count(got, "> "); depth > maxStorageDepth {
		t.Errorf("a page nested five thousand deep was quoted %d levels deep", depth)
	}
}

// Six blank lines in a row is not a paragraph break, it is the renderer showing
// through, and a wiki page nests deeply enough to produce them: a panel inside a
// list item inside a quote is three levels each leaving one behind.
func TestNestingDoesNotLeaveItsBlankLinesBehind(t *testing.T) {
	got := storage(`<blockquote><ul><li><ac:structured-macro ac:name="note"><ac:rich-text-body>` +
		`<p>Deep.</p><p>Deeper.</p></ac:rich-text-body></ac:structured-macro></li></ul></blockquote>`)

	if strings.Contains(got, "\n\n\n") {
		t.Errorf("the rendering has a run of blank lines in it:\n%q", got)
	}
	for _, want := range []string{"Deep.", "Deeper."} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering lost %q:\n%s", want, got)
		}
	}
}
