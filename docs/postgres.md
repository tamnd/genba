# The PostgreSQL driver

`store/pgstore` is a storage driver backed by PostgreSQL 18.
It exists for the shops that already run Postgres and are not going to be talked out of it.

Turning it on is one setting.

```
GENBA_STORE=postgres
GENBA_DSN=postgres://genba@db.example.com:5432/genba?sslmode=verify-full
```

Nothing else in the codebase knows which driver is active.
The API, the ranking, the cache and the browser interface are the same code against SQLite and against Postgres, and the conformance suite in `store/storetest` is the same suite for both.

## The trade

This is the driver to reach for when a customer would rather operate a database they know than a new one.
It is not the one to reach for when the corpus is large or the latency budget is tight.
SQLite with the full text index in the same file is faster on a single node, and it is what the default configuration uses.

Two things follow from Postgres being a server rather than a file, and both are visible in the numbers.
Every query is a round trip, so a search that costs three statements costs three round trips rather than three function calls.
And writes take a lock, which is the next section.

## Writing takes a lock

Every write transaction takes one advisory lock, so two servers ingesting into the same database take turns.

The reason is the corpus and term statistics.
They are maintained rather than derived, which is what turns "how many documents carry this term" from an aggregate over a posting list into a primary key hit, and maintaining them means reading what a document contributed before overwriting it.
That read and the write that follows it are separate statements, and two transactions interleaving them leave the counters wrong by however many writes raced.

The SQLite driver takes the same decision with a mutex.
This is that mutex somewhere every server in a deployment can see it.

What it costs is narrow.
Reads never take it, so a deployment ingesting flat out still serves queries at full speed.
Within one tenant the ingestion pipeline is a single writer sending batches of hundreds, so the lock is held for four bulk statements and contended by nobody.
Making it finer grained means keying the counters per tenant and locking per tenant, which is a change to the schema, and it is not worth making until somebody is actually ingesting two tenants at once into one database.

## Settings

The pool, timeout and retry settings live in the connection string.
They are there rather than in a second configuration block because the whole point of the storage layer is that a deployment picks a driver with one string and nothing else in the process knows which one it got.
A driver that needed six more environment variables would be a driver that leaked into `config`, `cmd` and the documentation for both.

| Key | Default | What it does |
| --- | --- | --- |
| `pool_max_conns` | 16 | The size of the pool. A pool larger than the database has cores turns a queue that is visible in genba into contention that is only visible in Postgres. |
| `pool_min_conns` | 2 | How many connections stay open when nothing is happening, so the first request after an idle period does not pay for a handshake. |
| `pool_max_conn_lifetime` | `1h` | Retires a connection regardless of health, which is what lets a failover or a pooler restart drain through without restarting every server. |
| `pool_max_conn_idle_time` | `30m` | Closes a connection nobody has used. |
| `connect_timeout` | `10` | Bounds one attempt to open a connection. This is libpq's own key, in seconds, and pgx reads it directly. |
| `statement_timeout` | `5000` | The server side limit on a single statement, in milliseconds. This is Postgres's own setting and it is passed straight through. |
| `genba_attempts` | 3 | How many times an operation is tried before its error is returned. One means no retry. |
| `genba_backoff` | `25ms` | The wait before the second attempt. It doubles for each one after that. |

Durations take a unit: `2h`, `5m`, `100ms`.
A bare number is rejected with a message saying what it should have looked like.

The `pool_` and `genba_` keys are read and removed by this driver before pgx sees the string.
Everything else is pgx's or the server's and is passed through untouched, so `sslmode`, `application_name`, `target_session_attrs` and the rest work the way the Postgres documentation says they do.
The libpq keyword shape works too, so an existing connection string does not have to be rewritten to try this driver.

```
host=db.example.com user=genba dbname=genba sslmode=verify-full pool_max_conns=32
```

### About the statement timeout

Five seconds is far beyond the ten millisecond budget the API gate holds queries to, and that is the point.
It is a backstop, not a policy.
The failure it prevents is the bad one: a query that hangs holds a connection out of a pool that has sixteen of them, so one pathological statement becomes an outage of everything else.

A connection string that names its own `statement_timeout` wins, because an operator who set one has a reason.

### About the retries

The list of errors worth retrying is short on purpose.
A constraint violation, a syntax error or a permission denial is a bug or a misconfiguration, and retrying it three times turns one clear error into three of the same error and a slower response.

What is retried is a database that moved: a failover, a pooler that recycled the connection, an administrator restarting the cluster, and the two isolation errors that mean another transaction won a race.
A cancelled or expired context is never retried, because that is the caller's decision.

The one operation that is not retried past its first row is a streaming read.
Once the caller has seen a document, a second attempt would hand it the same one again, which is a worse failure than the error it is trying to avoid.

## Migrations

The schema is SQL files under `store/pgstore/migrations`, embedded in the binary and applied at startup.

The SQLite driver keeps its migrations in a Go slice because that schema is managed entirely by the process that owns the file.
This one is not.
A Postgres database is somebody's cluster, it has a DBA, and the answer to "what is this about to do to my database" has to be a file rather than a build.

```
store/pgstore/migrations/0001_documents.up.sql
store/pgstore/migrations/0001_documents.down.sql
```

The name is the contract: four digits, an underscore, a name, and either `.up.sql` or `.down.sql`.
Versions are contiguous from one.
Anything else in the directory is an error rather than something quietly ignored, because a migration that does not run because somebody typed the name wrong is the kind of bug that is found in production.

Every pending migration and the row that records it are one transaction, taken under an advisory lock.
So two servers starting at once against a fresh database do not both try to create the schema, a migration that fails leaves the database at the version it was at rather than half way into the next one, and starting the second server is the same code path as installing the first.

Applied versions are recorded in `schema_migration`.
A database that has a version this build does not know is refused rather than opened, because an old server carrying on against a newer schema would write rows the new one cannot read.

### Rolling back

`pgstore.Rollback` undoes every migration above a version, newest first.
Version 0 is an empty database.

It is in the driver rather than left to a DBA with a copy of the down files because the version table has to be kept in step with them, and because running the files in the order they appear on disk undoes them in the order they were applied, which is backwards.

A migration that ships no down cannot be undone, and `Rollback` refuses the whole range rather than skipping it.
A partial rollback leaves a schema that matches no version of this code.
Not every change can ship a way back: one that drops a column cannot put the values back, and pretending otherwise in a file called `something.down.sql` is worse than saying so.

## What is in the schema

Seven tables.

| Table | What it holds |
| --- | --- |
| `document` | One row per document: the columns a query filters on, and the tsvector it matches on. |
| `document_data` | The document itself, as JSON. Separate so that a query that filters never reads the text it is not going to return. |
| `document_ref` | The allow and deny lists, as rows, in the key forms `acl` compares. |
| `document_content` | The bytes of a document that is not text. |
| `posting` | Per document term frequencies, keyed by document first. |
| `corpus` | Per tenant document and token counts. |
| `term_stat` | Per tenant document frequency for each term. |

Every table carries a comment explaining the decision behind it, so `\d+` in psql is a readable description of the schema rather than a list of column types.

Two of those decisions are worth repeating here.

The modified date is stored as unix nanoseconds in a `bigint`, so that a range filter is an integer comparison and a document read back compares equal to the one that went in.
PostgreSQL 18's virtual generated columns give an operator a readable `timestamptz` over the same bytes without storing a second copy of it, which is what `modified_utc` is.

The document JSON is `text` rather than `jsonb`.
Nothing queries inside it: the columns beside it are the index, and the JSON is the payload.
Storing it as `jsonb` would pay a parse on every write and a reserialisation on every read to support queries nothing runs, and it would not round trip the document byte for byte.

## Where the permission check happens

In SQL, in the same statement that applies the terms and the filters.

The principal's identity and group keys are bound as text arrays and the allow and deny lists are rows, so the visibility rule is set membership that Postgres evaluates while it walks its own index.
Nothing filters afterwards.
That is what makes a count or a facet computed from those rows safe to show, and it is why there is no second place for the rule to be forgotten.

The rule is not written twice.
The key forms come from `acl` and the fold from `store`, so the strings compared in SQL are the strings `acl.Permissions.Allows` compares in Go, and the conformance suite checks the two agree on a corpus.

Three tests hold the line.
One counts the rows the database handed back, because a driver that asked for everything and filtered in Go would have the same visible behaviour and a row count in the hundreds.
One reads the query plan and checks the deny list, the owner, the mode and the tenant are conditions in it, and that a query for a reader who may see nothing returns no rows at all.
The third is the conformance suite, which runs the same permission cases against every driver.

## The full text index

The tsvector is built in Go from the terms `doc.Tokenize` produced, rather than by handing Postgres the text and letting `to_tsvector` tokenize it a second time.

That matters more than it looks like it does.
A term the Go rule finds and the index does not is a document that `Retrieve` misses and `Scan` finds, which is a difference nobody notices until a customer says a document is missing.
Building the vector here means Postgres never re-tokenizes anything and there is exactly one tokenizer in the system.

Two details follow from doing it that way.

Positions are synthesised from the per field occurrence counts, weighted A for the title and B for the body.
There are no real ones to record, because the analysis keeps counts rather than offsets, and a count is what `ts_rank` actually reads.
Without them a document mentioning a term fifty times would sort the same as one mentioning it once, and the candidate cut would throw away the best document on a single term query.

Terms too long to store are hashed.
Postgres refuses a lexeme over 2046 bytes and the tokenizer has no length cap, so a base64 blob pasted into a wiki page is one multi kilobyte term.
Both the index and the query hash the same way, with a prefix the tokenizer cannot produce, so the match set is still exactly the one `store.Request.Matches` describes.

## Running the tests

There is no in process Postgres, so the driver's tests skip without a server.

```
make pg-server    start a throwaway PostgreSQL 18 in a container
make pg-test      run the driver's tests against it
```

`GENBA_TEST_POSTGRES` is the connection string they read.
Any PostgreSQL 18 will do, including one from Homebrew or a package manager.
Each test gets its own schema in that database and drops it afterwards, so nothing is left behind and two tests never see each other's documents.

CI runs the same suite against a `postgres:18` service container on every pull request, and then checks that the conformance tests reported a pass rather than a skip.
A driver whose suite skipped in CI would be a green tick over nothing.
