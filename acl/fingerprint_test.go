package acl_test

import (
	"slices"
	"testing"

	"github.com/tamnd/genba/acl"
)

// The fingerprint is the half of a cache key that says who is asking. Every
// test here is really the same question asked from two sides: two principals
// that see the same documents must share it, and two that do not must not.

func TestFingerprintIsStable(t *testing.T) {
	p := &acl.Principal{
		Tenant:  "acme",
		Subject: "u_mei",
		Groups:  acl.GroupSet{Version: 3, Members: []string{"gdrive:eng@acme.com"}},
	}
	first := acl.Fingerprint(p)
	if first == "" {
		t.Fatal("a principal fingerprinted to nothing")
	}
	if got := acl.Fingerprint(p); got != first {
		t.Fatalf("the same principal fingerprinted twice gave %q then %q", first, got)
	}
	if len(first) != 2*acl.FingerprintBytes {
		t.Errorf("a fingerprint is %d characters, want %d", len(first), 2*acl.FingerprintBytes)
	}
}

func TestFingerprintIgnoresGroupOrder(t *testing.T) {
	one := &acl.Principal{
		Tenant:  "acme",
		Subject: "u_mei",
		Groups:  acl.GroupSet{Version: 1, Members: []string{"gdrive:eng@acme.com", "gdrive:oncall@acme.com"}},
	}
	other := &acl.Principal{
		Tenant:  "acme",
		Subject: "u_mei",
		Groups:  acl.GroupSet{Version: 1, Members: []string{"gdrive:oncall@acme.com", "gdrive:eng@acme.com"}},
	}
	if acl.Fingerprint(one) != acl.Fingerprint(other) {
		t.Error("the same groups in a different order produced two fingerprints, so the cache would never hit")
	}

	// And the sort is on a copy, because the accessor hands back the principal's
	// own slice and reordering it under the caller would be a rude surprise.
	before := slices.Clone(other.Groups.Members)
	acl.Fingerprint(other)
	if !slices.Equal(before, other.Groups.Members) {
		t.Errorf("fingerprinting reordered the principal's groups: %v became %v", before, other.Groups.Members)
	}
}

func TestFingerprintSeparatesDifferentViews(t *testing.T) {
	base := func() *acl.Principal {
		return &acl.Principal{
			Tenant:  "acme",
			Subject: "u_mei",
			Groups:  acl.GroupSet{Version: 1, Members: []string{"gdrive:eng@acme.com"}},
		}
	}
	cases := []struct {
		name   string
		change func(*acl.Principal)
	}{
		{"another tenant", func(p *acl.Principal) { p.Tenant = "other" }},
		{"another person", func(p *acl.Principal) { p.Subject = "u_sam" }},
		{"another group", func(p *acl.Principal) { p.Groups.Members = []string{"gdrive:sales@acme.com"} }},
		{"one group more", func(p *acl.Principal) {
			p.Groups.Members = append(p.Groups.Members, "gdrive:sales@acme.com")
		}},
		{"no groups at all", func(p *acl.Principal) { p.Groups.Members = nil }},
		{"a newer expansion", func(p *acl.Principal) { p.Groups.Version = 2 }},
	}
	original := acl.Fingerprint(base())
	seen := map[string]string{original: "unchanged"}
	for _, tc := range cases {
		p := base()
		tc.change(p)
		got := acl.Fingerprint(p)
		if was, ok := seen[got]; ok {
			t.Errorf("%s fingerprints the same as %s, so their cache entries would be shared", tc.name, was)
		}
		seen[got] = tc.name
	}
}

// TestFingerprintCannotBeConfusedByRearranging is why every component is
// written with its length in front of it. Without that, a tenant of "ac" with a
// group "me:x" hashes the same as a tenant of "acme" with a group ":x", and two
// people who share nothing share a cache entry.
func TestFingerprintCannotBeConfusedByRearranging(t *testing.T) {
	one := &acl.Principal{
		Tenant:  "ac",
		Subject: "u",
		Groups:  acl.GroupSet{Version: 1, Members: []string{"me:x"}},
	}
	other := &acl.Principal{
		Tenant:  "acme",
		Subject: "u",
		Groups:  acl.GroupSet{Version: 1, Members: []string{":x"}},
	}
	if acl.Fingerprint(one) == acl.Fingerprint(other) {
		t.Fatal("two principals of different tenants share a fingerprint")
	}
}

func TestFingerprintOfNothingIsNothing(t *testing.T) {
	if got := acl.Fingerprint(nil); got != "" {
		t.Errorf("a nil principal fingerprinted to %q, want the empty string so that nothing is cached under it", got)
	}
}
