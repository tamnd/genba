// The home screen.
//
// It answers three questions and nothing else. What is in here, from the session
// and the index statistics. What was I doing, from the recent endpoint and from
// the searches this browser remembers. What is going on, which is what changed
// in the corpus. Everything on it is a real read against the same API the search
// page uses, so nothing here can show a document the person could not have found
// by searching for it.

import { h, replace } from "./dom.js";
import { api } from "./api.js";
import { cache } from "./cache.js";
import { queries } from "./queries.js";
import { LIMIT as RECENT_LIMIT } from "./recent.js";
import { label, sourceColor, when, number, initials } from "./format.js";
import { firstRun } from "./states.js";

// How many rows a panel on this screen carries. Home is a summary and the whole
// answer is one click away, so six is a panel somebody reads rather than one
// they scroll.
const PANEL_ROWS = 6;

export class Home {
  constructor({ onQuery, onOpen, onVisit }) {
    this.onQuery = onQuery;
    this.onOpen = onOpen;
    this.onVisit = onVisit;
    this.el = h("div", { class: "home" });
  }

  /**
   * render paints the home screen, from cache if there is one.
   *
   * Home is the destination of the back button and of the brand in the rail, so
   * it is loaded far more often than it is loaded for the first time. Both reads
   * are painted from whatever is held, and repainted only if the check behind
   * them comes back with something different.
   */
  async render(session) {
    const recentKey = cache.key("recent", { limit: RECENT_LIMIT });
    const statsKey = cache.key("stats", {});
    let recent = cache.read(recentKey).data || null;
    let stats = cache.read(statsKey).data || null;
    this.paint(session, recent, stats);

    let changed = false;
    // The paint callbacks fire once with what was already on screen, which is
    // not a change, and again only if the server disagreed with it.
    await Promise.all([
      cache
        .swr(recentKey, (opts) => api.recent(RECENT_LIMIT, opts), (d) => {
          if (d === recent) return;
          recent = d;
          changed = true;
        })
        .catch(() => {}),
      cache
        .swr(statsKey, (opts) => api.stats(opts), (d) => {
          if (d === stats) return;
          stats = d;
          changed = true;
        })
        .catch(() => {}),
    ]);
    if (changed) this.paint(session, recent, stats);
  }

  /** paint draws the screen, with skeletons standing in for what is not here. */
  paint(session, recent, stats) {
    // An index with nothing in it gets the screen about that and not this one.
    // A first install currently sees a dashboard reading zero in three places,
    // which is the only first impression this program will ever get and is
    // spent on a disappointment. Only once the count is known, because a
    // dashboard that flashes the install screen while it loads is worse again.
    if (stats && stats.documents === 0) {
      replace(this.el, firstRun());
      return;
    }
    replace(
      this.el,
      h(
        "header",
        { class: "home__greeting" },
        h("h1", { class: "home__title" }, greeting(session)),
        h("p", { class: "home__subtitle" }, "Everything your company knows, in one search."),
      ),
      recent
        ? h(
            "div",
            { class: "home__grid" },
            this.openedPanel(recent),
            this.changedPanel(recent),
            this.searchesPanel(),
            this.sourcesPanel(session),
            this.statsPanel(stats, session),
            this.tipsPanel(),
          )
        : h("div", { class: "home__grid" }, this.skeletonPanels()),
    );
  }

  skeletonPanels() {
    return Array.from({ length: 4 }, () =>
      h(
        "div",
        { class: "panel" },
        h("div", { class: "skeleton", style: { width: "40%", height: "24px", marginBottom: "24px" } }),
        ...Array.from({ length: 4 }, () =>
          h("div", { class: "skeleton", style: { width: "100%", height: "16px", marginBottom: "24px" } }),
        ),
      ),
    );
  }

  /**
   * openedPanel is what this person was reading.
   *
   * It comes from the server rather than from this browser, because the whole
   * value of the list is that it follows somebody from the laptop to the desk
   * machine. It is also the panel that is empty on a first visit, and an empty
   * panel that says why is better than one that is missing.
   */
  openedPanel(recent) {
    const opened = (recent && recent.opened) || [];
    return h(
      "section",
      { class: "panel" },
      this.head("Recently opened"),
      opened.length
        ? opened.slice(0, PANEL_ROWS).map((hit) => this.documentRow(hit, hit.at))
        : h("p", { class: "meta" }, "Nothing yet. Whatever you open shows up here, on any machine you sign in from."),
    );
  }

  /** changedPanel is what moved in the corpus, which is nobody's history. */
  changedPanel(recent) {
    const changed = (recent && recent.changed) || [];
    return h(
      "section",
      { class: "panel" },
      this.head("Recently updated"),
      changed.length
        ? changed.slice(0, PANEL_ROWS).map((hit) => this.documentRow(hit, hit.modified_at))
        : h("p", { class: "meta" }, "Nothing has been indexed yet."),
    );
  }

  /** head is a panel title with the way to the whole list beside it. */
  head(title) {
    return h(
      "div",
      { class: "panel__head" },
      h("h2", { class: "panel__title" }, title),
      h(
        "a",
        {
          class: "panel__link",
          href: "/recent",
          onClick: (e) => {
            if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
            e.preventDefault();
            this.onVisit("/recent");
          },
        },
        "See all",
      ),
    );
  }

  documentRow(hit, at) {
    return h(
      "button",
      { class: "panel__row", type: "button", onClick: () => this.onOpen(hit.id) },
      h("span", { class: "source__dot", style: { background: sourceColor(hit.source) } }),
      h("span", { class: "panel__row-title" }, hit.title || hit.id),
      h("span", { class: "panel__row-meta" }, when(at)),
    );
  }

  /**
   * searchesPanel is what this person searched for, from this browser.
   *
   * The one panel on the screen that is not a read. A query is somebody's own
   * input rather than corpus content, so it stays on their machine, which is
   * written down in the client cache specification and is why this list is empty
   * in a fresh browser even when the opened list is not.
   */
  searchesPanel() {
    const held = queries();
    if (!held.length) return null;
    return h(
      "section",
      { class: "panel" },
      h("div", { class: "panel__head" }, h("h2", { class: "panel__title" }, "Your searches")),
      held.slice(0, PANEL_ROWS).map((q) =>
        h(
          "button",
          { class: "panel__row", type: "button", onClick: () => this.onQuery({ q }) },
          h("span", { class: "panel__row-title" }, q),
        ),
      ),
    );
  }

  sourcesPanel(session) {
    const sources = (session && session.sources) || [];
    return h(
      "section",
      { class: "panel" },
      h("div", { class: "panel__head" }, h("h2", { class: "panel__title" }, "Sources")),
      sources.length
        ? sources.map((s) =>
            h(
              "button",
              {
                class: "panel__row",
                type: "button",
                onClick: () => this.onQuery({ q: "", source: [s.value] }),
              },
              h("span", { class: "source__dot", style: { background: sourceColor(s.value) } }),
              h("span", { class: "panel__row-title" }, label(s.value)),
              h("span", { class: "panel__row-meta" }, `${number(s.count)} documents`),
            ),
          )
        : h(
            "p",
            { class: "meta" },
            "No source has documents you can read yet. Start genbad with -corpus to index a directory.",
          ),
    );
  }

  statsPanel(stats, session) {
    if (!stats) return null;
    return h(
      "section",
      { class: "panel" },
      h("div", { class: "panel__head" }, h("h2", { class: "panel__title" }, "Index")),
      h(
        "div",
        { class: "stat" },
        h("span", { class: "stat__value" }, number(stats.documents)),
        h("span", { class: "stat__label" }, "documents indexed"),
      ),
      h(
        "div",
        { class: "stat" },
        h("span", { class: "stat__value" }, number(stats.quarantined)),
        h(
          "span",
          { class: "stat__label" },
          "held back because their permissions did not resolve, and never served to anyone",
        ),
      ),
      session && session.tenant ? h("p", { class: "meta" }, `Tenant ${session.tenant}`) : null,
    );
  }

  tipsPanel() {
    const tips = [
      ["app:slack", "only what came from one connector"],
      ["type:ticket", "only one kind of document"],
      ["in:platform", "a space, folder or channel"],
      ["from:mei", "written by a person"],
      ["updated:week", "changed in the last seven days"],
    ];
    return h(
      "section",
      { class: "panel" },
      h("div", { class: "panel__head" }, h("h2", { class: "panel__title" }, "Narrow a search")),
      // These rows run a search rather than opening a document, so their title
      // is prose rather than a link and stays in the text colour in every
      // state. The operator itself is the thing being offered.
      tips.map(([op, what]) =>
        h(
          "button",
          {
            class: "panel__row panel__row--static",
            type: "button",
            onClick: () => this.onQuery({ q: op }),
          },
          h("code", { class: "suggestion__badge" }, op),
          h("span", { class: "panel__row-title" }, what),
        ),
      ),
    );
  }
}

function greeting(session) {
  const hour = new Date().getHours();
  const part = hour < 5 ? "Good evening" : hour < 12 ? "Good morning" : hour < 18 ? "Good afternoon" : "Good evening";
  const who = session && session.subject ? session.subject : "";
  return who ? `${part}, ${who}` : part;
}

export { initials };
