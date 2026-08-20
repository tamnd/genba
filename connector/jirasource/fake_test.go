package jirasource_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Jira site with the parts that matter and none of the parts that do not. It
// has projects with permission schemes and roles, issues with descriptions and
// comments, issue security levels, offset paging, enough JQL to answer the two
// queries this adapter asks, a clock that only moves when something happens to
// it, and the ability to be told to refuse.
//
// It exists so that these tests do not need a site, and it is also what the
// committed recording under testdata was made from. Everything asserted against
// the recording is asserted against this first, so the two cannot drift without
// a test going red.

// start is when everything in these tests happens.
//
// JQL compares times to the minute, so the clock moves a minute at a time. A
// fake that ticked by a second would make every test agree with every query for
// the wrong reason.
var start = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

type site struct {
	mu       sync.Mutex
	clock    time.Time
	projects []*proj
	issues   []*tick
	levels   map[string]*level
	calls    map[string]int
	fields   map[string]string

	// named is what a test called a ticket, which is not what Jira calls it.
	// The suite writes a document called a.md and the site answers with LINE-1.
	named map[string]string

	// fail makes one path refuse with a status, and hidden makes a project's
	// permission scheme refuse the way it does for an account that is not an
	// administrator.
	fail   map[string]int
	hidden map[string]bool

	// throttle is how many more times the next request is answered with 429
	// before it is answered properly.
	throttle int

	// page is how many items go in one page, which is small on purpose so that
	// the paging is exercised by an ordinary sync rather than by one test.
	page int
}

type proj struct {
	id, key, name string
	grants        []grant
	roles         map[string]*role
	counter       int
}

type grant struct {
	kind, param string
}

type role struct {
	users  []string
	groups []string
}

type level struct {
	id, name string
	users    []string
	groups   []string
	roles    []int
}

type tick struct {
	key, project string
	summary      string
	description  any
	reporter     string
	assignee     string
	created      time.Time
	updated      time.Time
	status       string
	kind         string
	priority     string
	labels       []string
	security     string
	comments     []*note
}

type note struct {
	id      string
	author  string
	body    any
	created time.Time
	updated time.Time
}

func newSite() *site {
	s := &site{
		clock:  start,
		levels: make(map[string]*level),
		calls:  make(map[string]int),
		fields: make(map[string]string),
		named:  make(map[string]string),
		fail:   make(map[string]int),
		hidden: make(map[string]bool),
		page:   2,
	}
	p := s.addProject("LINE", "Line two")
	p.grants = []grant{{"projectRole", "10002"}}
	p.roles["10002"] = &role{users: []string{"acc-mei"}, groups: []string{"engineering"}}
	return s
}

// now is the site's clock, which is what the permission refresh schedule is
// measured against.
func (s *site) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clock
}

// tick moves the clock on a minute and returns where it got to.
//
// A minute rather than a second, because JQL compares times to the minute. A
// fake whose events all happened in the same minute would agree with every
// query for the wrong reason.
func (s *site) tick() time.Time {
	s.clock = s.clock.Add(time.Minute)
	return s.clock
}

// advance moves the clock without anything happening, which is how a test gets
// to the far side of a permission refresh interval without waiting for it.
func (s *site) advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = s.clock.Add(d)
}

func (s *site) addProject(key, name string) *proj {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &proj{
		id:    strconv.Itoa(10000 + len(s.projects)),
		key:   key,
		name:  name,
		roles: make(map[string]*role),
	}
	s.projects = append(s.projects, p)
	return p
}

func (s *site) project(key string) *proj {
	for _, p := range s.projects {
		if p.key == key {
			return p
		}
	}
	return nil
}

// addLevel makes an issue security level readable by some people.
func (s *site) addLevel(id, name string, users ...string) *level {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := &level{id: id, name: name, users: users}
	s.levels[id] = l
	return l
}

// file raises a ticket and returns its key.
func (s *site) file(project, summary, description string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.project(project)
	p.counter++
	at := s.tick()
	t := &tick{
		key:         fmt.Sprintf("%s-%d", project, p.counter),
		project:     project,
		summary:     summary,
		description: adf(description),
		reporter:    "acc-mei",
		created:     at,
		updated:     at,
		status:      "To Do",
		kind:        "Bug",
		priority:    "Medium",
	}
	s.issues = append(s.issues, t)
	s.named[summary] = t.key
	return t.key
}

// home is the project the conformance suite works in.
const home = "LINE"

// write files a ticket under a name, or rewrites the one already filed under
// it, which is what the conformance suite means by putting a document in a
// source.
func (s *site) write(name, body string) {
	s.mu.Lock()
	t := s.find(s.named[name])
	if t != nil {
		t.description = adf(body)
		t.updated = s.tick()
	}
	s.mu.Unlock()

	if t == nil {
		s.file(home, name, body)
	}
}

// keyOf is the Jira key of whatever a test filed under a name.
//
// A name nothing was ever filed under still gets a key, because the question
// being asked of it is what this source would call that document and the answer
// does not depend on whether the document is there. Jira numbers issues from
// one, so nothing on the site is ever LINE-0.
func (s *site) keyOf(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key := s.named[name]; key != "" {
		return key
	}
	return home + "-0"
}

// assign puts a ticket on somebody, which is a change with no words in it.
func (s *site) assign(key, who string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(key)
	t.assignee = who
	t.updated = s.tick()
}

// label tags a ticket.
func (s *site) label(key string, labels ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(key)
	t.labels = labels
	t.updated = s.tick()
}

// describe replaces a ticket's description with a document rather than with a
// sentence, which is how the rendering is tested against the real shape.
func (s *site) describe(key string, description any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(key)
	t.description = description
	t.updated = s.tick()
}

// hide makes a project's permission scheme unreadable, which is what a site
// does to an account that is a user rather than an administrator.
func (s *site) hide(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hidden[key] = true
}

// refuse makes one path answer with a status until it is told otherwise.
func (s *site) refuse(path string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[path] = status
}

// slowDown makes the next n requests come back as 429, which is how Jira asks a
// crawler to wait.
func (s *site) slowDown(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttle = n
}

func (s *site) find(key string) *tick {
	for _, t := range s.issues {
		if t.key == key {
			return t
		}
	}
	return nil
}

// comment adds to the argument underneath a ticket, which moves the ticket.
func (s *site) comment(key, author, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(key)
	at := s.tick()
	t.comments = append(t.comments, &note{
		id:      strconv.Itoa(20000 + len(t.comments) + len(s.issues)*10),
		author:  author,
		body:    adf(body),
		created: at,
		updated: at,
	})
	t.updated = at
}

// transition moves a ticket's status, which is a change with nothing written in
// it and is the case a source with no updated field could never report.
func (s *site) transition(key, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(key)
	t.status = status
	t.updated = s.tick()
}

// restrict puts a security level on a ticket.
func (s *site) restrict(key, levelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.find(key)
	t.security = levelID
	t.updated = s.tick()
}

// remove deletes a ticket. Nothing in JQL reports that it was ever there, which
// is why the sweep exists.
func (s *site) remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The name stays behind on purpose. A test that deletes a ticket goes on to
	// ask what its id was, and a fake that forgot would answer a different
	// question.
	s.issues = slices.DeleteFunc(s.issues, func(t *tick) bool { return t.key == key })
	s.tick()
}

func (s *site) counted(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path]
}

// lastFields is the fields parameter of the last request to a path, which is
// how a test checks that a crawl asked for what it needed and not for a site's
// three hundred custom fields.
func (s *site) lastFields(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fields[path]
}

func (s *site) resetCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = make(map[string]int)
}

// server starts the site on a listener and returns its base URL.
func (s *site) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (s *site) handle(rw http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	if err := req.ParseForm(); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.calls[path]++
	s.fields[path] = req.Form.Get("fields")
	if s.throttle > 0 {
		s.throttle--
		s.mu.Unlock()
		rw.Header().Set("Retry-After", "0")
		rw.WriteHeader(http.StatusTooManyRequests)
		return
	}
	status, refusing := s.fail[path]
	s.mu.Unlock()

	if refusing {
		s.deny(rw, status, "no")
		return
	}
	// Basic authentication with an email and an API token, which is what a real
	// site wants and what a request that forgot the header does not have.
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Basic ") {
		s.deny(rw, http.StatusUnauthorized, "unauthorised")
		return
	}

	switch {
	case path == "/rest/api/3/project/search":
		s.listProjects(rw, req)
	case path == "/rest/api/3/search":
		s.search(rw, req)
	case path == "/rest/api/3/issuesecurityschemes/level/member":
		s.levelMembers(rw, req)
	case strings.HasSuffix(path, "/permissionscheme"):
		s.scheme(rw, path)
	case strings.Contains(path, "/role/"):
		s.roleActors(rw, path)
	case strings.HasSuffix(path, "/comment"):
		s.listComments(rw, req, path)
	case strings.HasPrefix(path, "/rest/api/3/issue/"):
		s.readIssue(rw, path)
	default:
		s.deny(rw, http.StatusNotFound, "no such endpoint")
	}
}

func (s *site) deny(rw http.ResponseWriter, status int, why string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_, _ = rw.Write([]byte(`{"errorMessages":["` + why + `"],"errors":{}}`))
}

func (s *site) send(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}

func (s *site) listProjects(rw http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	rows := make([]map[string]any, 0, len(s.projects))
	for _, p := range s.projects {
		rows = append(rows, map[string]any{"id": p.id, "key": p.key, "name": p.name})
	}
	s.mu.Unlock()

	at, size := paging(req, s.page)
	page, last := window(rows, at, size)
	s.send(rw, map[string]any{
		"startAt":    at,
		"maxResults": size,
		"total":      len(rows),
		"isLast":     last,
		"values":     page,
	})
}

func (s *site) scheme(rw http.ResponseWriter, path string) {
	key := segment(path, "/rest/api/3/project/", "/permissionscheme")
	s.mu.Lock()
	p, hidden := s.project(key), s.hidden[key]
	var rows []map[string]any
	if p != nil {
		for _, g := range p.grants {
			rows = append(rows, map[string]any{
				"holder":     map[string]any{"type": g.kind, "parameter": g.param},
				"permission": "BROWSE_PROJECTS",
			})
		}
	}
	s.mu.Unlock()

	switch {
	case p == nil:
		s.deny(rw, http.StatusNotFound, "no such project")
	case hidden:
		s.deny(rw, http.StatusForbidden, "administrator only")
	default:
		s.send(rw, map[string]any{"id": 10100, "name": "scheme", "permissions": rows})
	}
}

func (s *site) roleActors(rw http.ResponseWriter, path string) {
	rest := strings.TrimPrefix(path, "/rest/api/3/project/")
	key, id, _ := strings.Cut(rest, "/role/")

	s.mu.Lock()
	p := s.project(key)
	var r *role
	if p != nil {
		r = p.roles[id]
	}
	var rows []map[string]any
	if r != nil {
		for _, u := range r.users {
			rows = append(rows, map[string]any{
				"type": "atlassian-user-role-actor", "displayName": u,
				"actorUser": map[string]any{"accountId": u},
			})
		}
		for _, g := range r.groups {
			rows = append(rows, map[string]any{
				"type": "atlassian-group-role-actor", "displayName": g,
				"actorGroup": map[string]any{"name": g},
			})
		}
	}
	s.mu.Unlock()

	if r == nil {
		s.deny(rw, http.StatusNotFound, "no such role")
		return
	}
	s.send(rw, map[string]any{"id": id, "name": "Role", "actors": rows})
}

func (s *site) levelMembers(rw http.ResponseWriter, req *http.Request) {
	id := req.Form.Get("levelId")

	s.mu.Lock()
	l := s.levels[id]
	var rows []map[string]any
	if l != nil {
		for _, u := range l.users {
			rows = append(rows, map[string]any{
				"id": "m" + u, "issueSecurityLevelId": id,
				"holder": map[string]any{"type": "user", "user": map[string]any{"accountId": u}},
			})
		}
		for _, g := range l.groups {
			rows = append(rows, map[string]any{
				"id": "m" + g, "issueSecurityLevelId": id,
				"holder": map[string]any{"type": "group", "group": map[string]any{"name": g}},
			})
		}
		for _, r := range l.roles {
			rows = append(rows, map[string]any{
				"id": "mr" + strconv.Itoa(r), "issueSecurityLevelId": id,
				"holder": map[string]any{"type": "projectRole", "projectRole": map[string]any{"id": r}},
			})
		}
	}
	s.mu.Unlock()

	if l == nil {
		s.deny(rw, http.StatusForbidden, "administrator only")
		return
	}
	at, size := paging(req, s.page)
	page, last := window(rows, at, size)
	s.send(rw, map[string]any{
		"startAt": at, "maxResults": size, "total": len(rows), "isLast": last, "values": page,
	})
}

func (s *site) readIssue(rw http.ResponseWriter, path string) {
	key := strings.TrimPrefix(path, "/rest/api/3/issue/")
	s.mu.Lock()
	t := s.find(key)
	var row map[string]any
	if t != nil {
		row = s.row(t, s.page)
	}
	s.mu.Unlock()

	if t == nil {
		s.deny(rw, http.StatusNotFound, "no such issue")
		return
	}
	s.send(rw, row)
}

func (s *site) listComments(rw http.ResponseWriter, req *http.Request, path string) {
	key := segment(path, "/rest/api/3/issue/", "/comment")
	s.mu.Lock()
	t := s.find(key)
	var rows []map[string]any
	if t != nil {
		for _, c := range t.comments {
			rows = append(rows, s.note(c))
		}
	}
	s.mu.Unlock()

	if t == nil {
		s.deny(rw, http.StatusNotFound, "no such issue")
		return
	}
	at, size := paging(req, s.page)
	page, _ := window(rows, at, size)
	s.send(rw, map[string]any{
		"startAt": at, "maxResults": size, "total": len(rows), "comments": page,
	})
}

// jqlProject and jqlUpdated are as much JQL as this fake understands, which is
// exactly the two clauses the adapter writes. A fake that parsed the whole
// language would be a second Jira with its own bugs in it.
var (
	jqlProject = regexp.MustCompile(`project\s*=\s*"([^"]+)"`)
	jqlUpdated = regexp.MustCompile(`updated\s*>=\s*"([^"]+)"`)
)

func (s *site) search(rw http.ResponseWriter, req *http.Request) {
	jql := req.Form.Get("jql")

	m := jqlProject.FindStringSubmatch(jql)
	if m == nil {
		s.deny(rw, http.StatusBadRequest, "expected a project clause: "+jql)
		return
	}
	key := m[1]

	var since time.Time
	if u := jqlUpdated.FindStringSubmatch(jql); u != nil {
		got, err := time.Parse("2006-01-02 15:04", u[1])
		if err != nil {
			s.deny(rw, http.StatusBadRequest, "bad time in "+jql)
			return
		}
		since = got.UTC()
	}

	s.mu.Lock()
	p, hidden := s.project(key), s.hidden[key]
	var found []*tick
	for _, t := range s.issues {
		if t.project == key && (since.IsZero() || !t.updated.Before(since)) {
			found = append(found, t)
		}
	}
	if strings.Contains(jql, "ORDER BY key") {
		slices.SortFunc(found, func(a, b *tick) int { return strings.Compare(a.key, b.key) })
	} else {
		slices.SortFunc(found, func(a, b *tick) int {
			if d := a.updated.Compare(b.updated); d != 0 {
				return d
			}
			return strings.Compare(a.key, b.key)
		})
	}
	// The fields a search asks for are honoured, because a crawl that asked for
	// two fields and got thirty would never notice it was billing for thirty.
	want := strings.Split(req.Form.Get("fields"), ",")
	rows := make([]map[string]any, 0, len(found))
	for _, t := range found {
		rows = append(rows, only(s.row(t, s.page), want))
	}
	s.mu.Unlock()

	switch {
	case p == nil:
		s.deny(rw, http.StatusBadRequest, "no such project: "+key)
		return
	case hidden && p.grants == nil:
		s.deny(rw, http.StatusForbidden, "you may not browse "+key)
		return
	}

	at, size := paging(req, s.page)
	page, _ := window(rows, at, size)
	s.send(rw, map[string]any{
		"startAt": at, "maxResults": size, "total": len(rows), "issues": page,
	})
}

// row is one issue the way the API reports it.
func (s *site) row(t *tick, size int) map[string]any {
	fields := map[string]any{
		"summary":     t.summary,
		"description": t.description,
		"created":     jiraTime(t.created),
		"updated":     jiraTime(t.updated),
		"reporter":    who(t.reporter),
		"creator":     who(t.reporter),
		"status":      map[string]any{"name": t.status},
		"issuetype":   map[string]any{"name": t.kind},
	}
	if len(t.labels) > 0 {
		fields["labels"] = t.labels
	}
	if t.assignee != "" {
		fields["assignee"] = who(t.assignee)
	}
	if t.priority != "" {
		fields["priority"] = map[string]any{"name": t.priority}
	}
	if t.security != "" {
		l := s.levels[t.security]
		name := t.security
		if l != nil {
			name = l.name
		}
		fields["security"] = map[string]any{"id": t.security, "name": name}
	}

	// A search returns the first page of comments inline and says how many
	// there are in total, which is what tells an adapter whether it has them
	// all.
	rows := make([]map[string]any, 0, len(t.comments))
	for _, c := range t.comments {
		rows = append(rows, s.note(c))
	}
	page, _ := window(rows, 0, size)
	fields["comment"] = map[string]any{
		"startAt": 0, "maxResults": size, "total": len(rows), "comments": page,
	}

	return map[string]any{"id": "i" + t.key, "key": t.key, "fields": fields}
}

func (s *site) note(c *note) map[string]any {
	return map[string]any{
		"id":      c.id,
		"author":  who(c.author),
		"body":    c.body,
		"created": jiraTime(c.created),
		"updated": jiraTime(c.updated),
	}
}

// only keeps the fields a search asked for.
func only(row map[string]any, want []string) map[string]any {
	fields, _ := row["fields"].(map[string]any)
	kept := make(map[string]any, len(want))
	for _, name := range want {
		name = strings.TrimSpace(name)
		if v, ok := fields[name]; ok {
			kept[name] = v
		}
	}
	return map[string]any{"id": row["id"], "key": row["key"], "fields": kept}
}

func who(id string) map[string]any {
	names := map[string]string{
		"acc-mei": "Mei Tanaka",
		"acc-sam": "Sam Okafor",
		"acc-lee": "Lee Berger",
	}
	name := names[id]
	if name == "" {
		name = id
	}
	return map[string]any{
		"accountId":    id,
		"displayName":  name,
		"emailAddress": strings.TrimPrefix(id, "acc-") + "@acme.com",
		"active":       true,
	}
}

// adf wraps a plain string in the smallest Atlassian document that holds it, so
// that a test writing a sentence gets the shape the real API sends.
func adf(text string) map[string]any {
	if text == "" {
		return nil
	}
	content := make([]any, 0, 1)
	for _, para := range strings.Split(text, "\n\n") {
		content = append(content, map[string]any{
			"type":    "paragraph",
			"content": []any{map[string]any{"type": "text", "text": para}},
		})
	}
	return map[string]any{"type": "doc", "version": 1, "content": content}
}

// jiraTime is the format a Jira site writes a timestamp in, which is ISO 8601
// with milliseconds and a numeric zone with no colon in it.
func jiraTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000-0700")
}

// paging reads the offset and size off a request.
func paging(req *http.Request, fallback int) (at, size int) {
	at, _ = strconv.Atoi(req.Form.Get("startAt"))
	size, _ = strconv.Atoi(req.Form.Get("maxResults"))
	if size <= 0 {
		size = fallback
	}
	// A real site caps what it will return whatever was asked for, which is the
	// behaviour that catches an adapter trusting maxResults.
	return at, min(size, fallback)
}

// window cuts a page out of a listing and says whether it is the last one.
func window[T any](items []T, at, size int) (page []T, last bool) {
	if at > len(items) {
		at = len(items)
	}
	to := min(at+size, len(items))
	return items[at:to], to >= len(items)
}

// segment pulls the middle out of a path.
func segment(path, prefix, suffix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
}
