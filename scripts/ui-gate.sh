#!/bin/sh
# The browser half of the performance gate.
#
# It starts the real binary over a real corpus, which is this repository, and
# audits fourteen screens: the home page, the home page over an index holding
# nothing, a results page, a results page with an answer quoted above it, a
# results page with a written answer above it, a results page with the document
# drawer open, a search that matched nothing, a filter that matched nothing, a
# grid of pictures, a document on a page of its own, the recent screen, the
# settings screen, administration and the answers screen. They are
# states rather than layouts, because a layout is looked at every day and a
# state is looked at once. Auditing a static fixture would audit the fixture,
# and every accessibility bug this is meant to catch lives in the markup the
# interface builds after a fetch.
#
# The keyboard walk, the rendering check, the states check, the budgets and axe
# are the parts that fail a build. axe reports violations of a standard rather
# than an opinion, it does not move when the runner is busy, and a violation is
# a person who cannot use the page. The walk presses the keys and clicks the
# links a person would and asserts where each one led, which is the half axe
# cannot see: markup can describe itself perfectly and still go nowhere when you
# click it. The states check drives the screens that are not the happy path,
# which is where an interface either has a way out or does not. The budgets are
# counts, of files fetched and nodes built, and a count does not flake.
#
# Lighthouse and the cold latency report are advisory. A Lighthouse performance
# score on a shared runner moves by ten points between two runs of the same
# commit, and Lighthouse is enforced on the nightly run instead. Set
# GENBA_UI_STRICT=1 to have it fail. The latency report is advisory for the
# other reason: it is over its budget on purpose, and it prints so that the
# number is watched while the work that fixes it is done.
set -eu

BIN=${BIN:-bin/genbad}
PORT=${PORT:-8123}
BASE="http://127.0.0.1:$PORT"
# The second server holds nothing, because an index with nothing in it is a
# state somebody sees on their first day and it cannot be reached from a server
# that has a corpus.
EMPTY_PORT=$((PORT + 1))
EMPTY="http://127.0.0.1:$EMPTY_PORT"
TENANT=demo
LIGHTHOUSE_MIN=${LIGHTHOUSE_MIN:-95}
SERVER=
BLANK=
# Where the two databases go. The gate runs on the driver a deployment runs on,
# not on the reference one: see the comment above the servers below.
STATE=$(mktemp -d -t genba-ui-gate.XXXXXX)
# Where the pictures go. It is inside the corpus because the corpus is this
# directory, it is gitignored, and it is not a dotted name because the file
# connector skips anything that starts with a dot and would index none of it.
IMAGES=${IMAGES:-gate-images}

cleanup() {
	for pid in $SERVER $BLANK; do
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
	rm -rf "$IMAGES" "$STATE"
}
trap cleanup EXIT INT TERM

if ! command -v node >/dev/null 2>&1; then
	echo "ui-gate: node is not on the path, so nothing that needs a browser can run"
	echo "ui-gate: the asset budgets and the markup safety tests already ran, and they are the part that never flakes"
	exit 0
fi

if [ ! -x "$BIN" ]; then
	echo "ui-gate: $BIN is not there, run make build first" >&2
	exit 1
fi

# Anything else on this port answers the health check and then serves its own
# pages to the audit, while our server exits because it could not bind. The
# failure that produces blames the corpus, which is a long way from the truth,
# so the port is checked before anything is started.
for url in "$BASE" "$EMPTY"; do
	if curl -fsS -o /dev/null "$url/healthz" 2>/dev/null; then
		echo "ui-gate: something is already listening on $url, set PORT to a free one" >&2
		exit 1
	fi
done

# ready waits for a server to answer its health check and then for it to finish
# reading its corpus, and gives up if the process it is waiting for is no longer
# there.
#
# The second wait is the one that matters here. The server binds its port before
# the first read is done and answers out of a part filled index while it runs,
# which is the right thing for a deployment and the wrong thing for a gate:
# every count this script asserts on, and every screen axe audits, would be
# measured against however much of the corpus had been read when the browser got
# there. That is not a number that means anything twice. So the gate waits for
# all of it, and the interface state during the read is audited on purpose by
# scripts/state-check.mjs, which builds it rather than racing for it.
ready() {
	i=0
	until curl -fsS "$1/healthz" >/dev/null 2>&1; do
		alive "$1" "$2"
		i=$((i + 1))
		if [ "$i" -gt 100 ]; then
			echo "ui-gate: the server on $1 never became healthy" >&2
			exit 1
		fi
		sleep 0.2
	done

	i=0
	while curl -fsS "$1/readyz" 2>/dev/null | grep -q '"indexing":true'; do
		alive "$1" "$2"
		i=$((i + 1))
		if [ "$i" -gt 600 ]; then
			echo "ui-gate: the server on $1 was still reading its corpus after two minutes" >&2
			exit 1
		fi
		sleep 0.2
	done
	# A server that died mid read stops matching too, and the loop above cannot
	# tell that apart from one that finished.
	alive "$1" "$2"
}

# alive fails the gate if the process being waited for has gone. Waiting the
# full timeout for a server that exited a second in reports a timeout, and a
# timeout is the one failure nobody reads the logs for.
alive() {
	if ! kill -0 "$2" 2>/dev/null; then
		echo "ui-gate: the server on $1 exited before it was ready" >&2
		exit 1
	fi
}

# The repository holds no images, so the grid and the thumbnail endpoint would
# have nothing to audit. Two dozen generated pictures are written into the corpus
# before it is indexed and removed by the trap above, whatever the gate exits on.
node scripts/gate-images.mjs "$IMAGES" 24

# The corpus is this repository. It is a few hundred documents of real prose and
# code, which is enough for a results page with snippets, facets and a drawer,
# and it needs nothing downloaded.
#
# On SQLite, which is the point. The memory driver is the reference one: it has
# no index of its own, so it is walked document by document and the ranking
# happens above it, re-running the analyzer over every matching body. On this
# same corpus, on the same machine, in the same minute, that measured at a p50
# of 1.8 seconds against 17 milliseconds for SQLite. Every latency number this
# gate has ever printed was a number for a driver nobody deploys, which is worse
# than printing none: it read as the product being two hundred times over its
# budget, and the budget was never being watched on the path anybody is on.
"$BIN" -addr "127.0.0.1:$PORT" -store sqlite -dsn "$STATE/corpus.db" \
	-tenant "$TENANT" -corpus . -corpus-name repo -admins dev -log-level error &
SERVER=$!

# The same binary with nothing to index. It is one more process rather than a
# fixture because the empty state is what the interface does with an answer of
# no documents, and an answer of no documents has to come from the server.
"$BIN" -addr "127.0.0.1:$EMPTY_PORT" -store sqlite -dsn "$STATE/empty.db" \
	-tenant "$TENANT" -log-level error &
BLANK=$!

ready "$BASE" "$SERVER"
ready "$EMPTY" "$BLANK"

# The drawer is opened by an id in the URL, so one has to be looked up. The
# headers are what a trusted proxy would pass down, and the corpus is readable
# by the whole tenant, so any subject in it will do.
ID=$(curl -fsS "$BASE/api/v1/search?q=cache&limit=1" \
	-H "X-Genba-Tenant: $TENANT" \
	-H "X-Genba-Subject: dev@example.com" \
	-H "X-Genba-Groups: everyone" |
	grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
if [ -z "$ID" ]; then
	echo "ui-gate: the corpus produced no results, so there is no results page to audit" >&2
	exit 1
fi

# A document id carries a source prefix and a path, so it goes through the URL
# encoder rather than into the string. The id itself is kept as it is, because a
# citation inside a JSON body is the id and not an address.
RAW_ID=$ID
ID=$(node -e 'process.stdout.write(encodeURIComponent(process.argv[1]))' "$ID")

# One verified document, so that the badge is on a screen the audit looks at.
# Nothing in a file corpus is verified by itself and a state nobody produces is
# a state nobody checks, which is how the expired badge would ship drawn in a
# colour that fails contrast and read out as nothing at all.
#
# dev is the subject the interface sends by default and the administrator this
# server was started with, so it is the one identity that may vouch for a
# document the file connector found no owner for.
curl -fsS -o /dev/null -X POST "$BASE/api/v1/documents/$ID/verify" \
	-H "X-Genba-Tenant: $TENANT" \
	-H "X-Genba-Subject: dev" \
	-H "Content-Type: application/json" \
	-d '{"note":"checked against the current deployment"}' || {
	echo "ui-gate: the document could not be verified, so the badge would be on no screen this audits" >&2
	exit 1
}

# One written answer, for the same reason. Nothing in a file corpus answers a
# question in prose, and the card that holds one is a heading, a paragraph of
# rendered markdown, a signature and a row of citations, none of which any other
# screen builds. The question is matched whole, so the query that reaches it is
# the words below and no others.
curl -fsS -o /dev/null -X PUT "$BASE/api/v1/admin/answers/gate" \
	-H "X-Genba-Tenant: $TENANT" \
	-H "X-Genba-Subject: dev" \
	-H "Content-Type: application/json" \
	-d '{"question":"what is the cache","variants":["how does caching work here"],"body":"The response cache holds whole answers, keyed by the query and the person asking.\n\n- A repeated search is answered without touching the index.\n- An entry is dropped when the documents behind it change.\n\nCall `cache.clear()` to empty it during a migration.","sources":["'"$RAW_ID"'"]}' || {
	echo "ui-gate: the answer could not be written, so the card would be on no screen this audits" >&2
	exit 1
}

status=0

# The walk runs first and needs nothing downloaded, so it still reports on a
# machine with no network. It skips itself when there is no Chrome to drive.
echo "ui-gate: keyboard walk"
if ! node scripts/keyboard-walk.mjs "$BASE"; then
	status=1
fi

# The renderers, called directly with a fixture each, because five of the six
# cannot be reached from this repository: it holds no notebook, no recording, no
# PDF and no page of generated HTML. It needs nothing downloaded either.
echo "ui-gate: rendering check"
if ! node scripts/render-check.mjs "$BASE"; then
	status=1
fi

# The screens that are not the happy path. It holds the search endpoint open
# from the browser side to produce a slow request and a failed one, so it needs
# nothing downloaded either and asks nothing of the server it is auditing.
echo "ui-gate: states check"
if ! node scripts/state-check.mjs "$BASE" "$EMPTY"; then
	status=1
fi

# Counts rather than timings, so they fail a build. What a screen fetches and
# how much markup it builds are both things that grow one commit at a time and
# are never noticed in any single one of them.
echo "ui-gate: budgets"
if ! node scripts/budget-check.mjs "$BASE" "$ID"; then
	status=1
fi

# Printed on every run and advisory until #55 lands. See the script.
echo "ui-gate: latency"
node scripts/latency-report.mjs "$BASE" "$TENANT" || status=1

if ! command -v npx >/dev/null 2>&1; then
	echo "ui-gate: npx is not on the path, so axe and Lighthouse cannot run"
	exit $status
fi

# A chromedriver that does not match the Chrome next to it is the most common
# way this fails, on a laptop and on a runner alike. axe downloads whichever
# chromedriver is current, and a runner image whose Chrome is one release behind
# then refuses every session. CHROMEDRIVER points at a matching one, and
# CHROMEWEBDRIVER is where a GitHub runner keeps the one that matches its own
# Chrome, so the default needs no plumbing in the workflow.
DRIVER=""
if [ -z "${CHROMEDRIVER:-}" ] && [ -x "${CHROMEWEBDRIVER:-}/chromedriver" ]; then
	CHROMEDRIVER="$CHROMEWEBDRIVER/chromedriver"
fi
if [ -n "${CHROMEDRIVER:-}" ]; then
	echo "ui-gate: chromedriver $CHROMEDRIVER"
	DRIVER="--chromedriver-path $CHROMEDRIVER"
fi

# Thirteen screens, and they are states rather than layouts. Three of them, the
# empty index, the query that matched nothing and the filter that matched
# nothing, produce markup no other screen produces, and that markup is where an
# accessibility bug survives longest: nobody looks at an empty page twice.
#
# One of them is a search that has quotes above it. The region is a heading, a
# list and a link per quote, none of which the results page below it has, and it
# appears on some searches and not others, so it is a screen no other one reaches.
#
# Another is a search that has a written answer above it instead. It is the same
# region and it is different markup: a rendered paragraph of somebody's markdown,
# a signature with an address in it and a row of citations. It is also the only
# place in the product where prose nobody indexed is drawn above a list of
# results, which makes it the one screen where a heading level or a link with no
# name would go unnoticed.
#
# The settings screen is the only one built out of form controls, and a radio
# group that has lost its label is the kind of thing that reads perfectly to
# whoever wrote it and not at all to anybody else.
#
# Administration is the only screen built out of tables, and a table whose
# headers are not headers is unreadable to anybody using a screen reader and
# looks perfect to everybody else. It is also the only screen that repaints
# itself on a timer, so it is the one place a focus ring can be quietly taken
# away from somebody five seconds after they put it there.
#
# The answers screen is the one place in the product where somebody writes
# rather than reads, so it is the one screen where a field with no label costs
# more than a sentence nobody hears.
for url in \
	"$BASE/" \
	"$EMPTY/" \
	"$BASE/?q=cache" \
	"$BASE/?q=drawer" \
	"$BASE/?q=what+is+the+cache" \
	"$BASE/?q=cache&open=$ID" \
	"$BASE/?q=zzqxzzqx" \
	"$BASE/?q=cache&kind=image" \
	"$BASE/?q=gatepix&kind=image&view=grid" \
	"$BASE/d/$ID" \
	"$BASE/recent" \
	"$BASE/settings" \
	"$BASE/admin" \
	"$BASE/answers"; do
	echo "ui-gate: axe $url"
	# The interface renders after a fetch, so the audit waits for the first
	# paint to have happened. Auditing an empty document passes and proves
	# nothing.
	# shellcheck disable=SC2086
	if ! npx --yes @axe-core/cli "$url" \
		--tags wcag2a,wcag2aa,wcag21a,wcag21aa \
		--load-delay 2000 \
		$DRIVER \
		--chrome-options="headless,no-sandbox,disable-gpu"; then
		status=1
	fi
done

echo "ui-gate: lighthouse"
report=$(mktemp -t genba-lighthouse.XXXXXX).json
if npx --yes lighthouse "$BASE/?q=cache" \
	--quiet --output json --output-path "$report" \
	--only-categories performance,accessibility,best-practices \
	--chrome-flags="--headless --no-sandbox --disable-gpu"; then
	# The report is read with node rather than with grep, because the categories
	# hold nested objects and a regular expression over them is a way of being
	# wrong on some other day.
	scores=$(node -e '
		const r = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
		for (const [key, c] of Object.entries(r.categories)) console.log(key, Math.round(c.score * 100));
	' "$report")
	echo "$scores" | while read -r category score; do
		echo "ui-gate: lighthouse $category $score"
	done
	if [ "${GENBA_UI_STRICT:-}" = "1" ]; then
		low=$(echo "$scores" | awk -v floor="$LIGHTHOUSE_MIN" '$2 < floor {print $1" "$2}')
		if [ -n "$low" ]; then
			echo "ui-gate: below the floor of $LIGHTHOUSE_MIN: $low" >&2
			status=1
		fi
	fi
else
	echo "ui-gate: lighthouse did not run, which is advisory and not a failure"
fi
rm -f "$report"

exit $status
