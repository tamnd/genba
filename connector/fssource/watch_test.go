package fssource_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/fssource"
)

// A watcher is asynchronous by nature. The write returns before the event about
// it does, and no backend promises how long that takes, so a test that wrote a
// file and immediately synced would be testing the scheduler.
//
// What these tests do instead is what a refresh loop does: sync again. A sync
// that runs before the event arrives takes an empty record and leaves the
// events that follow for the next one, so asking again is free of consequences
// and eventually correct. The deadline is what turns "eventually" into a test
// failure rather than a hang.

// watch starts a watcher on root and closes it when the test ends.
func watch(t *testing.T, root string, opts ...fssource.WatchOption) *fssource.Watcher {
	t.Helper()
	w, err := fssource.Watch(root, opts...)
	if err != nil {
		t.Fatalf("watching %s: %v", root, err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// watched returns a source over root that asks a watcher what changed.
func watched(t *testing.T, root string, w *fssource.Watcher, opts ...fssource.Option) *fssource.Source {
	t.Helper()
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"), append(opts, fssource.WithWatcher(w))...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// changes runs one sync and returns everything it emitted, in the order it did.
func changes(t *testing.T, s *fssource.Source, from connector.Cursor) ([]connector.Change, connector.Cursor) {
	t.Helper()
	var got []connector.Change
	next, err := s.Sync(t.Context(), from, func(_ context.Context, ch connector.Change) error {
		got = append(got, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return got, next
}

// until syncs repeatedly until want is happy with everything seen so far.
func until(t *testing.T, s *fssource.Source, from connector.Cursor, want func([]connector.Change) bool) ([]connector.Change, connector.Cursor) {
	t.Helper()
	var all []connector.Change
	next := from
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, cur := changes(t, s, next)
		all = append(all, got...)
		next = cur
		if want(all) {
			return all, next
		}
		if time.Now().After(deadline) {
			t.Fatalf("after ten seconds the syncs had produced %v", changed(all))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle gives an event that should never be acted on time to arrive, so that a
// test asserting nothing happened is asserting something.
func settle() { time.Sleep(250 * time.Millisecond) }

// changed is every id a run of syncs emitted.
func changed(all []connector.Change) []string {
	out := make([]string, 0, len(all))
	for _, ch := range all {
		out = append(out, ch.Document.ID)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// read is the ids that were actually read, which is what a sync spends its time
// on and what these tests are usually counting.
func read(all []connector.Change) []string {
	var out []string
	for _, ch := range all {
		if !ch.PermissionsOnly && !ch.Deleted {
			out = append(out, ch.Document.ID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func reported(all []connector.Change, id string) bool {
	return slices.Contains(changed(all), id)
}

func wasRead(all []connector.Change, id string) bool {
	return slices.Contains(read(all), id)
}

// waitFor blocks until the watcher's record holds at least n paths.
//
// Reading the record instead of syncing is the point wherever a test needs two
// events about one path to be in there together. A sync would take the first
// one away and decide on it alone.
func waitFor(t *testing.T, w *fssource.Watcher, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if held := w.Stats().Pending; held >= n {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("after ten seconds the watcher held %d paths, want %d", held, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// find returns the last change emitted for one id.
func find(all []connector.Change, id string) (connector.Change, bool) {
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Document.ID == id {
			return all[i], true
		}
	}
	return connector.Change{}, false
}

func put(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTheFirstWatchedSyncWalksTheWholeTree(t *testing.T) {
	root := tree(t, map[string]string{
		"README.md":       "# top\n",
		"docs/install.md": "# installing\n",
	})
	w := watch(t, root)
	s := watched(t, root, w)

	got, _ := changes(t, s, connector.Cursor{})
	if want := []string{"repo:README.md", "repo:docs/install.md"}; !slices.Equal(read(got), want) {
		t.Fatalf("read %v, want %v", read(got), want)
	}

	// A watcher that has just been built knows what is happening to the tree and
	// nothing about what is in it, so the first sync has to read the tree to
	// find out. That is not a cost worth avoiding: the first sync of an empty
	// index reads the whole corpus anyway.
	if stats := w.Stats(); stats.Walks != 1 || stats.Reason != "" {
		t.Fatalf("after the first sync the watcher is %+v, want one walk and no outstanding reason", stats)
	}
}

func TestAWatchedSyncReadsWhatChangedAndDoesNotWalk(t *testing.T) {
	root := tree(t, map[string]string{
		"README.md":         "# top\n",
		"docs/install.md":   "# installing\n",
		"docs/deep/note.md": "# note\n",
		"cmd/main.go":       "package main\n",
	})
	w := watch(t, root)
	s := watched(t, root, w)

	_, cursor := changes(t, s, connector.Cursor{})
	before := s.Counters()

	put(t, root, "docs/install.md", "# installing\n\nrun the thing\n")
	got, _ := until(t, s, cursor, func(all []connector.Change) bool {
		return wasRead(all, "repo:docs/install.md")
	})

	if want := []string{"repo:docs/install.md"}; !slices.Equal(read(got), want) {
		t.Fatalf("read %v, want %v", read(got), want)
	}
	// This is the whole claim of the change. Not "it read fewer files" but "it
	// never looked at the tree at all", which is what makes the cost of a sync a
	// function of how much moved rather than of how large the corpus is.
	if spent := s.Counters().Since(before); spent.Lists != 0 {
		t.Fatalf("the sync listed %d directories, want none at all", spent.Lists)
	}
}

func TestNothingChangingCostsNothing(t *testing.T) {
	root := tree(t, map[string]string{
		"README.md":       "# top\n",
		"docs/install.md": "# installing\n",
	})
	w := watch(t, root)
	s := watched(t, root, w)

	_, cursor := changes(t, s, connector.Cursor{})
	before := s.Counters()

	got, next := changes(t, s, cursor)
	if len(read(got)) != 0 {
		t.Fatalf("read %v, want nothing", read(got))
	}
	if spent := s.Counters().Since(before); spent.Lists != 0 || spent.Fetches != 0 || spent.Bytes != 0 {
		t.Fatalf("a quiet sync spent %+v, want nothing", spent)
	}
	// A sync that read nothing must not move the cursor backwards either, or the
	// next one after a fallback to walking would re-read the whole tree.
	if next.Value != cursor.Value {
		t.Fatalf("the cursor moved from %q to %q with nothing to move it", cursor.Value, next.Value)
	}
}

func TestAFileCreatedAfterTheWatchStartedIsFound(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "# top\n"})
	w := watch(t, root)
	s := watched(t, root, w)
	_, cursor := changes(t, s, connector.Cursor{})

	put(t, root, "CHANGELOG.md", "# what changed\n")
	got, _ := until(t, s, cursor, func(all []connector.Change) bool {
		return wasRead(all, "repo:CHANGELOG.md")
	})

	d, _ := find(got, "repo:CHANGELOG.md")
	if d.Document.Title != "what changed" {
		t.Fatalf("title %q, want the heading out of the file", d.Document.Title)
	}
}

func TestADirectoryCreatedAfterTheWatchStartedIsWatchedToo(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "# top\n"})
	w := watch(t, root)
	s := watched(t, root, w)
	_, cursor := changes(t, s, connector.Cursor{})

	// A directory and a file inside it, in that order and quickly, which is what
	// a checkout or a code generator does. The watch on the new directory is put
	// on after it exists, so the file may well be there before the watcher hears
	// about the directory, and the directory is walked as well as watched for
	// exactly that reason.
	put(t, root, "guides/start.md", "# getting started\n")
	got, cursor := until(t, s, cursor, func(all []connector.Change) bool {
		return wasRead(all, "repo:guides/start.md")
	})
	if !slices.Contains(read(got), "repo:guides/start.md") {
		t.Fatalf("read %v, want the file in the new directory", read(got))
	}

	// And the watch is now on it, so the next file lands without a walk.
	before := s.Counters()
	put(t, root, "guides/next.md", "# what next\n")
	until(t, s, cursor, func(all []connector.Change) bool {
		return wasRead(all, "repo:guides/next.md")
	})
	if spent := s.Counters().Since(before); spent.Lists != 0 {
		t.Fatalf("the sync listed %d directories, so the new directory was not being watched", spent.Lists)
	}
}

func TestADeletedFileIsAChangeOnlyAWatcherCanSee(t *testing.T) {
	root := tree(t, map[string]string{
		"README.md":   "# top\n",
		"docs/old.md": "# on its way out\n",
	})
	w := watch(t, root)
	s := watched(t, root, w)
	_, cursor := changes(t, s, connector.Cursor{})

	if err := os.Remove(filepath.Join(root, "docs", "old.md")); err != nil {
		t.Fatal(err)
	}
	got, _ := until(t, s, cursor, func(all []connector.Change) bool {
		return reported(all, "repo:docs/old.md")
	})

	ch, _ := find(got, "repo:docs/old.md")
	if !ch.Deleted {
		t.Fatalf("the change for the removed file is %+v, want a deletion", ch)
	}

	// Worth saying out loud, because it is the one thing the walk can never do.
	// A walk finds a deleted file by not finding it, which is to say it does not
	// find it, and the index keeps serving the document until a reconciliation
	// sweep counts both sides.
	plain, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}
	walked, _ := changes(t, plain, cursor)
	if reported(walked, "repo:docs/old.md") {
		t.Fatal("the walking source reported the deletion, which it has no way of knowing about")
	}
}

func TestAFileWrittenAndThenDeletedIsADeletionRatherThanARead(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "# top\n"})
	w := watch(t, root)
	s := watched(t, root, w)
	_, cursor := changes(t, s, connector.Cursor{})

	// Both events describe the same path and only one of them is still true, so
	// the record is not what decides. The filesystem is.
	//
	// Waiting for the write to reach the record before removing the file is what
	// makes this the case it is meant to be. Removing it first would leave the
	// backends free to notice nothing at all, since some of them find out what
	// changed in a directory by listing it, and a file that came and went
	// between two listings never appears in either.
	put(t, root, "scratch.md", "# temporary\n")
	waitFor(t, w, 1)
	if err := os.Remove(filepath.Join(root, "scratch.md")); err != nil {
		t.Fatal(err)
	}

	got, _ := changes(t, s, cursor)
	ch, _ := find(got, "repo:scratch.md")
	if !ch.Deleted {
		t.Fatalf("the change is %+v, want a deletion", ch)
	}
	if slices.Contains(read(got), "repo:scratch.md") {
		t.Fatal("the file that is no longer there was read")
	}
}

func TestTheWatchedSyncPassesOverTheSameDirectoriesTheWalkDoes(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "# top\n"})
	w := watch(t, root)
	s := watched(t, root, w)
	_, cursor := changes(t, s, connector.Cursor{})

	// A dependency tree written after the watcher started. It holds far more
	// files than the corpus and none of the content, and a watcher that
	// descended into it would spend its watches there and report every one of
	// them as a change.
	put(t, root, "node_modules/left-pad/index.js", "module.exports = 1\n")
	put(t, root, ".git/COMMIT_EDITMSG", "wip\n")
	put(t, root, "docs/real.md", "# a real document\n")

	got, _ := until(t, s, cursor, func(all []connector.Change) bool {
		return wasRead(all, "repo:docs/real.md")
	})
	settle()
	more, _ := changes(t, s, cursor)
	got = append(got, more...)

	for _, id := range changed(got) {
		if strings.Contains(id, "node_modules") || strings.Contains(id, ".git") {
			t.Fatalf("the sync reported %s, which is in a directory it does not index", id)
		}
	}
}

func TestALostRecordMakesTheNextSyncWalk(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "# top\n"})
	// Two paths is all this watcher will hold, which is what a real one looks
	// like when a build rewrites ten thousand files at once. Past that the
	// record has stopped being a saving and walking is bounded work.
	w := watch(t, root, fssource.WithMaxPending(2))
	s := watched(t, root, w)
	_, cursor := changes(t, s, connector.Cursor{})

	for _, name := range []string{"a.md", "b.md", "c.md", "d.md", "e.md"} {
		put(t, root, "docs/"+name, "# "+name+"\n")
	}

	// Everything is found, which is the point. Losing the record costs a walk
	// and not a document.
	//
	// Waiting for all five rather than for the first and the last of them,
	// because the walk that finds the overflow is not one sync and there is
	// nothing that says the two ends of the set arrive last. Stopping as soon
	// as those two were seen was a test that failed whenever the middle of the
	// batch happened to land in the sync after them.
	want := []string{"a.md", "b.md", "c.md", "d.md", "e.md"}
	got, _ := until(t, s, cursor, func(all []connector.Change) bool {
		for _, name := range want {
			if !wasRead(all, "repo:docs/"+name) {
				return false
			}
		}
		return true
	})

	for _, name := range want {
		if !slices.Contains(read(got), "repo:docs/"+name) {
			t.Fatalf("read %v, which is missing docs/%s", read(got), name)
		}
	}
	if stats := w.Stats(); stats.Walks < 2 {
		t.Fatalf("the watcher walked %d times, want the first one and at least one more for the overflow", stats.Walks)
	}
}

func TestAClosedWatcherLeavesASourceThatWalks(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "# top\n"})
	w := watch(t, root)
	s := watched(t, root, w)
	_, cursor := changes(t, s, connector.Cursor{})

	if err := w.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	put(t, root, "docs/after.md", "# written with nobody watching\n")

	got, _ := changes(t, s, cursor)
	if !slices.Contains(read(got), "repo:docs/after.md") {
		t.Fatalf("read %v, want the file written after the watcher was closed", read(got))
	}
}

func TestATreeThatNeedsMoreWatchesThanTheLimitIsRefused(t *testing.T) {
	root := tree(t, map[string]string{
		"README.md":       "# top\n",
		"docs/install.md": "# installing\n",
		"cmd/main.go":     "package main\n",
	})
	// Refusing is the useful answer. Every backend charges for a watch and they
	// charge differently, and the caller can log this and carry on with a source
	// that walks. Handing back a watcher holding some of the tree would be a
	// record that looks healthy and is missing whole directories.
	_, err := fssource.Watch(root, fssource.WithMaxWatches(1))
	if err == nil {
		t.Fatal("a tree of three directories was watched with a limit of one")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("the error is %q, and does not say what the limit was", err)
	}
}

func TestAWatcherOnAnotherTreeIsRefused(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "# top\n"})
	elsewhere := tree(t, map[string]string{"README.md": "# somewhere else\n"})

	w := watch(t, elsewhere)
	_, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"), fssource.WithWatcher(w))
	if err == nil {
		t.Fatal("a source took a watcher pointed at a different tree")
	}
	// The failure this prevents is silent: the watcher would report nothing
	// about the tree being read while looking to every sync like a record that
	// is up to date.
	if !strings.Contains(err.Error(), elsewhere) || !strings.Contains(err.Error(), root) {
		t.Fatalf("the error is %q, and does not name both trees", err)
	}
}

func TestANilWatcherIsTheSameAsNoWatcher(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "# top\n"})
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"), fssource.WithWatcher(nil))
	if err != nil {
		t.Fatal(err)
	}
	// This is what lets a caller log the error from Watch and carry on with one
	// branch instead of two.
	got, _ := changes(t, s, connector.Cursor{})
	if want := []string{"repo:README.md"}; !slices.Equal(read(got), want) {
		t.Fatalf("read %v, want %v", read(got), want)
	}
}

func TestACursorDoesNotGoBackwardsWhenAFileArrivesWithAnOldTime(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "# top\n"})
	w := watch(t, root)
	s := watched(t, root, w)
	_, cursor := changes(t, s, connector.Cursor{})

	// What "cp -p" does, and what restoring from a backup does. The file is new
	// to the index and old to the filesystem.
	put(t, root, "docs/restored.md", "# from last year\n")
	old := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "docs", "restored.md"), old, old); err != nil {
		t.Fatal(err)
	}

	_, next := until(t, s, cursor, func(all []connector.Change) bool {
		return wasRead(all, "repo:docs/restored.md")
	})
	if next.Time.Before(cursor.Time) {
		t.Fatalf("the cursor went from %s back to %s, which would make the next walk re-read the tree", cursor.Time, next.Time)
	}
}

func TestAFileOverTheSizeLimitIsPassedOverAndReported(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "# top\n"})
	w := watch(t, root)

	var skipped []string
	s := watched(t, root, w, fssource.WithMaxFileSize(32), fssource.WithSkipped(func(p string, _ error) {
		skipped = append(skipped, filepath.Base(p))
	}))
	_, cursor := changes(t, s, connector.Cursor{})

	put(t, root, "docs/huge.md", strings.Repeat("far too much prose. ", 64))
	put(t, root, "docs/small.md", "# small\n")
	got, _ := until(t, s, cursor, func(all []connector.Change) bool {
		return wasRead(all, "repo:docs/small.md") && slices.Contains(skipped, "huge.md")
	})

	if slices.Contains(read(got), "repo:docs/huge.md") {
		t.Fatal("the file over the limit was read")
	}
	if !slices.Contains(skipped, "huge.md") {
		t.Fatalf("skipped %v, and the file over the limit was dropped without a word", skipped)
	}
}

func TestAnEditToAPermissionRuleMakesTheSyncWalkTheTree(t *testing.T) {
	root := tree(t, map[string]string{
		"OWNERS":        "approvers:\n  - alice\n",
		"README.md":     "# top\n",
		"net/design.md": "# networking\n",
		"net/deep/x.md": "# deeper\n",
	})
	w := watch(t, root)
	policy := policyFor(t, root)
	s, err := fssource.New(root, "repo", policy, fssource.WithWatcher(w))
	if err != nil {
		t.Fatal(err)
	}
	_, cursor := changes(t, s, connector.Cursor{})
	before := s.Counters()

	// The one edit whose effect is nowhere near the file that changed. Who may
	// read every document in the tree just moved, and the only thing any backend
	// will ever say is that OWNERS was written.
	time.Sleep(10 * time.Millisecond)
	put(t, root, "OWNERS", "approvers:\n  - alice\n  - bob\n")

	got, _ := until(t, s, cursor, func(all []connector.Change) bool {
		return reported(all, "repo:net/deep/x.md")
	})

	// Walking is the expensive answer and it is the right one. A revocation that
	// was applied to the OWNERS file and to nothing it governs is not a
	// revocation.
	if spent := s.Counters().Since(before); spent.Lists == 0 {
		t.Fatal("the sync did not walk the tree, so the files the rule governs kept last week's answer")
	}
	ch, _ := find(got, "repo:net/deep/x.md")
	if !ch.PermissionsOnly {
		t.Fatalf("the change for a file the rule governs is %+v, want a permission change", ch)
	}
	if reason := w.Stats().Reason; reason != "" {
		t.Fatalf("after the walk the watcher still says %q, and would walk again for nothing", reason)
	}
}

func TestChangingOnlyTheModeIsAPermissionChangeAndNotAReRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows has no mode to change. os.Chmod there toggles the read only
		// attribute, and OSPolicy reads an access control list this test has no
		// way to write.
		t.Skip("a POSIX mode is what this is about")
	}
	root := tree(t, map[string]string{
		"README.md":     "# top\n",
		"docs/plan.md":  "# the plan\n",
		"docs/other.md": "# something else\n",
	})
	w := watch(t, root)
	policy, err := fssource.NewOSPolicy(root, "repo", "corp")
	if err != nil {
		t.Fatal(err)
	}
	s, err := fssource.New(root, "repo", policy, fssource.WithWatcher(w))
	if err != nil {
		t.Fatal(err)
	}
	_, cursor := changes(t, s, connector.Cursor{})
	before := s.Counters()

	if err := os.Chmod(filepath.Join(root, "docs", "plan.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := until(t, s, cursor, func(all []connector.Change) bool {
		return reported(all, "repo:docs/plan.md")
	})

	ch, _ := find(got, "repo:docs/plan.md")
	if !ch.PermissionsOnly {
		t.Fatalf("the change is %+v, want a permission change", ch)
	}
	// The cheap half of the whole design. A walk has to ask every file in the
	// tree whether its rule moved, and here the operating system already said
	// which one did, so the sync costs one write and reads no bytes.
	if spent := s.Counters().Since(before); spent.Fetches != 0 || spent.Bytes != 0 {
		t.Fatalf("a mode change cost %+v, want no reading at all", spent)
	}
}

func TestTheWatchedAndWalkedSyncsAgreeOnWhatIsInTheCorpus(t *testing.T) {
	files := map[string]string{
		"README.md":            "# top\n",
		"docs/install.md":      "# installing\n",
		"cmd/main.go":          "package main\n",
		"assets/logo.png":      "\x89PNG\r\n\x1a\n binary",
		".hidden/secret.md":    "# should not appear\n",
		"node_modules/dep.js":  "module.exports = 1",
		"docs/notes.unknownxt": "not an extension we read",
	}
	root := tree(t, files)
	w := watch(t, root)
	s := watched(t, root, w)
	_, cursor := changes(t, s, connector.Cursor{})

	// Every file rewritten with the same contents, so the two paths are being
	// asked the same question. They have to give the same answer, because the
	// watched sync falls back to the walking one whenever the record cannot be
	// trusted, and a corpus that changed shape depending on which ran would be a
	// corpus that changed shape at random.
	elsewhere := tree(t, files)
	plain, err := fssource.New(elsewhere, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}
	byWalking, _ := changes(t, plain, connector.Cursor{})

	for rel, body := range files {
		put(t, root, rel, body+"\n")
	}
	// Waiting for the names rather than for the count of them. The two are the
	// same number here only when the answer is already right, so counting would
	// have let a run where watching read something else of the same size through
	// to an assertion that then failed on a set difference.
	byWatching, _ := until(t, s, cursor, func(all []connector.Change) bool {
		for _, id := range read(byWalking) {
			if !wasRead(all, id) {
				return false
			}
		}
		return true
	})

	if !slices.Equal(read(byWatching), read(byWalking)) {
		t.Fatalf("watching read %v and walking read %v", read(byWatching), read(byWalking))
	}
}
