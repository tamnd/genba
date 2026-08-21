// The budgets, counted rather than timed.
//
// There is no build step, so the files on disk are the files on the wire and a
// Go test can weigh them. What it cannot see is how many of them a screen
// actually asks for and how much markup the interface builds once they arrive,
// and those are the two numbers that decide whether a page stays quick on a
// laptop that is not new.
//
// Everything here is a count. A count does not move when the runner is busy,
// which is why these fail a build and the Lighthouse score next to them does
// not.

import { available, evaluate, launch, reporter, visit } from "./chrome.mjs";

const BASE = process.argv[2] || "http://127.0.0.1:8123";
const ID = process.argv[3] || "";

const why = available();
if (why) {
  console.log(`budget-check: ${why}, so the budgets that need a browser cannot run`);
  process.exit(0);
}

// The wire budgets are the same numbers web/web_test.go holds the files on disk
// to. They are repeated rather than shared because they are two different
// claims: that what is committed is small, and that a screen asks for all of it
// and nothing else.
const BUDGETS = {
  script: 180 << 10,
  style: 40 << 10,
  requests: 40,
  // A results page of twenty, which is the default page size, and the same page
  // with a document open in the preview. The second is larger by a whole
  // document, and for a source file that is a highlighted span per token, which
  // is the largest thing the interface builds anywhere.
  results: 1200,
  preview: 3000,
};

// What the browser recorded about what it fetched. encodedBodySize is the body
// as it came off the wire, so a compressed response counts what was actually
// sent rather than what it turned into.
const ASSETS = `(() => {
  const entries = performance.getEntriesByType('resource');
  const bytes = (ext) => entries
    .filter((r) => new URL(r.name).pathname.endsWith(ext))
    .reduce((n, r) => n + (r.encodedBodySize || 0), 0);
  return {
    requests: entries.length,
    script: bytes('.js'),
    style: bytes('.css'),
  };
})()`;

const { state, check } = reporter("budget-check");
const browser = await launch();
try {
  await run(browser.session);
} catch (err) {
  console.error(`budget-check: ${err.message}`);
  state.failures++;
} finally {
  browser.stop();
}
process.exit(state.failures ? 1 : 0);

async function run(session) {
  await visit(session, `${BASE}/?q=cache`, "document.querySelectorAll('.result').length > 0");

  const assets = await evaluate(session, ASSETS);
  console.log(
    `budget-check: ${assets.requests} requests, ${assets.script} bytes of script of ${BUDGETS.script}, ${assets.style} bytes of style of ${BUDGETS.style}`,
  );
  await check(
    session,
    `the scripts a screen asks for are under ${BUDGETS.script} bytes on the wire`,
    `(${ASSETS}).script <= ${BUDGETS.script}`,
  );
  await check(
    session,
    `the styles a screen asks for are under ${BUDGETS.style} bytes on the wire`,
    `(${ASSETS}).style <= ${BUDGETS.style}`,
  );
  // A module per file means a request per file, and a screen that quietly grows
  // to a hundred of them is slow in a way no byte count shows.
  await check(
    session,
    `a screen asks for no more than ${BUDGETS.requests} files`,
    `(${ASSETS}).requests <= ${BUDGETS.requests}`,
  );

  const results = await evaluate(session, "document.getElementsByTagName('*').length");
  console.log(`budget-check: ${results} nodes on a page of twenty results, of ${BUDGETS.results}`);
  await check(
    session,
    `a page of twenty results is under ${BUDGETS.results} nodes`,
    `document.getElementsByTagName('*').length <= ${BUDGETS.results}`,
  );

  if (!ID) {
    console.log("budget-check: no document id was passed, so the preview is not counted");
    return;
  }
  await visit(session, `${BASE}/?q=cache&open=${ID}`, "Boolean(document.querySelector('.preview'))");
  const preview = await evaluate(session, "document.getElementsByTagName('*').length");
  console.log(`budget-check: ${preview} nodes with a document open, of ${BUDGETS.preview}`);
  await check(
    session,
    `a results page with a document open is under ${BUDGETS.preview} nodes`,
    `document.getElementsByTagName('*').length <= ${BUDGETS.preview}`,
  );
}
