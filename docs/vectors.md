# Vectors

A segment's sections hold columns for filtering and one section for embeddings.
This is that section, and the search over it.

Version 1.
The implementation is `store/vector`, and the container it lives inside is [the segment format](segment.md).

## The flat scan comes first

Every vector in the segment is scored against the query, and the best k are kept.

That is not a placeholder for a graph index.
It is the thing a graph index has to beat, and there is no way to know whether it does without the number it has to beat.
A flat scan has no candidate list, no entry point and no beam width, so its recall is one by construction rather than by tuning, and every accuracy question about this package reduces to the quantisation and nothing else.

It also stays useful after there is a second index.
A segment of ten thousand documents is scanned in under a millisecond, and a graph built over ten thousand vectors costs more to build than the scan it replaces costs to run.

## The permission filter is the loop

`Search` takes a bitmap of the rows the reader may see, and it walks that bitmap.
It does not walk the rows and ask the bitmap.

The difference is the whole point.
A scan that scores everything and filters afterwards has already read a document the caller is not allowed to see, and the only thing between that read and a leak is the code that comes next.
Here an unreadable document is never touched, so there is no result to drop, no count to correct and no score to accidentally surface in an aggregate.

It is also faster, and the benchmark below shows it: a reader who may see a hundredth of the corpus pays a hundredth of the scan.
A scan that filtered afterwards would cost the same at every selectivity.

A nil bitmap means every row, which is what a caller with no principal to apply passes.
That is the only way to reach rows nobody vouched for, and it is deliberately the shape that has to be written out rather than the default that happens by omission.

## Quantisation

A vector is normalised, then each component is stored as one signed byte.

The scale is the largest component of the normalised vector divided by 127, kept as a float32 beside the row.
A component becomes a code by dividing by that scale and rounding.
A query is quantised by the same rule, so the inner loop is an integer multiply and add over two byte arrays and the cosine is the integer dot product times the two scales.

Keeping the query at full precision would mean converting a byte to a float on every component of every candidate, which is the one operation on this path that runs a few hundred million times a query.

Per row the section costs `4 + dim` bytes against the `4 * dim` a float32 array would.
At 768 dimensions that is 772 bytes against 3072, so a million documents is 772 MB rather than 3 GB.

The accuracy that buys:

| Dimensions | Worst score off the exact cosine |
| --- | --- |
| 64 | 0.004076 |
| 384 | 0.001402 |
| 768 | 0.001514 |

Those are the worst row of five hundred, not the average, and they get better as the vectors get wider because the error per component is independent and the dot product averages over more of them.
An embedding model does not distinguish two documents at four thousandths of a cosine, so this is well inside the noise of the thing being measured.

The test that matters is not the size of the error though, it is whether the order survives it.
`TestQuantisationDoesNotReorderClearResults` computes the exact float64 cosine for every row of a 1500 row corpus, then checks every pair in the returned order for an inversion.
Two documents within the margin of each other may trade places, which is a swap nothing downstream can see.
A pair separated by more than a hundredth of a cosine coming back the wrong way round fails.

## Rows with no vector

A document with no embedding still gets a row and still costs its bytes.

The alternative is a section that skips it, which needs a second row numbering and a map between that and the segment's, and then every join between a vector result and a column filter goes through the map.
Row numbers being the segment's is what lets a permission bitmap built from the columns be handed straight to the scan.

A null row is a scale of zero, which is a value a real vector cannot have, because a vector with no largest component is a vector of all zeros.
Those are refused at write with `ErrZero` rather than stored as a null.
An embedder that returns zeros is broken, and a pipeline that quietly indexes that is a pipeline where a broken model looks like a corpus with no matches.

The cost is real and worth stating: a segment where a tenth of the documents have embeddings still writes the full `4 + dim` bytes for all of them.
That is the price of one row numbering, and it is the right trade until there is a corpus where it is not.

## The header

Fixed size, at the front, version first, the same shape the column format uses.

| Offset | Size | Field |
| --- | --- | --- |
| 0 | 1 | version |
| 1 | 1 | metric |
| 2 | 1 | index kind |
| 3 | 1 | flags, zero in this version |
| 4 | 4 | dimensions |
| 8 | 4 | rows |
| 12 | 4 | reserved, zero |

Then the scales, one float32 per row, then the codes, `dim` bytes per row.

All integers are little endian, for the same reason they are in the segment format.

## The index kind is a byte, not a branch

Byte 2 says which index the section carries.
`Open` reads it and returns an `Index`, and a caller holding one of those cannot tell an exact scan from an approximate index apart from asking it.

On the write side `NewBuilder` takes a kind, and `KindAuto` leaves the choice to the builder.
`Builder.resolve` is the single place that choice is made, and it is a method rather than a function because the size threshold a graph index would need is a row count and the builder has it.
Today every size resolves to flat.

That is what makes the second index a new file in this package rather than a change to everything that calls it.
It is not yet an operator facing setting, because nothing writes segments yet: see the last section.

## Reading bytes that may be anything

These bytes come off a disk, so `Open` checks the structure before it believes any of it, and there is a fuzz target whose only contract is that nothing it is fed ever panics.

| Error | What it means |
| --- | --- |
| `ErrVersion` | a section written by something newer, so go and find that thing |
| `ErrFormat` | bytes that were damaged or were never a section |
| `ErrKind` | an index this build cannot read |
| `ErrDim` | a width that is impossible, or a query from a different segment |

The checks that matter:

- An unknown flag is refused like an unknown version rather than skipped politely, because a flag this build does not know changes the meaning of the bytes below it in a way it cannot guess.
- Nothing is allocated from a length read out of the bytes.
  The row count and the width are turned into the size the section would have to be, and that has to equal the bytes actually on hand exactly, with nothing left over.
  A header claiming four billion rows fails that comparison rather than reserving memory for them.
- A row outside the section reads back as having no vector rather than panicking, because the row numbers arriving at the scan come from a bitmap the caller built.
  A bitmap from a longer segment stops the scan at the end of the section, which is correct and is also the cheap thing to do, since the bitmap walks ascending.

## What it costs

On an Apple M4, top 20, one query against the whole segment:

| Rows | Dimensions | Per query | Rows a second |
| --- | --- | --- | --- |
| 100 thousand | 128 | 6.3 ms | 15.8 million |
| 100 thousand | 768 | 42.5 ms | 2.4 million |
| 1 million | 128 | 75.2 ms | 13.3 million |
| 1 million | 384 | 201 ms | 5.0 million |

Read that as bytes rather than rows and it is the same number four times: 1.7 to 2.0 GB a second, which is memory bandwidth.
The scan is not compute bound and there is nothing clever left in the inner loop, so the next real gain is scanning fewer bytes rather than scanning them faster.

The same corpus, 100 thousand rows at 768 dimensions, with a permission bitmap:

| Rows the reader may see | Per query |
| --- | --- |
| 100 percent | 45.6 ms |
| 50 percent | 25.6 ms |
| 10 percent | 6.0 ms |
| 1 percent | 0.47 ms |

Walking the bitmap instead of the rows costs about seven percent when the reader may see everything, and it pays that back many times over at every other selectivity.

Building costs 347 thousand vectors a second at 768 dimensions, single threaded, and a query is quantised in 2.7 microseconds.
`go test -bench . ./store/vector/` is where those numbers come from.

One measurement in that path is worth writing down because it went the other way.
The rounding in `round` is `math.Round` rather than the obvious add a half and let the conversion truncate.
The hand written version needs the sign of the value, which is a branch on data that is half negative, and it mispredicts every other component.
Appending is three times faster with the library function, which is branchless bit manipulation.

## What is deliberately not here yet

A graph index, which is the reason `Index` and the kind byte exist.
It becomes worth its build cost somewhere above the sizes in the table above, and the flat scan is the baseline that will say where.

An operator facing setting for the kind.
No storage driver writes segments yet, so there is nothing in the server to configure and a knob that reaches no code would be worse than none.
The choice is a byte in the format and a single method on the builder, so it is a configuration decision the moment there is a configuration to hold it.

SIMD in the inner loop.
The dot product is an eight way unrolled scalar loop with the bounds checks removed, and it already runs at memory bandwidth, so wider instructions would speed up something that is not what is slow.

Scoring several segments in parallel, which is a property of the layer above this one.
A `Query` is quantised once and is safe to hand to every segment, which is why it is a value rather than an argument.
