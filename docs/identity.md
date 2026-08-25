# Turning a person into a set of groups

Every permission decision in this system is set membership on two lists of strings.
A document carries the references its source granted read to, a request carries the references that name the person asking, and the answer is whether the two sets intersect.
That is deliberately dull, it is written down once in `acl`, and a storage driver evaluates the same comparison in its own query language so the filter runs while the driver walks its own data.

`docs/permissions.md` is about the first list, the one a source produces.
This is about the second, which is harder for a reason that has nothing to do with us: it is a reachability question over a graph that somebody else maintains, changes without telling us, and did not design to be walked.

## Nobody grants access to a person

They grant it to `engineering`, which contains `platform`, which contains `storage`, which contains four people and a service account.
The person who joined last week can read the runbook because of a group nobody named in a rule anywhere.

So the question "which groups is this person in" is not a field lookup.
It is a transitive closure, and every real directory has three properties that make walking one interesting.

It is big.
Somebody at a large company is in a few hundred groups and the tail is in the thousands, and the expansion happens when a session starts, in front of a person waiting for a search box to work.

It has cycles.
Not because anybody meant to: two groups end up inside each other through a third one that was added during a reorganisation, and the directory does not mind because it never walks the graph the way we do.
A walk written without a seen set hangs, and it hangs in production rather than in a test, because the test directory is a diagram somebody drew on purpose.

It changes.
Somebody is removed from a group at eleven, and what matters is not how quickly that is noticed but that everything derived from the old answer stops being used the moment it is.

## What a provider has to answer

`directory.Directory` is two lookups and that is the whole interface.

```go
type Directory interface {
	Name() string
	Subject(ctx context.Context, id string) (Subject, error)
	Group(ctx context.Context, id string) (Group, error)
}
```

Okta, Entra ID, Google Workspace and LDAP disagree about almost everything else, and every one of them will say what one subject is directly a member of and what one group is directly a member of.
Everything above that is written once and shared: the closure, the cycle detection, the bound on how much work one expansion may cost, the concurrency, and the version the answer is stamped with.
Those are exactly the parts that are easy to get subtly wrong and impossible to notice from the outside, and leaving them to each adapter would mean four implementations of a graph walk written by four people who each needed one provider.

A provider that already expands transitively, and Entra ID does, is not penalised for it.
It returns the closure it was given, the walk finds nothing new above it and stops after one level.
A provider that only knows direct edges gets the same answer for more requests.

`Group` deliberately does not carry its members.
Listing them is the expensive direction, it is the one large directories rate limit hardest, and nothing here needs it: the walk goes upwards from a person, never downwards from a group.

## The walk

Breadth first, one level at a time, with the lookups within a level running together.
Breadth first rather than depth first is what makes the concurrency worth having, because the groups at one level are independent of each other and the ones above cannot be named until this level has answered.

A group already seen is never looked up again, which is both the cycle detection and most of the speed.
Real directories converge hard: a few hundred groups at the bottom reach the same dozen at the top, and a diamond costs one lookup rather than two.

The width is sixteen by default and it is a promise to the far side rather than a tuning knob.
A directory is shared by every session that starts, so an expansion that goes as wide as it can is an expansion that makes every other expansion slower.

The closure is bounded at ten thousand groups, and reaching the bound refuses rather than truncating.
A truncated group set is not a smaller answer, it is a wrong one, and the way it is wrong is that somebody stops seeing documents they can read with no error anywhere to explain it.
Ten thousand is far past what anybody is legitimately in, so it is a limit that catches a mistake rather than one somebody has to think about.

## The version is a fingerprint, not a clock

`acl.GroupSet` carries a version, and it is part of the cache key of every bitmap derived from the set.
When somebody is removed from a group the version moves, every key derived from the old membership stops matching, and the cached state is dead the moment the change lands rather than when a timer expires.

The number is a hash of the answer rather than a counter: the source, the subject's own revision if the directory keeps one, and every group in the closure with whatever revision the directory gave for it.

Two consequences are worth stating.
A directory with no revision of its own still gets correct invalidation, because the group names are in the hash and taking somebody out of a group changes them.
And a membership change somewhere in the directory that does not change this person's closure does not invalidate this person's state, which is the difference between a cache that works at a company with fifty thousand groups and one that does not.

## Which way it fails

A directory that cannot be reached fails the expansion.
It does not return an empty group set, and nothing in the path turns the error into one, because an empty group set is a valid answer that happens to mean "this person is in no groups at all" and everything above is built to trust it.
Refusing is visible and recoverable.
Quietly demoting somebody to no groups is a support ticket about missing search results, and on the sources that grant to a person directly it is a partial answer that looks complete.

A subject the directory has never heard of is refused, and so is one it holds and has deactivated.
Those are two different facts and both are a no: an account closed on Friday should stop resolving on Friday rather than when the last cached entry expires, and directories rarely delete anybody, so the deactivated case is the one that actually happens.

A group named in a membership that the directory does not hold is the one case that does not refuse.
The directory has said this person is in it, which is a statement about them, and the only thing missing is what that group is itself a member of.
Dropping it would take away access the directory granted on the strength of a lookup that failed, so the group is kept, its parents are not walked, and it is reported in `Expansion.Unknown` and counted.
A non empty `Unknown` is an inconsistent directory and worth an alert, and it is not worth failing a sign in over, which is why it is a field rather than an error.

## At the edge of the API

`api.Resolving` wraps an authenticator so that the group membership on a request comes from the directory rather than from whoever authenticated it.

```go
s := api.New(st, searcher, api.Resolving{
	Auth:     api.HeaderAuth{Tenant: "acme", Admins: []string{"u_mei"}},
	Resolver: resolver,
})
```

Who this is comes from a credential and is the authenticator's job.
What they are a member of is a fact about the company, it changes without anybody signing in again, and no token is a good place to keep it.
A proxy that passes a group list down is passing on a copy of an answer somebody else cached, and the day somebody is taken out of a group is the day the copy is wrong.

So the group set is replaced entirely rather than merged.
If the directory is the answer then a header saying otherwise is not a second opinion, it is a way in.
Identities are merged rather than replaced, because those legitimately come from two places: a token can say which Slack account signed in and the directory can say which Jira account is the same person.

A directory outage answers 503 rather than 401.
The credential was fine and the request is refused anyway, and an operator watching a wall of 401s during a directory outage would go looking at the wrong system.

## The conformance suite is the definition

`directory/directorytest` is what an adapter has to pass, and it rather than the interface is the definition of one.

A directory adapter is a small amount of code in front of somebody else's API and the damage it can do is out of all proportion to its size.
Each one is written by whoever needed that provider, usually against one tenant with a tidy directory, and the failure modes only appear against one that is not: a group inside itself, a membership naming a group deleted last year, an account deactivated on Friday, a person in four hundred groups.
None of that is visible from above.
Somebody quietly given nobody else's groups gets fewer results and blames the search engine.
Somebody quietly given somebody else's groups does not notice at all.

The suite exercises the adapter directly for the answers only it can give, and exercises the shared resolver over the same adapter for the properties that only appear once the graph is walked, because an adapter is never used without the resolver.

```go
func TestConformance(t *testing.T) {
	directorytest.Run(t, func(t *testing.T) directorytest.Fixture {
		d := directory.NewStatic("acme")
		return directorytest.Fixture{
			Directory: d,
			Put:       func(_ *testing.T, s directory.Subject) { d.Put(s) },
			PutGroup:  func(_ *testing.T, g directory.Group) { d.PutGroup(g) },
			Drop:      func(_ *testing.T, group string) { d.RemoveGroup(group) },
		}
	})
}
```

Three things about a fixture are not the same for every provider and the suite asks rather than assumes.

`Flat` says the provider's groups cannot contain groups, which skips the cases about walking a graph and takes a level off the arithmetic in the two that count lookups.
`Transitive` says the opposite thing, that the provider nests and hands over the whole closure itself, which runs every case and changes one number: a nest of any depth is walked one level.
That number is asserted rather than tolerated, because a provider claiming to expand transitively and then costing three levels on a three level nest is one that is not doing what it says.
`Identity` says which identity the provider can actually be made to hold, because a directory answers in its own vocabulary and no amount of putting a Slack id on an Okta user will make one come out.
All three default to the shape `directory.Static` has, so nothing that already passes the suite has to say anything, and what the cases are about survives either way.

`directory.Static` is the reference implementation the suite runs against, and it is also the directory a small deployment actually uses.
A company with forty people and six groups has the whole thing in a file, and making them stand up an identity provider to try a search engine is how a search engine does not get tried.

## The file

`genbad -directory` points at it, `directory.OpenStatic` reads it, and the README has the shape.

It is strict, and that is the point of it being a file.
An unknown field is a typo rather than a future version of the format, a group named in a membership and not defined is a typo too, and both refuse at startup rather than turning into somebody mysteriously missing a group at nine o'clock.
An identity provider cannot offer that, because its mistakes are in somebody else's data.

`-directory-refresh` reads it again on a ticker and swaps the whole thing in at once, so nothing ever reads a directory that is half of the old file and half of the new one.
A reread that finds the same bytes costs a hash rather than a parse, which is what makes a short interval affordable.
A reread that finds a change flushes the cache, everybody rather than one person, because the file changed and nothing in it says who was affected.

Two things fail rather than apply.
An edit that does not parse leaves the last good directory in place and logs, because an operator halfway through a change should not take everybody's groups away, and refusing every request until the file is valid again turns a typo into an outage.
A change to the directory's own name is refused outright: every group key carries the name, so renaming it renames every group in every rule at once, and that is a different directory rather than an edit to this one.

A file that does not parse at startup is a different matter and the process exits.
Nothing is loaded, so there is nothing to keep, and coming up resolving nobody would be a server that answers every request with a refusal.

## Okta

`directory/okta` is the first adapter for a hosted provider, and it is the shape the others take.

It answers the two lookups and nothing else.
The closure, the cycle detection, the bound on what one expansion may cost and the version an answer is stamped with are all in the shared code, because those are exactly the parts that are easy to get subtly wrong and impossible to notice from the outside.

The one fact about Okta that shapes the rest is that its groups do not contain groups.
A user is a member of groups and a group is a member of nothing, so there is no graph to walk and every expansion is one level deep.
That is also why the conformance fixture sets `Flat`, which skips the cases about walking a graph.
An adapter that passed those would be one that had invented a nesting the provider does not have.

Flat has a cost, though.
The walk asks about every group the person is in, and a person in three hundred groups would be three hundred requests against an organisation everybody else is signing in to at the same time.
So the group listing that answers the subject lookup, which returns whole group objects rather than ids, fills a small buffer that the group lookups then read, and the three hundred requests become one listing.

That buffer is worth being careful about, because there is meant to be exactly one cache in this system and it is not this.
The fact being held here is "this group exists and is a member of no groups", and for a provider with no nesting that fact cannot become false in a way that changes an answer.
A group deleted since it was buffered would move into the unresolved list if it were looked up again, and an unresolved group is still in the group set, because the directory saying somebody is a member is a statement about them.
So the buffer changes what an expansion costs and cannot change what it returns.

The version is the other decision worth writing down.
Okta sends two revisions on a group, `lastUpdated` and `lastMembershipUpdated`, and the second one is the obvious choice and the wrong one.
It moves whenever anybody at all joins the group, and what this version invalidates is one person's group set, which somebody else joining does not change.
At a company of any size something moves every second, so a version derived from it would be correct and useless: nothing cached above it would survive a working day.
This person joining or leaving is caught without it, because the group set is fingerprinted over the group ids as well as their versions, and joining or leaving is what changes the ids.

Deactivation is the third.
Okta has eight account states and only some of them are a decision somebody made about access.
`SUSPENDED` and `DEPROVISIONED` are what an administrator reaches for when somebody leaves, and `STAGED` is an account created and never activated, so all three refuse.
`LOCKED_OUT` and `PASSWORD_EXPIRED` do not, because both are a live account having a bad morning, and refusing on them would take somebody's search away for forgetting a password.

Rate limits are the transport's job rather than the adapter's.
`connector/limit` is the same one every connector uses, and Okta publishes its numbers on every response.
It spells the headers `X-Rate-Limit-Remaining` and `X-Rate-Limit-Reset`, with the hyphen in a different place from everybody else, which canonicalises to a different header name entirely.
A transport that read only the other three spellings would see an organisation sending its numbers on every single response as one sending none at all, and would find the edge of the quota by hitting it.

## Entra ID

`directory/entra` is the second adapter, and it is the interesting one because the provider does half the work.

Entra groups nest, and unlike most providers Microsoft Graph will walk the nesting for you.
The `transitiveMemberOf` collection is every group a person is in however deeply that membership is inherited, so somebody eight levels down a tree is one request rather than eight rounds of them.
That is why the conformance fixture sets `Transitive` and why `Group` answers with an empty membership.
It is not that a group here is a member of nothing, it is that whatever it is a member of already came back with the subject, and returning the parents again would have the resolver walk a graph it has already been handed.

The cast on the end of the path is load bearing.
Without `microsoft.graph.group` the collection also returns directory roles and administrative units, which are directory objects a person belongs to and are not groups.
Their ids would land in the group set beside the real ones, and a rule naming one would then allow everybody who holds that role.
The cast asks the service for groups only rather than filtering after the fact, so the ids that arrive are the ids that belong there.

Every property the adapter reads is named in a `$select`, and one of them is the reason.
Graph answers a user lookup with a default set of properties and `accountEnabled` is not in it.
An adapter that reads the answer without having asked for that field gets the zero value, which is false, or gets nothing at all and calls it true, depending on how it is written.
The first refuses everybody in the tenant and the second keeps resolving the groups of people who were deactivated months ago, and neither looks like a bug in the place it happens.
The fake next to the adapter fails the test when a user lookup arrives without that field named, so the whole suite enforces it rather than one case.

The version is where this provider differs from Okta most.
The v1.0 Graph exposes no last modified time on a user or on a group, so there is no revision to copy.
The group version is empty, and here that is complete rather than a gap: a version on a group exists to catch a change that alters somebody's group set without altering the ids in it, which for a nesting provider means a group being moved under another one, and that move arrives as a different closure because the closure is what the provider returns.
Hashing the display name in instead would invalidate every member's cached group set for a rename that alters no answer.
The subject version is a hash of exactly the fields the adapter reports about the person, because those are the ones the closure does not cover: a new alias has to reach the principal and the group ids do not move when one is added.

Two smaller decisions.
A guest's principal name is their address with the at sign replaced and the host tenant stuck on the end, which looks enough like an address to be mistaken for one and is nobody's mailbox, so it is not an email identity and their `mail` is.
And a lookup for something that is neither an id nor a principal name is refused with a bad request rather than a not found, so both are read as a person the tenant does not hold, since treating the first as a failure would turn one bad row in a store into an expansion that never succeeds.

Rate limits work the other way round from Okta.
Graph says nothing about what is left until it refuses, and then answers a 429 or a 503 with a `Retry-After`.
So the transport can only hold back after a refusal rather than before one, and the circuit breaker matters more for the same reason: a tenant that has started refusing wants to be left alone for a while, and the only way to learn that is to have been told once and remember it.

Signing in is part of the adapter because a Graph token lives about an hour.
`entra.Token` is a string somebody else keeps fresh and `entra.NewApplication` is the client credentials grant, which holds a token until five minutes before it expires and then replaces it.
It acquires one at a time on purpose: an expansion looks a level up in parallel, so the moment a token expires is the moment several goroutines notice at once, and letting each of them go and get their own would turn every expiry into a small burst against the endpoint most likely to throttle.

## Google Workspace

`directory/google` is the third adapter, and it is the first one where the provider does none of the walking.

Google groups nest and the Admin SDK will not expand the nesting, so this is the ordinary case the resolver was written for: the adapter answers what one key is directly a member of and the resolver goes up a level at a time.
That is why the conformance fixture here sets neither `Flat` nor `Transitive`, which the other two adapters each set one of.

One collection answers both lookups.
`GET /groups?userKey=` returns the groups a key is directly in, and the key is allowed to be a group as well as a person, which is the fact the whole adapter is built on.
Without it a nesting provider would need a second collection with different paging and different failure modes to answer the group lookup, and the two would disagree about something eventually.

The other direction was the obvious way to do it and it is the wrong one.
The members collection takes `includeDerivedMembership` and will happily hand back the closure, but it answers from a group towards its people rather than from a person towards their groups.
Expanding one person that way means reading the membership of every group they might be in, so a company wide group turns one sign in into a walk over every employee.

The buffer holds less here than it does in the other two adapters, and it is worth being precise about what it holds.
A listing carries the full group objects, so the group lookups that are about to arrive for those ids are already answered and cost nothing.
It says nothing about what those groups are inside, which is exactly the thing the next level of the walk needs, so a person in twenty groups is one listing plus twenty listings and not one plus forty requests.
Two tests state that in both directions, with the buffer on and with it off.

The version is a hash of the etag and of the fields the adapter reports, and both halves are load bearing.
Google has been dropping etags across its APIs, and an adapter that hashed only the etag would go quiet on a domain that stopped sending them, which is a version that never moves and a cache above it that never invalidates.
The group version also folds in the ids of the groups that group is inside, because the etag on a group does not move when somebody puts it under another one, and for a nesting provider that move changes the group set of everybody underneath it.

Deactivation has two spellings.
A suspended account is the obvious one and an archived account is the one that is easy to miss, since archiving is what an administrator reaches for when somebody leaves and the licence is being reclaimed.
Either one refuses, and a refused subject is refused before their groups are read rather than after.

Identities are the id, the primary address, the aliases and every address in `emails`.
`nonEditableAliases` is deliberately not among them: those are the automatic ones the domain generates, they are addresses nobody was given, and a rule written against one would be a rule about the domain's naming rather than about a person.

A key the service will not even look at comes back as a bad request with a reason of `invalid`, and that is read as nobody rather than as a failure, for the same reason it is in the Entra adapter.
One malformed row in a store should cost that row and not the whole expansion.

Rate limits are the one place this provider made the shared transport grow something.
The Admin SDK refuses a caller who is over the rate with a 403 rather than a 429, and the only thing separating that from an account which genuinely may not read the directory is a reason string in the body.
Retrying every 403 would hammer a service over permissions that are not going to change, and retrying none of them turns a throttle that would have cleared in two seconds into a sync that failed.
So `connector/limit` gained `WithThrottled`, a predicate the transport consults only for a refusal it was not already going to retry, and `limit.Peek`, which reads the first few kilobytes of a body and puts them back so the connector above still gets the whole answer.
The adapter treats `quotaExceeded`, `rateLimitExceeded` and `userRateLimitExceeded` as throttles.
`dailyLimitExceeded` is not among them, because the window that clears it is tomorrow and no backoff this transport is willing to wait is going to reach it.

Signing in is a service account acting for an administrator.
The account has no directory of its own, so on its own it resolves nothing, and what makes it work is domain wide delegation: an administrator grants the account's client id a list of scopes, and the account then asks for a token on behalf of a named person.
That name is the `sub` claim in the signed assertion and it is the whole of the delegation as far as this process is concerned.
`NewServiceAccount` refuses an empty one at construction rather than defaulting it, because the failure it produces otherwise is an `unauthorized_client` from the token endpoint, which is the same answer an ungranted scope gives and is not guessable from the status.
The private key is parsed at construction for the same reason, so a truncated or encrypted key file stops a deployment from starting instead of becoming a directory outage the first time somebody searches.
`CredentialsFromJSON` reads the key file the console hands over, and it exists so that the key stays in a file, since a private key is the one thing here that must not arrive on a command line where it is in the process listing for everybody on the host.

## More than one directory

`directory.Multi` unions several of them into one group set, and `genbad -directory` takes a list of files.

The shape it exists for is a company that acquired another company.
There are two identity providers, nobody is going to merge them this quarter, and there is one search box.
Half the people are in one directory, half are in the other, and a few are in both because they were given an account on the other side during the integration.
Nothing collides, because a group key already carries the name of the directory it came from, and `engineering` at one company and `engineering` at the other are two different groups here for the same reason they are two different groups in real life.
Two directories under one name are refused when the union is built, since that is the one arrangement where a rule naming one of them would match the other.

If any directory fails, the expansion fails.
This is the same rule the walk inside one directory follows and it matters more here, not less.
With several providers there are several things that can be having a bad day, and a person whose second directory timed out looks exactly like a person who is only in the first one.
Serving them the groups that did answer would take away half of what they can read with nothing anywhere to say why.

A directory that does not hold the subject at all is not a failure, because most people are in one directory of several, and a subject every directory refuses is `ErrNoSubject`.
A subject one directory holds and has deactivated refuses the whole expansion even where another directory still has them active, because deactivating an account is a statement somebody made on purpose.
During a migration, take the old directory out of the list rather than leaving deactivated accounts in it.

The cache goes above the union rather than under it.
One cache over a `Multi` is one entry per person and one lifetime to reason about.
One cache per directory is the same staleness bound, several times the entries, and an expansion that is a hit only when every part of it is.

## Remembering the answer

Expanding on every request is not affordable.
A person in three hundred groups costs three hundred lookups against a service everybody else is also using, and a search box that feels instant cannot start by doing that.

So `directory.Cache` wraps a resolver and holds what it produced, keyed by the subject, for a configured lifetime.
Concurrent requests for the same person do one expansion between them, which matters here more than in most caches: everybody arrives at nine o'clock, and a cold cache and a thousand people is a thousand walks over the same directory.

There is exactly one layer and that is the interesting decision.
The obvious second one, remembering what each group is a member of, would help, because real directories converge and the same dozen groups sit above everybody.
It would also mean an answer could be built out of edges that were themselves already a minute old, so the worst case age of a group set would be the sum of two lifetimes rather than one.
That is a staleness bound nobody can state without drawing a diagram, and a bound nobody can state is one nobody is holding anyone to.
One layer, the whole expansion, and the maximum age of any group set is the lifetime.
`Cache.Staleness` returns it and the metrics publish it, because a promise that only exists in a configuration file is one nobody is checking.

The lifetime and the version are two mechanisms doing two different jobs and they are easy to confuse.

The lifetime bounds how long it takes to notice a membership change.
Nothing else can, because a directory does not call to say somebody was removed from a group, so noticing means asking again.
A minute is the default, and it is a minute rather than five because this is a permission input and the hit rate barely moves between the two.
The traffic that matters is one person making forty requests while they are looking at something.

The version bounds how long anything built on top of a group set outlives it.
Every bitmap, every filter and every cached result keyed by the group set carries the version, so the moment a refreshed expansion produces a different closure, all of it stops matching at once.
Without that, a minute of staleness here would be an unbounded amount of staleness in every layer above, and the person removed from a group would keep its documents until something unrelated happened to expire.

`Cache.Forget` drops one person, and it exists because there are moments when waiting for the lifetime is the wrong answer and they are the moments that matter.
Somebody is taken out of a group during an incident, an account is closed, a provider sends a webhook.
Those are about one person, and flushing everybody to deal with one person means a sign in storm against the directory at exactly the time nobody wants one.

Errors are never held.
A directory that was unreachable for a moment is not a fact about a subject, and remembering it would turn a blip into a minute of refusals.
A refusal the directory meant, a subject it does not hold or has deactivated, still refuses on every request, because that one is an answer rather than a failure.

## Naming a provider in a file

All three adapters are reachable from Go and that is fine while there is one of them and it is being written.
It is not fine once there are three, because a deployment that wants its groups from Okta should not have to import a package and build its own binary.

`-directory` keeps its spelling exactly, because it already means a comma separated list of files unioned by `Multi`.
A file is either a directory written out in full or a description of a hosted one, and the two are told apart by reading them.
That is what makes a mixed list work: a company with an Okta organisation and forty contractors in a JSON file gets one flag value, and it works because it was always a list of files rather than because anything was added for the case.

The alternative was a spelling on the flag itself, `okta:acme.okta.com`, and it loses twice.
A credential cannot go in it, since argv is readable by every process on the machine, so the secret would have to arrive from an environment variable named after the source and now there are two places to look.
And a national cloud, a page size or a second organisation turns one flag value into a query string nobody can read.

The description never carries the credential.
`credential_file` is a path whose contents are the credential and `credential_env` names an environment variable holding it, and a description that carries one inline is refused with a message saying which of the two to use.
That last part is a field that exists only to be refused, because it is the mistake somebody is going to make and `unknown field` is not an answer to it.

A description is checked at startup by asking the provider about a subject who does not exist.
The answer that means everything is working is that they do not exist, which is the cheapest lookup any of these APIs has, and anything a provider will answer at all took a credential it accepted.
A credential the provider refuses stops the server coming up, because a server that starts and then refuses every sign in looks like an outage in the search engine rather than a token somebody forgot to rotate.

There is no reload loop on a description, unlike a file.
It names a service rather than a set of people, and the people behind it change without it changing, which is what the cache above the union is for.

## What is not here yet

Serving a stale answer through a directory outage.
The cache refuses once an entry has expired and the directory cannot be reached, which is the same direction the resolver fails in, and an operator who would rather serve a slightly old group set than refuse a sign in has no way to say so yet.
It is a real choice with a real cost on the other side, so it should be a configured window with its own metric rather than a default.

The rest of the hosted providers.
Okta, Entra ID and Google Workspace are done, and LDAP is the one that is not.

A description that names more than one thing.
One file is one provider, so a company with two Okta organisations writes two files, which is fine, and a provider that wanted a list of domains rather than one would not fit at all.
Nothing wants that yet and the shape it should take depends on which provider asks for it first.
