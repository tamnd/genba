package slacksource_test

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
	"time"
)

// A Slack workspace with the parts that matter and none of the parts that do
// not. It has public and private channels, threads with replies, users with
// names, cursor paging, a clock that only moves when something happens to it,
// and the ability to be told to refuse.
//
// It exists so that these tests do not need an account, and it is also what the
// committed recording under testdata was made from. Everything asserted against
// the recording is asserted against this first, so the two cannot drift without
// a test going red.

// epoch is when everything in these tests happens, as Slack counts time.
const epoch = 1780000000.0

type workspace struct {
	mu       sync.Mutex
	clock    float64
	channels []*chanel
	users    map[string]person
	calls    map[string]int

	// fail makes one method refuse with a Slack error code, and notIn makes the
	// token behave like a bot that is in the workspace but not in a channel.
	fail  map[string]string
	notIn map[string]bool

	// throttle is how many more times the next request is answered with 429
	// before it is answered properly.
	throttle int

	// page is how many items go in one page, which is small on purpose so that
	// the paging is exercised by an ordinary sync rather than by one test.
	page int

	// types records every kind of conversation any listing has asked for.
	types []string
}

type chanel struct {
	id, name string
	private  bool
	archived bool
	updated  int64 // milliseconds, the way Slack reports it
	members  []string
	msgs     []*msg
}

type msg struct {
	ts      string
	user    string
	text    string
	subtype string
	edited  string
	replies []*msg
}

type person struct {
	id, name, real, email string
}

func newWorkspace() *workspace {
	w := &workspace{
		clock: epoch,
		users: map[string]person{
			"U_MEI": {"U_MEI", "mei", "Mei Tanaka", "mei@acme.com"},
			"U_SAM": {"U_SAM", "sam", "Sam Okafor", "sam@acme.com"},
			"U_LEE": {"U_LEE", "lee", "Lee Berger", "lee@acme.com"},
		},
		calls: make(map[string]int),
		fail:  make(map[string]string),
		notIn: make(map[string]bool),
		page:  2,
	}
	w.addChannel("C_GENERAL", "general", false)
	return w
}

// now is the workspace's clock as a time, which is what the adapter measures
// its reply window against.
func (w *workspace) now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return stampOf(w.clock)
}

func stampOf(f float64) time.Time {
	sec := int64(f)
	micro := int64((f - float64(sec)) * 1e6)
	return time.Unix(sec, micro*int64(time.Microsecond)).UTC()
}

func (w *workspace) addChannel(id, name string, private bool) *chanel {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := &chanel{id: id, name: name, private: private, updated: int64(w.clock) * 1000}
	if private {
		c.members = []string{"U_MEI", "U_SAM"}
	}
	w.channels = append(w.channels, c)
	w.clock++
	return c
}

func (w *workspace) find(id string) *chanel {
	for _, c := range w.channels {
		if c.id == id {
			return c
		}
	}
	return nil
}

// tick hands out the next Slack timestamp.
func (w *workspace) tick() string {
	ts := fmt.Sprintf("%.6f", w.clock)
	w.clock++
	return ts
}

// post starts a thread, or replaces the text of the one already filed under
// that name, and returns its timestamp.
func (w *workspace) post(channelID, name, text string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.find(channelID)
	for _, m := range c.msgs {
		if m.text == name || strings.HasPrefix(m.text, name+"\n") {
			m.text = name + "\n" + text
			m.edited = w.tick()
			return m.ts
		}
	}
	m := &msg{ts: w.tick(), user: "U_MEI", text: name + "\n" + text}
	c.msgs = append(c.msgs, m)
	return m.ts
}

// tsOf is the timestamp a name was filed under, which is what turns the
// conformance suite's document names into ids this adapter would produce.
func (w *workspace) tsOf(channelID, name string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.find(channelID)
	for _, m := range c.msgs {
		if m.text == name || strings.HasPrefix(m.text, name+"\n") {
			return m.ts
		}
	}
	// A name nothing was ever written under still has to produce an id, because
	// a test asking for a document that is not there is a test about what
	// happens when it is not there.
	return "0.000000"
}

// reply answers a thread, which moves the thread and nothing else.
func (w *workspace) reply(channelID, name, user, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.find(channelID)
	for _, m := range c.msgs {
		if m.text == name || strings.HasPrefix(m.text, name+"\n") {
			m.replies = append(m.replies, &msg{ts: w.tick(), user: user, text: text})
			return
		}
	}
}

// system posts one of the things Slack writes about the channel itself rather
// than about anything anybody wanted to say.
func (w *workspace) system(channelID, subtype, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.find(channelID)
	c.msgs = append(c.msgs, &msg{ts: w.tick(), user: "U_SAM", text: text, subtype: subtype})
}

// remove deletes a thread. Nothing in Slack's history reports that it was ever
// there, which is why the sweep exists.
func (w *workspace) remove(channelID, name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.find(channelID)
	c.msgs = slices.DeleteFunc(c.msgs, func(m *msg) bool {
		return m.text == name || strings.HasPrefix(m.text, name+"\n")
	})
	w.clock++
}

// makePrivate converts a channel, which changes who may read everything in it
// without touching a single message.
func (w *workspace) makePrivate(channelID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.find(channelID)
	c.private = true
	c.members = []string{"U_MEI", "U_SAM"}
	w.clock++
	c.updated = int64(w.clock) * 1000
}

// removeMember takes somebody out of a private channel, which is the change
// Slack does not report anywhere.
func (w *workspace) removeMember(channelID, user string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.find(channelID)
	c.members = slices.DeleteFunc(c.members, func(m string) bool { return m == user })
	w.clock++
}

// askedFor reports whether any conversations.list call asked for a kind of
// conversation, which is how a test says that direct messages were never
// requested rather than requested and then filtered.
func (w *workspace) askedFor(kind string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Contains(w.types, kind)
}

func (w *workspace) counted(method string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[method]
}

func (w *workspace) resetCounts() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = make(map[string]int)
}

// server starts the workspace on a listener and returns its API base.
func (w *workspace) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(w.handle))
	t.Cleanup(srv.Close)
	return srv.URL + "/api"
}

func (w *workspace) handle(rw http.ResponseWriter, req *http.Request) {
	method := strings.TrimPrefix(req.URL.Path, "/api/")

	if err := req.ParseForm(); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	w.mu.Lock()
	w.calls[method]++
	if w.throttle > 0 {
		w.throttle--
		w.mu.Unlock()
		// Slack names its own delay, and a crawl that ignored it would be told
		// off again immediately.
		rw.Header().Set("Retry-After", "0")
		rw.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if code, ok := w.fail[method]; ok {
		w.mu.Unlock()
		w.deny(rw, code)
		return
	}
	w.mu.Unlock()

	// Every call carries the token as a header rather than as a form field, so
	// that a recording of one does not have the token in the file name.
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
		w.deny(rw, "not_authed")
		return
	}

	switch method {
	case "conversations.list":
		w.list(rw, req)
	case "conversations.members":
		w.membersOf(rw, req)
	case "conversations.history":
		w.history(rw, req)
	case "conversations.replies":
		w.thread(rw, req)
	case "users.info":
		w.user(rw, req)
	default:
		w.deny(rw, "unknown_method")
	}
}

func (w *workspace) deny(rw http.ResponseWriter, code string) {
	// A Slack refusal is a 200 with ok false, which is the whole reason the
	// adapter reads the body before it looks at the status.
	w.send(rw, map[string]any{"ok": false, "error": code})
}

func (w *workspace) send(rw http.ResponseWriter, body map[string]any) {
	rw.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(rw)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

// paged cuts a slice at the cursor and returns the page and the next cursor.
func paged[T any](items []T, cursor string, size int) (page []T, next string) {
	from := 0
	if cursor != "" {
		from, _ = strconv.Atoi(cursor)
	}
	if from > len(items) {
		from = len(items)
	}
	to := min(from+size, len(items))
	if to < len(items) {
		next = strconv.Itoa(to)
	}
	return items[from:to], next
}

func (w *workspace) list(rw http.ResponseWriter, req *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()

	types := req.Form.Get("types")
	for _, kind := range strings.Split(types, ",") {
		kind = strings.TrimSuffix(strings.TrimSpace(kind), "_channel")
		if kind != "" && !slices.Contains(w.types, kind) {
			w.types = append(w.types, kind)
		}
	}

	var want []map[string]any
	for _, c := range w.channels {
		if c.private && !strings.Contains(types, "private_channel") {
			continue
		}
		if !c.private && !strings.Contains(types, "public_channel") {
			continue
		}
		if c.archived && req.Form.Get("exclude_archived") == "true" {
			continue
		}
		want = append(want, map[string]any{
			"id":          c.id,
			"name":        c.name,
			"is_private":  c.private,
			"is_archived": c.archived,
			"is_channel":  true,
			"updated":     c.updated,
		})
	}

	page, next := paged(want, req.Form.Get("cursor"), w.page)
	w.send(rw, map[string]any{
		"ok":                true,
		"channels":          page,
		"response_metadata": map[string]any{"next_cursor": next},
	})
}

func (w *workspace) membersOf(rw http.ResponseWriter, req *http.Request) {
	w.mu.Lock()
	id := req.Form.Get("channel")
	c := w.find(id)
	notIn := w.notIn[id]
	var members []string
	if c != nil {
		members = slices.Clone(c.members)
	}
	size := w.page
	w.mu.Unlock()

	switch {
	case c == nil:
		w.deny(rw, "channel_not_found")
		return
	case notIn:
		w.deny(rw, "not_in_channel")
		return
	}

	// Slack does not promise an order, so this one hands them back in the least
	// helpful order it can.
	slices.Reverse(members)
	page, next := paged(members, req.Form.Get("cursor"), size)
	w.send(rw, map[string]any{
		"ok":                true,
		"members":           page,
		"response_metadata": map[string]any{"next_cursor": next},
	})
}

func (w *workspace) history(rw http.ResponseWriter, req *http.Request) {
	w.mu.Lock()
	id := req.Form.Get("channel")
	c := w.find(id)
	notIn := w.notIn[id]
	size := w.page

	var rows []map[string]any
	if c != nil {
		oldest, _ := strconv.ParseFloat(req.Form.Get("oldest"), 64)
		// Slack returns history newest first, and a walk that assumed otherwise
		// would be one that worked here and failed on the real thing.
		for i := len(c.msgs) - 1; i >= 0; i-- {
			m := c.msgs[i]
			at, _ := strconv.ParseFloat(m.ts, 64)
			if at < oldest {
				continue
			}
			rows = append(rows, w.row(m, true))
		}
	}
	w.mu.Unlock()

	switch {
	case c == nil:
		w.deny(rw, "channel_not_found")
		return
	case notIn:
		w.deny(rw, "not_in_channel")
		return
	}

	page, next := paged(rows, req.Form.Get("cursor"), size)
	w.send(rw, map[string]any{
		"ok":                true,
		"messages":          page,
		"has_more":          next != "",
		"response_metadata": map[string]any{"next_cursor": next},
	})
}

func (w *workspace) thread(rw http.ResponseWriter, req *http.Request) {
	w.mu.Lock()
	c := w.find(req.Form.Get("channel"))
	ts := req.Form.Get("ts")
	var parent *msg
	if c != nil {
		for _, m := range c.msgs {
			if m.ts == ts {
				parent = m
				break
			}
		}
	}
	var rows []map[string]any
	if parent != nil {
		rows = append(rows, w.row(parent, true))
		for _, r := range parent.replies {
			row := w.row(r, false)
			row["thread_ts"] = parent.ts
			rows = append(rows, row)
		}
	}
	size := w.page
	w.mu.Unlock()

	switch {
	case c == nil:
		w.deny(rw, "channel_not_found")
		return
	case parent == nil:
		w.deny(rw, "thread_not_found")
		return
	}

	page, next := paged(rows, req.Form.Get("cursor"), size)
	if req.Form.Get("cursor") != "" {
		// A paged reply listing repeats the parent on every page, which is the
		// behaviour that makes a naive adapter index the first message three
		// times.
		page = append([]map[string]any{w.row(parent, true)}, page...)
	}
	w.send(rw, map[string]any{
		"ok":                true,
		"messages":          page,
		"has_more":          next != "",
		"response_metadata": map[string]any{"next_cursor": next},
	})
}

// row is one message the way Slack reports it. The reply count and the latest
// reply are only on a parent read from history, which is exactly the asymmetry
// the adapter has to cope with.
func (w *workspace) row(m *msg, asParent bool) map[string]any {
	row := map[string]any{
		"type": "message",
		"user": m.user,
		"text": m.text,
		"ts":   m.ts,
	}
	if m.subtype != "" {
		row["subtype"] = m.subtype
	}
	if m.edited != "" {
		row["edited"] = map[string]any{"ts": m.edited, "user": m.user}
	}
	if asParent && len(m.replies) > 0 {
		row["thread_ts"] = m.ts
		row["reply_count"] = len(m.replies)
		row["latest_reply"] = m.replies[len(m.replies)-1].ts
	}
	return row
}

func (w *workspace) user(rw http.ResponseWriter, req *http.Request) {
	w.mu.Lock()
	p, ok := w.users[req.Form.Get("user")]
	w.mu.Unlock()
	if !ok {
		w.deny(rw, "user_not_found")
		return
	}
	w.send(rw, map[string]any{
		"ok": true,
		"user": map[string]any{
			"id":        p.id,
			"name":      p.name,
			"real_name": p.real,
			"profile":   map[string]any{"real_name": p.real, "email": p.email},
		},
	})
}
