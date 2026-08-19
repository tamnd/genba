package fssource_test

import (
	"slices"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector/fssource"
)

// The shape of a real OWNERS file, comments and all.
const k8sStyle = `# See the OWNERS docs at https://go.k8s.io/owners

approvers:
  - alice
  - bob   # on leave until March
reviewers:
  - carol
  - dave
labels:
  - sig/network
`

func ownersTree(t *testing.T) string {
	t.Helper()
	return tree(t, map[string]string{
		"OWNERS":              k8sStyle,
		"README.md":           "# top\n",
		"net/OWNERS":          "approvers:\n  - erin\nreviewers:\n  - frank\n",
		"net/design.md":       "# networking\n",
		"net/deep/detail.md":  "# deeper\n",
		"docs/guide.md":       "# guide\n",
		"unowned/notes.md":    "# notes\n",
		"inline/OWNERS":       "approvers: [grace, heidi]\n",
		"inline/thing.md":     "# thing\n",
		"broken/OWNERS":       "approvers: &anchor\n  <<: *defaults\n",
		"broken/mystery.md":   "# mystery\n",
		"emptyown/OWNERS":     "# nobody listed\n",
		"emptyown/orphan.md":  "# orphan\n",
		"labelsfirst/OWNERS":  "labels:\n  - sig/api\napprovers:\n  - ivan\n",
		"labelsfirst/spec.md": "# spec\n",
	})
}

func policyFor(t *testing.T, root string) *fssource.OwnersPolicy {
	t.Helper()
	p, err := fssource.NewOwnersPolicy(root, "repo", "github")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func allowedUsers(p acl.Permissions) []string {
	out := make([]string, 0, len(p.AllowUsers))
	for _, r := range p.AllowUsers {
		out = append(out, r.Value)
	}
	return out
}

func has(list []string, want string) bool {
	return slices.Contains(list, want)
}

func TestTheNearestOwnersFileWins(t *testing.T) {
	root := ownersTree(t)
	policy := policyFor(t, root)

	// A file directly under an OWNERS file gets that file's people.
	got, err := policy.Permissions(t.Context(), "net/design.md")
	if err != nil {
		t.Fatalf("net/design.md: %v", err)
	}
	users := allowedUsers(got)
	if !has(users, "erin") || !has(users, "frank") {
		t.Errorf("net/design.md allows %v, want the net owners", users)
	}
	// And not the ones from the root file it overrides.
	if has(users, "alice") {
		t.Errorf("net/design.md allows %v, which includes the root approvers the subtree replaced", users)
	}

	// A file deeper down with no OWNERS of its own inherits the nearest above.
	deep, err := policy.Permissions(t.Context(), "net/deep/detail.md")
	if err != nil {
		t.Fatalf("net/deep/detail.md: %v", err)
	}
	if !has(allowedUsers(deep), "erin") {
		t.Errorf("net/deep/detail.md allows %v, want the nearest owners above it", allowedUsers(deep))
	}

	// A file under a directory with no OWNERS falls back to the root file.
	top, err := policy.Permissions(t.Context(), "docs/guide.md")
	if err != nil {
		t.Fatalf("docs/guide.md: %v", err)
	}
	if !has(allowedUsers(top), "alice") || !has(allowedUsers(top), "carol") {
		t.Errorf("docs/guide.md allows %v, want the root approvers and reviewers", allowedUsers(top))
	}
}

func TestCommentsAndInlineListsAreRead(t *testing.T) {
	root := ownersTree(t)
	policy := policyFor(t, root)

	got, err := policy.Permissions(t.Context(), "README.md")
	if err != nil {
		t.Fatal(err)
	}
	users := allowedUsers(got)
	if !has(users, "bob") {
		t.Errorf("allows %v, want bob with the trailing comment stripped", users)
	}
	for _, v := range users {
		if v == "on leave until March" || v == "sig/network" {
			t.Errorf("allows %q, which is a comment or a label rather than a person", v)
		}
	}

	inline, err := policy.Permissions(t.Context(), "inline/thing.md")
	if err != nil {
		t.Fatal(err)
	}
	if !has(allowedUsers(inline), "grace") || !has(allowedUsers(inline), "heidi") {
		t.Errorf("inline list gave %v", allowedUsers(inline))
	}
}

// A key that is not a list of people must not swallow the values under it.
func TestAnotherKeyEndsTheList(t *testing.T) {
	root := ownersTree(t)
	policy := policyFor(t, root)

	got, err := policy.Permissions(t.Context(), "labelsfirst/spec.md")
	if err != nil {
		t.Fatal(err)
	}
	users := allowedUsers(got)
	if !has(users, "ivan") {
		t.Errorf("allows %v, want ivan", users)
	}
	if has(users, "sig/api") {
		t.Errorf("allows %v, which includes a label read as a person", users)
	}
}

func TestTheFirstApproverIsTheOwner(t *testing.T) {
	root := ownersTree(t)
	policy := policyFor(t, root)

	got, err := policy.Permissions(t.Context(), "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner.Value != "alice" || got.Owner.Source != "github" {
		t.Errorf("owner is %+v, want alice from github", got.Owner)
	}
	if got.Source != "repo" {
		t.Errorf("permission source is %q, want the connector name", got.Source)
	}
	if got.Mode != acl.ModeACL {
		t.Errorf("mode is %v, want an access control list", got.Mode)
	}
}

// The case worth getting right. No rule found is not the same as no
// restriction.
func TestAPathWithNoOwnersFileIsRefusedRatherThanOpened(t *testing.T) {
	// A tree whose root has no OWNERS file at all.
	root := tree(t, map[string]string{
		"loose/notes.md": "# notes\n",
	})
	policy := policyFor(t, root)

	if _, err := policy.Permissions(t.Context(), "loose/notes.md"); err == nil {
		t.Fatal("a path governed by no OWNERS file was given permissions")
	}
}

// An OWNERS file this parser cannot understand yields nobody, which quarantines
// its subtree rather than opening it.
func TestAnUnparseableOwnersFileAllowsNobody(t *testing.T) {
	root := ownersTree(t)
	policy := policyFor(t, root)

	for _, path := range []string{"broken/mystery.md", "emptyown/orphan.md"} {
		got, err := policy.Permissions(t.Context(), path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(got.AllowUsers) != 0 {
			t.Errorf("%s allows %v, want nobody", path, allowedUsers(got))
		}
		p := &acl.Principal{Tenant: "acme", Subject: "alice", Kind: acl.KindUser,
			Identities: []acl.Identity{{Source: "github", Value: "alice"}}}
		if got.Allows(p) {
			t.Errorf("%s is readable by somebody named in a file above it", path)
		}
	}
}

func TestAFallbackCoversThePathsNoOwnersFileGoverns(t *testing.T) {
	root := tree(t, map[string]string{"loose/notes.md": "# notes\n"})
	policy := policyFor(t, root).WithFallback(acl.Permissions{
		Mode:   acl.ModePublicToTenant,
		Source: "repo",
	})

	got, err := policy.Permissions(t.Context(), "loose/notes.md")
	if err != nil {
		t.Fatalf("with a fallback set this should resolve: %v", err)
	}
	if got.Mode != acl.ModePublicToTenant {
		t.Errorf("mode is %v, want the fallback", got.Mode)
	}
}

// The whole point of the mapping: a principal who is in the file can read, and
// one who is not cannot.
func TestOwnersDecideWhoCanRead(t *testing.T) {
	root := ownersTree(t)
	policy := policyFor(t, root)

	perms, err := policy.Permissions(t.Context(), "net/design.md")
	if err != nil {
		t.Fatal(err)
	}

	erin := &acl.Principal{Tenant: "acme", Subject: "u1", Kind: acl.KindUser,
		Identities: []acl.Identity{{Source: "github", Value: "erin"}}}
	alice := &acl.Principal{Tenant: "acme", Subject: "u2", Kind: acl.KindUser,
		Identities: []acl.Identity{{Source: "github", Value: "alice"}}}

	if !perms.Allows(erin) {
		t.Error("erin is an approver of net and cannot read it")
	}
	if perms.Allows(alice) {
		t.Error("alice is only an approver of the root and can read a subtree that replaced her")
	}
	if perms.Allows(nil) {
		t.Error("a nil principal was allowed")
	}
}

// The policy is asked once per file in a large tree, so the same directory is
// looked up thousands of times.
func TestRepeatedLookupsAgree(t *testing.T) {
	root := ownersTree(t)
	policy := policyFor(t, root)

	first, err := policy.Permissions(t.Context(), "net/deep/detail.md")
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		got, err := policy.Permissions(t.Context(), "net/deep/detail.md")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.AllowUsers) != len(first.AllowUsers) || got.Owner != first.Owner {
			t.Fatalf("a repeated lookup gave a different answer: %+v then %+v", first, got)
		}
	}
}

func TestNewOwnersPolicyRejectsNothingUsable(t *testing.T) {
	if _, err := fssource.NewOwnersPolicy(t.TempDir(), "repo", "github"); err != nil {
		t.Errorf("a real directory was refused: %v", err)
	}
}
