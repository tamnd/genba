package fssource_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/fssource"
)

// Reading a tree as fast as the disk allows is the right default and the wrong
// thing on the one tree where it matters. A first read of a large corpus is
// minutes of a busy disk, and the server is answering queries out of that same
// disk the whole time, which is the entire reason it opens its listener before
// the read is finished. Those minutes are meant to be usable.
//
// So the read can be given a pace. What the pace is stays with whoever chose
// it, because a token bucket, a semaphore shared with something else and a
// plain sleep are all one function from in here.

func TestTheReadWaitsOnceForEachFileItReads(t *testing.T) {
	root := tree(t, map[string]string{
		"handbook/deploying.md": "How to deploy.",
		"handbook/oncall.md":    "Who to call.",
		"notes/standup.md":      "What we said.",
	})

	var waited int
	src, err := fssource.New(root, "docs", fssource.PublicToTenant("docs"), fssource.WithPace(func(context.Context) error {
		waited++
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	docs, next := collect(t, src, connector.Cursor{})
	if len(docs) != 3 {
		t.Fatalf("read %v, want the three files in the tree", ids(docs))
	}
	if waited != 3 {
		t.Errorf("waited %d times for three files, want one wait each", waited)
	}

	// And nothing is waited for on a sync that reads nothing. A pace in front of
	// the walk rather than in front of the read would charge a refresh over an
	// unchanged tree the same as a first read, which on a server refreshing
	// every second is a ceiling that never stops binding.
	waited = 0
	if docs, _ := collect(t, src, next); len(docs) != 0 {
		t.Fatalf("a second sync over an unchanged tree read %v", ids(docs))
	}
	if waited != 0 {
		t.Errorf("waited %d times on a sync that read nothing", waited)
	}
}

func TestAPaceThatGivesUpStopsTheReadRatherThanSkippingTheFile(t *testing.T) {
	root := tree(t, map[string]string{
		"a.md": "One.",
		"b.md": "Two.",
		"c.md": "Three.",
	})

	// The only thing that makes a pace fail is the process shutting down, and a
	// sync that treated that as one unreadable file would spend the shutdown
	// reading the rest of the tree.
	stop := errors.New("closing time")
	var read int
	src, err := fssource.New(root, "docs", fssource.PublicToTenant("docs"), fssource.WithPace(func(context.Context) error {
		read++
		if read > 1 {
			return stop
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	var emitted int
	if _, err := src.Sync(t.Context(), connector.Cursor{}, func(context.Context, connector.Change) error {
		emitted++
		return nil
	}); !errors.Is(err, stop) {
		t.Fatalf("sync returned %v, want the pace's own error", err)
	}
	if emitted != 1 {
		t.Errorf("emitted %d documents after the pace gave up on the second, want 1", emitted)
	}
}

func TestAReadWithNoPaceStillStopsWhenTheContextDoes(t *testing.T) {
	root := tree(t, map[string]string{"a.md": "One.", "b.md": "Two."})
	src, err := fssource.New(root, "docs", fssource.PublicToTenant("docs"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := src.Sync(ctx, connector.Cursor{}, func(context.Context, connector.Change) error {
		t.Error("a sync on a cancelled context read a file")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("sync returned %v, want the cancellation", err)
	}
}
