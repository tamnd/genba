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
	"api":               {"", "acl", "cache", "doc", "index", "metric", "store"},

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
	"connector/aclmap":   {"acl"},
	"connector/fssource": {"acl", "connector", "connector/aclmap", "doc", "extract"},
	// objectsource sits at the same level as fssource and names the same five
	// packages. It talks to a network service and fssource does not, and none of
	// that difference is allowed to show up as a dependency: an S3 client of our
	// own is a file in the package rather than a layer under it.
	"connector/objectsource": {"acl", "connector", "connector/aclmap", "doc", "extract"},
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
	"cmd/genbad": {"", "api", "config", "connector", "connector/aclmap", "connector/fssource", "index", "ingest", "store", "store/memstore", "store/pgstore", "store/sqlitestore", "web"},
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
