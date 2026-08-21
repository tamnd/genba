// The application shell.
//
// It owns three things and nothing else: the URL, the keyboard, and which view
// is on screen. Everything that draws is in its own module and takes a callback
// rather than reaching back in here, which is what keeps a change to the result
// row from being a change to routing.

import { h, replace, svg } from "./dom.js";
import { api, identity, setIdentity, ApiError } from "./api.js";
import { cache } from "./cache.js";
import { Live } from "./live.js";
import * as urlState from "./state.js";
import { followable, icon, initials, label, number, sourceColor, when } from "./format.js";
import { Omnibox, modifierLabel, shortcutLabel } from "./omnibox.js";
import { Results, VERTICALS, verticalsFor } from "./results.js";
import { Drawer } from "./drawer.js";
import { Page } from "./page.js";
import { Home } from "./home.js";

const THEME_KEY = "genba.theme";
const DENSITY_KEY = "genba.density";

// LOADING_DELAY is how long a search may run before the interface admits to it.
//
// Every API in this product is budgeted under ten milliseconds, and at that
// latency a loading state appears and disappears inside one frame, which reads
// as a flicker rather than as feedback. So the skeleton is a timer that the
// response cancels: if the answer beats it, no loading state is ever mounted
// and the transition is a single paint.
const LOADING_DELAY = 120;

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
    this.currentKey = "";
    this.hoverTimer = null;
    this.pageTimer = null;
    this.suggestionTimer = null;
    this.guessing = 0;
    // The last results page this session painted, so that a document page has
    // somewhere to go back to. It is empty in a tab that opened straight onto a
    // document, which is the case the back link has to answer differently.
    this.lastSearch = "";

    this.omnibox = new Omnibox({
      onSearch: (text) => this.go({ ...this.query, q: text, offset: 0, open: "" }),
      onOpen: (id) => this.open(id),
      onHighlight: (item) => this.guessSuggestion(item),
    });
    this.results = new Results({
      onQuery: (q) => this.go(q),
      onOpen: (id) => this.open(id),
      onHover: (id) => this.guessDocument(id),
      onSay: (text) => this.say(text),
    });
    this.home = new Home({
      onQuery: (q) => this.go({ ...urlState.read(""), ...q }),
      onOpen: (id) => this.open(id),
    });
    this.drawer = new Drawer({
      onClose: () => this.go({ ...this.query, open: "" }, { replace: true }),
      onSay: (text) => this.say(text),
    });
    this.page = new Page({
      onBack: () => this.backFromDocument(),
      onSay: (text) => this.say(text),
    });

    this.main = h("main", {
      class: "main",
      id: "main",
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

    this.banner = h("div", { class: "banner", role: "status", hidden: true });

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
            "aria-current": this.query.q || urlState.count(this.query) ? null : "page",
            onClick: (e) => this.link(e, {}),
          },
          svg(icon("home"), 20),
          h("span", { class: "rail__label" }, "Home"),
        ),
        h(
          "a",
          {
            class: "rail__link",
            // Rooted. A query string on its own resolves against whatever path
            // is showing, which was always the search until a document got a
            // path of its own, and would otherwise make every rail entry a link
            // back to the same document with a stray parameter on the end.
            href: "/?sort=recent",
            title: "Recent",
            onClick: (e) => this.link(e, { q: "", sort: "recent" }),
          },
          svg(icon("clock"), 20),
          h("span", { class: "rail__label" }, "Recent"),
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
    this.go({ ...urlState.read(""), ...query });
  }

  async start() {
    try {
      this.session = (await api.me()).data;
      // Everything the cache holds is keyed under this, so it is set before
      // anything is read and the switcher below empties the cache by changing
      // it. Nothing cached under one identity is reachable from another.
      cache.as(this.session.view || "");
      this.results.verticals = verticalsFor(this.session.kinds);
      this.renderVerticals();
      this.renderSources();
    } catch (err) {
      this.fail(err);
      return;
    }
    this.sync();
    this.refresher.start();
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
    const url = urlState.write(query);
    if (opts.replace) history.replaceState(null, "", url);
    else history.pushState(null, "", url);
    this.query = urlState.read();
    this.sync();
  }

  /** sync renders whatever the URL currently says. */
  sync() {
    // Two routes: a document by path, and everything else by query string. A
    // document is the only screen somebody links to from outside the product,
    // which is why it is the only one with an address that does not carry the
    // state of whoever found it.
    const route = urlState.route();
    if (route.name === "document") {
      this.showDocument(route.id);
      return;
    }

    document.title = "genba";
    this.omnibox.value = this.query.q;
    const searching = Boolean(this.query.q) || urlState.count(this.query) > 0 || Boolean(this.query.sort);

    if (searching) this.search();
    else this.showHome();

    if (this.query.open && this.drawer.currentId !== this.query.open) {
      this.drawer.currentId = this.query.open;
      this.drawer.show(this.query.open);
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
    this.drawer.currentId = null;
    if (this.drawer.open) this.drawer.close({ notify: false, focus: false });
    if (this.main.firstChild !== this.page.el) replace(this.main, this.page.el);
    await this.page.show(id);
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

  async showHome() {
    if (this.main.firstChild !== this.home.el) replace(this.main, this.home.el);
    await this.home.render(this.session);
  }

  async search() {
    const showing = this.main.firstChild === this.results.el;
    if (!showing) replace(this.main, this.results.el);
    this.lastSearch = urlState.write(this.query);

    const request = urlState.params(this.query, VERTICALS);
    const k = cache.key("search", request);
    this.currentKey = k;

    let painted = false;
    const paint = (res) => {
      // The address bar moved on while this was in flight, so this answer is to
      // a question nobody is asking any more.
      if (this.currentKey !== k) return;
      painted = true;
      clearTimeout(this.loadingTimer);
      this.results.revalidating(false);
      this.results.render(this.query, res);
      this.announce(res);
      this.guessNextPage(res);
    };

    // A first search has nothing on screen to keep, so it gets the skeleton
    // after the delay. A subsequent one keeps the previous answer visible and
    // shows a progress bar instead, because the previous answer is almost
    // always still the right one and dimming it says otherwise.
    clearTimeout(this.loadingTimer);
    this.loadingTimer = setTimeout(() => {
      if (this.currentKey !== k) return;
      if (painted || showing) this.results.revalidating(true);
      else this.results.loading(this.query);
    }, LOADING_DELAY);

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
      clearTimeout(this.loadingTimer);
      if (this.currentKey === k) this.results.revalidating(false);
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
    this.banner.hidden = online;
    clearInterval(this.bannerTimer);
    if (online) return;
    this.sayHowOld();
    // The age is the whole point of the banner, so it keeps counting. A tab
    // left open through a long outage would otherwise still claim its results
    // are six seconds old.
    this.bannerTimer = setInterval(() => this.sayHowOld(), BANNER_TICK);
  }

  /** sayHowOld writes the offline banner, naming the age of what is on screen. */
  sayHowOld() {
    const held = this.currentKey ? cache.read(this.currentKey) : { state: "miss" };
    const age = held.state === "miss" ? "" : when(new Date(held.at).toISOString());
    replace(
      this.banner,
      h("span", { class: "banner__dot" }),
      age ? `You are offline. These are the results from ${age}.` : "You are offline. This is the last answer the server gave.",
    );
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
    replace(
      this.main,
      h(
        "div",
        { class: "state state--error" },
        h("span", { class: "state__icon" }, svg(icon(unauthenticated ? "people" : "close"), 40)),
        h("p", { class: "state__title" }, unauthenticated ? "Not signed in" : "Something went wrong"),
        h(
          "p",
          { class: "state__body" },
          unauthenticated
            ? "This build authenticates from a request header. Pick who to search as and try again."
            : err.message,
        ),
        h(
          "div",
          { class: "state__actions" },
          unauthenticated
            ? h(
                "button",
                { class: "button button--primary", type: "button", onClick: () => this.switchIdentity() },
                "Choose an identity",
              )
            : h(
                "button",
                { class: "button button--primary", type: "button", onClick: () => this.sync() },
                "Try again",
              ),
        ),
      ),
    );
  }

  open(id) {
    this.go({ ...this.query, open: id }, { replace: true });
  }

  theme() {
    const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    localStorage.setItem(THEME_KEY, next);
    this.header = this.buildHeader();
    this.build();
    this.sync();
  }

  /**
   * density is the one escape hatch in the spacing system.
   *
   * The restyle costs vertical space deliberately, and somebody triaging four
   * hundred results has a different job from somebody reading one. Compact
   * reduces the row padding and drops the body size one step. It does not
   * reintroduce borders, change the colour system or touch the type scale
   * above body, so it is two token overrides rather than a second theme.
   */
  density() {
    const next = document.documentElement.dataset.density === "compact" ? "comfortable" : "compact";
    document.documentElement.dataset.density = next;
    localStorage.setItem(DENSITY_KEY, next);
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

    setIdentity({
      subject: subject.trim() || "dev",
      tenant: tenant.trim(),
      groups: groups.split(",").map((s) => s.trim()).filter(Boolean),
      identities: identities.split(",").map((s) => s.trim()).filter(Boolean),
    });
    location.reload();
  }

  shortcuts() {
    const rows = [
      ["Focus search", [shortcutLabel()]],
      ["Next result", ["j"]],
      ["Previous result", ["k"]],
      ["Open preview", ["Enter"]],
      ["Open as a page, in a new tab", [modifierLabel(), "Enter"]],
      ["Open in source", ["o"]],
      ["Go home", ["g", "h"]],
      ["Close", ["Esc"]],
      ["This list", ["?"]],
    ];
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
        rows.map(([what, keys]) =>
          h(
            "div",
            { class: "shortcut" },
            what,
            h("span", { class: "shortcut__keys" }, keys.map((k) => h("kbd", { class: "kbd" }, k))),
          ),
        ),
      ),
    );
    document.body.appendChild(dialog);
    dialog.querySelector(".dialog__panel").focus();
  }

  bindKeys() {
    let chord = null;
    document.addEventListener("keydown", (e) => {
      const typing = e.target.matches("input, textarea, select");

      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        this.omnibox.focus();
        return;
      }
      // The modifier means the same thing here as it does on a link: open it
      // over there and leave me where I am.
      if ((e.metaKey || e.ctrlKey) && e.key === "Enter" && !typing) {
        const hit = this.results.current();
        if (!hit) return;
        e.preventDefault();
        window.open(urlState.documentPath(hit.id), "_blank", "noreferrer");
        return;
      }
      if (e.key === "Escape") {
        if (this.drawer.open) return; // the drawer handles its own Escape
        if (typing) e.target.blur();
        return;
      }
      if (typing || e.metaKey || e.ctrlKey || e.altKey) return;

      // A two key sequence: g then a destination. The first key arms it and it
      // disarms itself, so a stray g does nothing rather than waiting forever.
      if (chord === "g") {
        chord = null;
        const destinations = { h: {}, s: { q: "", sort: "recent" }, a: { tab: "all" } };
        if (destinations[e.key]) {
          e.preventDefault();
          this.go({ ...urlState.read(""), ...destinations[e.key] });
        }
        return;
      }

      switch (e.key) {
        case "/":
          e.preventDefault();
          this.omnibox.focus();
          break;
        case "g":
          chord = "g";
          setTimeout(() => (chord = null), 1200);
          break;
        case "j":
          e.preventDefault();
          this.results.move(1);
          break;
        case "k":
          e.preventDefault();
          this.results.move(-1);
          break;
        case "Enter":
        case "p": {
          const hit = this.results.current();
          if (!hit) return;
          e.preventDefault();
          this.open(hit.id);
          break;
        }
        case "o": {
          // Only where a browser would go there. Opening a file:// URL in a new
          // tab from an HTTP page leaves somebody looking at a blank tab, which
          // is worse than the key doing nothing.
          const hit = this.results.current();
          if (!hit || !followable(hit.url)) return;
          e.preventDefault();
          window.open(hit.url, "_blank", "noreferrer");
          break;
        }
        case "?":
          e.preventDefault();
          this.shortcuts();
          break;
        default:
          break;
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

// Theme before first paint, so a dark theme does not arrive as a white flash.
// The same block runs inline in index.html, and this one is what keeps the two
// in step when the module loads without a cached document.
const saved = localStorage.getItem(THEME_KEY);
if (saved) {
  document.documentElement.dataset.theme = saved;
} else if (window.matchMedia("(prefers-color-scheme: dark)").matches) {
  document.documentElement.dataset.theme = "dark";
}

const density = localStorage.getItem(DENSITY_KEY);
if (density) document.documentElement.dataset.density = density;

const app = new App(document.getElementById("app"));
app.start();
