package genba_test

import (
	"errors"
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/tamnd/genba"

// allowed lists, for each package of this module, the packages of this module it
// may import. A package that is not listed here has no business importing
// anything from inside the module, and a package missing from the map entirely
// is a new package whose place in the layering nobody has decided yet.
//
// The point is not tidiness. The permission model sits at the bottom, the
// storage drivers apply it, and everything above them gets documents that have
// already been filtered. An edge that skips a layer is how a component ends up
// holding a document it was never supposed to see.
var allowed = map[string][]string{
	"":    nil,
	"acl": nil,
	"doc": {"acl"},

	// directory sits under the permission model rather than beside it. It
	// resolves a person into groups and hands acl the strings it compares, so
	// it imports acl for the identity and group set types that are the shape of
	// its answer, and cache for the one layer that holds those answers. It has
	// never heard of a document, a store or a query, and it must not: a
	// directory that could see what it was resolving somebody for is a
	// directory that could be asked leading questions.
	"directory":               {"acl", "cache"},
	"directory/directorytest": {"acl", "directory"},

	// An adapter is the one place in the tree that talks to somebody else's
	// service, so it gets the transport that knows how to be refused politely
	// on top of what directory itself is allowed. It is still under acl rather
	// than beside it: an adapter answers two lookups and decides nothing.
	"directory/okta": {"acl", "cache", "connector/limit", "directory"},

	// The same place in the layering, for the same reasons.
	"directory/entra": {"acl", "cache", "connector/limit", "directory"},

	// And again. This one signs its own grant, so it reaches for crypto and
	// nothing of ours that the other two do not already have.
	"directory/google": {"acl", "cache", "connector/limit", "directory"},

	// provider sits above all three, which is the only place in the tree that
	// does. It exists so that a deployment can name one in a file instead of
	// importing it, so it has to know all of them and nothing else may know it
	// except the command that reads the flag.
	"directory/provider": {"directory", "directory/entra", "directory/google", "directory/okta"},

	// cache is a map with a lock on it and imports nothing of ours, which is
	// what lets index depend on it without the dependency meaning anything. A
	// cache that knew about principals would be a second copy of the permission
	// model in the package least equipped to hold one.
	"cache": nil,

	// metric is the same shape as cache: a lock over some numbers, importing
	// nothing of ours. A metrics package that knew about principals or
	// documents would be a place for either to leak into an endpoint that is
	// deliberately not behind the permission check.
	"metric": nil,

	"store": {"acl", "doc"},

	// segment is the on disk format and imports nothing of ours, not even doc.
	// A section is a run of bytes with a kind on it, and the encodings inside
	// the sections belong to the packages that own them. A container that knew
	// what a document was would be a format that has to change every time a
	// document does, which is the opposite of what a file format is for.
	"store/segment": nil,

	// column is one of those encodings, and it imports nothing of ours either,
	// not even segment. It deals in rows and codes and has never heard of a
	// document or a principal. That is what lets the permission filter be an
	// intersection between two bitmaps: a column package that knew what an ACL
	// was would be a second place for the permission model to live, in the
	// package least equipped to hold one.
	"store/column": nil,

	// vector is the embedding section and the scan over it, and the one thing it
	// imports of ours is column, for the bitmap. That edge is the point rather
	// than a convenience: the rows a principal may read arrive as a bitmap and
	// the scan walks it, so a vector search cannot score a document it was not
	// handed. Giving this package its own set type would mean converting between
	// two of them on the hot path, and giving it any knowledge of an ACL would
	// put the permission model in a second place again.
	"store/vector": {"store/column"},

	// graph is the entities and relationships of a segment, and it imports
	// column for two reasons that are both the point. The entity keys and the
	// edge kinds are columns, so looking an entity up by name is the same
	// sorted dictionary search a source filter is. And the rows a principal may
	// read arrive as the same bitmap the document scans use, which is what makes
	// an entity's visibility derived from the documents that mention it rather
	// than stored beside it. A second permission model here would be a second
	// thing to keep in step with the first, and the failure when they drift is a
	// name shown to somebody who was never meant to learn it.
	//
	// There is deliberately no edge to doc or connector. Extraction is a
	// connector's job and this package has no opinion about how it is done, so
	// it holds no extractor and defines no interface for one either.
	"store/graph": {"store/column"},

	// segdir owns which segments are on disk and which of them a reader can
	// see, so it needs the segment container to read a sequence number out of a
	// header and to check that a published file parses. It needs nothing else.
	// It does not know what a document is, it does not know what a query is,
	// and a directory of segments is the same problem whatever the segments
	// turn out to hold.
	"store/segdir": {"store/segment"},

	// kura is the Go side of the Rust engine's C ABI and imports nothing of
	// ours. It deals in document ids, bytes and vectors, and it is the one
	// package in the repository that is not in the default build at all. An
	// edge from it to store would put a cgo dependency underneath the interface
	// every driver implements, which is the opposite of the arrangement: the
	// engine is something a driver may reach for, not something the storage
	// layer is built on.
	"store/kura": nil,

	"store/memstore":    {"", "acl", "doc", "store"},
	"store/pgstore":     {"", "acl", "doc", "store"},
	"store/sqlitestore": {"", "acl", "doc", "store"},
	"store/storetest":   {"", "acl", "doc", "store"},
	"index":             {"acl", "cache", "doc", "store"},
	"config":            nil,
	"web":               nil,
	"api":               {"", "acl", "cache", "directory", "doc", "index", "metric", "store", "thumb"},

	// thumb imports nothing of ours. It is handed bytes and a size and hands
	// back a smaller picture, which means the package that decodes files a
	// stranger wrote knows nothing about documents, principals or storage. It
	// stays that way: a scaler that could read a store would put the most
	// hostile input in the system next to the data it protects.
	"thumb": nil,

	// Ingestion sits beside the query path rather than under it. A connector
	// describes documents and who may read them, the pipeline writes them, and
	// neither knows anything about ranking. In particular connector does not
	// import store: a source that could reach the store directly could write a
	// document without going through the checks the pipeline applies.
	"connector": {"acl", "doc"},

	// extract turns a file into text and imports nothing of ours, not even doc.
	// It is handed bytes and a name and hands back a document's text, headings
	// and title, which is the whole of what it knows. An extractor that knew
	// what a document was would be a package that has to change every time the
	// model does, and one that knew what an access control list was would be a
	// second place for the permission model to live, in the package most likely
	// to be handed a hostile file.
	"extract": nil,

	// aclmap is the one place a source's own permission vocabulary is turned
	// into the model, and it sits beside the connectors rather than inside acl
	// because it is about somebody else's product rather than about ours. It
	// imports acl and nothing else of ours, and it must stay that way: a
	// mapping layer that could reach a store or a document would be a second
	// place for the permission rule to live.
	"connector/aclmap": {"acl"},
	// connectortest sits above the connectors and beside none of them, the way
	// storetest sits above the drivers. It names acl, connector and doc because
	// those are the three things a change is made of, and it must never name a
	// connector: a suite that imported the reference implementation would be a
	// suite that could only be run by packages allowed to import it, and would
	// slowly turn into a description of that one connector.
	"connector/connectortest": {"acl", "connector", "doc"},
	// thread is the assembly of a conversation into one document, and it names
	// acl and doc because a document and the rule that governs it are what it
	// produces. It does not name connector, which is the point of it being a
	// separate package: assembling a thread is a transformation of values with
	// no cursor, no source and nothing to close, and keeping it out of reach of
	// the connector interface is what keeps it testable as arithmetic.
	"connector/thread":   {"acl", "doc"},
	"connector/fssource": {"acl", "connector", "connector/aclmap", "doc", "extract"},
	// objectsource sits at the same level as fssource and names the same five
	// packages. It talks to a network service and fssource does not, and none of
	// that difference is allowed to show up as a dependency: an S3 client of our
	// own is a file in the package rather than a layer under it.
	"connector/objectsource": {"acl", "connector", "connector/aclmap", "doc", "extract"},
	// threadsource is the same level again, and it names thread rather than
	// extract because a conversation arrives as messages instead of as bytes:
	// there is nothing to pull text out of, only a shape to assemble. It does not
	// name aclmap either, because the rule on a conversation comes from the
	// container the product already told us about rather than from a file we were
	// handed alongside it.
	"connector/threadsource": {"acl", "connector", "connector/thread", "doc"},
	// slacksource is a product adapter and sits one level below the crawl it
	// plugs into, so it names threadsource. It also names limit, because Slack
	// publishes a different rate per method and a client that respects that is
	// part of the adapter rather than something a caller assembles. It does not
	// name extract, ingest or store: what an adapter knows is one product, and
	// an adapter that knew where its documents were going would be a second
	// place the pipeline is described.
	"connector/slacksource": {"acl", "connector", "connector/limit", "connector/thread", "connector/threadsource", "doc"},
	// jirasource is the same shape for tickets, and the list is the same list.
	// The one thing it has that the chat adapter does not is a rule on a single
	// document rather than on the container, which is what an issue security
	// level is, and that is still acl and threadsource rather than anything new.
	"connector/jirasource": {"acl", "connector", "connector/adf", "connector/limit", "connector/thread", "connector/threadsource", "doc"},
	// confluencesource is the third adapter and the list is the same list again,
	// which is the point of there being a list. A wiki has two things the other
	// two do not, a body written in a markup of its own and a restriction on a
	// single page that inherits down a tree, and neither of them is a dependency:
	// the markup is rendered inside the package and the restriction is acl and
	// threadsource like everything else.
	"connector/confluencesource": {"acl", "connector", "connector/adf", "connector/limit", "connector/thread", "connector/threadsource", "doc"},
	// adf imports nothing of ours. It turns one JSON tree into Markdown and it
	// has never heard of a document, a connector or a permission, which is what
	// lets two product adapters share it without either of them being able to
	// bend it towards their own product. It is at this level rather than beside
	// thread because the format belongs to the editor: it is the same tree in a
	// ticket description, a page body and a comment on either.
	"connector/adf": nil,
	// recorded imports nothing of ours either. It is a round tripper over a
	// directory of files, it deals in requests and responses, and it has never
	// heard of a document or a connector. Keeping it that way is what lets it be
	// used by a connector's tests without the tests of this package needing a
	// connector to exercise it.
	"connector/recorded": nil,
	// limit imports nothing of ours at all. It is a round tripper and a token
	// bucket, and the only thing it knows about a crawl is that requests go out
	// on an http.Client. A rate limiter that imported connector would be one that
	// could only limit our connectors, and the reason it is a round tripper in the
	// first place is that the limit belongs to the service rather than to us.
	"connector/limit": nil,
	// ingest names acl because a permission change that arrives without the
	// document is a map of access control lists and nothing else, and there is
	// no way to carry one without saying what it is. It is the one type from
	// that package the pipeline handles directly.
	"ingest": {"", "acl", "connector", "doc", "store"},

	// The benchmark corpus sits above everything, like storetest does, because
	// it exists to be measured against rather than to be built on. It is the one
	// package that names a driver: the fixture it hands a benchmark is a SQLite
	// file on disk, cached between runs, and generating it is far too slow to do
	// through an interface for the sake of symmetry.
	"benchcorpus":     {"acl", "doc", "store", "store/sqlitestore"},
	"benchcorpus/gen": {"benchcorpus", "store/sqlitestore"},

	"cmd/genba":  {""},
	"cmd/genbad": {"", "api", "config", "connector", "connector/aclmap", "connector/fssource", "connector/limit", "connector/objectsource", "directory", "directory/provider", "index", "ingest", "store", "store/memstore", "store/pgstore", "store/sqlitestore", "web"},
}

func TestDependencyDirection(t *testing.T) {
	for _, pkg := range packages(t) {
		t.Run(name(pkg), func(t *testing.T) {
			permitted, ok := allowed[pkg]
			if !ok {
				t.Fatalf("package %q is not in the layering map in arch_test.go, add it with the imports it is allowed", pkg)
			}
			for _, imp := range imports(t, pkg) {
				if !slices.Contains(permitted, imp) {
					t.Errorf("%s imports %s, which the layering does not allow", name(pkg), name(imp))
				}
			}
		})
	}
}

// TestNoInternalPackages keeps every package importable from outside the
// module. The platform is meant to be used as a library as well as run as a
// binary, and an internal directory is a decision that it is not.
func TestNoInternalPackages(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "internal" {
			return &noInternal{path}
		}
		return nil
	})
	var found *noInternal
	switch {
	case errors.As(err, &found):
		t.Fatalf("%s is an internal directory, so everything under it stops being usable as a library", found.path)
	case err != nil:
		t.Fatalf("walking the module: %v", err)
	}
}

type noInternal struct{ path string }

func (e *noInternal) Error() string { return "internal directory: " + e.path }

// TestEveryPackageIsDocumented catches the package that ships without a package
// comment, which is the one somebody will have to read the code of to use.
func TestEveryPackageIsDocumented(t *testing.T) {
	for _, pkg := range packages(t) {
		p, err := build.ImportDir(dir(pkg), build.ImportComment)
		if err != nil {
			t.Fatalf("reading %s: %v", name(pkg), err)
		}
		if strings.TrimSpace(p.Doc) == "" {
			t.Errorf("package %s has no package comment", name(pkg))
		}
	}
}

// packages returns every package of the module, named by its path relative to
// the module root. The root package is the empty string.
func packages(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		base := d.Name()
		if path != "." && (strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "testdata" || base == "node_modules") {
			return fs.SkipDir
		}
		if hasGoFiles(t, path) {
			out = append(out, rel(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	slices.Sort(out)
	return out
}

func hasGoFiles(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

// imports returns the module local packages that pkg imports, from its
// non test files. Test files are left out on purpose: a test may reach for a
// reference driver or a conformance suite without that being an architectural
// dependency of the package under test.
func imports(t *testing.T, pkg string) []string {
	t.Helper()
	p, err := build.ImportDir(dir(pkg), 0)
	if err != nil {
		t.Fatalf("reading %s: %v", name(pkg), err)
	}
	var out []string
	for _, imp := range p.Imports {
		if imp == module {
			out = append(out, "")
			continue
		}
		if after, ok := strings.CutPrefix(imp, module+"/"); ok {
			out = append(out, after)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func dir(pkg string) string {
	if pkg == "" {
		return "."
	}
	return pkg
}

func rel(path string) string {
	if path == "." {
		return ""
	}
	return filepath.ToSlash(path)
}

func name(pkg string) string {
	if pkg == "" {
		return module
	}
	return module + "/" + pkg
}
