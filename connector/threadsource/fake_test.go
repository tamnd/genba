package threadsource_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/connectortest"
	"github.com/tamnd/genba/connector/thread"
	"github.com/tamnd/genba/connector/threadsource"
	"github.com/tamnd/genba/doc"
)

// The fake is a chat tool with the parts that matter and none of the parts that
// do not. It has rooms with a rule on each, threads with replies, a clock that
// only moves when something happens to it, and the ability to be told to fail.
//
// It stands in for three products rather than one. What a real adapter adds is
// paging, signing, a rate limit and a vocabulary, and every one of those is
// beneath the interface this exercises.

const source = "chat"

// epoch is when everything in these tests happens. A fixed clock is what makes
// a cursor comparable, and a source whose times came from time.Now would make
// every one of these tests a race against the second boundary.
var epoch = time.Date(2026, time.June, 2, 9, 0, 0, 0, time.UTC)

type fake struct {
	mu    sync.Mutex
	clock time.Time
	rooms map[string]*room
	order []string

	// asked records the containers Threads was called for, which is how a test
	// says that a resumed run did not walk a channel it had already finished.
	asked  []string
	listed []string
	reads  int
	closes int

	failContainers error
	failThreads    error
	failList       error
	failRead       error
}

type room struct {
	id       string
	name     string
	access   acl.Permissions
	accessAt time.Time
	convs    map[string]*conv
}

type conv struct {
	id      string
	room    string
	title   string
	body    string
	author  string
	created time.Time
	updated time.Time
	replies []thread.Message
	access  acl.Permissions
	gone    bool
}

func newFake() *fake {
	f := &fake{clock: epoch, rooms: make(map[string]*room)}
	f.addRoom("r1", "maintenance")
	return f
}

// addRoom adds a channel everybody in the tenant may read.
func (f *fake) addRoom(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms[id] = &room{
		id:     id,
		name:   name,
		access: acl.Permissions{Mode: acl.ModePublicToTenant, Source: source},
		convs:  make(map[string]*conv),
	}
	f.order = append(f.order, id)
}

// write starts a thread, or edits the one that is already there, and moves the
// clock on afterwards the way a real source's does.
func (f *fake) write(roomID, id, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rooms[roomID]
	c, ok := r.convs[id]
	if !ok {
		c = &conv{id: id, room: roomID, title: strings.ToUpper(id[:1]) + id[1:], author: "mei", created: f.clock}
		r.convs[id] = c
	}
	c.body = body
	c.updated = f.clock
	c.gone = false
	f.clock = f.clock.Add(time.Second)
}

// writeAt starts a thread stamped with a time of the caller's choosing and
// leaves the clock where it was, which is how two conversations end up sharing
// an instant.
func (f *fake) writeAt(roomID, id, body string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rooms[roomID]
	r.convs[id] = &conv{
		id: id, room: roomID, title: strings.ToUpper(id[:1]) + id[1:],
		author: "mei", body: body, created: at, updated: at,
	}
}

// reply adds a message to a thread, which is a change to the thread rather than
// a document of its own.
func (f *fake) reply(roomID, id, author, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.rooms[roomID].convs[id]
	c.replies = append(c.replies, thread.Message{
		ID:     id + "-r" + string(rune('1'+len(c.replies))),
		Author: person(author),
		At:     f.clock,
		Text:   text,
	})
	c.updated = f.clock
	f.clock = f.clock.Add(time.Second)
}

// remove deletes a thread. Nothing in this source's change feed reports it,
// which is the point: the only way it ever reaches the index is the sweep.
func (f *fake) remove(roomID, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rooms[roomID].convs, id)
	f.clock = f.clock.Add(time.Second)
}

// share makes a room private, which changes who may read everything in it
// without touching a single message.
func (f *fake) share(roomID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rooms[roomID]
	r.access = acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      source,
		AllowGroups: []acl.Ref{{Source: source, Value: r.id}},
		Version:     r.access.Version + 1,
	}
	r.accessAt = f.clock
	f.clock = f.clock.Add(time.Second)
}

// restrict gives one thread a rule of its own, the way a ticket with a security
// level on it has one.
func (f *fake) restrict(roomID, id string, p acl.Permissions) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms[roomID].convs[id].access = p
}

// quarantine puts one thread's rule beyond working out.
func (f *fake) quarantine(roomID, id string) {
	f.restrict(roomID, id, connector.Unresolved(source))
}

// unruled takes the rule off a room entirely, which is the state a source
// built without a policy is in.
func (f *fake) unruled(roomID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms[roomID].access = acl.Permissions{}
}

func (f *fake) Containers(context.Context) ([]threadsource.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failContainers != nil {
		return nil, f.failContainers
	}
	out := make([]threadsource.Container, 0, len(f.order))
	for _, id := range f.order {
		r := f.rooms[id]
		out = append(out, threadsource.Container{
			ID:       r.id,
			Name:     r.name,
			Access:   r.access,
			AccessAt: r.accessAt,
		})
	}
	// The order a product lists its channels in is not something anybody
	// promised, so this one hands them back in the least helpful order it can.
	slices.Reverse(out)
	return out, nil
}

func (f *fake) Threads(ctx context.Context, c threadsource.Container, since time.Time, fn func(context.Context, threadsource.Thread) error) error {
	f.mu.Lock()
	if f.failThreads != nil {
		err := f.failThreads
		f.mu.Unlock()
		return err
	}
	f.asked = append(f.asked, c.ID)
	var changed []*conv
	for _, cv := range f.rooms[c.ID].convs {
		// At or after, which is what the interface asks for and what leaves the
		// duplicate at the boundary to be dealt with above.
		if since.IsZero() || !cv.updated.Before(since) {
			changed = append(changed, cv)
		}
	}
	f.mu.Unlock()

	slices.SortFunc(changed, func(a, b *conv) int {
		if d := a.updated.Compare(b.updated); d != 0 {
			return d
		}
		return strings.Compare(a.id, b.id)
	})
	for _, cv := range changed {
		if err := fn(ctx, cv.thread()); err != nil {
			return err
		}
	}
	return nil
}

func (f *fake) List(_ context.Context, c threadsource.Container, fn func(connector.Item) bool) error {
	f.mu.Lock()
	if f.failList != nil {
		err := f.failList
		f.mu.Unlock()
		return err
	}
	f.listed = append(f.listed, c.ID)
	var items []connector.Item
	for _, cv := range f.rooms[c.ID].convs {
		items = append(items, connector.Item{ID: cv.id, Version: cv.updated.UTC().Format(time.RFC3339Nano)})
	}
	f.mu.Unlock()

	slices.SortFunc(items, func(a, b connector.Item) int { return strings.Compare(a.ID, b.ID) })
	for _, item := range items {
		if !fn(item) {
			return nil
		}
	}
	return nil
}

func (f *fake) Read(_ context.Context, id string) (threadsource.Thread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRead != nil {
		return threadsource.Thread{}, f.failRead
	}
	f.reads++
	for _, r := range f.rooms {
		if cv, ok := r.convs[id]; ok && !cv.gone {
			return cv.thread(), nil
		}
	}
	return threadsource.Thread{}, connector.ErrGone
}

func (f *fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

// thread is one conversation the way an adapter hands it over.
func (c *conv) thread() threadsource.Thread {
	return threadsource.Thread{
		Conversation: thread.Conversation{
			ID:        c.id,
			Kind:      "message",
			Title:     c.title,
			Container: "",
			URL:       "https://chat.example.com/" + c.room + "/" + c.id,
			Root: thread.Message{
				ID:     c.id,
				Author: person(c.author),
				At:     c.created,
				Text:   c.body,
			},
			Replies: slices.Clone(c.replies),
		},
		Container: c.room,
		Access:    c.access,
		Updated:   c.updated,
	}
}

func person(name string) doc.Person {
	return doc.Person{Name: name, Email: name + "@acme.com", Identity: acl.Identity{Source: source, Value: name}}
}

// The conformance suite is what decides whether this is a connector. Everything
// else in this package's tests is about the parts that are specific to a
// conversation, and none of it would matter if this failed.
func TestConformance(t *testing.T) {
	connectortest.Run(t, func(t *testing.T) connectortest.Fixture {
		f := newFake()
		src, err := threadsource.New(f, source)
		if err != nil {
			t.Fatal(err)
		}
		return connectortest.Fixture{
			Connector:    src,
			ID:           func(name string) string { return source + ":" + name },
			Write:        func(_ *testing.T, name, body string) { f.write("r1", name, body) },
			Remove:       func(_ *testing.T, name string) { f.remove("r1", name) },
			Share:        func(_ *testing.T, _ string) { f.share("r1") },
			Unresolvable: func(_ *testing.T, name string) { f.quarantine("r1", name) },
		}
	})
}
