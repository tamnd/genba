#!/bin/sh
# The browser half of the performance gate.
#
# It starts the real binary over a real corpus, which is this repository, and
# audits the four screens somebody actually looks at: the home page, a results
# page, a results page with the document drawer open, and a document on a page
# of its own. Auditing a static fixture instead would audit the fixture, and
# every accessibility bug this is meant to catch lives in the markup the
# interface builds after a fetch.
#
# The keyboard walk and axe are the parts that fail a build. axe reports
# violations of a standard rather than an opinion, it does not move when the
# runner is busy, and a violation is a person who cannot use the page. The walk
# presses the keys and clicks the links a person would and asserts where each
# one led, which is the half axe cannot see: markup can describe itself
# perfectly and still go nowhere when you click it.
#
# Lighthouse is advisory here and enforced on the nightly run, because a
# Lighthouse performance score on a shared runner moves by ten points between
# two runs of the same commit. Set GENBA_UI_STRICT=1 to have it fail.
set -eu

BIN=${BIN:-bin/genbad}
PORT=${PORT:-8123}
BASE="http://127.0.0.1:$PORT"
TENANT=demo
LIGHTHOUSE_MIN=${LIGHTHOUSE_MIN:-95}
SERVER=
# Where the pictures go. It is inside the corpus because the corpus is this
# directory, it is gitignored, and it is not a dotted name because the file
# connector skips anything that starts with a dot and would index none of it.
IMAGES=${IMAGES:-gate-images}

cleanup() {
	if [ -n "$SERVER" ]; then
		kill "$SERVER" 2>/dev/null || true
		wait "$SERVER" 2>/dev/null || true
	fi
	rm -rf "$IMAGES"
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
if curl -fsS -o /dev/null "$BASE/healthz" 2>/dev/null; then
	echo "ui-gate: something is already listening on $PORT, set PORT to a free one" >&2
	exit 1
fi

# The repository holds no images, so the grid and the thumbnail endpoint would
# have nothing to audit. Two dozen generated pictures are written into the corpus
# before it is indexed and removed by the trap above, whatever the gate exits on.
node scripts/gate-images.mjs "$IMAGES" 24

# The corpus is this repository. It is a few hundred documents of real prose and
# code, which is enough for a results page with snippets, facets and a drawer,
# and it needs nothing downloaded.
"$BIN" -addr "127.0.0.1:$PORT" -store memory -tenant "$TENANT" -corpus . -corpus-name repo -log-level error &
SERVER=$!

i=0
until curl -fsS "$BASE/healthz" >/dev/null 2>&1; do
	if ! kill -0 "$SERVER" 2>/dev/null; then
		echo "ui-gate: the server exited before it was ready" >&2
		exit 1
	fi
	i=$((i + 1))
	if [ "$i" -gt 100 ]; then
		echo "ui-gate: the server never became healthy" >&2
		exit 1
	fi
	sleep 0.2
done

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
# encoder rather than into the string.
ID=$(node -e 'process.stdout.write(encodeURIComponent(process.argv[1]))' "$ID")

status=0

# The walk runs first and needs nothing downloaded, so it still reports on a
# machine with no network. It skips itself when there is no Chrome to drive.
echo "ui-gate: keyboard walk"
if ! node scripts/keyboard-walk.mjs "$BASE"; then
	status=1
fi

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

for path in "/" "/?q=cache" "/?q=cache&open=$ID" "/d/$ID" "/?q=gatepix"; do
	echo "ui-gate: axe $path"
	# The interface renders after a fetch, so the audit waits for the first
	# paint to have happened. Auditing an empty document passes and proves
	# nothing.
	# shellcheck disable=SC2086
	if ! npx --yes @axe-core/cli "$BASE$path" \
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
