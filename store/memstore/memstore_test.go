package memstore_test

import (
	"testing"

	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
	"github.com/tamnd/genba/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return memstore.New() })
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
