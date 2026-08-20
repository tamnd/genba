# Durability

A segment is immutable once it is written, so the only mutable thing in an index is the answer to the question "which segments are there".
This is how that answer is stored, how it is changed, and what happens to it when the process dies in the middle of changing it.

Version 1.
The implementation is `store/segdir`, and the files it manages are [the segment format](segment.md).

## A crash is a correctness problem, not a durability one

Losing the last hour of a crawl to a power cut is annoying, and the fix is to crawl again.
Coming back with half a segment is a different kind of problem, because half a segment is half an access list.

A document whose permissions were written and whose content was not is a document with no readers.
That is merely wrong, and somebody notices when their own file stops turning up.
A document whose content was written and whose permissions were not is a document with every reader.
Nobody notices that one, and it is the thing this package exists to make impossible.

So the rule is not that writes are durable.
The rule is that a segment is either wholly visible or wholly invisible, at every instant, to every reader, including the reader that opens the directory after the machine came back.

## One rename is the whole design

A publish writes the segment to a temporary name, makes it durable, renames it into place, and only then rewrites the manifest, also through a temporary name and a rename.
A rename over an existing file is atomic on every filesystem worth running on, so the manifest is at every instant either exactly the old set or exactly the new one.
There is no third state, however the write was interrupted.

The manifest is written whole rather than appended to, and that is the decision worth defending.
A log of changes is the usual answer and it is cheaper to append to, but it has a torn tail after a crash.
A torn tail means a reader has to decide how much of the tail to believe, and that decision is where the bug lives.
A file replaced by a rename has no torn state to have an opinion about: the old one is complete, the new one is complete, and there is nothing else to write code for.

What it costs is rewriting sixteen bytes per live segment on every publish.
On a ten thousand segment index that is a 160 KB write next to a segment that is measured in megabytes, and the benchmark below says it does not show up until the index is very large indeed.

## Recovery is not a mode

There is no clean shutdown flag, and there is no fast path that skips the checks when the last run exited politely.
Every open sweeps the directory and verifies the live set, whether the last process was killed or not.

A path taken only after a crash is a path exercised only after a crash, which is to say a path that is broken and nobody knows.
Making the ordinary start take the recovery path means the recovery path runs thousands of times a day in development and in tests, and the first crash in production is the ten thousandth time it has run rather than the first.

Recovery is the publish rule read backwards.
Whatever the manifest names is what exists.
Everything else in the directory is deleted, because every temporary file is a publish that was interrupted and every unnamed segment is either the same thing one step further on or a segment removed before the manifest could be rewritten.
Nothing has ever been able to read either of them, so both are safe to delete, and deleting them is what keeps a directory that has crashed a thousand times the same size as one that never has.

A manifest that names a segment which is not there is an error, not a smaller index.
Quietly serving the segments that did survive is how a crash turns into a silent partial index, and a search that returns fewer answers looks exactly like a search that had fewer answers to give.
That is the failure that takes a month to notice, so it is refused loudly instead.

## The fsync policy

Three settings, and the zero value is the safe one.
A caller who has not thought about this gets the one that survives a power cut, and the weaker two have to be spelled out by somebody who has read what they give up.

| Policy | Flushes | Survives a kill | Survives a power cut |
| --- | --- | --- | --- |
| `SyncFull` | the segment, the manifest and the directory | yes | yes |
| `SyncManifest` | the manifest and the directory | yes | no |
| `SyncNone` | nothing | yes | no |

`SyncManifest` is the interesting one.
The segment's pages belong to the operating system by the time the process dies, so a kill cannot lose them, and a power cut can.
When it does, the manifest reaches the platter naming a segment whose bytes did not, which shows up at the next open as a segment that fails its size check or its verification.
That is an error rather than a smaller index, so the failure is loud, and it is a reasonable setting for an index that can be rebuilt from its sources and a bad one for anything else.

`Options.Verify` is separate from the sync policy.
Off, an open stats every live segment and compares the length to what the manifest recorded, which catches the two things a crash produces: a file that is missing and a file that is short.
On, it reads every segment, parses it and checks that the header holds the sequence the file name claims.
It is worth turning on when the hardware is suspect, because a bad sector produces a file of exactly the right length full of something else, and nothing but reading it finds that.

## The test kills a real process

`TestKillingTheProcessAtEveryWritePointRecovers` runs a child at every instant a publish can be interrupted at, kills it there, and then opens what it left behind.
Publish has six such instants and remove has three, and the test does not have those numbers written down anywhere: it keeps raising the crash point until the child survives, so adding a write to either path adds a case without anybody remembering to update a constant.

It is a subprocess rather than a simulation because the thing under test is what the operating system does with writes that were in flight.
A fake that decided which of them survived would be a test of the fake.
The child dies with `os.Exit`, which runs no deferred function and flushes nothing, which is what a kill or a panic in another goroutine does.

After each kill the directory has to satisfy all of this: it opens with verification on, everything published before the interrupted operation is still there and reads back as itself, the interrupted operation either wholly happened or wholly did not, the file count is exactly the live set plus the manifest, and a further publish and reopen work.
That last one matters as much as the rest.
A crash should leave something to carry on from rather than something to rebuild.

The test was checked by breaking the code on purpose.
Reordering a publish so the manifest is written before the segment rename makes it fail at two of the six crash points with "a published segment is missing or damaged", which is the ordering bug it exists to catch.

What it cannot test is a power cut.
The pages the child wrote and did not flush are the operating system's by the time it dies, so they arrive on disk afterwards, and only pulling the plug tells you whether an fsync was really called.
That gap is the reason `SyncFull` is the default rather than something to reach for.
The test proves the ordering is right, and fsync is what makes the ordering mean anything on a machine that loses power.

## What recovery costs

On an Apple M4, opening a directory with no crash to clean up, which is also what a clean start pays:

| Segments | Open | Open with `Verify` |
| --- | --- | --- |
| 100 | 325 us | 2.0 ms |
| 1 thousand | 3.5 ms | 20.8 ms |
| 10 thousand | 36.5 ms | 237 ms |

Linear in the number of segments and independent of what they hold, because a plain open reads the manifest, lists the directory and stats each live file.
Ten thousand segments is a large index, and it is back in under forty milliseconds.
Verification is six times that, which is the cost of reading the whole index rather than looking at it.

The same open on a directory left in a mess, with five hundred interrupted publishes to sweep:

| Segments | Open after a crash |
| --- | --- |
| 100 | 24.7 ms |
| 1 thousand | 29.0 ms |
| 10 thousand | 55.0 ms |

The extra is the unlinking, and it is charged once because the rubbish is gone afterwards.
A machine that has crashed repeatedly does not accumulate a recovery bill.

The write side, with the flushes turned off, because with them on this measures the disk rather than the design:

| Live segments | Per publish |
| --- | --- |
| 10 | 291 us |
| 100 | 282 us |
| 1 thousand | 298 us |
| 10 thousand | 499 us |

Flat until ten thousand, where rewriting the manifest whole finally starts to show, and even there it is half a millisecond.
That is the number the whole design trades for, and it says the trade was cheap.
`go test -bench . ./store/segdir/` is where these come from.

## What is deliberately not here

A write ahead log.
A publish is durable when it returns and not before, and a crawl interrupted halfway through a batch redoes the batch.
That is the right trade for a search index, where the source of truth is somebody else's system and everything here can be rebuilt from it.
A log would buy back the last few seconds of an operation that is already idempotent, in exchange for the torn tail this design does not have.

A lock.
One writer at a time is assumed and not enforced.
Two processes publishing into the same directory will produce a manifest that names segments one of them has never heard of, and the fix is not to do that.
A lock file is a small thing to add when there is a second writer to protect against, and inventing one before then would be guessing at what it should do when the lock holder dies.

Compaction.
`Publish` and `Remove` are enough to express a merge, and deciding which segments to merge and when is a policy that needs to see the query load.
That belongs above this package, which only has to make the swap safe.

Multiple directories.
An index is one directory here.
Spreading segments across volumes is a thing to want eventually, and it changes the manifest from a list of sequences to a list of locations, which is a version 2 with a flag rather than a redesign.
