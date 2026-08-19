# Columns

A segment's sections hold columns, and a column is one field across every row of the segment.
Which source a document came from, when it was last modified, who owns it, whether it is archived.

Version 1.
The implementation is `store/column`, and the container it lives inside is [the segment format](segment.md).

## What a scan returns

A bitmap of row numbers, and never the rows.

That is the whole design.
A filter that materialises a row before deciding whether to keep it has paid for every row it is about to throw away, and on a corpus where a source filter cuts a million rows to a thousand that is three orders of magnitude of decoding nobody asked for.
So a scan hands back which rows matched, the permission model hands back which rows a principal may read, and the answer is the intersection.
Only what survives both is ever turned back into a document.

The bitmap is dense, one bit per row.
A compressed bitmap would win on the selective filters and lose on the ones that match half the segment, and it would put a decoder in front of the one operation on this path that has to be as fast as an AND instruction.

## One physical representation

Every type is stored as a sequence of unsigned integer codes, and the codes are in the same order as the values they stand for.

| Type | What the code is | Base |
| --- | --- | --- |
| string | a rank in the column's sorted dictionary | 0 |
| int | the value less the smallest value in the column | that smallest value |
| time | the same, on Unix milliseconds | that smallest millisecond |
| bool | 0 or 1 | 0 |

Two things fall out of that table.

The first is that a range on values is a range on codes for every type, including strings, because the dictionary is sorted before it is written.
There is one scan loop underneath equality, set membership, prefix, integer range, date range and boolean, and it compares integers.
A filter for six sources over a million rows reads no text at all: the six values are resolved against the dictionary first, which costs a binary search each, and what the scan then walks is codes.

The second is that a column of timestamps a week apart needs the bits to tell a week apart, not the bits to hold a Unix epoch.
Subtracting the smallest value is what makes that true.

## Two encodings

Plain packs every code at the narrowest width that fits.
A thousand distinct sources is ten bits a row, not a string and not a pointer.

Runs stores each value once with the number of rows it covers, for the columns that arrive sorted or nearly so, which in a search index is most of the interesting ones.

The builder measures both and keeps the smaller.
There is no knob for it, because the builder has the data in front of it and the caller does not.
A column of a thousand rows in four runs wants run lengths, the same column shuffled wants packing, and nothing about the schema says which one arrived.

A column whose every value is the same stores no codes at all.
The width comes out at zero and the scan answers all rows or none without reading anything.

## Nulls

A null is absence, not a value, so it does not get a code.

A column that has any carries a presence bitmap, one bit per row, and every scan intersects with it.
A null never matches a predicate, including a range that happens to contain whatever placeholder sits in the code stream, and asking for the nulls deliberately is what `Nulls` is for.

That case is easy to get wrong and there is a test for exactly it.
A column holding 5, null, 7, null has a base of five, so a null's placeholder code of zero stands for the value five, and a range over every integer there is must still match two rows rather than four.

## Bit packing

Codes are packed least significant bit first, and a code is read by loading eight bytes at the byte its first bit lives in and shifting.

That load has to be able to see the whole code, so a code has to end within eight bytes of the byte it starts in: seven bits of offset plus the width has to stay inside sixty four.
Widths up to 57 satisfy it.
Anything wider is stored at the full sixty four, where every code starts on a byte boundary and the question does not arise.
It takes a column of more than a hundred quintillion distinct values to reach that, so the rule costs nothing and removes a case from the reader.

The packed section carries eight bytes of slack on the end so the load for the last code is always in bounds.
Without it the last code is a read past the end of a mapped file.

## The header

Fixed size, at the front, and the version is the first byte so a reader can refuse an unknown one before it has interpreted anything else.

| Offset | Size | Field |
| --- | --- | --- |
| 0 | 1 | version |
| 1 | 1 | type |
| 2 | 1 | encoding |
| 3 | 1 | bits per packed code |
| 4 | 4 | rows |
| 8 | 4 | dictionary entries, zero unless the type is string |
| 12 | 1 | flags, bit 0 says a presence bitmap follows |
| 13 | 3 | reserved, zero |
| 16 | 8 | base |

Then, in this order: the presence bitmap when there is one, the codes, and the dictionary when the type is string.

All integers are little endian, for the same reason they are in the segment format.

## Reading bytes that may be anything

These bytes come off a disk.
A reader that panics on a damaged file turns a recoverable segment into an outage, so `Open` checks the structure before it believes any of it, and there is a fuzz target whose only contract is that nothing it is fed ever panics.

| Error | What it means |
| --- | --- |
| `ErrVersion` | a column written by something newer, so go and find that thing |
| `ErrFormat` | bytes that were damaged or were never a column |
| `ErrType` | a query asking a string column for a date range, which is a caller bug rather than a data one |

Two of those are worth separating on purpose.
An operator told "malformed" about a file that is merely from a newer build goes looking in the wrong place.

The checks that matter:

- The declared sizes have to add up to exactly the bytes given, with nothing left over.
- A run's length has to be non zero and has to fit in the rows that are left, and the runs together have to cover the rows the header claims.
- The run count is a number read out of hostile bytes, so nothing is allocated from it directly.
  Every run costs at least two bytes, so the capacity comes from the smaller of the declared count and half the bytes remaining.
- Dictionary offsets are cumulative, so they have to start at zero and never go backwards.
  A pair that slices backwards would be a panic in an accessor, and checking it once here is what lets every accessor below skip it.
- A code that indexes past the end of the dictionary is possible in bytes that were tampered with, because the packed width can hold more values than the dictionary has.
  It is not checked at open, which would be a full scan of the column, so it is bounds checked where it is used: such a row reads back as having no value, and it matches nothing.

## What it costs

On an Apple M4, a million rows, a thousand distinct string values:

| Scan | rows a second |
| --- | --- |
| string equality, packed | 526 million |
| string set of four, packed | 537 million |
| string prefix, packed | 441 million |
| integer range, packed | 326 million |
| date range, packed | 195 million |
| boolean, packed | 298 million |
| any of them, run length encoded | above 60 billion |

The gap between the two halves of that table is the argument for measuring both encodings and keeping the smaller.
A run length scan does work per run rather than per row and fills whole words of the answer at a time, so a column that arrives sorted is read two to three hundred times faster than the same column shuffled.
`go test -bench . ./store/column/` is where those numbers come from.

The dictionary is worth what it costs.
A million rows of a thousand distinct values, 33 MB of text, encodes to 1.29 MB at ten bits a row, which is 25.6 times smaller.
There is a test that fails if that ratio drops below ten.

## What is deliberately not here yet

A time column keeps milliseconds, and a column whose values are all on an hour boundary spends twenty two bits a row on zeros that a common divisor would remove.
That shows up in the table above as the date range being the slowest of the packed scans, and it is the obvious next thing.

Facet counts, which are a tally per code over a bitmap rather than a new encoding.
`Dict` and `CodeAt` are what that will be built on, and they are exported for it.

Delta encoding for a sorted numeric column, which the run length encoding already covers whenever the values repeat and does not cover when they are sorted and distinct.
