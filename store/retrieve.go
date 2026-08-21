package store

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
)

// Retriever is the optional capability of a driver that can find the documents
// matching a query out of its own index, rather than handing every document it
// holds to the caller and letting the caller throw most of them away.
//
// It is optional because [Store] is the contract and this is an optimisation:
// a driver that does not implement it still works, and the caller falls back to
// [Store.Scan]. It is worth having as a capability rather than as a method on
// Store because the two have very different costs to implement. Scan is a loop.
// Retrieve is an inverted index.
//
// The permission rule does not move. A driver implementing Retrieve applies the
// principal inside the same query that applies the terms, which is the point:
// the candidate set never contains a document the asker may not read, so no
// count, facet or snippet computed from it can leak one.
type Retriever interface {
	// Retrieve calls fn for every document the principal may read that matches
	// the request, and stops early if fn returns false. The order is
	// unspecified, because ranking happens above this and a driver that sorted
	// would be doing work the caller discards.
	//
	// The match set is exactly the set [Request.Matches] describes, and
	// store/storetest checks that it is, against a driver's own Scan. A driver
	// whose index disagrees with that definition returns a different result for
	// the same query than a driver that does not implement this at all, which
	// is the kind of difference nobody finds until it is in production.
	Retrieve(ctx context.Context, p *acl.Principal, r Request, fn func(doc.Document) bool) error
}

// Request is a match set: the terms and the filters, with no ranking, no paging
// and no ordering in it.
//
// Every field narrows. Values within one field are alternatives, values across
// fields all have to hold. That is what a facet sidebar means when somebody
// ticks two sources and one document type, and matching it here is what makes a
// typed operator and a ticked box the same query.
type Request struct {
	// Terms are the analysed query terms. A document matches when it contains
	// at least one of them. An empty list matches every document, which is how
	// a filter only query browses.
	Terms []string

	// Sources restricts to the connectors that produced the documents.
	Sources []string

	// Kinds restricts to document kinds.
	Kinds []doc.Kind

	// Containers restricts to the folder, space, channel or repository a
	// document lives in.
	Containers []string

	// Authors and Owners restrict by person. A value matches the subject, the
	// email address, the local part of the email address or the display name,
	// case insensitively, which is what somebody typing from:mika means.
	Authors []string
	Owners  []string

	// Since and Until bound the modification time. A zero value is unbounded.
	Since time.Time
	Until time.Time
}

// FacetFields are the fields a sidebar is drawn from, in the order it shows
// them.
//
// They are the four request fields somebody ticks rather than types, which is
// why they are named here and the dates and the owner are not: a facet count is
// a claim about what ticking a box would do, and nothing draws a box for a date
// range. Both drivers and the Go fallback count exactly these, so the sidebar
// does not depend on which driver answered.
var FacetFields = []string{"source", "kind", "container", "author"}

// Constrains reports whether the request restricts the given facet field.
func (r Request) Constrains(field string) bool {
	switch field {
	case "source":
		return len(r.Sources) > 0
	case "kind":
		return len(r.Kinds) > 0
	case "container":
		return len(r.Containers) > 0
	case "author":
		return len(r.Authors) > 0
	}
	return false
}

// Without is the request with one facet field's own constraint lifted, and
// every other constraint left exactly where it was.
//
// This is what a facet count is counted over, and it is the whole of why a
// sidebar is worth reading. Counted over the request as it stands, a ticked
// type reports its own count and zero for every other type, which destroys the
// one thing somebody needs in order to decide whether to untick it. Counted
// with its own field lifted, each value answers the question the box is asking:
// how many results there would be if this were what you had chosen.
//
// The other fields stay applied, because they are different questions. How many
// pages there are is a useful answer; how many pages there are with every filter
// in the query ignored is a fact about the corpus rather than about the search.
func (r Request) Without(field string) Request {
	switch field {
	case "source":
		r.Sources = nil
	case "kind":
		r.Kinds = nil
	case "container":
		r.Containers = nil
	case "author":
		r.Authors = nil
	}
	return r
}

// WithoutFacets is the request with all four facet constraints lifted, which is
// the widest set any facet count is counted over.
func (r Request) WithoutFacets() Request {
	for _, field := range FacetFields {
		r = r.Without(field)
	}
	return r
}

// Empty reports whether the request narrows anything at all.
func (r Request) Empty() bool {
	return len(r.Terms) == 0 && len(r.Sources) == 0 && len(r.Kinds) == 0 &&
		len(r.Containers) == 0 && len(r.Authors) == 0 && len(r.Owners) == 0 &&
		r.Since.IsZero() && r.Until.IsZero()
}

// Matches is the definition of the match set, in Go.
//
// It is the reference a driver's index is checked against and the
// implementation a driver without an index uses, so a change here is a change
// to every driver at once. That is the intent: the alternative is a definition
// per driver, and the way those differ is that one deployment finds a document
// another does not.
func (r Request) Matches(d doc.Document) bool {
	return r.Filters(d) && r.MatchesTerms(d)
}

// Filters is [Request.Matches] without the terms, for a caller that is about to
// analyse the document anyway and would otherwise tokenize it twice.
func (r Request) Filters(d doc.Document) bool {
	if len(r.Sources) > 0 && !slices.Contains(r.Sources, d.Source) {
		return false
	}
	if len(r.Kinds) > 0 && !slices.Contains(r.Kinds, d.Kind) {
		return false
	}
	if len(r.Containers) > 0 && !containsFold(r.Containers, d.Container) {
		return false
	}
	if len(r.Authors) > 0 && !matchesPerson(r.Authors, d.Author) {
		return false
	}
	if len(r.Owners) > 0 && !matchesPerson(r.Owners, d.Owner) {
		return false
	}
	if !r.Since.IsZero() && d.ModifiedAt.Before(r.Since) {
		return false
	}
	if !r.Until.IsZero() && d.ModifiedAt.After(r.Until) {
		return false
	}
	return true
}

// Passes reports whether the document satisfies one facet field's constraint,
// ignoring every other part of the request.
//
// It is [Request.Filters] taken apart, and it is here so that a caller counting
// facets in Go can ask which single constraint a document failed. A document
// that fails exactly one of them is what a drill down count is made of: it is
// not in the match set, and it is in the match set that ticking that value
// instead would produce. A field the request does not constrain is passed by
// everything.
func (r Request) Passes(field string, d doc.Document) bool {
	switch field {
	case "source":
		return len(r.Sources) == 0 || slices.Contains(r.Sources, d.Source)
	case "kind":
		return len(r.Kinds) == 0 || slices.Contains(r.Kinds, d.Kind)
	case "container":
		return len(r.Containers) == 0 || containsFold(r.Containers, d.Container)
	case "author":
		return len(r.Authors) == 0 || matchesPerson(r.Authors, d.Author)
	}
	return true
}

// MatchesTerms reports whether the document carries at least one query term. A
// request with no terms matches every document.
func (r Request) MatchesTerms(d doc.Document) bool {
	if len(r.Terms) == 0 {
		return true
	}
	for _, t := range d.Terms() {
		if slices.Contains(r.Terms, t) {
			return true
		}
	}
	return false
}

// Fold is the case folding every text filter compares through.
//
// A driver that stores a folded column and a caller that folds a filter value
// have to fold the same way, or the two disagree on exactly the inputs nobody
// tests. Exporting the one function they both call is what stops that, and it
// is why the comparison is a fold to lower case rather than a call to
// [strings.EqualFold]: EqualFold is a relation, and a relation cannot be put in
// a column and indexed.
func Fold(s string) string { return strings.ToLower(s) }

// PersonKeys returns the folded strings a from: or owner: value is compared
// against.
//
// Somebody typing a name has one of these in mind and no idea which of them the
// connector managed to resolve, so all of them are tried. A driver stores this
// list next to the document and compares against it directly, which is the same
// comparison [Request.Filters] makes in Go.
func PersonKeys(p doc.Person) []string {
	candidates := []string{p.Subject, p.Email, p.Name, p.Identity.Value}
	if local, _, ok := strings.Cut(p.Email, "@"); ok {
		candidates = append(candidates, local)
	}
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if k := Fold(c); !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

func matchesPerson(values []string, p doc.Person) bool {
	keys := PersonKeys(p)
	for _, v := range values {
		if v != "" && slices.Contains(keys, Fold(v)) {
			return true
		}
	}
	return false
}

func containsFold(haystack []string, needle string) bool {
	if needle == "" {
		return false
	}
	folded := Fold(needle)
	for _, s := range haystack {
		if s != "" && Fold(s) == folded {
			return true
		}
	}
	return false
}
