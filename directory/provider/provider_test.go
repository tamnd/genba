package provider_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tamnd/genba/directory"
	"github.com/tamnd/genba/directory/provider"
)

// A service account key file, which is the one credential of the three that has
// to be a real one: the Google adapter parses the key at construction, on
// purpose, so a description naming a key file that is not a key is refused here
// rather than at the first search.
//
// One key for the whole package, because generating an RSA key is the slowest
// thing in this file by a long way and none of these cases is about which key it
// is.
var keyFile = sync.OnceValue(func() string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	raw, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "acme-search",
		"private_key_id": "0123456789abcdef",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "genba@acme.iam.gserviceaccount.com",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
})

// held writes a credential to a file and hands back the path, which is the
// shape a mounted secret arrives in.
func held(t *testing.T, credential string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// open builds a directory out of a description, with nothing in the
// environment unless a case says otherwise.
func open(t *testing.T, description string, env ...string) (directory.Directory, error) {
	t.Helper()
	set := map[string]string{}
	for i := 0; i+1 < len(env); i += 2 {
		set[env[i]] = env[i+1]
	}
	return provider.Open([]byte(description), func(k string) string { return set[k] })
}

// TestADescriptionIsToldFromADirectory is the whole of how one flag reaches
// both, so it is worth stating on its own.
func TestADescriptionIsToldFromADirectory(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  string
		want bool
	}{
		{"a description", `{"provider": "okta", "name": "acme"}`, true},
		{"a directory written out in full", `{"name": "acme", "groups": [{"id": "everyone"}]}`, false},
		{"a directory with somebody called provider in it", `{"name": "acme", "subjects": [{"id": "provider"}]}`, false},
		{"an empty object", `{}`, false},
		{"something that is not JSON at all", `not a file anybody meant to write`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := provider.Describes([]byte(c.raw)); got != c.want {
				t.Errorf("Describes = %v, want %v", got, c.want)
			}
		})
	}
}

// TestEachProviderIsBuiltFromItsOwnDescription is the point of the package:
// three providers, one spelling, and the name a rule is written against is the
// one in the file.
func TestEachProviderIsBuiltFromItsOwnDescription(t *testing.T) {
	for _, c := range []struct {
		name        string
		description string
	}{
		{"okta", fmt.Sprintf(`{
			"provider": "okta",
			"name": "acme",
			"endpoint": "https://acme.okta.com",
			"credential_file": %q
		}`, held(t, "00AnApiToken\n"))},

		{"entra", fmt.Sprintf(`{
			"provider": "entra",
			"name": "acme",
			"tenant": "8f7c1a2b-3d4e-4f50-9a6b-1c2d3e4f5a60",
			"client_id": "c3d4e5f6-0718-4923-a4b5-6c7d8e9f0a12",
			"credential_file": %q
		}`, held(t, "a-client-secret"))},

		{"google", fmt.Sprintf(`{
			"provider": "google",
			"name": "acme",
			"subject": "admin@acme.test",
			"credential_file": %q
		}`, held(t, keyFile()))},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, err := open(t, c.description)
			if err != nil {
				t.Fatal(err)
			}
			if d.Name() != "acme" {
				t.Errorf("the source is called %q, and every group key carries that", d.Name())
			}
		})
	}
}

// TestTheCredentialComesFromTheEnvironmentToo is the other half of it, which is
// what a container that was handed a secret looks like.
func TestTheCredentialComesFromTheEnvironmentToo(t *testing.T) {
	d, err := open(t, `{
		"provider": "okta",
		"name": "acme",
		"endpoint": "https://acme.okta.com",
		"credential_env": "ACME_OKTA_TOKEN"
	}`, "ACME_OKTA_TOKEN", "00AnApiToken")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "acme" {
		t.Errorf("the source is called %q", d.Name())
	}
}

// TestADescriptionThatCannotBeUsedIsRefused is one case rather than fifteen
// because every one of these is the same statement: whatever is wrong with a
// description is wrong at startup, and the message says which file and what.
func TestADescriptionThatCannotBeUsedIsRefused(t *testing.T) {
	token := held(t, "00AnApiToken")
	key := held(t, keyFile())

	for _, c := range []struct {
		name        string
		description string
		says        string
	}{
		{
			"a provider nobody has written an adapter for",
			fmt.Sprintf(`{"provider": "ldap", "name": "acme", "credential_file": %q}`, token),
			"is not a provider this knows",
		},
		{
			"no name, so nothing could tell its groups from another source's",
			fmt.Sprintf(`{"provider": "okta", "endpoint": "https://acme.okta.com", "credential_file": %q}`, token),
			"no name",
		},
		{
			"an Okta organisation with no organisation",
			fmt.Sprintf(`{"provider": "okta", "name": "acme", "credential_file": %q}`, token),
			"no endpoint",
		},
		{
			"an Entra ID application with no tenant",
			fmt.Sprintf(`{"provider": "entra", "name": "acme", "client_id": "c3d4e5f6-0718-4923-a4b5-6c7d8e9f0a12", "credential_file": %q}`, token),
			"no tenant",
		},
		{
			"an Entra ID tenant with no application",
			fmt.Sprintf(`{"provider": "entra", "name": "acme", "tenant": "8f7c1a2b-3d4e-4f50-9a6b-1c2d3e4f5a60", "credential_file": %q}`, token),
			"no client_id",
		},
		{
			"a service account with nobody to act for",
			fmt.Sprintf(`{"provider": "google", "name": "acme", "credential_file": %q}`, key),
			"no subject",
		},
		{
			"half of one description and half of another",
			fmt.Sprintf(`{"provider": "okta", "name": "acme", "endpoint": "https://acme.okta.com", "tenant": "8f7c1a2b", "credential_file": %q}`, token),
			`"tenant" means nothing to okta`,
		},
		{
			"the credential itself, which is the mistake worth a real message",
			`{"provider": "okta", "name": "acme", "endpoint": "https://acme.okta.com", "credential": "00AnApiToken"}`,
			"does not belong in this file",
		},
		{
			"no credential at all",
			`{"provider": "okta", "name": "acme", "endpoint": "https://acme.okta.com"}`,
			"no credential",
		},
		{
			"both of them, which is a merge somebody did not finish",
			fmt.Sprintf(`{"provider": "okta", "name": "acme", "endpoint": "https://acme.okta.com", "credential_file": %q, "credential_env": "ACME_OKTA_TOKEN"}`, token),
			"both credential_file and credential_env",
		},
		{
			"a credential file that is not there",
			`{"provider": "okta", "name": "acme", "endpoint": "https://acme.okta.com", "credential_file": "/etc/genba/gone"}`,
			"reading the credential",
		},
		{
			"an empty credential file, which is a secret that failed to mount",
			fmt.Sprintf(`{"provider": "okta", "name": "acme", "endpoint": "https://acme.okta.com", "credential_file": %q}`, held(t, "\n")),
			"is empty",
		},
		{
			"an environment variable nobody set",
			`{"provider": "okta", "name": "acme", "endpoint": "https://acme.okta.com", "credential_env": "ACME_OKTA_TOKEN"}`,
			"is not set",
		},
		{
			"a key file that is a user credential rather than a service account",
			fmt.Sprintf(`{"provider": "google", "name": "acme", "subject": "admin@acme.test", "credential_file": %q}`,
				held(t, `{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`)),
			"rather than a service account",
		},
		{
			"a key file that is not a key",
			fmt.Sprintf(`{"provider": "google", "name": "acme", "subject": "admin@acme.test", "credential_file": %q}`,
				held(t, `{"type":"service_account","client_email":"genba@acme.iam.gserviceaccount.com","private_key":"/etc/genba/key.pem"}`)),
			"not PEM",
		},
		{
			"a field nobody has ever heard of",
			fmt.Sprintf(`{"provider": "okta", "name": "acme", "endpoint": "https://acme.okta.com", "credentials_file": %q}`, token),
			"unknown field",
		},
		{
			"two descriptions in one file",
			fmt.Sprintf(`{"provider": "okta", "name": "acme", "endpoint": "https://acme.okta.com", "credential_file": %q}{"provider": "okta"}`, token),
			"more than one description",
		},
		{
			"something that is not a description at all",
			`name = acme`,
			"provider:",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := open(t, c.description)
			if err == nil {
				t.Fatal("that was accepted, and it would have failed at the first search instead")
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("the refusal is %q, which does not say %q", err, c.says)
			}
		})
	}
}

// TestACredentialTheProviderRefusesIsFoundAtStartup is what [provider.Reachable]
// is for. A server that comes up and then refuses every sign in looks like an
// outage in the search engine rather than a token somebody forgot to rotate.
func TestACredentialTheProviderRefusesIsFoundAtStartup(t *testing.T) {
	org := refusing(t, http.StatusUnauthorized, `{"errorCode":"E0000011","errorSummary":"Invalid token provided"}`)

	d, err := open(t, fmt.Sprintf(`{
		"provider": "okta",
		"name": "acme",
		"endpoint": %q,
		"credential_file": %q
	}`, org, held(t, "00NotAToken")))
	if err != nil {
		t.Fatal(err)
	}

	err = provider.Reachable(t.Context(), d)
	if err == nil {
		t.Fatal("a token the organisation will not accept was let through")
	}
	// Which source, because a deployment with three of them gets one line in a
	// unit file to work out which one is wrong.
	if !strings.Contains(err.Error(), "acme") {
		t.Errorf("the refusal is %q, which does not say which source it is about", err)
	}
	if !strings.Contains(err.Error(), "Invalid token provided") {
		t.Errorf("the refusal is %q, which does not carry what the provider said", err)
	}
}

// And the other way round: a credential that works produces nothing, and the
// question it asks is about nobody.
func TestACredentialThatWorksSaysNothing(t *testing.T) {
	org := refusing(t, http.StatusNotFound, `{"errorCode":"E0000007","errorSummary":"Not found: Resource not found"}`)

	d, err := open(t, fmt.Sprintf(`{
		"provider": "okta",
		"name": "acme",
		"endpoint": %q,
		"credential_file": %q
	}`, org, held(t, "00AnApiToken")))
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Reachable(t.Context(), d); err != nil {
		t.Errorf("a working credential was reported as %v", err)
	}
}

// TestReachableTakesAnAnswerHoweverItComes covers the two answers that are not
// a lookup that found nothing, without standing up three fake providers to get
// at them.
func TestReachableTakesAnAnswerHoweverItComes(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"nobody by that name", directory.ErrNoSubject, true},
		{"somebody by that name, deactivated", directory.ErrDisabled, true},
		{"somebody by that name", nil, true},
		{"a tenant that is not answering", errors.New("dial tcp: connection refused"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := provider.Reachable(t.Context(), stub{err: c.err})
			if (err == nil) != c.want {
				t.Errorf("Reachable = %v", err)
			}
		})
	}
}

// refusing is a service that answers everything the same way, which is all a
// credential check ever sees.
func refusing(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// stub is a directory that answers one way, for the cases above that are about
// what an answer means rather than about how it was obtained.
type stub struct{ err error }

func (stub) Name() string { return "acme" }

func (s stub) Subject(context.Context, string) (directory.Subject, error) {
	return directory.Subject{ID: "nobody"}, s.err
}

func (s stub) Group(context.Context, string) (directory.Group, error) {
	return directory.Group{}, s.err
}
