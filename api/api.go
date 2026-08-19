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
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/cache"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
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

	// heartbeat is how often an idle event stream sends a comment.
	heartbeat time.Duration
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
	}
	for _, opt := range opts {
		opt(s)
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
	mux.Handle("GET /api/v1/documents/{id}/content", s.authenticated(s.handleContent))
	mux.Handle("GET /api/v1/stats", s.authenticated(s.handleStats))
	mux.Handle("GET /api/v1/events", s.authenticated(s.handleEvents))
	if s.assets != nil {
		mux.Handle("GET /", s.assets)
	}
	return s.recoverPanic(mux)
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
	Uptime  string `json:"uptime"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Version: genba.Version,
		Commit:  genba.Commit,
		Uptime:  time.Since(s.started).Round(time.Second).String(),
	})
}

// handleReady reports whether the process can serve queries. It touches the
// store, because a process that is up but cannot reach its storage is not ready
// and a load balancer needs to know the difference.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.Stats(r.Context()); err != nil {
		s.log.Warn("readiness check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "storage is not available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type searchResponse struct {
	Query string `json:"query"`

	// Text is what was left of the query after the operators were parsed out of
	// it, so the interface can show which part of what somebody typed became a
	// filter rather than a search term.
	Text    string             `json:"text"`
	Filters searchFilters      `json:"filters"`
	Total   int                `json:"total"`
	Partial bool               `json:"partial,omitempty"`
	TookMS  float64            `json:"took_ms"`
	Hits    []searchHit        `json:"hits"`
	Facets  map[string][]facet `json:"facets"`
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
}

type passage struct {
	Text  string `json:"text"`
	Match bool   `json:"match,omitempty"`
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

	out := searchResponse{
		Query:   r.URL.Query().Get("q"),
		Text:    query.Text,
		Filters: filtersOf(query),
		Total:   res.Total,
		Partial: res.Truncated,
		TookMS:  float64(res.Took.Microseconds()) / 1000,
		Facets:  facets(res.Facets),
		Hits:    []searchHit{},
	}
	for _, h := range res.Hits {
		out.Hits = append(out.Hits, searchHit{
			ID:         h.Document.ID,
			Title:      h.Document.Title,
			URL:        h.Document.URL,
			Source:     h.Document.Source,
			Kind:       string(h.Document.Kind),
			Container:  h.Document.Container,
			Author:     personName(h.Document.Author),
			MediaType:  h.Document.Properties[doc.MediaType],
			ModifiedAt: h.Document.ModifiedAt,
			Snippet:    h.Snippet,
			Passages:   passages(h.Passages),
			Score:      h.Score,
		})
	}
	writeConditional(w, r, http.StatusOK, out, out.identity())
}

// identity is the response without the number that changes on every request.
// Two searches that found the same documents have the same identity even though
// one of them took a tenth of a millisecond longer.
func (r searchResponse) identity() any {
	r.TookMS = 0
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
	out := meResponse{Subject: p.Subject, Tenant: p.Tenant, Roles: p.Roles, Sources: []facet{}, Kinds: []facet{}}
	res, err := s.searcher.Search(r.Context(), p, index.Query{Limit: 1})
	if err != nil {
		s.log.Error("bootstrap failed", "error", err, "tenant", p.Tenant)
		writeError(w, http.StatusInternalServerError, "internal", "the session could not be loaded")
		return
	}
	out.Sources = facetValues(res.Facets["source"])
	out.Kinds = facetValues(res.Facets["kind"])
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
	writeConditional(w, r, http.StatusOK, documentResponse(d), nil)
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
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, _ *acl.Principal) {
	st, err := s.store.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "stats are not available")
		return
	}
	writeConditional(w, r, http.StatusOK, statsResponse{
		Documents:   st.Documents,
		Quarantined: st.Quarantined,
		Cache:       s.searcher.CacheStats(),
	}, nil)
}

func facets(in map[string][]index.Facet) map[string][]facet {
	out := make(map[string][]facet, len(in))
	for name, values := range in {
		fs := make([]facet, 0, len(values))
		for _, v := range values {
			fs = append(fs, facet{Value: v.Value, Count: v.Count})
		}
		out[name] = fs
	}
	return out
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
