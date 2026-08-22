package memstore_test

import (
	"testing"

	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
	"github.com/tamnd/genba/store/storetest"
)

func newStore(t *testing.T) store.Store { return memstore.New() }

func TestConformance(t *testing.T) {
	storetest.Run(t, newStore)
}

func TestMaintenanceConformance(t *testing.T) {
	storetest.RunMaintenance(t, newStore)
	storetest.RunQuarantine(t, newStore)
}

// memstore implements store.Statistician and neither of the other two, so most
// of this suite skips. The cases that do run are the ones that matter here: the
// corpus statistics are what the ranking is normalised against, and they have
// to mean the same thing on the driver that is walked as on the driver that is
// queried, or the same corpus ranks differently depending on where it is
// stored.
func TestRanking(t *testing.T) {
	storetest.RunRanker(t, newStore)
}

// The vocabulary a correction is drawn from, which this driver builds by
// walking the corpus. It is the answer the driver with a term table is checked
// against.
func TestSpelling(t *testing.T) {
	storetest.RunSpeller(t, newStore)
}

func TestClosedStoreRefusesWork(t *testing.T) {
	s := memstore.New()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Put(t.Context()); err == nil {
		t.Fatal("Put on a closed store returned no error")
	}
	if _, err := s.Stats(t.Context()); err == nil {
		t.Fatal("Stats on a closed store returned no error")
	}
}
