package objectsource_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector/connectortest"
	"github.com/tamnd/genba/connector/objectsource"
)

// TestConformance runs the connector conformance suite against this connector.
//
// It runs against the fake service for the same reason the rest of this
// package's tests do. What the suite is checking is the connector's behaviour,
// which a real bucket would answer the same way and would also make this need a
// network and an account.
func TestConformance(t *testing.T) {
	connectortest.Run(t, func(t *testing.T) connectortest.Fixture {
		store := newStore(t)
		policy := newFixturePolicy("objects")
		src, err := objectsource.New(store.client(t), "objects", policy)
		if err != nil {
			t.Fatal(err)
		}
		return connectortest.Fixture{
			Connector: src,
			ID:        func(name string) string { return "objects:" + name },
			Write: func(_ *testing.T, name, body string) {
				store.put(name, body)
				// Object storage keeps one modification time per object, to the
				// second, and this connector holds its cursor a second behind
				// the service's own clock so that an object written later in the
				// same second is not filed under a time the cursor has passed.
				// Moving the service's clock on after a write is what leaves a
				// sync something to settle on, and it is what a real bucket does
				// by itself while nobody is looking.
				store.tick(2 * time.Second)
			},
			Remove:       func(_ *testing.T, name string) { store.remove(name) },
			Share:        func(_ *testing.T, name string) { policy.share(name) },
			Unresolvable: func(_ *testing.T, name string) { policy.fail(name) },
		}
	})
}

// fixturePolicy is a policy whose answers a test can change.
//
// The bucket and object policies in this package read a real access control
// list and are tested against one elsewhere. What the suite needs is something
// that can be made to change its mind on demand, and to fail for one object
// without failing for the rest.
type fixturePolicy struct {
	source string

	mu      sync.Mutex
	at      time.Time
	perms   map[string]acl.Permissions
	changed map[string]time.Time
	broken  map[string]bool
}

func newFixturePolicy(source string) *fixturePolicy {
	return &fixturePolicy{
		source:  source,
		at:      time.Date(2026, time.May, 4, 10, 0, 0, 0, time.UTC),
		perms:   make(map[string]acl.Permissions),
		changed: make(map[string]time.Time),
		broken:  make(map[string]bool),
	}
}

var (
	_ objectsource.Policy    = (*fixturePolicy)(nil)
	_ objectsource.Versioned = (*fixturePolicy)(nil)
)

// tick moves the fixture's clock on and returns the new reading, so that a
// permission edit is always filed after the object was written.
func (p *fixturePolicy) tick() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.at = p.at.Add(time.Second)
	return p.at
}

func (p *fixturePolicy) Permissions(_ context.Context, key string) (acl.Permissions, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken[key] {
		return acl.Permissions{}, errors.New("the account this object is shared with does not resolve")
	}
	if perm, ok := p.perms[key]; ok {
		return perm, nil
	}
	return acl.Permissions{Mode: acl.ModePublicToTenant, Source: p.source}, nil
}

func (p *fixturePolicy) ChangedAt(_ context.Context, key string) (time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed[key], nil
}

// share narrows one object to one person, without the object being touched.
func (p *fixturePolicy) share(key string) {
	at := p.tick()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.perms[key] = acl.Permissions{
		Mode:       acl.ModeACL,
		Source:     p.source,
		AllowUsers: []acl.Ref{{Source: "email", Value: "alice@example.com"}},
		Version:    p.perms[key].Version + 1,
	}
	p.changed[key] = at
}

// fail puts one object's access control list beyond working out.
func (p *fixturePolicy) fail(key string) {
	at := p.tick()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.broken[key] = true
	p.changed[key] = at
}
