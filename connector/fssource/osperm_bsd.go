//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package fssource

import "syscall"

// posixACL always reports that there is none.
//
// These systems keep extended access control lists of the NFSv4 shape, in a
// place a program can only reach through the C library, and they are not read
// here. A file carrying one gets the answer its mode bits give, which is
// narrower than the truth where the list grants and wider than it where the
// list refuses. That second case is the reason this is stated in the package
// documentation rather than left to be discovered: on these systems the policy
// is a good answer for an ordinary tree and not a safe one for a tree somebody
// has been managing with the access control list editor.
func posixACL(string) (raw []byte, ok bool, err error) { return nil, false, nil }

// inodeChange is when the inode last changed.
func inodeChange(st *syscall.Stat_t) (sec, nsec int64) { return st.Ctimespec.Unix() }
