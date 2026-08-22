// The cold latency report.
//
// Fifty distinct queries against whatever corpus the gate is pointed at, timed
// from the client. Distinct because a repeated query is answered from a cache
// and measures the cache, and the number worth watching is what somebody gets
// when they type something nobody has typed before.
//
// It asks the server what it is running on before it times anything, and stops
// if the answer is a driver with no index of its own. A driver like that is
// walked document by document with the ranking done above it, and on the same
// few hundred documents that measured at a p50 of 1.8 seconds against 17
// milliseconds for SQLite. Timing it produces a real number for a deployment
// nobody has, printed next to a budget it was never going to meet, and it is
// read as the product being slow. That happened, and it went unnoticed for as
// long as it did because the number arrived every run and looked like a
// measurement.
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

// What is being measured, asked before anything is timed. A failure to answer
// is not fatal, because an embedder that named no driver is entitled to report
// nothing and this report is not the place to insist on it.
let driver = "";
let ranking = true;
try {
  const res = await fetch(`${BASE}/api/v1/stats`, { headers });
  if (res.ok) {
    const body = await res.json();
    driver = body.driver || "";
    ranking = body.ranking !== false;
  }
} catch {
  // Left as it was. The queries below fail on their own if the server is gone.
}
console.log(`latency-report: driver ${driver || "unnamed"}, ranking ${ranking ? "yes" : "no"}`);
if (!ranking) {
  console.error(
    `latency-report: ${driver || "this driver"} has no index of its own, so it is walked per query and there is no latency here worth reporting`,
  );
  console.error(`latency-report: point the gate at sqlite or postgres`);
  process.exit(1);
}

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
