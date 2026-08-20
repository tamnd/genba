# A recorded Jira site

These files are one crawl of a small Jira site, written down so the tests next
to them can exercise the whole adapter without an account, a token or a network.
Each file is one request and the answer it got, numbered in the order the crawl
made them.

The site has two projects.
`LINE` grants browse through a project role, and holds a ticket with a code
block in its description, two comments from two different people, an assignee
and a status that was moved, plus a second ticket with nothing special about it.
`SAFETY` grants browse to a group, and its one ticket sits behind an issue
security level so that a rule overriding the project it is in is in here too.
Between them they cover the project listing, the permission scheme, the role
resolution, the security level members, the JQL search, the offset paging, the
comment assembly and a single issue read.

## Refreshing them

    go test ./connector/jirasource/ -run TestRecordTheFixtures -update

That runs the crawl again against the fake site in `fake_test.go` and rewrites
every numbered file here.
It does not touch this README.

Two things are taken out before the files are written: the address the fake was
listening on, which is replaced with a Jira Cloud one, and the response headers
that say how long the body was and what time it was.
All of them change on every run and none of them is matched against, so leaving
them in would turn every refresh into a diff on every file.

The repeated requests are dropped as well.
A crawl asks what the projects are three times, once for the sync, once for the
sweep and once for a fetch, and three copies of the same answer is three files
to review and no more coverage.

## What is not in here

No credentials.
The API token is sent as basic authentication in a header and the header is
redacted before anything is written, and there is a test that reads these files
back and fails if the token, the crawling account or an encoded credential is in
them.

Nothing real either.
The people, the projects and the tickets are made up, and the site they came
from is the fake in `fake_test.go` rather than anybody's Jira.
