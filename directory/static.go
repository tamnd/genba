package directory

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
)

// Static is a directory held in memory.
//
// It is the reference implementation, in the sense that [directorytest] runs
// against it and a provider adapter that behaves differently is the one that is
// wrong. It is also the directory a small deployment actually uses: a company
// with forty people and six groups has the whole thing in its configuration
// file, and making them stand up an identity provider to try a search engine is
// how a search engine does not get tried.
//
// It is safe for concurrent use, and it is meant to be written to while it is
// being read from, because that is what reloading a configuration file is.
type Static struct {
	name string

	mu       sync.RWMutex
	subjects map[string]Subject
	groups   map[string]Group
}

// NewStatic returns an empty directory under the given identity source name.
func NewStatic(name string) *Static {
	return &Static{
		name:     name,
		subjects: make(map[string]Subject),
		groups:   make(map[string]Group),
	}
}

// Name is the identity source these ids belong to.
func (s *Static) Name() string { return s.name }

// Put adds or replaces one subject. A subject with no id is ignored rather than
// stored under the empty string, since the only thing that could look it up is
// a bug.
func (s *Static) Put(sub Subject) {
	if sub.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subjects[sub.ID] = clone(sub)
}

// PutGroup adds or replaces one group.
func (s *Static) PutGroup(g Group) {
	if g.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g.MemberOf = slices.Clone(g.MemberOf)
	s.groups[g.ID] = g
}

// Remove takes a subject out, which is what a deletion in the upstream
// directory looks like here. Deactivating somebody is [Static.Put] with
// [Subject.Disabled] set, and it is the one to reach for: directories rarely
// delete anybody, and a subject that vanished and one that was closed are
// different facts.
func (s *Static) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subjects, id)
}

// RemoveGroup takes a group out. Nothing rewrites the memberships that named
// it, on purpose: an expansion that reaches a group this directory no longer
// holds is exactly the inconsistency [Expansion.Unknown] exists to report, and
// a fake that tidied up after itself could not produce it.
func (s *Static) RemoveGroup(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.groups, id)
}

// Subject returns one subject by id.
func (s *Static) Subject(_ context.Context, id string) (Subject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.subjects[id]
	if !ok {
		return Subject{}, fmt.Errorf("%q: %w", id, ErrNoSubject)
	}
	return clone(sub), nil
}

// Group returns one group by id.
func (s *Static) Group(_ context.Context, id string) (Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	if !ok {
		return Group{}, fmt.Errorf("%q: %w", id, ErrNoGroup)
	}
	g.MemberOf = slices.Clone(g.MemberOf)
	return g, nil
}

// Load replaces the whole directory in one step, which is what a configuration
// reload wants: a reader either sees all of the old contents or all of the new
// ones, and never a moment in the middle where somebody is in no groups.
func (s *Static) Load(subjects []Subject, groups []Group) {
	subs := make(map[string]Subject, len(subjects))
	for _, sub := range subjects {
		if sub.ID != "" {
			subs[sub.ID] = clone(sub)
		}
	}
	gs := make(map[string]Group, len(groups))
	for _, g := range groups {
		if g.ID != "" {
			g.MemberOf = slices.Clone(g.MemberOf)
			gs[g.ID] = g
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subjects, s.groups = subs, gs
}

// Snapshot returns what the directory holds, for an admin screen and for tests.
func (s *Static) Snapshot() ([]Subject, []Group) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	subs := make([]Subject, 0, len(s.subjects))
	for _, sub := range slices.Sorted(maps.Keys(s.subjects)) {
		subs = append(subs, clone(s.subjects[sub]))
	}
	gs := make([]Group, 0, len(s.groups))
	for _, id := range slices.Sorted(maps.Keys(s.groups)) {
		g := s.groups[id]
		g.MemberOf = slices.Clone(g.MemberOf)
		gs = append(gs, g)
	}
	return subs, gs
}

// clone copies the slices in a subject, so that what a caller does with the
// answer cannot reach back into what the next caller is given.
func clone(sub Subject) Subject {
	sub.MemberOf = slices.Clone(sub.MemberOf)
	sub.Identities = slices.Clone(sub.Identities)
	return sub
}
