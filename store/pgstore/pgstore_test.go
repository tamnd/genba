package pgstore

import (
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/storetest"
)

// serverDSN is the database these tests run against.
//
// There is no in process Postgres, so without a server there is nothing to
// test and the tests skip rather than pass. A driver suite that quietly passed
// by testing nothing would be worse than one that says it did not run, so CI
// sets this and the skip is only ever hit on a laptop. See docs/postgres.md for
// how to get one.
func serverDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("GENBA_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set GENBA_TEST_POSTGRES to a PostgreSQL 18 connection string to run the pgstore tests")
	}
	return dsn
}

// schemas numbers the schemas so that two stores in one test do not share one.
var schemas atomic.Int64

// schemaDSN gives one test its own empty schema in the shared database.
//
// A schema rather than a database, because creating a database costs a template
// copy and there are a few hundred stores opened across this suite. The tables
// are all created unqualified, so a search_path pointing at an empty schema is
// enough to keep two tests from seeing each other's documents.
func schemaDSN(t *testing.T) string {
	t.Helper()
	base := serverDSN(t)

	name := "genba_test_" + sanitize(t.Name()) + "_" + strconv.FormatInt(schemas.Add(1), 10)
	admin, err := pgx.Connect(t.Context(), base)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = admin.Close(t.Context()) }()

	if _, err := admin.Exec(t.Context(), `DROP SCHEMA IF EXISTS "`+name+`" CASCADE`); err != nil {
		t.Fatalf("dropping a leftover schema: %v", err)
	}
	if _, err := admin.Exec(t.Context(), `CREATE SCHEMA "`+name+`"`); err != nil {
		t.Fatalf("creating a schema: %v", err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(t.Context(), base)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(t.Context()) }()
		_, _ = conn.Exec(t.Context(), `DROP SCHEMA IF EXISTS "`+name+`" CASCADE`)
	})
	return withSearchPath(t, base, name)
}

func sanitize(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func withSearchPath(t *testing.T, dsn, name string) string {
	t.Helper()
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn + " search_path=" + name
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing the connection string: %v", err)
	}
	q := u.Query()
	q.Set("search_path", name)
	u.RawQuery = q.Encode()
	return u.String()
}

func newStore(t *testing.T) store.Store { return open(t) }

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), schemaDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestConformance(t *testing.T) {
	storetest.Run(t, newStore)
}

func TestMaintenanceConformance(t *testing.T) {
	storetest.RunMaintenance(t, newStore)
}

func TestRetrieverConformance(t *testing.T) {
	storetest.RunRetriever(t, newStore)
}

// This driver has no [store.Speller], so every case here skips. The call is
// left in so that the day it grows one, the suite is already pointed at it.
func TestSpellerConformance(t *testing.T) {
	storetest.RunSpeller(t, newStore)
}

func TestRankerConformance(t *testing.T) {
	storetest.RunRanker(t, newStore)
}

// TestPermissionFilterIsInThePlan reads what Postgres says it is going to do.
//
// Counting rows tells you the filter ran somewhere in the database. The plan
// tells you where: that the deny list, the owner check and the mode are
// conditions on the scan rather than a step the driver bolted on afterwards, and
// that a query for a reader who may see nothing costs no rows at all. It
// explains the statement Retrieve actually runs, not a copy of it, because a
// plan for a query nothing executes proves nothing.
func TestPermissionFilterIsInThePlan(t *testing.T) {
	s := open(t)
	seed(t, s, 200)

	query, args := retrieveQuery(stranger(), store.Request{Terms: []string{"payments"}})
	var raw []byte
	if err := s.pool.QueryRow(t.Context(),
		`EXPLAIN (ANALYZE, FORMAT JSON) `+rebind(query), args...).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	plan := string(raw)

	// The three halves of acl.Permissions.Allows, in the plan. The deny list and
	// the ACL check are subplans over document_ref, the owner and the mode are
	// conditions on the document scan, and the tenant is on it too.
	for _, want := range []string{"document_ref", "owner_key", "mode", "tenant"} {
		if !strings.Contains(plan, want) {
			t.Fatalf("the plan does not mention %s, so that part of the permission rule is not in the query:\n%s", want, plan)
		}
	}

	var explained []struct {
		Plan struct {
			ActualRows float64 `json:"Actual Rows"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &explained); err != nil {
		t.Fatalf("reading the plan: %v", err)
	}
	if len(explained) != 1 {
		t.Fatalf("EXPLAIN returned %d plans", len(explained))
	}
	if got := explained[0].Plan.ActualRows; got != 0 {
		t.Fatalf("the plan returned %v rows to a reader who may see nothing, so something above it is doing the filtering:\n%s", got, plan)
	}
}

// TestPermissionFilterIsInTheQuery is the same claim from outside, and it is the
// one that has to keep passing.
//
// It counts the rows the database handed back. A driver that asked Postgres for
// everything and then dropped the documents the reader may not see would have
// the same visible behaviour and a row count in the hundreds, so the count is
// the only thing that tells the two apart without reading the code.
func TestPermissionFilterIsInTheQuery(t *testing.T) {
	s := open(t)
	seed(t, s, 500)

	s.ResetCounters()
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
	if got := s.Counters().Rows; got != 0 {
		t.Fatalf("the database returned %d rows for a reader who may see nothing, so the permission filter ran in Go rather than in the query", got)
	}

	// The same walk for somebody who may read one document has to cost one row,
	// not five hundred.
	only := readable("only")
	only.Permissions.AllowGroups = []acl.Ref{{Source: "gdrive", Value: "sales@acme.com"}}
	if err := s.Put(t.Context(), only); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s.ResetCounters()
	if err := s.Retrieve(t.Context(), stranger(), store.Request{}, func(doc.Document) bool { return true }); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if got := s.Counters().Rows; got != 1 {
		t.Fatalf("the database returned %d rows where one document is readable, want 1", got)
	}
}

// TestTermsAreInTheQuery is the same argument for the full text index. A driver
// that matched terms in Go would still return the right document and would have
// read the whole corpus to do it.
func TestTermsAreInTheQuery(t *testing.T) {
	s := open(t)
	seed(t, s, 500)

	needle := readable("needle")
	needle.Title = "Sourdough starter"
	needle.Body = "feed it twice a day with rye flour"
	if err := s.Put(t.Context(), needle); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s.ResetCounters()
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
	if rows := s.Counters().Rows; rows != 1 {
		t.Fatalf("the database returned %d rows for a term one document carries, so the term filter is not in the query", rows)
	}
}

// TestAnalyzerAgreesWithTheIndex covers the seam between the Go analyzer and the
// tsvector. They are two representations of the same words, and the way they
// fail is quietly: a term the Go rule finds and the index does not is a document
// that Retrieve misses and Scan finds.
func TestAnalyzerAgreesWithTheIndex(t *testing.T) {
	s := open(t)

	docs := []doc.Document{
		text("ascii", "Deploy runbook", "roll back the release with kubectl"),
		text("punct", "queue.go", "package queue, implements failover"),
		text("digits", "Postmortem 2026-01-14", "the incident lasted 42 minutes"),
		text("accents", "Café menu", "crème brûlée and a piccolo"),
		text("cjk", "支払いのランブック", "決済キューのフェイルオーバー手順"),
		text("mixed", "SLO review Q3", "p95 latency 250ms across the fleet"),
		text("quotes", "O'Brien's plan", `he said "ship it" and \escaped\ it`),
		text("stops", "the and of", "a an it is the"),
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

// TestOverlongTermsStillMatch covers the one place the index cannot store what
// the analyzer produced. Postgres refuses a lexeme over two kilobytes and a
// wiki page with a base64 blob in it tokenizes to exactly that, so both sides
// hash instead. If they ever hashed differently the document would be
// unfindable by its own text.
func TestOverlongTermsStillMatch(t *testing.T) {
	s := open(t)

	long := strings.Repeat("kubernetesdeployment", 400)
	if len(long) <= maxLexeme {
		t.Fatalf("the test term is %d bytes, which is short enough to store as it is", len(long))
	}
	d := text("blob", "Attachment", long+" and some ordinary words")
	if err := s.Put(t.Context(), d); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var got []string
	if err := s.Retrieve(t.Context(), reader(), store.Request{Terms: []string{long}}, func(d doc.Document) bool {
		got = append(got, d.ID)
		return true
	}); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != 1 || got[0] != "blob" {
		t.Fatalf("searching for a term too long to store returned %v", got)
	}
}

// TestPutIsIdempotent writes the same document repeatedly. The index, the
// postings and the corpus counters are all separate rows, so a second write is
// where a stale one shows up.
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

	corpus, err := s.Statistics(t.Context(), reader(), []string{"payments"})
	if err != nil {
		t.Fatalf("Statistics: %v", err)
	}
	if corpus.Documents != 1 {
		t.Fatalf("the corpus counts %d documents after four writes of one", corpus.Documents)
	}
	if corpus.DocFreq["payments"] != 1 {
		t.Fatalf("the term is recorded against %d documents, want 1", corpus.DocFreq["payments"])
	}
}

// TestStatisticsFollowTheCorpus is the bookkeeping test.
//
// The corpus and term counts are maintained rather than derived, which is what
// makes them a key lookup instead of an aggregate. The cost is that every path
// that changes a document has to keep them straight, and the only way that stays
// true is a test that walks all of them: write, rewrite with different text,
// quarantine, and delete.
func TestStatisticsFollowTheCorpus(t *testing.T) {
	s := open(t)
	p := reader()

	stat := func(want int, terms ...string) store.Corpus {
		t.Helper()
		c, err := s.Statistics(t.Context(), p, terms)
		if err != nil {
			t.Fatalf("Statistics: %v", err)
		}
		if c.Documents != want {
			t.Fatalf("the corpus counts %d documents, want %d", c.Documents, want)
		}
		return c
	}

	stat(0)

	a := text("a", "Payments runbook", "how to fail over the payments queue")
	b := text("b", "Billing runbook", "how to reconcile the billing ledger")
	if err := s.Put(t.Context(), a, b); err != nil {
		t.Fatalf("Put: %v", err)
	}
	c := stat(2, "payments", "runbook", "billing")
	if c.DocFreq["runbook"] != 2 || c.DocFreq["payments"] != 1 || c.DocFreq["billing"] != 1 {
		t.Fatalf("after two documents the term counts are %v", c.DocFreq)
	}
	if c.TitleTokens != 4 {
		t.Fatalf("the corpus counts %d title tokens across two two word titles", c.TitleTokens)
	}

	// A rewrite that drops a term has to take the term with it.
	a.Title, a.Body = "Payments runbook", "how to fail over the payments queue and the billing one"
	if err := s.Put(t.Context(), a); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if c := stat(2, "billing", "runbook"); c.DocFreq["billing"] != 2 || c.DocFreq["runbook"] != 2 {
		t.Fatalf("after a rewrite the term counts are %v", c.DocFreq)
	}

	// A permission that failed to resolve takes a document out of the index, so
	// it has to come out of the numbers the scorer divides by too.
	b.Permissions.Mode = acl.ModeUnknown
	if err := s.Put(t.Context(), b); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if c := stat(1, "billing", "runbook"); c.DocFreq["runbook"] != 1 || c.DocFreq["billing"] != 1 {
		t.Fatalf("after a quarantine the term counts are %v", c.DocFreq)
	}

	// And back again, because the next crawl can resolve what this one could not.
	b.Permissions = perm()
	if err := s.Put(t.Context(), b); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if c := stat(2, "runbook"); c.DocFreq["runbook"] != 2 {
		t.Fatalf("after the quarantine was lifted the term counts are %v", c.DocFreq)
	}

	if err := s.Delete(t.Context(), "a", "b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	c = stat(0, "payments", "runbook", "billing")
	if len(c.DocFreq) != 0 {
		t.Fatalf("after deleting everything the term counts are %v", c.DocFreq)
	}
	if c.TitleTokens != 0 || c.BodyTokens != 0 {
		t.Fatalf("after deleting everything the corpus holds %d title and %d body tokens", c.TitleTokens, c.BodyTokens)
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

// TestGeneratedColumnReadsTheTimestamp checks the one thing in the schema that
// is not a plain column. The modified date is stored as unix nanoseconds so that
// comparisons are integer comparisons, and PostgreSQL 18's virtual generated
// columns give an operator a readable timestamp over the same bytes without
// storing a second copy.
func TestGeneratedColumnReadsTheTimestamp(t *testing.T) {
	dsn := schemaDSN(t)
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Put(t.Context(), readable("d1")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var got time.Time
	if err := s.pool.QueryRow(t.Context(),
		`SELECT modified_utc FROM document WHERE id = 'd1'`).Scan(&got); err != nil {
		t.Fatalf("reading the generated column: %v", err)
	}
	want := readable("d1").ModifiedAt
	if !got.Equal(want) {
		t.Fatalf("the generated column reads %v, want %v", got, want)
	}
}

// TestClosedStore checks that a closed store says so rather than panicking on a
// pool that is not there any more.
func TestClosedStore(t *testing.T) {
	s, err := Open(t.Context(), schemaDSN(t))
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

func seed(t *testing.T, s *Store, n int) {
	t.Helper()
	docs := make([]doc.Document, 0, n)
	for i := range n {
		docs = append(docs, readable(id(i)))
	}
	if err := s.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

func id(i int) string { return "d" + string(rune('a'+i%26)) + "-" + strconv.Itoa(i) }

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
