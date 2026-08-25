package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/audit"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// Recent answers two questions with one request, because the home screen asks
// both of them at once and a screen that paints in two stages paints twice.
//
// What somebody opened is theirs and comes from the server, so it follows them
// from the laptop to the desk machine, which is the whole value of the list.
// What changed is the corpus, sorted by recency, and it is the same query the
// browse screen runs. Neither half is a search, and both are permission
// filtered inside the storage driver like everything else.
const (
	// RecentLimit is how many rows each half carries when the caller does not
	// say. Twenty is a screen of them.
	RecentLimit = 20

	// RecentMax is the most a caller can ask for. The list is read at a glance
	// and nobody scrolls two hundred rows of it, so the ceiling is here to keep
	// a URL from turning a four millisecond endpoint into a slow one.
	RecentMax = 50

	// recentBodyLimit is how many bytes a record of an open may be. The body is
	// one document id.
	recentBodyLimit = 4 << 10
)

type recentResponse struct {
	Opened  []openedHit `json:"opened"`
	Changed []searchHit `json:"changed"`

	// At is when the server answered, which is what the interface shows next to
	// a list it is holding from a minute ago.
	At time.Time `json:"at"`
}

// openedHit is a hit with the time this person opened it. The document fields
// are embedded rather than nested, so one row of the opened list and one row of
// a result list are the same shape to a client and can be drawn by the same
// code.
type openedHit struct {
	searchHit
	At time.Time `json:"at"`
}

// identity is the response without the fields that move on their own, for the
// same reason as [searchResponse.identity]. When the server answered is one of
// them: it is a different number on every request and hashing it would make a
// tag that can never match. The time an entry was opened is not, because that
// is a fact about the past.
func (r recentResponse) identity() any {
	r.At = time.Time{}
	opened := make([]openedHit, len(r.Opened))
	copy(opened, r.Opened)
	for i := range opened {
		opened[i].Score = 0
	}
	changed := make([]searchHit, len(r.Changed))
	copy(changed, r.Changed)
	for i := range changed {
		changed[i].Score = 0
	}
	r.Opened, r.Changed = opened, changed
	return r
}

func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	limit, err := intParam(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "limit must be a number")
		return
	}
	if limit <= 0 {
		limit = RecentLimit
	}
	limit = min(limit, RecentMax)

	out := recentResponse{
		Opened:  []openedHit{},
		Changed: []searchHit{},
		At:      s.now().UTC(),
	}
	// Both halves land on one record, because they are one screen and a trail
	// that split them would say somebody looked at two things when they looked
	// at a home page once.
	var shown []audit.Item

	// A driver that cannot remember an open serves the other half rather than an
	// error. The interface is built for that: a deployment on a store with no
	// history still has a home screen, it just has one list on it.
	if log, ok := s.store.(store.OpenLog); ok {
		opens, err := log.Opens(r.Context(), p, limit)
		if err != nil {
			// The same reasoning one level up. A history that could not be read is
			// a degraded home screen, and failing the whole request over it would
			// take the corpus half down with it.
			s.log.Warn("reading the open history failed", "error", err, "tenant", p.Tenant)
		}
		for _, o := range opens {
			out.Opened = append(out.Opened, openedHit{searchHit: hitOf(o.Document), At: o.At.UTC()})
			shown = append(shown, item(o.Document))
		}
	}

	changed, err := s.searcher.Recent(r.Context(), p, limit)
	if err != nil {
		s.log.Error("recent failed", "error", err, "tenant", p.Tenant)
		s.accessed(r, p, audit.Record{Action: audit.List, Outcome: audit.Failed})
		writeError(w, http.StatusInternalServerError, "internal", "the recent list could not be read")
		return
	}
	for _, d := range changed {
		out.Changed = append(out.Changed, hitOf(d))
		shown = append(shown, item(d))
	}
	s.accessed(r, p, audit.Record{
		Action:    audit.List,
		Outcome:   audit.Served,
		Documents: shown,
		Count:     len(shown),
	})

	// One question for both halves, because they are one screen. A badge that
	// showed up in results and not here would read as a bug in the badge rather
	// than as two endpoints that were written a week apart.
	if vouched := s.verifications(r, p, recentIDs(out)); len(vouched) > 0 {
		now := s.now()
		for i := range out.Opened {
			out.Opened[i].Verified = verifiedOf(vouched[out.Opened[i].ID], now)
		}
		for i := range out.Changed {
			out.Changed[i].Verified = verifiedOf(vouched[out.Changed[i].ID], now)
		}
	}
	writeConditional(w, r, http.StatusOK, out, out.identity())
}

// handleRecordOpen notes that the caller opened a document.
//
// It answers 204 for anything it understood, including an id nobody may read
// and a store that keeps no history. The client sends this with keepalive on
// the way to another screen and never looks at the answer, so the only reason
// to say anything else is a body this endpoint cannot parse, which is a bug in
// the caller rather than a fact about the corpus.
func (s *Server) handleRecordOpen(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, recentBodyLimit)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be a JSON object with an id")
		return
	}
	if body.ID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "id is required")
		return
	}

	if log, ok := s.store.(store.OpenLog); ok {
		if err := log.RecordOpen(r.Context(), p, body.ID, s.now()); err != nil {
			s.log.Warn("recording an open failed", "error", err, "tenant", p.Tenant)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// recentIDs is every document on the screen, in one slice.
//
// A document that is in both halves is in here twice, which costs a repeated id
// in one query and saves the caller a set. The two lists are twenty rows each
// and the driver answers on a primary key.
func recentIDs(out recentResponse) []string {
	ids := make([]string, 0, len(out.Opened)+len(out.Changed))
	for _, h := range out.Opened {
		ids = append(ids, h.ID)
	}
	for _, h := range out.Changed {
		ids = append(ids, h.ID)
	}
	return ids
}

// hitOf is the wire shape of a document on its own, with no search behind it.
//
// A result carries a snippet, a set of matched passages and a score, and none
// of those exist for a document that arrived by being recently opened or
// recently changed. Everything else about the row is the same, and it is the
// same function so that a field added to a result cannot go missing from the
// home screen.
func hitOf(d doc.Document) searchHit {
	return searchHit{
		ID:         d.ID,
		Title:      d.Title,
		URL:        d.URL,
		Source:     d.Source,
		Kind:       string(d.Kind),
		Container:  d.Container,
		Author:     personName(d.Author),
		MediaType:  d.Properties[doc.MediaType],
		ModifiedAt: d.ModifiedAt,
	}
}
