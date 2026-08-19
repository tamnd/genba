// Package segment is the on disk format everything the platform stores ends up
// in.
//
// A segment is immutable once it has been written. Nothing edits one in place,
// and a delete is a tombstone in a later segment rather than a change to an
// earlier one. That is what makes a segment safe to memory map, safe to share
// between readers without a lock, safe to copy while a writer is running and
// safe to cache by name, and every one of those properties disappears the first
// time something rewrites a byte of a published file.
//
// # The shape
//
//	header      40 bytes, fixed
//	table       24 bytes per section, in ascending kind order
//	body        the section bytes, in the same order
//
// The header names the file, the version, the sequence and the checksum. The
// table says where each section is. The body is the sections themselves, and
// this package does not know what is in any of them: a section is a run of
// bytes with a kind on it, and the encodings live in the packages that own
// them. Keeping the container ignorant of its contents is what allows the
// posting encoding to change without the file format changing.
//
// # Reading is hostile by assumption
//
// A segment can arrive from a disk with a bad sector, a half finished write, a
// truncated copy or somebody's fuzzer. Every one of those looks like a length
// field that says something untrue, so [Open] takes the bytes it is given and
// never allocates anything sized from them. Every section it hands back is a
// subslice of the input. A length that claims four gigabytes therefore fails a
// bounds check rather than an allocation, and the difference between those two
// failures is the difference between an error and a dead process.
//
// Version checking is a refusal rather than a best effort. A file written by a
// build this one does not know is rejected, because a reader that parses an
// unknown version hopefully is a reader that will one day return the wrong
// answer instead of an error.
package segment

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// Magic is what every segment starts with. Eight bytes rather than four,
// because four bytes of anything turn up by accident in files that are not
// this, and the cost of the other four is nothing.
const Magic = "genbaseg"

// Version is the format this build writes and the only one it reads.
//
// It is a refusal and not a negotiation. When the format changes in a way that
// an old reader would misread, this goes up and the old reader says so. When it
// changes in a way an old reader can ignore, it does not, and the change goes
// in a new section kind instead: an unknown kind is skipped, which is the whole
// reason sections are addressed by kind rather than by position.
const Version uint16 = 1

// The fixed sizes of the two structures a reader has to parse before it can
// trust anything.
const (
	headerSize = 40
	entrySize  = 24
)

// Header layout, byte for byte. Little endian throughout, because every machine
// this runs on is little endian and a byte swap on the hot path to be polite to
// hardware nobody has is a cost paid forever for a benefit paid never.
//
//	 0  8  magic, the eight bytes of Magic
//	 8  2  version, which the reader refuses rather than interprets
//	10  2  flags, reserved and zero, so that a future bit can be a refusal too
//	12  4  the number of sections, which bounds the table and nothing else
//	16  8  sequence, which is what "a later segment" means when a tombstone
//	       has to win over the document it deletes
//	24  8  length, the bytes after the header, so a reader knows the extent of
//	       the file before it trusts a single offset inside it
//	32  4  checksum, crc32 Castagnoli over every other byte of the file
//	36  4  reserved, zero
//
// The checksum covers the header and the table as well as the body, which is
// the whole file apart from the four bytes holding the checksum itself.
// Checksumming only the body is the obvious version and it is wrong twice over.
// It leaves the offsets that address the body unprotected, so one flipped bit
// in the table gives a segment that passes its integrity check and hands back
// the wrong bytes. And it leaves the sequence unprotected, so one flipped bit
// in the header silently changes which of two segments a tombstone belongs to,
// which is a deleted document coming back to life with nothing anywhere
// reporting a problem.
const (
	offVersion  = 8
	offFlags    = 10
	offSections = 12
	offSequence = 16
	offLength   = 24
	offChecksum = 32
	offReserved = 36
)

// Table entry layout.
//
//	 0  4  kind
//	 4  4  reserved, zero
//	 8  8  offset, from the start of the file
//	16  8  length
//
// The offset is absolute rather than relative to the body, because every check
// a reader makes is against the length of the file and an absolute offset is
// checked directly. A relative offset has to be added to something first, and
// an addition is where an overflow gets in.
const (
	entryKind   = 0
	entryPad    = 4
	entryOffset = 8
	entryLength = 16
)

// Kind names a section.
//
// A reader skips a kind it does not know, which is what makes adding a section
// a change that old readers survive. The values are permanent: a kind is a name
// written into files that outlive the code, so one is never reused for
// something else, and a kind that is retired stays retired.
type Kind uint32

// The sections a segment can hold.
const (
	// KindTerms is the term dictionary, which maps a term to the id the rest of
	// the segment refers to it by. It exists so that a term appearing in eight
	// thousand documents is stored once rather than eight thousand times.
	KindTerms Kind = 1

	// KindPostings is the posting lists, which say which documents hold a term
	// and how often.
	KindPostings Kind = 2

	// KindFields is the stored fields, which is everything a result needs to be
	// displayed and nothing a query needs to be answered.
	KindFields Kind = 3

	// KindVectors is the embeddings, kept apart from the fields because they
	// are read by a different query path and are an order of magnitude larger.
	KindVectors Kind = 4

	// KindACL is the access control lists, which are in the segment rather than
	// beside it so that the permission filter runs against the same immutable
	// bytes as the match. A permission that lives somewhere else is a permission
	// that can be stale relative to the document it guards.
	KindACL Kind = 5

	// KindTombstones is the deletes. A delete never edits an earlier segment,
	// so it is a tombstone here and the sequence in the header is what decides
	// which of two statements about a document is the current one.
	KindTombstones Kind = 6
)

// String names a kind for an error message.
func (k Kind) String() string {
	switch k {
	case KindTerms:
		return "terms"
	case KindPostings:
		return "postings"
	case KindFields:
		return "fields"
	case KindVectors:
		return "vectors"
	case KindACL:
		return "acl"
	case KindTombstones:
		return "tombstones"
	}
	return "kind(" + itoa(uint64(k)) + ")"
}

// known reports whether this build understands a kind. A writer refuses to
// write one it does not know, because a segment naming a section nobody can
// read is a file that will be argued about later. A reader accepts one, because
// refusing would make every added section a breaking change.
func (k Kind) known() bool {
	switch k {
	case KindTerms, KindPostings, KindFields, KindVectors, KindACL, KindTombstones:
		return true
	}
	return false
}

// The errors a caller can act on differently.
//
// They are separate values rather than one error with a string in it because
// the three of them mean genuinely different things to an operator. A bad magic
// is a file that is not a segment, which is usually a path bug. An unknown
// version is a segment written by a newer build, which is a rollback that went
// the wrong way. A failed checksum is a segment that was a segment and is not
// any more, which is hardware.
var (
	ErrMagic    = errors.New("segment: not a segment file")
	ErrVersion  = errors.New("segment: unknown format version")
	ErrChecksum = errors.New("segment: checksum mismatch")
	ErrFormat   = errors.New("segment: malformed segment")
)

// castagnoli is the polynomial with hardware support on every architecture this
// targets. The checksum is on the open path of every segment, so a software
// implementation would be a cost paid on every read forever.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// checksum is the sum over a whole segment except the four bytes that hold it.
// Skipping those is what makes the field verifiable at all: a checksum that
// covered itself would have no fixed point to compare against.
func checksum(b []byte) uint32 {
	c := crc32.Checksum(b[:offChecksum], castagnoli)
	return crc32.Update(c, castagnoli, b[offChecksum+4:])
}

// itoa avoids pulling strconv into a package that otherwise touches nothing.
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// le is the byte order of everything in here, named once so that the choice is
// made in one place.
var le = binary.LittleEndian
