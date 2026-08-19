package acl

import (
	"fmt"
	"hash/fnv"
)

// Visibility is the set of ordinals one principal may read in one segment.
//
// It is resolved once per query and then intersected with every posting list
// the query touches, which is what keeps a document the asker cannot read out
// of the candidate set entirely. A document that never enters the candidate set
// cannot influence a score, a facet count or a snippet, so there is nothing
// left to leak later in the pipeline.
type Visibility struct {
	Bitmap *Bitmap
	Key    Key
}

// Key identifies a resolved visibility set. Two requests with the same key see
// the same documents, which is what makes the set safe to cache and reuse.
//
// The group set version is part of the key on purpose. When somebody is removed
// from a group the version moves, every key derived from the old membership
// stops matching, and the cached bitmaps are dead the moment the change lands
// rather than when a timer expires.
type Key struct {
	Tenant       string
	Subject      string
	GroupVersion uint64
	Segment      string
	PermVersion  uint64
}

// String returns a stable cache key.
func (k Key) String() string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s\x00%d", k.Tenant, k.Subject, k.GroupVersion, k.Segment, k.PermVersion)
	return fmt.Sprintf("%016x", h.Sum64())
}

// Resolve builds the visibility set for a principal over a segment's documents.
//
// perms is indexed by ordinal, so perms[3] is the descriptor of the document at
// ordinal 3. A nil principal resolves to an empty set rather than to everything,
// because the failure mode of the opposite default is a breach.
func Resolve(p *Principal, segment string, permVersion uint64, perms []Permissions) Visibility {
	b := NewBitmap(len(perms))
	if p != nil {
		for i, perm := range perms {
			if perm.Allows(p) {
				b.Add(Ordinal(i))
			}
		}
	}
	v := Visibility{Bitmap: b}
	if p != nil {
		v.Key = Key{
			Tenant:       p.Tenant,
			Subject:      p.Subject,
			GroupVersion: p.Groups.Version,
			Segment:      segment,
			PermVersion:  permVersion,
		}
	}
	return v
}

// Filter keeps only the ordinals the principal may read. Callers hand it a
// candidate set from a posting list and get back the same set with everything
// invisible removed, in place.
func (v Visibility) Filter(candidates *Bitmap) {
	candidates.Intersect(v.Bitmap)
}
