package api

import (
	"context"
	"net/http"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/recheck"
)

// WithRecheck asks the sources whether somebody may still read what is about to
// be shown to them.
//
// It is off unless a caller passes one, and a set with no checkers in it changes
// nothing either, so turning this on is a decision made per source by the
// deployment that knows whether that source can answer a permission question in
// a few milliseconds. See the recheck package for what the check costs and what
// happens when it does not come back.
//
// Every surface that puts a document in front of somebody runs it. That is the
// same set of handlers the audit trail covers, and for the same reason: a rule
// that holds on the search page and not on the preview panel is not a rule.
func WithRecheck(set *recheck.Set) Option {
	return func(s *Server) { s.recheck = set }
}

// keeper answers whether one document survived the recheck.
type keeper func(id string) bool

// keepAll is the answer on a deployment that has not turned any of this on, and
// it is the answer this file is careful to reach quickly: no lock, no clock, no
// allocation.
func keepAll(string) bool { return true }

// keep asks about a whole page at once.
//
// One call per response rather than one per row, because the sources are asked
// in parallel under a single deadline and a handler that called this twice would
// spend two of them.
func (s *Server) keep(ctx context.Context, p *acl.Principal, items []recheck.Item) keeper {
	if s.recheck == nil || len(items) == 0 {
		return keepAll
	}
	allowed := s.recheck.Allowed(ctx, p, items)
	return func(id string) bool { return allowed[id] }
}

// stillReadable is the check for a surface that serves one document.
//
// False means the row goes, and what the caller does with that is answer the way
// it answers for a document that is not there. The source has just said this
// person may not read it, and the response that says so in as few words as
// possible is the one every other refusal on this surface already uses.
func (s *Server) stillReadable(r *http.Request, p *acl.Principal, d doc.Document) bool {
	if s.recheck == nil {
		return true
	}
	return s.keep(r.Context(), p, []recheck.Item{{ID: d.ID, Source: d.Source}})(d.ID)
}

// stillMatching is the check for a page of results.
func (s *Server) stillMatching(r *http.Request, p *acl.Principal, hits []index.Result) []index.Result {
	if s.recheck == nil || len(hits) == 0 {
		return hits
	}
	items := make([]recheck.Item, 0, len(hits))
	for _, h := range hits {
		items = append(items, recheck.Item{ID: h.Document.ID, Source: h.Document.Source})
	}
	keep := s.keep(r.Context(), p, items)
	out := make([]index.Result, 0, len(hits))
	for _, h := range hits {
		if keep(h.Document.ID) {
			out = append(out, h)
		}
	}
	return out
}

// onlyQuoting drops the quotes of documents that are not on the page.
//
// The written answer is built from the results, so a row removed from the page
// takes its quote with it. This is not an optimisation and it is not defensive
// tidying: a quote is a sentence out of the document, and an answer that kept
// quoting a document the source has just withdrawn would be serving the content
// of it under a different field name.
func onlyQuoting(a index.Answer, hits []index.Result) index.Answer {
	if len(a.Quotes) == 0 {
		return a
	}
	on := make(map[string]bool, len(hits))
	for _, h := range hits {
		on[h.Document.ID] = true
	}
	kept := make([]index.Quote, 0, len(a.Quotes))
	for _, q := range a.Quotes {
		if on[q.ID] {
			kept = append(kept, q)
		}
	}
	a.Quotes = kept
	return a
}

// stillVisible is the check for a list of documents.
func (s *Server) stillVisible(r *http.Request, p *acl.Principal, docs []doc.Document) []doc.Document {
	if s.recheck == nil || len(docs) == 0 {
		return docs
	}
	keep := s.keep(r.Context(), p, recheckItems(docs))
	out := make([]doc.Document, 0, len(docs))
	for _, d := range docs {
		if keep(d.ID) {
			out = append(out, d)
		}
	}
	return out
}

// recheckItems is what a checker is given: an id and a source, and none of the
// rest of the document. See [recheck.Item] for why that is the whole of it.
func recheckItems(docs []doc.Document) []recheck.Item {
	out := make([]recheck.Item, 0, len(docs))
	for _, d := range docs {
		out = append(out, recheck.Item{ID: d.ID, Source: d.Source})
	}
	return out
}
