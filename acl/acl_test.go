package acl_test

import (
	"testing"

	"github.com/tamnd/genba/acl"
)

func person() *acl.Principal {
	return &acl.Principal{
		Tenant:  "acme",
		Subject: "u_mei",
		Identities: []acl.Identity{
			{Source: "gdrive", Value: "mei@acme.com"},
			{Source: "slack", Value: "U04AB"},
		},
		Groups: acl.GroupSet{
			Version: 7,
			Members: []string{"gdrive:eng@acme.com", "slack:S-platform", "everyone"},
		},
	}
}

func TestAllows(t *testing.T) {
	tests := []struct {
		name string
		perm acl.Permissions
		want bool
	}{
		{
			name: "unresolved rules deny even with a matching allow",
			perm: acl.Permissions{
				Mode:       acl.ModeUnknown,
				AllowUsers: []acl.Ref{{Source: "gdrive", Value: "mei@acme.com"}},
			},
			want: false,
		},
		{
			name: "direct user allow",
			perm: acl.Permissions{
				Mode:       acl.ModeACL,
				AllowUsers: []acl.Ref{{Source: "gdrive", Value: "mei@acme.com"}},
			},
			want: true,
		},
		{
			name: "identity of another source does not match",
			perm: acl.Permissions{
				Mode:       acl.ModeACL,
				AllowUsers: []acl.Ref{{Source: "jira", Value: "mei@acme.com"}},
			},
			want: false,
		},
		{
			name: "group allow",
			perm: acl.Permissions{
				Mode:        acl.ModeACL,
				AllowGroups: []acl.Ref{{Source: "slack", Value: "S-platform"}},
			},
			want: true,
		},
		{
			name: "group the subject is not in",
			perm: acl.Permissions{
				Mode:        acl.ModeACL,
				AllowGroups: []acl.Ref{{Source: "slack", Value: "S-legal"}},
			},
			want: false,
		},
		{
			name: "deny on the user beats a group allow",
			perm: acl.Permissions{
				Mode:        acl.ModeACL,
				AllowGroups: []acl.Ref{{Source: "slack", Value: "S-platform"}},
				DenyUsers:   []acl.Ref{{Source: "slack", Value: "U04AB"}},
			},
			want: false,
		},
		{
			name: "deny on a group beats a direct user allow",
			perm: acl.Permissions{
				Mode:       acl.ModeACL,
				AllowUsers: []acl.Ref{{Source: "gdrive", Value: "mei@acme.com"}},
				DenyGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
			},
			want: false,
		},
		{
			name: "deny beats ownership",
			perm: acl.Permissions{
				Mode:      acl.ModeACL,
				Owner:     acl.Ref{Source: "gdrive", Value: "mei@acme.com"},
				DenyUsers: []acl.Ref{{Source: "gdrive", Value: "mei@acme.com"}},
			},
			want: false,
		},
		{
			name: "public to the tenant",
			perm: acl.Permissions{Mode: acl.ModePublicToTenant},
			want: true,
		},
		{
			name: "deny beats public to the tenant",
			perm: acl.Permissions{
				Mode:       acl.ModePublicToTenant,
				DenyGroups: []acl.Ref{{Source: "slack", Value: "S-platform"}},
			},
			want: false,
		},
		{
			name: "owner only, owner asking",
			perm: acl.Permissions{
				Mode:  acl.ModeOwnerOnly,
				Owner: acl.Ref{Source: "gdrive", Value: "mei@acme.com"},
			},
			want: true,
		},
		{
			name: "owner only, somebody else asking",
			perm: acl.Permissions{
				Mode:  acl.ModeOwnerOnly,
				Owner: acl.Ref{Source: "gdrive", Value: "kenji@acme.com"},
			},
			want: false,
		},
	}

	p := person()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.perm.Allows(p); got != tt.want {
				t.Errorf("Allows() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllowsNilPrincipal(t *testing.T) {
	perms := []acl.Permissions{
		{Mode: acl.ModePublicToTenant},
		{Mode: acl.ModeACL, AllowGroups: []acl.Ref{{Value: "everyone"}}},
	}
	for _, perm := range perms {
		if perm.Allows(nil) {
			t.Fatalf("a nil principal was allowed to read mode %v", perm.Mode)
		}
	}
}

func TestModeValid(t *testing.T) {
	for _, mode := range []acl.Mode{acl.ModeUnknown, acl.ModeACL, acl.ModePublicToTenant, acl.ModeOwnerOnly} {
		if !mode.Valid() {
			t.Errorf("mode %d is one of ours and Valid said it is not", int(mode))
		}
	}
	// Anything past the last mode is a number somebody made up, or a mode from
	// a version of this package that is not the one this binary was built
	// against. Neither is a rule this system knows how to apply.
	for _, mode := range []acl.Mode{acl.Mode(4), acl.Mode(9), acl.Mode(255)} {
		if mode.Valid() {
			t.Errorf("mode %d is not one of ours and Valid said it is", int(mode))
		}
	}
}

func TestResolve(t *testing.T) {
	perms := []acl.Permissions{
		0: {Mode: acl.ModePublicToTenant},
		1: {Mode: acl.ModeACL, AllowGroups: []acl.Ref{{Source: "slack", Value: "S-legal"}}},
		2: {Mode: acl.ModeACL, AllowUsers: []acl.Ref{{Source: "gdrive", Value: "mei@acme.com"}}},
		3: {Mode: acl.ModeUnknown, AllowGroups: []acl.Ref{{Value: "everyone"}}},
		4: {Mode: acl.ModeACL, AllowGroups: []acl.Ref{{Value: "everyone"}}, DenyUsers: []acl.Ref{{Source: "slack", Value: "U04AB"}}},
	}

	v := acl.Resolve(person(), "seg-0", 12, perms)
	got := v.Bitmap.Slice()
	want := []acl.Ordinal{0, 2}
	if len(got) != len(want) {
		t.Fatalf("visible ordinals = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("visible ordinals = %v, want %v", got, want)
		}
	}
}

func TestResolveNilPrincipalSeesNothing(t *testing.T) {
	perms := []acl.Permissions{{Mode: acl.ModePublicToTenant}, {Mode: acl.ModePublicToTenant}}
	if v := acl.Resolve(nil, "seg-0", 1, perms); !v.Bitmap.IsEmpty() {
		t.Fatal("a nil principal resolved to a non empty visibility set")
	}
}

func TestKeyChangesWithGroupVersion(t *testing.T) {
	perms := []acl.Permissions{{Mode: acl.ModePublicToTenant}}

	before := acl.Resolve(person(), "seg-0", 1, perms).Key.String()

	p := person()
	p.Groups.Version = 8
	after := acl.Resolve(p, "seg-0", 1, perms).Key.String()

	if before == after {
		t.Fatal("the cache key survived a group membership change, so a stale bitmap would be reused")
	}
}

func TestFilterRemovesInvisibleCandidates(t *testing.T) {
	perms := []acl.Permissions{
		0: {Mode: acl.ModePublicToTenant},
		1: {Mode: acl.ModeACL, AllowGroups: []acl.Ref{{Source: "slack", Value: "S-legal"}}},
		2: {Mode: acl.ModePublicToTenant},
	}
	v := acl.Resolve(person(), "seg-0", 1, perms)

	candidates := acl.NewBitmap(3)
	candidates.Add(0)
	candidates.Add(1)
	candidates.Add(2)

	v.Filter(candidates)

	if candidates.Contains(1) {
		t.Fatal("a document the asker cannot read survived the filter")
	}
	if !candidates.Contains(0) || !candidates.Contains(2) {
		t.Fatal("the filter dropped documents the asker can read")
	}
}
