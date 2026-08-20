package aclmap

// The vocabularies of the systems this project indexes.
//
// Every list below is the source system's own set of names, taken from its
// documentation rather than invented here, and that is the point of keeping
// them in one file. A role name is a fact about somebody else's product. It
// changes when they change it, it is the thing that goes stale, and when it
// does the symptom is documents quietly disappearing from search for the people
// who could read them yesterday. Having all of them in one place means the
// staleness is in one place, next to the tests that pin it.
//
// Each preset takes the connector's name, the identity source its ids belong
// to, and the email domains that are this tenant. What it does not take is a
// link policy, because that is a decision about a company rather than about a
// product, and the default is the safe one.

// Files is the mapping for a file system connector.
//
// A tree has no permission model of its own worth reading, so the roles here
// are the ones a policy over the tree invents. The OWNERS convention that
// Kubernetes and a number of other large repositories keep names approvers and
// reviewers, and both are people who can read the subtree.
//
// There is no deny in an OWNERS file, and no link sharing either. A subtree
// narrows what its parent allowed by replacing the list rather than by refusing
// anybody, which is why the file system connector never produces a deny and
// nothing here has to map one.
func Files(source, identity string, domains ...string) Rules {
	return Rules{
		Source:    source,
		Identity:  identity,
		ReadRoles: []string{"approver", "reviewer", "owner"},
		Domains:   domains,
	}
}

// ObjectStore is the mapping for S3 and the object stores that copy its access
// control lists.
//
// The permissions are S3's five: READ, WRITE, READ_ACP, WRITE_ACP and
// FULL_CONTROL. Only two of them let somebody read the object, and the
// distinction matters more here than it looks: WRITE on a bucket is permission
// to put objects into it, not to read them, and a mapping that treated every
// permission as read would hand a company's private buckets to whoever can
// upload to them.
//
// The two group grantees are the ones that reach outside a company.
// AllUsers means the open internet and maps to a public document. Anybody
// authenticated is every account holder at the provider, which is not this
// tenant and is not enumerable, so it quarantines like any other foreign
// domain. A connector reports that one as a [Domain] grant to
// "authenticated-users".
func ObjectStore(source, identity string, domains ...string) Rules {
	return Rules{
		Source:    source,
		Identity:  identity,
		ReadRoles: []string{"read", "full_control"},
		Domains:   domains,
	}
}

// Drive is the mapping for a document store of the Google Drive shape.
//
// The roles are Drive's own: owner, organizer, fileOrganizer, writer,
// commenter and reader. All six can read the file, which is worth stating
// because it is the case where the obvious mapping is right and somebody will
// otherwise wonder whether commenter was left out by mistake.
//
// This is the source that has all four subjects. A permission can name a user,
// a group, a domain or anyone, and anyone can be qualified with a link, which
// is why [LinkPolicy] exists at all.
func Drive(source, identity string, domains ...string) Rules {
	return Rules{
		Source:    source,
		Identity:  identity,
		ReadRoles: []string{"owner", "organizer", "fileorganizer", "writer", "commenter", "reader"},
		Domains:   domains,
	}
}

// Chat is the mapping for a chat system of the Slack shape.
//
// Chat has almost no vocabulary, because a message's access is the channel's
// membership and nothing else. A connector reports a private channel as a group
// grant naming the channel, a public one as a domain grant to the workspace's
// domain, and a direct message as a user grant per participant.
//
// The role is empty on all of them, which is what an empty [Rules.ReadRoles]
// list is for: a member of a channel reads the channel, and there is nothing
// finer to say.
func Chat(source, identity string, domains ...string) Rules {
	return Rules{
		Source:   source,
		Identity: identity,
		Domains:  domains,
	}
}

// Wiki is the mapping for a wiki of the Confluence shape.
//
// Space permissions carry names such as VIEW and EDIT, and a page can then
// carry restrictions that narrow the space. Confluence has two kinds of
// restriction and only one of them is a refusal in the sense used here: a view
// restriction listing people replaces the space's answer for that page, which a
// connector reports as an allow list, while an explicit exclusion is a deny.
//
// A connector that reports the space permissions and then the page restrictions
// gets the right answer from this package for free, because a deny is applied
// before any allow whatever order the statements arrived in.
func Wiki(source, identity string, domains ...string) Rules {
	return Rules{
		Source:    source,
		Identity:  identity,
		ReadRoles: []string{"view", "read", "edit", "admin"},
		Domains:   domains,
	}
}

// Tickets is the mapping for an issue tracker of the Jira shape.
//
// Reading an issue takes the BROWSE_PROJECTS permission on its project, which
// a permission scheme grants to users, groups and project roles. A connector
// reports a project role as a group, because that is what it is: a named set of
// people maintained by the source.
//
// The part that catches people out is issue security. A project can carry
// security levels that narrow an individual issue to a subset of the people who
// can browse the project, and an issue with a security level set is not
// readable by the project's list at all. A connector must report the security
// level's members and nothing else for such an issue, because reporting both
// would widen it back to the project.
func Tickets(source, identity string, domains ...string) Rules {
	return Rules{
		Source:    source,
		Identity:  identity,
		ReadRoles: []string{"browse_projects", "administer_projects"},
		Domains:   domains,
	}
}
