package fssource_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector/connectortest"
	"github.com/tamnd/genba/connector/fssource"
)

// TestConformance runs the connector conformance suite against this connector.
//
// This is the reference run. fssource is the connector the interface was
// written against, so a rule the suite checks that this cannot pass is a rule
// about somebody's idea of a source rather than about connectors, and belongs
// in neither place.
func TestConformance(t *testing.T) {
	connectortest.Run(t, func(t *testing.T) connectortest.Fixture {
		root := t.TempDir()
		policy := newFixturePolicy("files")
		src, err := fssource.New(root, "files", policy)
		if err != nil {
			t.Fatal(err)
		}
		return connectortest.Fixture{
			Connector: src,
			ID:        func(name string) string { return "files:" + name },
			Write: func(t *testing.T, name, body string) {
				t.Helper()
				full := filepath.Join(root, filepath.FromSlash(name))
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				// A modification time is as coarse as the platform says it is,
				// and two files written inside one test land on the same one on
				// more platforms than not. Stamping each write from a clock the
				// fixture keeps makes the order a sync sees the order the test
				// wrote in, everywhere, and it is the same thing a fixture over
				// a service with a one second clock has to do.
				at := policy.tick()
				if err := os.Chtimes(full, at, at); err != nil {
					t.Fatal(err)
				}
			},
			Remove: func(t *testing.T, name string) {
				t.Helper()
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
					t.Fatal(err)
				}
			},
			Share:        func(_ *testing.T, name string) { policy.share(name) },
			Unresolvable: func(_ *testing.T, name string) { policy.fail(name) },
		}
	})
}

// fixturePolicy is a policy whose answers a test can change.
//
// It stands in for the OWNERS file or the group export a real deployment plugs
// in here, and it keeps a clock of its own so that a permission edit is always
// filed after the last file was written. That is what a sync compares against
// when it decides whether an access control list moved since the last run.
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
	_ fssource.Policy    = (*fixturePolicy)(nil)
	_ fssource.Versioned = (*fixturePolicy)(nil)
)

// tick moves the fixture's clock on and returns the new reading.
func (p *fixturePolicy) tick() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.at = p.at.Add(time.Second)
	return p.at
}

func (p *fixturePolicy) Permissions(_ context.Context, rel string) (acl.Permissions, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken[rel] {
		return acl.Permissions{}, errors.New("the group this file is shared with does not resolve")
	}
	if perm, ok := p.perms[rel]; ok {
		return perm, nil
	}
	return acl.Permissions{Mode: acl.ModePublicToTenant, Source: p.source}, nil
}

func (p *fixturePolicy) ChangedAt(_ context.Context, rel string) (time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed[rel], nil
}

// share narrows one file to one person, which is the edit a real deployment
// makes in the file above it rather than on the file itself.
func (p *fixturePolicy) share(rel string) {
	at := p.tick()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.perms[rel] = acl.Permissions{
		Mode:       acl.ModeACL,
		Source:     p.source,
		AllowUsers: []acl.Ref{{Source: "unix", Value: "alice"}},
		Version:    p.perms[rel].Version + 1,
	}
	p.changed[rel] = at
}

// fail puts one file's access control list beyond working out.
func (p *fixturePolicy) fail(rel string) {
	at := p.tick()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.broken[rel] = true
	p.changed[rel] = at
}
