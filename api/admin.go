package api

import (
	"net/http"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

// HeldLimit is how many quarantined documents one administration response
// carries.
//
// A hundred, and the response says how many there are in total, so the screen
// can say a hundred of fourteen hundred rather than implying it has them all.
// The number is a compromise between two failures. Too few and a corpus with
// three connectors' worth of held documents shows one connector's, because the
// list is ordered by date and not spread across sources. Too many and a screen
// that has to answer in ten milliseconds is reading a megabyte of JSON to draw
// a list nobody scrolls to the bottom of.
const HeldLimit = 100

// Operations is what this deployment is doing, as whatever runs the connectors
// sees it.
//
// It is supplied by the caller through [WithOperations] rather than worked out
// here, for the same reason [Indexing] is: the thing that knows is the thing
// running the syncs, and this package deliberately does not. An embedder that
// indexes on its own schedule fills this in and gets the same screen.
type Operations struct {
	Connectors []Connector `json:"connectors"`
}

// Connector is one source this process syncs, and how it is getting on.
type Connector struct {
	// Source is the name the documents carry, which is what ties a row on this
	// screen to a filter in a search.
	Source string `json:"source"`

	// Kind is what sort of source it is, such as a directory or a bucket, and
	// Target is which one, so that two directories are two rows a person can
	// tell apart rather than two rows called directory.
	Kind   string `json:"kind"`
	Target string `json:"target"`

	// Tenant is whose corpus it feeds.
	Tenant string `json:"tenant"`

	// Refresh is how often it syncs again, as a duration, and is empty for a
	// source that syncs once at startup and never again.
	Refresh string `json:"refresh,omitempty"`

	// Syncing says a run is going right now. It is separate from the last run's
	// outcome because the two answer different questions, and a screen that
	// worked it out by comparing timestamps would flicker.
	Syncing bool `json:"syncing"`

	// Runs are the syncs this process has done, most recent first and bounded.
	// A source that has not finished one yet has none, which is not an error
	// and is what a screen should say rather than drawing a zero.
	Runs []Run `json:"runs"`

	// Permissions is what the access control mapping made of this source, and
	// is absent for a connector whose policy does not count. It is the aggregate
	// half of the quarantine: this says how many were held for each reason
	// across the whole corpus, and the list below says which documents.
	Permissions *Mapping `json:"permissions,omitempty"`
}

// Run is one sync of one source.
type Run struct {
	// Started is when it began, in RFC 3339, and Duration is how long it took
	// in milliseconds. A run still going has a duration of zero.
	Started  string `json:"started"`
	Duration int64  `json:"duration_ms"`

	// Error is what went wrong, and is empty for a run that finished.
	//
	// It is the message rather than a code, because there is nothing useful to
	// switch on: the failures here are a directory that disappeared, a bucket
	// refusing a signature and a token that expired, and what an operator needs
	// is the sentence. This is the whole of what box two of the issue asks for,
	// so it is not truncated.
	Error string `json:"error,omitempty"`

	// The counts, which are ingest.Stats as it crossed the wire. Indexed and
	// Quarantined are documents this run stored, Deleted is what it removed
	// because the source no longer had it, Repermissioned is what it rewrote
	// the access control list of without refetching, and Skipped is what it
	// rejected before the store.
	Indexed        int   `json:"indexed"`
	Quarantined    int   `json:"quarantined"`
	Deleted        int   `json:"deleted"`
	Repermissioned int   `json:"repermissioned"`
	Skipped        int   `json:"skipped"`
	Bytes          int64 `json:"bytes"`
}

// Mapping is what one source's permission mapping has seen, by reason.
//
// The three quarantine reasons are separate because they want three different
// actions. A foreign domain is a decision somebody has to make about the
// tenant, an unmappable deny is usually a source feature nobody has written the
// mapping for yet, and a malformed grant is a bug in a connector. One number
// for all three is a number nobody can act on.
type Mapping struct {
	Mapped         int64 `json:"mapped"`
	ForeignDomain  int64 `json:"foreign_domain"`
	UnmappableDeny int64 `json:"unmappable_deny"`
	Malformed      int64 `json:"malformed"`

	// Ignored is statements whose role does not confer read. It is not a
	// failure and it is worth watching: a sudden climb usually means a source
	// renamed a role and the mapping has not caught up, which shows up to
	// somebody as documents they used to be able to find.
	Ignored int64 `json:"ignored"`
}

// WithOperations supplies what the connectors are doing.
//
// Without it the administration screen still works and says this process runs
// no connectors, which is the truth for an embedder that indexes elsewhere and
// is a better answer than an empty list that looks like a broken sync.
func WithOperations(fn func() Operations) Option {
	return func(s *Server) { s.operations = fn }
}

// adminResponse is the whole administration screen in one request.
//
// One request rather than three, because every number on the screen is a
// snapshot of the same moment and three requests would let the list of held
// documents disagree with the count above it. It is also the cheapest shape:
// the connector state is in memory, the counts are one aggregate, and the list
// is one bounded query.
type adminResponse struct {
	Connectors []Connector `json:"connectors"`

	// Documents and Quarantined are the corpus, the same two numbers the stats
	// endpoint reports. They are repeated here rather than left to a second
	// request so that the total above the list and the list itself were read
	// together.
	Documents   int `json:"documents"`
	Quarantined int `json:"quarantined"`

	// Held is a bounded sample of the quarantine, newest first where the driver
	// can order it. Listable says whether the driver can produce one at all: a
	// screen showing an empty list under a count of fourteen hundred has to be
	// able to say the driver cannot list them rather than that there are none.
	Held     []held `json:"held"`
	Listable bool   `json:"listable"`
}

// held is one quarantined document on the wire.
type held struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`

	// ModifiedAt is when the source last changed it, in RFC 3339, and is absent
	// where the source never said.
	ModifiedAt string `json:"modified_at,omitempty"`
}

// admin wraps a handler that only an administrator may reach.
//
// The refusal is logged and the success is logged, both with the subject, which
// is the beginning of the audit trail these screens need. A read of what the
// deployment is doing is not a read of anybody's documents, and it is still
// something a deployment should be able to show somebody afterwards.
func (s *Server) admin(h func(http.ResponseWriter, *http.Request, *acl.Principal)) http.Handler {
	return s.authenticated(func(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
		if !p.HasRole(acl.RoleAdmin) {
			s.log.Warn("administration refused",
				"subject", p.Subject, "tenant", p.Tenant, "path", r.URL.Path)
			// Forbidden rather than not found. Hiding the existence of an
			// administration endpoint protects nothing, because it is in the
			// interface and in this file, and a person who has been given the
			// wrong account needs to be told which of the two things is wrong.
			writeError(w, http.StatusForbidden, "forbidden", "this needs an administrator")
			return
		}
		s.log.Info("administration read",
			"subject", p.Subject, "tenant", p.Tenant, "path", r.URL.Path)
		h(w, r, p)
	})
}

// handleAdmin reports what the deployment is doing.
//
// Like the stats endpoint this carries no entity tag, and for the same reason:
// half of what it reports is counters that move while nobody is looking, so a
// tag over the body could never match and a tag that excluded them would pin
// the client to the first answer it ever saw.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	st, err := s.store.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "the corpus statistics are not available")
		return
	}

	res := adminResponse{
		Connectors:  []Connector{},
		Documents:   st.Documents,
		Quarantined: st.Quarantined,
		Held:        []held{},
	}
	if s.operations != nil {
		res.Connectors = s.operations().Connectors
	}

	// The listing is a capability rather than part of the interface, so a driver
	// that cannot walk what it is not indexing says so and the screen says so
	// too. See [store.Quarantine].
	if q, ok := s.store.(store.Quarantine); ok {
		res.Listable = true
		// Only asked for when there is something to list. On the ordinary corpus
		// the count is zero and this is a query nobody has to make.
		if st.Quarantined > 0 {
			list, err := q.Quarantined(r.Context(), p.Tenant, HeldLimit)
			if err != nil {
				// Not fatal. The connectors and the counts are still the truth,
				// and a screen missing its list is a great deal more use than a
				// screen missing everything.
				s.log.Error("listing the quarantine", "tenant", p.Tenant, "error", err)
				res.Listable = false
			}
			res.Held = heldOf(list)
		}
	}

	h := w.Header()
	h.Set("Cache-Control", cacheControl)
	h.Set(varyHeader, varyValue)
	writeJSON(w, http.StatusOK, res)
}

// heldOf puts the store's quarantine list on the wire.
func heldOf(in []store.Held) []held {
	out := make([]held, len(in))
	for i, h := range in {
		out[i] = held{ID: h.ID, Title: h.Title, Source: h.Source, Reason: h.Reason}
		if !h.At.IsZero() {
			out[i].ModifiedAt = h.At.UTC().Format(time.RFC3339)
		}
	}
	return out
}
