package fssource

import (
	"fmt"
	"io/fs"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/tamnd/genba/connector/aclmap"
)

// The access rights that let somebody read a file.
//
// They are written out here rather than taken from a constant somewhere else
// because which of them counts is the decision this file exists to make. Write
// access is not read access on Windows any more than it is anywhere else, and a
// mapping that treated the whole mask as one thing would hand a share to
// everybody who can drop a file into it.
const (
	fileReadData = 0x00000001 // FILE_READ_DATA
	genericRead  = 0x80000000 // GENERIC_READ, as it appears before it is mapped
	genericAll   = 0x10000000 // GENERIC_ALL
	readAccess   = fileReadData | genericRead | genericAll
)

// inheritOnly marks an entry that governs the files created inside a directory
// rather than the object it is attached to.
const inheritOnly = 0x08 // INHERIT_ONLY_ACE

// The security identifiers that mean everybody.
//
// Everyone is exactly that, including accounts nobody in the company holds.
// Authenticated Users is every account the machine's domain will authenticate,
// which on a joined host is the company and on a standalone one is whoever has
// a local account. Neither is a tenant, so both go through the same door as the
// Unix world bit.
const (
	sidEveryone           = "S-1-1-0"
	sidAuthenticatedUsers = "S-1-5-11"
)

// accessRules reads the security descriptor of one file.
//
// The file information the walk already has is no help here. Windows keeps the
// permissions in a security descriptor rather than in the directory entry, so
// there is a call per file whatever the caller is holding.
func accessRules(full string, _ fs.FileInfo) ([]rule, error) {
	sd, err := windows.GetNamedSecurityInfo(full, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("fssource: %s: %w", full, err)
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, fmt.Errorf("fssource: %s: %w", full, err)
	}
	if dacl == nil {
		// A descriptor with no list at all grants everybody everything, which is
		// what Windows does with it and is not what an absent list looks like.
		return []rule{{subject: aclmap.Domain}}, nil
	}

	out := make([]rule, 0, dacl.AceCount)
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			// An entry that cannot be read is an entry that might be a refusal,
			// and carrying on without it would apply the rest of the list as
			// though this one had said nothing.
			return nil, fmt.Errorf("fssource: %s: entry %d: %w", full, i, err)
		}
		r, ok, err := aceRule(ace)
		if err != nil {
			return nil, fmt.Errorf("fssource: %s: %w", full, err)
		}
		if ok {
			out = append(out, r)
		}
	}
	return markOwner(out, sd), nil
}

// aceRule turns one entry into a statement, and reports whether the entry has
// anything to say about reading.
func aceRule(ace *windows.ACCESS_ALLOWED_ACE) (rule, bool, error) {
	deny := false
	switch ace.Header.AceType {
	case windows.ACCESS_ALLOWED_ACE_TYPE:
	case windows.ACCESS_DENIED_ACE_TYPE:
		deny = true
	default:
		// Auditing entries and the object entries a directory service uses say
		// nothing about who may read a file.
		return rule{}, false, nil
	}
	if ace.Header.AceFlags&inheritOnly != 0 {
		// The entry is a template for the files made inside this directory and
		// is not in force on the directory itself.
		return rule{}, false, nil
	}
	if ace.Mask&readAccess == 0 {
		return rule{}, false, nil
	}

	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	text := sid.String()
	if text == sidEveryone || text == sidAuthenticatedUsers {
		return rule{subject: aclmap.Domain, deny: deny}, true, nil
	}

	account, domain, kind, err := sid.LookupAccount("")
	if err != nil {
		// An entry left behind by a deleted account, which every file server of
		// any age is full of. The identifier still names exactly one account and
		// will never name another, so a grant can be carried under it and simply
		// matches nobody.
		//
		// A refusal cannot. Whether it belongs in the user list or the group
		// list decides whether it applies at all, and a refusal that does not
		// apply is the one failure this package will not have.
		if deny {
			return rule{}, false, fmt.Errorf("a refusal by %s, whose account does not resolve", text)
		}
		return rule{subject: aclmap.User, id: text, name: text}, true, nil
	}

	name := account
	if domain != "" {
		name = domain + `\` + account
	}
	switch kind {
	case windows.SidTypeUser, windows.SidTypeComputer:
		return rule{subject: aclmap.User, id: text, name: name, deny: deny}, true, nil
	case windows.SidTypeGroup, windows.SidTypeAlias, windows.SidTypeWellKnownGroup:
		return rule{subject: aclmap.Group, id: text, name: name, deny: deny}, true, nil
	default:
		if deny {
			return rule{}, false, fmt.Errorf("a refusal by %s, which is neither a person nor a group", name)
		}
		return rule{}, false, nil
	}
}

// markOwner records which of the statements belongs to the file's owner.
//
// Owning a file on Windows is not permission to read it. The owner may rewrite
// the list and then read it, which is not the same thing, and treating it as
// the same would grant a whole tree to whichever account happened to create it.
// So the owner is marked only where the list already lets them read.
func markOwner(rules []rule, sd *windows.SECURITY_DESCRIPTOR) []rule {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return rules
	}
	text := owner.String()
	for i := range rules {
		if rules[i].subject == aclmap.User && !rules[i].deny && rules[i].id == text {
			rules[i].owner = true
			break
		}
	}
	return rules
}

// changeTime has no answer on Windows.
//
// The file system does keep a change time, and reading it costs an open and a
// query per file, which is a price a sync over a large tree pays once per file
// per run for a fact that is almost always the same as last time. So a
// permission change here waits for the next full sync rather than being noticed
// between two.
func changeTime(fs.FileInfo) time.Time { return time.Time{} }
