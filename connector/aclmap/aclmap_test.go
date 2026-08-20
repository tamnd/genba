package aclmap_test

import (
	"errors"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector/aclmap"
)

// person builds a principal known to one identity source, in the groups named.
func person(identity, id string, groups ...string) *acl.Principal {
	members := make([]string, 0, len(groups))
	for _, g := range groups {
		members = append(members, identity+":"+g)
	}
	return &acl.Principal{
		Tenant:     "acme",
		Subject:    "u-" + id,
		Kind:       acl.KindUser,
		Identities: []acl.Identity{{Source: identity, Value: id}},
		Groups:     acl.GroupSet{Version: 1, Members: members},
	}
}

func mustNew(t *testing.T, r aclmap.Rules) *aclmap.Normalizer {
	t.Helper()
	n, err := aclmap.New(r)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return n
}

func TestNewRefusesRulesItCannotUse(t *testing.T) {
	if _, err := aclmap.New(aclmap.Rules{Identity: "google"}); err == nil {
		t.Error("a mapping with no source name was accepted")
	}
	// Without an identity source every user reference is compared as a bare
	// value, so alice at one company matches alice at another.
	if _, err := aclmap.New(aclmap.Rules{Source: "drive"}); err == nil {
		t.Error("a mapping with no identity source was accepted")
	}
}

// TestADenyBeatsAnAllow is the rule the whole package is arranged around, and
// it is checked through Allows rather than by reading the lists, because the
// lists are only worth anything if the evaluation order is right.
func TestADenyBeatsAnAllow(t *testing.T) {
	n := mustNew(t, aclmap.Drive("drive", "google", "acme.com"))

	tests := []struct {
		name   string
		grants []aclmap.Grant
		who    *acl.Principal
		want   bool
	}{
		{
			name: "a person named in both lists is refused",
			grants: []aclmap.Grant{
				{Subject: aclmap.User, ID: "alice@acme.com", Role: "reader"},
				{Subject: aclmap.User, ID: "alice@acme.com", Role: "reader", Effect: aclmap.Deny},
			},
			who:  person("google", "alice@acme.com"),
			want: false,
		},
		{
			name: "the order the statements arrived in does not matter",
			grants: []aclmap.Grant{
				{Subject: aclmap.User, ID: "alice@acme.com", Role: "reader", Effect: aclmap.Deny},
				{Subject: aclmap.User, ID: "alice@acme.com", Role: "reader"},
			},
			who:  person("google", "alice@acme.com"),
			want: false,
		},
		{
			name: "a group deny beats a group allow",
			grants: []aclmap.Grant{
				{Subject: aclmap.Group, ID: "eng@acme.com", Role: "writer"},
				{Subject: aclmap.Group, ID: "contractors@acme.com", Role: "reader", Effect: aclmap.Deny},
			},
			who:  person("google", "bob@acme.com", "eng@acme.com", "contractors@acme.com"),
			want: false,
		},
		{
			name: "a deny on one person does not refuse another",
			grants: []aclmap.Grant{
				{Subject: aclmap.Group, ID: "eng@acme.com", Role: "writer"},
				{Subject: aclmap.User, ID: "alice@acme.com", Role: "reader", Effect: aclmap.Deny},
			},
			who:  person("google", "bob@acme.com", "eng@acme.com"),
			want: true,
		},
		{
			name: "a deny beats the whole domain",
			grants: []aclmap.Grant{
				{Subject: aclmap.Domain, ID: "acme.com", Role: "reader"},
				{Subject: aclmap.User, ID: "alice@acme.com", Role: "reader", Effect: aclmap.Deny},
			},
			who:  person("google", "alice@acme.com"),
			want: false,
		},
		{
			name: "a deny beats being the owner",
			grants: []aclmap.Grant{
				{Subject: aclmap.User, ID: "alice@acme.com", Role: "owner", Owner: true},
				{Subject: aclmap.User, ID: "alice@acme.com", Role: "reader", Effect: aclmap.Deny},
			},
			who:  person("google", "alice@acme.com"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perm, err := n.Normalize(tt.grants)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if got := perm.Allows(tt.who); got != tt.want {
				t.Errorf("allows is %v, want %v, from %+v", got, tt.want, perm)
			}
		})
	}
}

// TestWhatCannotBeMappedIsQuarantinedAndCounted is the other half of the same
// rule. Approximating a statement is how a document reaches somebody it was
// taken away from, so nothing is approximated.
func TestWhatCannotBeMappedIsQuarantinedAndCounted(t *testing.T) {
	tests := []struct {
		name   string
		grants []aclmap.Grant
		reason aclmap.Reason
		count  func(aclmap.Counts) int64
	}{
		{
			name:   "a grant to somebody else's domain",
			grants: []aclmap.Grant{{Subject: aclmap.Domain, ID: "partner.example", Role: "reader"}},
			reason: aclmap.ReasonForeignDomain,
			count:  func(c aclmap.Counts) int64 { return c.ForeignDomain },
		},
		{
			name:   "a refusal aimed at a domain",
			grants: []aclmap.Grant{{Subject: aclmap.Domain, ID: "acme.com", Role: "reader", Effect: aclmap.Deny}},
			reason: aclmap.ReasonUnmappableDeny,
			count:  func(c aclmap.Counts) int64 { return c.UnmappableDeny },
		},
		{
			name:   "a refusal aimed at everybody",
			grants: []aclmap.Grant{{Subject: aclmap.Anyone, Role: "reader", Effect: aclmap.Deny}},
			reason: aclmap.ReasonUnmappableDeny,
			count:  func(c aclmap.Counts) int64 { return c.UnmappableDeny },
		},
		{
			name:   "a grant naming nobody",
			grants: []aclmap.Grant{{Subject: aclmap.User, Role: "reader"}},
			reason: aclmap.ReasonMalformed,
			count:  func(c aclmap.Counts) int64 { return c.Malformed },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := mustNew(t, aclmap.Drive("drive", "google", "acme.com"))
			perm, err := n.Normalize(tt.grants)

			var mapErr *aclmap.Error
			if !errors.As(err, &mapErr) {
				t.Fatalf("normalize returned %v, want an aclmap.Error", err)
			}
			if mapErr.Reason != tt.reason {
				t.Errorf("reason is %v, want %v", mapErr.Reason, tt.reason)
			}
			if mapErr.Detail == "" {
				t.Error("the error names nothing, so there is nothing to go and look at")
			}

			// The descriptor is the thing that matters. A connector that logs
			// the error and carries on has to end up with a document nobody can
			// read, not one nobody restricted.
			if perm.Mode != acl.ModeUnknown {
				t.Errorf("mode is %v, want unknown", perm.Mode)
			}
			if perm.Allows(person("google", "alice@acme.com")) {
				t.Error("a quarantined document is readable")
			}

			counts := n.Counts()
			if tt.count(counts) != 1 {
				t.Errorf("the reason was not counted: %+v", counts)
			}
			if counts.Quarantined() != 1 || counts.Mapped != 0 {
				t.Errorf("counts are %+v, want one quarantined and nothing mapped", counts)
			}
		})
	}
}

// TestLinkSharingIsRecordedRatherThanInferred is the case where the absence of
// a restriction is not the same as a decision. A link shared document has been
// shared, and an index that stores only its allow list cannot say so.
func TestLinkSharingIsRecordedRatherThanInferred(t *testing.T) {
	grants := []aclmap.Grant{
		{Subject: aclmap.User, ID: "alice@acme.com", Role: "owner", Owner: true},
		{Subject: aclmap.Anyone, Role: "reader", Link: true},
	}
	stranger := person("google", "mallory@acme.com")

	t.Run("by default a link is not a search grant", func(t *testing.T) {
		n := mustNew(t, aclmap.Drive("drive", "google", "acme.com"))
		perm, err := n.Normalize(grants)
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if perm.Sharing != acl.SharedByLink {
			t.Errorf("sharing is %v, want link", perm.Sharing)
		}
		// Somebody who has the link was given it. Somebody searching was not,
		// and the difference is the whole reason this is a setting.
		if perm.Allows(stranger) {
			t.Error("a link share was turned into a search result for somebody who never had the link")
		}
		if !perm.Allows(person("google", "alice@acme.com")) {
			t.Error("the owner lost access")
		}
	})

	t.Run("a deployment can say a link means the company", func(t *testing.T) {
		rules := aclmap.Drive("drive", "google", "acme.com")
		rules.Link = aclmap.LinkGrantsTenant
		n := mustNew(t, rules)

		perm, err := n.Normalize(grants)
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if perm.Mode != acl.ModePublicToTenant {
			t.Errorf("mode is %v, want public to tenant", perm.Mode)
		}
		// Still recorded, because how it became readable is a separate fact
		// from whether it is.
		if perm.Sharing != acl.SharedByLink {
			t.Errorf("sharing is %v, want link", perm.Sharing)
		}
		if !perm.Allows(stranger) {
			t.Error("the setting was asked for and did nothing")
		}
	})
}

func TestAPublicDocumentSaysSo(t *testing.T) {
	n := mustNew(t, aclmap.Drive("drive", "google", "acme.com"))

	perm, err := n.Normalize([]aclmap.Grant{
		{Subject: aclmap.User, ID: "alice@acme.com", Role: "owner", Owner: true},
		{Subject: aclmap.Anyone, Role: "reader"},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if perm.Sharing != acl.SharedPublic {
		t.Errorf("sharing is %v, want public", perm.Sharing)
	}
	if perm.Mode != acl.ModePublicToTenant {
		t.Errorf("mode is %v, want public to tenant", perm.Mode)
	}
	if !perm.Allows(person("google", "anybody@acme.com")) {
		t.Error("a document its source says is on the internet is not readable inside the company")
	}
}

// A widening statement cannot be undone by one that arrived after it, because
// the order an API returns permissions in is not a statement about anything.
func TestTheOrderOfStatementsDoesNotChangeTheAnswer(t *testing.T) {
	n := mustNew(t, aclmap.Drive("drive", "google", "acme.com"))

	forward, err := n.Normalize([]aclmap.Grant{
		{Subject: aclmap.Domain, ID: "acme.com", Role: "reader"},
		{Subject: aclmap.User, ID: "alice@acme.com", Role: "owner", Owner: true},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	backward, err := n.Normalize([]aclmap.Grant{
		{Subject: aclmap.User, ID: "alice@acme.com", Role: "owner", Owner: true},
		{Subject: aclmap.Domain, ID: "acme.com", Role: "reader"},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if forward.Mode != acl.ModePublicToTenant || backward.Mode != acl.ModePublicToTenant {
		t.Errorf("modes are %v and %v, want both public to tenant", forward.Mode, backward.Mode)
	}
}

func TestARoleThatDoesNotConferReadIsNotAGrant(t *testing.T) {
	// Object storage is where this bites hardest. WRITE on a bucket is
	// permission to put objects into it and says nothing about reading them.
	n := mustNew(t, aclmap.ObjectStore("s3", "aws", "acme.com"))

	perm, err := n.Normalize([]aclmap.Grant{
		{Subject: aclmap.User, ID: "uploader", Role: "WRITE"},
		{Subject: aclmap.User, ID: "reader", Role: "READ"},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if perm.Allows(person("aws", "uploader")) {
		t.Error("somebody who may upload to a bucket can read what is in it")
	}
	if !perm.Allows(person("aws", "reader")) {
		t.Error("somebody granted READ cannot read")
	}
	// Counted, because a climb in this number usually means a source renamed a
	// role and the mapping has not caught up.
	if c := n.Counts(); c.Ignored != 1 {
		t.Errorf("ignored %d statements, want 1: %+v", c.Ignored, c)
	}
}

func TestADocumentWithOnlyAnOwnerIsOwnerOnly(t *testing.T) {
	n := mustNew(t, aclmap.Drive("drive", "google", "acme.com"))

	perm, err := n.Normalize([]aclmap.Grant{
		{Subject: aclmap.User, ID: "alice@acme.com", Role: "owner", Owner: true},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if perm.Mode != acl.ModeOwnerOnly {
		t.Errorf("mode is %v, want owner only", perm.Mode)
	}
	if !perm.Allows(person("google", "alice@acme.com")) {
		t.Error("the owner cannot read their own document")
	}
	if perm.Allows(person("google", "bob@acme.com")) {
		t.Error("somebody else can read a private document")
	}
}

// A second owner keeps their access. One field cannot hold two of them, and
// dropping the rest would take away access somebody has.
func TestASecondOwnerStaysAReader(t *testing.T) {
	n := mustNew(t, aclmap.Drive("drive", "google", "acme.com"))

	perm, err := n.Normalize([]aclmap.Grant{
		{Subject: aclmap.User, ID: "alice@acme.com", Role: "owner", Owner: true},
		{Subject: aclmap.User, ID: "bob@acme.com", Role: "owner", Owner: true},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if perm.Owner.Value != "alice@acme.com" {
		t.Errorf("owner is %q, want the first one reported", perm.Owner.Value)
	}
	for _, who := range []string{"alice@acme.com", "bob@acme.com"} {
		if !perm.Allows(person("google", who)) {
			t.Errorf("%s cannot read a document they own", who)
		}
	}
}

// A document nobody has been given is readable by nobody, and that is a mapped
// document rather than a failure. There is nothing wrong with it and nothing to
// go and fix.
func TestNoStatementsIsAnEmptyListRatherThanAFailure(t *testing.T) {
	n := mustNew(t, aclmap.Drive("drive", "google", "acme.com"))

	perm, err := n.Normalize(nil)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if perm.Mode != acl.ModeACL {
		t.Errorf("mode is %v, want an access control list", perm.Mode)
	}
	if perm.Allows(person("google", "alice@acme.com")) {
		t.Error("a document nobody was given is readable")
	}
	if c := n.Counts(); c.Mapped != 1 || c.Quarantined() != 0 {
		t.Errorf("counts are %+v, want it counted as mapped", c)
	}
}

// A source reporting the same person twice under two roles is reporting the
// truth. The lists end up in a query, and a name in one four times is four
// terms in that query.
func TestRepeatedStatementsCollapse(t *testing.T) {
	n := mustNew(t, aclmap.Drive("drive", "google", "acme.com"))

	perm, err := n.Normalize([]aclmap.Grant{
		{Subject: aclmap.User, ID: "alice@acme.com", Role: "writer"},
		{Subject: aclmap.User, ID: "alice@acme.com", Role: "commenter"},
		{Subject: aclmap.Group, ID: "eng@acme.com", Role: "reader"},
		{Subject: aclmap.Group, ID: "eng@acme.com", Role: "writer"},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(perm.AllowUsers) != 1 || len(perm.AllowGroups) != 1 {
		t.Errorf("lists are %v and %v, want one entry each", perm.AllowUsers, perm.AllowGroups)
	}
}

func TestTheDescriptorNamesItsSourceAndIdentity(t *testing.T) {
	n := mustNew(t, aclmap.Drive("drive", "google", "acme.com"))

	perm, err := n.Normalize([]aclmap.Grant{{Subject: aclmap.Group, ID: "eng@acme.com", Role: "reader"}})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if perm.Source != "drive" {
		t.Errorf("source is %q, want the connector name", perm.Source)
	}
	// The identity source is what lets somebody who authenticated through one
	// system match a list written in terms of another.
	if perm.AllowGroups[0].Source != "google" {
		t.Errorf("the group reference belongs to %q, want google", perm.AllowGroups[0].Source)
	}
}
