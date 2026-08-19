# Contributing

Thanks for taking the time.
This file is short on ceremony and specific about the few things that matter here.

## Before you open a pull request

```
make fmt
make vet
make race
make lint
```

CI runs the same commands on Linux and macOS, plus a cross compile for Windows and FreeBSD, a build without the browser interface, `govulncheck`, a `go mod tidy` check and a license check.
Running them locally is faster than finding out from a red badge.

## If you touched the query path or the interface

```
make bench-counters
make bench-gate
make ui-gate
```

`bench-counters` is exact and fast.
It counts rows read, statements run and documents decoded, and those numbers are the same on a laptop and on a busy runner, so it is the check that catches a query which started walking the corpus.

`bench-gate` is the wall clock.
It measures the search endpoint per query class in interleaved rounds, compares the median of the quietest round against `benchcorpus/baseline.json`, and normalises by a calibration workload so a busy machine is not read as a slow query.
The first run generates the corpus and takes a few minutes.
If the baseline itself needs to move, `make bench-gate-record` writes a new one and it belongs in a commit of its own that says why the number moved.

`ui-gate` checks the asset budgets, then starts the binary over this repository as a corpus and runs axe and Lighthouse against the home page, a results page and a results page with the drawer open.
It needs `npx` and a Chrome, and it says so and stops rather than failing when there is not one.
A chromedriver that does not match the Chrome next to it is the usual reason it will not start, and `CHROMEDRIVER` points it at a matching one.

## The rule that is not negotiable

Anything that can return document content takes a principal, and the permission filter runs inside the storage driver while it walks its own data.
If your change touches a content path, it needs a test that shows a reader without access still gets nothing.
`store/storetest` is where those tests go when they apply to every driver, and a driver that does not pass that suite is not a driver.

Two more that follow from it:

- A permission that failed to resolve is not a permission.
  Hold the document back rather than guessing.
- A forbidden document and a missing one must be indistinguishable to the caller, down to the response body.

## Style

Write Go that reads like the standard library.
`gofmt -s`, no stutter in exported names, errors wrapped with `%w` and compared with `errors.Is`, table driven tests, `t.Context()` rather than `context.Background()` in tests.

Comments should say why, not what.
The code already says what it does.
A comment earns its place by recording the thing the next person would otherwise have to rediscover: the reason for the ordering, the case that made a bound necessary, the alternative that was tried and did not work.

## Architecture

`arch_test.go` holds the dependency direction between packages as a map.
A new package fails that test until it is added, which is on purpose: it forces one decision about where the package sits before anything imports it.

There is no `internal` directory in this module and there should not be one.
The platform is meant to be usable as a library, and an internal package is a decision that part of it is not.

## Commits and pull requests

Conventional commit prefixes are used for the changelog: `feat`, `fix`, `docs`, `test`, `chore`, `ci`, `perf`, `refactor`.
Keep the subject in the imperative and under about seventy characters.

A pull request should say what changed and why, and what you did to convince yourself it works.
Link the issue it closes.
Small and focused beats large and complete.

## Reporting a security issue

Do not open a public issue.
See [SECURITY.md](SECURITY.md).
