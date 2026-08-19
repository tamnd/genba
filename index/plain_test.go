package index

import (
	"strings"
	"testing"
)

func TestPlainTextKeepsTheWordsAndDropsTheSyntax(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want string
	}{
		{"heading", "# Permissions and governance", "Permissions and governance"},
		{"setext style hashes", "### 3.1 The count row", "3.1 The count row"},
		{"emphasis", "the rule is **never** shown to a *user*", "the rule is never shown to a user"},
		{"inline code", "call `store.Get` first", "call store.Get first"},
		{"link", "see [the spec](https://example.com/spec) for the rest", "see the spec for the rest"},
		{"image", "![architecture](diagram.png)", "architecture"},
		{"autolink", "written at <https://example.com/x>", "written at https://example.com/x"},
		{"bullet", "- a permission that failed to resolve", "a permission that failed to resolve"},
		{"numbered", "3. tokens are rewritten first", "tokens are rewritten first"},
		{"task", "- [x] every focus selector is focus-visible", "every focus selector is focus-visible"},
		{"quote", "> the principal is applied by the driver", "the principal is applied by the driver"},
		{"table row", "| Media type | Renderer |", "Media type Renderer"},
		{"snake case survives", "the store_id column is folded", "the store_id column is folded"},
		{"escaped marker", `a literal \* stays`, "a literal * stays"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := strings.TrimSpace(plainText(c.md)); got != c.want {
				t.Errorf("plainText(%q)\n got %q\nwant %q", c.md, got, c.want)
			}
		})
	}
}

func TestPlainTextDropsWholeLinesThatAreOnlySyntax(t *testing.T) {
	md := "# Head\n\n---\n\n| a | b |\n| --- | :-- |\n| one | two |\n"
	got := strings.Join(strings.Fields(plainText(md)), " ")
	if got != "Head a b one two" {
		t.Errorf("got %q, want the rule and the alignment row gone", got)
	}
}

// Code inside a fence is the one place a marker means itself, so it survives.
func TestPlainTextLeavesFencedCodeAlone(t *testing.T) {
	md := "before\n\n```go\nx := *p // **not** emphasis\n```\n\nafter\n"
	got := plainText(md)
	if !strings.Contains(got, "x := *p // **not** emphasis") {
		t.Errorf("fenced code was rewritten:\n%s", got)
	}
	if strings.Contains(got, "```") {
		t.Errorf("the fence itself survived:\n%s", got)
	}
}

func TestPlainTextIsUnchangedForTextThatIsNotMarkdown(t *testing.T) {
	// Nothing here is markdown syntax, so nothing should move.
	const prose = "Fail the payments queue over to the replica, then page the on call."
	if got := strings.TrimSpace(plainText(prose)); got != prose {
		t.Errorf("got %q, want it unchanged", got)
	}
}
