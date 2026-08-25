// Package audit records who saw what.
//
// Any company that would run a search engine over its own documents has to be
// able to answer that question, and answer it months after the fact, usually to
// somebody who is not going to accept "the logs have rolled". So this is not a
// feature of the interface, it is a property of the system: every path that
// returns content writes a record, there is no configuration that turns it off,
// and the only thing a deployment chooses is where the records go and how long
// they are kept.
//
// # What is in a record
//
// A record says who asked, what they asked, which documents came back and
// through which surface. It does not say what was in them. A title is content
// and a snippet is more of it, so neither is here: an audit log that quoted the
// documents would be a second copy of the corpus, kept for years, in a file that
// is deliberately easier to ship off the machine than the index is.
//
// Group names are left out for the same reason with a different edge to it. The
// rule that admitted somebody is recorded, because "they own it" and "the
// document is public to the tenant" are different facts about an access, but the
// group that matched is not, because a log of access decisions that named groups
// would describe the shape of an organisation to anybody who can read it.
//
// # What it costs
//
// Records are handed to a writer that runs on its own goroutine, so a request
// pays a channel send rather than a write to disk. The queue is bounded and a
// full one blocks the request rather than dropping the record, which is the
// deliberate choice: an audit log with a hole in it exactly where the busiest
// minute of the day was is worse than a slow request, because nobody can tell
// afterwards which of the two it was.
//
// # Where they go
//
// [Logging] writes them to the process log, which is the default and is why
// there is no way to switch this off: a deployment that configures nothing still
// has its records in whatever it already collects. [Open] writes JSON Lines to a
// directory, one file per day, which is the shape an existing system can ingest
// without anybody writing a parser. See docs/audit.md.
package audit

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Action is what somebody did, in the vocabulary of the product rather than of
// HTTP. A person who reads an audit log is looking for "who downloaded that",
// and GET is not the answer to it.
type Action string

// The actions. They are constants because an export is read by something that
// filters on them, and a typo in a string literal is a filter that silently
// matches nothing.
const (
	// Search is a query against the corpus. The documents on the record are the
	// page that came back, which is what was actually shown to somebody.
	Search Action = "search"

	// Suggest is the typeahead. It is separate from a search because it runs on
	// every keystroke and its records are the noisiest thing in the log, and
	// somebody reading a person's activity needs to be able to tell the two
	// apart at a glance.
	Suggest Action = "suggest"

	// List is a surface that returns documents without a query: what somebody
	// opened recently, what has been reported as out of date.
	List Action = "list"

	// Read is one document opened, which is the record most questions are
	// really about.
	Read Action = "read"

	// Content is the bytes themselves leaving the system, which is the one
	// somebody investigating an incident looks for first.
	Content Action = "content"

	// Thumbnail is a rendering of those bytes. It is not the document and it is
	// enough of it to read a page over somebody's shoulder, so it is audited
	// like the content it comes from.
	Thumbnail Action = "thumbnail"
)

// Outcome is what happened.
type Outcome string

const (
	// Served means content came back.
	Served Outcome = "served"

	// Refused means the request was answered without content: no such document,
	// nothing this person may read, or a role they do not have. The three are
	// deliberately one outcome here, for the same reason the API answers all
	// three with a 404. An audit log that separated them would be a way of
	// proving a document exists to somebody who cannot read it, and it would be
	// read by more people than the API is.
	Refused Outcome = "refused"

	// Failed means the request could not be answered. It is not a refusal and it
	// is not an access, and an investigation that counted it as either would be
	// counting outages as behaviour.
	Failed Outcome = "failed"
)

// Item is one document on a record.
//
// It is an id and where it came from, and never a title. The id is what an
// investigation follows back into the source, and the source is what turns a
// list of ids into a sentence somebody can act on: eleven documents out of the
// shared drive rather than eleven documents.
type Item struct {
	ID     string `json:"id"`
	Source string `json:"source,omitempty"`
}

// Record is one access.
type Record struct {
	// At is when the request was answered, in UTC. It is stamped by the log
	// rather than by the caller, so that two records cannot disagree about
	// order because two handlers read the clock differently.
	At time.Time `json:"at"`

	// Tenant and Subject are who asked. Subject is the identity the deployment
	// authenticated, which is what an investigation has to be able to join
	// against a directory months later.
	Tenant  string `json:"tenant"`
	Subject string `json:"subject"`

	// Kind is what sort of principal it was, a person or a service account.
	// Every deployment eventually has an integration reading documents on a
	// schedule, and an audit log where those are indistinguishable from people
	// is one where the interesting rows are buried.
	Kind string `json:"kind,omitempty"`

	// Surface is the route pattern the access came through, so a record says
	// through which door rather than only which document. The pattern rather
	// than the path, because a path carries document ids and they are already
	// on the record in a field something can filter on.
	Surface string `json:"surface"`

	// Action is what they did and Outcome is what happened.
	Action  Action  `json:"action"`
	Outcome Outcome `json:"outcome"`

	// Query is what somebody typed, on the surfaces where they typed something.
	//
	// It is their own words rather than anybody's document, which is why it is
	// allowed here at all, and it is the field that makes the log answer the
	// question people actually ask: not which documents were opened but what
	// somebody was looking for.
	Query string `json:"query,omitempty"`

	// Documents is what came back, and Count is how many. Count is not
	// len(Documents), because a surface that returns a page of a large result
	// set records the page it showed and the size of what it matched.
	Documents []Item `json:"documents,omitempty"`
	Count     int    `json:"count"`

	// Rule is the clause of the access control list that admitted them, where
	// one document was read and the answer is known. It is the rule and never
	// the group that matched it. See the package documentation.
	Rule string `json:"rule,omitempty"`

	// Bytes is how much content left the system, for the surfaces that serve
	// bytes. It is zero everywhere else rather than an estimate of a JSON body,
	// because the number is here to answer "how much of it did they take" and a
	// response size does not answer that.
	Bytes int64 `json:"bytes,omitempty"`
}

// QueueSize is how many records may be waiting to be written.
//
// It is large enough to absorb a burst from every handler on a busy process and
// small enough that a sink which has stopped accepting writes is felt as
// backpressure within a second rather than as a gigabyte of retained records.
const QueueSize = 4096

// Sink is where records are kept.
//
// Append is called from one goroutine, which is what lets a file sink hold a
// buffered writer without a lock on the write path. Flush is called when the
// queue is empty, so batching falls out of the arrival rate rather than out of a
// timer somebody has to tune.
type Sink interface {
	Append(Record) error
	Flush() error
	Close() error
}

// Stats is what the log has done, for the metrics that say whether it is
// keeping up.
type Stats struct {
	// Written is records the sink accepted.
	Written int64

	// Failed is records the sink refused. A record that could not be written is
	// reported and dropped: the alternative is a search endpoint that returns a
	// 500 because a disk is full, which turns a logging problem into an outage.
	Failed int64

	// Queued is how many are waiting. A number that is consistently near
	// [QueueSize] means the sink cannot keep up and requests are being made to
	// wait for it.
	Queued int
}

// Log is the append only audit log.
type Log struct {
	sink Sink
	log  *slog.Logger
	now  func() time.Time

	queue chan Record
	sync  chan chan error

	// quit is closed by Close and closed is how the writer says it has
	// finished. Two channels rather than one, because the writer has to be able
	// to report what the sink said on the way out.
	quit   chan struct{}
	closed chan error

	written atomic.Int64
	failed  atomic.Int64

	closeOnce sync.Once
	closeErr  error
}

// Option configures a [Log].
type Option func(*Log)

// WithLogger sets where a sink failure is reported. It is not where the records
// go: for that, pass [Logging] as the sink.
func WithLogger(l *slog.Logger) Option {
	return func(a *Log) {
		if l != nil {
			a.log = l
		}
	}
}

// WithClock sets the clock that stamps records.
func WithClock(now func() time.Time) Option {
	return func(a *Log) {
		if now != nil {
			a.now = now
		}
	}
}

// WithQueue sets how many records may be waiting.
func WithQueue(n int) Option {
	return func(a *Log) {
		if n > 0 {
			a.queue = make(chan Record, n)
		}
	}
}

// New returns a log writing to a sink.
//
// A nil sink is [Logging] over the logger, which is what makes this impossible
// to turn off by leaving something out of a configuration file. There is no
// option here that stops records being written, and that is on purpose: a
// deployment that could be configured into not auditing is one that will be, by
// somebody who is trying to make a disk fit.
func New(sink Sink, opts ...Option) *Log {
	a := &Log{
		log:    slog.Default(),
		now:    time.Now,
		queue:  make(chan Record, QueueSize),
		sync:   make(chan chan error),
		quit:   make(chan struct{}),
		closed: make(chan error, 1),
	}
	for _, opt := range opts {
		opt(a)
	}
	if sink == nil {
		sink = Logging(a.log)
	}
	a.sink = sink
	go a.run()
	return a
}

// Write records one access.
//
// It blocks if the queue is full, which is the whole design in one line. See the
// package documentation.
func (a *Log) Write(rec Record) {
	if rec.At.IsZero() {
		rec.At = a.now()
	}
	rec.At = rec.At.UTC()
	select {
	case a.queue <- rec:
	case <-a.quit:
		// The process is shutting down and this record arrived after the log
		// was closed. Reported rather than swallowed, because a handler still
		// serving content after the audit log has been closed is a shutdown
		// ordering bug and this is the only place it is visible.
		a.log.Error("an access was not audited because the log is closed",
			"tenant", rec.Tenant, "subject", rec.Subject, "surface", rec.Surface)
	}
}

// Sync waits for everything written so far to reach the sink, and flushes it.
//
// It is here for two callers: a test that has just made a request and wants to
// read the record back, and a process that is shutting down. Nothing on a
// request path should call it.
func (a *Log) Sync() error {
	reply := make(chan error, 1)
	select {
	case a.sync <- reply:
		return <-reply
	case <-a.quit:
		return nil
	}
}

// Stats reports what the log has done.
func (a *Log) Stats() Stats {
	return Stats{
		Written: a.written.Load(),
		Failed:  a.failed.Load(),
		Queued:  len(a.queue),
	}
}

// Close drains what is waiting, flushes the sink and closes it.
//
// A process that exits without calling this loses whatever had not reached the
// sink yet, which is the price of not paying for a write to disk on the request
// path. It is why this is called from the shutdown path of the command rather
// than left to a finaliser.
func (a *Log) Close() error {
	a.closeOnce.Do(func() {
		close(a.quit)
		a.closeErr = <-a.closed
	})
	return a.closeErr
}

// run is the writer. It is the only goroutine that touches the sink, which is
// what lets a sink hold a buffer without a lock on it.
func (a *Log) run() {
	for {
		select {
		case rec := <-a.queue:
			a.append(rec)
			// Flushed when nothing else is waiting, so a burst costs one write
			// to disk and a quiet process has its last record on disk
			// immediately rather than at the end of an interval.
			if len(a.queue) == 0 {
				a.flush()
			}
		case reply := <-a.sync:
			a.drain()
			reply <- a.flush()
		case <-a.quit:
			a.drain()
			err := a.flush()
			if cerr := a.sink.Close(); cerr != nil && err == nil {
				err = cerr
			}
			a.closed <- err
			return
		}
	}
}

// drain writes what is already queued and returns as soon as nothing is.
func (a *Log) drain() {
	for {
		select {
		case rec := <-a.queue:
			a.append(rec)
		default:
			return
		}
	}
}

// append writes one record.
//
// A sink that refuses is reported and the record is lost, because the caller is
// a request that has already been answered. Failing the request instead would
// mean a full disk answering a search with a 500, which is a logging problem
// escalated into an outage, and the count is on [Stats] so that a deployment can
// alert on it.
func (a *Log) append(rec Record) {
	if err := a.sink.Append(rec); err != nil {
		a.failed.Add(1)
		a.log.Error("an audit record could not be written",
			"error", err, "tenant", rec.Tenant, "subject", rec.Subject, "surface", rec.Surface)
		return
	}
	a.written.Add(1)
}

// flush pushes the sink's buffer, reporting a failure the same way.
func (a *Log) flush() error {
	err := a.sink.Flush()
	if err != nil {
		a.log.Error("audit records could not be flushed", "error", err)
	}
	return err
}
