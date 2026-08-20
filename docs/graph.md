# Graph

A segment holds documents.
This section holds the people, teams, projects and customers those documents are about, the relationships between them, and the walk over both.

Version 1.
The implementation is `store/graph`, and the container it lives inside is [the segment format](segment.md).

## An entity has no permissions of its own

An entity is visible to a reader when that reader may read a document that mentions it.
An edge is visible when they may read a document that says the relationship exists.
That is the whole permission model, and there is not a second one anywhere in this package.

The alternative is an access list per entity, and it is worse in a way that does not show up until it has been wrong for a while.
Two permission models have to be kept in step, they drift, and the failure when they drift is a name shown to somebody who was never meant to learn it.
Here there is nothing to keep in step.
The bitmap that filters documents is the bitmap that filters entities, over the same row numbers, so an entity cannot outlive the reasons a reader had for knowing about it.

It also gives the traversal a property worth stating plainly.
A result is a walk over the subgraph the reader could have built for themselves by reading their own documents and writing down what they said.
Nothing in it came from a document they cannot open.

The rule is total rather than a rule with a hole in it.
`Builder.Entity` refuses an entity with no mentions and `Builder.Edge` refuses an edge with no evidence, both with `ErrMention`, because a thing nobody can see through is invisible to every reader including the one allowed the whole segment.
That is not a useful row, it is a bug in whatever produced it, and it should say so at the call that made it rather than turn into a traversal that quietly stops.
In practice it costs nothing, because a document that evidences a relationship names both ends of it.

## Extraction is the connector's problem

There is no name recogniser here, no regular expression, no model, and no interface for one either.

An interface the store defines is still the store having an opinion about extraction, and it is an opinion the store is in the worst position to hold.
A connector knows what a person is in the system it reads, because that system has user ids.
It says so by calling `Builder.Entity` and `Builder.Edge`, and this package's entire contribution is that whatever it is told survives a round trip and is never shown to somebody who may not see it.

`arch_test.go` holds that: `store/graph` imports `store/column` and nothing else in the tree.
There is no edge to `doc` and none to `connector`.

The key is the connector's problem too.
It has to be stable across segments and unique within one, because it is what a traversal starts from and what two segments agree on when the same person is in both.
An email address, an account id, a URL.
This package requires only that it is not empty and not repeated.

## Entities are rows, in key order

An entity is a row in three columns: its key, its type, and its list of mentions.

The columns are `store/column`, the same ones the documents use, which is what makes the type of an entity a facet for free and the vocabulary of types a dictionary read rather than a scan.
The mentions are a posting list, gap encoded as variable length integers, exactly the shape the term postings have.

The rows are stored in key order rather than in the order a connector declared them, and that buys two things.
Looking an entity up by key becomes a binary search over the rows instead of a scan of them, which matters because every traversal begins with one of those per seed.
And the section stops depending on the order the ingest happened to walk the corpus in, so two ingests of the same documents produce the same bytes.

The cost is that the number `Builder.Entity` hands back is a handle in that builder rather than a row in the finished section, since the order is only known once the last entity is in.
`Builder.Edge` takes the handle, `Build` translates them, and a caller that wants the row asks the section with `Graph.Find`.

## Edges are one contiguous run per entity

The edges are sorted by source, then by kind, then by destination, and stored as an offset array of one `uint32` per entity plus a flat array of one `uint32` per edge.

An entity's edges are therefore a slice, and an entity with no edges is a zero length slice rather than a special case.
The kinds are a column, so the dictionary turns the kind names a traversal asks for into codes once before the walk and filtering by kind is an integer comparison per edge.
The destinations are a plain array rather than a column, because they are read once per edge on the one hot loop of the walk and packing four bytes into three buys less than the unpacking costs.

Direction is the caller's to define.
An edge is stored as given and followed from source to destination, so a relationship worth walking both ways is two edges.
That is cheaper than a flag every traversal has to reason about, and it lets the two directions carry different kinds, which "manages" and "reports to" usually should.

## The header

Fixed size, at the front, version first, the same shape the column and vector formats use.

| Offset | Size | Field |
| --- | --- | --- |
| 0 | 1 | version |
| 1 | 1 | flags, zero in this version |
| 2 | 2 | reserved, zero |
| 4 | 4 | entities |
| 8 | 4 | edges |
| 12 | 4 | document rows |

Then a directory of seven parts, each an offset and a length of four bytes: entity keys, entity types, mentions, adjacency, edge kinds, edge targets, evidence.

A posting part is a count, then that many offsets plus one, then the gaps.
All integers are little endian, for the same reason they are in the segment format.

The parts are required to be contiguous and in order and to end exactly at the end of the section.
Requiring that rather than tolerating gaps is what makes one comparison at the end cover every byte between.

## Reading bytes that may be anything

These bytes come off a disk, so `Open` checks the structure before it believes any of it, and there is a fuzz target whose only contract is that nothing it is fed ever panics.

| Error | What it means |
| --- | --- |
| `ErrVersion` | a section written by something newer, or a flag this build does not know |
| `ErrFormat` | bytes that were damaged or were never a section |
| `ErrEntity` | a key that is empty, repeated, or not in the section |
| `ErrRow` | a document row outside the segment |
| `ErrMention` | an entity or an edge nobody could ever see |
| `ErrDepth` | more hops than `MaxDepth` |
| `ErrLimit` | a negative limit |

The checks that matter:

- Nothing is allocated from a length read out of the bytes.
  The entity and edge counts are bounded first, then turned into the sizes the parts would have to be, and those have to match the bytes actually on hand.
- Every part is checked against the header about how many things it holds.
  A section that paired one entity with another entity's mentions would answer questions wrongly rather than fail, and a wrong answer here is a name shown to the wrong person.
- The adjacency is checked once at open: it has to ascend and to end at the edge count, which is what makes every entity's range a valid slice.
  Every edge destination is bounds checked there too, which is why the walk can use one as an index without checking again.
- A posting list's offsets have to ascend and the last one has to be the length of the data.
- `TestOpenRefusesEveryDamagedByte` flips every byte of a real section two ways and requires that each one either opens or returns an error.

## What a traversal costs

`Traverse` takes seed keys, the kinds to follow, a depth, a limit and the permission bitmap.
It is breadth first, and the bitmap is applied at every hop rather than at the end.
An edge whose evidence the reader cannot read is not crossed, so nothing behind it is reached through it, and an entity with no readable mention is not returned even when an edge led to it.

An entity is expanded at most once, because the walk keeps a bitmap of the ones it has reached, and expanding one reads its run of edges once.
So however deep the traversal goes it reads each edge at most once and each entity at most once, and the worst case is one pass over the section rather than something that grows with depth.
A cycle is not a special case, it is that bitmap doing its job.

The visibility checks are the part that is not constant.
Deciding something is visible stops at the first document the reader may see, so the usual cost is one variable length integer.
Deciding it is not visible reads the whole list, because that is what it takes to be sure.
A reader who may see very little is therefore the expensive case per entity rather than the cheap one, which is the right way round: their answer is the smallest, so paying more per entity still costs less overall.

`Limit` stops the walk rather than trimming the answer, so it bounds the work and not just the output, and what it keeps is the entities nearest the seeds.
`Result.Truncated` says the limit stopped it, and it is set only when there was more to find, not when the last thing there was to find happened to fill the page.

Every part of the order is a property of the section rather than of the ingest or of the order the seeds were listed in, so the same question over the same segment gives the same answer in the same order.

## What it costs

On an Apple M4, one seed, depth 6, following every kind, with no permission filter, so the walk reaches everything it can:

| Documents | Entities | Edges each | Section | Per traversal | Entities reached |
| --- | --- | --- | --- | --- | --- |
| 10 thousand | 2 thousand | 4 | 139 KB | 57 us | 1691 |
| 100 thousand | 20 thousand | 4 | 1.5 MB | 165 us | 4616 |
| 100 thousand | 20 thousand | 16 | 4.0 MB | 2.4 ms | 20000 |
| 1 million | 200 thousand | 4 | 15.2 MB | 190 us | 5388 |

That is 28 million entities a second in the first, second and fourth rows, which is the same number three times over corpora that differ by a hundred times in size.
What a traversal costs is the component it walks, not the segment it walks it in.
The dense row is the one that reaches the entire graph, and it is slower per entity because it also reads sixteen edges for each one.

The same walk with a limit of 20, which is what a search result actually asks for:

| Documents | Per traversal |
| --- | --- |
| 10 thousand | 1.9 us |
| 100 thousand | 2.9 us |
| 100 thousand dense | 2.5 us |
| 1 million | 2.8 us |

Flat, which is the number that says the graph can be on the request path.
A bounded traversal is three microseconds whatever the segment holds, so a request that does one is spending its budget somewhere else.

The 100 thousand document segment again, with a permission bitmap:

| Rows the reader may see | Per traversal | Entities reached |
| --- | --- | --- |
| 100 percent | 233 us | 4616 |
| 50 percent | 6.0 us | 102 |
| 10 percent | 0.6 us | 2 |
| 1 percent | 0.6 us | 1 |

The answer collapses faster than the fraction does, and that is correct rather than a bug.
Reaching an entity six hops out means every edge and every entity along the way was visible, so the odds multiply.
A reader restricted to half the corpus does not get half the graph, they get the part of it their own documents actually support.

Looking an entity up by key is 472 nanoseconds on the 20 thousand entity section, and encoding that section costs 8 milliseconds for 20 thousand entities and 80 thousand edges.
The sort into key order is paid there, once, so that no reader pays for it.
`go test -bench . ./store/graph/` is where those numbers come from.

## What is deliberately not here yet

Entity resolution.
Two segments agree that two entities are the same when their keys are equal, and nothing here merges "Alice Smith" with "asmith@example.com".
That is a decision with a confidence attached to it, which makes it a different kind of thing from anything in this package.

Ranking.
A traversal returns what is reachable, nearest first, and says nothing about which of those is worth showing.
Scoring an entity by how much evidence it has and how close it is belongs to the layer that already scores documents.

Merging graphs across segments.
A traversal walks one segment, and a query that spans a corpus runs one per segment and joins on the keys.
That join is the retrieval layer's, not the store's, and it needs entity resolution first to be worth more than concatenation.

Edge properties.
An edge is a kind, a direction and its evidence.
A weight or a time range would go in a column beside the kinds, which is a version 2 with a flag, not a redesign.
