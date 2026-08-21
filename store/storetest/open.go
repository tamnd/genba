package storetest

import (
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

// Remembering what somebody opened is the one write in this interface that a
// person makes about themselves rather than about the corpus, and it is the one
// place where a driver could reasonably decide the permission rule does not
// apply, on the grounds that this person had the document in front of them a
// moment ago. It does apply. Access is revoked between the open and the read
// often enough that it is the case worth writing down, and a list that keeps
// showing the title of a document somebody has been cut out of is a leak with a
// friendly name on it.

// openLog skips a case for a driver that cannot remember an open.
func openLog(t *testing.T, s store.Store) store.OpenLog {
	t.Helper()
	l, ok := s.(store.OpenLog)
	if !ok {
		t.Skip("driver does not implement store.OpenLog")
	}
	return l
}

// at is a fixed clock, so that the order of a history is the order the test
// asked for rather than whatever the machine managed between two calls.
func at(minute int) time.Time {
	return time.Date(2026, 8, 21, 9, minute, 0, 0, time.UTC)
}

func opened(t *testing.T, l store.OpenLog, p *acl.Principal, limit int) []string {
	t.Helper()
	opens, err := l.Opens(t.Context(), p, limit)
	if err != nil {
		t.Fatalf("Opens: %v", err)
	}
	ids := make([]string, 0, len(opens))
	for _, o := range opens {
		ids = append(ids, o.Document.ID)
	}
	return ids
}

func testOpenLogOrder(t *testing.T, s store.Store) {
	l := openLog(t, s)
	mustPut(t, s, document("d1", readable()), document("d2", readable()), document("d3", readable()))

	for i, id := range []string{"d1", "d2", "d3"} {
		if err := l.RecordOpen(t.Context(), reader(), id, at(i)); err != nil {
			t.Fatalf("RecordOpen %s: %v", id, err)
		}
	}

	got := opened(t, l, reader(), 10)
	want := []string{"d3", "d2", "d1"}
	if len(got) != len(want) {
		t.Fatalf("Opens returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Opens returned %v, want most recent first: %v", got, want)
		}
	}

	// The same document again moves rather than repeats, and the entry carries
	// the later time.
	if err := l.RecordOpen(t.Context(), reader(), "d1", at(9)); err != nil {
		t.Fatalf("RecordOpen: %v", err)
	}
	opens, err := l.Opens(t.Context(), reader(), 10)
	if err != nil {
		t.Fatalf("Opens: %v", err)
	}
	if len(opens) != 3 {
		t.Fatalf("Opens returned %d entries after opening a document twice, want 3", len(opens))
	}
	if opens[0].Document.ID != "d1" {
		t.Fatalf("Opens returned %s first, want the document that was just opened again", opens[0].Document.ID)
	}
	if !opens[0].At.Equal(at(9)) {
		t.Fatalf("the entry says it was opened at %v, want %v", opens[0].At, at(9))
	}

	// The document itself is in the entry, because every caller wants a title.
	if opens[0].Document.Title != "runbook d1" {
		t.Fatalf("the entry carries %+v, want the document", opens[0].Document)
	}
}

func testOpenLogLimit(t *testing.T, s store.Store) {
	l := openLog(t, s)
	mustPut(t, s, document("d1", readable()), document("d2", readable()))
	for i, id := range []string{"d1", "d2"} {
		if err := l.RecordOpen(t.Context(), reader(), id, at(i)); err != nil {
			t.Fatalf("RecordOpen: %v", err)
		}
	}
	if got := opened(t, l, reader(), 1); len(got) != 1 || got[0] != "d2" {
		t.Fatalf("Opens with a limit of one returned %v, want the most recent entry", got)
	}
	if got := opened(t, l, reader(), 0); len(got) != 0 {
		t.Fatalf("Opens with a limit of zero returned %v, want nothing", got)
	}
}

func testOpenLogPermissions(t *testing.T, s store.Store) {
	l := openLog(t, s)
	mustPut(t, s, document("d1", readable()))

	// Recording an open of something this person cannot read is not an error and
	// does not record anything. It cannot be an error: the answer would tell a
	// caller which ids exist.
	if err := l.RecordOpen(t.Context(), stranger(), "d1", at(0)); err != nil {
		t.Fatalf("RecordOpen for a document the caller may not read: %v", err)
	}
	if err := l.RecordOpen(t.Context(), stranger(), "does-not-exist", at(0)); err != nil {
		t.Fatalf("RecordOpen for a document that is not there: %v", err)
	}
	if got := opened(t, l, stranger(), 10); len(got) != 0 {
		t.Fatalf("Opens returned %v for a reader who opened nothing they may read", got)
	}

	// A history is one person's. Nobody else's open appears in it.
	if err := l.RecordOpen(t.Context(), reader(), "d1", at(0)); err != nil {
		t.Fatalf("RecordOpen: %v", err)
	}
	if got := opened(t, l, stranger(), 10); len(got) != 0 {
		t.Fatalf("Opens returned %v, want nothing: that was somebody else's open", got)
	}

	// Access revoked after the open. The entry stops being served, and it stops
	// being served without the caller filtering anything.
	mustPut(t, s, document("d1", acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "sales@acme.com"}},
		Version:     1,
	}))
	if got := opened(t, l, reader(), 10); len(got) != 0 {
		t.Fatalf("Opens returned %v after the reader lost access to it", got)
	}
}

func testOpenLogDelete(t *testing.T, s store.Store) {
	l := openLog(t, s)
	mustPut(t, s, document("d1", readable()), document("d2", readable()))
	for i, id := range []string{"d1", "d2"} {
		if err := l.RecordOpen(t.Context(), reader(), id, at(i)); err != nil {
			t.Fatalf("RecordOpen: %v", err)
		}
	}
	if err := s.Delete(t.Context(), "d2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got := opened(t, l, reader(), 10)
	if len(got) != 1 || got[0] != "d1" {
		t.Fatalf("Opens returned %v after the document was deleted, want only d1", got)
	}
}

func testOpenLogTenants(t *testing.T, s store.Store) {
	l := openLog(t, s)
	mustPut(t, s, document("d1", readable()))

	// The same subject string in another tenant is another person. A history
	// keyed by the subject alone would hand one of them the other's reading.
	other := reader()
	other.Tenant = "other"
	if err := l.RecordOpen(t.Context(), reader(), "d1", at(0)); err != nil {
		t.Fatalf("RecordOpen: %v", err)
	}
	if got := opened(t, l, other, 10); len(got) != 0 {
		t.Fatalf("Opens returned %v for the same subject in another tenant", got)
	}
}
