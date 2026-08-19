# Baseline

Where the query path stands after the two phase retrieval work, measured against the corpus this package generates.

The point of writing it down is that the next person who changes any of this has something to compare against, and that the numbers we are not happy with are on the record rather than in somebody's memory.

## What was measured

| | |
| --- | --- |
| Corpus | seed 2121, 20,000 documents |
| Queries | `benchcorpus/queries.txt`, cycled per class |
| Machine | Apple M4, 10 cores, macOS 15, `CGO_ENABLED=0` |
| Driver | `modernc.org/sqlite`, which is SQLite transpiled to Go |
| Command | `make bench-search` |
| Recorded | 19 August 2026 |

Read these as orientation, not as a gate.
They were taken on a laptop that was doing other things, and the same commit measured 43ms and 50ms for the same benchmark twenty minutes apart.
That is exactly why the thing CI enforces is `make bench-counters`, which counts rows and statements and cannot flake, and why the latency gate that will sit beside it compares against a baseline recorded on the runner it runs on.

## The endpoints

| Benchmark | Time | Allocations |
| --- | --- | --- |
| `BenchmarkAPIDocument` | 113µs | 122 |
| `BenchmarkAPIStats` | 1.9ms | 63 |
| `BenchmarkAPISearchFilter` | 16.2ms | 21,066 |
| `BenchmarkAPISuggest` | 42.7ms | 16,844 |
| `BenchmarkAPISearch` | 55.4ms | 22,558 |
| `BenchmarkAPIMe` | 86.8ms | 9,996 |

## The searcher

| Benchmark | Time | Allocations |
| --- | --- | --- |
| `BenchmarkSearchRare` | 3.8ms | 14,930 |
| `BenchmarkSearchTermFilter` | 10.5ms | 18,984 |
| `BenchmarkSearchFilterOnly` | 18.5ms | 20,925 |
| `BenchmarkSearchMultiTerm` | 26.6ms | 25,437 |
| `BenchmarkSearchStranger` | 37.6ms | 23,318 |
| `BenchmarkSearchByRecency` | 43.3ms | 50,844 |
| `BenchmarkSearchCommonTerm` | 50.4ms | 22,425 |
| `BenchmarkSearchDeepPage` | 57.0ms | 71,934 |
| `BenchmarkSearchPathological` | 128.9ms | 65,853 |

## The driver

| Benchmark | Time | Allocations |
| --- | --- | --- |
| `BenchmarkStatistics` | 31µs | 84 |
| `BenchmarkGetByIDs` | 910µs | 674 |
| `BenchmarkRankFilterOnly` | 12.5ms | 5,097 |
| `BenchmarkRankMultiTerm` | 20.8ms | 14,137 |
| `BenchmarkRank` | 45.0ms | 13,723 |
| `BenchmarkPutBatch` | 510 documents/s | 2,670 per document |

## Where the time goes

Every number above is explained by one sentence: the cost of a query is proportional to the number of documents the asker may read that match it, and to nothing else.

A rare term matches a handful of documents and costs 3.8ms whether the corpus holds five thousand documents or twenty thousand.
A common term in this corpus matches four thousand and is visible in three thousand, and costs 50ms.
The pathological class is the term most of the corpus carries, and it costs 129ms.
Fetching a page of twenty documents by id costs 910µs and does not move at all, because it is twenty primary key lookups.

Two of the three statements a search runs walk the visible match set.
The candidate cut walks it to find the best five hundred, and the counts statement walks it again to produce the total and the four facet lists.
Timed separately on this corpus, with the C build of SQLite so the driver is out of the picture, the candidate cut is 6ms and the counts statement is 7ms for a match set of three thousand.
Through the Go driver each is roughly one and a half times that, and the rest of the difference between 13ms and 50ms is the classes with larger match sets that the benchmark averages over.

`BenchmarkAPIMe` is the worst number here and it is the first request the interface makes.
It runs a search with no terms and no filters, so the match set is every document the reader may see, which is fifteen thousand of the twenty thousand.
It then throws away everything except the source and kind facet lists, which are the same on every page load and change only when somebody indexes a document.
That is a cache, and it is the next piece of work.

## What is not met yet

The budgets are in the measurement spec and three of them are missed.

**Search under ten milliseconds.** Met for a rare term and for a term with a filter, missed for a common term by a factor of five. Closing it needs the counts statement to stop being a second walk of the match set, and needs the repeated queries that a real workload is mostly made of to be served from a cache.

**Two thousand documents indexed per second.** Measured at 510. A document averages three hundred and thirty distinct terms, and each one costs a posting insert and a term statistic upsert, so a document is about six hundred and sixty statements. Batching those is the obvious fix and it has not been done.

**Index overhead under sixty percent of the corpus.** Measured at a hundred and forty three percent. The raw document JSON is 119MB, and on top of it sit 131MB of postings, 35MB of full text index and 4MB of everything else. The posting table stores the term as text on every row, so a term appearing in eight thousand documents is stored eight thousand times. A term dictionary with integer ids would take most of that back.

None of these are surprises and none of them are hidden.
They are written here so that the next change to this code starts from a number rather than from a guess.
