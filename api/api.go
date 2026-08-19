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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
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

// WithClock sets the clock used for uptime reporting.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.started = now() }
}

// New returns a server. It does not listen: [Server.Handler] returns something
// the caller mounts wherever it likes, which is what makes the platform
// embeddable in an existing Go service.
func New(st store.Store, searcher *index.Searcher, auth Authenticator, opts ...Option) *Server {
	s := &Server{
		store:    st,
		searcher: searcher,
		auth:     auth,
		log:      slog.Default(),
		started:  time.Now(),
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
	mux.Handle("GET /api/v1/search", s.authenticated(s.handleSearch))
	mux.Handle("GET /api/v1/documents/{id}", s.authenticated(s.handleDocument))
	mux.Handle("GET /api/v1/stats", s.authenticated(s.handleStats))
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
	Query  string             `json:"query"`
	Total  int                `json:"total"`
	Hits   []searchHit        `json:"hits"`
	Facets map[string][]facet `json:"facets"`
}

// facet is the wire shape of index.Facet. The extra type is worth it: it keeps
// the ranking package free of json tags, so a field can be renamed there
// without changing what a client parses.
type facet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type searchHit struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	URL     string  `json:"url,omitempty"`
	Source  string  `json:"source"`
	Kind    string  `json:"kind"`
	Snippet string  `json:"snippet,omitempty"`
	Score   float64 `json:"score"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	q := r.URL.Query()
	limit, err := intParam(q.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "limit must be a number")
		return
	}
	offset, err := intParam(q.Get("offset"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "offset must be a number")
		return
	}

	query := index.Query{
		Text:      q.Get("q"),
		Sources:   splitList(q.Get("source")),
		Kinds:     kinds(splitList(q.Get("kind"))),
		Container: q.Get("container"),
		Limit:     limit,
		Offset:    offset,
	}

	res, err := s.searcher.Search(r.Context(), p, query)
	if err != nil {
		s.log.Error("search failed", "error", err, "tenant", p.Tenant)
		writeError(w, http.StatusInternalServerError, "internal", "the search could not be run")
		return
	}

	out := searchResponse{Query: query.Text, Total: res.Total, Facets: facets(res.Facets), Hits: []searchHit{}}
	for _, h := range res.Hits {
		out.Hits = append(out.Hits, searchHit{
			ID:      h.Document.ID,
			Title:   h.Document.Title,
			URL:     h.Document.URL,
			Source:  h.Document.Source,
			Kind:    string(h.Document.Kind),
			Snippet: h.Snippet,
			Score:   h.Score,
		})
	}
	writeJSON(w, http.StatusOK, out)
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
	writeJSON(w, http.StatusOK, documentResponse(d))
}

type documentBody struct {
	ID         string            `json:"id"`
	Title      string            `json:"title"`
	Body       string            `json:"body"`
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
		URL:        d.URL,
		Source:     d.Source,
		Kind:       string(d.Kind),
		Container:  d.Container,
		ModifiedAt: d.ModifiedAt,
		Properties: d.Properties,
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, _ *acl.Principal) {
	st, err := s.store.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "stats are not available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{
		"documents":   st.Documents,
		"quarantined": st.Quarantined,
	})
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

func splitList(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
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
