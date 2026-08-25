package api

import (
	"net/http"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/audit"
	"github.com/tamnd/genba/recheck"
	"github.com/tamnd/genba/store"
)

// Reported is the other half of saying a document is out of date: what somebody
// said has to reach the person who can do something about it.
//
// A report that lands in a table nobody reads is a report that was never made,
// and the reader who took the trouble to write the sentence learns that taking
// the trouble does nothing. So this endpoint exists for the owner rather than
// for the reporter, and it answers with the documents this caller owns or wrote
// that somebody has complained about, most recently complained about first.
const (
	// ReportedLimit is how many the caller gets when they do not say. It is the
	// home screen panel, which holds six rows and is a summary rather than a
	// queue.
	ReportedLimit = 10

	// ReportedMax is the most a caller can ask for, for the reason RecentMax
	// exists: this is read at a glance, and a URL should not be able to turn a
	// fast endpoint into a slow one.
	ReportedMax = 50
)

// reportedResponse is the inbox.
type reportedResponse struct {
	// Documents is what has been reported, and is an empty list rather than a
	// null when there is nothing, so the interface can count it without
	// checking whether it is there.
	Documents []reportedHit `json:"documents"`

	// At is when the server answered, which is what the interface shows beside a
	// list it is holding from a minute ago.
	At time.Time `json:"at"`
}

// reportedHit is one document with what was said about it.
//
// The document fields are embedded rather than nested, exactly as they are in
// the recent lists, so a row of this list and a row of a result list are the
// same shape to a client and are drawn by the same code.
type reportedHit struct {
	searchHit
	Stale *staleness `json:"stale"`
}

// identity is the response without the fields that move on their own, so that
// two answers a minute apart share a tag. When the server answered is one of
// them; when somebody reported a document is not, because that is a fact about
// the past.
func (r reportedResponse) identity() any {
	r.At = time.Time{}
	return r
}

// handleReported answers with what this caller's readers have said about this
// caller's own documents.
//
// A deployment whose driver cannot remember a report gets an empty list rather
// than a refusal, the way the recent screen gets one list instead of two when
// there is no history to read. The panel this feeds simply does not draw, and a
// home screen with one section missing is a better answer than a home screen
// with an error on it.
func (s *Server) handleReported(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	limit, err := intParam(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "limit must be a number")
		return
	}
	if limit <= 0 {
		limit = ReportedLimit
	}
	limit = min(limit, ReportedMax)

	out := reportedResponse{Documents: []reportedHit{}, At: s.now().UTC()}
	rep, ok := s.reporter()
	if !ok {
		// An empty list is still an answer about the corpus, and a deployment
		// whose driver remembers no reports should not have a hole in its trail
		// where this screen is.
		s.accessed(r, p, audit.Record{Action: audit.List, Outcome: audit.Served})
		writeConditional(w, r, http.StatusOK, out, out.identity())
		return
	}

	flagged, err := rep.Reported(r.Context(), p, limit)
	if err != nil {
		s.log.Error("the reported list could not be read", "error", err, "tenant", p.Tenant)
		s.accessed(r, p, audit.Record{Action: audit.List, Outcome: audit.Failed})
		writeError(w, http.StatusInternalServerError, "internal", "the reported list could not be read")
		return
	}
	// This list is somebody's own documents and the rows still go through the
	// recheck, because ownership in the index is as much of a snapshot as
	// readership is and a document that moved out from under somebody is one they
	// should stop being shown reports about.
	keep := s.keep(r.Context(), p, reportedItems(flagged))
	shown := make([]audit.Item, 0, len(flagged))
	for _, f := range flagged {
		if !keep(f.Document.ID) {
			continue
		}
		out.Documents = append(out.Documents, reportedHit{
			searchHit: hitOf(f.Document),
			Stale:     staleOf(f.Stale),
		})
		shown = append(shown, item(f.Document))
	}
	s.accessed(r, p, audit.Record{
		Action:    audit.List,
		Outcome:   audit.Served,
		Documents: shown,
		Count:     len(shown),
	})
	writeConditional(w, r, http.StatusOK, out, out.identity())
}

// reportedItems is the flagged documents as the recheck is asked about them.
func reportedItems(flagged []store.Flagged) []recheck.Item {
	out := make([]recheck.Item, 0, len(flagged))
	for _, f := range flagged {
		out = append(out, recheck.Item{ID: f.Document.ID, Source: f.Document.Source})
	}
	return out
}
