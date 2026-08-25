package ingest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// SampleSize is how many ids a [Reconciliation] keeps per category.
//
// The counts are exact and the ids are a sample, because the first sweep after
// a fresh index finds the whole corpus missing and a report that named all of
// it would be a copy of the corpus in memory and a log line nobody reads. A
// handful of ids is what an operator actually uses: it is enough to go and look
// at one and find out what is wrong.
const SampleSize = 20

// Discrepancies is how many documents fell into one category and a sample of
// which ones.
type Discrepancies struct {
	// Count is exact.
	Count int

	// IDs holds the first [SampleSize] of them, in the order they were found.
	IDs []string
}

func (d *Discrepancies) add(id string) {
	d.Count++
	if len(d.IDs) < SampleSize {
		d.IDs = append(d.IDs, id)
	}
}

// Reconciliation is what one sweep found and what it did about it.
type Reconciliation struct {
	// Tenant and Source are what was compared.
	Tenant string
	Source string

	// SourceItems and IndexItems are the two sides of the comparison, before
	// anything was repaired.
	SourceItems int
	IndexItems  int

	// Missing is what the source holds and the index does not. It is the drift
	// a dropped change feed event leaves behind.
	Missing Discrepancies

	// Stale is what both hold, at different revisions. It is what a sync that
	// was killed between the store and the checkpoint leaves behind, and what a
	// source that changed a document without raising an event leaves behind.
	Stale Discrepancies

	// Extra is what the index holds and the source no longer does. It is the
	// one an operator cares about most, because a document that was deleted at
	// the source and is still in the index is still being served.
	Extra Discrepancies

	// Held is what the index is holding back because its permissions did not
	// resolve. It is not drift, and that is why it is not in [Drift]: the index
	// is right about it, it is right on purpose, and it is right until whatever
	// failed to resolve is fixed.
	//
	// It is refetched anyway, because the fix is at the source. A directory that
	// was down is up, a token that could not list a channel has been given the
	// scope, a group that did not exist has been created, and none of those
	// change the document's revision, so nothing else in this sweep would think
	// to ask about it again.
	Held Discrepancies

	// Repaired is how many were refetched and stored, held documents included.
	Repaired int

	// Released is how many of the held ones came back queryable, which is the
	// number an operator watching a fix take effect is actually looking for. A
	// sweep that refetched forty held documents and released none of them means
	// the cause is still there.
	Released int

	// Deleted is how many were removed from the index.
	Deleted int

	// Requests is what the sweep spent at the source, for a connector that
	// counts. A sweep over an index that is already correct should show one
	// enumeration and no fetches.
	Requests connector.Counters

	// Duration is the wall clock time of the sweep.
	Duration time.Duration
}

// Drift is how many documents disagreed, whether or not they were repaired.
func (r Reconciliation) Drift() int {
	return r.Missing.Count + r.Stale.Count + r.Extra.Count
}

// Reconcile compares what a source holds against what the index holds, repairs
// the difference, and reports it.
//
// This is the part that catches what the incremental path missed, and it exists
// because the incremental path cannot catch it by definition. A sync is a
// stream of changes, and it is exactly as complete as the change feed under it:
// a feed that drops an event, a source that does a bulk edit without raising
// any, a permission model that grants through a group the connector cannot see,
// a process killed at the wrong moment. None of those produce an error. They
// produce an index that is quietly wrong, and the only way to find out is to
// count both sides.
//
// It needs a [store.Maintenance] store and a [connector.Enumerator]. Refetching
// what is missing or stale also needs a [connector.Fetcher]: without one the
// sweep still reports and still deletes, and the missing documents wait for a
// full sync.
//
// # Quarantined documents are retried
//
// A document held back because its permissions did not resolve is refetched
// too, whatever its revision says. That is the automatic retry: the reason a
// document is held is at the source, so a directory that has come back up, a
// token that has been given the scope it was missing or a group that has since
// been created releases it on the next sweep without anybody going and finding
// the ids. A sweep on an index where nothing is held costs nothing extra, and
// [Reconciliation.Released] says how many came back.
//
// # Nothing is deleted on a partial enumeration
//
// If the walk of the source fails halfway, this returns the error and repairs
// nothing at all. It is the most important rule here. A list that stopped early
// is indistinguishable from a source that lost half its documents, and acting
// on the second reading would delete a working index because of a timeout.
func (p *Pipeline) Reconcile(ctx context.Context, tenant genba.TenantID, c connector.Connector) (Reconciliation, error) {
	if c == nil {
		return Reconciliation{}, errors.New("ingest: nil connector")
	}
	if tenant == "" {
		return Reconciliation{}, errors.New("ingest: empty tenant")
	}
	source := c.Source()
	rec := Reconciliation{Tenant: string(tenant), Source: source}

	if p.maint == nil {
		return rec, errors.New("ingest: this store cannot be reconciled, it does not implement store.Maintenance")
	}
	enum, ok := c.(connector.Enumerator)
	if !ok {
		return rec, fmt.Errorf("ingest: connector %s cannot be reconciled, it does not implement connector.Enumerator", source)
	}

	start := p.clock()
	before := countersOf(c)

	// One: what the source holds. This is held in memory for the length of the
	// sweep, which is the real cost of reconciliation and is worth naming: an
	// id and a version per document, so tens of megabytes for a corpus of a few
	// million. The alternative is sorting both sides on disk and merging them,
	// which is the right answer an order of magnitude further up and is not
	// worth its complexity here.
	items := make(map[string]string)
	if err := enum.Enumerate(ctx, func(it connector.Item) bool {
		if it.ID != "" {
			items[it.ID] = it.Version
		}
		return true
	}); err != nil {
		return rec, fmt.Errorf("ingest: enumerate %s: %w", source, err)
	}
	rec.SourceItems = len(items)

	// Two: what the index holds. Whatever is left in items afterwards is what
	// the index has never seen.
	var (
		stale  []string
		extra  []string
		quaran []string
	)
	if err := p.maint.Inventory(ctx, string(tenant), source, func(it store.Item) bool {
		rec.IndexItems++
		version, ok := items[it.ID]
		switch {
		case !ok:
			extra = append(extra, it.ID)
		case version != "" && version != it.Version:
			stale = append(stale, it.ID)
		case it.Held:
			// Only when the source still holds it and the revision agrees, so a
			// held document that is also stale is refetched once as stale rather
			// than twice, and one the source has deleted is deleted rather than
			// fetched.
			quaran = append(quaran, it.ID)
		}
		delete(items, it.ID)
		return true
	}); err != nil {
		return rec, fmt.Errorf("ingest: inventory %s: %w", source, err)
	}

	missing := make([]string, 0, len(items))
	for id := range items {
		missing = append(missing, id)
	}
	// Sorted so that two sweeps over the same drift report the same sample, and
	// so that the repair below touches documents in a stable order. Map order
	// would make the sample a lottery and the log unreadable.
	slices.Sort(missing)
	slices.Sort(stale)
	slices.Sort(extra)
	slices.Sort(quaran)

	for _, id := range missing {
		rec.Missing.add(id)
	}
	for _, id := range stale {
		rec.Stale.add(id)
	}
	for _, id := range extra {
		rec.Extra.add(id)
	}
	for _, id := range quaran {
		rec.Held.add(id)
	}

	if err := p.repair(ctx, string(tenant), c, &rec, missing, stale, extra, quaran); err != nil {
		rec.Duration = p.clock().Sub(start)
		rec.Requests = countersOf(c).Since(before)
		return rec, err
	}

	rec.Duration = p.clock().Sub(start)
	rec.Requests = countersOf(c).Since(before)

	p.log.Info("reconciled",
		"source", source,
		"tenant", string(tenant),
		"source_items", rec.SourceItems,
		"index_items", rec.IndexItems,
		"missing", rec.Missing.Count,
		"stale", rec.Stale.Count,
		"extra", rec.Extra.Count,
		"held", rec.Held.Count,
		"repaired", rec.Repaired,
		"released", rec.Released,
		"deleted", rec.Deleted,
		"requests", rec.Requests.Requests(),
		"duration", rec.Duration,
	)
	return rec, nil
}

// repair puts back what is missing, rewrites what is stale and deletes what the
// source no longer has.
//
// It reuses the run from a normal sync rather than writing to the store itself,
// so that a repaired document goes through the same batching, the same tenant
// and source stamping and the same quarantine rule as one that arrived from a
// change feed. A document that is only correct when it comes in through one of
// the two doors is a bug waiting for the other door.
func (p *Pipeline) repair(ctx context.Context, tenant string, c connector.Connector, rec *Reconciliation, missing, stale, extra, held []string) error {
	r := &run{pipeline: p, tenant: tenant, source: c.Source()}

	for _, id := range extra {
		if err := r.emit(ctx, connector.Change{Document: docWithID(id), Deleted: true}); err != nil {
			return err
		}
	}

	fetcher, ok := c.(connector.Fetcher)
	if !ok {
		if n := len(missing) + len(stale) + len(held); n > 0 {
			// Said once, loudly, rather than per document. A connector that
			// cannot fetch by id is not broken, but an operator reading a report
			// with a missing count in it needs to know why nothing happened
			// about it.
			p.log.Warn("documents are missing, stale or held and this connector cannot fetch by id, so they wait for a full sync",
				"source", c.Source(), "tenant", tenant, "count", n)
		}
		if err := r.flush(ctx); err != nil {
			return err
		}
		rec.Deleted = r.stats.Deleted
		return nil
	}

	refetch := func(ids []string) error {
		for _, id := range ids {
			d, err := fetcher.Fetch(ctx, id)
			switch {
			case errors.Is(err, connector.ErrGone):
				// The source changed its mind between the enumeration and now,
				// which is normal on a corpus people are working in. It is not
				// missing, it is deleted, and it is treated as one.
				if err := r.emit(ctx, connector.Change{Document: docWithID(id), Deleted: true}); err != nil {
					return err
				}
				continue
			case err != nil:
				return fmt.Errorf("ingest: fetch %s from %s: %w", id, c.Source(), err)
			}
			d.ID = id
			if err := r.emit(ctx, connector.Change{Document: d}); err != nil {
				return err
			}
		}
		return nil
	}

	if err := refetch(slices.Concat(missing, stale)); err != nil {
		return err
	}

	// The held ones are fetched second and counted separately, because what
	// they answer is a different question. Everything above is drift, and a
	// number that mixed the two would be one an operator cannot use: a sweep
	// that indexed forty documents says nothing about whether the directory
	// somebody has just fixed is fixed. The counter moves in emit rather than in
	// the flush, so this reads the line between the two without writing
	// anything in the middle of a batch.
	indexed := r.stats.Indexed
	if err := refetch(held); err != nil {
		return err
	}
	rec.Released = r.stats.Indexed - indexed

	if err := r.flush(ctx); err != nil {
		return err
	}
	rec.Repaired = r.stats.Indexed + r.stats.Quarantined
	rec.Deleted = r.stats.Deleted
	return nil
}

// docWithID is the whole of a document for a deletion. There is nothing left to
// index, so there is nothing else to fill in.
func docWithID(id string) doc.Document { return doc.Document{ID: id} }

// countersOf reads a connector's counters, or a zero reading from one that does
// not count.
func countersOf(c connector.Connector) connector.Counters {
	if counted, ok := c.(connector.Counted); ok {
		return counted.Counters()
	}
	return connector.Counters{}
}
