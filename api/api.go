// Package api is the HTTP surface of the platform.
//
// It is a thin layer on purpose. A handler parses the request, resolves the
// caller to a principal, calls one package below it and encodes the result. No
// handler makes a permission decision of its own, because the decision belongs
// to whichever component owns the data, and a handler that starts filtering is
// a handler that will eventually filter differently from the storage driver.
//
// The router is the standard library one. Method and path patterns have been
// part of net/http since Go 1.22, and the third party routers we would reach
// for buy very little on top of it while making the handler signature ours
// instead of the standard one.
package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/cache"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/thumb"
)

// Authenticator resolves an HTTP request to the principal making it.
//
// It returns an error when the request carries no usable credential. There is
// no third outcome: a request either has a principal or it is rejected, and no
// part of the platform runs with an implicit one.
type Authenticator interface {
	Authenticate(r *http.Request) (*acl.Principal, error)
}

// AuthenticatorFunc adapts a function to [Authenticator].
type AuthenticatorFunc func(r *http.Request) (*acl.Principal, error)

// Authenticate calls f.
func (f AuthenticatorFunc) Authenticate(r *http.Request) (*acl.Principal, error) { return f(r) }

// ErrUnauthenticated is what an [Authenticator] returns when the request
// carries no credential it recognises.
var ErrUnauthenticated = errors.New("api: unauthenticated")

// Server holds the dependencies of the HTTP layer.
type Server struct {
	store    store.Store
	searcher *index.Searcher
	auth     Authenticator
	log      *slog.Logger

	// assets is served under / when it is not nil, which is how the single
	// binary ships the web interface.
	assets http.Handler

	// started is when the process came up, reported by the health endpoint.
	started time.Time
	now     func() time.Time

	// driver names the storage this process was started with. It is passed in
	// rather than derived from the store, because the name an operator knows is
	// the one they typed on the command line and a Go type name is not it.
	driver string

	// indexed is when the store last reported a committed write, as Unix
	// nanoseconds, and zero until one happens.
	//
	// It is not persisted and it is not asked of the driver. A process that has
	// just started has not seen a sync, and saying so is more honest than
	// reporting one it was not there for.
	indexed atomic.Int64

	// indexing is asked, on every health check and every stats request, whether
	// a source is still being read for the first time. It is nil for an embedder
	// that runs no connectors, and a nil one is the same answer as no.
	indexing func() (Indexing, bool)

	// operations is asked, on every administration request, what the connectors
	// are doing. It is nil for an embedder that runs none, and a nil one is the
	// same answer as no connectors.
	operations func() Operations

	// supervisor is what can change the connectors. It is nil for a deployment
	// that configures them on the command line, and a nil one is what makes the
	// administration screen say so rather than draw a form nothing is behind.
	supervisor Supervisor

	// heartbeat is how often an idle event stream sends a comment.
	heartbeat time.Duration

	// metrics is what the process publishes about itself. It is always
	// recorded, because a histogram nobody scrapes costs a lock and nine
	// comparisons, and it is only served where a caller mounts [Server.Metrics].
	metrics *metrics

	// thumbs holds the rendered thumbnails. It lives on the server rather than
	// in the store because it is derived data with a cost that is measured in
	// milliseconds of decoding rather than in whether the bytes exist, and
	// because a driver that holds no content still has one of these and never
	// puts anything in it.
	thumbs *cache.Cache[thumb.Thumbnail]
}

// Option configures a [Server].
type Option func(*Server)

// WithLogger sets the logger. The default discards nothing and writes to the
// slog default, which is what a library embedder usually wants to replace.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.log = l
		}
	}
}

// WithAssets serves a web interface under /. Without it the server is an API
// only deployment, which is a supported way to run.
func WithAssets(h http.Handler) Option {
	return func(s *Server) { s.assets = h }
}

// WithClock sets the clock used for uptime reporting and for timestamping
// index events.
func WithClock(now func() time.Time) Option {
	return func(s *Server) {
		if now != nil {
			s.now, s.started = now, now()
		}
	}
}

// WithDriver names the storage driver this process was started with, for the
// stats endpoint to report. Without it the server says nothing about its
// storage, which is what an embedder that never chose a driver name should say.
func WithDriver(name string) Option {
	return func(s *Server) { s.driver = name }
}

// Indexing is one source being read for the first time.
//
// Done and Total count the same thing, which is documents this source will put
// in the index, and Total is an estimate: it is what the source held when the
// run counted it, and a source people are working in moves while it is read.
// That is why the interface says "about" and why nothing here should be used
// for anything but telling somebody the answer they are looking at is not the
// whole one yet.
type Indexing struct {
	Source string `json:"source"`
	Done   int    `json:"done"`
	Total  int    `json:"total"`
}

// WithIndexing supplies the answer to whether a source is still being read.
//
// The caller owns the tracking, because the thing that knows is whatever is
// running the connectors and this package deliberately does not. An embedder
// that indexes on its own schedule passes its own function and gets the same
// banner for free.
func WithIndexing(fn func() (Indexing, bool)) Option {
	return func(s *Server) { s.indexing = fn }
}

// WithHeartbeat sets how often an idle event stream sends a comment. It exists
// so that a test does not have to wait [HeartbeatInterval] to see one.
func WithHeartbeat(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.heartbeat = d
		}
	}
}

// New returns a server. It does not listen: [Server.Handler] returns something
// the caller mounts wherever it likes, which is what makes the platform
// embeddable in an existing Go service.
func New(st store.Store, searcher *index.Searcher, auth Authenticator, opts ...Option) *Server {
	s := &Server{
		store:     st,
		searcher:  searcher,
		auth:      auth,
		log:       slog.Default(),
		started:   time.Now(),
		now:       time.Now,
		heartbeat: HeartbeatInterval,
		thumbs:    newThumbnailCache(),
	}
	for _, opt := range opts {
		opt(s)
	}
	// After the options, because the cache layers and the storage driver are
	// what the counters read and both arrive with the server rather than with
	// the registry.
	s.metrics = newMetrics(s)
	// A driver that reports its writes is how this process knows a sync
	// happened. The unsubscribe is dropped on purpose: a server is built once
	// and lives as long as the process does, so there is no point in the life
	// of one where this should stop listening.
	if n, ok := st.(store.Notifier); ok {
		n.OnChange(func(store.Change) { s.indexed.Store(s.now().UnixNano()) })
	}
	return s
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.Handle("GET /api/v1/me", s.authenticated(s.handleMe))
	mux.Handle("GET /api/v1/search", s.authenticated(s.handleSearch))
	mux.Handle("GET /api/v1/suggest", s.authenticated(s.handleSuggest))
	mux.Handle("GET /api/v1/documents/{id}", s.authenticated(s.handleDocument))
	mux.Handle("POST /api/v1/documents/{id}/verify", s.authenticated(s.handleVerify))
	mux.Handle("DELETE /api/v1/documents/{id}/verify", s.authenticated(s.handleUnverify))
	mux.Handle("PUT /api/v1/documents/{id}/owner", s.authenticated(s.handleSetOwner))
	mux.Handle("DELETE /api/v1/documents/{id}/owner", s.authenticated(s.handleClearOwner))
	mux.Handle("POST /api/v1/documents/{id}/stale", s.authenticated(s.handleReport))
	mux.Handle("DELETE /api/v1/documents/{id}/stale", s.authenticated(s.handleResolve))
	mux.Handle("GET /api/v1/documents/{id}/content", s.authenticated(s.handleContent))
	mux.Handle("GET /api/v1/documents/{id}/thumbnail", s.authenticated(s.handleThumbnail))
	mux.Handle("GET /api/v1/reported", s.authenticated(s.handleReported))
	mux.Handle("GET /api/v1/recent", s.authenticated(s.handleRecent))
	mux.Handle("POST /api/v1/recent", s.authenticated(s.handleRecordOpen))
	mux.Handle("GET /api/v1/stats", s.authenticated(s.handleStats))
	mux.Handle("GET /api/v1/events", s.authenticated(s.handleEvents))
	mux.Handle("GET /api/v1/admin/operations", s.admin(s.handleAdmin))
	mux.Handle("GET /api/v1/admin/access", s.admin(s.handleAccess))
	mux.Handle("POST /api/v1/admin/connectors", s.admin(s.handleAddConnector))
	mux.Handle("DELETE /api/v1/admin/connectors/{source}", s.admin(s.handleDropConnector))
	mux.Handle("POST /api/v1/admin/connectors/{source}/start", s.admin(s.handleStartConnector))
	mux.Handle("POST /api/v1/admin/connectors/{source}/stop", s.admin(s.handleStopConnector))
	mux.Handle("POST /api/v1/admin/connectors/{source}/sync", s.admin(s.handleSyncConnector))
	if s.assets != nil {
		mux.Handle("GET /", s.assets)
	}
	return s.recoverPanic(s.measure(mux))
}

// authenticated wraps a handler that needs a principal.
func (s *Server) authenticated(h func(http.ResponseWriter, *http.Request, *acl.Principal)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err != nil || p == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "this request needs a credential")
			return
		}
		h(w, r, p)
	})
}

// recoverPanic keeps one bad request from taking the process down, and logs
// enough to find it. It deliberately does not put the panic in the response.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("handler panicked", "method", r.Method, "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal", "the request could not be served")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Built   string `json:"built"`
	Uptime  string `json:"uptime"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Version: genba.Version,
		Commit:  genba.Commit,
		Built:   genba.Date,
		Uptime:  time.Since(s.started).Round(time.Second).String(),
	})
}

type readyResponse struct {
	Status string `json:"status"`

	// Indexing says a source is still being read for the first time, so the
	// answers this process gives are true and incomplete.
	//
	// It is a bare boolean rather than the counts the stats endpoint reports,
	// because this endpoint is unauthenticated and how large somebody's corpus
	// is and what their sources are called is not for anybody who can reach the
	// port. A probe needs to know whether to wait. It does not need to know what
	// it is waiting for.
	Indexing bool `json:"indexing,omitempty"`
}

// handleReady reports whether the process can serve queries. It touches the
// store, because a process that is up but cannot reach its storage is not ready
// and a load balancer needs to know the difference.
//
// A process that is still reading a source for the first time is ready. It
// answers, the answers are correct as far as they go, and holding the whole
// deployment out of rotation until a crawl finishes is how a rolling restart
// turns into an outage. The flag is there so that a caller who does care, which
// in practice is a benchmark rather than a load balancer, can wait.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.Stats(r.Context()); err != nil {
		s.log.Warn("readiness check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "storage is not available")
		return
	}
	_, indexing := s.stillIndexing()
	writeJSON(w, http.StatusOK, readyResponse{Status: "ready", Indexing: indexing})
}

// stillIndexing asks whoever is running the connectors whether one of them is
// still reading a source for the first time.
func (s *Server) stillIndexing() (Indexing, bool) {
	if s.indexing == nil {
		return Indexing{}, false
	}
	return s.indexing()
}

type searchResponse struct {
	Query string `json:"query"`

	// Text is what was left of the query after the operators were parsed out of
	// it, so the interface can show which part of what somebody typed became a
	// filter rather than a search term.
	Text    string        `json:"text"`
	Filters searchFilters `json:"filters"`
	Total   int           `json:"total"`
	Partial bool          `json:"partial,omitempty"`
	TookMS  float64       `json:"took_ms"`
	Hits    []searchHit   `json:"hits"`

	Facets map[string][]facet `json:"facets"`

	// FacetsPartial says each facet count is a lower bound, because the counting
	// stopped at [index.FacetPool] documents rather than reading every matching
	// one. It is the same claim partial makes about the total, about the other
	// number on the screen, and the interface writes both the same way.
	FacetsPartial bool `json:"facets_partial,omitempty"`

	// Answer is the passages worth reading above the list, quoted out of the
	// documents on this page. Absent whenever there are none, and the interface
	// draws nothing at all rather than an empty region, because a heading with
	// no content under it is the most common way an answer surface makes a page
	// worse than it was.
	Answer *answer `json:"answer,omitempty"`

	// Correction is a spelling of the query that would have found something. It
	// is only ever present on a search that found nothing, and it has already
	// been run as the person asking, so it is a query they can run rather than
	// a word somebody else's documents contain.
	Correction string `json:"correction,omitempty"`
}

// searchFilters echoes the parsed query back. A shareable URL and a typed
// operator have to end up in the same place, and the way to be sure of that is
// for the server to say what it understood instead of the client guessing.
type searchFilters struct {
	Sources    []string `json:"sources,omitempty"`
	Kinds      []string `json:"kinds,omitempty"`
	Containers []string `json:"containers,omitempty"`
	Authors    []string `json:"authors,omitempty"`
	Owners     []string `json:"owners,omitempty"`
	Since      string   `json:"since,omitempty"`
	Until      string   `json:"until,omitempty"`
	Sort       string   `json:"sort,omitempty"`
}

// facet is the wire shape of index.Facet. The extra type is worth it: it keeps
// the ranking package free of json tags, so a field can be renamed there
// without changing what a client parses.
type facet struct {
	Value string `json:"value"`
	Count int    `json:"count"`

	// Selected says the query already narrows to this value, so the interface
	// draws it ticked.
	//
	// The server says it rather than the client working it out, because working
	// it out means comparing a filter somebody typed against a display string a
	// connector produced, and the comparison that decided whether the document
	// matched has already been made once, down in the store. A client that
	// compared them a second time and got a different answer would draw a box
	// unticked next to a count that only makes sense because it is ticked.
	Selected bool `json:"selected,omitempty"`
}

type searchHit struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	URL        string    `json:"url,omitempty"`
	Source     string    `json:"source"`
	Kind       string    `json:"kind"`
	Container  string    `json:"container,omitempty"`
	Author     string    `json:"author,omitempty"`
	MediaType  string    `json:"media_type,omitempty"`
	ModifiedAt time.Time `json:"modified_at,omitzero"`
	Snippet    string    `json:"snippet,omitempty"`

	// Passages is the snippet split at the words that matched, so the interface
	// marks exactly what the index matched rather than running a substring
	// search of its own and highlighting the wrong halves of words.
	Passages []passage `json:"passages,omitempty"`
	Score    float64   `json:"score"`

	// Verified is who vouched for this document and until when, and is absent
	// when nobody has. It is on the row rather than only in the preview because
	// the whole value of the signal is that it is visible while somebody is
	// choosing which of ten results to open.
	Verified *verification `json:"verified,omitempty"`
}

type passage struct {
	Text  string `json:"text"`
	Match bool   `json:"match,omitempty"`
}

// answer is the wire shape of index.Answer.
type answer struct {
	Quotes []quote `json:"quotes"`
}

// quote is one passage and the document it came from.
//
// The document is an id and nothing else. Every quote is taken from a document
// that is already in the hits of the same response, so the title, the source and
// the date are on the wire once rather than twice, and there is no way for the
// citation under a quote to disagree with the result it points at.
type quote struct {
	ID       string    `json:"id"`
	Text     string    `json:"text"`
	Passages []passage `json:"passages,omitempty"`
}

func answerOf(a index.Answer) *answer {
	if len(a.Quotes) == 0 {
		return nil
	}
	out := answer{Quotes: make([]quote, 0, len(a.Quotes))}
	for _, q := range a.Quotes {
		out.Quotes = append(out.Quotes, quote{ID: q.ID, Text: q.Text, Passages: passages(q.Passages)})
	}
	return &out
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	query, err := parseQuery(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	res, err := s.searcher.Search(r.Context(), p, query)
	if err != nil {
		s.log.Error("search failed", "error", err, "tenant", p.Tenant)
		writeError(w, http.StatusInternalServerError, "internal", "the search could not be run")
		return
	}

	s.metrics.observeSearch(res.Took, res.Candidates, res.Total)

	out := searchResponse{
		Query:         r.URL.Query().Get("q"),
		Text:          query.Text,
		Filters:       filtersOf(query),
		Total:         res.Total,
		Partial:       res.Truncated,
		TookMS:        float64(res.Took.Microseconds()) / 1000,
		Facets:        facets(res.Facets, query.Request()),
		FacetsPartial: res.Approximate,

		Hits:       []searchHit{},
		Answer:     answerOf(res.Answer),
		Correction: res.Correction,
	}
	ids := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		ids = append(ids, h.Document.ID)
	}
	// One question for the whole page rather than one per row, and the badges
	// are left off rather than the search failing when it cannot be answered.
	vouched, now := s.verifications(r, p, ids), s.now()
	for _, h := range res.Hits {
		hit := hitOf(h.Document)
		hit.Snippet, hit.Passages, hit.Score = h.Snippet, passages(h.Passages), h.Score
		hit.Verified = verifiedOf(vouched[h.Document.ID], now)
		out.Hits = append(out.Hits, hit)
	}
	writeConditional(w, r, http.StatusOK, out, out.identity())
}

// identity is the response without the numbers that change on every request.
//
// Two of them do. How long the search took is the obvious one. The other is the
// score, which carries a recency prior that decays against the wall clock, so
// it is a slightly different number every time the same query is run and it
// would make an entity tag that never matches, which is a revalidation that can
// never succeed. What the tag has to mean is that the same documents came back
// in the same order with the same content, and it still means exactly that:
// scores that moved enough to matter moved the order too.
func (r searchResponse) identity() any {
	r.TookMS = 0
	hits := make([]searchHit, len(r.Hits))
	copy(hits, r.Hits)
	for i := range hits {
		hits[i].Score = 0
	}
	r.Hits = hits
	return r
}

// parseQuery builds a query from the URL.
//
// The q parameter goes through the operator grammar, and the repeated
// parameters add to whatever that produced. Ticking a source in the sidebar and
// typing app:slack therefore land in the same field, which is what lets the
// interface keep the box and the sidebar in sync without a second grammar of
// its own.
func parseQuery(v url.Values) (index.Query, error) {
	limit, err := intParam(v.Get("limit"))
	if err != nil {
		return index.Query{}, errors.New("limit must be a number")
	}
	offset, err := intParam(v.Get("offset"))
	if err != nil {
		return index.Query{}, errors.New("offset must be a number")
	}

	q := index.Parse(v.Get("q"))
	q.Sources = append(q.Sources, listParam(v, "source")...)
	q.Kinds = append(q.Kinds, kinds(listParam(v, "kind"))...)
	q.Containers = append(q.Containers, listParam(v, "container")...)
	q.Authors = append(q.Authors, listParam(v, "author")...)
	q.Owners = append(q.Owners, listParam(v, "owner")...)
	q.Limit, q.Offset = limit, offset

	if since := v.Get("since"); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return index.Query{}, errors.New("since must be an RFC 3339 timestamp")
		}
		q.Since = t
	}
	if until := v.Get("until"); until != "" {
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return index.Query{}, errors.New("until must be an RFC 3339 timestamp")
		}
		q.Until = t
	}
	if sort := v.Get("sort"); sort == string(index.ByRecent) || sort == "recent" {
		q.Sort = index.ByRecent
	}
	return q, nil
}

func filtersOf(q index.Query) searchFilters {
	f := searchFilters{
		Sources:    q.Sources,
		Containers: q.Containers,
		Authors:    q.Authors,
		Owners:     q.Owners,
		Sort:       string(q.Sort),
	}
	for _, k := range q.Kinds {
		f.Kinds = append(f.Kinds, string(k))
	}
	if !q.Since.IsZero() {
		f.Since = q.Since.UTC().Format(time.RFC3339)
	}
	if !q.Until.IsZero() {
		f.Until = q.Until.UTC().Format(time.RFC3339)
	}
	return f
}

func passages(in []index.Passage) []passage {
	if len(in) == 0 {
		return nil
	}
	out := make([]passage, 0, len(in))
	for _, p := range in {
		out = append(out, passage{Text: p.Text, Match: p.Match})
	}
	return out
}

func personName(p doc.Person) string {
	switch {
	case p.Name != "":
		return p.Name
	case p.Email != "":
		return p.Email
	default:
		return p.Identity.Value
	}
}

type meResponse struct {
	Subject string   `json:"subject"`
	Tenant  string   `json:"tenant"`
	Roles   []string `json:"roles,omitempty"`

	// Groups is what the principal is a member of, as the authenticator
	// resolved it. It is on the wire because the first question somebody asks
	// when a document says they may not read it is which groups they are in,
	// and the answer they need is the server's rather than whatever the browser
	// last sent. Absent where the principal is in none.
	Groups []string `json:"groups,omitempty"`

	// View names what this principal can see, and is the same fingerprint the
	// server keys its own caches by.
	//
	// The interface holds results in memory and one tab can hold them for more
	// than one identity over its life, because the identity switcher exists.
	// Serving one identity's cached results to another is the same bug in a
	// browser as it is in a server, so the browser prepends this to every cache
	// key it makes. It is computed here rather than derived there so that the
	// two cannot drift apart, and it says nothing about the principal that the
	// rest of this response does not already say out loud.
	View string `json:"view"`

	// Sources is what this principal can actually see something from, which is
	// what the interface builds its source filter out of. It is per principal
	// rather than a list of configured connectors, because telling somebody a
	// connector exists that they have no documents in is a small leak of how
	// the company is organised.
	Sources []facet `json:"sources"`
	Kinds   []facet `json:"kinds"`
}

// handleMe is what the interface loads before it can draw anything: who the
// caller is and which filters are worth showing them.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	out := meResponse{
		Subject: p.Subject,
		Tenant:  p.Tenant,
		Roles:   p.Roles,
		Groups:  p.Groups.Members,
		View:    acl.Fingerprint(p),
		Sources: []facet{},
		Kinds:   []facet{},
	}
	// The counts and nothing else. This used to run a search with a limit of one,
	// which is not a cheap search: the candidate pool has a floor, so it asked
	// the driver for five hundred documents, scored all five hundred, sorted
	// them, took a page of one and fetched it, and then used none of that. It was
	// the most expensive endpoint in the product and it is the one that has to
	// answer before a single pixel can be drawn.
	facets, err := s.searcher.Filters(r.Context(), p)
	if err != nil {
		s.log.Error("bootstrap failed", "error", err, "tenant", p.Tenant)
		writeError(w, http.StatusInternalServerError, "internal", "the session could not be loaded")
		return
	}
	out.Sources = facetValues(facets["source"])
	out.Kinds = facetValues(facets["kind"])
	writeConditional(w, r, http.StatusOK, out, nil)
}

type suggestResponse struct {
	Query       string       `json:"query"`
	TookMS      float64      `json:"took_ms"`
	Suggestions []suggestion `json:"suggestions"`
}

// suggestion is one row of the typeahead. Kind says how the interface should
// draw it and what happens on Enter.
type suggestion struct {
	Kind  string `json:"kind"` // "document", "operator" or "query"
	Text  string `json:"text"`
	Hint  string `json:"hint,omitempty"`
	ID    string `json:"id,omitempty"`
	URL   string `json:"url,omitempty"`
	Value string `json:"value,omitempty"`
}

// SuggestLimit is how many rows the typeahead returns. It is small because the
// list is read at a glance while somebody is still typing, and because the
// budget for this endpoint is tens of milliseconds.
const SuggestLimit = 8

func (s *Server) handleSuggest(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	start := time.Now()
	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	out := suggestResponse{Query: raw, Suggestions: []suggestion{}}
	if raw == "" {
		writeConditional(w, r, http.StatusOK, out, out.identity())
		return
	}

	// Operator completions come first and cost nothing, so a slow store cannot
	// make the box feel broken while somebody is typing "app:".
	out.Suggestions = append(out.Suggestions, operatorSuggestions(raw)...)

	q := index.Parse(raw)
	q.Limit = SuggestLimit
	res, err := s.searcher.Search(r.Context(), p, q)
	if err != nil {
		s.log.Warn("suggest failed", "error", err, "tenant", p.Tenant)
		writeConditional(w, r, http.StatusOK, out, out.identity())
		return
	}
	for _, h := range res.Hits {
		if len(out.Suggestions) >= SuggestLimit {
			break
		}
		out.Suggestions = append(out.Suggestions, suggestion{
			Kind: "document",
			Text: h.Document.Title,
			Hint: h.Document.Source,
			ID:   h.Document.ID,
			URL:  h.Document.URL,
		})
	}
	out.TookMS = float64(time.Since(start).Microseconds()) / 1000
	writeConditional(w, r, http.StatusOK, out, out.identity())
}

// identity is the suggestions without the timing, for the same reason as
// [searchResponse.identity].
func (r suggestResponse) identity() any {
	r.TookMS = 0
	return r
}

// operators is the list the typeahead offers, which is also the documentation
// most people will ever read about the query language.
var operators = []struct{ Name, Hint string }{
	{"app", "limit to a connected app"},
	{"type", "limit to a document type"},
	{"in", "limit to a space, folder or channel"},
	{"from", "limit to an author"},
	{"owner", "limit to an owner"},
	{"updated", "limit by when it changed"},
}

func operatorSuggestions(raw string) []suggestion {
	last := raw[strings.LastIndex(raw, " ")+1:]
	if last == "" || strings.Contains(last, ":") {
		return nil
	}
	var out []suggestion
	for _, op := range operators {
		if !strings.HasPrefix(op.Name, strings.ToLower(last)) {
			continue
		}
		out = append(out, suggestion{
			Kind:  "operator",
			Text:  op.Name + ":",
			Hint:  op.Hint,
			Value: raw[:len(raw)-len(last)] + op.Name + ":",
		})
	}
	return out
}

func facetValues(in []index.Facet) []facet {
	out := make([]facet, 0, len(in))
	for _, v := range in {
		out = append(out, facet{Value: v.Value, Count: v.Count})
	}
	return out
}

func (s *Server) handleDocument(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	d, err := s.store.Get(r.Context(), p, r.PathValue("id"))
	switch {
	case errors.Is(err, genba.ErrNotFound):
		// A document the caller may not read and a document that does not exist
		// produce the same response, all the way out to the status code.
		writeError(w, http.StatusNotFound, "not_found", "no such document")
		return
	case err != nil:
		s.log.Error("document lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "the document could not be read")
		return
	}
	body := documentResponse(d)
	// The preview is where somebody decides whether to trust what they are
	// reading, so it carries the claim and whether this reader is one of the
	// people who could make one. Both come from the same request, because a
	// second one to find out whether to draw a button is a second round trip
	// before the panel can be painted.
	if v, ok := s.verifier(); ok {
		body.CanVerify = store.MayVerify(p, d)
		if got, err := v.Verifications(r.Context(), p, []string{d.ID}); err != nil {
			s.log.Error("verifications could not be read", "error", err, "document", d.ID)
		} else {
			body.Verified = verifiedOf(got[d.ID], s.now())
		}
	}
	// The owner is on the panel whether or not the driver can remember a
	// correction, because the document carries one either way. What the
	// capability adds is where the answer came from and the control to change
	// it.
	body.Owner = ownerOf(d, s.correction(r, p, d.ID))
	if _, ok := s.ownership(); ok {
		body.CanReassign = store.MayReassign(p, d)
	}
	// What other people have said about the document, on the same request as
	// everything else on the panel. Reporting needs no permission beyond reading,
	// so the button is offered to everybody the moment the driver can remember a
	// report, and only clearing it asks who this is.
	if _, ok := s.reporter(); ok {
		body.CanReport = true
		body.CanResolve = store.MayResolve(p, d)
		body.Stale = staleOf(s.reported(r, p, d.ID))
	}
	writeConditional(w, r, http.StatusOK, body, nil)
}

type documentBody struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`

	// MediaType is what the body is, so the interface can decide how to render
	// it instead of guessing from the file name a second time. It is lifted out
	// of the properties because every document has one and a client should not
	// have to reach into a bag of source specific strings for it.
	MediaType  string            `json:"media_type,omitempty"`
	URL        string            `json:"url,omitempty"`
	Source     string            `json:"source"`
	Kind       string            `json:"kind"`
	Container  string            `json:"container,omitempty"`
	ModifiedAt time.Time         `json:"modified_at,omitzero"`
	Properties map[string]string `json:"properties,omitempty"`

	// Verified is who vouched for this document and until when, absent when
	// nobody has, and CanVerify says whether this reader is one of the people
	// who could. Both are absent on a deployment whose driver cannot remember a
	// claim, which is what makes the interface leave the whole thing out rather
	// than offer a button that fails.
	Verified  *verification `json:"verified,omitempty"`
	CanVerify bool          `json:"can_verify,omitempty"`

	// Owner is who is accountable for the document, with the provenance on it
	// when somebody has corrected what the source said, and CanReassign says
	// whether this reader is one of the people who could. It is on the preview
	// rather than on every result because the owner is not what somebody is
	// choosing between ten results on, and it is the first thing they want when
	// the document turns out to be wrong.
	Owner       *owner `json:"owner,omitempty"`
	CanReassign bool   `json:"can_reassign,omitempty"`

	// Stale is what readers have said is wrong with the document, absent when
	// nobody has said anything. CanReport is whether this deployment can remember
	// a report at all, which is the same question as whether to offer the button,
	// because anybody who can read the document can make one. CanResolve is
	// whether this reader is one of the people who could clear what was said.
	Stale      *staleness `json:"stale,omitempty"`
	CanReport  bool       `json:"can_report,omitempty"`
	CanResolve bool       `json:"can_resolve,omitempty"`
}

// documentResponse is where the wire shape is decided. Permissions are not part
// of it: the caller has already been told what they may read by being given the
// document at all, and echoing an access control list back is how an internal
// group name ends up in somebody's browser.
func documentResponse(d doc.Document) documentBody {
	return documentBody{
		ID:         d.ID,
		Title:      d.Title,
		Body:       d.Body,
		MediaType:  d.Properties[doc.MediaType],
		URL:        d.URL,
		Source:     d.Source,
		Kind:       string(d.Kind),
		Container:  d.Container,
		ModifiedAt: d.ModifiedAt,
		Properties: d.Properties,
	}
}

// inlineTypes are the media types served with Content-Disposition: inline.
//
// The list is short and it is an allow list rather than a deny list. Everything
// on it is something a browser renders in an img element, where it cannot run
// script, and everything else is a download. An SVG is on the list for exactly
// that reason: as an image it is inert, and it is only dangerous when a
// document embeds it as markup, which this interface never does.
var inlineTypes = map[string]bool{
	"image/png":     true,
	"image/jpeg":    true,
	"image/gif":     true,
	"image/webp":    true,
	"image/svg+xml": true,
}

func (s *Server) handleContent(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	cs, ok := s.store.(store.ContentStore)
	if !ok {
		// A deployment on a driver that holds no bytes has no content to serve,
		// and says so the same way it would for a document that is not there.
		writeError(w, http.StatusNotFound, "not_found", "no such document")
		return
	}

	id := r.PathValue("id")
	// The document is read first because it is where the permission decision and
	// the media type both come from. The driver applies the principal again on
	// the content read, so this is a lookup rather than the check itself.
	d, err := s.store.Get(r.Context(), p, id)
	if err == nil {
		var c doc.Content
		c, err = cs.Content(r.Context(), p, id)
		if err == nil {
			s.writeContent(w, r, d, c)
			return
		}
	}
	switch {
	case errors.Is(err, genba.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such document")
	default:
		s.log.Error("content lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "the document could not be read")
	}
}

// writeContent sends the bytes with the caching and the sniffing rules that
// make an image safe to put in a page.
func (s *Server) writeContent(w http.ResponseWriter, r *http.Request, d doc.Document, c doc.Content) {
	media := d.Properties[doc.MediaType]
	disposition := "inline"
	if !inlineTypes[media] {
		// A type nobody vetted is not rendered in the page and is not described
		// to the browser as anything it might act on.
		media, disposition = "application/octet-stream", "attachment"
	}

	sum := sha256.Sum256(c.Bytes)
	w.Header().Set("Content-Type", media)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:8])+`"`)
	// Private, because the response depends on who asked. A shared cache that
	// kept this would be handing one tenant's screenshot to another.
	w.Header().Set("Cache-Control", "private, max-age=600")
	if c.Width > 0 && c.Height > 0 {
		w.Header().Set("X-Content-Dimensions", strconv.Itoa(c.Width)+"x"+strconv.Itoa(c.Height))
	}
	// ServeContent handles the conditional request, the range request and the
	// length, which are three things worth not writing again.
	http.ServeContent(w, r, "", d.ModifiedAt, bytes.NewReader(c.Bytes))
}

// statsResponse is what the corpus holds and what the caches have done with it.
//
// The cache numbers are here rather than behind an operator only endpoint
// because a cache nobody can see the hit rate of is a cache nobody can tell is
// broken. A layer sitting at a two percent hit rate is doing nothing but
// spending memory, and the only way anyone finds that out is by being able to
// look.
type statsResponse struct {
	Documents   int                    `json:"documents"`
	Quarantined int                    `json:"quarantined"`
	Cache       map[string]cache.Stats `json:"cache,omitempty"`

	// Driver is the storage this process was started with, and IndexedAt is
	// when it last committed a write, in RFC 3339. Both are empty where there
	// is nothing true to say: an embedder that named no driver, and a process
	// that has not seen the corpus move since it came up.
	//
	// They are here rather than on the health endpoint because that one is
	// unauthenticated, and what a deployment runs on is not something to hand
	// to anybody who can reach the port.
	Driver    string `json:"driver,omitempty"`
	IndexedAt string `json:"indexed_at,omitempty"`

	// Ranking reports whether the driver cuts to a candidate pool for itself, or
	// is walked document by document with the ranking done above it.
	//
	// It is the difference between a search whose cost follows the page and one
	// whose cost follows the corpus, and on the same few hundred documents it
	// measured as seventeen milliseconds against two seconds. That is far too
	// large a gap to leave somebody to infer from a latency graph, and it is not
	// a gap anybody guesses at: both drivers answer the same queries with the
	// same results, so the only thing that gives it away is the clock.
	//
	// It is always present rather than omitted when false, because false is the
	// case worth knowing about and a key that disappears is one nobody checks
	// for.
	Ranking bool `json:"ranking"`

	// Indexing is the source being read for the first time, and is absent when
	// nothing is, which is the ordinary case.
	//
	// It is a pointer so that absent means absent. The interface renders its
	// banner on the key being there and never has to compare two numbers to
	// work out whether a sync is running, which is what stops it flickering the
	// banner on every refresh of a corpus that is already complete.
	Indexing *Indexing `json:"indexing,omitempty"`
}

// handleStats reports the corpus and the cache counters.
//
// This is the one read endpoint with no entity tag, because the counters it
// reports are moved by the act of reading them. A tag over a body that counts
// its own reads can never match on the next request, so a conditional request
// here is a full response with an extra header on it, and a tag that excluded
// the counters would be worse: the client would hold the first hit rate it ever
// saw and never be told a different one. The response is a couple of hundred
// bytes and the client holds it for a TTL, so the round trip is cheap and the
// number is always real.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, _ *acl.Principal) {
	st, err := s.store.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "stats are not available")
		return
	}
	h := w.Header()
	h.Set("Cache-Control", cacheControl)
	h.Set(varyHeader, varyValue)
	var indexing *Indexing
	if current, ok := s.stillIndexing(); ok {
		indexing = &current
	}
	writeJSON(w, http.StatusOK, statsResponse{
		Documents:   st.Documents,
		Quarantined: st.Quarantined,
		Cache:       s.cacheStats(),
		Driver:      s.driver,
		IndexedAt:   s.indexedAt(),
		Ranking:     s.searcher.Ranking(),
		Indexing:    indexing,
	})
}

// indexedAt is when the corpus last moved, or empty if it has not moved since
// this process came up.
func (s *Server) indexedAt() string {
	ns := s.indexed.Load()
	if ns == 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339)
}

// cacheStats is every cache the process runs, under one name each.
//
// The searcher reports nil when it is running without a cache, so the map is
// built here rather than written into: an operator looking at a deployment with
// the search cache turned off should still be able to see whether the
// thumbnails are being generated once or on every scroll.
func (s *Server) cacheStats() map[string]cache.Stats {
	out := s.searcher.CacheStats()
	if out == nil {
		out = make(map[string]cache.Stats, 1)
	}
	out["thumbnail"] = s.thumbs.Stats()
	return out
}

// facets is the sidebar on the wire, with the values the query already narrows
// to marked.
//
// The counts here are not all counted over the same set of documents. A field
// the query narrows is counted with its own filter lifted, so its values say
// what choosing them instead would find, and the value already chosen says what
// is on the screen. Which of the two a number is depends entirely on the flag
// next to it, which is why the flag is on the wire rather than inferred.
func facets(in map[string][]index.Facet, r store.Request) map[string][]facet {
	out := make(map[string][]facet, len(in))
	for name, values := range in {
		chosen := chosenValues(r, name)
		fs := make([]facet, 0, len(values))
		for _, v := range values {
			fs = append(fs, facet{Value: v.Value, Count: v.Count, Selected: contains(chosen, v.Value)})
		}
		out[name] = fs
	}
	return out
}

// chosenValues is what the query narrows the field to, and nothing for a field
// it leaves alone.
func chosenValues(r store.Request, field string) []string {
	switch field {
	case "source":
		return r.Sources
	case "kind":
		kinds := make([]string, 0, len(r.Kinds))
		for _, k := range r.Kinds {
			kinds = append(kinds, string(k))
		}
		return kinds
	case "container":
		return r.Containers
	case "author":
		return r.Authors
	}
	return nil
}

// contains compares the way the filters themselves compare, through
// [store.Fold], so a link somebody typed in a different case still ticks the box
// it filtered by.
func contains(values []string, value string) bool {
	folded := store.Fold(value)
	for _, v := range values {
		if store.Fold(v) == folded {
			return true
		}
	}
	return false
}

func intParam(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.Atoi(v)
}

// listParam reads a filter that can appear more than once. Both ?source=a&
// source=b and ?source=a,b work, because links get written by hand and by
// code and neither form is wrong.
func listParam(v url.Values, name string) []string {
	var out []string
	for _, raw := range v[name] {
		for p := range strings.SplitSeq(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func kinds(values []string) []doc.Kind {
	if len(values) == 0 {
		return nil
	}
	out := make([]doc.Kind, 0, len(values))
	for _, v := range values {
		out = append(out, doc.Kind(v))
	}
	return out
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// The status line is already out, so a broken encode can only be logged by
	// the caller's middleware. Encoding into a buffer first would fix that at
	// the cost of a copy per response, and none of these bodies can fail to
	// encode.
	_ = json.NewEncoder(w).Encode(v)
}
