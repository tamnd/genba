//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package fssource

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"syscall"
	"time"

	"github.com/tamnd/genba/connector/aclmap"
)

// The read bits of a Unix mode, and the read bit of a POSIX access control list
// entry, which is the same value for a different reason.
const (
	readOwner = 0o400
	readGroup = 0o040
	readWorld = 0o004
	aclRead   = 0o4
)

// idOf is how a numeric user or group id is written for a lookup.
func idOf(id uint32) string { return strconv.FormatUint(uint64(id), 10) }

// accessRules reads the permissions Unix keeps on one file.
func accessRules(full string, info fs.FileInfo) ([]rule, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Every file system this runs on reports an owner, so reaching here means
		// the file information came from somewhere that is not the operating
		// system. Guessing at that point would be inventing an access control
		// list.
		return nil, errors.New("fssource: this file system does not report an owner")
	}

	raw, ok, err := posixACL(full)
	if err != nil {
		return nil, err
	}
	if ok {
		entries, err := decodePosixACL(raw)
		if err != nil {
			return nil, fmt.Errorf("fssource: %s: %w", full, err)
		}
		return aclRules(entries, st.Uid, st.Gid), nil
	}
	return modeRules(info.Mode().Perm(), st.Uid, st.Gid), nil
}

// modeRules turns the three read bits into three statements.
//
// The owner is the owner as well as a reader, which is what makes a file nobody
// else can read come out as owned by one person rather than as a list with one
// name in it. The two are the same grant today and they are not the same fact,
// and the mode is what a feature such as everything of mine reads.
func modeRules(mode fs.FileMode, uid, gid uint32) []rule {
	out := make([]rule, 0, 3)
	if mode&readOwner != 0 {
		out = append(out, rule{subject: aclmap.User, id: idOf(uid), owner: true})
	}
	if mode&readGroup != 0 {
		out = append(out, rule{subject: aclmap.Group, id: idOf(gid)})
	}
	if mode&readWorld != 0 {
		out = append(out, rule{subject: aclmap.Domain})
	}
	return out
}

// The tags a POSIX access control list entry can carry.
const (
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20
)

// aclEntry is one entry of a POSIX access control list.
type aclEntry struct {
	tag  uint16
	perm uint16

	// id is the user or group the entry names, and is meaningless for the
	// entries that name a position rather than a person.
	id uint32
}

// aclRules turns a POSIX access control list into statements, applying the
// mask.
//
// The mask is the part that is easy to leave out and expensive to leave out. It
// is the ceiling on every entry except the owner's and the world's, so a list
// that names a group with read and carries a mask without it is a list that
// does not let that group read the file. A reader that reported the entry and
// ignored the mask would offer the file to a team that cannot open it, which is
// the direction that hands somebody a document.
func aclRules(entries []aclEntry, uid, gid uint32) []rule {
	mask := uint16(0o7)
	for _, e := range entries {
		if e.tag == aclMask {
			mask = e.perm
		}
	}

	out := make([]rule, 0, len(entries))
	for _, e := range entries {
		switch e.tag {
		case aclUserObj:
			if e.perm&aclRead != 0 {
				out = append(out, rule{subject: aclmap.User, id: idOf(uid), owner: true})
			}
		case aclUser:
			if e.perm&mask&aclRead != 0 {
				out = append(out, rule{subject: aclmap.User, id: idOf(e.id)})
			}
		case aclGroupObj:
			if e.perm&mask&aclRead != 0 {
				out = append(out, rule{subject: aclmap.Group, id: idOf(gid)})
			}
		case aclGroup:
			if e.perm&mask&aclRead != 0 {
				out = append(out, rule{subject: aclmap.Group, id: idOf(e.id)})
			}
		case aclOther:
			if e.perm&aclRead != 0 {
				out = append(out, rule{subject: aclmap.Domain})
			}
		}
	}
	return out
}

// decodePosixACL reads the on disk form of a POSIX access control list.
//
// The layout is a version word followed by fixed size entries, all little
// endian whatever the machine is, which is what makes a disk written on one
// architecture readable on another.
func decodePosixACL(b []byte) ([]aclEntry, error) {
	const (
		version = 2
		header  = 4
		size    = 8
	)
	if len(b) < header {
		return nil, errors.New("access control list shorter than its header")
	}
	if v := binary.LittleEndian.Uint32(b); v != version {
		// A version this does not know is a layout this does not know, and
		// reading it anyway would produce entries naming people at random.
		return nil, fmt.Errorf("access control list version %d", v)
	}
	b = b[header:]
	if len(b)%size != 0 {
		return nil, errors.New("access control list ends inside an entry")
	}

	out := make([]aclEntry, 0, len(b)/size)
	for len(b) > 0 {
		out = append(out, aclEntry{
			tag:  binary.LittleEndian.Uint16(b),
			perm: binary.LittleEndian.Uint16(b[2:]),
			id:   binary.LittleEndian.Uint32(b[4:]),
		})
		b = b[size:]
	}
	return out, nil
}

// changeTime is when the inode last changed, which is when the mode, the owner
// or the access control list was last written.
func changeTime(info fs.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	sec, nsec := inodeChange(st)
	return time.Unix(sec, nsec)
}
