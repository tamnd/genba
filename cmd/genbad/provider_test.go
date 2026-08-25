package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// An organisation with one person in it, which is as much of a provider as
// these cases need. What is being tested here is the flag and the startup, and
// the adapter itself is tested against a fake and a recording next to it.
func fakeOrg(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/mei", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{
			"id": "mei",
			"status": "ACTIVE",
			"lastUpdated": "2026-02-11T09:14:22.000Z",
			"profile": {"login": "mei@cloud.example", "email": "mei@cloud.example", "firstName": "Mei", "lastName": "Tanaka"}
		}`)
	})
	mux.HandleFunc("/api/v1/users/mei/groups", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `[
			{"id": "sales", "lastUpdated": "2026-01-04T11:02:00.000Z", "profile": {"name": "Sales"}}
		]`)
	})
	// Everybody else, which includes the id the startup check asks about. A
	// lookup that finds nothing is the cheapest answer there is and it takes a
	// credential the organisation accepted, which is the whole of what that
	// check is asking.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, `{"errorCode":"E0000007","errorSummary":"Not found: Resource not found"}`)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// describe writes a description of a hosted directory and hands back the path,
// alongside the credential file it names.
func describe(t *testing.T, org, credential string) string {
	t.Helper()
	dir := t.TempDir()

	secret := filepath.Join(dir, "token")
	if err := os.WriteFile(secret, []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cloud.json")
	body := fmt.Sprintf(`{
  "provider": "okta",
  "name": "cloud",
  "endpoint": %q,
  "credential_file": %q
}`, org, secret)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAFileAndAnOrganisationAreUnionedIntoOneSession is why the description is
// a file rather than a spelling on the flag. The company that acquired another
// company keeps forty contractors in a JSON file and everybody else in an Okta
// organisation, and that is one flag value and one search box.
func TestAFileAndAnOrganisationAreUnionedIntoOneSession(t *testing.T) {
	paths := write(t, smallCompany) + "," + describe(t, fakeOrg(t), "00AnApiToken\n")
	base := serving(t, "-directory", paths)

	got := groupsOf(t, base)
	want := []string{"acme:engineering", "acme:everyone", "cloud:sales"}
	if !slices.Equal(got, want) {
		t.Errorf("the session carries %v, want %v", got, want)
	}
}

// TestTheCredentialCanComeFromTheEnvironment is the other half of it, and the
// reason neither half is a flag: a secret is the one thing that must not arrive
// on a command line, because argv is readable by every process on the machine.
func TestTheCredentialCanComeFromTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cloud.json")
	body := fmt.Sprintf(`{
  "provider": "okta",
  "name": "cloud",
  "endpoint": %q,
  "credential_env": "CLOUD_OKTA_TOKEN"
}`, fakeOrg(t))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	base := servingWith(t, env(map[string]string{"CLOUD_OKTA_TOKEN": "00AnApiToken"}), "-directory", path)
	got := groupsOf(t, base)
	if want := []string{"cloud:sales"}; !slices.Equal(got, want) {
		t.Errorf("the session carries %v, want %v", got, want)
	}
}

// TestACredentialTheOrganisationRefusesStopsTheServerStarting is the failure
// worth catching at startup rather than at the first search. A server that
// comes up and then resolves everybody into nothing looks like a bug in the
// search engine, and this looks like what it is.
func TestACredentialTheOrganisationRefusesStopsTheServerStarting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusUnauthorized, `{"errorCode":"E0000011","errorSummary":"Invalid token provided"}`)
	}))
	t.Cleanup(srv.Close)

	path := describe(t, srv.URL, "00NotAToken")

	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"-addr", freeAddr(t), "-directory", path, "-log-level", "error"}, env(nil), &out, &errOut)
	if err == nil {
		t.Fatal("the server started with a credential the organisation will not accept")
	}
	// Which file, because a deployment with three of these gets one line in a
	// unit file to work out which one is wrong.
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error is %q and does not name the file", err)
	}
	if !strings.Contains(err.Error(), "Invalid token provided") {
		t.Errorf("the error is %q and does not carry what the organisation said", err)
	}
}

// A description that names an environment variable nobody set is the same sort
// of failure as a directory file that is not there, and it is refused in the
// same place rather than at the first search.
func TestASecretThatDidNotArriveStopsTheServerStarting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cloud.json")
	body := fmt.Sprintf(`{
  "provider": "okta",
  "name": "cloud",
  "endpoint": %q,
  "credential_env": "CLOUD_OKTA_TOKEN"
}`, fakeOrg(t))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"-addr", freeAddr(t), "-directory", path, "-log-level", "error"}, env(nil), &out, &errOut)
	if err == nil {
		t.Fatal("the server started with no credential at all")
	}
	if !strings.Contains(err.Error(), "CLOUD_OKTA_TOKEN") {
		t.Errorf("the error is %q and does not say where the credential was to come from", err)
	}
}
