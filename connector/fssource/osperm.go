package fssource

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector/aclmap"
)

// OSPolicy derives permissions from what the operating system keeps on the
// files themselves: the owner, the group and the mode bits on Unix, and the
// discretionary access control list on Windows.
//
// It is the policy for a tree that is the file server, because there the
// operating system is the access control system and there is nothing better
// above it. It is the wrong policy for a copy of one. A tree that was rsynced
// to the crawler carries the permissions the copy has, which are the crawler's
// own, and indexing those would hand the lot to whoever the crawler runs as.
//
// # Unix
//
// The owner and the group become a user and a group reference under the
// identity source given to [NewOSPolicy], and each one is a grant only where
// its read bit is set. An owner who took away their own read bit could put it
// back, so there is an argument for granting to them regardless, and the
// literal reading is used instead: being wrong this way costs somebody a file
// of their own they cannot find, and being wrong the other way costs a file
// shown to somebody who was refused it.
//
// The world bit is the one that cannot be mapped without being told something
// first. It says every account on this host may read the file, and a host's
// accounts are not a tenant: on a laptop they are one person, on a login server
// they are the company, and on a machine with a guest account they are more
// than the company. So it grants nothing at all unless [NewOSPolicy] was given
// the domain those accounts belong to.
//
// Where a file carries a POSIX access control list, that list is read in place
// of the mode bits, because the two disagree in the direction that leaks. The
// group bits of such a file are the mask rather than the group's own
// permission, so a group the mask has taken read away from still reads as
// allowed, and a mapping built on the mode alone offers the file to people who
// cannot open it.
//
// The extended access control lists macOS and the BSDs keep are a different
// format in a different place, reachable only through the C library, and they
// are not read. A file carrying one gets the answer its mode bits give, which
// is narrower than the truth where the list grants and wider than it where the
// list refuses. That is the one gap in this policy that goes the wrong way, and
// it is stated here rather than left to be found: on those systems this is a
// good answer for an ordinary tree and not a safe one for a tree somebody has
// been managing with the access control list editor.
//
// # Windows
//
// The owner comes from the security descriptor and the grants come from the
// discretionary access control list, which is the one platform here with
// refusals in it. A refusal becomes a deny and beats every grant, which is what
// Windows does too.
//
// Entries that exist only to be inherited by files created later are skipped,
// because they say nothing about this file. Entries naming Everyone or
// Authenticated Users go through the same door as the Unix world bit and grant
// nothing until a domain names them.
//
// # Names
//
// Every identifier is resolved to an account name through the password and
// group databases, or through the account database on Windows, and a file whose
// owner does not resolve is quarantined rather than indexed under a number. A
// numeric user id is not an identity: it means one person on one host and
// somebody else on the next, so a grant written in terms of one would either
// match nobody or match the wrong person.
type OSPolicy struct {
	root     string
	source   string
	identity string
	domain   string

	acls *aclmap.Normalizer

	mu    sync.Mutex
	named map[string]string
}

// NewOSPolicy returns a policy reading the permissions the operating system
// keeps on the files under root.
//
// source names the connector. identity names the identity source the account
// names belong to, for example "unix" for a machine's own accounts or the name
// of the directory the host is joined to. Getting that one right is what lets
// somebody who authenticated through the company directory match a list that
// came out of a password file.
//
// worldDomain is optional and is the domain the accounts on this host belong
// to. Give it and a world readable file is readable by everybody in the tenant.
// Leave it out and the world bit grants nothing, which is the safe reading and
// the reason it is not the caller's job to remember to pass a flag.
func NewOSPolicy(root, source, identity string, worldDomain ...string) (*OSPolicy, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fssource: os policy: %w", err)
	}
	var domain string
	if len(worldDomain) > 0 {
		domain = worldDomain[0]
	}
	acls, err := aclmap.New(aclmap.Files(source, identity, worldDomain...))
	if err != nil {
		return nil, fmt.Errorf("fssource: os policy: %w", err)
	}
	return &OSPolicy{
		root:     abs,
		source:   source,
		identity: identity,
		domain:   domain,
		acls:     acls,
		named:    make(map[string]string),
	}, nil
}

var (
	_ Policy        = (*OSPolicy)(nil)
	_ Versioned     = (*OSPolicy)(nil)
	_ Reloader      = (*OSPolicy)(nil)
	_ statPolicy    = (*OSPolicy)(nil)
	_ statVersioned = (*OSPolicy)(nil)
)

// Counts returns what the mapping has seen, so that a deployment can watch the
// quarantine numbers rather than hear about them from somebody who cannot find
// a document.
func (p *OSPolicy) Counts() aclmap.Counts { return p.acls.Counts() }

// Reload drops the resolved account names.
//
// The names are the only thing cached here, because the permissions themselves
// are read from the file every time and there is nothing to go stale about
// them. A name can still go stale: an account renamed between two syncs would
// otherwise keep the name it had when the process started, and a grant to a
// name nobody has any more is a grant to nobody.
func (p *OSPolicy) Reload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.named)
}

// Permissions returns the access control list the operating system holds for a
// path.
func (p *OSPolicy) Permissions(ctx context.Context, relPath string) (acl.Permissions, error) {
	info, err := os.Stat(p.full(relPath))
	if err != nil {
		return acl.Permissions{}, err
	}
	return p.permissionsFor(ctx, relPath, info)
}

// permissionsFor is the same answer for a file the caller has already stated.
//
// A walk has the file information in its hand when it asks, and asking for it
// again would double the number of system calls a sync makes on a tree of a
// million files. The exported method is what everything else uses.
func (p *OSPolicy) permissionsFor(ctx context.Context, relPath string, info fs.FileInfo) (acl.Permissions, error) {
	if err := ctx.Err(); err != nil {
		return acl.Permissions{}, err
	}
	rules, err := accessRules(p.full(relPath), info)
	if err != nil {
		return acl.Permissions{}, err
	}

	grants := make([]aclmap.Grant, 0, len(rules))
	for _, r := range rules {
		g, ok, err := p.grant(r)
		if err != nil {
			return acl.Permissions{}, err
		}
		if ok {
			grants = append(grants, g)
		}
	}
	return p.acls.Normalize(grants)
}

// ChangedAt returns when the permissions on a path last changed.
func (p *OSPolicy) ChangedAt(ctx context.Context, relPath string) (time.Time, error) {
	info, err := os.Stat(p.full(relPath))
	if err != nil {
		return time.Time{}, err
	}
	return p.changedAtFor(ctx, relPath, info)
}

// changedAtFor is what makes a chmod take effect without a recrawl.
//
// The answer is the inode change time, which moves when the mode, the owner or
// the access control list is written and stays where it is when only the
// content is. That is exactly the event a sync comparing modification times
// cannot see, and the one that has to be seen: a revocation that takes effect
// the next time somebody happens to edit the file is not a revocation.
//
// Windows has no equivalent that can be read without opening every file, so it
// answers with the zero time and a permission change there waits for the file
// to be touched. That is stated rather than worked around, because the way to
// work around it is a stat that costs an open per file per sync.
func (p *OSPolicy) changedAtFor(ctx context.Context, _ string, info fs.FileInfo) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	return changeTime(info), nil
}

// full is the absolute path of a path relative to the root.
func (p *OSPolicy) full(relPath string) string {
	return filepath.Join(p.root, filepath.FromSlash(relPath))
}

// grant turns one operating system rule into a statement aclmap understands,
// and reports whether there is a statement to make at all.
func (p *OSPolicy) grant(r rule) (aclmap.Grant, bool, error) {
	g := aclmap.Grant{Subject: r.subject, Owner: r.owner}
	if r.deny {
		g.Effect = aclmap.Deny
	}

	switch r.subject {
	case aclmap.Domain:
		if p.domain == "" {
			if r.deny {
				// A refusal aimed at everybody cannot be dropped for want of a
				// domain to name them by. Reporting it as aimed at everybody is
				// what it is, and aclmap has no deny list for that, so the
				// document is quarantined instead of quietly widened.
				return aclmap.Grant{Subject: aclmap.Anyone, Effect: aclmap.Deny}, true, nil
			}
			return aclmap.Grant{}, false, nil
		}
		g.ID = p.domain
		return g, true, nil

	case aclmap.User, aclmap.Group:
		name, err := p.lookup(r)
		if err != nil {
			return aclmap.Grant{}, false, err
		}
		g.ID = name
		return g, true, nil

	case aclmap.Anyone:
		return g, true, nil

	default:
		return aclmap.Grant{}, false, fmt.Errorf("fssource: unknown subject %v", r.subject)
	}
}

// lookup resolves one identifier to an account name, caching the answer.
//
// A large tree asks about the same handful of owners once per file, and the
// lookup goes to a password file or, on a joined host, to a directory server
// over the network. Caching it is the difference between a sync that reads the
// tree and a sync that interrogates the directory a million times.
func (p *OSPolicy) lookup(r rule) (string, error) {
	if r.name != "" {
		// The platform resolved it on the way past, which is what Windows does:
		// working out whether a security identifier names a person or a group is
		// the same call that returns the name, so throwing the name away here
		// would mean asking twice.
		return r.name, nil
	}

	key := r.subject.String() + ":" + r.id
	p.mu.Lock()
	name, ok := p.named[key]
	p.mu.Unlock()
	if ok {
		return name, nil
	}

	name, err := resolve(r)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.named[key] = name
	p.mu.Unlock()
	return name, nil
}

// resolve asks the operating system what an identifier is called.
func resolve(r rule) (string, error) {
	switch r.subject {
	case aclmap.Group:
		g, err := user.LookupGroupId(r.id)
		if err != nil {
			return "", fmt.Errorf("fssource: group %s: %w", r.id, err)
		}
		return g.Name, nil
	case aclmap.User, aclmap.Domain, aclmap.Anyone:
		u, err := user.LookupId(r.id)
		if err != nil {
			return "", fmt.Errorf("fssource: user %s: %w", r.id, err)
		}
		return u.Username, nil
	default:
		return "", fmt.Errorf("fssource: unknown subject %v", r.subject)
	}
}

// rule is one statement the operating system makes about one file, in the
// vocabulary the platforms have in common.
//
// Producing these is the whole of what the platform specific code does, which
// is what keeps the mode bits, the access control list entries and the Windows
// access masks each inside the one file that knows what they mean. Everything
// above works in users, groups and everybody.
type rule struct {
	// subject is who the statement is about. Domain means everybody with an
	// account on this host, which is the Unix world bit and the Windows Everyone
	// entry, and which grants nothing until a domain names them.
	subject aclmap.Subject

	// id is the operating system's own identifier: a numeric user or group id on
	// Unix, a security identifier on Windows.
	id string

	// name is the account name where the platform already knows it, so that a
	// second lookup is not needed to find out.
	name string

	// deny says the statement refuses rather than grants. Only Windows produces
	// one.
	deny bool

	// owner says the subject owns the file.
	owner bool
}
