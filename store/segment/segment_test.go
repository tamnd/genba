package segment_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/tamnd/genba/store/segment"
)

var le = binary.LittleEndian

// build is the segment most of these tests work over: every section populated,
// with contents that are distinguishable from each other so that a reader
// handing back the wrong window is a failure rather than a coincidence.
func build(t testing.TB) []byte {
	t.Helper()

	b, err := populated()
	if err != nil {
		t.Fatalf("building a segment: %v", err)
	}
	return b
}

// populated is build without a testing.TB, so that the fuzz seed corpus can be
// built at registration time rather than by handing a fabricated *testing.T to
// something that would call Fatalf on it.
func populated() ([]byte, error) {
	w := segment.NewWriter(9)
	for _, s := range []struct {
		kind  segment.Kind
		bytes []byte
	}{
		{segment.KindTerms, []byte("terms section")},
		{segment.KindPostings, bytes.Repeat([]byte{0xAB}, 4096)},
		{segment.KindFields, []byte(`{"title":"a document"}`)},
		{segment.KindVectors, bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 128)},
		{segment.KindACL, []byte("group:everyone")},
		{segment.KindTombstones, []byte{0, 0, 0, 0, 0, 0, 0, 7}},
	} {
		if err := w.Add(s.kind, s.bytes); err != nil {
			return nil, err
		}
	}
	return w.Bytes()
}

func TestAnEmptySegmentIsASegment(t *testing.T) {
	// A compaction that removed everything still has to say so, so a segment
	// with no sections has to be writable and readable rather than an edge case
	// somebody discovers in production.
	b, err := segment.NewWriter(0).Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	s, err := segment.Open(b)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(s.Kinds()) != 0 {
		t.Errorf("an empty segment holds %v", s.Kinds())
	}
	if _, ok := s.Section(segment.KindTerms); ok {
		t.Error("an empty segment produced a terms section")
	}
	if s.Sequence() != 0 {
		t.Errorf("Sequence = %d, want 0", s.Sequence())
	}
}

func TestOneSectionRoundTrips(t *testing.T) {
	w := segment.NewWriter(1)
	want := []byte(`{"id":"repo:README.md","title":"README"}`)
	if err := w.Add(segment.KindFields, want); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := w.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	s, err := segment.Open(b)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, ok := s.Section(segment.KindFields)
	if !ok {
		t.Fatal("the section that was written is not there")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Section = %q, want %q", got, want)
	}
	if _, ok := s.Section(segment.KindPostings); ok {
		t.Error("a section that was never written came back")
	}
}

func TestEverySectionRoundTrips(t *testing.T) {
	s, err := segment.Open(build(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Sequence() != 9 {
		t.Errorf("Sequence = %d, want 9", s.Sequence())
	}

	kinds := s.Kinds()
	want := []segment.Kind{
		segment.KindTerms, segment.KindPostings, segment.KindFields,
		segment.KindVectors, segment.KindACL, segment.KindTombstones,
	}
	if len(kinds) != len(want) {
		t.Fatalf("Kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("Kinds = %v, want %v", kinds, want)
		}
	}

	terms, _ := s.Section(segment.KindTerms)
	if string(terms) != "terms section" {
		t.Errorf("terms = %q", terms)
	}
	postings, _ := s.Section(segment.KindPostings)
	if len(postings) != 4096 || postings[0] != 0xAB || postings[4095] != 0xAB {
		t.Errorf("postings came back as %d bytes starting %#x", len(postings), postings[:min(4, len(postings))])
	}
	tombstones, _ := s.Section(segment.KindTombstones)
	if len(tombstones) != 8 || tombstones[7] != 7 {
		t.Errorf("tombstones = %v", tombstones)
	}
}

func TestTheSameSectionsProduceTheSameBytes(t *testing.T) {
	// Determinism is what lets two machines compare segments by name rather
	// than by content, so the order sections are added in must not reach the
	// file.
	forward := segment.NewWriter(4)
	backward := segment.NewWriter(4)
	adds := []struct {
		kind  segment.Kind
		bytes []byte
	}{
		{segment.KindTerms, []byte("a")},
		{segment.KindPostings, []byte("bb")},
		{segment.KindACL, []byte("ccc")},
	}
	for _, a := range adds {
		if err := forward.Add(a.kind, a.bytes); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	for i := len(adds) - 1; i >= 0; i-- {
		if err := backward.Add(adds[i].kind, adds[i].bytes); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	a, err := forward.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	b, err := backward.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("the order sections were added in changed the file")
	}
}

func TestWriteToWritesTheSameBytes(t *testing.T) {
	w := segment.NewWriter(2)
	if err := w.Add(segment.KindTerms, []byte("terms")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	want, err := w.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	var buf bytes.Buffer
	n, err := w.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != int64(len(want)) || !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("WriteTo wrote %d bytes, want %d identical ones", n, len(want))
	}
}

func TestAWriterRefusesWhatCannotBeReadBack(t *testing.T) {
	w := segment.NewWriter(1)
	if err := w.Add(segment.Kind(4242), []byte("x")); !errors.Is(err, segment.ErrFormat) {
		t.Errorf("Add of an unknown kind = %v, want ErrFormat", err)
	}
	if err := w.Add(segment.KindTerms, []byte("x")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := w.Add(segment.KindTerms, []byte("y")); !errors.Is(err, segment.ErrFormat) {
		t.Errorf("Add of a duplicate kind = %v, want ErrFormat", err)
	}
}

func TestTheThreeRefusalsAreDistinct(t *testing.T) {
	valid := build(t)

	tests := []struct {
		name string
		bad  func([]byte) []byte
		want error
	}{
		{"an empty file", func([]byte) []byte { return nil }, segment.ErrMagic},
		{"something that is not a segment", func([]byte) []byte {
			return bytes.Repeat([]byte("not a segment at all, just some bytes"), 8)
		}, segment.ErrMagic},
		{"a segment with one letter of the magic wrong", func(b []byte) []byte {
			b[3] ^= 0x20
			return b
		}, segment.ErrMagic},
		{"a version this build does not read", func(b []byte) []byte {
			le.PutUint16(b[8:], 99)
			return b
		}, segment.ErrVersion},
		{"a flag this build does not know", func(b []byte) []byte {
			le.PutUint16(b[10:], 1)
			return b
		}, segment.ErrVersion},
		{"a flipped bit in the body", func(b []byte) []byte {
			b[len(b)-3] ^= 0x01
			return b
		}, segment.ErrChecksum},
		{"a flipped bit in the offset table", func(b []byte) []byte {
			b[48] ^= 0x01
			return b
		}, segment.ErrChecksum},
		{"reserved header bytes that are not zero", func(b []byte) []byte {
			le.PutUint32(b[36:], 1)
			return b
		}, segment.ErrFormat},
		{"a length that claims more than the file holds", func(b []byte) []byte {
			le.PutUint64(b[24:], math.MaxUint64)
			return b
		}, segment.ErrFormat},
		{"bytes appended after the end", func(b []byte) []byte {
			return append(b, 0, 0, 0, 0)
		}, segment.ErrFormat},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := segment.Open(tt.bad(bytes.Clone(valid)))
			if !errors.Is(err, tt.want) {
				t.Fatalf("Open = %v, want %v", err, tt.want)
			}
		})
	}

	// The version check has to happen before anything below it is trusted, so
	// a file that is a newer version and also fails its checksum must report
	// the version. Reporting the checksum would send somebody looking at a disk
	// when the answer is that they rolled a binary back.
	b := bytes.Clone(valid)
	le.PutUint16(b[8:], 99)
	b[len(b)-1] ^= 0xFF
	if _, err := segment.Open(b); !errors.Is(err, segment.ErrVersion) {
		t.Errorf("a newer segment that is also corrupt reported %v, want ErrVersion", err)
	}
}

// TestTruncationIsAnErrorAndNeverAPanic cuts a valid segment at every length
// there is.
//
// This is the test the format exists to pass. A truncated segment is what a
// half finished write, a full disk and an interrupted copy all look like, and
// every one of them presents as a length field describing bytes that are not
// there. Not one of them may reach a slice expression.
func TestTruncationIsAnErrorAndNeverAPanic(t *testing.T) {
	valid := build(t)
	for n := range len(valid) {
		s, err := segment.Open(valid[:n:n])
		if err == nil {
			t.Fatalf("a segment truncated to %d of %d bytes opened cleanly", n, len(valid))
		}
		if s != nil {
			t.Fatalf("a segment truncated to %d bytes returned both a segment and an error", n)
		}
	}

	// And the whole thing still opens, so the sweep above was not passing
	// because everything fails.
	if _, err := segment.Open(valid); err != nil {
		t.Fatalf("the untruncated segment does not open: %v", err)
	}
}

// TestEveryFlippedByteIsCaught is the other half of the sweep: a segment that
// is the right length and the wrong bytes.
func TestEveryFlippedByteIsCaught(t *testing.T) {
	valid := build(t)
	for i := range valid {
		// The checksum field itself is skipped, since flipping a bit there is
		// tested above and is the one byte whose corruption is detected by
		// being different rather than by covering something different.
		b := bytes.Clone(valid)
		b[i] ^= 0xFF
		if _, err := segment.Open(b); err == nil {
			t.Fatalf("byte %d of %d could be changed without the segment noticing", i, len(valid))
		}
	}
}

// TestASectionCannotBeMadeToAliasAnother is the reason the table is checked for
// order and overlap rather than only for bounds.
//
// A table that passes its bounds checks can still point two sections at the
// same bytes, or point one backwards into the table itself, and a reader that
// only checked bounds would hand those windows out.
func TestASectionCannotBeMadeToAliasAnother(t *testing.T) {
	valid := build(t)

	tests := []struct {
		name string
		bad  func([]byte)
	}{
		{"a section that starts before the one before it", func(b []byte) {
			// The second entry's offset, moved back over the first section.
			le.PutUint64(b[40+24+8:], uint64(40+6*24))
		}},
		{"a section that reaches into the table", func(b []byte) {
			le.PutUint64(b[40+8:], 40)
		}},
		{"two sections of the same kind", func(b []byte) {
			le.PutUint32(b[40+24:], uint32(segment.KindTerms))
		}},
		{"a section that runs off the end", func(b []byte) {
			le.PutUint64(b[40+16:], math.MaxUint64)
		}},
		{"reserved bytes of an entry that are not zero", func(b []byte) {
			le.PutUint32(b[40+4:], 1)
		}},
		{"a section count larger than the table", func(b []byte) {
			le.PutUint32(b[12:], math.MaxUint32)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := bytes.Clone(valid)
			tt.bad(b)
			// The checksum is recomputed, because this is not about detecting
			// corruption. It is about a segment that is internally consistent
			// and structurally impossible, which is what a hostile writer
			// produces.
			reseal(b)
			if _, err := segment.Open(b); !errors.Is(err, segment.ErrFormat) {
				t.Fatalf("Open = %v, want ErrFormat", err)
			}
		})
	}
}

// reseal recomputes the checksum over a segment that has been edited, so that a
// test about structure is not accidentally a test about the checksum.
func reseal(b []byte) {
	le.PutUint32(b[32:], crc32c(b))
}

// crc32c is the same sum the package computes, written out the slow way so that
// the tests prove the format rather than proving a function agrees with itself.
// It covers every byte except the four holding the checksum.
func crc32c(b []byte) uint32 {
	const poly = 0x82f63b78
	crc := ^uint32(0)
	for i, v := range b {
		if i >= 32 && i < 36 {
			continue
		}
		crc ^= uint32(v)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ poly
			} else {
				crc >>= 1
			}
		}
	}
	return ^crc
}

func TestAnUnknownSectionIsSkippedRatherThanRefused(t *testing.T) {
	// Adding a section has to be a change old readers survive, which is why a
	// reader accepts a kind it cannot name. The writer refuses to produce one,
	// so this builds it by hand the way a newer build would.
	valid := build(t)
	b := bytes.Clone(valid)
	le.PutUint32(b[40+5*24:], 999) // the last entry, retyped to a kind from the future
	reseal(b)

	s, err := segment.Open(b)
	if err != nil {
		t.Fatalf("a segment with a section from the future does not open: %v", err)
	}
	if _, ok := s.Section(segment.KindTombstones); ok {
		t.Error("the retyped section still answers to its old kind")
	}
	if _, ok := s.Section(segment.Kind(999)); !ok {
		t.Error("a section this build cannot name is not readable by number either")
	}
	if got, _ := s.Section(segment.KindTerms); string(got) != "terms section" {
		t.Errorf("a section from the future broke the ones around it: terms = %q", got)
	}
}

func TestASectionIsAWindowAndNotACopy(t *testing.T) {
	// The whole point of opening over bytes is that a hundred megabyte segment
	// costs a hundred megabytes however many readers it has.
	b := build(t)
	s, err := segment.Open(b)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	postings, _ := s.Section(segment.KindPostings)
	if cap(postings) != len(postings) {
		t.Errorf("a section window has %d bytes of capacity past its length, so a caller can append into the next section", cap(postings)-len(postings))
	}
	b[len(b)-1] = 0xFF
	tombstones, _ := s.Section(segment.KindTombstones)
	if tombstones[len(tombstones)-1] != 0xFF {
		t.Error("a section was copied rather than windowed")
	}
}

func TestKindNamesItself(t *testing.T) {
	for _, tt := range []struct {
		kind segment.Kind
		want string
	}{
		{segment.KindTerms, "terms"},
		{segment.KindPostings, "postings"},
		{segment.KindFields, "fields"},
		{segment.KindVectors, "vectors"},
		{segment.KindACL, "acl"},
		{segment.KindTombstones, "tombstones"},
		{segment.Kind(77), "kind(77)"},
	} {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("Kind(%d).String() = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func FuzzOpenSurvivesAnything(f *testing.F) {
	if b, err := populated(); err == nil {
		f.Add(b)
	}
	f.Add([]byte(nil))
	f.Add([]byte("genbaseg"))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := segment.Open(b)
		if err != nil {
			return
		}
		// A segment that opened has to be usable, since the point of refusing
		// early is that nothing downstream has to check again.
		for _, k := range s.Kinds() {
			if _, ok := s.Section(k); !ok {
				t.Fatalf("Kinds reported %s and Section does not have it", k)
			}
		}
		if s.Size() != len(b) {
			t.Fatalf("Size = %d over %d bytes", s.Size(), len(b))
		}
	})
}
