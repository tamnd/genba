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

Then query it:

```
export GENBA_SUBJECT=u_mei
export GENBA_TENANT=acme
export GENBA_GROUPS=gdrive:eng@acme.com
genba search payments failover runbook
```

The browser interface is at http://127.0.0.1:8080 and is compiled into the binary, so there is no static directory to deploy alongside it.

By default every file in the corpus is readable by everybody in the tenant, which is the right rule for a public checkout and the wrong one for almost anything else.
If the tree has OWNERS files in it, `-corpus-acl owners` reads them instead, and a query then returns different results depending on who is asking.
Paths that no OWNERS file governs are quarantined rather than published, and the count of them is on the sync log line.

## Configuration

Every setting has a flag and an environment variable.
The environment variable is the flag in upper case with a `GENBA_` prefix, and a flag wins over it.

| Variable | Default | What it does |
| --- | --- | --- |
| `GENBA_ADDR` | `127.0.0.1:8080` | listen address |
| `GENBA_STORE` | `memory` | storage driver: `memory`, `sqlite`, `postgres` or `kura` |
| `GENBA_DSN` | empty | path or connection string for the driver |
| `GENBA_TENANT` | empty | tenant served by a single tenant deployment |
| `GENBA_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `GENBA_READ_TIMEOUT` | `30s` | request read timeout |
| `GENBA_WRITE_TIMEOUT` | `60s` | request write timeout |
| `GENBA_SHUTDOWN_GRACE` | `15s` | how long a shutdown waits for in flight requests |

The corpus flags have no environment variables, because a directory to index is a thing you type once while trying the binary out rather than a property of a deployment.

| Flag | Default | What it does |
| --- | --- | --- |
| `-corpus` | empty | directory to index at startup |
| `-corpus-name` | `files` | source name the documents carry, and what `-source` filters on |
| `-corpus-acl` | `tenant` | who may read it: `tenant` for everybody in the tenant, `owners` to read OWNERS files |
| `-corpus-refresh` | `0` | how often to sync again, zero for once at startup |

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
| `index` | query parsing, retrieval and ranking |
| `connector` | the ingestion contract, cursors and checkpoints |
| `connector/fssource` | the reference connector, a directory tree with OWNERS files |
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

## Storage

The interface in `store` is deliberately narrow, and a driver passes or fails one conformance suite.
Four drivers are planned:

- `memstore`, in memory, the reference implementation and what the tests run on.
- `sqlitestore`, pure Go, for a single node install that wants to keep its data.
- `pgstore`, PostgreSQL 18, for a deployment that already runs one.
- `kurastore`, which links [tamnd/kura](https://github.com/tamnd/kura), a storage engine written in Rust that holds columnar, vector and graph data in one file.
  It is compiled in with `-tags kura` and `CGO_ENABLED=1`, and everything else keeps working without it.

## Build

```
make build      # the server, with the interface compiled in
make headless   # the server without the interface, for an API only deployment
make cli        # the command line client
make test       # go test ./...
make race       # the same with the race detector
make lint       # golangci-lint
```

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md).
The short version is that a change to anything on a content path needs a test that proves the wrong person still cannot see the document.

## License

MIT.
See [LICENSE](LICENSE).
