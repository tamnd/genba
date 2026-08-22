package main

import (
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store/memstore"
)

// The rules about when the server says a source is still being read.
//
// Every one of them is about not putting a banner up that says results are
// partial when they are not. A caveat somebody sees while the answer is
// complete is worse than no caveat at all, because after the second time they
// stop reading it and it is still there the day it is true.

func emptyTracker(t *testing.T) *indexing {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })
	return newIndexing(t.Context(), st)
}

func TestASourceIsReportedFromBeforeItHasBeenCounted(t *testing.T) {
	track := emptyTracker(t)

	if !track.expect("notes") {
		t.Fatal("a process that came up on an empty index is not tracking its first read")
	}
	got, ok := track.State()
	if !ok {
		t.Fatal("nothing was reported between the listener opening and the count arriving")
	}
	if got.Source != "notes" || got.Total != 0 {
		t.Errorf("state = %+v, want the source named and no total yet", got)
	}

	track.counting("notes", 900)
	track.advance("notes", 500)
	if got, _ = track.State(); got.Done != 500 || got.Total != 900 {
		t.Errorf("state = %+v, want 500 of 900", got)
	}
}

func TestASourceStopsBeingReportedWhenItIsRead(t *testing.T) {
	track := emptyTracker(t)
	track.expect("notes")
	track.counting("notes", 900)

	track.advance("notes", 900)
	if _, ok := track.State(); ok {
		t.Error("a source that reached its own count is still being reported")
	}

	// A tree people are working in grows while it is read, and a line saying
	// 22,100 of about 22,000 makes the one number on it the one nobody believes.
	track.advance("notes", 1200)
	if _, ok := track.State(); ok {
		t.Error("a source that overshot its own count is still being reported")
	}
}

// A first read that failed is over. The banner promises that results are
// partial until this finishes, and a sync that died has finished.
func TestAFailedFirstReadStopsBeingReported(t *testing.T) {
	track := emptyTracker(t)
	track.expect("notes")
	track.counting("notes", 900)
	track.advance("notes", 12)

	track.finished("notes")
	if _, ok := track.State(); ok {
		t.Error("a run that is over is still being reported")
	}
}

// The one that matters most. A restart against a SQLite file that already holds
// the whole corpus re-reads it from a zero cursor, because checkpoints are held
// in memory. Nothing is missing from the index while that runs, so nothing is
// partial and there is nothing to say.
func TestAProcessThatCameUpOnAFullIndexReportsNothing(t *testing.T) {
	st := memstore.New()
	defer func() { _ = st.Close() }()

	d := doc.Document{
		ID: "notes:readme.md", Tenant: "acme", Source: "notes", Kind: doc.KindPage,
		Title: "Readme", Permissions: acl.Permissions{Mode: acl.ModePublicToTenant, Source: "notes", Version: 1},
	}
	if err := st.Put(t.Context(), d); err != nil {
		t.Fatalf("Put: %v", err)
	}

	track := newIndexing(t.Context(), st)
	if track.expect("notes") {
		t.Fatal("a restart over a corpus that is already indexed counted its source again")
	}
	track.counting("notes", 900)
	track.advance("notes", 12)
	if _, ok := track.State(); ok {
		t.Error("a catch up over a full index was reported as a first read")
	}
}

// Two sources, and the banner has room for one. The one that started first is
// the one reported, so it does not swap between them while both are going.
func TestTheSourceThatStartedFirstIsTheOneReported(t *testing.T) {
	track := emptyTracker(t)
	track.expect("notes")
	track.counting("notes", 900)
	track.expect("objects")
	track.counting("objects", 40)

	if got, _ := track.State(); got.Source != "notes" {
		t.Errorf("state named %q, want the source that started first", got.Source)
	}

	track.finished("notes")
	if got, ok := track.State(); !ok || got.Source != "objects" {
		t.Errorf("state = %+v %v, want the second source once the first is done", got, ok)
	}

	track.finished("objects")
	if _, ok := track.State(); ok {
		t.Error("something is still being reported with both sources read")
	}
}

// A refresh is not a first read. Once a source has been through one, later syncs
// over it say nothing, forever.
func TestARefreshIsNotAFirstRead(t *testing.T) {
	track := emptyTracker(t)
	track.expect("notes")
	track.counting("notes", 4)
	track.advance("notes", 4)
	track.finished("notes")

	if track.expect("notes") {
		t.Fatal("a refresh registered itself as a first read")
	}
	track.advance("notes", 1)
	if _, ok := track.State(); ok {
		t.Error("a refresh over a source that has been read is being reported")
	}
}
