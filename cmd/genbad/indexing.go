package main

import (
	"context"
	"sync"

	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/store"
)

// indexing is what the server answers when somebody asks whether the corpus on
// screen is all of it.
//
// It only ever reports a source being read for the very first time, which is
// the state where an answer is true and incomplete at the same time. A refresh
// over a corpus that is already indexed is not reported, because during one of
// those every query returns the whole answer and a banner saying otherwise
// would be a lie that appears once a minute forever.
type indexing struct {
	// firstRun says this process came up on an index with nothing in it.
	//
	// It is decided once, before any connector starts, rather than per source.
	// Asking the store twice would give two different answers on a server
	// reading a directory and a bucket, because by the time the second one asks
	// the first has already written something.
	firstRun bool

	mu sync.Mutex

	// runs is the state of each source, and order is the order they were first
	// seen in. A process reading a directory and a bucket has two, and the one
	// that started first is the one reported, so the banner does not swap
	// between them while both are going.
	runs  map[string]*firstRead
	order []string
}

// firstRead is one source being read for the first time.
type firstRead struct {
	// total is what the source held when it was counted, and zero until the
	// count comes back or when the source cannot be counted at all.
	total int

	// done is how many documents the sync has stored so far.
	done int

	// over is set when the run finished, and is what keeps a source that has
	// been read from being counted again on the next refresh.
	over bool
}

// newIndexing decides whether this process is starting on an empty index, which
// is the only situation any of this reports on.
func newIndexing(ctx context.Context, st store.Store) *indexing {
	return &indexing{firstRun: indexIsEmpty(ctx, st), runs: make(map[string]*firstRead, 2)}
}

// expect records that a source is about to be read for the first time.
//
// It is called before the sync starts, and before the count that fills in the
// second number, so that the moment between a server answering and a server
// knowing how much work it has is a moment it reports as indexing rather than
// as done. The alternative is a readiness check that says a crawl has finished
// because it has not begun.
// It returns whether the source is being tracked, which is the answer to
// whether counting it is worth a walk.
func (i *indexing) expect(source string) bool {
	if !i.firstRun {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, seen := i.runs[source]; seen {
		return false
	}
	i.runs[source] = &firstRead{}
	i.order = append(i.order, source)
	return true
}

// counting fills in how much of a source there is to read. A total of zero
// means the count failed or the source cannot be counted.
func (i *indexing) counting(source string, total int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if r, ok := i.runs[source]; ok {
		r.total = total
	}
}

// advance records how far the run on a source has got. It does nothing for a
// source nobody is expecting, which is every refresh after the first read.
func (i *indexing) advance(source string, done int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if r, ok := i.runs[source]; ok {
		r.done = done
	}
}

// finished records that the run on a source is over, whether it succeeded or
// not. A failed first read is still over: the banner promises that results are
// partial until this finishes, and a sync that died has finished.
func (i *indexing) finished(source string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if r, ok := i.runs[source]; ok {
		r.over = true
	}
}

// State is what the API reports, and is the function handed to
// [api.WithIndexing].
//
// A source that is being read but has not been counted yet is reported with a
// total of zero, which says a sync is running and declines to guess how long it
// has left. Whoever renders it decides what to do with that, and the interface
// waits for the second number before it puts a banner up.
func (i *indexing) State() (api.Indexing, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, source := range i.order {
		r := i.runs[source]
		// A run that has already stored more than the count said there was has
		// nothing left to say. That happens on a tree people are working in, and
		// reporting 22,100 of about 22,000 would make the one number on the line
		// the one nobody believes.
		if r.over || (r.total > 0 && r.done >= r.total) {
			continue
		}
		return api.Indexing{Source: source, Done: r.done, Total: r.total}, true
	}
	return api.Indexing{}, false
}

// countSource is how many documents a source holds, for the second number on
// the banner.
//
// It is the enumeration the reconciliation sweep is built on, which reads
// metadata and no content: on a filesystem that is a stat per file against a
// read per file, and on a bucket it is one list request per thousand objects.
// A source that cannot enumerate returns zero and gets no banner, which is the
// right answer rather than a guess.
func countSource(ctx context.Context, c connector.Connector) int {
	enum, ok := c.(connector.Enumerator)
	if !ok {
		return 0
	}
	n := 0
	if err := enum.Enumerate(ctx, func(connector.Item) bool {
		n++
		return true
	}); err != nil {
		return 0
	}
	return n
}

// indexIsEmpty says whether this process started on an index with nothing in
// it, which is what makes the syncs about to run first reads rather than catch
// ups.
//
// The store is asked rather than the checkpoints, because checkpoints are held
// in memory and a restart against a SQLite file that already holds the whole
// corpus reads it all again from a zero cursor. That run is not a first read.
// Nothing on screen is missing while it goes, and it must not raise a banner.
func indexIsEmpty(ctx context.Context, st store.Store) bool {
	s, err := st.Stats(ctx)
	if err != nil {
		return false
	}
	return s.Documents == 0
}
