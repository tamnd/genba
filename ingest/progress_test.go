package ingest_test

import (
	"testing"

	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/ingest"
	"github.com/tamnd/genba/store/memstore"
)

// How far a run has got, reported while it is still going.
//
// The stats a run returns arrive when it is over, which is exactly when nobody
// needs to be told how far along it is. Everything here is about the numbers
// somebody watching a screen sees before that.

func TestProgressClimbsWhileTheRunIsStillGoing(t *testing.T) {
	st := memstore.New()
	defer func() { _ = st.Close() }()

	var seen []ingest.Progress
	p, err := ingest.New(st, connector.NewMemoryCheckpoints(),
		ingest.WithBatchSize(10),
		ingest.WithProgress(func(pr ingest.Progress) { seen = append(seen, pr) }),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	run(t, p, &fakeSource{name: "notes", changes: changes(25)})

	// Twenty five documents in batches of ten is two full batches and a flush
	// of the rest, plus the report that goes out before anything is read.
	if len(seen) != 4 {
		t.Fatalf("progress was reported %d times, want 4: %+v", len(seen), seen)
	}
	// The first report is the one that says a run has started. Without it the
	// only news of a sync arrives a whole batch late, which on a slow source is
	// the whole of the time somebody is waiting.
	if seen[0].Done != 0 {
		t.Errorf("the first report already had %d documents in it", seen[0].Done)
	}
	for i, want := range []int{0, 10, 20, 25} {
		if seen[i].Done != want {
			t.Errorf("report %d said %d documents, want %d", i, seen[i].Done, want)
		}
		if seen[i].Source != "notes" || seen[i].Tenant != tenant {
			t.Errorf("report %d = %+v, want it to name the source and the tenant", i, seen[i])
		}
	}
}

// A run that resumed from a checkpoint says so, because the index already held
// documents from this source before it started and nothing on screen is missing
// while it catches up.
func TestProgressSaysWhetherTheRunResumed(t *testing.T) {
	st := memstore.New()
	defer func() { _ = st.Close() }()

	var resumed []bool
	p, err := ingest.New(st, connector.NewMemoryCheckpoints(),
		ingest.WithProgress(func(pr ingest.Progress) { resumed = append(resumed, pr.Resumed) }),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	src := &fakeSource{name: "notes", changes: changes(5), final: connector.Cursor{Value: "0004"}}
	run(t, p, src)
	if resumed[0] {
		t.Error("the first run over an empty checkpoint reported itself as a resume")
	}

	resumed = nil
	run(t, p, src)
	if !resumed[0] {
		t.Error("a run that started from a saved cursor did not report itself as a resume")
	}
}

// A pipeline with nobody watching it does not pay for the reporting, and more
// to the point does not crash trying to call a function that is not there.
func TestAPipelineWithNoProgressFunctionRunsAnyway(t *testing.T) {
	st := memstore.New()
	defer func() { _ = st.Close() }()

	p, err := ingest.New(st, connector.NewMemoryCheckpoints())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if got := run(t, p, &fakeSource{name: "notes", changes: changes(3)}).Indexed; got != 3 {
		t.Errorf("indexed %d, want 3", got)
	}
}
