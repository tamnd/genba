package api

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/audit"
)

// DocumentsMax is how many documents one lookup may name.
//
// Fifty, which is more than any screen in this product cites at once and few
// enough that the driver reads them in one statement. It is a bound on the
// request rather than a page size: there is no cursor here and there is not
// meant to be, because this endpoint answers a question about a list somebody
// already holds rather than about the corpus.
const DocumentsMax = 50

// documentsResponse is a handful of documents named by id.
//
// The rows are the same shape a result row is, which is what makes this useful:
// a screen that has ids and wants titles draws them with the code it already
// has for drawing a result.
type documentsResponse struct {
	Documents []searchHit `json:"documents"`
}

// handleDocuments resolves a handful of ids into rows.
//
// The screen that needs this is the answers editor. An answer's sources are
// stored as ids, deliberately, so that an answer written by somebody with
// broad access does not become a list of documents a reader may not open, and
// the price of that is that anything editing one has ids where it needs
// titles. Nobody knows the id of a document, so an editor that printed them
// would be asking somebody to proofread a list of paths.
//
// It resolves through the caller like everything else, so an id this person
// cannot read is simply not in the answer, and the caller is never told the
// difference between a document that was taken down and one they may not see.
// That is also why what comes back is display only: the editor keeps the ids it
// was given and saves those, because saving what came back would quietly drop
// every source the editor happens not to have access to.
func (s *Server) handleDocuments(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	// Deduplicated, keeping the order they were asked for in. A screen that
	// cites the same document twice is a screen with a bug in it, and answering
	// it with the document twice makes that bug somebody else's to find.
	var ids []string
	for _, id := range r.URL.Query()["id"] {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) > DocumentsMax {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("name at most %d documents in one request", DocumentsMax))
		return
	}

	out := documentsResponse{Documents: []searchHit{}}
	found := s.documents(r, p, ids)
	for _, d := range found {
		out.Documents = append(out.Documents, hitOf(d))
	}
	// What came back rather than what was asked for. An id the caller may not
	// read is not on the record as a refusal, because this endpoint is handed a
	// list somebody already holds and half of it not resolving is the normal
	// case rather than an event.
	s.accessed(r, p, audit.Record{
		Action:    audit.List,
		Outcome:   audit.Served,
		Documents: items(found),
		Count:     len(found),
	})
	writeConditional(w, r, http.StatusOK, out, nil)
}
