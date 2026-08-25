# Reading somebody else's access control list

Every system this index reads from models permissions differently.
Some grant to people, some to groups, some to a whole email domain, some to anybody holding a link, and several of them let a refusal override a grant.
None of them agree on what to call any of it.
The same idea is a `reader` in one, `READ` in another, `VIEW` in a third and `BROWSE_PROJECTS` in a fourth.

A connector could map its own system straight onto `acl.Permissions`, and the first two connectors would look fine.
The trouble starts at the edges, and the edges are where a search engine leaks.
Every connector would decide on its own what a grant to a domain means, whether a link share is a grant, and what to do with a statement it does not understand.
The decision that costs a company its private documents is the last one, made once, quietly, by whoever was writing a connector on a Friday afternoon.

So the decisions are made in one package, `connector/aclmap`, and a connector's job shrinks to translating its own vocabulary into `Grant` values.

```go
n, err := aclmap.New(aclmap.Drive("drive", "google", "acme.com"))
...
perms, err := n.Normalize([]aclmap.Grant{
	{Subject: aclmap.User, ID: "alice@acme.com", Role: "owner", Owner: true},
	{Subject: aclmap.Group, ID: "eng@acme.com", Role: "writer"},
})
```

`aclmap` imports `acl` and nothing else of ours, which `arch_test.go` enforces.
A mapping layer that could reach a store or a document would be a second place for the permission rule to live.

## What a connector says

A `Grant` is one statement, in the source's own words, with no interpretation applied.

| Field | Meaning |
| --- | --- |
| `Subject` | `User`, `Group`, `Domain` or `Anyone` |
| `ID` | who the source named, empty for `Anyone` and required otherwise |
| `Effect` | `Allow` or `Deny` |
| `Role` | the source's own word, matched case insensitively, empty means read |
| `Link` | the statement only holds for somebody with a link |
| `Owner` | this user owns the document |

`Normalize` returns a descriptor that is safe to store whether or not the error is nil.
On failure it is `acl.ModeUnknown`, which every query path holds back, so a connector that logs the error and carries on has quarantined the document rather than published it.

An empty list of statements is not a failure.
A document nobody has been given is a document nobody may read, and that is represented exactly: an empty access control list, which allows nobody.

## A refusal beats a grant

A deny in the source is a deny in the index, in every source that has the concept.
Denies aimed at a user or a group go into `DenyUsers` and `DenyGroups`, and `acl.Permissions.Allows` applies them before it looks at anything else.
The order the statements arrived in does not change the answer, which matters because a wiki connector naturally reports the space permissions first and the page restrictions second.

There is no deny list for a domain or for everybody, and there is no safe approximation either.
Treating a refusal aimed at a domain as one aimed at nobody would keep the document readable by exactly the people it was taken away from.
So that statement quarantines the document instead.

## What cannot be mapped is quarantined and counted

Three things fail, and they are counted separately because they want three different actions.

| Reason | What it is | Who fixes it |
| --- | --- | --- |
| `ReasonForeignDomain` | a grant to an email domain that is not this tenant's | somebody has to decide what that domain means here |
| `ReasonUnmappableDeny` | a refusal aimed at a domain or at everybody | usually a source feature nobody has written the mapping for |
| `ReasonMalformed` | a statement naming nobody, such as a user grant with an empty id | a bug in a connector |

A fourth counter, `Ignored`, is not a failure.
It counts statements whose role does not confer read, such as a permission to change a label.
It is worth watching anyway: a sudden climb usually means a source renamed a role and the mapping has not caught up, which shows up to a person as documents they could find yesterday and cannot find today.

A quarantined document is invisible from every other angle.
The sync succeeded, no error was returned to anybody, and the only symptom is somebody who cannot find a document they know exists.
That is why `genbad` logs the counts after every sync when the policy reports them:

```
level=WARN msg="permissions that could not be mapped" mapped=1841 quarantined=12 foreign_domain=12 unmappable_deny=0 malformed=0 ignored_roles=93
```

## Link sharing and public documents are recorded

A document shared by link and a document with no restrictions look the same if all you store is a list of names.
They are not the same thing, so `acl.Permissions` carries what the source said:

| `acl.Sharing` | What the source said |
| --- | --- |
| `SharedNone` | the lists are the whole story |
| `SharedByLink` | anybody holding a link can open it |
| `SharedPublic` | it is out in the open |

`Sharing` is a record and not a rule.
`Allows` never reads it, no storage driver filters on it, and that is deliberate.
The permission rule has exactly one implementation in Go and one in each driver's SQL, and a field that changed the answer would have to be added to both, in agreement, forever.

What a link share means for search is a decision about a company rather than about a product, so it is a setting.
The default, `LinkGrantsNothing`, keeps the document readable by whoever the lists name and records the share.
`LinkGrantsTenant` treats a link share as readable by everybody in the deployment.
It has to be asked for by name, because in a company where turning on a link is not how people publish, it is the setting that hands several years of link shared documents to everybody at once.

## The mappings

Each preset in `connector/aclmap/sources.go` carries one source system's own list of names, taken from its documentation.
A role name is a fact about somebody else's product.
It changes when they change it, and when it does the symptom is documents quietly disappearing from search.
Keeping all of them in one file means the staleness is in one place, next to the table driven tests in `sources_test.go` that pin it.

### Files

A directory tree has two permission models, and which one to read is a question about the tree rather than about the files in it.

| Role | Reads |
| --- | --- |
| `owner` | yes |
| `approver` | yes |
| `reviewer` | yes |

The roles above are the ones a policy over the tree invents, and `OwnersPolicy` is the one that uses them.
The OWNERS convention that Kubernetes and a number of other large repositories keep names approvers and reviewers, and both are people who can read the subtree.
There is no deny and no link sharing.
A subtree narrows what its parent allowed by replacing the list rather than by refusing anybody.
A path with no OWNERS file anywhere above it has no answer at all and is quarantined, which is the case worth getting right: the default for "no rule found" is not "no restriction".

The other model is the one the operating system already keeps, and `OSPolicy` reads it.

| What the file system has | How it is read |
| --- | --- |
| the owner, where the owner read bit is set | a `User` grant marked as the owner |
| the group, where the group read bit is set | a `Group` grant |
| the world read bit | a `Domain` grant, and nothing at all until a domain is named |
| a POSIX access control list | read in place of the mode bits, with the mask applied |
| a Windows access control entry that allows reading | a `User` or `Group` grant |
| a Windows entry that denies reading | a `Deny`, which beats every grant |
| Everyone and Authenticated Users on Windows | the same `Domain` grant as the world bit |

This is the right policy for a tree that is the file server, because there the operating system is the access control system and there is nothing better above it.
It is the wrong policy for a copy of one.
A tree that was rsynced to the crawler carries the permissions the copy has, which are the crawler's own, and indexing those would hand the lot to whoever the crawler runs as.

Three of those rows are worth expanding on.

The read bits are read literally, so an owner who took away their own read bit is not granted the file.
There is an argument the other way, since they could put the bit back, and being wrong this way costs somebody a file of their own they cannot find while being wrong the other way costs a file shown to somebody who was refused it.

The world bit cannot be mapped without being told something first.
It says every account on this host may read the file, and a host's accounts are not a tenant: on a laptop they are one person, on a login server they are the company, and on a machine with a guest account they are more than the company.
So it grants nothing until a deployment names the domain those accounts belong to.

The mask on a POSIX list is a ceiling rather than a decoration, and it is the reason the list is read at all.
The group bits in the mode of a file that carries a list are the mask, not the group's own permission, so a group the mask has taken read away from still looks allowed in the mode.
A mapping built on the mode alone would offer that file to a team who cannot open it.

The gap is macOS and the BSDs.
They keep extended access control lists of the NFSv4 shape, in a place a program can only reach through the C library, and they are not read.
A file carrying one gets the answer its mode bits give, which is narrower than the truth where the list grants and wider than it where the list refuses.
That second case is the one gap in this policy that goes the wrong way, and it is written down rather than left to be found: on those systems this is a good answer for an ordinary tree and not a safe one for a tree somebody has been managing with the access control list editor.

Every identifier is resolved to an account name, and a file whose owner does not resolve is quarantined rather than indexed under a number.
A numeric user id is not an identity.
It means one person on one host and somebody else on the next, so a grant written in terms of one would either match nobody or match the wrong person.

### Object storage

S3's five permissions, and the object stores that copy them.

| Permission | Reads |
| --- | --- |
| `READ` | yes |
| `FULL_CONTROL` | yes |
| `WRITE` | no |
| `READ_ACP` | no |
| `WRITE_ACP` | no |

The distinction matters more than it looks.
`WRITE` on a bucket is permission to put objects into it, not to read them, and a mapping that treated every permission as read would hand a company's private buckets to whoever can upload to them.

The two group grantees are the ones that reach outside a company.
`AllUsers` is the open internet and maps to a public document, reported as an `Anyone` grant.
`AuthenticatedUsers` is every account holder at the provider, which is not this tenant and cannot be enumerated, so a connector reports it as a `Domain` grant to `authenticated-users` and it quarantines like any other foreign domain.

`connector/objectsource` reports these, and it takes the list from one of two places.
`BucketPolicy` reads the bucket's own list once per sync and gives every object the same answer, which is one request rather than one per object and is correct whenever the objects were written by one process and are read by one team.
`ObjectPolicy` reads the list of each object, which is exact and costs a request per object per sync.
Neither is the default, because there is no permissive default: a source built without a policy quarantines every document, so a bucket nobody has thought about yet is loud rather than public.

A canonical user id is used as an identity only when it is all a grant carries.
It names an account at the provider rather than a person and will not match anything somebody signs in with, so an email address is preferred and a display name after that.
Owning an object is not reading it, and the owner is marked as one only where the list already gives that account `READ` or `FULL_CONTROL`.

### Drive

The roles are Drive's own, and all six of them can read the file.

| Role | Reads |
| --- | --- |
| `owner` | yes |
| `organizer` | yes |
| `fileOrganizer` | yes |
| `writer` | yes |
| `commenter` | yes |
| `reader` | yes |

That is worth stating because it is the case where the obvious mapping is right, and somebody will otherwise wonder whether `commenter` was left out by mistake.

This is the source with all four subjects.
A permission can name a user, a group, a domain or anyone, and anyone can be qualified with a link, which is why `LinkPolicy` exists at all.
A grant to the company's own domain is `ModePublicToTenant`.
A grant to a partner's domain names real people this index cannot enumerate, so it quarantines.

### Chat

Chat has almost no vocabulary, because a message's access is the channel's membership and nothing else.

| What the source has | How a connector reports it |
| --- | --- |
| a private channel | a `Group` grant naming the channel |
| a public channel | a `Domain` grant to the workspace's domain |
| a direct message | one `User` grant per participant |

The role is empty on all of them, which is what an empty `ReadRoles` list is for.
A member of a channel reads the channel, and there is nothing finer to say.

### Wiki

Space permissions carry names such as `VIEW` and `EDIT`, and a page can then carry restrictions that narrow the space.

| Permission | Reads |
| --- | --- |
| `VIEW` | yes |
| `READ` | yes |
| `EDIT` | yes |
| `ADMIN` | yes |

Confluence has two kinds of restriction and only one of them is a refusal in the sense used here.
A view restriction listing people replaces the space's answer for that page, which a connector reports as an allow list.
An explicit exclusion is a deny.
A connector that reports the space permissions and then the page restrictions gets the right answer for free, because a deny is applied first whatever order the statements arrived in.

### Tickets

Reading an issue takes the `BROWSE_PROJECTS` permission on its project.

| Permission | Reads |
| --- | --- |
| `BROWSE_PROJECTS` | yes |
| `ADMINISTER_PROJECTS` | yes |

A permission scheme grants those to users, groups and project roles.
A connector reports a project role as a group, because that is what it is: a named set of people maintained by the source.

The part that catches people out is issue security.
A project can carry security levels that narrow an individual issue to a subset of the people who can browse the project, and an issue with a security level set is not readable by the project's list at all.
A connector must report the security level's members and nothing else for such an issue, because reporting both would widen it straight back to the project.

## Asking again while the response is written

Everything above happens during a sync, and what it produces is a copy of somebody else's answer.
Between two syncs the copy can go wrong in the direction that matters: a person is taken off a page on Monday morning and the index still says they may read it until the crawler comes round again.
On a large tree that is hours, and the symptom is a document turning up in search for somebody it was taken away from.

The `recheck` package closes that window by putting the question back to the source while the response is being written.

```go
set := recheck.New(recheck.WithTimeout(20 * time.Millisecond))
set.Add("wiki", recheck.Func(func(ctx context.Context, p *acl.Principal, ids []string) (map[string]bool, error) {
	return wiki.MayRead(ctx, p.Subject, ids)
}))

srv := api.New(st, searcher, auth, api.WithRecheck(set))
```

It is off unless a deployment passes a set, and a set decides one source at a time, because whether a source can answer a permission question in a few milliseconds is a fact about that source.
A source nobody registered a checker for is served from the index exactly as it was before, which is what makes this safe to turn on for one connector and leave off for the rest.

A checker is handed a principal and a list of ids and nothing else.
It is not handed the document, because a check that could read the row it is checking would be reading the stale answer this exists to go around.

### What it costs and what happens when it does not come back

One question covers a whole page, the sources are asked in parallel, and they share a single deadline of 20 milliseconds.
Answers are held for 10 seconds, per person and per document, so a reader paging through results asks each source once.
That is the staleness this leaves behind, and it is a number a deployment sets rather than one that emerges from a stack of caches.

Every way this can go wrong removes the row.

| What happened | What the reader sees |
| --- | --- |
| the source said no | the document is not on the page |
| the source returned an error | the document is not on the page |
| the deadline passed | the document is not on the page |
| the source answered without mentioning the id | the document is not on the page |

The last row is the one worth stating out loud.
An id a source left out of its answer is a source that did not answer, and reading silence as a yes would turn a partial reply into a leak.
None of the four are cached, so a source having a bad thirty seconds costs those thirty seconds and not the next ten minutes.

### Where it runs

Every surface that puts a document in front of somebody: search, suggestions, one document, its content, its thumbnail, the recent list, the reported list and the curated lists.
That is the same set of handlers the audit trail covers, and for the same reason.
A rule that holds on the search page and not on the preview panel is not a rule.

The quotes in a written answer are filtered with the page they were built from.
A quote is a sentence out of a document, so an answer that kept quoting one the source has just withdrawn would be serving its content under a different field name.

A document that does not survive the check is answered the way a document that does not exist is answered, because the alternative confirms it exists.
The access is still written to the audit trail, as a refusal.

Three counters say what the checks are doing, labelled by source.

| Metric | What it is |
| --- | --- |
| `genba_recheck_checked_total` | documents put to that source |
| `genba_recheck_denied_total` | documents it said no to |
| `genba_recheck_failed_total` | checks that errored or ran out of time |

Denied climbing is the feature working.
Failed climbing is a source that is slow or down, and while it is climbing that source's documents are disappearing from search, which is the safe failure and still an incident.

## Adding a source

Write a preset in `sources.go` with the source's own role names and a comment saying where they came from.
Write a table in `sources_test.go` with a row per interesting statement, including the ones that should quarantine.
Add the section here.
Then the connector fills in `Grant` values and never decides anything.
