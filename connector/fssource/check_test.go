package fssource_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector/fssource"
)

// checkTree is a repository with one rule at the root and one below it, which
// is the arrangement every question here is about: who the rule names now,
// rather than who it named when the crawler went past.
func checkTree(t *testing.T) string {
	t.Helper()
	return tree(t, map[string]string{
		"OWNERS":        "approvers:\n  - alice\nreviewers:\n  - bob\n",
		"README.md":     "# top\n",
		"net/OWNERS":    "approvers:\n  - erin\n",
		"net/design.md": "# networking\n",
	})
}

func checker(t *testing.T, root string) *fssource.Checker {
	t.Helper()
	c, err := fssource.NewChecker(root, "repo", policyFor(t, root))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func github(name string) *acl.Principal {
	return &acl.Principal{Tenant: "acme", Subject: "u_" + name, Kind: acl.KindUser,
		Identities: []acl.Identity{{Source: "github", Value: name}}}
}

// rewrite replaces a file in the tree, which is what somebody taking an access
// away looks like on a filesystem.
func rewrite(t *testing.T, root, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTheRuleIsReadAgainRatherThanRemembered(t *testing.T) {
	root := checkTree(t)
	c := checker(t, root)
	erin := github("erin")

	got, err := c.Allowed(t.Context(), erin, []string{"repo:net/design.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !got["repo:net/design.md"] {
		t.Fatal("erin is an approver of net and was refused it")
	}

	// The edit a sync would not see for another hour. Nothing about the document
	// changed, only the rule above it.
	rewrite(t, root, "net/OWNERS", "approvers:\n  - frank\n")

	got, err = c.Allowed(t.Context(), erin, []string{"repo:net/design.md"})
	if err != nil {
		t.Fatal(err)
	}
	if got["repo:net/design.md"] {
		t.Error("erin was taken off the file and the check still says yes, which is the cached answer this exists to go around")
	}
	got, err = c.Allowed(t.Context(), github("frank"), []string{"repo:net/design.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !got["repo:net/design.md"] {
		t.Error("frank was put on the file and cannot read it")
	}
}

func TestAFileThatIsGoneIsRefused(t *testing.T) {
	root := checkTree(t)
	c := checker(t, root)

	if err := os.Remove(filepath.Join(root, "net", "design.md")); err != nil {
		t.Fatal(err)
	}
	got, err := c.Allowed(t.Context(), github("erin"), []string{"repo:net/design.md"})
	if err != nil {
		t.Fatal(err)
	}
	if allowed, ok := got["repo:net/design.md"]; !ok || allowed {
		t.Errorf("a deleted file answered %v, %v, want a plain refusal", allowed, ok)
	}
}

func TestADirectoryIsNotADocument(t *testing.T) {
	root := checkTree(t)
	c := checker(t, root)

	got, err := c.Allowed(t.Context(), github("alice"), []string{"repo:net"})
	if err != nil {
		t.Fatal(err)
	}
	if got["repo:net"] {
		t.Error("a directory was served as a document")
	}
}

// An id this checker did not mint is not this checker's to answer for, and the
// caller reads the silence as a check that did not happen.
func TestAnIdFromAnotherSourceIsLeftAlone(t *testing.T) {
	root := checkTree(t)
	c := checker(t, root)

	got, err := c.Allowed(t.Context(), github("alice"), []string{"wiki:README.md", "README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("answered %v for ids belonging to somebody else", got)
	}
}

// The ids come out of the index, the index was written by a connector, and a
// connector is a lot of code to trust with a string that turns into a path.
func TestAPathThatTriesToLeaveTheTreeGetsNothing(t *testing.T) {
	root := checkTree(t)
	c := checker(t, root)

	outside := filepath.Join(filepath.Dir(root), "elsewhere.md")
	if err := os.WriteFile(outside, []byte("# not in the corpus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, id := range []string{
		"repo:../elsewhere.md",
		"repo:net/../../elsewhere.md",
		"repo:/etc/passwd",
	} {
		got, err := c.Allowed(t.Context(), github("alice"), []string{id})
		if err != nil {
			t.Fatal(err)
		}
		if got[id] {
			t.Errorf("%s was allowed, which is a path that left the tree", id)
		}
	}
}

func TestOnePageIsOneReadOfTheRules(t *testing.T) {
	root := checkTree(t)
	c := checker(t, root)

	got, err := c.Allowed(t.Context(), github("alice"), []string{
		"repo:README.md", "repo:net/design.md", "repo:missing.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got["repo:README.md"] {
		t.Error("alice is an approver at the root and was refused a file there")
	}
	if got["repo:net/design.md"] {
		t.Error("alice was allowed a subtree whose own rule replaced her")
	}
	if allowed, ok := got["repo:missing.md"]; !ok || allowed {
		t.Errorf("a file that is not there answered %v, %v", allowed, ok)
	}
}

// A check that ran out of time hands back what it decided and says why. The ids
// it did not reach are missing rather than allowed, which is how the caller
// tells the two apart.
func TestACheckThatIsCancelledAnswersForNobodyElse(t *testing.T) {
	root := checkTree(t)
	c := checker(t, root)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, err := c.Allowed(ctx, github("alice"), []string{"repo:README.md", "repo:net/design.md"})
	if err == nil {
		t.Fatal("a cancelled check reported no error")
	}
	if len(got) != 0 {
		t.Errorf("answered %v after being cancelled", got)
	}
}

func TestACheckerWithoutAPolicyIsRefusedAtTheDoor(t *testing.T) {
	root := checkTree(t)

	if _, err := fssource.NewChecker(root, "repo", nil); err == nil {
		t.Error("a checker with no policy was built, and it would refuse every document in the tree")
	}
	if _, err := fssource.NewChecker(root, "", policyFor(t, root)); err == nil {
		t.Error("a checker with no source name was built, and no id would ever match it")
	}
}
