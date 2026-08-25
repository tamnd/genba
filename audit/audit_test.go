package audit_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/genba/audit"
)

// noon is the clock these tests run at, because a record carries a timestamp
// and a test that reads the wall clock is a test that cannot assert on one.
var noon = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

// memory is a sink that keeps what it was given.
type memory struct {
	mu      sync.Mutex
	records []audit.Record
	flushes int
	closed  bool
	err     error
}

func (m *memory) Append(rec audit.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.records = append(m.records, rec)
	return nil
}

func (m *memory) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushes++
	return nil
}

func (m *memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *memory) held() []audit.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]audit.Record(nil), m.records...)
}

// read is one document access, which is the record most of these tests write.
func read(subject, id string) audit.Record {
	return audit.Record{
		Tenant: "acme", Subject: subject, Kind: "user",
		Surface: "GET /api/v1/documents/{id}",
		Action:  audit.Read, Outcome: audit.Served,
		Documents: []audit.Item{{ID: id, Source: "gdrive"}},
		Count:     1,
		Rule:      "group",
	}
}

func logging(t *testing.T, sink audit.Sink) *audit.Log {
	t.Helper()
	a := audit.New(sink, audit.WithClock(func() time.Time { return noon }))
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// TestARecordReachesTheSink is the whole of the write path, and the timestamp
// assertion is the part worth having: it is stamped by the log rather than by
// the caller, so two handlers cannot disagree about the order of two accesses
// because they read the clock differently.
func TestARecordReachesTheSink(t *testing.T) {
	sink := &memory{}
	a := logging(t, sink)

	a.Write(read("u_mei", "d1"))
	if err := a.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	held := sink.held()
	if len(held) != 1 {
		t.Fatalf("the sink holds %d records, want 1", len(held))
	}
	if held[0].At != noon {
		t.Errorf("the record is stamped %v, want the log's clock", held[0].At)
	}
	if held[0].Subject != "u_mei" || held[0].Documents[0].ID != "d1" {
		t.Errorf("the record came through as %+v", held[0])
	}
	if sink.flushes == 0 {
		t.Error("nothing was flushed, so a quiet process would hold its last record in a buffer")
	}
}

// TestClosingWritesWhatIsStillWaiting. The queue is the reason a search does not
// wait for a disk, and it is also the reason a process that exits without
// draining loses the last records it served.
func TestClosingWritesWhatIsStillWaiting(t *testing.T) {
	sink := &memory{}
	a := audit.New(sink, audit.WithClock(func() time.Time { return noon }))

	for i := range 200 {
		a.Write(read("u_mei", string(rune('a'+i%26))))
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := len(sink.held()); got != 200 {
		t.Errorf("%d records reached the sink, want 200", got)
	}
	if !sink.closed {
		t.Error("the sink was left open, so a file would be left unflushed")
	}
}

// TestClosingTwiceIsAllowed, because shutdown paths run twice more often than
// anybody plans for and the second one should not panic on a closed channel.
func TestClosingTwiceIsAllowed(t *testing.T) {
	a := audit.New(&memory{})
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("the second close returned %v", err)
	}
}

// TestASinkThatRefusesIsCountedRatherThanFatal. The caller is a request that has
// already been answered, so failing it is not on the table, and a search
// endpoint returning 500 because a disk filled up is a logging problem promoted
// into an outage.
func TestASinkThatRefusesIsCountedRatherThanFatal(t *testing.T) {
	sink := &memory{err: errors.New("the disk is full")}
	var logged bytes.Buffer
	a := audit.New(sink,
		audit.WithClock(func() time.Time { return noon }),
		audit.WithLogger(slog.New(slog.NewTextHandler(&logged, nil))))
	t.Cleanup(func() { _ = a.Close() })

	a.Write(read("u_mei", "d1"))
	if err := a.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	st := a.Stats()
	if st.Failed != 1 || st.Written != 0 {
		t.Errorf("the log reports %d failed and %d written, want 1 and 0", st.Failed, st.Written)
	}
	if !strings.Contains(logged.String(), "could not be written") {
		t.Errorf("a record that was lost was not reported:\n%s", logged.String())
	}
}

// TestTheDefaultSinkIsTheProcessLog is why there is no way to turn this off. A
// deployment that configures nothing still has its records in whatever collects
// the process log.
func TestTheDefaultSinkIsTheProcessLog(t *testing.T) {
	var logged bytes.Buffer
	a := audit.New(nil,
		audit.WithClock(func() time.Time { return noon }),
		audit.WithLogger(slog.New(slog.NewTextHandler(&logged, nil))))

	a.Write(read("u_mei", "d1"))
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	line := logged.String()
	if !strings.Contains(line, audit.Message) {
		t.Fatalf("the record is not in the log:\n%s", line)
	}
	for _, want := range []string{"subject=u_mei", "tenant=acme", "action=read", "documents=d1", "sources=gdrive", "rule=group"} {
		if !strings.Contains(line, want) {
			t.Errorf("the logged record is missing %q:\n%s", want, line)
		}
	}
}

// TestOneLinePerAccess, whatever the size of the page. A log where one search
// becomes eleven lines is one where the number of accesses is whatever the page
// size happened to be.
func TestOneLinePerAccess(t *testing.T) {
	var logged bytes.Buffer
	a := audit.New(audit.Logging(slog.New(slog.NewTextHandler(&logged, nil))),
		audit.WithClock(func() time.Time { return noon }))

	a.Write(audit.Record{
		Tenant: "acme", Subject: "u_mei", Surface: "GET /api/v1/search",
		Action: audit.Search, Outcome: audit.Served, Query: "payments failover",
		Documents: []audit.Item{
			{ID: "d1", Source: "gdrive"},
			{ID: "d2", Source: "gdrive"},
			{ID: "d3", Source: "slack"},
		},
		Count: 3,
	})
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := strings.TrimSpace(logged.String())
	if lines := strings.Count(got, "\n") + 1; lines != 1 {
		t.Fatalf("one search wrote %d lines:\n%s", lines, got)
	}
	if !strings.Contains(got, "documents=d1,d2,d3") {
		t.Errorf("the page is not on the record:\n%s", got)
	}
	if !strings.Contains(got, "sources=gdrive,slack") {
		t.Errorf("the sources are not deduplicated:\n%s", got)
	}
}

// TestEveryRecordIsWrittenUnderLoad is the guarantee the queue exists to make.
// Nothing is sampled and nothing is dropped, however many goroutines are
// serving requests.
func TestEveryRecordIsWrittenUnderLoad(t *testing.T) {
	sink := &memory{}
	a := audit.New(sink,
		audit.WithClock(func() time.Time { return noon }),
		// Far smaller than the number written, so the writers have to be made
		// to wait rather than allowed to drop anything.
		audit.WithQueue(4))

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 250 {
				a.Write(read("u_mei", "d1"))
			}
		}()
	}
	wg.Wait()
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := len(sink.held()); got != 2000 {
		t.Errorf("%d records of 2000 were written", got)
	}
	if st := a.Stats(); st.Written != 2000 {
		t.Errorf("the log reports %d written, want 2000", st.Written)
	}
}
