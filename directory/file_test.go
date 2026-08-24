package directory_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/directory"
)

const acme = `{
  "name": "acme",
  "groups": [
    {"id": "everyone"},
    {"id": "engineering", "name": "Engineering", "member_of": ["everyone"]},
    {"id": "storage", "member_of": ["engineering"]}
  ],
  "subjects": [
    {"id": "mei", "name": "Mei", "email": "mei@acme.com", "identities": ["slack:U04AB"], "member_of": ["storage"]},
    {"id": "lee", "member_of": ["everyone"], "disabled": true},
    {"id": "sam"}
  ]
}`

func read(t *testing.T, in string) (*directory.Static, error) {
	t.Helper()
	return directory.ReadStatic(strings.NewReader(in))
}

func good(t *testing.T, in string) *directory.Static {
	t.Helper()
	d, err := read(t, in)
	if err != nil {
		t.Fatalf("reading the directory: %v", err)
	}
	return d
}

func TestAWrittenDirectoryResolvesTheSameAsAnyOther(t *testing.T) {
	c, err := directory.NewCache(mustResolve(t, good(t, acme)))
	if err != nil {
		t.Fatal(err)
	}

	got := resolved(t, c, "mei")
	want := []string{"acme:engineering", "acme:everyone", "acme:storage"}
	if !slices.Equal(got.Groups.Members, want) {
		t.Errorf("mei resolved to %v, want %v", got.Groups.Members, want)
	}
	if got.Subject.Email != "mei@acme.com" {
		t.Errorf("the email did not survive the file: %q", got.Subject.Email)
	}
	if want := (acl.Identity{Source: "slack", Value: "U04AB"}); !slices.Contains(got.Subject.Identities, want) {
		t.Errorf("the identities are %v, want %v in them", got.Subject.Identities, want)
	}
}

func TestSomebodyInNoGroupsIsOneLine(t *testing.T) {
	c, err := directory.NewCache(mustResolve(t, good(t, acme)))
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved(t, c, "sam"); len(got.Groups.Members) != 0 {
		t.Errorf("sam resolved to %v, want nothing", got.Groups.Members)
	}
}

func TestADeactivatedAccountInTheFileIsStillDeactivated(t *testing.T) {
	if _, err := good(t, acme).Subject(t.Context(), "lee"); err != nil {
		t.Fatalf("the directory does not hold lee at all: %v", err)
	}
	c, err := directory.NewCache(mustResolve(t, good(t, acme)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Expand(t.Context(), "lee"); err == nil {
		t.Error("a deactivated account in the file resolved anyway")
	}
}

// The whole advantage a file has over an identity provider is that its mistakes
// can be caught before anybody signs in, so it is strict.
func TestABadFileIsRefusedWithSomethingToActOn(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		says string
	}{
		{"no name", `{"subjects": [{"id": "mei"}]}`, "name"},
		{"not json", `nonsense`, "invalid character"},
		{"an unknown field", `{"name": "acme", "membors": []}`, "membors"},
		{"a group with no id", `{"name": "acme", "groups": [{"name": "Engineering"}]}`, "no id"},
		{"a subject with no id", `{"name": "acme", "subjects": [{"name": "Mei"}]}`, "no id"},
		{"the same group twice", `{"name": "acme", "groups": [{"id": "a"}, {"id": "a"}]}`, "twice"},
		{"the same subject twice", `{"name": "acme", "subjects": [{"id": "mei"}, {"id": "mei"}]}`, "twice"},
		{
			"a subject in a group that is not defined",
			`{"name": "acme", "subjects": [{"id": "mei", "member_of": ["enginering"]}]}`,
			"enginering",
		},
		{
			"a group in a group that is not defined",
			`{"name": "acme", "groups": [{"id": "a", "member_of": ["b"]}]}`,
			`"b"`,
		},
		{
			"an identity that is not source and value",
			`{"name": "acme", "subjects": [{"id": "mei", "identities": ["U04AB"]}]}`,
			"source:value",
		},
		{"two documents", `{"name": "acme"}{"name": "other"}`, "more than one"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := read(t, c.in)
			if err == nil {
				t.Fatal("the file was accepted")
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("the error is %q, and %q is not in it", err, c.says)
			}
		})
	}
}

// The order groups are written in is not something anybody should have to think
// about.
func TestAGroupMayBeWrittenAfterTheOneItIsIn(t *testing.T) {
	good(t, `{
	  "name": "acme",
	  "groups": [
	    {"id": "storage", "member_of": ["engineering"]},
	    {"id": "engineering"}
	  ]
	}`)
}

func TestOpeningAFileThatIsNotThereSaysWhichFile(t *testing.T) {
	_, err := directory.OpenStatic(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("a directory that is not there was opened")
	}
	if !strings.Contains(err.Error(), "absent.json") {
		t.Errorf("the error is %q and does not name the file", err)
	}
}

func TestOpeningAFileThatDoesNotParseSaysWhichFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acme.json")
	if err := os.WriteFile(path, []byte(`{"name": "acme", "groups": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := directory.OpenStatic(path)
	if err == nil {
		t.Fatal("a truncated directory was accepted")
	}
	if !strings.Contains(err.Error(), "acme.json") {
		t.Errorf("the error is %q and does not name the file", err)
	}
}

func TestAWrittenDirectoryReadsBackFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acme.json")
	if err := os.WriteFile(path, []byte(acme), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := directory.OpenStatic(path)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "acme" {
		t.Errorf("the directory is named %q", d.Name())
	}
	if _, err := d.Subject(t.Context(), "mei"); err != nil {
		t.Errorf("mei is not in the directory that was read: %v", err)
	}
}

func mustResolve(t *testing.T, d directory.Directory) *directory.Resolver {
	t.Helper()
	r, err := directory.New(d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
