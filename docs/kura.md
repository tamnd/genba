# The Rust engine

[kura](https://github.com/tamnd/kura) is a separate repository that holds a compression, posting list and vector implementation in Rust and exposes them over a C ABI.
This document is about the Go side of that ABI, which lives in `store/kura`.

## It is off by default, and that is the important part

The whole premise of this repository is a single static binary with no dependencies.
Linking a Rust library gives that up: it needs a C toolchain, it needs the library built for the target, and it ends cross compilation to five platforms from one machine.

So the engine is opt in twice over.

```
go build ./...                              the pure Go build, which is what ships
CGO_ENABLED=1 go build -tags kura ./...     linked against the engine
```

Without the `kura` tag, every function in the package returns `ErrUnavailable` and the error says which build to use.
It is not a panic, and it is deliberately not a fallback to the pure Go implementations elsewhere in the repository.
A caller that asked for the engine and quietly got something else has no way to find out, and "the fast path was not linked" is a fact an operator should be told once at startup rather than work out from a latency graph six weeks later.

The issue this was built for asked for a plain `cgo` tag.
A plain `cgo` tag would mean that anybody with the default `CGO_ENABLED=1` could not build this repository at all until they had a copy of the engine on disk, which is a worse default than the one it was trying to avoid.
The `kura` tag is the same decision made explicit: nothing links the engine unless somebody asked for it by name.

## Getting the engine

There are no releases to download yet, so the script builds it from source.

```
make kura         fetch and build the engine into third_party
make kura-build   build the server against it
make kura-test    run the binding's tests against it
```

`scripts/kura.sh` clones the engine into `third_party/kura-src`, runs `cargo build --release -p kura-ffi`, and copies the header and the static library into `third_party/kura`, which is where the cgo directives in `store/kura/engine.go` look for them.
All of `third_party` is ignored by git.
A checked in binary is a binary nobody can review and a build nobody can reproduce.

`KURA_REPO`, `KURA_REF` and `KURA_SRC` override where it comes from, which is what you want when testing a change to the engine alongside a change here.

## What the ABI covers

| Area | What is there |
| --- | --- |
| Bitmaps | An opaque set of document ids, with insert, remove, contains, length, intersect, union and a copy out |
| Postings | Encode ascending ids, read the count from the header, decode, and answer membership by decoding one block |
| Vectors | Cosine similarity, quantise to one signed byte per dimension plus a scale, and score two quantised vectors |

What is not there is a document store.
The ABI has no notion of a document, a field, a principal or a transaction, so `store/kura` does not implement `store.Store` and does not run the `store/storetest` conformance suite.
There is nothing to run it against.
When the engine grows a document store this package is where it will be bound, and the conformance suite is already written and waiting for it.

## The rules of the boundary

Every call returns a status code and every one of them is checked.
A status that is not `StatusOK` becomes a Go error and no out parameter is read.

The status codes are mirrored into Go constants rather than read through cgo, so that `errors.Is(err, kura.StatusNotSorted)` means the same thing in a build that has the engine and one that does not.
There is a test under the `kura` tag that checks every mirrored string against `kura_status_message`, so a code that changes meaning in the engine fails a test here rather than turning into a wrong error somewhere.

Memory the engine allocates is freed by the engine.
Every call that gets a buffer back copies it into Go memory and frees it before returning, so nothing outside the package ever holds a pointer the engine owns.
The one handle that outlives a call is `Bitmap`, and it has a `Close` that is safe to call twice.
A second `Close` being a no op is the difference between a double free and nothing happening, and a double free in a C library is not a Go panic.

No Go pointer is handed to the engine except for the duration of one call, which is what the cgo pointer rules allow.
An empty slice has no first element to take the address of and a null pointer is an error to the engine rather than an empty input, so an empty slice points at a fixed byte of scratch that the engine is told is zero long.
That keeps "no ids at all" a legal argument rather than a special case at every call site.

The ABI version is checked before the first call and a mismatch is a refusal rather than a warning.
Two builds that disagree about a struct layout produce wrong answers rather than errors, and wrong answers out of a storage engine are the worst kind of failure there is.

## Bytes that may be anything

A posting list read off disk is bytes, and bytes may be anything.
On this side of a cgo boundary the cost of being wrong about them is a segmentation fault rather than a Go panic, so the same contract the pure Go formats have applies here: every malformed input is an error and none of them is a crash.

The fuzz target `FuzzPostingsSurvivesAnything` is what holds that up.
It is skipped in a build without the engine, because there is nothing to fuzz, and it runs for ten minutes on the nightly schedule where there is time for it to find something.
A crasher it finds is kept as an artifact and belongs in `store/kura/testdata` as a regression test.

It has already found one.
The count at the front of a posting list is a variable length integer bounded only by the width of a `u32`, and nothing in the format bounds it against the size of the input.
So seven bytes can claim four billion ids, and a decoder that sizes its destination from the header asks the allocator for seventeen gigabytes.
An id costs at least one byte in the blocks section and the blocks are part of the input, so a count larger than the input cannot be honest, and `DecodePostings` refuses one with `StatusTruncated` rather than trying to allocate for it.

## What CI does with it

The `kura` job in `ci.yml` builds the engine, builds the server against it, and runs the binding's tests with the race detector on.
Nothing else in CI links it, and the binary that ships is still the pure Go one.

The job exists so the binding does not rot.
The engine is a separate repository that moves on its own, and a change to its ABI should fail in this repository's CI rather than in somebody's build.

The leak tests are in that job too.
They run twenty thousand allocate and free cycles and compare the resident set high water mark either side, because the memory at risk is the engine's and the Go allocator has never heard of it.
