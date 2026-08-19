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
	"":                nil,
	"acl":             nil,
	"doc":             {"acl"},
	"store":           {"acl", "doc"},
	"store/memstore":  {"", "acl", "doc", "store"},
	"store/storetest": {"", "acl", "doc", "store"},
	"index":           {"acl", "doc", "store"},
	"config":          nil,
	"web":             nil,
	"api":             {"", "acl", "doc", "index", "store"},

	// Ingestion sits beside the query path rather than under it. A connector
	// describes documents and who may read them, the pipeline writes them, and
	// neither knows anything about ranking. In particular connector does not
	// import store: a source that could reach the store directly could write a
	// document without going through the checks the pipeline applies.
	"connector":          {"acl", "doc"},
	"connector/fssource": {"acl", "connector", "doc"},
	"ingest":             {"", "connector", "doc", "store"},

	"cmd/genba":  {""},
	"cmd/genbad": {"", "api", "config", "connector", "connector/fssource", "index", "ingest", "store", "store/memstore", "web"},
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
