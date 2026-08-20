package genba_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two strings this test looks for. The first is the compile time assertion
// every connector in this repository carries, and the second is the line that
// runs the suite against it.
const (
	declaresAConnector = "connector.Connector = "
	runsTheSuite       = "connectortest.Run("
)

// TestEveryConnectorRunsTheConformanceSuite is how the suite is enforced rather
// than merely offered.
//
// A conformance suite nobody runs is documentation, and documentation about
// permissions is what gets skipped when a connector is written in a hurry
// against a source somebody needs indexed by Friday. So the rule is mechanical:
// a package that declares a connector has to run the suite against it, and this
// test is what fails in continuous integration when the next one does not.
//
// It reads the files rather than the types because the thing being checked is
// that a test exists. A reflective check would need the test to be running to
// find out that the test is not there.
func TestEveryConnectorRunsTheConformanceSuite(t *testing.T) {
	var found int
	for _, pkg := range packages(t) {
		if !strings.HasPrefix(pkg, "connector/") || !declares(t, pkg) {
			continue
		}
		found++
		t.Run(name(pkg), func(t *testing.T) {
			if !covered(t, pkg) {
				t.Errorf("%s implements connector.Connector and no test in it calls connectortest.Run, so nothing checks that the documents it indexes carry the access control lists that govern them", name(pkg))
			}
		})
	}
	if found == 0 {
		// The walk found no connectors at all, which means the way they are
		// written has changed and this test has quietly stopped checking
		// anything.
		t.Fatalf("no package in the module declares a connector, so this test is not checking what it says it is")
	}
}

// declares reports whether a package implements the connector interface, which
// every connector here says in one line at the top of the file.
func declares(t *testing.T, pkg string) bool {
	t.Helper()
	return contains(t, pkg, declaresAConnector, false)
}

// covered reports whether a package's own tests run the conformance suite.
func covered(t *testing.T, pkg string) bool {
	t.Helper()
	return contains(t, pkg, runsTheSuite, true)
}

// contains reports whether any Go file in a package has a string in it, looking
// at either the package's own files or its tests.
func contains(t *testing.T, pkg, want string, tests bool) bool {
	t.Helper()
	entries, err := os.ReadDir(dir(pkg))
	if err != nil {
		t.Fatalf("reading %s: %v", name(pkg), err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") != tests {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir(pkg), e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if strings.Contains(string(body), want) {
			return true
		}
	}
	return false
}
