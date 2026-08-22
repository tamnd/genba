// The states check.
//
// Everything the gate looks at elsewhere is the happy path, and most of what
// decides whether somebody trusts a search product is not. This drives the
// states: a search that is slow, a search that is very slow, a check that
// failed behind an answer, a query that matched nothing because of a filter or
// because of a typo, an index with nothing in it, a server that is still
// reading its corpus, and a document that is either missing or forbidden.
//
// Four of those need a request that does not answer, so this one holds the
// search endpoint open from the browser side rather than asking the server for
// a delay it has no reason to offer. Holding it is also what makes the timings
// here real: the hundred and twenty milliseconds before a loading state appears
// is measured from the moment the request left the page.
//
// The state during a first read is answered rather than held: the stats
// endpoint is fulfilled from here with the report a server halfway through a
// crawl sends. It is built rather than raced for on purpose. The alternative is
// starting a server on a corpus large enough that the read is still going when
// the browser arrives, which makes the gate slower, makes every count it
// asserts on depend on how far the read had got, and still cannot produce the
// number the banner rounds.
//
// The rest are rendered directly, because they are the states that must be
// compared rather than watched. A 403 and a 404 on a document have to produce
// the same output, and the only way to check that is to render both and put one
// string next to the other.

import { available, evaluate, launch, reporter, settle, sleep, visit } from "./chrome.mjs";

const BASE = process.argv[2] || "http://127.0.0.1:8123";
// A second server holding nothing, when the gate started one. The empty index
// is rendered from a module below like the other states, and this is the same
// screen reached the way somebody reaches it, which is the only way to find out
// whether the count of documents survives the trip.
const EMPTY = process.argv[3] || "";

const why = available();
if (why) {
  console.log(`state-check: ${why}, so the states check cannot run`);
  process.exit(0);
}

// What the page records about itself before any of its own code runs. The time
// the first search left the page and the time the first placeholder appeared
// are both needed, and neither can be measured from out here: a round trip on
// the debugging protocol is worth more than the delay being measured.
const WATCH = `
  window.__genba = { searchAt: 0, skeletonAt: 0, skeletonHeight: 0 };
  const sent = window.fetch;
  window.fetch = function (input, init) {
    const url = String((input && input.url) || input || "");
    if (url.includes("/api/v1/search") && !window.__genba.searchAt) {
      window.__genba.searchAt = performance.now();
    }
    return sent.apply(this, arguments);
  };
  new MutationObserver((records) => {
    if (window.__genba.skeletonAt) return;
    for (const record of records) {
      for (const node of record.addedNodes) {
        if (node.nodeType !== 1) continue;
        if (node.matches(".skeleton-result") || node.querySelector(".skeleton-result")) {
          window.__genba.skeletonAt = performance.now();
          return;
        }
      }
    }
  }).observe(document, { childList: true, subtree: true });
`;

const { state, check } = reporter("state-check");
const browser = await launch();
try {
  await run(browser.session);
} catch (err) {
  console.error(`state-check: ${err.message}`);
  state.failures++;
} finally {
  browser.stop();
}
process.exit(state.failures ? 1 : 0);

async function run(session) {
  // pass, hold or fail. Everything asked of the search endpoint goes through
  // here, so a state that only exists while a request is outstanding can be
  // held open for as long as the assertion about it needs.
  let mode = "pass";
  const held = new Set();
  // What the stats endpoint answers, when this script is answering it. Null
  // means the real server does, which is every assertion but the last group.
  let stats = null;

  session.on("Fetch.requestPaused", ({ requestId, request }) => {
    if (request && request.url.includes("/api/v1/stats")) {
      if (!stats) {
        session("Fetch.continueRequest", { requestId }).catch(() => {});
        return;
      }
      session("Fetch.fulfillRequest", {
        requestId,
        responseCode: 200,
        responseHeaders: [{ name: "content-type", value: "application/json" }],
        body: Buffer.from(JSON.stringify(stats)).toString("base64"),
      }).catch(() => {});
      return;
    }
    if (mode === "hold") {
      held.add(requestId);
      return;
    }
    const answer = mode === "fail" ? { errorReason: "Failed" } : {};
    // A request the page has already abandoned is gone by the time this runs,
    // and saying so is not a failure of anything being tested.
    session(mode === "fail" ? "Fetch.failRequest" : "Fetch.continueRequest", {
      requestId,
      ...answer,
    }).catch(() => {});
  });

  const letGo = async (how) => {
    const waiting = [...held];
    held.clear();
    for (const requestId of waiting) {
      await session(how, { requestId, ...(how === "Fetch.failRequest" ? { errorReason: "Failed" } : {}) }).catch(
        () => {},
      );
    }
  };

  await session("Page.enable");
  await session("Fetch.enable", {
    patterns: [
      { urlPattern: "*/api/v1/search*", requestStage: "Request" },
      { urlPattern: "*/api/v1/stats*", requestStage: "Request" },
    ],
  });
  await session("Page.addScriptToEvaluateOnNewDocument", { source: WATCH });

  // Slow, and then not slow. One navigation covers both the delay before a
  // loading state is allowed to appear and the shape of the thing that appears.
  mode = "hold";
  await session("Page.navigate", { url: `${BASE}/?q=cache` });
  await settle(session, "Boolean(document.querySelector('.skeleton-result'))");

  await check(
    session,
    "no loading state appears in the first hundred and twenty milliseconds of a search",
    "window.__genba.skeletonAt - window.__genba.searchAt >= 120",
  );

  await evaluate(
    session,
    "window.__genba.skeletonHeight = document.querySelector('.skeleton-result').getBoundingClientRect().height",
  );
  mode = "pass";
  await letGo("Fetch.continueRequest");
  await settle(session, "Boolean(document.querySelector('.result'))");

  // The reason the skeleton is measured rather than eyeballed. A placeholder
  // that is not the height of what replaces it moves the whole list the moment
  // the answer lands, which reads as a page that cannot make up its mind.
  await check(
    session,
    "a skeleton row is the same height as a real row",
    `Math.abs(window.__genba.skeletonHeight - document.querySelector('.result').getBoundingClientRect().height) <= 1`,
  );

  // A revalidation that fails behind an answer somebody is reading. The cache
  // is marked stale and the same address is visited again, which is what the
  // back button and the refresher both do, and then the request is refused.
  const rows = await evaluate(session, "document.querySelectorAll('.result').length");
  mode = "fail";
  await evaluate(
    session,
    expr(`
      const { cache } = await import('genba/cache.js');
      cache.invalidate('');
      window.dispatchEvent(new PopStateEvent('popstate'));
      return true;
    `),
  );
  await sleep(600);
  await check(
    session,
    "a check that failed keeps the answer that is already on screen",
    `document.querySelectorAll('.result').length === ${rows} && !document.querySelector('.state--error')`,
  );

  // Very slow. Five seconds of nothing, on a screen with nothing on it, is the
  // one case that gets a control to stop.
  mode = "hold";
  await session("Page.navigate", { url: `${BASE}/?q=cache&sort=recent` });
  await settle(session, "Boolean(document.querySelector('.state__slow'))");
  await check(
    session,
    "a search still running after five seconds offers to stop it",
    "document.querySelector('.state__slow button').textContent === 'Cancel'",
  );

  await evaluate(session, "document.querySelector('.state__slow button').click(), true");
  await check(
    session,
    "stopping it leaves the words in the box and a way to run the search again",
    `document.querySelector('.state__title').textContent === 'Search cancelled' &&
      document.querySelector('.omnibox__input').value === 'cache' &&
      [...document.querySelectorAll('.state__actions button')].some((b) => b.textContent === 'Try again')`,
  );
  mode = "pass";
  await letGo("Fetch.failRequest");

  // Nothing matched, on a query that only matches nothing because of a filter.
  // The count comes from a second search of the same words without them, so
  // this is the one assertion here that the server has to take part in.
  await visit(session, `${BASE}/?q=cache&kind=image`, "Boolean(document.querySelector('.state__filters'))");
  await check(
    session,
    "a zero result page says how many results the filters removed and names them",
    `/Your filters removed \\d+ results\\./.test(document.querySelector('.state').textContent) &&
      document.querySelector('.state__filters').textContent.includes('image')`,
  );

  await evaluate(session, "document.querySelector('.state__actions button').click(), true");
  await check(
    session,
    "clearing the filters from that page brings the results back",
    "Boolean(document.querySelector('.result'))",
  );

  // Nothing matched because of a typo, which is the one empty screen that has a
  // way out that is not a filter. The word comes from the index of this
  // repository rather than from a dictionary, and the server has already run it
  // as this reader, so what is on screen is a search that works.
  await visit(session, `${BASE}/?q=serach`, "Boolean(document.querySelector('.state__correction'))");
  await check(
    session,
    "a search that found nothing because of a typo offers the spelling that works",
    "document.querySelector('.state__correction').textContent === 'search'",
  );

  await evaluate(session, "document.querySelector('.state__correction').click(), true");
  await settle(session, "Boolean(document.querySelector('.result'))");
  await check(
    session,
    "and pressing it runs that search, box and address and all",
    `document.querySelector('.omnibox__input').value === 'search' &&
      location.search.includes('q=search') && document.querySelectorAll('.result').length > 0`,
  );

  // The first ninety seconds. The server binds its port before it has read a
  // corpus and answers out of an index that is still filling, so every screen
  // says so until it is done. Nothing here is faked in the page: the report
  // comes back over the wire, through the same cache and the same refresh as
  // the real one, and the assertions are about what a person ends up reading.
  const report = async (indexing) => {
    stats = { documents: 4182, ...(indexing ? { indexing } : {}) };
    await evaluate(
      session,
      expr(`
        const { cache } = await import('genba/cache.js');
        cache.invalidate('');
        window.dispatchEvent(new PopStateEvent('popstate'));
        return true;
      `),
    );
  };

  // A source the server has not finished counting reports a total of zero, and
  // it says nothing at all rather than "0 of about 0".
  await report({ source: "repo", done: 0, total: 0 });
  await sleep(600);
  await check(
    session,
    "a first read the server has not finished counting puts nothing on screen",
    "document.querySelector('.banner').hidden === true",
  );

  await report({ source: "repo", done: 4182, total: 22235 });
  await settle(session, `document.querySelector('.banner').dataset.state === 'indexing'`);
  await check(
    session,
    "a server still reading its corpus says so, with what it has done and roughly how much there is",
    `(() => {
      const banner = document.querySelector('.banner');
      return banner.hidden === false && banner.getAttribute('role') === 'status' &&
        banner.querySelector('.banner__what').textContent === 'Indexing Repo' &&
        banner.querySelector('.banner__count').textContent === '4,182 of about 22,000' &&
        banner.textContent.includes('Results are partial until this finishes.');
    })()`,
  );

  // The same report twice leaves the same elements. A banner rebuilt on every
  // refresh is one a screen reader starts again from the top every few seconds,
  // which is worse for the person who most needs to be told.
  await evaluate(session, "window.__genba.banner = document.querySelector('.banner__count'), true");
  await report({ source: "repo", done: 4182, total: 22235 });
  await sleep(600);
  await check(
    session,
    "a report that has not changed does not rebuild the line somebody is reading",
    "document.querySelector('.banner__count') === window.__genba.banner",
  );

  // Two caveats and one line. Results that are partial because a read is
  // running are still the server's current answer, and results from before the
  // connection dropped are not, so the connection wins.
  await evaluate(session, "window.dispatchEvent(new Event('offline')), true");
  await settle(session, `document.querySelector('.banner').dataset.state === 'offline'`);
  await check(
    session,
    "being offline wins the banner from a read that is still running",
    `document.querySelector('.banner').textContent.includes('You are offline') &&
      !document.querySelector('.banner').textContent.includes('Indexing')`,
  );

  await evaluate(session, "window.dispatchEvent(new Event('online')), true");
  await settle(session, `document.querySelector('.banner').dataset.state === 'indexing'`);
  await check(
    session,
    "and the read comes back to it when the connection does",
    "document.querySelector('.banner__what').textContent === 'Indexing Repo'",
  );

  // And it goes. A caveat still on screen after it stopped being true is worse
  // than never having had one, because the next time it is true nobody reads it.
  await report(null);
  await settle(session, "document.querySelector('.banner').hidden === true");
  await check(
    session,
    "the banner goes when the corpus is read, and leaves nothing behind it",
    `(() => {
      const banner = document.querySelector('.banner');
      return banner.hidden === true && !banner.dataset.state && banner.childNodes.length === 0;
    })()`,
  );
  stats = null;

  // The security assertion, and the reason both messages live in one module.
  // Which of the two happened is what the permission system is keeping from
  // whoever is asking, so the rendered output has to be identical rather than
  // merely similar.
  await check(
    session,
    "a document that is not there and one nobody may read are the same page",
    expr(`
      const { Page } = await import('genba/page.js');
      const drawn = (status, code, message) => {
        const page = new Page({ onBack: () => ({ href: '/', title: 'Search', go() {} }) });
        page.renderError({ status, code, message });
        return page.el.innerHTML;
      };
      const forbidden = drawn(403, 'forbidden', 'this document is not yours to read');
      const missing = drawn(404, 'not_found', 'no document has that id');
      return forbidden === missing &&
        forbidden.includes('You do not have access to this document.') &&
        !forbidden.includes('403') && !forbidden.includes('404') &&
        !forbidden.includes('not yours') && !forbidden.includes('no document has');
    `),
  );

  await check(
    session,
    "and the same two are the same preview",
    expr(`
      const { Drawer } = await import('genba/drawer.js');
      const drawn = (status, message) => {
        const drawer = new Drawer({ onClose() {} });
        drawer.renderError({ status, message });
        return drawer.el.innerHTML;
      };
      const forbidden = drawn(403, 'this document is not yours to read');
      const missing = drawn(404, 'no document has that id');
      return forbidden === missing && forbidden.includes('You do not have access to this document.');
    `),
  );

  // An index with nothing in it is not a search that failed, so it does not get
  // written as one. It is told apart by the count of documents rather than by
  // the count of results, which is the difference between an empty product and
  // an unlucky query.
  await check(
    session,
    "an index with nothing in it says how to fill it rather than that nothing matched",
    expr(`
      const { nothingMatched } = await import('genba/states.js');
      const out = nothingMatched({ q: 'anything' }, () => {}, { documents: 0 });
      return out.classList.contains('state--first') &&
        out.textContent.includes('Nothing indexed yet') &&
        out.querySelector('.state__command').textContent.includes('-corpus');
    `),
  );

  await check(
    session,
    "and home is that screen too, rather than a dashboard reading zero in three places",
    expr(`
      const { Home } = await import('genba/home.js');
      const home = new Home({ onQuery() {}, onOpen() {}, onVisit() {} });
      home.paint({ subject: 'dev' }, null, { documents: 0 });
      const first = Boolean(home.el.querySelector('.state--first')) && !home.el.querySelector('.panel');
      home.paint({ subject: 'dev' }, null, { documents: 12 });
      return first && Boolean(home.el.querySelector('.home__greeting'));
    `),
  );

  await check(
    session,
    "the filters on a zero result page are the ones above the list, and clearing them keeps the words",
    expr(`
      const { nothingMatched } = await import('genba/states.js');
      const chip = document.createElement('span');
      chip.textContent = 'Type: image';
      let cleared = null;
      const out = nothingMatched(
        { q: 'cache', kind: ['image'], tab: 'all' },
        (next) => { cleared = next; },
        { removed: 57, chips: [chip] },
      );
      out.querySelector('.state__actions button').click();
      return out.textContent.includes('Your filters removed 57 results.') &&
        out.querySelector('.state__filters').textContent === 'Type: image' &&
        Boolean(cleared) && cleared.q === 'cache' && cleared.kind.length === 0;
    `),
  );

  // A failure says which of the three it was, because that is what decides
  // whether somebody waits, retries, or goes and looks at a server.
  await check(
    session,
    "a request that failed says what kind of failure it was, with the request id and a way to try again",
    expr(`
      const { failed } = await import('genba/states.js');
      let again = 0;
      const answered = failed({ status: 503, message: 'the server returned 503', requestID: 'r-1234' }, () => { again++; });
      answered.querySelector('.state__actions button').click();
      const unreachable = failed({ status: 0, message: 'the server could not be reached' });
      const refused = failed({ status: 400, message: 'that query is not valid' });
      return answered.querySelector('.state__title').textContent === 'The server could not answer' &&
        answered.textContent.includes('r-1234') && again === 1 &&
        unreachable.querySelector('.state__title').textContent === 'The server could not be reached' &&
        refused.querySelector('.state__title').textContent === 'That request was refused' &&
        !unreachable.querySelector('.state__actions');
    `),
  );

  if (!EMPTY) {
    console.log("state-check: no empty server was passed, so the first run is only checked from the module");
    return;
  }

  // The same screen as the two assertions above, reached the way somebody
  // reaches it on the day they install this. The module can be told there are
  // no documents; only the server can be empty.
  await visit(session, `${EMPTY}/`, "Boolean(document.querySelector('.state'))");
  await check(
    session,
    "a server with an empty index opens on how to fill it",
    `(() => {
      const state = document.querySelector('.state--first');
      return Boolean(state) && state.textContent.includes('Nothing indexed yet') &&
        state.querySelector('.state__command').textContent.includes('-corpus');
    })()`,
  );
  // Searching an empty index is not an unlucky query and is not written as one.
  await visit(session, `${EMPTY}/?q=anything`, "Boolean(document.querySelector('.state'))");
  await check(
    session,
    "and searching it says the same thing rather than that nothing matched",
    `(() => {
      const state = document.querySelector('.state--first');
      return Boolean(state) && state.textContent.includes('Nothing indexed yet');
    })()`,
  );
}

/** expr wraps a block of statements in something Runtime.evaluate can await. */
function expr(code) {
  return `(async () => {${code}})()`;
}
