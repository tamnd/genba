package fssource

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tamnd/genba/acl"
)

// OwnersFile is the name of the file an [OwnersPolicy] reads.
const OwnersFile = "OWNERS"

// OwnersPolicy derives permissions from OWNERS files in the tree.
//
// This is the convention Kubernetes and a number of other large repositories
// use: a file named OWNERS in a directory lists the people who approve and
// review changes below it, and the nearest one going up the tree wins. It is
// worth supporting because it is a real access control list, maintained by real
// people for their own reasons, over a corpus anybody can check out. Developing
// a permission model against it means developing against the awkward parts,
// such as a subtree that narrows the list its parent set, which invented test
// data never has because nobody invents inconvenience.
//
// The mapping is deliberately literal. Approvers and reviewers become allowed
// users under the identity source given to [NewOwnersPolicy], and the first
// approver becomes the owner. Nothing is inferred beyond that.
//
// A file below a directory with no OWNERS file anywhere above it has no answer,
// and gets one that does not resolve, so it is quarantined rather than
// published. That is the case worth getting right: the default for "no rule
// found" is not "no restriction".
type OwnersPolicy struct {
	root     string
	source   string
	identity string
	fallback acl.Permissions
	hasFall  bool

	mu     sync.RWMutex
	byDir  map[string]*owners
	parsed map[string]bool
}

// owners is one parsed OWNERS file.
type owners struct {
	approvers []string
	reviewers []string

	// modTime is when the file was last written, which is the answer to
	// [OwnersPolicy.ChangedAt] for everything the file governs.
	modTime time.Time
}

// NewOwnersPolicy returns a policy reading OWNERS files under root.
//
// source names the connector, and identity names the identity source the
// entries in the file belong to, for example "github". Getting that second one
// right is what lets a principal who authenticated through one system match a
// list written in terms of another.
func NewOwnersPolicy(root, source, identity string) (*OwnersPolicy, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fssource: owners policy: %w", err)
	}
	return &OwnersPolicy{
		root:     abs,
		source:   source,
		identity: identity,
		byDir:    make(map[string]*owners),
		parsed:   make(map[string]bool),
	}, nil
}

// WithFallback sets the permissions used for a path with no OWNERS file above
// it.
//
// Without it those paths are quarantined. Set it when the tree genuinely has a
// default, such as a public documentation repository where the unowned files
// are as public as the owned ones, and leave it alone otherwise.
func (o *OwnersPolicy) WithFallback(p acl.Permissions) *OwnersPolicy {
	o.fallback = p
	o.hasFall = true
	return o
}

var (
	_ Policy    = (*OwnersPolicy)(nil)
	_ Versioned = (*OwnersPolicy)(nil)
	_ Reloader  = (*OwnersPolicy)(nil)
)

// Reload drops the cache so that the next lookup reads the tree again.
//
// A source calls this at the start of every walk. Without it a process that
// stays up for a week answers with the OWNERS files as they were when it
// started, and a revocation made on Tuesday is applied to nothing. Holding the
// cache for the length of one walk is what keeps the cost at one read per
// OWNERS file per sync instead of one per document.
func (o *OwnersPolicy) Reload() {
	o.mu.Lock()
	defer o.mu.Unlock()
	clear(o.byDir)
	clear(o.parsed)
}

// ChangedAt returns when the OWNERS file governing relPath was last written.
//
// A path no OWNERS file governs has no answer and returns the zero time, even
// when a fallback is set, because a fallback is a constant and a constant never
// changed.
func (o *OwnersPolicy) ChangedAt(ctx context.Context, relPath string) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	own, err := o.nearest(relPath)
	if err != nil || own == nil {
		return time.Time{}, err
	}
	return own.modTime, nil
}

// nearest walks up from a file's directory to the root and returns the first
// OWNERS file it finds, or nil.
//
// Nearest wins, which is what the convention means and what makes a subtree
// able to narrow what its parent allowed.
func (o *OwnersPolicy) nearest(relPath string) (*owners, error) {
	dir := path.Dir(relPath)
	for {
		own, err := o.at(dir)
		if err != nil {
			return nil, err
		}
		if own != nil {
			return own, nil
		}
		if dir == "." || dir == "/" || dir == "" {
			return nil, nil
		}
		dir = path.Dir(dir)
	}
}

// Permissions returns the access control list governing relPath.
func (o *OwnersPolicy) Permissions(ctx context.Context, relPath string) (acl.Permissions, error) {
	if err := ctx.Err(); err != nil {
		return acl.Permissions{}, err
	}

	own, err := o.nearest(relPath)
	if err != nil {
		return acl.Permissions{}, err
	}
	if own != nil {
		return o.permissionsFrom(own), nil
	}

	if o.hasFall {
		return o.fallback, nil
	}
	return acl.Permissions{}, fmt.Errorf("fssource: no OWNERS file governs %q", relPath)
}

// permissionsFrom maps a parsed file onto a descriptor.
func (o *OwnersPolicy) permissionsFrom(own *owners) acl.Permissions {
	p := acl.Permissions{Mode: acl.ModeACL, Source: o.source}
	for _, name := range own.approvers {
		p.AllowUsers = append(p.AllowUsers, acl.Ref{Source: o.identity, Value: name})
	}
	for _, name := range own.reviewers {
		p.AllowUsers = append(p.AllowUsers, acl.Ref{Source: o.identity, Value: name})
	}
	if len(own.approvers) > 0 {
		p.Owner = acl.Ref{Source: o.identity, Value: own.approvers[0]}
	}
	return p
}

// at returns the parsed OWNERS file for a directory, or nil if there is none.
// Results are cached because a large tree asks about the same directory once
// per file in it.
func (o *OwnersPolicy) at(dir string) (*owners, error) {
	o.mu.RLock()
	own := o.byDir[dir]
	done := o.parsed[dir]
	o.mu.RUnlock()
	if done {
		return own, nil
	}

	full := filepath.Join(o.root, filepath.FromSlash(dir), OwnersFile)
	b, err := os.ReadFile(full)
	switch {
	case os.IsNotExist(err):
		own = nil
	case err != nil:
		return nil, fmt.Errorf("fssource: read %s: %w", full, err)
	default:
		own = parseOwners(string(b))
		// The stat is separate from the read on purpose. Reading first and then
		// asking how old it is means the time belongs to the bytes that were
		// parsed, where the other order can record a time for content that was
		// replaced in between and leave the change invisible until the file is
		// touched again.
		if info, err := os.Stat(full); err == nil {
			own.modTime = info.ModTime()
		}
	}

	o.mu.Lock()
	o.byDir[dir] = own
	o.parsed[dir] = true
	o.mu.Unlock()
	return own, nil
}

// parseOwners reads the approvers and reviewers out of an OWNERS file.
//
// The real files are YAML, and this reads the two list keys it needs rather
// than pulling in a YAML parser for them. That is a defensible trade for a
// format this shape, and it is worth being clear about what it gives up: a file
// using anchors, flow sequences or nested aliases is not understood. Such a
// file yields no entries, which quarantines its subtree rather than opening it,
// so the failure is safe as well as visible.
func parseOwners(text string) *owners {
	out := &owners{}
	var current *[]string

	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if key, rest, ok := strings.Cut(trimmed, ":"); ok && !strings.HasPrefix(trimmed, "-") {
			switch strings.TrimSpace(key) {
			case "approvers":
				current = &out.approvers
			case "reviewers":
				current = &out.reviewers
			default:
				// Any other key ends the list that was being read. Without this
				// a key such as labels would swallow its own values into
				// whichever list came before it.
				current = nil
			}
			if rest = strings.TrimSpace(rest); rest != "" && current != nil {
				// An inline list, approvers: [alice, bob].
				rest = strings.TrimPrefix(rest, "[")
				rest = strings.TrimSuffix(rest, "]")
				for item := range strings.SplitSeq(rest, ",") {
					if name := cleanName(item); name != "" {
						*current = append(*current, name)
					}
				}
			}
			continue
		}

		if item, ok := strings.CutPrefix(trimmed, "-"); ok && current != nil {
			if name := cleanName(item); name != "" {
				*current = append(*current, name)
			}
		}
	}
	return out
}

// cleanName strips the quoting and comments a name can arrive wrapped in, and
// returns empty for anything that is YAML machinery rather than a person.
//
// The rejection matters more than the stripping. An anchor, an alias or a merge
// key is syntax this parser does not implement, and reading one as a name would
// grant access to a user account called "&anchor" that will never exist, which
// is a silent partial failure. Returning nothing instead leaves the list empty,
// and an empty list allows nobody.
func cleanName(s string) string {
	if i := strings.Index(s, "#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	switch s[0] {
	case '&', '*', '<', '{', '[', '!', '?', '|', '>', '%', '@', '`':
		return ""
	}
	// A real account name has no spaces in it. Anything that does is a phrase
	// this parser has misread the structure of.
	if strings.ContainsAny(s, " \t:,") {
		return ""
	}
	return s
}
