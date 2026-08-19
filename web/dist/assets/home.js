// The home screen.
//
// It answers "what is in here and what changed" without anybody having to
// think of a query first. Everything on it is a real query against the same
// API the search page uses, so nothing here can show a document the person
// could not have found by searching for it.

import { h, replace } from "./dom.js";
import { api } from "./api.js";
import { label, sourceColor, when, number, initials } from "./format.js";

export class Home {
  constructor({ onQuery, onOpen }) {
    this.onQuery = onQuery;
    this.onOpen = onOpen;
    this.el = h("div", { class: "home" });
  }

  async render(session) {
    replace(
      this.el,
      h(
        "header",
        { class: "home__greeting" },
        h("h1", { class: "home__title" }, greeting(session)),
        h("p", { class: "home__subtitle" }, "Everything your company knows, in one search."),
      ),
      h("div", { class: "home__grid" }, this.skeletonPanels()),
    );

    const [recent, stats] = await Promise.all([
      api.search({ sort: "recent", limit: 6 }).catch(() => null),
      api.stats().catch(() => null),
    ]);

    replace(
      this.el,
      h(
        "header",
        { class: "home__greeting" },
        h("h1", { class: "home__title" }, greeting(session)),
        h("p", { class: "home__subtitle" }, "Everything your company knows, in one search."),
      ),
      h(
        "div",
        { class: "home__grid" },
        this.recentPanel(recent),
        this.sourcesPanel(session, recent),
        this.statsPanel(stats, session),
        this.tipsPanel(),
      ),
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

  recentPanel(res) {
    const hits = (res && res.hits) || [];
    return h(
      "section",
      { class: "panel" },
      h(
        "div",
        { class: "panel__head" },
        h("h2", { class: "panel__title" }, "Recently updated"),
        h(
          "button",
          { class: "panel__link", type: "button", onClick: () => this.onQuery({ q: "", sort: "recent" }) },
          "See all",
        ),
      ),
      hits.length
        ? hits.map((hit) =>
            h(
              "button",
              { class: "panel__row", type: "button", onClick: () => this.onOpen(hit.id) },
              h("span", { class: "source__dot", style: { background: sourceColor(hit.source) } }),
              h("span", { class: "panel__row-title" }, hit.title || hit.id),
              h("span", { class: "panel__row-meta" }, when(hit.modified_at)),
            ),
          )
        : h("p", { class: "meta" }, "Nothing has been indexed yet."),
    );
  }

  sourcesPanel(session, res) {
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
