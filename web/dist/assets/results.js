// The results view: verticals, active filters, result rows and facets.

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

const FACET_VISIBLE = 8;

export class Results {
  constructor({ onQuery, onOpen }) {
    this.onQuery = onQuery;
    this.onOpen = onOpen;
    this.expanded = new Set();
    this.selected = -1;
    this.hits = [];

    this.tabs = h("div", { class: "tabs", role: "tablist", "aria-label": "Result types" });
    this.facets = h("aside", { class: "facets", "aria-label": "Filters" });

    // The facet column becomes a disclosure below the medium breakpoint, where
    // there is no room for a margin column. The button is hidden by CSS above
    // it rather than by a resize listener, so nothing recomputes on drag.
    this.toggle = h(
      "button",
      {
        class: "button button--ghost filterbar__toggle",
        type: "button",
        "aria-expanded": "false",
        onClick: () => {
          const open = this.facets.dataset.open === "true";
          this.facets.dataset.open = String(!open);
          this.toggle.setAttribute("aria-expanded", String(!open));
        },
      },
      svg(icon("slider"), 18),
      "Filters",
    );

    this.filterbar = h("div", { class: "filterbar" }, this.tabs, this.toggle);
    this.progress = h("div", { class: "progress", hidden: true });
    this.chips = h("div", { class: "chips" });
    this.head = h("div", { class: "results__head" });
    this.list = h("div", { class: "results__list", role: "list" });

    this.el = h(
      "div",
      {},
      this.filterbar,
      this.progress,
      this.chips,
      h(
        "div",
        { class: "results" },
        this.facets,
        h("div", { class: "results__main" }, this.head, this.list),
      ),
    );
  }

  /**
   * loading paints the shape of the answer before the answer arrives.
   *
   * The shell decides when to call this. Under the 120ms threshold in the
   * motion spec it never calls it at all, and the previous answer stays on
   * screen until the new one replaces it in a single paint.
   */
  loading(query) {
    this.renderTabs(query, null);
    replace(this.chips);
    replace(this.head, h("div", { class: "skeleton", style: { width: "180px", height: "16px" } }));
    replace(
      this.list,
      Array.from({ length: 6 }, () =>
        h(
          "div",
          { class: "skeleton-result" },
          h("div", { class: "skeleton", style: { width: "60%", height: "20px" } }),
          h("div", { class: "skeleton", style: { width: "40%", height: "14px" } }),
          h("div", { class: "skeleton", style: { width: "100%", height: "14px" } }),
          h("div", { class: "skeleton", style: { width: "82%", height: "14px" } }),
        ),
      ),
    );
    replace(this.facets);
  }

  /**
   * revalidating shows that a cached answer is being checked.
   *
   * The content underneath is not dimmed, blurred or overlaid. Dimming stale
   * content tells the reader that what they are currently reading is wrong,
   * which it almost never is.
   */
  revalidating(on) {
    this.progress.hidden = !on;
    this.list.setAttribute("aria-busy", String(Boolean(on)));
  }

  render(query, res) {
    this.hits = res.hits || [];
    this.selected = -1;
    this.renderTabs(query, res);
    this.renderChips(query);
    this.renderHead(query, res);
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

  renderChips(query) {
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
              svg(icon("close"), 14),
            ),
          ),
        );
      }
    }

    replace(
      this.chips,
      active,
      active.length > 0 &&
        h(
          "button",
          { class: "chip", type: "button", onClick: () => this.onQuery(urlState.clear(query)) },
          "Clear all",
        ),
    );
  }

  renderHead(query, res) {
    replace(
      this.head,
      h(
        "span",
        { class: "results__count" },
        res.partial
          ? `${number(res.total)}+ results`
          : `${number(res.total)} ${res.total === 1 ? "result" : "results"}`,
        h("span", { class: "results__took" }, duration(res.took_ms)),
      ),
      h(
        "label",
        {},
        h("span", { class: "visually-hidden" }, "Sort results"),
        h(
          "select",
          {
            class: "sort",
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
      this.hits.map((hit, i) => this.row(hit, i)),
      pager(query, res, this.onQuery),
    );
  }

  /**
   * row is one result.
   *
   * Title first, then the line of provenance, then the snippet. The old order
   * put provenance above the title, which meant the first thing on every row
   * was the least distinguishing thing about it.
   */
  row(hit, i) {
    const open = () => this.onOpen(hit.id);
    return h(
      "article",
      {
        class: "result",
        role: "listitem",
        dataset: { index: String(i) },
        onClick: (e) => {
          // A click on the title follows the source link. A click anywhere else
          // on the row opens the preview, which is the cheaper of the two and
          // the one people want when they are still scanning.
          if (e.target.closest("a")) return;
          open();
        },
      },
      hit.url
        ? h(
            "a",
            { class: "result__title", href: hit.url, rel: "noreferrer noopener", target: "_blank" },
            hit.title || hit.id,
          )
        : h("button", { class: "result__title", type: "button", onClick: open }, hit.title || hit.id),
      h(
        "div",
        { class: "result__meta" },
        h(
          "span",
          { class: "source" },
          h("span", { class: "source__dot", style: { background: sourceColor(hit.source) } }),
          label(hit.source),
        ),
        h("span", { class: "crumbs__sep" }, "·"),
        h("span", { class: "crumbs" }, svg(kindIcon(hit.kind), 14), label(hit.kind)),
        hit.container && h("span", { class: "crumbs__sep" }, "·"),
        hit.container && h("span", { class: "crumbs" }, hit.container),
        hit.author && h("span", { class: "crumbs__sep" }, "·"),
        hit.author && h("span", { class: "crumbs" }, hit.author),
        hit.modified_at && h("span", { class: "crumbs__sep" }, "·"),
        hit.modified_at &&
          h("time", { title: exact(hit.modified_at), datetime: hit.modified_at }, when(hit.modified_at)),
      ),
      h("p", { class: "result__snippet" }, passages(hit)),
      h(
        "div",
        { class: "result__actions" },
        h(
          "button",
          { class: "icon-button", type: "button", title: "Preview (p)", "aria-label": "Preview", onClick: open },
          svg(icon("preview"), 18),
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
            svg(icon("external"), 18),
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
                h("span", { class: "facet__box" }, on ? svg(icon("check"), 12) : null),
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

    replace(
      this.facets,
      groups.length ? h("h2", { class: "facets__title" }, "Filters") : null,
      groups.length ? groups : null,
    );
  }

  /** move walks the selection with j and k, and scrolls it into view. */
  move(delta) {
    if (!this.hits.length) return;
    const next = Math.min(Math.max(this.selected + delta, 0), this.hits.length - 1);
    this.select(next);
  }

  select(i) {
    const rows = this.list.querySelectorAll(".result");
    rows.forEach((row, n) => row.setAttribute("data-active", String(n === i)));
    this.selected = i;
    if (rows[i]) rows[i].scrollIntoView({ block: "nearest" });
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
    { class: "pager", "aria-label": "Result pages" },
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
      { class: "pager__meta" },
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
      h("span", { class: "state__icon" }, svg(icon("search"), 40)),
      h("p", { class: "state__title" }, "Search your company"),
      h("p", { class: "state__body" }, "Start typing above. Add app:, type:, in: or from: to narrow it down."),
    );
  }
  if (urlState.count(query) > 0 || (query.tab && query.tab !== "all")) {
    return h(
      "div",
      { class: "state" },
      h("span", { class: "state__icon" }, svg(icon("slider"), 40)),
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
    h("span", { class: "state__icon" }, svg(icon("search"), 40)),
    h("p", { class: "state__title" }, `Nothing found for "${query.q}"`),
    h(
      "p",
      { class: "state__body" },
      "Try fewer words, or check that the source you are looking in has finished indexing.",
    ),
  );
}
