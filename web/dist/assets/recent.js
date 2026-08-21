// The recent screen.
//
// It answers a different question from search: not what matches, but what has
// been going on. Until now it was a rail entry that ran a recency sorted search,
// which answers half of that and cannot answer the other half at all, because no
// amount of searching reconstructs what one person happened to read.
//
// Two sections, one request. The server sends both halves together because a
// screen that fetches twice paints twice, and both are permission filtered
// inside the storage driver like everything else, so a document somebody lost
// access to leaves this screen silently.

import { h, replace, svg } from "./dom.js";
import { api } from "./api.js";
import { cache } from "./cache.js";
import { icon } from "./format.js";
import { RowList } from "./rows.js";

// LIMIT is how many rows each half carries, and it is the server's own default.
// Twenty is a screen of them, and this list is read at a glance rather than
// scrolled, so there is no pager under it.
//
// Home shows the top few of the same answer rather than asking for a shorter
// one, so the two screens share a cache entry and moving between them costs a
// revalidation at most.
export const LIMIT = 20;

export class Recent {
  constructor({ onOpen, onHover, onSay }) {
    // Two lists rather than one, because they are two answers and a reader
    // needs to be told which is which. Each carries its own name, since a
    // screen with two lists on it is a screen where "list, 20 items" twice
    // says nothing.
    this.opened = new RowList({ onOpen, onHover, onSay, label: "Documents you opened" });
    this.changed = new RowList({ onOpen, onHover, onSay, label: "Documents that changed" });
    this.openedNote = h("div", {});
    this.changedNote = h("div", {});
    this.painted = false;

    this.el = h(
      "div",
      { class: "recent" },
      h(
        "header",
        { class: "home__greeting" },
        h("h1", { class: "home__title" }, "Recent"),
        h("p", { class: "home__subtitle" }, "What you opened, and what has changed since."),
      ),
      section("You opened", this.opened.el, this.openedNote),
      section("Changed in the corpus", this.changed.el, this.changedNote),
    );
  }

  /** key is the entry this screen paints from, which the offline banner names. */
  key() {
    return cache.key("recent", { limit: LIMIT });
  }

  /**
   * render paints the screen, from cache if there is one.
   *
   * This is a screen people come back to rather than one they load once, so what
   * is held is painted first and the check runs behind it. The entity tag means
   * the usual answer to that check is a few hundred bytes and no repaint at all,
   * which is what keeps the cursor and the scroll position where they were.
   */
  async render() {
    const k = this.key();
    let held = cache.read(k).data || null;
    if (held) this.paint(held);
    else this.loading();

    await cache.swr(k, (opts) => api.recent(LIMIT, opts), (data) => {
      if (data === held) return;
      held = data;
      this.paint(data);
    });
  }

  /** loading is the shape of the answer, for a first visit with nothing held. */
  loading() {
    this.opened.skeleton(3);
    this.changed.skeleton(3);
    replace(this.openedNote);
    replace(this.changedNote);
  }

  paint(data) {
    this.painted = true;
    const opened = (data && data.opened) || [];
    const changed = (data && data.changed) || [];
    // The cursor each list already had, rather than none, because this runs
    // again on a revalidation and losing the row somebody was on to a
    // background request is the thing the tag exists to avoid.
    this.opened.render(opened, { cursor: this.opened.selected });
    this.changed.render(changed, { cursor: this.changed.selected });

    replace(
      this.openedNote,
      opened.length
        ? null
        : note(
            "search",
            "Nothing opened yet",
            "Open a document from a search and it appears here, on whichever machine you sign in from.",
          ),
    );
    replace(
      this.changedNote,
      changed.length
        ? null
        : note("clock", "Nothing has changed", "Nothing you can read has been indexed or updated yet."),
    );
  }

  /**
   * active is the list the keyboard is walking.
   *
   * Two lists on one screen means j and k have to mean one of them. Whichever
   * holds focus wins, and before anything has focus it is the first list with
   * rows in it, so the first press of j lands somewhere useful rather than on a
   * heading with nothing under it.
   */
  active() {
    if (this.opened.holds()) return this.opened;
    if (this.changed.holds()) return this.changed;
    return this.opened.hits.length ? this.opened : this.changed;
  }
}

function section(title, list, note) {
  return h(
    "section",
    { class: "recent__section" },
    h("div", { class: "panel__head" }, h("h2", { class: "panel__title" }, title)),
    list,
    note,
  );
}

function note(name, title, body) {
  return h(
    "div",
    { class: "state" },
    h("span", { class: "state__icon" }, svg(icon(name), 40)),
    h("p", { class: "state__title" }, title),
    h("p", { class: "state__body" }, body),
  );
}
