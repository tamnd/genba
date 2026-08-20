package objectsource_test

import (
	"slices"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/objectsource"
)

// What an access control list turns into.
//
// These are the assertions worth having about this connector, because they are
// the ones whose failure is a document shown to somebody who was not meant to
// see it. Each case is a list a real bucket can have, and the answer is the
// permission descriptor the rest of the platform filters on.
func TestWhatAnAccessControlListMeans(t *testing.T) {
	for _, c := range []struct {
		name  string
		list  string
		check func(t *testing.T, got acl.Permissions)
	}{
		{
			name: "a read grant to somebody in the tenant is a reader",
			list: listOf("owner@example.com", userGrant("reader@example.com", "READ")),
			check: func(t *testing.T, got acl.Permissions) {
				if want := []string{"reader@example.com"}; !slices.Equal(refs(got.AllowUsers), want) {
					t.Errorf("allows %v, want %v", refs(got.AllowUsers), want)
				}
				if got.Mode != acl.ModeACL {
					t.Errorf("the mode is %v, want a list", got.Mode)
				}
			},
		},
		{
			name: "writing to a bucket is not reading from it",
			list: listOf("owner@example.com",
				userGrant("reader@example.com", "READ"),
				userGrant("uploader@example.com", "WRITE"),
			),
			check: func(t *testing.T, got acl.Permissions) {
				// This is the mapping most worth pinning. WRITE on a bucket is
				// permission to put objects in it, and a mapping that read every
				// permission as read would hand a private bucket to whoever can
				// upload to it.
				if want := []string{"reader@example.com"}; !slices.Equal(refs(got.AllowUsers), want) {
					t.Errorf("allows %v, want %v", refs(got.AllowUsers), want)
				}
			},
		},
		{
			name: "changing the list is not reading the object either",
			list: listOf("owner@example.com",
				userGrant("reader@example.com", "READ"),
				userGrant("auditor@example.com", "READ_ACP"),
				userGrant("admin@example.com", "WRITE_ACP"),
			),
			check: func(t *testing.T, got acl.Permissions) {
				if want := []string{"reader@example.com"}; !slices.Equal(refs(got.AllowUsers), want) {
					t.Errorf("allows %v, want %v", refs(got.AllowUsers), want)
				}
			},
		},
		{
			name: "full control includes reading",
			list: listOf("owner@example.com", userGrant("everything@example.com", "FULL_CONTROL")),
			check: func(t *testing.T, got acl.Permissions) {
				if want := []string{"everything@example.com"}; !slices.Equal(refs(got.AllowUsers), want) {
					t.Errorf("allows %v, want %v", refs(got.AllowUsers), want)
				}
			},
		},
		{
			name: "a bucket left open to the internet is public",
			list: listOf("owner@example.com", groupGrant(uriAllUsers, "READ")),
			check: func(t *testing.T, got acl.Permissions) {
				if got.Mode != acl.ModePublicToTenant {
					t.Errorf("the mode is %v, want it public", got.Mode)
				}
			},
		},
		{
			name: "every account at the provider is not this company",
			list: listOf("owner@example.com", groupGrant(uriAuthenticatedUsers, "READ")),
			check: func(t *testing.T, got acl.Permissions) {
				// Anybody with an account at the provider is a set nobody here
				// can enumerate and is emphatically not the tenant. Quarantining
				// is the only honest answer, and the alternative is publishing a
				// bucket to every customer the provider has.
				if got.Mode != acl.ModeUnknown {
					t.Errorf("the mode is %v, want it unresolved", got.Mode)
				}
			},
		},
		{
			name: "a bucket owned by one account and shared with nobody is owner only",
			list: ownedBy("owner@example.com"),
			check: func(t *testing.T, got acl.Permissions) {
				if got.Mode != acl.ModeOwnerOnly {
					t.Errorf("the mode is %v, want owner only", got.Mode)
				}
				if got.Owner.Value != "owner@example.com" {
					t.Errorf("the owner is %q", got.Owner.Value)
				}
			},
		},
		{
			name: "a grant naming nobody is not a grant to everybody",
			list: `<AccessControlPolicy><Owner><ID>abc123</ID></Owner><AccessControlList>` +
				`<Grant><Grantee></Grantee><Permission>READ</Permission></Grant>` +
				`</AccessControlList></AccessControlPolicy>`,
			check: func(t *testing.T, got acl.Permissions) {
				if got.Mode != acl.ModeUnknown {
					t.Errorf("the mode is %v, want it unresolved", got.Mode)
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := newStore(t)
			store.setACL(c.list)

			policy, err := objectsource.NewBucketPolicy(store.client(t), sourceName, identity, "example.com")
			if err != nil {
				t.Fatal(err)
			}
			// An error here is the quarantine being reported rather than
			// something going wrong, and the descriptor is what is being
			// checked either way.
			got, _ := policy.Permissions(t.Context(), "")
			if got.Source != sourceName {
				t.Errorf("the descriptor names %q as its source, want %q", got.Source, sourceName)
			}
			c.check(t, got)
		})
	}
}

// The identity source has to be on every reference, because a name on its own
// says nothing about which directory it can be looked up in, and two
// directories with the same name in them is the normal case rather than the odd
// one.
func TestEveryReferenceNamesTheDirectoryItCameFrom(t *testing.T) {
	store := newStore(t)
	store.setACL(listOf("owner@example.com",
		userGrant("reader@example.com", "READ"),
		userGrant("other@example.com", "READ"),
	))

	policy, err := objectsource.NewBucketPolicy(store.client(t), sourceName, identity)
	if err != nil {
		t.Fatal(err)
	}
	got, err := policy.Permissions(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got.AllowUsers {
		if r.Source != identity {
			t.Errorf("%q came from %q, want %q", r.Value, r.Source, identity)
		}
	}
}

// A bucket whose list cannot be read is one problem, and asking it once per
// object turns one problem into a denial of service against the store.
func TestABucketWithNoAccessControlListIsAskedOncePerSync(t *testing.T) {
	store := newStore(t)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		store.put(name+".md", "the "+name+" file")
	}
	store.settle()

	client := store.client(t)
	policy, err := objectsource.NewBucketPolicy(client, sourceName, identity)
	if err != nil {
		t.Fatal(err)
	}
	source, err := objectsource.New(client, sourceName, policy)
	if err != nil {
		t.Fatal(err)
	}

	store.requests()
	changes, _ := syncAll(t, source, connector.Cursor{})
	if len(changes) != 5 {
		t.Fatalf("read %d documents, want 5", len(changes))
	}
	for _, c := range changes {
		if c.Document.Permissions.Mode != acl.ModeUnknown {
			t.Errorf("%s is %v, want it quarantined", c.Document.ID, c.Document.Permissions.Mode)
		}
	}
	if got := countACLs(store.requests()); got != 1 {
		t.Errorf("asked for the access control list %d times, want once", got)
	}
}

// The per object policy really does read one list per object, which is why it
// has to be asked for by name rather than being the default.
func TestTheObjectPolicyReadsTheListOfEachObject(t *testing.T) {
	store := newStore(t)
	store.put("open.md", "anybody in the tenant")
	store.put("closed.md", "one person")
	store.setACL(listOf("owner@example.com", userGrant("owner@example.com", "READ")))
	store.setObjectACL("open.md", listOf("owner@example.com", groupGrant(uriAllUsers, "READ")))
	store.setObjectACL("closed.md", listOf("owner@example.com", userGrant("one@example.com", "READ")))
	store.settle()

	client := store.client(t)
	policy, err := objectsource.NewObjectPolicy(client, sourceName, identity, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	source, err := objectsource.New(client, sourceName, policy)
	if err != nil {
		t.Fatal(err)
	}

	changes, _ := syncAll(t, source, connector.Cursor{})
	byID := make(map[string]acl.Permissions, len(changes))
	for _, c := range changes {
		byID[c.Document.ID] = c.Document.Permissions
	}

	if got := byID["bucket:open.md"].Mode; got != acl.ModePublicToTenant {
		t.Errorf("open.md is %v, want it public", got)
	}
	if got := refs(byID["bucket:closed.md"].AllowUsers); !slices.Equal(got, []string{"one@example.com"}) {
		t.Errorf("closed.md allows %v, want the one person named on it", got)
	}
}
