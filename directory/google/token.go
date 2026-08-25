package google

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tamnd/genba/connector/limit"
)

// Tokens hands out a bearer token for the Admin SDK.
//
// It is an interface because a Google access token lives for about an hour,
// which makes a fixed string the wrong shape for anything that runs longer than
// that. An adapter built on one would work all afternoon and start refusing
// every lookup at some point in the evening, and the symptom above it is not an
// error saying the token expired, it is everybody losing their groups at once.
type Tokens interface {
	// Token returns a token that is valid now.
	Token(ctx context.Context) (string, error)
}

// Token is a bearer token somebody else obtained.
//
// It is for a test, for a script, and for a deployment where something outside
// this process is already responsible for keeping a token fresh. It is not for a
// daemon holding one it acquired at startup, for the reason in [Tokens].
type Token string

// Token returns the token, which is the whole of what this type does.
func (t Token) Token(context.Context) (string, error) {
	if t == "" {
		return "", errors.New("google: the token is empty")
	}
	return string(t), nil
}

// The scopes this adapter needs, which are the read only halves of the two
// collections it reads and nothing else.
//
// They are separate constants because they are granted separately. Domain wide
// delegation is configured by pasting a list of scopes into an admin console
// next to a client id, and an operator who pasted one of these and not the other
// gets a directory that resolves people and finds them in no groups.
const (
	ScopeUsers  = "https://www.googleapis.com/auth/admin.directory.user.readonly"
	ScopeGroups = "https://www.googleapis.com/auth/admin.directory.group.readonly"
)

// DefaultTokenEndpoint is where a token comes from.
const DefaultTokenEndpoint = "https://oauth2.googleapis.com/token"

// DefaultEarly is how long before a token expires it is replaced.
//
// It is not tuning. A token that is checked, found to have four seconds left and
// then used is a request that arrives after it expired, and the failure is
// indistinguishable from a credential that was revoked. Five minutes is far
// longer than any request here takes and far shorter than a token lives.
const DefaultEarly = 5 * time.Minute

// assertionLife is how long the signed statement asking for a token is good
// for, and it is the longest the service accepts. It has nothing to do with how
// long the token that comes back lives.
const assertionLife = time.Hour

// Credentials is a service account and the administrator it acts as.
//
// The service account is an identity with no directory of its own, so on its own
// it resolves nothing: every lookup is refused or answers about a domain with
// nobody in it. What makes it work is domain wide delegation, where an
// administrator grants the account's client id a list of scopes and the account
// then asks for a token on behalf of a named person. That person is [Subject],
// and they have to be somebody in the domain who can read the directory.
//
// The three fields before it come out of the JSON key file the console hands
// over when the account is created, and [CredentialsFromJSON] reads one.
type Credentials struct {
	// Email is the service account, which the key file calls client_email.
	Email string

	// PrivateKey is the account's key, in PEM, which the key file calls
	// private_key.
	PrivateKey string

	// KeyID is which of the account's keys this is, which the key file calls
	// private_key_id. It is optional and it goes in the header of the assertion
	// so that a service account with two keys can have one of them rotated.
	KeyID string

	// Subject is the administrator to act as. Without one the token is refused,
	// and the refusal is the same one an ungranted scope produces.
	Subject string
}

// keyFile is the JSON a service account key is handed over as.
type keyFile struct {
	Type       string `json:"type"`
	Email      string `json:"client_email"`
	PrivateKey string `json:"private_key"`
	KeyID      string `json:"private_key_id"`
}

// CredentialsFromJSON reads a service account key file and pairs it with the
// administrator to act as.
//
// It exists so that the key stays in a file. A private key is the one thing here
// that must not arrive on a command line, where it is in the process listing for
// everybody on the host and in the shell history of whoever started it.
func CredentialsFromJSON(raw []byte, subject string) (Credentials, error) {
	var f keyFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return Credentials{}, fmt.Errorf("google: reading the service account key: %w", err)
	}
	// A user credential file has the same extension, lives in the same place and
	// is a different thing entirely, and the failure it causes without this is a
	// missing private key three steps later.
	if f.Type != "" && f.Type != "service_account" {
		return Credentials{}, fmt.Errorf("google: the key is for a %q rather than a service account", f.Type)
	}
	return Credentials{
		Email:      f.Email,
		PrivateKey: f.PrivateKey,
		KeyID:      f.KeyID,
		Subject:    subject,
	}, nil
}

// ServiceAccount signs in with the JWT bearer grant and holds the token it gets
// until shortly before it expires.
//
// It is safe for concurrent use and it acquires one token at a time. Holding the
// lock across the request is deliberate: an expansion looks up a level of the
// graph in parallel, so the moment a token expires is the moment several
// goroutines notice at once, and letting each of them go and get one would turn
// every expiry into a small burst against the endpoint most likely to throttle.
type ServiceAccount struct {
	creds    Credentials
	key      *rsa.PrivateKey
	http     *http.Client
	endpoint string
	scopes   []string
	early    time.Duration
	now      func() time.Time

	mu    sync.Mutex
	token string
	until time.Time
}

var _ Tokens = (*ServiceAccount)(nil)

// ServiceAccountOption configures a [ServiceAccount].
type ServiceAccountOption func(*ServiceAccount)

// WithTokenEndpoint replaces where tokens are asked for, which a test needs.
//
// It is also what the assertion is addressed to, because the audience of a
// signed statement is whoever it is being presented to. Changing one without the
// other would produce an assertion the endpoint refuses to look at.
func WithTokenEndpoint(endpoint string) ServiceAccountOption {
	return func(a *ServiceAccount) { a.endpoint = endpoint }
}

// WithScopes replaces what the token is asked to be good for.
func WithScopes(scopes ...string) ServiceAccountOption {
	return func(a *ServiceAccount) { a.scopes = scopes }
}

// WithTokenClient replaces the client the token is fetched with.
func WithTokenClient(c *http.Client) ServiceAccountOption {
	return func(a *ServiceAccount) {
		if c != nil {
			a.http = c
		}
	}
}

// WithEarly sets how long before expiry a token is replaced. A duration below
// zero selects [DefaultEarly].
func WithEarly(d time.Duration) ServiceAccountOption {
	return func(a *ServiceAccount) { a.early = d }
}

// WithTokenClock replaces the clock, for a test that needs a token to expire
// without waiting an hour for it.
func WithTokenClock(now func() time.Time) ServiceAccountOption {
	return func(a *ServiceAccount) {
		if now != nil {
			a.now = now
		}
	}
}

// NewServiceAccount returns a [Tokens] that signs in as a service account acting
// for an administrator.
//
// The key is parsed here rather than at the first lookup, so a key file that is
// truncated, encrypted or of the wrong kind stops a deployment from starting
// instead of turning into a directory outage the first time somebody searches.
func NewServiceAccount(c Credentials, opts ...ServiceAccountOption) (*ServiceAccount, error) {
	switch {
	case c.Email == "":
		return nil, errors.New("google: a service account address is required")
	case c.PrivateKey == "":
		return nil, errors.New("google: a private key is required")
	case c.Subject == "":
		// Refused rather than defaulted, because there is nothing to default to
		// and the alternative is the least readable failure this adapter has: a
		// token endpoint refusing an unauthorized_client, which is the same
		// answer an ungranted scope gives.
		return nil, errors.New("google: an administrator to act as is required, since a service account has no directory of its own")
	}
	key, err := parseKey(c.PrivateKey)
	if err != nil {
		return nil, err
	}

	a := &ServiceAccount{
		creds:    c,
		key:      key,
		http:     &http.Client{Transport: limit.NewTransport(limit.Limits{})},
		endpoint: DefaultTokenEndpoint,
		scopes:   []string{ScopeUsers, ScopeGroups},
		early:    DefaultEarly,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.early < 0 {
		a.early = DefaultEarly
	}
	if a.endpoint == "" {
		return nil, errors.New("google: the token endpoint cannot be empty")
	}
	if len(a.scopes) == 0 {
		return nil, errors.New("google: a token with no scopes on it is good for nothing")
	}
	return a, nil
}

// parseKey reads the PEM a service account key file carries.
//
// Both encodings are accepted because both turn up. The console writes PKCS 8
// and a key that has been through a conversion tool at some point is often
// PKCS 1, and the difference is invisible to whoever pasted it into a secret
// store.
func parseKey(key string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return nil, errors.New("google: the private key is not PEM, so it is probably a path or a truncated copy of one")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("google: the private key is a %T, and the grant is signed with RSA", parsed)
		}
		return rsaKey, nil
	}
	parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("google: reading the private key: %w", err)
	}
	return parsed, nil
}

// Token returns a token that is valid now, signing in again if the one being
// held is close enough to expiring to be worth replacing.
func (a *ServiceAccount) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && a.now().Before(a.until.Add(-a.early)) {
		return a.token, nil
	}
	token, lasts, err := a.acquire(ctx)
	if err != nil {
		// Leaving the old token where it is. It is either expired, in which case
		// the next caller tries again and gets the same error, or it has a few
		// minutes left, in which case a token endpoint having a bad moment costs
		// nothing at all.
		return "", err
	}
	a.token, a.until = token, a.now().Add(lasts)
	return a.token, nil
}

// acquire is one JWT bearer grant.
func (a *ServiceAccount) acquire(ctx context.Context) (token string, lasts time.Duration, err error) {
	assertion, err := a.assertion(a.now())
	if err != nil {
		return "", 0, err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("google: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("google: signing in as %q: %w", a.creds.Email, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	var body struct {
		Token       string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(raw, &body)

	if resp.StatusCode != http.StatusOK {
		// The description is where the reason lives, and here it is nearly
		// always the same one: the account's client id has not been granted
		// these scopes in the admin console, or the person it is acting for
		// cannot read the directory. Both arrive as unauthorized_client and
		// neither is guessable from the status alone.
		if body.Description != "" {
			return "", 0, fmt.Errorf("google: signing in as %q for %q: %s: %s",
				a.creds.Email, a.creds.Subject, resp.Status, firstLine(body.Description))
		}
		return "", 0, fmt.Errorf("google: signing in as %q for %q: %s", a.creds.Email, a.creds.Subject, resp.Status)
	}
	if body.Token == "" {
		return "", 0, fmt.Errorf("google: signing in as %q: the answer carried no token", a.creds.Email)
	}
	lasts = time.Duration(body.ExpiresIn) * time.Second
	if lasts <= 0 {
		// A grant with no lifetime on it is one this holds for as long as it
		// dares rather than one it holds forever.
		lasts = a.early + time.Minute
	}
	return body.Token, lasts, nil
}

// assertion is the signed statement the grant is made with.
//
// The subject claim is what turns a service account into an administrator, and
// it is the whole of domain wide delegation as far as this process is concerned.
// Without it the account asks about its own directory, which is empty, and the
// answers are a domain where nobody exists rather than an error saying the
// delegation was never configured.
func (a *ServiceAccount) assertion(now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if a.creds.KeyID != "" {
		header["kid"] = a.creds.KeyID
	}
	claims := map[string]any{
		"iss":   a.creds.Email,
		"sub":   a.creds.Subject,
		"scope": strings.Join(a.scopes, " "),
		"aud":   a.endpoint,
		"iat":   now.Unix(),
		"exp":   now.Add(assertionLife).Unix(),
	}

	encoded, err := segments(header, claims)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("google: signing the grant: %w", err)
	}
	return encoded + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// segments is the part of a JWT that gets signed, which is the two documents
// base64 encoded and joined with a dot.
func segments(parts ...any) (string, error) {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		raw, err := json.Marshal(part)
		if err != nil {
			return "", fmt.Errorf("google: building the grant: %w", err)
		}
		out = append(out, base64.RawURLEncoding.EncodeToString(raw))
	}
	return strings.Join(out, "."), nil
}
