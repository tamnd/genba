package fssource_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/fssource"
	"github.com/tamnd/genba/doc"
)

// enumerate runs one enumeration and returns what it listed, sorted.
func enumerate(t *testing.T, s *fssource.Source) []connector.Item {
	t.Helper()
	var got []connector.Item
	if err := s.Enumerate(t.Context(), func(it connector.Item) bool {
		got = append(got, it)
		return true
	}); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	slices.SortFunc(got, func(a, b connector.Item) int { return strings.Compare(a.ID, b.ID) })
	return got
}

func itemIDs(items []connector.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// TestEnumerateListsWhatTheSyncIndexes is the property reconciliation depends
// on. If the two walks disagreed about which files are in the corpus, a sweep
// would report drift that is not there and then delete real documents to fix
// it.
func TestEnumerateListsWhatTheSyncIndexes(t *testing.T) {
	root := tree(t, map[string]string{
		"README.md":           "# The project\n",
		"docs/install.md":     "# Installing\n",
		"docs/deep/nested.md": "a line\n",
		"cmd/main.go":         "package main\n",
		".hidden/secret.md":   "# not in the corpus\n",
		"node_modules/dep.js": "module.exports = 1",
		"notes.unknownxt":     "not an extension we read",
	})
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	indexed, _ := collect(t, s, connector.Cursor{})
	listed := enumerate(t, s)

	if !slices.Equal(itemIDs(listed), ids(indexed)) {
		t.Fatalf("enumerate listed %v\nsync indexed %v", itemIDs(listed), ids(indexed))
	}

	// The version has to be the same string the document carries, because that
	// is what an inventory compares against to decide a document is stale.
	stored := map[string]string{}
	for _, d := range indexed {
		stored[d.ID] = d.SourceUpdate
	}
	for _, it := range listed {
		if it.Version == "" {
			t.Errorf("%s was listed without a version, so nothing can tell whether it moved on", it.ID)
		}
		if it.Version != stored[it.ID] {
			t.Errorf("%s is listed at version %q and stored at %q", it.ID, it.Version, stored[it.ID])
		}
	}
}

func TestEnumerateStopsWhenTheCallerSaysSo(t *testing.T) {
	root := tree(t, map[string]string{"a.md": "1", "b.md": "2", "c.md": "3", "d.md": "4"})
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}

	seen := 0
	err = s.Enumerate(t.Context(), func(connector.Item) bool {
		seen++
		return seen < 2
	})
	// Stopping on purpose is not a failed walk. A reconciliation that read it as
	// one would take a short list for a source that lost its documents.
	if err != nil {
		t.Fatalf("an early stop was reported as an error: %v", err)
	}
	if seen != 2 {
		t.Errorf("the walk listed %d items after being told to stop at 2", seen)
	}
}

// TestEnumerateReadsNothing is the reason a sweep is affordable. Listing a tree
// costs a stat per file, and the whole point of doing it on a schedule is that
// it does not cost a read per file.
func TestEnumerateReadsNothing(t *testing.T) {
	root := tree(t, map[string]string{
		"a.md": "# a\n", "b.md": "# b\n", "docs/c.md": "# c\n",
	})
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}

	before := s.Counters()
	if listed := enumerate(t, s); len(listed) != 3 {
		t.Fatalf("listed %d files, want 3", len(listed))
	}
	spent := s.Counters().Since(before)

	if spent.Fetches != 0 || spent.Bytes != 0 {
		t.Errorf("an enumeration read %d files and %d bytes, want none", spent.Fetches, spent.Bytes)
	}
	if spent.Metadata != 3 {
		t.Errorf("an enumeration statted %d files, want 3", spent.Metadata)
	}
}

func TestFetchReturnsTheDocumentForAnID(t *testing.T) {
	root := tree(t, map[string]string{
		"docs/install.md": "# Installing\n\nRun the thing.\n",
	})
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}

	indexed, _ := collect(t, s, connector.Cursor{})
	if len(indexed) != 1 {
		t.Fatalf("indexed %d documents, want 1", len(indexed))
	}

	got, err := s.Fetch(t.Context(), "repo:docs/install.md")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// A repaired document has to be the same document a sync would have
	// produced, or the two ways into the index disagree and which one a
	// document came through becomes something a reader can tell.
	if got.ID != indexed[0].ID || got.Title != indexed[0].Title || got.Body != indexed[0].Body {
		t.Errorf("fetch returned %+v\nsync produced %+v", got, indexed[0])
	}
	if got.Kind != doc.KindPage || got.SourceUpdate == "" {
		t.Errorf("fetch returned kind %q and version %q", got.Kind, got.SourceUpdate)
	}
}

// TestFetchSaysGoneRatherThanFailing covers every way an id can fail to name a
// file this source will read. All of them are the same answer, because the
// caller does the same thing with all of them: take the document out of the
// index.
func TestFetchSaysGoneRatherThanFailing(t *testing.T) {
	root := tree(t, map[string]string{
		"a.md":       "# a\n",
		"docs/b.md":  "# b\n",
		"huge.md":    string(make([]byte, 4096)),
		"secret.txt": "not part of this source\n",
	})
	// A file outside the tree, which a traversal would reach if the id were
	// used as a path without cleaning it.
	outside := filepath.Join(filepath.Dir(root), "outside.md")
	if err := os.WriteFile(outside, []byte("# outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"), fssource.WithMaxFileSize(1024))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"a file that is not there", "repo:gone.md"},
		{"an id another source minted", "other:a.md"},
		{"an id with no path in it", "repo:"},
		{"a directory", "repo:docs"},
		{"a file over the size limit", "repo:huge.md"},
		{"a path that tries to leave the tree", "repo:../" + filepath.Base(outside)},
		{"a path that tries harder", "repo:docs/../../../../etc/passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Fetch(t.Context(), tc.id)
			if !errors.Is(err, connector.ErrGone) {
				t.Fatalf("fetch %q returned (%q, %v), want ErrGone", tc.id, got.ID, err)
			}
		})
	}
}

// TestASecondSyncOverAnUnchangedTreeReadsNothing is the first box of the
// incremental work, measured the way the box asks for it. The walk is
// unavoidable on a filesystem, which has no change feed, but the reads are not,
// and the reads are the whole cost.
func TestASecondSyncOverAnUnchangedTreeReadsNothing(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		files["docs/"+name+".md"] = "# " + name + "\n\nsome body text\n"
	}
	root := tree(t, files)

	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}

	first, cursor := collect(t, s, connector.Cursor{})
	if len(first) != len(files) {
		t.Fatalf("the first sync read %d files, want %d", len(first), len(files))
	}
	after := s.Counters()
	if after.Fetches != int64(len(files)) {
		t.Fatalf("the first sync read %d files, want %d", after.Fetches, len(files))
	}

	second, _ := collect(t, s, cursor)
	if len(second) != 0 {
		t.Fatalf("an unchanged tree produced %v", ids(second))
	}

	spent := s.Counters().Since(after)
	if spent.Fetches != 0 || spent.Bytes != 0 {
		t.Errorf("a second sync read %d files and %d bytes, want none", spent.Fetches, spent.Bytes)
	}
	// What it does spend is one stat per file and one listing per directory,
	// which is the floor for a source without a change feed.
	if spent.Metadata != int64(len(files)) {
		t.Errorf("a second sync statted %d files, want %d", spent.Metadata, len(files))
	}
	if spent.Requests() >= after.Requests() {
		t.Errorf("a second sync cost %d requests against the first sync's %d, so it saved nothing",
			spent.Requests(), after.Requests())
	}
}

// TestAnOwnersEditIsAPermissionChangeAndNotARecrawl is the third box, end to
// end and on a real permission model. Editing one OWNERS file changes who may
// read every document below it without touching a single one of them, and the
// sync has to notice that without reading any of them again.
func TestAnOwnersEditIsAPermissionChangeAndNotARecrawl(t *testing.T) {
	root := tree(t, map[string]string{
		"OWNERS":         "approvers:\n  - alice\n",
		"docs/a.md":      "# a\n",
		"docs/b.md":      "# b\n",
		"other/OWNERS":   "approvers:\n  - carol\n",
		"other/notes.md": "# notes\n",
	})
	policy, err := fssource.NewOwnersPolicy(root, "repo", "github")
	if err != nil {
		t.Fatal(err)
	}
	// The OWNERS files themselves are left out of the corpus, so that what the
	// counters show is what happened to the documents they govern.
	s, err := fssource.New(root, "repo", policy,
		fssource.WithInclude(func(name string) bool { return strings.HasSuffix(name, ".md") }))
	if err != nil {
		t.Fatal(err)
	}

	first, cursor := collect(t, s, connector.Cursor{})
	if len(first) != 3 {
		t.Fatalf("the first sync read %v, want three documents", ids(first))
	}
	for _, d := range first {
		if len(d.Permissions.AllowUsers) != 1 {
			t.Fatalf("%s starts with %d allowed users, want 1", d.ID, len(d.Permissions.AllowUsers))
		}
	}
	after := s.Counters()

	// One edit at the root, well into the future so the change is unambiguous
	// on a filesystem with a coarse timestamp.
	owners := filepath.Join(root, fssource.OwnersFile)
	if err := os.WriteFile(owners, []byte("approvers:\n  - alice\n  - bob\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(owners, later, later); err != nil {
		t.Fatal(err)
	}

	var changes []connector.Change
	next, err := s.Sync(t.Context(), cursor, func(_ context.Context, ch connector.Change) error {
		changes = append(changes, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Only the two documents the edited file governs. The third lives under an
	// OWNERS file nobody touched.
	got := make([]string, 0, len(changes))
	for _, ch := range changes {
		if !ch.PermissionsOnly {
			t.Errorf("%s came back as a content change, so the file was read again", ch.Document.ID)
		}
		got = append(got, ch.Document.ID)
	}
	slices.Sort(got)
	if want := []string{"repo:docs/a.md", "repo:docs/b.md"}; !slices.Equal(got, want) {
		t.Fatalf("the edit produced changes for %v, want %v", got, want)
	}
	for _, ch := range changes {
		if len(ch.Document.Permissions.AllowUsers) != 2 {
			t.Errorf("%s now allows %d users, want the two in the edited file",
				ch.Document.ID, len(ch.Document.Permissions.AllowUsers))
		}
	}

	spent := s.Counters().Since(after)
	if spent.Fetches != 0 || spent.Bytes != 0 {
		t.Errorf("applying a permission change read %d files and %d bytes, so it was a recrawl",
			spent.Fetches, spent.Bytes)
	}

	// And it happens once. A cursor that did not move past the edit would
	// replay the same change on every sync from here on.
	changes = nil
	if _, err := s.Sync(t.Context(), next, func(_ context.Context, ch connector.Change) error {
		changes = append(changes, ch)
		return nil
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("the permission change was reported again on the next sync: %d changes", len(changes))
	}
}

// TestAPolicyThatCachesIsToldWhenAWalkStarts is the bug underneath the box. A
// policy that held its parsed files for the life of the process would answer a
// week old question forever, and the revocation above would apply to nothing.
func TestAPolicyThatCachesIsToldWhenAWalkStarts(t *testing.T) {
	root := tree(t, map[string]string{
		"OWNERS": "approvers:\n  - alice\n",
		"a.md":   "# a\n",
	})
	policy, err := fssource.NewOwnersPolicy(root, "repo", "github")
	if err != nil {
		t.Fatal(err)
	}
	s, err := fssource.New(root, "repo", policy,
		fssource.WithInclude(func(name string) bool { return strings.HasSuffix(name, ".md") }))
	if err != nil {
		t.Fatal(err)
	}

	if _, cursor := collect(t, s, connector.Cursor{}); cursor.IsZero() {
		t.Fatal("the first sync returned no cursor")
	}

	if err := os.WriteFile(filepath.Join(root, fssource.OwnersFile), []byte("approvers:\n  - bob\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The document itself is touched, so this is a plain content sync. What is
	// being checked is the access control list that comes with it.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "a.md"), later, later); err != nil {
		t.Fatal(err)
	}

	got, _ := collect(t, s, connector.Cursor{})
	if len(got) != 1 {
		t.Fatalf("read %d documents, want 1", len(got))
	}
	users := got[0].Permissions.AllowUsers
	if len(users) != 1 || users[0].Value != "bob" {
		t.Errorf("the document allows %v, want the edited file's single approver", users)
	}
}
