package entra

import (
	"context"
	"encoding/json"
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

// Tokens hands out a bearer token for the Graph.
//
// It is an interface because a Graph token lives for about an hour, which makes
// a fixed string the wrong shape for anything that runs longer than that. An
// adapter built on one would work all afternoon and start refusing every lookup
// at some point in the evening, and the symptom above it is not an error that
// says the token expired, it is everybody losing their groups at once.
type Tokens interface {
	// Token returns a token that is valid now.
	Token(ctx context.Context) (string, error)
}

// Token is a bearer token somebody else obtained.
//
// It is for a test, for a script, and for a deployment where something outside
// this process is already responsible for keeping a token fresh. It is not for
// a daemon holding one it acquired at startup, for the reason in [Tokens].
type Token string

// Token returns the token, which is the whole of what this type does.
func (t Token) Token(context.Context) (string, error) {
	if t == "" {
		return "", errors.New("entra: the token is empty")
	}
	return string(t), nil
}

// Credentials is an application in the tenant signing in as itself.
//
// The application needs User.Read.All and GroupMember.Read.All as application
// permissions, granted by an administrator. Delegated permissions are the wrong
// kind here: they resolve the groups of whoever is signed in, and this resolves
// the groups of whoever was asked about.
type Credentials struct {
	// Tenant is the directory to sign in to, either the tenant id or a domain
	// such as acme.onmicrosoft.com.
	Tenant string

	// ID is the application, which Entra calls the client id.
	ID string

	// Secret is the client secret.
	Secret string
}

// DefaultAuthority is where a token comes from.
const DefaultAuthority = "https://login.microsoftonline.com"

// DefaultScope is what the token is asked to be good for. The .default suffix
// means the application permissions an administrator already granted, rather
// than a list this process gets to name for itself.
const DefaultScope = "https://graph.microsoft.com/.default"

// DefaultEarly is how long before a token expires it is replaced.
//
// It is not tuning. A token that is checked, found to have four seconds left
// and then used is a request that arrives after it expired, and the failure is
// indistinguishable from a credential that was revoked. Five minutes is far
// longer than any request here takes and far shorter than a token lives.
const DefaultEarly = 5 * time.Minute

// Application signs in with the client credentials grant and holds the token it
// gets until shortly before it expires.
//
// It is safe for concurrent use and it acquires one token at a time. Holding
// the lock across the request is deliberate: an expansion looks up a level of
// the graph in parallel, so the moment a token expires is the moment several
// goroutines notice at once, and letting each of them go and get one would turn
// every expiry into a small burst against the endpoint most likely to throttle.
type Application struct {
	creds     Credentials
	http      *http.Client
	authority string
	scope     string
	early     time.Duration
	now       func() time.Time

	mu    sync.Mutex
	token string
	until time.Time
}

var _ Tokens = (*Application)(nil)

// ApplicationOption configures an [Application].
type ApplicationOption func(*Application)

// WithAuthority replaces where tokens are asked for, which a national cloud and
// a test both need.
func WithAuthority(base string) ApplicationOption {
	return func(a *Application) { a.authority = strings.TrimSuffix(base, "/") }
}

// WithScope replaces what the token is asked to be good for.
func WithScope(scope string) ApplicationOption {
	return func(a *Application) { a.scope = scope }
}

// WithTokenClient replaces the client the token is fetched with.
func WithTokenClient(c *http.Client) ApplicationOption {
	return func(a *Application) {
		if c != nil {
			a.http = c
		}
	}
}

// WithEarly sets how long before expiry a token is replaced. A duration below
// zero selects [DefaultEarly].
func WithEarly(d time.Duration) ApplicationOption {
	return func(a *Application) { a.early = d }
}

// WithTokenClock replaces the clock, for a test that needs a token to expire
// without waiting an hour for it.
func WithTokenClock(now func() time.Time) ApplicationOption {
	return func(a *Application) {
		if now != nil {
			a.now = now
		}
	}
}

// NewApplication returns a [Tokens] that signs in as an application.
func NewApplication(c Credentials, opts ...ApplicationOption) (*Application, error) {
	switch {
	case c.Tenant == "":
		return nil, errors.New("entra: a tenant is required")
	case c.ID == "":
		return nil, errors.New("entra: an application id is required")
	case c.Secret == "":
		return nil, errors.New("entra: a client secret is required")
	}

	a := &Application{
		creds:     c,
		http:      &http.Client{Transport: limit.NewTransport(limit.Limits{})},
		authority: DefaultAuthority,
		scope:     DefaultScope,
		early:     DefaultEarly,
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.early < 0 {
		a.early = DefaultEarly
	}
	if a.authority == "" {
		return nil, errors.New("entra: the authority cannot be empty")
	}
	return a, nil
}

// Token returns a token that is valid now, signing in again if the one being
// held is close enough to expiring to be worth replacing.
func (a *Application) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && a.now().Before(a.until.Add(-a.early)) {
		return a.token, nil
	}
	token, lasts, err := a.acquire(ctx)
	if err != nil {
		// Leaving the old token where it is. It is either expired, in which
		// case the next caller tries again and gets the same error, or it has a
		// few minutes left, in which case a token endpoint having a bad moment
		// costs nothing at all.
		return "", err
	}
	a.token, a.until = token, a.now().Add(lasts)
	return a.token, nil
}

// acquire is one client credentials grant.
func (a *Application) acquire(ctx context.Context) (token string, lasts time.Duration, err error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {a.creds.ID},
		"client_secret": {a.creds.Secret},
		"scope":         {a.scope},
	}
	endpoint := a.authority + "/" + url.PathEscape(a.creds.Tenant) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("entra: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("entra: signing in as %q: %w", a.creds.ID, err)
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
		// The description is where the reason lives, and the reason is nearly
		// always one an operator can act on: a secret that expired, a tenant
		// spelled wrong, a consent nobody granted.
		if body.Description != "" {
			return "", 0, fmt.Errorf("entra: signing in as %q: %s: %s", a.creds.ID, resp.Status, firstLine(body.Description))
		}
		return "", 0, fmt.Errorf("entra: signing in as %q: %s", a.creds.ID, resp.Status)
	}
	if body.Token == "" {
		return "", 0, fmt.Errorf("entra: signing in as %q: the answer carried no token", a.creds.ID)
	}
	lasts = time.Duration(body.ExpiresIn) * time.Second
	if lasts <= 0 {
		// A grant with no lifetime on it is one this holds for as long as it
		// dares rather than one it holds forever.
		lasts = a.early + time.Minute
	}
	return body.Token, lasts, nil
}

// firstLine trims a token endpoint error down to the sentence worth reading.
// The rest of it is a correlation id, a timestamp and a stack of trace ids,
// which belong in a support ticket rather than in a log line.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
