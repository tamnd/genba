package api_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/cache"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
	"github.com/tamnd/genba/store/sqlitestore"
)

// cachingServer builds the server over the SQLite driver rather than the
// reference one, because the layers under test are the ones a driver that ranks
// for itself and reports its own writes turns on, and the reference driver does
// neither.
func cachingServer(t *testing.T, opts ...api.Option) (store.Store, http.Handler) {
	t.Helper()
	st, err := sqlitestore.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Put(t.Context(), cacheCorpus()...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	searcher := index.New(st, index.WithCache(index.NewCache()))
	t.Cleanup(func() { _ = searcher.Close() })
	return st, api.New(st, searcher, api.HeaderAuth{Tenant: "acme"}, opts...).Handler()
}

func cacheCorpus() []doc.Document {
	perm := func(group string) acl.Permissions {
		return acl.Permissions{
			Mode:        acl.ModeACL,
			Source:      "gdrive",
			AllowGroups: []acl.Ref{{Source: "gdrive", Value: group}},
			Version:     1,
		}
	}
	// Both carry a modification time, because the recency prior is part of the
	// score and a corpus of documents with no date has a prior of exactly one
	// for every document, which is the one case where the score cannot drift.
	written := time.Date(2026, 1, 14, 11, 0, 0, 0, time.UTC)
	return []doc.Document{
		{
			ID: "d1", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments failover runbook", Body: "Fail the payments queue over to the replica.",
			ModifiedAt: written, Permissions: perm("eng@acme.com"),
		},
		{
			ID: "d2", Tenant: "acme", Source: "salesforce", Kind: doc.KindTicket,
			Title: "Renewal for Globex", Body: "The payments discount expires in March.",
			ModifiedAt: written.AddDate(0, -3, 0), Permissions: perm("sales@acme.com"),
		},
	}
}

func seller() map[string]string {
	return map[string]string{
		api.HeaderSubject: "u_sam",
		api.HeaderGroups:  "gdrive:sales@acme.com",
	}
}

// TestAuthenticatedResponsesCarryTheirCachingRules covers the headers that let
// a browser hold a copy without a proxy in the middle holding one too.
func TestAuthenticatedResponsesCarryTheirCachingRules(t *testing.T) {
	_, h := cachingServer(t)
	// Stats is the one endpoint with no tag, for the reason given on its
	// handler, so it is listed here for its headers and excused the tag.
	for path, tagged := range map[string]bool{
		"/api/v1/me":                true,
		"/api/v1/search?q=payments": true,
		"/api/v1/suggest?q=pay":     true,
		"/api/v1/documents/d1":      true,
		"/api/v1/stats":             false,
	} {
		t.Run(path, func(t *testing.T) {
			w := request(t, h, http.MethodGet, path, engineer())
			if w.Code != http.StatusOK {
				t.Fatalf("got %d, want 200", w.Code)
			}
			if got := w.Header().Get("Cache-Control"); got != "private, max-age=0, must-revalidate" {
				t.Errorf("Cache-Control is %q, want a private response that must be revalidated", got)
			}
			if got := w.Header().Get("Vary"); !strings.Contains(got, "Authorization") || !strings.Contains(got, "Cookie") {
				t.Errorf("Vary is %q, want the credential headers: without them a cache may serve one caller's answer to another", got)
			}
			if tagged && w.Header().Get("ETag") == "" {
				t.Error("the response has no ETag, so a client can only revalidate by refetching the whole body")
			}
		})
	}
}

// TestStatsCarryNoTagBecauseReadingThemMovesThem is the exception to the rule
// above, written down so that adding the tag back looks like the regression it
// would be. The body reports cache hits and misses, and serving it is itself a
// read of those caches, so the tag computed for one response describes a state
// the next response is no longer in. The client holds stats for a TTL instead,
// which bounds the request rate without ever showing a number that is not real.
func TestStatsCarryNoTagBecauseReadingThemMovesThem(t *testing.T) {
	_, h := cachingServer(t)

	// Warm the layers so the counters have somewhere to move to.
	request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())

	first := request(t, h, http.MethodGet, "/api/v1/stats", engineer())
	if got := first.Header().Get("ETag"); got != "" {
		t.Fatalf("stats carry ETag %q, which no later request can match", got)
	}

	request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	second := request(t, h, http.MethodGet, "/api/v1/stats", engineer())
	if second.Body.String() == first.Body.String() {
		t.Fatal("two stats responses either side of a search are identical, so the counters are not being reported")
	}
}

// TestAdministrationCarriesNoTagForTheSameReasonStatsDoNot covers the second
// and last endpoint excused the tag. Half of what it reports is what the
// connectors are doing right now, and a sync that starts between two requests
// changes the body without changing anything a tag could be computed over
// cheaply. Tagging it would either produce a tag that never matches, which is a
// revalidation that can never succeed, or a tag computed over the parts that
// hold still, which would pin an operator to the first answer they ever saw on
// the one screen they open to find out whether anything is wrong.
//
// The headers that keep the answer out of a shared cache still apply, because
// this body names documents that are being held back and who is allowed to see
// them is exactly what a proxy in the middle does not know.
func TestAdministrationCarriesNoTagForTheSameReasonStatsDoNot(t *testing.T) {
	_, h := cachingServer(t)

	w := request(t, h, http.MethodGet, "/api/v1/admin/operations", operator())
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("ETag"); got != "" {
		t.Errorf("administration carries ETag %q, which no later request can match", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "private, max-age=0, must-revalidate" {
		t.Errorf("Cache-Control is %q, want a private response that must be revalidated", got)
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Authorization") || !strings.Contains(got, "Cookie") {
		t.Errorf("Vary is %q, want the credential headers: without them a cache may serve this to somebody without the role", got)
	}
}

func TestARepeatedRequestIsAnsweredWithoutABody(t *testing.T) {
	_, h := cachingServer(t)

	first := request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("the first response has no ETag")
	}

	headers := engineer()
	headers["If-None-Match"] = tag
	second := request(t, h, http.MethodGet, "/api/v1/search?q=payments", headers)
	if second.Code != http.StatusNotModified {
		t.Fatalf("got %d, want 304: the answer has not changed", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Errorf("a 304 carried %d bytes of body", second.Body.Len())
	}
	if got := second.Header().Get("ETag"); got != tag {
		t.Errorf("the 304 reports ETag %q, want %q", got, tag)
	}

	// A wildcard is a client saying it has some copy and would like to know
	// whether anything is current.
	headers["If-None-Match"] = "*"
	if w := request(t, h, http.MethodGet, "/api/v1/search?q=payments", headers); w.Code != http.StatusNotModified {
		t.Errorf("a wildcard conditional request got %d, want 304", w.Code)
	}
}

// TestHowLongASearchTookIsNotPartOfTheTag is what makes the whole thing work.
// The response reports its own latency, and hashing that would produce a tag
// that never matches, which is a revalidation that can never succeed.
func TestHowLongASearchTookIsNotPartOfTheTag(t *testing.T) {
	_, h := cachingServer(t)

	first := request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	second := request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	if first.Header().Get("ETag") != second.Header().Get("ETag") {
		t.Fatalf("two identical searches produced two tags, %q and %q", first.Header().Get("ETag"), second.Header().Get("ETag"))
	}
}

// TestARankThatDriftsDoesNotMoveTheTag is the same problem as the one above
// and much easier to miss. The score carries a recency prior that decays
// against the wall clock, so a query run twice a second apart produces two
// numbers that differ somewhere past the tenth decimal place and a tag that
// never matches. The clock here jumps an hour between readings to make it
// obvious, but on a real deployment a microsecond is enough.
func TestARankThatDriftsDoesNotMoveTheTag(t *testing.T) {
	st, err := sqlitestore.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Put(t.Context(), cacheCorpus()...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		at = at.Add(time.Hour)
		return at
	}
	searcher := index.New(st, index.WithCache(index.NewCache()), index.WithClock(clock))
	t.Cleanup(func() { _ = searcher.Close() })
	h := api.New(st, searcher, api.HeaderAuth{Tenant: "acme"}).Handler()

	first := request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	second := request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	if first.Header().Get("ETag") != second.Header().Get("ETag") {
		t.Fatalf("the same documents in the same order produced two tags, %q and %q", first.Header().Get("ETag"), second.Header().Get("ETag"))
	}

	headers := engineer()
	headers["If-None-Match"] = first.Header().Get("ETag")
	if w := request(t, h, http.MethodGet, "/api/v1/search?q=payments", headers); w.Code != http.StatusNotModified {
		t.Fatalf("a revalidation of an unchanged search returned %d, want 304", w.Code)
	}
}

// TestTheViewNamesWhoIsAsking is the same leak assertion one layer out. The
// browser holds answers in memory and one tab holds them for more than one
// identity over its life, so every key it makes starts with this value. Two
// people who see different documents must not be able to produce the same key.
func TestTheViewNamesWhoIsAsking(t *testing.T) {
	_, h := cachingServer(t)

	view := func(headers map[string]string) string {
		t.Helper()
		return decode[struct {
			View string `json:"view"`
		}](t, request(t, h, http.MethodGet, "/api/v1/me", headers)).View
	}

	mine := view(engineer())
	if mine == "" {
		t.Fatal("the me endpoint named no view, so the interface has nothing to key its cache by")
	}
	if again := view(engineer()); again != mine {
		t.Errorf("the same caller was given two views, %q then %q, so nothing would ever be reused", mine, again)
	}
	if theirs := view(seller()); theirs == mine {
		t.Error("two callers who see different documents were given the same view")
	}
	if wider := view(engineerWithBothGroups()); wider == mine {
		t.Error("adding a group did not change the view, so entries from before the change stay reachable")
	}
}

// TestTwoCallersNeverShareATag is the leak assertion at the HTTP layer. The two
// people ask for the same URL and get different answers, so they must not be
// able to satisfy each other's conditional requests.
func TestTwoCallersNeverShareATag(t *testing.T) {
	_, h := cachingServer(t)

	mine := request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	theirs := request(t, h, http.MethodGet, "/api/v1/search?q=payments", seller())
	if mine.Header().Get("ETag") == theirs.Header().Get("ETag") {
		t.Fatal("two callers who see different documents share an ETag")
	}

	headers := seller()
	headers["If-None-Match"] = mine.Header().Get("ETag")
	w := request(t, h, http.MethodGet, "/api/v1/search?q=payments", headers)
	if w.Code == http.StatusNotModified {
		t.Fatal("one caller's tag satisfied another caller's conditional request")
	}
	if strings.Contains(w.Body.String(), "failover runbook") {
		t.Fatalf("the seller was served the engineer's document: %s", w.Body.String())
	}
}

func TestTheTagChangesWhenTheAnswerDoes(t *testing.T) {
	st, h := cachingServer(t)

	before := request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer()).Header().Get("ETag")
	if err := st.Put(t.Context(), doc.Document{
		ID: "d9", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
		Title: "Payments postmortem", Body: "The payments queue backed up for an hour.",
		Permissions: acl.Permissions{
			Mode:        acl.ModeACL,
			Source:      "gdrive",
			AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
			Version:     1,
		},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	after := request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	if after.Header().Get("ETag") == before {
		t.Fatal("a new matching document did not change the tag, so a client would never fetch it")
	}
	if !strings.Contains(after.Body.String(), "postmortem") {
		t.Errorf("the new document is missing from the results: %s", after.Body.String())
	}
}

func TestStatsReportEachCacheLayer(t *testing.T) {
	_, h := cachingServer(t)
	request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())
	request(t, h, http.MethodGet, "/api/v1/search?q=payments", engineer())

	body := decode[struct {
		Documents int                    `json:"documents"`
		Cache     map[string]cache.Stats `json:"cache"`
	}](t, request(t, h, http.MethodGet, "/api/v1/stats", engineer()))

	if body.Documents != 2 {
		t.Errorf("stats report %d documents, want 2", body.Documents)
	}
	for _, layer := range []string{"results", "corpus", "documents"} {
		got, ok := body.Cache[layer]
		if !ok {
			t.Fatalf("stats do not report the %s cache layer", layer)
		}
		if got.Hits+got.Misses == 0 {
			t.Errorf("the %s layer reports no lookups after two searches", layer)
		}
	}
}

// The event stream is checked over a real connection, because the thing under
// test is what arrives and when, and a recorder that collects the whole
// response before anybody looks at it cannot show that.

func TestEventStreamReportsIndexChanges(t *testing.T) {
	st, h := cachingServer(t, api.WithHeartbeat(50*time.Millisecond))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	lines, cancel := stream(t, srv.URL+"/api/v1/events", engineer())
	defer cancel()

	if got := next(t, lines); got != ": open" {
		t.Fatalf("the stream opened with %q, want a comment so the browser fires its open event", got)
	}

	// The write happens after the stream is open, so the subscription is
	// registered and the test is not racing the handler.
	if err := st.Put(t.Context(), doc.Document{
		ID: "d9", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
		Title: "Payments postmortem", Body: "The payments queue backed up.",
		Permissions: acl.Permissions{Mode: acl.ModeACL, Source: "gdrive", AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}}, Version: 1},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	event, data := indexFrame(t, lines)
	if event != "event: index" {
		t.Fatalf("the frame names event %q, want index", event)
	}
	if !strings.Contains(data, `"tenant":"acme"`) || !strings.Contains(data, `"documents":1`) {
		t.Errorf("the event says %q, want the tenant and how many documents moved", data)
	}
	if strings.Contains(data, "d9") || strings.Contains(data, "postmortem") {
		t.Errorf("the event carries the document itself: %q", data)
	}
}

func TestEventStreamIsScopedToTheCallersTenant(t *testing.T) {
	st, h := cachingServer(t, api.WithHeartbeat(time.Hour))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	lines, cancel := stream(t, srv.URL+"/api/v1/events", engineer())
	defer cancel()
	next(t, lines)

	// A write in somebody else's tenant, then one in the caller's. If the first
	// were reported the frame below would carry two documents rather than one,
	// and the tenant of a company this caller has never heard of.
	elsewhere := doc.Document{
		ID: "x1", Tenant: "globex", Source: "gdrive", Kind: doc.KindPage,
		Title: "Their runbook", Body: "Not for us.",
		Permissions: acl.Permissions{Mode: acl.ModeACL, Source: "gdrive", AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}}, Version: 1},
	}
	mine := doc.Document{
		ID: "d9", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
		Title: "Our runbook", Body: "For us.",
		Permissions: acl.Permissions{Mode: acl.ModeACL, Source: "gdrive", AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}}, Version: 1},
	}
	if err := st.Put(t.Context(), elsewhere, mine); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, data := indexFrame(t, lines)
	if !strings.Contains(data, `"tenant":"acme"`) {
		t.Fatalf("the caller was told about another tenant: %q", data)
	}
	if !strings.Contains(data, `"documents":1`) {
		t.Errorf("the event says %q, want the one document of this tenant", data)
	}
}

func TestEventStreamKeepsItselfAlive(t *testing.T) {
	_, h := cachingServer(t, api.WithHeartbeat(20*time.Millisecond))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	lines, cancel := stream(t, srv.URL+"/api/v1/events", engineer())
	defer cancel()
	next(t, lines)

	if got := next(t, lines); got != ": beat" {
		t.Fatalf("an idle stream sent %q, want a heartbeat comment: the proxies in front of a deployment close a silent connection", got)
	}
}

func TestEventStreamNeedsACredential(t *testing.T) {
	_, h := cachingServer(t)
	if w := request(t, h, http.MethodGet, "/api/v1/events", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated stream request got %d, want 401", w.Code)
	}
}

// TestEventStreamIsAbsentWithoutADriverThatReportsWrites covers the deployment
// on a driver that cannot say when it changed. It is not a broken deployment,
// so it is a not found rather than an error, and the interface falls back to
// its timer.
func TestEventStreamIsAbsentWithoutADriverThatReportsWrites(t *testing.T) {
	st := deafStore{memstore.New()}
	t.Cleanup(func() { _ = st.Close() })
	if _, ok := any(st).(store.Notifier); ok {
		t.Fatal("deafStore reports its writes, so this test covers nothing")
	}
	searcher := index.New(st, index.WithCache(index.NewCache()))
	t.Cleanup(func() { _ = searcher.Close() })
	h := api.New(st, searcher, api.HeaderAuth{Tenant: "acme"}).Handler()

	if w := request(t, h, http.MethodGet, "/api/v1/events", engineer()); w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

// deafStore is a driver that does not report its writes.
type deafStore struct{ inner *memstore.Store }

func (d deafStore) Put(ctx context.Context, docs ...doc.Document) error {
	return d.inner.Put(ctx, docs...)
}
func (d deafStore) Delete(ctx context.Context, ids ...string) error {
	return d.inner.Delete(ctx, ids...)
}
func (d deafStore) Get(ctx context.Context, p *acl.Principal, id string) (doc.Document, error) {
	return d.inner.Get(ctx, p, id)
}
func (d deafStore) Scan(ctx context.Context, p *acl.Principal, fn func(doc.Document) bool) error {
	return d.inner.Scan(ctx, p, fn)
}
func (d deafStore) Stats(ctx context.Context) (store.Stats, error) { return d.inner.Stats(ctx) }
func (d deafStore) Close() error                                   { return d.inner.Close() }

// stream opens an event stream and reads it line by line on a goroutine, so
// that a test can wait for a line with a deadline instead of blocking forever
// on a stream that is working correctly by staying quiet.
func stream(t *testing.T, url string, headers map[string]string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		cancel()
		t.Fatalf("building the request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("opening the stream: %v", err)
	}
	// Every path out of here that is not the goroutine below has to close the
	// body itself, because the goroutine that would have closed it is never
	// started.
	fail := func(format string, args ...any) {
		cancel()
		_ = resp.Body.Close()
		t.Fatalf(format, args...)
	}
	if resp.StatusCode != http.StatusOK {
		fail("the stream returned %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		fail("the stream is %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("the stream says Cache-Control %q, want no-store", got)
	}

	lines := make(chan string, 32)
	go func() {
		defer close(lines)
		defer func() { _ = resp.Body.Close() }()
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if line = strings.TrimRight(line, "\r\n"); line != "" {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return lines, cancel
}

// next returns the next non blank line, or fails the test.
func next(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case line, ok := <-lines:
		if !ok {
			t.Fatal("the stream closed while the test was waiting for a line")
		}
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived on the stream")
		return ""
	}
}

// indexFrame reads until it finds an index event and returns its two lines,
// skipping the heartbeats that may be interleaved with it.
func indexFrame(t *testing.T, lines <-chan string) (event, data string) {
	t.Helper()
	for range 20 {
		line := next(t, lines)
		if !strings.HasPrefix(line, "event:") {
			continue
		}
		return line, next(t, lines)
	}
	t.Fatal("no index event arrived")
	return "", ""
}
