package directory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/tamnd/genba/acl"
)

// A directory written down.
//
// The shape is meant to be typed by hand rather than generated, because the
// deployment this is for is the one with forty people and six groups, and
// making them stand up an identity provider in order to try a search engine is
// how a search engine does not get tried.
//
//	{
//	  "name": "acme",
//	  "groups": [
//	    {"id": "everyone"},
//	    {"id": "engineering", "member_of": ["everyone"]}
//	  ],
//	  "subjects": [
//	    {"id": "mei", "email": "mei@acme.com", "identities": ["slack:U04AB"], "member_of": ["engineering"]}
//	  ]
//	}
//
// Identities are "source:value" because that is the convention everywhere else,
// and a pair of nested objects to say the same thing is three lines of typing
// for no more meaning.
type file struct {
	Name     string        `json:"name"`
	Groups   []fileGroup   `json:"groups"`
	Subjects []fileSubject `json:"subjects"`
}

type fileGroup struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	MemberOf []string `json:"member_of"`
}

type fileSubject struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Email      string   `json:"email"`
	Version    string   `json:"version"`
	Identities []string `json:"identities"`
	MemberOf   []string `json:"member_of"`
	Disabled   bool     `json:"disabled"`
}

// OpenStatic reads a directory out of a file.
//
// The error names the path, because the person who gets it is looking at a unit
// file and a directory they wrote by hand, and "unexpected end of JSON input"
// on its own tells them nothing about which of the two is wrong.
func OpenStatic(path string) (*Static, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	defer func() { _ = f.Close() }()

	d, err := ReadStatic(f)
	if err != nil {
		return nil, fmt.Errorf("directory %s: %w", path, err)
	}
	return d, nil
}

// ReadStatic reads a directory out of JSON.
//
// It is strict on purpose. An unknown field is a typo rather than a future
// version of the format, a group named in a membership and not defined is a
// typo too, and both of them fail at startup rather than turning into somebody
// mysteriously missing a group at nine o'clock. This is a file somebody wrote,
// and the whole advantage a file has over an identity provider is that its
// mistakes can be caught before anybody signs in.
func ReadStatic(r io.Reader) (*Static, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var in file
	if err := dec.Decode(&in); err != nil {
		return nil, err
	}
	// Anything after the object is a second document pasted in by accident, and
	// silently reading the first half of a file is worse than refusing it.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one directory in the file")
	}
	if in.Name == "" {
		return nil, errors.New("no name, and a directory without one cannot have its groups told apart from another's")
	}

	d := NewStatic(in.Name)
	defined := make(map[string]bool, len(in.Groups))
	for _, g := range in.Groups {
		if g.ID == "" {
			return nil, errors.New("a group with no id")
		}
		if defined[g.ID] {
			return nil, fmt.Errorf("group %q appears twice", g.ID)
		}
		defined[g.ID] = true
		d.PutGroup(Group{ID: g.ID, Name: g.Name, Version: g.Version, MemberOf: slices.Clone(g.MemberOf)})
	}

	// The membership check happens after every group is defined, so the order
	// they are written in is not something anybody has to think about.
	for _, g := range in.Groups {
		for _, of := range g.MemberOf {
			if !defined[of] {
				return nil, fmt.Errorf("group %q is a member of %q, which is not defined here", g.ID, of)
			}
		}
	}

	seen := make(map[string]bool, len(in.Subjects))
	for _, s := range in.Subjects {
		if s.ID == "" {
			return nil, errors.New("a subject with no id")
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("subject %q appears twice", s.ID)
		}
		seen[s.ID] = true

		ids := make([]acl.Identity, 0, len(s.Identities))
		for _, raw := range s.Identities {
			id, err := identity(raw)
			if err != nil {
				return nil, fmt.Errorf("subject %q: %w", s.ID, err)
			}
			ids = append(ids, id)
		}
		for _, of := range s.MemberOf {
			if !defined[of] {
				return nil, fmt.Errorf("subject %q is a member of %q, which is not defined here", s.ID, of)
			}
		}

		d.Put(Subject{
			ID:         s.ID,
			Name:       s.Name,
			Email:      s.Email,
			Version:    s.Version,
			Identities: ids,
			MemberOf:   slices.Clone(s.MemberOf),
			Disabled:   s.Disabled,
		})
	}
	return d, nil
}

// identity splits "slack:U04AB" the way the rest of the system writes one.
func identity(raw string) (acl.Identity, error) {
	source, value, ok := strings.Cut(raw, ":")
	if !ok || source == "" || value == "" {
		return acl.Identity{}, fmt.Errorf("identity %q is not source:value", raw)
	}
	return acl.Identity{Source: source, Value: value}, nil
}
