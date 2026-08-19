package store

import "sync"

// Notifier is the optional capability of a driver that reports its own writes.
//
// Two things above the storage layer need to know that the corpus moved. A
// cache has to drop what a write made wrong, and the browser has to be told
// that the results on screen are no longer the whole story. Both were
// previously a timer, which is the answer that is either too slow to be useful
// or too expensive to be cheap.
//
// A driver reports a change after it has committed, never before. A subscriber
// that acted on an uncommitted write would drop a cache entry and then refill
// it from the state the write was about to replace, which is a worse cache than
// no cache.
type Notifier interface {
	// OnChange registers fn and returns a function that unregisters it.
	//
	// fn is called from whichever goroutine performed the write, so it must not
	// block. A subscriber that needs to do real work hands the change to its own
	// goroutine and returns.
	OnChange(fn func(Change)) (stop func())
}

// Change is what one committed write did.
type Change struct {
	// Tenant is whose corpus moved. A driver reports one change per tenant, so
	// this is never a mixture and a subscriber never has to guess.
	Tenant string

	// IDs are the documents written or removed.
	IDs []string

	// Deleted distinguishes a removal from a write.
	//
	// The server cache treats the two the same, because either way everything it
	// derived from the tenant's corpus is now a guess. The difference is for the
	// browser: a write means what is on screen may be out of date and should be
	// revalidated, and a delete means something on screen may already be gone,
	// which is worth acting on before the revalidation comes back.
	Deleted bool
}

// Notify is the broadcaster a driver embeds to implement [Notifier].
//
// The zero value is ready to use. It is in this package rather than in each
// driver because there is nothing driver specific about it, and two copies of a
// subscriber list is two chances to leak one.
type Notify struct {
	mu   sync.RWMutex
	next uint64
	fns  map[uint64]func(Change)
}

// OnChange registers fn.
func (n *Notify) OnChange(fn func(Change)) (stop func()) {
	if fn == nil {
		return func() {}
	}
	n.mu.Lock()
	if n.fns == nil {
		n.fns = make(map[uint64]func(Change))
	}
	id := n.next
	n.next++
	n.fns[id] = fn
	n.mu.Unlock()

	return func() {
		n.mu.Lock()
		delete(n.fns, id)
		n.mu.Unlock()
	}
}

// Changed tells every subscriber. A driver calls it after the transaction has
// committed, and never while holding a lock a subscriber might want.
func (n *Notify) Changed(c Change) {
	if c.Tenant == "" && len(c.IDs) == 0 {
		return
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, fn := range n.fns {
		fn(c)
	}
}

// Changes groups a batch of ids by tenant and reports each group. A driver
// takes a batch that may span tenants, and a subscriber that had to split one
// would be doing the driver's bookkeeping for it.
func (n *Notify) Changes(byTenant map[string][]string, deleted bool) {
	for tenant, ids := range byTenant {
		n.Changed(Change{Tenant: tenant, IDs: ids, Deleted: deleted})
	}
}
