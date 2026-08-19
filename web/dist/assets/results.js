// The results view: verticals, filter bar, result cards and facets.

import { h, replace, svg } from "./dom.js";
import { kindIcon, sourceColor, label, when, exact, number, duration, icon } from "./format.js";
import * as urlState from "./state.js";

/**
 * VERTICALS are the tabs above the results.
 *
 * They are presets over document kinds rather than separate indexes, so the
 * counts across them add up and moving between them does not run a different
 * query with different ranking. A tab with no kinds is everything.
 */
export const VERTICALS = [
  { id: "all", title: "All", kinds: [] },
  { id: "documents", title: "Documents", kinds: ["page", "file"] },
  { id: "messages", title: "Messages", kinds: ["message", "email"] },
  { id: "tickets", title: "Tickets", kinds: ["ticket"] },
  { id: "code", title: "Code", kinds: ["code"] },
  { id: "people", title: "People", kinds: ["person"] },
];

// A source and a kind are ours and read better capitalised. A container and a
// person are somebody else's string, and a folder called store/sqlitestore is
// not called Store/sqlitestore, so those are shown exactly as they arrived.
const FACETS = [
  { key: "source", field: "source", title: "Source", titled: true },
  { key: "kind", field: "kind", title: "Type", titled: true },
  { key: "container", field: "container", title: "In" },
  { key: "author", field: "author", title: "Person" },
];

const FACET_VISIBLE = 6;

export class Results {
  constructor({ onQuery, onOpen }) {
    this.onQuery = onQuery;
    this.onOpen = onOpen;
    this.expanded = new Set();
    this.selected = -1;
    this.hits = [];

    this.tabs = h("div", { class: "tabs", role: "tablist", "aria-label": "Result types" });
    this.filterbar = h("div", { class: "filterbar" });
    this.list = h("div", { class: "results__list", role: "list" });
    this.facets = h("aside", { class: "facets", "aria-label": "Filters" });

    this.el = h(
      "div",
      {},
      this.tabs,
      this.filterbar,
      h("div", { class: "results" }, this.list, this.facets),
    );
  }

  /** loading paints the shape of the answer before the answer arrives. */
  loading(query) {
    this.renderTabs(query, null);
    replace(
      this.filterbar,
      h("div", { class: "skeleton", style: { width: "220px", height: "20px" } }),
    );
    replace(
      this.list,
      Array.from({ length: 5 }, () =>
        h(
          "div",
          { class: "skeleton-card" },
          h("div", { class: "skeleton", style: { width: "30%", height: "12px" } }),
          h("div", { class: "skeleton", style: { width: "70%", height: "18px" } }),
          h("div", { class: "skeleton", style: { width: "100%", height: "12px" } }),
          h("div", { class: "skeleton", style: { width: "85%", height: "12px" } }),
        ),
      ),
    );
    replace(this.facets);
  }

  render(query, res) {
    this.hits = res.hits || [];
    this.selected = -1;
    this.renderTabs(query, res);
    this.renderFilterbar(query, res);
    this.renderList(query, res);
    this.renderFacets(query, res);
  }

  renderTabs(query, res) {
    replace(
      this.tabs,
      VERTICALS.map((v) => {
        const active = (query.tab || "all") === v.id;
        const count = res ? countFor(res, v) : null;
        return h(
          "button",
          {
            class: "tab",
            role: "tab",
            type: "button",
            "aria-selected": String(active),
            onClick: () => this.onQuery({ ...query, tab: v.id, kind: [], offset: 0 }),
          },
          v.title,
          count !== null && count !== undefined && h("span", { class: "tab__count" }, number(count)),
        );
      }),
    );
  }

  renderFilterbar(query, res) {
    const active = [];
    for (const { key, field } of FACETS) {
      for (const value of query[key] || []) {
        active.push(
          h(
            "span",
            { class: "chip chip--on" },
            `${labelFor(field)}: ${value}`,
            h(
              "button",
              {
                class: "chip__remove",
                type: "button",
                "aria-label": `Remove the ${labelFor(field)} filter ${value}`,
                onClick: () => this.onQuery(urlState.toggle(query, key, value)),
              },
              svg(icon("close"), 12),
            ),
          ),
        );
      }
    }

    replace(
      this.filterbar,
      active,
      active.length > 0 &&
        h(
          "button",
          { class: "chip", type: "button", onClick: () => this.onQuery(urlState.clear(query)) },
          "Clear all",
        ),
      h("span", { class: "filterbar__spacer" }),
      h(
        "span",
        { class: "filterbar__meta" },
        res.partial
          ? `${number(res.total)}+ results in ${duration(res.took_ms)}`
          : `${number(res.total)} ${res.total === 1 ? "result" : "results"} in ${duration(res.took_ms)}`,
      ),
      h(
        "label",
        { class: "filterbar__meta" },
        h("span", { class: "visually-hidden" }, "Sort results"),
        h(
          "select",
          {
            class: "select",
            onChange: (e) => this.onQuery({ ...query, sort: e.target.value, offset: 0 }),
          },
          h("option", { value: "", selected: !query.sort }, "Most relevant"),
          h("option", { value: "recent", selected: query.sort === "recent" }, "Most recent"),
        ),
      ),
    );
  }

  renderList(query, res) {
    if (!this.hits.length) {
      replace(this.list, emptyState(query, res, this.onQuery));
      return;
    }

    replace(
      this.list,
      this.hits.map((hit, i) => this.card(hit, i)),
      pager(query, res, this.onQuery),
    );
  }

  card(hit, i) {
    const open = () => this.onOpen(hit.id);
    return h(
      "article",
      {
        class: "card",
        role: "listitem",
        dataset: { index: String(i) },
        onClick: (e) => {
          // A click on the title follows the source link. A click anywhere else
          // on the card opens the preview, which is the cheaper of the two and
          // the one people want when they are still scanning.
          if (e.target.closest("a")) return;
          open();
        },
      },
      h(
        "div",
        { class: "card__head" },
        h(
          "span",
          { class: "source" },
          h("span", { class: "source__dot", style: { background: sourceColor(hit.source) } }),
          label(hit.source),
        ),
        h("span", { class: "crumbs__sep" }, "/"),
        h("span", { class: "crumbs" }, svg(kindIcon(hit.kind), 13), label(hit.kind)),
        hit.container && h("span", { class: "crumbs__sep" }, "/"),
        hit.container && h("span", { class: "crumbs" }, hit.container),
      ),
      hit.url
        ? h("a", { class: "card__title", href: hit.url, rel: "noreferrer noopener", target: "_blank" }, hit.title || hit.id)
        : h("button", { class: "card__title", type: "button", onClick: open }, hit.title || hit.id),
      h("p", { class: "card__snippet" }, passages(hit)),
      h(
        "div",
        { class: "card__foot" },
        hit.author && h("span", {}, hit.author),
        hit.modified_at &&
          h("span", { title: exact(hit.modified_at) }, `Updated ${when(hit.modified_at)}`),
      ),
      h(
        "div",
        { class: "card__actions" },
        h(
          "button",
          { class: "icon-button", type: "button", title: "Preview (p)", "aria-label": "Preview", onClick: open },
          svg(icon("preview"), 15),
        ),
        hit.url &&
          h(
            "a",
            {
              class: "icon-button",
              href: hit.url,
              target: "_blank",
              rel: "noreferrer noopener",
              title: "Open in source",
              "aria-label": "Open in source",
            },
            svg(icon("external"), 15),
          ),
      ),
    );
  }

  renderFacets(query, res) {
    const groups = FACETS.map(({ key, field, title, titled }) => {
      const values = (res.facets && res.facets[field]) || [];
      if (!values.length) return null;
      const expanded = this.expanded.has(field);
      const shown = expanded ? values : values.slice(0, FACET_VISIBLE);
      return h(
        "section",
        { class: "facet" },
        h("h2", { class: "facet__title" }, title),
        h(
          "ul",
          {},
          shown.map((v) => {
            const on = (query[key] || []).includes(v.value);
            return h(
              "li",
              {},
              h(
                "button",
                {
                  class: "facet__item",
                  type: "button",
                  "aria-pressed": String(on),
                  onClick: () => this.onQuery(urlState.toggle(query, key, v.value)),
                },
                h("span", { class: "facet__box" }, on ? svg(icon("check"), 10) : null),
                h("span", { class: "facet__label", title: v.value }, titled ? label(v.value) : v.value),
                h("span", { class: "facet__count" }, number(v.count)),
              ),
            );
          }),
        ),
        values.length > FACET_VISIBLE &&
          h(
            "button",
            {
              class: "facet__more",
              type: "button",
              onClick: () => {
                if (expanded) this.expanded.delete(field);
                else this.expanded.add(field);
                this.renderFacets(query, res);
              },
            },
            expanded ? "Show fewer" : `Show all ${values.length}`,
          ),
      );
    }).filter(Boolean);

    replace(this.facets, groups.length ? groups : null);
  }

  /** move walks the selection with j and k, and scrolls it into view. */
  move(delta) {
    if (!this.hits.length) return;
    const next = Math.min(Math.max(this.selected + delta, 0), this.hits.length - 1);
    this.select(next);
  }

  select(i) {
    const cards = this.list.querySelectorAll(".card");
    cards.forEach((card, n) => card.setAttribute("data-active", String(n === i)));
    this.selected = i;
    if (cards[i]) cards[i].scrollIntoView({ block: "nearest" });
  }

  current() {
    return this.hits[this.selected];
  }
}

function countFor(res, vertical) {
  if (!vertical.kinds.length) return res.total;
  const kinds = (res.facets && res.facets.kind) || [];
  // The counts are over the current result set, so a vertical with nothing in
  // it shows nothing rather than a zero that invites a click.
  const total = kinds
    .filter((k) => vertical.kinds.includes(k.value))
    .reduce((n, k) => n + k.count, 0);
  return total || undefined;
}

function labelFor(field) {
  return { source: "Source", kind: "Type", container: "In", author: "Person" }[field] || field;
}

/**
 * passages renders the snippet with the matched words marked.
 *
 * The server decided which runs matched, using the analyzer that produced the
 * index. Marking them here with a substring search instead would highlight
 * things the index never matched, which teaches people the wrong thing about
 * why a result came back.
 */
function passages(hit) {
  if (!hit.passages || !hit.passages.length) return hit.snippet || "";
  return hit.passages.map((p) => (p.match ? h("mark", {}, p.text) : document.createTextNode(p.text)));
}

function pager(query, res, onQuery) {
  const limit = query.limit || 20;
  const offset = query.offset || 0;
  const hasPrev = offset > 0;
  const hasNext = offset + limit < res.total;
  if (!hasPrev && !hasNext) return null;

  return h(
    "nav",
    { class: "filterbar", "aria-label": "Result pages" },
    h(
      "button",
      {
        class: "button",
        type: "button",
        disabled: !hasPrev,
        onClick: () => onQuery({ ...query, offset: Math.max(0, offset - limit) }),
      },
      "Previous",
    ),
    h(
      "span",
      { class: "filterbar__meta" },
      `${number(offset + 1)} to ${number(Math.min(offset + limit, res.total))} of ${number(res.total)}`,
    ),
    h(
      "button",
      {
        class: "button",
        type: "button",
        disabled: !hasNext,
        onClick: () => onQuery({ ...query, offset: offset + limit }),
      },
      "Next",
    ),
  );
}

/**
 * emptyState says why there is nothing here and offers the way out.
 *
 * The three cases are genuinely different. Nothing typed is not a failure.
 * Nothing matched with filters on is almost always the filters. Nothing matched
 * without filters is a query to change or a source that is not connected, and
 * saying so is better than a shrug.
 */
function emptyState(query, res, onQuery) {
  if (!query.q && urlState.count(query) === 0) {
    return h(
      "div",
      { class: "state" },
      h("p", { class: "state__title" }, "Search your company"),
      h("p", { class: "state__body" }, "Start typing above. Add app:, type:, in: or from: to narrow it down."),
    );
  }
  if (urlState.count(query) > 0 || (query.tab && query.tab !== "all")) {
    return h(
      "div",
      { class: "state" },
      h("p", { class: "state__title" }, "No results with these filters"),
      h("p", { class: "state__body" }, `Nothing matches ${query.q ? `"${query.q}"` : "this search"} in the selected filters.`),
      h(
        "div",
        { class: "state__actions" },
        h(
          "button",
          { class: "button button--primary", type: "button", onClick: () => onQuery(urlState.clear(query)) },
          "Clear filters",
        ),
      ),
    );
  }
  return h(
    "div",
    { class: "state" },
    h("p", { class: "state__title" }, `Nothing found for "${query.q}"`),
    h(
      "p",
      { class: "state__body" },
      "Try fewer words, or check that the source you are looking in has finished indexing.",
    ),
  );
}
