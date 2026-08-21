// The keyboard walk.
//
// It drives a real Chrome through the sequence a person performs and asserts
// where each key and each click actually led. axe checks that the markup
// describes itself; this checks that the interface does what the markup
// promises, which is a different failure and the one that shipped: the primary
// click target on every result row was an anchor to a file:// URL, which a
// browser served over HTTP will not navigate to, so clicking a result title did
// nothing at all. No audit finds that, because the markup is perfectly correct.

import {
  available,
  evaluate,
  launch,
  narrow,
  press,
  reporter,
  settle,
  type,
  visit,
} from "./chrome.mjs";

const BASE = process.argv[2] || "http://127.0.0.1:8123";

const why = available();
if (why) {
  console.log(`keyboard-walk: ${why}, so the walk cannot run`);
  process.exit(0);
}

const { state, check } = reporter("keyboard-walk");
const browser = await launch();
try {
  await walk(browser.session);
} catch (err) {
  console.error(`keyboard-walk: ${err.message}`);
  state.failures++;
} finally {
  browser.stop();
}
process.exit(state.failures ? 1 : 0);

async function walk(session) {
  // A results page over the corpus the gate already started, and an id from it
  // for the document route.
  await visit(session, `${BASE}/?q=cache`, "document.querySelectorAll('.result').length > 0");

  await check(
    session,
    "the title of a result is a link into the product",
    `(() => {
      const a = document.querySelector('.result__title');
      return a && a.tagName === 'A' && a.getAttribute('href').startsWith('/d/');
    })()`,
  );

  // The corpus here is this repository, which holds code, pages and files and
  // no messages, tickets or people. Those three tabs used to be on screen
  // anyway, and every one of them was a click that led to an empty page.
  await check(
    session,
    "the tab strip is what the corpus holds and nothing else",
    `(() => {
      const on = [...document.querySelectorAll('.tab')].map((t) => t.textContent);
      const named = (name) => on.some((t) => t.startsWith(name));
      return named('All') && named('Documents') && named('Code') &&
        !named('Messages') && !named('Tickets') && !named('People');
    })()`,
  );

  await check(
    session,
    "one source is not a filter, so the rail does not offer it",
    "document.querySelector('#rail-sources').children.length === 0",
  );

  // The part of the page that scrolls is a tab stop of its own, so a screen
  // whose rows have not arrived yet can still be scrolled and read. Everything
  // else on this walk is content, and content is exactly what is missing in the
  // moment this covers.
  await check(
    session,
    "the region that scrolls can be reached from the keyboard",
    "document.querySelector('#main').tabIndex === 0",
  );

  // The two boundaries the corpus above cannot show, asked of the function
  // directly. It is a module in the page, so the browser is the test runner and
  // there is nothing to install.
  await check(
    session,
    "images are a vertical, and an empty index has none at all",
    `import('genba/results.js').then(({ verticalsFor }) => {
      const withImages = verticalsFor([
        { value: 'page', count: 4 },
        { value: 'image', count: 912 },
      ]);
      return withImages.some((v) => v.id === 'images') && verticalsFor([]).length === 0;
    })`,
  );

  await press(session, "j", "KeyJ", 74);
  await check(
    session,
    "j moves the cursor onto the first row",
    "document.querySelectorAll('.result[data-active=\"true\"]').length === 1",
  );

  await press(session, "Enter", "Enter", 13);
  await check(
    session,
    "Enter on a row opens the preview",
    "!document.querySelector('.drawer').hidden && location.search.includes('open=')",
  );
  await check(
    session,
    "the preview took focus",
    "document.querySelector('.drawer').contains(document.activeElement)",
  );

  await press(session, "Escape", "Escape", 27);
  await check(
    session,
    "Escape closes the preview",
    "document.querySelector('.drawer').hidden && !location.search.includes('open=')",
  );

  // A plain click on the title opens the preview rather than following the
  // link. This is the assertion the shipped defect would have failed, in the
  // other direction: the click did nothing at all.
  await evaluate(session, "document.querySelector('.result__title').click()");
  await check(
    session,
    "a plain click on the title opens the preview and stays on the results",
    "!document.querySelector('.drawer').hidden && location.pathname === '/'",
  );

  const id = await evaluate(session, "document.querySelector('.result__title').getAttribute('href')");
  await visit(session, BASE + id, "document.querySelector('.page__title').textContent.trim().length > 0");
  await check(
    session,
    "the document route renders the document with a way back",
    `(() => {
      const back = document.querySelector('.page__back-link');
      const title = document.querySelector('.page__title');
      return Boolean(back && back.getAttribute('href') && title.tagName === 'H1');
    })()`,
  );
  await check(
    session,
    "the document page is not the preview drawer",
    "document.querySelector('.drawer').hidden",
  );
  // A link written as a bare query string resolves against whatever path is
  // showing, which was harmless while every path was the search and is not any
  // more. This catches the next one written that way.
  //
  // A link to a line is a fragment, so it keeps the address it is written on and
  // the words on the end of it with it, which is the whole point of it. What
  // this rules out is a link that puts anything else on a document address.
  await check(
    session,
    "no link on the document page hangs the state of a search off the document",
    `[...document.querySelectorAll('a[href]')].every((a) => {
      if (a.getAttribute('href').startsWith('#')) return true;
      const u = new URL(a.href, location.href);
      if (!u.pathname.startsWith('/d/')) return true;
      return [...new URLSearchParams(u.search).keys()].every((k) => k === 'q');
    })`,
  );

  // The keyboard on its own. Everything above uses it in passing; this is the
  // part an audit finds and the part somebody using it all day notices.
  await visit(session, `${BASE}/?q=cache`, "document.querySelectorAll('.result').length > 0");

  await check(
    session,
    "the result list is one tab stop rather than twenty",
    `(() => {
      const rows = [...document.querySelectorAll('.results__list [data-index]')];
      const stops = rows.filter((r) => r.tabIndex === 0);
      const inside = [...document.querySelectorAll('.results__list a[href], .results__list button')];
      return rows.length > 1 && stops.length === 1 && stops[0] === rows[0] &&
        inside.length > 0 && inside.every((el) => el.tabIndex === -1);
    })()`,
  );

  await press(session, "j", "KeyJ", 74);
  await check(
    session,
    "the cursor takes focus with it and writes itself into the URL",
    `document.activeElement.dataset.index === '0' &&
      document.activeElement.dataset.active === 'true' &&
      new URLSearchParams(location.search).get('cursor') === '0'`,
  );

  await press(session, "End", "End", 35);
  await check(
    session,
    "End moves the cursor to the last result on the page",
    `(() => {
      const rows = [...document.querySelectorAll('.results__list [data-index]')];
      const last = String(rows.length - 1);
      return document.activeElement.dataset.index === last &&
        new URLSearchParams(location.search).get('cursor') === last;
    })()`,
  );

  await press(session, "Home", "Home", 36);
  await check(session, "Home moves it back to the first", "document.activeElement.dataset.index === '0'");

  await press(session, "Tab", "Tab", 9);
  await check(
    session,
    "Tab leaves the list in one press",
    "!document.querySelector('.results__list').contains(document.activeElement)",
  );

  // A row named in the URL is the row the page opens on, which is what makes
  // the cursor survive a repaint and a link to a particular result work.
  await visit(session, `${BASE}/?q=cache&cursor=2`, "document.querySelectorAll('.result').length > 2");
  await check(
    session,
    "a cursor in the URL is where the page opens",
    "document.querySelector('.result[data-active=\"true\"]').dataset.index === '2'",
  );

  const opened = await evaluate(session, "document.querySelector('.result__title').getAttribute('href')");
  await visit(session, BASE + opened, "document.querySelector('.page__title').textContent.trim().length > 0");
  await evaluate(session, "history.back()");
  await settle(session, "document.querySelectorAll('.result').length > 2");
  await check(
    session,
    "back from a document returns to the row the eye was on",
    "document.querySelector('.result[data-active=\"true\"]').dataset.index === '2'",
  );

  // The omnibox as a screen reader meets it. axe cannot type, so the list is
  // opened here and the relationships it would check are asserted directly.
  await press(session, "/", "Slash", 191);
  await type(session, "cache");
  await settle(session, "document.querySelectorAll('.suggestion').length > 0");
  await check(
    session,
    "an open suggestion list is a combobox that describes itself",
    `(() => {
      const input = document.querySelector('.omnibox__input');
      const list = document.getElementById(input.getAttribute('aria-controls'));
      const options = [...list.querySelectorAll('[role="option"]')];
      return input.getAttribute('role') === 'combobox' &&
        input.getAttribute('aria-expanded') === 'true' &&
        list.getAttribute('role') === 'listbox' &&
        Boolean(list.getAttribute('aria-label')) &&
        options.length > 0 && options.every((o) => o.id);
    })()`,
  );
  await check(
    session,
    "the suggestion count is announced politely",
    `(() => {
      const region = document.querySelector('.omnibox [role="status"]');
      const options = document.querySelectorAll('.omnibox [role="option"]').length;
      return region.getAttribute('aria-live') === 'polite' &&
        region.textContent.trim() === options + (options === 1 ? ' suggestion' : ' suggestions');
    })()`,
  );

  await press(session, "ArrowDown", "ArrowDown", 40);
  await check(
    session,
    "the arrow keys move a highlight the input points at",
    `(() => {
      const input = document.querySelector('.omnibox__input');
      const id = input.getAttribute('aria-activedescendant');
      const option = id && document.getElementById(id);
      return Boolean(option) && option.getAttribute('aria-selected') === 'true' &&
        document.activeElement === input;
    })()`,
  );

  await press(session, "Escape", "Escape", 27);
  await check(
    session,
    "Escape closes the list and keeps what was typed",
    `document.querySelector('.suggestions').hidden &&
      document.querySelector('.omnibox__input').value === 'cache' &&
      document.querySelector('.omnibox__input').getAttribute('aria-expanded') === 'false'`,
  );

  await press(session, "Escape", "Escape", 27);
  await check(
    session,
    "a second Escape clears the field",
    "document.querySelector('.omnibox__input').value === ''",
  );

  // The recent screen. It is the one screen made of somebody's own history
  // rather than of the corpus, so the walk has to make some history first and
  // then go and look for it.
  await visit(session, `${BASE}/?q=cache`, "document.querySelectorAll('.result').length > 0");
  const read = await evaluate(session, "document.querySelector('.result__title').textContent.trim()");
  await evaluate(session, "document.querySelector('.result__title').click()");
  await settle(session, "!document.querySelector('.drawer').hidden");
  await press(session, "Escape", "Escape", 27);

  // Through the rail rather than by typing the address, because being one click
  // away from every other screen is the reason it is on the rail at all.
  await evaluate(session, "document.querySelector('.rail__link[data-route=\"recent\"]').click()");
  await settle(session, "location.pathname === '/recent'");
  await check(
    session,
    "the rail leads to the recent screen and says which entry you are on",
    `document.querySelector('.rail__link[data-route="recent"]').getAttribute('aria-current') === 'page' &&
      !document.querySelector('.rail__link[data-route="home"]').hasAttribute('aria-current')`,
  );

  await check(
    session,
    "the two halves are two lists with names of their own",
    `(() => {
      const lists = [...document.querySelectorAll('.recent [role="list"]')];
      const names = lists.map((l) => l.getAttribute('aria-label'));
      return lists.length === 2 && names.every(Boolean) && names[0] !== names[1];
    })()`,
  );

  // The assertion the whole endpoint exists for. Opening a document from a
  // search is recorded on the server, so it is there on the next screen and
  // would be there on another machine.
  await settle(session, "document.querySelectorAll('.recent .result').length > 0");
  await check(
    session,
    "a document opened from a search is at the top of your own history",
    `(() => {
      const first = document.querySelector('.recent [aria-label="Documents you opened"] .result');
      return Boolean(first) && first.querySelector('.result__title').textContent.trim() === ${JSON.stringify(read)};
    })()`,
  );

  await check(
    session,
    "a row of history is the same row as a result, with the time it was read on it",
    `(() => {
      const first = document.querySelector('.recent [aria-label="Documents you opened"] .result');
      const at = first && first.querySelector('.result__meta time[datetime]:last-of-type');
      return Boolean(at) && at.textContent.includes('you opened this');
    })()`,
  );

  await press(session, "j", "KeyJ", 74);
  await check(
    session,
    "j walks the list of what you read rather than the one under it",
    `document.querySelector('.recent [aria-label="Documents you opened"]').contains(document.activeElement) &&
      document.activeElement.dataset.index === '0'`,
  );

  await press(session, "Enter", "Enter", 13);
  await check(
    session,
    "Enter opens the preview without leaving the recent screen",
    `!document.querySelector('.drawer').hidden && location.pathname === '/recent' &&
      location.search.includes('open=')`,
  );

  await press(session, "Escape", "Escape", 27);
  await check(
    session,
    "Escape closes it and leaves the cursor on the row it opened from",
    `document.querySelector('.drawer').hidden && !location.search.includes('open=') &&
      document.querySelector('.recent [aria-label="Documents you opened"]').contains(document.activeElement)`,
  );

  // Home asks the same question of the same endpoint and shows the top of the
  // answer, with the way to the whole of it beside the heading.
  await visit(session, `${BASE}/`, "document.querySelectorAll('.home .panel').length > 0");
  await check(
    session,
    "home shows what was opened and what changed, with a real link to the rest",
    `(() => {
      const links = [...document.querySelectorAll('.home .panel__link')];
      const rows = document.querySelectorAll('.home .panel__row-title').length;
      return links.length >= 2 && links.every((a) => a.getAttribute('href') === '/recent') && rows > 0;
    })()`,
  );

  // The grid. The pictures behind this query are written into the corpus by the
  // gate before the server starts, because the repository itself holds none.
  await visit(session, `${BASE}/?q=gatepix`, "document.querySelectorAll('.cell').length > 0");

  await check(
    session,
    "a page of nothing but images opens as a grid",
    `document.querySelector('.results__list').dataset.view === 'grid' &&
      document.querySelectorAll('.result').length === 0`,
  );

  // The assertion the endpoint exists for. Twenty four pictures at a megabyte
  // each is what this page used to move, and no amount of loading them lazily
  // makes that acceptable once somebody scrolls to the bottom of it.
  await settle(
    session,
    "performance.getEntriesByType('resource').some((e) => e.name.includes('/thumbnail'))",
  );
  await check(
    session,
    "a page of image results transfers well under a megabyte of picture",
    `(() => {
      const image = performance
        .getEntriesByType('resource')
        .filter((e) => e.name.includes('/thumbnail') || e.name.includes('/content'));
      const bytes = image.reduce((n, e) => n + (e.encodedBodySize || e.transferSize || 0), 0);
      return image.length > 0 && bytes < 1_000_000;
    })()`,
  );

  await check(
    session,
    "every picture in the grid carries its own width and height",
    `(() => {
      const images = [...document.querySelectorAll('.cover .image__img')];
      return images.length > 0 && images.every((img) => img.getAttribute('width') && img.getAttribute('height'));
    })()`,
  );

  // The choice goes into the address bar, so a grid somebody sends to a
  // colleague opens as a grid for them too.
  await evaluate(session, "document.querySelectorAll('.view__button')[0].click()");
  await check(
    session,
    "asking for the list writes the layout into the URL",
    `location.search.includes('view=list') &&
      document.querySelector('.results__list').dataset.view === 'list'`,
  );

  await check(
    session,
    "an image row does not reserve blank snippet lines",
    `(() => {
      const rows = [...document.querySelectorAll('.result')];
      return rows.length > 0 && rows.every((row) => !row.querySelector('.result__snippet'));
    })()`,
  );

  // Last, because it changes the viewport for everything after it. 390 is the
  // narrowest phone worth drawing for, and it is the width at which the strip
  // used to shrink its tabs to fit instead of scrolling, so the last one read
  // "Tick".
  await narrow(session, 390, 780);
  await visit(session, `${BASE}/?q=cache`, "document.querySelectorAll('.tab').length > 0");
  await check(
    session,
    "at 390 pixels the tab strip scrolls rather than cutting a label in half",
    `(() => {
      const tabs = document.querySelector('.tabs');
      const whole = [...document.querySelectorAll('.tab')].every(
        (t) => t.scrollWidth <= t.clientWidth + 1,
      );
      return whole && tabs.scrollWidth > tabs.clientWidth && tabs.dataset.scroll === 'end';
    })()`,
  );
}
