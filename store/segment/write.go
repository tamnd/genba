package segment

import (
	"fmt"
	"io"
	"slices"
)

// Writer assembles a segment.
//
// It holds the sections until the whole file can be written at once, because
// the header carries a checksum over everything after it and a length that
// bounds the file, and neither is known until the last section has arrived.
// Streaming a segment would mean seeking back to fill those in, which rules out
// writing to a pipe and rules out the reader's assumption that a file it can
// read at all is a file that was finished.
//
// The zero value is usable and writes a segment with sequence zero and no
// sections, which is a valid segment. An empty one is worth being able to write:
// a compaction that removes everything still has to say so.
type Writer struct {
	sequence uint64
	sections []section
}

type section struct {
	kind  Kind
	bytes []byte
}

// NewWriter returns a writer for a segment at the given sequence.
//
// The sequence is the caller's, not this package's. Segments are published by
// something that knows the order they were published in, and a package that
// invented its own counter would be a second opinion about which of two
// statements on a document is current.
func NewWriter(sequence uint64) *Writer {
	return &Writer{sequence: sequence}
}

// Add stores a section.
//
// The bytes are not copied. A writer is a short lived thing that exists between
// building the sections and writing them, and copying a hundred megabytes of
// postings to be defensive about a caller that is about to drop them anyway
// would be the most expensive line in the package. The contract is that the
// caller leaves them alone until [Writer.WriteTo] returns.
//
// A duplicate kind and an unknown kind are both refused. Two sections of the
// same kind have no defined meaning, and a section this build cannot name is a
// section nothing can read back.
func (w *Writer) Add(kind Kind, b []byte) error {
	if !kind.known() {
		return fmt.Errorf("%w: cannot write unknown section %s", ErrFormat, kind)
	}
	if slices.ContainsFunc(w.sections, func(s section) bool { return s.kind == kind }) {
		return fmt.Errorf("%w: section %s added twice", ErrFormat, kind)
	}
	w.sections = append(w.sections, section{kind: kind, bytes: b})
	return nil
}

// Bytes returns the finished segment.
//
// Sections come out in ascending kind order whatever order they went in, so the
// same sections produce the same bytes every time. That is what lets two
// machines building the same segment compare the files rather than the
// contents, and it is what lets a compaction that changed nothing be recognised
// as having changed nothing.
func (w *Writer) Bytes() ([]byte, error) {
	sections := slices.Clone(w.sections)
	slices.SortFunc(sections, func(a, b section) int { return int(a.kind) - int(b.kind) })

	table := headerSize + entrySize*len(sections)
	size := table
	for _, s := range sections {
		size += len(s.bytes)
	}

	out := make([]byte, size)
	copy(out, Magic)
	le.PutUint16(out[offVersion:], Version)
	le.PutUint16(out[offFlags:], 0)
	le.PutUint32(out[offSections:], uint32(len(sections)))
	le.PutUint64(out[offSequence:], w.sequence)
	le.PutUint64(out[offLength:], uint64(size-headerSize))
	le.PutUint32(out[offReserved:], 0)

	at := table
	for i, s := range sections {
		e := out[headerSize+i*entrySize:]
		le.PutUint32(e[entryKind:], uint32(s.kind))
		le.PutUint32(e[entryPad:], 0)
		le.PutUint64(e[entryOffset:], uint64(at))
		le.PutUint64(e[entryLength:], uint64(len(s.bytes)))
		copy(out[at:], s.bytes)
		at += len(s.bytes)
	}

	// Last, because it covers every other byte of the file: the header above it,
	// the table and the body.
	le.PutUint32(out[offChecksum:], checksum(out))
	return out, nil
}

// WriteTo writes the finished segment.
func (w *Writer) WriteTo(dst io.Writer) (int64, error) {
	b, err := w.Bytes()
	if err != nil {
		return 0, err
	}
	n, err := dst.Write(b)
	return int64(n), err
}
