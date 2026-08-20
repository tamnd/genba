# A recorded Slack workspace

These files are one crawl of a small Slack workspace, written down so the tests
next to them can exercise the whole adapter without an account, a token or a
network.
Each file is one request and the answer it got, numbered in the order the crawl
made them.

The workspace has three channels.
`general` holds one message, `line-two` holds a thread with two replies on it
and a join notice we refuse to index, and `safety` is private with two members.
Between them they cover the listing, the paging, the thread assembly, the name
lookups and the membership read.

## Refreshing them

    go test ./connector/slacksource/ -run TestRecordTheFixtures -update

That runs the crawl again against the fake workspace in `fake_test.go` and
rewrites every numbered file here.
It does not touch this README.

Two things are taken out before the files are written: the address the fake was
listening on, which is replaced with Slack's, and the response headers that say
how long the body was and what time it was.
All three change on every run and none of them is matched against, so leaving
them in would turn every refresh into a diff on all eleven files.

The repeated requests are dropped as well.
A crawl asks what the channels are three times, once for the sync, once for the
sweep and once for a fetch, and three copies of the same answer is three files
to review and no more coverage.

## What is not in here

No credentials.
The token is sent as a bearer header and the header is redacted before anything
is written, and there is a test that reads these files back and fails if
anything shaped like a Slack token is in them.

Nothing real either.
The people, the channels and the messages are made up, and the workspace they
came from is the fake in `fake_test.go` rather than anybody's Slack.
