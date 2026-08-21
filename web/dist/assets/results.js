// The results view: verticals, active filters, result rows and facets.

import { h, replace, svg } from "genba/dom.js";
import { label, number, duration, icon, facetLabels } from "genba/format.js";
import { shapeOf } from "genba/content.js";
import { RowList } from "genba/rows.js";
import { nothingMatched, slow, stopped } from "genba/states.js";
import * as urlState from "genba/state.js";

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
  {
    id: "messages",
    title: "Messages",
    icon: "chat",
    kinds: ["message", "email"],
  },
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
  const held = VERTICALS.filter((v) =>
    v.kinds.some((k) => (counts.get(k) || 0) > 0),
  );
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

// The width at which the filter panel stops being a margin column and becomes a
// sheet. It is the same number as the layout breakpoint in app.css, and it is
// the line between a panel sitting beside the results and a dialog sitting over
// them, which decides whether Tab is allowed to leave it.
const SHEET = "(max-width: 960px)";

export class Results {
  constructor({ onQuery, onOpen, onHover, onSay, onCursor }) {
    this.onQuery = onQuery;
    this.expanded = new Set();
    this.hits = [];
    this.total = 0;
    // What the last paint was of, so that a sentence arriving after it can be
    // added without asking the server the same question a second time. The
    // answer to why there is nothing here is worked out in pieces: the search
    // comes back first, what the filters removed comes back after it, and what
    // the index holds may already be known or may still be in flight.
    this.query = null;
    this.res = null;
    this.context = {};
    this.waitingNow = false;
    // The rows and the keyboard that walks them are shared with the recent
    // screen. What is left in here is everything a result list has that a list
    // of documents does not: the verticals, the filters, the count and the
    // pager.
    this.rows = new RowList({ onOpen, onHover, onSay, onCursor });
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
    this.facets = h("aside", {
      class: "facets",
      "aria-label": "Filters",
      tabindex: "-1",
      onKeydown: (e) => this.trapFilters(e),
    });
    // Behind the sheet and never behind the column, which is what closes it by
    // clicking away from it on the one width where there is an away.
    this.scrim = h("div", {
      class: "scrim scrim--filters",
      hidden: true,
      onClick: () => this.showFilters(false),
    });

    // The facet column becomes a sheet below the medium breakpoint, where there
    // is no room for a margin column. The button is hidden by CSS above it
    // rather than by a resize listener, so nothing recomputes on drag.
    this.toggle = h(
      "button",
      {
        class: "button button--ghost filterbar__toggle",
        type: "button",
        "aria-expanded": "false",
        "aria-controls": "filters",
        onClick: () => this.showFilters(true),
      },
      svg(icon("slider"), 18),
      "Filters",
    );
    this.facets.id = "filters";
    // The sheet's own way out. Below the breakpoint the button that opened it is
    // behind the scrim, and a panel with no visible control to dismiss it is a
    // panel somebody closes by reloading the page.
    this.done = h(
      "button",
      {
        class: "icon-button facets__done",
        type: "button",
        "aria-label": "Close filters",
        onClick: () => this.showFilters(false),
      },
      svg(icon("close"), 20),
    );

    // A window dragged back across the breakpoint while the sheet is open turns
    // it into the margin column, and a margin column that still claims to be a
    // modal dialog makes the results beside it unreachable.
    window.matchMedia(SHEET).addEventListener("change", () => {
      if (!window.matchMedia(SHEET).matches) this.showFilters(false, { focus: false });
    });

    this.filterbar = h("div", { class: "filterbar" }, this.tabs, this.toggle);
    this.progress = h("div", { class: "progress", hidden: true });
    this.chips = h("div", { class: "chips" });
    this.head = h("div", { class: "results__head" });
    this.list = this.rows.el;
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
        this.scrim,
        this.facets,
        h("div", { class: "results__main" }, this.head, this.list, this.aside),
      ),
    );
  }

  /**
   * showFilters opens and closes the sheet, and is a no-op above the breakpoint
   * where the panel is simply on the page.
   *
   * The two are the same panel, which is the point of it. A second copy of the
   * filters for narrow windows is a second list of counts to keep in step with
   * the first, and the way those go wrong is that one of them is a query old.
   */
  showFilters(open, opts = {}) {
    const sheet = window.matchMedia(SHEET).matches;
    this.facets.dataset.open = String(open);
    this.toggle.setAttribute("aria-expanded", String(open));
    this.scrim.hidden = !open;
    // One surface. While the sheet is up, the button that opened it goes, and
    // the sheet's own close button is the only filter control on the screen.
    // Leaving it there is how the narrow layout ends up with a Filters button
    // sitting next to an open panel of filters.
    this.toggle.hidden = open && sheet;
    if (open && sheet) {
      // A dialog only while it is one. Above the breakpoint this is a
      // complementary region beside the results and saying it is modal would
      // take the results away from a screen reader.
      this.facets.setAttribute("role", "dialog");
      this.facets.setAttribute("aria-modal", "true");
      this.facets.focus();
      return;
    }
    this.facets.removeAttribute("role");
    this.facets.removeAttribute("aria-modal");
    if (!open && opts.focus !== false && this.toggle.offsetParent !== null) {
      this.toggle.focus();
    }
  }

  /**
   * trapFilters keeps Tab inside the sheet while the sheet is what is on
   * screen, and lets it out where the panel is a column beside the results.
   */
  trapFilters(e) {
    if (this.facets.dataset.open !== "true" || !window.matchMedia(SHEET).matches) return;
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      this.showFilters(false);
      return;
    }
    if (e.key !== "Tab") return;
    const focusable = this.facets.querySelectorAll("button:not([disabled])");
    if (!focusable.length) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
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
    // The list is emptied through the row list rather than around it, so that
    // the cursor and the hits it walks go with the rows on screen.
    this.hits = [];
    this.rows.skeleton();
    replace(this.chips);
    replace(
      this.head,
      h("div", {
        class: "skeleton",
        style: { width: "180px", height: "16px" },
      }),
    );
    replace(this.aside);
    replace(this.facets);
  }

  /**
   * waiting says a request has gone on long enough to be worth a word about,
   * and offers the way out of it.
   *
   * It goes under the skeleton rather than over it, because the skeleton is the
   * shape of the answer and this is a remark about the wait. Nothing already on
   * screen is touched: a person reading the previous answer while a slow check
   * runs behind it has not asked for anything and does not need a control.
   */
  waiting(on, onCancel) {
    if (on) {
      this.waitingNow = true;
      replace(this.aside, slow(onCancel));
      return;
    }
    // Only what this put there. The same corner of the screen holds the pager
    // and the empty state, and taking down a remark about a wait must not take
    // down the answer that ended it.
    if (!this.waitingNow) return;
    this.waitingNow = false;
    replace(this.aside);
  }

  /**
   * cancelled is what a search that somebody stopped leaves behind.
   *
   * Only ever with nothing on screen. A cancelled check behind an answer that
   * is already painted leaves that answer alone, because it was correct when it
   * was painted and stopping the check did not change that.
   */
  cancelled(onRetry) {
    this.hits = [];
    this.rows.render([], { view: "list", cursor: -1 });
    replace(this.head);
    replace(this.facets);
    replace(this.aside, stopped(onRetry));
  }

  /**
   * knows adds to what the empty state is allowed to say, and repaints it.
   *
   * Two things arrive after a search does. What the same words match without the
   * filters, which is a second search and only worth running when the first one
   * came back with nothing. And how many documents are in the index at all,
   * which decides whether an empty answer means nothing matched or nothing has
   * been indexed. Both are additions to a state that reads correctly without
   * them, so neither is waited for.
   */
  knows(more) {
    this.context = { ...this.context, ...more };
    if (this.hits.length || !this.query) return;
    replace(this.aside, this.empty(this.query));
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
    this.rows.busy(on);
  }

  /** render draws one answer, cursor and all. */
  render(query, res) {
    this.query = query;
    this.res = res;
    // What the filters removed was measured against the previous query, so it
    // is dropped here rather than carried. The spelling that would have worked
    // belongs to this answer for the same reason. What the index holds is a
    // fact about the corpus and survives.
    this.context = {
      ...this.context,
      removed: null,
      correction: res.correction || null,
    };
    this.hits = res.hits || [];
    // Kept because the paging keys have to know where the last page ends, and
    // the pager itself is rebuilt from the response every time.
    this.total = res.total || 0;
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
        // All is the total and the rest are added up out of the kind facet, so
        // which of the two numbers is a lower bound depends on the tab.
        const partial =
          res && (v.kinds.length ? res.facets_partial : res.partial);
        return h(
          "button",
          {
            class: "tab",
            role: "tab",
            type: "button",
            "aria-selected": String(active),
            onClick: () =>
              this.onQuery({ ...query, tab: v.id, kind: [], offset: 0 }),
          },
          v.title,
          count !== null &&
            count !== undefined &&
            h("span", { class: "tab__count" }, atLeast(count, partial)),
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
    const state =
      before && after ? "both" : before ? "start" : after ? "end" : "none";
    if (this.tabs.dataset.scroll !== state) this.tabs.dataset.scroll = state;
  }

  renderChips(query) {
    const active = chipsFor(query, this.onQuery);
    replace(
      this.chips,
      active,
      active.length > 0 &&
        h(
          "button",
          {
            class: "chip",
            type: "button",
            onClick: () => this.onQuery(urlState.clear(query)),
          },
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
            onChange: (e) =>
              this.onQuery({ ...query, sort: e.target.value, offset: 0 }),
          },
          h("option", { value: "", selected: !query.sort }, "Most relevant"),
          h(
            "option",
            { value: "recent", selected: query.sort === "recent" },
            "Most recent",
          ),
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
    return this.hits.length > 0 &&
      this.hits.every((hit) => shapeOf(hit) === "image")
      ? "grid"
      : "list";
  }

  // The list holds results and nothing else. A list that also holds its own
  // pager or its empty state is a list whose every child is announced as an
  // item, so a reader on a screen reader is told there are twenty one results
  // and the last one is a pair of buttons.
  renderList(query, res) {
    this.rows.render(this.hits, {
      view: this.viewFor(query),
      cursor: query.cursor,
    });
    replace(
      this.aside,
      this.hits.length ? pager(query, res, this.onQuery) : this.empty(query),
    );
  }

  /** empty is why there is nothing here, with everything known so far in it. */
  empty(query) {
    const chips = chipsFor(query, this.onQuery);
    // The vertical is a filter that does not look like one, because it is a tab
    // rather than a chip. On a page with nothing on it, a tab holding the whole
    // answer back while the state lists the filters and does not mention it is
    // the same dead end with better manners.
    const vertical = this.verticals.find(
      (v) => v.id === query.tab && v.id !== "all",
    );
    if (vertical) chips.push(tabChip(vertical, query, this.onQuery));
    return nothingMatched(query, this.onQuery, { ...this.context, chips });
  }

  renderFacets(query, res) {
    const groups = FACETS.map(({ key, field, title, titled }) => {
      const values = (res.facets && res.facets[field]) || [];
      if (!values.length) return null;
      const expanded = this.expanded.has(field);
      // Over the whole facet rather than over the eight that are showing, so
      // that a label does not change under somebody the moment they press Show
      // all and a ninth value collides with one already on screen.
      const named = facetLabels(values.map((v) => v.value));
      const shown = expanded ? values : values.slice(0, FACET_VISIBLE);
      return h(
        "section",
        { class: "facet" },
        h("h2", { class: "facet__title" }, title),
        h(
          "ul",
          {},
          shown.map((v, i) => {
            // The server says which values the query narrows to, because it made
            // that comparison already to answer the query and a person typing
            // from:mei@acme.com has not typed the display name the row carries.
            // The query is still consulted, so a facet arriving without the flag
            // still draws its ticks.
            const on = v.selected === true || (query[key] || []).includes(v.value);
            const { label: text, context } = named[i];
            return h(
              "li",
              {},
              h(
                "button",
                {
                  class: "facet__item",
                  type: "button",
                  "aria-pressed": String(on),
                  onClick: () =>
                    this.onQuery(urlState.toggle(query, key, v.value)),
                },
                h(
                  "span",
                  { class: "facet__box" },
                  on ? svg(icon("check"), 12) : null,
                ),
                h(
                  "span",
                  { class: "facet__text", title: v.value },
                  h(
                    "span",
                    { class: "facet__label" },
                    titled ? label(text) : text,
                  ),
                  // Isolated, so that the right to left trick that elides the
                  // front of the line does not reorder the slashes inside it.
                  context &&
                    h("span", { class: "facet__context" }, h("bdi", {}, context)),
                ),
                h(
                  "span",
                  { class: "facet__count" },
                  atLeast(v.count, res.facets_partial),
                ),
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
      groups.length
        ? h(
            "div",
            { class: "facets__head" },
            h("h2", { class: "facets__title" }, "Filters"),
            this.done,
          )
        : null,
      groups.length ? groups : null,
    );
  }

  /**
   * focusFilters is the f key.
   *
   * The first filter where the panel is on screen, and below the breakpoint the
   * sheet, opened, because the panel is behind a button there and focusing
   * something inside a closed one is focusing nothing.
   */
  focusFilters() {
    const first = this.facets.querySelector(".facet__item");
    if (first && first.offsetParent !== null) {
      first.focus();
      return;
    }
    if (this.toggle.offsetParent !== null) this.showFilters(true);
  }
}

/**
 * atLeast writes a count, and says so when it is only a lower bound.
 *
 * The server counts the facets over the first thousand documents that matched
 * rather than over all of them, because counting four fields of every matching
 * document is the one part of a search with no bound on it. Past that the number
 * under a filter is at least this many rather than exactly this many, and the
 * plus is how the result count already says the same thing about a total it
 * stopped counting.
 */
function atLeast(n, partial) {
  return partial ? `${number(n)}+` : number(n);
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
  return (
    { source: "Source", kind: "Type", container: "In", author: "Person" }[
      field
    ] || field
  );
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
        onClick: () =>
          onQuery({ ...query, offset: Math.max(0, offset - limit) }),
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
 * chipsFor is one control per filter that is on, each removing only itself.
 *
 * The same controls are offered above a list of results and inside the state
 * that says there are none, because they are the same claim about what is on.
 * Two sets built from the same query in two places is two chances to disagree
 * about it.
 */
function tabChip(vertical, query, onQuery) {
  return h(
    "span",
    { class: "chip chip--on" },
    `Showing: ${vertical.title}`,
    h(
      "button",
      {
        class: "chip__remove",
        type: "button",
        "aria-label": `Search everything rather than ${vertical.title}`,
        onClick: () => onQuery({ ...query, tab: "all", kind: [], offset: 0 }),
      },
      svg(icon("close"), 14),
    ),
  );
}

function chipsFor(query, onQuery) {
  const out = [];
  for (const { key, field } of FACETS) {
    for (const value of query[key] || []) {
      out.push(
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
              onClick: () => onQuery(urlState.toggle(query, key, value)),
            },
            svg(icon("close"), 14),
          ),
        ),
      );
    }
  }
  return out;
}
