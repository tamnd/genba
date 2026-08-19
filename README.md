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

Then query it:

```
export GENBA_SUBJECT=u_mei
export GENBA_TENANT=acme
export GENBA_GROUPS=gdrive:eng@acme.com
genba search payments failover runbook
```

The browser interface is at http://127.0.0.1:8080 and is compiled into the binary, so there is no static directory to deploy alongside it.

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
| `config` | runtime configuration and the rules for loading it |
| `api` | the HTTP surface |
| `web` | the browser interface, compiled into the binary |
| `cmd/genbad` | the server |
| `cmd/genba` | the command line client |

`arch_test.go` asserts the dependency direction between these, so an import that skips a layer fails the build rather than being noticed in review a month later.

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
