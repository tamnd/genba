package fssource_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector/fssource"
)

// theIdentity is the identity source the tests hand to the policy. Everything
// the policy produces is a reference under this name, and a principal only
// matches a reference that agrees about it, which is the mistake the name is
// here to make visible.
const theIdentity = "host"

// osPolicy builds a policy over a directory holding one file, and returns both.
func osPolicy(t *testing.T, body string, domains ...string) (policy *fssource.OSPolicy, root string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	policy, err := fssource.NewOSPolicy(root, "files", theIdentity, domains...)
	if err != nil {
		t.Fatal(err)
	}
	return policy, root
}

// named builds a principal holding one account name in the identity source the
// tests use.
func named(name string) *acl.Principal {
	return &acl.Principal{
		Tenant:     "acme",
		Subject:    "u_" + name,
		Identities: []acl.Identity{{Source: theIdentity, Value: name}},
	}
}

// TestTheOperatingSystemIsAskedWhoMayReadAFile is the smoke test, and it is the
// only one of these that runs everywhere. What it proves is that the platform
// half of the policy read something real: a descriptor that named nobody would
// pass every mapping test in this package and index a corpus nobody can find.
func TestTheOperatingSystemIsAskedWhoMayReadAFile(t *testing.T) {
	p, _ := osPolicy(t, "# a note\n")

	perm, err := p.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	if perm.Mode == acl.ModeUnknown {
		t.Fatalf("a file this process just wrote came back unresolved: %+v", perm)
	}
	if perm.Source != "files" {
		t.Errorf("the descriptor is attributed to %q, want %q", perm.Source, "files")
	}
	if perm.Owner.Value == "" && len(perm.AllowUsers) == 0 && len(perm.AllowGroups) == 0 {
		t.Fatalf("a file this process just wrote is readable by nobody: %+v", perm)
	}
	for _, ref := range append([]acl.Ref{perm.Owner}, perm.AllowUsers...) {
		if ref.Value != "" && ref.Source != theIdentity {
			t.Errorf("%q is written under identity source %q, want %q", ref.Value, ref.Source, theIdentity)
		}
	}
}

// TestSomebodyTheListDoesNotNameIsRefused is the default the whole package
// exists to hold. The account is one no machine has, so every path through
// Allows has to end in false.
func TestSomebodyTheListDoesNotNameIsRefused(t *testing.T) {
	p, _ := osPolicy(t, "# a note\n")

	perm, err := p.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	if perm.Mode == acl.ModePublicToTenant {
		t.Skip("this machine makes files readable by everybody with an account")
	}
	if perm.Allows(named("nobody-by-that-name")) {
		t.Fatalf("a stranger was allowed by %+v", perm)
	}
}

// TestTheNamesTheListCarriesAreTheNamesThatMatch checks the two halves of the
// rule agree. The mapping writes references and Allows compares keys, and a
// disagreement between them would be a policy that reads the file system
// correctly and lets nobody in.
func TestTheNamesTheListCarriesAreTheNamesThatMatch(t *testing.T) {
	p, _ := osPolicy(t, "# a note\n")

	perm, err := p.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	who := perm.Owner
	if who.Value == "" && len(perm.AllowUsers) > 0 {
		who = perm.AllowUsers[0]
	}
	if who.Value == "" {
		t.Skip("this machine grants the file through a group rather than a person")
	}
	if !perm.Allows(named(who.Value)) {
		t.Fatalf("%q is named on %+v and was refused", who.Value, perm)
	}
}

// TestAPathThatIsNotThereIsAnErrorRatherThanAnEmptyList matters because an
// empty list is a working answer meaning nobody may read the file. A deleted
// file that came back that way would be indexed as a document under permissions
// nobody wrote.
func TestAPathThatIsNotThereIsAnErrorRatherThanAnEmptyList(t *testing.T) {
	p, _ := osPolicy(t, "# a note\n")

	if _, err := p.Permissions(t.Context(), "gone.md"); err == nil {
		t.Error("a path that is not there was given an access control list")
	}
	if _, err := p.ChangedAt(t.Context(), "gone.md"); err == nil {
		t.Error("a path that is not there was given a change time")
	}
}

// TestTheIdentitySourceHasToBeNamed is the one piece of configuration that
// cannot be guessed. Without it every reference is written under an empty
// source, which matches whatever a principal happens to carry.
func TestTheIdentitySourceHasToBeNamed(t *testing.T) {
	root := t.TempDir()
	for _, c := range []struct {
		name             string
		source, identity string
	}{
		{"no connector name", "", theIdentity},
		{"no identity source", "files", ""},
	} {
		if _, err := fssource.NewOSPolicy(root, c.source, c.identity); err == nil {
			t.Errorf("a policy with %s was built without complaint", c.name)
		}
	}
}

// TestReloadKeepsAnswering is a small thing with a large failure. Reload is
// called at the start of every walk, and a policy that cleared the wrong state
// or left a lock held would take the next sync of the tree with it.
func TestReloadKeepsAnswering(t *testing.T) {
	p, _ := osPolicy(t, "# a note\n")

	before, err := p.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	p.Reload()
	after, err := p.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions after a reload: %v", err)
	}
	if after.Owner != before.Owner || after.Mode != before.Mode {
		t.Fatalf("a reload changed the answer from %+v to %+v", before, after)
	}
	if c := p.Counts(); c.Mapped < 2 {
		t.Errorf("the mapping counted %d documents, want at least 2", c.Mapped)
	}
}
