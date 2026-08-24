# A recorded Confluence site

These files are one crawl of a small Confluence site, written down so the tests
next to them can exercise the whole adapter without an account, a token or a
network.
Each file is one request and the answer it got, numbered in the order the crawl
made them.

The site has two spaces.
`OPS` grants read to a group, and holds a page with two comments from two
different people and a pair of labels, a page written before the editor changed
whose body is storage format with a code macro and a warning panel in it, and a
page nested under a parent and restricted to one account and one group so that a
rule overriding the space it is in is in here too.
`ENG` is readable by anybody signed in, which is the other permission shape a
space can have, and holds one ordinary page.
Between them they cover the space listing, the space permissions, the CQL
search, the cursor paging, the comment assembly, the inline read restrictions,
the second request for an old body and a single page read.

## Refreshing them

    go test ./connector/confluencesource/ -run TestRecordTheFixtures -update

That runs the crawl again against the fake site in `fake_test.go` and rewrites
every numbered file here.
It does not touch this README.

Two things are taken out before the files are written: the address the fake was
listening on, which is replaced with a Confluence Cloud one, and the response
headers that say how long the body was and what time it was.
All of them change on every run and none of them is matched against, so leaving
them in would turn every refresh into a diff on every file.

The repeated requests are dropped as well.
A crawl asks what the spaces are three times, once for the sync, once for the
sweep and once for a fetch, and three copies of the same answer is three files
to review and no more coverage.

## What is not in here

No credentials.
The API token is sent as basic authentication in a header and the header is
redacted before anything is written, and there is a test that reads these files
back and fails if the token, the crawling account or an encoded credential is in
them.

Nothing real either.
The people, the spaces and the pages are made up, and the site they came from is
the fake in `fake_test.go` rather than anybody's Confluence.
