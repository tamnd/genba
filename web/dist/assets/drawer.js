// The preview drawer.
//
// Opening a result in its source is a context switch and a page load. Most of
// the time somebody wants to check whether this is the right document, and the
// drawer answers that without leaving the results.
//
// It is a modal dialog as far as assistive technology is concerned: focus goes
// in, Tab stays inside, Escape closes it and focus goes back where it was.

import { h, replace, svg } from "./dom.js";
import { api } from "./api.js";
import { cache } from "./cache.js";
import { copies } from "./clipboard.js";
import { icon, label, sourceColor, when, exact, followable, copyable } from "./format.js";
import { body as renderBody, shapeOf } from "./content.js";
import { documentPath } from "./state.js";

export class Drawer {
  constructor({ onClose, onSay }) {
    this.onClose = onClose;
    this.onSay = onSay || (() => {});
    this.returnTo = null;
    this.currentKey = "";

    this.title = h("h2", { class: "drawer__title", id: "drawer-title" });
    this.meta = h("div", { class: "drawer__meta" });
    this.body = h("div", { class: "drawer__body" });
    this.foot = h("div", { class: "drawer__foot" });

    this.scrim = h("div", { class: "scrim", hidden: true, onClick: () => this.close() });
    this.el = h(
      "div",
      {
        class: "drawer",
        role: "dialog",
        "aria-modal": "true",
        "aria-labelledby": "drawer-title",
        hidden: true,
        onKeydown: (e) => this.trap(e),
      },
      h(
        "div",
        { class: "drawer__head" },
        h("div", { class: "drawer__heading" }, this.meta, this.title),
        h(
          "button",
          { class: "icon-button", type: "button", "aria-label": "Close preview", onClick: () => this.close() },
          svg(icon("close"), 20),
        ),
      ),
      this.body,
      this.foot,
    );
  }

  get open() {
    return !this.el.hidden;
  }

  async show(id) {
    this.returnTo = document.activeElement;
    this.el.hidden = false;
    this.scrim.hidden = false;
    this.el.focus();

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
      d.container && h("span", { class: "crumbs__sep" }, "·"),
      d.container && h("span", {}, d.container),
    );
    // The body decides its own shape from the media type, so the drawer only
    // has to say where it goes and how wide it is.
    this.body.dataset.shape = shapeOf(d);
    replace(this.body, renderBody(d));
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
    );
    // Focus lands on the first thing worth acting on once the content is in,
    // rather than on a spinner.
    const first = this.el.querySelector("a, button");
    if (first) first.focus();
  }

  renderError(err) {
    // A document that is not there and a document this viewer may not read say
    // the same thing, because a message that told them apart would let anybody
    // enumerate what exists by reading the difference.
    const missing = err.status === 404 || err.status === 403;
    replace(this.title, missing ? "Not available" : "Could not load this document");
    replace(this.meta);
    this.body.dataset.shape = "text";
    replace(
      this.body,
      missing
        ? "This document no longer exists, or you do not have access to it."
        : err.message,
    );
    replace(this.foot);
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
    this.el.hidden = true;
    this.scrim.hidden = true;
    if (opts.focus !== false && this.returnTo && this.returnTo.focus) this.returnTo.focus();
    if (opts.notify !== false) this.onClose();
  }

  /** trap keeps Tab inside the dialog while it is open. */
  trap(e) {
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      this.close();
      return;
    }
    if (e.key !== "Tab") return;
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
