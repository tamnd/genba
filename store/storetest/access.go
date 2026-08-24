package storetest

import (
	"maps"
	"slices"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// RunReachable checks a driver's [store.Access] against what the two screens
// built on it rely on: the one that answers "what can this person see", and the
// filter rail, which asks the same question of the person looking at it.
//
// Every case here is a way of counting the wrong documents while looking
// correct, and all of them matter more than an ordinary miscount because of
// what the numbers are used for: an operator compares one against what they
// expect somebody's access to be, and a count that is too high sends them
// looking for a leak that is not there while a count that is too low hides one
// that is.
//
// The case that is easy to skip is the quarantine. A held document is invisible
// to every principal, and a driver that counts with an aggregate rather than
// with the read path is exactly the sort of driver that forgets to say so.
func RunReachable(t *testing.T, newStore Factory) {
	t.Helper()
	for _, c := range reachableCases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })

			a, ok := s.(store.Access)
			if !ok {
				t.Skip("this driver does not implement store.Access")
			}
			c.run(t, s, a)
		})
	}
}

type reachableCase struct {
	name string
	run  func(t *testing.T, s store.Store, a store.Access)
}

var reachableCases = []reachableCase{
	{"only what the principal may read is counted", testReachableCounts},
	{"held documents are counted for nobody", testReachableQuarantine},
	{"another tenant's documents are not counted", testReachableTenant},
	{"a principal who may see nothing gets nothing", testReachableEmpty},
	{"the counts are per source", testReachableSources},
	{"the counts are per kind as well", testReachableKinds},
	{"the whole rule is applied in order", testReachableRuleOrder},
	{"the count is the read path's own answer", testReachableAgreesWithScan},
}

// reach collects the counts by source as a map, because the order is
// unspecified and a test that depended on it would pass on one driver and fail
// on the next.
func reach(t *testing.T, a store.Access, p *acl.Principal) map[string]int {
	t.Helper()
	got, err := a.Reachable(t.Context(), p)
	if err != nil {
		t.Fatalf("Reachable: %v", err)
	}
	return tally(t, "source", got.Sources)
}

// reachKinds is [reach] over the other grouping.
func reachKinds(t *testing.T, a store.Access, p *acl.Principal) map[string]int {
	t.Helper()
	got, err := a.Reachable(t.Context(), p)
	if err != nil {
		t.Fatalf("Reachable: %v", err)
	}
	return tally(t, "kind", got.Kinds)
}

func tally(t *testing.T, field string, in []store.Facet) map[string]int {
	t.Helper()
	out := make(map[string]int, len(in))
	for _, f := range in {
		if _, seen := out[f.Value]; seen {
			t.Fatalf("%s %q is counted twice, so the driver is grouping by something else", field, f.Value)
		}
		out[f.Value] = f.Count
	}
	return out
}

// sourced is a document from a named connector.
func sourced(id, source string, perm acl.Permissions) doc.Document {
	d := document(id, perm)
	d.Source = source
	return d
}

// kinded is a document of a stated type, in a source of its own so that a
// miscount cannot be hidden by the source grouping agreeing.
func kinded(id string, kind doc.Kind, perm acl.Permissions) doc.Document {
	d := document(id, perm)
	d.Kind = kind
	return d
}

func testReachableCounts(t *testing.T, s store.Store, a store.Access) {
	// Two the engineer may read and one belonging to a group they are not in.
	mustPut(t, s,
		document("d1", readable()),
		document("d2", readable()),
		document("d3", acl.Permissions{
			Mode:        acl.ModeACL,
			Source:      "gdrive",
			AllowGroups: []acl.Ref{{Source: "gdrive", Value: "sales@acme.com"}},
			Version:     1,
		}),
	)

	if got := reach(t, a, reader()); got["gdrive"] != 2 {
		t.Fatalf("the engineer reaches %v, want two documents in gdrive", got)
	}
	// The same corpus from the other side. A driver that counted the corpus and
	// filtered afterwards would give both of them three.
	if got := reach(t, a, stranger()); got["gdrive"] != 1 {
		t.Fatalf("the seller reaches %v, want one document in gdrive", got)
	}
}

func testReachableQuarantine(t *testing.T, s store.Store, a store.Access) {
	mustPut(t, s, document("d1", readable()), held("d2", "a deny aimed at everybody"))

	// The engineer is the person the servable document is for, so the only way
	// to reach two here is by counting the held one.
	if got := reach(t, a, reader()); got["gdrive"] != 1 {
		t.Fatalf("the engineer reaches %v, want one document: the held one is nobody's", got)
	}
}

func testReachableTenant(t *testing.T, s store.Store, a store.Access) {
	other := document("d2", acl.Permissions{Mode: acl.ModePublicToTenant, Source: "gdrive", Version: 1})
	other.Tenant = "other"
	mustPut(t, s, document("d1", readable()), other)

	// Public to the tenant means public to that tenant. A driver that reads the
	// mode without reading the tenant column counts the other one here, which
	// on a shared deployment is a leak reported as a number.
	if got := reach(t, a, reader()); got["gdrive"] != 1 {
		t.Fatalf("the engineer reaches %v, want one document from their own tenant", got)
	}
}

func testReachableEmpty(t *testing.T, s store.Store, a store.Access) {
	mustPut(t, s, document("d1", readable()))

	// Nobody's groups, which is what an operator typing a name with no
	// membership yet is asking about. Nothing, rather than an error and rather
	// than a source counted at zero.
	nobody := &acl.Principal{Tenant: "acme", Subject: "u_new"}
	if got := reach(t, a, nobody); len(got) != 0 {
		t.Fatalf("a principal in no groups reaches %v, want nothing at all", got)
	}
}

// ruleCorpus is one document per clause of the permission rule, named after the
// clause it exercises and put in a source of its own so that a miscount says
// which clause was got wrong rather than only that a total is out by one.
func ruleCorpus() []doc.Document {
	mei := acl.Ref{Source: "gdrive", Value: "mei@acme.com"}
	eng := acl.Ref{Source: "gdrive", Value: "eng@acme.com"}
	return []doc.Document{
		sourced("listed", "listed", readable()),
		sourced("tenant", "tenant", acl.Permissions{
			Mode: acl.ModePublicToTenant, Source: "gdrive", Version: 1,
		}),
		sourced("owner", "owner", acl.Permissions{
			Mode: acl.ModeOwnerOnly, Source: "gdrive", Owner: mei, Version: 1,
		}),
		sourced("stranger-owner", "stranger-owner", acl.Permissions{
			Mode: acl.ModeOwnerOnly, Source: "gdrive",
			Owner:   acl.Ref{Source: "gdrive", Value: "kenji@acme.com"},
			Version: 1,
		}),
		// A deny beats the group that would otherwise admit them.
		sourced("denied-user", "denied-user", acl.Permissions{
			Mode: acl.ModeACL, Source: "gdrive",
			AllowGroups: []acl.Ref{eng}, DenyUsers: []acl.Ref{mei}, Version: 1,
		}),
		sourced("denied-group", "denied-group", acl.Permissions{
			Mode: acl.ModeACL, Source: "gdrive",
			AllowGroups: []acl.Ref{eng}, DenyGroups: []acl.Ref{eng}, Version: 1,
		}),
		// A deny beats ownership too, which is the clause a driver that checks
		// the owner column first gets wrong.
		sourced("denied-owner", "denied-owner", acl.Permissions{
			Mode: acl.ModeOwnerOnly, Source: "gdrive",
			Owner: mei, DenyUsers: []acl.Ref{mei}, Version: 1,
		}),
		// A list nobody is on, which is the ordinary refusal and has to stay
		// distinct from a deny.
		sourced("not-listed", "not-listed", acl.Permissions{
			Mode: acl.ModeACL, Source: "gdrive",
			AllowGroups: []acl.Ref{{Source: "gdrive", Value: "legal@acme.com"}}, Version: 1,
		}),
	}
}

// testReachableRuleOrder walks the permission rule clause by clause.
//
// A driver is free to count with an aggregate instead of with its read path,
// and the ones that can are the ones that will, because that is the difference
// between a query and a walk over the whole corpus. What that buys in speed it
// costs in having the rule written down twice, and this is the case that keeps
// the second copy honest: every clause of it, in the order that decides which
// one wins.
func testReachableRuleOrder(t *testing.T, s store.Store, a store.Access) {
	mustPut(t, s, ruleCorpus()...)

	got := reach(t, a, reader())
	want := map[string]int{"listed": 1, "tenant": 1, "owner": 1}
	if !maps.Equal(got, want) {
		t.Fatalf("the engineer reaches %v, want %v", got, want)
	}
}

// testReachableAgreesWithScan is the assertion underneath all of the others.
//
// The count is worth something only because it is the same answer the read path
// gives, so an operator reading it can tell somebody what they can see. Here
// both answers are taken over the same corpus and compared, which catches a
// driver whose aggregate is self consistent and wrong.
func testReachableAgreesWithScan(t *testing.T, s store.Store, a store.Access) {
	corpus := append(ruleCorpus(), held("held", "a deny aimed at everybody"))
	mustPut(t, s, corpus...)

	for _, p := range []*acl.Principal{reader(), stranger()} {
		sources, kinds := map[string]int{}, map[string]int{}
		if err := s.Scan(t.Context(), p, func(d doc.Document) bool {
			sources[d.Source]++
			kinds[string(d.Kind)]++
			return true
		}); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if counted := reach(t, a, p); !maps.Equal(counted, sources) {
			t.Errorf("%s is counted %v by source and reads %v, so the two rules disagree", p.Subject, counted, sources)
		}
		// Both groupings, because a driver that counts one of them from the
		// rule and the other from something else would pass on the first alone.
		if counted := reachKinds(t, a, p); !maps.Equal(counted, kinds) {
			t.Errorf("%s is counted %v by kind and reads %v, so the two rules disagree", p.Subject, counted, kinds)
		}
	}
}

// testReachableKinds is the grouping the filter rail draws its verticals from.
//
// The corpus is deliberately lopsided against the source grouping: every
// document is in the same source, so a driver that counted by source and
// labelled the answer kind gets one row of three here instead of three rows.
func testReachableKinds(t *testing.T, s store.Store, a store.Access) {
	mustPut(t, s,
		kinded("d1", doc.KindPage, readable()),
		kinded("d2", doc.KindMessage, readable()),
		kinded("d3", doc.KindMessage, readable()),
		// Not readable by the engineer, so a kind nobody else carries has to be
		// missing rather than present at zero.
		kinded("d4", doc.KindTicket, acl.Permissions{
			Mode:        acl.ModeACL,
			Source:      "gdrive",
			AllowGroups: []acl.Ref{{Source: "gdrive", Value: "sales@acme.com"}},
			Version:     1,
		}),
	)

	got := reachKinds(t, a, reader())
	kinds := slices.Sorted(maps.Keys(got))
	if !slices.Equal(kinds, []string{"message", "page"}) {
		t.Fatalf("the kinds are %v, want message and page", kinds)
	}
	if got["page"] != 1 || got["message"] != 2 {
		t.Fatalf("the counts are %v, want one page and two messages", got)
	}
}

func testReachableSources(t *testing.T, s store.Store, a store.Access) {
	mustPut(t, s,
		sourced("d1", "gdrive", readable()),
		sourced("d2", "slack", readable()),
		sourced("d3", "slack", readable()),
	)

	got := reach(t, a, reader())
	sources := slices.Sorted(maps.Keys(got))
	if !slices.Equal(sources, []string{"gdrive", "slack"}) {
		t.Fatalf("the sources are %v, want gdrive and slack", sources)
	}
	if got["gdrive"] != 1 || got["slack"] != 2 {
		t.Fatalf("the counts are %v, want one in gdrive and two in slack", got)
	}
}
