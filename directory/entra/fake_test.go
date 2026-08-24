package entra_test

import (
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

// A fake tenant, because the alternative is a suite that only runs for whoever
// has a directory to point it at.
//
// It is deliberately more awkward than it needs to be in two places. A user
// lookup that did not ask for accountEnabled fails the test rather than
// quietly answering without it, and the membership collection without the cast
// on the end of it returns a directory role alongside the groups. Both are what
// Graph does, and both are mistakes an adapter can make without anything above
// it noticing.

// bearer is the token the fake accepts, and the one its token endpoint hands
// out.
const bearer = "eyJ0eXAiOiJKV1QifQ.fakeGraphTokenForTests"

// role is the directory role every person here holds. It exists to be the thing
// that must not end up in a group set.
const role = "62e90394-69f5-4237-9190-012177145e10"

type person struct {
	name     string
	mail     string
	upn      string
	other    []string
	enabled  bool
	memberOf []string
}

type team struct {
	name     string
	memberOf []string
}

type tenant struct {
	t *testing.T

	mu     sync.Mutex
	users  map[string]*person
	groups map[string]*team
	calls  map[string]int
	base   string

	// secret is what the token endpoint insists on before it hands one out.
	secret string

	// quiet leaves accountEnabled out of a user, which is what Graph does when
	// nobody asked for it.
	quiet bool
}

// silent makes the tenant answer a user lookup without the account state, the
// way the real one answers a lookup that did not name it.
func (o *tenant) silent() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.quiet = true
}

// newTenant starts a fake Graph and returns it along with its endpoint.
func newTenant(t *testing.T) (fake *tenant, endpoint string) {
	t.Helper()
	o := &tenant{
		t:      t,
		users:  map[string]*person{},
		groups: map[string]*team{},
		calls:  map[string]int{},
		secret: "a-client-secret",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", o.token)
	mux.Handle("GET /v1.0/users/{id}", o.authorised(o.user))
	mux.Handle("GET /v1.0/users/{id}/transitiveMemberOf", o.authorised(o.uncast))
	mux.Handle("GET /v1.0/users/{id}/transitiveMemberOf/microsoft.graph.group", o.authorised(o.memberships))
	mux.Handle("GET /v1.0/groups/{id}", o.authorised(o.group))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	o.base = srv.URL
	return o, srv.URL
}

// authorised refuses anything that did not arrive as a bearer, the way Graph
// does, so a test cannot pass with a token the adapter never sent.
func (o *tenant) authorised(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			o.refuse(w, http.StatusUnauthorized, "InvalidAuthenticationToken", "Access token is empty or invalid.")
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "json") {
			o.refuse(w, http.StatusNotAcceptable, "NotAcceptable", "The request asked for something this endpoint does not produce.")
			return
		}
		h(w, r)
	})
}

func (o *tenant) token(w http.ResponseWriter, r *http.Request) {
	o.count("token")
	if err := r.ParseForm(); err != nil {
		o.refuse(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	o.mu.Lock()
	secret := o.secret
	o.mu.Unlock()

	if r.PostForm.Get("client_secret") != secret {
		// The shape of a real refusal, correlation id and all, because the
		// adapter trims one of these down to the sentence worth reading.
		o.write(w, http.StatusUnauthorized, map[string]any{
			"error":             "invalid_client",
			"error_description": "AADSTS7000215: Invalid client secret provided.\r\nTrace ID: 0000\r\nCorrelation ID: 0000",
		})
		return
	}
	o.write(w, http.StatusOK, map[string]any{
		"token_type":   "Bearer",
		"expires_in":   3599,
		"access_token": bearer,
	})
}

func (o *tenant) user(w http.ResponseWriter, r *http.Request) {
	o.count("user")
	id := r.PathValue("id")

	// The whole reason the adapter names its properties. A lookup that did not
	// ask for accountEnabled gets an answer without it, and an adapter reading
	// that answer decides whether somebody still works here from a field that
	// is not in front of it.
	o.mu.Lock()
	p, ok := o.users[id]
	quiet := o.quiet
	o.mu.Unlock()

	if selected := r.URL.Query().Get("$select"); !quiet && !slices.Contains(strings.Split(selected, ","), "accountEnabled") {
		o.t.Errorf("a user lookup selected %q, which does not include accountEnabled, so the answer would not say whether the account is live", selected)
	}
	if !ok {
		o.refuse(w, http.StatusNotFound, "Request_ResourceNotFound",
			fmt.Sprintf("Resource '%s' does not exist or one of its queried reference-property objects are not present.", id))
		return
	}

	body := map[string]any{
		"id":                id,
		"displayName":       p.name,
		"mail":              nullable(p.mail),
		"userPrincipalName": p.upn,
		"otherMails":        emptyList(p.other),
		"accountEnabled":    p.enabled,
	}
	if quiet {
		delete(body, "accountEnabled")
	}
	o.write(w, http.StatusOK, body)
}

// uncast is the collection without microsoft.graph.group on the end of it,
// which answers with the directory roles as well.
func (o *tenant) uncast(w http.ResponseWriter, r *http.Request) {
	o.count("uncast")
	o.list(w, r, true)
}

func (o *tenant) memberships(w http.ResponseWriter, r *http.Request) {
	o.count("memberships")
	o.list(w, r, false)
}

func (o *tenant) list(w http.ResponseWriter, r *http.Request, roles bool) {
	id := r.PathValue("id")

	o.mu.Lock()
	_, ok := o.users[id]
	held := o.closure(id)
	o.mu.Unlock()
	if !ok {
		o.refuse(w, http.StatusNotFound, "Request_ResourceNotFound",
			fmt.Sprintf("Resource '%s' does not exist or one of its queried reference-property objects are not present.", id))
		return
	}

	value := make([]map[string]any, 0, len(held)+1)
	if roles {
		value = append(value, map[string]any{
			"@odata.type": "#microsoft.graph.directoryRole",
			"id":          role,
			"displayName": "Global Reader",
		})
	}

	size := len(held)
	if top := r.URL.Query().Get("$top"); top != "" {
		n, err := strconv.Atoi(top)
		if err != nil || n < 1 {
			o.refuse(w, http.StatusBadRequest, "Request_BadRequest", "Invalid value for $top.")
			return
		}
		size = n
	}
	from := 0
	if skip := r.URL.Query().Get("$skiptoken"); skip != "" {
		n, err := strconv.Atoi(skip)
		if err != nil || n < 0 {
			o.refuse(w, http.StatusBadRequest, "Request_BadRequest", "Invalid skip token.")
			return
		}
		from = min(n, len(held))
	}
	to := min(from+size, len(held))

	for _, gid := range held[from:to] {
		o.mu.Lock()
		g := o.groups[gid]
		o.mu.Unlock()
		value = append(value, map[string]any{
			"@odata.type": "#microsoft.graph.group",
			"id":          gid,
			"displayName": g.name,
		})
	}

	body := map[string]any{"value": value}
	if to < len(held) {
		// Absolute, and carrying everything the first request carried, which is
		// what makes following it verbatim the only sensible thing to do.
		next := *r.URL
		q := next.Query()
		q.Set("$skiptoken", strconv.Itoa(to))
		next.RawQuery = q.Encode()
		body["@odata.nextLink"] = o.base + next.Path + "?" + next.RawQuery
	}
	o.write(w, http.StatusOK, body)
}

func (o *tenant) group(w http.ResponseWriter, r *http.Request) {
	o.count("group")
	id := r.PathValue("id")

	o.mu.Lock()
	g, ok := o.groups[id]
	o.mu.Unlock()
	if !ok {
		o.refuse(w, http.StatusNotFound, "Request_ResourceNotFound",
			fmt.Sprintf("Resource '%s' does not exist or one of its queried reference-property objects are not present.", id))
		return
	}
	o.write(w, http.StatusOK, map[string]any{"id": id, "displayName": g.name})
}

// closure is every group the person is in, however deeply, which is the whole
// of what this provider does that the others do not. The caller holds the lock.
func (o *tenant) closure(id string) []string {
	p, ok := o.users[id]
	if !ok {
		return nil
	}
	var (
		out   []string
		seen  = map[string]bool{}
		queue = slices.Clone(p.memberOf)
	)
	for len(queue) > 0 {
		g := queue[0]
		queue = queue[1:]
		if seen[g] {
			continue
		}
		seen[g] = true
		// A membership naming a group the tenant does not hold is not something
		// Graph can answer with, because the collection returns the objects
		// rather than their ids.
		team, ok := o.groups[g]
		if !ok {
			continue
		}
		out = append(out, g)
		queue = append(queue, team.memberOf...)
	}
	slices.Sort(out)
	return out
}

// put adds or replaces one person.
func (o *tenant) put(t *testing.T, s directory.Subject) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.users[s.ID] = &person{
		name:     s.Name,
		mail:     s.Email,
		upn:      s.ID + "@acme.test",
		enabled:  !s.Disabled,
		memberOf: slices.Clone(s.MemberOf),
	}
}

// putGroup adds or replaces one group.
func (o *tenant) putGroup(t *testing.T, g directory.Group) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.groups[g.ID] = &team{name: g.Name, memberOf: slices.Clone(g.MemberOf)}
}

// edit changes one person, for the things the conformance suite has no
// vocabulary for.
func (o *tenant) edit(t *testing.T, id string, change func(*person)) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.users[id]
	if !ok {
		t.Fatalf("there is nobody called %q in the tenant", id)
	}
	change(p)
}

// spent is how many requests of one kind arrived.
func (o *tenant) spent(kind string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls[kind]
}

func (o *tenant) count(kind string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls[kind]++
}

func (o *tenant) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		o.t.Errorf("writing the answer: %v", err)
	}
}

func (o *tenant) refuse(w http.ResponseWriter, status int, code, message string) {
	o.write(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// nullable is how Graph reports a mailbox somebody does not have.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// emptyList is how Graph reports a collection with nothing in it, which is not
// the same as leaving the property out.
func emptyList(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// describe joins ids for a failure worth reading.
func describe(in []string) string { return strings.Join(in, " ") }
