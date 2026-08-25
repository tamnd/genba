package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// recheckTree is the corpus tree with two approvers on the guides directory, so
// that one of them can be taken off it while the server is running.
func recheckTree(t *testing.T) string {
	t.Helper()
	root := corpusTree(t)
	writeOwners(t, root, "approvers:\n  - bob\n  - dave\n")
	return root
}

func writeOwners(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "guides", "OWNERS"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// serveCorpus starts a server on the tree and returns its address.
//
// There is no refresh interval on purpose. Every sync this process will ever do
// has happened by the time the queries below run, so anything that changes
// after that is a change only a check at query time can see.
func serveCorpus(t *testing.T, root string, extra ...string) string {
	t.Helper()
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	args := append([]string{
		"-addr", addr,
		"-tenant", "acme",
		"-corpus", root,
		"-corpus-name", "handbook",
		"-corpus-acl", aclOwners,
		"-log-level", "error",
	}, extra...)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, args, env(nil), &out, &errOut)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the server did not shut down")
		}
	})

	waitForIndex(t, "http://"+addr)
	return addr
}

// The window this closes: the OWNERS file is edited, the crawler is not due for
// another hour, and the person who was taken off the directory asks for it.
func TestATakenAwayFileIsGoneBeforeTheNextSync(t *testing.T) {
	root := recheckTree(t)
	addr := serveCorpus(t, root, "-corpus-recheck")

	// bob is the control, and he is asked first on purpose. An answer is held
	// for ten seconds per person and per document, so asking about dave here as
	// well would mean the query after the edit was answered by that cache rather
	// than by the tree, and the test would be waiting out a timer to prove
	// something the timer is not about. dave has never been asked, so his first
	// question below is a real one.
	if got := searchAs(t, addr, "bob", "legacy"); len(got.Hits) == 0 {
		t.Fatal("bob approves the guides directory and cannot see a file in it")
	}

	// dave is taken off the directory. Nothing about the document changed, so a
	// walk would find nothing to do even if one were due, and the index still
	// carries the list that named him.
	writeOwners(t, root, "approvers:\n  - bob\n")

	if got := searchAs(t, addr, "dave", "legacy"); len(got.Hits) != 0 {
		t.Errorf("dave was taken off the directory and still gets results: %v", got.Hits)
	}
	if got := searchAs(t, addr, "bob", "legacy"); len(got.Hits) == 0 {
		t.Error("bob is still an approver and the check took his results away")
	}
	// The check only ever takes rows away, and this is where that shows. erin was
	// added to the file a moment ago and still cannot find the document, because
	// the index decides which rows are candidates and the index has not been told
	// yet. A grant waits for the sync. A revocation does not, and that asymmetry
	// is the whole shape of the feature.
	writeOwners(t, root, "approvers:\n  - bob\n  - erin\n")
	if got := searchAs(t, addr, "erin", "legacy"); len(got.Hits) != 0 {
		t.Error("erin was let in by a check that is only able to take rows away")
	}
}

// The other half, and the reason the flag exists rather than the behaviour
// being on for everybody. Without it the server answers out of the index, which
// is the whole point of an index and is also a stale list until the next sync.
func TestWithoutTheFlagTheIndexIsWhatAnswers(t *testing.T) {
	root := recheckTree(t)
	addr := serveCorpus(t, root)

	if got := searchAs(t, addr, "dave", "legacy"); len(got.Hits) == 0 {
		t.Fatal("dave approves the guides directory and cannot see a file in it")
	}
	writeOwners(t, root, "approvers:\n  - bob\n")
	if got := searchAs(t, addr, "dave", "legacy"); len(got.Hits) == 0 {
		t.Error("the results changed without a sync and without the flag that reads the tree again")
	}
}

// A file somebody removed leaves the results at once rather than waiting for
// the sweep, because a file that is not there is nobody's to read.
func TestADeletedFileIsGoneBeforeTheSweep(t *testing.T) {
	root := recheckTree(t)
	addr := serveCorpus(t, root, "-corpus-recheck")

	if got := searchAs(t, addr, "dave", "legacy"); len(got.Hits) == 0 {
		t.Fatal("the file was not indexed in the first place")
	}
	if err := os.Remove(filepath.Join(root, "guides", "legacy.md")); err != nil {
		t.Fatal(err)
	}
	if got := searchAs(t, addr, "bob", "legacy"); len(got.Hits) != 0 {
		t.Errorf("a file that is no longer in the tree came back: %v", got.Hits)
	}
	if got := searchAs(t, addr, "bob", "deploying"); len(got.Hits) == 0 {
		t.Error("the check took away a document the tree still holds")
	}
}

// A flag that quietly does nothing is worse than one that is refused, because
// what it looks like from outside is a deployment where revocations land in
// seconds.
func TestARecheckWithNoCorpusIsRefused(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"-corpus-recheck", "-log-level", "error"}, env(nil), &out, &errOut)
	if err == nil {
		t.Fatal("a server started with a permission check over nothing")
	}
}
