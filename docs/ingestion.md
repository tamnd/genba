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
`-bucket-rate`, `-bucket-burst` and `-bucket-retries` are the ceiling the crawl keeps itself under, and the section below is about what they do and why there is no value meaning unlimited.

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

## Staying inside a service's limits

A crawler that ignores an API's limits gets the company's integration token revoked, and that is a worse outcome than a slow crawl by a wide margin.
A slow crawl finishes late.
A revoked token is an index that stops updating, a conversation with whoever owns the integration, and in most companies a week before anybody is allowed to try again.

`connector/limit` is where that is dealt with, and it is an `http.RoundTripper` rather than something each connector calls.
That is the layer the requests actually are at, so a connector built on `http.Client` gets all of it by being handed a different client and needs no code of its own:

```go
client := &http.Client{Transport: limit.NewTransport(limit.Limits{Rate: 8, Burst: 16})}
src, err := objectsource.New(objectsource.NewClient(cfg, objectsource.WithHTTPClient(client)), name, policy)
```

The transport does four things around every request.

It waits for a token, so the request rate stays under a ceiling.
The bucket is a token bucket rather than a fixed delay because real work is lumpy: a page of a listing followed immediately by the four documents on it is a burst of five, and a quota is a rate over a window rather than a rule about one request at a time.
It refills by arithmetic on the clock rather than by a goroutine, so a limiter that is never used costs nothing and a process holding a hundred of them holds no timers.

It reads what the response says about the quota, off every response and not only off a refusal.
A response saying none is left is the last request before the wall rather than the first one after it, and holding back there is the difference between a crawl that stays inside its quota and one that finds the edge by hitting it.
Three header conventions are in use and there is no way to tell which one a service follows except by looking, so all three are read: `Retry-After`, the newer `RateLimit-Reset` and `RateLimit-Remaining`, and the older `X-RateLimit-Reset` and `X-RateLimit-Remaining`.
A reset is a delta in seconds under one convention and a Unix timestamp under the other, and the two are told apart by size, because a delta large enough to be mistaken for a timestamp would be a window of three hundred years.
Anything claiming a window more than an hour away is ignored, on the grounds that it is almost certainly a format nobody expected rather than a service that really wants to be left alone until the afternoon.

It retries what is worth retrying.
Too many requests and the five hundreds are, because both mean the same request might work in a moment.
A four hundred is not: a request the service considers malformed is malformed on every attempt, and retrying it burns quota to arrive at the same answer more slowly.
The wait doubles from `MinBackoff` towards `MaxBackoff` and is jittered between half and all of itself, which is what stops a fleet of crawlers that were all refused at the same moment from all coming back at the same moment.
A source that named its own time is honoured exactly, with the jitter added on top rather than taken off it, because coming back one millisecond early is coming back before the window rolled over.
Only a request with no body and an idempotent method is ever sent twice, because a round tripper is not allowed to modify the request it was handed and a POST that timed out may well have been carried out.

It stops the source when it has been refusing everything.
After `Failures` consecutive failures the circuit opens and every request fails with `limit.ErrOpen` until `Cooldown` has passed, at which point exactly one goes through to find out whether the source has recovered.
A crawler that kept retrying a source which has been down for a minute is doing nothing except making the outage look like load.
The count is of consecutive failures rather than of failures, because a crawl of any size has a few of those and a source that is actually broken has nothing else: one success anywhere in the run says the credentials are good, the network is up and the service is answering.
Unauthorised counts towards it, because a revoked token is exactly the state this is here to notice.
Forbidden deliberately does not, because at an object store it is the ordinary answer for one object out of a million that this account may not read, and a breaker that tripped on it would stop a healthy crawl over objects that were never part of the corpus.
Too many requests does not either, because that is the service working exactly as designed and saying so.

The defaults are cautious on purpose: five requests a second, a burst of ten, four retries, and a minute of cooldown.
A default that is too slow costs a longer first sync and nothing else, and a default that is too fast costs the token.
There is no value meaning unlimited, and an operator who wants one asks for a rate high enough that it never binds, which is a number in the log rather than a special case in the code.

A limiter belongs to one source, because a quota does.
Two connectors reading two different services have nothing to do with each other, and sharing a limiter between them would mean a slow wiki holding up a fast bucket.
Two connectors reading the same service with the same credentials share a quota whether they like it or not, and those two should share one transport.

What comes out of it lands in the sync line.
The `bucket synced` line carries `retries`, `throttled`, `throttled_for` and `quota_pauses` alongside the request counts, because a crawl that is being throttled looks exactly like a crawl that is slow.
`quota_pauses` is the one to watch: it means the ceiling is set above what the service is actually willing to give, and the crawl is finding that out by being told off rather than by staying under it.

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

### A conversation is one document

Chat, ticket trackers and wikis keep the same shape underneath their vocabulary.
Something is said, and then people say things about it: a message and its replies, an issue and its comments, a page and the discussion at the bottom of it.
The source stores those as separate rows because that is how they were written, one at a time, by different people on different days.

An index that copies that shape answers badly.
Ask it why the gearbox order was cancelled and it returns fourteen rows from the same conversation, ranked against each other, none of which is the answer on its own.
The reply that says the supplier could not meet the date scores nothing at all for the word gearbox, because the word gearbox was in the message above it and was never repeated.
The fourteen rows also crowd out the thirteen other conversations that should have been on the page.

So a conversation is one document, and `connector/thread` is where it is assembled.
It talks to nothing.
A connector fetches the messages, resolves who wrote them and works out who may read the result, and hands the pieces over.

```go
d := thread.Conversation{
	ID:        "chat:" + channel + ":" + ts,
	Container: channel,
	Root:      thread.Message{ID: ts, Author: author, At: when, Text: text},
	Replies:   replies,
}.Document(perms)
```

The permissions are an argument rather than a field, for the reason the rest of this document keeps coming back to: a signature that lets a caller forget the answer is a signature that invites a thread being indexed without one.

Four decisions in there are worth knowing about.

The author of each message goes into the body in front of what they said, which puts the name in the index as well as on the screen.
That is the difference between being able to search for what Mei said about the gearbox and only being able to search for the gearbox.

A repeated message is kept once.
A paged reply listing at more than one source repeats the parent message on every page, so a connector that concatenates the pages hands the root over three times, and saying it three times in the body trebles its weight in the ranking as well.

An edit moves the version.
A message edited in place changes what the document says without any reply being added, so a version derived only from reply times leaves the index serving the old text until somebody happens to answer.

A conversation too long for the body limit keeps its beginning and its end.
The root stays because it is what the conversation is about, and the end stays because a thread long enough to be cut is usually one that took a while to work something out, and the working out is at the bottom.
The document records how many messages were left out, and nothing is written in place of them, because a marker in a body is a phrase in the index that nobody at the source ever typed and it turns up in snippets.

### The crawl the three of them share

Assembling one conversation is the easy half.
The other half is the crawl around it, and writing that three times produces three connectors that drift apart on exactly the parts that must not drift: the cursor, the resume, the sweep and the permission refresh.
So it is written once, in `connector/threadsource`, and a product adapter is left with the only thing that is genuinely different, which is how to ask its API.

An adapter answers four questions.

```go
type Service interface {
	Containers(ctx context.Context) ([]Container, error)
	Threads(ctx context.Context, c Container, since time.Time, fn func(context.Context, Thread) error) error
	List(ctx context.Context, c Container, fn func(connector.Item) bool) error
	Read(ctx context.Context, id string) (Thread, error)
}
```

A container is a channel, a project or a space, and it carries the access rule and the time that rule last changed.
A thread is a `thread.Conversation` plus the container it is in, an optional rule of its own and the time it last changed.
All four questions are required, and the last two are the ones worth arguing about: none of these products reports a deletion in a change feed, so a message removed, an issue deleted and a page archived all leave the index holding a document that nothing will ever take away, and the sweep is the only thing that removes it.
`List` is what the sweep reads and `Read` is what turns it from a report into a repair.

The rule comes from the container, because that is how all three products actually work.
A private channel is private because of the channel, and a page in a restricted space is restricted because of the space.
A conversation may override it, which is not an edge case: a ticket with a security level on it is readable by that level's members and nobody else, whatever the project says.
A container nobody has said anything about quarantines everything in it rather than defaulting to readable, which is the same rule the rest of this document keeps coming back to.

Making a channel private touches no message in it, so a sync that only asked what changed would find nothing and the index would keep answering with the old rule.
An adapter reports when the rule changed, and when that is newer than the cursor the source lists the container and emits a permission change per conversation in it, carrying the new rule and no body.
That costs one write per thread rather than a refetch of the channel, and the run after it says nothing, because the rule has not changed again.
A full sync refreshes nothing, because every conversation it emits already carries the rule its container has right now, and emitting a permission change alongside each one would double the first sync of a workspace to say the same thing twice.

The cursor is a time plus an edge set, and the edge set is what makes it correct.
Two conversations that changed in the same instant is the case a bare timestamp gets wrong in both directions: asking for what changed strictly after the cursor loses the second one for ever, and asking for what changed at or after it emits both again on every run until something else happens.
So the cursor asks for at or after and carries the ids already emitted at exactly that instant, capped at a few hundred, and a workspace busy enough to overflow the cap re-emits rather than loses.

The cursor a change carries also records how far the run had got, as a container and a time inside it, while keeping the last completed run's boundary.
That is what makes an interrupted crawl cheap to resume: the next run skips the containers it had already finished and picks up inside the one it was in.
The boundary itself does not move mid-run, because advancing it would skip changes in the containers the run had not reached yet.

```go
src, err := threadsource.New(svc, "chat", threadsource.WithMaxBody(64<<10))
```

`WithSkipped` is told about a conversation the source could not index, which so far means one that arrived with no id.
An index quietly missing what nobody could read looks exactly like an index that is complete, and the difference only shows up when somebody cannot find a thread they remember.

### The conformance suite is the definition

`connector/connectortest` is what a connector has to pass, and it rather than the interface is the definition of one.
The interface says what compiles.
The suite says what works, because most of what a connector has to get right is not expressible as a method signature: that a full sync finds everything, that resuming from a cursor loses nothing, that a second sync of a source nothing changed in reads nothing, and that every document says who may read it.

Running it takes a fixture, which is the adapter between the suite and one source.
The suite cannot write a file, put an object or post a message, and it should not know which of those it is doing, so a fixture is a connector plus a handful of functions that do those things to the system behind it.

```go
func TestConformance(t *testing.T) {
	connectortest.Run(t, func(t *testing.T) connectortest.Fixture {
		dir := t.TempDir()
		src, err := fssource.New(dir, "files", fssource.PublicToTenant("files"))
		if err != nil {
			t.Fatal(err)
		}
		return connectortest.Fixture{
			Connector: src,
			ID:        func(name string) string { return "files:" + name },
			Write:     func(t *testing.T, name, body string) { /* put it in the source */ },
			Remove:    func(t *testing.T, name string) { /* take it out again */ },
		}
	})
}
```

`Remove`, `Share` and `Unresolvable` are optional and a fixture that leaves one nil skips the cases that need it, which is how a source nothing is ever deleted from is not failed for a deletion it cannot do.
The optional capabilities work the same way: a connector that does not implement `Enumerator` skips the listing cases, and one that does gets held to them.
The suite is deliberately harder on a connector that claims more.

`Write` has one responsibility worth knowing about before writing a fixture.
It has to leave the source in a state a sync can settle on, which for a source that keeps time to the second means moving that clock on afterwards.
Without it a sync taken straight after a write records a cursor the write is not yet behind, and every later sync reads the same document again, which the suite reports as an incremental sync that is not incremental.
The fixture over object storage ticks the fake service's clock and the one over the filesystem stamps each file, and both are doing the same thing for the same reason.

The one rule with no way out is permissions.
Every document a connector emits has to say where its access control list came from, and a connector that could not work one out says so with `connector.Unresolved` rather than leaving the field empty.
Both produce `acl.ModeUnknown` and both quarantine the document, and the difference is that one of them says at the call site that the question was considered.
A change that arrives with an empty `Permissions.Source` is a connector that forgot, and it fails the suite.

Running the suite is not optional either.
`TestEveryConnectorRunsTheConformanceSuite` at the root of the module looks for the packages that declare a connector and fails for any of them whose tests do not call `connectortest.Run`.
A conformance suite nobody runs is documentation, and documentation about permissions is the first thing skipped when a connector is written in a hurry against a source somebody needs indexed by Friday.

### Testing against a recording of the real service

A connector for somebody else's product is mostly a reading of that product's API, and the tests that matter are the ones that say what happens when it answers the way it really does.
There are three ways to get that and two of them are bad.
Hand written JSON says what somebody thought the API returns, which is how a connector ends up parsing a field that was renamed two years ago.
A live account makes the suite depend on a network, a token, a workspace somebody has to keep populated and a rate limit, and the test then fails for four reasons that have nothing to do with the change under review.

The third way is `connector/recorded`.
It is an `http.RoundTripper` that talks to the real service once, writes down exactly what it said, and answers out of that afterwards.

```go
rec := recorded.Record(http.DefaultTransport)
// ... drive the connector against the live service once ...
if err := rec.Save("testdata/chat"); err != nil {
	t.Fatal(err)
}

// ... and from then on, with no account and no network:
rt, err := recorded.Replay("testdata/chat")
```

A recording is one JSON file per exchange in a numbered directory, because a crawl is twenty or thirty requests and a change to one of them should be a change to one file.
A JSON body is nested into the file as JSON rather than escaped into a string, so the day the service renames a field the diff is one line and the review is the notice.
The number in front is the order the requests were made in, since a listing sorted alphabetically would put the second page of results before the first.

Two decisions in the matching are worth knowing about before recording anything.
The scheme and the host are not part of what a request asked, so a fixture set describes an API rather than the workspace it was captured from and a test does not have to point its client at somebody's subdomain.
The parameters that carry credentials are not part of it either, which is what lets a recording made with a token answer a test that has none.
A form post is compared field by field for the same reason, because the order a client writes its fields in is not part of the question.

Secrets never reach the file.
The usual headers and query parameters are replaced with `REDACTED` before anything is written, and `recorded.WithScrubber` handles the ones that are in a body instead, such as a signed download URL or an invite link that comes back in a listing.
This matters more than it sounds.
A recording is committed, and a token committed once is a token leaked permanently, whatever the next commit does.

A request nothing was recorded for is an error naming what was asked and what the recording holds, rather than an empty response the connector fails to parse fifty lines away from the cause.
`Unused` reports the other direction, the recorded requests nothing asked for, which is how a fixture set left behind by a connector that stopped calling an endpoint gets noticed instead of being read as a description of what the connector does.
