package sqlitestore

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/storetest"
)

func newStore(t *testing.T) store.Store { return open(t) }

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "genba.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestConformance(t *testing.T) {
	storetest.Run(t, newStore)
}

func TestRetrieverConformance(t *testing.T) {
	storetest.RunRetriever(t, newStore)
}

// TestMigrationsAreIdempotent opens the same file twice. The second open runs
// the same migration code against a database that is already current, which is
// the case every restart hits and the one that is easy to get wrong.
func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "genba.db")

	first, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Put(t.Context(), readable("d1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if _, err := second.Get(t.Context(), reader(), "d1"); err != nil {
		t.Fatalf("the document written before the reopen is gone: %v", err)
	}
	st, err := second.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Documents != 1 {
		t.Fatalf("after reopening, Stats reports %d documents, want 1", st.Documents)
	}
}

// TestPermissionFilterIsInTheQuery is the one that has to keep passing.
//
// It counts the rows the database handed back. A driver that asked SQLite for
// everything and then dropped the documents the reader may not see would have
// the same visible behaviour and a row count in the thousands, so the count is
// the only thing that tells the two apart from outside.
func TestPermissionFilterIsInTheQuery(t *testing.T) {
	s := open(t)

	const n = 500
	docs := make([]doc.Document, 0, n)
	for i := range n {
		docs = append(docs, readable(id(i)))
	}
	if err := s.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s.rows.Store(0)
	var seen int
	if err := s.Retrieve(t.Context(), stranger(), store.Request{}, func(doc.Document) bool {
		seen++
		return true
	}); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if seen != 0 {
		t.Fatalf("a reader with no access retrieved %d documents", seen)
	}
	if got := s.rows.Load(); got != 0 {
		t.Fatalf("the database returned %d rows for a reader who may see nothing, so the permission filter ran in Go rather than in the query", got)
	}

	// The same walk for somebody who may read one document has to cost one row,
	// not five hundred.
	only := readable("only")
	only.Permissions.AllowGroups = []acl.Ref{{Source: "gdrive", Value: "sales@acme.com"}}
	if err := s.Put(t.Context(), only); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s.rows.Store(0)
	if err := s.Retrieve(t.Context(), stranger(), store.Request{}, func(doc.Document) bool { return true }); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if got := s.rows.Load(); got != 1 {
		t.Fatalf("the database returned %d rows where one document is readable, want 1", got)
	}
}

// TestTermsAreInTheQuery is the same argument for the full text index. A driver
// that matched terms in Go would still return the right document and would have
// read the whole corpus to do it.
func TestTermsAreInTheQuery(t *testing.T) {
	s := open(t)

	const n = 500
	docs := make([]doc.Document, 0, n+1)
	for i := range n {
		docs = append(docs, readable(id(i)))
	}
	needle := readable("needle")
	needle.Title = "Sourdough starter"
	needle.Body = "feed it twice a day with rye flour"
	docs = append(docs, needle)
	if err := s.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s.rows.Store(0)
	var got []string
	if err := s.Retrieve(t.Context(), reader(), store.Request{Terms: []string{"sourdough"}}, func(d doc.Document) bool {
		got = append(got, d.ID)
		return true
	}); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != 1 || got[0] != "needle" {
		t.Fatalf("retrieved %v, want just the one document carrying the term", got)
	}
	if rows := s.rows.Load(); rows != 1 {
		t.Fatalf("the database returned %d rows for a term one document carries, so the term filter is not in the query", rows)
	}
}

// TestAnalyzerAgreesWithFTS covers the seam between the Go analyzer and
// SQLite's tokenizer. They are two pieces of code splitting text into words, and
// the way they fail is quietly: a term one produces and the other does not is a
// document that Retrieve misses and Scan finds.
func TestAnalyzerAgreesWithFTS(t *testing.T) {
	s := open(t)

	docs := []doc.Document{
		text("ascii", "Deploy runbook", "roll back the release with kubectl"),
		text("punct", "queue.go", "package queue, implements failover"),
		text("digits", "Postmortem 2026-01-14", "the incident lasted 42 minutes"),
		text("accents", "Café menu", "crème brûlée and a piccolo"),
		text("cjk", "支払いのランブック", "決済キューのフェイルオーバー手順"),
		text("mixed", "SLO review Q3", "p95 latency 250ms across the fleet"),
	}
	if err := s.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, d := range docs {
		for _, term := range d.Terms() {
			r := store.Request{Terms: []string{term}}

			var got []string
			if err := s.Retrieve(t.Context(), reader(), r, func(d doc.Document) bool {
				got = append(got, d.ID)
				return true
			}); err != nil {
				t.Fatalf("Retrieve %q: %v", term, err)
			}

			var want []string
			if err := s.Scan(t.Context(), reader(), func(d doc.Document) bool {
				if r.Matches(d) {
					want = append(want, d.ID)
				}
				return true
			}); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("term %q: the index found %v and the go rule found %v", term, got, want)
			}
		}
	}
}

// TestPutIsIdempotent writes the same document twice. The full text index is a
// separate table keyed on the rowid, so a second write is where a stale row
// would show up as a duplicate hit.
func TestPutIsIdempotent(t *testing.T) {
	s := open(t)

	d := readable("d1")
	if err := s.Put(t.Context(), d, d, d); err != nil {
		t.Fatalf("Put: %v", err)
	}
	d.Title = "Payments failover, revised"
	if err := s.Put(t.Context(), d); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var got []string
	if err := s.Retrieve(t.Context(), reader(), store.Request{Terms: []string{"payments"}}, func(d doc.Document) bool {
		got = append(got, d.ID)
		return true
	}); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("after writing the same document four times, retrieve returned %v", got)
	}

	back, err := s.Get(t.Context(), reader(), "d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if back.Title != "Payments failover, revised" {
		t.Fatalf("Get returned the title %q, want the one that was written last", back.Title)
	}
}

// TestRoundTrip checks that a document comes back the way it went in. The
// columns are an index over the document, and the document itself is stored
// whole precisely so this holds.
func TestRoundTrip(t *testing.T) {
	s := open(t)

	want := doc.Document{
		ID:     "d1",
		Tenant: "acme",
		Source: "gdrive",
		Kind:   doc.KindPage,
		Title:  "Payments failover runbook",
		Body:   "how to fail over the payments queue",
		URL:    "https://drive.example.com/d/1",
		Author: doc.Person{Subject: "u_mei", Name: "Mei Tanaka", Email: "mei@acme.com",
			Identity: acl.Identity{Source: "gdrive", Value: "mei@acme.com"}},
		Owner:        doc.Person{Subject: "u_mei", Name: "Mei Tanaka", Email: "mei@acme.com"},
		Container:    "Platform",
		CreatedAt:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ModifiedAt:   time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
		IndexedAt:    time.Date(2026, 2, 3, 4, 5, 7, 0, time.UTC),
		SourceUpdate: "rev-9",
		Permissions:  perm(),
		Properties:   map[string]string{"mime": "application/vnd.google-apps.document"},
	}
	if err := s.Put(t.Context(), want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(t.Context(), reader(), "d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.ModifiedAt.Equal(want.ModifiedAt) || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("timestamps did not survive the round trip: got %v and %v", got.CreatedAt, got.ModifiedAt)
	}
	got.CreatedAt, got.ModifiedAt, got.IndexedAt = want.CreatedAt, want.ModifiedAt, want.IndexedAt
	if got.ID != want.ID || got.Title != want.Title || got.Body != want.Body ||
		got.URL != want.URL || got.Container != want.Container ||
		got.SourceUpdate != want.SourceUpdate || got.Author != want.Author ||
		got.Properties["mime"] != want.Properties["mime"] {
		t.Fatalf("the document did not survive the round trip:\n got %+v\nwant %+v", got, want)
	}
}

// TestClosedStore checks that a closed store says so rather than panicking on a
// nil database somewhere further down.
func TestClosedStore(t *testing.T) {
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "genba.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("closing twice: %v", err)
	}
	if err := s.Put(t.Context(), readable("d1")); err == nil {
		t.Fatal("Put on a closed store returned no error")
	}
	if _, err := s.Get(t.Context(), reader(), "d1"); err == nil {
		t.Fatal("Get on a closed store returned no error")
	}
}

func id(i int) string { return "d" + string(rune('a'+i%26)) + "-" + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func perm() acl.Permissions {
	return acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
		Version:     1,
	}
}

func readable(docID string) doc.Document {
	return doc.Document{
		ID:          docID,
		Tenant:      "acme",
		Source:      "gdrive",
		Kind:        doc.KindPage,
		Title:       "Payments failover runbook",
		Body:        "how to fail over the payments queue",
		Container:   "Platform",
		ModifiedAt:  time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC),
		Permissions: perm(),
	}
}

func text(docID, title, body string) doc.Document {
	d := readable(docID)
	d.Title, d.Body = title, body
	return d
}

func reader() *acl.Principal {
	return &acl.Principal{
		Tenant:     "acme",
		Subject:    "u_mei",
		Identities: []acl.Identity{{Source: "gdrive", Value: "mei@acme.com"}},
		Groups:     acl.GroupSet{Version: 1, Members: []string{"gdrive:eng@acme.com"}},
	}
}

func stranger() *acl.Principal {
	return &acl.Principal{
		Tenant:     "acme",
		Subject:    "u_kenji",
		Identities: []acl.Identity{{Source: "gdrive", Value: "kenji@acme.com"}},
		Groups:     acl.GroupSet{Version: 1, Members: []string{"gdrive:sales@acme.com"}},
	}
}
