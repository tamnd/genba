package acl

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
)

// A cache key that does not name the asker's visibility is a permission bug.
//
// Two people typing the same query get different answers, and a cache that
// hands one of those answers to the other has leaked a document. It will not
// look like a leak in any log or any test. It will look like a search result.
//
// So nothing derived from a permission filtered query is cached under the query
// alone. [Fingerprint] is the other half of every such key.

// FingerprintBytes is how much of the hash a fingerprint keeps.
//
// Sixteen bytes, because a collision here is two different views sharing a
// cache entry, which is the leak this file exists to prevent. At a hundred and
// twenty eight bits an accidental collision is not something that happens to a
// deployment, and the eight bytes saved by truncating further buy nothing worth
// having.
const FingerprintBytes = 16

// Fingerprint is a short, stable name for everything the visibility rule reads
// about a principal.
//
// Two principals share a fingerprint exactly when the set of documents they may
// read is the same set, so far as the permission rule can tell. Anything
// derived from a permission filtered query can be cached under it.
//
// It is built from the same key accessors the rule itself uses,
// [Principal.UserKeys] and [Principal.GroupKeys], rather than from the fields
// underneath them. A fingerprint computed from different fields than the rule
// reads is a fingerprint that will eventually disagree with it, and the
// disagreement is a leak rather than a stale result.
//
// The keys are sorted before they are hashed, because two principals whose
// groups arrived in a different order are the same view and should share an
// entry. Sorting is done on a copy: [Principal.GroupKeys] returns the
// principal's own slice, and reordering it under a caller would be a rude
// surprise.
//
// The group expansion version is part of it as well. It is the field the
// permission model already designates as the safety catch on derived state, and
// including it means a session holding a stale expansion cannot reach entries
// built from a fresh one. That costs a cache entry when two sessions were
// expanded either side of a directory change, which is correct: at that moment
// they are not the same view.
//
// A nil principal fingerprints to the empty string. Nothing is cached under it,
// because there is no anonymous read path to cache.
func Fingerprint(p *Principal) string {
	if p == nil {
		return ""
	}
	h := sha256.New()

	// Every component is written with its length in front of it, so that no
	// choice of tenant, subject or group name can be rearranged into another
	// principal's fingerprint. Without the lengths, a tenant of "ac" with a group
	// "me:x" and a tenant of "acme" with a group ":x" hash the same.
	write := func(s string) {
		_, _ = h.Write([]byte(strconv.Itoa(len(s))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(s))
	}

	write(p.Tenant)
	write(strconv.FormatUint(p.Groups.Version, 10))
	for _, keys := range [][]string{p.UserKeys(), p.GroupKeys()} {
		sorted := slices.Clone(keys)
		slices.Sort(sorted)
		sorted = slices.Compact(sorted)
		write(strconv.Itoa(len(sorted)))
		for _, k := range sorted {
			write(k)
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:FingerprintBytes])
}
