package adf_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tamnd/genba/connector/adf"
)

// doc wraps a run of nodes in the document node every Atlassian document has at
// its root, so a case below is the part that is actually being tested.
func document(content ...any) json.RawMessage {
	raw, err := json.Marshal(map[string]any{"type": "doc", "version": 1, "content": content})
	if err != nil {
		panic(err)
	}
	return raw
}

// text is a text node, with any marks on it.
func text(s string, marks ...any) map[string]any {
	n := map[string]any{"type": "text", "text": s}
	if len(marks) > 0 {
		n["marks"] = marks
	}
	return n
}

func mark(kind string, attrs map[string]any) map[string]any {
	m := map[string]any{"type": kind}
	if attrs != nil {
		m["attrs"] = attrs
	}
	return m
}

func para(content ...any) map[string]any {
	return map[string]any{"type": "paragraph", "content": content}
}

func item(content ...any) map[string]any {
	return map[string]any{"type": "listItem", "content": content}
}

func cell(s string) map[string]any {
	return map[string]any{"type": "tableCell", "content": []any{para(text(s))}}
}

func row(cells ...string) map[string]any {
	out := make([]any, 0, len(cells))
	for _, c := range cells {
		out = append(out, cell(c))
	}
	return map[string]any{"type": "tableRow", "content": out}
}

// The renderer's job is that structure survives. Each case names the thing that
// would be lost by the obvious wrong implementation, which is concatenating
// every text node in the tree.
func TestStructureSurvives(t *testing.T) {
	cases := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{
			name: "a heading is a heading at the level it says",
			in: document(map[string]any{
				"type":    "heading",
				"attrs":   map[string]any{"level": 3},
				"content": []any{text("What we tried")},
			}),
			want: "### What we tried",
		},
		{
			name: "a heading with no level is a heading anyway",
			in: document(map[string]any{
				"type":    "heading",
				"content": []any{text("Summary")},
			}),
			want: "# Summary",
		},
		{
			name: "a bulleted list keeps one line per item",
			in: document(map[string]any{
				"type":    "bulletList",
				"content": []any{item(para(text("Replaced the bearing"))), item(para(text("Checked the alignment")))},
			}),
			want: "- Replaced the bearing\n- Checked the alignment",
		},
		{
			name: "a numbered list counts from where it says it does",
			in: document(map[string]any{
				"type":    "orderedList",
				"attrs":   map[string]any{"order": 4},
				"content": []any{item(para(text("Drain it"))), item(para(text("Refill it")))},
			}),
			want: "4. Drain it\n5. Refill it",
		},
		{
			name: "a code block keeps its fence and its language",
			in: document(map[string]any{
				"type":    "codeBlock",
				"attrs":   map[string]any{"language": "go"},
				"content": []any{text("panic: index out of range [3]")},
			}),
			want: "```go\npanic: index out of range [3]\n```",
		},
		{
			name: "a code block with no language still has a fence",
			in: document(map[string]any{
				"type":    "codeBlock",
				"content": []any{text("make build")},
			}),
			want: "```\nmake build\n```",
		},
		{
			name: "a table is a table with a header row",
			in: document(map[string]any{
				"type":    "table",
				"content": []any{row("Shift", "Temperature"), row("Morning", "62")},
			}),
			want: "| Shift | Temperature |\n| --- | --- |\n| Morning | 62 |",
		},
		{
			name: "a quote is quoted on every line it covers",
			in: document(map[string]any{
				"type":    "blockquote",
				"content": []any{para(text("It has always done that.")), para(text("Since the rebuild."))},
			}),
			want: "> It has always done that.\n>\n> Since the rebuild.",
		},
		{
			name: "a panel is a quote, since the colour is all it added",
			in: document(map[string]any{
				"type":    "panel",
				"attrs":   map[string]any{"panelType": "warning"},
				"content": []any{para(text("Do not run this on a Friday."))},
			}),
			want: "> Do not run this on a Friday.",
		},
		{
			name: "a task list keeps which ones are done",
			in: document(map[string]any{
				"type": "taskList",
				"content": []any{
					map[string]any{"type": "taskItem", "attrs": map[string]any{"state": "DONE"}, "content": []any{text("Order the part")}},
					map[string]any{"type": "taskItem", "attrs": map[string]any{"state": "TODO"}, "content": []any{text("Fit the part")}},
				},
			}),
			want: "- [x] Order the part\n- [ ] Fit the part",
		},
		{
			name: "a rule is a rule",
			in:   document(map[string]any{"type": "rule"}),
			want: "---",
		},
		{
			name: "an expander keeps its title and what was folded under it",
			in: document(map[string]any{
				"type":    "expand",
				"attrs":   map[string]any{"title": "The long version"},
				"content": []any{para(text("It started in March."))},
			}),
			want: "**The long version**\n\nIt started in March.",
		},
		{
			name: "a list inside a list is indented under its bullet",
			in: document(map[string]any{
				"type": "bulletList",
				"content": []any{item(
					para(text("Check the pump")),
					map[string]any{"type": "bulletList", "content": []any{item(para(text("Inlet")))}},
				)},
			}),
			want: "- Check the pump\n\n  - Inlet",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := adf.Render(tt.in); got != tt.want {
				t.Errorf("rendered\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// The marks are the other half. A link is the one that earns its place: the
// words and where they go are two different things, and somebody searching for
// a runbook by name should find the ticket that linked to it.
func TestFormattingSurvives(t *testing.T) {
	cases := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{
			name: "code stays code",
			in:   document(para(text("restart --hard", mark("code", nil)))),
			want: "`restart --hard`",
		},
		{
			name: "bold and italic and struck through",
			in: document(para(
				text("very", mark("strong", nil)),
				text(" "),
				text("quietly", mark("em", nil)),
				text(" "),
				text("wrong", mark("strike", nil)),
			)),
			want: "**very** *quietly* ~~wrong~~",
		},
		{
			name: "a link keeps its words and its address",
			in: document(para(
				text("See "),
				text("the runbook", mark("link", map[string]any{"href": "https://wiki.acme.com/runbook"})),
			)),
			want: "See [the runbook](https://wiki.acme.com/runbook)",
		},
		{
			name: "a link whose words are already the address is written once",
			in: document(para(
				text("https://wiki.acme.com/runbook", mark("link", map[string]any{"href": "https://wiki.acme.com/runbook"})),
			)),
			want: "https://wiki.acme.com/runbook",
		},
		{
			name: "a mention reads as the name it carries",
			in: document(para(
				text("Assigned to "),
				map[string]any{"type": "mention", "attrs": map[string]any{"id": "5f3a", "text": "@ade"}},
			)),
			want: "Assigned to @ade",
		},
		{
			name: "a mention with no name is better as an id than as nothing",
			in: document(para(
				text("Assigned to "),
				map[string]any{"type": "mention", "attrs": map[string]any{"id": "5f3a"}},
			)),
			want: "Assigned to @5f3a",
		},
		{
			name: "a date is a date rather than a number of milliseconds",
			in: document(para(
				text("Due "),
				map[string]any{"type": "date", "attrs": map[string]any{"timestamp": "1755561600000"}},
			)),
			want: "Due 2025-08-19",
		},
		{
			name: "a card is the address it points at",
			in:   document(para(map[string]any{"type": "inlineCard", "attrs": map[string]any{"url": "https://acme.atlassian.net/browse/OPS-4"}})),
			want: "https://acme.atlassian.net/browse/OPS-4",
		},
		{
			name: "an attachment is named, since the bytes are not here",
			in: document(map[string]any{
				"type":    "mediaSingle",
				"content": []any{map[string]any{"type": "media", "attrs": map[string]any{"alt": "gearbox.png", "id": "9c1"}}},
			}),
			want: "(attachment: gearbox.png)",
		},
		{
			name: "a hard break is a break and not a space",
			in:   document(para(text("Line one"), map[string]any{"type": "hardBreak"}, text("Line two"))),
			want: "Line one\nLine two",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := adf.Render(tt.in); got != tt.want {
				t.Errorf("rendered %q, want %q", got, tt.want)
			}
		})
	}
}

// What arrives is somebody else's JSON, and the rule throughout is that a
// document this cannot read costs its text and not the whole item it was
// attached to. A ticket with a description nothing could parse still has a
// summary, a reporter, a status and a comment thread.
func TestWhatCannotBeReadCostsOnlyItself(t *testing.T) {
	cases := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{"nothing at all", nil, ""},
		{"an empty document", document(), ""},
		{"a plain string, which the older API sends", json.RawMessage(`"  Just a sentence.  "`), "Just a sentence."},
		{"something that is not JSON", json.RawMessage(`{oh no`), ""},
		{"a number where a document should be", json.RawMessage(`42`), ""},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := adf.Render(tt.in); got != tt.want {
				t.Errorf("rendered %q, want %q", got, tt.want)
			}
		})
	}
}

// A node type nobody has heard of is still rendered. The format gains node
// types, and a description written with next year's editor should be a ticket
// with its text in the index rather than a ticket that mysteriously has none.
func TestANodeTypeNobodyKnowsStillGivesUpItsText(t *testing.T) {
	got := adf.Render(document(map[string]any{
		"type":    "decisionList",
		"content": []any{map[string]any{"type": "decisionItem", "content": []any{text("We are replacing it.")}}},
	}))
	if got != "We are replacing it." {
		t.Errorf("rendered %q, want the text out of an unknown node", got)
	}
}

// The input is a stranger's JSON and the walk is recursive, so a document that
// nests itself further than anything legitimate is a truncated body rather than
// a crashed indexer.
func TestADocumentThatNestsForeverIsTruncatedRatherThanFatal(t *testing.T) {
	deep := map[string]any{"type": "paragraph", "content": []any{text("the bottom")}}
	for range 5000 {
		deep = map[string]any{"type": "blockquote", "content": []any{deep}}
	}

	done := make(chan string, 1)
	go func() { done <- adf.Render(document(deep)) }()
	got := <-done

	if strings.Contains(got, "the bottom") {
		t.Error("a document nested five thousand deep was walked all the way down")
	}
}

// Six blank lines in a row is not a paragraph break, it is the renderer showing
// through. Nesting produces one on the way out of every level.
func TestNestingDoesNotLeaveItsBlankLinesBehind(t *testing.T) {
	got := adf.Render(document(map[string]any{
		"type": "blockquote",
		"content": []any{map[string]any{
			"type":    "bulletList",
			"content": []any{item(map[string]any{"type": "panel", "content": []any{para(text("Deep.")), para(text("Deeper."))}})},
		}},
	}))
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("the rendering has a run of blank lines in it:\n%q", got)
	}
	for _, want := range []string{"Deep.", "Deeper."} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering lost %q:\n%s", want, got)
		}
	}
}
