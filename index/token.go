package index

import "github.com/tamnd/genba/doc"

// Tokenize splits text into lowercased terms. It is [doc.Tokenize], kept here
// because ranking code reads better without the package qualifier on every
// line, and because the analyzer belonging to the document model rather than to
// the ranking is a fact about the layering, not something a caller has to know.
func Tokenize(text string) []string { return doc.Tokenize(text) }

// stopwords are dropped from a query but not from a document, so a search for
// "the deploy runbook" is not diluted while a phrase in the body stays intact.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "but": true, "by": true, "for": true, "from": true, "how": true,
	"in": true, "is": true, "it": true, "of": true, "on": true, "or": true,
	"that": true, "the": true, "this": true, "to": true, "was": true,
	"what": true, "when": true, "where": true, "which": true, "with": true,
}

// queryTerms tokenizes a query and drops stopwords, unless the query is nothing
// but stopwords, in which case they are all we have to work with.
func queryTerms(q string) []string {
	all := Tokenize(q)
	kept := make([]string, 0, len(all))
	for _, t := range all {
		if !stopwords[t] {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return all
	}
	return dedupe(kept)
}

// dedupe removes repeated terms, keeping the first occurrence. A term counted
// twice in a query would be scored twice, which turns "test test" into a
// different ranking from "test" for no reason a person would expect.
func dedupe(terms []string) []string {
	seen := make(map[string]bool, len(terms))
	out := terms[:0]
	for _, t := range terms {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
