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

## What is not here yet

Serving a stale answer through a directory outage.
The cache refuses once an entry has expired and the directory cannot be reached, which is the same direction the resolver fails in, and an operator who would rather serve a slightly old group set than refuse a sign in has no way to say so yet.
It is a real choice with a real cost on the other side, so it should be a configured window with its own metric rather than a default.

Adapters for the hosted providers.
The interface is two methods and the suite is what they have to pass, so each is a small amount of code and a fake, in the shape the connectors already use.
