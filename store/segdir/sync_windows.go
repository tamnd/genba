//go:build windows

package segdir

// syncDir does nothing on Windows, because there is nothing to do.
//
// A directory handle cannot be opened for writing and cannot be flushed, so the
// call that works everywhere else has no counterpart here. What Windows gives
// instead is that MoveFileEx with the replace flag, which is what os.Rename
// uses, is atomic with respect to the directory entry, so a reader sees the old
// file or the new one and never a partial name.
//
// The gap that leaves is a power cut in the window between the rename and the
// file system flushing its own metadata, which is bounded by the file system
// rather than by this package. It is written down here rather than papered
// over, because a durability claim that is not true on one of the platforms
// this ships on is worse than one that says which platform it means.
func syncDir(string) error { return nil }
