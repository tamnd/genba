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
That is exactly why the thing CI enforces is `make bench-counters`, which counts rows and statements and cannot flake, and why the latency gate that sits beside it compares against a baseline recorded on the runner it runs on.

## The latency gate

`make bench-gate` is the wall clock half, and `benchcorpus/baseline.json` is what it compares against.

It drives the search endpoint rather than the searcher, because the budget is what a browser waits for and request parsing and JSON encoding are part of that wait.
It warms every class, then measures them all eight times over, and reports p50, p95 and p99 for each.

What it compares was picked by measuring the same commit twice rather than by reasoning about it.
Two runs of an unchanged tree, on the same laptop, disagreed by a factor of two on the p95 of the most expensive class.
The median of the quietest round of those same runs held to within a quarter, and to within a tenth for half the classes.
So the number that fails a build is the quietest median, the tolerance is set above what an unchanged tree was measured to do rather than at a number that sounds strict, and the percentiles are recorded and printed and never compared.

That limit is worth stating plainly: a regression that only moves the tail will not fail this gate.
Nothing that runs on a shared machine can catch that, and a check that pretends otherwise goes red on a Tuesday for no reason and is deleted on the Wednesday.

Four things keep it honest:

- It compares against a baseline rather than against an absolute number, and allows a thirty five percent move.
- It measures the classes in interleaved rounds rather than one class at a time, so a burst of load lands across all of them rather than in whichever one was unlucky, and every round runs the same queries in the same order.
- It times a calibration workload that does not touch the search path, between every round rather than once at the start, and scales the comparison by the median of those readings.
  A runner half the speed of the one that recorded the baseline shows up in the calibration rather than in every class, and load that arrives halfway through a run shows up there too.
  The workload allocates and sorts its way through a few megabytes rather than hashing a buffer that fits in cache, because the first version did the latter and reported the machine getting slower across two runs where every search class got faster.
  It is a proxy, so it is applied only once the two figures are a tenth apart, and below that the run is compared unscaled rather than corrected by the proxy's own noise.
- It refuses to compare at all when the calibration is more than twice apart, or when the corpus differs, and says so instead of passing quietly.

The absolute budgets are still in the code, as a backstop at twice the budget.
A class that the recorded baseline itself misses is exempt from that backstop and is held to the baseline instead, which is what keeps the three misses below from failing every pull request while they are being worked on.
The exemption ends by itself on the day a baseline inside the budget is recorded.

A machine with no baseline at all gets no backstop either.
The exemption is read out of the baseline, so without one every class that is aiming at a budget it has not met yet looks like a failure, and the budgets were stated against the laptop in the table above rather than against a two core runner that is several times slower than it.
An absolute millisecond applied to a machine nothing has characterised is the flake the rest of this design exists to avoid, so that run records its numbers, says in the log that it enforced nothing, and leaves the counters to be the half that runs everywhere.

The checked in baseline is the laptop in the table above.
CI points `GENBA_GATE_BASELINE` at `benchcorpus/baseline-ci.json`, which the nightly workflow produces as an artifact for somebody to commit, because a baseline that updates itself is a ratchet that only turns one way.

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

`BenchmarkAPIMe` was the worst number here and it is the first request the interface makes.
The 86.8ms in the table above is what it cost when it ran a search with a limit of one to get two facet lists out of it.
That is not a small search: the candidate pool has a floor of five hundred, so it asked the driver for five hundred documents, scored all five hundred, sorted them, took a page of one, fetched that one and used none of it.

It now asks for the counts and no candidates, which is #141.
Measured on the twenty thousand document corpus, eight runs of each on the same loaded machine in the same half hour, taking the minimum because the machine is shared and the minimum is the one least contended for:

| | Time | Bytes | Allocations |
| --- | --- | --- | --- |
| through a search | 502ms | 491KB | 10,015 |
| counts and no candidates | 347ms | 60KB | 1,038 |

The times are that machine and not this table's laptop, so read the ratio and not the number.
At the driver, again minimum of three, the same selection with the pool costs 846ms and without it 316ms.

What is left on this path is a count over every visible document that nobody reads, and four facet groupings where two are used.
Both are smaller than what was removed and neither is free.

## What is not met yet

The budgets are in the measurement spec and three of them are missed.

**Search under ten milliseconds.** Met for a rare term and for a term with a filter, missed for a common term by a factor of five. Closing it needs the counts statement to stop being a second walk of the match set, and needs the repeated queries that a real workload is mostly made of to be served from a cache.

**Two thousand documents indexed per second.** Measured at 510. A document averages three hundred and thirty distinct terms, and each one costs a posting insert and a term statistic upsert, so a document is about six hundred and sixty statements. Batching those is the obvious fix and it has not been done.

**Index overhead under sixty percent of the corpus.** Measured at a hundred and forty three percent. The raw document JSON is 119MB, and on top of it sit 131MB of postings, 35MB of full text index and 4MB of everything else. The posting table stores the term as text on every row, so a term appearing in eight thousand documents is stored eight thousand times. A term dictionary with integer ids would take most of that back.

None of these are surprises and none of them are hidden.
They are written here so that the next change to this code starts from a number rather than from a guess.
