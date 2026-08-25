package google_test

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tamnd/genba/directory"
)

// A fake domain, because the alternative is a suite that only runs for whoever
// has a Workspace tenancy to point it at.
//
// It is deliberately more awkward than it needs to be in three places. The
// groups collection refuses a page size above the two hundred the real one
// accepts, so an adapter that passed a larger number straight through would fail
// here rather than in production. The token endpoint verifies the signature on
// the grant and refuses one with nobody to act for, so domain wide delegation is
// something the tests can be wrong about. And a group listing answers with the
// group objects and nothing about what those groups are inside, which is the
// fact the whole walk is shaped around.

// bearer is the token the fake accepts, and the one its token endpoint hands
// out.
const bearer = "ya29.fakeAdminSdkTokenForTests"

type person struct {
	name      string
	email     string
	aliases   []string
	others    []string
	suspended bool
	archived  bool
	memberOf  []string
}

type team struct {
	name     string
	email    string
	memberOf []string
}

type domain struct {
	t *testing.T

	mu     sync.Mutex
	users  map[string]*person
	groups map[string]*team
	calls  map[string]int
	base   string

	// pub is what the token endpoint checks the grant against, and admin is who
	// the delegation was granted for. Both are empty until a test arms them,
	// because most of these cases are about the directory rather than about
	// signing in.
	pub   *rsa.PublicKey
	admin string

	// owed is how many refusals the directory still has to hand out before it
	// answers anything, and reason is what kind they are.
	owed   int
	reason string

	// bare leaves the etag off every object, which is what a Google API that has
	// stopped sending them looks like.
	bare bool
}

// newDomain starts a fake Admin SDK and returns it along with its endpoint.
func newDomain(t *testing.T) (fake *domain, endpoint string) {
	t.Helper()
	o := &domain{
		t:      t,
		users:  map[string]*person{},
		groups: map[string]*team{},
		calls:  map[string]int{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", o.token)
	mux.Handle("GET /admin/directory/v1/users/{id}", o.authorised(o.user))
	mux.Handle("GET /admin/directory/v1/groups", o.authorised(o.list))
	mux.Handle("GET /admin/directory/v1/groups/{id}", o.authorised(o.group))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	o.base = srv.URL
	return o, srv.URL
}

// authorised refuses anything that did not arrive as a bearer, the way the Admin
// SDK does, so a test cannot pass with a token the adapter never sent. It is
// also where an injected refusal is handed out, since a throttle arrives before
// anything is looked at.
func (o *domain) authorised(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			o.refuse(w, http.StatusUnauthorized, "authError", "Invalid Credentials")
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "json") {
			o.refuse(w, http.StatusNotAcceptable, "notAcceptable", "The request asked for something this endpoint does not produce.")
			return
		}
		o.mu.Lock()
		owed, reason := o.owed, o.reason
		if owed > 0 {
			o.owed--
		}
		o.mu.Unlock()

		if owed > 0 {
			o.count("refused")
			o.refuse(w, http.StatusForbidden, reason, "Quota exceeded for quota metric 'Queries' and limit 'Queries per minute per user'.")
			return
		}
		h(w, r)
	})
}

// throttle makes the next n directory requests refuse with one reason, which is
// how a rate limit and a permission problem are told apart here.
func (o *domain) throttle(n int, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.owed, o.reason = n, reason
}

// forgetEtags is a domain that has stopped sending a revision on anything.
func (o *domain) forgetEtags() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.bare = true
}

// trust arms the token endpoint with the key it verifies grants against and the
// person the delegation was granted for.
func (o *domain) trust(pub *rsa.PublicKey, admin string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pub, o.admin = pub, admin
}

func (o *domain) token(w http.ResponseWriter, r *http.Request) {
	o.count("token")
	if err := r.ParseForm(); err != nil {
		o.deny(w, "invalid_request", err.Error())
		return
	}
	if got := r.PostForm.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		o.deny(w, "unsupported_grant_type", "The grant type "+got+" is not the one a service account uses.")
		return
	}

	o.mu.Lock()
	pub, admin := o.pub, o.admin
	o.mu.Unlock()
	if pub == nil {
		o.deny(w, "invalid_client", "This domain has no service account registered.")
		return
	}

	claims, err := verify(r.PostForm.Get("assertion"), pub, o.base+"/token")
	if err != nil {
		o.deny(w, "invalid_grant", err.Error())
		return
	}
	// The refusal domain wide delegation produces when it was never configured,
	// which is worth having in a test because it is the same one an ungranted
	// scope produces and neither is guessable from the status.
	if sub, _ := claims["sub"].(string); sub != admin {
		o.deny(w, "unauthorized_client",
			"Client is unauthorized to retrieve access tokens using this method, or client not authorized for any of the scopes requested.")
		return
	}
	o.write(w, http.StatusOK, map[string]any{
		"token_type":   "Bearer",
		"expires_in":   3599,
		"access_token": bearer,
	})
}

// verify checks a grant the way the token endpoint does, which is the only way a
// test can say the assertion was actually signed with the account's key rather
// than merely well shaped.
func verify(assertion string, pub *rsa.PublicKey, audience string) (map[string]any, error) {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("the assertion has %d segments, want 3", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("the signature is not base64: %w", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil, fmt.Errorf("the assertion was not signed with this account's key: %w", err)
	}

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("the claims are not base64: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, fmt.Errorf("the claims are not JSON: %w", err)
	}
	if aud, _ := claims["aud"].(string); aud != audience {
		return nil, fmt.Errorf("the assertion is addressed to %q rather than to %q", aud, audience)
	}
	scope, _ := claims["scope"].(string)
	if !strings.Contains(scope, "admin.directory.group") {
		return nil, fmt.Errorf("the grant asks for %q, which cannot read groups", scope)
	}
	return claims, nil
}

func (o *domain) user(w http.ResponseWriter, r *http.Request) {
	o.count("user")
	key := r.PathValue("id")
	if malformed(key) {
		o.refuse(w, http.StatusBadRequest, "invalid", "Invalid Input: "+key)
		return
	}

	o.mu.Lock()
	id, p := o.person(key)
	bare := o.bare
	o.mu.Unlock()
	if p == nil {
		o.refuse(w, http.StatusNotFound, "notFound", "Resource Not Found: userKey")
		return
	}

	emails := make([]map[string]any, 0, len(p.others)+1)
	if p.email != "" {
		emails = append(emails, map[string]any{"address": p.email, "primary": true})
	}
	for _, a := range p.others {
		emails = append(emails, map[string]any{"address": a})
	}
	body := map[string]any{
		"kind":         "admin#directory#user",
		"id":           id,
		"primaryEmail": p.email,
		"name":         map[string]any{"fullName": p.name},
		"aliases":      emptyList(p.aliases),
		"emails":       emails,
		"suspended":    p.suspended,
		"archived":     p.archived,
	}
	if !bare {
		body["etag"] = revision(id, p.name, p.email, strings.Join(p.aliases, ","), strconv.FormatBool(p.suspended), strconv.FormatBool(p.archived))
	}
	o.write(w, http.StatusOK, body)
}

func (o *domain) group(w http.ResponseWriter, r *http.Request) {
	o.count("group")
	key := r.PathValue("id")

	o.mu.Lock()
	id, g := o.team(key)
	bare := o.bare
	o.mu.Unlock()
	if g == nil {
		o.refuse(w, http.StatusNotFound, "notFound", "Resource Not Found: groupKey")
		return
	}
	o.write(w, http.StatusOK, o.object(id, g, bare))
}

// list is the collection that answers both lookups. It takes a userKey which is
// a person or a group, and it says which groups that key is directly in and
// nothing about what those are inside.
func (o *domain) list(w http.ResponseWriter, r *http.Request) {
	o.count("list")
	key := r.URL.Query().Get("userKey")
	switch {
	case key == "":
		// The real one wants a customer or a domain instead, and this adapter
		// has no use for a listing of the whole directory.
		o.refuse(w, http.StatusBadRequest, "required", "Missing required field: userKey")
		return
	case malformed(key):
		o.refuse(w, http.StatusBadRequest, "invalid", "Invalid Input: "+key)
		return
	}

	size := 200
	if raw := r.URL.Query().Get("maxResults"); raw != "" {
		n, err := strconv.Atoi(raw)
		// The ceiling the real endpoint enforces. An adapter that passed a
		// caller's number straight through would turn a configuration mistake
		// into an expansion that reports the person is in no groups.
		if err != nil || n < 1 || n > 200 {
			o.refuse(w, http.StatusBadRequest, "invalid", "Invalid Input: maxResults")
			return
		}
		size = n
	}

	o.mu.Lock()
	held, ok := o.above(key)
	bare := o.bare
	o.mu.Unlock()
	if !ok {
		o.refuse(w, http.StatusNotFound, "notFound", "Resource Not Found: userKey")
		return
	}

	from := 0
	if token := r.URL.Query().Get("pageToken"); token != "" {
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 {
			o.refuse(w, http.StatusBadRequest, "invalid", "Invalid Input: pageToken")
			return
		}
		from = min(n, len(held))
	}
	to := min(from+size, len(held))

	value := make([]map[string]any, 0, to-from)
	for _, id := range held[from:to] {
		o.mu.Lock()
		g := o.groups[id]
		o.mu.Unlock()
		value = append(value, o.object(id, g, bare))
	}

	body := map[string]any{"kind": "admin#directory#groups", "groups": value}
	if to < len(held) {
		body["nextPageToken"] = strconv.Itoa(to)
	}
	o.write(w, http.StatusOK, body)
}

// object is one group the way the API sends it, which is what it is and not
// where it sits.
func (o *domain) object(id string, g *team, bare bool) map[string]any {
	body := map[string]any{
		"kind":  "admin#directory#group",
		"id":    id,
		"email": g.email,
		"name":  g.name,
	}
	if !bare {
		body["etag"] = revision(id, g.name, g.email)
	}
	return body
}

// person finds somebody by id or by any address they answer to. The caller holds
// the lock.
func (o *domain) person(key string) (string, *person) {
	if p, ok := o.users[key]; ok {
		return key, p
	}
	for id, p := range o.users {
		if p.email == key || slices.Contains(p.aliases, key) || slices.Contains(p.others, key) {
			return id, p
		}
	}
	return "", nil
}

// team finds a group by id or by its address. The caller holds the lock.
func (o *domain) team(key string) (string, *team) {
	if g, ok := o.groups[key]; ok {
		return key, g
	}
	for id, g := range o.groups {
		if g.email == key {
			return id, g
		}
	}
	return "", nil
}

// above is the groups one key is directly a member of, and whether the domain
// holds that key at all. The caller holds the lock.
//
// A membership naming a group that is not there is simply absent from the
// answer, because the collection returns the objects rather than their ids and
// an object that is not there is not in the answer.
func (o *domain) above(key string) ([]string, bool) {
	var in []string
	if _, p := o.person(key); p != nil {
		in = p.memberOf
	} else if _, g := o.team(key); g != nil {
		in = g.memberOf
	} else {
		return nil, false
	}

	out := make([]string, 0, len(in))
	for _, id := range in {
		if _, ok := o.groups[id]; ok {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, true
}

// put adds or replaces one person.
func (o *domain) put(t *testing.T, s directory.Subject) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	email := s.Email
	if email == "" {
		email = s.ID + "@acme.test"
	}
	o.users[s.ID] = &person{
		name:      s.Name,
		email:     email,
		suspended: s.Disabled,
		memberOf:  slices.Clone(s.MemberOf),
	}
}

// putGroup adds or replaces one group.
func (o *domain) putGroup(t *testing.T, g directory.Group) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.groups[g.ID] = &team{
		name:     g.Name,
		email:    g.ID + "@acme.test",
		memberOf: slices.Clone(g.MemberOf),
	}
}

// edit changes one person, for the things the conformance suite has no
// vocabulary for.
func (o *domain) edit(t *testing.T, id string, change func(*person)) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.users[id]
	if !ok {
		t.Fatalf("there is nobody called %q in the domain", id)
	}
	change(p)
}

// editGroup changes one group, for the same reason.
func (o *domain) editGroup(t *testing.T, id string, change func(*team)) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	g, ok := o.groups[id]
	if !ok {
		t.Fatalf("there is no group called %q in the domain", id)
	}
	change(g)
}

// spent is how many requests of one kind arrived.
func (o *domain) spent(kind string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls[kind]
}

func (o *domain) count(kind string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls[kind]++
}

func (o *domain) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		o.t.Errorf("writing the answer: %v", err)
	}
}

func (o *domain) refuse(w http.ResponseWriter, status int, reason, message string) {
	o.write(w, status, map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": message,
			"errors": []map[string]any{
				{"domain": "global", "reason": reason, "message": message},
			},
		},
	})
}

// deny is how the token endpoint refuses, which is a different document from the
// one the API refuses with.
func (o *domain) deny(w http.ResponseWriter, code, description string) {
	o.write(w, http.StatusBadRequest, map[string]any{
		"error":             code,
		"error_description": description,
	})
}

// revision is a stand in for an etag: a short string that changes when anything
// it is derived from does.
func revision(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:9]) + `"`
}

// malformed is a key the service will not even look up, which is a real answer
// and not a failure. A string with a space in it is neither an id nor an
// address, and the endpoint says so with a bad request.
func malformed(key string) bool { return strings.ContainsAny(key, " \t") }

// emptyList is how the API reports a collection with nothing in it, which is not
// the same as leaving the property out.
func emptyList(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// describe joins ids for a failure worth reading.
func describe(in []string) string { return strings.Join(in, " ") }
