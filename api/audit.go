package api

import (
	"net/http"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/audit"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
)

// WithAudit sets where the record of every content access is written.
//
// There is no option that switches auditing off, and there is not going to be
// one. A server built without this one still writes its records, to the process
// log, because a deployment that can quietly stop recording who read what is a
// deployment whose audit trail is worth nothing: the answer to "was this on at
// the time" has to be yes by construction rather than by a configuration file
// somebody would have to go and read.
//
// What a deployment chooses here is where the records go and how long they are
// kept. See [audit.Open] for the file sink and docs/audit.md for the shape of
// what it writes.
func WithAudit(a *audit.Log) Option {
	return func(s *Server) {
		if a != nil {
			s.audit = a
		}
	}
}

// Close releases what the server owns, which is the audit log it built for
// itself and nothing else.
//
// A store, a searcher and an audit log that were passed in belong to whoever
// passed them, and a server that closed them would be shutting down half of
// somebody else's process. An embedder that never calls this loses at most the
// records still queued when the process exits.
func (s *Server) Close() error {
	if !s.ownAudit {
		return nil
	}
	return s.audit.Close()
}

// accessed writes one record for a request that has been answered.
//
// The caller fills in what happened and this fills in who and where, so that a
// handler cannot get the identity fields subtly wrong and no handler has to
// know how a principal maps onto a record.
func (s *Server) accessed(r *http.Request, p *acl.Principal, rec audit.Record) {
	if p != nil {
		rec.Tenant, rec.Subject, rec.Kind = p.Tenant, p.Subject, kindOf(p.Kind)
	}
	rec.Surface = surfaceOf(r)
	s.audit.Write(rec)
}

// kindOf is the principal kind as the record spells it.
//
// It is written out here rather than taken from a String method, because these
// strings are filtered on by whatever reads an export and they must not change
// when somebody renames a constant.
func kindOf(k acl.Kind) string {
	switch k {
	case acl.KindService:
		return "service"
	case acl.KindAgent:
		return "agent"
	default:
		return "user"
	}
}

// surfaceOf is the route the access came through.
//
// The registered pattern rather than the path, because a path carries document
// ids and they are already on the record in a field something can filter on. A
// handler called directly, which is a test rather than a deployment, has no
// pattern and gets the path.
func surfaceOf(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.Method + " " + r.URL.Path
}

// item is one document on a record: an identifier and where it came from, never
// a title.
func item(d doc.Document) audit.Item {
	return audit.Item{ID: d.ID, Source: d.Source}
}

// items is the same for a list of documents.
func items(docs []doc.Document) []audit.Item {
	out := make([]audit.Item, 0, len(docs))
	for _, d := range docs {
		out = append(out, item(d))
	}
	return out
}

// hitItems is the same for a page of results.
func hitItems(hits []index.Result) []audit.Item {
	out := make([]audit.Item, 0, len(hits))
	for _, h := range hits {
		out = append(out, item(h.Document))
	}
	return out
}

// ruleOf is the clause of the access control list that admitted somebody to one
// document.
//
// Only the clause. The reference it matched is deliberately dropped, because
// that is a group name and a trail of access decisions that named groups would
// describe the shape of an organisation to everybody who can read the trail.
func ruleOf(p *acl.Principal, d doc.Document) string {
	return string(d.Permissions.Decide(p).Rule)
}
