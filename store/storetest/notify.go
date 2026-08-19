package storetest

import (
	"slices"
	"sync"
	"testing"

	"github.com/tamnd/genba/store"
)

// Reporting writes is an optional capability, and the two things that consume
// it are a cache and a live interface. Both of them are wrong in a way nobody
// notices if a driver reports a write before it is readable: the cache drops an
// entry and immediately refills it from the state the write was about to
// replace, and the browser is told to refetch something that is not there yet.
//
// So the conformance case is not only that a change arrives. It is that a
// subscriber which reads the store from inside the callback sees the write it
// was just told about.

// notified collects what a driver reported.
type notified struct {
	mu      sync.Mutex
	changes []store.Change
}

func (n *notified) record(c store.Change) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.changes = append(n.changes, c)
}

func (n *notified) all() []store.Change {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.changes)
}

func testNotifyWrites(t *testing.T, s store.Store) {
	n, ok := s.(store.Notifier)
	if !ok {
		t.Skip("this driver does not report its writes")
	}

	var (
		got     notified
		visible = make(map[string]bool)
	)
	stop := n.OnChange(func(c store.Change) {
		got.record(c)
		// Reading the store from inside the callback is the point. A driver that
		// reports a write while still holding its own lock deadlocks here, and one
		// that reports before committing returns not found.
		for _, id := range c.IDs {
			if _, err := s.Get(t.Context(), reader(), id); err == nil {
				visible[id] = true
			}
		}
	})

	mustPut(t, s, document("d1", readable()), document("d2", readable()))
	changes := got.all()
	if len(changes) != 1 {
		t.Fatalf("a put of two documents reported %d changes, want 1 per tenant", len(changes))
	}
	if changes[0].Tenant != "acme" {
		t.Errorf("the change names tenant %q, want acme: a change without a tenant is one a subscriber cannot act on", changes[0].Tenant)
	}
	if changes[0].Deleted {
		t.Error("a put reported itself as a delete")
	}
	ids := slices.Clone(changes[0].IDs)
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"d1", "d2"}) {
		t.Errorf("the change names %v, want both documents", ids)
	}
	if !visible["d1"] || !visible["d2"] {
		t.Error("a document was reported before it could be read, so a subscriber would cache the state the write replaced")
	}

	if err := s.Delete(t.Context(), "d1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	changes = got.all()
	if len(changes) != 2 {
		t.Fatalf("a delete reported %d changes in total, want 2", len(changes))
	}
	last := changes[1]
	if !last.Deleted {
		t.Error("a delete did not report itself as a delete, so a cache would keep serving the document in its result lists")
	}
	if last.Tenant != "acme" || !slices.Equal(last.IDs, []string{"d1"}) {
		t.Errorf("the delete reported tenant %q and ids %v, want acme and [d1]", last.Tenant, last.IDs)
	}

	// Deleting an id that is not there is not a write, so there is nothing to
	// report and a subscriber is not woken for it.
	if err := s.Delete(t.Context(), "never-there"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if now := got.all(); len(now) != 2 {
		t.Errorf("deleting an absent id reported a change: %d changes in total, want 2", len(now))
	}

	stop()
	mustPut(t, s, document("d3", readable()))
	if now := got.all(); len(now) != 2 {
		t.Errorf("a subscriber that unsubscribed was still called: %d changes in total, want 2", len(now))
	}
}
