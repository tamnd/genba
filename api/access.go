package api

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

// GroupLimit is how many groups one access question may name.
//
// A real expansion of a large directory is hundreds of groups and this is above
// that. It is here because the groups arrive in a query string that somebody
// can type, and a question naming ten thousand of them is a request to build a
// ten thousand element predicate, which is a way of making one screen expensive
// for everybody else on the deployment.
const GroupLimit = 512

// accessResponse is what one person can reach, as an operator sees it.
//
// It is counts and never content. The administrator role grants nothing over
// documents, and a screen that answered this question with a list of titles
// would be a way of reading any corpus through somebody else's eyes, which is
// exactly the thing the role is careful not to be. Counts answer what the
// question is really for: whether somebody's access is the shape it should be,
// and whether it changed.
type accessResponse struct {
	// The principal as it was assembled, echoed back. An operator who typed a
	// group name wrongly gets an answer of zero, and the only way to tell that
	// apart from somebody genuinely having no access is to see what was asked.
	Subject    string   `json:"subject"`
	Tenant     string   `json:"tenant"`
	Groups     []string `json:"groups"`
	Identities []string `json:"identities"`

	// Documents is the total, and Sources is the same total broken up by
	// connector, largest first. Both are empty unless the question asked for
	// them, so read Counted rather than reading a zero.
	Documents int      `json:"documents"`
	Sources   []source `json:"sources"`

	// Countable says the driver can answer the count at all. A screen showing
	// zero under a corpus of a million documents has to be able to say the
	// driver cannot count rather than that this person can see nothing.
	Countable bool `json:"countable"`

	// Counted says this answer carries the counts.
	//
	// They are asked for rather than always computed, because they are an
	// aggregate over every document in the tenant and the check beside them is
	// two indexed reads. Charging the second for the first would make the
	// question an operator asks constantly wait on the one they ask
	// occasionally. See [Server.handleAccess].
	Counted bool `json:"counted"`

	// Document is the answer about one document, present only when the question
	// named one.
	Document *documentAccess `json:"document,omitempty"`
}

// source is one connector and how much of it this person can reach.
type source struct {
	Source    string `json:"source"`
	Documents int    `json:"documents"`
}

// documentAccess is whether one person can read one document, and why.
//
// The why is only ever filled in for a document they can read. That asymmetry
// is deliberate and it is the line this endpoint does not cross: explaining a
// refusal means reading the descriptor of a document the operator has no right
// to, and the difference between "the rules do not admit them", "it is held
// back" and "it is not there" is exactly the difference somebody could use to
// prove a document exists.
type documentAccess struct {
	ID      string `json:"id"`
	Visible bool   `json:"visible"`

	// Rule is [acl.Rule], the clause that admitted them, and Matched is the
	// group or the account it matched, in the form the rule compares. Both are
	// empty for a document they cannot read.
	Rule    string `json:"rule,omitempty"`
	Matched string `json:"matched,omitempty"`
}

// handleAccess answers what one person can see.
//
// It is the question every operator eventually asks, usually in the shape of
// "why can the contractor find that" or "why can nobody in support find
// anything". Answering it by logging in as somebody else is what people do
// without this, and that is worse in every way: it is not audited, it needs
// their credentials, and it reads their documents.
//
// The audit line is the point of the endpoint as much as the answer is. The
// wrapper has already recorded that an administrator read something, and this
// records which person they asked about, because those are two different things
// to have to explain afterwards.
//
// There are two answers here and they cost very different amounts. Whether one
// person can read one document is two indexed reads and it is the question an
// operator asks over and over, usually with a document id somebody has just
// pasted to them. How much of the corpus that person can reach is an aggregate
// over every document in the tenant, which is tens of milliseconds on a corpus
// of twenty thousand and grows with it. So the counts are asked for with
// counts=1 rather than computed every time, and the screen loads the answer it
// needs immediately and fetches the aggregate behind a button. See #149 for
// what it would take to make the aggregate cheap enough not to have to.
func (s *Server) handleAccess(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	q := r.URL.Query()
	subject := strings.TrimSpace(q.Get("subject"))
	if subject == "" {
		writeError(w, http.StatusBadRequest, "invalid", "name the subject to ask about")
		return
	}
	groups := splitCSV(q.Get("groups"))
	if len(groups) > GroupLimit {
		writeError(w, http.StatusBadRequest, "invalid", "that is more groups than a directory expansion produces")
		return
	}
	id := strings.TrimSpace(q.Get("id"))
	counts := q.Get("counts") == "1"

	// The tenant is the operator's own and is not a parameter. An administrator
	// of one tenant asking about a subject in another is a question this
	// deployment does not answer, and the way to be sure of that is not to read
	// a tenant from the request.
	about := &acl.Principal{
		Tenant:     p.Tenant,
		Subject:    subject,
		Kind:       acl.KindUser,
		Groups:     acl.GroupSet{Version: 1, Members: groups},
		Identities: identitiesOf(splitCSV(q.Get("identities"))),
	}

	s.log.Info("administration access check",
		"subject", p.Subject, "tenant", p.Tenant, "about", subject,
		"groups", len(about.Groups.Members), "document", id, "counts", counts)

	res := accessResponse{
		Subject:    about.Subject,
		Tenant:     about.Tenant,
		Groups:     about.Groups.Members,
		Identities: identityKeys(about.Identities),
		Sources:    []source{},
	}
	if res.Groups == nil {
		res.Groups = []string{}
	}

	// The counting is a capability, like the quarantine listing, because a
	// driver that ranks for itself may have no way to count a match set it did
	// not produce. See [store.Access].
	a, ok := s.store.(store.Access)
	res.Countable = ok
	if ok && counts {
		res.Counted = true
		reach, err := a.Reachable(r.Context(), about)
		if err != nil {
			s.log.Error("counting what a subject can reach",
				"tenant", p.Tenant, "about", subject, "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "the counts are not available")
			return
		}
		// The kinds come back with the sources and this screen does not draw
		// them. It asks whether somebody's access is the shape it should be,
		// which is a question about which connectors they can see into, and a
		// second breakdown of the same documents by document type answers a
		// question nobody opened this screen with.
		res.Sources = sourcesOf(reach.Sources)
		for _, one := range res.Sources {
			res.Documents += one.Documents
		}
	}

	if id != "" {
		access, err := s.documentAccess(r, about, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "that document could not be read")
			return
		}
		res.Document = access
	}

	h := w.Header()
	h.Set("Cache-Control", cacheControl)
	h.Set(varyHeader, varyValue)
	writeJSON(w, http.StatusOK, res)
}

// documentAccess answers the question about one document, as that person.
//
// It is the store's own read path with their principal rather than a second
// evaluation of the rule beside it, which is what makes the answer worth
// anything: it is the same call a search makes, so an answer of yes here and a
// document missing from their results is a bug in one place rather than a
// disagreement between two.
func (s *Server) documentAccess(r *http.Request, about *acl.Principal, id string) (*documentAccess, error) {
	out := &documentAccess{ID: id}
	d, err := s.store.Get(r.Context(), about, id)
	switch {
	case err == nil:
		decision := d.Permissions.Decide(about)
		out.Visible = decision.Allowed
		out.Rule = string(decision.Rule)
		out.Matched = matchedKey(decision)
	case errors.Is(err, genba.ErrNotFound):
		// Not there, not theirs, or held back, and the answer says none of the
		// three. See [documentAccess].
	default:
		return nil, err
	}
	return out, nil
}

// matchedKey is the reference that decided, in the form the rule compares it
// in, and is empty where nothing was matched.
func matchedKey(d acl.Decision) string {
	if d.Ref.Value == "" {
		return ""
	}
	if d.Rule == acl.RuleOwner {
		return d.Ref.UserKey()
	}
	return d.Ref.GroupKey()
}

// sourcesOf puts the counts on the wire, largest first so that the connector
// somebody has most of is the one they read first, and by name within a tie so
// that two readings of an unchanged corpus are the same list.
func sourcesOf(in []store.Facet) []source {
	out := make([]source, len(in))
	for i, f := range in {
		out[i] = source{Source: f.Value, Documents: f.Count}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Documents != out[j].Documents {
			return out[i].Documents > out[j].Documents
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// identitiesOf reads the source:value pairs a question names, dropping anything
// that is not one. A malformed identity is not an error, because the answer to
// a question with one is the same shape as the answer without it and a screen
// asking about somebody with a half typed identity should see the count fall
// rather than see a refusal.
func identitiesOf(raw []string) []acl.Identity {
	out := make([]acl.Identity, 0, len(raw))
	for _, one := range raw {
		source, value, ok := strings.Cut(one, ":")
		if !ok || source == "" || value == "" {
			continue
		}
		out = append(out, acl.Identity{Source: source, Value: value})
	}
	return out
}

// identityKeys echoes the identities back in the form they were given in.
func identityKeys(ids []acl.Identity) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, acl.Ref(id).UserKey())
	}
	return out
}
