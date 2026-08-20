package fssource

import (
	"errors"
	"fmt"
	"syscall"
)

// The name Linux stores a POSIX access control list under, and a size that
// holds a list with a few hundred names in it.
//
// Asking for the length first and then reading would be two system calls per
// file over a tree where almost no file has a list at all, so the buffer is
// sized to cover the ones that do and a list longer than it is reported rather
// than truncated.
const (
	posixACLAttr = "system.posix_acl_access"
	posixACLMax  = 4 << 10
)

// posixACL reads the access control list attached to a file, and reports false
// where there is none, which is the ordinary case.
func posixACL(full string) (raw []byte, ok bool, err error) {
	buf := make([]byte, posixACLMax)
	n, err := syscall.Getxattr(full, posixACLAttr, buf)
	switch {
	case errors.Is(err, syscall.ENODATA), errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP):
		// No list on this file, or a file system that does not keep them. Both
		// mean the mode bits are the whole answer.
		return nil, false, nil
	case errors.Is(err, syscall.ERANGE):
		// A list longer than the buffer. Reading the front of it would leave
		// entries out, and an entry left out is either a grant nobody gets or a
		// refusal nobody applies.
		return nil, false, fmt.Errorf("fssource: %s: access control list over %d bytes", full, posixACLMax)
	case err != nil:
		return nil, false, fmt.Errorf("fssource: %s: %w", full, err)
	}
	return buf[:n], true, nil
}

// inodeChange is when the inode last changed.
func inodeChange(st *syscall.Stat_t) (sec, nsec int64) { return st.Ctim.Unix() }
