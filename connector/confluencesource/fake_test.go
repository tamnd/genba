package confluencesource_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Confluence site with the parts that matter and none of the parts that do
// not. It has spaces with read permissions on them, pages with bodies in both
// formats and comments underneath, read restrictions that inherit down a tree,
// two paging models, enough CQL to answer the two queries this adapter asks, a
// clock that only moves when something happens to it, and the ability to be told
// to refuse.
//
// It exists so that these tests do not need a site, and it is also what the
// committed recording under testdata was made from. Everything asserted against
// the recording is asserted against this first, so the two cannot drift without
// a test going red.

// start is when everything in these tests happens.
//
// CQL compares times to the minute, so the clock moves a minute at a time. A
// fake that ticked by a second would make every test agree with every query for
// the wrong reason.
var start = time.Date(2026, 4, 6, 9, 0, 0, 0, time.UTC)

type site struct {
	mu     sync.Mutex
	clock  time.Time
	spaces []*area
	pages  []*leaf
	calls  map[string]int
	expand map[string]string

	// named is what a test called a page, which is not what Confluence calls it.
	// The suite writes a document called a.md and the site answers with 100001.
	named map[string]string

	// fail makes one path refuse with a status, which is how a test asks what
	// happens when a restriction cannot be read at all.
	fail map[string]int

	// throttle is how many more times the next request is answered with 429
	// before it is answered properly.
	throttle int

	// page is how many items go in one page, which is small on purpose so that
	// the paging is exercised by an ordinary sync rather than by one test.
	page int

	// counter is where page ids come from. Confluence ids are numbers and the
	// adapter checks that they are, so a fake handing out names would be testing
	// against something the real site cannot produce.
	counter int
}

// area is a space and who may read it.
type area struct {
	id            int
	key, name     string
	users, groups []string

	// open is a space readable without signing in, and dark is one whose
	// permissions this token was not shown, which on a real site is every space
	// it does not administer.
	open bool
	dark bool
}

// leaf is a page. The name is not "page" because that is what a listing calls
// one of its own results.
type leaf struct {
	id      string
	space   string
	title   string
	body    string
	parent  string
	created time.Time
	updated time.Time
	version int
	author  string
	labels  []string

	// legacy is a page written before the editor changed, which has a body in
	// the storage format and no Atlassian document at all.
	legacy bool

	// archived is a page somebody put away. It stays readable by id and stops
	// being current, which is one of the three ways a page leaves the index.
	archived bool

	rule     *bar
	comments []*note
}

// bar is a read restriction on one page.
type bar struct {
	users, groups []string

	// short is a restriction the site sends only part of, which is what happens
	// when it names more people than fit in one answer. The list is paged like
	// everything else and the size is the only thing that says so.
	short bool
}

type note struct {
	id      string
	author  string
	body    string
	created time.Time
	updated time.Time
}

// home is the space the conformance suite works in.
const home = "OPS"

func newSite() *site {
	s := &site{
		clock:  start,
		calls:  make(map[string]int),
		expand: make(map[string]string),
		named:  make(map[string]string),
		fail:   make(map[string]int),
		page:   2,
	}
	a := s.addSpace(home, "Line two operations")
	a.groups = []string{"engineering"}
	a.users = []string{"acc-mei"}
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
// A minute rather than a second, because CQL compares times to the minute. A
// fake whose events all happened in the same minute would agree with every query
// for the wrong reason.
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

func (s *site) addSpace(key, name string) *area {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &area{id: 100 + len(s.spaces), key: key, name: name}
	s.spaces = append(s.spaces, a)
	return a
}

func (s *site) space(key string) *area {
	for _, a := range s.spaces {
		if a.key == key {
			return a
		}
	}
	return nil
}

// create writes a page and returns its id.
func (s *site) create(space, title, body string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.add(space, title, body)
}

func (s *site) add(space, title, body string) string {
	s.counter++
	at := s.tick()
	p := &leaf{
		id:      strconv.Itoa(100000 + s.counter),
		space:   space,
		title:   title,
		body:    body,
		created: at,
		updated: at,
		version: 1,
		author:  "acc-mei",
	}
	s.pages = append(s.pages, p)
	s.named[title] = p.id
	return p.id
}

// write puts a page in the space the conformance suite works in, or rewrites the
// one already there under that title.
func (s *site) write(title, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.find(s.named[title]); p != nil {
		p.body = body
		p.version++
		p.updated = s.tick()
		return
	}
	s.add(home, title, body)
}

// idOf is the Confluence id of whatever a test wrote under a name.
//
// A name nothing was ever written under still gets an id, because the question
// being asked is what this source would call that document and the answer does
// not depend on whether the document is there. Confluence numbers content from
// well above this, so nothing on the site is ever 1.
func (s *site) idOf(title string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id := s.named[title]; id != "" {
		return id
	}
	return "1"
}

func (s *site) find(id string) *leaf {
	for _, p := range s.pages {
		if p.id == id {
			return p
		}
	}
	return nil
}

// nest puts one page under another, which is what makes a restriction on the
// parent reach the child.
func (s *site) nest(child, parent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.find(child).parent = parent
}

// restrict puts a read restriction on a page.
func (s *site) restrict(id string, users, groups []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.find(id).rule = &bar{users: users, groups: groups}
}

// truncate puts a restriction on a page and has the site send only part of it,
// which is what a restriction naming more people than fit in one answer looks
// like from outside.
func (s *site) truncate(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.find(id).rule = &bar{users: []string{"acc-lee"}, short: true}
}

// comment adds to the argument underneath a page.
//
// It moves the comment and not the page, which is the gap this adapter exists to
// work around: Confluence dates a comment and leaves the page it is on alone.
func (s *site) comment(id, author, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(id)
	at := s.tick()
	p.comments = append(p.comments, &note{
		id:      strconv.Itoa(900000 + len(p.comments) + len(s.pages)*10),
		author:  author,
		body:    body,
		created: at,
		updated: at,
	})
}

// label tags a page, which is a change with no words in it.
func (s *site) label(id string, labels ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(id)
	p.labels = labels
	p.version++
	p.updated = s.tick()
}

// legacy turns a page into one written before the editor changed, whose body is
// the storage format and which has no Atlassian document at all.
func (s *site) legacy(id, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(id)
	p.legacy = true
	p.body = body
	p.version++
	p.updated = s.tick()
}

// archive puts a page away. It is still readable by id and it is no longer
// current, which is one of the three ways a page leaves the index without
// anything reporting that it has.
func (s *site) archive(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.find(id).archived = true
	s.tick()
}

// remove deletes a page. Nothing in CQL reports that it was ever there, which is
// why the sweep exists.
func (s *site) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The name stays behind on purpose. A test that deletes a page goes on to
	// ask what its id was, and a fake that forgot would answer a different
	// question.
	s.pages = slices.DeleteFunc(s.pages, func(p *leaf) bool { return p.id == id })
	s.tick()
}

// refuse makes one path answer with a status until it is told otherwise.
func (s *site) refuse(path string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[path] = status
}

// slowDown makes the next n requests come back as 429, which is how Confluence
// asks a crawler to wait.
func (s *site) slowDown(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttle = n
}

func (s *site) counted(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path]
}

// lastExpand is the expand parameter of the last request to a path, which is how
// a test checks that a listing asked for what it needed and not for the body of
// every page in the space.
func (s *site) lastExpand(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expand[path]
}

func (s *site) resetCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = make(map[string]int)
}

// server starts the site on a listener and returns its base URL.
//
// The address handed back has no /wiki on it, because that is one of the two
// things people have in front of them and the adapter is supposed to take
// either.
func (s *site) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (s *site) handle(rw http.ResponseWriter, req *http.Request) {
	// Everything the product serves is under /wiki, and a request that went
	// somewhere else is an adapter that built an address out of the bare host.
	if !strings.HasPrefix(req.URL.Path, "/wiki/") {
		s.deny(rw, http.StatusNotFound, "no such endpoint: "+req.URL.Path)
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/wiki")

	if err := req.ParseForm(); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.calls[path]++
	s.expand[path] = req.Form.Get("expand")
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
	case path == "/rest/api/space":
		s.listSpaces(rw, req)
	case path == "/rest/api/content/search":
		s.search(rw, req)
	case strings.HasSuffix(path, "/restriction/byOperation/read"):
		s.readRestriction(rw, segment(path, "/rest/api/content/", "/restriction/byOperation/read"))
	case strings.HasSuffix(path, "/child/comment"):
		s.listComments(rw, req, segment(path, "/rest/api/content/", "/child/comment"))
	case strings.HasPrefix(path, "/rest/api/content/"):
		s.readPage(rw, req, strings.TrimPrefix(path, "/rest/api/content/"))
	default:
		s.deny(rw, http.StatusNotFound, "no such endpoint")
	}
}

func (s *site) deny(rw http.ResponseWriter, status int, why string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_, _ = rw.Write([]byte(`{"statusCode":` + strconv.Itoa(status) + `,"message":"` + why + `","reason":"refused"}`))
}

func (s *site) send(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}

// listSpaces pages by offset, which is what the version one listings do.
//
// The next link it hands back is relative to the context path, which is the
// shape a real site sends and the reason the adapter takes the query off it
// rather than following it.
func (s *site) listSpaces(rw http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	rows := make([]map[string]any, 0, len(s.spaces))
	for _, a := range s.spaces {
		rows = append(rows, s.spaceRow(a, req.Form.Get("expand")))
	}
	s.mu.Unlock()

	at, size := offset(req, s.page)
	page, last := window(rows, at, size)

	out := map[string]any{
		"results": page,
		"start":   at,
		"limit":   size,
		"size":    len(page),
	}
	if !last {
		q := url.Values{}
		for k, v := range req.Form {
			q[k] = append([]string(nil), v...)
		}
		q.Set("start", strconv.Itoa(at+size))
		q.Set("limit", strconv.Itoa(size))
		out["_links"] = map[string]any{"next": "/wiki/rest/api/space?" + q.Encode()}
	}
	s.send(rw, out)
}

func (s *site) spaceRow(a *area, expand string) map[string]any {
	row := map[string]any{
		"id":     a.id,
		"key":    a.key,
		"name":   a.name,
		"type":   "global",
		"status": "current",
	}
	if !has(split(expand), "permissions") || a.dark {
		// A site that was not asked for the permissions does not send them, and
		// neither does one asked by an account that may not see them. The adapter
		// cannot tell those apart and is not supposed to guess at either.
		return row
	}

	perms := []map[string]any{
		// A grant to do something other than read, which is on every space and is
		// not the question being asked.
		{
			"operation": map[string]any{"operation": "create", "targetType": "page"},
			"subjects":  people(a.users, a.groups),
		},
	}
	switch {
	case a.open:
		perms = append(perms, map[string]any{
			"operation":       map[string]any{"operation": "read", "targetType": "space"},
			"subjects":        map[string]any{},
			"anonymousAccess": true,
		})
	default:
		perms = append(perms, map[string]any{
			"operation": map[string]any{"operation": "read", "targetType": "space"},
			"subjects":  people(a.users, a.groups),
		})
	}
	row["permissions"] = perms
	return row
}

// people is the shape a set of accounts and groups arrives in, which is the same
// shape for a space permission and for a page restriction.
func people(users, groups []string) map[string]any {
	u := make([]map[string]any, 0, len(users))
	for _, id := range users {
		u = append(u, who(id))
	}
	g := make([]map[string]any, 0, len(groups))
	for _, name := range groups {
		g = append(g, map[string]any{"name": name, "id": "g-" + name, "type": "group"})
	}
	return map[string]any{
		"user":  map[string]any{"results": u, "size": len(u)},
		"group": map[string]any{"results": g, "size": len(g)},
	}
}

// cqlSpace, cqlSince and cqlComments are as much CQL as this fake understands,
// which is exactly the clauses the adapter writes. A fake that parsed the whole
// language would be a second Confluence with its own bugs in it.
var (
	cqlSpace = regexp.MustCompile(`space\s*=\s*"([^"]+)"`)
	cqlSince = regexp.MustCompile(`lastModified\s*>=\s*"([^"]+)"`)
)

// search answers a CQL query, paging by cursor, which is what the search
// endpoint does and the space listing does not.
func (s *site) search(rw http.ResponseWriter, req *http.Request) {
	cql := req.Form.Get("cql")

	m := cqlSpace.FindStringSubmatch(cql)
	if m == nil {
		s.deny(rw, http.StatusBadRequest, "expected a space clause: "+cql)
		return
	}
	key := m[1]

	var since time.Time
	if u := cqlSince.FindStringSubmatch(cql); u != nil {
		got, err := time.Parse("2006/01/02 15:04", u[1])
		if err != nil {
			s.deny(rw, http.StatusBadRequest, "bad time in "+cql)
			return
		}
		since = got.UTC()
	}
	comments := strings.Contains(cql, "type = comment")
	byID := strings.Contains(cql, "order by id asc")

	s.mu.Lock()
	a := s.space(key)
	var hits []hit
	for _, p := range s.pages {
		if p.space != key || p.archived {
			continue
		}
		if since.IsZero() || !p.updated.Before(since) {
			hits = append(hits, hit{page: p, at: p.updated})
		}
		if !comments {
			continue
		}
		for _, c := range p.comments {
			if since.IsZero() || !c.updated.Before(since) {
				hits = append(hits, hit{page: p, note: c, at: c.updated})
			}
		}
	}
	if byID {
		slices.SortFunc(hits, func(x, y hit) int { return strings.Compare(x.id(), y.id()) })
	} else {
		slices.SortFunc(hits, func(x, y hit) int {
			if d := x.at.Compare(y.at); d != 0 {
				return d
			}
			return strings.Compare(x.id(), y.id())
		})
	}

	// The expands a search asks for are honoured, because a listing that asked
	// for two fields and got the body of every page in the space would never
	// notice it was paying for the bodies.
	want := split(req.Form.Get("expand"))
	rows := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		rows = append(rows, s.rowOf(h, want))
	}
	s.mu.Unlock()

	if a == nil {
		s.deny(rw, http.StatusBadRequest, "no such space: "+key)
		return
	}

	at, size := cursor(req, s.page)
	page, last := window(rows, at, size)
	out := map[string]any{
		"results":      page,
		"start":        at,
		"limit":        size,
		"size":         len(page),
		"totalSize":    len(rows),
		"cqlQuery":     cql,
		"searchDurati": 1,
	}
	if !last {
		q := url.Values{}
		for k, v := range req.Form {
			q[k] = append([]string(nil), v...)
		}
		q.Set("cursor", encodeCursor(at+size))
		q.Set("limit", strconv.Itoa(size))
		out["_links"] = map[string]any{"next": "/wiki/rest/api/content/search?" + q.Encode()}
	}
	s.send(rw, out)
}

// hit is one result of a search, which is a page or a comment on one.
type hit struct {
	page *leaf
	note *note
	at   time.Time
}

func (h hit) id() string {
	if h.note != nil {
		return h.note.id
	}
	return h.page.id
}

func (s *site) rowOf(h hit, want []string) map[string]any {
	if h.note != nil {
		row := s.noteRow(h.note, want)
		row["container"] = map[string]any{"id": h.page.id, "type": "page", "title": h.page.title}
		return row
	}
	return s.pageRow(h.page, want)
}

func (s *site) readPage(rw http.ResponseWriter, req *http.Request, id string) {
	s.mu.Lock()
	p := s.find(id)
	var row map[string]any
	if p != nil {
		row = s.pageRow(p, split(req.Form.Get("expand")))
	}
	s.mu.Unlock()

	if p == nil {
		s.deny(rw, http.StatusNotFound, "no such content")
		return
	}
	s.send(rw, row)
}

func (s *site) listComments(rw http.ResponseWriter, req *http.Request, id string) {
	s.mu.Lock()
	p := s.find(id)
	var rows []map[string]any
	if p != nil {
		want := split(req.Form.Get("expand"))
		for _, c := range p.comments {
			rows = append(rows, s.noteRow(c, want))
		}
	}
	s.mu.Unlock()

	if p == nil {
		s.deny(rw, http.StatusNotFound, "no such content")
		return
	}

	at, size := offset(req, s.page)
	page, last := window(rows, at, size)
	out := map[string]any{"results": page, "start": at, "limit": size, "size": len(page)}
	if !last {
		q := url.Values{}
		for k, v := range req.Form {
			q[k] = append([]string(nil), v...)
		}
		q.Set("start", strconv.Itoa(at+size))
		q.Set("limit", strconv.Itoa(size))
		out["_links"] = map[string]any{"next": "/wiki/rest/api/content/" + id + "/child/comment?" + q.Encode()}
	}
	s.send(rw, out)
}

func (s *site) readRestriction(rw http.ResponseWriter, id string) {
	s.mu.Lock()
	p := s.find(id)
	var row map[string]any
	if p != nil {
		row = restrictionRow(p.rule)
	}
	s.mu.Unlock()

	if p == nil {
		s.deny(rw, http.StatusNotFound, "no such content")
		return
	}
	s.send(rw, row)
}

// restrictionRow is a read restriction the way the API reports one, which is
// sent for every page whether there is a restriction on it or not.
func restrictionRow(r *bar) map[string]any {
	row := map[string]any{"operation": "read"}
	if r == nil {
		row["restrictions"] = people(nil, nil)
		return row
	}
	subjects := people(r.users, r.groups)
	if r.short {
		// One more than was sent, which is the site saying there is another page
		// of this list and is the only thing that says so.
		u, _ := subjects["user"].(map[string]any)
		u["size"] = len(r.users) + 1
	}
	row["restrictions"] = subjects
	return row
}

// pageRow is one page the way the API reports it, holding only what was asked
// for.
func (s *site) pageRow(p *leaf, want []string) map[string]any {
	status := "current"
	if p.archived {
		status = "archived"
	}
	row := map[string]any{
		"id":     p.id,
		"type":   "page",
		"status": status,
		"title":  p.title,
		"_links": map[string]any{"webui": "/spaces/" + p.space + "/pages/" + p.id + "/" + url.PathEscape(p.title)},
	}

	if has(want, "space") {
		a := s.space(p.space)
		row["space"] = map[string]any{"id": a.id, "key": a.key, "name": a.name}
	}
	if has(want, "version") {
		row["version"] = map[string]any{
			"by":     who(p.author),
			"when":   wikiTime(p.updated),
			"number": p.version,
		}
	}
	if has(want, "history") {
		row["history"] = map[string]any{
			"createdBy":   who(p.author),
			"createdDate": wikiTime(p.created),
		}
	}
	if has(want, "ancestors") {
		row["ancestors"] = s.ancestorsOf(p)
	}
	if has(want, "metadata.labels") {
		labels := make([]map[string]any, 0, len(p.labels))
		for _, l := range p.labels {
			labels = append(labels, map[string]any{"name": l, "prefix": "global"})
		}
		row["metadata"] = map[string]any{"labels": map[string]any{"results": labels}}
	}
	if has(want, "restrictions.read") {
		row["restrictions"] = map[string]any{"read": restrictionRow(p.rule)}
	}
	if body := s.bodyOf(p, want); body != nil {
		row["body"] = body
	}
	if has(want, "children.comment") {
		rows := make([]map[string]any, 0, len(p.comments))
		for _, c := range p.comments {
			rows = append(rows, s.noteRow(c, want))
		}
		// A read returns the first page of comments inline and links to the rest,
		// which is most pages in one request rather than two.
		page, last := window(rows, 0, s.page)
		inline := map[string]any{"results": page, "start": 0, "limit": s.page, "size": len(page)}
		if !last {
			inline["_links"] = map[string]any{
				"next": "/wiki/rest/api/content/" + p.id + "/child/comment?start=" + strconv.Itoa(s.page),
			}
		}
		row["children"] = map[string]any{"comment": inline}
	}
	return row
}

// bodyOf is a page's body in whichever formats were asked for and exist.
//
// A page written before the editor changed has a storage body and no Atlassian
// document at all, which is the case that costs the adapter a second request.
func (s *site) bodyOf(p *leaf, want []string) map[string]any {
	body := map[string]any{}
	if has(want, "body.atlas_doc_format") {
		value := ""
		if !p.legacy {
			value = adfOf(p.body)
		}
		body["atlas_doc_format"] = map[string]any{"value": value, "representation": "atlas_doc_format"}
	}
	if has(want, "body.storage") {
		value := p.body
		if !p.legacy {
			value = "<p>" + p.body + "</p>"
		}
		body["storage"] = map[string]any{"value": value, "representation": "storage"}
	}
	if len(body) == 0 {
		return nil
	}
	return body
}

func (s *site) noteRow(c *note, want []string) map[string]any {
	row := map[string]any{
		"id":     c.id,
		"type":   "comment",
		"status": "current",
		"title":  "Re: a page",
	}
	if has(want, "version") || has(want, "children.comment.version") {
		row["version"] = map[string]any{"by": who(c.author), "when": wikiTime(c.updated), "number": 1}
	}
	if has(want, "history") || has(want, "children.comment.history") {
		row["history"] = map[string]any{"createdBy": who(c.author), "createdDate": wikiTime(c.created)}
	}
	if has(want, "body.atlas_doc_format") || has(want, "children.comment.body.atlas_doc_format") {
		row["body"] = map[string]any{
			"atlas_doc_format": map[string]any{"value": adfOf(c.body), "representation": "atlas_doc_format"},
		}
	}
	return row
}

// ancestorsOf is the pages above one, outermost first, which is the order
// Confluence sends them in and the order the restriction walk expects.
func (s *site) ancestorsOf(p *leaf) []map[string]any {
	var up []map[string]any
	for at, seen := p.parent, map[string]bool{p.id: true}; at != "" && !seen[at]; {
		seen[at] = true
		parent := s.find(at)
		if parent == nil {
			break
		}
		up = append([]map[string]any{{"id": parent.id, "type": "page", "title": parent.title}}, up...)
		at = parent.parent
	}
	return up
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
		"type":        "known",
		"accountId":   id,
		"displayName": name,
		"publicName":  name,
		"email":       strings.TrimPrefix(id, "acc-") + "@acme.com",
	}
}

// adfOf wraps a plain string in the smallest Atlassian document that holds it,
// as a string holding JSON, which is how the body of a page actually arrives.
func adfOf(text string) string {
	if text == "" {
		return ""
	}
	content := make([]any, 0, 1)
	for _, para := range strings.Split(text, "\n\n") {
		content = append(content, map[string]any{
			"type":    "paragraph",
			"content": []any{map[string]any{"type": "text", "text": para}},
		})
	}
	raw, err := json.Marshal(map[string]any{"type": "doc", "version": 1, "content": content})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// wikiTime is the format a Confluence site writes a timestamp in, which is ISO
// 8601 with milliseconds and an offset with a colon in it.
func wikiTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000-07:00")
}

// split reads an expand parameter into the names in it.
func split(expand string) []string {
	if expand == "" {
		return nil
	}
	out := strings.Split(expand, ",")
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

// has reports whether an expand parameter asked for something, counting a
// request for a thing further in as a request for what holds it.
func has(want []string, name string) bool {
	for _, w := range want {
		if w == name || strings.HasPrefix(w, name+".") {
			return true
		}
	}
	return false
}

// offset reads the start and size off a request, which is how the version one
// listings page.
func offset(req *http.Request, fallback int) (at, size int) {
	at, _ = strconv.Atoi(req.Form.Get("start"))
	size, _ = strconv.Atoi(req.Form.Get("limit"))
	if size <= 0 {
		size = fallback
	}
	// A real site caps what it will return whatever was asked for, which is the
	// behaviour that catches an adapter trusting the limit it sent.
	return at, min(size, fallback)
}

// cursor reads the position off a request the way the search endpoint pages,
// which is an opaque token rather than an offset.
//
// It is opaque on a real site and it is opaque here, in the sense that the
// adapter is given no way to construct one. Everything it can do with it is hand
// it back.
func cursor(req *http.Request, fallback int) (at, size int) {
	_, size = offset(req, fallback)
	if raw := req.Form.Get("cursor"); raw != "" {
		at = decodeCursor(raw)
	}
	return at, size
}

func encodeCursor(at int) string { return "raNDom" + strconv.Itoa(at) + "Token" }

func decodeCursor(raw string) int {
	at, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(raw, "raNDom"), "Token"))
	return at
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
