package entra_test

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tamnd/genba/directory"
	"github.com/tamnd/genba/directory/entra"
)

func TestTheApplicationSignsInOnceAndTheTokenIsUsedAfterThat(t *testing.T) {
	o, endpoint := newTenant(t)
	o.putGroup(t, directory.Group{ID: "engineering"})
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"engineering"}})

	d, err := entra.New(application(t, endpoint), entra.WithEndpoint(endpoint), entra.WithBuffer(0, 0), entra.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		expand(t, d, "mei")
	}
	// A token lasts an hour and this took a moment, so signing in more than
	// once here would be a token endpoint asked for something it already gave.
	if n := o.spent("token"); n != 1 {
		t.Errorf("five expansions signed in %d times, want once", n)
	}
}

func TestATokenIsReplacedBeforeItExpiresRatherThanAfter(t *testing.T) {
	o, endpoint := newTenant(t)
	o.put(t, directory.Subject{ID: "mei"})

	// The fake hands out a token good for 3599 seconds and the application
	// replaces one with less than five minutes left on it, so this clock moves
	// to a point where the token is still valid and not valid for long.
	var elapsed atomic.Int64
	now := func() time.Time {
		return time.Unix(1<<30, 0).Add(time.Duration(elapsed.Load()))
	}
	tokens, err := entra.NewApplication(
		entra.Credentials{Tenant: "acme.onmicrosoft.com", ID: "an-application", Secret: "a-client-secret"},
		entra.WithAuthority(endpoint),
		entra.WithTokenClock(now),
		entra.WithTokenClient(http.DefaultClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	d, err := entra.New(tokens, entra.WithEndpoint(endpoint), entra.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}

	expand(t, d, "mei")
	if n := o.spent("token"); n != 1 {
		t.Fatalf("the first expansion signed in %d times, want once", n)
	}

	// Fifty six minutes in, which is four minutes before it expires and a
	// minute past the point where it is worth replacing.
	elapsed.Store(int64(56 * time.Minute))
	expand(t, d, "mei")
	if n := o.spent("token"); n != 2 {
		t.Errorf("a token with four minutes left was used for a request instead of being replaced, after %d sign ins", n)
	}

	// And having replaced it, the fresh one is used rather than replaced again.
	expand(t, d, "mei")
	if n := o.spent("token"); n != 2 {
		t.Errorf("a token acquired a moment ago was replaced anyway, after %d sign ins", n)
	}
}

func TestASecretTheTenantRefusesSaysWhatIsWrongWithIt(t *testing.T) {
	_, endpoint := newTenant(t)
	tokens, err := entra.NewApplication(
		entra.Credentials{Tenant: "acme.onmicrosoft.com", ID: "an-application", Secret: "the-old-secret"},
		entra.WithAuthority(endpoint),
	)
	if err != nil {
		t.Fatal(err)
	}
	d, err := entra.New(tokens, entra.WithEndpoint(endpoint), entra.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}

	_, err = attempt(t, d, "mei")
	if err == nil {
		t.Fatal("a refused secret resolved somebody anyway")
	}
	// The code is what an operator searches for, and this one means the secret
	// expired, which is the single most common reason an Entra integration
	// stops working.
	if !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Errorf("a refused secret gave %v, which does not say why", err)
	}
	// And the trace ids are not in it. They belong in a support ticket rather
	// than in every line of a log.
	if strings.Contains(err.Error(), "Trace ID") {
		t.Errorf("a refused secret gave %v, which is three lines of correlation ids", err)
	}
	if !strings.Contains(err.Error(), "an-application") {
		t.Errorf("a refused secret gave %v, which does not say which application it was", err)
	}
}

func TestSigningInHappensOnceWhenSeveralLookupsNoticeAtTheSameTime(t *testing.T) {
	o, endpoint := newTenant(t)
	for i := range 20 {
		o.putGroup(t, directory.Group{ID: "g" + string(rune('a'+i%26))})
	}
	o.put(t, directory.Subject{ID: "mei", MemberOf: []string{"ga", "gb", "gc"}})

	d, err := entra.New(application(t, endpoint), entra.WithEndpoint(endpoint), entra.WithBuffer(0, 0), entra.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}

	// An expansion looks a level up in parallel, so the moment a token is
	// needed is a moment several goroutines need one. Each of them going to get
	// their own would turn every expiry into a small burst against the endpoint
	// most likely to throttle.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := attempt(t, d, "mei"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if n := o.spent("token"); n != 1 {
		t.Errorf("eight expansions at once signed in %d times, want once", n)
	}
}

func TestNewApplicationRefusesCredentialsItCannotSignInWith(t *testing.T) {
	for _, c := range []struct {
		name  string
		creds entra.Credentials
	}{
		{"no tenant", entra.Credentials{ID: "an-application", Secret: "a-client-secret"}},
		{"no application", entra.Credentials{Tenant: "acme.onmicrosoft.com", Secret: "a-client-secret"}},
		{"no secret", entra.Credentials{Tenant: "acme.onmicrosoft.com", ID: "an-application"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := entra.NewApplication(c.creds); err == nil {
				t.Error("that was accepted, and the failure would arrive at the first lookup instead")
			}
		})
	}
}

func TestAnEmptyTokenIsRefusedRatherThanSent(t *testing.T) {
	if _, err := entra.Token("").Token(t.Context()); err == nil {
		t.Error("an empty token was handed out, and the tenant would answer every lookup with a refusal")
	}
}

// application is the client credentials grant against the fake tenant.
func application(t *testing.T, endpoint string) *entra.Application {
	t.Helper()
	a, err := entra.NewApplication(
		entra.Credentials{Tenant: "acme.onmicrosoft.com", ID: "an-application", Secret: "a-client-secret"},
		entra.WithAuthority(endpoint),
		entra.WithTokenClient(http.DefaultClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
