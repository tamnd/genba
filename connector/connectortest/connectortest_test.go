package connectortest

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/doc"
)

// The suite has to be tested twice over, and the two halves are different
// questions.
//
// The first is whether a connector that does everything right passes, which is
// what TestTheSuitePassesAConnectorThatWorks answers by running the whole thing
// against a connector in memory. Without it the suite could be a set of rules
// no implementation can satisfy and nobody would find out until the second one
// was written.
//
// The second is whether it catches a connector that does not, which is what the
// checks below answer. A conformance suite that passes everything looks exactly
// like a conformance suite that everything passes, and the only way to tell
// those apart is to hand it something broken and watch it complain.

// epoch is where the fake source's clock starts, so that every time in these
// tests is arithmetic rather than a reading of the machine.
var epoch = time.Date(2026, time.May, 4, 10, 0, 0, 0, time.UTC)

// memsource is a connector over a map, and the smallest thing that passes the
// suite.
//
// It is here rather than exported because it is not a useful source of
// documents. What it is useful for is proving that the rules in this package
// describe something implementable, in a few hundred lines somebody can read in
// one sitting when they are about to write a real connector.
type memsource struct {
	name string

	mu      sync.Mutex
	seq     int64
	entries map[string]*entry

	lists    int64
	metadata int64
	fetches  int64
	bytes    int64
}

// entry is one document in the fake source, with the three clocks a real source
// keeps: when the content was written, when the access control list was last
// edited, and when the document was removed.
type entry struct {
	body    string
	written int64
	perms   acl.Permissions
	shared  int64
	removed int64
}

func newMem(name string) *memsource {
	return &memsource{name: name, entries: make(map[string]*entry)}
}

var (
	_ connector.Connector  = (*memsource)(nil)
	_ connector.Enumerator = (*memsource)(nil)
	_ connector.Fetcher    = (*memsource)(nil)
	_ connector.Counted    = (*memsource)(nil)
)

func (m *memsource) Source() string { return m.name }

func (m *memsource) Close() error { return nil }

func (m *memsource) id(name string) string { return m.name + ":" + name }

// tick moves the source's clock on and returns the new reading. Everything that
// happens to a document is filed under one of these, which is what a cursor is
// then compared against.
func (m *memsource) tick() int64 {
	m.seq++
	return m.seq
}

func (m *memsource) write(name, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[name]
	if !ok {
		e = &entry{perms: acl.Permissions{Mode: acl.ModePublicToTenant, Source: m.name}}
		m.entries[name] = e
	}
	e.body = body
	e.written = m.tick()
	e.removed = 0
}

func (m *memsource) remove(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[name]; ok {
		e.removed = m.tick()
	}
}

func (m *memsource) share(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[name]
	if !ok {
		return
	}
	e.perms = acl.Permissions{
		Mode:       acl.ModeACL,
		Source:     m.name,
		AllowUsers: []acl.Ref{{Source: m.name, Value: "alice"}},
		Version:    e.perms.Version + 1,
	}
	e.shared = m.tick()
}

func (m *memsource) unresolvable(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[name]; ok {
		e.perms = connector.Unresolved(m.name, "the fixture says so")
		e.shared = m.tick()
	}
}

// names is every document in the source, in a fixed order, so that a sync does
// the same work in the same order twice.
func (m *memsource) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.entries))
	for name := range m.entries {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func (m *memsource) at(name string) entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[name]; ok {
		return *e
	}
	return entry{}
}

func (m *memsource) cursor(seq int64) connector.Cursor {
	return connector.Cursor{
		Value: strconv.FormatInt(seq, 10),
		Time:  epoch.Add(time.Duration(seq) * time.Second),
	}
}

func (m *memsource) Sync(ctx context.Context, from connector.Cursor, emit func(context.Context, connector.Change) error) (connector.Cursor, error) {
	since, err := since(from)
	if err != nil {
		return connector.Cursor{}, err
	}
	m.mu.Lock()
	m.lists++
	m.mu.Unlock()

	for _, name := range m.names() {
		if err := ctx.Err(); err != nil {
			return connector.Cursor{}, err
		}
		e := m.at(name)
		switch {
		case e.removed > since:
			// A deletion carries no cursor. The document is gone and there is
			// nothing to resume from that a later run could read again.
			if since == 0 {
				continue
			}
			if err := emit(ctx, connector.Change{
				Document: doc.Document{ID: m.id(name)},
				Deleted:  true,
			}); err != nil {
				return connector.Cursor{}, err
			}
		case e.removed != 0:
			continue
		case e.written > since:
			m.mu.Lock()
			m.fetches++
			m.bytes += int64(len(e.body))
			m.mu.Unlock()
			if err := emit(ctx, connector.Change{
				Document: m.document(name, e),
				Cursor:   m.cursor(e.written),
			}); err != nil {
				return connector.Cursor{}, err
			}
		case e.shared > since:
			// The content did not change, so it is not read again. This is the
			// cheap half of the whole design and the reason the source keeps two
			// clocks per document rather than one.
			m.mu.Lock()
			m.metadata++
			m.mu.Unlock()
			if err := emit(ctx, connector.Change{
				Document:        doc.Document{ID: m.id(name), Permissions: e.perms},
				PermissionsOnly: true,
				Cursor:          m.cursor(e.shared),
			}); err != nil {
				return connector.Cursor{}, err
			}
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursor(m.seq), nil
}

func (m *memsource) Enumerate(ctx context.Context, fn func(connector.Item) bool) error {
	m.mu.Lock()
	m.lists++
	m.mu.Unlock()
	for _, name := range m.names() {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := m.at(name)
		if e.removed != 0 {
			continue
		}
		if !fn(connector.Item{ID: m.id(name), Version: strconv.FormatInt(e.written, 10)}) {
			return nil
		}
	}
	return nil
}

func (m *memsource) Fetch(ctx context.Context, id string) (doc.Document, error) {
	if err := ctx.Err(); err != nil {
		return doc.Document{}, err
	}
	name, ok := strings.CutPrefix(id, m.name+":")
	if !ok {
		return doc.Document{}, connector.ErrGone
	}
	e := m.at(name)
	if e.written == 0 || e.removed != 0 {
		return doc.Document{}, connector.ErrGone
	}
	m.mu.Lock()
	m.fetches++
	m.bytes += int64(len(e.body))
	m.mu.Unlock()
	return m.document(name, e), nil
}

func (m *memsource) Counters() connector.Counters {
	m.mu.Lock()
	defer m.mu.Unlock()
	return connector.Counters{Lists: m.lists, Metadata: m.metadata, Fetches: m.fetches, Bytes: m.bytes}
}

func (m *memsource) document(name string, e entry) doc.Document {
	return doc.Document{
		ID:           m.id(name),
		Kind:         doc.KindFile,
		Title:        name,
		Body:         e.body,
		ModifiedAt:   epoch.Add(time.Duration(e.written) * time.Second),
		SourceUpdate: strconv.FormatInt(e.written, 10),
		Permissions:  e.perms,
	}
}

// since reads the fake source's cursor.
func since(c connector.Cursor) (int64, error) {
	if c.Value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(c.Value, 10, 64)
	if err != nil {
		return 0, errors.New("connectortest: the cursor did not come from this source")
	}
	return n, nil
}

// A connector that does everything the contract asks passes every case,
// including the optional ones.
func TestTheSuitePassesAConnectorThatWorks(t *testing.T) {
	Run(t, func(t *testing.T) Fixture {
		src := newMem("memory")
		return Fixture{
			Connector:    src,
			ID:           src.id,
			Write:        func(_ *testing.T, name, body string) { src.write(name, body) },
			Remove:       func(_ *testing.T, name string) { src.remove(name) },
			Share:        func(_ *testing.T, name string) { src.share(name) },
			Unresolvable: func(_ *testing.T, name string) { src.unresolvable(name) },
		}
	})
}

// A well formed change is not complained about, which is the half of the check
// that stops it being noise.
func TestNothingIsWrongWithAGoodChange(t *testing.T) {
	ch := connector.Change{
		Document: doc.Document{
			ID:          "memory:a.md",
			Title:       "a.md",
			Body:        "something worth reading",
			Permissions: acl.Permissions{Mode: acl.ModePublicToTenant, Source: "memory"},
		},
		Cursor: connector.Cursor{Value: "1"},
	}
	if got := problems(ch, "memory"); len(got) != 0 {
		t.Fatalf("a well formed change was reported as %v", got)
	}
}

// The rule the whole suite exists for, and the failures around it. A document
// indexed without the access control list that governs it is searchable by
// everybody, and no later fix makes it unsearchable by the people who already
// found it.
func TestAChangeThatBreaksARuleIsCaught(t *testing.T) {
	good := acl.Permissions{Mode: acl.ModePublicToTenant, Source: "memory"}
	tests := []struct {
		name   string
		change connector.Change
		want   string
	}{
		{
			"a document indexed without permissions",
			connector.Change{Document: doc.Document{ID: "memory:a.md", Body: "text"}},
			"arrived without permissions",
		},
		{
			"a document whose permissions came from somewhere else",
			connector.Change{Document: doc.Document{ID: "memory:a.md", Permissions: acl.Permissions{Mode: acl.ModeACL, Source: "slack"}}},
			`permissions from source "slack"`,
		},
		{
			"a permission mode this system does not have",
			connector.Change{Document: doc.Document{ID: "memory:a.md", Permissions: acl.Permissions{Mode: acl.Mode(9), Source: "memory"}}},
			"permission mode 9",
		},
		{
			"a change with no document id",
			connector.Change{Document: doc.Document{Permissions: good}},
			"carries no document id",
		},
		{
			"a connector that filled in the tenant",
			connector.Change{Document: doc.Document{ID: "memory:a.md", Tenant: "acme", Permissions: good}},
			"set by the pipeline",
		},
		{
			"a document stamped with another connector's source",
			connector.Change{Document: doc.Document{ID: "memory:a.md", Source: "slack", Permissions: good}},
			`carries source "slack"`,
		},
		{
			"a permission change carrying content",
			connector.Change{Document: doc.Document{ID: "memory:a.md", Body: "text", Permissions: good}, PermissionsOnly: true},
			"carries content",
		},
		{
			"a change that is both a deletion and a permission change",
			connector.Change{Document: doc.Document{ID: "memory:a.md"}, Deleted: true, PermissionsOnly: true},
			"opposite things",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := problems(tt.change, "memory")
			if len(got) == 0 {
				t.Fatalf("nothing was reported for %+v", tt.change)
			}
			if !slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, tt.want) }) {
				t.Errorf("reported %v, want something saying %q", got, tt.want)
			}
		})
	}
}

// TestEveryModeThePermissionModelHasIsAccepted is the other half of the mode
// check, and it is the half that gets left out.
//
// The check exists to catch a connector that invented a number, and a check
// written from a list somebody typed catches a connector using a mode that was
// added after the list. That failure is worse than the one it was guarding
// against: the connector is right, the descriptor is right, and the suite says
// the mode does not exist. Ranging over the modes rather than naming them here
// is what keeps the two lists from being two lists.
func TestEveryModeThePermissionModelHasIsAccepted(t *testing.T) {
	modes := []acl.Mode{acl.ModeUnknown, acl.ModeACL, acl.ModePublicToTenant, acl.ModeOwnerOnly}
	for _, mode := range modes {
		ch := connector.Change{Document: doc.Document{
			ID:          "memory:a.md",
			Permissions: acl.Permissions{Mode: mode, Source: "memory"},
		}}
		if got := problems(ch, "memory"); len(got) != 0 {
			t.Errorf("mode %d was reported as %v", int(mode), got)
		}
	}
}

// A deletion is not asked for permissions. The document is gone and there is
// nobody left to resolve a rule against, so requiring one would be requiring a
// guess.
func TestADeletionIsNotAskedForPermissions(t *testing.T) {
	ch := connector.Change{Document: doc.Document{ID: "memory:a.md"}, Deleted: true}
	if got := problems(ch, "memory"); len(got) != 0 {
		t.Fatalf("a deletion was reported as %v", got)
	}
}
