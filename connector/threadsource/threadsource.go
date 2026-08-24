// Package threadsource is the connector chat, ticket trackers and wikis share.
//
// Three products that look nothing alike keep the same shape underneath the
// vocabulary. There are containers, which a chat tool calls channels, a tracker
// calls projects and a wiki calls spaces, and the container is where the access
// rule lives. Inside a container there are conversations, which are a thread, an
// issue with its comments, or a page with its comments, and a conversation is a
// first message and the replies to it. Everything else is naming.
//
// Writing that three times produces three connectors that drift apart, and the
// part they drift on is the part that must not drift. So the crawl, the cursor,
// the resume and the permission refresh are here, and a product adapter is left
// with the only thing that is genuinely different: how to ask its API.
//
// # What an adapter provides
//
// A [Service] is four questions a product's API has to be able to answer. List
// the containers and say who may read each one, walk the conversations in a
// container that changed since a time, list the conversation ids a container
// holds, and read one conversation by id.
//
// The four are all required, and the last two are the ones worth arguing about.
// Listing is what reconciliation is built on, and none of these products has a
// change feed that reports a deletion: a message removed, an issue deleted and
// a page archived all leave the index holding a document nothing will ever take
// away. Reading one conversation by id is what turns the sweep from a report
// into a repair.
//
// # Permissions come from the container
//
// A conversation inherits the rule that governs the container it is in, which is
// how every one of these products actually works. A private channel is private
// because of the channel, and a page in a restricted space is restricted because
// of the space.
//
// A conversation may override it, and that is not an edge case: a ticket with a
// security level on it is readable by that level's members and by nobody else,
// whatever the project says, and a page with restrictions on it is the same. An
// adapter says so by filling in [Thread.Access].
//
// There is no permissive default. A container whose rule is the zero value
// quarantines everything in it, which is loud and safe, rather than publishing a
// private channel to the tenant, which is quiet and not.
//
// # Incremental sync
//
// The cursor is the newest change time the last completed run saw, and a later
// run asks each container for what changed at or after it. Asking for at or
// after rather than strictly after is deliberate, because the alternative loses
// a conversation that changed in the same instant as the cursor, and the ids
// already emitted at that instant are carried in the cursor so that nothing is
// emitted twice for it.
//
// An interrupted run resumes at the container it reached rather than at the
// beginning. Containers are walked in a fixed order and the cursor a change
// carries names the container and how far into it the run had got, so a crawl
// killed two hours in does not start again from the first channel.
//
// A container's rule can be rewritten without a single conversation in it being
// touched, and nothing in a listing of changes says so. An adapter that can say
// when the rule last changed turns that into a permission change carrying no
// body, which is how a revocation reaches the index without the channel being
// read again.
//
// The conversations that override their container are the exception, and they
// are the reason a listing reports a rule as well as an id. A refresh that gave
// them the container's new rule would be handing the rule of a project to the
// ticket somebody put a security level on, on a schedule, without anybody having
// done anything.
package threadsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/thread"
	"github.com/tamnd/genba/doc"
)

// Container is a channel, a project or a space: the thing conversations are in
// and the thing the access rule is usually on.
type Container struct {
	// ID is the source's own identifier, and it is what the cursor records.
	// It has to be stable, because a container whose id changes is a container
	// a resuming run will walk again.
	ID string

	// Name is what a person calls it, and it lands in
	// [doc.Document.Container] for every conversation inside it.
	Name string

	// Access is who may read everything in it. The zero value quarantines the
	// container's contents rather than publishing them.
	Access acl.Permissions

	// AccessAt is when the rule last changed, and the zero time means the
	// product cannot say.
	//
	// It is what a permission change without a recrawl is built on. A product
	// that cannot answer it is not broken and pays for it in the obvious way:
	// a revocation reaches the index when the conversations under it are next
	// read for some other reason.
	AccessAt time.Time
}

// Thread is one conversation as an adapter hands it over.
type Thread struct {
	// Conversation is the content, assembled into one document by
	// [thread.Conversation.Document].
	Conversation thread.Conversation

	// Container is the id of the container it is in, which is where its access
	// rule comes from when it has none of its own.
	Container string

	// Access overrides the container's rule for this conversation alone. The
	// zero value means the conversation inherits, and an adapter that
	// considered the question and failed says so with [connector.Unresolved]
	// rather than leaving this empty.
	Access acl.Permissions

	// Updated is when the source says the conversation last changed, and is
	// what the cursor is made of. Leaving it zero is allowed and the last
	// message in the conversation is used instead, which is right for a chat
	// thread and wrong for a ticket whose fields changed without a comment.
	Updated time.Time
}

// Item is one conversation as a listing reports it.
//
// It is a [connector.Item] with room for the conversation's own access rule
// beside it, and that rule is there for one reason, which is the permission
// refresh below.
type Item struct {
	connector.Item

	// Access is the conversation's own rule, and the zero value means it
	// inherits its container's.
	//
	// It is on the listing rather than being read one conversation at a time
	// because reading them is the thing the refresh exists to avoid. A refresh
	// that had to read a container to find out which conversations in it
	// override the container is a recrawl, and a recrawl is what the whole
	// mechanism is instead of.
	//
	// An adapter for a product where a conversation cannot carry its own rule
	// leaves this alone and nothing changes. An adapter for one where it can has
	// to fill it in, because the alternative is a scheduled refresh handing a
	// restricted conversation the rule of the container it was restricted out
	// of, which publishes exactly the documents somebody went out of their way
	// to keep.
	Access acl.Permissions
}

// Service is what a product's API has to be able to answer.
//
// An implementation talks to one product and is where every difference between
// them is allowed to live: the paging, the signing, the rate limit, the shape
// of an id and the vocabulary. None of that reaches this package, and nothing
// in this package reaches a network.
type Service interface {
	// Containers returns the channels, projects or spaces this source covers,
	// with the rule that governs each.
	//
	// It is called once per sync and once per enumeration, so an adapter that
	// pages is expected to page here rather than to hand back a first page.
	Containers(ctx context.Context) ([]Container, error)

	// Threads calls fn for every conversation in a container that changed at
	// or after since, oldest change first.
	//
	// A zero since means everything the container holds. At or after rather
	// than strictly after is what keeps a conversation that changed in the
	// same instant as the cursor from being lost, and the duplicate that
	// causes is dealt with here rather than by the adapter.
	//
	// An error from fn is returned to the caller unchanged and stops the walk,
	// the same rule [connector.Connector.Sync] follows, because it is the same
	// error travelling up.
	Threads(ctx context.Context, c Container, since time.Time, fn func(context.Context, Thread) error) error

	// List calls fn for every conversation the container currently holds, and
	// stops early if fn returns false.
	//
	// It has to be complete or it has to fail. A listing that quietly returns
	// part of a channel reads, to the sweep above it, as a channel the rest of
	// which was deleted.
	List(ctx context.Context, c Container, fn func(Item) bool) error

	// Read returns one conversation by the source's own id, which is the
	// document id with the source name taken off the front.
	//
	// It returns [connector.ErrGone] if the source no longer has it, which is
	// not a failure: it is the answer, and it is how a repair learns that what
	// it was about to refetch should be deleted instead.
	Read(ctx context.Context, id string) (Thread, error)
}

// Source is a connector over one [Service].
type Source struct {
	service Service
	name    string
	maxBody int
	skipped func(id string, reason error)

	lists    atomic.Int64
	metadata atomic.Int64
	fetches  atomic.Int64
	bytes    atomic.Int64

	once     sync.Once
	closeErr error
}

// Option configures a source.
type Option func(*Source)

// WithMaxBody sets the largest conversation body that is indexed. A value below
// one selects [thread.DefaultMaxBody].
func WithMaxBody(n int) Option {
	return func(s *Source) {
		if n > 0 {
			s.maxBody = n
		}
	}
}

// WithSkipped installs a callback for conversations the sync passed over.
//
// A sync does not abandon a workspace because one conversation in it could not
// be turned into a document. What it must not be is silent: an index quietly
// missing everything nobody could read looks exactly like an index that is
// complete, and the difference only shows up when somebody cannot find a thread
// they remember. The default does nothing.
func WithSkipped(f func(id string, reason error)) Option {
	return func(s *Source) {
		if f != nil {
			s.skipped = f
		}
	}
}

// New returns a source reading svc and naming itself name.
func New(svc Service, name string, opts ...Option) (*Source, error) {
	if svc == nil {
		return nil, errors.New("threadsource: nil service")
	}
	if name == "" {
		return nil, errors.New("threadsource: empty source name")
	}
	s := &Source{
		service: svc,
		name:    name,
		maxBody: thread.DefaultMaxBody,
		skipped: func(string, error) {},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

var (
	_ connector.Connector  = (*Source)(nil)
	_ connector.Enumerator = (*Source)(nil)
	_ connector.Fetcher    = (*Source)(nil)
	_ connector.Counted    = (*Source)(nil)
)

// Source returns the connector's name.
func (s *Source) Source() string { return s.name }

// Close closes the service if it is something that can be closed, and is safe
// to call more than once.
//
// Most adapters hold an HTTP client that outlives them and have nothing to
// release, which is why this is an interface check rather than a method on
// [Service]: requiring every adapter to write an empty Close would make the one
// that does need to release something look like all the ones that do not.
func (s *Source) Close() error {
	s.once.Do(func() {
		if c, ok := s.service.(interface{ Close() error }); ok {
			s.closeErr = c.Close()
		}
	})
	return s.closeErr
}

// edgeLimit is how many ids the cursor carries for the instant it stopped at.
//
// The list exists so that a conversation changed in the same instant as the
// cursor is neither lost nor emitted twice, and one instant normally holds one
// conversation. A bulk import that stamps ten thousand of them with the same
// time is the case this bound is for, and what going over it costs is a handful
// of documents indexed again on the next run, which is the harmless direction.
const edgeLimit = 256

// syncPoint is what this connector puts in a cursor.
//
// Since is how far the last completed run got, and Edge is the conversations
// emitted at exactly that instant. Container and At are only set on the cursors
// carried by individual changes: they say which container an interrupted run
// had reached and how far into it, so a cursor with a container in it is
// exactly the record of a run that did not finish.
//
// Perms is the same high water mark for access rules, and it is separate
// because the two move for different reasons. A rule rewritten at midnight
// governs conversations nobody has touched for a year, and folding the two
// together would mean either re-reading the year or missing the rewrite.
type syncPoint struct {
	Since     time.Time `json:"since"`
	Edge      []string  `json:"edge,omitempty"`
	Perms     time.Time `json:"perms,omitempty"`
	Container string    `json:"container,omitempty"`
	At        time.Time `json:"at,omitempty"`
}

// Sync walks every container and emits the conversations that changed.
func (s *Source) Sync(ctx context.Context, from connector.Cursor, emit func(context.Context, connector.Change) error) (connector.Cursor, error) {
	start, err := parseCursor(from)
	if err != nil {
		return connector.Cursor{}, err
	}

	containers, err := s.containers(ctx)
	if err != nil {
		return connector.Cursor{}, err
	}

	var (
		highest = start.Since
		edge    = slices.Clone(start.Edge)
		latest  = start.Perms

		// A full sync has nothing to refresh. Every conversation it emits
		// carries the rule the container has right now, so emitting a
		// permission change alongside it would be saying the same thing twice
		// and doubling the size of the first sync of a workspace.
		full = from.IsZero()
	)
	for _, c := range containers {
		if err := ctx.Err(); err != nil {
			return connector.Cursor{}, err
		}

		// The high water mark for rules covers every container whether or not
		// this run walked it, because the next run compares against it and a
		// container skipped here was walked by the run this one is resuming.
		if c.AccessAt.After(latest) {
			latest = c.AccessAt
		}

		// A container the interrupted run had already finished is one whose
		// changes were emitted with the cursor this one started from, so
		// walking it again would cost a full listing to emit nothing.
		if start.Container != "" && c.ID < start.Container {
			continue
		}

		since := start.Since
		if c.ID == start.Container && start.At.After(since) {
			since = start.At
		}

		if !full && c.AccessAt.After(start.Perms) {
			if err := s.refresh(ctx, c, start, emit); err != nil {
				return connector.Cursor{}, err
			}
		}

		err := s.service.Threads(ctx, c, since, func(ctx context.Context, th Thread) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			at := changedAt(th)
			if !s.wanted(th, at, start, since) {
				return nil
			}

			document, err := s.document(th, c)
			if err != nil {
				s.skipped(th.Conversation.ID, err)
				return nil
			}
			if err := emit(ctx, connector.Change{
				Document: document,
				Cursor:   resume(start, c.ID, at),
			}); err != nil {
				return err
			}

			switch {
			case at.After(highest):
				// The instant moved on, so what was emitted at the old one is
				// no longer what a resuming run has to skip.
				highest, edge = at, append(edge[:0], th.Conversation.ID)
			case at.Equal(highest) && len(edge) < edgeLimit:
				edge = append(edge, th.Conversation.ID)
			}
			return nil
		})
		if err != nil {
			return connector.Cursor{}, err
		}
	}

	if highest.IsZero() && latest.IsZero() {
		return from, nil
	}
	return cursorAt(syncPoint{Since: highest, Edge: edge, Perms: latest}, highest), nil
}

// wanted decides whether a conversation the service handed over is one this run
// should emit.
//
// Two of the three reasons to say no are about the boundary the cursor sits on.
// A service is asked for what changed at or after a time so that nothing in
// that instant is lost, which means it hands back what was already emitted at
// that instant, and the cursor carries those ids so that they are not emitted
// twice.
func (s *Source) wanted(th Thread, at time.Time, start syncPoint, since time.Time) bool {
	switch {
	case th.Conversation.ID == "":
		s.skipped("", errors.New("the conversation has no id, so there is nothing to file it under"))
		return false
	case at.IsZero():
		// Nothing about it says when it changed, so every run will emit it
		// again. That is the safe direction and it is worth saying out loud
		// rather than dropping the conversation.
		return true
	case !since.IsZero() && at.Before(since):
		return false
	case at.Equal(start.Since) && slices.Contains(start.Edge, th.Conversation.ID):
		return false
	default:
		return true
	}
}

// refresh emits a permission change for every conversation in a container whose
// rule was rewritten, without reading any of them.
//
// This is the whole of "a revocation takes effect without a resync". Somebody
// makes a channel private and nothing inside it is touched, so a sync that only
// asked what changed would find nothing at all and the index would keep
// answering with the old rule.
//
// A conversation that carries its own rule is the one thing the container's new
// rule must not be written over. It was restricted out of the container, so
// giving it the container's answer is not a refresh, it is a disclosure, and it
// is one that happens on a schedule rather than because anybody did anything.
func (s *Source) refresh(ctx context.Context, c Container, start syncPoint, emit func(context.Context, connector.Change) error) error {
	inherited := s.access(Thread{}, c)
	cursor := resume(start, c.ID, start.At)

	var failed error
	s.lists.Add(1)
	err := s.service.List(ctx, c, func(item Item) bool {
		if item.ID == "" {
			return true
		}
		perms := inherited
		if resolved(item.Access) {
			perms = item.Access
		}
		s.metadata.Add(1)
		failed = emit(ctx, connector.Change{
			Document:        doc.Document{ID: s.id(item.ID), Source: s.name, Permissions: perms},
			PermissionsOnly: true,
			Cursor:          cursor,
		})
		return failed == nil
	})
	if failed != nil {
		return failed
	}
	return err
}

// Enumerate lists every conversation the source currently holds.
//
// It is what the reconciliation sweep runs on, and for these products it is the
// only thing that ever removes anything. A message deleted, an issue deleted and
// a page archived are all changes no change feed reports, so without this the
// index would keep serving a thread the source no longer has.
func (s *Source) Enumerate(ctx context.Context, fn func(connector.Item) bool) error {
	containers, err := s.containers(ctx)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if err := ctx.Err(); err != nil {
			return err
		}
		stopped := false
		s.lists.Add(1)
		err := s.service.List(ctx, c, func(item Item) bool {
			if item.ID == "" {
				return true
			}
			// The rule an adapter reported alongside the id is for the refresh
			// and is nothing to a sweep, which compares versions and has no
			// opinion about who may read what.
			out := item.Item
			out.ID = s.id(out.ID)
			if !fn(out) {
				stopped = true
				return false
			}
			return true
		})
		if err != nil {
			return err
		}
		if stopped {
			// A listing stopped on purpose is not a failed listing, and the
			// caller that stopped it is not asking for the next container.
			return nil
		}
	}
	return nil
}

// Fetch reads one conversation by document id, with its permissions resolved
// the way a sync would resolve them.
func (s *Source) Fetch(ctx context.Context, id string) (doc.Document, error) {
	raw, ok := strings.CutPrefix(id, s.name+":")
	if !ok || raw == "" {
		return doc.Document{}, fmt.Errorf("threadsource: %q is not an id from %s", id, s.name)
	}

	th, err := s.service.Read(ctx, raw)
	if err != nil {
		return doc.Document{}, err
	}

	// The container is where the rule lives, and a fetch arrives with nothing
	// but an id. An adapter that filled the conversation's own access in has
	// answered the question already and this costs nothing.
	var c Container
	if !resolved(th.Access) && th.Container != "" {
		containers, err := s.containers(ctx)
		if err != nil {
			return doc.Document{}, err
		}
		if i := slices.IndexFunc(containers, func(x Container) bool { return x.ID == th.Container }); i >= 0 {
			c = containers[i]
		}
	}
	return s.document(th, c)
}

// Counters returns what this connector has spent at the source.
//
// They are counted in conversations rather than in requests, because how many
// requests a conversation costs is the adapter's business and changes with
// every page size. What matters above here is the shape: a second sync of a
// workspace nothing changed in fetches nothing.
func (s *Source) Counters() connector.Counters {
	return connector.Counters{
		Lists:    s.lists.Load(),
		Metadata: s.metadata.Load(),
		Fetches:  s.fetches.Load(),
		Bytes:    s.bytes.Load(),
	}
}

// containers lists the containers in a fixed order.
//
// The order is the resume point. A cursor says which container an interrupted
// run reached and the next run skips the ones before it, which is only sound if
// the two runs walk them in the same order, and the order a product's API
// happens to list channels in is not something to rely on.
func (s *Source) containers(ctx context.Context) ([]Container, error) {
	s.lists.Add(1)
	containers, err := s.service.Containers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(containers))
	for _, c := range containers {
		if c.ID != "" {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b Container) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// document turns one conversation into one document.
func (s *Source) document(th Thread, c Container) (doc.Document, error) {
	if th.Conversation.ID == "" {
		return doc.Document{}, errors.New("the conversation has no id, so there is nothing to file it under")
	}

	d := th.Conversation.Document(s.access(th, c), thread.WithMaxBody(s.maxBody))
	d.ID = s.id(th.Conversation.ID)
	d.Source = s.name
	if d.Container == "" {
		d.Container = c.Name
	}

	s.fetches.Add(1)
	s.bytes.Add(int64(len(d.Body)))
	return d, nil
}

// access is the rule that governs one conversation.
//
// The conversation's own answer wins, because a ticket with a security level on
// it is readable by that level's members whatever the project says. Otherwise it
// inherits the container, and a container with no rule at all quarantines what
// is in it rather than publishing it.
func (s *Source) access(th Thread, c Container) acl.Permissions {
	switch {
	case resolved(th.Access):
		return th.Access
	case resolved(c.Access):
		return c.Access
	default:
		return connector.Unresolved(s.name, "neither the conversation nor the container it is in said who may read it")
	}
}

// resolved reports whether a descriptor is one somebody filled in.
//
// A descriptor naming its source is one a connector produced, including the one
// [connector.Unresolved] produces, which says the question was considered and
// could not be answered. The zero value is the other thing entirely: nobody
// said anything, and that is what inheriting is for.
func resolved(p acl.Permissions) bool { return p.Source != "" || p.Mode != acl.ModeUnknown }

// id is the document id for one conversation.
func (s *Source) id(raw string) string { return s.name + ":" + raw }

// changedAt is when a conversation last changed.
//
// The source's own answer is used when there is one, because a ticket whose
// priority was raised changed without anybody writing a word and only the source
// knows that. Otherwise the last thing anybody said in it is the honest answer.
func changedAt(th Thread) time.Time {
	if !th.Updated.IsZero() {
		return th.Updated
	}
	last := latestOf(th.Conversation.Root, time.Time{})
	for _, m := range th.Conversation.Replies {
		last = latestOf(m, last)
	}
	return last
}

func latestOf(m thread.Message, so time.Time) time.Time {
	for _, at := range []time.Time{m.At, m.Edited} {
		if at.After(so) {
			so = at
		}
	}
	return so
}

// parseCursor reads back what this connector wrote.
func parseCursor(c connector.Cursor) (syncPoint, error) {
	if c.IsZero() {
		return syncPoint{}, nil
	}
	var p syncPoint
	if err := json.Unmarshal([]byte(c.Value), &p); err != nil {
		// A cursor this connector cannot read was written by a different
		// version or a different connector. Refusing is better than silently
		// resyncing a workspace, which on a large one is hours of somebody
		// else's rate limit.
		return syncPoint{}, fmt.Errorf("threadsource: unreadable cursor %q: %w", c.Value, err)
	}
	return p, nil
}

func cursorAt(p syncPoint, at time.Time) connector.Cursor {
	// The only way this fails is a time that cannot be formatted, and there is
	// no such time. An empty value would be read back as no cursor at all,
	// which resyncs the workspace rather than losing anything.
	raw, err := json.Marshal(p)
	if err != nil {
		return connector.Cursor{}
	}
	return connector.Cursor{Value: string(raw), Time: at}
}

// resume is the cursor carried by one change, and is where an interrupted run
// picks up.
//
// It keeps the completed run's Since rather than moving it on, and records the
// progress separately. Moving it on would be wrong in a way that is invisible:
// the containers this run has not reached yet still have to be asked from the
// old point, and a cursor that had advanced past them would skip everything in
// them that changed in between.
func resume(start syncPoint, container string, at time.Time) connector.Cursor {
	return cursorAt(syncPoint{
		Since:     start.Since,
		Edge:      start.Edge,
		Perms:     start.Perms,
		Container: container,
		At:        at,
	}, start.Since)
}
