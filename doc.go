// Package genba is the root of an enterprise knowledge intelligence platform.
//
// genba indexes the documents a company already has, models how they relate to
// each other and to the people who wrote them, enforces the permissions of the
// systems the content came from, and serves that context through search, a
// grounded assistant and agents.
//
// The project is a library first and a set of binaries second. Every package
// under this module is importable on its own, so a program that only wants a
// permission aware index can take [github.com/tamnd/genba/index] and nothing
// else. There is no internal directory anywhere in the tree, which means every
// exported name is part of the public surface and is governed accordingly.
//
// # Layout
//
//	acl      principals, groups, document permissions, visibility bitmaps
//	doc      the canonical document model every source normalises into
//	store    the storage interface and its drivers
//	index    retrieval built on a store, with permissions applied in the scan
//	config   typed configuration loading
//	api      the HTTP surface
//	web      the built web client, embedded
//	cmd      thin mains that wire the packages above together
//
// # Permissions
//
// Every path that can turn a query into document content takes an
// [github.com/tamnd/genba/acl.Principal]. That is not a convention, it is the
// reason the type appears in so many signatures: a retrieval call that cannot
// name the person asking has no way to filter, and a filter applied after
// ranking leaks through counts, facets and snippets even when the final list
// looks correct.
package genba
