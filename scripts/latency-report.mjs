// The cold latency report.
//
// Fifty distinct queries against whatever corpus the gate is pointed at, timed
// from the client. Distinct because a repeated query is answered from a cache
// and measures the cache, and the number worth watching is what somebody gets
// when they type something nobody has typed before.
//
// It prints and it does not fail, until #55 lands and the query stops being
// linear in the size of the match set. It prints anyway, because a number
// nobody prints is a number nobody watches, and that is exactly how a fifth of
// a second on a first query went unnoticed for as long as it did. Set
// GENBA_LATENCY_STRICT=1 to have it fail against the budget.

const BASE = process.argv[2] || "http://127.0.0.1:8123";
const TENANT = process.argv[3] || "demo";
const BUDGET = Number(process.env.GENBA_LATENCY_BUDGET || 10);
const STRICT = process.env.GENBA_LATENCY_STRICT === "1";

// Fifty words, spelled out rather than sampled, so that two runs measure the
// same thing and a change in the number is a change in the server. They are
// words this repository is made of, because this repository is the corpus.
const QUERIES = [
  "cache",
  "ranker",
  "posting",
  "tenant",
  "facet",
  "snippet",
  "connector",
  "postgres",
  "sqlite",
  "migration",
  "keyboard",
  "preview",
  "drawer",
  "thumbnail",
  "notebook",
  "markdown",
  "highlight",
  "permission",
  "subject",
  "group",
  "identity",
  "crawl",
  "bucket",
  "signature",
  "retry",
  "backoff",
  "watcher",
  "reconcile",
  "checkpoint",
  "cursor",
  "budget",
  "baseline",
  "calibration",
  "percentile",
  "histogram",
  "counter",
  "decode",
  "candidate",
  "corpus",
  "vocabulary",
  "analyzer",
  "token",
  "stemming",
  "suggest",
  "correction",
  "recency",
  "boost",
  "scoring",
  "pagination",
  "throughput",
];

const headers = {
  "X-Genba-Tenant": TENANT,
  "X-Genba-Subject": "dev@example.com",
  "X-Genba-Groups": "everyone",
};

const timings = [];
let empty = 0;
for (const q of QUERIES) {
  const url = `${BASE}/api/v1/search?q=${encodeURIComponent(q)}&limit=20`;
  const started = performance.now();
  let res;
  try {
    res = await fetch(url, { headers });
  } catch (err) {
    console.log(`latency-report: ${q} did not answer: ${err.message}`);
    process.exit(0);
  }
  const body = await res.json();
  const took = performance.now() - started;
  if (!res.ok) {
    console.log(`latency-report: ${q} answered ${res.status}, so there is nothing to measure`);
    process.exit(0);
  }
  if (!body.total) empty++;
  timings.push({ q, took });
}

timings.sort((a, b) => a.took - b.took);
const at = (p) => timings[Math.min(timings.length - 1, Math.floor((p / 100) * timings.length))];
const p50 = at(50);
const p95 = at(95);
const p99 = at(99);
const slowest = timings[timings.length - 1];

const ms = (n) => `${n.toFixed(1)}ms`;
console.log(
  `latency-report: ${timings.length} distinct queries, p50 ${ms(p50.took)}, p95 ${ms(p95.took)}, p99 ${ms(p99.took)}, budget ${ms(BUDGET)}`,
);
console.log(`latency-report: slowest was ${slowest.q} at ${ms(slowest.took)}`);
// A word that is in no document is answered from an empty posting list, which
// is the fastest path there is. Too many of them and the percentiles above are
// a measurement of nothing, so the count is printed next to them.
if (empty) console.log(`latency-report: ${empty} of them matched nothing in this corpus`);

if (p95.took <= BUDGET) process.exit(0);
if (!STRICT) {
  console.log(`latency-report: over the budget, which is advisory until #55 lands`);
  process.exit(0);
}
console.error(`latency-report: p95 is ${ms(p95.took)}, over the budget of ${ms(BUDGET)}`);
process.exit(1);
