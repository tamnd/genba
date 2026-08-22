package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

// connectorBodyLimit is how large a connector's configuration may be.
//
// Sixteen kilobytes, which is a hundred times the largest configuration any
// connector here has and small enough that a request nobody meant to send is
// refused before it is read. The settings are a directory, an endpoint and a
// handful of names, not a document.
const connectorBodyLimit = 16 << 10

// ErrUnmanaged and ErrBadConnector are what a [Supervisor] returns for the two
// refusals that are not a server fault.
//
// A missing connector is [genba.ErrNotFound], which is the sentinel the rest of
// this system already uses for it.
var (
	// ErrUnmanaged is a connector this process was started with. It is on the
	// screen because it is running and it cannot be changed from there, because
	// the next restart would read the command line again and undo whatever was
	// typed. Saying so is better than either lying about it or hiding it.
	ErrUnmanaged = errors.New("connector was configured on the command line")

	// ErrBadConnector is settings that cannot be run: a directory that is not
	// there, an access control policy nobody has written, a bucket with no
	// endpoint. It is the caller's mistake and the message says which.
	ErrBadConnector = errors.New("connector configuration is not valid")
)

// Supervisor is whatever runs the connectors, from this package's side.
//
// It is supplied by the caller through [WithSupervisor] for the same reason
// [Operations] is supplied through [WithOperations]: the thing that knows how
// to build a crawler out of a directory name is the process that owns the
// crawlers, and this package deliberately does not. What is here is the role
// check, the audit line and the shape of the request.
//
// Every method takes the tenant separately from the source because that pair is
// the key, and because an administrator of one deployment must not be able to
// reach another tenant's connectors by naming one. The tenant is the
// administrator's own and this package fills it in.
//
// The methods are expected to return only after the change has been written
// down. A call that came back and then lost the connector on the next restart
// would be the one failure this whole capability exists to remove.
type Supervisor interface {
	// Add saves a connector and starts it if it is enabled. Adding one that is
	// already there replaces its settings, which is what makes editing a field
	// the same request as creating it.
	Add(ctx context.Context, f store.Feed) error

	// Remove stops a connector and forgets how it was configured. The documents
	// it indexed stay, because forgetting how a corpus was read is not a
	// decision to delete the corpus.
	Remove(ctx context.Context, tenant, source string) error

	// Start and Stop turn a configured connector on and off without losing its
	// settings, which is the difference between silencing a source that is
	// producing errors and having to type it in again afterwards.
	Start(ctx context.Context, tenant, source string) error
	Stop(ctx context.Context, tenant, source string) error

	// Sync asks for a run now rather than at the next interval. It returns as
	// soon as the run has been asked for, not when it finishes: a crawl of a
	// real source takes minutes and a request that waited for one would time
	// out somewhere in between and leave nobody able to say whether it ran.
	Sync(ctx context.Context, tenant, source string) error
}

// WithSupervisor supplies the thing that can change the connectors.
//
// Without it the administration screen still lists what this process is doing
// and says the connectors cannot be changed from here, which is the truth for
// an embedder that wires its own crawlers and is a better answer than a form
// whose answers go nowhere.
func WithSupervisor(sup Supervisor) Option {
	return func(s *Server) { s.supervisor = sup }
}

// connectorRequest is a connector as it arrives from the interface.
//
// It is not [store.Feed]. The wire shape carries no timestamps and no author,
// because both of those are facts the server records rather than claims the
// caller gets to make, and a request that could set them would be a request
// that could rewrite who configured what.
type connectorRequest struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`

	// Enabled says whether it should start. It defaults to false when it is
	// absent, which reads backwards until you write it out: a connector arrives
	// from a form that has the box on it, and a client that forgot to send the
	// field is a client whose connector does not start, rather than one that
	// starts crawling somebody's file server because a field was missing.
	Enabled bool `json:"enabled"`

	// Config is the kind specific settings, passed through untouched. This
	// package does not know what is in it and does not look.
	Config json.RawMessage `json:"config"`
}

// handleAddConnector saves a connector and starts it.
//
// The audit line names the source and the person, which the wrapper's line
// cannot: it records that an administrator changed something, and this records
// what. Those are two different things to have to explain afterwards.
func (s *Server) handleAddConnector(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	sup := s.supervisor
	if sup == nil {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment configures its connectors on the command line")
		return
	}

	var body connectorRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, connectorBodyLimit)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be a JSON object with a source, a kind and a config")
		return
	}
	body.Source = strings.TrimSpace(body.Source)
	body.Kind = strings.TrimSpace(body.Kind)
	if body.Source == "" || body.Kind == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "a connector needs a source and a kind")
		return
	}

	f := store.Feed{
		Tenant:  p.Tenant,
		Source:  body.Source,
		Kind:    body.Kind,
		Enabled: body.Enabled,
		Config:  body.Config,
		By:      p.Subject,
	}
	s.log.Info("connector saved", "subject", p.Subject, "tenant", p.Tenant, "source", f.Source, "kind", f.Kind, "enabled", f.Enabled)
	if err := sup.Add(r.Context(), f); err != nil {
		s.connectorError(w, "saving a connector", f.Source, err)
		return
	}
	s.writeConnectors(w)
}

// handleDropConnector forgets a connector.
func (s *Server) handleDropConnector(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	s.connectorAction(w, r, p, "connector removed", func(sup Supervisor, source string) error {
		return sup.Remove(r.Context(), p.Tenant, source)
	})
}

// handleStartConnector switches a configured connector on.
func (s *Server) handleStartConnector(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	s.connectorAction(w, r, p, "connector started", func(sup Supervisor, source string) error {
		return sup.Start(r.Context(), p.Tenant, source)
	})
}

// handleStopConnector switches a configured connector off and keeps it.
func (s *Server) handleStopConnector(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	s.connectorAction(w, r, p, "connector stopped", func(sup Supervisor, source string) error {
		return sup.Stop(r.Context(), p.Tenant, source)
	})
}

// handleSyncConnector asks for a run now.
func (s *Server) handleSyncConnector(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	s.connectorAction(w, r, p, "connector sync asked for", func(sup Supervisor, source string) error {
		return sup.Sync(r.Context(), p.Tenant, source)
	})
}

// connectorAction is the part the four one word endpoints have in common: is
// there a supervisor, is there a source in the path, say who did what, do it,
// and answer with the list the screen redraws from.
func (s *Server) connectorAction(w http.ResponseWriter, r *http.Request, p *acl.Principal, what string, do func(Supervisor, string) error) {
	sup := s.supervisor
	if sup == nil {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment configures its connectors on the command line")
		return
	}
	source := strings.TrimSpace(r.PathValue("source"))
	if source == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name the connector")
		return
	}
	s.log.Info(what, "subject", p.Subject, "tenant", p.Tenant, "source", source)
	if err := do(sup, source); err != nil {
		s.connectorError(w, what, source, err)
		return
	}
	s.writeConnectors(w)
}

// writeConnectors answers with what the connectors are doing now.
//
// A mutation answers with the whole list rather than with nothing, because the
// screen that sent it draws that list and the alternative is a second request
// for state the server had in its hand. It is the same body the administration
// endpoint reports under connectors, so the screen has one way of reading it.
func (s *Server) writeConnectors(w http.ResponseWriter) {
	out := Operations{Connectors: []Connector{}}
	if s.operations != nil {
		out = s.operations()
	}
	h := w.Header()
	h.Set("Cache-Control", cacheControl)
	h.Set(varyHeader, varyValue)
	writeJSON(w, http.StatusOK, out)
}

// connectorError turns a supervisor's refusal into a status.
//
// The three that are not a server fault say so, because each of them wants a
// different thing from whoever is reading: a name that is not there is a typo,
// a connector from the command line is a unit file to edit, and settings that
// cannot be run are a field to correct. Anything else is this deployment's
// problem and is logged with the source rather than shown.
func (s *Server) connectorError(w http.ResponseWriter, what, source string, err error) {
	switch {
	case errors.Is(err, genba.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "there is no connector called "+source)
	case errors.Is(err, ErrUnmanaged):
		writeError(w, http.StatusConflict, "unmanaged", "this connector was configured on the command line and has to be changed there")
	case errors.Is(err, ErrBadConnector):
		// The message is the supervisor's, because the useful half of it is the
		// part this package cannot write: which field is wrong and why.
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	default:
		s.log.Error(what+" failed", "source", source, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "the connector could not be changed")
	}
}
