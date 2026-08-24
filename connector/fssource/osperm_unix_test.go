//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package fssource_test

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/fssource"
)

// whoAmI is the account name the policy will resolve this process to, asked for
// the same way the policy asks so that a machine with an unusual password
// database does not turn a real failure into a puzzle.
func whoAmI(t *testing.T) string {
	t.Helper()
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Skipf("this machine cannot name the account it is running as: %v", err)
	}
	return u.Username
}

// groupOf is the name of the group a file belongs to, which on the BSDs is
// inherited from the directory rather than taken from the process.
func groupOf(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("this file system does not report a group")
	}
	g, err := user.LookupGroupId(strconv.FormatUint(uint64(st.Gid), 10))
	if err != nil {
		t.Skipf("this machine cannot name group %d: %v", st.Gid, err)
	}
	return g.Name
}

// inGroup builds a principal whose only claim is membership of one group.
func inGroup(group string) *acl.Principal {
	return &acl.Principal{
		Tenant:  "acme",
		Subject: "u_visitor",
		Groups:  acl.GroupSet{Members: []string{theIdentity + ":" + group}},
	}
}

// chmod is os.Chmod with the failure reported where it happened, because a mode
// that quietly did not take turns every assertion below into a lie.
func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TestAFileOnlyItsOwnerCanReadIsOwnedRatherThanShared is the mode a feature
// such as everything of mine reads. A list with one name in it behaves the same
// way today and does not mean the same thing.
func TestAFileOnlyItsOwnerCanReadIsOwnedRatherThanShared(t *testing.T) {
	p, root := osPolicy(t, "# a note\n")
	chmod(t, filepath.Join(root, "note.md"), 0o600)
	me := whoAmI(t)

	perm, err := p.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	if perm.Mode != acl.ModeOwnerOnly {
		t.Errorf("mode 0600 came out as %v, want owner only", perm.Mode)
	}
	if want := (acl.Ref{Source: theIdentity, Value: me}); perm.Owner != want {
		t.Errorf("the owner is %+v, want %+v", perm.Owner, want)
	}
	if len(perm.AllowUsers) != 0 || len(perm.AllowGroups) != 0 {
		t.Errorf("a private file also allows %v and %v", perm.AllowUsers, perm.AllowGroups)
	}
	if !perm.Allows(named(me)) {
		t.Error("the owner cannot read their own file")
	}
	if perm.Allows(named("somebody-else")) {
		t.Error("a private file was shown to somebody else")
	}
}

func TestTheGroupBitIsAGrantToTheGroup(t *testing.T) {
	p, root := osPolicy(t, "# a note\n")
	note := filepath.Join(root, "note.md")
	chmod(t, note, 0o640)
	group := groupOf(t, note)

	perm, err := p.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	if perm.Mode != acl.ModeACL {
		t.Errorf("mode 0640 came out as %v, want an access control list", perm.Mode)
	}
	want := acl.Ref{Source: theIdentity, Value: group}
	if !slices.Contains(perm.AllowGroups, want) {
		t.Fatalf("the group list is %v, want it to contain %+v", perm.AllowGroups, want)
	}
	if !perm.Allows(inGroup(group)) {
		t.Errorf("somebody in %q was refused a file their group can read", group)
	}
	if perm.Allows(inGroup("some-other-team")) {
		t.Errorf("somebody in another group was allowed by %+v", perm)
	}
}

// TestTheWorldBitGrantsNothingUntilSomebodySaysWhoTheWorldIs is the one
// mapping in this policy that cannot be made without configuration. Every
// account on this host is not a tenant: on a laptop it is one person and on a
// login server it is the company, and only the deployment knows which.
func TestTheWorldBitGrantsNothingUntilSomebodySaysWhoTheWorldIs(t *testing.T) {
	silent, root := osPolicy(t, "# a note\n")
	chmod(t, filepath.Join(root, "note.md"), 0o644)

	perm, err := silent.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	if perm.Mode != acl.ModeACL {
		t.Errorf("without a domain, mode 0644 came out as %v, want an access control list", perm.Mode)
	}
	if perm.Allows(named("anybody-with-a-login")) {
		t.Errorf("the world bit was read as a tenant nobody named: %+v", perm)
	}

	told, err := fssource.NewOSPolicy(root, "files", theIdentity, "acme.example")
	if err != nil {
		t.Fatal(err)
	}
	perm, err = told.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	if perm.Mode != acl.ModePublicToTenant {
		t.Errorf("with a domain, mode 0644 came out as %v, want public to the tenant", perm.Mode)
	}
	if !perm.Allows(named("anybody-with-a-login")) {
		t.Errorf("a world readable file was refused to the tenant it belongs to: %+v", perm)
	}
}

func TestAFileNobodyCanReadAllowsNobody(t *testing.T) {
	p, root := osPolicy(t, "# a note\n")
	note := filepath.Join(root, "note.md")
	chmod(t, note, 0o000)
	me := whoAmI(t)

	perm, err := p.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	// Not unresolved. Nobody may read it is an answer the file system gave,
	// and it is a different fact from nobody could work out who may read it.
	if perm.Mode == acl.ModeUnknown {
		t.Fatalf("a file with no read bits came back unresolved: %+v", perm)
	}
	if perm.Owner.Value != "" || len(perm.AllowUsers) != 0 || len(perm.AllowGroups) != 0 {
		t.Fatalf("a file with no read bits still names somebody: %+v", perm)
	}
	if perm.Allows(named(me)) {
		t.Error("the owner was allowed a file they took their own read bit away from")
	}
	if perm.Allows(inGroup(groupOf(t, note))) {
		t.Error("the group was allowed a file with no group read bit")
	}
}

// TestAChmodMovesTheChangeTimeAndNotTheModificationTime is what makes the sync
// below possible at all. A revocation that waits for somebody to edit the file
// is not a revocation.
func TestAChmodMovesTheChangeTimeAndNotTheModificationTime(t *testing.T) {
	p, root := osPolicy(t, "# a note\n")
	note := filepath.Join(root, "note.md")

	before, err := p.ChangedAt(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("changed at: %v", err)
	}
	if before.IsZero() {
		t.Fatal("this platform has no change time, so a chmod can never be noticed")
	}
	info, err := os.Stat(note)
	if err != nil {
		t.Fatal(err)
	}
	modified := info.ModTime()

	// Filesystems with a coarse clock would otherwise report the two events in
	// the same tick, and the comparison would pass or fail on timing.
	time.Sleep(10 * time.Millisecond)
	chmod(t, note, 0o600)

	after, err := p.ChangedAt(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("changed at: %v", err)
	}
	if !after.After(before) {
		t.Errorf("a chmod left the change time at %v", after)
	}
	info, err = os.Stat(note)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(modified) {
		t.Errorf("a chmod moved the modification time from %v to %v, so the file was rewritten",
			modified, info.ModTime())
	}
}

// TestAChmodIsAPermissionChangeAndNotARecrawl is the box, end to end and on the
// permission model the operating system keeps. Taking read away from one file
// has to reach the index without the sync reading a single byte of the tree.
func TestAChmodIsAPermissionChangeAndNotARecrawl(t *testing.T) {
	root := tree(t, map[string]string{
		"a.md":      "# a\n",
		"b.md":      "# b\n",
		"docs/c.md": "# c\n",
	})
	policy, err := fssource.NewOSPolicy(root, "repo", theIdentity)
	if err != nil {
		t.Fatal(err)
	}
	s, err := fssource.New(root, "repo", policy)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A second between writing the tree and reading it, which the rest of these
	// tests get by dating the files they write. This one cannot: the time that
	// matters here is the inode change time, which is set when the file is made
	// and cannot be set to anything else afterwards. Without the wait the first
	// sync hands back a cursor that stops short of a tree made this second, and
	// every file in it looks like a rule that just moved.
	time.Sleep(1100 * time.Millisecond)

	first, cursor := collect(t, s, connector.Cursor{})
	if len(first) != 3 {
		t.Fatalf("the first sync read %v, want three documents", ids(first))
	}
	for _, d := range first {
		if d.Permissions.Mode != acl.ModeACL {
			t.Fatalf("%s starts at %v, want an access control list with the group on it",
				d.ID, d.Permissions.Mode)
		}
	}
	after := s.Counters()

	chmod(t, filepath.Join(root, "a.md"), 0o600)

	// The same second again. The chmod has to be unambiguously past the cursor
	// on a filesystem with a coarse timestamp, and it has to be far enough back
	// for the sync that finds it to hand back a cursor sitting on it, because
	// the two syncs below are there to say the change is reported once.
	time.Sleep(1100 * time.Millisecond)

	var changes []connector.Change
	next, err := s.Sync(t.Context(), cursor, func(_ context.Context, ch connector.Change) error {
		changes = append(changes, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	if len(changes) != 1 {
		got := make([]string, 0, len(changes))
		for _, ch := range changes {
			got = append(got, ch.Document.ID)
		}
		t.Fatalf("one chmod produced changes for %v, want repo:a.md alone", got)
	}
	got := changes[0]
	if !got.PermissionsOnly {
		t.Errorf("%s came back as a content change, so the file was read again", got.Document.ID)
	}
	if got.Document.ID != "repo:a.md" {
		t.Errorf("the change is for %s, want repo:a.md", got.Document.ID)
	}
	if mode := got.Document.Permissions.Mode; mode != acl.ModeOwnerOnly {
		t.Errorf("the file is now %v, want owner only", mode)
	}

	spent := s.Counters().Since(after)
	if spent.Fetches != 0 || spent.Bytes != 0 {
		t.Errorf("applying a permission change read %d files and %d bytes, so it was a recrawl",
			spent.Fetches, spent.Bytes)
	}

	// And once. A cursor that did not move past the chmod would replay it on
	// every sync from here on.
	changes = nil
	if _, err := s.Sync(t.Context(), next, func(_ context.Context, ch connector.Change) error {
		changes = append(changes, ch)
		return nil
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("the chmod was reported again on the next sync: %d changes", len(changes))
	}
}
