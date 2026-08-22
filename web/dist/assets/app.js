// The application shell.
//
// It owns three things and nothing else: the URL, the keyboard, and which view
// is on screen. Everything that draws is in its own module and takes a callback
// rather than reaching back in here, which is what keeps a change to the result
// row from being a change to routing.

import { h, replace, svg } from "genba/dom.js";
import { api, identity, setIdentity, ApiError } from "genba/api.js";
import { cache } from "genba/cache.js";
import { Live } from "genba/live.js";
import { copies, copy } from "genba/clipboard.js";
import * as urlState from "genba/state.js";
import { icon, initials, label, number, roughly, sourceColor, when } from "genba/format.js";
import { Omnibox } from "genba/omnibox.js";
import { CHORD, CHORD_TIMEOUT, SHORTCUTS, arms, binding, keysOf } from "genba/keys.js";
import { apply as applyPrefs, density, setDensity, setTheme } from "genba/prefs.js";
import { Results, VERTICALS, verticalsFor } from "genba/results.js";
import { Drawer } from "genba/drawer.js";
import { Page } from "genba/page.js";
import { Home } from "genba/home.js";
import { Recent } from "genba/recent.js";
import { Settings } from "genba/settings.js";
import { forget, remember } from "genba/queries.js";
import { failed } from "genba/states.js";

// LOADING_DELAY is how long a search may run before the interface admits to it.
//
// Every API in this product is budgeted under ten milliseconds, and at that
// latency a loading state appears and disappears inside one frame, which reads
// as a flicker rather than as feedback. So the skeleton is a timer that the
// response cancels: if the answer beats it, no loading state is ever mounted
// and the transition is a single paint.
const LOADING_DELAY = 120;

// SLOW_DELAY is when a wait stops being a wait and becomes a question about
// whether anything is happening at all.
//
// Five seconds is four hundred times the budget, so this is not a threshold any
// healthy request comes near. It is for the ones that are never going to
// answer, and what it buys is a way to stop them: an interface with no way to
// abandon something has taken the machine away from the person using it.
const SLOW_DELAY = 5_000;

// How long each prefetch waits before it decides somebody meant it.
//
// All three are bets, and the delay is what keeps a bet cheap. Passing the
// pointer over eight rows on the way to the ninth should cost nothing, and
// arrow keying down a suggestion list should not fire eight searches.
const PREFETCH_PAGE = 300;
const PREFETCH_HOVER = 150;
const PREFETCH_SUGGESTION = 200;

// At most four documents are being guessed at once. Beyond that the prefetches
// are competing with the request somebody is actually waiting for.
const PREFETCH_LIMIT = 4;

// How often the offline banner recounts the age of what is on screen. It only
// runs while the connection is gone, which is the only time anybody is reading
// it.
const BANNER_TICK = 30_000;

class App {
  constructor(root) {
    this.root = root;
    this.session = null;
    this.query = urlState.read();
    this.loadingTimer = null;
    this.slowTimer = null;
    this.currentKey = "";
    this.hoverTimer = null;
    this.pageTimer = null;
    this.suggestionTimer = null;
    this.guessing = 0;
    // The last document this session told the server about, so a page that
    // renders itself again does not say it twice.
    this.recorded = "";
    // The last results page this session painted, so that a document page has
    // somewhere to go back to. It is empty in a tab that opened straight onto a
    // document, which is the case the back link has to answer differently.
    this.lastSearch = "";
    // The address the settings screen was opened from, for the same reason and
    // with the same empty case. It is not the last search: settings is reached
    // from every screen, and landing back on a results page from a document
    // somebody was reading would be the wrong way out.
    this.lastScreen = "";

    this.omnibox = new Omnibox({
      // Explicitly the search, because the box is on every screen and a search
      // run from the recent screen is a search rather than a recent screen with
      // a query string on it.
      onSearch: (text) =>
        this.go({ ...this.query, q: text, offset: 0, open: "", at: "" }, { path: "/" }),
      onOpen: (id) => this.open(id),
      onHighlight: (item) => this.guessSuggestion(item),
    });
    this.results = new Results({
      onQuery: (q) => this.go(q),
      onOpen: (id) => this.open(id),
      onHover: (id) => this.guessDocument(id),
      onSay: (text) => this.say(text),
      onCursor: (i) => this.rememberCursor(i),
      onCite: (id, text) => this.cite(id, text),
    });
    this.home = new Home({
      onQuery: (q) => this.go({ ...urlState.read(""), ...q }, { path: "/" }),
      onOpen: (id) => this.open(id),
      onVisit: (path) => this.visit(path),
    });
    this.recent = new Recent({
      onOpen: (id) => this.open(id),
      onHover: (id) => this.guessDocument(id),
      onSay: (text) => this.say(text),
    });
    this.drawer = new Drawer({
      onClose: () => {
        this.go({ ...this.query, open: "", at: "" }, { replace: true });
        // Back to the row it was opened from rather than to the top of the
        // page, which is the whole reason the cursor is in the URL.
        const rows = this.rows();
        if (rows) rows.focusCursor();
      },
      onSay: (text) => this.say(text),
    });
    this.page = new Page({
      onBack: () => this.backFromDocument(),
      onSay: (text) => this.say(text),
    });
    this.settings = new Settings({
      onBack: () => this.backFromSettings(),
      onIdentity: () => this.switchIdentity(),
      onSay: (text) => this.say(text),
    });

    this.main = h("main", {
      class: "main",
      id: "main",
      // This element is what scrolls, and a region that scrolls has to be
      // reachable from the keyboard, because Safari will not scroll one that
      // focus cannot enter. Until now it passed for the wrong reason: every
      // screen happened to have something focusable on it, which stops being
      // true for the moment a screen is loading and its rows are placeholders.
      // It is also where the skip link points, and a target with no tabindex is
      // a skip link that scrolls without moving focus.
      tabindex: "0",
      // The header carries a hairline only once there is something scrolled
      // under it, so a page at rest has one less line on it.
      onScroll: () => {
        const scrolled = this.main.scrollTop > 0;
        if ((this.header.dataset.scrolled === "true") !== scrolled) {
          this.header.dataset.scrolled = String(scrolled);
        }
      },
    });
    this.live = h("div", {
      class: "visually-hidden",
      role: "status",
      "aria-live": "polite",
      "aria-atomic": "true",
    });

    // One banner, two things that can put something in it. Being offline and a
    // source still being read are unrelated and can both be true, and there is
    // one row above the content for either of them to say so in.
    this.banner = h("div", { class: "banner", role: "status", hidden: true });
    this.offline = false;
    this.indexing = null;

    this.rail = this.buildRail();
    this.header = this.buildHeader();
    this.build();
    this.bindKeys();

    // The page checks itself against the server on four triggers, none of which
    // is a person pressing reload. What is on screen was correct when it was
    // painted, and the only question this answers is whether it still is.
    this.refresher = new Live({
      onRefresh: () => this.sync(),
      onConnection: (online) => this.connection(online),
      onIndex: () => cache.invalidate(),
    });

    window.addEventListener("popstate", () => {
      this.query = urlState.read();
      this.sync();
    });

    // A line address is the one link that lands on the screen it was sent from.
    // Nothing above reacts to it: the path is the same, so sync would repaint
    // the document it is already showing, and the document itself only looks at
    // the address when it paints. This asks it to look again.
    window.addEventListener("hashchange", () => {
      if (urlState.route().name === "document") this.page.toLine();
    });
  }

  build() {
    replace(
      this.root,
      h(
        "div",
        { class: "app" },
        h("a", { class: "visually-hidden", href: "#main" }, "Skip to results"),
        this.rail,
        this.header,
        this.banner,
        this.main,
      ),
      this.drawer.scrim,
      this.drawer.el,
      this.live,
    );
  }

  buildHeader() {
    return h(
      "header",
      { class: "header" },
      h(
        "button",
        {
          class: "icon-button rail__toggle",
          type: "button",
          "aria-label": "Menu",
          onClick: () => {
            const open = this.rail.dataset.open === "true";
            this.rail.dataset.open = String(!open);
          },
        },
        svg(icon("menu"), 20),
      ),
      this.omnibox.el,
      h(
        "div",
        { class: "header__actions" },
        h(
          "button",
          {
            class: "icon-button",
            type: "button",
            "aria-label": "Switch density",
            title: "Switch between comfortable and compact rows",
            onClick: () => this.density(),
          },
          svg(icon("rows"), 20),
        ),
        h(
          "button",
          {
            class: "icon-button",
            type: "button",
            "aria-label": "Keyboard shortcuts",
            title: "Keyboard shortcuts (?)",
            onClick: () => this.shortcuts(),
          },
          svg(icon("keyboard"), 20),
        ),
        h(
          "button",
          {
            class: "icon-button",
            type: "button",
            "aria-label": "Switch theme",
            title: "Switch theme",
            onClick: () => this.theme(),
          },
          svg(icon(document.documentElement.dataset.theme === "dark" ? "sun" : "moon"), 20),
        ),
      ),
    );
  }

  buildRail() {
    const who = identity();
    return h(
      "nav",
      { class: "rail", "aria-label": "Main" },
      h(
        "a",
        { class: "brand", href: "/", title: "genba", onClick: (e) => this.link(e, {}) },
        h("span", { class: "brand__mark" }, "G"),
        h("span", {}, "genba"),
      ),
      h(
        "div",
        { class: "rail__section" },
        h(
          "a",
          {
            class: "rail__link",
            href: "/",
            title: "Home",
            // Which entry is current is decided on every render rather than
            // once here, because the rail is built when the shell is and
            // outlives every screen it points at.
            dataset: { route: "home" },
            onClick: (e) => this.link(e, {}),
          },
          svg(icon("home"), 20),
          h("span", { class: "rail__label" }, "Home"),
        ),
        h(
          "a",
          {
            class: "rail__link",
            // A screen of its own rather than a recency sorted search. The two
            // look alike and answer different questions: a search cannot say
            // what this person read, and that is the half everybody comes here
            // for.
            href: urlState.RECENT,
            title: "Recent",
            dataset: { route: "recent" },
            onClick: (e) => this.follow(e, urlState.RECENT),
          },
          svg(icon("clock"), 20),
          h("span", { class: "rail__label" }, "Recent"),
        ),
        h(
          "a",
          {
            class: "rail__link",
            href: urlState.SETTINGS,
            title: "Settings",
            dataset: { route: "settings" },
            onClick: (e) => this.follow(e, urlState.SETTINGS),
          },
          svg(icon("slider"), 20),
          h("span", { class: "rail__label" }, "Settings"),
        ),
      ),
      // Both of these are filled once the session says what the corpus holds.
      // They are empty until then rather than full of guesses, because a rail
      // that shortens a moment after it paints is worse than one that arrives a
      // moment late.
      h("div", { class: "rail__section", id: "rail-verticals" }),
      h("div", { class: "rail__section", id: "rail-sources" }),
      h(
        "button",
        {
          class: "rail__foot",
          type: "button",
          title: "Change who you are searching as",
          onClick: () => this.switchIdentity(),
        },
        h("span", { class: "avatar" }, initials(who.subject)),
        h("span", { class: "rail__label" }, who.subject),
      ),
    );
  }

  /** link makes a rail entry a real anchor that still routes on the client. */
  link(e, query) {
    if (e.metaKey || e.ctrlKey || e.shiftKey) return;
    e.preventDefault();
    // Rooted at the search. A query string on its own resolves against whatever
    // path is showing, which was harmless while every path was the search and
    // would now make every rail entry a link back to the document or the recent
    // screen with a stray parameter on the end.
    this.go({ ...urlState.read(""), ...query }, { path: "/" });
  }

  /** follow is the same thing for a rail entry that is a path rather than a query. */
  follow(e, path) {
    if (e.metaKey || e.ctrlKey || e.shiftKey) return;
    e.preventDefault();
    this.visit(path);
  }

  /**
   * visit moves to a screen that has an address rather than a query.
   *
   * The query is emptied rather than carried, because a filter belongs to the
   * search it was set on and a sort belongs to a result list. Nothing on the
   * recent screen is a view of either.
   */
  visit(path) {
    // Where the settings screen was opened from, taken before the address
    // moves. No other screen needs this: the rest either carry their own state
    // in the address or have a list behind them to go back to.
    if (path === urlState.SETTINGS && urlState.route().name !== "settings") {
      this.lastScreen = location.pathname + location.search;
    }
    if (location.pathname !== path || location.search) history.pushState(null, "", path);
    this.query = urlState.read("");
    this.sync();
  }

  async start() {
    try {
      this.session = (await api.me()).data;
      // Everything the cache holds is keyed under this, so it is set before
      // anything is read and the switcher below empties the cache by changing
      // it. Nothing cached under one identity is reachable from another.
      cache.as(this.session.view || "");
      this.results.verticals = verticalsFor(this.session.kinds);
      this.results.knows({ sources: this.session.sources || [] });
      this.renderVerticals();
      this.renderSources();
    } catch (err) {
      this.fail(err);
      return;
    }
    this.sync();
    this.refresher.start();
    this.countCorpus();
  }

  /**
   * countCorpus is how the interface tells nothing matched apart from nothing
   * indexed.
   *
   * Not awaited, because the first paint does not need it and the one screen
   * that turns on it repaints when it lands. Home asks the same question under
   * the same key, so on the screen where both are true this is the request Home
   * would have made rather than a second one.
   */
  async countCorpus() {
    try {
      await cache.swr(cache.key("stats", {}), (opts) => api.stats(opts), (d) => {
        this.results.knows({ documents: d.documents });
      });
    } catch {
      // An interface that cannot count what is indexed says nothing about it,
      // which is what it said before this ran.
    }
  }

  /** renderVerticals fills the rail with the verticals the corpus has. */
  renderVerticals() {
    const holder = this.rail.querySelector("#rail-verticals");
    // All is the tab strip's way of saying no filter, and on the rail it is the
    // brand and the Home link, so it does not get a row of its own.
    const verticals = this.results.verticals.filter((v) => v.id !== "all");
    if (!verticals.length) {
      replace(holder);
      return;
    }
    replace(
      holder,
      h("h2", { class: "rail__title" }, "Verticals"),
      verticals.map((v) =>
        h(
          "a",
          {
            class: "rail__link",
            href: `/?tab=${v.id}`,
            title: v.title,
            onClick: (e) => this.link(e, { ...this.query, tab: v.id, kind: [], offset: 0 }),
          },
          svg(icon(v.icon), 20),
          h("span", { class: "rail__label" }, v.title),
          h("span", { class: "rail__count" }, number(countIn(this.session.kinds, v))),
        ),
      ),
    );
  }

  /**
   * renderSources fills the rail with where the documents came from.
   *
   * One source is not a filter. It always selects everything, and it sits above
   * a facet panel saying the same word again.
   */
  renderSources() {
    const holder = this.rail.querySelector("#rail-sources");
    const sources = (this.session && this.session.sources) || [];
    if (sources.length < 2) {
      replace(holder);
      return;
    }
    replace(
      holder,
      h("h2", { class: "rail__title" }, "Sources"),
      sources.slice(0, 8).map((s) =>
        h(
          "a",
          {
            class: "rail__link",
            href: `/?source=${encodeURIComponent(s.value)}`,
            title: label(s.value),
            onClick: (e) => this.link(e, { source: [s.value] }),
          },
          h("span", { class: "source__dot", style: { background: sourceColor(s.value) } }),
          h("span", { class: "rail__label" }, label(s.value)),
          h("span", { class: "rail__count" }, number(s.count)),
        ),
      ),
    );
  }

  /** go pushes a new query into the URL and renders it. */
  go(query, opts = {}) {
    const next = { ...query };
    // A different question gets a fresh cursor, and the same question keeps
    // the one it had. Opening a preview is the second case, which is what
    // makes the way back from a preview land on the row it opened from.
    if (!urlState.same(urlState.params(next, VERTICALS), urlState.params(this.query, VERTICALS))) {
      next.cursor = -1;
    }
    const url = urlState.write(next, opts.path === undefined ? this.here() : opts.path);
    if (opts.replace) history.replaceState(null, "", url);
    else history.pushState(null, "", url);
    this.query = urlState.read();
    this.sync();
  }

  /**
   * rememberCursor writes the row somebody is on into the address bar.
   *
   * Replace rather than push, because twenty presses of j are one journey and
   * not twenty, and back from a result should leave the results rather than
   * walk up the page a row at a time. Nothing is fetched and nothing is
   * rendered: the cursor is not part of the request, so the answer on screen is
   * already the answer to this URL.
   */
  rememberCursor(i) {
    if (this.query.cursor === i) return;
    this.query = { ...this.query, cursor: i };
    history.replaceState(null, "", urlState.write(this.query, this.here()));
  }

  /**
   * here is the path the query state currently belongs to.
   *
   * Two screens carry it: the search and the recent list. A document page never
   * does, so a go from one of those is a go back to the search, which is the
   * only place its query came from.
   */
  here() {
    return urlState.route().name === "recent" ? urlState.RECENT : "/";
  }

  /** sync renders whatever the URL currently says. */
  sync() {
    this.askWhatIsLeft();
    // Three routes: a document and the recent list by path, and everything else
    // by query string. A document is the only screen somebody links to from
    // outside the product, which is why it is the only one with an address that
    // does not carry the state of whoever found it.
    const route = urlState.route();
    this.markRail();
    if (route.name === "document") {
      this.showDocument(route.id);
      return;
    }
    if (route.name === "settings") {
      this.showSettings();
      return;
    }

    document.title = "genba";
    this.omnibox.value = this.query.q;
    const searching = Boolean(this.query.q) || urlState.count(this.query) > 0 || Boolean(this.query.sort);

    if (route.name === "recent") this.showRecent();
    else if (searching) this.search();
    else this.showHome();

    // The passage is part of what is on screen, not only the document. Two
    // quotes out of the same document are two different places to be sent to,
    // and comparing the id alone would make the second citation do nothing.
    const showing = this.query.open && `${this.query.open}\n${this.query.at}`;
    if (this.query.open && this.drawer.currentId !== showing) {
      this.drawer.currentId = showing;
      this.drawer.show(this.query.open, this.query.q, this.query.at);
    } else if (!this.query.open) {
      this.drawer.currentId = null;
      if (this.drawer.open) this.drawer.close();
    }
  }

  /**
   * showDocument renders one document as the whole page.
   *
   * The preview is closed without telling the shell, because the URL has
   * already moved to the document and the close handler would move it back.
   */
  async showDocument(id) {
    this.currentKey = "";
    // Both ways in count. A document reached by its own address, from a link
    // somebody was sent or from a new tab, was read exactly as much as one
    // opened in the preview beside a list.
    this.record(id);
    this.drawer.currentId = null;
    if (this.drawer.open) this.drawer.close({ notify: false, focus: false });
    if (this.main.firstChild !== this.page.el) replace(this.main, this.page.el);
    // A document page carries the words that found it, so it opens where they
    // are. A link somebody was sent carries none and opens at the top.
    await this.page.show(id, this.query.q);
  }

  /**
   * backFromDocument is where the way out of a document page leads.
   *
   * A document reached from a result list goes back to that list. A document
   * opened in a new tab, or from a link somebody was sent, has no list behind
   * it, and offering a back button that lands on whatever else was in that tab
   * is worse than offering the search.
   */
  backFromDocument() {
    const href = this.lastSearch || "/";
    return {
      href,
      title: this.lastSearch ? "Back to results" : "Search",
      // The destination rather than history.back(), so that the link and the
      // click agree. The previous history entry is not reliably the search this
      // page came from, and a back button that lands somewhere else is worse
      // than one that costs an entry.
      go: () => this.go(urlState.read(new URL(href, location.origin).search)),
    };
  }

  /**
   * showSettings renders the preferences, the keys, and who the server says
   * this is.
   *
   * It takes no arguments and asks the network for very little, so unlike every
   * other screen there is nothing here that can fail into an error page. The
   * two facts that come from the server are left out where they did not arrive,
   * which the screen says by not printing them.
   */
  async showSettings() {
    this.currentKey = "";
    this.drawer.currentId = null;
    if (this.drawer.open) this.drawer.close({ notify: false, focus: false });
    document.title = "Settings · genba";
    if (this.main.firstChild !== this.settings.el) replace(this.main, this.settings.el);
    await this.settings.render(this.session);
  }

  /**
   * backFromSettings is the way out, which is wherever this screen was opened
   * from.
   *
   * A tab that opened straight onto this address has nothing behind it, and a
   * back button that lands on whatever else was in that tab is worse than one
   * that offers the search.
   */
  backFromSettings() {
    const href = this.lastScreen || "/";
    return {
      href,
      title: this.lastScreen ? "Back" : "Search",
      // The address itself rather than history.back(), so that the link and the
      // click agree. That costs a history entry and buys a way out that lands
      // where it says it will, which is the same trade the document page makes.
      go: () => {
        history.pushState(null, "", href);
        this.query = urlState.read(new URL(href, location.origin).search);
        this.sync();
      },
    };
  }

  async showHome() {
    if (this.main.firstChild !== this.home.el) replace(this.main, this.home.el);
    await this.home.render(this.session);
  }

  /**
   * showRecent renders what this person opened and what changed.
   *
   * A failure only reaches the error screen when there is nothing to show. A
   * check that failed behind a list already on screen keeps the list, for the
   * same reason a search does: what is there was correct when it was painted,
   * and the banner already says the connection is gone.
   */
  async showRecent() {
    this.currentKey = this.recent.key();
    if (this.main.firstChild !== this.recent.el) replace(this.main, this.recent.el);
    try {
      await this.recent.render();
    } catch (err) {
      if (err.name === "AbortError") return;
      if (!this.recent.painted) this.fail(err);
    }
  }

  /** markRail says which rail entry is the screen somebody is on. */
  markRail() {
    const searching = Boolean(this.query.q) || urlState.count(this.query) > 0 || Boolean(this.query.sort);
    const route = urlState.route();
    const here =
      route.name === "recent" || route.name === "settings"
        ? route.name
        : route.name === "search" && !searching
          ? "home"
          : "";
    for (const link of this.rail.querySelectorAll("[data-route]")) {
      if (link.dataset.route === here) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    }
  }

  async search() {
    const showing = this.main.firstChild === this.results.el;
    if (!showing) replace(this.main, this.results.el);
    this.lastSearch = urlState.write(this.query);
    // Their own words, on their own machine. It happens here rather than in the
    // omnibox so that a search reached from a link or from the back button is
    // remembered too, which is what makes the list a history rather than a log
    // of what was typed.
    remember(this.query.q);

    const request = urlState.params(this.query, VERTICALS);
    const k = cache.key("search", request);
    this.currentKey = k;

    let painted = false;
    const paint = (res) => {
      // The address bar moved on while this was in flight, so this answer is to
      // a question nobody is asking any more.
      if (this.currentKey !== k) return;
      painted = true;
      this.stopWaiting();
      this.results.revalidating(false);
      this.results.render(this.query, res);
      this.announce(res);
      this.guessNextPage(res);
      if (!res.total) this.whyNothing(k);
    };

    // A first search has nothing on screen to keep, so it gets the skeleton
    // after the delay. A subsequent one keeps the previous answer visible and
    // shows a progress bar instead, because the previous answer is almost
    // always still the right one and dimming it says otherwise.
    this.stopWaiting();
    this.loadingTimer = setTimeout(() => {
      if (this.currentKey !== k) return;
      if (painted || showing) this.results.revalidating(true);
      else this.results.loading(this.query);
    }, LOADING_DELAY);

    // Far later, and only for a request that is not coming back. The control it
    // offers is only offered where there is nothing else on screen: a slow
    // check behind an answer somebody is already reading is not something they
    // asked for and not something they need a button for.
    this.slowTimer = setTimeout(() => {
      if (this.currentKey !== k || painted || showing) return;
      this.results.waiting(true, () => this.stopSearch(k));
    }, SLOW_DELAY);

    try {
      await cache.swr(k, (opts) => api.search(request, opts), paint);
    } catch (err) {
      if (err.name === "AbortError") return;
      // A check that failed behind an answer already on screen keeps the
      // answer. Replacing a page somebody is reading with an error page because
      // a background request failed is worse than being a minute out of date,
      // and the banner already says the connection is gone.
      if (!painted && this.currentKey === k) this.fail(err);
    } finally {
      this.stopWaiting();
      if (this.currentKey === k) this.results.revalidating(false);
    }
  }

  /** stopWaiting takes down both timers and anything either of them mounted. */
  stopWaiting() {
    clearTimeout(this.loadingTimer);
    clearTimeout(this.slowTimer);
    this.results.waiting(false);
  }

  /**
   * stopSearch abandons a request somebody has given up on.
   *
   * The words stay in the box and the address bar does not move, so trying
   * again is one key away and the link to this search is still the link to this
   * search. What was cached is kept, because a request being abandoned says
   * nothing about the answer that was already held.
   */
  stopSearch(k) {
    cache.cancel(k);
    this.stopWaiting();
    this.results.revalidating(false);
    this.results.cancelled(() => {
      this.currentKey = "";
      this.search();
    });
  }

  /**
   * whyNothing asks the one question the empty state cannot answer for itself.
   *
   * A zero that a filter caused is the most common way somebody decides a
   * search product is broken, and the only way to know whether the filters
   * caused it is to run the same words without them. It costs one request, in
   * the one case where there is nothing on screen for it to slow down, and the
   * state it adds a sentence to is already correct without it.
   */
  async whyNothing(k) {
    const bare = urlState.clear(this.query);
    const request = { ...urlState.params(bare, VERTICALS), limit: 1 };
    if (urlState.same(request, { ...urlState.params(this.query, VERTICALS), limit: 1 })) return;
    try {
      const res = await cache.swr(cache.key("search", request), (opts) => api.search(request, opts), () => {});
      if (this.currentKey !== k || !res) return;
      this.results.knows({ removed: res.total || 0 });
    } catch {
      // A sentence that does not appear. The state reads correctly without it,
      // and a second failure on a screen that is already empty is not worth
      // reporting twice.
    }
  }

  /**
   * connection shows or hides the offline banner.
   *
   * Nothing is dimmed and nothing is hidden while offline. What is on screen
   * was true when it was fetched, the banner says when that was, and that is a
   * more useful page than an error.
   */
  connection(online) {
    this.offline = !online;
    clearInterval(this.bannerTimer);
    this.showBanner();
    if (online) return;
    // The age is the whole point of the offline banner, so it keeps counting. A
    // tab left open through a long outage would otherwise still claim its
    // results are six seconds old.
    this.bannerTimer = setInterval(() => this.showBanner(), BANNER_TICK);
  }

  /**
   * askWhatIsLeft finds out whether the server is still reading a source.
   *
   * It costs nothing that was not already being spent. Every write to the index
   * is an event on the stream this page is already holding, a sync writes one
   * per batch, and each of those already brings the page here to check itself
   * against the server. So the count on the banner climbs on the server's own
   * schedule and there is no timer behind it.
   */
  async askWhatIsLeft() {
    try {
      const stats = await cache.swr(cache.key("stats", {}), (opts) => api.stats(opts), () => {});
      // Both numbers or nothing. A sync the server has not finished counting
      // reports a total of zero, and a banner reading "0 of about 0" is worse
      // than no banner: it is a sentence that makes somebody doubt the page
      // rather than the index.
      const running = stats && stats.indexing;
      const now = running && running.total > 0 ? running : null;
      // Compared rather than assigned, because this runs on every refresh and
      // the banner is on every screen. Rebuilding an unchanged one would move
      // it under a screen reader that is in the middle of reading it.
      if (same(this.indexing, now)) return;
      this.indexing = now;
      this.showBanner();
    } catch {
      // Nothing. A stats request that failed says nothing about whether a sync
      // is running, and the honest answer to not knowing is to leave the banner
      // exactly as it was.
    }
  }

  /**
   * showBanner writes the one banner there is.
   *
   * Two things can be true at once and only one line fits, so being offline
   * wins. Results that are partial because a sync is running are still the
   * server's current answer, and results from before the connection dropped are
   * not, which makes the second the larger caveat.
   */
  showBanner() {
    if (this.offline) {
      const held = this.currentKey ? cache.read(this.currentKey) : { state: "miss" };
      const age = held.state === "miss" ? "" : when(new Date(held.at).toISOString());
      this.banner.hidden = false;
      this.banner.dataset.state = "offline";
      replace(
        this.banner,
        h("span", { class: "banner__dot" }),
        age ? `You are offline. These are the results from ${age}.` : "You are offline. This is the last answer the server gave.",
      );
      return;
    }
    if (this.indexing) {
      const { source, done, total } = this.indexing;
      this.banner.hidden = false;
      this.banner.dataset.state = "indexing";
      replace(
        this.banner,
        h("span", { class: "banner__dot" }),
        h("span", { class: "banner__what" }, `Indexing ${label(source)}`),
        h("span", { class: "banner__count" }, `${number(done)} of about ${roughly(total)}`),
        h("span", { class: "banner__why" }, "Results are partial until this finishes."),
      );
      return;
    }
    // Emptied as well as hidden, so that the row it sits in reserves nothing
    // and the grid is the height it would be if this element were not here.
    this.banner.hidden = true;
    delete this.banner.dataset.state;
    replace(this.banner);
  }

  /**
   * guessNextPage fills the cache with the page after this one.
   *
   * Paging is the most predictable thing anybody does on a results page, and
   * one page ahead is where the guessing stops. Two pages ahead is a request
   * for something most people never look at.
   */
  guessNextPage(res) {
    clearTimeout(this.pageTimer);
    const limit = this.query.limit || 20;
    if (!res.hits || res.hits.length < limit) return;
    const next = urlState.params({ ...this.query, offset: (this.query.offset || 0) + limit }, VERTICALS);
    this.pageTimer = setTimeout(() => this.guess("search", next, (opts) => api.search(next, opts)), PREFETCH_PAGE);
  }

  /** guessDocument fetches what the pointer has been resting on. */
  guessDocument(id) {
    clearTimeout(this.hoverTimer);
    if (!id || this.guessing >= PREFETCH_LIMIT) return;
    this.hoverTimer = setTimeout(() => {
      this.guessing++;
      this.guess("document", { id }, (opts) => api.document(id, opts)).finally(() => this.guessing--);
    }, PREFETCH_HOVER);
  }

  /** guessSuggestion fetches the search behind a highlighted suggestion. */
  guessSuggestion(item) {
    clearTimeout(this.suggestionTimer);
    if (!item || item.kind === "operator") return;
    const text = item.value || item.text;
    if (!text) return;
    const request = urlState.params({ ...urlState.read(""), q: text }, VERTICALS);
    this.suggestionTimer = setTimeout(
      () => this.guess("search", request, (opts) => api.search(request, opts)),
      PREFETCH_SUGGESTION,
    );
  }

  /**
   * guess fills a cache entry for something nobody has asked for yet.
   *
   * A prefetch that fails is a prefetch that did not happen. It is not retried
   * and it never reaches the error state, because the real request will report
   * the failure properly and with something on screen to attach it to.
   */
  guess(name, params, run) {
    const k = cache.key(name, params);
    if (cache.read(k).state !== "miss") return Promise.resolve();
    return cache
      .once(k, (signal) => run({ signal }))
      .then((res) => {
        if (res.modified) cache.write(k, res.data, res.etag);
      })
      .catch(() => {});
  }

  /**
   * announce tells a screen reader what happened, because the results appear
   * somewhere else on the page and nothing else would say so.
   */
  announce(res) {
    const n = res.total || 0;
    this.say(`${n} ${n === 1 ? "result" : "results"} for ${this.query.q || "the current filters"}`);
  }

  /**
   * say puts one sentence in the polite region.
   *
   * Anything a view does that changes nothing on screen goes through here. A
   * copy button is the clearest case: the tick it draws is invisible to a
   * screen reader, so without this the action has no outcome at all.
   */
  say(text) {
    replace(this.live, text);
  }

  fail(err) {
    const unauthenticated = err instanceof ApiError && err.status === 401;
    // The one failure that empties the cache. Everything in it was read under a
    // session that is now over, and the next session is not entitled to it.
    if (unauthenticated) cache.clear();
    if (!unauthenticated) {
      replace(this.main, failed(err, () => this.sync()));
      return;
    }
    replace(
      this.main,
      h(
        "div",
        { class: "state state--error" },
        h("span", { class: "state__icon" }, svg(icon("people"), 40)),
        h("p", { class: "state__title" }, "Not signed in"),
        h(
          "p",
          { class: "state__body" },
          "This build authenticates from a request header. Pick who to search as and try again.",
        ),
        h(
          "div",
          { class: "state__actions" },
          h(
            "button",
            { class: "button button--primary", type: "button", onClick: () => this.switchIdentity() },
            "Choose an identity",
          ),
        ),
      ),
    );
  }

  /**
   * open shows a document in the preview.
   *
   * ahead is the direction the reader is travelling in, which is down until
   * they say otherwise. The document that way is fetched as soon as the preview
   * opens, because the next thing somebody reading a candidate does is ask for
   * the one under it.
   */
  open(id, ahead = 1) {
    this.record(id);
    // The passage goes with the document it was quoted from. Opening a row is a
    // request to read that document from the top, and a passage left over from
    // the last citation would scroll it to somewhere nobody asked to be.
    this.go({ ...this.query, open: id, at: "" }, { replace: true });
    this.guessNeighbour(ahead);
  }

  /**
   * cite opens the document a quote came from, at the quote.
   *
   * It goes through the preview rather than anything of its own, so a citation
   * lands in the same viewer as a result row, with the same keys, the same way
   * out and the neighbour already fetched. The only difference is the passage it
   * carries, and that is a parameter on the address rather than a mode the
   * drawer is put into, so the link can be copied and it still lands there.
   */
  cite(id, text) {
    this.record(id);
    this.go({ ...this.query, open: id, at: text }, { replace: true });
    this.guessNeighbour(1);
  }

  /**
   * step replaces what the open preview is showing with the row j or k moved to.
   *
   * The address moves with it, so the preview on screen is the preview a reload
   * or a copied link gives back, and the row under it is fetched behind the
   * paint so the next press has nothing to wait for.
   */
  step(delta) {
    const rows = this.rows();
    const hit = rows && rows.current();
    if (!hit || hit.id === this.query.open) return;
    this.open(hit.id, delta);
  }

  /**
   * guessNeighbour fetches the document on the other side of the one on screen.
   *
   * One ahead and no further, in the direction of travel. Two ahead is a
   * request for something most people never reach, and it goes through the same
   * guess as every other prefetch, so one that fails is one that did not happen.
   */
  guessNeighbour(delta) {
    const rows = this.rows();
    if (!rows || rows.selected < 0) return;
    const next = rows.hits[rows.selected + delta];
    if (!next) return;
    this.guess("document", { id: next.id }, (opts) => api.document(next.id, opts));
  }

  /**
   * record tells the server this document was read, and forgets what it knew.
   *
   * Nothing waits for either half. The write is fire and forget by design, and
   * the cache entry is marked stale rather than dropped, so the recent screen
   * still paints instantly from what it holds and finds the new row on the
   * revalidation behind that paint.
   */
  record(id) {
    // The same document twice running is one read. A document page is rendered
    // again on every background refresh, and without this the recency list
    // would be a record of how long a tab was left open.
    if (!id || this.recorded === id) return;
    this.recorded = id;
    api.recordOpen(id);
    cache.invalidate(cache.key("recent", {}));
  }

  /**
   * theme is the header button: the other one of the two.
   *
   * It toggles what is on screen rather than what is stored, because what is
   * stored may be nothing at all and somebody pressing this on a machine set to
   * dark means light. The settings screen is where the three way choice lives,
   * including the way back to following the system.
   */
  theme() {
    setTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark");
    this.header = this.buildHeader();
    this.build();
    this.sync();
  }

  /** density is the header button for the one escape hatch in the spacing system. */
  density() {
    setDensity(density() === "compact" ? "comfortable" : "compact");
  }

  /**
   * switchIdentity is how the permission model gets tested by hand.
   *
   * Every request carries these headers and the storage driver applies them, so
   * changing the subject or the groups here changes what the same query
   * returns. That is the fastest way to see for yourself that the filter is
   * real.
   */
  switchIdentity() {
    const who = identity();
    const subject = prompt("Search as which subject?", who.subject);
    if (subject === null) return;
    const tenant = prompt("Tenant, or empty for the single tenant case", who.tenant);
    if (tenant === null) return;
    const groups = prompt(
      "Groups, comma separated. These are the group keys a connector wrote, for example gdrive:eng@acme.com",
      who.groups.join(","),
    );
    if (groups === null) return;
    const identities = prompt(
      "Source identities, comma separated, as source:value. For example gdrive:mei@acme.com",
      who.identities.join(","),
    );
    if (identities === null) return;

    // The searches go with the results. They are this person's own input, and
    // the next person at this machine is not entitled to know what the last one
    // was looking for.
    forget();
    setIdentity({
      subject: subject.trim() || "dev",
      tenant: tenant.trim(),
      groups: groups.split(",").map((s) => s.trim()).filter(Boolean),
      identities: identities.split(",").map((s) => s.trim()).filter(Boolean),
    });
    location.reload();
  }

  /**
   * shortcuts is the dialog over whatever is on screen.
   *
   * It draws the same table the settings screen does and the same table the
   * listener below dispatches through, so there is one answer to what a key
   * does and three places that read it.
   */
  shortcuts() {
    const returnTo = document.activeElement;
    const dismiss = () => {
      dialog.remove();
      if (returnTo && returnTo.focus) returnTo.focus();
    };
    const dialog = h(
      "div",
      {
        class: "dialog",
        role: "dialog",
        "aria-modal": "true",
        "aria-label": "Keyboard shortcuts",
        onClick: (e) => {
          if (e.target === dialog) dismiss();
        },
        onKeydown: (e) => {
          if (e.key === "Escape" || e.key === "?") {
            e.preventDefault();
            e.stopPropagation();
            dismiss();
          }
        },
      },
      h(
        "div",
        { class: "dialog__panel", tabindex: "-1" },
        h("h2", { class: "dialog__title" }, "Keyboard shortcuts"),
        SHORTCUTS.map((row) => h("div", { class: "shortcut" }, row.what, keysOf(row))),
      ),
    );
    document.body.appendChild(dialog);
    dialog.querySelector(".dialog__panel").focus();
  }

  /**
   * page is the bracket keys: the page before this one and the page after.
   *
   * It reports whether it did anything, so that a bracket typed on the last
   * page is a bracket rather than a key that was quietly swallowed.
   */
  page(delta) {
    const limit = this.query.limit || 20;
    const offset = (this.query.offset || 0) + delta * limit;
    if (offset < 0 || offset >= this.results.total) return false;
    this.go({ ...this.query, offset, open: "", at: "" });
    return true;
  }

  /**
   * rows is the list the keyboard is walking, or nothing.
   *
   * Two screens are made of rows now and the keys mean the same thing on both,
   * so the shell asks which one is on screen rather than holding a reference to
   * the results and hoping. Home and the document page have no list at all,
   * which is why this returns nothing rather than an empty one: j on the home
   * screen should be a j, not a silent no-op somewhere else.
   */
  rows() {
    if (this.main.firstChild === this.results.el) return this.results.rows;
    if (this.main.firstChild === this.recent.el) return this.recent.active();
    return null;
  }

  /** showingResults reports whether the search is the screen on the page. */
  showingResults() {
    return this.main.firstChild === this.results.el;
  }

  /** goHome is the search with nothing asked of it, which is the home screen. */
  goHome() {
    this.go({ ...urlState.read("") }, { path: "/" });
  }

  /**
   * copyLink is the y key: a link to the result under the cursor.
   *
   * The link is this product's address for the document rather than the
   * source's, which is the same choice the title makes and for the same reason:
   * a connector's URL is whatever it found, and for a file that is a file://
   * URL that nobody can follow from a message.
   */
  copyLink() {
    const rows = this.rows();
    const hit = rows && rows.current();
    if (!hit) return false;
    const link = new URL(urlState.documentPath(hit.id), location.origin).href;
    const button = rows.copyTarget();
    // The tick where there is a control to draw it on, so the keyboard and the
    // pointer produce the same answer, and the spoken sentence either way.
    if (button) copies(button, link, (text) => this.say(text));
    else copy(link, (text) => this.say(text));
    return true;
  }

  /** inList reports whether focus is on a row of a document list. */
  inList() {
    const rows = this.rows();
    return Boolean(rows && rows.holds());
  }

  /**
   * bindKeys is the one keyboard listener, dispatching through the table.
   *
   * There is no switch here on purpose. Every key and everything it does lives
   * in keys.js, which is also what the settings screen and the shortcut sheet
   * print, so a key cannot be handled without being written down.
   */
  bindKeys() {
    let chord = null;
    document.addEventListener("keydown", (e) => {
      // Read and clear together. A press either completes an armed sequence or
      // ends it, and either way the next press starts from nothing.
      const armed = chord || "";
      chord = null;

      const row = binding(e, armed);
      if (row) {
        row.run(this, e);
        return;
      }

      // A two key sequence: g then a destination. The first key arms it and it
      // disarms itself, so a stray g does nothing rather than waiting forever.
      if (!armed && arms(e)) {
        chord = CHORD;
        setTimeout(() => (chord = null), CHORD_TIMEOUT);
      }
    });
  }
}

/**
 * countIn adds up the corpus wide counts of the kinds a vertical covers.
 *
 * This is the rail's number and it is deliberately not the tab strip's. The rail
 * says how much of this there is, which does not change while somebody types.
 * The tab says how much of this the current query matched.
 */
function countIn(kinds, vertical) {
  return (kinds || [])
    .filter((k) => vertical.kinds.includes(k.value))
    .reduce((n, k) => n + k.count, 0);
}

/**
 * same compares two indexing reports, either of which may be nothing.
 *
 * It is three fields written out rather than a stringify, because the second
 * one would compare key order as well and the server is free to change that.
 */
function same(a, b) {
  if (!a || !b) return !a && !b;
  return a.source === b.source && a.done === b.done && a.total === b.total;
}

// Theme before first paint, so a dark theme does not arrive as a white flash.
// The same two preferences are applied inline in index.html, and this call is
// what keeps the two in step when the module loads without a cached document.
applyPrefs();

const app = new App(document.getElementById("app"));
app.start();
