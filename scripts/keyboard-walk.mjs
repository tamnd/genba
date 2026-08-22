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
  SHIFT,
  sleep,
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

  // Facet labels, asked of the function for the same reason. The corpus the
  // gate indexes has whatever paths it has, and the case worth pinning is the
  // one where two of them would read identically.
  await check(
    session,
    "a facet value reads as its last segment, and a collision grows one",
    `import('genba/format.js').then(({ facetLabels }) => {
      const out = facetLabels([
        'team/alpha/docs',
        'team/beta/docs',
        '/Users/mei/notes/Spec/2121',
      ]);
      return out[0].label === 'alpha/docs' && out[1].label === 'beta/docs' &&
        out[0].context === 'team' &&
        out[2].label === '2121' &&
        out[2].context === 'Users / mei / notes / Spec';
    })`,
  );

  await check(
    session,
    "a filter row shows the end of the value rather than the start",
    `(() => {
      const items = [...document.querySelectorAll('.facet__item')];
      return items.length > 0 && items.every((el) => {
        const value = el.querySelector('.facet__text').title;
        const shown = el.querySelector('.facet__label').textContent.trim().toLowerCase();
        const last = (value.split('/').filter(Boolean).pop() || value).toLowerCase();
        return shown.endsWith(last);
      });
    })()`,
  );

  // The gate put a claim on the first result before the browser started, so the
  // badge is on the row rather than only in the preview. That is the whole
  // point of the signal: it is read while somebody is choosing which of ten
  // results to open, not after they have opened one.
  await check(
    session,
    "a verified result carries the badge on the row",
    `(() => {
      const badge = document.querySelector('.result .verified');
      return Boolean(badge) && badge.classList.contains('verified--fresh') &&
        badge.textContent.trim() === 'Verified' && (badge.title || '').includes('Current until');
    })()`,
  );

  // The other two states, asked of the function, because a corpus read minutes
  // ago holds nothing that has run out and a badge nobody can produce is a
  // badge nobody checks. An expired claim has to read as expired: the shape it
  // would ship in otherwise is a green tick on a document last looked at in
  // 2023.
  await check(
    session,
    "a claim that has run out reads as run out rather than as a tick",
    `import('genba/verify.js').then(({ badge }) => {
      const made = (state) =>
        badge({ state, by: 'Mei Tanaka', at: '2026-01-01T00:00:00Z', until: '2026-02-01T00:00:00Z' });
      return made('fresh').textContent === 'Verified' &&
        made('expiring').textContent.startsWith('Verified, expires') &&
        made('expired').textContent.startsWith('Verification expired') &&
        made('expired').classList.contains('verified--expired') &&
        badge(null) === null && badge({}) === null;
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
  await check(
    session,
    "focus is on the heading rather than on the first thing to press",
    `document.activeElement === document.querySelector('.drawer__title') &&
      document.activeElement.tabIndex === -1`,
  );

  // Wide enough that the list is beside the drawer rather than behind it, which
  // is the case where saying the drawer is modal takes a visible list away from
  // a screen reader.
  await check(
    session,
    "beside a visible list the preview does not claim to be modal",
    `window.innerWidth > 720 &&
      document.querySelector('.drawer').getAttribute('aria-modal') === 'false'`,
  );

  // The preview is where somebody decides whether to trust what they are
  // reading, so the claim is in the header with a name on it rather than in the
  // tooltip it carries on a row.
  await check(
    session,
    "the preview names the person who vouched for the document",
    `(() => {
      const badge = document.querySelector('.drawer__meta .verified');
      return Boolean(badge) && badge.textContent.includes('Verified by');
    })()`,
  );

  // The words that were searched for, in the document that came back. This
  // query matches the corpus in the hundreds, so a preview with no marks in it
  // at all is the marking having stopped working.
  await check(
    session,
    "the query is marked in the preview, with a count of how many",
    `(() => {
      const marks = document.querySelectorAll('.drawer__body mark.hit');
      const count = document.querySelector('.matches__count').textContent;
      return marks.length > 1 &&
        !document.querySelector('.matches').hidden &&
        count === '1 of ' + marks.length &&
        document.querySelectorAll('.drawer__body mark.hit--current').length === 1;
    })()`,
  );

  const first = await evaluate(
    session,
    "document.querySelector('.drawer__body mark.hit--current').textContent",
  );
  await press(session, "n", "KeyN", 78);
  await check(
    session,
    "n moves to the next match and says which one it is on",
    `(() => {
      const marks = [...document.querySelectorAll('.drawer__body mark.hit')];
      const current = document.querySelector('.drawer__body mark.hit--current');
      return marks.indexOf(current) === 1 &&
        document.querySelector('.matches__count').textContent === '2 of ' + marks.length;
    })()`,
  );

  await press(session, "N", "KeyN", 78, SHIFT);
  await check(
    session,
    "shift and n goes back to the one before it",
    `document.querySelector('.matches__count').textContent.startsWith('1 of') &&
      document.querySelector('.drawer__body mark.hit--current').textContent === ${JSON.stringify(first)}`,
  );

  // The interaction the drawer exists for. Reading through five candidates used
  // to be five open and close cycles.
  const showing = await evaluate(session, "document.querySelector('.drawer__title').textContent");
  await press(session, "j", "KeyJ", 74);
  await check(
    session,
    "j moves the preview to the next result without closing it",
    `!document.querySelector('.drawer').hidden &&
      document.querySelector('.drawer__title').textContent !== ${JSON.stringify(showing)} &&
      document.querySelector('.result[data-active="true"]').dataset.index === '1' &&
      new URLSearchParams(location.search).get('cursor') === '1'`,
  );
  await check(
    session,
    "the keys stay with the preview rather than moving focus behind it",
    "document.querySelector('.drawer').contains(document.activeElement)",
  );

  // One ahead, so the next press paints from memory rather than from the
  // network. The cache is the app's own, reached the way the app reaches it.
  await check(
    session,
    "the result under the one on screen has already been fetched",
    `import('genba/cache.js').then(({ cache }) => {
      const rows = [...document.querySelectorAll('.result__title')];
      const next = rows[2].getAttribute('href').replace('/d/', '').split('?')[0];
      return cache.read(cache.key('document', { id: decodeURIComponent(next) })).state !== 'miss';
    })`,
  );

  await press(session, "k", "KeyK", 75);
  await check(
    session,
    "k moves it back",
    `document.querySelector('.drawer__title').textContent === ${JSON.stringify(showing)} &&
      document.querySelector('.result[data-active="true"]').dataset.index === '0'`,
  );

  await press(session, "Escape", "Escape", 27);
  await check(
    session,
    "Escape closes the preview",
    "document.querySelector('.drawer').hidden && !location.search.includes('open=')",
  );
  await check(
    session,
    "closing puts focus back on the row the preview was opened from",
    `document.activeElement.dataset.index === '0' &&
      document.activeElement.classList.contains('result')`,
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

  // Verification, from the screen it is decided on. The two writes are here
  // rather than only in the API tests because what they have to leave alone is
  // the document underneath them: a page opened at line four hundred that jumps
  // back to the top because somebody put their name to it is a page nobody
  // verifies twice.
  // The note is put on the claim here rather than only by the shell that starts
  // the server, because the two writes below leave a claim with no note on it,
  // which is what the button sends. Without this the walk passes once against a
  // server and fails on the second run against the same one, and a check that
  // depends on how many times it has been run is a check nobody trusts.
  await evaluate(
    session,
    `import('genba/api.js').then(({ api }) =>
      api.verify(decodeURIComponent(location.pathname.slice(3)), {
        note: 'checked against the current deployment',
      }))`,
  );
  await visit(session, BASE + id, "Boolean(document.querySelector('.page__meta .verified'))");

  await check(
    session,
    "the document page says who vouched for it and until when",
    `(() => {
      const badge = document.querySelector('.page__meta .verified');
      return Boolean(badge) && badge.classList.contains('verified--fresh') &&
        badge.textContent.includes('Verified by') && (badge.title || '').includes('Current until');
    })()`,
  );
  await check(
    session,
    "the verifier's own sentence is on the page and not only in a tooltip",
    `(document.querySelector('.verified__note') || {}).textContent === 'checked against the current deployment'`,
  );

  // A mark on a node the body already had, so that the claim below can be about
  // this element surviving rather than about the page looking similar.
  await evaluate(session, "document.querySelector('.page__body > *').dataset.walk = 'kept'");
  await evaluate(session, foot("Withdraw") + ".click()");
  await check(
    session,
    "withdrawing takes the badge off and leaves the document where it was",
    `!document.querySelector('.page__meta .verified') &&
      Boolean(document.querySelector('.page__body [data-walk="kept"]'))`,
  );
  await check(
    session,
    "the button offers the write that is now available",
    `Boolean(${foot("Verify")}) && !${foot("Withdraw")}`,
  );

  await evaluate(session, foot("Verify") + ".click()");
  await check(
    session,
    "verifying again puts the badge back, from this screen, with no reload",
    `(() => {
      const badge = document.querySelector('.page__meta .verified');
      return Boolean(badge) && badge.classList.contains('verified--fresh') &&
        Boolean(document.querySelector('.page__body [data-walk="kept"]'));
    })()`,
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

  // Five presses down the list. Moving the cursor is a move through an answer
  // the page already has, so nothing on screen is asked for a second time. Two
  // requests are allowed to go out and both are ahead of where somebody is: the
  // document under the cursor, which is why the preview opens instantly, and
  // the next page of results, which is why the end of the list is not a wait.
  await evaluate(
    session,
    `(() => {
      window.__walk = { searches: [] };
      const sent = window.fetch;
      window.fetch = function (input) {
        const url = String((input && input.url) || input);
        if (url.includes('/api/v1/search')) window.__walk.searches.push(url);
        return sent.apply(this, arguments);
      };
      return true;
    })()`,
  );
  for (let i = 0; i < 5; i++) await press(session, "ArrowDown", "ArrowDown", 40);
  await check(
    session,
    "five presses down land on the sixth result and ask again for nothing already on screen",
    `document.activeElement.dataset.index === '5' &&
      window.__walk.searches.every((u) => Number(new URL(u).searchParams.get('offset') || 0) > 0)`,
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

  // The settings screen, reached the way the screen itself says it is reached.
  // A shortcut that is printed on a list and does not work is worse than one
  // that was never written down, so the walk presses the keys off the list.
  await visit(session, `${BASE}/?q=cache`, "document.querySelectorAll('.result').length > 0");
  await press(session, "g", "KeyG", 71);
  await press(session, ",", "Comma", 188);
  await settle(session, "location.pathname === '/settings'");
  await check(
    session,
    "g then comma reaches the settings screen and the rail says so",
    `document.querySelector('.rail__link[data-route="settings"]').getAttribute('aria-current') === 'page' &&
      document.activeElement === document.querySelector('.settings__title')`,
  );

  // The reason keys.js exists. Both lists are drawn from the same rows, so the
  // count on this screen is the count the sheet shows, and neither of them can
  // be a key that nothing answers.
  await check(
    session,
    "the printed list of keys is the table the handler reads",
    `(() => {
      const printed = [...document.querySelectorAll('.settings .shortcut')];
      const keyed = printed.filter((row) => row.querySelectorAll('.kbd').length > 0);
      return printed.length >= 15 && keyed.length === printed.length;
    })()`,
  );

  await evaluate(
    session,
    "document.querySelector('.choice input[value=\"dark\"]').click()",
  );
  await check(
    session,
    "choosing a theme applies it at once and remembers it",
    `document.documentElement.dataset.theme === 'dark' &&
      localStorage.getItem('genba.theme') === 'dark'`,
  );

  await evaluate(
    session,
    "document.querySelector('.choice input[value=\"\"]').click()",
  );
  await check(
    session,
    "following the system again forgets the choice rather than storing one",
    "localStorage.getItem('genba.theme') === null",
  );

  await evaluate(session, "document.querySelector('.settings .page__back-link').click()");
  await settle(session, "location.pathname === '/'");
  await check(
    session,
    "the way out leads back to the search it was opened from",
    "location.search.includes('q=cache')",
  );

  // Administration. The gate names the browser's development subject as an
  // administrator, so the rail offers it here and offers it to nobody else,
  // which is the half of this that cannot be checked from a screen that already
  // has the role.
  await visit(session, `${BASE}/?q=cache`, "document.querySelectorAll('.result').length > 0");
  await check(
    session,
    "the rail offers administration to somebody who holds the role",
    "document.querySelectorAll('.rail__link[data-route=\"admin\"]').length === 1",
  );

  await evaluate(session, "document.querySelector('.rail__link[data-route=\"admin\"]').click()");
  await settle(session, "location.pathname === '/admin'");
  await check(
    session,
    "the rail entry reaches administration and the screen takes the focus",
    `document.querySelector('.rail__link[data-route="admin"]').getAttribute('aria-current') === 'page' &&
      document.activeElement === document.querySelector('.admin__title')`,
  );

  await settle(session, "document.querySelectorAll('.admin .stat__value').length >= 2");
  // The screen opens on a held copy and repaints when the server answers, which
  // is a fifth of a second later and is where the cursor used to be dropped.
  // The check above ran in that window and passed, so this is the same claim
  // made after the answer has landed rather than before it.
  await check(
    session,
    "the answer landing leaves the cursor where opening the screen put it",
    "document.activeElement === document.querySelector('.admin__title')",
  );
  await check(
    session,
    "the corpus is reported as what is servable and what is held back",
    `(() => {
      const labels = [...document.querySelectorAll('.admin__corpus .stat__label')].map((s) => s.textContent);
      const values = [...document.querySelectorAll('.admin__corpus .stat__value')].map((s) => s.textContent);
      return labels.includes('Servable') && labels.includes('Held back') &&
        values.every((v) => v.trim().length > 0);
    })()`,
  );

  // The connector the gate started, reported back. A screen that draws its
  // cards from a fixture would pass this and say nothing.
  await check(
    session,
    "the connector this process is running is named with its history",
    `(() => {
      const card = document.querySelector('.connector');
      if (!card) return false;
      const title = card.querySelector('.connector__title').textContent.trim();
      const rows = card.querySelectorAll('.connector__runs tbody tr').length;
      return title === 'repo' && rows >= 1;
    })()`,
  );

  // A table whose headers are not headers reads as a grid of unrelated cells,
  // and there is no way to tell from looking at it.
  await check(
    session,
    "every column on this screen is headed by a real header cell",
    `[...document.querySelectorAll('.admin__table')].every((t) =>
      t.tHead && [...t.tHead.rows[0].cells].every((c) =>
        c.tagName === 'TH' && c.getAttribute('scope') === 'col'))`,
  );

  // The corpus here is a checkout that everybody in the tenant may read, so
  // nothing is held back and the screen has to say so rather than drawing an
  // empty table under a heading.
  await check(
    session,
    "an empty quarantine is said out loud rather than drawn as an empty table",
    `(() => {
      const note = document.querySelector('.admin__note-title');
      return Boolean(note) && note.textContent.includes('Nothing is being held back');
    })()`,
  );

  // The connector this process was started with cannot be changed from here,
  // because the next restart would read the command line again and undo it. A
  // screen that offered the buttons anyway would be offering a refusal.
  await check(
    session,
    "a connector from the command line says where it is configured instead of offering buttons",
    `(() => {
      const card = [...document.querySelectorAll('.connector')].find(
        (c) => c.querySelector('.connector__title').textContent.trim() === 'repo');
      if (!card) return false;
      return card.querySelector('.connector__controls') === null &&
        card.querySelector('.connector__fixed').textContent.includes('command line');
    })()`,
  );

  // Adding one. The connector this adds is deliberately switched off and is
  // removed again below, because the corpus this gate measures is the checkout
  // itself and a second crawler over it would move every number the rest of the
  // gate checks.
  await settle(session, "document.querySelector('#connector-source') !== null");
  await evaluate(session, "document.querySelector('#connector-source').focus()");
  await type(session, "walk");
  await evaluate(session, "document.querySelector('#connector-corpus-dir').value = '/no/such/directory'");
  await evaluate(session, "document.querySelector('#connector-enabled').checked = false");
  await evaluate(session, "document.querySelector('.connectors .button--primary').click()");
  await settle(session, "document.querySelector('.connectors__output--bad') !== null");
  await check(
    session,
    "settings that cannot be run are refused in the server's own words, with the form left as it was",
    `(() => {
      const said = document.querySelector('.connectors__output--bad').textContent.trim();
      return said.length > 0 &&
        document.querySelector('#connector-source').value === 'walk' &&
        document.querySelector('#connector-corpus-dir').value === '/no/such/directory';
    })()`,
  );

  await evaluate(session, "document.querySelector('#connector-corpus-dir').value = '/tmp'");
  await evaluate(session, "document.querySelector('.connectors .button--primary').click()");
  await settle(
    session,
    "[...document.querySelectorAll('.connector__title')].some((t) => t.textContent.trim() === 'walk')",
  );
  await check(
    session,
    "a connector added from the form is on the screen, off because that is what was asked for",
    `(() => {
      const card = [...document.querySelectorAll('.connector')].find(
        (c) => c.querySelector('.connector__title').textContent.trim() === 'walk');
      const buttons = [...card.querySelectorAll('.connector__controls .button')].map((b) => b.textContent.trim());
      return card.querySelector('.pill').textContent.trim() === 'Switched off' &&
        buttons.includes('Start') && buttons.includes('Remove') &&
        document.querySelector('.connectors__output--good').textContent.includes('walk') &&
        document.querySelector('#connector-source').value === '';
    })()`,
  );

  // Removing forgets how a connector was configured and nothing here can undo
  // it, so it asks twice. The second question replaces the button the first
  // press was on, which is what puts a keyboard on it without going looking.
  await evaluate(session, 'document.querySelector(\'[data-focus-key="remove:walk"]\').focus()');
  await press(session, "Enter", "Enter", 13);
  await check(
    session,
    "removing asks again, on the button the press was on",
    `(() => {
      const now = document.querySelector('[data-focus-key="remove:walk"]');
      return Boolean(now) && now.textContent.trim() === 'Remove it' && document.activeElement === now;
    })()`,
  );

  await press(session, "Enter", "Enter", 13);
  await settle(
    session,
    "[...document.querySelectorAll('.connector__title')].every((t) => t.textContent.trim() !== 'walk')",
  );
  await check(
    session,
    "a removal is said out loud and the keyboard lands on the line that says it",
    `(() => {
      const said = document.querySelector('.connectors__output');
      return Boolean(said) && said.textContent.includes('walk') && document.activeElement === said;
    })()`,
  );

  // The access check. The subject to ask about is the gate's own, because it is
  // the only identity in this corpus whose answer is known: it is the one the
  // browser has been searching as all the way down this walk.
  await settle(session, "document.querySelector('#access-subject') !== null");
  await evaluate(session, "document.querySelector('#access-subject').focus()");
  await type(session, "dev");
  await evaluate(session, "document.querySelector('.access .button--primary').click()");
  await check(
    session,
    "the access check answers about the person it was asked about",
    `(() => {
      const values = [...document.querySelectorAll('.access__asked .facts__value')].map((d) => d.textContent);
      return values[0] === 'dev' && values.length === 3;
    })()`,
  );

  // Nothing was asked for yet, so nothing is counted. The aggregate is behind
  // its own button because it reads every document in the tenant, and a screen
  // that quietly ran it would be a screen nobody can keep open.
  await check(
    session,
    "the reach is not counted until somebody asks for it",
    "document.querySelector('.access__reach') === null",
  );

  await evaluate(
    session,
    "[...document.querySelectorAll('.access__actions .button')].find((b) => b.textContent.includes('Count')).click()",
  );
  await settle(session, "document.querySelector('.access__reach') !== null");
  await check(
    session,
    "the count of what one person can reach is a real count of this corpus",
    `(() => {
      const total = Number(document.querySelector('.access__total strong').textContent.replace(/[^0-9]/g, ''));
      const rows = document.querySelectorAll('.access__reach tbody tr').length;
      return total > 0 && rows >= 1;
    })()`,
  );

  // This screen asks the server again every five seconds. A repaint that took
  // the caret out of a half typed group name would be a form nobody could use,
  // so the wait here is longer than the interval on purpose.
  await evaluate(session, "document.querySelector('#access-groups').focus()");
  await type(session, "eng");
  await sleep(6000);
  await check(
    session,
    "a repaint on the timer leaves the half typed question and the caret alone",
    `document.activeElement === document.querySelector('#access-groups') &&
      document.querySelector('#access-groups').value === 'eng' &&
      document.querySelector('#access-groups').selectionStart === 3`,
  );

  // The same timer, from outside the screen. A poll that arrived while somebody
  // was typing the next search would have taken the cursor back to the heading,
  // which is a screen that fights the person reading it.
  await evaluate(session, "document.querySelector('.omnibox__input').focus()");
  await sleep(6000);
  await check(
    session,
    "a repaint on the timer leaves the cursor alone when it is not on this screen",
    "document.activeElement === document.querySelector('.omnibox__input')",
  );

  await evaluate(session, "document.querySelector('.admin .page__back-link').click()");
  await settle(session, "location.pathname === '/'");
  await check(
    session,
    "the way out of administration leads back to the search it was opened from",
    "location.search.includes('q=cache')",
  );

  // The answer above the results.
  //
  // The query is one this corpus can answer. A repository is mostly source
  // files, and a source file is never quoted, so the words here are ones that
  // appear in the prose in it rather than in the code.
  await visit(session, `${BASE}/?q=drawer`, "document.querySelectorAll('.result').length > 0");
  await check(
    session,
    "an answer quotes documents that are on the page below it",
    `(() => {
      const quotes = [...document.querySelectorAll('.answer__quote')];
      if (!quotes.length) return false;
      const titles = [...document.querySelectorAll('.result__title')].map((a) => a.textContent.trim());
      return quotes.length <= 3 &&
        quotes.every((q) => titles.includes(q.querySelector('.quote__title').textContent.trim())) &&
        document.querySelector('.answer__note').textContent.trim().length > 0;
    })()`,
  );

  // The layout contract, which is the whole of what this region promises the
  // list underneath it. There is no stream, so nothing arrives late by design,
  // and this is the assertion that keeps it that way when one is added.
  const under = await evaluate(session, "document.querySelector('.result').getBoundingClientRect().top");
  await check(
    session,
    "nothing under the answer moves once the page has painted",
    `new Promise((done) => setTimeout(
      () => done(Math.abs(document.querySelector('.result').getBoundingClientRect().top - ${under}) < 1),
      1200,
    ))`,
  );

  const quoted = await evaluate(session, "document.querySelector('.quote__text').textContent");
  await evaluate(session, "document.querySelector('.quote__cite').click()");
  await settle(session, "!document.querySelector('.drawer').hidden");
  await check(
    session,
    "one click on a citation opens that document at the sentence that was quoted",
    `(() => {
      const marks = [...document.querySelectorAll('.drawer__body mark.hit--passage')];
      if (!marks.length) return false;
      const squeeze = (s) => s.replace(/\\s+/g, '');
      const found = squeeze(marks.map((m) => m.textContent).join(''));
      return location.search.includes('at=') &&
        squeeze(${JSON.stringify(quoted)}).includes(found) &&
        marks.some((m) => m.classList.contains('hit--current'));
    })()`,
  );

  await press(session, "Escape", "Escape", 27);
  await check(
    session,
    "closing it takes the passage out of the address along with the document",
    "!location.search.includes('at=') && !location.search.includes('open=')",
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

  // A page of pictures has nothing to quote, and the region that would hold the
  // quotes is not on the page rather than on it and empty.
  await check(
    session,
    "a search with nothing worth quoting draws no answer region at all",
    `(() => {
      const region = document.querySelector('.answer');
      return region.hidden && region.childElementCount === 0;
    })()`,
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

  // The wide half of the one filter surface rule. The panel is the surface, so
  // the button that would open it somewhere else is not on the page at all. The
  // width is set rather than assumed, because a headless window opens at 800 and
  // every rule here is about which side of 960 we are on.
  await narrow(session, 1280, 900);
  await visit(session, `${BASE}/?q=cache`, "document.querySelectorAll('.facet__item').length > 0");
  await check(
    session,
    "the panel is the only filter surface where there is room for it",
    `(() => {
      const panel = document.querySelector('.facets');
      const toggle = document.querySelector('.filterbar__toggle');
      return panel.getBoundingClientRect().height > 0 &&
        toggle.offsetParent === null &&
        panel.getAttribute('aria-modal') === null;
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

  // The narrow half of the one filter surface rule. The same panel comes up as
  // a sheet, and the button that raised it goes, so there is never a Filters
  // button sitting next to an open panel of filters.
  await settle(session, "document.querySelectorAll('.facet__item').length > 0");
  await evaluate(session, "document.querySelector('.filterbar__toggle').click()");
  await check(
    session,
    "at 390 pixels the filters are a sheet and the button that opened it is gone",
    `(() => {
      const panel = document.querySelector('.facets');
      const toggle = document.querySelector('.filterbar__toggle');
      return panel.dataset.open === 'true' &&
        panel.getBoundingClientRect().height > 0 &&
        panel.getAttribute('aria-modal') === 'true' &&
        toggle.offsetParent === null &&
        panel.contains(document.activeElement);
    })()`,
  );

  await press(session, "Escape", "Escape", 27);
  await check(
    session,
    "Escape closes the sheet and puts focus back on the button",
    `document.querySelector('.facets').dataset.open === 'false' &&
      document.querySelector('.scrim--filters').hidden &&
      document.activeElement === document.querySelector('.filterbar__toggle')`,
  );

  // At this width the drawer is the window and there is nothing behind it to
  // read, which is the half of the breakpoint where a dialog is a dialog.
  await settle(session, "document.querySelectorAll('.result').length > 0");
  await evaluate(session, "document.querySelector('.result__title').click()");
  await settle(session, "!document.querySelector('.drawer').hidden");
  await check(
    session,
    "at 390 pixels the preview covers the list and says it is modal",
    "document.querySelector('.drawer').getAttribute('aria-modal') === 'true'",
  );
}

/**
 * foot is an expression naming one button under a document, by the words on it.
 *
 * By the words rather than by a class, because the words are what somebody
 * reading the screen has to press, and a button whose label stopped matching
 * what it does is exactly the thing this walk is for.
 */
function foot(words) {
  return `[...document.querySelectorAll('.page__foot .button')]
    .find((b) => b.textContent.trim() === ${JSON.stringify(words)})`;
}
