// The preview drawer.
//
// Opening a result in its source is a context switch and a page load. Most of
// the time somebody wants to check whether this is the right document, and the
// drawer answers that without leaving the results.
//
// Escape closes it and focus goes back to the row it was opened from. Whether
// it is modal depends on how wide the window is, which is what [Drawer.modal]
// is about.

import { h, replace, svg } from "genba/dom.js";
import { api } from "genba/api.js";
import { cache } from "genba/cache.js";
import { copies } from "genba/clipboard.js";
import { icon, label, sourceColor, when, exact, followable, copyable } from "genba/format.js";
import { body as renderBody, shapeOf, detailOf } from "genba/content.js";
import { reveal } from "genba/marks.js";
import { badge, note, control } from "genba/verify.js";
import { owner, reassign } from "genba/own.js";
import { NOT_AVAILABLE, NO_ACCESS } from "genba/states.js";
import { documentPath } from "genba/state.js";

// The width at which the drawer stops being a panel beside the list and becomes
// the whole window. It is the drawer's own width from app.css, and it is the
// line between a dialog that has to trap focus and one that must not.
const COVERS = "(max-width: 720px)";

export class Drawer {
  constructor({ onClose, onSay }) {
    this.onClose = onClose;
    this.onSay = onSay || (() => {});
    this.returnTo = null;
    this.currentKey = "";
    this.query = "";
    // The passage a citation sent this reader to, or nothing. It is the quote's
    // own text rather than an offset into the body, because an offset is a
    // promise about a document that the renderer breaks the moment it takes the
    // markup out, and because text survives being pasted into a message.
    this.quote = "";
    // The marked words in the body, and which of them is the current one.
    this.marks = [];
    this.at = -1;
    // Whether focus has been put where this document wants it yet.
    this.landed = false;

    this.title = h("h2", { class: "drawer__title", id: "drawer-title", tabindex: "-1" });
    this.meta = h("div", { class: "drawer__meta" });
    this.body = h("div", { class: "drawer__body" });
    this.foot = h("div", { class: "drawer__foot" });
    this.count = h("span", { class: "matches__count" });
    this.matches = h(
      "div",
      { class: "matches", hidden: true },
      this.count,
      h(
        "button",
        {
          class: "icon-button",
          type: "button",
          title: "Previous match (shift+N)",
          "aria-label": "Previous match",
          onClick: () => this.toMatch(-1),
        },
        svg(icon("arrow-up"), 16),
      ),
      h(
        "button",
        {
          class: "icon-button",
          type: "button",
          title: "Next match (n)",
          "aria-label": "Next match",
          onClick: () => this.toMatch(1),
        },
        svg(icon("arrow-down"), 16),
      ),
    );

    this.scrim = h("div", { class: "scrim", hidden: true, onClick: () => this.close() });
    this.el = h(
      "div",
      {
        class: "drawer",
        role: "dialog",
        "aria-modal": "false",
        "aria-labelledby": "drawer-title",
        // So that focus can be taken the moment it opens, before the document
        // it is opening has arrived. Without it the first Tab out of a loading
        // preview lands on the next row behind it.
        tabindex: "-1",
        hidden: true,
        onKeydown: (e) => this.trap(e),
      },
      h(
        "div",
        { class: "drawer__head" },
        h("div", { class: "drawer__heading" }, this.meta, this.title),
        this.matches,
        h(
          "button",
          { class: "icon-button", type: "button", "aria-label": "Close preview", onClick: () => this.close() },
          svg(icon("close"), 20),
        ),
      ),
      this.body,
      this.foot,
    );

    // A window resized across the breakpoint while the preview is open changes
    // the answer, and a drawer that claims to be modal while a list is visible
    // beside it is the case this exists to avoid.
    window.matchMedia(COVERS).addEventListener("change", () => {
      if (this.open) this.modal();
    });
  }

  get open() {
    return !this.el.hidden;
  }

  async show(id, query = "", quote = "") {
    this.query = query;
    this.quote = quote;
    // Only on the way in. Stepping from one result to the next with j replaces
    // what is in an open drawer, and the row to go back to is still the row it
    // was opened from rather than the heading inside it.
    if (!this.open) this.returnTo = document.activeElement;
    this.el.hidden = false;
    this.scrim.hidden = false;
    this.modal();
    this.landed = false;
    this.el.focus();
    this.matches.hidden = true;

    const k = cache.key("document", { id });
    this.currentKey = k;

    // A document the pointer rested on for a moment is usually already here, in
    // which case the drawer never shows a loading state at all. The skeleton is
    // only for the ones that are not.
    if (cache.read(k).state === "miss") this.skeleton();

    let painted = false;
    try {
      await cache.swr(
        k,
        (opts) => api.document(id, opts),
        (d) => {
          if (this.currentKey !== k) return;
          painted = true;
          this.render(d);
        },
      );
    } catch (err) {
      if (err.name === "AbortError") return;
      if (!painted && this.currentKey === k) this.renderError(err);
    }
  }

  skeleton() {
    replace(this.title, "Loading");
    replace(this.meta);
    replace(
      this.body,
      h("div", { class: "skeleton", style: { width: "100%", height: "14px", marginBottom: "8px" } }),
      h("div", { class: "skeleton", style: { width: "92%", height: "14px", marginBottom: "8px" } }),
      h("div", { class: "skeleton", style: { width: "70%", height: "14px" } }),
    );
    replace(this.foot);
  }

  render(d) {
    replace(this.title, d.title || d.id);
    this.stamp(d);
    // The body decides its own shape from the media type, so the drawer only
    // has to say where it goes and how wide it is.
    this.body.dataset.shape = shapeOf(d);
    replace(this.body, renderBody(d, { query: this.query, at: this.quote }));
    // Once the body is on the page and not before, because scrolling to a match
    // that is still in a fragment scrolls to nothing.
    //
    // A passage somebody was sent to wins over the first word that matched, the
    // same way a line number does on a document page and for the same reason: a
    // citation was clicked by a person who meant that sentence, and the query is
    // only how the document came to be on the screen it was clicked from.
    const cited = this.quote ? this.body.querySelector("mark.hit--passage") : null;
    if (cited) cited.scrollIntoView({ block: "center", inline: "nearest" });
    else reveal(this.body);
    this.matched(cited);
    this.land();
  }

  /**
   * stamp draws everything about the document that is not the document.
   *
   * It is its own method because verifying one redraws this and must not redraw
   * the body: rebuilding the body would lose the scroll position, the marked
   * words and the match somebody was standing on, all to change a badge in the
   * header.
   */
  stamp(d) {
    const detail = detailOf(d);
    replace(
      this.meta,
      h(
        "span",
        { class: "source" },
        h("span", { class: "source__dot", style: { background: sourceColor(d.source) } }),
        label(d.source),
      ),
      h("span", { class: "crumbs__sep" }, "·"),
      h("span", {}, label(d.kind)),
      detail && h("span", { class: "crumbs__sep" }, "·"),
      detail && h("span", {}, detail),
      d.container && h("span", { class: "crumbs__sep" }, "·"),
      d.container && h("span", {}, d.container),
      d.owner && h("span", { class: "crumbs__sep" }, "·"),
      owner(d.owner),
      d.verified && h("span", { class: "crumbs__sep" }, "·"),
      badge(d.verified, { by: true }),
    );
    // Opening at the source is only offered where a browser would go there. For
    // every document the file connector read it would not, so the path takes
    // its place: a path somebody can paste into a terminal is worth something,
    // and a link that silently does nothing is worth less than no link at all.
    replace(
      this.foot,
      followable(d.url) &&
        h(
          "a",
          { class: "button button--primary", href: d.url, target: "_blank", rel: "noreferrer noopener" },
          "Open in source",
          svg(icon("external"), 16),
        ),
      h(
        "button",
        {
          class: "button",
          type: "button",
          onClick: (e) =>
            copies(e.currentTarget, new URL(documentPath(d.id), location.origin).href, this.onSay),
        },
        svg(icon("link"), 16),
        "Copy link",
      ),
      d.url &&
        !followable(d.url) &&
        h(
          "button",
          {
            class: "button",
            type: "button",
            onClick: (e) => copies(e.currentTarget, copyable(d.url), this.onSay),
          },
          svg(icon("copy"), 16),
          "Copy path",
        ),
      d.modified_at &&
        h("span", { class: "meta", title: exact(d.modified_at) }, `Updated ${when(d.modified_at)}`),
      // The writes, offered only to somebody the server has already said may
      // make them. Redrawing this and the header is all any of them changes,
      // which is why they are in here.
      control(d, {
        onSay: this.onSay,
        onChange: (next) => {
          d.verified = next;
          this.stamp(d);
        },
      }),
      // Changing who owns it, on the same terms. The write answers with the
      // owner and with what this reader may do next, under the same names the
      // document carries them, so merging it in is the whole update.
      reassign(d, {
        onSay: this.onSay,
        onChange: (next) => {
          Object.assign(d, next);
          this.stamp(d);
        },
      }),
      note(d.verified),
    );
  }

  /**
   * land moves focus to the heading, once, on the way in.
   *
   * The heading rather than the first button, because the first thing somebody
   * asked for is the document and a screen reader should say what it is before
   * it says what can be done to it. It carries tabindex="-1" so it can be
   * focused without becoming a tab stop.
   *
   * Once only, because a background revalidation paints the same document again
   * with focus already somewhere in here, and pulling it back to the top then
   * would take a reader out of the sentence they were on.
   */
  land() {
    if (this.landed) return;
    this.landed = true;
    this.title.focus();
  }

  /**
   * matched wires the header controls to the words marked in the body.
   *
   * The count is read off the page rather than off the query, because marks.js
   * is allowed to find nothing: it does not stem, so a search for cache in a
   * document that only says caching marks nothing at all, and a header offering
   * to walk zero matches is worse than a header that says nothing.
   *
   * here is the mark the document opened on, which is the cited passage when
   * there was one. Without it the count would open at the first match while the
   * page is scrolled to the fifth, and pressing n would jump backwards.
   */
  matched(here) {
    this.marks = [...this.body.querySelectorAll("mark.hit")];
    this.at = -1;
    this.matches.hidden = this.marks.length === 0;
    if (!this.marks.length) {
      replace(this.count);
      return;
    }
    // reveal has already brought one of them into view, so the count opens by
    // saying which one that is rather than starting at nothing.
    this.point(Math.max(0, this.marks.indexOf(here)));
  }

  /**
   * toMatch is n and shift+N, and the two buttons beside the count.
   *
   * It wraps at both ends. Somebody who has walked to the last match in a file
   * wants the first one again, not a key that stops working.
   */
  toMatch(delta) {
    const n = this.marks.length;
    if (!n) return;
    this.point((((this.at + delta) % n) + n) % n).scrollIntoView({ block: "center", inline: "nearest" });
    this.onSay(`Match ${this.at + 1} of ${n}`);
  }

  /** point makes one mark the current one and says which of how many it is. */
  point(i) {
    for (const was of this.marks) was.classList.remove("hit--current");
    this.at = i;
    this.marks[i].classList.add("hit--current");
    replace(this.count, `${i + 1} of ${this.marks.length}`);
    return this.marks[i];
  }

  /**
   * modal says whether the drawer covers the list, and tells assistive
   * technology the same thing.
   *
   * Under the breakpoint the drawer is the whole window and everything behind
   * it is out of reach, which is what aria-modal means and what a focus trap is
   * for. Above it there is a list beside the drawer that is still on screen and
   * still worth reading, and claiming to be modal there takes that list away
   * from a screen reader in exchange for nothing.
   */
  modal() {
    const covers = window.matchMedia(COVERS).matches;
    this.el.setAttribute("aria-modal", String(covers));
    return covers;
  }

  renderError(err) {
    // A document that is not there and a document this viewer may not read say
    // the same thing, from the same two constants, because a message that told
    // them apart would let anybody enumerate what exists by reading the
    // difference.
    const missing = err.status === 404 || err.status === 403;
    replace(this.title, missing ? NOT_AVAILABLE : "Could not load this document");
    replace(this.meta);
    this.body.dataset.shape = "text";
    replace(this.body, h("p", { class: "preview__empty" }, missing ? NO_ACCESS : err.message));
    replace(this.foot);
    this.matched();
    this.land();
  }

  /**
   * close hides the drawer and puts focus back where it came from.
   *
   * notify is false when the URL has already moved on, which is what happens
   * when somebody follows a result title to its own page while the preview of
   * another result is open. Telling the shell to clear the open parameter then
   * would navigate away from the page they just asked for.
   */
  close(opts = {}) {
    if (!this.open) return;
    // A request already in flight is left to finish and fill the cache, because
    // reopening the same document is the most likely next thing to happen. It
    // is the key that is dropped, so nothing it returns is painted.
    this.currentKey = "";
    this.quote = "";
    this.marks = [];
    this.at = -1;
    this.el.hidden = true;
    this.scrim.hidden = true;
    if (opts.focus !== false && this.returnTo && this.returnTo.focus) this.returnTo.focus();
    if (opts.notify !== false) this.onClose();
  }

  /**
   * trap keeps Tab inside the dialog while it is the only thing on screen.
   *
   * Above the breakpoint it does not, because the list is beside the drawer and
   * a trap there is the one way of making a visible list unreachable.
   */
  trap(e) {
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      this.close();
      return;
    }
    if (e.key !== "Tab" || !this.modal()) return;
    const focusable = this.el.querySelectorAll(
      'a[href], button:not([disabled]), input, [tabindex]:not([tabindex="-1"])',
    );
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
}
