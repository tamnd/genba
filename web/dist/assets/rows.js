// A list of documents, and the keyboard that walks it.
//
// It is one module rather than part of the results view because there is more
// than one screen made of these rows now. The recent screen shows what somebody
// opened and what changed, and those have to be the same row as a search result:
// the same tile, the same line of provenance, the same click that opens a
// preview, the same j and k. A second implementation of a row would be a second
// place every one of those decisions lives, and the two would drift within a
// month.
//
// What it does not know about is where the documents came from. There is no
// query in here, no paging, no facets and no empty state, because those are
// different questions on every screen that uses this one.

import { h, replace, svg } from "./dom.js";
import { kindIcon, sourceColor, label, when, exact, icon, followable, copyable } from "./format.js";
import { tile, cover } from "./content.js";
import { copies } from "./clipboard.js";
import * as urlState from "./state.js";

/**
 * asked is the words in the address bar.
 *
 * A row does not hold a query and is not about to start. It reads the address
 * when it builds a link, which is the same thing every other reader of this
 * state does: the address bar is the state, and a copy of it kept in here would
 * be a copy that could disagree.
 */
function asked() {
  return urlState.read().q;
}

export class RowList {
  constructor({ onOpen, onHover, onSay, onCursor, label: name } = {}) {
    this.onOpen = onOpen || (() => {});
    this.onHover = onHover || (() => {});
    this.onSay = onSay || (() => {});
    this.onCursor = onCursor || (() => {});
    this.hits = [];
    this.selected = -1;

    this.el = h("div", { class: "results__list", role: "list" });
    // A screen with two of these on it has two lists, and a reader moving
    // between them is told which is which. The results page has one and its
    // heading is the count above it, so it passes no name.
    if (name) this.el.setAttribute("aria-label", name);
  }

  /**
   * render draws one page of documents.
   *
   * The cursor is a parameter rather than state, because this runs again on
   * every repaint and on the way back from a preview, and a cursor that only
   * lived in this object would be lost both times.
   */
  render(hits, opts = {}) {
    this.hits = hits || [];
    const cursor = opts.cursor === undefined ? -1 : opts.cursor;
    this.selected = cursor >= 0 && cursor < this.hits.length ? cursor : -1;

    const view = opts.view === "grid" ? "grid" : "list";
    this.el.dataset.view = view;
    if (!this.hits.length) {
      replace(this.el);
      return;
    }

    // Every row on screen is about to be replaced, and one of them may be the
    // element focus is on. Losing focus to the body in the middle of a
    // background revalidation is how a keyboard interface quietly stops.
    const held = this.el.contains(document.activeElement);
    replace(
      this.el,
      this.hits.map((hit, i) => (view === "grid" ? this.cell(hit, i) : this.row(hit, i))),
    );
    // The row is the tab stop, so nothing inside a row is one. A list of twenty
    // rows with a title and three buttons each is eighty tab stops between the
    // search box and the pager, which is the thing the roving tabindex exists
    // to prevent. Everything in here has a key of its own instead: Enter
    // previews, o opens at the source and y copies the link.
    for (const control of this.el.querySelectorAll("a[href], button")) control.tabIndex = -1;
    this.mark();
    // Focus is only ever put back, never taken. A repaint while somebody is
    // typing in the search box must not pull the caret out of it.
    if (held) this.focusCursor({ scroll: false });
  }

  /** busy says a cached answer is being checked, without dimming it. */
  busy(on) {
    this.el.setAttribute("aria-busy", String(Boolean(on)));
  }

  /**
   * skeleton paints the shape of an answer before the answer arrives.
   *
   * The placeholders go inside the list and carry the role its children are
   * supposed to carry, because a placeholder that is not an item makes the list
   * itself invalid for as long as it is loading.
   */
  skeleton(n = 6) {
    this.render([]);
    replace(
      this.el,
      Array.from({ length: n }, () =>
        h(
          "div",
          { class: "skeleton-result", role: "listitem" },
          h("div", { class: "skeleton skeleton-result__tile" }),
          h("div", { class: "skeleton", style: { width: "60%", height: "20px" } }),
          h("div", { class: "skeleton", style: { width: "40%", height: "14px" } }),
          h("div", { class: "skeleton", style: { width: "100%", height: "14px" } }),
          h("div", { class: "skeleton", style: { width: "82%", height: "14px" } }),
        ),
      ),
    );
  }

  /**
   * seat is what every row and every cell carries so the list is one tab stop.
   *
   * A list where each of twenty rows is a tab stop takes forty presses to get
   * past, which is the case the roving tabindex pattern exists for. Exactly one
   * of them is reachable with Tab and the arrow keys move which one that is.
   *
   * Roving tabindex here rather than aria-activedescendant, which is what the
   * omnibox uses, because a row is genuinely focusable and a browser scrolls a
   * focused element into view on its own.
   */
  seat(i) {
    return {
      role: "listitem",
      tabindex: "-1",
      dataset: { index: String(i) },
      // Tabbing in lands on whichever row holds the zero, and clicking a row
      // focuses it. Either way the cursor is now there, so j and k carry on
      // from what somebody is looking at rather than from where they left off.
      onFocus: () => this.at(i),
    };
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
        ...this.seat(i),
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
          href: urlState.documentPath(hit.id, asked()),
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
        ...this.seat(i),
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
          href: urlState.documentPath(hit.id, asked()),
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
        // Only the recent endpoint sends this, and only on the half of it that
        // is somebody's own history. It is on the provenance line rather than
        // in a column of its own so that a row is the same row everywhere, with
        // one more fact on it where there is one more fact to say.
        hit.at && h("span", { class: "crumbs__sep" }, "·"),
        hit.at &&
          h(
            "time",
            { class: "crumbs", title: exact(hit.at), datetime: hit.at },
            `you opened this ${when(hit.at)}`,
          ),
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
                // The class is how the y key finds this button, so that a
                // copy from the keyboard draws the same tick as a copy from
                // the pointer rather than happening invisibly.
                class: "icon-button icon-button--copy",
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

  /** move walks the cursor with j and k, or with the arrow keys. */
  move(delta) {
    if (!this.hits.length) return;
    const next = Math.min(Math.max(this.selected + delta, 0), this.hits.length - 1);
    this.select(next);
  }

  /** edge is Home and End: the first row and the last row on this page. */
  edge(which) {
    if (!this.hits.length) return;
    this.select(which === "first" ? 0 : this.hits.length - 1);
  }

  /** select moves the cursor to a row and puts focus on it. */
  select(i) {
    this.at(i);
    // Focus rather than scrollIntoView: the browser brings a focused element
    // into view on its own, and a row that is highlighted but not focused is a
    // row a screen reader is not reading.
    this.focusCursor();
  }

  /**
   * focusCursor puts focus on the row the cursor is on, if there is one.
   *
   * It is also the way back from the preview. The drawer keeps a reference to
   * whatever had focus when it opened, and by the time it closes that row has
   * been rebuilt by a repaint, so the element it remembers is not in the
   * document any more. The cursor is, and it names the same row.
   */
  focusCursor(opts = {}) {
    if (this.selected < 0) return;
    const row = this.el.querySelector(`[data-index="${this.selected}"]`);
    if (row) row.focus({ preventScroll: opts.scroll === false });
  }

  /** at records where the cursor is without touching focus. */
  at(i) {
    if (this.selected === i) return;
    this.selected = i;
    this.mark();
    this.onCursor(i);
  }

  /**
   * mark writes the cursor into the list.
   *
   * Both layouts, by the attribute they share rather than by class, so j and k
   * walk a grid exactly as they walk a list. Exactly one row holds the zero,
   * and where there is no cursor yet that is the first row, so the first Tab
   * into the list arrives at the top of it rather than nowhere.
   */
  mark() {
    const rows = this.el.querySelectorAll("[data-index]");
    const roving = this.selected < 0 ? 0 : this.selected;
    rows.forEach((row, n) => {
      row.setAttribute("data-active", String(n === this.selected));
      row.tabIndex = n === roving ? 0 : -1;
    });
  }

  /** current is the document under the cursor, if the cursor is on one. */
  current() {
    return this.hits[this.selected];
  }

  /** holds reports whether focus is inside this list. */
  holds() {
    return this.el.contains(document.activeElement);
  }

  /** copyTarget is the copy button on the row under the cursor, if it has one. */
  copyTarget() {
    const row = this.el.querySelector(`[data-index="${this.selected}"]`);
    return row && row.querySelector(".icon-button--copy");
  }
}

/** hasText reports whether the server found anything in this hit worth quoting. */
export function hasText(hit) {
  return Boolean((hit.passages && hit.passages.length) || hit.snippet);
}

/**
 * passages renders the snippet with the matched words marked.
 *
 * The server decided which runs matched, using the analyzer that produced the
 * index. Marking them here with a substring search instead would highlight
 * things the index never matched, which teaches people the wrong thing about
 * why a result came back.
 */
export function passages(hit) {
  if (!hit.passages || !hit.passages.length) return hit.snippet || "";
  return hit.passages.map((p) => (p.match ? h("mark", {}, p.text) : document.createTextNode(p.text)));
}
