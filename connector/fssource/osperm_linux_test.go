package fssource_test

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/tamnd/genba/acl"
)

// setfacl edits a file's access control list, and skips the test where the tool
// or the file system is missing.
//
// Driving the kernel rather than writing the bytes by hand is the point of
// these tests. The decoder in this package reads a layout nothing here defines,
// so the only way to find out it is being read correctly is to have somebody
// else write it.
func setfacl(t *testing.T, path string, spec ...string) {
	t.Helper()
	tool, err := exec.LookPath("setfacl")
	if err != nil {
		t.Skip("setfacl is not installed on this machine")
	}
	args := make([]string, 0, len(spec)*2+1)
	for _, s := range spec {
		args = append(args, "-m", s)
	}
	args = append(args, path)
	out, err := exec.CommandContext(t.Context(), tool, args...).CombinedOutput()
	if err != nil {
		t.Skipf("this file system does not take access control lists: %v: %s", err, out)
	}
}

// stranger is an account other than the one running the tests, which every
// machine that has a password database at all has one of.
func stranger(t *testing.T) *user.User {
	t.Helper()
	for _, name := range []string{"nobody", "daemon", "bin", "games"} {
		u, err := user.Lookup(name)
		if err == nil && u.Uid != strconv.Itoa(os.Getuid()) {
			return u
		}
	}
	t.Skip("this machine has no second account to grant anything to")
	return nil
}

// TestAnAccessControlListOnDiskNamesSomebodyTheModeBitsCannot is the reader
// against a list the kernel wrote. A file at 0600 with one extra name on it has
// a second reader that no mode bit can express, and a policy that answered from
// the mode alone would hide the file from them.
func TestAnAccessControlListOnDiskNamesSomebodyTheModeBitsCannot(t *testing.T) {
	p, root := osPolicy(t, "# a note\n")
	note := filepath.Join(root, "note.md")
	chmod(t, note, 0o600)
	guest := stranger(t)
	setfacl(t, note, "u:"+guest.Uid+":r")

	perm, err := p.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	want := acl.Ref{Source: theIdentity, Value: guest.Username}
	if !slices.Contains(perm.AllowUsers, want) {
		t.Fatalf("the list allows %v, want it to contain %+v", perm.AllowUsers, want)
	}
	if !perm.Allows(named(guest.Username)) {
		t.Errorf("%q was named on the list and refused", guest.Username)
	}
	if !perm.Allows(named(whoAmI(t))) {
		t.Error("the owner lost their own file to an entry about somebody else")
	}
}

// TestTheMaskTakesTheFileBackIsTheWholeReasonTheListIsRead is the case the mode
// bits get wrong in the direction that hands somebody a document. After this
// edit the file still shows a group read bit, and the person named on it can no
// longer open it.
func TestTheMaskTakesTheFileBackIsTheWholeReasonTheListIsRead(t *testing.T) {
	p, root := osPolicy(t, "# a note\n")
	note := filepath.Join(root, "note.md")
	chmod(t, note, 0o600)
	guest := stranger(t)
	setfacl(t, note, "u:"+guest.Uid+":r")
	setfacl(t, note, "m::-")

	info, err := os.Stat(note)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o040 != 0 {
		t.Logf("the mode now reads %04o, which is the mask and not a group grant", info.Mode().Perm())
	}

	perm, err := p.Permissions(t.Context(), "note.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	if perm.Allows(named(guest.Username)) {
		t.Fatalf("%q was refused by the mask and allowed by %+v", guest.Username, perm)
	}
	if !perm.Allows(named(whoAmI(t))) {
		t.Error("the mask took the file away from its owner, which it does not reach")
	}
}
