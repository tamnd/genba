# The segment format

Everything the platform stores ends up in a segment.
The format decides what is possible later, and it is expensive to undo once anybody has data in it, so this file says what it is and why each field is there.

Version 1.
The implementation is `store/segment`.

## The shape of a file

```
+------------------------------------------+
| header        40 bytes                   |
+------------------------------------------+
| offset table  24 bytes per section       |
+------------------------------------------+
| body          the sections themselves    |
+------------------------------------------+
```

All integers are little endian.
Every machine this runs on is little endian, and a byte swap on the open path to be polite to hardware nobody has is a cost paid forever for a benefit paid never.

## The header

| Offset | Size | Field | Why it is there |
| --- | --- | --- | --- |
| 0 | 8 | magic, `genbaseg` | Eight bytes rather than four, because four bytes of anything turn up by accident in files that are not this, and the other four cost nothing |
| 8 | 2 | version | A refusal rather than a negotiation, see below |
| 10 | 2 | flags | Reserved and zero, so that a future bit can be a refusal too |
| 12 | 4 | sections | Bounds the offset table and nothing else |
| 16 | 8 | sequence | What "a later segment" means when a tombstone has to win over the document it deletes |
| 24 | 8 | length | The bytes after the header, so a reader knows the extent of the file before it trusts a single offset inside it |
| 32 | 4 | checksum | crc32 Castagnoli over every other byte of the file |
| 36 | 4 | reserved | Zero |

## The offset table

One entry per section, in ascending kind order.

| Offset | Size | Field |
| --- | --- | --- |
| 0 | 4 | kind |
| 4 | 4 | reserved, zero |
| 8 | 8 | offset from the start of the file |
| 16 | 8 | length |

The offset is absolute rather than relative to the body.
Every check a reader makes is against the length of the file, and an absolute offset is checked directly, whereas a relative one has to be added to something first and an addition is where an overflow gets in.

## The sections

| Kind | Name | What it holds |
| --- | --- | --- |
| 1 | terms | The term dictionary, mapping a term to the id the rest of the segment refers to it by |
| 2 | postings | Which documents hold a term and how often |
| 3 | fields | Everything a result needs to be displayed and nothing a query needs to be answered |
| 4 | vectors | Embeddings, kept apart from the fields because they are read by a different query path and are an order of magnitude larger |
| 5 | acl | The access control lists |
| 6 | tombstones | The deletes |

Kind values are permanent.
A kind is a name written into files that outlive the code, so one is never reused for something else and a retired kind stays retired.

The container does not know what is inside any of these.
A section is a run of bytes with a kind on it, and the encodings live in the packages that own them, which is what allows the posting encoding to change without the file format changing.

## Why the access control lists are in the segment

The permission filter has to run against the same immutable bytes as the match.
A permission that lives beside the segment rather than in it is a permission that can be stale relative to the document it guards, and the window in which it is stale is a window in which somebody reads a document they cannot read.

## Why a delete is a tombstone

Segments are immutable once published.
Nothing edits one in place, which is what makes a segment safe to memory map, safe to share between readers without a lock, safe to copy while a writer is running and safe to cache by name.
Every one of those properties disappears the first time something rewrites a byte of a published file.

So a delete is a tombstone in a later segment, and the sequence in the header is what decides which of two statements about the same document is the current one.

## What the checksum covers

Every byte of the file except the four holding the checksum itself.

Checksumming only the body is the obvious version and it is wrong twice over.
It leaves the offsets that address the body unprotected, so one flipped bit in the table gives a segment that passes its integrity check and hands back the wrong bytes.
And it leaves the sequence unprotected, so one flipped bit in the header silently changes which segment a tombstone belongs to, which is a deleted document coming back to life with nothing anywhere reporting a problem.

This was not a design decision made up front.
The first version checksummed the body, and the test that flips every byte of a valid segment in turn found byte 16, which is the sequence.

## Version checking is a refusal

A file written by a build the reader does not know is rejected with a clear error rather than parsed hopefully.
A reader that parses an unknown version hopefully is a reader that will one day return the wrong answer instead of an error.

The same goes for an unknown flag bit.
A flag this build does not know changes the meaning of the bytes below it in a way it cannot guess, so it is the same refusal rather than something to ignore politely.

An unknown *section kind* is the exception, and it is skipped rather than refused.
That is the whole reason sections are addressed by kind rather than by position: adding a section has to be a change old readers survive, or the version goes up every time anything is added and every deployment becomes a lockstep upgrade.

## Reading is hostile by assumption

A segment can arrive from a disk with a bad sector, a half finished write, a truncated copy or somebody's fuzzer.
Every one of those looks like a length field that says something untrue.

`Open` takes the bytes it is given and never allocates anything sized from them.
Every section it hands back is a subslice of the input, so a length claiming four gigabytes fails a bounds check rather than an allocation, and the difference between those two failures is the difference between an error and a dead process.

The one allocation `Open` makes is the parsed table, and it is bounded by the length of the file before the section count is used for anything.

The checks run in this order, and the order is the point:

1. **Magic**, so a file that is not a segment says that rather than something about a version.
2. **Version and flags**, because a newer format could put anything at all in the bytes that follow and nothing below this line is worth doing until the layout is known.
3. **Declared length against actual length**, exactly. Shorter is a truncated file. Longer is refused too, because a segment with room after it is a segment somebody will one day put something after, and once one file has a use for that space the format has grown a feature nobody designed.
4. **Checksum**, before the table, because a failed checksum explains every structural error that would otherwise be reported instead of it. "Malformed at offset 240" sends somebody looking for a bug in a writer when the answer is a bad disk.
5. **Structure**: sections ascending by offset, non overlapping, starting after the table, ending inside the file, with kinds strictly ascending so no kind appears twice.

Bounds are written as subtractions rather than additions, so an offset near the top of the range cannot be carried past it.

## The errors

| Error | Means | Usually |
| --- | --- | --- |
| `ErrMagic` | This is not a segment | A path bug |
| `ErrVersion` | A segment from a build this one does not know | A rollback that went the wrong way |
| `ErrChecksum` | This was a segment and is not any more | Hardware |
| `ErrFormat` | Structurally impossible | A writer bug, or something hostile |

They are separate values rather than one error with a string in it, because those four mean genuinely different things to whoever is on call.

## Writing

Sections are held until the whole file can be written at once, because the header carries a checksum over everything else and a length that bounds the file, and neither is known until the last section has arrived.
Streaming would mean seeking back to fill those in, which rules out writing to a pipe and rules out the reader's assumption that a file it can read at all is a file that was finished.

Sections come out in ascending kind order whatever order they went in, so the same sections produce the same bytes every time.
That is what lets two machines building the same segment compare files rather than contents, and what lets a compaction that changed nothing be recognised as having changed nothing.

The writer refuses a kind it does not know and a kind added twice.
The reader accepts an unknown kind, for the forward compatibility reason above.
That asymmetry is deliberate: be strict in what you write, and be exactly as permissive in what you accept as you decided in advance to be.
