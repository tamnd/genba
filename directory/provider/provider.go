// Package provider builds a directory out of a description of one.
//
// The adapters in the packages next to this one are reachable from Go and
// nothing else, which is fine while there is one of them and it is being
// written, and is not fine once there are three. A deployment that wants its
// groups from Okta should not have to import a package and build its own
// binary.
//
// # Why a file
//
// `-directory` already takes a comma separated list of files, unioned by
// [directory.Multi], and that is the whole of the spelling. A file here is
// either a directory written out in full, which is the deployment with forty
// people in it, or a description of a hosted one, which is this package. A
// mixed list works because it was always a list of files, so a company that
// acquired another company can point at their own Okta organisation and at the
// forty contractors somebody keeps in a JSON file and get one search box.
//
// The alternative was a spelling on the flag itself, "okta:acme.okta.com", and
// it loses twice. A credential cannot go in it, because argv is readable by
// every process on the machine, so the secret would have to arrive from an
// environment variable named after the source, and now there are two places to
// look. And a national cloud, a page size or a second organisation turns one
// flag value into a query string nobody can read.
//
// # Where the credential comes from
//
//	{
//	  "provider": "okta",
//	  "name": "acme",
//	  "endpoint": "https://acme.okta.com",
//	  "credential_file": "/etc/genba/okta-token"
//	}
//
// Never from the description itself. A file like the one above is the sort of
// thing that gets pasted into a ticket, committed to a repository of manifests
// and read by everybody who can list a directory, and none of that is true of
// the file it names. `credential_file` is a path whose contents are the
// credential, which is what a mounted secret looks like, and `credential_env`
// names an environment variable instead, which is what a container that was
// handed one looks like. A description that carries the credential inline is
// refused with a message saying which of the two to use, because that is the
// mistake somebody is going to make and "unknown field" is not an answer to it.
//
// # What is checked at startup
//
// A credential the provider will not accept stops the server coming up, see
// [Reachable]. A server that starts and then refuses every sign in looks like
// an outage in the search engine rather than a token somebody forgot to
// rotate, and it is worth one request at startup to tell those apart.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tamnd/genba/directory"
	"github.com/tamnd/genba/directory/entra"
	"github.com/tamnd/genba/directory/google"
	"github.com/tamnd/genba/directory/okta"
)

// The providers a description can name.
const (
	Okta   = "okta"
	Entra  = "entra"
	Google = "google"
)

// spec is a description of a hosted directory.
//
// It is not exported. Somebody writing Go has the adapters themselves, which
// take their own options and are better at it, and an exported struct here
// would be a second way to do the same thing that has to keep up with all three
// of them.
type spec struct {
	// Provider is which of the three this is, and its presence is what
	// separates a description from a directory written out in full.
	Provider string `json:"provider"`

	// Name is the identity source the group keys carry. It is required for the
	// same reason it is required in a directory file: it is what a rule is
	// written against, so it cannot be something that got picked by default.
	Name string `json:"name"`

	// Endpoint is the organisation URL for Okta, which has no default, and an
	// override of the service for the other two, which do.
	Endpoint string `json:"endpoint"`

	// Tenant and ClientID are the halves of an Entra ID application that are
	// not the secret.
	Tenant   string `json:"tenant"`
	ClientID string `json:"client_id"`

	// Authority is where tokens are asked for, which a national cloud and a
	// test both need. Okta has no such thing, since its API token is the
	// credential rather than something a credential is exchanged for.
	Authority string `json:"authority"`

	// Subject is the administrator a Google Workspace service account acts as.
	Subject string `json:"subject"`

	// CredentialFile is a path whose contents are the credential, and
	// CredentialEnv names an environment variable holding it instead. Exactly
	// one of them.
	CredentialFile string `json:"credential_file"`
	CredentialEnv  string `json:"credential_env"`

	// Credential is here to be refused. It is where somebody will put the
	// secret, and a field that exists produces a better error than an unknown
	// one does.
	Credential string `json:"credential"`
}

// Describes reports whether a document is a description of a hosted provider
// rather than a directory written out in full.
//
// It looks at one field and ignores everything else, including whether the rest
// of the document makes sense, because the caller has a file it has to decide
// what to do with and a description that is wrong should be reported by the
// thing that understands descriptions.
func Describes(raw []byte) bool {
	var in struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return false
	}
	return in.Provider != ""
}

// Open builds the directory a description names.
//
// getenv is where `credential_env` is looked up, and nil means the process
// environment.
func Open(raw []byte, getenv func(string) string) (directory.Directory, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	// Strict for the same reason the directory file is: an unknown field here
	// is a typo, and a typo in the name of a credential is a deployment that
	// comes up against the wrong tenant or does not come up at all, which are
	// both better found now.
	dec.DisallowUnknownFields()

	var s spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}
	// Anything after the object is a second description pasted in by accident,
	// and silently reading the first half of a file is worse than refusing it.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, errors.New("provider: more than one description in the file")
	}
	if s.Name == "" {
		return nil, errors.New("provider: no name, and a directory without one cannot have its groups told apart from another's")
	}

	credential, err := s.credential(getenv)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", s.Name, err)
	}

	var d directory.Directory
	switch s.Provider {
	case Okta:
		d, err = s.okta(credential)
	case Entra:
		d, err = s.entra(credential)
	case Google:
		d, err = s.google(credential)
	default:
		return nil, fmt.Errorf("provider %s: %q is not a provider this knows, which are %s, %s and %s",
			s.Name, s.Provider, Okta, Entra, Google)
	}
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", s.Name, err)
	}
	return d, nil
}

func (s spec) okta(token string) (directory.Directory, error) {
	if s.Endpoint == "" {
		return nil, errors.New("no endpoint, which for this provider is the organisation URL, https://acme.okta.com")
	}
	if err := s.unused(Okta, "tenant", "client_id", "authority", "subject"); err != nil {
		return nil, err
	}
	return okta.New(s.Endpoint, token, okta.WithName(s.Name))
}

func (s spec) entra(secret string) (directory.Directory, error) {
	switch {
	case s.Tenant == "":
		return nil, errors.New("no tenant, which is the directory id of the Entra ID tenant")
	case s.ClientID == "":
		return nil, errors.New("no client_id, which is the application registration this signs in as")
	}
	if err := s.unused(Entra, "subject"); err != nil {
		return nil, err
	}

	var opts []entra.ApplicationOption
	if s.Authority != "" {
		opts = append(opts, entra.WithAuthority(s.Authority))
	}
	app, err := entra.NewApplication(entra.Credentials{
		Tenant: s.Tenant,
		ID:     s.ClientID,
		Secret: secret,
	}, opts...)
	if err != nil {
		return nil, err
	}

	dir := []entra.Option{entra.WithName(s.Name)}
	if s.Endpoint != "" {
		dir = append(dir, entra.WithEndpoint(s.Endpoint))
	}
	return entra.New(app, dir...)
}

func (s spec) google(key string) (directory.Directory, error) {
	if s.Subject == "" {
		return nil, errors.New("no subject, and a service account with no administrator to act for has no directory of its own")
	}
	if err := s.unused(Google, "tenant", "client_id"); err != nil {
		return nil, err
	}

	// The credential here is the whole key file the console hands over rather
	// than one field out of it, so that the file it arrived in is the file that
	// gets mounted and nobody has to copy a private key from one place to
	// another to deploy this.
	creds, err := google.CredentialsFromJSON([]byte(key), s.Subject)
	if err != nil {
		return nil, err
	}

	var opts []google.ServiceAccountOption
	if s.Authority != "" {
		opts = append(opts, google.WithTokenEndpoint(s.Authority))
	}
	account, err := google.NewServiceAccount(creds, opts...)
	if err != nil {
		return nil, err
	}

	dir := []google.Option{google.WithName(s.Name)}
	if s.Endpoint != "" {
		dir = append(dir, google.WithEndpoint(s.Endpoint))
	}
	return google.New(account, dir...)
}

// credential reads the one thing that is never in the description.
func (s spec) credential(getenv func(string) string) (string, error) {
	switch {
	case s.Credential != "":
		return "", errors.New(`the credential itself does not belong in this file, name the file or the environment variable that holds it with "credential_file" or "credential_env"`)
	case s.CredentialFile != "" && s.CredentialEnv != "":
		return "", errors.New("both credential_file and credential_env are set, and there is no rule about which of them wins")
	case s.CredentialFile != "":
		raw, err := os.ReadFile(s.CredentialFile)
		if err != nil {
			return "", fmt.Errorf("reading the credential: %w", err)
		}
		// Trimmed because a token in a file put there by a person has a newline
		// on the end of it, and a token with a newline on the end is refused by
		// the provider with something that does not mention the newline.
		got := strings.TrimSpace(string(raw))
		if got == "" {
			return "", fmt.Errorf("the credential in %s is empty", s.CredentialFile)
		}
		return got, nil
	case s.CredentialEnv != "":
		got := strings.TrimSpace(getenv(s.CredentialEnv))
		if got == "" {
			return "", fmt.Errorf("%s is not set, and it is where the credential was to come from", s.CredentialEnv)
		}
		return got, nil
	}
	return "", errors.New(`no credential, so name the file or the environment variable that holds it with "credential_file" or "credential_env"`)
}

// unused refuses a field that means nothing to the provider named.
//
// A tenant on an Okta description is somebody who copied one of these from the
// other and changed half of it. Ignoring it would leave them with a deployment
// pointed at the organisation in the other half of the file.
func (s spec) unused(kind string, named ...string) error {
	set := map[string]string{
		"endpoint":  s.Endpoint,
		"tenant":    s.Tenant,
		"client_id": s.ClientID,
		"authority": s.Authority,
		"subject":   s.Subject,
	}
	for _, field := range named {
		if set[field] != "" {
			return fmt.Errorf("%q means nothing to %s and this description sets it", field, kind)
		}
	}
	return nil
}

// probe is the id [Reachable] asks about. It is not a person anywhere and it
// says what it is, so an administrator reading a log of lookups against their
// own tenant can see why one arrived from us at startup.
const probe = "genba-startup-credential-check"

// Reachable asks a directory one question, so that a credential it will not
// accept stops the server starting.
//
// The question is about somebody who does not exist, and the answer that means
// everything is working is that they do not exist. Anything a provider will
// answer at all takes a credential it accepted, and there is no lookup in any
// of these APIs that costs less than one that finds nothing.
//
// A subject the provider does hold is fine too, in the unlikely event somebody
// is named after this, and so is one it has deactivated. Both are answers, and
// the only thing being asked here is whether the provider is answering.
func Reachable(ctx context.Context, d directory.Directory) error {
	_, err := d.Subject(ctx, probe)
	switch {
	case err == nil,
		errors.Is(err, directory.ErrNoSubject),
		errors.Is(err, directory.ErrDisabled):
		return nil
	}
	return fmt.Errorf("%s: %w", d.Name(), err)
}
