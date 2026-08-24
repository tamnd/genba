package okta_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tamnd/genba/directory"
)

// token is what the fake expects, and it is not a secret in a recording because
// the recording is of this and not of anybody's organisation.
const token = "00fakeApiTokenForTests"

// org is a fake Okta organisation.
//
// It is enough of one to be wrong against: the three endpoints the adapter
// reads, the SSWS scheme, the Link header pagination, and the two timestamps
// Okta keeps on a group. What it is not is a mock. Nothing here asserts that a
// particular call was made, because the whole point of the conformance suite is
// that the adapter is judged on its answers.
type org struct {
	mu     sync.Mutex
	users  map[string]*person
	groups map[string]*team

	// clock stands in for a timestamp and moves whenever anything is written,
	// so that a revision is a revision and not a wall clock the tests would
	// have to sleep for.
	clock int

	// page is how many groups it will put in one page of a listing.
	page int

	// calls counts requests per route, for the cases that are about how many
	// requests an expansion costs rather than what it answers.
	calls map[string]int
}

type person struct {
	id, status                string
	updated                   string
	login, email, secondEmail string
	first, last, display      string
	memberOf                  []string
}

type team struct {
	id, name           string
	updated, memberSet string
}

// newOrg starts a fake organisation and returns it with its base URL.
func newOrg(t *testing.T) (fake *org, base string) {
	t.Helper()
	o := &org{
		users:  map[string]*person{},
		groups: map[string]*team{},
		page:   200,
		calls:  map[string]int{},
	}
	srv := httptest.NewServer(o.routes())
	t.Cleanup(srv.Close)
	return o, srv.URL
}

func (o *org) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/users/{id}", o.user)
	mux.HandleFunc("GET /api/v1/users/{id}/groups", o.memberships)
	mux.HandleFunc("GET /api/v1/groups/{id}", o.group)
	return o.authorised(mux)
}

// authorised is the SSWS scheme. An API token is not a bearer token and Okta
// refuses it under the other name, so the fake does too, otherwise an adapter
// that sent the wrong one would pass every test here and work against nobody's
// organisation.
func (o *org) authorised(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "SSWS "+token {
			refuse(w, http.StatusUnauthorized, "E0000011", "Invalid token provided")
			return
		}
		if r.Header.Get("Accept") != "application/json" {
			refuse(w, http.StatusNotAcceptable, "E0000009", "The request was not acceptable")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (o *org) user(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.calls["user"]++
	u, ok := o.users[r.PathValue("id")]
	body := any(nil)
	if ok {
		body = u.json()
	}
	o.mu.Unlock()

	if !ok {
		refuse(w, http.StatusNotFound, "E0000007", "Not found: Resource not found: "+r.PathValue("id")+" (User)")
		return
	}
	send(w, body)
}

func (o *org) group(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.calls["group"]++
	g, ok := o.groups[r.PathValue("id")]
	body := any(nil)
	if ok {
		body = g.json()
	}
	o.mu.Unlock()

	if !ok {
		refuse(w, http.StatusNotFound, "E0000007", "Not found: Resource not found: "+r.PathValue("id")+" (UserGroup)")
		return
	}
	send(w, body)
}

// memberships is the listing the adapter reads a whole group set out of, one
// page at a time.
func (o *org) memberships(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	o.mu.Lock()
	o.calls["memberships"]++
	u, ok := o.users[id]
	var held []*team
	if ok {
		// Sorted, because a cursor over an unordered listing is a page boundary
		// that moves and a group that is served twice or not at all.
		for _, name := range slices.Sorted(slices.Values(u.memberOf)) {
			if g, ok := o.groups[name]; ok {
				held = append(held, g)
			}
		}
	}
	size := o.page
	o.mu.Unlock()

	if !ok {
		refuse(w, http.StatusNotFound, "E0000007", "Not found: Resource not found: "+id+" (User)")
		return
	}

	if limit, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && limit > 0 && limit < size {
		size = limit
	}
	after := r.URL.Query().Get("after")
	if after != "" {
		held = held[min(cursor(held, after), len(held)):]
	}

	// The self link carries a filter with a comma in it, which is legal and is
	// the thing a Link header parser that splits on every comma gets wrong.
	self := fmt.Sprintf(`<%s?%s>; rel="self"`, o.url(r), url.Values{
		"filter": {`type eq "OKTA_GROUP", "APP_GROUP"`},
	}.Encode())
	w.Header().Add("Link", self)

	if len(held) > size {
		held = held[:size]
		next := url.Values{"limit": {strconv.Itoa(size)}, "after": {last(held)}}
		w.Header().Add("Link", fmt.Sprintf(`<%s?%s>; rel="next"`, o.url(r), next.Encode()))
	}
	page := make([]map[string]any, 0, len(held))
	for _, g := range held {
		page = append(page, g.json())
	}
	send(w, page)
}

// url is this request without its query, which is what a link back to it is
// built from.
func (o *org) url(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.Path
}

// cursor is where in the listing the page after the given id starts.
func cursor(held []*team, after string) int {
	for i, g := range held {
		if g.id == after {
			return i + 1
		}
	}
	return len(held)
}

func last(held []*team) string { return held[len(held)-1].id }

// put adds or replaces one person, in the shape the conformance suite hands
// them over in.
func (o *org) put(t *testing.T, s directory.Subject) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()

	o.clock++
	u := &person{
		id:       s.ID,
		status:   "ACTIVE",
		updated:  o.stamp(),
		email:    s.Email,
		display:  s.Name,
		memberOf: slices.Clone(s.MemberOf),
	}
	if s.Disabled {
		u.status = "SUSPENDED"
	}
	if u.email == "" {
		u.email = s.ID + "@example.test"
	}
	u.login = u.email

	// Membership is a fact about the group in Okta rather than about the
	// person, so joining or leaving one moves the group's membership revision.
	// The adapter is expected not to build a version out of that, and this is
	// what makes the case about it mean something.
	held := map[string]bool{}
	if was, ok := o.users[s.ID]; ok {
		for _, g := range was.memberOf {
			held[g] = true
		}
	}
	for _, g := range s.MemberOf {
		if held[g] {
			delete(held, g)
			continue
		}
		o.touch(g)
	}
	for g := range held {
		o.touch(g)
	}
	o.users[s.ID] = u
}

// putGroup adds or replaces one group.
func (o *org) putGroup(t *testing.T, g directory.Group) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()

	o.clock++
	name := g.Name
	if name == "" {
		name = g.ID
	}
	o.groups[g.ID] = &team{id: g.ID, name: name, updated: o.stamp(), memberSet: o.stamp()}
}

// touch moves a group's membership revision without moving the group itself.
func (o *org) touch(id string) {
	if g, ok := o.groups[id]; ok {
		o.clock++
		g.memberSet = o.stamp()
	}
}

// stamp is a revision in the shape Okta writes one, moving with the counter
// rather than with a wall clock nothing would want to wait for.
func (o *org) stamp() string {
	return fmt.Sprintf("2026-01-01T00:00:00.%03dZ", o.clock)
}

// count is how many times a route has been asked.
func (o *org) count(route string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls[route]
}

// forget resets the counts, for a case that wants to measure one expansion.
func (o *org) forget() {
	o.mu.Lock()
	defer o.mu.Unlock()
	clear(o.calls)
}

func (u *person) json() map[string]any {
	return map[string]any{
		"id":          u.id,
		"status":      u.status,
		"created":     "2026-01-01T00:00:00.000Z",
		"lastUpdated": u.updated,
		"profile": map[string]any{
			"login":       u.login,
			"email":       u.email,
			"secondEmail": u.secondEmail,
			"firstName":   u.first,
			"lastName":    u.last,
			"displayName": u.display,
		},
	}
}

func (g *team) json() map[string]any {
	return map[string]any{
		"id":                    g.id,
		"created":               "2026-01-01T00:00:00.000Z",
		"lastUpdated":           g.updated,
		"lastMembershipUpdated": g.memberSet,
		"type":                  "OKTA_GROUP",
		"profile": map[string]any{
			"name":        g.name,
			"description": "",
		},
	}
}

func send(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	// The numbers Okta puts on every answer, which is what the transport reads
	// to slow itself down before it is refused rather than after.
	w.Header().Set("X-Rate-Limit-Limit", "600")
	w.Header().Set("X-Rate-Limit-Remaining", "599")
	w.Header().Set("X-Rate-Limit-Reset", "1767225600")
	_ = json.NewEncoder(w).Encode(body)
}

func refuse(w http.ResponseWriter, status int, code, summary string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errorCode":    code,
		"errorSummary": summary,
		"errorLink":    code,
		"errorId":      "oaeFakeRequestId",
		"errorCauses":  []any{},
	})
}

// edit changes one person in place, for the account states and profile fields
// the conformance suite has no vocabulary for.
func (o *org) edit(t *testing.T, id string, change func(*person)) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	u, ok := o.users[id]
	if !ok {
		t.Fatalf("the fake organisation holds nobody called %q", id)
	}
	change(u)
}

// describe is a group set written the way a failure is easiest to read.
func describe(members []string) string { return strings.Join(members, " ") }
