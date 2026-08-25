package google_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/genba/directory"
	"github.com/tamnd/genba/directory/google"
)

// account is the service account key these cases sign with.
//
// One key for the whole package, because generating an RSA key is the slowest
// thing in this file by a wide margin and none of these cases is about which key
// it is. A failure here is a machine that cannot make a key at all, which is not
// something a test can report usefully, so it panics rather than pretending to
// be a case that failed.
var account = sync.OnceValue(func() signer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	return signer{
		key:     key,
		encoded: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	}
})

type signer struct {
	key     *rsa.PrivateKey
	encoded string
}

// theAdmin is the person the delegation in these cases was granted for.
const theAdmin = "admin@acme.test"

// delegated is a domain whose token endpoint trusts the package key, and the
// credentials that go with it.
func delegated(t *testing.T) (fake *domain, endpoint string, creds google.Credentials) {
	t.Helper()
	o, endpoint := newDomain(t)
	o.trust(&account().key.PublicKey, theAdmin)
	return o, endpoint, google.Credentials{
		Email:      "genba@acme.iam.gserviceaccount.com",
		PrivateKey: account().encoded,
		KeyID:      "0123456789abcdef",
		Subject:    theAdmin,
	}
}

// TestAServiceAccountSignsInAndResolvesSomebody is the whole grant end to end:
// a JWT this package built and signed, checked by something that has only the
// public key, and then a directory lookup with the token that came back.
func TestAServiceAccountSignsInAndResolvesSomebody(t *testing.T) {
	o, endpoint, creds := delegated(t)
	o.putGroup(t, directory.Group{ID: "engineering", Name: "Engineering"})
	o.put(t, directory.Subject{ID: "mei", Name: "Mei", MemberOf: []string{"engineering"}})

	tokens, err := google.NewServiceAccount(creds,
		google.WithTokenEndpoint(endpoint+"/token"),
		google.WithTokenClient(http.DefaultClient),
	)
	if err != nil {
		t.Fatal(err)
	}

	d, err := google.New(tokens, google.WithEndpoint(endpoint), google.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	got := expand(t, d, "mei")
	if len(got.Groups.Members) != 1 {
		t.Fatalf("mei resolved to %v, want one group", got.Groups.Members)
	}
	if n := o.spent("token"); n != 1 {
		t.Errorf("one expansion cost %d grants, want 1", n)
	}
}

// TestAGrantForSomebodyTheDelegationDoesNotCoverSaysWhy is the failure an
// operator actually hits, and the one that is impossible to diagnose from the
// status alone.
func TestAGrantForSomebodyTheDelegationDoesNotCoverSaysWhy(t *testing.T) {
	_, endpoint, creds := delegated(t)
	creds.Subject = "somebody.else@acme.test"

	tokens, err := google.NewServiceAccount(creds,
		google.WithTokenEndpoint(endpoint+"/token"),
		google.WithTokenClient(http.DefaultClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tokens.Token(t.Context())
	if err == nil {
		t.Fatal("a grant for somebody the delegation does not cover was accepted")
	}
	// The description is the whole of what there is to go on. Without it this is
	// a bad request against an endpoint that will not say which of the four
	// things that can be wrong is wrong.
	if !strings.Contains(err.Error(), "unauthorized to retrieve access tokens") {
		t.Errorf("the refusal came back as %v, which does not carry the reason", err)
	}
	// And it names both halves, because the account and the person it is acting
	// for are configured in different places by different people.
	for _, want := range []string{creds.Email, creds.Subject} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal came back as %v, which does not mention %q", err, want)
		}
	}
}

// TestATokenIsHeldUntilItIsNearlyExpired is what makes this worth having at all.
// A grant per lookup would be a request to the endpoint most likely to throttle
// in front of every request to the one being read.
func TestATokenIsHeldUntilItIsNearlyExpired(t *testing.T) {
	o, endpoint, creds := delegated(t)

	now := time.Now()
	tokens, err := google.NewServiceAccount(creds,
		google.WithTokenEndpoint(endpoint+"/token"),
		google.WithTokenClient(http.DefaultClient),
		google.WithTokenClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}

	for range 5 {
		if _, err := tokens.Token(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := o.spent("token"); n != 1 {
		t.Fatalf("five lookups cost %d grants, want 1", n)
	}

	// The fake hands out a token good for an hour, so this is inside it and
	// inside the five minutes before it expires. A token with four seconds left
	// is a request that arrives after it expired, and that failure is
	// indistinguishable from a credential somebody revoked.
	now = now.Add(56 * time.Minute)
	if _, err := tokens.Token(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := o.spent("token"); n != 2 {
		t.Errorf("a token close to expiring was asked for again %d times, want 2 grants in all", n)
	}
}

// TestOneTokenIsAcquiredWhenEverybodyNoticesAtOnce is the case the lock is
// there for. An expansion looks a level up in parallel, so the moment a token
// expires is the moment several goroutines find out together.
func TestOneTokenIsAcquiredWhenEverybodyNoticesAtOnce(t *testing.T) {
	o, endpoint, creds := delegated(t)
	tokens, err := google.NewServiceAccount(creds,
		google.WithTokenEndpoint(endpoint+"/token"),
		google.WithTokenClient(http.DefaultClient),
	)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tokens.Token(t.Context()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if n := o.spent("token"); n != 1 {
		t.Errorf("sixteen goroutines noticing at once cost %d grants, want 1", n)
	}
}

func TestCredentialsFromJSONReadsAServiceAccountKey(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"type":           "service_account",
		"project_id":     "acme-search",
		"private_key_id": "0123456789abcdef",
		"private_key":    account().encoded,
		"client_email":   "genba@acme.iam.gserviceaccount.com",
		"client_id":      "112233445566778899",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatal(err)
	}

	creds, err := google.CredentialsFromJSON(raw, theAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Email != "genba@acme.iam.gserviceaccount.com" {
		t.Errorf("the account came out as %q", creds.Email)
	}
	if creds.KeyID != "0123456789abcdef" {
		t.Errorf("the key id came out as %q, so a rotation would sign with a key the console cannot name", creds.KeyID)
	}
	if creds.Subject != theAdmin {
		t.Errorf("the administrator came out as %q", creds.Subject)
	}
	if _, err := google.NewServiceAccount(creds); err != nil {
		t.Errorf("the credentials off a real key file were refused: %v", err)
	}
}

// TestAKeyFileThatIsNotAServiceAccountIsRefused is about the mistake rather than
// about the format. A user credential file has the same extension, lives in the
// same directory and is a different thing entirely.
func TestAKeyFileThatIsNotAServiceAccountIsRefused(t *testing.T) {
	raw := []byte(`{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`)
	if _, err := google.CredentialsFromJSON(raw, theAdmin); err == nil {
		t.Error("a user credential file was accepted as a service account key")
	}
}

// TestNewServiceAccountRefusesWhatWouldFailLater is the whole reason the key is
// parsed at construction. Every one of these produces a directory that comes up
// fine and refuses every lookup once somebody searches.
func TestNewServiceAccountRefusesWhatWouldFailLater(t *testing.T) {
	good := google.Credentials{
		Email:      "genba@acme.iam.gserviceaccount.com",
		PrivateKey: account().encoded,
		Subject:    theAdmin,
	}
	for _, c := range []struct {
		name  string
		creds google.Credentials
		opts  []google.ServiceAccountOption
	}{
		{"no service account", google.Credentials{PrivateKey: good.PrivateKey, Subject: theAdmin}, nil},
		{"no private key", google.Credentials{Email: good.Email, Subject: theAdmin}, nil},
		{"nobody to act for", google.Credentials{Email: good.Email, PrivateKey: good.PrivateKey}, nil},
		{"a path where the key should be", google.Credentials{Email: good.Email, PrivateKey: "/etc/genba/key.pem", Subject: theAdmin}, nil},
		{"a key that is not a key", google.Credentials{Email: good.Email, Subject: theAdmin,
			PrivateKey: "-----BEGIN PRIVATE KEY-----\nbm90IGEga2V5\n-----END PRIVATE KEY-----\n"}, nil},
		{"no token endpoint", good, []google.ServiceAccountOption{google.WithTokenEndpoint("")}},
		{"no scopes", good, []google.ServiceAccountOption{google.WithScopes()}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := google.NewServiceAccount(c.creds, c.opts...); err == nil {
				t.Error("that was accepted, and the failure would arrive at the first search instead")
			}
		})
	}
}

// TestAGrantAskingForNothingUsefulIsRefusedByTheEndpoint says that the scopes
// actually travel. Domain wide delegation is configured by pasting a list of
// them next to a client id, and the two halves have to agree.
func TestAGrantAskingForNothingUsefulIsRefusedByTheEndpoint(t *testing.T) {
	_, endpoint, creds := delegated(t)
	tokens, err := google.NewServiceAccount(creds,
		google.WithTokenEndpoint(endpoint+"/token"),
		google.WithTokenClient(http.DefaultClient),
		google.WithScopes(google.ScopeUsers),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Token(t.Context()); err == nil {
		t.Error("a grant that cannot read groups was accepted, so a directory would come up resolving everybody into nothing")
	}
}
