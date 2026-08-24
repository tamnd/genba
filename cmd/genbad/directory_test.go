package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/api"
)

const smallCompany = `{
  "name": "acme",
  "groups": [
    {"id": "everyone"},
    {"id": "engineering", "member_of": ["everyone"]}
  ],
  "subjects": [
    {"id": "mei", "email": "mei@acme.com", "member_of": ["engineering"]}
  ]
}`

// write puts a directory in a temporary file and hands back the path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "directory.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// serving starts a server with these flags and stops it when the test ends.
func serving(t *testing.T, args ...string) string {
	t.Helper()
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, append([]string{"-addr", addr, "-tenant", "acme", "-log-level", "error"}, args...), env(nil), &out, &errOut)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the server did not shut down after its context was cancelled")
		}
	})

	waitForHealth(t, "http://"+addr+"/healthz")
	return "http://" + addr
}

// groupsOf asks who the server thinks somebody is.
func groupsOf(t *testing.T, base string, header ...string) []string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/v1/me", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.HeaderSubject, "mei")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/v1/me answered %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out.Groups
}

// The reason all of this exists. A group list on a request is a copy of an
// answer somebody else cached, and a copy somebody can edit is a way in.
func TestAGroupsHeaderIsThrownAwayWhenThereIsADirectory(t *testing.T) {
	base := serving(t, "-directory", write(t, smallCompany))

	got := groupsOf(t, base, api.HeaderGroups, "acme:administrators,acme:finance")
	if slices.Contains(got, "acme:administrators") {
		t.Fatalf("a group from the request reached the session: %v", got)
	}
	if want := []string{"acme:engineering", "acme:everyone"}; !slices.Equal(got, want) {
		t.Errorf("the session carries %v, want %v", got, want)
	}
}

// And a deployment without one behaves exactly as it did, because a search
// engine somebody is trying out on a laptop should not need an identity
// provider, and a proxy that has already done the resolution is a real shape.
func TestWithoutADirectoryTheRequestIsStillBelieved(t *testing.T) {
	base := serving(t)

	got := groupsOf(t, base, api.HeaderGroups, "acme:engineering")
	if want := []string{"acme:engineering"}; !slices.Equal(got, want) {
		t.Errorf("the session carries %v, want %v", got, want)
	}
}

// Editing a group and then bouncing the server is how somebody ends up not
// editing the group.
func TestEditingTheDirectoryTakesEffectWithoutARestart(t *testing.T) {
	path := write(t, smallCompany)
	base := serving(t, "-directory", path, "-directory-refresh", "20ms")

	if want := []string{"acme:engineering", "acme:everyone"}; !slices.Equal(groupsOf(t, base), want) {
		t.Fatalf("the session started as %v, want %v", groupsOf(t, base), want)
	}

	edited := strings.Replace(smallCompany,
		`{"id": "everyone"},`,
		`{"id": "everyone"},
    {"id": "on-call"},`, 1)
	edited = strings.Replace(edited, `"member_of": ["engineering"]`, `"member_of": ["engineering", "on-call"]`, 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	want := []string{"acme:engineering", "acme:everyone", "acme:on-call"}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Equal(groupsOf(t, base), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the edit never took effect, the session still carries %v", groupsOf(t, base))
}

// An operator halfway through an edit should not take everybody's groups away,
// and refusing every request until the file parses again turns a typo into an
// outage.
func TestAnEditThatDoesNotParseKeepsWhatWasAlreadyLoaded(t *testing.T) {
	path := write(t, smallCompany)
	base := serving(t, "-directory", path, "-directory-refresh", "20ms")

	if err := os.WriteFile(path, []byte(`{"name": "acme", "groups": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if want := []string{"acme:engineering", "acme:everyone"}; !slices.Equal(groupsOf(t, base), want) {
		t.Errorf("a broken edit changed the session to %v, want %v", groupsOf(t, base), want)
	}
}

// A file that does not parse at startup is a different matter. Nothing is
// loaded, so there is nothing to keep, and coming up resolving nobody would be
// a server that answers every request with a refusal.
func TestADirectoryThatDoesNotParseStopsTheServerStarting(t *testing.T) {
	path := write(t, `{"name": "acme", "subjects": [{"id": "mei", "member_of": ["enginering"]}]}`)

	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"-addr", freeAddr(t), "-directory", path, "-log-level", "error"}, env(nil), &out, &errOut)
	if err == nil {
		t.Fatal("the server started with a directory that does not parse")
	}
	if !strings.Contains(err.Error(), "enginering") {
		t.Errorf("the error is %q and does not say what is wrong with the file", err)
	}
}

func TestADirectoryThatIsNotThereStopsTheServerStarting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")

	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"-addr", freeAddr(t), "-directory", path, "-log-level", "error"}, env(nil), &out, &errOut)
	if err == nil {
		t.Fatal("the server started with a directory that is not there")
	}
	if !strings.Contains(err.Error(), "absent.json") {
		t.Errorf("the error is %q and does not name the file", err)
	}
}

// Every group key carries the directory's name, so renaming it renames every
// group in every rule at once. That is a different directory rather than an
// edit to this one.
func TestRenamingTheDirectoryIsRefusedRatherThanApplied(t *testing.T) {
	path := write(t, smallCompany)
	held := &swap{path: path}
	if err := held.read(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(strings.Replace(smallCompany, `"name": "acme"`, `"name": "other"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := held.reread(); err == nil {
		t.Fatal("a renamed directory was swapped in")
	}
	if held.Name() != "acme" {
		t.Errorf("the directory is now named %q", held.Name())
	}
}

// A reread that finds the same bytes costs a hash rather than a parse and a
// cache flush, which is what makes a twenty second ticker affordable.
func TestARereadThatChangesNothingSaysSo(t *testing.T) {
	held := &swap{path: write(t, smallCompany)}
	if err := held.read(); err != nil {
		t.Fatal(err)
	}
	changed, err := held.reread()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("rereading an unchanged file reported a change, so every tick would flush the cache")
	}
}
