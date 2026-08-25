package fssource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tamnd/genba/acl"
)

// Checker answers whether somebody may still read a file, while a response is
// being written rather than at the next sync.
//
// The permissions in the index were read when the crawler last came past. On a
// tree people are working in, that copy goes wrong in the direction that
// matters: an OWNERS file is edited on Monday morning, a mode bit is taken away
// at lunchtime, and until the next walk the index still says the old thing. A
// checker closes that window by reading the rule again, per request, for the
// handful of documents that are about to go on somebody's screen.
//
// It is cheap here and that is why the filesystem is the first source to have
// one. The answer is on the same machine: the id carries the path, the rule is
// a file a few directories up, and both are almost always in the page cache. A
// source reached over somebody else's API is a different conversation about
// what fits inside a query, and it is not this type.
//
// The shape is what the recheck package asks for, so a [Checker] can be
// registered under a source name without either package importing the other.
//
// # The policy has to be its own
//
// A checker must not be given the same [Policy] the running source holds.
// [OwnersPolicy] caches its answers for the length of a walk, which is right
// for a sync and wrong here: a check reading that cache would be answering out
// of the snapshot it exists to go around, and would agree with the stale index
// every time. So a checker reloads its policy before every batch, and a policy
// being reloaded underneath a sync would cost that sync its cache.
//
// Build a second one. They are cheap, and the two have different lifetimes for
// a reason.
type Checker struct {
	root   string
	name   string
	policy Policy
}

// NewChecker returns a checker for the tree under root.
//
// name is the source name the documents carry, which is what the ids are
// prefixed with and what the checker is registered under.
//
// A nil policy is refused rather than treated as permissive. A checker without
// one would answer no to everything, which is safe and is also a server that
// serves nothing, and the difference between the two is worth finding at
// startup rather than in a support conversation.
func NewChecker(root, name string, policy Policy) (*Checker, error) {
	if name == "" {
		return nil, errors.New("fssource: checker: the source has no name, and the ids to check are prefixed with it")
	}
	if policy == nil {
		return nil, errors.New("fssource: checker: no permission policy, which would refuse every document in the tree")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fssource: checker: %w", err)
	}
	return &Checker{root: abs, name: name, policy: policy}, nil
}

// Source returns the name of the source this checker answers for.
func (c *Checker) Source() string { return c.name }

// Allowed reports which of the ids the principal may still read.
//
// An id left out of the answer is not an allow. The caller reads a missing id
// as a check that did not happen and removes the document, so everything this
// method cannot answer for is simply not in the map: an id from another source,
// a path that tried to leave the tree, a file it could not stat, and a rule
// that stopped resolving. What is in the map with a false is the ordinary
// answer, that the file is gone or that this person is no longer on it.
//
// The policy is reloaded once per call, not once per id, which is what keeps a
// page of results at one read of each OWNERS file rather than one per document.
func (c *Checker) Allowed(ctx context.Context, p *acl.Principal, ids []string) (map[string]bool, error) {
	if r, ok := c.policy.(Reloader); ok {
		r.Reload()
	}

	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			// Whatever has been decided so far is returned with it. The ids that
			// were not reached are missing from the map, which is exactly how a
			// caller reads a check that ran out of time.
			return out, err
		}
		rel, ok := relOf(c.name, id)
		if !ok {
			continue
		}
		full := filepath.Join(c.root, filepath.FromSlash(rel))
		if !c.inside(full) {
			continue
		}

		info, err := os.Lstat(full)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// A deleted file is an answer rather than a failure, and it is the
			// one this catches soonest: the row leaves the results the moment
			// somebody removes the file, and the reconciliation that takes it out
			// of the index can happen in its own time.
			out[id] = false
			continue
		case err != nil:
			continue
		case !info.Mode().IsRegular():
			// A directory or a device with a document's id is not a document
			// anybody is reading, and the policies below are written about files.
			out[id] = false
			continue
		}

		perms, err := permissionsOf(ctx, c.policy, c.name, rel, info)
		if err != nil {
			continue
		}
		// A path whose rule no longer resolves comes back quarantined, which
		// allows nobody, so it is denied here for the same reason the sync
		// quarantines it: nobody can currently say who may read it.
		out[id] = perms.Allows(p)
	}
	return out, nil
}

// inside reports whether a path is under the root.
//
// [relOf] has already collapsed the obvious ways out, and this is the one that
// does not depend on reading the string correctly. It is here because the ids
// arrive from the index, and an id is a string a connector wrote that turns
// into a file path.
func (c *Checker) inside(full string) bool {
	return strings.HasPrefix(full, c.root+string(filepath.Separator))
}
