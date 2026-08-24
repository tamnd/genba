// Package connectortest is the conformance suite every connector has to pass.
//
// A connector is where the risk in this system concentrates. It is the piece
// that is written once per source system, usually by whoever needs that source,
// and it is the piece that decides what gets indexed and who is allowed to read
// it. Nothing above it can tell a connector that lost half a corpus from a
// corpus that is half the size, or a connector that never asked about
// permissions from a source where everything really is readable by everybody.
// This suite is how that gets told apart.
//
// The rules here are the contract in [connector] written down as tests, which
// makes the suite rather than the interface the definition of a connector: the
// interface says what compiles, and this says what works.
//
// # What is optional and what is not
//
// A connector is not required to be able to do everything. Listing a source,
// fetching one document by id, reporting a deletion, reporting a permission
// change without the content, and counting what a sync spent are all optional,
// and a fixture that leaves the matching hook nil skips those cases rather than
// failing them. What is not optional is that everything a connector does claim
// to do is right, and the suite is deliberately harder on a connector that
// claims more.
//
// The one rule with no way out is permissions. Every document a connector emits
// has to say where its access control list came from, and a connector that
// could not work one out says so with [connector.Unresolved] rather than
// leaving the field empty. A document that arrives without that is a document
// nobody can say who may read, and it fails the suite.
//
// Usage from a connector's own test file:
//
//	func TestConformance(t *testing.T) {
//		connectortest.Run(t, func(t *testing.T) connectortest.Fixture {
//			dir := t.TempDir()
//			src, err := fssource.New(dir, "files", fssource.PublicToTenant("files"))
//			if err != nil {
//				t.Fatal(err)
//			}
//			return connectortest.Fixture{
//				Connector: src,
//				ID:        func(name string) string { return "files:" + name },
//				Write: func(t *testing.T, name, body string) {
//					if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
//						t.Fatal(err)
//					}
//				},
//				Remove: func(t *testing.T, name string) {
//					if err := os.Remove(filepath.Join(dir, name)); err != nil {
//						t.Fatal(err)
//					}
//				},
//			}
//		})
//	}
package connectortest

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
)

// Fixture is one connector and the handful of things a test has to be able to
// do to the system behind it.
//
// The suite cannot write a file, put an object or post a message, and it should
// not know which of those it is doing. A fixture is the adapter that turns "put
// a document called a.md in the source" into whatever that means for one
// connector, and it is the only part of running the suite that has to be
// written per source.
type Fixture struct {
	// Connector is the connector under test, over a source that holds nothing
	// yet. It is closed for you when the case ends.
	Connector connector.Connector

	// ID is the document id the connector will mint for a name that was handed
	// to Write. It is spelled out by the fixture rather than worked out by the
	// suite because the shape of an id is the connector's business.
	ID func(name string) string

	// Write puts a document in the source, replacing one of the same name.
	//
	// It has to leave the source in a state a sync can settle on. A source that
	// keeps time to the second needs its clock moved on afterwards, or a sync
	// taken immediately after the write records a cursor the write is not yet
	// behind, and every later sync reads it again. Fixtures for such sources
	// tick that clock here.
	Write func(t *testing.T, name, body string)

	// Remove deletes a document from the source. Leave it nil for a source
	// nothing is ever removed from, which skips the deletion cases.
	Remove func(t *testing.T, name string)

	// Share changes who may read one document, without touching its content.
	//
	// It is what the permission change case is built on, and leaving it nil
	// skips that case. A connector that cannot report a permission change
	// without recrawling the document is allowed, and it pays for it on every
	// access control edit at the source.
	Share func(t *testing.T, name string)

	// Unresolvable puts one document's access control list beyond working out,
	// by pointing it at a group that does not resolve or a rule that cannot be
	// read. Leave it nil for a source where that cannot happen.
	//
	// This is the hook behind the rule that matters most: what the suite checks
	// is that such a document is quarantined and says so, rather than being
	// indexed with a guess or dropped without a word.
	Unresolvable func(t *testing.T, name string)
}

// Factory returns a fresh fixture over an empty source for one test case.
type Factory func(t *testing.T) Fixture

// Run executes the suite against a connector.
func Run(t *testing.T, newFixture Factory) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			switch {
			case f.Connector == nil:
				t.Fatal("the fixture has no connector in it")
			case f.ID == nil:
				t.Fatal("the fixture does not say what a document id looks like")
			case f.Write == nil:
				t.Fatal("the fixture cannot put a document in the source, and every case starts by doing that")
			}
			t.Cleanup(func() { _ = f.Connector.Close() })
			c.run(t, f)
		})
	}
}

type testCase struct {
	name string
	run  func(t *testing.T, f Fixture)
}

var cases = []testCase{
	{"the source names itself", testSourceName},
	{"a full sync finds everything the source holds", testFullSync},
	{"a full sync run twice says the same thing", testFullSyncRepeats},
	{"every document arrives with permissions", testPermissions},
	{"a document nobody can resolve is quarantined and says so", testUnresolved},
	{"a sync resumes from a cursor without losing what came after it", testResume},
	{"an error from emit stops the sync and comes back unchanged", testEmitError},
	{"a cancelled sync stops", testCancel},
	{"a second sync of an unchanged source reads nothing", testIncremental},
	{"a document written after a sync is found by the next one", testNewDocument},
	{"a deleted document stops being part of the source", testDeleted},
	{"a permission change arrives without the content", testPermissionChange},
	{"a permission change leaves a document's own rule alone", testPermissionChangeKeepsOverrides},
	{"enumerate lists what a sync found", testEnumerate},
	{"enumerate stops when the callback says so", testEnumerateEarlyStop},
	{"fetch returns the document a sync would have", testFetch},
	{"fetch of something the source does not have is not a failure", testFetchGone},
	{"the counters say what a sync spent", testCounters},
	{"close is repeatable", testClose},
}

// write puts a document in the source.
func write(t *testing.T, f Fixture, name, body string) {
	t.Helper()
	f.Write(t, name, body)
}

// syncFrom runs one sync and returns every change it emitted, having checked
// each one on the way past.
//
// Every case goes through here, so the well formedness rules are applied to
// every change the suite ever sees rather than only to the ones a case thought
// to look at.
func syncFrom(t *testing.T, f Fixture, from connector.Cursor) ([]connector.Change, connector.Cursor) {
	t.Helper()
	var got []connector.Change
	next, err := f.Connector.Sync(t.Context(), from, func(_ context.Context, ch connector.Change) error {
		verify(t, f, ch)
		got = append(got, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return got, next
}

// verify fails the case for a change that breaks a rule every connector has to
// keep.
func verify(t *testing.T, f Fixture, ch connector.Change) {
	t.Helper()
	for _, p := range problems(ch, f.Connector.Source()) {
		t.Error(p)
	}
}

// ids is the document id of every change, in the order they were emitted.
func ids(changes []connector.Change) []string {
	out := make([]string, 0, len(changes))
	for _, ch := range changes {
		out = append(out, ch.Document.ID)
	}
	return out
}

// sorted is the ids of a set of changes, in an order two of them can be
// compared in. The order a connector emits in is its own business.
func sorted(changes []connector.Change) []string {
	out := ids(changes)
	slices.Sort(out)
	return out
}

// find returns the change for one document, or nil if there was none.
func find(changes []connector.Change, id string) *connector.Change {
	for i := range changes {
		if changes[i].Document.ID == id {
			return &changes[i]
		}
	}
	return nil
}

// enumerate lists the source, and fails the case if the walk does not finish.
func enumerate(t *testing.T, e connector.Enumerator) []string {
	t.Helper()
	var out []string
	if err := e.Enumerate(t.Context(), func(it connector.Item) bool {
		if it.ID == "" {
			t.Error("the enumeration listed an item with no id, which cannot be compared against anything")
		}
		out = append(out, it.ID)
		return true
	}); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	slices.Sort(out)
	return out
}

// enumerator returns the connector's listing capability, or skips the case.
//
// The capability is optional, so a connector without it is not failing the
// suite. A connector with it does not get to opt out of the rules about it.
func enumerator(t *testing.T, f Fixture) connector.Enumerator {
	t.Helper()
	e, ok := f.Connector.(connector.Enumerator)
	if !ok {
		t.Skip("the connector does not list its source")
	}
	return e
}

// fetcher returns the connector's single document read, or skips the case.
func fetcher(t *testing.T, f Fixture) connector.Fetcher {
	t.Helper()
	c, ok := f.Connector.(connector.Fetcher)
	if !ok {
		t.Skip("the connector cannot read one document by id")
	}
	return c
}

// counted returns the connector's counters, or skips the case.
func counted(t *testing.T, f Fixture) connector.Counted {
	t.Helper()
	c, ok := f.Connector.(connector.Counted)
	if !ok {
		t.Skip("the connector does not report what it spent")
	}
	return c
}

// mentions reports whether a document's body has a word in it.
//
// The suite compares on a marker rather than on the whole body because a
// connector is allowed to do work on the way past. A source that stores
// markdown and indexes the text of it is not returning the bytes that went in,
// and that is the connector doing its job.
func mentions(body, marker string) bool {
	return strings.Contains(body, marker)
}

// samePermissions reports whether two descriptors say the same thing.
func samePermissions(a, b acl.Permissions) bool {
	return a.Mode == b.Mode &&
		a.Source == b.Source &&
		a.Version == b.Version &&
		a.Sharing == b.Sharing &&
		a.Owner == b.Owner &&
		slices.Equal(a.AllowUsers, b.AllowUsers) &&
		slices.Equal(a.AllowGroups, b.AllowGroups) &&
		slices.Equal(a.DenyUsers, b.DenyUsers) &&
		slices.Equal(a.DenyGroups, b.DenyGroups)
}
