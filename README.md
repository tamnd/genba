# genba

[![ci](https://github.com/tamnd/genba/actions/workflows/ci.yml/badge.svg)](https://github.com/tamnd/genba/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tamnd/genba.svg)](https://pkg.go.dev/github.com/tamnd/genba)
[![Go Report Card](https://goreportcard.com/badge/github.com/tamnd/genba)](https://goreportcard.com/report/github.com/tamnd/genba)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

genba is an enterprise knowledge intelligence platform.
It indexes the tools a company already runs, answers questions over what it finds, and never shows anybody a document they were not already allowed to open.

The name is 現場, the Japanese word for the place where the work actually happens.
That is the point of the project.
The answer to a question is usually already written down somewhere, in a document nobody remembers, in a thread from eight months ago, in a ticket that was closed with a one line comment.
Finding it should not depend on knowing who to ask.

## What it does

- **Search across every source.** One query goes to documents, wikis, chat, tickets, code, email and calendars, and comes back as one ranked list rather than as ten tabs.
- **Answers with citations.** An assistant that reads the same corpus, answers in prose, and links every claim back to the document it came from.
- **Agents.** Long running work that reads the corpus and takes an action, with the same permissions as the person who started it and never more.
- **Knowledge management.** Curated answers, verification, expertise and ownership, so the good document wins over the stale one.
- **Platform APIs.** Everything the interface does is available over HTTP, so search can be embedded wherever people already are.

## Permissions come first

This is the part that decides whether a product like this can be deployed at all, so it is the part that was built first.

- Every call that can return content takes a principal.
  Passing nil is a programming error, not a way to search anonymously.
- A deny always beats an allow, because several of the systems worth indexing model permissions that way and inverting the precedence turns a private document into a search result.
- A permission that failed to resolve is not a permission.
  Those documents are held out of every query path instead of being indexed with a guess, and the count of them is a number an operator can watch.
- The filter runs inside the storage driver, while it walks its own data.
  Nothing above storage is trusted to filter, and `store/storetest` fails any driver that hands documents up and expects somebody else to do it.
- A document you may not read and a document that does not exist produce the same error, the same status code and the same response body.
  A caller who can tell those apart can use the difference to prove a document exists.

## Install

```
go install github.com/tamnd/genba/cmd/genbad@latest
go install github.com/tamnd/genba/cmd/genba@latest
```

Prebuilt binaries, Linux packages and a container image are attached to every release.
Homebrew and Scoop entries are published for tagged versions.

```
docker run --rm -p 8080:8080 ghcr.io/tamnd/genba:latest
```

## Quick start

Start a server.
With no configuration it keeps everything in memory and listens on localhost, which is the right default for the first five minutes.

```
genbad
```

That server is empty, which makes it hard to judge.
Point it at a directory you already have and it indexes it before it starts listening:

```
genbad -tenant acme -corpus ~/src/some-repo -corpus-name repo
```

An in memory index is gone when the process is, which gets old quickly once there is anything worth indexing.
Give it a file instead and the same command keeps its work:

```
genbad -tenant acme -store sqlite -dsn ~/.genba/genba.db -corpus ~/src/some-repo -corpus-name repo
```

Then query it:

```
export GENBA_SUBJECT=u_mei
export GENBA_TENANT=acme
export GENBA_GROUPS=gdrive:eng@acme.com
genba search payments failover runbook
```

The browser interface is at http://127.0.0.1:8080 and is compiled into the binary, so there is no static directory to deploy alongside it.

## The interface

One box takes everything.
Text, an operator, or the name of a document you already know, and the box works out which of those it was rather than making you pick a mode first.

| Operator | Example | What it does |
| --- | --- | --- |
| `app:` or `source:` | `app:slack` | only documents from one connector |
| `type:` or `kind:` | `type:ticket` | only one kind of document |
| `in:` or `container:` | `in:incidents` | a space, folder, channel or repository |
| `from:`, `by:` or `author:` | `from:mei` | written by a person |
| `owner:` | `owner:mei@acme.com` | owned by a person |
| `updated:` | `updated:week`, `updated:2026-01-01..2026-03-31` | changed inside a window |
| `sort:` | `sort:recent` | newest first instead of most relevant |

Repeating an operator widens and combining different ones narrows, which is the same rule the facet sidebar follows.
Ticking a box in the sidebar and typing the operator produce the same query, so learning one is learning the other.
Anything the grammar does not recognise is treated as text, because a colon in a sentence is far more common than a typo in an operator.

Every filter, the sort, the page and the open document live in the address bar, so a search can be linked, bookmarked and reloaded, and the back button does what a back button should.

`⌘K` or `/` focuses the box, `j` and `k` walk the results, `Enter` or `p` opens a preview, `o` opens the document in its source, `g` then `h` goes home, and `?` lists all of it.
`j` and `k` keep working inside the preview, so reading through five candidates is five keystrokes rather than five open and close cycles, and the one below is fetched while the current one is being read.
The words that were searched for are marked in the body with a count beside them, and `n` and `shift` `n` walk between them.

The identity switcher at the bottom of the rail sends a different subject, tenant and set of groups with every request.
It is there because the permission model is the part of this system worth checking by hand, and the fastest way to check it is to run the same query as two different people and watch the results change.

The administration screen reports what every connector is doing, what the corpus holds, which documents are being held back and why, and what one named person can see.
It is also the one screen that writes: a connector is added, switched off, asked to sync and removed from there, and the change takes effect without a restart.
A connector that was named on the command line is on that screen because it is running, and says where it is configured rather than offering a button that cannot work.

The whole interface is hand written HTML, CSS and ES modules, committed exactly as they are served, so a clone builds a working interface with the Go toolchain and nothing else.
There is no bundler and there is not going to be one, since the graph is thirty five modules of our own with no third party dependency anywhere in it.
What a bundler would have bought is done by the server instead.
Every file is hashed as it is read and served under a second name that says what is in it, cacheable for a year, and the document is rewritten on the way out so that its import map and its preload list point at those names.
Bodies are compressed with brotli and gzip once at startup rather than once per request.
`web/graph_test.go` walks the import graph on every build, so a module added without a preload fails the build instead of costing every visitor a round trip.

## HTTP API

Everything the interface does is an HTTP call, and there is nothing it can reach that a client cannot.

| Endpoint | What it returns |
| --- | --- |
| `GET /api/v1/search` | ranked hits, facet counts, the total and the server side timing |
| `GET /api/v1/suggest` | operator completions and documents matching a prefix |
| `GET /api/v1/documents/{id}` | one document, or the same error as one that does not exist |
| `POST /api/v1/documents/{id}/verify` | records that the caller vouches for a document, with an optional note and expiry |
| `DELETE /api/v1/documents/{id}/verify` | withdraws the claim |
| `PUT /api/v1/documents/{id}/owner` | corrects who owns a document, when the connector named the account that imported it |
| `DELETE /api/v1/documents/{id}/owner` | puts back the owner the source reports |
| `POST /api/v1/documents/{id}/stale` | records that the caller says a document is out of date, with an optional note |
| `DELETE /api/v1/documents/{id}/stale` | clears the reports, which the owner or the author may do |
| `GET /api/v1/reported` | the documents the caller owns or wrote that somebody has reported, most recent first |
| `GET /api/v1/me` | the caller, and the sources and kinds that caller can actually see |
| `GET /api/v1/stats` | how much is indexed and how much is quarantined |
| `GET /api/v1/admin/operations` | what the connectors are doing, and what is being held back and why |
| `GET /api/v1/admin/access` | whether one named person can read one document, and why |
| `GET /api/v1/admin/answers` | the questions this tenant has written an answer to, most recently written first |
| `PUT /api/v1/admin/answers/{id}` | writes an answer to a question, or replaces the one that is there |
| `DELETE /api/v1/admin/answers/{id}` | takes an answer down |
| `POST /api/v1/admin/connectors` | adds a connector, or replaces one, and starts it |
| `DELETE /api/v1/admin/connectors/{source}` | stops a connector and forgets how it was configured |
| `POST /api/v1/admin/connectors/{source}/start` | switches one back on |
| `POST /api/v1/admin/connectors/{source}/stop` | switches one off and keeps its settings |
| `POST /api/v1/admin/connectors/{source}/sync` | asks a running connector for a run now |
| `GET /healthz`, `GET /readyz` | liveness, and whether the store answers |

`search` takes `q` for the text and the operators, and `source`, `kind`, `container`, `author` and `owner` as repeated or comma separated parameters, plus `since`, `until`, `sort`, `limit` and `offset`.
The snippet comes back as marked passages rather than as offsets, so a client highlights what the analyzer matched without reimplementing the analyzer.

A verification is a named claim with a date on it rather than a flag, so search results and the document itself carry who vouched for it, when, and when that stops counting.
It lasts six months unless the verifier says otherwise, and only the owner or the author of a document can make one, because a badge anybody can apply is a badge that means somebody read the title.
A driver that cannot record one leaves the badge off rather than failing the search behind it.

A question people ask often enough is worth answering once, so an administrator can write the answer down and it stands above the results for everybody in the tenant.
It carries the name of whoever wrote it, the date they last stood behind it and the documents they drew it from, and it takes the place of the quoted passages rather than sitting beside them, because that region is the answer to the question in the box and two answers to one question is a reader deciding which of ours to believe.
The question is matched whole, over the phrasing it was filed under and any others its author listed, so a search that is close but not the same gets the ordinary results page it would have got before this existed.
Its sources are resolved through the reader asking, so an answer written by somebody who can read everything never becomes a list of documents the reader in front of it cannot open.

Ownership is derived from the source, and what a source derives is very often the account that ran the import, so it can be corrected.
The correction carries the name of the person who made it and the date, it survives every crawl after it, and clearing it puts back whatever the connector reports today rather than what it reported the day somebody disagreed.
It is the same rule as a verification and deliberately so, because being the owner is what makes somebody able to vouch for a document.

Every `admin` endpoint needs the `admin` role, which comes from `X-Genba-Roles` or from `GENBA_ADMINS`, and the default is that nobody has it.
The role grants nothing over documents and it must not start to.
Both the reads and the refusals are logged with the subject.

`admin/access` takes `subject`, `groups` and `identities` for the person being asked about, `id` to ask about one document, and `counts=1` to also count what that person can reach.
It answers as that person, by the same rule a search applies, and it never returns a document or a title.
A yes about a document says which clause admitted them and which group or account it matched; a no says nothing further, because the difference between held back, another tenant, not on the list and not there would prove a document exists.
The counts are asked for rather than always returned because they are an aggregate over every document in the tenant, and the log line names both the operator and the person they asked about.

The `connectors` endpoints take a source name, a kind of `corpus` or `bucket`, and the settings for that kind as a JSON object, and they answer with the same connector list `admin/operations` returns.
Settings that cannot be run are refused before they are written down, so a directory that is not there is a message about that directory rather than a connector that fails in a log line a minute later.
The configuration is kept in the store, which is what lets three servers behind a load balancer agree on what is being crawled, and it needs a driver that can hold it: `sqlite` and `postgres` can, `memory` forgets it at the next restart, and a deployment whose driver cannot is told so by `manageable` on the same response.
Removing a connector forgets how a corpus was read and leaves the corpus, because the two are different decisions and a full crawl is an expensive undo.
Credentials are not part of any of this and are never stored: a bucket added here uses the keys the process already has in its environment.
A connector named on the command line is on the screen because it is running and cannot be changed here, since the next restart would read the command line again and undo whatever was typed.
`sync` returns as soon as the run has been asked for, because a crawl of a real source takes minutes and a request that waited for one would time out in the middle of it.

By default every file in the corpus is readable by everybody in the tenant, which is the right rule for a public checkout and the wrong one for almost anything else.
If the tree has OWNERS files in it, `-corpus-acl owners` reads them instead, and a query then returns different results depending on who is asking.
Paths that no OWNERS file governs are quarantined rather than published, and the count of them is on the sync log line.

If the tree is the file server, `-corpus-acl os` reads the permissions the operating system already keeps on it: the owner, the group and the mode bits on Unix, the POSIX access control list where a file carries one, and the security descriptor on Windows.
It needs `-corpus-identity` to say which identity source the account names belong to, so that somebody who signed in through the company directory matches a list that came out of a password file.
A world readable file grants nothing until `-corpus-domain` names the domain the accounts on the host belong to, because a host's accounts are not a tenant.
Point this at a copy of a file server rather than the file server and you get the permissions the copy has, which are the ones the crawler runs as, so do not.
A `chmod` reaches the index on the next sync without the file being read again.

## Configuration

Every setting has a flag and an environment variable.
The environment variable is the flag in upper case with a `GENBA_` prefix, and a flag wins over it.

| Variable | Default | What it does |
| --- | --- | --- |
| `GENBA_ADDR` | `127.0.0.1:8080` | listen address |
| `GENBA_METRICS_ADDR` | empty | listen address for the metrics endpoint, empty to serve none |
| `GENBA_STORE` | `memory` | storage driver: `memory`, `sqlite`, `postgres` or `kura` |
| `GENBA_DSN` | empty | path or connection string for the driver |
| `GENBA_TENANT` | empty | tenant served by a single tenant deployment |
| `GENBA_ADMINS` | empty | subjects that hold the administrator role, comma separated |
| `GENBA_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `GENBA_READ_TIMEOUT` | `30s` | request read timeout |
| `GENBA_WRITE_TIMEOUT` | `60s` | request write timeout |
| `GENBA_SHUTDOWN_GRACE` | `15s` | how long a shutdown waits for in flight requests |

The source flags have no environment variables, because a directory or a bucket to index is a thing you type once while trying the binary out rather than a property of a deployment.
The one exception is the object storage credentials, which are read from the environment and have no flag at all.

| Flag | Default | What it does |
| --- | --- | --- |
| `-corpus` | empty | directory to index at startup |
| `-corpus-name` | `files` | source name the documents carry, and what `-source` filters on |
| `-corpus-acl` | `tenant` | who may read it: `tenant` for everybody in the tenant, `owners` to read OWNERS files, `os` to read the file system's own permissions |
| `-corpus-identity` | `unix` | identity source the account names in the tree belong to, for `-corpus-acl os` |
| `-corpus-domain` | empty | domain the accounts on this host belong to, for `-corpus-acl os`, empty to grant nothing on the world bit |
| `-corpus-refresh` | `0` | how often to sync again, zero for once at startup |
| `-corpus-watch` | `false` | ask the operating system what changed instead of walking the tree, needs `-corpus-refresh` |
| `-corpus-reconcile` | `0` | how often to sweep the index against the tree, zero for after every sync |

`-corpus-watch` is what makes a short refresh interval affordable on a large tree.
Without it every refresh walks, which is a stat of every file to find the four that moved, and with it the cost of a refresh is a function of how much changed rather than of how large the corpus is.
A machine that cannot give out that many watches logs a line and carries on walking, so it is safe to set and never a reason for the server not to start.

`-corpus-reconcile` exists because of it.
The sweep that finds deleted files walks the tree, so on a watched server it is the whole remaining cost of a refresh, and separating the two lets a change be noticed in a second while both sides are still counted every few minutes.

```
genbad -tenant acme -corpus ~/src/handbook -corpus-refresh 1s -corpus-watch -corpus-reconcile 5m
```

The same server can read an S3 compatible bucket, either instead of a directory or as well as one.

| Flag | Default | What it does |
| --- | --- | --- |
| `-bucket` | empty | bucket to index at startup |
| `-bucket-endpoint` | empty | base URL of the service, for example `https://s3.eu-west-1.amazonaws.com` |
| `-bucket-region` | `us-east-1` | region the bucket is in, which is part of what a signature authenticates |
| `-bucket-prefix` | empty | read only the keys under this prefix, empty for the whole bucket |
| `-bucket-name` | `objects` | source name the documents carry, and what `-source` filters on |
| `-bucket-acl` | `tenant` | who may read it: `tenant` for everybody in the tenant, `bucket` for the bucket's own access control list, `object` for each object's |
| `-bucket-identity` | empty | identity source the names in the access control lists belong to, for `-bucket-acl bucket` or `object` |
| `-bucket-domain` | empty | mail domain that counts as this tenant in a grant written against an address |
| `-bucket-path-style` | `false` | put the bucket in the path rather than in the host name, which MinIO and Ceph need |
| `-bucket-refresh` | `0` | how often to list the bucket again, zero for once at startup |
| `-bucket-reconcile` | `0` | how often to sweep the index against the bucket, zero for after every sync |
| `-bucket-rate` | `5` | requests per second the crawl keeps itself under |
| `-bucket-burst` | `10` | how many requests may go out back to back before the rate binds |
| `-bucket-retries` | `4` | how many times to try a refused request again, negative for never |

Credentials come from `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_SESSION_TOKEN`, and there is no flag for them.
A secret in argv is readable by every process on the machine for as long as the server runs, and it ends up in the shell history of whoever started it.
A bucket with no credentials at all is read unsigned, which is what a public bucket wants and what nothing else does.

```
genbad -tenant acme \
  -bucket company-docs \
  -bucket-endpoint https://s3.eu-west-1.amazonaws.com \
  -bucket-region eu-west-1 \
  -bucket-prefix handbook/ \
  -bucket-acl bucket \
  -bucket-identity google \
  -bucket-domain acme.com \
  -bucket-refresh 30s \
  -bucket-reconcile 15m
```

A server given both a corpus and a bucket runs them as two feeds with two cursors rather than one merged crawl, so a bucket that is refusing requests does not stop the directory being reindexed.
`docs/ingestion.md` has the worked examples for both, including MinIO on a laptop.

Every request the bucket makes goes out through one rate limiter, and there is no value of `-bucket-rate` meaning unlimited.
A crawler that ignores a service's limits gets the credentials revoked, and that is a worse outcome than a slow crawl by a wide margin: a slow crawl finishes late, and a revoked key is an index that stops updating until somebody has a conversation about it.
Anybody who knows their quota can set a rate high enough that it never binds, which is a number in the log rather than a special case in the code.
A refusal that says to come back later is waited out rather than failed, with the wait doubling up to half a minute and the source's own `Retry-After` honoured over anything computed, and a source that has been refusing everything stops the sync rather than being retried all afternoon.
The `bucket synced` line carries `retries`, `throttled`, `throttled_for` and `quota_pauses`, because a crawl that is being throttled looks exactly like a crawl that is slow and the difference decides whether you go looking at the network or ask for more quota.

## Metrics

Set `GENBA_METRICS_ADDR` and the process opens a second listener that serves the Prometheus text format at any path on it.

```
GENBA_METRICS_ADDR=127.0.0.1:9100 genbad
curl -s http://127.0.0.1:9100/metrics | head
```

It is a second listener rather than a route on the API on purpose.
What it publishes is not secret and is not public either: it says how much traffic there is, how large the match sets are and how hard the caches are working.
The deployment that gets this right binds it somewhere the outside cannot reach, and the API address never serves it.

| Metric | What it is |
| --- | --- |
| `genba_request_duration_milliseconds` | histogram per endpoint, labelled with the route rather than the path |
| `genba_search_duration_milliseconds` | histogram of the search itself, without request parsing or encoding |
| `genba_search_candidates` | how many documents were ranked to produce one page |
| `genba_search_matches` | how many matched, before paging |
| `genba_cache_hits_total` | per layer, alongside misses, evictions and the entry count |
| `genba_store_rows_total` | rows the driver returned, alongside statements and decodes |

The buckets are 1, 2, 5, 10, 25, 50, 100, 250 and 500 milliseconds, which are tighter at the bottom than a default histogram because the question here is what fraction of requests came back in under ten milliseconds.

Candidates against matches is the pair worth putting on a dashboard.
A healthy two phase search has a candidate count bounded by the pool and a match count bounded by nothing, and the day the two start moving together is the day the first phase stopped cutting.

[docs/alerts.yml](docs/alerts.yml) has the one alert to start with, and says what is deliberately not alerted on.

## Use it as a library

genba is a Go library first and a pair of binaries second.
There is no `internal` directory anywhere in the module, so every package is importable and every type on this page is part of the public surface.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

func main() {
	st := memstore.New()
	defer st.Close()

	searcher := index.New(st)

	me := &acl.Principal{
		Tenant:  "acme",
		Subject: "u_mei",
		Groups:  acl.GroupSet{Version: 1, Members: []string{"gdrive:eng@acme.com"}},
	}
	res, err := searcher.Search(context.Background(), me, index.Query{Text: "payments runbook"})
	if err != nil {
		log.Fatal(err)
	}
	for _, hit := range res.Hits {
		fmt.Println(hit.Document.Title)
	}

	// Or mount the whole HTTP surface inside a service you already run.
	srv := api.New(st, searcher, api.HeaderAuth{Tenant: "acme"})
	http.Handle("/genba/", http.StripPrefix("/genba", srv.Handler()))
}
```

## Layout

| Package | What lives there |
| --- | --- |
| `acl` | principals, groups, permission descriptors, visibility bitmaps |
| `doc` | the canonical document model every connector normalises into |
| `store` | the storage interface, plus `storetest`, the conformance suite |
| `store/memstore` | the reference in memory driver |
| `store/sqlitestore` | the SQLite driver, pure Go, FTS5 and the permission check in one query |
| `store/pgstore` | the PostgreSQL 18 driver, migrations as SQL files and the permission check in one query |
| `store/segment` | the on disk segment container, [docs/segment.md](docs/segment.md) |
| `store/column` | one field across every row of a segment, and the scans over it, [docs/columns.md](docs/columns.md) |
| `store/vector` | the embedding section of a segment and the search over it, [docs/vectors.md](docs/vectors.md) |
| `store/graph` | the entities and relationships of a segment and the walk over them, [docs/graph.md](docs/graph.md) |
| `store/segdir` | the directory of segments, the manifest and the crash recovery, [docs/durability.md](docs/durability.md) |
| `index` | query parsing, retrieval and ranking |
| `connector` | the ingestion contract, cursors and checkpoints |
| `connector/connectortest` | the conformance suite that defines what a connector is |
| `connector/fssource` | the reference connector, a directory tree with OWNERS files, walked or watched |
| `connector/objectsource` | an S3 compatible bucket, signed and paged, [docs/ingestion.md](docs/ingestion.md) |
| `connector/thread` | a conversation and its replies assembled into one document |
| `connector/threadsource` | the crawl, cursor and permission refresh chat, ticket trackers and wikis share, [docs/ingestion.md](docs/ingestion.md) |
| `connector/slacksource` | Slack, a thread at a time, with per method rate limits and a recorded workspace to test against |
| `connector/jirasource` | Jira, an issue and its comments as one document, with security levels and a recorded site to test against |
| `connector/limit` | the rate limit, backoff and circuit breaker every connector shares, as a round tripper |
| `connector/recorded` | HTTP exchanges captured from a real service and replayed from a directory, so tests need no account |
| `extract` | text and structure out of PDF, Word, PowerPoint, Excel, HTML and Markdown, [docs/extraction.md](docs/extraction.md) |
| `ingest` | the pipeline that runs a connector into a store |
| `config` | runtime configuration and the rules for loading it |
| `api` | the HTTP surface |
| `web` | the browser interface, compiled into the binary |
| `cmd/genbad` | the server |
| `cmd/genba` | the command line client |

`arch_test.go` asserts the dependency direction between these, so an import that skips a layer fails the build rather than being noticed in review a month later.

## Connectors

A connector describes documents in some source system and who may read them.
It does not decide how they are stored, ranked or filtered, and it never touches the store itself.
The whole interface is three methods:

```go
type Connector interface {
	Source() string
	Sync(ctx context.Context, from Cursor, emit func(context.Context, Change) error) (Cursor, error)
	Close() error
}
```

`Sync` walks the source from a cursor and calls `emit` once per change.
`emit` does the batching and the storing on the calling goroutine, so there is no queue between a connector and the store.
That is deliberate.
A source that produces faster than the store can absorb is slowed down by the handover itself, which shows up as a slower sync rather than as memory that keeps growing until something is killed.

The pipeline stores a batch and then saves the cursor for it, never the other way round.
A crash between the two replays documents, which is harmless because storing the same document twice is the same as storing it once.
The other order loses documents and nothing downstream ever notices they are missing.
`ingest` has a test that kills the store after every possible number of writes and checks that a resume finds all of them.

A connector that cannot work out who may read a document says so, by leaving the permissions unresolved, and the pipeline stores that document out of every query path and counts it.
Failing to answer is not permission to publish.

`connector/fssource` is the reference implementation, and it is the one to read before writing another.
It walks a directory tree, skips version control and dependency directories, reads text files up to a size limit, and asks a `Policy` who may read each one:

```go
policy, err := fssource.NewOwnersPolicy(root, "repo", "github")
if err != nil {
	log.Fatal(err)
}
src, err := fssource.New(root, "repo", policy)
if err != nil {
	log.Fatal(err)
}

pipeline, err := ingest.New(st, connector.NewMemoryCheckpoints())
if err != nil {
	log.Fatal(err)
}
stats, err := pipeline.Run(ctx, "acme", src)
```

Permissions come from the policy rather than from the walk, because a directory tree says almost nothing about access on its own.
The mode bits describe the account the crawler runs as, not the people in the company.
`OwnersPolicy` reads the OWNERS files that Kubernetes and a number of other large repositories keep, taking the nearest one going up the tree, which is a real access control list maintained by real people over a corpus anybody can check out.
A source built with no policy at all quarantines everything, so having not thought about permissions yet is a visible state in the stats rather than an invisible one in the index.

A source can also be given a watcher, which asks the operating system what changed and turns a refresh into a read of the handful of files that moved rather than a walk of the whole tree.
The watcher is untrusted until a walk has vouched for it, and anything out of the ordinary sends it back to untrusted, so a dropped event costs one walk rather than a document that never gets reindexed.
`docs/ingestion.md` has the details.

`connector/objectsource` is the second one, and the first that talks to a network service, which is where most of what a real connector has to get right lives.
It reads an S3 compatible bucket, which is one connector rather than eight because Amazon's own service, MinIO, Ceph, Cloudflare R2, Backblaze B2, Wasabi and DigitalOcean Spaces all answer to the same two calls signed the same way.

```go
client, err := objectsource.NewClient(objectsource.Config{
	Endpoint:        "https://s3.eu-west-1.amazonaws.com",
	Region:          "eu-west-1",
	Bucket:          "acme-reports",
	AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
	SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
})
if err != nil {
	log.Fatal(err)
}
policy, err := objectsource.NewBucketPolicy(client, "reports", "okta", "acme.com")
if err != nil {
	log.Fatal(err)
}
src, err := objectsource.New(client, "reports", policy, objectsource.WithPrefix("quarterly/"))
if err != nil {
	log.Fatal(err)
}
```

The signing is signature version 4 written out in the package rather than taken from a vendor SDK, so the binary stays one file with no cloud provider dependency tree under it, and it is pinned against the published worked examples rather than against itself.
`WithPrefix` narrows the listing rather than filtering it afterwards, so a source pointed at one folder of a bucket of a hundred million objects costs what that folder costs, and several sources can read the same bucket under different prefixes with different policies.
Which of the two ways the bucket goes in the URL is a setting rather than a guess, because it is the one thing that genuinely differs between these services.

What a source said about who may read a document is turned into the model in one place, `connector/aclmap`, rather than once per connector.
Every system names permissions differently, and the same idea is a `reader` in one, `READ` in another, `VIEW` in a third and `BROWSE_PROJECTS` in a fourth.
Mapping each of those is easy on its own, and the collection of them is where a search engine leaks, because every connector would otherwise decide on its own what a grant to a partner's domain means and what to do with a statement it does not understand.
So a refusal beats a grant everywhere, a link share is recorded rather than inferred from the absence of a restriction, and anything that cannot be represented faithfully is quarantined and counted by reason instead of approximated.
[docs/permissions.md](docs/permissions.md) has the mapping table for each source and the reasoning behind the awkward cases.

A connector hands the pipeline a body, and for the PDF attached to a ticket or the deck a quarter was reviewed from, the bytes are not it.
`extract` turns those into text using the standard library and nothing else: no office suite, no headless browser and nothing to install alongside the binary.
Every reader writes into one builder and produces one shape, a Markdown subset with headings, paragraphs, lists and tables, so a heading is the same three bytes whether it came from a Word style or an `h2` and the heading offsets cannot drift out of step with the text.
A PDF is read through the font's own character map where the file has one, and a page that comes out as glyph codes rather than characters is treated as having no text instead of filling the index with terms nobody can type.
A scan extracts as nothing for the same reason, and is still indexed by its name, its size and who may read it.
Each budget bounds one file rather than the run: a zip bomb, a truncated archive and a PDF that expands past its limit each cost one document, and the failures are told apart so that a half copied `.docx` reads as recopy the file rather than as a format nobody supports.
[docs/extraction.md](docs/extraction.md) has the details, including the corpus of generated files the readers are tested against and the two bugs those tests found.

Everything after the first sync is incremental.
A second run over an unchanged tree reads no files at all, an OWNERS edit costs one write per document rather than a recrawl of the subtree, and a reconciliation sweep after every sync catches what a change feed cannot report, starting with the file somebody deleted.
The same holds over a network: a second sync of an unchanged bucket fetches no objects and reads no bytes, and rewriting the bucket's access control list costs one write per object rather than a fetch of the whole bucket.
[docs/ingestion.md](docs/ingestion.md) has the details, including the optional capabilities a connector implements to get each of those and the rule that stops a timed out enumeration from emptying a working index.

Chat, ticket trackers and wikis all keep the same shape underneath the vocabulary, and `connector/thread` is the one place it is assembled.
Something is said and then people say things about it: a message and its replies, an issue and its comments, a page and the discussion at the bottom of it.
An index that copies the source's row per message shape answers badly, because a question about a cancelled order returns fourteen rows from the same conversation and none of them is the answer on its own, while the reply that holds the answer scores nothing for the word in the question because that word was in the message above it.
So a conversation is one document that ranks as a whole, the author of each message goes into the body in front of what they said, a repeated message is kept once because a paged reply listing repeats the parent on every page, and a thread too long to fit keeps its beginning and its end and says how many messages it left out.
Nothing is written in place of what was left out, because a marker in a body is a phrase in the index that nobody at the source ever typed.

Assembling one conversation is the easy half.
`connector/threadsource` is the other half, and it is written once for all three products: the crawl over containers, the cursor that survives an interrupted run, the sweep that finds what a change feed never reports, and the permission refresh that runs when a channel is made private without a message in it being touched.
A product adapter answers four questions about its API and gets a connector, which is why a channel somebody restricted this morning costs one write per thread in it rather than a recrawl, and why a thread and a channel that changed in the same second are both emitted exactly once.
The rule comes from the container, a conversation may override it the way a ticket with a security level on it does, and a container nobody has said anything about quarantines what is in it rather than defaulting to readable.

`connector/slacksource` is the first product on it, and most of what it does is deal with the things Slack does not tell you.
There is no endpoint for what changed since a time, so a sync reads history back to the cursor and to a reply window as well, because a reply moves nothing but the parent's latest reply and a parent older than the cursor would otherwise never be looked at again.
A reply older than the window is missed on purpose, and the version in the listing is what makes the sweep repair it, which is the honest version of a source with no change feed.
Nothing reports that somebody was removed from a private channel either, so membership is reapplied on a schedule as well as when Slack says the channel changed, because a revocation that lands whenever somebody next posts is not a revocation.
Slack publishes a rate limit per method rather than per token and the published rates differ by a hundred times across the methods one crawl uses, so each tier gets its own bucket and a request is routed by the method it names.
Direct messages are never asked for rather than asked for and filtered, joins and leaves and topic changes are not documents, and the whole thing is tested twice over: against a fake workspace for behaviour, and against a committed recording of a crawl for the wire format, so nobody needs an account to run the tests.

`connector/jirasource` is the second product on the same interface, and it is interesting for the opposite reason.
Jira has a real change feed, because an issue's `updated` field moves when anything about it moves and JQL will filter and order by it, so a sync is one query per project with no window to widen and nothing to guess at, and a ticket moved from one column to another with nobody writing a word is found the same way a comment is.
The sweep is still there and it is only for deletion, because nothing in JQL reports an issue that was deleted or moved somewhere this token cannot follow.
Permissions are the hard half instead: browse comes from the project's permission scheme, which grants it to some mixture of groups, named accounts and project roles, and a role is resolved to the people in it rather than left as the name of a thing.
An issue security level replaces the project's answer rather than adding to it, which is the concrete case a conversation is allowed to override its container for, and a level this token cannot resolve quarantines the ticket instead of falling back to the project, because a security level is the one thing somebody set on purpose to keep other people out.
A description is not text but a document tree, so it is rendered as Markdown rather than flattened, which keeps the stack trace in a code block and the table of readings a table, since a search result that shows somebody a flattened version of the thing they were looking for has answered the query and failed the person.

What a connector is gets decided by `connector/connectortest` rather than by the interface.
The interface says what compiles, and the suite says what works: that a full sync finds everything, that resuming from a cursor loses nothing that came after it, that a second sync of a source nothing changed in reads nothing, that a deleted document stops being part of the source one way or the other, and that every document says who may read it.
A connector that cannot work out an access control list and says so is quarantined and passes.
A connector that indexes a document without one fails, which is the whole reason the suite exists.
The optional capabilities are optional in the suite too, so a connector that cannot list its source skips those cases, and one that can is held to them.
Running it is not optional either: a test at the root of the module finds the packages that declare a connector and fails any of them whose tests do not run the suite, because a conformance suite nobody runs is documentation.

## Storage

The interface in `store` is deliberately narrow, and a driver passes or fails one conformance suite.
Four drivers are planned:

- `memstore`, in memory, the reference implementation and what the tests run on.
- `sqlitestore`, pure Go, for a single node install that wants to keep its data.
- `pgstore`, PostgreSQL 18, for a deployment that already runs one.
  Connection pooling, retries and the schema all come out of one connection string, and the migrations are SQL files a DBA can read before running them.
  [docs/postgres.md](docs/postgres.md) has the details, including the trade it makes and the lock the write path takes.
- `kurastore`, which will link [tamnd/kura](https://github.com/tamnd/kura), a storage engine written in Rust.
  `store/kura` binds what its C ABI offers today, which is bitmaps, posting lists and vectors rather than a document store, so there is a binding and not yet a driver.
  It is compiled in with `-tags kura` and `CGO_ENABLED=1`, and everything else keeps working without it.
  [docs/kura.md](docs/kura.md) has the details.

A driver that can do better than a scan says so by implementing `store.Retriever`, and the searcher asks it for the match set instead of walking everything.
`sqlitestore` does, so the permission check, the filters and the terms are all one SQL statement over an FTS5 index, and the rows the database returns are already the rows the caller may read.
There is one definition of the match set and both paths are held to it.
`store/storetest` runs a driver's `Retrieve` against its own `Scan` and fails any disagreement, and `index` runs the same searches through both drivers and requires the same ranked answer, so a driver cannot quietly drift from the analyzer.
`sqlitestore` also counts the rows the database hands back, which is what its own tests assert on: a caller who may read nothing costs zero rows rather than five hundred rows filtered afterwards.
`pgstore` does the same, and adds a test that reads the query plan, because the plan is the only thing that says where in the database the filtering happened rather than just that it happened somewhere.

## Build

```
make build      # the server, with the interface compiled in
make headless   # the server without the interface, for an API only deployment
make cli        # the command line client
make test       # go test ./...
make race       # the same with the race detector
make lint       # golangci-lint
```

The Rust engine is off by default and none of the above touches it.

```
make kura       # fetch and build the engine into third_party
make kura-build # the server linked against it
make kura-test  # the binding's tests, against it
```

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md).
The short version is that a change to anything on a content path needs a test that proves the wrong person still cannot see the document.

## License

MIT.
See [LICENSE](LICENSE).
