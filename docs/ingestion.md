# Keeping an index in step with its source

The first sync is the easy half.
Read everything, store everything, and the index matches the source because it was just built from it.

Everything after that is the hard half, and it is the half a search engine spends its whole life in.
A corpus of a hundred thousand documents changes by a few dozen a day, so a sync that reads all of it to find them costs several thousand times what the change is worth.
Reading only what changed is the whole game, and it has three parts that fail in different ways.

## What changed: cursors

A `Sync` is given a cursor and returns one.
The cursor is opaque to everything except the connector that wrote it, because what it means is entirely a property of the source: a modification time for a directory tree, a change token for a document API, a log sequence number for a database.

```go
type Cursor struct {
	Value string
	Time  time.Time
}
```

The pipeline stores a batch and then saves the cursor for it, never the other way round.
A crash between the two replays documents, which is harmless because storing the same document twice is the same as storing it once.
The other order loses documents and nothing downstream ever notices they are missing.

For `fssource` the cursor is the highest modification time the last walk saw, and the saving is the read rather than the walk.
A filesystem has no change feed of its own, so a sync that only knows how to walk cannot avoid statting every file, but there is every way to avoid opening them.
That is what the counters measure.
Asking the operating system to report changes instead removes the walk as well, and is the section after next.

```go
before := src.Counters()
stats, err := pipeline.Run(ctx, "acme", src)
spent := src.Counters().Since(before)
```

`Counters` is optional, and a connector that implements it reports four numbers: listings, metadata lookups, fetches and bytes.
A second sync over an unchanged tree of eight files spends eight metadata lookups, zero fetches and zero bytes.
That is the floor for a source without a change feed, and the test that says so is `TestASecondSyncOverAnUnchangedTreeReadsNothing`.

### Not walking at all

The walk is the floor for a filesystem asked nothing but "what is here".
It is not the floor for a filesystem that was asked to say when something changes, and every operating system worth deploying on will do that.

```go
w, err := fssource.Watch(root)
if err != nil {
	log.Warn("watching the tree, syncs will walk it instead", "err", err)
}
src, err := fssource.New(root, "docs", policy, fssource.WithWatcher(w))
```

The watcher is built separately from the source and owned by the caller, because building one fails for reasons that belong to the machine rather than to the program.
A tree over the inotify limit, a filesystem the backend does not support, a process near its descriptor limit.
None of those are a reason for a server not to start, so `Watch` returns an error the caller logs, and a nil watcher means exactly what passing no watcher means.

A watcher is an optimisation that is wrong rather than slow when it fails.
A dropped event is a document that never gets reindexed and nothing anywhere reports it, which is the same shape of failure as a permission change that quietly went nowhere.
So the record starts out untrusted, goes back to untrusted the moment anything is out of the ordinary, and a sync that finds it untrusted walks the tree.
Untrusted covers the first sync, a queue that overflowed, any error the backend reported, more changed paths than a walk would have cost, and the watcher being closed.
The worst a watcher can do is cost what not having one costs.

Three things about it are worth writing down.

A deletion is a change only a watcher can see.
A walk finds a deleted file by not finding it, which is to say it does not find it, and until now the only thing that ever removed one from the index was the reconciliation sweep below.
A watched sync emits the deletion directly, and the sweep goes back to being the thing that catches what everything else missed.

Existence is decided by the stat and not by the event.
A file that was written and then deleted produces both kinds of event and only one of them is still true, and the filesystem is the one that knows which.

The last one is the reason there is an interface for it.

```go
type Ruled interface {
	IsRule(relPath string) bool
}
```

An edit to an OWNERS file changes who may read every document in the subtree below it.
The only event anybody raises is about the OWNERS file, and a sync that read the reported paths and nothing else would apply the edit to the OWNERS file and to nothing it governs.
So a policy says which paths are its rules, and a sync that finds one of them among the changes throws the record away and walks that round.
That is the expensive answer and it is the right one, because the cheap answer is a revocation that appears to have been applied and has not.
`OSPolicy` needs none of this, since its rule for a file is the file's own mode and changing that raises an event on the file itself.

The thing that took the longest to get right is not in any of that.
Some backends report an ordinary write as an attribute change first and the write itself a moment later, so a sync landing between the two sees a mode change and nothing else.
That is the cheap path, a permission change with no read, and taking it is correct.
What is not correct is moving the cursor, because the file's modification time is now newer than the cursor while its content has not been read, and the next sync that falls back to walking would skip it for ever.
So the permission only path puts nothing into the cursor at all.
It costs one permission change written twice and it is the difference between a watcher that saves work and one that loses documents.

### The same problem over a network

`objectsource` makes the same trade against an S3 compatible bucket, and two things about it are different enough to be worth writing down.

The listing is ordered by key rather than by when anything changed, so the modification times a run sees go up and down as it proceeds.
A cursor holding the time of the change just written would tell the next run to skip everything older, and the objects further down the listing that had not been read yet would be skipped for ever.
So the cursor a change carries holds the last key instead, and the high water mark is only written when the run finishes.
That is also what makes an interrupted run resumable: the next one lists again with `start-after` set to the key it reached.

The store's modification times have a second of resolution and a listing of a large bucket takes a great deal longer than that.
An object written later in the same second as the newest one a run saw would be filed under a time the cursor had already passed.
The cursor is therefore held one second behind the store's own clock, read from the `Date` header of the response rather than from this machine's clock, and that second is looked at once more on the next run.
It costs re-reading a handful of objects after a write and it costs nothing on a bucket that has been quiet.

## Who may read it: permissions without a recrawl

A permission change is not a content change, and a sync built only on modification times cannot see one at all.
Somebody edits an OWNERS file at the root of a repository, and who may read every document below it changes without a single one of those documents being touched.

The connector reports this as a change with no document in it.

```go
type Change struct {
	Document        doc.Document
	Deleted         bool
	PermissionsOnly bool
	Cursor          Cursor
}
```

Only `ID` and `Permissions` are read from a `PermissionsOnly` change.
The rest is ignored, so a connector cannot use it to sneak a content update past the pipeline: the content will not be stored.

The pipeline batches these the same way it batches documents and applies them with one call:

```go
type Maintenance interface {
	Inventory(ctx context.Context, tenant, source string, fn func(Item) bool) error
	SetPermissions(ctx context.Context, tenant string, perms map[string]acl.Permissions) (int, error)
}
```

`SetPermissions` is a store capability rather than a store method, because not every driver can do it and a driver that cannot should say so rather than pretend.
A pipeline over a store without it refuses a permission change instead of quietly dropping it.
That is deliberate: a sync that reported success while a revocation went nowhere is worse than one that failed, because the failure is visible and the revocation is not.

The subtle case inside a driver is the quarantine line.
A document whose permissions do not resolve is held out of the full text index, the posting lists, the term statistics and the corpus counts, so a permission change that crosses that line has to retire or reindex the document, while a change from one resolved access control list to another must not touch any of it.
The conformance suite in `store/storetest` checks both, on every driver.

On the connector side this needs a policy that can say when its answer changed.

```go
type Versioned interface {
	ChangedAt(ctx context.Context, relPath string) (time.Time, error)
}

type Reloader interface {
	Reload()
}
```

Every file the walk skips as unchanged is asked about, so the answer has to be cheap.
For `OwnersPolicy` it is a map read after the first file in each directory.
For `OSPolicy` it is the inode change time, which the walk is already holding, so it costs nothing at all.
`Reload` is called once at the start of every walk, and it is what stops a process that has been up for a week from answering with the OWNERS files as they were when it started.
The cache lives for exactly one walk: long enough to cost one read per OWNERS file per sync instead of one per document, short enough that the next sync notices the edit.

The inode change time is worth a sentence of its own, because it is the whole of how a `chmod` reaches the index.
It moves when the mode, the owner or the access control list is written, and stays where it is when only the content is, which is exactly the event a sync comparing modification times cannot see.
A revocation that takes effect the next time somebody happens to edit the file is not a revocation.

Windows has no equivalent that can be read without opening every file, so `OSPolicy` answers with the zero time there and a permission change waits for the next full sync.
That is stated rather than worked around, because the way to work around it is a stat that costs an open per file per sync.

Asking twice is the other cost worth avoiding.
A policy that reads the file system itself already has everything it needs in the `fs.FileInfo` the walk is carrying, and calling `Permissions` with a path would make it stat the file a second time.
On a corpus of a million files that is a million system calls a sync spends finding out something it was told a moment ago, so the source hands the file information over where a policy can use it.
That handover is deliberately not a public interface: it is one more thing every policy would have to implement, for a saving only the policies that read the file system get.

Object storage has no equivalent of the inode change time and no per object "the list changed at" anywhere.
The only way to find out whether one object's list was rewritten is to read it, which is the request per object the incremental path exists to avoid.
So `BucketPolicy` reads the bucket's list once per sync, fingerprints the statements in it rather than the bytes, and compares that with the last sync's.
A bucket whose fingerprint moved gives every object under it a permission change without a single byte being fetched, and a bucket whose fingerprint did not costs one request for the whole sync.
The fingerprint is built from the parsed statements because the order of grants is not promised by any of these services and some of them put a request id in the response, and a fingerprint that moved on its own would rewrite the permissions of the whole bucket on every sync.
`ObjectPolicy` deliberately does not implement `Versioned` at all, which is the honest answer rather than an expensive one.

## What the sync could not have seen: reconciliation

An index built only out of a change feed drifts away from its source, and nothing in the feed ever says so.

A feed drops an event.
A bulk edit at the source raises none.
A process is killed between the store and the checkpoint.
A file is deleted, and a walk of a directory tree can never see that, because there is nothing left to walk past.

None of those produce an error.
They produce an index that is quietly wrong, and the only way to find out is to count both sides.

```go
rec, err := pipeline.Reconcile(ctx, "acme", src)
```

The sweep enumerates the source, walks the index, and sorts the difference into three piles.

| Pile | What it means | What is done about it |
| --- | --- | --- |
| Missing | the source holds it and the index does not | refetched and stored |
| Stale | both hold it, at different versions | refetched and stored |
| Extra | the index holds it and the source no longer does | deleted |

It needs two optional capabilities: a `store.Maintenance` store to list what is indexed, and a `connector.Enumerator` to list what the source holds.
Repairing what is missing also needs a `connector.Fetcher`, and without one the sweep still reports and still deletes, and says once in the log that the rest is waiting for a full sync.

```go
type Enumerator interface {
	Connector
	Enumerate(ctx context.Context, fn func(Item) bool) error
}
```

An enumeration reads nothing.
On a filesystem it is a stat per file against a read per file, which on a real corpus is a second against a minute, and that price is the reason the sweep can run on the refresh interval rather than nightly.

### Nothing is deleted on a partial enumeration

If the walk of the source fails halfway, `Reconcile` returns the error and repairs nothing at all.
It is the most important rule in the package.
A list that stopped early is indistinguishable from a source that lost half its documents, and acting on the second reading would empty a working index because of a timeout.

### The report

The counts are exact and the ids are a sample of twenty per pile.
The first sweep after a fresh index finds the whole corpus missing, and a report that named all of it would be a copy of the corpus in memory and a log line nobody reads.
Twenty ids is what an operator actually uses, because it is enough to go and look at one and find out what is wrong.

`genbad` runs a sweep after every sync and logs at warning level only when something drifted.
An index that agrees with its source produces nothing in the log, so a line there means the incremental path missed something.

On a server that is watching the tree, the sweep is the only part of a refresh that still walks, which makes it the whole cost.
So it has its own interval, `-corpus-reconcile`, and the two settings are meant to be set apart: notice a change in a second, count both sides every few minutes.
The default is unchanged and sweeps after every sync.

### What it costs to hold

The sweep keeps an id and a version per source document in memory for its duration, which is tens of megabytes for a corpus of a few million.
The alternative is sorting both sides on disk and merging them, which is the right answer an order of magnitude further up and is not worth its complexity yet.

## Configuring a source

`genbad` can be pointed at a directory, at a bucket, or at both at once.
The two are separate feeds with separate cursors rather than one merged crawl, because a bucket that is refusing requests should not stop a directory being reindexed.

A directory, watched, sweeping every five minutes:

```
genbad \
  -tenant acme \
  -corpus ~/src/handbook \
  -corpus-name handbook \
  -corpus-acl owners \
  -corpus-refresh 1s \
  -corpus-watch \
  -corpus-reconcile 5m
```

`-corpus-acl owners` reads the OWNERS files in the tree, which is a real access control list maintained by real people.
The other two are `tenant`, where everybody in the tenant may read everything, and `os`, which reads the mode bits and needs `-corpus-identity` to say which directory the account names belong to.

A bucket, listed every thirty seconds, scoped to one prefix:

```
export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE
export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY

genbad \
  -tenant acme \
  -bucket company-docs \
  -bucket-endpoint https://s3.eu-west-1.amazonaws.com \
  -bucket-region eu-west-1 \
  -bucket-prefix handbook/ \
  -bucket-name handbook \
  -bucket-acl bucket \
  -bucket-identity google \
  -bucket-domain acme.com \
  -bucket-refresh 30s \
  -bucket-reconcile 15m
```

The credentials are read from the environment and there is no flag for them.
A secret in argv is readable by every process on the machine for as long as the server runs, and it ends up in the shell history of whoever started it.
The names are the ones every other tool in this space already uses, so a machine that can already reach the bucket needs nothing new set.
A bucket with no credentials at all is read unsigned, which is what a public bucket wants and what nothing else does.

`-bucket-acl bucket` reads the bucket's own access control list once per sync and gives that answer for every object in it, which is one request rather than a million.
`-bucket-acl object` reads each object's own list, which is exact and costs a request per object per sync, so it is worth reaching for only when the objects really do differ.
`-bucket-domain` is what a grant written against an email address is checked against, and a grant to an address outside it is quarantined rather than published.

A service that is not S3 itself almost certainly needs `-bucket-path-style`, which puts the bucket in the path rather than in the host name.
MinIO on a laptop is the shortest way to try the whole thing:

```
genbad \
  -tenant acme \
  -bucket corpus \
  -bucket-endpoint http://127.0.0.1:9000 \
  -bucket-path-style \
  -bucket-refresh 5s
```

Both feeds log the same numbers after every sync, under `corpus synced` and `bucket synced`.
The directory adds what its watcher has to say, and the bucket adds the request counts, which are what a bill is made of.
A listing count that climbs by one per sync is a healthy incremental run, and a fetch count that climbs with it on a bucket nobody is writing to means the cursor is not doing its job.

## Writing a connector

The required interface is still three methods, and a connector that implements only those works.
Everything else is optional, and each piece buys one thing.

| Capability | What it buys |
| --- | --- |
| `Enumerator` | reconciliation, so deletions and dropped events are found |
| `Fetcher` | repair, so what reconciliation finds is fixed rather than only reported |
| `Counted` | the numbers that say whether the incremental path is actually incremental |
| `Change.PermissionsOnly` | a permission change costs a write instead of a recrawl |
| `Change.Deleted` | a deletion reaches the index without waiting for a sweep |

`connector/fssource` implements every one of them and is the one to read before writing another.
`connector/objectsource` implements all but the deletion, over a network service, and is the one to read for the parts a local source never has to deal with: signing, paging, and a listing whose order has nothing to do with what changed.
A bucket listing is the same shape of problem as a walk, and an object that is no longer in it is found by the sweep for the same reason.
