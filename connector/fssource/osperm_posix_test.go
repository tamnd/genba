//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package fssource

import (
	"encoding/binary"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/tamnd/genba/connector/aclmap"
)

// show writes a list of rules the way a failure should read, because a
// difference between two slices of a struct with five fields is otherwise a
// wall nobody reads twice.
func show(rules []rule) string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		s := r.subject.String() + " " + r.id
		if r.deny {
			s = "deny " + s
		}
		if r.owner {
			s += " (owner)"
		}
		out = append(out, s)
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// TestOnlyTheReadBitsBecomeGrants is the whole of the Unix mapping. Write
// access is not read access, and a mode that granted on any bit at all would
// hand a document to everybody who can drop a file next to it.
func TestOnlyTheReadBitsBecomeGrants(t *testing.T) {
	const (
		uid = 501
		gid = 20
	)
	owner := rule{subject: aclmap.User, id: "501", owner: true}
	group := rule{subject: aclmap.Group, id: "20"}
	world := rule{subject: aclmap.Domain}

	for _, c := range []struct {
		mode fs.FileMode
		want []rule
		why  string
	}{
		{0o600, []rule{owner}, "a file only its owner can read"},
		{0o640, []rule{owner, group}, "the group can read it too"},
		{0o644, []rule{owner, group, world}, "and so can everybody with an account"},
		{0o604, []rule{owner, world}, "a hole in the middle is a hole in the middle"},
		{0o060, []rule{group}, "an owner who took away their own read bit is not a reader"},
		{0o000, nil, "a file nobody can read"},
		{0o222, nil, "writing is not reading"},
		{0o111, nil, "nor is running"},
		{0o333, nil, "nor is both of them at once"},
	} {
		got := modeRules(c.mode, uid, gid)
		if !slices.Equal(got, c.want) {
			t.Errorf("mode %04o gave %s, want %s: %s", c.mode, show(got), show(c.want), c.why)
		}
	}
}

// entry is the on disk form of one access control list entry.
func entry(tag, perm uint16, id uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b, tag)
	binary.LittleEndian.PutUint16(b[2:], perm)
	binary.LittleEndian.PutUint32(b[4:], id)
	return b
}

// list is a whole access control list as the file system stores it.
func list(version uint32, entries ...[]byte) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, version)
	for _, e := range entries {
		b = append(b, e...)
	}
	return b
}

func TestAnAccessControlListNamesPeopleTheModeBitsCannot(t *testing.T) {
	got := aclRules([]aclEntry{
		{tag: aclUserObj, perm: 0o6},
		{tag: aclUser, perm: 0o4, id: 1001},
		{tag: aclGroupObj, perm: 0o4},
		{tag: aclGroup, perm: 0o4, id: 2002},
		{tag: aclMask, perm: 0o7},
		{tag: aclOther, perm: 0o0},
	}, 501, 20)

	want := []rule{
		{subject: aclmap.User, id: "501", owner: true},
		{subject: aclmap.User, id: "1001"},
		{subject: aclmap.Group, id: "20"},
		{subject: aclmap.Group, id: "2002"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the list gave %s, want %s", show(got), show(want))
	}
}

// TestTheMaskIsACeilingAndNotADecoration is the reason this package reads the
// list at all. A file with a mask that has taken read away still shows a group
// read bit in its mode, so a mapping built on the mode alone offers the file to
// a team that cannot open it.
func TestTheMaskIsACeilingAndNotADecoration(t *testing.T) {
	entries := []aclEntry{
		{tag: aclUserObj, perm: 0o6},
		{tag: aclUser, perm: 0o4, id: 1001},
		{tag: aclGroupObj, perm: 0o4},
		{tag: aclGroup, perm: 0o4, id: 2002},
		{tag: aclMask, perm: 0o2},
		{tag: aclOther, perm: 0o4},
	}
	got := aclRules(entries, 501, 20)

	// The owner and the world are the two the mask does not reach, which is
	// what POSIX says and what makes the answer look odd at first reading.
	want := []rule{
		{subject: aclmap.User, id: "501", owner: true},
		{subject: aclmap.Domain},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("a mask of 2 gave %s, want %s", show(got), show(want))
	}
}

// TestAListWithNoMaskGrantsWhatItSays covers the ordering trap. The mask can
// sit anywhere in the list, including after the entries it governs, so a reader
// that applied it as it went would let through whatever came first.
func TestAListWithNoMaskGrantsWhatItSays(t *testing.T) {
	got := aclRules([]aclEntry{
		{tag: aclGroup, perm: 0o4, id: 2002},
		{tag: aclUserObj, perm: 0o4},
	}, 501, 20)

	want := []rule{
		{subject: aclmap.Group, id: "2002"},
		{subject: aclmap.User, id: "501", owner: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("a list without a mask gave %s, want %s", show(got), show(want))
	}

	// And with the mask last, the group it refuses is gone even though the
	// entry was read before the mask was.
	got = aclRules([]aclEntry{
		{tag: aclGroup, perm: 0o4, id: 2002},
		{tag: aclUserObj, perm: 0o4},
		{tag: aclMask, perm: 0o1},
	}, 501, 20)
	want = []rule{{subject: aclmap.User, id: "501", owner: true}}
	if !slices.Equal(got, want) {
		t.Fatalf("a trailing mask gave %s, want %s", show(got), show(want))
	}
}

func TestAnAccessControlListIsReadOffTheDisk(t *testing.T) {
	raw := list(2,
		entry(aclUserObj, 0o6, 0),
		entry(aclUser, 0o4, 1001),
		entry(aclMask, 0o7, 0),
		entry(aclOther, 0o0, 0),
	)
	got, err := decodePosixACL(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []aclEntry{
		{tag: aclUserObj, perm: 0o6},
		{tag: aclUser, perm: 0o4, id: 1001},
		{tag: aclMask, perm: 0o7},
		{tag: aclOther, perm: 0o0},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("decoded %v, want %v", got, want)
	}
}

// TestAListThisReaderDoesNotUnderstandIsRefused is the one place a wrong answer
// would be silent. Every one of these produces entries that name real accounts
// at random, and an access control list nobody wrote is worse than none.
func TestAListThisReaderDoesNotUnderstandIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  []byte
	}{
		{"shorter than its header", []byte{2, 0}},
		{"a version from the future", list(3, entry(aclUserObj, 0o4, 0))},
		{"a version from before there was one", list(0)},
		{"ending inside an entry", append(list(2, entry(aclUserObj, 0o4, 0)), 1, 2, 3)},
	} {
		if _, err := decodePosixACL(c.raw); err == nil {
			t.Errorf("a list %s was read without complaint", c.name)
		}
	}

	// An empty list is a list, and it is a file nobody may read rather than a
	// broken read. The two have to stay distinguishable.
	got, err := decodePosixACL(list(2))
	if err != nil {
		t.Fatalf("an empty list was refused: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an empty list decoded to %v", got)
	}
}
