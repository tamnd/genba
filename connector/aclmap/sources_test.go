package aclmap_test

import (
	"errors"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector/aclmap"
)

// mapCase is one statement of what a source said and what the index should make
// of it.
//
// The tables below are the documented mapping for each source, in the form that
// fails the build when somebody changes it. docs/permissions.md is the same
// tables in prose, and the two are meant to be read together.
type mapCase struct {
	name   string
	grants []aclmap.Grant

	// mode and sharing are what the descriptor should say. They are only
	// checked when reason is ReasonNone.
	mode    acl.Mode
	sharing acl.Sharing

	// reason is the failure expected, or ReasonNone for a document that maps.
	reason aclmap.Reason

	// can and cannot are the people the answer is really about.
	can    []*acl.Principal
	cannot []*acl.Principal
}

func runCases(t *testing.T, rules aclmap.Rules, cases []mapCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := mustNew(t, rules)
			perm, err := n.Normalize(tc.grants)

			if tc.reason != aclmap.ReasonNone {
				var mapErr *aclmap.Error
				if !errors.As(err, &mapErr) {
					t.Fatalf("normalize returned %v, want an aclmap.Error", err)
				}
				if mapErr.Reason != tc.reason {
					t.Fatalf("reason is %v, want %v", mapErr.Reason, tc.reason)
				}
				if perm.Mode != acl.ModeUnknown {
					t.Fatalf("mode is %v, want unknown", perm.Mode)
				}
			} else {
				if err != nil {
					t.Fatalf("normalize: %v", err)
				}
				if perm.Mode != tc.mode {
					t.Errorf("mode is %v, want %v", perm.Mode, tc.mode)
				}
				if perm.Sharing != tc.sharing {
					t.Errorf("sharing is %v, want %v", perm.Sharing, tc.sharing)
				}
			}

			for _, p := range tc.can {
				if !perm.Allows(p) {
					t.Errorf("%s cannot read it", p.Subject)
				}
			}
			for _, p := range tc.cannot {
				if perm.Allows(p) {
					t.Errorf("%s can read it", p.Subject)
				}
			}
		})
	}
}

// A file system has no permission model of its own worth reading, so the roles
// are the ones the OWNERS convention invents. There is no deny in an OWNERS
// file: a subtree narrows what its parent allowed by replacing the list rather
// than by refusing anybody.
func TestFilesMapping(t *testing.T) {
	rules := aclmap.Files("handbook", "github")
	alice := person("github", "alice")
	bob := person("github", "bob")
	mallory := person("github", "mallory")

	runCases(t, rules, []mapCase{
		{
			name: "approvers and reviewers both read",
			grants: []aclmap.Grant{
				{Subject: aclmap.User, ID: "alice", Role: "approver"},
				{Subject: aclmap.User, ID: "bob", Role: "reviewer"},
			},
			mode:   acl.ModeACL,
			can:    []*acl.Principal{alice, bob},
			cannot: []*acl.Principal{mallory},
		},
		{
			name:   "a subtree with no OWNERS file above it grants nobody",
			grants: nil,
			mode:   acl.ModeACL,
			cannot: []*acl.Principal{alice, bob, mallory},
		},
		{
			name: "a file with one owner and nobody else is owner only",
			grants: []aclmap.Grant{
				{Subject: aclmap.User, ID: "alice", Role: "owner", Owner: true},
			},
			mode:   acl.ModeOwnerOnly,
			can:    []*acl.Principal{alice},
			cannot: []*acl.Principal{bob},
		},
	})
}

// S3 and the object stores that copy its access control lists. The permissions
// are S3's five and only two of them let anybody read the object.
func TestObjectStoreMapping(t *testing.T) {
	rules := aclmap.ObjectStore("archive", "aws", "acme.com")
	reader := person("aws", "canonical-1")
	uploader := person("aws", "canonical-2")

	runCases(t, rules, []mapCase{
		{
			name: "READ and FULL_CONTROL read, the rest do not",
			grants: []aclmap.Grant{
				{Subject: aclmap.User, ID: "canonical-1", Role: "READ"},
				{Subject: aclmap.User, ID: "canonical-2", Role: "WRITE"},
			},
			mode:   acl.ModeACL,
			can:    []*acl.Principal{reader},
			cannot: []*acl.Principal{uploader},
		},
		{
			name: "the AllUsers group is the open internet",
			grants: []aclmap.Grant{
				{Subject: aclmap.Anyone, Role: "READ"},
			},
			mode:    acl.ModePublicToTenant,
			sharing: acl.SharedPublic,
			can:     []*acl.Principal{reader, uploader},
		},
		{
			// Every account holder at the provider is not this company and
			// cannot be enumerated, so there is nothing faithful to store.
			name: "the authenticated users group is not the tenant",
			grants: []aclmap.Grant{
				{Subject: aclmap.Domain, ID: "authenticated-users", Role: "READ"},
			},
			reason: aclmap.ReasonForeignDomain,
			cannot: []*acl.Principal{reader},
		},
		{
			name: "a bucket policy that denies one account",
			grants: []aclmap.Grant{
				{Subject: aclmap.Domain, ID: "acme.com", Role: "READ"},
				{Subject: aclmap.User, ID: "canonical-2", Role: "READ", Effect: aclmap.Deny},
			},
			mode:   acl.ModePublicToTenant,
			can:    []*acl.Principal{reader},
			cannot: []*acl.Principal{uploader},
		},
	})
}

// A document store of the Google Drive shape is the source with all four
// subjects, and the only one where a share can be qualified with a link.
func TestDriveMapping(t *testing.T) {
	rules := aclmap.Drive("drive", "google", "acme.com")
	alice := person("google", "alice@acme.com")
	eng := person("google", "bob@acme.com", "eng@acme.com")
	anyone := person("google", "mallory@acme.com")

	runCases(t, rules, []mapCase{
		{
			name: "every role from owner to commenter reads",
			grants: []aclmap.Grant{
				{Subject: aclmap.User, ID: "alice@acme.com", Role: "owner", Owner: true},
				{Subject: aclmap.Group, ID: "eng@acme.com", Role: "commenter"},
			},
			mode:   acl.ModeACL,
			can:    []*acl.Principal{alice, eng},
			cannot: []*acl.Principal{anyone},
		},
		{
			name: "a grant to the company domain is everybody in it",
			grants: []aclmap.Grant{
				{Subject: aclmap.Domain, ID: "acme.com", Role: "reader"},
			},
			mode: acl.ModePublicToTenant,
			can:  []*acl.Principal{alice, eng, anyone},
		},
		{
			name: "a grant to a partner's domain names people this index cannot",
			grants: []aclmap.Grant{
				{Subject: aclmap.Domain, ID: "partner.example", Role: "reader"},
			},
			reason: aclmap.ReasonForeignDomain,
			cannot: []*acl.Principal{alice, eng, anyone},
		},
		{
			name: "a link share is recorded and grants nothing by default",
			grants: []aclmap.Grant{
				{Subject: aclmap.User, ID: "alice@acme.com", Role: "owner", Owner: true},
				{Subject: aclmap.Anyone, Role: "reader", Link: true},
			},
			mode:    acl.ModeOwnerOnly,
			sharing: acl.SharedByLink,
			can:     []*acl.Principal{alice},
			cannot:  []*acl.Principal{eng, anyone},
		},
		{
			name: "published to the internet is public and says so",
			grants: []aclmap.Grant{
				{Subject: aclmap.Anyone, Role: "reader"},
			},
			mode:    acl.ModePublicToTenant,
			sharing: acl.SharedPublic,
			can:     []*acl.Principal{alice, anyone},
		},
	})
}

// Chat has almost no vocabulary, because a message's access is the channel's
// membership and nothing else.
func TestChatMapping(t *testing.T) {
	rules := aclmap.Chat("slack", "slack", "acme.com")
	member := person("slack", "U04AB", "C-private")
	other := person("slack", "U04CD")

	runCases(t, rules, []mapCase{
		{
			name: "a private channel is its membership",
			grants: []aclmap.Grant{
				{Subject: aclmap.Group, ID: "C-private"},
			},
			mode:   acl.ModeACL,
			can:    []*acl.Principal{member},
			cannot: []*acl.Principal{other},
		},
		{
			name: "a public channel is the workspace",
			grants: []aclmap.Grant{
				{Subject: aclmap.Domain, ID: "acme.com"},
			},
			mode: acl.ModePublicToTenant,
			can:  []*acl.Principal{member, other},
		},
		{
			name: "a direct message is its participants",
			grants: []aclmap.Grant{
				{Subject: aclmap.User, ID: "U04AB"},
				{Subject: aclmap.User, ID: "U04EF"},
			},
			mode:   acl.ModeACL,
			can:    []*acl.Principal{member},
			cannot: []*acl.Principal{other},
		},
	})
}

// A wiki of the Confluence shape has space permissions and then page
// restrictions that narrow them, and an exclusion is a real refusal.
func TestWikiMapping(t *testing.T) {
	rules := aclmap.Wiki("wiki", "atlassian", "acme.com")
	staff := person("atlassian", "alice", "staff")
	contractor := person("atlassian", "dave", "staff", "contractors")

	runCases(t, rules, []mapCase{
		{
			name: "a space grants VIEW to a group",
			grants: []aclmap.Grant{
				{Subject: aclmap.Group, ID: "staff", Role: "VIEW"},
			},
			mode: acl.ModeACL,
			can:  []*acl.Principal{staff, contractor},
		},
		{
			name: "a page restriction refuses inside a space that allows",
			grants: []aclmap.Grant{
				{Subject: aclmap.Group, ID: "staff", Role: "VIEW"},
				{Subject: aclmap.Group, ID: "contractors", Role: "VIEW", Effect: aclmap.Deny},
			},
			mode:   acl.ModeACL,
			can:    []*acl.Principal{staff},
			cannot: []*acl.Principal{contractor},
		},
	})
}

// An issue tracker of the Jira shape grants reading through BROWSE_PROJECTS,
// and issue security levels narrow individual issues below that.
func TestTicketsMapping(t *testing.T) {
	rules := aclmap.Tickets("jira", "atlassian", "acme.com")
	developer := person("atlassian", "alice", "developers")
	security := person("atlassian", "erin", "security-team")

	runCases(t, rules, []mapCase{
		{
			name: "a project role can browse the project",
			grants: []aclmap.Grant{
				{Subject: aclmap.Group, ID: "developers", Role: "BROWSE_PROJECTS"},
			},
			mode:   acl.ModeACL,
			can:    []*acl.Principal{developer},
			cannot: []*acl.Principal{security},
		},
		{
			// The connector reports the security level and nothing else for an
			// issue that has one. Reporting the project's list as well would
			// widen the issue straight back to the project, which is the whole
			// thing the level exists to stop.
			name: "an issue with a security level is only its level",
			grants: []aclmap.Grant{
				{Subject: aclmap.Group, ID: "security-team", Role: "BROWSE_PROJECTS"},
			},
			mode:   acl.ModeACL,
			can:    []*acl.Principal{security},
			cannot: []*acl.Principal{developer},
		},
		{
			name: "a permission to administer a project also reads it",
			grants: []aclmap.Grant{
				{Subject: aclmap.Group, ID: "developers", Role: "ADMINISTER_PROJECTS"},
			},
			mode: acl.ModeACL,
			can:  []*acl.Principal{developer},
		},
	})
}
