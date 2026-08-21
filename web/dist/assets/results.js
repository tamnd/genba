// The results view: verticals, active filters, result rows and facets.

import { h, replace, svg } from "./dom.js";
import {
  kindIcon,
  sourceColor,
  label,
  when,
  exact,
  number,
  duration,
  icon,
  followable,
  copyable,
} from "./format.js";
import { tile, cover, shapeOf } from "./content.js";
import { copies } from "./clipboard.js";
import * as urlState from "./state.js";

/**
 * VERTICALS are the tabs above the results.
 *
 * They are presets over document kinds rather than separate indexes, so the
 * counts across them add up and moving between them does not run a different
 * query with different ranking. A tab with no kinds is everything.
 *
 * The table is the editorial part and it stays a constant, because which kinds
 * belong together is a judgement a count cannot make. Which of them a person is
 * shown is decided by verticalsFor below.
 */
export const VERTICALS = [
  { id: "all", title: "All", icon: "rows", kinds: [] },
  { id: "documents", title: "Documents", icon: "doc", kinds: ["page", "file"] },
  { id: "messages", title: "Messages", icon: "chat", kinds: ["message", "email"] },
  { id: "tickets", title: "Tickets", icon: "ticket", kinds: ["ticket"] },
  { id: "code", title: "Code", icon: "code", kinds: ["code"] },
  { id: "images", title: "Images", icon: "image", kinds: ["image", "video"] },
  { id: "people", title: "People", icon: "people", kinds: ["person"] },
];

/**
 * verticalsFor is the navigation this viewer should actually be offered.
 *
 * It takes the per kind counts from /api/v1/me, which are corpus wide and for
 * this principal, and keeps a vertical when at least one of its kinds has a
 * document in it. Counts for the current query are a different number and they
 * belong on the tab; a tab that vanished when a search narrowed would be a tab
 * nobody could use to widen one again.
 *
 * Being per viewer rather than per deployment is the same argument the source
 * list already makes. Telling somebody a vertical exists that they have nothing
 * in says a little about what other people can see.
 *
 * Two boundary cases both come out as nothing at all. An empty index reads as a
 * system with nothing in it rather than as one with five broken tabs. A corpus
 * that lands in exactly one vertical makes that vertical and All the same set of
 * documents, and a choice between two identical answers is not a choice.
 */
export function verticalsFor(kinds) {
  const counts = new Map((kinds || []).map((k) => [k.value, k.count]));
  const held = VERTICALS.filter((v) => v.kinds.some((k) => (counts.get(k) || 0) > 0));
  return held.length > 1 ? [VERTICALS[0], ...held] : [];
}

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
  constructor({ onQuery, onOpen, onHover, onSay }) {
    this.onQuery = onQuery;
    this.onOpen = onOpen;
    this.onHover = onHover || (() => {});
    this.onSay = onSay || (() => {});
    this.expanded = new Set();
    this.selected = -1;
    this.hits = [];
    // Empty until the session says what the corpus holds, so the strip is never
    // painted with tabs that are about to be taken away.
    this.verticals = [];

    this.tabs = h("div", {
      class: "tabs",
      role: "tablist",
      "aria-label": "Result types",
      hidden: true,
      onScroll: () => this.edges(),
    });
    // The strip only scrolls on a narrow viewport, and whether it can is a
    // question about its own width rather than about the window's, since the
    // facet column comes and goes beside it.
    if (typeof ResizeObserver === "function") {
      new ResizeObserver(() => this.edges()).observe(this.tabs);
    }
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
    this.aside = h("div", { class: "results__aside" });

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
        h("div", { class: "results__main" }, this.head, this.list, this.aside),
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
          // The skeleton rows sit inside the list, so they carry the role its
          // children are supposed to carry. A placeholder that is not an item
          // makes the list itself invalid while it is loading.
          { class: "skeleton-result", role: "listitem" },
          h("div", { class: "skeleton skeleton-result__tile" }),
          h("div", { class: "skeleton", style: { width: "60%", height: "20px" } }),
          h("div", { class: "skeleton", style: { width: "40%", height: "14px" } }),
          h("div", { class: "skeleton", style: { width: "100%", height: "14px" } }),
          h("div", { class: "skeleton", style: { width: "82%", height: "14px" } }),
        ),
      ),
    );
    replace(this.aside);
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
    this.tabs.hidden = this.verticals.length === 0;
    replace(
      this.tabs,
      this.verticals.map((v) => {
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
    this.edges();
  }

  /**
   * edges records which way the tab strip can still be scrolled.
   *
   * The fade at the edge of a narrow strip means there is more over there, so it
   * has to know whether there is. A mask that is always on fades the last tab of
   * a strip that fits, which reads as a rendering fault rather than as an
   * invitation to scroll.
   */
  edges() {
    const room = this.tabs.scrollWidth - this.tabs.clientWidth;
    // A scroll position on a display whose pixel ratio is not one lands on a
    // fraction and never reaches the far end exactly, so each end gets a pixel.
    const before = this.tabs.scrollLeft > 1;
    const after = room > 1 && this.tabs.scrollLeft < room - 1;
    const state = before && after ? "both" : before ? "start" : after ? "end" : "none";
    if (this.tabs.dataset.scroll !== state) this.tabs.dataset.scroll = state;
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
      this.viewToggle(query),
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

  /**
   * viewToggle offers the grid where a grid would be worth having.
   *
   * A page with no pictures in it is a page where every cell would be an icon
   * and a file name, which is a list with more scrolling, so the control is not
   * offered at all rather than offered and pointless. Where it is offered it
   * writes the choice into the URL, because the shape of a page somebody sends
   * to a colleague should be the shape they were looking at.
   */
  viewToggle(query) {
    if (!this.hits.some((hit) => shapeOf(hit) === "image")) return null;
    const current = this.viewFor(query);
    const button = (id, title, name) =>
      h(
        "button",
        {
          class: "view__button",
          type: "button",
          "aria-pressed": String(current === id),
          title,
          "aria-label": title,
          onClick: () => this.onQuery({ ...query, view: id }),
        },
        svg(icon(name), 18),
      );
    return h(
      "div",
      { class: "view", role: "group", "aria-label": "Result layout" },
      button("list", "List", "rows"),
      button("grid", "Grid", "grid"),
    );
  }

  /**
   * viewFor is which layout this page of results is drawn in.
   *
   * The URL wins where it says anything, and where it says nothing the answer
   * comes from the results themselves: a page that is all pictures is a page
   * where the file names are the least useful thing on it, and a grid shows
   * three times as many of them in the same space. A page with one screenshot
   * and nineteen documents stays a list, because a grid of mostly icons is a
   * worse list.
   */
  viewFor(query) {
    if (query.view) return query.view;
    return this.hits.length > 0 && this.hits.every((hit) => shapeOf(hit) === "image") ? "grid" : "list";
  }

  // The list holds results and nothing else. A list that also holds its own
  // pager or its empty state is a list whose every child is announced as an
  // item, so a reader on a screen reader is told there are twenty one results
  // and the last one is a pair of buttons.
  renderList(query, res) {
    if (!this.hits.length) {
      this.list.dataset.view = "list";
      replace(this.list);
      replace(this.aside, emptyState(query, res, this.onQuery));
      return;
    }

    const view = this.viewFor(query);
    this.list.dataset.view = view;
    replace(
      this.list,
      this.hits.map((hit, i) => (view === "grid" ? this.cell(hit, i) : this.row(hit, i))),
    );
    replace(this.aside, pager(query, res, this.onQuery));
  }

  /**
   * cell is one result in the grid.
   *
   * The picture is the result and everything else is a label under it, so the
   * cell carries the title and one line of provenance and nothing more. A
   * snippet is left out rather than truncated: an image has no text to snip, and
   * the ones that do are the reason the list view still exists.
   */
  cell(hit, i) {
    const open = () => this.onOpen(hit.id);
    return h(
      "article",
      {
        class: "cell",
        role: "listitem",
        dataset: { index: String(i) },
        onMouseenter: () => this.onHover(hit.id),
        onMouseleave: () => this.onHover(null),
        onClick: (e) => {
          if (e.target.closest("a, button")) return;
          open();
        },
      },
      cover(hit),
      h(
        "a",
        {
          class: "cell__title",
          href: urlState.documentPath(hit.id),
          title: hit.title || hit.id,
          onClick: (e) => {
            if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
            e.preventDefault();
            open();
          },
        },
        hit.title || hit.id,
      ),
      // Where it is, rather than what it is. A grid of pictures from one source
      // is twenty four copies of the same word, and the folder a picture is in
      // is the thing that tells two similar screenshots apart.
      h(
        "div",
        { class: "cell__meta" },
        h("span", { class: "source__dot", style: { background: sourceColor(hit.source) } }),
        h("span", { class: "cell__where", title: hit.container || label(hit.source) }, hit.container || label(hit.source)),
      ),
    );
  }

  /**
   * row is one result.
   *
   * A tile, then the title, then the line of provenance, then the snippet. The
   * old order put provenance above the title, which meant the first thing on
   * every row was the least distinguishing thing about it. The tile is the only
   * part of a row that is not words, and for an image it is the whole answer.
   */
  row(hit, i) {
    const open = () => this.onOpen(hit.id);
    return h(
      "article",
      {
        class: "result",
        role: "listitem",
        dataset: { index: String(i) },
        // A pointer resting on a row is a good guess at the next preview, and
        // the shell is what decides how long resting means and how many of
        // those guesses may be in the air at once.
        onMouseenter: () => this.onHover(hit.id),
        onMouseleave: () => this.onHover(null),
        onClick: (e) => {
          // Anything that is already a control handles its own click. A click
          // on the rest of the row opens the preview, which is what somebody
          // scanning a list wants and is cheaper than loading a page.
          if (e.target.closest("a, button")) return;
          open();
        },
      },
      tile(hit),
      // The title is an anchor to this document's own page, and the default is
      // prevented on a plain left click so that the preview opens instead.
      //
      // It is an anchor rather than a button so that a middle click, a command
      // click, the context menu and copying the link address all do what they
      // do everywhere else on the web. It points at us rather than at the
      // document's source, because a source URL is whatever a connector found
      // and for the file connector that is a file:// URL, which a browser
      // served over HTTP will not navigate to. Clicking the primary target on
      // every row of a file corpus did nothing at all.
      h(
        "a",
        {
          class: "result__title",
          href: urlState.documentPath(hit.id),
          onClick: (e) => {
            if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
            e.preventDefault();
            open();
          },
        },
        hit.title || hit.id,
      ),
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
      // A row with nothing to quote does not reserve the space for a quote. An
      // image has no text in it, so every image row used to end in two empty
      // lines, which is what made a list of screenshots look like a list of
      // documents that had failed to load.
      hasText(hit) && h("p", { class: "result__snippet" }, passages(hit)),
      h(
        "div",
        { class: "result__actions" },
        h(
          "button",
          { class: "icon-button", type: "button", title: "Preview (p)", "aria-label": "Preview", onClick: open },
          svg(icon("preview"), 18),
        ),
        // Opening at the source is the secondary action, and it is only offered
        // where it would work. Where it would not, the path is the useful thing
        // to hand over, so the button copies it rather than pretending to be a
        // link and doing nothing.
        followable(hit.url)
          ? h(
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
            )
          : hit.url &&
            h(
              "button",
              {
                class: "icon-button",
                type: "button",
                title: "Copy path",
                "aria-label": "Copy path",
                onClick: (e) => copies(e.currentTarget, copyable(hit.url), this.onSay),
              },
              svg(icon("copy"), 18),
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
    // Both layouts, by the attribute they share rather than by class, so j and
    // k walk a grid exactly as they walk a list.
    const rows = this.list.querySelectorAll("[data-index]");
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
/** hasText reports whether the server found anything in this hit worth quoting. */
function hasText(hit) {
  return Boolean((hit.passages && hit.passages.length) || hit.snippet);
}

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
