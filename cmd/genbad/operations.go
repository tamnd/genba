package main

import (
	"slices"
	"sync"
	"time"

	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/ingest"
)

// runHistory is how many syncs per source are kept.
//
// Ten, which is enough to see a pattern and small enough to hold forever. The
// thing an operator is looking for on this screen is not one failure, it is
// whether the failures started at four this morning or have been there since
// the process came up, and a single last run cannot answer that. Keeping them
// all could: a source on a ten second refresh produces eight and a half
// thousand entries a day, in memory, to answer a question the last ten already
// answered.
const runHistory = 10

// operations is what the administration screen reads.
//
// It is the same shape of thing as [indexing] and it is here for the same
// reason: the process running the connectors is the only thing that knows how
// they are getting on, and the API package deliberately does not. This is where
// that knowledge is written down, and [operations.State] is the function handed
// to [api.WithOperations].
//
// Everything in it is in memory and nothing survives a restart. That is honest
// rather than lazy. A sync history read out of a database would be a history of
// every process that ever ran against it, and the question this screen answers
// is what is happening now, which is a question about this process.
type operations struct {
	mu sync.Mutex

	// sources is each connector's state, and order is the order they were
	// registered in, so that a process reading a directory and a bucket draws
	// them in the same order every time rather than in map order.
	sources map[string]*sourceOps
	order   []string
}

// sourceOps is one connector.
type sourceOps struct {
	// info is everything about the source that does not change while it runs.
	info api.Connector

	// runs are the syncs, newest first.
	runs []api.Run

	// syncing says one is going right now.
	syncing bool

	// policy is the permission policy, asked for its counters at report time
	// rather than copied, because they climb during a run and a copy taken when
	// the connector was registered would be zeroes forever. It is an any
	// because the two connectors have unrelated policy interfaces and what is
	// wanted here is the one method some of their implementations share.
	policy any
}

// newOperations returns an empty recorder.
func newOperations() *operations {
	return &operations{sources: make(map[string]*sourceOps, 2)}
}

// register records a connector before it starts syncing.
//
// Called before the first run rather than after it, so that a screen opened
// during the first crawl of a large corpus shows the connector with no runs
// yet, rather than showing nothing and looking like a process with no
// connectors at all.
func (o *operations) register(info api.Connector, policy any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if s, seen := o.sources[info.Source]; seen {
		// A connector that was stopped and started again is the same row with
		// the same history rather than a second connector, so the settings are
		// replaced and the runs are left alone. An operator who switched a
		// source off to change a directory and switched it back on wants the
		// failures that made them do it to still be on the screen.
		s.info = info
		s.policy = policy
		return
	}
	o.sources[info.Source] = &sourceOps{info: info, policy: policy}
	o.order = append(o.order, info.Source)
}

// enable records whether a connector is meant to be running.
//
// It is separate from register because stopping one keeps everything else about
// it: the settings, the run history and the place in the order. Losing those on
// a stop would make switching a noisy source off cost the evidence of why.
func (o *operations) enable(source string, on bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.sources[source]
	if !ok {
		return
	}
	s.info.Enabled = on
	if !on {
		// Nothing is running, so nothing is syncing. The goroutine that would
		// have said so has been cancelled, and a row left saying it is syncing
		// forever is worse than one that says nothing.
		s.syncing = false
	}
}

// forget drops a connector that has been removed.
//
// The run history goes with it, which is the one place this screen loses
// something on purpose: the connector is gone, and a history of a source
// nothing feeds any more is a row nobody can act on.
func (o *operations) forget(source string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.sources[source]; !ok {
		return
	}
	delete(o.sources, source)
	o.order = slices.DeleteFunc(o.order, func(s string) bool { return s == source })
}

// starting records that a sync has begun.
func (o *operations) starting(source string, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.sources[source]
	if !ok {
		return
	}
	s.syncing = true
	s.runs = append([]api.Run{{Started: at.UTC().Format(time.RFC3339)}}, s.runs...)
	if len(s.runs) > runHistory {
		s.runs = s.runs[:runHistory]
	}
}

// finished records what a sync did, and what went wrong if anything did.
//
// The stats are recorded whether or not there is an error, because a run that
// failed halfway still indexed what it got to and an operator reading a failure
// wants to know whether it managed anything at all.
func (o *operations) finished(source string, took time.Duration, stats ingest.Stats, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.sources[source]
	if !ok || len(s.runs) == 0 {
		return
	}
	s.syncing = false
	run := &s.runs[0]
	run.Duration = took.Milliseconds()
	run.Indexed = stats.Indexed
	run.Quarantined = stats.Quarantined
	run.Deleted = stats.Deleted
	run.Repermissioned = stats.Repermissioned
	run.Skipped = stats.Skipped
	run.Bytes = stats.Bytes
	if err != nil {
		run.Error = err.Error()
	}
}

// State is what the API reports, and is the function handed to
// [api.WithOperations].
func (o *operations) State() api.Operations {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := api.Operations{Connectors: make([]api.Connector, 0, len(o.order))}
	for _, source := range o.order {
		s := o.sources[source]
		c := s.info
		c.Syncing = s.syncing
		// Copied rather than shared. The caller encodes this while syncs keep
		// running, and a slice handed out under a lock is a slice read without
		// one.
		c.Runs = append([]api.Run(nil), s.runs...)
		c.Permissions = mappingOf(s.policy)
		out.Connectors = append(out.Connectors, c)
	}
	return out
}

// mappingOf is what a source's permission policy has seen, or nil for one that
// does not count.
//
// The interface is the same one [reportMapping] asserts on, which is the point:
// this screen and that log line report the same numbers from the same place,
// so a deployment that reads its logs and a deployment that reads its screens
// cannot be told two different things.
func mappingOf(policy any) *api.Mapping {
	counter, ok := policy.(aclCounter)
	if !ok {
		return nil
	}
	c := counter.Counts()
	return &api.Mapping{
		Mapped:         c.Mapped,
		ForeignDomain:  c.ForeignDomain,
		UnmappableDeny: c.UnmappableDeny,
		Malformed:      c.Malformed,
		Ignored:        c.Ignored,
	}
}

// refreshOf is how a sync interval reads on the screen, and is empty for a
// source that syncs once and never again.
func refreshOf(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}
